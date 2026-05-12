// session_cli_test.go
// -----------------------------------------------------------------------------
// Unit tests for the per-CLI argument builders, event detection, and display-text
// extraction used by the SessionManager when proxying claude / codex / gemini.
//
// This is the safety net that pins behaviour for the three supported CLI agents:
//
//   - claude:  --output-format stream-json + --input-format stream-json + -p,
//              prompt sent as NDJSON on stdin, stdin held open until the
//              "result" event arrives, then closed so claude observes EOF and
//              exits.
//   - codex:   exec --json --dangerously-bypass-approvals-and-sandbox, prompt
//              as a positional arg, stdin closed at start (codex exec appends
//              piped stdin to the prompt; leaving the pipe open makes it wait
//              indefinitely for EOF).
//   - gemini:  prompt as the first positional arg, -o stream-json,
//              --approval-mode auto_edit, stdin closed at start.
//
// Critical regression context (v0.9.6/v0.9.7): a fix to make codex exec actually
// terminate (PR #17) changed stdin handling in a way that risked breaking the
// claude flow if the LLM ever issued `claude` without a prompt in args (which
// would leave stdinPrompt empty and prematurely close stdin). v0.9.7 added
// detectCLITerminalEvent + a pre-stdin-close flushBatch so the final text
// chunk is always queued for publish before the CLI process tears down — the
// bug that surfaced as "agent didn't wait for terminal response — calls cross
// between steps" in the documentDesign agent.
//
// These tests pin all three CLI flows so a future fix for one CLI doesn't
// silently regress another.

package main

import (
	"reflect"
	"strings"
	"testing"
)

/* --------------------------------------------------------------------------
   buildClaudeInteractiveArgs
   --------------------------------------------------------------------------
   Claude is the only CLI that returns a non-empty stdinPrompt — the prompt is
   removed from argv and sent as NDJSON on stdin. The args function must:
     1. Always include --output-format stream-json + --input-format stream-json
        + --verbose + --include-partial-messages + --dangerously-skip-permissions
     2. Absorb any user-supplied -p / --print (we add our own).
     3. Recognise valued flags (--model X, --system-prompt X, etc.) and keep
        the value tied to the flag — otherwise the value gets misparsed as a
        prompt word.
     4. Always end with -p so claude runs in print mode.
   ------------------------------------------------------------------------ */

func TestBuildClaudeInteractiveArgs_PromptOnly(t *testing.T) {
	args, prompt := buildClaudeInteractiveArgs([]string{"explain the auth flow"})

	if prompt != "explain the auth flow" {
		t.Errorf("prompt = %q, want %q", prompt, "explain the auth flow")
	}

	// Required flags must be present and the prompt must NOT be in argv.
	mustContain(t, args,
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
		"-p",
	)
	for _, a := range args {
		if a == "explain" || a == "the" || a == "auth" || a == "flow" {
			t.Errorf("prompt word leaked into argv: %v", args)
			break
		}
	}
}

func TestBuildClaudeInteractiveArgs_MultiWordPromptJoinedWithSpaces(t *testing.T) {
	_, prompt := buildClaudeInteractiveArgs([]string{"design", "the", "login", "page"})
	if prompt != "design the login page" {
		t.Errorf("prompt = %q, want %q", prompt, "design the login page")
	}
}

func TestBuildClaudeInteractiveArgs_AbsorbsUserSuppliedPrintFlag(t *testing.T) {
	// We add our own -p; any user-provided -p/--print must be dropped so we
	// don't end up with `-p -p` (claude is tolerant but it's confusing).
	args, prompt := buildClaudeInteractiveArgs([]string{"-p", "hello"})
	if prompt != "hello" {
		t.Errorf("prompt = %q, want %q", prompt, "hello")
	}
	// Exactly one -p (the one we appended at the end).
	count := 0
	for _, a := range args {
		if a == "-p" || a == "--print" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("-p occurrence count = %d, want 1, args=%v", count, args)
	}
}

