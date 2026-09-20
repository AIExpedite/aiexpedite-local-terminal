// grok_argv.go — the Grok maintenance-smoke argv: its canonical shape, the
// bounded compatibility ladder, the validators, and the maintenance-only env
// policy. Mirrors claude_argv.go for Claude.
//
// Why a dedicated file: the smoke's child argv used to be a hand-frozen
// 12-token list re-asserted in TWO places in session.go (the wire request and
// the shaped child argv), and the shape it froze carried an EMPTY argv element
// (`--tools ""`). On Windows the `grok` on PATH is frequently an npm `.cmd`
// shim, and a `cmd.exe` re-parse of the shim's `%*` can drop an empty operand —
// leaving `--tools` to swallow `--disable-web-search` as its value. The child
// then exits non-zero during option parsing, BEFORE inference, so no marker is
// ever echoed and the pre- and post-update smokes fail identically while
// `grok models` (no empty operand, none of the contested flags) stays healthy.
//
// The canonical shape below therefore has three pinned invariants, each with a
// test in grok_argv_test.go:
//   - no argv element is ever the empty string (`--tools=` is one token);
//   - the marker nonce never appears in argv — the prompt rides in a 0600
//     temp file via `--prompt-file`, never inline `-p` (Grok's headless mode
//     does not read a piped stdin, and `-p <nonce>` would sit in a process
//     listing any local user can read);
//   - `--tools` is always the equals form.
//
// Only the SMOKE takes its argv from here. The ordinary interactive
// `session_start` path keeps buildGrokInteractiveArgs (session.go) untouched.
package main

import (
	"fmt"
	"strings"
)

// grokMaintenanceSmokeControlArg is an AI Expedite-only in-process control
// derived from the signed session_start contract. It is consumed before Grok
// argv shaping and must never be forwarded to the CLI.
const grokMaintenanceSmokeControlArg = "--aiexpedite-maintenance-smoke"

// grokMaintenanceSmokePromptPrefix is the fixed instruction the signed
// session_start smoke prompt must start with; everything after it is the
// marker the model is asked to echo.
const grokMaintenanceSmokePromptPrefix = "Return exactly this marker and nothing else: "

