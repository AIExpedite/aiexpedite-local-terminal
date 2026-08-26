// session_cli_test.go
// -----------------------------------------------------------------------------
// Unit tests for the per-CLI argument builders, event detection, and display-text
// extraction used by the SessionManager when proxying claude / codex / antigravity / grok.
//
// This is the safety net that pins behaviour for the supported CLI agents:
//
//   - claude:  --output-format stream-json + --input-format stream-json + -p,
//              prompt sent as NDJSON on stdin, stdin held open until the
//              "result" event arrives, then closed so claude observes EOF and
//              exits.
//   - codex:   exec --json --dangerously-bypass-approvals-and-sandbox, prompt
//              as a positional arg, stdin closed at start (codex exec appends
//              piped stdin to the prompt; leaving the pipe open makes it wait
//              indefinitely for EOF).
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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
   agy ≥ 1.1.x: --print takes the prompt as its FLAG VALUE. Order must be
   --dangerously-skip-permissions --print <prompt>. Putting --print first
   makes agy treat --dangerously-skip-permissions as the prompt (tertiary
   Review symptom). No stdin path (agy ignores piped input).
   ------------------------------------------------------------------------ */

func TestBuildAntigravityInteractiveArgs_PrintAndPrompt(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"hello"})
	// Exact shape: permission skip, then --print <prompt> as flag value.
	if len(args) != 3 ||
		args[0] != "--dangerously-skip-permissions" ||
		args[1] != "--print" ||
		args[2] != "hello" {
		t.Fatalf("want [--dangerously-skip-permissions --print hello], got %#v", args)
	}
}

func TestBuildAntigravityInteractiveArgs_EmptyArgs(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{})
	// No prompt at all → omit --print (distinct from explicit empty below).
	if len(args) != 1 || args[0] != "--dangerously-skip-permissions" {
		t.Fatalf("want [--dangerously-skip-permissions], got %#v", args)
	}
}

// Explicit empty prompt must still emit --print "" so agy stays one-shot
// (not interactive TUI). Covers args [""], --print=, and bare --print.
func TestBuildAntigravityInteractiveArgs_ExplicitEmptyPrint(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{"empty string arg", []string{""}},
		{"print equals empty", []string{"--print="}},
		{"prompt equals empty", []string{"--prompt="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := buildAntigravityInteractiveArgs(tc.in)
			if len(args) != 3 ||
				args[0] != "--dangerously-skip-permissions" ||
				args[1] != "--print" ||
				args[2] != "" {
				t.Fatalf("want [skip --print \"\"], got %#v", args)
			}
		})
	}
}

// A *bare* trailing --print is different from an explicitly empty one: agy's
// string flag has no value, so the CLI exits with `flag needs an argument:
// -print` (verified on 1.1.11). Inventing `--print ""` would turn the caller's
// error into a permission-skipping empty-prompt run, so the argv passes through
// untouched — which, unlike omitting --print, cannot drop agy into the TUI.
func TestBuildAntigravityInteractiveArgs_BarePrintKeepsItsError(t *testing.T) {
	for _, in := range [][]string{{"--print"}, {"-p"}, {"--prompt"}, {"--print", "hi", "--print"}} {
		got := buildAntigravityInteractiveArgs(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want it unchanged", in, got)
		}
	}
	orig := `agy --print`
	_, shaped := shapePTYExecArgs("bash", []string{"-c", orig})
	if shaped[1] != orig {
		t.Errorf("shell-wrapped bare --print = %q, want it unchanged", shaped[1])
	}
	// ...but a print flag whose value merely looks like a flag is a valid
	// one-shot: agy takes the next token as the value, so `agy --print --print`
	// has the prompt "--print" and must still be shaped.
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{[]string{"--print", "--print"}, []string{"--dangerously-skip-permissions", "--print", "--print"}},
		{[]string{"-p", "-p"}, []string{"--dangerously-skip-permissions", "--print", "-p"}},
		{[]string{"--add-dir", "/repo", "--print", "--prompt"}, []string{"--dangerously-skip-permissions", "--add-dir", "/repo", "--print", "--prompt"}},
	} {
		got := buildAntigravityInteractiveArgs(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// agy rejects an equals-form boolean it cannot parse — `agy --continue=maybe
// review` exits with `invalid boolean value "maybe" for -continue` on 1.1.11 —
// so the safety strip must not swallow the token and run the prompt anyway.
func TestBuildAntigravityInteractiveArgs_KeepsInvalidBoolEqualsError(t *testing.T) {
	for _, in := range [][]string{
		{"--continue=maybe", "review"},
		{"-c=yes-please", "review"},
		{"--dangerously-skip-permissions=nope", "review"},
	} {
		got := buildAntigravityInteractiveArgs(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want it unchanged", in, got)
		}
	}
	// Valid boolean spellings still strip (Go accepts 1/0/t/f/TRUE/...).
	for _, in := range [][]string{{"--continue=1", "review"}, {"-c=false", "review"}} {
		got := buildAntigravityInteractiveArgs(in)
		want := []string{"--dangerously-skip-permissions", "--print", "review"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want %#v", in, got, want)
		}
	}
	orig := `agy --continue=maybe review`
	_, shaped := shapePTYExecArgs("bash", []string{"-c", orig})
	if shaped[1] != orig {
		t.Errorf("shell-wrapped invalid bool = %q, want it unchanged", shaped[1])
	}
	// Past the prompt boundary the same text is prompt material, not a flag —
	// Go's parsing already stopped at `review` — so it must not block shaping.
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{
			[]string{"review", "--continue=maybe"},
			[]string{"--dangerously-skip-permissions", "--print", "review --continue=maybe"},
		},
		{
			[]string{"review", "--print"},
			[]string{"--dangerously-skip-permissions", "--print", "review --print"},
		},
	} {
		got := buildAntigravityInteractiveArgs(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	origAfterPrompt := `agy review --continue=maybe`
	_, shapedAfter := shapePTYExecArgs("bash", []string{"-c", origAfterPrompt})
	wantAfter := `agy --dangerously-skip-permissions --print 'review --continue=maybe'`
	if shapedAfter[1] != wantAfter {
		t.Errorf("post-prompt flag text = %q, want %q", shapedAfter[1], wantAfter)
	}
}

// Known leading flags stay on argv; the remainder becomes the single --print value.
func TestBuildAntigravityInteractiveArgs_LeadingFlagsThenPrompt(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"--add-dir", "../shared-library", "do the task"})
	if len(args) != 5 ||
		args[0] != "--dangerously-skip-permissions" ||
		args[1] != "--add-dir" ||
		args[2] != "../shared-library" ||
		args[3] != "--print" ||
		args[4] != "do the task" {
		t.Fatalf("want [skip --add-dir path --print prompt], got %#v", args)
	}
}

// A recognized valued flag with no operand must not be hoisted ahead of
// --print: `--conversation --print review` would make `--print` the
// conversation value and drop the one-shot entirely. It stays last instead.
func TestBuildAntigravityInteractiveArgs_ValuelessFlagStaysAfterPrint(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		{
			[]string{"--print", "review", "--conversation"},
			[]string{"--dangerously-skip-permissions", "--print", "review", "--conversation"},
		},
		{
			[]string{"--add-dir", "/repo", "--print", "review", "--model"},
			[]string{"--dangerously-skip-permissions", "--add-dir", "/repo", "--print", "review", "--model"},
		},
		{
			// No print at all: the flag is the whole argv, nothing to swallow.
			[]string{"--conversation"},
			[]string{"--dangerously-skip-permissions", "--conversation"},
		},
	} {
		got := buildAntigravityInteractiveArgs(tc.in)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// Same contract for the shell-wrapped reshaper: a value-less flag may not end
// up in front of --print in the rebuilt payload.
func TestShapePTYExecArgs_ValuelessFlagStaysAfterPrint(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy --print review --conversation`})
	want := `agy --dangerously-skip-permissions --print review --conversation`
	if args[1] != want {
		t.Errorf("shaped payload = %q, want %q", args[1], want)
	}
}

// The cross-chat-contamination guard strips --continue / -c. Go's flag package
// also accepts `-flag=x` for booleans, so the equals spellings must be stripped
// too — otherwise `-c=true` reaches agy and resumes an unrelated conversation.
func TestBuildAntigravityInteractiveArgs_StripsContinueEqualsForms(t *testing.T) {
	for _, in := range [][]string{
		{"-c=true", "--print", "hi"},
		{"--continue=true", "--print", "hi"},
		{"-c=1", "--print", "hi"},
	} {
		got := buildAntigravityInteractiveArgs(in)
		for _, a := range got {
			if strings.HasPrefix(a, "-c=") || strings.HasPrefix(a, "--continue=") {
				t.Errorf("buildAntigravityInteractiveArgs(%#v) forwarded %q (full %#v)", in, a, got)
			}
		}
	}
	// Same through the shell-wrapped reshaper.
	_, shaped := shapePTYExecArgs("bash", []string{"-c", `agy -c=true --print hi`})
	if strings.Contains(shaped[1], "-c=true") {
		t.Errorf("shaped payload forwarded -c=true: %q", shaped[1])
	}
	// Stripping the word must not discard an expansion the shell already ran:
	// bash gives `agy --continue="${x:=false}" "$x"` the prompt `false`, while
	// dropping the flag leaves x unset and the prompt empty (stub-agy diff on
	// 5.2.21). Those payloads decline rather than silently changing the prompt.
	for _, orig := range []string{
		`agy --continue="${x:=false}" "$x"`,
		`agy -c="${x:=1}" "$x"`,
	} {
		_, expanding := shapePTYExecArgs("bash", []string{"-c", orig})
		if expanding[1] != orig {
			t.Errorf("stripped expanding flag was reshaped: got %q, want original %q", expanding[1], orig)
		}
	}
}

// Diagnostic invocations must reach agy untouched: `agy --version` prints the
// version (1.1.11 locally, and that is how the native capability probe queries
// it), while shaping would emit `--print --version` and burn a model run.
func TestBuildAntigravityInteractiveArgs_PassesThroughDiagnostics(t *testing.T) {
	// The equals spellings are included so their errors survive too: on 1.1.11
	// `agy -v=true` reports an invalid value (it is an int flag, not a version
	// alias) and `agy --version=true` prints nothing — neither should become a
	// permission-skipping model run.
	for _, in := range [][]string{
		{"--version"}, {"-v"}, {"--help"}, {"-h"}, {"models"}, {"update"},
		{"-v=true"}, {"--version=true"}, {"--help=true"},
	} {
		got := buildAntigravityInteractiveArgs(in)
		if len(got) != 1 || got[0] != in[0] {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want it unchanged", in, got)
		}
		// Same through the shell-wrapped reshaper.
		orig := "agy " + in[0]
		_, shaped := shapePTYExecArgs("bash", []string{"-c", orig})
		if shaped[1] != orig {
			t.Errorf("shell-wrapped %q = %q, want it unchanged", orig, shaped[1])
		}
	}
	// agy pre-scans its whole command line for --version / --help / -h ahead of
	// Go's flag parsing, so they act from any position: on 1.1.12
	// `agy --model gemini --help` and even `agy explain the --help flag please`
	// print the usage banner, and `agy review -version` prints the version.
	// Reshaping those would launch a model run the caller never asked for.
	for _, in := range [][]string{
		{"--model", "gemini", "--help"},
		{"--model", "gemini", "--version"},
		{"review", "--help"},
		{"review", "-version"},
		{"explain", "the", "--help", "flag", "please"},
		{"review", "-h"},
	} {
		got := buildAntigravityInteractiveArgs(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want it unchanged", in, got)
		}
		orig := "agy " + strings.Join(in, " ")
		_, shaped := shapePTYExecArgs("bash", []string{"-c", orig})
		if shaped[1] != orig {
			t.Errorf("shell-wrapped %q = %q, want it unchanged", orig, shaped[1])
		}
	}
	// `-v` is an ordinary int flag, so `agy review -v now` really is a prompt.
	vPrompt := buildAntigravityInteractiveArgs([]string{"review", "-v", "now"})
	wantV := []string{"--dangerously-skip-permissions", "--print", "review -v now"}
	if !reflect.DeepEqual(vPrompt, wantV) {
		t.Errorf("prompt containing -v = %#v, want %#v", vPrompt, wantV)
	}
	// A near-miss spelling is prompt text (`agy review --versions` runs it).
	nearMiss := buildAntigravityInteractiveArgs([]string{"review", "--versions"})
	wantNearMiss := []string{"--dangerously-skip-permissions", "--print", "review --versions"}
	if !reflect.DeepEqual(nearMiss, wantNearMiss) {
		t.Errorf("near-miss spelling = %#v, want %#v", nearMiss, wantNearMiss)
	}

	// Multi-token invocations stay prompts — a brief may open with such a word.
	got := buildAntigravityInteractiveArgs([]string{"help", "me", "refactor"})
	want := []string{"--dangerously-skip-permissions", "--print", "help me refactor"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("multi-token prompt = %#v, want %#v", got, want)
	}
}

// The command word must keep its quoting. A path whose text contains shell
// metacharacters is literal when the caller quoted it, but emitting the raw
// value turned it into live syntax: verified in bash 5.2.21 that
// `/tmp/'$(touch /tmp/marker)'/agy review` runs the binary and leaves no
// marker, while the unquoted rebuild created one.
func TestShapePTYExecArgs_QuotesCommandWord(t *testing.T) {
	_, got := shapePTYExecArgs("bash", []string{"-c", `/tmp/'$(touch marker)'/agy review`})
	want := `'/tmp/$(touch marker)/agy' --dangerously-skip-permissions --print review`
	if got[1] != want {
		t.Errorf("command word = %q, want %q", got[1], want)
	}
	// An *unquoted* expansion in the command word must decline instead: with
	// BIN='/tmp/my dir', bash splits `$BIN/agy` and fails with
	// `/tmp/my: No such file or directory`, while quoting it on rebuild would
	// resolve the path and run agy with --dangerously-skip-permissions.
	origSplit := `$BIN/agy --print review`
	_, split := shapePTYExecArgs("bash", []string{"-c", origSplit})
	if split[1] != origSplit {
		t.Errorf("unquoted expanding command word was reshaped: got %q", split[1])
	}
	// A quoted expansion cannot split, so it still reshapes.
	_, quoted := shapePTYExecArgs("bash", []string{"-c", `"$BIN"/agy --print review`})
	wantQuoted := `"$BIN"/agy --dangerously-skip-permissions --print review`
	if quoted[1] != wantQuoted {
		t.Errorf("quoted expanding command word = %q, want %q", quoted[1], wantQuoted)
	}

	// Ordinary paths stay unquoted.
	for _, tc := range []struct{ orig, want string }{
		{`agy review`, `agy --dangerously-skip-permissions --print review`},
		{`/usr/local/bin/agy review`, `/usr/local/bin/agy --dangerously-skip-permissions --print review`},
	} {
		_, plain := shapePTYExecArgs("bash", []string{"-c", tc.orig})
		if plain[1] != tc.want {
			t.Errorf("plain command word = %q, want %q", plain[1], tc.want)
		}
	}
}

// Go's flag package treats `-flag` and `--flag` as the same option (verified on
// agy 1.1.12: `agy -model` reports `flag needs an argument: -model`), and `--`
// ends flag parsing and is dropped from the arguments.
func TestBuildAntigravityInteractiveArgs_SingleDashAndTerminator(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
	}{
		// Single-dash long flags are recognized; the caller's spelling is kept.
		{
			[]string{"-model", "gemini", "review"},
			[]string{"--dangerously-skip-permissions", "-model", "gemini", "--print", "review"},
		},
		{[]string{"-print", "review"}, []string{"--dangerously-skip-permissions", "--print", "review"}},
		{[]string{"-prompt", "review"}, []string{"--dangerously-skip-permissions", "--print", "review"}},
		// `--` is consumed, so the prompt is what follows it.
		{[]string{"--", "review"}, []string{"--dangerously-skip-permissions", "--print", "review"}},
		{[]string{"--", "review", "this"}, []string{"--dangerously-skip-permissions", "--print", "review this"}},
		{
			[]string{"-add-dir", "/repo", "--", "review"},
			[]string{"--dangerously-skip-permissions", "-add-dir", "/repo", "--print", "review"},
		},
		// The single-dash spelling of a stripped flag is still stripped.
		{[]string{"-continue", "review"}, []string{"--dangerously-skip-permissions", "--print", "review"}},
	} {
		got := buildAntigravityInteractiveArgs(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
	// Same through the shell-wrapped reshaper.
	for _, tc := range []struct{ orig, want string }{
		{`agy -model gemini review`, `agy --dangerously-skip-permissions -model gemini --print review`},
		{`agy -- review`, `agy --dangerously-skip-permissions --print review`},
		{`agy -print review`, `agy --dangerously-skip-permissions --print review`},
	} {
		_, shaped := shapePTYExecArgs("bash", []string{"-c", tc.orig})
		if shaped[1] != tc.want {
			t.Errorf("shaped %q = %q, want %q", tc.orig, shaped[1], tc.want)
		}
	}
	// After an explicit print value the terminator must stay: dropping it would
	// re-expose the trailing positionals to flag parsing and really select a
	// model.
	if got := buildAntigravityInteractiveArgs([]string{"--print", "review", "--", "--model", "gemini"}); !reflect.DeepEqual(
		got, []string{"--dangerously-skip-permissions", "--print", "review", "--", "--model", "gemini"}) {
		t.Errorf("terminator after explicit print = %#v", got)
	}
	_, shapedTerm := shapePTYExecArgs("bash", []string{"-c", `agy --print review -- --model gemini`})
	wantTerm := `agy --dangerously-skip-permissions --print review -- --model gemini`
	if shapedTerm[1] != wantTerm {
		t.Errorf("shell-wrapped terminator = %q, want %q", shapedTerm[1], wantTerm)
	}
	// An equals-form probe inside the leading flag region is answered (or
	// rejected) by agy: `-v=true` is an invalid value for its int flag, so the
	// invocation must reach agy instead of becoming a prompt.
	for _, in := range [][]string{
		{"--model", "gemini", "-v=true"},
		{"--model", "gemini", "--help=true"},
	} {
		if got := buildAntigravityInteractiveArgs(in); !reflect.DeepEqual(got, in) {
			t.Errorf("buildAntigravityInteractiveArgs(%#v) = %#v, want it unchanged", in, got)
		}
	}
	origProbe := `agy --model gemini -v=true`
	_, shapedProbe := shapePTYExecArgs("bash", []string{"-c", origProbe})
	if shapedProbe[1] != origProbe {
		t.Errorf("shell-wrapped equals probe = %q, want it unchanged", shapedProbe[1])
	}
	// A bare flag-shaped diagnostic in the leading flag region is agy's to
	// answer: `agy --model gemini -v` reports `flag needs an argument: -v`, so
	// it must not become a prompt. Bare words like `models` past the first flag
	// stay prompt text.
	if got := buildAntigravityInteractiveArgs([]string{"--model", "gemini", "-v"}); !reflect.DeepEqual(
		got, []string{"--model", "gemini", "-v"}) {
		t.Errorf("bare -v after flags = %#v, want it unchanged", got)
	}
	if got := buildAntigravityInteractiveArgs([]string{"--model", "gemini", "models"}); !reflect.DeepEqual(
		got, []string{"--dangerously-skip-permissions", "--model", "gemini", "--print", "models"}) {
		t.Errorf("bare word after flags = %#v", got)
	}
	// A payload ending in backslash-newline is a line continuation the shell
	// deletes, so the prompt is `review`. Trimming the payload before rewriting
	// used to leave a dangling backslash, which bash passes as an extra
	// argument (verified on 5.2.21).
	_, contShaped := shapePTYExecArgs("bash", []string{"-c", "agy review " + `\` + "\n"})
	wantCont := `agy --dangerously-skip-permissions --print review`
	if contShaped[1] != wantCont {
		t.Errorf("trailing continuation = %q, want %q", contShaped[1], wantCont)
	}
	// Short flags keep their meaning: -p is print, -c is the stripped continue.
	if got := buildAntigravityInteractiveArgs([]string{"-p", "review"}); !reflect.DeepEqual(
		got, []string{"--dangerously-skip-permissions", "--print", "review"}) {
		t.Errorf("-p prompt = %#v", got)
	}
}

// Regression: --print must NOT be followed by another flag (agy 1.1.x eats it as the prompt).
func TestBuildAntigravityInteractiveArgs_PrintValueIsNeverAFlag(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"review this design"})
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--print" || args[i] == "-p" || args[i] == "--prompt" {
			if strings.HasPrefix(args[i+1], "-") {
				t.Fatalf("--print value must not be a flag token: %#v", args)
			}
		}
	}
}

// Multi-token prompt joins into one --print value (not truncated at first word).
func TestBuildAntigravityInteractiveArgs_JoinsMultiTokenPrompt(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"fix", "the", "bug"})
	if len(args) != 3 || args[2] != "fix the bug" {
		t.Fatalf("want single --print value %q, got %#v", "fix the bug", args)
	}
}

// A brief that starts with "-" is still prompt text (not a flag).
func TestBuildAntigravityInteractiveArgs_DashPrefixedPrompt(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"---", "title"})
	if len(args) != 3 || args[1] != "--print" || args[2] != "--- title" {
		t.Fatalf("dash-prefixed brief must be --print value, got %#v", args)
	}
}

// agy flag matching is case-sensitive: --PRINT / -P are not --print / -p and
// must stay part of the prompt rather than being treated as control syntax.
func TestBuildAntigravityInteractiveArgs_CaseSensitiveFlags(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"--PRINT", "review"})
	if len(args) != 3 ||
		args[0] != "--dangerously-skip-permissions" ||
		args[1] != "--print" ||
		args[2] != "--PRINT review" {
		t.Fatalf("uppercase --PRINT must be prompt text, got %#v", args)
	}

	// Known lowercase flags still peel.
	ok := buildAntigravityInteractiveArgs([]string{"--print", "review"})
	if len(ok) != 3 || ok[1] != "--print" || ok[2] != "review" {
		t.Fatalf("lowercase --print must still peel, got %#v", ok)
	}
}

func TestBuildAntigravityInteractiveArgs_PreservesTokensAfterExplicitPrint(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"--print", "review", "--PRINT"})
	want := []string{"--dangerously-skip-permissions", "--print", "review", "--PRINT"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("trailing unknown token was dropped: got %#v, want %#v", args, want)
	}
}

/* --------------------------------------------------------------------------
   shapePTYExecArgs
   --------------------------------------------------------------------------
   A tty=true `execute` request routed to the PTY path must get the SAME
   one-shot argv shaping StartSession applies, otherwise agy/antigravity starts
   its interactive TUI and hangs until the prompt timeout instead of returning
   a result. Only antigravity needs shaping; other eligible commands pass
   through untouched.
   ------------------------------------------------------------------------ */

func TestShapePTYExecArgs_ShapesAntigravity(t *testing.T) {
	// Direct agy execute: --dangerously-skip-permissions --print <prompt>
	// (agy 1.1.x: --print takes the prompt as its value).
	cmd, args := shapePTYExecArgs("agy", []string{"fix the bug"})
	if cmd != "agy" {
		t.Errorf("shapePTYExecArgs command = %q, want agy", cmd)
	}
	if len(args) != 3 ||
		args[0] != "--dangerously-skip-permissions" ||
		args[1] != "--print" ||
		args[2] != "fix the bug" {
		t.Fatalf("shaped argv = %#v, want [skip --print fix the bug]", args)
	}

	// The `antigravity` alias is shaped identically.
	_, aliasArgs := shapePTYExecArgs("antigravity", []string{"do it"})
	if len(aliasArgs) != 3 || aliasArgs[1] != "--print" || aliasArgs[2] != "do it" {
		t.Fatalf("alias shaped argv = %#v", aliasArgs)
	}

	// Robust to an explicit path / .exe suffix (isAntigravityCommand normalizes).
	_, pathArgs := shapePTYExecArgs("/usr/local/bin/agy", []string{"go"})
	if len(pathArgs) != 3 || pathArgs[2] != "go" {
		t.Fatalf("path shaped argv = %#v", pathArgs)
	}
}

func TestShapePTYExecArgs_ShapesShellWrappedAntigravity(t *testing.T) {
	// A shell-wrapped single-agent payload (`bash -c "agy …"`) keeps the shell as
	// its base command, but the inner agy must still gain the one-shot shaping —
	// otherwise it drops into the interactive TUI and hangs until the prompt
	// timeout. isPTYEligibleCommand already cleared it of shell chaining.
	cmd, args := shapePTYExecArgs("bash", []string{"-c", "agy --brief x"})
	if cmd != "bash" || len(args) != 2 || args[0] != "-c" {
		t.Fatalf("shapePTYExecArgs reshaped shell wrapper: cmd=%q args=%v", cmd, args)
	}
	// Unknown leading tokens after agy become the --print value (quoted: space).
	// No unescaped $ / ` → literal rebuild prefers single quotes.
	want := `agy --dangerously-skip-permissions --print '--brief x'`
	if args[1] != want {
		t.Errorf("shell-wrapped payload = %q, want %q", args[1], want)
	}

	// A bare `bash -c "agy"` with no prompt still gains the permission flag
	// (no bare --print — that would steal the next token as its value).
	_, bareArgs := shapePTYExecArgs("bash", []string{"-c", "agy"})
	if bareArgs[1] != "agy --dangerously-skip-permissions" {
		t.Errorf("bare shell-wrapped agy = %q", bareArgs[1])
	}

	// The `antigravity` alias inside a shell wrapper is shaped identically.
	// Multi-word prompt is quoted so bash -c re-parse keeps it as one --print value.
	_, aliasArgs := shapePTYExecArgs("bash", []string{"-c", "antigravity do it"})
	wantAlias := `antigravity --dangerously-skip-permissions --print 'do it'`
	if aliasArgs[1] != wantAlias {
		t.Errorf("shell-wrapped antigravity = %q, want %q", aliasArgs[1], wantAlias)
	}
}

// Shell-quoted / escaped prompts must be evaluated before rebuild so the model
// does not receive literal quote or backslash characters that bash would strip.
func TestShapePTYExecArgs_ShellWrappedPreservesQuoteSemantics(t *testing.T) {
	// bash -c 'agy "fix the bug"' → prompt value is fix the bug (no quote chars).
	// No unescaped $ / ` → literal rebuild prefers single quotes.
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy "fix the bug"`})
	wantMulti := `agy --dangerously-skip-permissions --print 'fix the bug'`
	if args[1] != wantMulti {
		t.Errorf("double-quoted prompt = %q, want %q", args[1], wantMulti)
	}

	// bash -c 'agy fix\ bug' → backslash escape yields "fix bug" (one space word).
	_, escArgs := shapePTYExecArgs("bash", []string{"-c", `agy fix\ bug`})
	wantEsc := `agy --dangerously-skip-permissions --print 'fix bug'`
	if escArgs[1] != wantEsc {
		t.Errorf("backslash-escaped prompt = %q, want %q", escArgs[1], wantEsc)
	}

	// Single quotes: agy 'fix the bug' → literal rebuild prefers single quotes.
	_, sqArgs := shapePTYExecArgs("bash", []string{"-c", `agy 'fix the bug'`})
	wantSQ := `agy --dangerously-skip-permissions --print 'fix the bug'`
	if sqArgs[1] != wantSQ {
		t.Errorf("single-quoted prompt = %q, want %q", sqArgs[1], wantSQ)
	}

	// Control operators must be re-quoted on rebuild. After shellWords strips the
	// protective single quotes from `agy 'review;id'`, literal rebuild uses
	// single quotes so bash does not treat `;` as a command separator.
	_, semiArgs := shapePTYExecArgs("bash", []string{"-c", `agy 'review;id'`})
	wantSemi := `agy --dangerously-skip-permissions --print 'review;id'`
	if semiArgs[1] != wantSemi {
		t.Errorf("semicolon prompt = %q, want %q", semiArgs[1], wantSemi)
	}

	// Other control / glob metacharacters also force quoting (literal path).
	_, pipeArgs := shapePTYExecArgs("bash", []string{"-c", `agy 'a|b&c'`})
	wantPipe := `agy --dangerously-skip-permissions --print 'a|b&c'`
	if pipeArgs[1] != wantPipe {
		t.Errorf("pipe/amp prompt = %q, want %q", pipeArgs[1], wantPipe)
	}
}

// Parameter expansion after a literal prompt boundary must survive rebuild.
// A leading expansion is left unshaped because it can evaluate to an agy flag.
func TestShapePTYExecArgs_PreservesParameterExpansion(t *testing.T) {
	origLeading := `agy "$TASK"`
	_, leading := shapePTYExecArgs("bash", []string{"-c", origLeading})
	if leading[1] != origLeading {
		t.Errorf("leading expanding word was reshaped: got %q, want original %q", leading[1], origLeading)
	}

	// A later expansion in an implicit prompt is no safer: agy pre-scans its
	// whole argv for --help / --version, so an expansion anywhere in it can
	// change what the command does once folded into a single --print value.
	origLater := `agy review "$TASK"`
	_, later := shapePTYExecArgs("bash", []string{"-c", origLater})
	if later[1] != origLater {
		t.Errorf("later expanding word was reshaped: got %q, want original %q", later[1], origLater)
	}

	// An explicit print operand cannot be reclassified as a flag.
	_, explicit := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK"`})
	wantExplicit := `agy --dangerously-skip-permissions --print "$TASK"`
	if explicit[1] != wantExplicit {
		t.Errorf("explicit expanding print = %q, want %q", explicit[1], wantExplicit)
	}

	// Single-quoted $ stays literal (prefer single quotes on rebuild).
	_, litArgs := shapePTYExecArgs("bash", []string{"-c", `agy '$TASK'`})
	wantLit := `agy --dangerously-skip-permissions --print '$TASK'`
	if litArgs[1] != wantLit {
		t.Errorf("literal $ prompt = %q, want %q", litArgs[1], wantLit)
	}
}