func TestBuildClaudeInteractiveArgs_KeepsValuedFlagWithItsValue(t *testing.T) {
	// --model sonnet must travel together: if "sonnet" were treated as a
	// prompt word, claude would silently fall back to the default model and
	// the prompt would be misread as "sonnet research the auth flow".
	args, prompt := buildClaudeInteractiveArgs([]string{
		"--model", "sonnet",
		"research", "the", "auth", "flow",
	})

	if prompt != "research the auth flow" {
		t.Errorf("prompt = %q, want %q", prompt, "research the auth flow")
	}

	// Find --model and verify the next arg is "sonnet".
	foundModel := false
	for i, a := range args {
		if a == "--model" {
			if i+1 >= len(args) || args[i+1] != "sonnet" {
				t.Errorf("--model not followed by 'sonnet': args=%v", args)
			}
			foundModel = true
			break
		}
	}
	if !foundModel {
		t.Errorf("--model flag missing: args=%v", args)
	}
}

func TestBuildClaudeInteractiveArgs_AllValuedFlagsRecognised(t *testing.T) {
	// One round trip per valued flag — keeps the registry from drifting if
	// someone adds a new claude flag without updating valuedFlags.
	flags := []string{
		"--model", "--system-prompt", "--append-system-prompt",
		"--permission-mode", "--max-budget-usd", "--effort",
		"--agent", "--agents", "--session-id",
		"--mcp-config", "--settings", "--json-schema",
		"--fallback-model", "--debug-file", "--setting-sources",
	}
	for _, f := range flags {
		t.Run(f, func(t *testing.T) {
			args, prompt := buildClaudeInteractiveArgs([]string{f, "VALUE", "the prompt"})
			if prompt != "the prompt" {
				t.Errorf("%s VALUE leaked into prompt: prompt=%q args=%v", f, prompt, args)
			}
			// VALUE must appear directly after f in args.
			for i, a := range args {
				if a == f {
					if i+1 >= len(args) || args[i+1] != "VALUE" {
						t.Errorf("%s not followed by VALUE: args=%v", f, args)
					}
					return
				}
			}
			t.Errorf("%s missing from args: %v", f, args)
		})
	}
}

func TestBuildClaudeInteractiveArgs_BooleanFlagsAreNotValued(t *testing.T) {
	// --verbose is a boolean — the next token MUST be treated as a prompt
	// word, not consumed as a value. Without this check, a regression in the
	// valuedFlags map (e.g. accidentally marking --verbose true) would silently
	// eat the first prompt word.
	args, prompt := buildClaudeInteractiveArgs([]string{"--verbose", "hello world"})
	if prompt != "hello world" {
		t.Errorf("prompt = %q, want %q (args=%v)", prompt, "hello world", args)
	}
}

func TestBuildClaudeInteractiveArgs_EmptyArgsProducesEmptyPrompt(t *testing.T) {
	// Edge case: terminal call with no args at all. stdinPrompt is empty,
	// which means shouldCloseStdinAfterStart returns true and stdin gets
	// closed immediately — claude reads EOF on stdin and exits without doing
	// anything. The caller is responsible for never doing this, but the
	// function itself should NOT panic.
	args, prompt := buildClaudeInteractiveArgs([]string{})
	if prompt != "" {
		t.Errorf("prompt = %q, want empty", prompt)
	}
	mustContain(t, args, "-p", "--output-format", "stream-json")
}

func TestBuildClaudeInteractiveArgs_PromptEndsWithDashedToken(t *testing.T) {
	// "implement -h flag handling" — the "-h" is part of the prompt text, NOT
	// a flag. The current implementation treats anything starting with "-" as
	// a flag, so the user must quote dashed tokens. We pin that behaviour so
	// any future change here is intentional.
	args, prompt := buildClaudeInteractiveArgs([]string{"implement", "-h", "flag", "handling"})
	if strings.Contains(prompt, "-h") {
		t.Errorf("current contract: dashed tokens are treated as flags, not prompt words; prompt=%q", prompt)
	}
	// -h ended up in flags, prompt is "implement flag handling".
	if prompt != "implement flag handling" {
		t.Errorf("prompt = %q, want %q (args=%v)", prompt, "implement flag handling", args)
	}
}

/* --------------------------------------------------------------------------
   buildGeminiInteractiveArgs
   --------------------------------------------------------------------------
   Gemini takes the prompt as the FIRST positional arg, then we append
   -o stream-json and --approval-mode auto_edit. stdinPrompt is empty (we don't
   relay stdin to gemini).
   ------------------------------------------------------------------------ */

