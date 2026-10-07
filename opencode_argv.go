// opencode_argv.go — the ONE OpenCode invocation contract: the shape ladder
// every non-interactive run is built from, the caller-argv normalization, the
// single shim-aware launcher, and the legacy signed `session_start` smoke
// contract. Mirrors grok_argv.go for Grok.
//
// Why a dedicated file: OpenCode had TWO argv builders — buildOpenCodeNativeArgs
// (direct chat, prompt on stdin) and buildOpenCodeInteractiveArgs (terminal
// session_start, prompt as a trailing positional) — shaping the same CLI
// differently, and TWO bare exec.Command spawn sites. A fix in one path missed
// the other, and neither could start the `.cmd` / `.bat` npm shim that
// `npm install -g` puts on PATH on Windows: CreateProcess cannot launch a batch
// file, the exact failure Grok and Codex already route around through
// cliSmokeShimCommand. That is why the Windows OpenCode maintenance smoke
// failed identically before and after a CLI update and never returned a marker.
//
// The contract, stated once:
//   - `run` and `--format json` are FORCED on every non-diagnostic invocation.
//   - `--session <id>` is added only on the direct path, only above
//     openCodeNativeMinVersion, and is the one droppable token in the ladder.
//   - Manager-owned flags a caller sent (openCodeStrippedFlags) are removed
//     with their values; every other TOKEN a caller sent is forwarded AS-IS,
//     because nothing tells us whether an unlearned option is boolean or
//     consumes the next token — dropping can eat the prompt and keeping can
//     turn a value into prompt text. An unlearned option's SEPARATE operand
//     therefore stays with the prompt rather than being guessed onto argv; the
//     unambiguous `--flag=value` form is forwarded intact. See
//     openCodeForwardedValuedFlags for why that asymmetry is the safe one.
//   - The prompt is NEVER on argv. Two transports: a one-shot stages it in a
//     0600 file handed to the child as stdin (openCodeLaunch.Stdin); a legacy
//     session returns it as stdinPrompt and the session manager writes it
//     through the live stdin pipe.
//   - Diagnostic invocations (`--version`, `--help`, `models`, `auth …`) pass
//     through verbatim: reshaping one into a `run` would spend a turn nobody
//     asked for.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

/* --------------------------------------------------------------------------
   Shape ladder
   -------------------------------------------------------------------------- */

// openCodeRunShape is one candidate non-interactive invocation. The ID is the
// only thing published (cliSmokeResult.ArgvShapeID) — it names a ladder entry,
// never the flags it produces.
type openCodeRunShape struct {
	ID string
	// Session adds `--session <id>` when the caller has a native id to resume.
	// The direct chat path prefers this rung; the maintenance probe never uses
	// it (it has no conversation to resume, and a resume flag a build rejects
	// would fuse a pre-inference option error into the probe's verdict).
	Session bool
}

const (
	openCodeRunShapeIDSession = "opencode-run-json-session-v1"
	openCodeRunShapeIDPlain   = "opencode-run-json-v1"
)

var (
	// openCodeRunShapeResume is the preferred rung on the direct path.
	openCodeRunShapeResume = openCodeRunShape{ID: openCodeRunShapeIDSession, Session: true}
	// openCodeRunShapeNoSession is the no-resume rung: what the maintenance
	// probe always spawns, and what the direct path falls back to when the
	// binary is below the resume floor or rejected `--session`.
	openCodeRunShapeNoSession = openCodeRunShape{ID: openCodeRunShapeIDPlain}
)

// buildOpenCodeRunArgs builds the manager-owned argv for one non-interactive
// OpenCode run. It is the ONLY producer of a child argv for this CLI: the
// direct chat turn, the legacy session_start smoke and the `__cli_smoke__`
// probe all come through here, so the forced tokens cannot drift between them.
//
// The prompt is never a parameter — a builder that cannot receive the prompt
// cannot put it in a process listing.
func buildOpenCodeRunArgs(shape openCodeRunShape, nativeSessionID string) []string {
	args := []string{"run", "--format", "json"}
	if shape.Session && nativeSessionID != "" {
		// Exact-id resume only — never --continue, which resumes whatever the
		// user last ran globally, including in their own local TUI.
		args = append(args, "--session", nativeSessionID)
	}
	return args
}