func TestShapePTYExecArgs_DeclinesExpansionBeforePromptBoundary(t *testing.T) {
	for _, orig := range []string{
		`agy "$FLAG" review`,
		`agy "$FLAG"`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("leading flag-capable expansion was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// A literal positional stops Go's flag parsing, but not agy's argv pre-scan
	// for --help / --version: with HELP=--help, `agy review "$HELP"` prints the
	// usage banner while the folded `--print "review --help"` runs the prompt
	// (verified on 1.1.12). So later expansions decline as well.
	origPost := `agy review "$FLAG"`
	_, post := shapePTYExecArgs("bash", []string{"-c", origPost})
	if post[1] != origPost {
		t.Errorf("post-boundary expansion was reshaped: got %q, want original %q", post[1], origPost)
	}
	// The explicit form keeps its shaping: the expansion is already its own
	// argv token there, so the rebuild reproduces the caller's layout.
	_, explicitPost := shapePTYExecArgs("bash", []string{"-c", `agy --print "$FLAG"`})
	wantExplicitPost := `agy --dangerously-skip-permissions --print "$FLAG"`
	if explicitPost[1] != wantExplicitPost {
		t.Errorf("explicit expanding print = %q, want %q", explicitPost[1], wantExplicitPost)
	}
}

// Escaped command substitutions must stay inert on rebuild:
// agy "\$(touch /tmp/pwn)" must NOT become --print "$(touch /tmp/pwn)".
func TestShapePTYExecArgs_KeepsEscapedCommandSubstitutionLiteral(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy "\$(touch /tmp/pwn)"`})
	want := `agy --dangerously-skip-permissions --print '$(touch /tmp/pwn)'`
	if args[1] != want {
		t.Errorf("escaped substitution = %q, want %q", args[1], want)
	}

	// Unquoted escaped form without spaces (space would split shell words).
	// Parentheses must also be escaped or shellWords rejects unquoted ().
	_, uArgs := shapePTYExecArgs("bash", []string{"-c", `agy \$\(cmd\)`})
	wantU := `agy --dangerously-skip-permissions --print '$(cmd)'`
	if uArgs[1] != wantU {
		t.Errorf("unquoted escaped substitution = %q, want %q", uArgs[1], wantU)
	}

	// Escaped $(...) followed by a later legitimate expansion in the same
	// double-quoted region must not OR Expand over the escaped dollars.
	_, mixArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "\$(touch /tmp/pwn)$TASK"`})
	wantMix := `agy --dangerously-skip-permissions --print '$(touch /tmp/pwn)'"$TASK"`
	if mixArgs[1] != wantMix {
		t.Errorf("escaped+expand mix = %q, want %q", mixArgs[1], wantMix)
	}

	// Reverse order: expand first, then escaped substitution in the same region.
	_, revArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK\$(touch /tmp/pwn)"`})
	wantRev := `agy --dangerously-skip-permissions --print "$TASK"'$(touch /tmp/pwn)'`
	if revArgs[1] != wantRev {
		t.Errorf("expand+escaped mix = %q, want %q", revArgs[1], wantRev)
	}
}

