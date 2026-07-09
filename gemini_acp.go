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
// raw single-turn `session_start` path — sanitizeGeminiACPEnv strips the
// embedded-IDE markers (including GEMINI_CLI_IDE_*), pins
// GEMINI_CLI_TRUST_WORKSPACE so a committed workspace `.env` cannot trust the
// workspace (the env twin of `--skip-trust`) while preserving the operator's
// own headless trust opt-in from the agent's launch environment, pins
// GEMINI_CLI_IDE_WORKSPACE_PATH empty so the same `.env` cannot add outside
// directories (the env twin of `--include-directories`), and pins Gemini
// config-location and sandbox env vars so workspace dotenv cannot redirect
// settings or enable repo-controlled sandbox startup.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// A signed gemini_acp_send can create or load a session rooted at a
	// different directory inside the workspace than the launch cwd Start
	// screened. Re-screen that send-time cwd's `.gemini/settings.json` before
	// the frame reaches the child so a subdirectory's project config cannot
	// re-enable mcpServers / hooks / auto-approval past the Start-time guard.
	screenSetupCwd:        screenGeminiWorkspaceSettings,
	rejectSetupMCPServers: true,
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
		// The argv/env sanitizers close the flag/env routes to auto-approval
		// and containment escape, but `gemini --experimental-acp` still loads
		// the workspace's own `cwd/.gemini/settings.json`, which Gemini
		// documents as overriding user settings. A repo-local settings file
		// could re-enable auto-approval (general.defaultApprovalMode /
		// autoAccept / yolo) or add directories outside WorkspaceRoot
		// (context.includeDirectories), bypassing the same guards the argv
		// sanitizer enforces. Screen it before spawning and fail the session
		// start if it grants privilege — surfaced as a gemini_acp_error, the
		// same way an unusable binary is.
		if err := screenGeminiWorkspaceSettings(cwd, opts.WorkspaceRoot); err != nil {
			return acpSpawnPlan{}, err
		}
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
//   - `--experimental-acp` / `--acp` — owned by buildGeminiACPArgs; duplicates
//     plus yargs alias/equals/negated forms are dropped so caller extras cannot
//     override ACP mode.
//   - `-p`/`--prompt` and `-i`/`--prompt-interactive` (kebab/camel/equals
//     forms) — any of
//     these switches gemini OUT of ACP stdio mode into a one-shot headless /
//     interactive run, which would break the JSON-RPC handshake the
//     orchestrator is about to drive. The single-turn path (session_start →
//     buildGeminiInteractiveArgs) is where prompts belong.
//   - the POSIX `--` end-of-options delimiter — everything after it would be
//     read as a positional prompt, which switches modes exactly like `-p`.
//   - bare positionals — gemini reads any non-flag positional as a prompt
//     word too (the undelimited spelling of the `--` tail above), so a legacy
//     caller reusing the `session_start` shape with the prompt in `Args[0]`
//     would flip the child out of ACP mode. A bare token survives only as the
//     value of the immediately preceding kept separate-token flag (e.g.
//     `--model gemini-3-pro`); anywhere else it is dropped. Failing closed is
//     safe because a flag's value always directly follows its flag.
//   - privilege-escalating flags (see geminiACPPrivilegedFlag): `-y`/`--yolo`
//     and `--approval-mode` auto-approve tool calls, bypassing the
//     orchestrator-driven `session/request_permission` flow this manager
//     relies on for approvals; `--allowed-tools` pre-approves named tools the
//     same way; `--policy`/`--admin-policy` load extra policy-engine files
//     whose `allow` rules auto-approve tools without confirmation — the
//     policy engine is the documented successor to `--allowed-tools`, so
//     both routes to it are closed; `--skip-trust` trusts the current
//     workspace for the session without prompting, which re-enables the
//     project `.gemini/settings.json` (tool auto-acceptance / project
//     policies / include-directory behavior) this sanitizer is otherwise
//     blocking; `--sandbox`/`-s` enables or steers Gemini's full-process
//     startup sandbox before ACP approvals (and `--no-sandbox` can disable an
//     operator-provided sandbox); `--delete-session` runs Gemini's saved-session
//     deletion flow instead of starting a chat; `--worktree`/`-w` starts the
//     child inside a fresh git worktree under `.gemini/worktrees/` — a path
//     outside WorkspaceRoot — escaping the start/send cwd containment and the
//     workspace-settings screen the same way `--include-directories` does;
//     `--include-directories` adds extra workspace directories, routing around
//     the WorkspaceRoot cwd containment Start enforces. Unlike
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

		if geminiACPOwnedTransportFlag(lower) {
			continue
		}

		// Prompt flags take a value in separate-token form; consume it too.
		if lower == "-p" || lower == "--prompt" || lower == "-i" ||
			lower == "--prompt-interactive" || lower == "--promptinteractive" {
			if i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--prompt=") || strings.HasPrefix(lower, "-p=") ||
			strings.HasPrefix(lower, "--prompt-interactive=") ||
			strings.HasPrefix(lower, "--promptinteractive=") ||
			strings.HasPrefix(lower, "-i=") {
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

		// Grouped short-option clusters: gemini's yargs parser expands a
		// cluster like `-yd` into individual boolean flags (`-y -d`), so the
		// exact-match checks above (which only see the whole `-yd` token)
		// would let a smuggled `-y` (yolo) or `-p`/`-i` (prompt, mode-break)
		// through. Drop the whole cluster when it carries one of those
		// letters — failing closed is safe here because a legitimate caller
		// can always pass the flags separately.
		if geminiACPShortFlagClusterHasPrivilege(lower) {
			continue
		}

		// POSIX end-of-options delimiter: everything after it is a
		// positional prompt, which flips gemini out of ACP mode exactly like
		// `-p`. Drop the delimiter AND everything after it — a legitimate
		// flag value never follows a bare `--`, so nothing usable is lost.
		if a == "--" {
			break
		}

		// Bare positionals are prompt words (rationale in the function doc):
		// keep one only as the value of a KNOWN value-taking flag kept
		// immediately before it (e.g. `--model gemini-3-pro`); otherwise drop
		// it so it cannot flip the child out of ACP mode. A boolean flag such
		// as `--debug` does NOT consume its following token — gemini reads that
		// token as the positional `query`, leaving ACP mode and breaking the
		// JSON-RPC handshake — so a bare token after a boolean (or any flag not
		// on the known-value list) must be dropped, not preserved. Comparing
		// against the last KEPT token (not the previous raw token) also fails
		// closed when the preceding flag was itself dropped.
		if !geminiACPFlagShaped(a) {
			if len(cleaned) == 0 || !geminiACPKnownSeparateValueFlag(cleaned[len(cleaned)-1]) {
				continue
			}
		}

		cleaned = append(cleaned, a)
	}
	return cleaned
}