// extractGrokMaintenanceSmokeControl removes the internal maintenance-smoke
// token and reports whether it was present. Keeping the signal separate from
// --tools preserves ordinary no-tools invocations and their normal auth/config
// behavior.
func extractGrokMaintenanceSmokeControl(args []string) ([]string, bool) {
	requested := false
	cleaned := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == grokMaintenanceSmokeControlArg {
			requested = true
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return cleaned, requested
}

// grokMaintenanceSmokeRequest recognises the updater's reserved marker prompt
// envelope. Args are part of commandMsg's HMAC payload, so deriving the private
// control bit here keeps it authenticated without adding a new wire field that
// older publishers cannot sign. StartSession then validates the exact canonical
// request before spawning; malformed marker requests are promoted specifically
// so they fail closed instead of falling through as ordinary Grok sessions.
func grokMaintenanceSmokeRequest(args []string) bool {
	cleaned, _ := extractGrokMaintenanceSmokeControl(args)
	shaped := buildGrokInteractiveArgs(cleaned, false)
	for i, arg := range shaped {
		if arg != "-p" || i+1 >= len(shaped) {
			continue
		}
		return validGrokMaintenanceSmokePrompt(shaped[i+1])
	}
	return false
}

func validGrokMaintenanceSmokePrompt(prompt string) bool {
	marker := strings.TrimPrefix(prompt, grokMaintenanceSmokePromptPrefix)
	return marker != prompt && strings.TrimSpace(marker) != "" && !strings.ContainsAny(marker, "\r\n")
}

/* --------------------------------------------------------------------------
   Signed session_start wire request
   -------------------------------------------------------------------------- */

// grokSmokeWireRequest is the updater's canonical signed session_start argv,
// minus the trailing prompt. It is the LEGACY transport's contract and is kept
// byte-stable on purpose: older publishers sign exactly these tokens. The
// child argv is NOT this list — StartSession derives it from the ladder below
// (buildGrokNoToolsSmokeArgs), so the wire shape and the spawn shape are each
// defined exactly once and the empty `--tools` operand never reaches a child.
var grokSmokeWireRequest = []string{
	"--tools", "", "--disable-web-search", "--no-subagents",
	"--max-turns", "1", "--verbatim",
}

// validateGrokSmokeRequest accepts only the updater's canonical signed wire
// argv and returns its prompt. In particular, permission, model/provider,
// system-prompt, schema, sandbox, rules, and debug-output options are rejected
// before the general builder can strip or normalize them. The error is
// deliberately fixed text so an option value containing credentials or a
// private path is never reflected into a published session error.
func validateGrokSmokeRequest(args []string) (prompt string, err error) {
	if len(args) != len(grokSmokeWireRequest)+1 {
		return "", errGrokSmokeRequestContract
	}
	for i, want := range grokSmokeWireRequest {
		if args[i] != want {
			return "", errGrokSmokeRequestContract
		}
	}
	prompt = args[len(args)-1]
	if !validGrokMaintenanceSmokePrompt(prompt) {
		return "", errGrokSmokeRequestContract
	}
	return prompt, nil
}

var errGrokSmokeRequestContract = fmt.Errorf("grok maintenance smoke must use the exact no-tools single-turn contract")

// grokNoToolsExternalLoaderArg rejects caller-controlled loader surfaces that
// would defeat the isolated no-tools home. It returns only the canonical flag
// name, never an equals-form value: those values can contain credentials, raw
// agent JSON, or private file paths and are interpolated into a published start
// error by StartSession.
func grokNoToolsExternalLoaderArg(args []string) (string, bool) {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		for _, name := range []string{
			"--plugin-dir", "--config", "--agent", "--agents",
			"--cwd", "-w", "--worktree", "--worktree-ref", "--ref",
			"-r", "--resume", "-c", "--continue", "-s", "--session-id",
			"--fork-session", "--restore-code", "--leader-socket",
		} {
			if lower == name || strings.HasPrefix(lower, name+"=") {
				return name, true
			}
		}
	}
	return "", false
}

/* --------------------------------------------------------------------------
   Child argv: canonical shape + compatibility ladder
   -------------------------------------------------------------------------- */

// grokSmokeArgvShape is one candidate no-tools headless invocation. The ladder
// below is tried in order and the winner is cached per binary (cliagent_smoke.go)
// so a build that rejects a flag costs one extra child on the first probe and
// nothing afterwards.
type grokSmokeArgvShape struct {
	// ID is published in the smoke result so a maintenance run can tell which
	// shape the device settled on. It is a fixed identifier, never argv text.
	ID string
	// Hardened adds the headless-only isolation switches Grok 1.0.13+
	// documents (`--disable-web-search`, `--no-subagents`). The fallback rung
	// drops them for a build that predates them: `--tools=` already disables
	// every built-in tool, so the switches are belt-and-braces, not the only
	// thing keeping the probe off the network.
	Hardened bool
	// NoAutoUpdate adds `--no-auto-update`, the pre-1.0.13 headless guidance
	// that keeps a background updater from racing protocol output. It is NOT
	// on the canonical rung: newer builds reject it at the root command (the
	// exact pre-inference non-zero exit this file exists to remove), and the
	// isolated home already pins `auto_update = false`.
	NoAutoUpdate bool
}

// grokSmokeArgvShapes is the bounded (2-entry) ladder, newest-known-good first.
//
//   - rung 0, the canonical shape: equals-form flags only, both isolation
//     switches, no auto-update flag.
//   - rung 1, the legacy shape: drops the two isolation switches and carries
//     `--no-auto-update` instead — the flag set an older build documented.
//
// A full ladder miss is classified `protocol`, never retried further.
var grokSmokeArgvShapes = []grokSmokeArgvShape{
	{ID: "streaming-notools-prompt-file", Hardened: true},
	{ID: "streaming-notools-prompt-file-legacy", NoAutoUpdate: true},
}

