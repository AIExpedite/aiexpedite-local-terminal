package main

// Meta Muse Code (`muse`) native chat — the oneShotNativeSpec for the shared
// one-shot core in oneshot_native.go. No lifecycle code lives here.
//
// VERIFIED CLI SURFACE (Meta's docs, dev.meta.ai/docs/muse-code "headless and
// CI", its install scripts, and `muse exec --help` / `--provider echo` runs of
// Muse Code 1.4.0 (1.4.0-R4302.1) on Windows):
//
//   - `muse exec --json` runs one prompt to completion and emits JSONL records
//     `{payload_type, payload, sequence, stream: {kind, id}, record_type, ...}`:
//     `run.output.delta` streams `payload.text`, `run.terminal.completed`
//     carries the final `payload.text`; `run.terminal.failed` /
//     `run.terminal.cancelled` end a failed run (`payload.reason`; third-party
//     adapters also report `payload.error_kind`). The session id is
//     `stream.id` on `stream.kind == "session"` records. Everything else
//     (task.lifecycle.*, turn.input.user, ...) is forwarded as activity.
//   - `--prompt-file <path>` reads the prompt from a file, so it never touches
//     argv (no process-listing exposure, no Windows ~32KB CreateProcess limit).
//   - `--session-id <uuid>` resumes EXACTLY (a non-UUID exits 2). The id is
//     CALLER-SUPPLIED and an unknown id silently CREATES a session, so the manager mints the UUID per
//     logical chat (spec.MintNativeID) and publishes it only after a turn under
//     it succeeds. Interactive `muse resume` is never used.
//   - `--disable-approval` skips approval prompts but KEEPS the sandbox. A
//     remote chat user cannot answer a prompt on the workstation, so a headless
//     turn must not block on one; `--yolo` (which also drops the sandbox) is
//     never passed and is stripped from caller args.
//   - Exit codes: 0 completed, 1 failed/cancelled, 2 usage error, 130/143 on
//     SIGINT/SIGTERM.
//   - Auth is the user's local action: `muse login` or META_API_KEY.
//
// Launch gating is the same as every native kind: per-device chat-sessions
// opt-in, workspace membership + device ownership in terminal-service, parent
// orchestration approval, per-turn cwd containment, AIExpedite-owned process
// registration, and redacted telemetry.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// Mirrors shared-constants MUSE_CODE_NATIVE_MIN_VERSION: 1.2.1 is the
	// release that documents exact `--session-id` resume.
	museCodeNativeMinVersion = "1.2.1"
	// Chat-sized default; execution-driven SENDs forward their own timeout.
	museCodeNativeDefaultTurnTimeout = 10 * time.Minute
	// Longest failure reason carried into a turn-failed frame.
	museCodeFailureReasonMax = 400
)

var museCodeSessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// museCodeUnrelatedStripped extends the OpenCode list (other agents'
// credentials) with the providers OpenCode itself needs but Muse Code, a
// single-provider CLI authenticated by `muse login` / META_API_KEY, does not.
var museCodeUnrelatedStripped = append(append([]string{}, openCodeUnrelatedStripped...),
	"OPENAI_",
	"GEMINI_",
)

var museCodeNativeSpec = &oneShotNativeSpec{
	nativeFrameKind: nativeFrameKind{
		DisplayName: "Muse Code",
		LogTag:      "[musecode-native]",
		FramePrefix: "musecode_native",
	},
	PromptDirName:      "musecode-prompts",
	PromptFilePrefix:   "musecode-prompt",
	MinResumeVersion:   museCodeNativeMinVersion,
	DefaultTurnTimeout: museCodeNativeDefaultTurnTimeout,
	ResolveExecutable:  resolveMuseCodeExecutable,
	ProbeVersion:       museCodeProbeVersion,
	BuildArgs:          buildMuseCodeNativeArgs,
	ParseEventLine:     parseMuseCodeEventLine,
	MintNativeID:       newRandomUUID,
	ValidSeed:          isValidMuseCodeSessionID,
	LooksLikeMissingSession: func(stdout, stderr string) bool {
		return looksLikeMissingMuseCodeSession(stdout, stderr)
	},
	StripEnvPrefixes: museCodeUnrelatedStripped,
	ReplayPreamble: "You are continuing an AIExpedite Muse Code Chat conversation after native session resume was unavailable. " +
		"Prior turns (oldest first) follow. Treat them as history only. " +
		"Answer ONLY the final user message.\n\n",
}