func TestBuildGeminiInteractiveArgs_PromptFirst(t *testing.T) {
	args := buildGeminiInteractiveArgs([]string{"explain the auth flow"})
	want := []string{
		"explain the auth flow",
		"-o", "stream-json",
		"--approval-mode", "auto_edit",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestBuildGeminiInteractiveArgs_PreservesUserFlags(t *testing.T) {
	args := buildGeminiInteractiveArgs([]string{"explain", "--model", "gemini-2.0-pro"})
	// Whatever the user passes goes through verbatim before our trailing flags.
	if args[0] != "explain" || args[1] != "--model" || args[2] != "gemini-2.0-pro" {
		t.Errorf("user args not preserved at head: %v", args)
	}
	mustContain(t, args, "-o", "stream-json", "--approval-mode", "auto_edit")
}

func TestBuildGeminiInteractiveArgs_EmptyArgs(t *testing.T) {
	// Should still produce the required trailing flags so gemini at least
	// boots in stream-json mode even if the LLM forgot the prompt.
	args := buildGeminiInteractiveArgs([]string{})
	mustContain(t, args, "-o", "stream-json", "--approval-mode", "auto_edit")
}

/* --------------------------------------------------------------------------
   buildInteractiveCLIArgs (router)
   --------------------------------------------------------------------------
   The dispatcher is what makes the empty-stdinPrompt contract per CLI work —
   claude returns (args, prompt), codex/gemini return (args, "").
   ------------------------------------------------------------------------ */

func TestBuildInteractiveCLIArgs_RoutesByCommand(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		_, prompt := buildInteractiveCLIArgs("claude", []string{"hello"})
		if prompt != "hello" {
			t.Errorf("claude prompt = %q, want %q", prompt, "hello")
		}
	})
	t.Run("codex", func(t *testing.T) {
		_, prompt := buildInteractiveCLIArgs("codex", []string{"hello"})
		if prompt != "" {
			t.Errorf("codex stdinPrompt MUST be empty (prompt goes on argv): %q", prompt)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		_, prompt := buildInteractiveCLIArgs("gemini", []string{"hello"})
		if prompt != "" {
			t.Errorf("gemini stdinPrompt MUST be empty (prompt goes on argv): %q", prompt)
		}
	})
	t.Run("case-insensitive", func(t *testing.T) {
		// The router checks command.ToLower() exactly + startswith — make sure
		// "Claude" / "CODEX" still route correctly.
		_, p1 := buildInteractiveCLIArgs("Claude", []string{"hi"})
		if p1 == "" {
			t.Errorf("Claude (mixed case) should still route to claude builder, got empty prompt")
		}
		args, _ := buildInteractiveCLIArgs("CODEX", []string{"hi"})
		if len(args) == 0 || args[0] != "exec" {
			t.Errorf("CODEX (upper case) should still route to codex builder, args=%v", args)
		}
	})
	t.Run("unknown-command-passes-through", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("git", []string{"status"})
		if prompt != "" {
			t.Errorf("unknown command should have empty stdinPrompt, got %q", prompt)
		}
		if !reflect.DeepEqual(args, []string{"status"}) {
			t.Errorf("unknown command should pass args through verbatim, got %v", args)
		}
	})
}

/* --------------------------------------------------------------------------
   detectResultEvent — Claude only
   --------------------------------------------------------------------------
   This is the trigger that closes Claude's stdin so it exits. If detection
   breaks, Claude hangs forever in --input-format stream-json (since the input
   stream stays open waiting for the next turn).
   ------------------------------------------------------------------------ */