// grokSmokePromptFileFlag is the only separate-value flag in the shape; its
// value is a path this process created, never caller text.
const grokSmokePromptFileFlag = "--prompt-file"

// buildGrokNoToolsSmokeArgs builds the sanctioned one-shot Grok argv for the
// CLI-maintenance smoke. Flag-by-flag:
//
//   - `--output-format=streaming-json` yields per-event NDJSON frames (text /
//     thought / end); `end` is the terminal envelope the classifier waits for.
//   - `--tools=` — THE FIX. An equals-form empty value carries no separate
//     empty argv element, so nothing can be dropped by a cmd.exe / `.cmd`-shim
//     re-parse and `--disable-web-search` can never be consumed as the tools
//     operand.
//   - `--disable-web-search`, `--no-subagents` (hardened rung only).
//   - `--max-turns=1` bounds the run to the single turn.
//   - `--no-auto-update` (legacy rung only) — see grokSmokeArgvShape.
//   - `--prompt-file <path>` — the marker prompt in a 0600 temp file. The
//     prompt is deliberately NOT a parameter: a shape that cannot receive the
//     marker cannot leak it into argv, a process listing, or a published
//     result.
//   - `--verbatim` is absent: it is not a documented root flag on current
//     builds, and the prompt itself asks for a verbatim echo.
//   - `--always-approve` / any permission-bypass flag is absent by
//     construction; validateGrokSmokeShape refuses argv that carries one.
func buildGrokNoToolsSmokeArgs(shape grokSmokeArgvShape, promptFilePath string) []string {
	args := []string{"--output-format=streaming-json"}
	if shape.NoAutoUpdate {
		args = append(args, "--no-auto-update")
	}
	args = append(args, "--tools=")
	if shape.Hardened {
		args = append(args, "--disable-web-search", "--no-subagents")
	}
	args = append(args, "--max-turns=1", grokSmokePromptFileFlag, promptFilePath)
	return args
}

// validateGrokSmokeShape verifies a child argv is EXACTLY one ladder rung with
// a non-empty prompt-file path. It is the single contract check for both call
// sites (the `__cli_smoke__` probe and the session_start smoke), replacing two
// hand-transcribed token lists: any injected approval, provider, filesystem or
// response-shaping option — or an empty argv element — fails here. Fixed error
// text: an argv value is never reflected into a published error.
func validateGrokSmokeShape(args []string) error {
	if len(args) == 0 {
		return errGrokSmokeShapeContract
	}
	promptFilePath := args[len(args)-1]
	if promptFilePath == "" || strings.HasPrefix(promptFilePath, "-") {
		return errGrokSmokeShapeContract
	}
	for _, shape := range grokSmokeArgvShapes {
		if argvEqual(args, buildGrokNoToolsSmokeArgs(shape, promptFilePath)) {
			return nil
		}
	}
	return errGrokSmokeShapeContract
}

var errGrokSmokeShapeContract = fmt.Errorf("grok maintenance smoke child argv violates the exact no-tools single-turn contract")

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// grokSmokeRetryableFlags are the flags that some LATER rung of the ladder
// omits — derived from the shapes themselves rather than re-listed, so a third
// rung (or a change to what the fallback drops) cannot leave a stale copy
// behind. Lowercased for case-insensitive matching against a CLI's error text.
//
// Used by the smoke's retry gate: a rejection naming one of these can be fixed
// by the next rung and is safe to retry (option parsing precedes inference),
// while a rejection of any flag EVERY rung carries would spend a second child
// to fail identically.
var grokSmokeRetryableFlags = computeGrokSmokeRetryableFlags()