// --print=value and valued flags must retain per-segment expand metadata.
func TestShapePTYExecArgs_PrintEqualsAndFlagSegments(t *testing.T) {
	// Equals form with concatenated expand + literal single-quoted $(...).
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy --print="$TASK"'$(touch /tmp/pwn)'`})
	want := `agy --dangerously-skip-permissions --print "$TASK"'$(touch /tmp/pwn)'`
	if args[1] != want {
		t.Errorf("print= segments = %q, want %q", args[1], want)
	}

	// Valued flag operand with mixed segments; prompt is plain "task".
	_, flagArgs := shapePTYExecArgs("bash", []string{"-c", `agy --add-dir "$ROOT"'$(touch /tmp/pwn)' task`})
	wantFlag := `agy --dangerously-skip-permissions --add-dir "$ROOT"'$(touch /tmp/pwn)' --print task`
	if flagArgs[1] != wantFlag {
		t.Errorf("flag segments = %q, want %q", flagArgs[1], wantFlag)
	}
}

// Mixed expand fragments must not OR Expand into one double-quoted blob:
// agy prefix '$(touch /tmp/pwn)' must keep the single-quoted substitution inert.
func TestShapePTYExecArgs_PreservesPerFragmentExpansion(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy prefix '$(touch /tmp/pwn)'`})
	// No expanding fragment → join then single-quote the whole prompt.
	want := `agy --dangerously-skip-permissions --print 'prefix $(touch /tmp/pwn)'`
	if args[1] != want {
		t.Errorf("mixed literal prompt = %q, want %q", args[1], want)
	}

	// Expanding + literal-with-$ uses adjacent quote concatenation. Implicit
	// prompts containing an expansion now decline, so the serializer is
	// exercised through the explicit --print form (the trailing literal word
	// stays on argv as a trailing token, exactly as agy would receive it).
	_, mixArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK" '$(touch /tmp/pwn)'`})
	wantMix := `agy --dangerously-skip-permissions --print "$TASK" '$(touch /tmp/pwn)'`
	if mixArgs[1] != wantMix {
		t.Errorf("mixed expand/literal = %q, want %q", mixArgs[1], wantMix)
	}

	// Concatenated quote segments within ONE shell word (no space between):
	// "$TASK"'$(touch /tmp/pwn)' must not OR Expand across segments.
	_, concatArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK"'$(touch /tmp/pwn)'`})
	wantConcat := `agy --dangerously-skip-permissions --print "$TASK"'$(touch /tmp/pwn)'`
	if concatArgs[1] != wantConcat {
		t.Errorf("concatenated segments = %q, want %q", concatArgs[1], wantConcat)
	}

	// Lone `"$"` is not a bash expansion (literal `$`); concatenated with a
	// single-quoted suffix that would form $(…) if double-quoted as one blob.
	// Bash passes literal `$(touch /tmp/pwn)`; rebuild must keep it inert.
	_, dollarArgs := shapePTYExecArgs("bash", []string{"-c", `agy "$"'(touch /tmp/pwn)'`})
	wantDollar := `agy --dangerously-skip-permissions --print '$(touch /tmp/pwn)'`
	if dollarArgs[1] != wantDollar {
		t.Errorf("literal $ + paren suffix = %q, want %q", dollarArgs[1], wantDollar)
	}

	// Multi-word form of the same trap: must not flatten into
	// --print "$(touch /tmp/pwn) more" (live command substitution under PTY).
	_, dollarMore := shapePTYExecArgs("bash", []string{"-c", `agy "$"'(touch /tmp/pwn)' more`})
	wantDollarMore := `agy --dangerously-skip-permissions --print '$(touch /tmp/pwn) more'`
	if dollarMore[1] != wantDollarMore {
		t.Errorf("literal $ + paren + more = %q, want %q", dollarMore[1], wantDollarMore)
	}

	// Same for `$` + literal name suffix (must not become one double-quoted
	// `$TASKmore` which would expand a different variable name). Adjacent
	// unquoted more is fine — bash concatenates `"$TASK"more` → TASK+more.
	_, nameArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK"'more'`})
	wantName := `agy --dangerously-skip-permissions --print "$TASK"more`
	if nameArgs[1] != wantName {
		t.Errorf("expandable $TASK + literal more = %q, want %q", nameArgs[1], wantName)
	}

	// Multi-seg expand (`"$x"'y'`) must not flatten to `"$xy"` (wrong parameter
	// name). Per-segment quoting, via the explicit --print form.
	_, multiSegMore := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK"'x' more`})
	wantMultiSegMore := `agy --dangerously-skip-permissions --print "$TASK"x more`
	if multiSegMore[1] != wantMultiSegMore {
		t.Errorf("multi-seg expand + more = %q, want %q", multiSegMore[1], wantMultiSegMore)
	}
}

// Unmatched / unsupported unquoted parentheses must NOT be reshaped into a valid
// permission-skipping agy command — leave the original so bash reports the error.
func TestShapePTYExecArgs_RejectsUnmatchedShellGrouping(t *testing.T) {
	orig := `agy review)`
	_, args := shapePTYExecArgs("bash", []string{"-c", orig})
	if args[1] != orig {
		t.Errorf("unmatched ) was reshaped: got %q, want original %q", args[1], orig)
	}

	orig2 := `agy review(`
	_, args2 := shapePTYExecArgs("bash", []string{"-c", orig2})
	if args2[1] != orig2 {
		t.Errorf("unmatched ( was reshaped: got %q, want original %q", args2[1], orig2)
	}
}