// openCodeShapeForResume picks the rung for a direct-chat turn: the resume rung
// when this turn has a usable native id, the no-resume rung otherwise.
func openCodeShapeForResume(nativeSessionID string) openCodeRunShape {
	if nativeSessionID != "" {
		return openCodeRunShapeResume
	}
	return openCodeRunShapeNoSession
}

/* --------------------------------------------------------------------------
   Caller argv policy
   -------------------------------------------------------------------------- */

// openCodeStrippedFlags are caller-supplied flags the manager always removes.
//
//   - --format / --print-logs would flip the child out of the JSON mode this
//     driver's parser depends on, producing an unrenderable turn.
//   - --session / --continue / --fork would re-point the conversation at
//     something other than the session the cloud reserved, letting a caller
//     read or extend a chat that is not theirs.
//
// The manager owns these positions; everything it does NOT own is forwarded.
var openCodeStrippedFlags = map[string]bool{
	"--format":     true,
	"-f":           true,
	"--session":    true,
	"-s":           true,
	"--continue":   true,
	"-c":           true,
	"--fork":       true,
	"--print-logs": true,
}

// openCodeValuedStrippedFlags are the stripped flags that consume the NEXT argv
// token as their value, so removing the flag must also remove its value (a
// dangling `json` would otherwise land as a positional prompt token).
var openCodeValuedStrippedFlags = map[string]bool{
	"--format":  true,
	"-f":        true,
	"--session": true,
	"-s":        true,
	"--fork":    true,
}

// openCodeForwardedValuedFlags are caller flags the manager passes through that
// take the NEXT argv token as their value. Without this the value would be
// mistaken for prompt text and reordered behind the flags.
//
// A flag NOT listed here is still forwarded — see buildOpenCodeInteractiveArgs
// — it simply cannot have a SEPARATE operand re-ordered ahead of the prompt,
// because nothing tells us whether an unlearned option is boolean or consumes
// the next token. Both guesses are unsound, and they are not symmetric: moving
// that token onto argv when the option turns out to be boolean splits the prompt
// across argv and stdin AND puts prompt text — including the maintenance smoke's
// marker nonce — into a process listing any local user can read, the exact
// exposure this transport exists to close. Leaving it with the prompt instead
// costs a bare option OpenCode answers with a missing-operand rejection, which
// classifies as flag_rejected: a precise, actionable diagnostic. Two cheap
// remedies, so nothing is stuck waiting on this file: send the unambiguous
// `--flag=value` inline form, which is forwarded intact whether or not the flag
// is known, or add a one-line entry here once OpenCode ships the option.
var openCodeForwardedValuedFlags = map[string]bool{
	"--model": true,
	"-m":      true,
	// `opencode run --variant <level>` is the provider-specific reasoning
	// effort; the orchestrator composes it beside --model (shared-constants
	// CLI_AGENT_EFFORT_FLAG_BY_ID). Without this entry the level would be read
	// as prompt text and the run would silently use the default effort.
	"--variant": true,
	"--agent":   true,
	"--port":    true,
	"--host":    true,
}

// openCodeFlagNameAt splits a caller token into its flag NAME and whether it
// carried an inline `=value`. One spelling rule, so the strip and forward
// tables are always consulted with the same key.
func openCodeFlagNameAt(arg string) (name string, inlineValue bool) {
	if idx := strings.Index(arg, "="); idx > 0 {
		return arg[:idx], true
	}
	return arg, false
}

// openCodeStrippedCallerFlagAt reports whether args[i] is a manager-owned flag
// that must be removed, and the index the caller should resume from (skipping a
// separate value when the flag consumes one).
//
// ONE strip policy, and the single answer to "which flags may a caller set?" —
// the question terminal-service's own normalizeOpenCodeArgs answers on the other
// end of the wire, so the two ends stay comparable. The direct and session paths
// used to carry a hand-copied loop each, which is exactly how two answers to
// that question drift apart, and a flag stripped on one path but forwarded on
// the other re-points a conversation at a chat the caller does not own.
func openCodeStrippedCallerFlagAt(args []string, i int) (resume int, stripped bool) {
	name, inlineValue := openCodeFlagNameAt(args[i])
	if !openCodeStrippedFlags[name] {
		return i, false
	}
	// `--flag=value` carries its value inline; the separate-value form consumes
	// the next token as well (a dangling `json` would otherwise land as a
	// positional prompt token).
	if !inlineValue && openCodeValuedStrippedFlags[name] {
		return i + 1, true
	}
	return i, true
}

