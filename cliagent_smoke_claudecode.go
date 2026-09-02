// cliagent_smoke_claudecode.go — the no-tools, non-interactive Claude Code
// health probe behind the signed `__cli_smoke__` operational command.
//
// The CLI-maintenance harness runs this once before and once after a CLI
// upgrade to prove the binary can still complete a round trip. It used to build
// its own `claude -p …` argv, which was pushed through the interactive session
// path: `--print` was stripped, `--input-format stream-json` was forced on, and
// stdin was then closed without an NDJSON envelope — so Claude 2.1.x exited 1 on
// the framing contract before any assistant turn and the marker could never come
// back. Both the pre- and post-update smokes reported `errorCategory: protocol`.
//
// The probe therefore lives HERE, not in the harness: this process is the only
// component that knows the resolved binary path, the sanitized child env, and
// the flag shape that actually works (claude_argv.go).
//
// Cost discipline — a smoke spends ONE real inference turn against the user's
// own subscription window, the same quota the CLI Agents tab reports:
//   - `--version` and `auth status --json` pre-checks short-circuit to
//     provider_unavailable / not_authenticated BEFORE a turn is spent.
//   - A per-CLI cooldown serves the previous verdict to any caller that asks
//     again too soon, so a retry storm cannot drain the user's quota.
//   - The cooldown is INVALIDATED by a binary change (path/mtime/size/version),
//     because that is exactly the post-upgrade smoke the harness must not be
//     served a stale pre-upgrade answer for.
//
// Retention discipline — the child's stdout and stderr are read to derive a
// verdict and are then DISCARDED. They are not published, and they are not
// written to this device's log either: the agent log rotates to disk and can be
// uploaded on request, so "local" is not a safety property. Neither a byte cap
// nor the denylist redactor can guarantee removal of a raw config fragment, a
// private path, a short password, or a credential format the patterns do not
// know — so the only sound rule is to keep no vendor-authored bytes at all.
// What leaves this file is the closed cliSmokeDiagnostic set, the fixed argv
// shape id, and counts. The marker nonce, the prompt, the resolved argv,
// `~/.claude.json` and `settings.json` are never included at any severity.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// claudeSmokeMarkerPrefix + 8 hex chars is the nonce the model is asked to
	// echo. Generated per run: a FIXED marker could be satisfied by a cached
	// transcript or a resumed session and would pass on a dead binary.
	claudeSmokeMarkerPrefix = "AIEXPEDITE_CLI_SMOKE_OK_"

	// cliSmokeCooldown is the minimum spacing between two quota-spending smokes
	// for the same CLI on this device. Within it, the previous verdict is
	// replayed. Reset on agent restart (in-memory) and bypassed when the binary
	// itself changed — see claudeBinaryStamp.
	cliSmokeCooldown = 15 * time.Minute

	cliSmokeStatusSuccess = "success"
	cliSmokeStatusFailed  = "failed"
)

// claudeSmokeTimeout bounds the one inference turn. Deliberately NOT
// machineInfoProbeTimeout (3s), which bounds the pre-checks below: those are
// local process spawns, while this one waits on a model round trip and would
// time out on every healthy device at 3s.
//
// A var, not a const, so a test can shrink it and exercise the real
// deadline-kill path (a killed child reports an *exec.ExitError, NOT a wrapped
// context error — see runClaudeCodeSmoke).
var claudeSmokeTimeout = 60 * time.Second

