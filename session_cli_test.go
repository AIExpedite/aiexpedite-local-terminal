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
     2. Strip any user-supplied -p / --print — interactive sessions must NEVER
        run in print mode, or claude exits after the first turn and the kickoff
        → session.sendInput handoff breaks.
     3. Recognise valued flags (--model X, --system-prompt X, etc.) and keep
        the value tied to the flag — otherwise the value gets misparsed as a
        prompt word.
     4. NEVER append -p (interactive multi-turn requires keeping claude alive
        between sendInput cycles).
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
	)
	for _, a := range args {
		if a == "-p" || a == "--print" {
			t.Errorf("-p/--print must never appear in interactive args: %v", args)
			break
		}
	}
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

func TestBuildClaudeInteractiveArgs_StripsUserSuppliedPrintFlag(t *testing.T) {
	// -p / --print are stripped on the interactive path — claude in print mode
	// exits after the first turn, killing the kickoff → sendInput handoff.
	args, prompt := buildClaudeInteractiveArgs([]string{"-p", "hello"})
	if prompt != "hello" {
		t.Errorf("prompt = %q, want %q", prompt, "hello")
	}
	for _, a := range args {
		if a == "-p" || a == "--print" {
			t.Errorf("-p/--print must be stripped from interactive args, got: %v", args)
		}
	}
}