// geminiACPFlagShaped reports whether a token is flag-shaped: a leading dash
// followed by at least one more character. The lone `-` stdin marker is a
// positional, not a flag.
func geminiACPFlagShaped(tok string) bool {
	return len(tok) >= 2 && tok[0] == '-'
}

// geminiACPSeparateValueFlagShaped reports whether a kept token could be
// consuming a following separate-token value: flag-shaped with no inline
// `=value` (an equals-form flag already carries its value, so a bare token
// after it is a positional).
func geminiACPSeparateValueFlagShaped(tok string) bool {
	return geminiACPFlagShaped(tok) && !strings.Contains(tok, "=")
}

// geminiACPOwnedTransportFlag reports whether lower is an ACP transport flag
// spelling owned by buildGeminiACPArgs. Gemini's yargs parser maps
// `--experimental-acp` to `experimentalAcp`, accepts camelCase, may expose
// `--acp` as an alias, and honors yargs' `--no-*` boolean negation syntax, so
// every long-form spelling and inline value is stripped before caller extras
// are appended after the built-in `--experimental-acp`.
func geminiACPOwnedTransportFlag(lower string) bool {
	switch lower {
	case "--experimental-acp", "--experimentalacp", "--acp",
		"--no-experimental-acp", "--no-experimentalacp", "--noexperimentalacp",
		"--no-acp", "--noacp":
		return true
	}
	for _, prefix := range []string{
		"--experimental-acp=",
		"--experimentalacp=",
		"--acp=",
		"--no-experimental-acp=",
		"--no-experimentalacp=",
		"--noexperimentalacp=",
		"--no-acp=",
		"--noacp=",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// geminiACPKnownValueFlags is the set of gemini flags that consume the NEXT
// argument as their value, keyed by the spelling-agnostic form
// geminiNormalizeFlag produces (leading + interior dashes stripped, lowercased)
// so a flag's short (`-m`), kebab-case (`--output-format`) and camelCase
// (`--outputFormat`) spellings — all of which gemini's yargs parser accepts —
// collapse to one key. Only a flag on this list legitimises keeping a bare
// following token as its value; after a boolean or unknown flag that token is a
// positional prompt (see sanitizeGeminiACPExtraArgs).
//
// Mirrors buildGeminiInteractiveArgs' valuedFlags, minus the entries that never
// survive to the positional check here: the mode-breaking prompt flags
// (`-i`/`-p`, dropped earlier) and the privileged value-taking flags
// (`--approval-mode` / `--allowed-tools` / `--policy` / `--admin-policy` /
// `--delete-session` / `--worktree` / `--include-directories`, dropped as
// privileged). Their values must NOT be preserved either, so they are
// deliberately absent.
var geminiACPKnownValueFlags = map[string]bool{
	"m": true, "model": true,
	"o": true, "outputformat": true,
	"e": true, "extensions": true,
	"r": true, "resume": true,
	"allowedmcpservernames": true,
}

// geminiNormalizeFlag reduces a flag token to a spelling-agnostic key: strip
// leading dashes, strip any interior dashes, lowercase. `--output-format`,
// `--outputFormat` and (separately) `-o` map to comparable keys, matching how
// gemini's yargs parser treats the kebab/camel/short spellings as equivalent.
// Returns "" for a non-flag token.
func geminiNormalizeFlag(tok string) string {
	if !geminiACPFlagShaped(tok) {
		return ""
	}
	t := strings.TrimLeft(tok, "-")
	t = strings.ReplaceAll(t, "-", "")
	return strings.ToLower(t)
}

// geminiACPKnownSeparateValueFlag reports whether tok is a separate-token
// (no inline `=value`) gemini flag KNOWN to consume the next argument as its
// value. Only such a flag lets sanitizeGeminiACPExtraArgs keep a following bare
// token; after a boolean flag (e.g. `--debug`) or any flag not on the
// known-value list the bare token is a positional prompt that would break ACP
// mode and must be dropped.
func geminiACPKnownSeparateValueFlag(tok string) bool {
	if !geminiACPSeparateValueFlagShaped(tok) {
		return false
	}
	return geminiACPKnownValueFlags[geminiNormalizeFlag(tok)]
}

// geminiACPPrivilegedFlag classifies a lowercased extra-arg token against the
// deny list of privilege-escalating gemini flags: `-y`/`--yolo` and
// `--approval-mode` bypass per-tool approvals, `--allowed-tools` pre-approves
// named tools, `--policy`/`--admin-policy` load extra policy files whose
// `allow` rules auto-approve tools the same way, `--skip-trust` trusts the
// workspace without prompting (re-enabling project settings/auto-acceptance),
// `--sandbox`/`-s` controls Gemini's startup sandbox before ACP approvals (and
// `--no-sandbox` can disable an operator-provided sandbox), `--delete-session`
// runs Gemini's saved-session deletion flow instead of ACP chat,
// `--worktree`/`-w` starts the CLI inside a fresh git worktree under
// `.gemini/worktrees/` (a path outside WorkspaceRoot, escaping the start/send
// cwd containment and the workspace-settings screen), and
// `--include-directories` escapes the WorkspaceRoot containment.
// gemini's yargs parser also accepts camelCase spellings of every kebab-case
// flag, so both spellings are matched (ToLower collapses camelCase to the
// dash-less form). takesValue is true only for the separate-token value form;
// equals-form tokens carry their value inline and consume nothing extra.
func geminiACPPrivilegedFlag(lower string) (privileged, takesValue bool) {
	switch lower {
	case "-y", "--yolo",
		"--skip-trust", "--skiptrust",
		"-s", "--sandbox", "--no-sandbox", "--nosandbox":
		return true, false
	case "--approval-mode", "--approvalmode",
		"--allowed-tools", "--allowedtools",
		"--policy",
		"--admin-policy", "--adminpolicy",
		"--delete-session", "--deletesession",
		"-w", "--worktree",
		"--include-directories", "--includedirectories":
		return true, true
	}
	for _, prefix := range []string{
		"--yolo=",
		"--skip-trust=", "--skiptrust=",
		"-s=", "--sandbox=", "--no-sandbox=", "--nosandbox=",
		"--approval-mode=", "--approvalmode=",
		"--allowed-tools=", "--allowedtools=",
		"--policy=",
		"--admin-policy=", "--adminpolicy=",
		"--delete-session=", "--deletesession=",
		"-w=", "--worktree=",
		"--include-directories=", "--includedirectories=",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true, false
		}
	}
	return false, false
}

// geminiACPDangerousShortFlags are the single-letter gemini short options that
// must never survive onto the ACP argv, even hidden inside a grouped cluster:
// `y` enables YOLO/auto-approve (bypassing the orchestrator-driven
// session/request_permission flow), `p`/`i` switch gemini out of ACP stdio
// mode into a one-shot prompt run (breaking the JSON-RPC handshake), `s`
// controls Gemini's full-process sandbox startup, and `w` (worktree) starts
// the child inside a `.gemini/worktrees/` path outside the WorkspaceRoot
// containment. gemini's yargs parser expands `-yd` into `-y -d`, so the
// exact-match deny lists above do not catch clustered spellings.
var geminiACPDangerousShortFlags = map[rune]bool{'y': true, 'p': true, 'i': true, 's': true, 'w': true}

// geminiACPShortFlagClusterHasPrivilege reports whether a lowercased token is a
// short-option cluster (a single leading dash followed by one or more ASCII
// letters) that carries one of geminiACPDangerousShortFlags. gemini's
// yargs-parser accepts a short-option equals value (e.g. `-y=true`, and by
// extension a clustered `-yd=true`), so an `=value` suffix is stripped before
// the letter check — this catches the short yolo equals form that the
// exact-match deny list in geminiACPPrivilegedFlag (which only sees `-y=…` if
// it were listed) would otherwise miss.
//
// yargs-parser also accepts an ATTACHED short-option value: a non-word
// character immediately after a short option (`letters[j+1].match(/\W/)`)
// makes the remainder that option's value — so `-w/tmp/foo` enables worktree
// mode and `-p/tmp/foo` flips out of ACP mode, both with a `/`-led attached
// value the old `[a-z]`-only screen let through. The scan therefore walks the
// leading contiguous letters (each a real short flag in the cluster) and
// returns true the moment it hits a dangerous one; the first non-letter marks
// the start of the preceding option's attached value, and since no dangerous
// letter was seen before it the token is benign. Long flags (`--…`) and the
// `--` delimiter return false so `--model gemini-3-pro` style extras still
// pass through.
func geminiACPShortFlagClusterHasPrivilege(lower string) bool {
	if len(lower) < 2 || lower[0] != '-' || lower[1] == '-' {
		return false
	}
	body := lower[1:]
	// yargs accepts a short-option equals value (`-y=true`); the letters
	// before the `=` are still expanded as boolean short flags, so screen them.
	if eq := strings.IndexByte(body, '='); eq >= 0 {
		body = body[:eq]
	}
	if body == "" {
		return false
	}
	for _, r := range body {
		if geminiACPDangerousShortFlags[r] {
			return true
		}
		if r < 'a' || r > 'z' {
			// Non-letter: yargs reads it and everything after as the attached
			// value of the preceding short option. None of the letters scanned
			// so far were dangerous, so nothing privileged reaches the argv.
			return false
		}
	}
	return false
}

// geminiTrustWorkspaceEnvVar is the environment variable Gemini documents as
// trusting the current workspace for the session — the env-file twin of the
// stripped `--skip-trust` flag. See
// https://geminicli.com/docs/reference/configuration/.
const geminiTrustWorkspaceEnvVar = "GEMINI_CLI_TRUST_WORKSPACE"

// geminiIDEWorkspacePathEnvVar is the variable Gemini's IDE companion sets to
// tell the CLI which IDE workspace folders it runs inside. Gemini's config
// loader reads it unconditionally and pushes those folders into
// includeDirectories — the env twin of the stripped `--include-directories`
// flag and the screened `context.includeDirectories` setting, so it is another
// route to grant the child access to directories outside WorkspaceRoot.
const geminiIDEWorkspacePathEnvVar = "GEMINI_CLI_IDE_WORKSPACE_PATH"

// geminiACPConfigPathEnvVars are Gemini environment variables that redirect
// where the CLI reads persistent config from. A workspace `.env` must not be
// able to set them before Gemini loads settings: that would move user/system
// settings, defaults, or folder-trust data to repo-controlled files outside
// the workspace settings screen below. Preserving an already-inherited value is
// intentional: it was supplied by the operator's launch environment, not by
// the repo-local dotenv Gemini has not loaded yet.
var geminiACPConfigPathEnvVars = []string{
	"GEMINI_CLI_HOME",
	"GEMINI_CLI_SYSTEM_SETTINGS_PATH",
	"GEMINI_CLI_SYSTEM_DEFAULTS_PATH",
	"GEMINI_CLI_TRUSTED_FOLDERS_PATH",
}

// geminiACPSandboxEnvVars are Gemini sandbox controls that must be present
// before Gemini loads workspace dotenv. Inherited operator values are
// preserved; absent vars are pinned empty so a repo-local `.env` cannot enable
// sandbox startup, declare that Gemini is already sandboxed, select a
// repo-controlled proxy/mount/flags, or build `.gemini/sandbox.Dockerfile`
// before the ACP permission loop exists.
var geminiACPSandboxEnvVars = []string{
	"SANDBOX",
	"GEMINI_SANDBOX",
	"GEMINI_SANDBOX_PROXY_COMMAND",
	"BUILD_SANDBOX",
	"BUILD_SANDBOX_FLAGS",
	"SANDBOX_FLAGS",
	"SANDBOX_MOUNTS",
	"SEATBELT_PROFILE",
}

// sanitizeGeminiACPEnv applies a strip list to the inherited environment
// before forwarding it to the Gemini ACP child, then pins the workspace-trust
// variable off. Mirrors sanitizeCodexAppServerEnv for the IDE markers:
// CLAUDECODE / CLAUDE_* / CODEX_IDE_* would tell downstream tooling it is
// running embedded inside another IDE / agent, which is not true here.
// GEMINI_* / GOOGLE_* / PATH / HOME etc. are forwarded by omission so the
// child's shell environment stays intact and the user's `gemini` OAuth creds
// under ~/.gemini remain discoverable.
//
// GEMINI_CLI_TRUST_WORKSPACE is handled specially: stripping the `--skip-trust`
// flag on the argv is not enough because Gemini auto-loads a `.env` from the
// workspace (and its ancestors) and documents GEMINI_CLI_TRUST_WORKSPACE=true
// as trusting the workspace for the session — the same trust bypass that
// re-enables the project `.gemini/settings.json` / tool auto-acceptance the ACP
// guards otherwise block. A repo could therefore commit a `.env` carrying that
// variable and get the bypass for free. So the variable is always pinned in the
// child's process env — Gemini's dotenv loader does not override a variable
// already present in the environment, so the workspace `.env` can never set it.
//
// WHICH value gets pinned depends on the inherited environment. Gemini's trust
// check honors only the exact value `true` (checkPathTrust: `=== 'true'`); any
// other value behaves like unset, falling through to the user's Folder Trust
// config (~/.gemini/trustedFolders.json). When the AGENT'S OWN environment —
// set by the operator who launched the terminal agent, not reachable from any
// repo — carries exactly GEMINI_CLI_TRUST_WORKSPACE=true, that value is
// preserved: it is Gemini's documented non-interactive trust bypass for
// headless runs, and without it a Folder-Trust-enabled user whose workspace is
// not in trustedFolders.json would hit FatalUntrustedWorkspaceError before the
// ACP handshake with no non-interactive way out (the workspace-settings screen
// above keeps guarding the privileged settings regardless of trust). Any other
// inherited value is normalized to the inert `false`.
//
// GEMINI_CLI_IDE_* gets the same treatment for the same reason: this manager
// is not Gemini's IDE companion, and GEMINI_CLI_IDE_WORKSPACE_PATH in
// particular feeds includeDirectories directly (Gemini's config loader pushes
// its folders into the context), sidestepping the `--include-directories`
// strip and the `context.includeDirectories` settings screen. Drop any
// inherited GEMINI_CLI_IDE_* value and pin the workspace-path variable to
// empty — present-but-empty blocks the dotenv override just like the trust
// pin, and Gemini ignores an empty value.
//
// Gemini also lets environment variables override config file locations:
// GEMINI_CLI_HOME moves the user's `.gemini` directory, while
// GEMINI_CLI_SYSTEM_SETTINGS_PATH / GEMINI_CLI_SYSTEM_DEFAULTS_PATH and
// GEMINI_CLI_TRUSTED_FOLDERS_PATH point at specific config files. If a
// workspace `.env` can set those, it can route Gemini to repo-controlled JSON
// carrying mcpServers, hooks, policy files, or trust state that the workspace
// settings screen never inspected. Pin each variable before dotenv runs:
// inherited operator values survive, absent variables become empty so Gemini
// falls back to its default paths and dotenv cannot fill them in later.
//
// The sandbox env family follows the same "preserve operator, pin repo" rule:
// inherited values from the terminal agent's launch environment remain
// available, but absent entries are pinned empty before Gemini loads project
// dotenv. That blocks a committed `.env` from turning on full-process
// sandboxing (`GEMINI_SANDBOX`), selecting a project-controlled profile/
// proxy/mount/flags, or building `.gemini/sandbox.Dockerfile`
// (`BUILD_SANDBOX`) during CLI startup before ACP approvals can run.
func sanitizeGeminiACPEnv(env []string) []string {
	filtered := make([]string, 0, len(env)+2+len(geminiACPConfigPathEnvVars)+len(geminiACPSandboxEnvVars))
	inheritedTrust := false
	configPathValues := make(map[string]string, len(geminiACPConfigPathEnvVars))
	sandboxValues := make(map[string]string, len(geminiACPSandboxEnvVars))
	for _, e := range env {
		upper := strings.ToUpper(e)
		if key, value, ok := strings.Cut(e, "="); ok {
			if canonical, found := geminiACPConfigPathEnvVar(strings.ToUpper(key)); found {
				configPathValues[canonical] = value
				continue
			}
			if canonical, found := geminiACPSandboxEnvVar(strings.ToUpper(key)); found {
				sandboxValues[canonical] = value
				continue
			}
		}
		if strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "CODEX_IDE_") ||
			strings.HasPrefix(upper, "GEMINI_CLI_IDE_") ||
			strings.HasPrefix(upper, geminiTrustWorkspaceEnvVar+"=") {
			// Only the exact value `true` means anything to Gemini's trust
			// check; remember it so the operator's own headless trust opt-in
			// survives the pin below.
			if strings.HasPrefix(upper, geminiTrustWorkspaceEnvVar+"=") &&
				e[strings.IndexByte(e, '=')+1:] == "true" {
				inheritedTrust = true
			}
			continue
		}
		filtered = append(filtered, e)
	}
	// Pin workspace trust so a committed `.env` cannot set it: `true` only
	// when the operator's own launch environment opted in, inert `false`
	// otherwise.
	trustValue := "false"
	if inheritedTrust {
		trustValue = "true"
	}
	filtered = append(filtered, geminiTrustWorkspaceEnvVar+"="+trustValue)
	// Pin the IDE workspace path empty so a committed `.env` cannot add
	// directories outside WorkspaceRoot through the IDE-companion route.
	filtered = append(filtered, geminiIDEWorkspacePathEnvVar+"=")
	// Pin config-location overrides so workspace dotenv cannot redirect
	// Gemini to repo-controlled settings/defaults/trust files.
	for _, name := range geminiACPConfigPathEnvVars {
		filtered = append(filtered, name+"="+configPathValues[name])
	}
	// Pin sandbox controls so workspace dotenv cannot enable or steer sandbox
	// startup unless the operator already supplied those values.
	for _, name := range geminiACPSandboxEnvVars {
		value, ok := sandboxValues[name]
		if !ok {
			value = geminiACPSandboxDefaultEnvValue(name)
		}
		filtered = append(filtered, name+"="+value)
	}
	return filtered
}

