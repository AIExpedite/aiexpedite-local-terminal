// claude_argv.go — every Claude Code argv shape this agent spawns.
//
// Two shapes exist and they share NOTHING but the print-flag classifier:
//
//   - buildClaudeInteractiveArgs — the resident/PTY session shape. Bidirectional
//     stream-json, prompt on stdin as NDJSON, `-p`/`--print` STRIPPED (print mode
//     exits after one turn and, since 2026-06-15, bills against the Agent SDK
//     credit pool instead of the interactive subscription allowance).
//   - buildClaudeNonInteractivePrintArgs — the one-shot probe shape used by the
//     CLI-maintenance smoke (cliagent_smoke_claudecode.go). `--print` is KEPT,
//     no tools, no MCP, a single terminal `result` JSON envelope.
//
// They live in one file on purpose. While the interactive builder was the only
// Claude argv builder, a print-mode probe had nowhere to go: it was pushed down
// the interactive path, silently lost its `--print`, inherited
// `--input-format stream-json`, and then had stdin closed with no NDJSON
// envelope ever written — which Claude 2.1.x rejects before any assistant turn
// (`Error: --input-format=stream-json requires output-format=stream-json.`,
// exit 1). That is the `errorCategory: protocol` smoke failure this file exists
// to make impossible: the print flag is named in exactly one place, one builder
// strips it and the other requires it.
package main

import "strings"

// claudePrintFlag / claudePrintShortFlag are the one-shot ("print") mode flags.
// Single-sourced so the two builders below can never disagree about what the
// flag IS: buildClaudeInteractiveArgs strips every form of it, and
// buildClaudeNonInteractivePrintArgs emits the long form as its first argument.
const (
	claudePrintFlag      = "--print"
	claudePrintShortFlag = "-p"
)

// claudePrintFlagPrompt classifies one user-supplied argv token against the
// print flag. It returns whether the token IS the print flag in any of its four
// accepted forms (`-p`, `--print`, `-p=…`, `--print=…`) and, for the equals
// forms, the inline prompt text carried after the `=`.
//
// Case-sensitive on purpose: claude's own CLI is, so `-P` / `--PRINT` are NOT
// the print flag and must pass through to surface as claude's own unknown-flag
// error rather than being silently swallowed here.
func claudePrintFlagPrompt(arg string) (isPrintFlag bool, inlinePrompt string) {
	switch arg {
	case claudePrintShortFlag, claudePrintFlag:
		return true, ""
	}
	for _, prefix := range []string{claudePrintShortFlag + "=", claudePrintFlag + "="} {
		if strings.HasPrefix(arg, prefix) {
			return true, strings.TrimPrefix(arg, prefix)
		}
	}
	return false, ""
}

// buildClaudeInteractiveArgs builds Claude Code CLI args for bidirectional
// stream-json mode.  The prompt is NOT passed as a CLI arg — it is returned
// separately so the caller can send it as an NDJSON message on stdin.
// Returns (cliArgs, promptText).
//
// IMPORTANT: this path is for INTERACTIVE multi-turn sessions. We never add
// `-p` / `--print` — that flag puts claude in one-shot mode and exits after
// the first response, killing the session before a follow-up `session.sendInput`
// can land. Any user-supplied `-p`/`--print` is stripped for the same reason.
// Callers that want one-shot claude must use buildClaudeNonInteractivePrintArgs
// (the sanctioned non-interactive shape) or the non-session execute.runAndWait
// path.
func buildClaudeInteractiveArgs(args []string) ([]string, string) {
	result := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}

	// Claude flags that consume the next argument as their value.
	// Without this, "--model sonnet" would treat "sonnet" as a prompt word.
	valuedFlags := map[string]bool{
		"--model": true, "--system-prompt": true, "--append-system-prompt": true,
		"--permission-mode": true, "--max-budget-usd": true, "--effort": true,
		"--agent": true, "--agents": true, "--session-id": true,
		"--mcp-config": true, "--settings": true, "--json-schema": true,
		"--fallback-model": true, "--debug-file": true, "--setting-sources": true,
	}

	// Separate user-provided flags from prompt words.
	// -p / --print (and the equals-form variants -p=... / --print=...) are
	// stripped — we never want claude in print/one-shot mode on this path; it
	// would exit after the first turn and break cross-step session.sendInput.
	// Keeping the interactive launch shape is also load-bearing for billing:
	// starting 2026-06-15 Anthropic routes `claude -p` / Agent SDK usage on
	// Pro/Max/Team subscriptions through a separate Agent SDK credit pool,
	// while plain interactive Claude Code keeps drawing from the normal
	// subscription allowance. See CLI_AGENT_INTEGRATION.md.
	var flags []string
	var promptParts []string
	skipNext := false
	for i, a := range args {
		if skipNext {
			skipNext = false
			flags = append(flags, a)
			continue
		}
		// Equals-form: strip the print/one-shot mode flag but keep the inline
		// prompt text (e.g. `claude --print=hello` → prompt "hello") so the
		// interactive launch still answers the caller's query instead of
		// hanging on empty stdin.
		if isPrint, inline := claudePrintFlagPrompt(a); isPrint {
			if inline != "" {
				promptParts = append(promptParts, inline)
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			// If this flag expects a value and there's a next arg, consume it too
			if valuedFlags[a] && i+1 < len(args) {
				skipNext = true
			}
			continue
		}
		promptParts = append(promptParts, a)
	}

	result = append(result, flags...)

	return result, strings.Join(promptParts, " ")
}