// cliSmokeDiagnostic is the CLOSED set of locally-derived diagnostics the
// result may carry. It exists because the alternative — publishing the child's
// stderr tail — is unsafe at any cap: stderr is arbitrary vendor text that can
// contain a settings fragment, a private path, or a credential shape no
// denylist regex anticipates. A redactor can only remove what it recognises,
// so the fix is to publish a code we authored, never text the CLI authored.
//
// Every value here is a compile-time constant chosen by our own classifier;
// nothing derived from the child's bytes ever reaches the wire — or the log
// (see claudeSmokeFailureLogLine, which cannot even accept text).
const (
	cliSmokeDiagnosticNone = "" // success, or nothing further to say
	// The CLI rejected one of OUR flags — the one failure that is safe to
	// retry on the next argv rung, because it happens during option parsing,
	// before any inference.
	cliSmokeDiagnosticFlagRejected = "flag_rejected"
	// The CLI rejected the input/output framing contract (the 2.1.x
	// `--input-format=stream-json requires output-format=stream-json` class).
	cliSmokeDiagnosticFramingRejected = "framing_rejected"
	// Exited without emitting the documented terminal `result` envelope.
	cliSmokeDiagnosticNoEnvelope = "no_envelope"
	// A well-formed envelope reporting an auth failure.
	cliSmokeDiagnosticAuthError = "auth_error"
	// A well-formed envelope reporting a provider-side refusal (API error
	// status, overloaded, usage limit).
	cliSmokeDiagnosticProviderError = "provider_error"
	// A well-formed successful envelope whose text was not the marker.
	cliSmokeDiagnosticMarkerMismatch = "marker_mismatch"
	// Our per-attempt deadline killed the child.
	cliSmokeDiagnosticTimeout = "timeout"
	// Pre-check failures — no turn was spent.
	cliSmokeDiagnosticBinaryMissing = "binary_missing"
	cliSmokeDiagnosticNotLoggedIn   = "not_logged_in"
	cliSmokeDiagnosticInternal      = "internal"
	// An unrecognised cliId reached the probe.
	cliSmokeDiagnosticUnknownCLI = "unknown_cli"
)

// cliSmokeResult is the entire published payload of `__cli_smoke_result__`.
// Every field is a metric, a closed-enum value, or a fixed identifier. By
// construction there is NO field that can carry CLI stdout, CLI stderr, prompt
// text, argv values or config content — the diagnostic is a code we chose, and
// the shape id names a ladder entry rather than the flags it produces.
type cliSmokeResult struct {
	CliID         string `json:"cliId"`
	Version       string `json:"version,omitempty"`
	Status        string `json:"status"`
	ErrorCategory string `json:"errorCategory,omitempty"`
	MarkerMatched bool   `json:"markerMatched"`
	DurationMs    int64  `json:"durationMs"`
	// ArgvShapeID names the ladder entry that ran (claudeArgvShapes), never the
	// argv itself.
	ArgvShapeID string `json:"argvShapeId,omitempty"`
	// Diagnostic is one of the cliSmokeDiagnostic* constants — a locally
	// authored code, never vendor text.
	Diagnostic string `json:"diagnostic,omitempty"`
}

/* --------------------------------------------------------------------------
   Exec seam
   -------------------------------------------------------------------------- */

// runClaudeSmokeCommand spawns the probe. A package-level var so tests drive
// the classification without ever spawning a real binary or spending a turn —
// the same seam shape as runClaudeAuthStatusCommand.
//
// The prompt goes on STDIN as plain text and stdin is closed as soon as it is
// written (a strings.Reader reaches EOF, which os/exec turns into a close), so
// the marker never appears in argv or in a process listing.
var runClaudeSmokeCommand = func(ctx context.Context, path string, args []string, env []string, prompt string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Background probe on a tray app with no console of its own — without this
	// a console window flashes on the user's desktop. Same treatment as every
	// other silent probe.
	hideWindow(cmd)
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// claudeBinaryStamp identifies a specific Claude Code binary: its path, its
// (mtime, size), and the version it reports. The same triple systemInfo.go's
// version-probe cache keys on, plus the version string so a rebuild that
// preserves mtime/size but bumps the version still reads as a change.
//
// It keys the smoke cooldown: a verdict cached before an upgrade must never be
// served for the post-upgrade binary. (Hook repair after a Claude update is a
// separate mechanism — ensureClaudeStatusLineHookIfStale in statusline_install.go
// — and does not read this stamp.)
func claudeBinaryStamp(path, version string) string {
	if path == "" {
		return "absent"
	}
	info, err := os.Stat(path)
	if err != nil {
		return path + "|?|?|" + version
	}
	return fmt.Sprintf("%s|%d|%d|%s", path, info.ModTime().UnixNano(), info.Size(), version)
}

/* --------------------------------------------------------------------------
   Resolved-shape cache (per binary)
   -------------------------------------------------------------------------- */

// claudeSmokeShapeKey is keyed on (path, mtime, size) — the same key the
// `--version` probe uses in systemInfo.go. A reinstall or upgrade changes the
// key, so a shape that stopped working is re-resolved exactly when the binary
// changes and never re-probed in between.
type claudeSmokeShapeKey struct {
	Path    string
	ModUnix int64
	Size    int64
}

var (
	claudeSmokeShapeMu    sync.Mutex
	claudeSmokeShapeCache = map[claudeSmokeShapeKey]string{}
)

func claudeSmokeShapeKeyFor(path string) (claudeSmokeShapeKey, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return claudeSmokeShapeKey{}, false
	}
	return claudeSmokeShapeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}, true
}