func computeGrokSmokeRetryableFlags() []string {
	if len(grokSmokeArgvShapes) < 2 {
		return nil
	}
	const placeholder = "prompt-file-placeholder"
	kept := map[string]bool{}
	for _, arg := range buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[len(grokSmokeArgvShapes)-1], placeholder) {
		kept[grokSmokeFlagName(arg)] = true
	}
	var optional []string
	for _, arg := range buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[0], placeholder) {
		name := grokSmokeFlagName(arg)
		if strings.HasPrefix(name, "--") && !kept[name] {
			optional = append(optional, strings.ToLower(name))
		}
	}
	return optional
}

// grokSmokeFlagName strips an equals-form value (`--tools=` → `--tools`) so
// rungs are compared by flag, not by value.
func grokSmokeFlagName(arg string) string {
	if name, _, found := strings.Cut(arg, "="); found {
		return name
	}
	return arg
}

/* --------------------------------------------------------------------------
   Maintenance-only environment policy
   -------------------------------------------------------------------------- */

// sanitizeGrokMaintenanceSmokeEnv is stricter than the reusable ACP sanitizer:
// maintenance smokes must not inherit any Grok execution, routing, logging, or
// extension-discovery override. Grok's environment surface grows independently
// of this agent, so a denylist is unsafe here: strip every inherited GROK_* and
// OTEL_* variable, plus related xAI endpoint and Rust diagnostic controls, then
// add back only fixed-off compatibility/tool-scanner controls. OTEL_* must be
// removed even though GROK_EXTERNAL_OTEL is also stripped: a system managed
// `[telemetry] otel_enabled = true` survives GROK_HOME isolation and otherwise
// turns inherited exporter/content controls back on. This also prevents
// GROK_LOG_FILE from persisting raw diagnostics outside the isolated home and
// RUST_LOG/RUST_BACKTRACE or an OTEL console exporter from adding non-protocol
// output to the exact marker.
func sanitizeGrokMaintenanceSmokeEnv(env []string) []string {
	base := sanitizeGrokACPEnv(env, false)
	filtered := make([]string, 0, len(base)+2)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GROK_") || strings.HasPrefix(upper, "OTEL_") || upper == "XAI_API_BASE_URL" ||
			upper == "RUST_LOG" || upper == "RUST_BACKTRACE" || upper == "RUST_LIB_BACKTRACE" {
			continue
		}
		filtered = append(filtered, entry)
	}
	for _, name := range grokNeutralisedIntegrationSwitches {
		filtered = setEnvVar(filtered, name, "0")
	}
	return filtered
}

// grokNeutralisedIntegrationSwitches are the workspace-integration switches a
// non-interactive Grok child runs with forced OFF — the maintenance smoke and
// the model-list probe (cliagent_models.go) both pin them so the child neither
// loads editor skills/rules/MCPs nor writes session state.
var grokNeutralisedIntegrationSwitches = []string{
	"GROK_CURSOR_SKILLS_ENABLED", "GROK_CURSOR_RULES_ENABLED", "GROK_CURSOR_AGENTS_ENABLED",
	"GROK_CURSOR_MCPS_ENABLED", "GROK_CURSOR_HOOKS_ENABLED", "GROK_CURSOR_SESSIONS_ENABLED",
	"GROK_CLAUDE_SKILLS_ENABLED", "GROK_CLAUDE_RULES_ENABLED", "GROK_CLAUDE_AGENTS_ENABLED",
	"GROK_CLAUDE_MCPS_ENABLED", "GROK_CLAUDE_HOOKS_ENABLED", "GROK_CLAUDE_SESSIONS_ENABLED",
	"GROK_CODEX_SKILLS_ENABLED", "GROK_CODEX_RULES_ENABLED", "GROK_CODEX_AGENTS_ENABLED",
	"GROK_CODEX_MCPS_ENABLED", "GROK_CODEX_HOOKS_ENABLED", "GROK_CODEX_SESSIONS_ENABLED",
	"GROK_MANAGED_MCPS_ENABLED", "GROK_MANAGED_MCP_GATEWAY_TOOLS_ENABLED",
	"GROK_WORKSPACE_TOOL_DEFS_ENABLED", "GROK_WORKSPACE_TOOL_STATE_ENABLED",
}