func geminiACPConfigPathEnvVar(upperKey string) (string, bool) {
	return geminiACPListedEnvVar(upperKey, geminiACPConfigPathEnvVars)
}

func geminiACPSandboxEnvVar(upperKey string) (string, bool) {
	return geminiACPListedEnvVar(upperKey, geminiACPSandboxEnvVars)
}

func geminiACPListedEnvVar(upperKey string, names []string) (string, bool) {
	for _, name := range names {
		if upperKey == name {
			return name, true
		}
	}
	return "", false
}

func geminiACPSandboxDefaultEnvValue(name string) string {
	if name == "SEATBELT_PROFILE" {
		// Match Gemini's default macOS sandbox profile while still blocking a
		// workspace dotenv value from selecting `.gemini/sandbox-macos-*.sb`.
		return "permissive-open"
	}
	return ""
}

/* --------------------------------------------------------------------------
   workspace settings screen
   -------------------------------------------------------------------------- */

// screenGeminiWorkspaceSettings inspects the workspace-scoped
// `.gemini/settings.json` files (if any) BEFORE spawning the ACP child and
// rejects the session start when one of them would grant a privilege the argv
// sanitizer strips on the command line. Gemini resolves its "workspace"
// settings from the PROJECT ROOT — the nearest ancestor of cwd it recognises
// as the project (git root etc.), not cwd itself — so when a chat session runs
// from a subdirectory the privileged file lives above cwd. Screening only
// `cwd/.gemini/settings.json` would miss a repo-root file that Gemini still
// applies. So walk every directory from cwd up to and including WorkspaceRoot
// (the containment root cwd is guaranteed to sit inside) and — because Gemini
// resolves project settings from the enclosing Git project root, which can sit
// ABOVE WorkspaceRoot when the workspace is a subdirectory of a larger repo —
// keep climbing to that Git root, screening each `.gemini/settings.json` on the
// path; the first privileged one blocks the start (see geminiSettingsScreenDirs
// for the exact bounds). When WorkspaceRoot is empty (no containment
// configured) or cwd is not lexically under it, fall back to screening cwd
// alone. The upward climb deliberately stops at the Git project root and never
// enters the user's home directory, so it never reaches the user's global
// `~/.gemini/settings.json`, which is the user's own choice and out of scope
// for this repo-supplied-settings screen. Gemini loads workspace settings and
// lets them override user settings, so a repo-local settings file is an
// equivalent route to the `--yolo` / `--approval-mode` / `--include-directories`
// flags sanitizeGeminiACPExtraArgs already blocks: it can re-enable tool
// auto-approval (bypassing the orchestrator's ACP `session/request_permission`
// flow), pre-approve named tools so they skip the confirmation dialog
// (`tools.allowed` / legacy `allowedTools` — the settings twin of the
// stripped `--allowed-tools` flag), load extra policy files whose `allow`
// rules auto-approve tools (policyPaths / adminPolicyPaths — the settings
// twin of the stripped `--policy`/`--admin-policy` flags), declare workspace
// MCP servers that Gemini spawns/connects during discovery — stdio transport
// runs repo-controlled local code before any approval, so ANY non-empty
// `mcpServers` block is treated as privileged, not just `mcpServers.*.trust:
// true` — name workspace executables gemini runs for MCP startup
// (`mcp.serverCommand`), custom-tool discovery and calls
// (`tools.discoveryCommand` / `tools.callCommand`, legacy flat
// `toolDiscoveryCommand` / `toolCallCommand`) or attach command hooks to
// lifecycle events (`hooks`), all the same pre-approval code-execution
// vector as `mcpServers` — or add context directories outside the
// WorkspaceRoot containment `start` enforces.
//
// A missing or unreadable file is NOT fatal (absence is the common case), and
// neither is a file that stays unparseable even after we normalise the JSONC
// extensions Gemini's own loader tolerates (`//` + `/* */` comments and
// trailing commas — see stripJSONCForParsing): gemini would ignore/repair a
// genuinely malformed file rather than apply a privileged setting, so failing
// the spawn on parse errors would diverge from the CLI and turn a cosmetic typo
// in a repo into a chat outage. Only a parseable file that positively declares
// a privilege-escalating value blocks the start. The scan walks the whole JSON
// tree so both the nested (`general.defaultApprovalMode`, `context.includeDirectories`)
// and any legacy flat spellings are caught regardless of nesting.
func screenGeminiWorkspaceSettings(cwd, workspaceRoot string) error {
	for _, dir := range geminiSettingsScreenDirs(cwd, workspaceRoot) {
		if err := screenGeminiSettingsFile(filepath.Join(dir, ".gemini", "settings.json")); err != nil {
			return err
		}
	}
	return nil
}