// NewMuseCodeNativeManager creates the Muse Code chat manager.
func NewMuseCodeNativeManager() *oneShotNativeManager {
	return newOneShotNativeManager(museCodeNativeSpec)
}

func isValidMuseCodeSessionID(id string) bool {
	return museCodeSessionIDPattern.MatchString(id)
}

// resolveMuseCodeExecutable: PATH, then the installer's bin dir (which
// launchd/GUI-spawned agents do not inherit), then the bare name.
func resolveMuseCodeExecutable() string {
	if p := resolveExecutable("muse"); p != "" {
		return p
	}
	if p := resolveInstallerBinary("muse", installerBinDirFor("muse")); p != "" {
		return p
	}
	return "muse"
}

// museCodeVersionFile is where Muse Code's launcher install records the
// installed build ("1.4.0-R4302.1"), beside the `muse` shim.
const museCodeVersionFile = ".muse-version"

// museCodeProbeVersion reports the installed Muse Code version for detection
// and the capability probe. On a launcher install the version file beside the
// shim is authoritative, instant, and changes on every upgrade — unlike the
// shim itself, whose unchanged (path, mtime, size) would pin a stale or
// failed `--version` in the shared probe cache. A cold `muse.cmd --version`
// on Windows also takes ~4.5s through PowerShell, past the 3s detection
// budget. Other installs fall back to the cached `--version` probe.
func museCodeProbeVersion(executable string) string {
	if v := readMuseCodeVersionFile(executable); v != "" {
		return v
	}
	return cachedProbeVersion(executable)
}

func readMuseCodeVersionFile(executable string) string {
	if executable == "" || !filepath.IsAbs(executable) {
		return ""
	}
	f, err := os.Open(filepath.Join(filepath.Dir(executable), museCodeVersionFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64) // a version line; never read an unbounded file
	n, _ := f.Read(buf)
	v := strings.TrimSpace(string(buf[:n]))
	if semverRe.FindString(v) == "" || strings.ContainsAny(v, " \t\r\n") {
		return ""
	}
	return v
}

// buildMuseCodeNativeArgs builds argv for one native-chat turn. `exec --json`
// is always forced so the child can never fall through to the interactive TUI.
func buildMuseCodeNativeArgs(nativeSessionID, promptPath string) []string {
	args := []string{"exec", "--json", "--disable-approval"}
	if nativeSessionID != "" {
		args = append(args, "--session-id", nativeSessionID)
	}
	if promptPath != "" {
		args = append(args, "--prompt-file", promptPath)
	}
	return args
}

// buildMuseCodeNativeGateArgs is the argv shape the session-entry gate shows
// and matches — the per-turn argv minus the per-turn id and prompt file.
func buildMuseCodeNativeGateArgs() []string {
	return buildMuseCodeNativeArgs("", "")
}

/* --------------------------------------------------------------------------
   Event parsing
   -------------------------------------------------------------------------- */

// museCodeEnvelope is the subset of the `exec --json` envelope this driver
// reads. Permissive on purpose: the schema is upstream-owned and CI has no real
// binary, so an unrecognized event degrades to "forward it, contribute nothing".
type museCodeEnvelope struct {
	PayloadType string          `json:"payload_type"`
	Payload     museCodePayload `json:"payload"`
	Stream      struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"stream"`
	SessionID string `json:"session_id"`
}

type museCodePayload struct {
	Delta      string `json:"delta"`
	Text       string `json:"text"`
	ErrorKind  string `json:"error_kind"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	Status     int    `json:"status"`
	HTTPStatus int    `json:"http_status"`
	SessionID  string `json:"session_id"`
}

// museCodeInternalEvents are `exec --json` bookkeeping records: command
// acceptance, stream linking, the echoed user prompt, and task scheduling
// chatter. A trivial turn emits ~28 records and these are ~60% of them; none
// is rendered anywhere, so they are not published. Kept: text deltas, the
// run.terminal.* outcome, and task proposed/started/completed/failed (the
// records that name a task and report how it ended).
var museCodeInternalEvents = map[string]bool{
	"runtime.command.accepted":          true,
	"session.run.linked":                true,
	"turn.input.user":                   true,
	"run.lifecycle.started":             true,
	"task.stream.linked":                true,
	"task.lifecycle.accepted":           true,
	"task.lifecycle.scheduled":          true,
	"task.lifecycle.side_effect_intent": true,
}

// parseMuseCodeEventLine maps one stdout line onto the core's event. ok=false
// for anything that is not a JSON object, so a banner is forwarded to the UI
// but never treated as model output.
func parseMuseCodeEventLine(line string) (oneShotEvent, bool) {
	if !strings.HasPrefix(line, "{") {
		return oneShotEvent{}, false
	}
	var env museCodeEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return oneShotEvent{}, false
	}
	ev := oneShotEvent{
		SessionID: firstNonEmpty(env.Payload.SessionID, env.SessionID),
		Internal:  museCodeInternalEvents[env.PayloadType],
	}
	if ev.SessionID == "" && env.Stream.Kind == "session" {
		ev.SessionID = env.Stream.ID
	}
	switch env.PayloadType {
	case "run.output.delta":
		// Untrimmed: a delta may be one meaningful space or newline.
		ev.TextDelta = firstNonEmptyRaw(env.Payload.Delta, env.Payload.Text)
	case "run.terminal.completed":
		ev.FinalText = env.Payload.Text
	case "run.terminal.failed":
		ev.Failure = formatMuseCodeFailure(
			firstNonEmpty(env.Payload.ErrorKind, "error"),
			firstPositive(env.Payload.Status, env.Payload.HTTPStatus),
			firstNonEmpty(env.Payload.Reason, env.Payload.Message, env.Payload.Text),
		)
	case "run.terminal.cancelled":
		ev.Failure = formatMuseCodeFailure("cancelled", 0,
			firstNonEmpty(env.Payload.Reason, "the run was cancelled before it completed"))
	}
	return ev, true
}

