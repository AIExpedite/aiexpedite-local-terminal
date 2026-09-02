// agent_secret_redaction.go — the single secret-redaction pass applied to any
// CLI-agent text this device publishes to the cloud (stderr frames, start/turn
// failures, probe tails).
//
// Provider-neutral by design: bearer headers, `api_key=` pairs, OAuth URLs,
// credential-file paths and long opaque blobs are shapes, not vendors. It began
// life as `redactAntigravitySecrets` in antigravity_native.go with OpenCode
// calling through a pass-through alias; three providers now depend on it
// (Antigravity, OpenCode, and the Claude Code smoke probe), and a provider name
// on a shared redactor is exactly how a second, drifting copy gets written —
// one agent's frames would then leak what the other's mask.
package main

import "regexp"

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
