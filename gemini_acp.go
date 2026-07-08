// File: gemini_acp.go
// -----------------------------------------------------------------------------
// GeminiACPManager — long-lived `gemini --experimental-acp` sessions used by
// AI Expedite to drive Google's Gemini CLI via its ACP (Agent Client Protocol)
// JSON-RPC 2.0 interface over the child process' stdio (newline-delimited
// JSON). Same wire protocol family as the Grok ACP integration, so the
// transport, framing, fail-fast policy, lifecycle and cleanup story all live
// in the shared ACP core (acp_core.go) — this file contributes only the
// Gemini-specific configuration: the `gemini_acp_*` result-type names, the
// argv builder/sanitiser, and the env policy.
//
// The single-turn Gemini path (buildGeminiInteractiveArgs in session.go,
// prompt piped over stdin in default interactive mode) is a SEPARATE code
// path and stays untouched: over a pipe gemini treats stdin as one turn and
// exits, which is exactly what `session_start` wants and exactly what a
// multi-turn chat cannot use. Multi-turn sessions come through here instead,
// on gemini's experimental ACP stdio mode.
//
// Auth posture mirrors Grok's: enforced by the orchestrator, not here. The
// user's local `gemini` login (OAuth creds under ~/.gemini) is what the child
// authenticates with, so usage ties to the terminal computer user's Google
// account. Unlike Grok there is no API-key opt-in gate on this path: no
// credential flags are accepted on the argv, and inherited env keys
// (GEMINI_API_KEY / GOOGLE_API_KEY) pass through exactly as they do on the
// raw single-turn `session_start` path — sanitizeGeminiACPEnv only strips
// the embedded-IDE markers.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"strings"
)

// geminiACPSpec parameterises the shared ACP core for the Gemini family. The
// stall hint mirrors Grok's: a `gemini --experimental-acp` child that spawns
// but never emits a frame is almost always blocked on an interactive OAuth
// sign-in it cannot present over headless stdio.
var geminiACPSpec = acpSpec{
	family:      "gemini_acp",
	logTag:      "gemini-acp",
	noun:        "gemini acp",
	agentName:   "gemini",
	transport:   "gemini --experimental-acp",
	startHint:   "is `gemini` on PATH? run `gemini` in a terminal to sign in",
	stallHint:   "it is most likely not signed in on this computer (its saved Google OAuth token expired; run `gemini` in a terminal on the terminal computer to sign in again) or wedged at startup.",
	messageType: "gemini_acp_message",
	stderrType:  "gemini_acp_stderr",
	errorType:   "gemini_acp_error",
	endedType:   "gemini_acp_ended",
	// Gemini usage-limit telemetry: the ACP transport is the primary path
	// for multi-turn Gemini sessions (`gemini_acp_start`), and quota /
	// rate-limit errors (429 RESOURCE_EXHAUSTED, "quota … exceeded") arrive
	// on this same stdout stream. The raw `session_start` path in session.go
	// calls captureGeminiUsageLimitLine; without this hook, the CLI Agents
	// card stays Unknown for the primary Gemini chat flow.
	captureLine: captureGeminiUsageLimitLine,
}

// GeminiACPSession is the Gemini-family view of a shared ACP session. Alias
// rather than a wrapper: the session carries no Gemini-specific state.
type GeminiACPSession = acpSession

// GeminiStartOptions bundles the per-session policy knobs the dispatcher
// reads from Config + commandMsg before spawning a Gemini ACP child. Same
// shape as GrokStartOptions minus the Grok-only auth/approval gates — Gemini
// has no argv credential surface, and its tool approvals ride inside the ACP
// protocol (session/request_permission), which the orchestrator answers.
type GeminiStartOptions struct {
	// TimeoutMs is the backend-requested per-session deadline. 0 means
	// "no deadline" and the session lives until the 6h stale GC, End(),
	// orchestrator-driven cancellation, or the child's natural exit. Values
	// above acpMaxLifetime are clamped to acpMaxLifetime.
	TimeoutMs int64

	// WorkspaceRoot, when non-empty, is treated as a containment root: the
	// requested cwd must resolve (after EvalSymlinks) to a path strictly
	// inside this root. When empty, no containment check runs — but Start
	// still requires cwd to be absolute and exist. Sourced from
	// Config.WorkingDirectory at the dispatcher.
	WorkspaceRoot string
}