func TestDetectResultEvent_ClaudeResultEvent(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "minimal result event",
			line: `{"type":"result"}`,
			want: true,
		},
		{
			name: "full result event with subtype and metadata",
			line: `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"result":"final answer","session_id":"abc","total_cost_usd":0.01}`,
			want: true,
		},
		{
			name: "result event with leading/trailing whitespace",
			line: `   {"type":"result"}   `,
			want: true,
		},
		{
			name: "non-result type",
			line: `{"type":"assistant","message":{"content":[]}}`,
			want: false,
		},
		{
			name: "stream_event envelope is NOT a result event",
			line: `{"type":"stream_event","event":{"type":"content_block_delta"}}`,
			want: false,
		},
		{
			name: "malformed JSON",
			line: `{"type":"result"`,
			want: false,
		},
		{
			name: "empty string",
			line: ``,
			want: false,
		},
		{
			name: "plain text (no leading brace)",
			line: `result: success`,
			want: false,
		},
		{
			name: "JSON array (not an object)",
			line: `[{"type":"result"}]`,
			want: false,
		},
		{
			name: "type field is not a string",
			line: `{"type":42}`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectResultEvent("claude", tc.line)
			if got != tc.want {
				t.Errorf("detectResultEvent(claude, %q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestDetectResultEvent_OnlyTriggersForClaude(t *testing.T) {
	// codex and gemini both emit events with type:"result" too, but THEIR
	// result events do NOT mean stdin should be closed — they're already
	// going to exit on their own.
	line := `{"type":"result","result":"done"}`
	for _, cmd := range []string{"codex", "gemini", "git", "powershell"} {
		t.Run(cmd, func(t *testing.T) {
			if detectResultEvent(cmd, line) {
				t.Errorf("detectResultEvent(%q, ...) returned true — only claude should trigger stdin close", cmd)
			}
		})
	}
}

func TestDetectResultEvent_CaseInsensitiveCommand(t *testing.T) {
	if !detectResultEvent("Claude", `{"type":"result"}`) {
		t.Errorf("detectResultEvent should be case-insensitive on the command name")
	}
	if !detectResultEvent("CLAUDE", `{"type":"result"}`) {
		t.Errorf("detectResultEvent should accept fully upper-cased command name")
	}
}

/* --------------------------------------------------------------------------
   detectCLITerminalEvent (v0.9.7)
   --------------------------------------------------------------------------
   This is the new flush-trigger added in v0.9.7 to fix the documentDesign
   regression where the final stream chunk could race session_ended. It must
   recognise the end-of-turn event for each CLI so the batch flushes before
   the process exit cascade publishes session_ended.
   ------------------------------------------------------------------------ */

func TestDetectCLITerminalEvent_Claude(t *testing.T) {
	if !detectCLITerminalEvent("claude", `{"type":"result"}`) {
		t.Error("claude result event must be a terminal event")
	}
	if detectCLITerminalEvent("claude", `{"type":"assistant"}`) {
		t.Error("claude assistant event is NOT a terminal event")
	}
}

func TestDetectCLITerminalEvent_Codex(t *testing.T) {
	// Both signal "we're done" — codex emits turn.completed at the end of a
	// turn and thread.completed right before exit.
	if !detectCLITerminalEvent("codex", `{"type":"thread.completed"}`) {
		t.Error("codex thread.completed must be a terminal event")
	}
	if !detectCLITerminalEvent("codex", `{"type":"turn.completed"}`) {
		t.Error("codex turn.completed must be a terminal event")
	}
	if detectCLITerminalEvent("codex", `{"type":"turn.started"}`) {
		t.Error("codex turn.started is NOT a terminal event")
	}
	if detectCLITerminalEvent("codex", `{"type":"item.completed"}`) {
		t.Error("codex item.completed is NOT a terminal event (per-item, not per-turn)")
	}
}

func TestDetectCLITerminalEvent_Gemini(t *testing.T) {
	if !detectCLITerminalEvent("gemini", `{"type":"result"}`) {
		t.Error("gemini result event must be a terminal event")
	}
	if detectCLITerminalEvent("gemini", `{"type":"message"}`) {
		t.Error("gemini message event is NOT a terminal event")
	}
}

func TestDetectCLITerminalEvent_NonJsonAndMalformedReturnFalse(t *testing.T) {
	// We must NEVER flushBatch on a plain-text line by mistake — that would
	// cause double flushes and could publish empty batches.
	cases := []string{
		"",
		"just text",
		`{"missing-type":true}`,
		`{"type":"result"`, // malformed JSON
	}
	for _, line := range cases {
		for _, cmd := range []string{"claude", "codex", "gemini"} {
			if detectCLITerminalEvent(cmd, line) {
				t.Errorf("detectCLITerminalEvent(%q, %q) returned true; expected false", cmd, line)
			}
		}
	}
}

func TestDetectCLITerminalEvent_UnknownCommand(t *testing.T) {
	// For non-CLI sessions (git, powershell, etc.) we never want to flush
	// based on JSON event content — those flows use plain-text output.
	if detectCLITerminalEvent("git", `{"type":"result"}`) {
		t.Error("non-CLI command must never match a terminal event")
	}
}

/* --------------------------------------------------------------------------
   extractDisplayText — Claude
   ------------------------------------------------------------------------ */

func TestExtractDisplayText_Claude_TextDeltaInsideStreamEvent(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello world"}}}`
	got := extractDisplayText("claude", line)
	if got != "Hello world" {
		t.Errorf("got %q, want %q", got, "Hello world")
	}
}

func TestExtractDisplayText_Claude_ThinkingDeltaInsideStreamEvent(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"considering..."}}}`
	got := extractDisplayText("claude", line)
	if got != "considering..." {
		t.Errorf("got %q, want %q", got, "considering...")
	}
}

func TestExtractDisplayText_Claude_ToolUseBlockStart(t *testing.T) {
	line := `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash"}}}`
	got := extractDisplayText("claude", line)
	if !strings.Contains(got, "Bash") {
		t.Errorf("got %q, want output to mention 'Bash'", got)
	}
}