// geminiSettingsScreenDirs returns the directories whose `.gemini/settings.json`
// must be screened for a session rooted at cwd: cwd first, then each ancestor
// up to and including workspaceRoot, then — when workspaceRoot is itself a
// subdirectory of a larger Git project — each further ancestor up to and
// including that project root. This mirrors how far up Gemini itself searches
// for the project settings that override user/default settings: Gemini treats
// the enclosing Git project root as the place for `.gemini/settings.json` and
// applies it when running from that project OR any of its subdirectories, so a
// repo-root settings file just above the configured workspace is still loaded
// by Gemini and must be screened too.
//
// When workspaceRoot is empty, or cwd does not sit lexically under it, only cwd
// is screened. The upward climb stops at the Git project root (mirroring how
// far Gemini looks) and never enters the user's home directory, so the global
// `~/.gemini/settings.json` — the user's own out-of-scope choice — is left
// alone even when the workspace lives directly under home.
func geminiSettingsScreenDirs(cwd, workspaceRoot string) []string {
	// Resolve symlinks on both sides before any containment math so this walk
	// agrees with the symlink-resolved containment check acpManager.start
	// already enforced. A raw lexical Rel mis-classifies a contained cwd as
	// outside the root when the two are spelled through different symlinks
	// (e.g. workspaceRoot=/link/repo, cwd=/real/repo/sub): start accepts the
	// session, but the lexical check here would return only cwd and skip the
	// workspace/Git-root `.gemini/settings.json` that Gemini still loads,
	// letting a repo re-enable yolo/MCP/tool settings past the guard. Fall back
	// to a lexical Clean when resolution fails so an unresolvable path still
	// screens cwd alone.
	cwd = resolveScreenPath(cwd)
	if workspaceRoot == "" {
		return dropGeminiHomeDir([]string{cwd})
	}
	root := resolveScreenPath(workspaceRoot)
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// cwd is not under workspaceRoot (unexpected given containment) —
		// screen cwd alone rather than walking an unrelated ancestor chain.
		return dropGeminiHomeDir([]string{cwd})
	}
	dirs := []string{cwd}
	for dir := cwd; dir != root; {
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the filesystem root without matching workspaceRoot
		}
		dir = parent
		dirs = append(dirs, dir)
	}
	// Extend the screen above workspaceRoot to the enclosing Git project root,
	// which Gemini still loads project settings from when the workspace is a
	// subdirectory of a larger repo.
	return dropGeminiHomeDir(append(dirs, geminiProjectRootAncestors(root)...))
}