/* --------------------------------------------------------------------------
   GeminiACPManager — Gemini configuration over the shared ACP core
   -------------------------------------------------------------------------- */

// GeminiACPManager owns the active `gemini --experimental-acp` processes. One
// manager handles many concurrent sessions, mirroring GrokACPManager's shape.
// All generic lifecycle methods (Send/End/Get/ActiveCount/CleanupStale/
// ShutdownAll/ArmFirstFrameWatchdog) are promoted from the embedded core.
type GeminiACPManager struct {
	acpManager
}

// NewGeminiACPManager creates a fresh manager.
func NewGeminiACPManager() *GeminiACPManager {
	return &GeminiACPManager{acpManager: newACPManager(geminiACPSpec)}
}

// Start launches `gemini --experimental-acp` in cwd. extraArgs are passed
// through after the built-in transport flag so the orchestrator can supply
// Gemini-specific knobs (e.g. `--model gemini-3-pro`) without us
// special-casing every Gemini flag. The shared core owns cwd validation,
// containment and process lifecycle; the prepare callback below owns the
// Gemini argv/env policy. An unusable binary (not on PATH) surfaces as a
// start error the dispatcher publishes as `gemini_acp_error`; a child that
// spawns but exits immediately (e.g. an old gemini that rejects
// `--experimental-acp`) surfaces through the normal stream path as
// `gemini_acp_stderr` frames plus a terminal `gemini_acp_ended` with the
// non-zero exit code — matching how Grok startup failures are reported.
func (m *GeminiACPManager) Start(id, cwd string, extraArgs []string, workspaceID, uid string, opts GeminiStartOptions, publishFn PublishFunc) error {
	return m.start(id, cwd, opts.WorkspaceRoot, opts.TimeoutMs, workspaceID, uid, publishFn, func() (acpSpawnPlan, error) {
		executable := resolveExecutable("gemini")
		args := buildGeminiACPArgs(extraArgs)
		return acpSpawnPlan{
			executable: executable,
			args:       args,
			logArgs:    redactArgs(args),
			env:        sanitizeGeminiACPEnv(os.Environ()),
		}, nil
	})
}

/* --------------------------------------------------------------------------
   argv + env builders
   -------------------------------------------------------------------------- */

// buildGeminiACPArgs constructs argv for `gemini --experimental-acp`.
//
// The transport flag comes first and is owned here; extraArgs are appended
// after it with the tokens that would break ACP mode stripped by
// sanitizeGeminiACPExtraArgs. Unlike Grok there is no subcommand ordering
// constraint — gemini takes plain flags — so surviving extras can trail the
// transport flag directly.
func buildGeminiACPArgs(extraArgs []string) []string {
	args := []string{"--experimental-acp"}
	args = append(args, sanitizeGeminiACPExtraArgs(extraArgs)...)
	return args
}