func TestBuildClaudeInteractiveArgs_StripsPrintFlagVariants(t *testing.T) {
	// All of these forms must be removed from argv — leaving any of them
	// would put claude into print/one-shot mode (kills multi-turn) and, after
	// 2026-06-15, divert billing to the Agent SDK credit pool.
	cases := []struct {
		name       string
		input      []string
		wantPrompt string
	}{
		{"short flag leading", []string{"-p", "hello"}, "hello"},
		{"long flag leading", []string{"--print", "hello"}, "hello"},
		{"short equals form", []string{"-p=hello", "world"}, "hello world"},
		{"long equals form", []string{"--print=hello", "world"}, "hello world"},
		{"trailing short", []string{"hello", "-p"}, "hello"},
		{"trailing long", []string{"hello", "--print"}, "hello"},
		{"mixed with valued flag", []string{"--model", "sonnet", "-p", "design auth"}, "design auth"},
		{"long equals only", []string{"--print="}, ""},
		{"short equals only preserves prompt", []string{"-p=hello"}, "hello"},
		{"long equals only preserves prompt", []string{"--print=hello world"}, "hello world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, prompt := buildClaudeInteractiveArgs(tc.input)
			if prompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q (args=%v)", prompt, tc.wantPrompt, args)
			}
			for _, a := range args {
				if a == "-p" || a == "--print" ||
					strings.HasPrefix(a, "-p=") || strings.HasPrefix(a, "--print=") {
					t.Errorf("print-flag variant %q leaked into argv: %v", a, args)
				}
			}
		})
	}

	// Case sensitivity: claude's own CLI is case-sensitive on flag names,
	// so "-P" / "--PRINT" are NOT the print flag and must pass through
	// unchanged (they'll surface as an unknown-flag error from claude
	// itself, which is the correct user-visible failure mode).
	t.Run("uppercase variants pass through", func(t *testing.T) {
		args, _ := buildClaudeInteractiveArgs([]string{"-P", "--PRINT", "hello"})
		foundP, foundPrint := false, false
		for _, a := range args {
			if a == "-P" {
				foundP = true
			}
			if a == "--PRINT" {
				foundPrint = true
			}
		}
		if !foundP || !foundPrint {
			t.Errorf("uppercase -P / --PRINT must pass through, got %v", args)
		}
	})
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
	mustContain(t, args, "--output-format", "stream-json")
	for _, a := range args {
		if a == "-p" || a == "--print" {
			t.Errorf("-p/--print must never appear in interactive args: %v", args)
		}
	}
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
   Gemini receives the prompt via STDIN in its default INTERACTIVE mode (no -p)
   rather than as a positional argv — the switch fixes the Windows cmd.exe
   8191-char command-line cap that bit multi-KB kickoff briefs ("The command
   line is too long.", exit 1). The prompt is returned as stdinPrompt; argv
   carries only flags plus our fixed -o stream-json / --approval-mode auto_edit.
   No -p/--print: gemini's interactive mode reads the piped stdin as the prompt
   (verified against gemini 0.32.1).
   ------------------------------------------------------------------------ */

func TestBuildGeminiInteractiveArgs_PromptGoesToStdin(t *testing.T) {
	args, prompt := buildGeminiInteractiveArgs([]string{"explain the auth flow"})
	if prompt != "explain the auth flow" {
		t.Errorf("gemini stdinPrompt = %q, want %q", prompt, "explain the auth flow")
	}
	// The prompt MUST NOT appear on argv (that's the whole point of the fix).
	for _, a := range args {
		if a == "explain the auth flow" {
			t.Errorf("prompt leaked onto argv: %v", args)
		}
	}
	// stream-json flags present; NO -p (interactive mode, per the chosen style).
	mustContain(t, args, "-o", "stream-json", "--approval-mode", "auto_edit")
	for _, a := range args {
		if a == "-p" || a == "--prompt" {
			t.Errorf("gemini must run interactive (no -p/--print), got %v", args)
		}
	}
}

func TestBuildGeminiInteractiveArgs_PreservesUserFlags(t *testing.T) {
	args, prompt := buildGeminiInteractiveArgs([]string{"explain", "--model", "gemini-3-pro"})
	if prompt != "explain" {
		t.Errorf("gemini stdinPrompt = %q, want %q", prompt, "explain")
	}
	// User flag + its value stay on argv (and the value is NOT misread as prompt).
	mustContain(t, args, "--model", "gemini-3-pro", "-o", "stream-json", "--approval-mode", "auto_edit")
}

func TestBuildGeminiInteractiveArgs_EmptyArgs(t *testing.T) {
	// No prompt → empty stdinPrompt, but the stream-json flags must still be
	// present so gemini boots in stream-json mode.
	args, prompt := buildGeminiInteractiveArgs([]string{})
	if prompt != "" {
		t.Errorf("empty input should yield empty stdinPrompt, got %q", prompt)
	}
	mustContain(t, args, "-o", "stream-json", "--approval-mode", "auto_edit")
}

// Regression: a user-supplied `-p "..."` must be converted into stdinPrompt
// and stripped from argv. In interactive mode we never want ANY `-p` on argv —
// a stray `-p` would switch gemini to headless mode and collide with the
// interactive piped-stdin contract.
func TestBuildGeminiInteractiveArgs_UserPromptFlagRoutedToStdin(t *testing.T) {
	args, prompt := buildGeminiInteractiveArgs([]string{"-p", "explain the auth flow"})
	if prompt != "explain the auth flow" {
		t.Errorf("gemini stdinPrompt = %q, want %q", prompt, "explain the auth flow")
	}
	// NO `-p`/`--prompt` may appear on argv (interactive mode).
	for _, a := range args {
		if a == "-p" || a == "--prompt" {
			t.Errorf("`-p`/`--prompt` must be stripped from argv (interactive mode), got %v", args)
		}
		if a == "explain the auth flow" {
			t.Errorf("prompt leaked onto argv: %v", args)
		}
	}
}

func TestBuildGeminiInteractiveArgs_UserPromptEqualsFormRoutedToStdin(t *testing.T) {
	// `--prompt=foo` is a single token; the value still has to end up on stdin
	// rather than argv. Same for `-p=foo`.
	for _, in := range [][]string{
		{"--prompt=explain"},
		{"-p=explain"},
	} {
		args, prompt := buildGeminiInteractiveArgs(in)
		if prompt != "explain" {
			t.Errorf("input=%v: stdinPrompt = %q, want %q", in, prompt, "explain")
		}
		for _, a := range args {
			if a == "--prompt=explain" || a == "-p=explain" {
				t.Errorf("input=%v: prompt-equals token leaked onto argv: %v", in, args)
			}
		}
	}
}

// User-supplied `-p` plus positional prompt parts should both end up on stdin
// (joined), not silently dropped.
func TestBuildGeminiInteractiveArgs_UserPromptFlagAndPositionalCombine(t *testing.T) {
	args, prompt := buildGeminiInteractiveArgs([]string{"-p", "first", "second"})
	if prompt != "first second" {
		t.Errorf("stdinPrompt = %q, want %q (args=%v)", prompt, "first second", args)
	}
}

// Regression for P2 review on PR #33: Gemini's `--worktree`/`-w` is a string
// option (https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/cli-reference.md).
// Without marking it as value-taking, `gemini --worktree fix-branch "do the task"`
// would keep only `--worktree` on argv and fold `fix-branch` into the stdin
// prompt — gemini receives an empty worktree value and a polluted prompt.
func TestBuildGeminiInteractiveArgs_PreservesWorktreeFlag(t *testing.T) {
	for _, flag := range []string{"--worktree", "-w"} {
		t.Run(flag, func(t *testing.T) {
			args, prompt := buildGeminiInteractiveArgs([]string{flag, "fix-branch", "do the task"})
			if prompt != "do the task" {
				t.Errorf("prompt = %q, want %q (args=%v)", prompt, "do the task", args)
			}
			mustContain(t, args, flag, "fix-branch")
			if strings.Contains(prompt, "fix-branch") {
				t.Errorf("worktree value leaked into stdinPrompt %q", prompt)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   buildCodexInteractiveArgs
   --------------------------------------------------------------------------
   Codex receives the prompt via stdin (`-` placeholder) rather than as a
   positional argv. The switch from argv to stdin fixes the Windows
   CreateProcess ~32KB cmdline cap that bit multi-KB review briefs.
   ------------------------------------------------------------------------ */

func TestBuildCodexInteractiveArgs_PromptGoesToStdin(t *testing.T) {
	args, prompt := buildCodexInteractiveArgs([]string{"review the diff"})
	if prompt != "review the diff" {
		t.Errorf("codex stdinPrompt = %q, want %q", prompt, "review the diff")
	}
	// args must end with `-` so codex reads the prompt from stdin.
	if len(args) == 0 || args[len(args)-1] != "-" {
		t.Errorf("codex args must end with `-` placeholder, got %v", args)
	}
	mustContain(t, args, "exec", "--json", "--dangerously-bypass-approvals-and-sandbox")
}

func TestBuildCodexInteractiveArgs_EmptyArgsStillEndsWithDash(t *testing.T) {
	// Defensive: even with no prompt parts we still want the canonical args
	// shape (`exec --json … -`) so codex surfaces a clean error rather than
	// being launched in some other mode.
	args, prompt := buildCodexInteractiveArgs([]string{})
	if prompt != "" {
		t.Errorf("empty args should yield empty stdinPrompt, got %q", prompt)
	}
	if len(args) == 0 || args[len(args)-1] != "-" {
		t.Errorf("codex args must end with `-` placeholder even on empty input, got %v", args)
	}
}

func TestBuildCodexInteractiveArgs_PreservesValuedFlags(t *testing.T) {
	// --model takes the next arg as its value; that value must NOT leak into
	// the stdin prompt as a prompt word.
	args, prompt := buildCodexInteractiveArgs([]string{"--model", "o3", "review the diff"})
	if prompt != "review the diff" {
		t.Errorf("prompt = %q, want %q (args=%v)", prompt, "review the diff", args)
	}
	mustContain(t, args, "--model", "o3")
}

func TestBuildCodexInteractiveArgs_PreservesShortValuedFlag(t *testing.T) {
	// -m is the short alias for --model. NOTE: prompt is intentionally NOT
	// "review" — that would collide with the `codex exec review` subcommand
	// (which codex itself would interpret as a subcommand call, not a prompt).
	args, prompt := buildCodexInteractiveArgs([]string{"-m", "o3", "summarize"})
	if prompt != "summarize" {
		t.Errorf("prompt = %q, want %q (args=%v)", prompt, "summarize", args)
	}
	mustContain(t, args, "-m", "o3")
}

func TestBuildCodexInteractiveArgs_StripsDuplicateJsonFlag(t *testing.T) {
	// sanitizeCodexExecArgs drops user-supplied --json (we always add it).
	args, _ := buildCodexInteractiveArgs([]string{"--json", "hello"})
	jsonCount := 0
	for _, a := range args {
		if a == "--json" {
			jsonCount++
		}
	}
	if jsonCount != 1 {
		t.Errorf("expected exactly one --json in args, got %d (%v)", jsonCount, args)
	}
}

// Regression for P1 review on PR #28: `codex exec` documents additional
// value-taking options beyond `--model` / `--config`. If those values get
// reclassified as prompt words, codex will either reject the flag (missing
// value) or silently drop a critical setting.
func TestBuildCodexInteractiveArgs_PreservesAllValuedFlags(t *testing.T) {
	cases := []struct {
		name string
		flag string
		val  string
	}{
		{"output-last-message-long", "--output-last-message", "/tmp/last.txt"},
		{"output-last-message-short", "-o", "/tmp/last.txt"},
		{"output-schema", "--output-schema", "/tmp/schema.json"},
		{"add-dir", "--add-dir", "/workspace/extra"},
		{"profile-long", "--profile", "dev"},
		{"profile-short", "-p", "dev"},
		{"local-provider", "--local-provider", "lmstudio"},
		{"color", "--color", "never"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, prompt := buildCodexInteractiveArgs([]string{tc.flag, tc.val, "review the diff"})
			if prompt != "review the diff" {
				t.Errorf("prompt = %q, want %q (args=%v)", prompt, "review the diff", args)
			}
			mustContain(t, args, tc.flag, tc.val)
			// The value must NOT show up in the stdin prompt.
			if strings.Contains(prompt, tc.val) {
				t.Errorf("value %q leaked into stdinPrompt %q", tc.val, prompt)
			}
		})
	}
}

// Regression for P1 review on PR #28: `codex exec` accepts `resume` / `review`
// / `help` subcommands. Treating those positional tokens as prompt words
// destroys the call — codex sees the subcommand's own flags at the top level
// and either errors or runs a fresh session instead of resuming.
func TestBuildCodexInteractiveArgs_PreservesResumeSubcommand(t *testing.T) {
	args, prompt := buildCodexInteractiveArgs([]string{"resume", "--last", "follow-up"})
	// Subcommand path keeps everything in argv; no stdin routing.
	if prompt != "" {
		t.Errorf("subcommand path must not produce stdinPrompt, got %q (args=%v)", prompt, args)
	}
	if len(args) > 0 && args[len(args)-1] == "-" {
		t.Errorf("subcommand path must NOT append `-` placeholder, got %v", args)
	}
	mustContain(t, args, "exec", "resume", "--last", "follow-up")
	// Order check: `resume` must precede `--last` so codex routes the flag
	// under the subcommand parser, not the top-level exec parser.
	resumeIdx := argIndex(args, "resume")
	lastIdx := argIndex(args, "--last")
	if resumeIdx < 0 || lastIdx < 0 || resumeIdx > lastIdx {
		t.Errorf("expected `resume` before `--last`, got %v", args)
	}
}

func TestBuildCodexInteractiveArgs_PreservesResumeSessionAndPrompt(t *testing.T) {
	args, prompt := buildCodexInteractiveArgs([]string{"resume", "abc-123", "follow-up text"})
	if prompt != "" {
		t.Errorf("subcommand path must not produce stdinPrompt, got %q", prompt)
	}
	// Both session id and prompt must remain in argv, in order.
	mustContain(t, args, "resume", "abc-123", "follow-up text")
	resumeIdx := argIndex(args, "resume")
	idIdx := argIndex(args, "abc-123")
	promptIdx := argIndex(args, "follow-up text")
	if !(resumeIdx < idIdx && idIdx < promptIdx) {
		t.Errorf("expected order resume < session_id < prompt, got %v", args)
	}
}

func TestBuildCodexInteractiveArgs_PreservesReviewSubcommand(t *testing.T) {
	args, prompt := buildCodexInteractiveArgs([]string{"review", "--uncommitted"})
	if prompt != "" {
		t.Errorf("subcommand path must not produce stdinPrompt, got %q", prompt)
	}
	mustContain(t, args, "exec", "review", "--uncommitted")
}

func TestBuildCodexInteractiveArgs_PromptWordResumeNotMistakenForSubcommand(t *testing.T) {
	// If the FIRST positional isn't a known subcommand, "resume" appearing
	// later in the prompt must be treated as prompt text, not a subcommand.
	args, prompt := buildCodexInteractiveArgs([]string{"please", "resume", "the", "discussion"})
	if prompt != "please resume the discussion" {
		t.Errorf("prompt = %q, want %q (args=%v)", prompt, "please resume the discussion", args)
	}
	if len(args) == 0 || args[len(args)-1] != "-" {
		t.Errorf("non-subcommand path must end with `-`, got %v", args)
	}
}

/* --------------------------------------------------------------------------
   buildAntigravityInteractiveArgs
   --------------------------------------------------------------------------
   agy is claude-code-shaped on its flag surface but ships without
   --output-format / stream-json input, and (verified against agy 1.0.4) it
   does NOT read a piped stdin — so the prompt stays a POSITIONAL argv and we
   run a one-shot `--print`. agy resolves to a native agy.exe (not a cmd.exe
   shim), so it is launched via CreateProcess (~32KB cap) and is not subject to
   gemini's 8191-char cmd.exe cap. Single return value (no stdinPrompt).
   ------------------------------------------------------------------------ */

func TestBuildAntigravityInteractiveArgs_PrintAndPrompt(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"hello"})
	mustContain(t, args, "--print", "--dangerously-skip-permissions", "hello")
}

func TestBuildAntigravityInteractiveArgs_EmptyArgs(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{})
	mustContain(t, args, "--print", "--dangerously-skip-permissions")
}

// agy gets every arg on argv (prompt + any flags), in order, after the two
// leading flags. Nothing is reclassified or routed to stdin (agy ignores it).
func TestBuildAntigravityInteractiveArgs_PassesArgsThroughPositional(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"--add-dir", "../shared-library", "do the task"})
	mustContain(t, args, "--print", "--dangerously-skip-permissions", "--add-dir", "../shared-library", "do the task")
}