// openCodeDiagnosticTokens are invocations that ask OpenCode for information
// instead of running a prompt. Reshaping any of these into `run` would burn a
// model call the caller never asked for.
var openCodeDiagnosticTokens = map[string]bool{
	"--version": true, "-version": true, "-v": true,
	"--help": true, "-help": true, "-h": true,
	"auth": true, "models": true, "upgrade": true, "serve": true,
	"github": true, "mcp": true, "agent": true, "stats": true,
	// `session` (list / delete) and `export` read the session store. They spend
	// nothing, so reshaping either into a `run` would burn a turn — and the
	// usage reconciliation (cliagent_usage_opencode_store.go) runs both, so
	// they must also never arm a usage run floor.
	"session": true, "export": true,
}

// isOpenCodeDiagnosticInvocation reports an invocation OpenCode answers with
// information rather than a model run. Like agy, OpenCode pre-scans its whole
// command line for `--help` / `--version`, so those are matched wherever they
// appear; subcommands only count as the FIRST token.
func isOpenCodeDiagnosticInvocation(args []string) bool {
	for i, a := range args {
		lowered := strings.ToLower(strings.TrimSpace(a))
		if name, _, ok := strings.Cut(lowered, "="); ok {
			lowered = name
		}
		switch lowered {
		case "--version", "-version", "-v", "--help", "-help", "-h":
			return true
		}
		if i == 0 && openCodeDiagnosticTokens[lowered] && !strings.HasPrefix(lowered, "-") {
			return true
		}
	}
	return false
}

// isOpenCodeSynthesizedRun reports whether args match the forced non-interactive
// `run --format json …` shape buildOpenCodeRunArgs produces. A promptless argv
// still satisfies it — the prompt left argv when it moved to stdin.
func isOpenCodeSynthesizedRun(args []string) bool {
	return len(args) >= 3 && args[0] == "run" && args[1] == "--format" && args[2] == "json"
}

// buildOpenCodeInteractiveArgs shapes a one-shot `opencode` invocation for the
// legacy session_start / PTY path and returns (argv, stdinPrompt).
//
// `run --format json` is ALWAYS forced and any caller token that would re-enter
// the interactive TUI — a bare `opencode`, a second `run`, or a `--format`
// override — is stripped. A bare `opencode` on a headless remote session starts
// the TUI, which emits escape-sequence noise and never exits: the identical
// trap grok_acp.go documents for bare `grok`.
//
// The prompt is returned SEPARATELY and never placed on argv. It used to be a
// trailing positional, which made this path subject to the Windows
// CreateProcess ~32KB command-line ceiling and put the maintenance smoke's
// marker nonce in a process listing any local user can read. The session
// manager writes it through the child's stdin pipe and closes after the write
// (shouldCloseStdinAfterStart → hasPrompt).
//
// Diagnostic invocations are returned verbatim with no prompt.
func buildOpenCodeInteractiveArgs(args []string) (cliArgs []string, stdinPrompt string) {
	if isOpenCodeDiagnosticInvocation(args) {
		return args, ""
	}

	forwarded := make([]string, 0, len(args))
	var prompt []string
	for i := 0; i < len(args); i++ {
		if resume, stripped := openCodeStrippedCallerFlagAt(args, i); stripped {
			i = resume
			continue
		}
		a := args[i]
		name, inlineValue := openCodeFlagNameAt(a)
		if a == "run" && len(forwarded) == 0 && len(prompt) == 0 {
			// Already forced below; a second `run` parses as prompt text.
			continue
		}
		if strings.HasPrefix(a, "-") {
			forwarded = append(forwarded, a)
			// Flags the manager forwards that consume the next token.
			if openCodeForwardedValuedFlags[name] && !inlineValue && i+1 < len(args) {
				i++
				forwarded = append(forwarded, args[i])
			}
			continue
		}
		prompt = append(prompt, a)
	}

	return append(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), forwarded...),
		strings.Join(prompt, " ")
}

/* --------------------------------------------------------------------------
   The one launcher
   -------------------------------------------------------------------------- */