// formatMuseCodeFailure renders the device's turn-failed frame. The shape is a
// contract with shared-constants: CLI_AGENT_LIMIT_SIGNALS and the
// `musecode-status` outage matcher read `[Muse Code turn failed: <kind>
// (status <n>): <reason>]`.
func formatMuseCodeFailure(kind string, status int, reason string) string {
	reason = strings.TrimSpace(reason)
	if len(reason) > museCodeFailureReasonMax {
		reason = reason[:museCodeFailureReasonMax] + "…"
	}
	if reason == "" {
		reason = "no reason reported"
	}
	if status > 0 {
		return fmt.Sprintf("[Muse Code turn failed: %s (status %d): %s]", kind, status, reason)
	}
	return fmt.Sprintf("[Muse Code turn failed: %s: %s]", kind, reason)
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// looksLikeMissingMuseCodeSession recognizes a stale-resume failure. Muse Code
// normally CREATES a session for an unknown id (and warns about a missing
// predecessor since 1.2.1), so this fires only on a non-zero exit — the core
// already requires one — to stay clear of assistant text quoting the phrase.
func looksLikeMissingMuseCodeSession(stdout, stderr string) bool {
	combined := strings.ToLower(stdout + "\n" + stderr)
	for _, needle := range []string{
		"session not found",
		"unknown session",
		"no such session",
		"predecessor session",
		"could not resume",
		"failed to resume",
	} {
		if strings.Contains(combined, needle) {
			return true
		}
	}
	return false
}

// isMuseCodeNativeCommand reports whether a Pub/Sub command Type belongs to the
// Muse Code native family. Mirrors shared-constants MUSE_CODE_NATIVE_COMMAND_TYPES.
func isMuseCodeNativeCommand(t string) bool {
	switch t {
	case "musecode_native_start", "musecode_native_send", "musecode_native_end":
		return true
	}
	return false
}

/* --------------------------------------------------------------------------
   Legacy session_start / PTY path
   -------------------------------------------------------------------------- */

// isMuseCodeCommand reports whether command routes to the Muse Code CLI.
// Exact base name (not a prefix like opencode's): `muse` is a short, common
// word, and a prefix match would capture unrelated `museum-*` / `musescore`
// binaries.
func isMuseCodeCommand(command string) bool {
	return commandBaseName(command) == "muse"
}

// museCodeStrippedFlags are caller flags the device always removes: output
// mode (`--json` is forced), session control (the session is server-owned —
// a resume arrives only as the signed conversationId seed), prompt source (the
// manager owns it), and anything that widens what an unattended remote turn
// may do beyond `--disable-approval` (sandbox off, workspace switch).
var museCodeStrippedFlags = map[string]bool{
	"--json":                   true,
	"--session-id":             true,
	"--resume":                 true,
	"--prompt-file":            true,
	"--yolo":                   true,
	"--disable-sandbox":        true,
	"--approval-mode":          true,
	"--disable-approval":       true,
	"--allow-workspace-switch": true,
	"--sandbox-network":        true,
	// Provider / credential redirection and workspace re-rooting.
	"--provider":        true,
	"--base-url":        true,
	"--api-key-stdin":   true,
	"--workspace":       true,
	"--trust-workspace": true,
}

// museCodeValuedStrippedFlags consume the next token as their value.
var museCodeValuedStrippedFlags = map[string]bool{
	"--session-id":      true,
	"--prompt-file":     true,
	"--approval-mode":   true,
	"--sandbox-network": true,
	"--provider":        true,
	"--base-url":        true,
	"--workspace":       true,
}

// museCodeForwardedValuedFlags pass through with their value token.
var museCodeForwardedValuedFlags = map[string]bool{
	"--model":            true,
	"--reasoning-effort": true,
	"--max-model-steps":  true,
}

// museCodeDiagnosticTokens ask for information rather than a model run;
// reshaping them into `exec` would burn a model call nobody asked for.
var museCodeDiagnosticTokens = map[string]bool{
	"login": true, "logout": true, "auth": true, "schema": true, "serve": true,
	"resume": true, "export": true, "trace": true, "models": true, "update": true,
	"config": true, "mcp": true, "help": true, "version": true,
}

// isMuseCodeDiagnosticInvocation: `--version` / `--help` anywhere, or an
// information subcommand as the FIRST token.
func isMuseCodeDiagnosticInvocation(args []string) bool {
	for i, a := range args {
		lowered := strings.ToLower(strings.TrimSpace(a))
		if name, _, ok := strings.Cut(lowered, "="); ok {
			lowered = name
		}
		switch lowered {
		case "--version", "-v", "--help", "-h":
			return true
		}
		if i == 0 && museCodeDiagnosticTokens[lowered] {
			return true
		}
	}
	return false
}

// buildMuseCodeInteractiveArgs shapes a one-shot `muse` invocation for the
// legacy session_start / PTY path: `exec --json --disable-approval`, forwarded
// caller flags, then `--` and the prompt as a trailing positional (subject to
// the argv byte cap — see oneShotPositionalMaxPromptBytes). A bare `muse`
// would open the interactive TUI, which never exits on a headless session.
func buildMuseCodeInteractiveArgs(args []string) []string {
	if isMuseCodeDiagnosticInvocation(args) {
		return args
	}
	forwarded := make([]string, 0, len(args))
	prompt := make([]string, 0, len(args))
	afterSeparator := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if afterSeparator {
			prompt = append(prompt, a)
			continue
		}
		if a == "--" {
			afterSeparator = true
			continue
		}
		name := a
		inlineValue := false
		if idx := strings.Index(a, "="); idx > 0 {
			name = a[:idx]
			inlineValue = true
		}
		if museCodeStrippedFlags[name] {
			if !inlineValue && museCodeValuedStrippedFlags[name] {
				i++
			}
			continue
		}
		if a == "exec" && len(forwarded) == 0 && len(prompt) == 0 {
			continue // forced below; a second `exec` would parse as prompt text
		}
		if strings.HasPrefix(a, "-") {
			forwarded = append(forwarded, a)
			if museCodeForwardedValuedFlags[name] && !inlineValue && i+1 < len(args) {
				i++
				forwarded = append(forwarded, args[i])
			}
			continue
		}
		prompt = append(prompt, a)
	}
	result := append([]string{"exec", "--json", "--disable-approval"}, forwarded...)
	if len(prompt) > 0 {
		// `--` so a prompt that starts with `-` is never read as a flag.
		result = append(append(result, "--"), prompt...)
	}
	return result
}

// isMuseCodeSynthesizedRun reports the forced headless shape produced above.
func isMuseCodeSynthesizedRun(args []string) bool {
	return len(args) >= 3 && args[0] == "exec" && args[1] == "--json" && args[2] == "--disable-approval"
}