// dropGeminiHomeDir removes the user's home directory from a screen list. When
// the configured workspace is left at (or is itself) the home directory — the
// default WorkingDirectory when none is set — the upward walk includes home and
// would screen `~/.gemini/settings.json`, the user's own global config, causing
// every ACP chat to be rejected for users who set global mcpServers /
// allowedTools / a non-default approval mode. Gemini's project-settings search
// and the geminiProjectRootAncestors climb both deliberately exclude the global
// config; this keeps the direct walk consistent with that boundary. Home is
// resolved through the same symlink resolution as the screen dirs so the guard
// still fires when home is reached through a symlink.
func dropGeminiHomeDir(dirs []string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return dirs
	}
	home = resolveScreenPath(home)
	out := dirs[:0]
	for _, dir := range dirs {
		if dir != home {
			out = append(out, dir)
		}
	}
	return out
}

// resolveScreenPath returns the symlink-resolved absolute form of p, matching
// the resolution acpManager.start uses for containment (so the two views of a
// contained path can't diverge). It reuses resolveCwdForContainment, which
// resolves the existing prefix even when a trailing component doesn't exist,
// and falls back to a lexical Clean when even that can't resolve so callers
// always get a usable path.
func resolveScreenPath(p string) string {
	if resolved, err := resolveCwdForContainment(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// geminiProjectRootAncestors returns the directories strictly above start, up
// to and including the nearest ancestor that looks like a Git project root (an
// ancestor containing a `.git` entry — a directory for a normal clone or a file
// for a worktree/submodule). It returns nil when start is itself a project root
// (Gemini looks no higher), when no project root is found before the filesystem
// root, or when the climb would reach the user's home directory. Bounding the
// climb at the Git root matches how far Gemini searches for project
// `.gemini/settings.json`; the home-dir guard keeps the user's global config
// out of this repo-supplied screen.
func geminiProjectRootAncestors(start string) []string {
	if hasGitEntry(start) {
		// start already IS the project root — Gemini resolves no higher.
		return nil
	}
	// Resolve the home dir the same way the ancestor chain is resolved so the
	// guard below still fires when home (or start) is reached through a symlink.
	home := ""
	if h, err := os.UserHomeDir(); err == nil {
		home = resolveScreenPath(h)
	}
	var ancestors []string
	for dir := start; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil // filesystem root reached without finding a project root
		}
		dir = parent
		if home != "" && dir == home {
			return nil // reached the user's home dir — leave the global config alone
		}
		ancestors = append(ancestors, dir)
		if hasGitEntry(dir) {
			return ancestors // Git project root reached (inclusive) — Gemini stops here
		}
	}
}