// openCodeLaunch is everything one OpenCode child is spawned with. Bundled so
// the launcher (and the smoke's exec seam) has a single stable signature and
// the Windows shim route sees the whole launch at once.
type openCodeLaunch struct {
	Path string
	Args []string
	Env  []string
	Dir  string
	// PromptFile is the staged 0600 prompt path whose open handle is Stdin.
	// Carried separately (never on argv) so a test double can recover what the
	// child was asked without the prompt or its marker ever reaching a process
	// listing.
	PromptFile string
	// Stdin, when set, is handed to the child. Left nil by the session path,
	// which wires its own pipe after the child is built, and by the diagnostic
	// probes, which send nothing.
	Stdin *os.File
	// Maintenance marks a maintenance smoke child (the `__cli_smoke__` probe
	// and the legacy session_start smoke). newOpenCodeCmd applies
	// openCodeMaintenanceEnvPins to it. Ordinary runs never set it: pinning
	// self-update off for them would change the user's update behaviour.
	Maintenance bool
}

// openCodeMaintenanceEnvPins are the env values every maintenance smoke child
// runs with.
//
//   - OPENCODE_DISABLE_AUTOUPDATE: a pre-update smoke that updates the CLI in
//     the background replaces its own install mid-turn (on Windows an npm
//     update renames the running package aside), and the harness's
//     "before / after" comparison stops meaning anything.
//   - OPENCODE_DISABLE_TERMINAL_TITLE: a terminal-title OSC write on stdout
//     sits in front of the JSON frame stream the smoke parses.
var openCodeMaintenanceEnvPins = [][2]string{
	{"OPENCODE_DISABLE_AUTOUPDATE", "true"},
	{"OPENCODE_DISABLE_TERMINAL_TITLE", "true"},
}

// withOpenCodeMaintenanceEnv returns env with every maintenance pin set,
// replacing an inherited value rather than appending a duplicate. A nil env
// starts from os.Environ(), which is what the child would otherwise inherit.
func withOpenCodeMaintenanceEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	for _, pin := range openCodeMaintenanceEnvPins {
		env = setEnvVar(env, pin[0], pin[1])
	}
	return env
}

// errOpenCodeShimUnrenderable is the typed refusal a Windows `.cmd` / `.bat`
// launch gets when openCodeShimScript will not render the invocation (see its
// character policy). On Windows this is TERMINAL: falling back to a direct
// `.cmd` spawn would fail in CreateProcess anyway, so failing closed — as
// launch_error, a diagnostic distinct from "the child ran and produced no
// envelope" — is the honest answer.
var errOpenCodeShimUnrenderable = fmt.Errorf("opencode launch cannot be rendered for a Windows shim")

// newOpenCodeCmd builds the child for one OpenCode invocation: a native binary
// directly, a Windows `.cmd` / `.bat` npm shim through cmd.exe. Hidden on every
// route — these are background children of a tray app with no console of its
// own.
//
// Every OpenCode spawn goes through here (direct chat turn, legacy session,
// readiness probes, version probe, maintenance smoke), which is the point: the
// shim route used to be reachable only from Grok's and Codex's smokes, so an
// npm-installed `opencode.cmd` could not be started at all.
func newOpenCodeCmd(ctx context.Context, launch openCodeLaunch) (*exec.Cmd, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if launch.Maintenance {
		// Before the shim route builds its env, so the pins reach the Node
		// grandchild behind cmd.exe as well as a native binary.
		launch.Env = withOpenCodeMaintenanceEnv(launch.Env)
	}
	var cmd *exec.Cmd
	if isWindowsShimPath(launch.Path) {
		shimmed, ok := openCodeShimCommand(ctx, launch)
		if !ok {
			return nil, errOpenCodeShimUnrenderable
		}
		cmd = shimmed
	} else {
		cmd = exec.CommandContext(ctx, launch.Path, launch.Args...)
		cmd.Env = launch.Env
		cmd.Dir = launch.Dir
		hideWindow(cmd)
	}
	if launch.Stdin != nil {
		cmd.Stdin = launch.Stdin
	}
	return cmd, nil
}

/* --------------------------------------------------------------------------
   Legacy signed session_start smoke contract
   -------------------------------------------------------------------------- */

// openCodeMaintenanceSmokeControlArg is an AI Expedite-only in-process control
// derived from the signed session_start contract. It is consumed before argv
// shaping and must never be forwarded to the CLI.
const openCodeMaintenanceSmokeControlArg = "--aiexpedite-opencode-maintenance-smoke"

// openCodeMaintenanceSmokePromptPrefix is the fixed instruction the signed
// session_start smoke prompt must start with; everything after it is the marker
// the model is asked to echo. Byte-identical to Grok's, so both CLIs are asked
// the same thing on the same transport.
const openCodeMaintenanceSmokePromptPrefix = grokMaintenanceSmokePromptPrefix