func TestExtractDisplayText_Claude_AssistantMessageWithMultipleContentBlocks(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Part A"},{"type":"tool_use","name":"Read"},{"type":"text","text":"Part B"}]}}`
	got := extractDisplayText("claude", line)
	// Joined together, so we should see all three signals.
	for _, want := range []string{"Part A", "Read", "Part B"} {
		if !strings.Contains(got, want) {
			t.Errorf("got %q, missing %q", got, want)
		}
	}
}

func TestExtractDisplayText_Claude_SkipsMetadataEvents(t *testing.T) {
	// init / system / user / tool_result / result / rate_limit_event must NOT
	// produce display text — they're metadata.
	cases := []string{
		`{"type":"init","session_id":"abc"}`,
		`{"type":"system","msg":"hi"}`,
		`{"type":"user","message":{}}`,
		`{"type":"tool_result","tool_use_id":"x"}`,
		`{"type":"result","subtype":"success","result":"final"}`,
		`{"type":"rate_limit_event","retry_after":5}`,
	}
	for _, line := range cases {
		got := extractDisplayText("claude", line)
		if got != "" {
			t.Errorf("expected empty display text for %q, got %q", line, got)
		}
	}
}

func TestExtractDisplayText_Claude_NonJsonPassthrough(t *testing.T) {
	// Plain-text errors / stderr go straight through.
	got := extractDisplayText("claude", "fatal: not a git repository")
	if got != "fatal: not a git repository" {
		t.Errorf("plain text should passthrough, got %q", got)
	}
}

func TestExtractDisplayText_Claude_MalformedJsonPassthrough(t *testing.T) {
	got := extractDisplayText("claude", `{"type":"assistant"`)
	if got != `{"type":"assistant"` {
		t.Errorf("malformed JSON should passthrough, got %q", got)
	}
}

func TestExtractDisplayText_Claude_SkipsInputJsonDelta(t *testing.T) {
	// input_json_delta carries tool-input JSON being streamed; it's noise to
	// the human reader.
	line := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"foo\":"}}}`
	got := extractDisplayText("claude", line)
	if got != "" {
		t.Errorf("input_json_delta should be skipped, got %q", got)
	}
}

/* --------------------------------------------------------------------------
   extractDisplayText — Codex
   ------------------------------------------------------------------------ */

func TestExtractDisplayText_Codex_AgentMessage(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"agent_message","text":"Here is your answer."}}`
	got := extractDisplayText("codex", line)
	if got != "Here is your answer." {
		t.Errorf("got %q, want %q", got, "Here is your answer.")
	}
}