// hasGitEntry reports whether dir contains a `.git` entry, the marker Gemini
// uses to identify a project root. A normal clone has a `.git` directory; a
// worktree or submodule has a `.git` file — os.Stat matches either.
func hasGitEntry(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// screenGeminiSettingsFile screens a single `.gemini/settings.json` path,
// returning a non-nil error only when the file parses AND positively declares a
// privilege-escalating setting. A missing/unreadable/malformed file is not
// fatal (see the screenGeminiWorkspaceSettings doc for the rationale).
func screenGeminiSettingsFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		// No workspace settings (or unreadable) → nothing to screen.
		return nil
	}
	var parsed any
	if json.Unmarshal(stripJSONCForParsing(raw), &parsed) != nil {
		// Even after tolerating the JSONC extensions Gemini's settings loader
		// accepts (`//` + `/* */` comments via strip-json-comments, and
		// trailing commas), the file is not valid JSON. Gemini would
		// ignore/repair it rather than apply a privileged setting, so don't
		// fail the spawn on it here.
		return nil
	}
	if reason := geminiSettingsPrivilegeReason(parsed); reason != "" {
		return fmt.Errorf(
			"workspace %s grants %s, which would bypass the ACP approval/containment guards; "+
				"remove it or run Gemini chat from a workspace without it", path, reason)
	}
	return nil
}