// claudeSmokeShapeLadder returns the shapes to try, newest-known-good first.
// When a shape has already been resolved for this exact binary it is the ONLY
// entry, so a steady-state smoke spawns one child rather than walking the
// ladder again.
func claudeSmokeShapeLadder(path string) []claudeArgvShape {
	key, ok := claudeSmokeShapeKeyFor(path)
	if !ok {
		return claudeArgvShapes
	}
	claudeSmokeShapeMu.Lock()
	resolved, cached := claudeSmokeShapeCache[key]
	claudeSmokeShapeMu.Unlock()
	if !cached {
		return claudeArgvShapes
	}
	for _, shape := range claudeArgvShapes {
		if shape.ID == resolved {
			return []claudeArgvShape{shape}
		}
	}
	return claudeArgvShapes
}

func rememberClaudeSmokeShape(path, shapeID string) {
	key, ok := claudeSmokeShapeKeyFor(path)
	if !ok {
		return
	}
	claudeSmokeShapeMu.Lock()
	// Drop stale entries for the same path first: an upgrade leaves the old
	// (mtime,size) key behind and nothing else prunes this map.
	for k := range claudeSmokeShapeCache {
		if k.Path == path && k != key {
			delete(claudeSmokeShapeCache, k)
		}
	}
	claudeSmokeShapeCache[key] = shapeID
	claudeSmokeShapeMu.Unlock()
}

// resetClaudeSmokeState clears the resolved-shape and cooldown caches.
// Test-only seam.
func resetClaudeSmokeState() {
	claudeSmokeShapeMu.Lock()
	claudeSmokeShapeCache = map[claudeSmokeShapeKey]string{}
	claudeSmokeShapeMu.Unlock()
	cliSmokeCooldownMu.Lock()
	cliSmokeCooldownCache = map[string]cliSmokeCooldownEntry{}
	cliSmokeCooldownMu.Unlock()
}

/* --------------------------------------------------------------------------
   Cooldown
   -------------------------------------------------------------------------- */

type cliSmokeCooldownEntry struct {
	At     time.Time
	Stamp  string
	Result cliSmokeResult
}

var (
	cliSmokeCooldownMu    sync.Mutex
	cliSmokeCooldownCache = map[string]cliSmokeCooldownEntry{}
)

// resolveClaudeSmokePath resolves the binary the smoke will probe, through the
// same cached resolver every other Claude path in this process uses. A var so
// tests can point it at a stub without inheriting the process-wide
// sync.Once-cached real install.
var resolveClaudeSmokePath = func() string { return resolveExecutable("claude") }