func TestExtractDisplayText_Codex_CommandExecution(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`
	got := extractDisplayText("codex", line)
	if !strings.Contains(got, "go test ./...") {
		t.Errorf("got %q, want output to include the executed command", got)
	}
}

func TestExtractDisplayText_Codex_FileChangeWithAction(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"file_change","path":"main.go","action":"modified"}}`
	got := extractDisplayText("codex", line)
	if !strings.Contains(got, "main.go") || !strings.Contains(got, "modified") {
		t.Errorf("got %q, want path + action", got)
	}
}

func TestExtractDisplayText_Codex_TurnFailedSurfacesError(t *testing.T) {
	line := `{"type":"turn.failed","error":"context_length_exceeded"}`
	got := extractDisplayText("codex", line)
	if !strings.Contains(got, "context_length_exceeded") {
		t.Errorf("turn.failed should surface the error: got %q", got)
	}
}

func TestExtractDisplayText_Codex_SkipsLifecycleEvents(t *testing.T) {
	// Lifecycle chatter is just noise to the human reader.
	cases := []string{
		`{"type":"thread.started"}`,
		`{"type":"turn.started"}`,
		`{"type":"turn.completed"}`,
		`{"type":"item.started"}`,
		`{"type":"thread.completed"}`,
	}
	for _, line := range cases {
		got := extractDisplayText("codex", line)
		if got != "" {
			t.Errorf("lifecycle event should be skipped, got %q for %q", got, line)
		}
	}
}

/* --------------------------------------------------------------------------
   extractDisplayText — Gemini
   ------------------------------------------------------------------------ */

func TestExtractDisplayText_Gemini_AssistantMessageWithContentArray(t *testing.T) {
	line := `{"type":"message","role":"assistant","content":[{"type":"text","text":"Hi"}]}`
	got := extractDisplayText("gemini", line)
	if got != "Hi" {
		t.Errorf("got %q, want %q", got, "Hi")
	}
}

func TestExtractDisplayText_Gemini_AssistantMessageWithStringContent(t *testing.T) {
	// Older Gemini event shape used a plain string for content; both must work.
	line := `{"type":"message","role":"assistant","content":"Hi there"}`
	got := extractDisplayText("gemini", line)
	if got != "Hi there" {
		t.Errorf("got %q, want %q", got, "Hi there")
	}
}

func TestExtractDisplayText_Gemini_ModelRoleIsTreatedAsAssistant(t *testing.T) {
	// "model" is what the Google AI Studio / Vertex APIs use for the assistant
	// turn; the gemini CLI may surface either.
	line := `{"type":"message","role":"model","content":"From the model"}`
	got := extractDisplayText("gemini", line)
	if got != "From the model" {
		t.Errorf("got %q, want %q", got, "From the model")
	}
}

func TestExtractDisplayText_Gemini_SkipsUserRole(t *testing.T) {
	line := `{"type":"message","role":"user","content":"the original prompt"}`
	got := extractDisplayText("gemini", line)
	if got != "" {
		t.Errorf("user role must NOT echo back into stream output, got %q", got)
	}
}

func TestExtractDisplayText_Gemini_ToolUse(t *testing.T) {
	line := `{"type":"tool_use","tool_name":"ReadFile"}`
	got := extractDisplayText("gemini", line)
	if !strings.Contains(got, "ReadFile") {
		t.Errorf("got %q, want output to mention 'ReadFile'", got)
	}

	// Gemini sometimes uses "name" instead of "tool_name" — both must work.
	got2 := extractDisplayText("gemini", `{"type":"tool_use","name":"WriteFile"}`)
	if !strings.Contains(got2, "WriteFile") {
		t.Errorf("got %q, want output to mention 'WriteFile' (name field)", got2)
	}
}

func TestExtractDisplayText_Gemini_ErrorEventSurfacesMessage(t *testing.T) {
	line := `{"type":"error","message":"context window exceeded"}`
	got := extractDisplayText("gemini", line)
	if !strings.Contains(got, "context window exceeded") {
		t.Errorf("error event must surface message: got %q", got)
	}
}

/* --------------------------------------------------------------------------
   detectPromptFromJSON
   ------------------------------------------------------------------------ */