// openCodeSmokeWireRequests are the frozen signed `session_start` argvs, each
// followed by exactly ONE trailing prompt token. Two shapes are accepted for
// the rollout window:
//
//   - `run --pure --format json <prompt>` — what deployed publishers sign
//     today. `--pure` is not a flag current OpenCode builds accept, which is
//     why the deployed smoke exits during option parsing and never echoes a
//     marker.
//   - `run --format json <prompt>` — the same request once `--pure` is
//     dropped, so a publisher can stop sending it without a flag day.
//
// The child argv is NEVER this list: StartSession derives it from the ladder
// (buildOpenCodeRunArgs), so `--pure` is consumed by validation and never
// reaches OpenCode. Exactly the split that keeps Grok's empty `--tools`
// operand off its child's command line.
var openCodeSmokeWireRequests = [][]string{
	{"run", "--pure", "--format", "json"},
	{"run", "--format", "json"},
}

// openCodeSmokeReservedTokens is the vocabulary that makes an argv recognizable
// as maintenance traffic rather than an ordinary session. Recognition is broad
// (any layout of these tokens plus a trailing marker prompt is promoted, so a
// mutation fails CLOSED in StartSession instead of quietly running as an
// ordinary session) and BOUNDED by this set: an argv carrying anything outside
// it — `--model x`, a different subcommand — is never maintenance traffic and
// passes through untouched.
// It is DERIVED from openCodeSmokeWireRequests rather than re-listed, so a third
// frozen shape (or a token leaving one) cannot leave a stale copy behind — a
// stale vocabulary would stop recognising the deployed envelope and let its
// tokens reach the ordinary session path, which is the bug this feature exists
// to fix. Same discipline as computeGrokSmokeRetryableFlags.
var openCodeSmokeReservedTokens = computeOpenCodeSmokeReservedTokens()

func computeOpenCodeSmokeReservedTokens() map[string]bool {
	reserved := map[string]bool{}
	for _, wire := range openCodeSmokeWireRequests {
		for _, token := range wire {
			reserved[token] = true
		}
	}
	return reserved
}