// geminiSettingsPrivilegeReason walks a decoded settings JSON value and returns
// a human-readable reason string for the first privilege-escalating setting it
// finds, or "" when none is present. Key matching is case-insensitive and
// ignores `-`/`_` separators so kebab-case, camelCase and snake_case spellings
// all match.
func geminiSettingsPrivilegeReason(v any) string {
	return geminiSettingsPrivilegeReasonUnder(v, "")
}

// geminiSettingsPrivilegeReasonUnder is the parent-aware recursion behind
// geminiSettingsPrivilegeReason. `parent` is the normalised key of the object
// currently being walked (empty at the root), which lets the ambiguous
// `allowed` key be classified by context: `tools.allowed` pre-approves tool
// calls (privileged) while `mcp.allowed` merely restricts which MCP servers are
// enabled (a benign allowlist). The unambiguous legacy-flat `allowedTools`
// spelling stays position-blind.
func geminiSettingsPrivilegeReasonUnder(v any, parent string) string {
	switch node := v.(type) {
	case map[string]any:
		for k, val := range node {
			key := geminiSettingsKey(k)
			switch key {
			case "defaultapprovalmode", "approvalmode":
				if s, ok := val.(string); ok {
					mode := geminiSettingsKey(s)
					// `default`/`manual` prompt per action and `plan` is
					// read-only (no edits/tool runs), so none of them bypass
					// the ACP `session/request_permission` flow; only
					// `auto_edit`/`yolo` auto-approve actions. See
					// https://geminicli.com/docs/cli/settings/.
					if mode != "" && mode != "default" && mode != "manual" && mode != "plan" {
						return fmt.Sprintf("auto-approval mode %q", s)
					}
				}
			case "sandbox":
				if parent == "tools" {
					if reason := geminiSandboxSettingReason(k, val); reason != "" {
						return reason
					}
				}
			case "sandboxallowedpaths":
				if parent == "tools" {
					switch paths := val.(type) {
					case string:
						if strings.TrimSpace(paths) != "" {
							return fmt.Sprintf("extra sandbox paths (%s)", k)
						}
					case []any:
						if len(paths) > 0 {
							return fmt.Sprintf("extra sandbox paths (%s)", k)
						}
					}
				}
			case "sandboxnetworkaccess":
				if parent == "tools" {
					if b, ok := val.(bool); ok && b {
						return fmt.Sprintf("sandbox network access (%s)", k)
					}
				}
			case "autoaccept", "yolo":
				if b, ok := val.(bool); ok && b {
					return fmt.Sprintf("tool auto-acceptance (%s)", k)
				}
			case "includedirectories":
				if arr, ok := val.([]any); ok && len(arr) > 0 {
					return "extra context directories (includeDirectories)"
				}
			case "allowed":
				// `tools.allowed` pre-approves the named tools, skipping the
				// confirmation dialog — the settings twin of the stripped
				// `--allowed-tools` flag. But `allowed` is NOT unique to tools:
				// `mcp.allowed` is a benign allowlist of permitted MCP server
				// NAMES (a restriction, not an auto-approval), and other blocks
				// spell similar allowlists. So only treat a bare `allowed` as
				// privileged when it sits directly under `tools`; elsewhere it
				// is a restrictive allowlist and passes.
				if parent == "tools" {
					if reason := geminiAllowedToolsReason(k, val); reason != "" {
						return reason
					}
				}
			case "allowedtools":
				// The legacy flat `allowedTools` spelling is unambiguous —
				// always pre-approved tools, regardless of nesting.
				if reason := geminiAllowedToolsReason(k, val); reason != "" {
					return reason
				}
			case "policypaths", "adminpolicypaths":
				switch paths := val.(type) {
				case string:
					if strings.TrimSpace(paths) != "" {
						return fmt.Sprintf("extra policy files (%s)", k)
					}
				case []any:
					if len(paths) > 0 {
						return fmt.Sprintf("extra policy files (%s)", k)
					}
				}
			case "mcpservers":
				// A workspace `mcpServers` block makes Gemini spawn/connect
				// those servers during MCP discovery — stdio transport launches
				// a local subprocess — which runs repo-controlled code BEFORE
				// any ACP `session/request_permission` approval is involved, and
				// regardless of whether the server is marked `trust: true`. So a
				// repo can execute arbitrary local code just by shipping a
				// settings file; treat ANY non-empty workspace mcpServers as
				// privileged. The `trust: true` case below is the stricter
				// subset (auto-approving that server's tool CALLS) and is kept
				// for defense in depth / oddly-nested spellings.
				if m, ok := val.(map[string]any); ok && len(m) > 0 {
					return "workspace-defined MCP servers (mcpServers)"
				}
			case "trust":
				// mcpServers.<name>.trust: true bypasses per-call tool
				// confirmations for that server. The walk is positional-blind,
				// but no benign Gemini setting spells a bare boolean `trust`.
				if b, ok := val.(bool); ok && b {
					return "a trusted MCP server (trust: true)"
				}
			case "servercommand":
				if parent == "mcp" {
					// mcp.serverCommand names a workspace-controlled MCP
					// process Gemini starts during MCP discovery, before any
					// ACP permission request can be surfaced.
					if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
						return fmt.Sprintf("a workspace MCP command (%s)", k)
					}
				}
			case "discoverycommand", "tooldiscoverycommand",
				"callcommand", "toolcallcommand":
				// tools.discoveryCommand / tools.callCommand (and the legacy
				// flat toolDiscoveryCommand / toolCallCommand spellings) name a
				// workspace-controlled executable gemini runs at custom-tool
				// discovery and on every custom-tool call — repo-controlled
				// code execution before the ACP `session/request_permission`
				// flow is involved, the same vector as `mcpServers`.
				if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
					return fmt.Sprintf("a workspace tool command (%s)", k)
				}
			case "hooks":
				// Gemini hook settings attach workspace-defined shell commands
				// to lifecycle events (PreToolUse etc.), which run outside the
				// ACP approval flow. Any non-empty hooks block is treated as
				// privileged, same policy as `mcpServers`. No benign Gemini
				// setting spells a non-empty `hooks` container.
				switch hooks := val.(type) {
				case map[string]any:
					if len(hooks) > 0 {
						return "workspace-defined hooks (hooks)"
					}
				case []any:
					if len(hooks) > 0 {
						return "workspace-defined hooks (hooks)"
					}
				}
			}
			if reason := geminiSettingsPrivilegeReasonUnder(val, key); reason != "" {
				return reason
			}
		}
	case []any:
		// Array elements inherit the current parent key (e.g. items under a
		// `tools.allowed` array are still "under" tools).
		for _, item := range node {
			if reason := geminiSettingsPrivilegeReasonUnder(item, parent); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func geminiSandboxSettingReason(rawKey string, val any) string {
	switch sandbox := val.(type) {
	case bool:
		if sandbox {
			return fmt.Sprintf("full-process sandbox startup (%s)", rawKey)
		}
	case string:
		switch geminiSettingsKey(sandbox) {
		case "", "false", "0":
			return ""
		default:
			return fmt.Sprintf("full-process sandbox startup (%s)", rawKey)
		}
	case map[string]any:
		if geminiSandboxConfigEnabled(sandbox) {
			return fmt.Sprintf("full-process sandbox startup (%s)", rawKey)
		}
	}
	return ""
}

func geminiSandboxConfigEnabled(config map[string]any) bool {
	for k, enabled := range config {
		if geminiSettingsKey(k) != "enabled" {
			continue
		}
		switch v := enabled.(type) {
		case bool:
			return v
		case string:
			switch geminiSettingsKey(v) {
			case "", "false", "0":
				return false
			default:
				return true
			}
		case float64:
			return v != 0
		default:
			return enabled != nil
		}
	}
	return false
}

// geminiAllowedToolsReason returns a privilege reason for a `tools.allowed` /
// legacy `allowedTools` value when it names at least one pre-approved tool,
// accepting both the string and array spellings; it returns "" for an empty or
// blank value.
func geminiAllowedToolsReason(rawKey string, val any) string {
	switch tools := val.(type) {
	case string:
		if strings.TrimSpace(tools) != "" {
			return fmt.Sprintf("pre-approved tools (%s)", rawKey)
		}
	case []any:
		if len(tools) > 0 {
			return fmt.Sprintf("pre-approved tools (%s)", rawKey)
		}
	}
	return ""
}

// stripJSONCForParsing rewrites the JSONC extensions Gemini's settings loader
// tolerates into plain JSON so a file that is valid to Gemini also parses here.
// Gemini imports strip-json-comments and permits trailing commas, so a workspace
// settings file may legitimately carry `//` line comments, `/* */` block
// comments, or a trailing comma before `}`/`]`. Without normalising them first,
// strict encoding/json would reject such a file, screenGeminiWorkspaceSettings
// would treat it as "malformed → allow", and a privileged value like
// `defaultApprovalMode: "yolo"` sitting next to a comment would slip past the
// screen while Gemini still applied it. Comments/commas inside string literals
// (respecting `\` escapes) are left untouched.
func stripJSONCForParsing(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	inString := false
	inLineComment := false
	inBlockComment := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
				out = append(out, c)
			}
		case inBlockComment:
			if c == '*' && i+1 < len(raw) && raw[i+1] == '/' {
				inBlockComment = false
				i++
			}
		case inString:
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(raw) && raw[i+1] == '/':
			inLineComment = true
			i++
		case c == '/' && i+1 < len(raw) && raw[i+1] == '*':
			inBlockComment = true
			i++
		default:
			out = append(out, c)
		}
	}
	return stripJSONTrailingCommas(out)
}

// stripJSONTrailingCommas drops a comma that is followed only by whitespace and
// then a closing `}` or `]`, matching Gemini's trailing-comma tolerance. Commas
// inside string literals are preserved. Input is expected to already be
// comment-free (stripJSONCForParsing calls it last).
func stripJSONTrailingCommas(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString := false
	escaped := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // drop the trailing comma
			}
		}
		out = append(out, c)
	}
	return out
}

// geminiSettingsKey normalises a settings key or enum value for comparison:
// lowercased with `-` and `_` separators removed.
func geminiSettingsKey(s string) string {
	lower := strings.ToLower(s)
	lower = strings.ReplaceAll(lower, "-", "")
	lower = strings.ReplaceAll(lower, "_", "")
	return lower
}