func TestDetectPromptFromJSON_Claude_PermissionRequest(t *testing.T) {
	line := `{"type":"permission_request","tool":"Bash","description":"Run rm -rf /"}`
	info := detectPromptFromJSON("claude", line)
	if info == nil || info.Type != "permission" {
		t.Fatalf("expected permission prompt, got %+v", info)
	}
	if !strings.Contains(info.Text, "rm -rf") {
		t.Errorf("description should be in prompt text, got %q", info.Text)
	}
}

func TestDetectPromptFromJSON_Claude_PermissionRequestNoDescriptionFallsBackToTool(t *testing.T) {
	line := `{"type":"permission_request","tool":"Bash"}`
	info := detectPromptFromJSON("claude", line)
	if info == nil {
		t.Fatal("expected non-nil prompt info")
	}
	if !strings.Contains(info.Text, "Bash") {
		t.Errorf("tool name should appear when description is empty, got %q", info.Text)
	}
}

func TestDetectPromptFromJSON_Codex_ApprovalRequest(t *testing.T) {
	line := `{"type":"approval_request","command":"rm /etc/passwd"}`
	info := detectPromptFromJSON("codex", line)
	if info == nil || info.Type != "permission" {
		t.Fatalf("expected permission prompt, got %+v", info)
	}
	if !strings.Contains(info.Text, "rm /etc/passwd") {
		t.Errorf("command should appear, got %q", info.Text)
	}
}

func TestDetectPromptFromJSON_Gemini_ToolCallApproval(t *testing.T) {
	line := `{"type":"toolCallApproval","toolName":"WriteFile"}`
	info := detectPromptFromJSON("gemini", line)
	if info == nil || info.Type != "permission" {
		t.Fatalf("expected permission prompt, got %+v", info)
	}
	if !strings.Contains(info.Text, "WriteFile") {
		t.Errorf("tool name should appear, got %q", info.Text)
	}
}

func TestDetectPromptFromJSON_NonPromptEventsReturnNil(t *testing.T) {
	cases := map[string]string{
		"claude assistant": `{"type":"assistant","message":{}}`,
		"codex item":       `{"type":"item.completed"}`,
		"gemini message":   `{"type":"message","role":"assistant"}`,
		"malformed":        `{"type":"permission_request"`,
		"non-JSON":         "plain text",
		"empty":            "",
	}
	for name, line := range cases {
		for _, cmd := range []string{"claude", "codex", "gemini"} {
			if info := detectPromptFromJSON(cmd, line); info != nil {
				t.Errorf("%s on %s: expected nil, got %+v", name, cmd, info)
			}
		}
	}
}

/* --------------------------------------------------------------------------
   shouldCloseStdinAfterStart — expanded coverage
   --------------------------------------------------------------------------
   The original test in session_test.go covered the basics. This one pins the
   contract that the stdin-close decision is driven ONLY by stdinPrompt
   (regardless of command), which is the v0.9.6 fix that lets codex exit.
   ------------------------------------------------------------------------ */

func TestShouldCloseStdinAfterStart_StdinPromptIsTheOnlySignal(t *testing.T) {
	// Anything with a non-empty stdinPrompt keeps stdin open (claude). Anything
	// with an empty stdinPrompt closes it (codex/gemini/cmd/powershell).
	cases := []struct {
		stdinPrompt string
		want        bool // want close?
	}{
		{stdinPrompt: "", want: true},
		{stdinPrompt: "hello", want: false},
		{stdinPrompt: " ", want: false}, // a single space counts as non-empty
		{stdinPrompt: "\n", want: false},
	}
	for _, tc := range cases {
		for _, cmd := range []string{"claude", "codex", "gemini", "powershell", "git", ""} {
			got := shouldCloseStdinAfterStart(cmd, tc.stdinPrompt)
			if got != tc.want {
				t.Errorf("shouldCloseStdinAfterStart(%q, %q) = %v, want %v",
					cmd, tc.stdinPrompt, got, tc.want)
			}
		}
	}
}

/* --------------------------------------------------------------------------
   helpers
   ------------------------------------------------------------------------ */

// mustContain fails the test if args is missing any of the expected substrings,
// in any order. Convenient for argument-builder tests where we don't care about
// position.
func mustContain(t *testing.T, args []string, expected ...string) {
	t.Helper()
	for _, want := range expected {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("args missing %q (got %v)", want, args)
		}
	}
}