// claudeArgvShape is one candidate no-tools print invocation. The ladder below
// is tried in order and the winner is cached per binary — see
// cliagent_smoke_claudecode.go — so a build that rejects a flag costs one extra
// child on the first probe and nothing afterwards.
type claudeArgvShape struct {
	// ID is published in the smoke result so a maintenance run can tell which
	// shape the device settled on. It is a fixed identifier, never argv text.
	ID string
	// StrictMCP pins an EMPTY MCP server map so the user's own MCP config can
	// neither inject a server nor stall the probe on a server handshake.
	StrictMCP bool
}

// claudeArgvShapes is the bounded (2-entry) ladder of no-tools print shapes.
// The preferred shape isolates the probe from the user's MCP config; the
// fallback drops ONLY the MCP flags, for a build that does not accept them.
// A full ladder miss is classified `protocol`, never retried further.
var claudeArgvShapes = []claudeArgvShape{
	{ID: "print-json-notools-strict-mcp", StrictMCP: true},
	{ID: "print-json-notools", StrictMCP: false},
}

// claudeEmptyMCPConfig is an inline MCP config with no servers. Passed as a
// JSON string (`--mcp-config` accepts files or strings) so the probe never
// materialises a temp file.
const claudeEmptyMCPConfig = `{"mcpServers":{}}`

// buildClaudeNonInteractivePrintArgs builds the sanctioned one-shot Claude argv
// for the CLI-maintenance smoke. Verified against Claude Code 2.1.247.
//
// The prompt is deliberately NOT a parameter: it — and the marker nonce it
// carries — goes on stdin as plain text and is closed immediately (Windows
// CreateProcess has a ~32KB command-line ceiling, and a prompt on argv would be
// visible in any process listing). A shape that cannot receive the marker
// cannot leak it into argv, into a `ps` snapshot, or into a published result.
//
// Flag-by-flag, and why each is or is not here:
//
//   - `--print` is KEPT. This is the whole point of the shape; the interactive
//     builder's strip rule does not apply.
//   - `--output-format json` yields ONE terminal `{"type":"result",…}` envelope.
//     `stream-json` is never requested, so `--verbose` and
//     `--include-partial-messages` (which only apply to it) are omitted.
//   - `--input-format` is left at its default (`text`). Pairing
//     `--input-format stream-json` with anything but `--output-format
//     stream-json` + `--verbose` is the exact framing violation that produced
//     the exit-1 protocol failure this shape replaces.
//   - `--tools ""` disables every built-in tool (claude's own documented way to
//     do it), so the probe is one inference turn and cannot touch the machine.
//   - `--max-turns 1` bounds it to that single turn.
//   - `--dangerously-skip-permissions` is deliberately ABSENT: it is refused
//     outright in some sandbox/root contexts (a protocol-shaped exit 1 of its
//     own) and is meaningless when no tool can run.
func buildClaudeNonInteractivePrintArgs(shape claudeArgvShape) []string {
	args := []string{
		claudePrintFlag,
		"--output-format", "json",
		"--tools", "",
		"--max-turns", "1",
	}
	if shape.StrictMCP {
		args = append(args, "--strict-mcp-config", "--mcp-config", claudeEmptyMCPConfig)
	}
	return args
}

// claudeRetryableArgvFlags are the flags that some LATER rung of the ladder
// omits — derived from the shapes themselves rather than re-listed, so a third
// rung (or a change to what the fallback drops) cannot leave a stale copy
// behind. Lowercased for case-insensitive matching against a CLI's error text.
//
// Used by the smoke's retry gate: a rejection naming one of these can be fixed
// by the next rung and is safe to retry (option parsing precedes inference),
// while a rejection of any flag EVERY rung carries would spend a second turn to
// fail identically.
var claudeRetryableArgvFlags = computeClaudeRetryableArgvFlags()

func computeClaudeRetryableArgvFlags() []string {
	if len(claudeArgvShapes) < 2 {
		return nil
	}
	kept := map[string]bool{}
	for _, arg := range buildClaudeNonInteractivePrintArgs(claudeArgvShapes[len(claudeArgvShapes)-1]) {
		kept[arg] = true
	}
	var optional []string
	for _, arg := range buildClaudeNonInteractivePrintArgs(claudeArgvShapes[0]) {
		if strings.HasPrefix(arg, "--") && !kept[arg] {
			optional = append(optional, strings.ToLower(arg))
		}
	}
	return optional
}