// extractOpenCodeMaintenanceSmokeControl removes the internal control token and
// reports whether it was present. It can never reach a child.
func extractOpenCodeMaintenanceSmokeControl(args []string) ([]string, bool) {
	requested := false
	cleaned := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == openCodeMaintenanceSmokeControlArg {
			requested = true
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return cleaned, requested
}

// validOpenCodeMaintenanceSmokePrompt accepts the prefix plus a non-empty,
// CR/LF-free suffix — and imposes NO grammar on that suffix.
//
// The marker is OPAQUE on this transport. The publisher's marker grammar is not
// visible from this repo, and Grok's own two transports already disagree (its
// direct probe mints `AIEXPEDITE_GROK_SMOKE_OK_` + 8 lowercase hex; the legacy
// fixture carries `AIEXPEDITE_GROK_SMOKE_MARKER_` + 6 uppercase hex).
// Requiring the probe's grammar here would leave the REAL deployed request
// unrecognized and let `--pure` reach the ordinary session path. The probe's
// own marker satisfies this rule as a subset; it is never a precondition.
// Byte-for-byte the rule validGrokMaintenanceSmokePrompt applies.
func validOpenCodeMaintenanceSmokePrompt(prompt string) bool {
	marker := strings.TrimPrefix(prompt, openCodeMaintenanceSmokePromptPrefix)
	return marker != prompt && strings.TrimSpace(marker) != "" && !strings.ContainsAny(marker, "\r\n")
}

// openCodeMaintenanceSmokeRequest recognises the updater's reserved marker
// prompt envelope. Args are part of commandMsg's HMAC payload, so deriving the
// private control bit here keeps it authenticated without a new wire field
// older publishers cannot sign.
//
// True when the LAST token is a valid marker prompt AND every other token is
// drawn only from openCodeSmokeReservedTokens. Both frozen shapes and near-miss
// mutations (reordered, duplicated, one token missing) are therefore promoted
// and then judged by validateOpenCodeSmokeRequest's exact check. An ordinary
// session can collide only by sending nothing but reserved tokens plus that
// exact sentence — the same exposure Grok accepts.
func openCodeMaintenanceSmokeRequest(args []string) bool {
	cleaned, _ := extractOpenCodeMaintenanceSmokeControl(args)
	if len(cleaned) < 2 {
		return false
	}
	if !validOpenCodeMaintenanceSmokePrompt(cleaned[len(cleaned)-1]) {
		return false
	}
	for _, arg := range cleaned[:len(cleaned)-1] {
		if !openCodeSmokeReservedTokens[arg] {
			return false
		}
	}
	return true
}

// validateOpenCodeSmokeRequest accepts only a frozen wire argv plus its marker
// prompt, and returns that prompt. The error is deliberately FIXED text: a
// rejected token is never echoed, because an option value can carry a
// credential or a private path and StartSession's error is published.
func validateOpenCodeSmokeRequest(args []string) (prompt string, err error) {
	if len(args) < 2 {
		return "", errOpenCodeSmokeRequestContract
	}
	prompt = args[len(args)-1]
	wire := args[:len(args)-1]
	for _, want := range openCodeSmokeWireRequests {
		if !argvEqual(wire, want) {
			continue
		}
		if !validOpenCodeMaintenanceSmokePrompt(prompt) {
			return "", errOpenCodeSmokeRequestContract
		}
		return prompt, nil
	}
	return "", errOpenCodeSmokeRequestContract
}

var errOpenCodeSmokeRequestContract = fmt.Errorf(
	"opencode maintenance smoke must use the exact non-interactive single-turn contract")

/* --------------------------------------------------------------------------
   Shared event predicates
   -------------------------------------------------------------------------- */

// isOpenCodeTerminalEventType reports whether a `--format json` event closes a
// turn. Lifted out of detectCLITerminalEvent so BOTH transports (the streamed
// session and the maintenance probe) agree on turn completion.
//
// Matched by suffix because the exact type name has moved across releases; an
// unrecognised terminal event is harmless (the process-exit path still
// flushes), a false positive is not, so `error` is excluded.
//
// `step_finish` (the `run --format json` formatter's spelling, `step-finish` /
// `step.finish` in other builds) closes EVERY model step, including the
// intermediate ones that end in a tool call. finishReason is the event's
// `part.reason` (or top-level `reason`): a tool-call reason means the turn
// continues, so that frame is not terminal. An absent reason is treated as
// terminal, matching how `step.completed` has always been read.
func isOpenCodeTerminalEventType(eventType, finishReason string) bool {
	lowered := strings.ToLower(strings.TrimSpace(eventType))
	if lowered == "" || strings.Contains(lowered, "error") {
		return false
	}
	if strings.Contains(strings.ToLower(finishReason), "tool") {
		return false
	}
	return strings.HasSuffix(lowered, "completed") ||
		strings.HasSuffix(lowered, "done") ||
		strings.HasSuffix(lowered, "finish") ||
		lowered == "session.idle"
}

// openCodeEventFinishReason extracts the finish reason an OpenCode event carries
// in `part.reason` (the step_finish shape) or a top-level `reason`. Returns ""
// when neither is a string.
func openCodeEventFinishReason(event map[string]interface{}) string {
	if part, ok := event["part"].(map[string]interface{}); ok {
		if reason, ok := part["reason"].(string); ok && reason != "" {
			return reason
		}
	}
	reason, _ := event["reason"].(string)
	return reason
}

// isOpenCodeTerminalEventLine reports whether one `run --format json` stdout
// line is the turn's natural end. Shared by the native stream tap and the
// maintenance probe so the two transports cannot disagree about what "the turn
// finished" looks like.
func isOpenCodeTerminalEventLine(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return false
	}
	var event map[string]interface{}
	if json.Unmarshal([]byte(line), &event) != nil {
		return false
	}
	eventType, _ := event["type"].(string)
	return isOpenCodeTerminalEventType(eventType, openCodeEventFinishReason(event))
}

// openCodeOptionRejectionText reports whether already-lowercased CLI text is an
// OPTION-PARSING rejection — a refusal that happens before any inference, and
// therefore the one failure class a caller may act on without having paid for a
// turn. Shared by the smoke classifier (flag_rejected / framing_rejected) and
// the direct path's `--session` replay recovery.
//
// The text is read only to pick between locally authored constants; not one
// byte of it travels further.
func openCodeOptionRejectionText(lower string) bool {
	for _, needle := range []string{
		"unknown option",
		"unknown flag",
		"unknown argument",
		"unexpected argument",
		"unrecognized",
		"unrecognised",
		"invalid option",
		"invalid flag",
		"unknown or unexpected option",
		"wasn't expected",
		"a value is required",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