// sanitizeGeminiACPExtraArgs filters caller-supplied extra args down to
// tokens that are safe to splice onto a `gemini --experimental-acp` argv.
// Dropped tokens:
//
//   - `--experimental-acp` — owned by buildGeminiACPArgs; a duplicate is
//     harmless on current gemini but pointless on the argv.
//   - `-p`/`--prompt` and `-i`/`--prompt-interactive` (both forms) — any of
//     these switches gemini OUT of ACP stdio mode into a one-shot headless /
//     interactive run, which would break the JSON-RPC handshake the
//     orchestrator is about to drive. The single-turn path (session_start →
//     buildGeminiInteractiveArgs) is where prompts belong.
//   - the POSIX `--` end-of-options delimiter — everything after it would be
//     read as a positional prompt, which switches modes exactly like `-p`.
//   - privilege-escalating flags (see geminiACPPrivilegedFlag): `-y`/`--yolo`
//     and `--approval-mode` auto-approve tool calls, bypassing the
//     orchestrator-driven `session/request_permission` flow this manager
//     relies on for approvals; `--allowed-tools` pre-approves named tools the
//     same way; `--include-directories` adds extra workspace directories,
//     routing around the WorkspaceRoot cwd containment Start enforces. Unlike
//     Grok's `--allow` there is no per-workspace opt-in gate for these —
//     Gemini approvals must ride the ACP protocol, so they are stripped
//     unconditionally.
//
// Everything else (e.g. `--model <x>`) passes through verbatim — gemini
// validates its own flags and an unrecognized one fails the child fast, which
// the stream/exit path surfaces as `gemini_acp_stderr` + `gemini_acp_ended`.
func sanitizeGeminiACPExtraArgs(extraArgs []string) []string {
	cleaned := make([]string, 0, len(extraArgs))
	skipNext := false
	for i := 0; i < len(extraArgs); i++ {
		a := extraArgs[i]
		if skipNext {
			skipNext = false
			continue
		}
		lower := strings.ToLower(a)

		if lower == "--experimental-acp" {
			continue
		}

		// Prompt flags take a value in separate-token form; consume it too.
		if lower == "-p" || lower == "--prompt" || lower == "-i" || lower == "--prompt-interactive" {
			if i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--prompt=") || strings.HasPrefix(lower, "-p=") ||
			strings.HasPrefix(lower, "--prompt-interactive=") || strings.HasPrefix(lower, "-i=") {
			continue
		}

		// Privileged flags never reach the argv (rationale in the function
		// doc). Separate-token value forms consume the following token too.
		if priv, takesValue := geminiACPPrivilegedFlag(lower); priv {
			if takesValue && i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}

		// POSIX end-of-options delimiter: everything after it is a
		// positional prompt, which flips gemini out of ACP mode exactly like
		// `-p`. Drop the delimiter AND everything after it — a legitimate
		// flag value never follows a bare `--`, so nothing usable is lost.
		if a == "--" {
			break
		}

		cleaned = append(cleaned, a)
	}
	return cleaned
}

// geminiACPPrivilegedFlag classifies a lowercased extra-arg token against the
// deny list of privilege-escalating gemini flags: `-y`/`--yolo` and
// `--approval-mode` bypass per-tool approvals, `--allowed-tools` pre-approves
// named tools, `--include-directories` escapes the WorkspaceRoot containment.
// gemini's yargs parser also accepts camelCase spellings of every kebab-case
// flag, so both spellings are matched (ToLower collapses camelCase to the
// dash-less form). takesValue is true only for the separate-token value form;
// equals-form tokens carry their value inline and consume nothing extra.
func geminiACPPrivilegedFlag(lower string) (privileged, takesValue bool) {
	switch lower {
	case "-y", "--yolo":
		return true, false
	case "--approval-mode", "--approvalmode",
		"--allowed-tools", "--allowedtools",
		"--include-directories", "--includedirectories":
		return true, true
	}
	for _, prefix := range []string{
		"--yolo=",
		"--approval-mode=", "--approvalmode=",
		"--allowed-tools=", "--allowedtools=",
		"--include-directories=", "--includedirectories=",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true, false
		}
	}
	return false, false
}

// sanitizeGeminiACPEnv applies a strip list to the inherited environment
// before forwarding it to the Gemini ACP child. Mirrors
// sanitizeCodexAppServerEnv: CLAUDECODE / CLAUDE_* / CODEX_IDE_* would tell
// downstream tooling it is running embedded inside another IDE / agent,
// which is not true here. GEMINI_* / GOOGLE_* / PATH / HOME etc. are
// forwarded by omission so the child's shell environment stays intact and
// the user's `gemini` OAuth creds under ~/.gemini remain discoverable.
func sanitizeGeminiACPEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		if strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "CODEX_IDE_") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