// Unquoted glob / expanding-brace / leading-tilde expansion cannot be reshaped
// without losing bash semantics (rebuild would single-quote and freeze the
// metacharacters). Leave the original payload alone. Quoted forms, mid-word
// tilde, and non-expanding braces still reshape as literals.
func TestShapePTYExecArgs_RejectsUnquotedShellExpansion(t *testing.T) {
	for _, orig := range []string{
		`agy file{1,2}`,
		`agy review {a,b}`,
		`agy review *.go`,
		`agy review ~/proj`,
		`agy review file[ab]`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("unquoted expansion was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Unmatched '[' is literal in bash/dash (not a pathname pattern) — reshape
	// so the one-shot still gets --print rather than hanging interactive.
	_, openBracket := shapePTYExecArgs("bash", []string{"-c", `agy review [draft`})
	wantOpenBracket := `agy --dangerously-skip-permissions --print 'review [draft'`
	if openBracket[1] != wantOpenBracket {
		t.Errorf("unmatched [ prompt = %q, want %q", openBracket[1], wantOpenBracket)
	}
	// Lone ] is also literal.
	_, closeBracket := shapePTYExecArgs("bash", []string{"-c", `agy review draft]`})
	wantCloseBracket := `agy --dangerously-skip-permissions --print 'review draft]'`
	if closeBracket[1] != wantCloseBracket {
		t.Errorf("literal ] prompt = %q, want %q", closeBracket[1], wantCloseBracket)
	}
	// Quoted/escaped whitespace inside a bracket class stays one word and still
	// expands — leave unshaped (do not treat the space as a word boundary).
	for _, orig := range []string{
		`agy [a" "b]`,
		`agy [a\ b]`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("quoted-space bracket glob was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Degenerate closed classes (`[]`, `[!]`, `[^]`, `[]]`) are still pathname
	// patterns to bash: with failglob (verified on bash 5.2.21 via an exported
	// BASHOPTS) `agy [!]` dies with `no match: [!]`, and with nullglob the word
	// disappears. Reshaping them into `--print '[!]'` would run agy instead —
	// leave the payload unshaped so the shell keeps its behaviour.
	for _, orig := range []string{
		`agy []`,
		`agy [!]`,
		`agy [^]`,
		`agy []]`,
		`agy [!]x`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("degenerate bracket class was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Word-initial tilde with a quoted prefix is literal in bash/dash
	// (`~"root"` → ~root). Still reshape with --print rather than declining
	// as an expanding tilde. Fully unquoted ~/x stays unshaped (above).
	for _, tc := range []struct {
		orig, want string
	}{
		{`agy ~"root"`, `agy --dangerously-skip-permissions --print '~root'`},
		{`agy ~'user'`, `agy --dangerously-skip-permissions --print '~user'`},
		{`agy ~us"er"/x`, `agy --dangerously-skip-permissions --print '~user/x'`},
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", tc.orig})
		if args[1] != tc.want {
			t.Errorf("quoted tilde prefix %q = %q, want %q", tc.orig, args[1], tc.want)
		}
		_, dashArgs := shapePTYExecArgs("dash", []string{"-c", tc.orig})
		if dashArgs[1] != tc.want {
			t.Errorf("dash quoted tilde prefix %q = %q, want %q", tc.orig, dashArgs[1], tc.want)
		}
	}

	// Unknown login names stay literal in bash — reshape with --print rather
	// than declining and leaving interactive agy waiting.
	_, noUser := shapePTYExecArgs("bash", []string{"-c", `agy ~user_that_does_not_exist_xyzzy`})
	wantNoUser := `agy --dangerously-skip-permissions --print '~user_that_does_not_exist_xyzzy'`
	if noUser[1] != wantNoUser {
		t.Errorf("unknown tilde login = %q, want %q", noUser[1], wantNoUser)
	}
	// An unquoted ':' ends a word-initial tilde-prefix on bash/zsh/ksh, not just
	// in assignment values: `bash -c 'agy ~:x'` passes `$HOME:x` (verified on
	// bash 5.2.21). Reshaping to `--print '~:x'` would silently drop the
	// expansion — leave those unshaped.
	// (`~user:x` depends on the host's user database, so only the HOME forms are
	// pinned here.)
	for _, orig := range []string{
		`agy ~:suffix`,
		`agy ~:a:b`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("bash colon-terminated tilde was reshaped: got %q, want original %q", args[1], orig)
		}
	}
	// `bash +B` / `bash +o braceexpand` turn brace expansion off, so `{a,b}` is
	// literal (verified on 5.2.21) — reshape instead of declining.
	for _, pre := range [][]string{{"+B"}, {"+o", "braceexpand"}} {
		args := append(append([]string{}, pre...), "-c", `agy {a,b}`)
		got := shapeShellWrappedPTYArgs("bash", args)
		want := `agy --dangerously-skip-permissions --print '{a,b}'`
		if got[len(got)-1] != want {
			t.Errorf("bash %v brace = %q, want %q", pre, got[len(got)-1], want)
		}
	}
	// Single-letter options combine, and the `+` prefix disables: `bash +BH -c`
	// passes a literal `{a,b}` while `-BH` expands (verified on 5.2.21).
	compactOff := shapeShellWrappedPTYArgs("bash", []string{"+BH", "-c", `agy {a,b}`})
	wantCompactOff := `agy --dangerously-skip-permissions --print '{a,b}'`
	if compactOff[len(compactOff)-1] != wantCompactOff {
		t.Errorf("bash +BH brace = %q, want %q", compactOff[len(compactOff)-1], wantCompactOff)
	}
	compactOn := shapeShellWrappedPTYArgs("bash", []string{"-BH", "-c", `agy {a,b}`})
	if compactOn[len(compactOn)-1] != `agy {a,b}` {
		t.Errorf("bash -BH brace was reshaped: got %q", compactOn[len(compactOn)-1])
	}
	// `-f` / `-o noglob` disable pathname generation, so `*`, `?` and `[…]` are
	// literals and the payload can be reshaped (bash 5.2.21 and dash 0.5.12
	// both pass a literal `f*`). `-f +f` globs again — last wins.
	for _, tc := range []struct {
		shell string
		pre   []string
	}{
		{"bash", []string{"-f"}},
		{"bash", []string{"-o", "noglob"}},
		{"dash", []string{"-f"}},
	} {
		args := append(append([]string{}, tc.pre...), "-c", `agy review *.go`)
		got := shapeShellWrappedPTYArgs(tc.shell, args)
		want := `agy --dangerously-skip-permissions --print 'review *.go'`
		if got[len(got)-1] != want {
			t.Errorf("%s %v glob = %q, want %q", tc.shell, tc.pre, got[len(got)-1], want)
		}
	}
	globBack := shapeShellWrappedPTYArgs("bash", []string{"-f", "+f", "-c", `agy review *.go`})
	if globBack[len(globBack)-1] != `agy review *.go` {
		t.Errorf("bash -f +f glob was reshaped: got %q", globBack[len(globBack)-1])
	}
	// zsh's `-f` means "skip startup files", not noglob — `zsh -f -c` still
	// globs, so it must keep declining.
	zshDashF := shapeShellWrappedPTYArgs("zsh", []string{"-f", "-c", `agy review *.go`})
	if zshDashF[len(zshDashF)-1] != `agy review *.go` {
		t.Errorf("zsh -f glob was reshaped: got %q", zshDashF[len(zshDashF)-1])
	}
	// A startup file can undo the wrapper flag (`set -B` in $BASH_ENV makes
	// `bash +B -c` expand again on 5.2.21), so when one may run the disable is
	// not trustworthy — decline instead of freezing the braces.
	t.Setenv("BASH_ENV", "/tmp/startup.sh")
	braceStartup := shapeShellWrappedPTYArgs("bash", []string{"+B", "-c", `agy {a,b}`})
	if braceStartup[len(braceStartup)-1] != `agy {a,b}` {
		t.Errorf("bash +B with BASH_ENV was reshaped: got %q", braceStartup[len(braceStartup)-1])
	}
	// An inherited $SHELLOPTS listing braceexpand is reapplied after the
	// invocation flags, so `env SHELLOPTS=braceexpand bash +B -c` expands again
	// (5.2.21) and `+B` cannot be trusted. SHELLOPTS only turns options on, so
	// it never undoes `-f`.
	os.Unsetenv("BASH_ENV")
	t.Setenv("SHELLOPTS", "braceexpand:hashall")
	shelloptsBrace := shapeShellWrappedPTYArgs("bash", []string{"+B", "-c", `agy {a,b}`})
	if shelloptsBrace[len(shelloptsBrace)-1] != `agy {a,b}` {
		t.Errorf("bash +B with SHELLOPTS=braceexpand was reshaped: got %q", shelloptsBrace[len(shelloptsBrace)-1])
	}
	shelloptsGlob := shapeShellWrappedPTYArgs("bash", []string{"-f", "-c", `agy review *.go`})
	wantShelloptsGlob := `agy --dangerously-skip-permissions --print 'review *.go'`
	if shelloptsGlob[len(shelloptsGlob)-1] != wantShelloptsGlob {
		t.Errorf("bash -f with SHELLOPTS = %q, want %q", shelloptsGlob[len(shelloptsGlob)-1], wantShelloptsGlob)
	}
	// An inherited SHELLOPTS listing noglob turns globbing off with no `-f` at
	// all (`env SHELLOPTS=noglob bash -c 'printf "[%s]" f*'` prints `[f*]`), so
	// wildcard prompts become literals and reshape.
	t.Setenv("SHELLOPTS", "noglob:hashall")
	_, shelloptsNoGlob := shapePTYExecArgs("bash", []string{"-c", `agy review *.go`})
	wantNoGlob := `agy --dangerously-skip-permissions --print 'review *.go'`
	if shelloptsNoGlob[1] != wantNoGlob {
		t.Errorf("bash with SHELLOPTS=noglob = %q, want %q", shelloptsNoGlob[1], wantNoGlob)
	}
	// $SHELLOPTS is a bash variable — dash and zsh ignore it (both still glob
	// under SHELLOPTS=noglob), so it must not leak into their classification.
	for _, shell := range []string{"dash", "zsh"} {
		orig := `agy review *.go`
		_, ignored := shapePTYExecArgs(shell, []string{"-c", orig})
		if ignored[1] != orig {
			t.Errorf("%s with SHELLOPTS=noglob was reshaped: got %q", shell, ignored[1])
		}
	}
	os.Unsetenv("SHELLOPTS")
	t.Setenv("BASH_ENV", "/tmp/startup.sh")
	// Same caveat for `-f`: a startup file can `set +f` and put globbing back.
	globStartup := shapeShellWrappedPTYArgs("bash", []string{"-f", "-c", `agy review *.go`})
	if globStartup[len(globStartup)-1] != `agy review *.go` {
		t.Errorf("bash -f with BASH_ENV was reshaped: got %q", globStartup[len(globStartup)-1])
	}
	os.Unsetenv("BASH_ENV")
	// Groups take part in the last-wins state too.
	groupLastWins := shapeShellWrappedPTYArgs("bash", []string{"+BH", "-HB", "-c", `agy {a,b}`})
	if groupLastWins[len(groupLastWins)-1] != `agy {a,b}` {
		t.Errorf("bash +BH -HB brace was reshaped: got %q", groupLastWins[len(groupLastWins)-1])
	}
	// Wrapper options apply left to right, so the last one wins: `+B -B` still
	// expands (5.2.21 passes a and b) and `-o posix +o posix` is not POSIX.
	lastWins := shapeShellWrappedPTYArgs("bash", []string{"+B", "-B", "-c", `agy {a,b}`})
	if lastWins[len(lastWins)-1] != `agy {a,b}` {
		t.Errorf("bash +B -B brace was reshaped: got %q", lastWins[len(lastWins)-1])
	}
	posixOff := shapeShellWrappedPTYArgs("bash", []string{"-o", "posix", "+o", "posix", "-c", `agy HOME=~ review`})
	if posixOff[len(posixOff)-1] != `agy HOME=~ review` {
		t.Errorf("bash -o posix +o posix assignment tilde was reshaped: got %q", posixOff[len(posixOff)-1])
	}
	// A line continuation inside a brace sequence is deleted before expansion,
	// so `{1.\<newline>.3}` expands to 1 2 3 on bash 5.2.21 — decline.
	origSeq := "agy {1.\\\n.3}"
	_, contSeq := shapePTYExecArgs("bash", []string{"-c", origSeq})
	if contSeq[1] != origSeq {
		t.Errorf("continued brace sequence was reshaped: got %q, want original %q", contSeq[1], origSeq)
	}
	// Same for zsh MAGIC_EQUAL_SUBST split across a continuation.
	origMagicCont := "agy review foo=\\\n=ls"
	_, magicCont := shapePTYExecArgs("zsh", []string{"-c", origMagicCont})
	if magicCont[1] != origMagicCont {
		t.Errorf("continued magic-equals was reshaped: got %q, want original %q", magicCont[1], origMagicCont)
	}
	// Default bash still brace-expands — decline.
	origBrace := `agy {a,b}`
	_, braceOn := shapePTYExecArgs("bash", []string{"-c", origBrace})
	if braceOn[1] != origBrace {
		t.Errorf("bash brace expansion was reshaped: got %q", braceOn[1])
	}
	// An exported-but-empty HOME still expands on dash (`HOME= dash -c 'printf
	// "[%s]" ~ ~/x'` gives `[]` and `[/x]`), so presence decides, not value.
	t.Setenv("HOME", "")
	for _, orig := range []string{`agy ~`, `agy ~/x`} {
		_, emptyHome := shapePTYExecArgs("dash", []string{"-c", orig})
		if emptyHome[1] != orig {
			t.Errorf("dash tilde with empty HOME was reshaped: got %q, want original %q", emptyHome[1], orig)
		}
	}
	// Assignment-word tilde expansion is a bash extension that POSIX mode turns
	// off, so a `sh` wrapper passes `HOME=~` literally even when /bin/sh is
	// bash (verified with a sh symlink and `bash --posix` on 5.2.21) — reshape
	// rather than declining and leaving agy in the TUI.
	_, shAssign := shapePTYExecArgs("sh", []string{"-c", `agy HOME=~ review`})
	wantShAssign := `agy --dangerously-skip-permissions --print 'HOME=~ review'`
	if shAssign[1] != wantShAssign {
		t.Errorf("sh assignment tilde = %q, want %q", shAssign[1], wantShAssign)
	}
	// bash proper does expand it — decline there.
	origBashAssign := `agy HOME=~ review`
	_, bashAssign := shapePTYExecArgs("bash", []string{"-c", origBashAssign})
	if bashAssign[1] != origBashAssign {
		t.Errorf("bash assignment tilde was reshaped: got %q, want original %q", bashAssign[1], origBashAssign)
	}
	// ...unless the wrapper's own argv asks for POSIX mode, which disables the
	// extension the same way `sh` does (`bash --posix -c 'printf %s HOME=~'`
	// prints `HOME=~` on 5.2.21). Flags after -c belong to the payload.
	wantPosixAssign := `agy --dangerously-skip-permissions --print 'HOME=~ review'`
	for _, pre := range [][]string{{"--posix"}, {"-o", "posix"}} {
		args := append(append([]string{}, pre...), "-c", origBashAssign)
		got := shapeShellWrappedPTYArgs("bash", args)
		if got[len(got)-1] != wantPosixAssign {
			t.Errorf("bash %v assignment tilde = %q, want %q", pre, got[len(got)-1], wantPosixAssign)
		}
	}
	// POSIX mode also arrives through the environment ($POSIXLY_CORRECT, or an
	// inherited $SHELLOPTS listing posix) — both leave `HOME=~` literal on bash
	// 5.2.21, so both must reshape.
	for _, env := range []struct{ k, v string }{
		{"POSIXLY_CORRECT", "1"},
		{"SHELLOPTS", "braceexpand:posix"},
	} {
		t.Setenv(env.k, env.v)
		_, envPosix := shapePTYExecArgs("bash", []string{"-c", origBashAssign})
		if envPosix[1] != wantPosixAssign {
			t.Errorf("bash %s assignment tilde = %q, want %q", env.k, envPosix[1], wantPosixAssign)
		}
		// Unset rather than blank it: an exported-but-empty POSIXLY_CORRECT
		// still means POSIX mode, which is exactly what the next case relies on
		// NOT being in effect.
		os.Unsetenv(env.k)
	}
	// A $BASH_ENV startup file could `set -o posix` (making `HOME=~` literal) or
	// be a no-op (leaving it expanding to $HOME) — both verified on 5.2.21. That
	// is unknowable, so it resolves the same way as brace expansion and
	// globbing: assume the shell expands, and decline.
	t.Setenv("BASH_ENV", "/tmp/startup.sh")
	_, startupAssign := shapePTYExecArgs("bash", []string{"-c", origBashAssign})
	if startupAssign[1] != origBashAssign {
		t.Errorf("bash assignment tilde with BASH_ENV was reshaped: got %q", startupAssign[1])
	}
	os.Unsetenv("BASH_ENV")
	// An exported-but-empty POSIXLY_CORRECT is still POSIX mode on bash 5.2.21.
	t.Setenv("POSIXLY_CORRECT", "")
	_, emptyPosix := shapePTYExecArgs("bash", []string{"-c", origBashAssign})
	if emptyPosix[1] != wantPosixAssign {
		t.Errorf("bash empty POSIXLY_CORRECT = %q, want %q", emptyPosix[1], wantPosixAssign)
	}
	os.Unsetenv("POSIXLY_CORRECT")
	// Dash keeps ':' inside the login name, where it never resolves, so the
	// tilde stays literal there and the payload still reshapes.
	_, dashColon := shapePTYExecArgs("dash", []string{"-c", `agy ~:suffix`})
	wantDashColon := `agy --dangerously-skip-permissions --print '~:suffix'`
	if dashColon[1] != wantDashColon {
		t.Errorf("dash colon tilde = %q, want %q", dashColon[1], wantDashColon)
	}
	// zsh fails unknown ~user before invoking agy — leave unshaped so the
	// shell still errors instead of launching with a frozen name.
	origZshNoUser := `agy ~user_that_does_not_exist_xyzzy`
	_, zshNoUser := shapePTYExecArgs("zsh", []string{"-c", origZshNoUser})
	if zshNoUser[1] != origZshNoUser {
		t.Errorf("zsh unknown tilde was reshaped: got %q, want original %q", zshNoUser[1], origZshNoUser)
	}
	// Bash specials ~+ and stack index 0 (~+0 / ~-0) always expand — leave
	// unshaped. ~- expands only when OLDPWD is set (covered next).
	t.Setenv("OLDPWD", filepath.ToSlash(os.TempDir()))
	for _, orig := range []string{
		`agy ~+`,
		`agy ~-`,
		`agy ~+/x`,
		`agy ~-/x`,
		`agy ~+0`,
		`agy ~-0`,
		`agy ~+0/x`,
		`agy ~+00`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("bash tilde special was reshaped: got %q, want original %q", args[1], orig)
		}
	}
	// A backslash-newline inside the tilde-prefix is a line continuation the
	// shell deletes, so `~\<newline>/brief` expands to $HOME/brief on bash and
	// dash (verified on 5.2.21 / 0.5.12) — decline rather than freezing it.
	// Pin HOME so the dash path (which needs it to expand ~/…) matches on
	// Windows CI, where HOME is often unset.
	t.Setenv("HOME", filepath.ToSlash(os.TempDir()))
	for _, shell := range []string{"bash", "dash"} {
		orig := "agy ~\\\n/brief"
		_, args := shapePTYExecArgs(shell, []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("%s continued tilde was reshaped: got %q, want original %q", shell, args[1], orig)
		}
	}
	// A backslash before anything else still quotes the prefix (literal tilde).
	origEscaped := `agy ~\user_no_such/brief`
	_, escaped := shapePTYExecArgs("bash", []string{"-c", origEscaped})
	if escaped[1] == origEscaped {
		t.Errorf("bash escaped tilde prefix was declined: %q", escaped[1])
	}
	// Bash validates $OLDPWD at startup and clears anything that is not an
	// existing absolute directory, so `~-` stays literal for those (verified:
	// /no/such, a relative path, and a regular file all print `~-`).
	regularFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	for _, bad := range []string{"/no/such/dir", "relative/path", filepath.ToSlash(regularFile)} {
		t.Setenv("OLDPWD", bad)
		_, badOld := shapePTYExecArgs("bash", []string{"-c", `agy ~-`})
		wantBadOld := `agy --dangerously-skip-permissions --print '~-'`
		if badOld[1] != wantBadOld {
			t.Errorf("bash ~- with OLDPWD=%q = %q, want %q", bad, badOld[1], wantBadOld)
		}
	}
	// Empty/unset OLDPWD and no startup file: bash leaves ~- literal — still
	// reshape with --print.
	t.Setenv("OLDPWD", "")
	t.Setenv("BASH_ENV", "")
	t.Setenv("ENV", "")
	_, emptyOld := shapePTYExecArgs("bash", []string{"-c", `agy ~-`})
	wantEmptyOld := `agy --dangerously-skip-permissions --print '~-'`
	if emptyOld[1] != wantEmptyOld {
		t.Errorf("bash ~- without OLDPWD = %q, want %q", emptyOld[1], wantEmptyOld)
	}
	// A startup file runs before the payload and can export OLDPWD where this
	// process cannot see it (verified on bash 5.2.21:
	// `env -u OLDPWD BASH_ENV=… bash -c 'printf %s ~-'` → /var). Decline the
	// reshape whenever such a file may run: $BASH_ENV / $ENV for bash, and zsh
	// unconditionally (.zshenv).
	t.Setenv("BASH_ENV", "/tmp/env.sh")
	_, bashEnvOld := shapePTYExecArgs("bash", []string{"-c", `agy ~-`})
	if bashEnvOld[1] != `agy ~-` {
		t.Errorf("bash ~- with BASH_ENV was reshaped: got %q", bashEnvOld[1])
	}
	// A startup-file $BASH_ENV can also push onto the directory stack, making
	// non-zero `~+N` / `~-N` resolve (bash 5.2.21 with `pushd /tmp` in
	// BASH_ENV expands `~+1` to the pre-push directory). Decline those too.
	for _, orig := range []string{`agy ~+1`, `agy ~-1`, `agy ~+2/x`} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("bash %q with BASH_ENV was reshaped: got %q", orig, args[1])
		}
	}
	// bash invoked as `sh`, or with an explicit POSIX flag, ignores $BASH_ENV
	// (verified: `env -u OLDPWD BASH_ENV=… sh -c 'printf %s ~-'` and the same
	// through `bash --posix` both print a literal `~-`) — those still reshape.
	wantLiteralOld := `agy --dangerously-skip-permissions --print '~-'`
	_, shBashEnv := shapePTYExecArgs("sh", []string{"-c", `agy ~-`})
	if shBashEnv[1] != wantLiteralOld {
		t.Errorf("sh ~- with BASH_ENV = %q, want %q", shBashEnv[1], wantLiteralOld)
	}
	posixArgs := shapeShellWrappedPTYArgs("bash", []string{"--posix", "-c", `agy ~-`})
	if posixArgs[len(posixArgs)-1] != wantLiteralOld {
		t.Errorf("bash --posix ~- with BASH_ENV = %q, want %q", posixArgs[len(posixArgs)-1], wantLiteralOld)
	}
	t.Setenv("BASH_ENV", "")
	// Without a startup file the stack has only entry 0, so `~+1` is literal
	// (verified: `bash -c 'printf %s ~+1'` prints `~+1`) — reshape.
	_, stackOne := shapePTYExecArgs("bash", []string{"-c", `agy ~+1`})
	wantStackOne := `agy --dangerously-skip-permissions --print '~+1'`
	if stackOne[1] != wantStackOne {
		t.Errorf("bash ~+1 without startup file = %q, want %q", stackOne[1], wantStackOne)
	}
	// $ENV is not a bash startup-file signal: `env ENV=… bash -c`, a
	// bash-backed `sh -c`, and `bash --posix -c` all ignore it (POSIX sources
	// $ENV only for interactive shells), so these must still reshape.
	t.Setenv("ENV", "/tmp/env.sh")
	for _, shell := range []string{"bash", "sh"} {
		_, envOld := shapePTYExecArgs(shell, []string{"-c", `agy ~-`})
		wantEnvOld := `agy --dangerously-skip-permissions --print '~-'`
		if envOld[1] != wantEnvOld {
			t.Errorf("%s ~- with ENV = %q, want %q", shell, envOld[1], wantEnvOld)
		}
	}
	t.Setenv("ENV", "")
	_, zshOld := shapePTYExecArgs("zsh", []string{"-c", `agy ~-`})
	if zshOld[1] != `agy ~-` {
		t.Errorf("zsh ~- was reshaped: got %q", zshOld[1])
	}
	// dash sources nothing for `-c`, so an ambient $ENV must not change it.
	t.Setenv("ENV", "/tmp/env.sh")
	_, dashOld := shapePTYExecArgs("dash", []string{"-c", `agy ~-`})
	wantDashOld := `agy --dangerously-skip-permissions --print '~-'`
	if dashOld[1] != wantDashOld {
		t.Errorf("dash ~- with ENV = %q, want %q", dashOld[1], wantDashOld)
	}
	t.Setenv("ENV", "")
	// Dash leaves ~+ / ~- / ~+0 literal — still reshape with --print.
	for _, tc := range []struct {
		orig, want string
	}{
		{`agy ~+`, `agy --dangerously-skip-permissions --print '~+'`},
		{`agy ~-`, `agy --dangerously-skip-permissions --print '~-'`},
		{`agy ~+0`, `agy --dangerously-skip-permissions --print '~+0'`},
		{`agy ~-0`, `agy --dangerously-skip-permissions --print '~-0'`},
	} {
		_, args := shapePTYExecArgs("dash", []string{"-c", tc.orig})
		if args[1] != tc.want {
			t.Errorf("dash literal tilde special %q = %q, want %q", tc.orig, args[1], tc.want)
		}
	}
	// Non-zero ~+N / ~-N need a live directory-stack entry; fresh bash usually
	// has none, so leave literal and still reshape with --print.
	_, stackIdx := shapePTYExecArgs("bash", []string{"-c", `agy ~+1`})
	wantStackIdx := `agy --dangerously-skip-permissions --print '~+1'`
	if stackIdx[1] != wantStackIdx {
		t.Errorf("tilde stack index = %q, want %q", stackIdx[1], wantStackIdx)
	}

	// Quoted braces/globs are literal — safe to reshape with single quotes.
	_, qArgs := shapePTYExecArgs("bash", []string{"-c", `agy 'file{1,2}'`})
	wantQ := `agy --dangerously-skip-permissions --print 'file{1,2}'`
	if qArgs[1] != wantQ {
		t.Errorf("quoted brace prompt = %q, want %q", qArgs[1], wantQ)
	}

	// Non-expanding brace form `{foo}` is ordinary literal data.
	_, litBrace := shapePTYExecArgs("bash", []string{"-c", `agy review {foo}`})
	wantLitBrace := `agy --dangerously-skip-permissions --print 'review {foo}'`
	if litBrace[1] != wantLitBrace {
		t.Errorf("literal brace prompt = %q, want %q", litBrace[1], wantLitBrace)
	}

	// Invalid sequence endpoints stay literal in bash (`{foo..bar}`, `{1..x}`).
	for _, tc := range []struct{ in, want string }{
		{`agy {foo..bar}`, `agy --dangerously-skip-permissions --print '{foo..bar}'`},
		{`agy {1..x}`, `agy --dangerously-skip-permissions --print '{1..x}'`},
	} {
		_, got := shapePTYExecArgs("bash", []string{"-c", tc.in})
		if got[1] != tc.want {
			t.Errorf("invalid brace sequence %q = %q, want %q", tc.in, got[1], tc.want)
		}
	}

	// Valid numeric / letter sequences still expand — leave unshaped.
	// Zero increment uses bash's default step and still expands.
	for _, orig := range []string{
		`agy {1..3}`,
		`agy {a..c}`,
		`agy file{1..2}`,
		`agy {1..3..0}`,
		`agy {1..3..+1}`,
		`agy {+1..+3}`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("valid brace sequence was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Brace expansion spanning quoted segments (`{a,'b'}`) still expands in
	// bash — leave unshaped rather than freezing to '{a,b}'.
	for _, orig := range []string{
		`agy {a,'b'}`,
		`agy {a,"b"}`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("quoted-span brace expansion was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Mid-word tilde does not tilde-expand — reshape as a literal prompt.
	_, midTilde := shapePTYExecArgs("bash", []string{"-c", `agy review foo~bar`})
	wantMidTilde := `agy --dangerously-skip-permissions --print 'review foo~bar'`
	if midTilde[1] != wantMidTilde {
		t.Errorf("mid-word tilde prompt = %q, want %q", midTilde[1], wantMidTilde)
	}

	// Colon without a prior unquoted '=' is not an assignment-like tilde
	// position in bash (`foo:~` stays literal) — reshape safely.
	_, colonTilde := shapePTYExecArgs("bash", []string{"-c", `agy foo:~`})
	wantColonTilde := `agy --dangerously-skip-permissions --print 'foo:~'`
	if colonTilde[1] != wantColonTilde {
		t.Errorf("non-assignment colon-tilde = %q, want %q", colonTilde[1], wantColonTilde)
	}

	// Assignment-like tilde (`HOME=~`, `HOME+=~`, `PATH=…:~/x`) expands in
	// bash — leave unshaped. Compound += also activates tilde expansion.
	// PATH=~: ends the empty tilde-prefix at ':' (PATH=$HOME:) — same rule.
	for _, orig := range []string{
		`agy HOME=~`,
		`agy HOME+=~`,
		`agy PATH=~/bin:~/x`,
		`agy PATH=/bin:~/x`,
		`agy PATH=~:`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("assignment tilde was reshaped: got %q, want original %q", args[1], orig)
		}
	}
	// Assignment position with a non-expanding tilde prefix is literal in
	// bash (quoted login / unknown user) — still reshape with --print.
	for _, tc := range []struct{ in, want string }{
		{`agy HOME=~"root"`, `agy --dangerously-skip-permissions --print 'HOME=~root'`},
		{`agy PATH=/bin:~user_that_does_not_exist_xyzzy/x`, `agy --dangerously-skip-permissions --print 'PATH=/bin:~user_that_does_not_exist_xyzzy/x'`},
	} {
		_, got := shapePTYExecArgs("bash", []string{"-c", tc.in})
		if got[1] != tc.want {
			t.Errorf("literal assign-tilde %q = %q, want %q", tc.in, got[1], tc.want)
		}
	}

	// Dash does not expand assignment-position tilde — reshape as literals
	// so one-shots still get --print (do not hang interactive).
	for _, tc := range []struct{ in, want string }{
		{`agy HOME=~`, `agy --dangerously-skip-permissions --print 'HOME=~'`},
		{`agy PATH=/bin:~/x`, `agy --dangerously-skip-permissions --print 'PATH=/bin:~/x'`},
	} {
		_, got := shapePTYExecArgs("dash", []string{"-c", tc.in})
		if got[1] != tc.want {
			t.Errorf("dash assign-tilde %q = %q, want %q", tc.in, got[1], tc.want)
		}
	}
	// Word-initial ~/ still expands on dash when HOME is set (POSIX) — leave
	// unshaped. Pin HOME so Windows CI (HOME often unset) matches that path.
	t.Setenv("HOME", filepath.ToSlash(os.TempDir()))
	origHome := `agy review ~/proj`
	_, dashHome := shapePTYExecArgs("dash", []string{"-c", origHome})
	if dashHome[1] != origHome {
		t.Errorf("dash word-initial tilde was reshaped: got %q, want original %q", dashHome[1], origHome)
	}
	// Dash leaves bare ~ / ~/x literal when HOME is unavailable; preserve the
	// one-shot reshape in that environment. Bash falls back to passwd data.
	// Unset, not blank: dash expands an exported-but-empty HOME (`HOME= dash -c
	// 'printf "[%s]" ~ ~/x'` gives `[]` and `[/x]`), so blanking would exercise
	// the opposite path.
	os.Unsetenv("HOME")
	for _, tc := range []struct{ in, want string }{
		{`agy ~`, `agy --dangerously-skip-permissions --print '~'`},
		{`agy ~/x`, `agy --dangerously-skip-permissions --print '~/x'`},
	} {
		_, got := shapePTYExecArgs("dash", []string{"-c", tc.in})
		if got[1] != tc.want {
			t.Errorf("dash tilde without HOME %q = %q, want %q", tc.in, got[1], tc.want)
		}
	}
	_, bashHomeFallback := shapePTYExecArgs("bash", []string{"-c", `agy ~`})
	if bashHomeFallback[1] != `agy ~` {
		t.Errorf("bash bare tilde without HOME was reshaped: got %q", bashHomeFallback[1])
	}

	// Indexed assignment A[0]=~ also tilde-expands; unquoted '[' declines reshape.
	origIdx := `agy A[0]=~`
	_, idxArgs := shapePTYExecArgs("bash", []string{"-c", origIdx})
	if idxArgs[1] != origIdx {
		t.Errorf("indexed assignment tilde was reshaped: got %q, want original %q", idxArgs[1], origIdx)
	}

	// Quoted '=' is not an assignment separator — `'HOME='~` is literal HOME=~.
	_, qEqTilde := shapePTYExecArgs("bash", []string{"-c", `agy 'HOME='~`})
	wantQEqTilde := `agy --dangerously-skip-permissions --print 'HOME=~'`
	if qEqTilde[1] != wantQEqTilde {
		t.Errorf("quoted-eq tilde prompt = %q, want %q", qEqTilde[1], wantQEqTilde)
	}

	// Invalid, quoted, escaped, and non-assignment prefixes do not tilde-expand.
	for _, tc := range []struct{ in, want string }{
		{`agy 1foo=~`, `agy --dangerously-skip-permissions --print '1foo=~'`},
		{`agy 'HOME'=~`, `agy --dangerously-skip-permissions --print 'HOME=~'`},
		{`agy HOME\=~`, `agy --dangerously-skip-permissions --print 'HOME=~'`},
		{`agy HOME-=~`, `agy --dangerously-skip-permissions --print 'HOME-=~'`},
	} {
		_, got := shapePTYExecArgs("bash", []string{"-c", tc.in})
		if got[1] != tc.want {
			t.Errorf("non-assign tilde %q = %q, want %q", tc.in, got[1], tc.want)
		}
	}

	// Dash does not brace-expand — reshape `file{1,2}` as a literal prompt.
	_, dashBrace := shapePTYExecArgs("dash", []string{"-c", `agy file{1,2}`})
	wantDashBrace := `agy --dangerously-skip-permissions --print 'file{1,2}'`
	if dashBrace[1] != wantDashBrace {
		t.Errorf("dash brace prompt = %q, want %q", dashBrace[1], wantDashBrace)
	}

	// Bare `sh` is resolved like dollar-quote: bash-backed or ambiguous sh
	// still brace-expands, so expanding forms must stay unshaped (not frozen
	// as a single-quoted 'file{1,2}' prompt). Dash-backed sh reshapes.
	if shellSupportsBraceExpansion("sh") {
		origSh := `agy file{1,2}`
		_, shBrace := shapePTYExecArgs("sh", []string{"-c", origSh})
		if shBrace[1] != origSh {
			t.Errorf("bash-like sh brace was reshaped: got %q, want original %q", shBrace[1], origSh)
		}
	} else {
		_, shBrace := shapePTYExecArgs("sh", []string{"-c", `agy file{1,2}`})
		wantSh := `agy --dangerously-skip-permissions --print 'file{1,2}'`
		if shBrace[1] != wantSh {
			t.Errorf("dash-like sh brace prompt = %q, want %q", shBrace[1], wantSh)
		}
	}
}

// shellSupportsBraceExpansion("sh") must use the same resolution as
// shellSupportsDollarQuote — never hardcode every `sh` as non-expanding.
func TestShellSupportsBraceExpansion_ShMatchesDollarQuote(t *testing.T) {
	if shellSupportsBraceExpansion("dash") {
		t.Error("dash must not brace-expand")
	}
	if !shellSupportsBraceExpansion("bash") {
		t.Error("bash must brace-expand")
	}
	if shellSupportsBraceExpansion("sh") != shellSupportsDollarQuote("sh") {
		t.Errorf("sh brace=%v dollarQuote=%v; resolution must match",
			shellSupportsBraceExpansion("sh"), shellSupportsDollarQuote("sh"))
	}
}

// Unquoted expansions (single- or multi-fragment) must not be double-quoted on
// rebuild (word-split / pathname-expand / empty-expand semantics change).
// Double-quoted single-field expansions still reshape; multi-field forms
// (`"$@"`, `"${arr[@]}"`) leave the payload unshaped so --print cannot re-split.
func TestShapePTYExecArgs_RejectsUnquotedExpandPrompts(t *testing.T) {
	for _, orig := range []string{
		`agy $OPTIONAL`,
		`agy $OPTIONAL task`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("unquoted expand was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// A double-quoted expansion inside an implicit prompt declines, wherever it
	// sits: folding it into one --print value hides it from agy's argv pre-scan.
	origDQ := `agy review "$TASK" more`
	_, dq := shapePTYExecArgs("bash", []string{"-c", origDQ})
	if dq[1] != origDQ {
		t.Errorf("double-quoted multi-frag expand was reshaped: got %q, want original %q", dq[1], origDQ)
	}

	// Quoted multi-field expansions still split after --print — leave unshaped.
	// Nested forms (${x:+$@}) also yield multiple fields when the outer
	// expansion is active — decline those too. Nested ${@:1} needs balanced
	// brace matching (first-} would truncate the body and miss multi-word).
	// Bare indirect ${!ref} may resolve to @/* (ref=@ → multi fields).
	for _, orig := range []string{
		`agy "$@"`,
		`agy "${files[@]}"`,
		`agy "${!pre_@}"`,
		`agy "${!ref}"`,
		`agy --add-dir "$@" task`,
		`agy "$@"'more'`,
		`agy "${x:+$@}"`,
		`agy "${x:-${files[@]}}"`,
		`agy "${x:-${@:1}}"`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("multi-field quoted expand was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Literal "[@]" in an operator default is not multi-field — Bash yields a
	// single field (e.g. foo[@] when x is unset). Still reshape with --print.
	_, litAt := shapePTYExecArgs("bash", []string{"-c", `agy --print "${x:-foo[@]}"`})
	wantLitAt := `agy --dangerously-skip-permissions --print "${x:-foo[@]}"`
	if litAt[1] != wantLitAt {
		t.Errorf("literal [@] in PE default = %q, want %q", litAt[1], wantLitAt)
	}

	// zsh EQUALS: unquoted =cmd expands to the command path. Joining into a
	// single-quoted --print freezes it — leave unshaped on zsh only.
	origZsh := `agy =ls review`
	_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", origZsh})
	if zshArgs[1] != origZsh {
		t.Errorf("zsh equals sub was reshaped: got %q, want original %q", zshArgs[1], origZsh)
	}
	// bash leaves =ls literal — still reshape.
	_, bashEq := shapePTYExecArgs("bash", []string{"-c", origZsh})
	wantBashEq := `agy --dangerously-skip-permissions --print '=ls review'`
	if bashEq[1] != wantBashEq {
		t.Errorf("bash literal =ls = %q, want %q", bashEq[1], wantBashEq)
	}
	// An escaped leading equals is literal even on zsh, so the eligible
	// one-shot should still reshape. Bash follows the same literal path.
	// Tokenizer marks escaped `=` Unquoted=false so hasZshEqualsSub does not
	// treat stripped Value "=ls" as EQUALS.
	escapedZshEq := `agy \=ls review`
	wantEscapedZshEq := `agy --dangerously-skip-permissions --print '=ls review'`
	for _, shell := range []string{"zsh", "bash"} {
		_, args := shapePTYExecArgs(shell, []string{"-c", escapedZshEq})
		if args[1] != wantEscapedZshEq {
			t.Errorf("%s escaped literal =ls = %q, want %q", shell, args[1], wantEscapedZshEq)
		}
	}
	// Quoted leading `=` is also literal (EQUALS requires unquoted `=`).
	for _, quotedEq := range []string{`agy "=ls" review`, `agy '=ls' review`} {
		wantQuoted := `agy --dangerously-skip-permissions --print '=ls review'`
		for _, shell := range []string{"zsh", "bash"} {
			_, args := shapePTYExecArgs(shell, []string{"-c", quotedEq})
			if args[1] != wantQuoted {
				t.Errorf("%s quoted literal =ls (%s) = %q, want %q", shell, quotedEq, args[1], wantQuoted)
			}
		}
	}
	// Empty quotes before unquoted `=` still leave word-initial EQUALS after
	// quote removal (`''=ls` → =ls). Decline reshape on zsh only.
	for _, emptyQ := range []string{`agy ''=ls review`, `agy ""=ls review`} {
		_, zshEmpty := shapePTYExecArgs("zsh", []string{"-c", emptyQ})
		if zshEmpty[1] != emptyQ {
			t.Errorf("zsh empty-quote equals sub was reshaped: got %q, want original %q", zshEmpty[1], emptyQ)
		}
		_, bashEmpty := shapePTYExecArgs("bash", []string{"-c", emptyQ})
		wantBashEmpty := `agy --dangerously-skip-permissions --print '=ls review'`
		if bashEmpty[1] != wantBashEmpty {
			t.Errorf("bash empty-quote literal =ls (%s) = %q, want %q", emptyQ, bashEmpty[1], wantBashEmpty)
		}
	}

	// zsh PE split/array flags still multi-field when double-quoted
	// (`"${(s.:.)TASK}"` with TASK=fix:bug → fix bug). Reshape would put only
	// the first field in --print — leave unshaped. Single-field flags ((U))
	// still reshape. ${=TASK} is the SH_WORD_SPLIT shorthand (same issue).
	for _, orig := range []string{
		// `(=)` is documented as forcing SH_WORD_SPLIT; zsh 5.9 instead rejects
		// it with "error in flags", aborting before agy runs. Either way a
		// frozen --print value would diverge, so decline.
		`agy "${(=)1}"`,
		// (P) dereferences the parameter named by the value. A referent of
		// `@` / `name[@]` re-splits inside double quotes (zsh 5.9: three fields
		// for `zsh script @ fix bug`), and we cannot resolve the referent.
		`agy "${(P)1}"`,
		`agy "${(P)TARGET}"`,
		`agy "${(s.:.)TASK}"`,
		`agy "${(@)arr}"`,
		`agy "${(f)TASK}"`,
		`agy "${(z)TASK}"`,
		`agy "${=TASK}"`,
		`agy "${==TASK}"`,
		`agy "${^argv}"`,
	} {
		_, args := shapePTYExecArgs("zsh", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("zsh multi-field PE flags was reshaped: got %q, want original %q", args[1], orig)
		}
	}
	// Any *named* zsh parameter can be an array: zsh sources .zshenv even for
	// `zsh -c`, so a user file may hold `TASK=(fix bug)` alongside the built-in
	// arrays (argv, path, …). Nothing proves a name is scalar, so decline the
	// reshape for every named reference on zsh — bash keeps its shaping.
	for _, orig := range []string{
		`agy "$argv"`,
		`agy "${argv}"`,
		`agy "${argv:1}"`,
		`agy "$path"`,
		`agy "${path}"`,
		`agy "$commands"`,
		`agy "${commands}"`,
		`agy "do $argv please"`,
		// User-defined names from .zshenv — same hazard, no allowlist to hit.
		`agy "$TASK"`,
		`agy "${TASK}"`,
		`agy "${TASK:-fallback}"`,
		`agy "${(U)TASK}"`,
		`agy "do $TASK please"`,
		`agy --add-dir "$ROOT" task`,
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshArgs[1] != orig {
			t.Errorf("zsh named param was reshaped: got %q, want original %q", zshArgs[1], orig)
		}
	}
	// zsh EXTENDED_GLOB (settable from the .zshenv we cannot see) turns unquoted
	// `#`, `^` and `~` into pathname-generation operators: on zsh 5.9 with the
	// option set, `foo#` expands to the matching files and a non-matching
	// `zzz#` aborts with `no matches found`. Decline on zsh; bash keeps them
	// literal and still reshapes.
	for _, orig := range []string{
		`agy foo#`,
		`agy issue#12`,
		`agy f^o`,
		`agy a~b`,
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshArgs[1] != orig {
			t.Errorf("zsh extended-glob operator was reshaped: got %q, want original %q", zshArgs[1], orig)
		}
	}
	// zsh also rejects unmatched bracket patterns outright (`zsh -c 'printf %s
	// [draft'` → `bad pattern: [draft`, likewise `foo[bar`), where bash keeps
	// them literal. Decline every unquoted `[` on zsh.
	for _, orig := range []string{
		`agy [draft`,
		`agy review foo[bar`,
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshArgs[1] != orig {
			t.Errorf("zsh unmatched bracket was reshaped: got %q, want original %q", zshArgs[1], orig)
		}
		// bash keeps them literal and still reshapes.
		_, bashArgs := shapePTYExecArgs("bash", []string{"-c", orig})
		if bashArgs[1] == orig {
			t.Errorf("bash unmatched bracket was declined: %q", bashArgs[1])
		}
	}
	// zsh MAGIC_EQUAL_SUBST extends equals-substitution to the value side of an
	// assignment-like word: with the option on, `agy review foo==ls` passes
	// `foo=/usr/bin/ls` and `p=a:==ls` errors with `=ls not found` (zsh 5.9).
	// Decline both on zsh; bash keeps them literal.
	for _, orig := range []string{
		`agy review foo==ls`,
		`agy p=a:==ls`,
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshArgs[1] != orig {
			t.Errorf("zsh magic-equals was reshaped: got %q, want original %q", zshArgs[1], orig)
		}
		_, bashArgs := shapePTYExecArgs("bash", []string{"-c", orig})
		if bashArgs[1] == orig {
			t.Errorf("bash magic-equals was declined: %q", bashArgs[1])
		}
	}
	// A single `=` in an ordinary flag or word is not equals-substitution —
	// still reshape on zsh.
	_, zshFlagEq := shapePTYExecArgs("zsh", []string{"-c", `agy --model=fast review`})
	wantZshFlagEq := `agy --dangerously-skip-permissions --model=fast --print review`
	if zshFlagEq[1] != wantZshFlagEq {
		t.Errorf("zsh flag with = = %q, want %q", zshFlagEq[1], wantZshFlagEq)
	}
	// Quoted / escaped forms are literal in zsh too — still reshape.
	for _, tc := range []struct{ orig, want string }{
		{`agy 'foo#'`, `agy --dangerously-skip-permissions --print 'foo#'`},
		{`agy foo\#`, `agy --dangerously-skip-permissions --print 'foo#'`},
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", tc.orig})
		if zshArgs[1] != tc.want {
			t.Errorf("zsh quoted %q = %q, want %q", tc.orig, zshArgs[1], tc.want)
		}
	}
	// bash treats them as ordinary text. (A `#` at a word boundary is a comment
	// on every shell, so only mid-word forms are meaningful here.)
	_, bashHash := shapePTYExecArgs("bash", []string{"-c", `agy issue#12 f^o a~b`})
	wantBashHash := `agy --dangerously-skip-permissions --print 'issue#12 f^o a~b'`
	if bashHash[1] != wantBashHash {
		t.Errorf("bash mid-word # = %q, want %q", bashHash[1], wantBashHash)
	}

	// `zsh -f` narrows the startup hazard but does not remove it: /etc/zshenv
	// (Debian/Ubuntu /etc/zsh/zshenv) is read unconditionally, so EXTENDED_GLOB
	// / MAGIC_EQUAL_SUBST and array declarations remain possible. Verified on
	// zsh 5.9 with `setopt EXTENDED_GLOB` in /etc/zsh/zshenv: `zsh -f -c
	// 'printf "[%s]" foo#'` still glob-expands. These must keep declining.
	for _, pre := range [][]string{{"-f"}, {"-f", "+f"}, {"+f"}, {"--no-rcs"}} {
		for _, orig := range []string{`agy "$TASK"`, `agy foo#`, `agy review foo==ls`} {
			args := append(append([]string{}, pre...), "-c", orig)
			got := shapeShellWrappedPTYArgs("zsh", args)
			if got[len(got)-1] != orig {
				t.Errorf("zsh %v %q was reshaped: got %q", pre, orig, got[len(got)-1])
			}
		}
	}
	// zsh's BRACE_CCL (settable from either zshenv) expands a brace group with
	// no comma and no `..`: `zsh -c 'printf "[%s]" x{ab}y'` prints `[xay][xby]`
	// with it on, while default zsh and bash print `[x{ab}y]`. Decline every
	// balanced group on zsh; bash keeps the non-expanding forms literal.
	for _, orig := range []string{`agy x{ab}y`, `agy {foo}`} {
		_, zshBrace := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshBrace[1] != orig {
			t.Errorf("zsh brace group was reshaped: got %q, want original %q", zshBrace[1], orig)
		}
		_, bashBrace := shapePTYExecArgs("bash", []string{"-c", orig})
		if bashBrace[1] == orig {
			t.Errorf("bash non-expanding brace was declined: %q", bashBrace[1])
		}
	}

	// Default zsh behaviour does not depend on startup files, so an unmatched
	// `[` still declines even with -f.
	zshDashFBracket := shapeShellWrappedPTYArgs("zsh", []string{"-f", "-c", `agy [draft`})
	if zshDashFBracket[len(zshDashFBracket)-1] != `agy [draft` {
		t.Errorf("zsh -f unmatched bracket was reshaped: got %q", zshDashFBracket[len(zshDashFBracket)-1])
	}

	// bash: $argv / $path / $TASK are ordinary scalar print values.
	for _, c := range []struct{ orig, want string }{
		{`agy --print "$argv"`, `agy --dangerously-skip-permissions --print "$argv"`},
		{`agy --print "$TASK"`, `agy --dangerously-skip-permissions --print "$TASK"`},
		{`agy --print "${(U)TASK}"`, `agy --dangerously-skip-permissions --print "${(U)TASK}"`},
	} {
		_, bashArgs := shapePTYExecArgs("bash", []string{"-c", c.orig})
		if bashArgs[1] != c.want {
			t.Errorf("bash scalar param %q = %q, want %q", c.orig, bashArgs[1], c.want)
		}
	}
	// A positional slot with a literal default is provably one field — the
	// letters belong to the operand, not to a parameter name (zsh 5.9:
	// `zsh -c 'printf [%s] "${1:-review}"'` → `[review]`). Nested expansions in
	// the operand are still inspected, so `${1:-$TASK}` declines.
	_, zshPositional := shapePTYExecArgs("zsh", []string{"-c", `agy --print "${1:-review}"`})
	wantZshPositional := `agy --dangerously-skip-permissions --print "${1:-review}"`
	if zshPositional[1] != wantZshPositional {
		t.Errorf("zsh positional default = %q, want %q", zshPositional[1], wantZshPositional)
	}
	for _, orig := range []string{
		`agy "${1:-$TASK}"`,
		`agy "${1:-${TASK}}"`,
	} {
		_, zshNested := shapePTYExecArgs("zsh", []string{"-c", orig})
		if zshNested[1] != orig {
			t.Errorf("zsh nested named param in operand was reshaped: got %q, want original %q", zshNested[1], orig)
		}
	}
	// zsh: provably single-field forms remain safe explicit print values.
	for _, c := range []struct{ orig, want string }{
		{`agy --print "$(cat brief.txt)"`, `agy --dangerously-skip-permissions --print "$(cat brief.txt)"`},
		{`agy --print "$((1 + 2))"`, `agy --dangerously-skip-permissions --print "$((1 + 2))"`},
		{`agy --print "${#argv}"`, `agy --dangerously-skip-permissions --print "${#argv}"`},
		{`agy --print "$1"`, `agy --dangerously-skip-permissions --print "$1"`},
	} {
		_, zshArgs := shapePTYExecArgs("zsh", []string{"-c", c.orig})
		if zshArgs[1] != c.want {
			t.Errorf("zsh scalar form %q = %q, want %q", c.orig, zshArgs[1], c.want)
		}
	}

	// "$*" / "${files[*]}" stay a single field when double-quoted — still reshape.
	_, star := shapePTYExecArgs("bash", []string{"-c", `agy --print "$*"`})
	wantStar := `agy --dangerously-skip-permissions --print "$*"`
	if star[1] != wantStar {
		t.Errorf("quoted $* expand = %q, want %q", star[1], wantStar)
	}

	// Trailing bare `$` is not an expansion — reshape (do not decline as
	// unquoted expand and leave interactive agy waiting on the PTY).
	_, cost := shapePTYExecArgs("bash", []string{"-c", `agy cost$`})
	wantCost := `agy --dangerously-skip-permissions --print 'cost$'`
	if cost[1] != wantCost {
		t.Errorf("trailing literal $ = %q, want %q", cost[1], wantCost)
	}

	// Nested quotes inside ${…} (`"${x:-"foo bar"}"`) must not exit double-quote
	// mode and split the word — leave unshaped rather than rebuild broken.
	for _, orig := range []string{
		`agy "${x:-"foo bar"}"`,
		`agy "${x:-"foo"}"`,
		`agy ${x:-"foo bar"}`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("nested PE quotes was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Bash closes a ${…} at the first `}` even when the operand holds literal
	// braces: `"${x:-{}"` (x unset) is the one-character argument `{`, and
	// `"${x:-{}"foo"}"` is the single word `{foo}`. Treating the operand `{` as
	// a nested level left the PE "open" forever and declined the reshape, so
	// these payloads must reshape — with the prompt bytes preserved (verified
	// against bash 5.2.21: both forms deliver the same argv).
	for _, tc := range []struct{ orig, want string }{
		{`agy --print "${x:-{}"`, `agy --dangerously-skip-permissions --print "${x:-{}"`},
		{`agy --print "${x:-{}"foo"}"`, `agy --dangerously-skip-permissions --print "${x:-{}"foo'}'`},
		{`agy --print "${x:-{}""}"`, `agy --dangerously-skip-permissions --print "${x:-{}"'}'`},
		{`agy --print "${x:-a{b}c}"`, `agy --dangerously-skip-permissions --print "${x:-a{b}c}"`},
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", tc.orig})
		if args[1] != tc.want {
			t.Errorf("PE literal brace %q = %q, want %q", tc.orig, args[1], tc.want)
		}
	}

	// Nested ${…} still raises the depth, so a quote inside the *outer* PE is
	// PE content and the payload stays unshaped.
	for _, orig := range []string{
		`agy "${x:-${y:-"foo bar"}}"`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("nested PE quotes was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Balanced literal braces without nested quotes still reshape.
	_, peBrace := shapePTYExecArgs("bash", []string{"-c", `agy --print "${x:-{}y}"`})
	wantPEBrace := `agy --dangerously-skip-permissions --print "${x:-{}y}"`
	if peBrace[1] != wantPEBrace {
		t.Errorf("PE balanced literal braces = %q, want %q", peBrace[1], wantPEBrace)
	}

	// Plain double-quoted PE without nested quotes still reshapes.
	_, pe := shapePTYExecArgs("bash", []string{"-c", `agy --print "${x:-foo}"`})
	wantPE := `agy --dangerously-skip-permissions --print "${x:-foo}"`
	if pe[1] != wantPE {
		t.Errorf("plain PE default = %q, want %q", pe[1], wantPE)
	}

	// Parameter expansions whose pattern uses backslash escapes (e.g. %\} to
	// match a literal '}') cannot be re-double-quoted: allowExpand doubling
	// turns \ into \\ and changes the expansion. Leave unshaped.
	// Command / arithmetic / backtick substitution bodies are code the shell
	// re-parses, so the same doubling corrupts them: `agy "$(printf '\141')"`
	// supplies `a`, but `--print "$(printf '\\141')"` supplies the literal
	// `\141` (verified on bash 5.2.21 with a stub agy).
	for _, orig := range []string{
		`agy "${x%\}}"`,
		`agy "${x#\*}"`,
		`agy --add-dir "${x%\}}" task`,
		`agy "$(printf '\141')"`,
		"agy \"`printf '\\141'`\"",
		`agy "$(( 1 + \0 ))"`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("expansion backslash was reshaped: got %q, want original %q", args[1], orig)
		}
	}
	// A substitution without backslashes still reshapes.
	_, cleanSub := shapePTYExecArgs("bash", []string{"-c", `agy --print "$(cat brief.txt)"`})
	wantCleanSub := `agy --dangerously-skip-permissions --print "$(cat brief.txt)"`
	if cleanSub[1] != wantCleanSub {
		t.Errorf("clean substitution = %q, want %q", cleanSub[1], wantCleanSub)
	}

	// Literal backslash after a simple expansion (`"$ROOT\docs"`) is safe to
	// re-double-quote — do not treat it like PE pattern escapes.
	_, rootDocs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$ROOT\docs"`})
	wantRootDocs := `agy --dangerously-skip-permissions --print "$ROOT\\docs"`
	if rootDocs[1] != wantRootDocs {
		t.Errorf("expand + literal backslash = %q, want %q", rootDocs[1], wantRootDocs)
	}

	// Carriage return is not shell IFS whitespace — keep it inside the prompt
	// word (do not split into fragments and rejoin with a space).
	crPayload := "agy a\rb"
	_, crArgs := shapePTYExecArgs("bash", []string{"-c", crPayload})
	wantCR := "agy --dangerously-skip-permissions --print 'a\rb'"
	if crArgs[1] != wantCR {
		t.Errorf("literal CR prompt = %q, want %q", crArgs[1], wantCR)
	}

	// Legacy bash arithmetic `$[…]` still expands inside double quotes — keep
	// Expand so rebuild emits --print "$[1+2]" (not a single-quoted literal).
	_, arith := shapePTYExecArgs("bash", []string{"-c", `agy --print "$[1+2]"`})
	wantArith := `agy --dangerously-skip-permissions --print "$[1+2]"`
	if arith[1] != wantArith {
		t.Errorf("legacy arithmetic expand = %q, want %q", arith[1], wantArith)
	}

	// Dash leaves `$[…]` literal (no legacy arith). Incomplete `$[1+2` must not
	// be treated as unquoted expansion — still reshape with --print.
	_, dashArith := shapePTYExecArgs("dash", []string{"-c", `agy $[1+2`})
	wantDashArith := `agy --dangerously-skip-permissions --print '$[1+2'`
	if dashArith[1] != wantDashArith {
		t.Errorf("dash literal $[ = %q, want %q", dashArith[1], wantDashArith)
	}
	// Bash unquoted `$[…]` is expansion — leave unshaped (hasUnquotedExpand).
	origBashArith := `agy $[1+2]`
	_, bashUnq := shapePTYExecArgs("bash", []string{"-c", origBashArith})
	if bashUnq[1] != origBashArith {
		t.Errorf("bash unquoted $[ was reshaped: got %q, want original %q", bashUnq[1], origBashArith)
	}
}

// Dash lacks ANSI-C / locale dollar-quoting: $'text' is literal "$text" and
// must still reshape (unlike bash, which we refuse to reshape for $'…').
func TestShapePTYExecArgs_DashDollarQuoteIsLiteral(t *testing.T) {
	_, args := shapePTYExecArgs("dash", []string{"-c", `agy $'review text'`})
	want := `agy --dangerously-skip-permissions --print '$review text'`
	if args[1] != want {
		t.Errorf("dash dollar-quote prompt = %q, want %q", args[1], want)
	}

	// Bash still declines ANSI-C so we never rebuild into an expandable form.
	orig := `agy $'review text'`
	_, bashArgs := shapePTYExecArgs("bash", []string{"-c", orig})
	if bashArgs[1] != orig {
		t.Errorf("bash ANSI-C was reshaped: got %q, want original %q", bashArgs[1], orig)
	}
}

// Adjacent quote segments must not be glued: `"$""${TASK}"` rebuilds with the
// bare `$` (not an expansion) single-quoted and `${TASK}` double-quoted —
// never `"$${TASK}"` (which would expand $$ as PID).
func TestShapePTYExecArgs_PreservesExpandSegmentBoundaries(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy --print "$""${TASK}"`})
	want := `agy --dangerously-skip-permissions --print '$'"${TASK}"`
	if args[1] != want {
		t.Errorf("adjacent expand segments = %q, want %q", args[1], want)
	}
}

// Explicit --print takes one value; trailing recognized options (native
// --conversation id) stay flags, not prompt text.
func TestShapePTYExecArgs_PreservesTrailingOptionsAfterPrint(t *testing.T) {
	// Order is preserved: a flag that followed the print value stays after it,
	// so shell expansions keep their original left-to-right order. agy accepts
	// options after the print value (that is the native resume shape).
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy --print "fix bug" --conversation abc-123`})
	want := `agy --dangerously-skip-permissions --print 'fix bug' --conversation abc-123`
	if args[1] != want {
		t.Errorf("print+conversation = %q, want %q", args[1], want)
	}

	// Reordering would change what the shell computes: with the original order
	// `${x:=review}` assigns before `"$x"` is read, so both are `review`;
	// hoisting the flag makes --add-dir empty (verified against bash 5.2.21
	// with a stub agy).
	_, ordered := shapePTYExecArgs("bash", []string{"-c", `agy --print "${x:=review}" --add-dir "$x"`})
	wantOrdered := `agy --dangerously-skip-permissions --print "${x:=review}" --add-dir "$x"`
	if ordered[1] != wantOrdered {
		t.Errorf("expansion order = %q, want %q", ordered[1], wantOrdered)
	}

	// A second explicit --print discards the first value. Harmless for literals
	// (agy keeps the last one too), but when the discarded word expanded, the
	// shell had already evaluated it: `agy --print "${x:=review}" --print "$x"`
	// passes review twice, while emitting only `--print "$x"` leaves x unset and
	// the prompt empty (bash 5.2.21 + stub agy). Decline those.
	for _, orig := range []string{
		`agy --print "${x:=review}" --print "$x"`,
		`agy --print="${x:=review}" --print "$x"`,
	} {
		_, dup := shapePTYExecArgs("bash", []string{"-c", orig})
		if dup[1] != orig {
			t.Errorf("duplicate expanding print was reshaped: got %q, want original %q", dup[1], orig)
		}
	}
	// zsh's EQUALS lookup runs on a discarded word-initial `=` value even though
	// no segment is expandable: `zsh -c 'agy --print =missing --print review'`
	// aborts with `missing not found` before agy starts (zsh 5.9).
	origZshEqDup := `agy --print =definitely_missing_xyz --print review`
	_, zshEqDup := shapePTYExecArgs("zsh", []string{"-c", origZshEqDup})
	if zshEqDup[1] != origZshEqDup {
		t.Errorf("discarded zsh equals print was reshaped: got %q, want original %q", zshEqDup[1], origZshEqDup)
	}
	// A print that arrives after a state-mutating post-flag would be hoisted
	// ahead of it: bash runs `agy --print first --add-dir "${x:=dir}" --print
	// "$x"` with the prompt `dir`, while the reordered rebuild yields an empty
	// prompt (stub-agy diff on 5.2.21). Decline those.
	origReorder := `agy --print first --add-dir "${x:=dir}" --print "$x"`
	_, reorder := shapePTYExecArgs("bash", []string{"-c", origReorder})
	if reorder[1] != origReorder {
		t.Errorf("print reordered across expanding post-flag: got %q, want original %q", reorder[1], origReorder)
	}
	// Literal post-flags carry no state, so those still reshape.
	_, litReorder := shapePTYExecArgs("bash", []string{"-c", `agy --print first --add-dir /repo --print second`})
	wantLitReorder := `agy --dangerously-skip-permissions --print second --add-dir /repo`
	if litReorder[1] != wantLitReorder {
		t.Errorf("literal post-flag reorder = %q, want %q", litReorder[1], wantLitReorder)
	}
	// Literal duplicates still reshape (last value wins, same as agy).
	_, dupLit := shapePTYExecArgs("bash", []string{"-c", `agy --print first --print second`})
	wantDupLit := `agy --dangerously-skip-permissions --print second`
	if dupLit[1] != wantDupLit {
		t.Errorf("duplicate literal print = %q, want %q", dupLit[1], wantDupLit)
	}

	// Direct (non-shell) argv path — same partitioner.
	cmd, dArgs := shapePTYExecArgs("agy", []string{"--print", "fix bug", "--conversation", "abc-123"})
	if cmd != "agy" {
		t.Fatalf("cmd=%q", cmd)
	}
	wantDirect := []string{"--dangerously-skip-permissions", "--print", "fix bug", "--conversation", "abc-123"}
	if len(dArgs) != len(wantDirect) {
		t.Fatalf("direct args=%#v, want %#v", dArgs, wantDirect)
	}
	for i := range wantDirect {
		if dArgs[i] != wantDirect[i] {
			t.Errorf("direct args[%d]=%q, want %q (full %#v)", i, dArgs[i], wantDirect[i], dArgs)
		}
	}

	// Unknown trailing tokens after explicit --print (e.g. case-sensitive
	// --PRINT) must not be dropped — same as buildAntigravityInteractiveArgs.
	_, trail := shapePTYExecArgs("bash", []string{"-c", `agy --print review --PRINT`})
	wantTrail := `agy --dangerously-skip-permissions --print review --PRINT`
	if trail[1] != wantTrail {
		t.Errorf("print+unknown trailing = %q, want %q", trail[1], wantTrail)
	}
	_, trailEq := shapePTYExecArgs("bash", []string{"-c", `agy --print=review --PRINT extra`})
	wantTrailEq := `agy --dangerously-skip-permissions --print review --PRINT extra`
	if trailEq[1] != wantTrailEq {
		t.Errorf("print=value+unknown trailing = %q, want %q", trailEq[1], wantTrailEq)
	}

	// --print==ls peels value "=ls". On zsh, reshaping to a separate word
	// `--print =ls` would trigger EQUALS; decline. Bash still reshapes with
	// a safely quoted print value. Unquoted must survive sliceShellSegmentsAfter.
	origPrintEq := `agy --print==ls`
	_, zshPrintEq := shapePTYExecArgs("zsh", []string{"-c", origPrintEq})
	if zshPrintEq[1] != origPrintEq {
		t.Errorf("zsh --print==ls was reshaped: got %q, want original %q", zshPrintEq[1], origPrintEq)
	}
	// bash has no EQUALS; literal =ls is fine unquoted (shellArgMeta omits =).
	_, bashPrintEq := shapePTYExecArgs("bash", []string{"-c", origPrintEq})
	wantBashPrintEq := `agy --dangerously-skip-permissions --print =ls`
	if bashPrintEq[1] != wantBashPrintEq {
		t.Errorf("bash --print==ls = %q, want %q", bashPrintEq[1], wantBashPrintEq)
	}
}

// Unquoted ${…} must close paramDepth so a later escaped \$ outside the
// expansion is not mis-treated as inside it (shellWords must succeed). Flag
// unquoted expand still declines reshape (see RejectsUnquotedExpandInFlags).
func TestShapePTYExecArgs_ClosesUnquotedParamExpansion(t *testing.T) {
	// Double-quoted param + escaped $ still reshapes with per-segment split.
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy --add-dir "${ROOT}\$dir" task`})
	want := `agy --dangerously-skip-permissions --add-dir "${ROOT}"'$dir' --print task`
	if args[1] != want {
		t.Errorf("quoted param + escaped $ = %q, want %q", args[1], want)
	}

	// Unquoted form must parse without "inside parameter expansion" error.
	words, err := shellWords(`agy --add-dir ${ROOT}\$dir task`, shellWordOptions{braceExpand: true})
	if err != nil {
		t.Fatalf("shellWords unquoted param+escaped $: %v", err)
	}
	if len(words) != 4 || words[2].Value != `${ROOT}$dir` {
		t.Fatalf("shellWords unquoted param+escaped $ = %#v", words)
	}
}

// Unquoted expansions on flag operands must not be double-quoted on rebuild.
func TestShapePTYExecArgs_RejectsUnquotedExpandInFlags(t *testing.T) {
	for _, orig := range []string{
		`agy --add-dir $ROOT task`,
		`agy --add-dir ${ROOT}\$dir task`,
	} {
		_, args := shapePTYExecArgs("bash", []string{"-c", orig})
		if args[1] != orig {
			t.Errorf("unquoted flag expand was reshaped: got %q, want original %q", args[1], orig)
		}
	}

	// Double-quoted flag expand still reshapes.
	_, dq := shapePTYExecArgs("bash", []string{"-c", `agy --add-dir "$ROOT" task`})
	wantDQ := `agy --dangerously-skip-permissions --add-dir "$ROOT" --print task`
	if dq[1] != wantDQ {
		t.Errorf("double-quoted flag expand = %q, want %q", dq[1], wantDQ)
	}
}

// Bash line continuation (backslash-newline) removes both characters; the
// reshaped --print value must not contain a literal newline.
func TestShapePTYExecArgs_DropsShellLineContinuation(t *testing.T) {
	// Double-quoted: agy "fix\<newline>bug" → prompt "fixbug".
	payload := "agy \"fix\\\nbug\""
	_, args := shapePTYExecArgs("bash", []string{"-c", payload})
	want := `agy --dangerously-skip-permissions --print fixbug`
	if args[1] != want {
		t.Errorf("double-quoted line continuation = %q, want %q", args[1], want)
	}

	// Unquoted: agy fix\<newline>bug → same joined word.
	payloadU := "agy fix\\\nbug"
	_, uArgs := shapePTYExecArgs("bash", []string{"-c", payloadU})
	wantU := `agy --dangerously-skip-permissions --print fixbug`
	if uArgs[1] != wantU {
		t.Errorf("unquoted line continuation = %q, want %q", uArgs[1], wantU)
	}

	// Continuation at a word boundary must not start a word: following #
	// remains a shell comment (not mid-word prompt text).
	payloadC := "agy review \\\n# internal note"
	_, cArgs := shapePTYExecArgs("bash", []string{"-c", payloadC})
	wantC := `agy --dangerously-skip-permissions --print review`
	if cArgs[1] != wantC {
		t.Errorf("continuation before comment = %q, want %q", cArgs[1], wantC)
	}
}

// Escaped $ / ` inside an open ${…} cannot be segment-split without breaking
// the expansion on rebuild — leave the original payload unshaped.
func TestShapePTYExecArgs_RejectsEscapedExpansionInsideParam(t *testing.T) {
	orig := `agy "${TASK:-\$(touch /tmp/pwn)}"`
	_, args := shapePTYExecArgs("bash", []string{"-c", orig})
	if args[1] != orig {
		t.Errorf("escaped $ inside ${…} was reshaped: got %q, want original %q", args[1], orig)
	}

	// Outside ${…}, escaped $ still splits cleanly into inert rebuild.
	_, okArgs := shapePTYExecArgs("bash", []string{"-c", `agy --print "$TASK\$(touch /tmp/pwn)"`})
	wantOK := `agy --dangerously-skip-permissions --print "$TASK"'$(touch /tmp/pwn)'`
	if okArgs[1] != wantOK {
		t.Errorf("escaped $ outside ${…} = %q, want %q", okArgs[1], wantOK)
	}
}

// ANSI-C ($'…') / locale ($"…") quoting is not implemented — leave unshaped so
// we never rebuild $'(touch …)' into an expandable "$(touch …)".
func TestShapePTYExecArgs_RejectsANSICQuoting(t *testing.T) {
	orig := `agy $'(touch /tmp/pwn)'`
	_, args := shapePTYExecArgs("bash", []string{"-c", orig})
	if args[1] != orig {
		t.Errorf("ANSI-C quote was reshaped: got %q, want original %q", args[1], orig)
	}

	orig2 := `agy $"hello"`
	_, args2 := shapePTYExecArgs("bash", []string{"-c", orig2})
	if args2[1] != orig2 {
		t.Errorf("locale quote was reshaped: got %q, want original %q", args2[1], orig2)
	}

	// Line continuation between $ and quote still forms ANSI-C after bash joins.
	orig3 := "agy $\\\n'(touch /tmp/pwn)'"
	_, args3 := shapePTYExecArgs("bash", []string{"-c", orig3})
	if args3[1] != orig3 {
		t.Errorf("ANSI-C via line continuation was reshaped: got %q, want original %q", args3[1], orig3)
	}

	orig4 := "agy $\\\n\"hello\""
	_, args4 := shapePTYExecArgs("bash", []string{"-c", orig4})
	if args4[1] != orig4 {
		t.Errorf("locale quote via line continuation was reshaped: got %q, want original %q", args4[1], orig4)
	}
}

// Unquoted # at a word boundary is a shell comment — not part of the prompt.
func TestShapePTYExecArgs_StopsAtShellComment(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy review # internal note`})
	want := `agy --dangerously-skip-permissions --print review`
	if args[1] != want {
		t.Errorf("comment payload = %q, want %q", args[1], want)
	}

	// Mid-word # stays literal (not a comment).
	_, mid := shapePTYExecArgs("bash", []string{"-c", `agy review#note`})
	wantMid := `agy --dangerously-skip-permissions --print 'review#note'`
	if mid[1] != wantMid {
		t.Errorf("mid-word hash = %q, want %q", mid[1], wantMid)
	}
}

// Unterminated quotes must NOT be repaired into a runnable permission-skipping
// agy command — leave the original payload so bash reports the syntax error.
func TestShapePTYExecArgs_RejectsUnterminatedShellQuotes(t *testing.T) {
	orig := `agy 'review`
	_, args := shapePTYExecArgs("bash", []string{"-c", orig})
	if args[1] != orig {
		t.Errorf("unterminated payload was reshaped: got %q, want original %q", args[1], orig)
	}

	orig2 := `agy "fix`
	_, args2 := shapePTYExecArgs("bash", []string{"-c", orig2})
	if args2[1] != orig2 {
		t.Errorf("unterminated double-quote was reshaped: got %q, want original %q", args2[1], orig2)
	}
}

// Explicit empty shell prompt agy "" must emit --print "".
func TestShapePTYExecArgs_ShellWrappedExplicitEmptyPrint(t *testing.T) {
	_, args := shapePTYExecArgs("bash", []string{"-c", `agy ""`})
	want := `agy --dangerously-skip-permissions --print ""`
	if args[1] != want {
		t.Errorf("empty print = %q, want %q", args[1], want)
	}
}

func TestShellWords(t *testing.T) {
	cases := []struct {
		in         string
		wantVals   []string
		wantExpand []bool
		wantErr    bool
	}{
		// Expand is true only when a word carries unescaped $ or ` .
		{`agy fix the bug`, []string{"agy", "fix", "the", "bug"}, []bool{false, false, false, false}, false},
		{`agy "fix the bug"`, []string{"agy", "fix the bug"}, []bool{false, false}, false},
		{`agy 'fix the bug'`, []string{"agy", "fix the bug"}, []bool{false, false}, false},
		{`agy fix\ bug`, []string{"agy", "fix bug"}, []bool{false, false}, false},
		{`agy --add-dir "../shared library" do it`, []string{"agy", "--add-dir", "../shared library", "do", "it"}, []bool{false, false, false, false, false}, false},
		{`agy "$TASK"`, []string{"agy", "$TASK"}, []bool{false, true}, false},
		{`agy '$TASK'`, []string{"agy", "$TASK"}, []bool{false, false}, false},
		{`agy "\$(touch /tmp/pwn)"`, []string{"agy", "$(touch /tmp/pwn)"}, []bool{false, false}, false},
		{`agy "$TASK"'$(x)'`, []string{"agy", "$TASK$(x)"}, []bool{false, true}, false},
		{`agy "\$(x)$TASK"`, []string{"agy", "$(x)$TASK"}, []bool{false, true}, false},
		{`agy 'file{1,2}'`, []string{"agy", "file{1,2}"}, []bool{false, false}, false},
		{"agy \"fix\\\nbug\"", []string{"agy", "fixbug"}, []bool{false, false}, false},
		{"agy fix\\\nbug", []string{"agy", "fixbug"}, []bool{false, false}, false},
		{"agy review \\\n# note", []string{"agy", "review"}, []bool{false, false}, false},
		{`agy ""`, []string{"agy", ""}, []bool{false, false}, false},
		{`  agy   `, []string{"agy"}, []bool{false}, false},
		{``, nil, nil, false},
		{`agy 'review`, nil, nil, true},
		{`agy "fix`, nil, nil, true},
		{`agy review)`, nil, nil, true},
		{`agy review(`, nil, nil, true},
		{`agy file{1,2}`, nil, nil, true},
		{`agy review {a,b}`, nil, nil, true},
		{`agy {a,'b'}`, nil, nil, true},
		{`agy {a,"b"}`, nil, nil, true},
		{`agy {1..3}`, nil, nil, true},
		{`agy {a..c}`, nil, nil, true},
		{`agy file{1..2}`, nil, nil, true},
		{`agy {1..3..0}`, nil, nil, true},
		{`agy {1..3..+1}`, nil, nil, true},
		{`agy {+1..+3}`, nil, nil, true},
		{`agy {foo..bar}`, []string{"agy", "{foo..bar}"}, []bool{false, false}, false},
		{`agy {1..x}`, []string{"agy", "{1..x}"}, []bool{false, false}, false},
		{`agy review *.go`, nil, nil, true},
		{`agy review ~/proj`, nil, nil, true},
		{`agy review file[ab]`, nil, nil, true},
		{`agy review [draft`, []string{"agy", "review", "[draft"}, []bool{false, false, false}, false},
		{`agy review draft]`, []string{"agy", "review", "draft]"}, []bool{false, false, false}, false},
		{`agy A[0]=~`, nil, nil, true},
		{`agy review {foo}`, []string{"agy", "review", "{foo}"}, []bool{false, false, false}, false},
		{`agy review foo~bar`, []string{"agy", "review", "foo~bar"}, []bool{false, false, false}, false},
		{`agy HOME=~`, nil, nil, true},
		{`agy HOME+=~`, nil, nil, true},
		{`agy HOME-=~`, []string{"agy", "HOME-=~"}, []bool{false, false}, false},
		{`agy HOME\=~`, []string{"agy", "HOME=~"}, []bool{false, false}, false},
		{`agy PATH=~/bin:~/x`, nil, nil, true},
		{`agy PATH=/bin:~/x`, nil, nil, true},
		{`agy foo:~`, []string{"agy", "foo:~"}, []bool{false, false}, false},
		{`agy 'HOME='~`, []string{"agy", "HOME=~"}, []bool{false, false}, false},
		{`agy 1foo=~`, []string{"agy", "1foo=~"}, []bool{false, false}, false},
		{`agy 'HOME'=~`, []string{"agy", "HOME=~"}, []bool{false, false}, false},
		// Nested quotes inside ${…} decline (do not split the outer word).
		{`agy "${x:-"foo bar"}"`, nil, nil, true},
		{`agy ${x:-"foo bar"}`, nil, nil, true},
		{`agy "${x:-foo}"`, []string{"agy", `${x:-foo}`}, []bool{false, true}, false},

		{`agy "$""${TASK}"`, []string{"agy", "$${TASK}"}, []bool{false, true}, false},
		{`agy "$@"`, []string{"agy", "$@"}, []bool{false, true}, false},
		{`agy "${files[@]}"`, []string{"agy", `${files[@]}`}, []bool{false, true}, false},
		{`agy "${!pre_@}"`, []string{"agy", `${!pre_@}`}, []bool{false, true}, false},
		{`agy "${x:+$@}"`, []string{"agy", `${x:+$@}`}, []bool{false, true}, false},
		{`agy "$*"`, []string{"agy", "$*"}, []bool{false, true}, false},
		{`agy $OPTIONAL task`, []string{"agy", "$OPTIONAL", "task"}, []bool{false, true, false}, false},
		// Trailing / non-expanding `$` is literal (not unquoted expand).
		{`agy cost$`, []string{"agy", "cost$"}, []bool{false, false}, false},
		{`agy "cost$"`, []string{"agy", "cost$"}, []bool{false, false}, false},
		{`agy "$"`, []string{"agy", "$"}, []bool{false, false}, false},
		{`agy --add-dir ${ROOT}\$dir task`, []string{"agy", "--add-dir", `${ROOT}$dir`, "task"}, []bool{false, false, true, false}, false},
		{`agy "${TASK:-\$(x)}"`, nil, nil, true},
		{`agy $'(x)'`, nil, nil, true},
		{`agy $"x"`, nil, nil, true},
		{"agy $\\\n'(x)'", nil, nil, true},
		{"agy $\\\n\"x\"", nil, nil, true},
		{`agy review # note`, []string{"agy", "review"}, []bool{false, false}, false},
		{`agy review#note`, []string{"agy", "review#note"}, []bool{false, false}, false},
	}
	// Table cases model bash (brace expand + dollar-quote).
	bashOpts := shellWordOptions{braceExpand: true, dollarQuote: true, assignTilde: true, legacyArith: true}
	for _, tc := range cases {
		got, err := shellWords(tc.in, bashOpts)
		if tc.wantErr {
			if err == nil {
				t.Errorf("shellWords(%q) expected error, got %#v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("shellWords(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.wantVals) {
			t.Errorf("shellWords(%q) = %#v, want vals %#v", tc.in, got, tc.wantVals)
			continue
		}
		for i := range got {
			if got[i].Value != tc.wantVals[i] {
				t.Errorf("shellWords(%q)[%d].Value = %q, want %q", tc.in, i, got[i].Value, tc.wantVals[i])
			}
			if got[i].Expand != tc.wantExpand[i] {
				t.Errorf("shellWords(%q)[%d].Expand = %v, want %v", tc.in, i, got[i].Expand, tc.wantExpand[i])
			}
		}
	}

	// Dash/POSIX: braces are literal — no error, token preserved.
	gotDash, errDash := shellWords(`agy file{1,2}`, shellWordOptions{braceExpand: false, dollarQuote: false})
	if errDash != nil {
		t.Fatalf("shellWords dash braces: %v", errDash)
	}
	if len(gotDash) != 2 || gotDash[1].Value != "file{1,2}" {
		t.Errorf("shellWords dash braces = %#v, want file{1,2}", gotDash)
	}

	// Dash: $'text' is literal dollar + quoted text, not ANSI-C.
	gotDQ, errDQ := shellWords(`agy $'review text'`, shellWordOptions{braceExpand: false, dollarQuote: false})
	if errDQ != nil {
		t.Fatalf("shellWords dash dollar-quote: %v", errDQ)
	}
	if len(gotDQ) != 2 || gotDQ[1].Value != "$review text" || gotDQ[1].Expand {
		t.Errorf("shellWords dash dollar-quote = %#v, want $review text Expand=false", gotDQ)
	}
}

func TestShapePTYExecArgs_PassesThroughNonAntigravity(t *testing.T) {
	// A non-antigravity command is never shaped — the direct check and the
	// shell-wrapped check both miss it, so cmd/args pass through untouched.
	cmd, args := shapePTYExecArgs("git", []string{"fetch", "--all"})
	if cmd != "git" || len(args) != 2 || args[0] != "fetch" || args[1] != "--all" {
		t.Errorf("shapePTYExecArgs mutated non-antigravity command: cmd=%q args=%v", cmd, args)
	}

	// A shell-wrapped non-antigravity payload is likewise left alone.
	_, shArgs := shapePTYExecArgs("bash", []string{"-c", "echo hi"})
	if shArgs[1] != "echo hi" {
		t.Errorf("shapePTYExecArgs mutated non-antigravity shell payload: %q", shArgs[1])
	}
}

// TestShapeShellWrappedPTYArgs_SessionStartPath mirrors StartSession's tty PTY
// branch: cliArgs comes from buildInteractiveCLIArgs, then shapeShellWrappedPTYArgs
// runs before startPTYSession. A shell-wrapped `bash -c "agy …"` must be shaped
// there (buildInteractiveCLIArgs leaves it on the shell's default branch), while
// a DIRECT agy — already shaped by buildInteractiveCLIArgs — must NOT be shaped
// twice.
func TestShapeShellWrappedPTYArgs_SessionStartPath(t *testing.T) {
	// Shell-wrapped: buildInteractiveCLIArgs("bash", …) passes the payload through
	// unshaped, so shapeShellWrappedPTYArgs must inject the one-shot flags.
	cliArgs, _ := buildInteractiveCLIArgs("bash", []string{"-c", "agy --brief x"}, false)
	ptyArgs := shapeShellWrappedPTYArgs("bash", cliArgs)
	// agy ≥ 1.1.x: --dangerously-skip-permissions --print <value>
	want := `agy --dangerously-skip-permissions --print '--brief x'`
	if len(ptyArgs) != 2 || ptyArgs[0] != "-c" || ptyArgs[1] != want {
		t.Errorf("session_start shell-wrapped agy = %v, want -c %q", ptyArgs, want)
	}

	// Direct agy: buildInteractiveCLIArgs already shaped it; shapeShellWrappedPTYArgs
	// must leave it untouched (no duplicate --print/--dangerously-skip-permissions).
	directArgs, _ := buildInteractiveCLIArgs("agy", []string{"fix the bug"}, false)
	got := shapeShellWrappedPTYArgs("agy", directArgs)
	if strings.Join(got, " ") != strings.Join(directArgs, " ") {
		t.Errorf("shapeShellWrappedPTYArgs double-shaped direct agy: %v -> %v", directArgs, got)
	}
	if n := countOccurrences(got, "--print"); n != 1 {
		t.Errorf("direct agy has %d --print flags, want exactly 1: %v", n, got)
	}
}

func countOccurrences(s []string, want string) int {
	n := 0
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
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
		{"agy", ""},     // antigravity keeps argv (it ignores piped stdin); no stdin prompt
		{"agy.exe", ""}, // native agy.exe — still positional, no stdin prompt
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
     - agy:    prompt stays positional (agy ignores piped stdin)
     - other:  passes through verbatim
   ------------------------------------------------------------------------ */

func TestBuildInteractiveCLIArgs_RoutesByCommand(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		_, prompt := buildInteractiveCLIArgs("claude", []string{"hello"}, false)
		if prompt != "hello" {
			t.Errorf("claude prompt = %q, want %q", prompt, "hello")
		}
	})
	t.Run("codex", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("codex", []string{"hello"}, false)
		if prompt != "hello" {
			t.Errorf("codex stdinPrompt = %q, want %q", prompt, "hello")
		}
		if len(args) == 0 || args[len(args)-1] != "-" {
			t.Errorf("codex args must end with `-` placeholder, got %v", args)
		}
	})
	t.Run("antigravity", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("agy", []string{"hello"}, false)
		if prompt != "" {
			t.Errorf("agy stdinPrompt MUST be empty (agy ignores piped stdin; prompt goes on argv): %q", prompt)
		}
		mustContain(t, args, "--print", "--dangerously-skip-permissions", "hello")
	})
	t.Run("case-insensitive", func(t *testing.T) {
		// The router checks command.ToLower() exactly + startswith — make sure
		// "Claude" / "CODEX" still route correctly.
		_, p1 := buildInteractiveCLIArgs("Claude", []string{"hi"}, false)
		if p1 == "" {
			t.Errorf("Claude (mixed case) should still route to claude builder, got empty prompt")
		}
		args, _ := buildInteractiveCLIArgs("CODEX", []string{"hi"}, false)
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
			_, prompt := buildInteractiveCLIArgs(cmd, []string{"hi"}, false)
			if prompt == "" {
				t.Errorf("buildInteractiveCLIArgs(%q) did not route to claude (empty stdinPrompt)", cmd)
			}
			if !isClaudeCommand(cmd) {
				t.Errorf("isClaudeCommand(%q) = false, but router treated it as claude — predicates drifted", cmd)
			}
		}
	})
	t.Run("unknown-command-passes-through", func(t *testing.T) {
		args, prompt := buildInteractiveCLIArgs("git", []string{"status"}, false)
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
	// codex also emits events with type:"result" too, but THEIR
	// result events do NOT mean stdin should be closed — they're already
	// going to exit on their own.
	line := `{"type":"result","result":"done"}`
	for _, cmd := range []string{"codex", "git", "powershell"} {
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
		for _, cmd := range []string{"claude", "codex"} {
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

func TestReadOutputStream_ClaudeFailurePrecedesTurnComplete(t *testing.T) {
	session := &CLISession{
		ID:             "claude-failed-turn",
		Command:        "claude",
		Stdout:         io.NopCloser(strings.NewReader(`{"type":"result","is_error":true,"error":"Prompt is too long"}` + "\n")),
		Stderr:         io.NopCloser(strings.NewReader("")),
		streamDone:     make(chan struct{}),
		firstRealFrame: make(chan struct{}),
	}
	published := make(chan resultMsg, 4)

	NewSessionManager(nil).readOutputStream(session, func(msg resultMsg) {
		published <- msg
	})
	close(published)

	var errorSeq, completeSeq int
	for msg := range published {
		if msg.Type == "stream" && strings.Contains(msg.Output, "Prompt is too long") {
			errorSeq = msg.Seq
		}
		if msg.Type == "prompt" && msg.PromptType == "turn_complete" {
			completeSeq = msg.Seq
		}
	}
	if errorSeq == 0 || completeSeq == 0 {
		t.Fatalf("missing failure stream or turn_complete prompt: error Seq=%d, complete Seq=%d", errorSeq, completeSeq)
	}
	if errorSeq >= completeSeq {
		t.Fatalf("failure stream Seq=%d must precede turn_complete Seq=%d", errorSeq, completeSeq)
	}
}

func TestReadOutputStream_CodexCapturesFinalNumericTokenWithoutNewline(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "codex-rate-limits.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	frame := `{"type":"token_count","rate_limits":{"primary":{"used_percent":36,"window_minutes":300,"resets_in_seconds":3600}}}`
	session := &CLISession{
		ID:             "codex-final-token",
		Command:        "/opt/codex.cmd",
		Stdout:         io.NopCloser(strings.NewReader(frame)),
		Stderr:         io.NopCloser(strings.NewReader("")),
		streamDone:     make(chan struct{}),
		firstRealFrame: make(chan struct{}),
	}
	before := time.Now()
	NewSessionManager(nil).readOutputStream(session, func(resultMsg) {})
	after := time.Now()

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected final scanner token to populate Codex cache")
	}
	b := snap.Buckets[codexWindowPrimary]
	if b.UsedPercentage != 36 {
		t.Fatalf("bucket=%+v, want managed 36%% capture", b)
	}
	observed := time.UnixMilli(b.ObservedAtMs)
	if observed.Before(before.Add(-time.Millisecond)) || observed.After(after.Add(time.Millisecond)) {
		t.Fatalf("ObservedAt=%s, want receive time between %s and %s", observed, before, after)
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
	// init / system / user / tool_result / SUCCESSFUL result / rate_limit_event
	// must NOT produce display text — they're metadata. (A failing result is a
	// different case entirely — see the is_error tests below.)
	cases := []string{
		`{"type":"init","session_id":"abc"}`,
		`{"type":"system","msg":"hi"}`,
		`{"type":"user","message":{}}`,
		`{"type":"tool_result","tool_use_id":"x"}`,
		`{"type":"result","subtype":"success","result":"final"}`,
		`{"type":"result","subtype":"success","result":"final","is_error":false}`,
		`{"type":"rate_limit_event","retry_after":5}`,
	}
	for _, line := range cases {
		got := extractDisplayText("claude", line)
		if got != "" {
			t.Errorf("expected empty display text for %q, got %q", line, got)
		}
	}
}

func TestExtractDisplayText_Claude_FailedResultSurfacesErrorText(t *testing.T) {
	// The 2026-08-07 stall: every turn ended in an is_error result whose text
	// this parser discarded, so the orchestrator only ever saw empty output and
	// backed off for 16h instead of failing. The reason must reach the chunk
	// stream. Claude Code has used three different field names for it across
	// versions — all three must surface.
	cases := []struct {
		line string
		want string
	}{
		{`{"type":"result","is_error":true,"result":"Claude AI usage limit reached"}`, "Claude AI usage limit reached"},
		{`{"type":"result","is_error":true,"error":"Prompt is too long"}`, "Prompt is too long"},
		{`{"type":"result","is_error":true,"message":"OAuth token expired"}`, "OAuth token expired"},
	}
	for _, tc := range cases {
		got := extractDisplayText("claude", tc.line)
		if !strings.Contains(got, tc.want) {
			t.Errorf("extractDisplayText(%q) = %q, want it to contain %q", tc.line, got, tc.want)
		}
		if !strings.Contains(got, "Claude turn failed") {
			t.Errorf("extractDisplayText(%q) = %q, want a 'Claude turn failed' marker", tc.line, got)
		}
	}
}

func TestExtractDisplayText_Claude_FailedResultWithoutTextStillReportsFailure(t *testing.T) {
	// An is_error result carrying no readable text must still tell the reader
	// the turn FAILED. Silence here is what let the orchestrator mistake a
	// broken CLI for a CLI that simply had nothing to say.
	got := extractDisplayText("claude", `{"type":"result","is_error":true}`)
	if !strings.Contains(got, "Claude turn failed") {
		t.Errorf("got %q, want a 'Claude turn failed' marker", got)
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
	// `tool_name` across versions; both must work.
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

// TestExtractDisplayText_Grok_SubcommandJSONPassthrough guards that JSON
// output from a carved-out Grok subcommand — which bypasses the managed
// `--output-format streaming-json -p` headless path and runs verbatim
// (e.g. `grok sessions --json`) — is not swallowed by the streaming-json
// parser. Every event in the streaming-json schema carries a `type` field,
// so a JSON object with no `type` is structured subcommand output the user
// asked for and must passthrough rather than render as an empty line.
// Pairs with TestBuildGrokInteractiveArgs_SubcommandCarveOutRequiresUnambiguousArgv's
// `sessions --json` carve-out case.
func TestExtractDisplayText_Grok_SubcommandJSONPassthrough(t *testing.T) {
	cases := []string{
		`{"id":"sess_1","name":"work","createdAt":"2026-06-20T00:00:00Z"}`,
		`{"models":["grok-4","grok-3"]}`,
		`{"version":"1.2.3","build":"abc"}`,
	}
	for _, line := range cases {
		got := extractDisplayText("grok", line)
		if got != line {
			t.Errorf("subcommand JSON without type field should passthrough; got %q, want %q", got, line)
		}
	}
}

// TestExtractDisplayText_RoutesByBasename guards parser selection against
// path-launched sessions: buildInteractiveCLIArgs and detectCLITerminalEvent
// both classify via commandBaseName, so an explicit grok path like
// `/home/user/.grok/bin/grok` or `C:\tools\grok.exe` is shaped as the
// managed streaming-json `-p` headless turn. extractDisplayText must use the
// same normalisation — otherwise the parser fell through to default and
// raw NDJSON frames were emitted as chat text. Same risk for claude / codex
// installed at a non-bare path.
func TestExtractDisplayText_RoutesByBasename(t *testing.T) {
	cases := []struct {
		name    string
		command string
		line    string
		want    string
	}{
		{
			name:    "grok absolute unix path",
			command: "/home/user/.grok/bin/grok",
			line:    `{"type":"text","text":"hello"}`,
			want:    "hello",
		},
		{
			name:    "grok windows path with .exe",
			command: `C:\tools\grok.exe`,
			line:    `{"type":"text","text":"hello"}`,
			want:    "hello",
		},
		{
			name:    "grok windows path mixed case",
			command: `C:\Tools\Grok.EXE`,
			line:    `{"type":"text","text":"hi"}`,
			want:    "hi",
		},
		{
			name:    "claude absolute unix path skips assistant recap",
			command: "/usr/local/bin/claude",
			line:    `{"type":"assistant","message":{"content":[{"type":"text","text":"recap"}]}}`,
			want:    "",
		},
		{
			name:    "codex windows shim routes to codex parser",
			command: `C:\Users\u\AppData\Roaming\npm\codex.cmd`,
			line:    `{"type":"turn.started"}`,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractDisplayText(tc.command, tc.line)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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

func TestDetectPromptFromJSON_NonPromptEventsReturnNil(t *testing.T) {
	cases := map[string]string{
		"claude assistant": `{"type":"assistant","message":{}}`,
		"codex item":       `{"type":"item.completed"}`,
		"malformed":        `{"type":"permission_request"`,
		"non-JSON":         "plain text",
		"empty":            "",
	}
	for name, line := range cases {
		for _, cmd := range []string{"claude", "codex"} {
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
   - Codex: close after the write when a prompt was queued at start — codex
     exec reads the stdin-piped prompt to EOF before inference, so leaving
     stdin open hangs it.
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
		// Codex — close after the write when a prompt was queued at start; codex
		// exec reads stdin to completion before inference, so leaving it open
		// hangs the child waiting for EOF. But with an EMPTY prompt (chat-direct
		// opens the session eagerly and delivers the first message later via
		// SendInput), DEFER: closing here hands codex an immediate EOF with no
		// prompt and v0.140+ exits 1 with "No prompt provided via stdin."
		// SendInput closes the pipe after writing the first prompt instead.
		{cmd: "codex", stdinPrompt: "", want: false},
		{cmd: "codex", stdinPrompt: "review the diff", want: true},
		{cmd: "codex.cmd", stdinPrompt: "review", want: true},
		{cmd: "CODEX", stdinPrompt: "review", want: true},
		// Path-routed claude/codex — same policy must apply when the
		// caller supplied an absolute or relative path. Otherwise the argv
		// builder shapes a stdin-fed codex session but stdin is left
		// open and the child hangs waiting for EOF.
		{cmd: "/opt/claude-nightly/claude", stdinPrompt: "", want: false},
		{cmd: `C:\tools\claude.cmd`, stdinPrompt: "hi", want: false},
		{cmd: "/opt/bin/codex", stdinPrompt: "review", want: true},
		{cmd: "./codex", stdinPrompt: "", want: false},
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