// runCLISmoke is the entry point the `__cli_smoke__` handler calls. It resolves
// the CLI, applies the cooldown, and returns (result, servedFromCooldown).
//
// An unrecognised cliId returns provider_unavailable WITHOUT spawning anything
// — it must never fall through to a generic execute.
func runCLISmoke(ctx context.Context, cliID string) (cliSmokeResult, bool) {
	if cliID != "claudeCode" {
		return cliSmokeResult{
			CliID:         cliID,
			Status:        cliSmokeStatusFailed,
			ErrorCategory: cliUsageErrorProviderUnavailable,
			Diagnostic:    cliSmokeDiagnosticUnknownCLI,
		}, false
	}

	path := resolveClaudeSmokePath()
	version := ""
	if path != "" {
		version = cachedProbeVersion(path)
	}
	stamp := claudeBinaryStamp(path, version)

	cliSmokeCooldownMu.Lock()
	entry, seen := cliSmokeCooldownCache[cliID]
	cliSmokeCooldownMu.Unlock()
	if seen && entry.Stamp == stamp && time.Since(entry.At) < cliSmokeCooldown {
		return entry.Result, true
	}

	result := runClaudeCodeSmoke(ctx, path, version)

	cliSmokeCooldownMu.Lock()
	cliSmokeCooldownCache[cliID] = cliSmokeCooldownEntry{At: time.Now(), Stamp: stamp, Result: result}
	cliSmokeCooldownMu.Unlock()
	return result, false
}

/* --------------------------------------------------------------------------
   Probe
   -------------------------------------------------------------------------- */

// claudePrintResultEnvelope is the single terminal object `--output-format json`
// emits. Only the fields the classification needs are decoded; `result` is
// compared against the marker and NEVER published.
type claudePrintResultEnvelope struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	APIErrorStatus *int   `json:"api_error_status"`
}

// runClaudeCodeSmoke performs the probe against an already-resolved binary.
// Split from runCLISmoke so the cooldown/catalog concerns stay out of the
// classification logic.
func runClaudeCodeSmoke(ctx context.Context, path, version string) cliSmokeResult {
	result := cliSmokeResult{CliID: "claudeCode", Version: version, Status: cliSmokeStatusFailed}
	started := time.Now()
	finish := func(category string) cliSmokeResult {
		result.ErrorCategory = category
		result.DurationMs = time.Since(started).Milliseconds()
		return result
	}

	// Pre-check 1: the binary must exist and answer `--version`. Both failures
	// mean there is nothing to smoke — and neither costs a turn.
	if path == "" || version == "" {
		result.Diagnostic = cliSmokeDiagnosticBinaryMissing
		return finish(cliUsageErrorProviderUnavailable)
	}

	// Pre-check 2: a logged-out CLI cannot complete a turn, so spawning one
	// only costs a child process and reports a vaguer category than the truth.
	// An INCONCLUSIVE probe (known == false) deliberately proceeds: that is how
	// a working env-credential login looks, and refusing there would report a
	// broken CLI that is in fact healthy.
	if loggedIn, known := claudeAuthStatusProbe(path); known && !loggedIn {
		result.Diagnostic = cliSmokeDiagnosticNotLoggedIn
		return finish(cliUsageErrorNotAuthenticated)
	}

	marker, err := newClaudeSmokeMarker()
	if err != nil {
		result.Diagnostic = cliSmokeDiagnosticInternal
		return finish(cliUsageErrorInternal)
	}
	prompt := claudeSmokePrompt(marker)
	env, _ := prepareClaudeChildEnv(path, os.Environ())

	var lastCategory, lastDiagnostic string
	for _, shape := range claudeSmokeShapeLadder(path) {
		runCtx, cancel := context.WithTimeout(ctx, claudeSmokeTimeout)
		stdout, stderr, runErr := runClaudeSmokeCommand(
			runCtx, path, buildClaudeNonInteractivePrintArgs(shape), env, prompt)
		// Read the PER-ATTEMPT context before cancelling it. exec.CommandContext
		// kills the child when its deadline fires and then reports an
		// *exec.ExitError ("signal: killed" / "exit status 1") — NOT an error
		// wrapping context.DeadlineExceeded. Judging the outcome by the parent
		// ctx (which is still healthy) read a 60-second timeout as `protocol`,
		// and — worse — sent the ladder off to burn another 60 seconds.
		timedOut := runCtx.Err() != nil || ctx.Err() != nil
		cancel()

		result.ArgvShapeID = shape.ID

		category, diagnostic, matched := classifyClaudeSmokeRun(timedOut, stdout, stderr, runErr, marker)
		if category == "" {
			result.Status = cliSmokeStatusSuccess
			result.MarkerMatched = matched
			result.Diagnostic = diagnostic
			result.DurationMs = time.Since(started).Milliseconds()
			rememberClaudeSmokeShape(path, shape.ID)
			return result
		}
		lastCategory, lastDiagnostic = category, diagnostic
		// Device-local log line. It carries ONLY closed values plus the stderr
		// LENGTH — a metric, not content. The child's stderr is discarded here;
		// it lives no longer than the classifier that read it.
		//
		// An earlier revision logged a capped, redacted tail. That was the same
		// mistake as publishing it: this agent's log is rotated to disk and can
		// be uploaded on request, and a denylist redactor cannot see a raw
		// config fragment, a private path, a short password, or a credential
		// format it does not know. "Local" is not a safety property; not
		// retaining the bytes is.
		fmt.Print(claudeSmokeFailureLogLine(shape.ID, category, diagnostic, len(stderr)))

		// Retry the next rung ONLY when the CLI positively rejected a flag that
		// rung drops. That rejection happens during option parsing, before any
		// inference, so it is the one failure a retry cannot double-charge for.
		// Any other protocol failure (malformed output, an undocumented error
		// envelope) may already have consumed the user's turn, so retrying it
		// would break the one-turn-per-smoke bound this probe promises.
		if diagnostic != cliSmokeDiagnosticFlagRejected {
			break
		}
	}

	result.MarkerMatched = false
	result.Diagnostic = lastDiagnostic
	return finish(lastCategory)
}