/* --------------------------------------------------------------------------
   stdinPromptFormat
   --------------------------------------------------------------------------
   Centralises per-CLI stdin protocol so StartSession's write path doesn't
   hard-code claude's NDJSON envelope against every stdinPrompt.
   ------------------------------------------------------------------------ */

func TestStdinPromptFormat(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"claude", "ndjson"},
		{"claude.exe", "ndjson"},
		{"Claude", "ndjson"},
		{"codex", "plain"},
		{"codex.cmd", "plain"},
		{"CODEX", "plain"},
		{"gemini", "plain"},     // gemini routes the prompt via stdin (interactive, no -p)
		{"gemini.cmd", "plain"}, // normalize the Windows npm shim suffix too
		{"agy", ""},             // antigravity keeps argv (it ignores piped stdin); no stdin prompt
		{"agy.exe", ""},         // native agy.exe — still positional, no stdin prompt
		{"powershell", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := stdinPromptFormat(tc.cmd)
		if got != tc.want {
			t.Errorf("stdinPromptFormat(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

/* --------------------------------------------------------------------------
   buildInteractiveCLIArgs (router)
   --------------------------------------------------------------------------
   Each CLI has its own stdinPrompt contract:
     - claude: prompt goes via NDJSON on stdin
     - codex:  prompt goes via raw stdin (`-` placeholder)
     - gemini: prompt goes via raw stdin (interactive mode, no -p)
     - agy:    prompt stays positional (agy ignores piped stdin)
     - other:  passes through verbatim
   ------------------------------------------------------------------------ */

func TestBuildInteractiveCLIArgs_RoutesByCommand(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		_, prompt := buildInteractiveCLIArgs("claude", []string{"hello"})
		if prompt != "hello" {
			t.Errorf("claude prompt = %q, want %q", prompt, "hello")
		}
	})
	t.Run("codex", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("codex", []string{"hello"})
		if prompt != "hello" {
			t.Errorf("codex stdinPrompt = %q, want %q", prompt, "hello")
		}
		if len(args) == 0 || args[len(args)-1] != "-" {
			t.Errorf("codex args must end with `-` placeholder, got %v", args)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("gemini", []string{"hello"})
		if prompt != "hello" {
			t.Errorf("gemini stdinPrompt = %q, want %q (prompt now goes via stdin)", prompt, "hello")
		}
		// Prompt must be off argv; interactive stream-json flags present, no -p.
		for _, a := range args {
			if a == "hello" {
				t.Errorf("gemini prompt must NOT be on argv, got %v", args)
			}
			if a == "-p" || a == "--prompt" {
				t.Errorf("gemini must run interactive (no -p), got %v", args)
			}
		}
		mustContain(t, args, "-o", "stream-json")
	})
	t.Run("antigravity", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("agy", []string{"hello"})
		if prompt != "" {
			t.Errorf("agy stdinPrompt MUST be empty (agy ignores piped stdin; prompt goes on argv): %q", prompt)
		}
		mustContain(t, args, "--print", "--dangerously-skip-permissions", "hello")
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
	t.Run("path-and-shim-route-like-isClaudeCommand", func(t *testing.T) {
		// Regression guard: the router and isClaudeCommand MUST classify
		// commands identically — otherwise an absolute path like
		// `/usr/local/bin/claude` or a Windows shim could get one half of the
		// claude treatment (stream-json shaping) without the other (billing
		// strip), or vice-versa.
		for _, cmd := range []string{
			"/usr/local/bin/claude",
			`C:\Users\u\AppData\Roaming\npm\claude.cmd`,
			"./claude",
			"claude-edge",
		} {
			_, prompt := buildInteractiveCLIArgs(cmd, []string{"hi"})
			if prompt == "" {
				t.Errorf("buildInteractiveCLIArgs(%q) did not route to claude (empty stdinPrompt)", cmd)
			}
			if !isClaudeCommand(cmd) {
				t.Errorf("isClaudeCommand(%q) = false, but router treated it as claude — predicates drifted", cmd)
			}
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

func TestExtractDisplayText_Claude_AssistantWholesaleRecapSkipped(t *testing.T) {
	// Claude launched with `--include-partial-messages` (which buildClaudeInteractiveArgs
	// always sets) emits BOTH the streaming content_block_delta events AND a
	// final wholesale `{type:"assistant",...}` recap of the same turn. The
	// streaming chunks cover everything that's in the recap — emitting both
	// produces the visible doubled output we saw in chat ("Octopuses have
	// three hearts… Octopuses have three hearts…"). Skip the recap entirely.
	cases := []string{
		// Original assistant-recap shape with text + tool_use blocks.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Part A"},{"type":"tool_use","name":"Read"},{"type":"text","text":"Part B"}]}}`,
		// Flat `{type:"message", role:"assistant"}` shape — same recap, different envelope.
		`{"type":"message","role":"assistant","content":[{"type":"text","text":"Recap"}]}`,
		// Empty / malformed recap shapes — still skipped.
		`{"type":"assistant","message":{"content":[]}}`,
		`{"type":"assistant"}`,
	}
	for _, line := range cases {
		got := extractDisplayText("claude", line)
		if got != "" {
			t.Errorf("expected empty (recap should be skipped) for %q, got %q", line, got)
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
   extractDisplayText — Grok
   --------------------------------------------------------------------------
   buildGrokInteractiveArgs forces `--output-format streaming-json`, which
   emits per-event NDJSON frames (text / thought / end). Without a grok
   branch in extractDisplayText, every line falls through to the default
   passthrough and the user sees raw `{"type":"text",...}` JSON in chat.
   ------------------------------------------------------------------------ */

func TestExtractDisplayText_Grok_TextFrameReturnsText(t *testing.T) {
	line := `{"type":"text","text":"hello from grok"}`
	got := extractDisplayText("grok", line)
	if got != "hello from grok" {
		t.Errorf("got %q, want %q", got, "hello from grok")
	}
}

func TestExtractDisplayText_Grok_SkipsThoughtAndEndAndMetadata(t *testing.T) {
	// thought / end / lifecycle frames are noise to the human reader. `end`
	// is also the detectCLITerminalEvent signal — surfacing it as display
	// text would emit a stray `{}` looking blob right before turn close.
	cases := []string{
		`{"type":"thought","text":"reasoning..."}`,
		`{"type":"end"}`,
		`{"type":"start"}`,
		`{"type":"init"}`,
		`{"type":"result"}`,
		`{"type":"tool_result"}`,
	}
	for _, line := range cases {
		got := extractDisplayText("grok", line)
		if got != "" {
			t.Errorf("expected empty for %q, got %q", line, got)
		}
	}
}

func TestExtractDisplayText_Grok_ToolUseSurfacesName(t *testing.T) {
	// Grok's tool-use frames carry the tool name under either `name` or
	// `tool_name` across versions; both must work, mirroring gemini.
	got := extractDisplayText("grok", `{"type":"tool_use","name":"Bash"}`)
	if !strings.Contains(got, "Bash") {
		t.Errorf("got %q, want output to mention 'Bash'", got)
	}
	got2 := extractDisplayText("grok", `{"type":"tool_use","tool_name":"ReadFile"}`)
	if !strings.Contains(got2, "ReadFile") {
		t.Errorf("got %q, want output to mention 'ReadFile'", got2)
	}
}

func TestExtractDisplayText_Grok_ErrorEventSurfacesMessage(t *testing.T) {
	line := `{"type":"error","message":"rate limited"}`
	got := extractDisplayText("grok", line)
	if !strings.Contains(got, "rate limited") {
		t.Errorf("error event must surface message: got %q", got)
	}
}

func TestExtractDisplayText_Grok_NonJsonPassthrough(t *testing.T) {
	got := extractDisplayText("grok", "error: unrecognized subcommand 'a'")
	if got != "error: unrecognized subcommand 'a'" {
		t.Errorf("plain text should passthrough, got %q", got)
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
   Contract (post-claude-stays-open fix):
   - Claude: ALWAYS keep stdin open. Sessions are interactive and the
     orchestrator wants to SendInput follow-ups regardless of whether an
     initial prompt was queued. Closing stdin makes claude EOF and exit in
     ~3s — the prod failure mode for `terminal({command: "claude"})` with
     empty args observed in chatDefault test runs.
   - Codex AND gemini: ALWAYS close — both read the stdin-piped prompt to
     EOF before/at inference start, so leaving stdin open hangs them.
   - Everything else (powershell, git, ...): close stdin when stdinPrompt is
     empty.
   ------------------------------------------------------------------------ */

func TestShouldCloseStdinAfterStart_ClaudeAlwaysOpen_OthersGatedByPrompt(t *testing.T) {
	cases := []struct {
		cmd         string
		stdinPrompt string
		want        bool // want close?
	}{
		// Claude — always open regardless of prompt (multi-turn stream-json
		// keeps stdin open between SendInput calls).
		{cmd: "claude", stdinPrompt: "", want: false},
		{cmd: "claude", stdinPrompt: "hello", want: false},
		{cmd: "claude", stdinPrompt: " ", want: false},
		{cmd: "claude", stdinPrompt: "\n", want: false},
		// Normalize: claude.exe, claude.cmd, claude.ps1 should match too.
		{cmd: "claude.exe", stdinPrompt: "", want: false},
		{cmd: "Claude", stdinPrompt: "", want: false},
		// Codex — always close. codex exec reads stdin to completion before
		// inference, so we close right after writing the prompt; otherwise
		// codex hangs waiting for EOF. Behaviour is identical whether the
		// prompt is empty (will error out cleanly) or non-empty (the new
		// stdin-piped path).
		{cmd: "codex", stdinPrompt: "", want: true},
		{cmd: "codex", stdinPrompt: "review the diff", want: true},
		{cmd: "codex.cmd", stdinPrompt: "review", want: true},
		{cmd: "CODEX", stdinPrompt: "review", want: true},
		// Gemini — always close (one-shot; reads the stdin-piped prompt to EOF,
		// same as codex). Behaviour is identical whether the prompt is empty or
		// the new stdin-piped path.
		{cmd: "gemini", stdinPrompt: "", want: true},
		{cmd: "gemini", stdinPrompt: "hello", want: true},
		{cmd: "gemini.cmd", stdinPrompt: "review", want: true},
		// Path-routed claude/codex/gemini — same policy must apply when the
		// caller supplied an absolute or relative path. Otherwise the argv
		// builder shapes a stdin-fed codex/gemini session but stdin is left
		// open and the child hangs waiting for EOF.
		{cmd: "/opt/claude-nightly/claude", stdinPrompt: "", want: false},
		{cmd: `C:\tools\claude.cmd`, stdinPrompt: "hi", want: false},
		{cmd: "/opt/bin/codex", stdinPrompt: "review", want: true},
		{cmd: `C:\tools\gemini.cmd`, stdinPrompt: "review", want: true},
		{cmd: "./codex", stdinPrompt: "", want: true},
		// Shells / non-CLI: legacy rule — close iff empty prompt.
		{cmd: "powershell", stdinPrompt: "", want: true},
		{cmd: "git", stdinPrompt: "", want: true},
		{cmd: "", stdinPrompt: "", want: true},
	}
	for _, tc := range cases {
		got := shouldCloseStdinAfterStart(tc.cmd, tc.stdinPrompt)
		if got != tc.want {
			t.Errorf("shouldCloseStdinAfterStart(%q, %q) = %v, want %v",
				tc.cmd, tc.stdinPrompt, got, tc.want)
		}
	}
}

/* --------------------------------------------------------------------------
   helpers
   ------------------------------------------------------------------------ */

// argIndex returns the position of needle in args, or -1 if not found. Local
// to the test file so it's available on every platform (the production
// indexOf in processes_windows.go is Windows-only).
func argIndex(args []string, needle string) int {
	for i, v := range args {
		if v == needle {
			return i
		}
	}
	return -1
}

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
