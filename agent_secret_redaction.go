// agent_secret_redaction.go — the single secret-redaction pass applied to any
// CLI-agent text this device publishes to the cloud (stderr frames, start/turn
// failures, probe tails).
//
// Provider-neutral by design: bearer headers, `api_key=` pairs, OAuth URLs,
// credential-file paths and long opaque blobs are shapes, not vendors. It began
// life as `redactAntigravitySecrets` in antigravity_native.go with OpenCode
// calling through a pass-through alias; four providers now depend on it
// (Antigravity, OpenCode, Muse Code via the one-shot core — whose META_API_KEY
// is covered by the `api_key=` shape — and the Claude Code smoke probe), and a provider name
// on a shared redactor is exactly how a second, drifting copy gets written —
// one agent's frames would then leak what the other's mask.
package main

import (
	"regexp"
	"strings"
)

var agentSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
	regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)\S+`),
	regexp.MustCompile(`(?i)(token\s*[=:]\s*)[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`https://accounts\.google\.com/[^\s]+`),
	regexp.MustCompile(`https://[^\s]*oauth[^\s]*`),
	// Credential FILE paths (…/.credentials.json, …\.credentials.json). The
	// path is not itself a secret, but a CLI that fails while reading one
	// frequently quotes surrounding file content on the same line, and the
	// user's home/profile layout is not something a published frame needs.
	regexp.MustCompile(`(?i)\S*\.credentials\.json\S*`),
}

// agentOpaqueBlobPattern matches very long opaque blobs (likely tokens/JWTs),
// deliberately NOT ordinary UUIDs (~36 chars) or short hashes that appear in
// diagnostics. Compiled once at package level — this runs on every published
// stderr frame.
var agentOpaqueBlobPattern = regexp.MustCompile(`[A-Za-z0-9_-]{80,}`)

// redactAgentSecrets masks credential material in text that is about to leave
// the device. Safe to apply twice (the replacements contain no secret shapes).
func redactAgentSecrets(s string) string {
	out := s
	for _, re := range agentSecretPatterns {
		out = re.ReplaceAllString(out, "${1}[REDACTED]")
	}
	out = agentOpaqueBlobPattern.ReplaceAllStringFunc(out, func(m string) string {
		return m[:8] + "…[REDACTED]"
	})
	return out
}

// agentSecretCarryTailPattern matches a trailing credential KEY whose value has
// not fully arrived: `api_key=`, `token: ab`, `authorization: bearer `. The
// per-frame patterns above need the whole pair on one string, so a pair split
// across two streamed frames would publish the value verbatim.
var agentSecretCarryTailPattern = regexp.MustCompile(
	`(?i)(?:authorization:\s*bearer|api[_-]?key\s*[=:]|token\s*[=:])\s*[A-Za-z0-9._\-]*$`)

// agentOpaqueBlobCarryTailPattern matches a trailing opaque run long enough to
// grow into the 80-char blob shape. The 16-char floor is where an unbroken run
// stops looking like ordinary streamed prose: lower would hold back the tail of
// almost every flush (and stall words like "thinking"), higher would let a
// split blob through. Residual, by construction: a blob split at fewer than 16
// characters publishes that leading fragment unmasked; maskOpaqueContinuation
// then masks the rest of the run once the two halves together reach the blob
// shape — so no whole credential streams out, but a short prefix can.
var agentOpaqueBlobCarryTailPattern = regexp.MustCompile(`[A-Za-z0-9_-]{16,}$`)

// agentSecretCarryMaxBytes bounds the held-back tail. Past it the text is
// emitted (masked by the per-frame pass) rather than buffered without limit —
// an unbroken multi-kilobyte run is not a credential the carry would classify.
const agentSecretCarryMaxBytes = 4096

// splitRedactionCarry splits streamed text into the part that is safe to
// publish now and a tail to prepend to the next flush, so redactAgentSecrets
// sees credential material whole. A masked frame cannot be retracted, so the
// ambiguous tail waits for the bytes that would classify it; every caller
// flushes the carry unconditionally when the stream ends.
func splitRedactionCarry(text string) (emit string, carry string) {
	if text == "" {
		return "", ""
	}
	idx := -1
	if loc := agentSecretCarryTailPattern.FindStringIndex(text); loc != nil {
		idx = loc[0]
	} else if loc := agentOpaqueBlobCarryTailPattern.FindStringIndex(text); loc != nil {
		idx = loc[0]
	}
	// idx == 0 means the ambiguous tail IS the whole buffer — a timer window
	// that held nothing but `META_API_KEY=`, or nothing but the opaque run that
	// follows one. That is exactly the case the carry exists for, so it is
	// carried too: publishing it would stream the label without its value (or
	// the value without its label), and neither frame can then be matched by
	// the per-frame pass. Progress is still guaranteed because the carry is
	// released unconditionally once it reaches the bound below — which equals
	// the coalescer's own flush threshold — or when the stream ends.
	if idx < 0 || len(text)-idx >= agentSecretCarryMaxBytes {
		return text, ""
	}
	return text[:idx], text[idx:]
}

// agentOpaqueBlobMinLen is the run length agentOpaqueBlobPattern masks.
const agentOpaqueBlobMinLen = 80

// agentOpaqueRunChars is the character class of agentOpaqueBlobPattern.
const agentOpaqueRunChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// maskOpaqueContinuation closes the residual splitRedactionCarry leaves: a run
// published below the carry floor (a 10-char prefix) whose continuation — up
// to 79 chars, too short for the per-frame blob pattern on its own — follows
// in a later flush. publishedRun is the length of the unbroken run the
// previous published text ENDED with; when it plus this text's leading run
// reaches the blob shape, the leading run is masked. Returns the text to
// publish and the run length to pass into the next call. text is the raw
// (pre-render) stream text, so the run is measured on what the CLI streamed.
func maskOpaqueContinuation(text string, publishedRun int) (string, int) {
	lead := len(text) - len(strings.TrimLeft(text, agentOpaqueRunChars))
	if lead == len(text) {
		nextRun := publishedRun + lead
		if publishedRun > 0 && nextRun >= agentOpaqueBlobMinLen {
			return "[REDACTED]", nextRun
		}
		return text, nextRun
	}
	tail := len(text) - len(strings.TrimRight(text, agentOpaqueRunChars))
	if lead > 0 && publishedRun > 0 && publishedRun+lead >= agentOpaqueBlobMinLen {
		text = "[REDACTED]" + text[lead:]
	}
	return text, tail
}