// claudeSmokeFailureLogLine renders the device-local diagnostic line.
//
// It takes the stderr LENGTH rather than the bytes, deliberately: a function
// that cannot receive vendor text cannot leak it, no matter how a future caller
// wires it up. Every other argument is a value this package defines. Returned
// as a string (rather than printed inline) so a test can assert the exact
// vocabulary that reaches the log.
func claudeSmokeFailureLogLine(shapeID, category, diagnostic string, stderrBytes int) string {
	return fmt.Sprintf("%s[cli-smoke] claudeCode shape=%s category=%s diagnostic=%s stderrBytes=%d%s\n",
		colorYellow, shapeID, category, diagnostic, stderrBytes, colorReset)
}

// classifyClaudeSmokeRun maps one exec outcome onto the closed receipt enum and
// a locally authored diagnostic. Returns ("", …, true) only for an exact marker
// match. Kept pure (no exec, no clock) so every arm is unit-testable; `timedOut`
// is passed in because only the caller can see the per-attempt context.
func classifyClaudeSmokeRun(timedOut bool, stdout, stderr []byte, runErr error, marker string) (category, diagnostic string, matched bool) {
	// An expired/cancelled attempt outranks whatever the child reported: the
	// kill IS the reason the output is incomplete, and the child's own
	// *exec.ExitError says nothing about why it died.
	if timedOut || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		return cliUsageErrorProviderTimeout, cliSmokeDiagnosticTimeout, false
	}

	envelope, ok := parseClaudePrintResultEnvelope(stdout)
	if !ok {
		// Exit non-zero with no envelope, or unparseable/absent JSON: the CLI
		// rejected our invocation shape before producing its documented output.
		// This is the exit-1 framing failure the smoke exists to detect.
		return cliUsageErrorProtocol, claudeSmokeNoEnvelopeDiagnostic(stderr), false
	}

	if envelope.IsError || envelope.Subtype != "success" {
		lower := strings.ToLower(envelope.Result + " " + envelope.Subtype)
		switch {
		case strings.Contains(lower, "authenticat"), strings.Contains(lower, "login"),
			strings.Contains(lower, "logged out"), strings.Contains(lower, "credential"):
			return cliUsageErrorNotAuthenticated, cliSmokeDiagnosticAuthError, false
		case envelope.APIErrorStatus != nil, strings.Contains(lower, "overloaded"),
			strings.Contains(lower, "usage limit"), strings.Contains(lower, "rate limit"):
			// The CLI and our invocation are both fine; the provider refused.
			return cliUsageErrorProviderUnavailable, cliSmokeDiagnosticProviderError, false
		default:
			return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
		}
	}

	// A well-formed successful envelope whose text is not the marker means the
	// model was chatty ("Sure! …"), NOT that the CLI contract is broken —
	// classifying that as `protocol` would make a healthy CLI look like the
	// regression this feature fixes.
	if strings.TrimSpace(envelope.Result) != marker {
		return cliUsageErrorParseFailed, cliSmokeDiagnosticMarkerMismatch, false
	}
	return "", cliSmokeDiagnosticNone, true
}

// claudeSmokeNoEnvelopeDiagnostic separates the two no-envelope failures that
// must be treated differently:
//
//   - flag_rejected — the CLI refused one of OUR options during parsing. No
//     turn was spent, so the next ladder rung (which drops exactly those
//     options) may be tried.
//   - framing_rejected / no_envelope — anything else. A turn MAY already have
//     been consumed, so the ladder stops here.
//
// The stderr bytes are read only to pick between these constants; not one byte
// of them travels any further.
func claudeSmokeNoEnvelopeDiagnostic(stderr []byte) string {
	lower := strings.ToLower(string(stderr))
	rejected := strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "unknown argument") ||
		strings.Contains(lower, "unrecognized option") ||
		strings.Contains(lower, "unrecognised option") ||
		strings.Contains(lower, "unknown or unexpected option")
	if rejected && claudeSmokeMentionsOptionalFlag(lower) {
		return cliSmokeDiagnosticFlagRejected
	}
	if strings.Contains(lower, "input-format") || strings.Contains(lower, "output-format") ||
		strings.Contains(lower, "stream-json") {
		return cliSmokeDiagnosticFramingRejected
	}
	return cliSmokeDiagnosticNoEnvelope
}

// claudeSmokeMentionsOptionalFlag reports whether the rejection names a flag
// that a LATER ladder rung actually drops. A rejection of `--print` or
// `--output-format` is not retryable — no rung omits those — and treating it as
// such would spend a second turn to fail identically.
func claudeSmokeMentionsOptionalFlag(lowerStderr string) bool {
	for _, flag := range claudeRetryableArgvFlags {
		if strings.Contains(lowerStderr, flag) {
			return true
		}
	}
	return false
}

// parseClaudePrintResultEnvelope extracts the terminal `result` object from the
// probe's stdout. `--output-format json` emits exactly one object, but the
// scan is line-tolerant so a build that prefixes a banner line (or emits
// NDJSON) still yields its result envelope rather than reading as protocol.
func parseClaudePrintResultEnvelope(stdout []byte) (claudePrintResultEnvelope, bool) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return claudePrintResultEnvelope{}, false
	}
	var envelope claudePrintResultEnvelope
	if json.Unmarshal(trimmed, &envelope) == nil && envelope.Type == "result" {
		return envelope, true
	}
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var candidate claudePrintResultEnvelope
		if json.Unmarshal(line, &candidate) == nil && candidate.Type == "result" {
			envelope = candidate
			return envelope, true
		}
	}
	return claudePrintResultEnvelope{}, false
}

// claudeSmokePrompt is the one-turn instruction. It names no path, no account
// and no configuration — the marker is the only variable part, and it never
// leaves this process (the published result carries markerMatched, not the
// marker).
func claudeSmokePrompt(marker string) string {
	return "Reply with exactly this text and nothing else, no punctuation and no explanation: " + marker
}

func newClaudeSmokeMarker() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return claudeSmokeMarkerPrefix + hex.EncodeToString(buf), nil
}
