package main

import (
	"strings"
	"testing"
)

/* --------------------------------------------------------------------------
   claude_argv_test.go — both Claude argv shapes.
   --------------------------------------------------------------------------
   The interactive cases below moved here VERBATIM from session_cli_test.go
   when the builders moved to claude_argv.go; they are the regression guard
   proving that move changed no behaviour. The non-interactive cases at the
   bottom pin the opposite contract: the print shape keeps `--print`, never
   requests stream-json framing, and runs with no tools.
   ------------------------------------------------------------------------ */

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
   buildClaudeNonInteractivePrintArgs
   --------------------------------------------------------------------------
   The sanctioned one-shot shape used by the CLI-maintenance smoke. Verified
   against Claude Code 2.1.247. Every assertion here is a failure mode that was
   actually observed or is one flag away from the exit-1 protocol regression
   this shape replaces.
   ------------------------------------------------------------------------ */

// argvHasFlagValue reports whether args contains flag immediately followed by
// value. Used instead of a bare "contains" so a flag/value pair that drifts
// apart is caught.
func argvHasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag {
			return i+1 < len(args) && args[i+1] == value
		}
	}
	return false
}

func argvContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestBuildClaudeNonInteractivePrintArgs_KeepsPrintAndSingleJSONEnvelope(t *testing.T) {
	args := buildClaudeNonInteractivePrintArgs(claudeArgvShapes[0])

	if !argvContains(args, "--print") {
		t.Fatalf("--print must be present on the non-interactive path: %v", args)
	}
	if !argvHasFlagValue(args, "--output-format", "json") {
		t.Errorf("--output-format json missing or detached: %v", args)
	}
	// --input-format must not be requested at all: pairing stream-json input
	// with anything but stream-json output is the framing violation that
	// produced `exit 1` with no assistant turn.
	if argvContains(args, "--input-format") {
		t.Errorf("--input-format must be left at its default: %v", args)
	}
	for _, banned := range []string{
		"stream-json", "--verbose", "--include-partial-messages",
		"--dangerously-skip-permissions",
	} {
		if argvContains(args, banned) {
			t.Errorf("%s must not appear on the non-interactive path: %v", banned, args)
		}
	}
	if !argvHasFlagValue(args, "--tools", "") {
		t.Errorf("tools must be explicitly disabled with an empty set: %v", args)
	}
	if !argvHasFlagValue(args, "--max-turns", "1") {
		t.Errorf("--max-turns 1 missing or detached: %v", args)
	}
}

func TestBuildClaudeNonInteractivePrintArgs_PreferredShapePinsEmptyMCPConfig(t *testing.T) {
	args := buildClaudeNonInteractivePrintArgs(claudeArgvShapes[0])
	if !argvContains(args, "--strict-mcp-config") {
		t.Errorf("preferred shape must isolate the probe from user MCP config: %v", args)
	}
	if !argvHasFlagValue(args, "--mcp-config", `{"mcpServers":{}}`) {
		t.Errorf("preferred shape must pin an EMPTY server map: %v", args)
	}
}

func TestBuildClaudeNonInteractivePrintArgs_FallbackDropsOnlyMCPFlags(t *testing.T) {
	if len(claudeArgvShapes) != 2 {
		t.Fatalf("ladder must stay bounded at 2 attempts, got %d", len(claudeArgvShapes))
	}
	preferred := buildClaudeNonInteractivePrintArgs(claudeArgvShapes[0])
	fallback := buildClaudeNonInteractivePrintArgs(claudeArgvShapes[1])

	if argvContains(fallback, "--strict-mcp-config") || argvContains(fallback, "--mcp-config") {
		t.Fatalf("fallback shape must drop the MCP flags: %v", fallback)
	}
	// …and nothing else: the fallback exists for a build that rejects the MCP
	// flags, not as a second, weaker contract.
	var want []string
	skip := false
	for _, a := range preferred {
		if skip {
			skip = false
			continue
		}
		if a == "--strict-mcp-config" {
			continue
		}
		if a == "--mcp-config" {
			skip = true
			continue
		}
		want = append(want, a)
	}
	if strings.Join(fallback, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("fallback = %v, want %v", fallback, want)
	}
	if claudeArgvShapes[0].ID == claudeArgvShapes[1].ID {
		t.Error("ladder entries must have distinct ids so the published shape is identifiable")
	}
}

func TestBuildClaudeNonInteractivePrintArgs_CarriesNoPromptOrMarker(t *testing.T) {
	// The builder cannot receive the prompt or marker at all — that is the
	// structural guarantee. Assert the produced argv holds neither the marker
	// prefix nor any prose, so a future change that threads them in fails here.
	for _, shape := range claudeArgvShapes {
		for _, a := range buildClaudeNonInteractivePrintArgs(shape) {
			if strings.Contains(a, claudeSmokeMarkerPrefix) {
				t.Errorf("marker leaked into argv for shape %s: %q", shape.ID, a)
			}
			if strings.Contains(a, "Reply with exactly") {
				t.Errorf("prompt leaked into argv for shape %s: %q", shape.ID, a)
			}
		}
	}
}

func TestClaudePrintFlagPrompt_ClassifiesEveryForm(t *testing.T) {
	// The single classifier both builders depend on: the interactive path
	// strips exactly what this reports, and the print path emits the same
	// long-form constant it recognises.
	cases := []struct {
		arg        string
		wantIs     bool
		wantInline string
	}{
		{"-p", true, ""},
		{"--print", true, ""},
		{"-p=hello", true, "hello"},
		{"--print=hello world", true, "hello world"},
		{"--print=", true, ""},
		{"-P", false, ""},
		{"--PRINT", false, ""},
		{"--printer", false, ""},
		{"--model", false, ""},
		{"print", false, ""},
	}
	for _, tc := range cases {
		gotIs, gotInline := claudePrintFlagPrompt(tc.arg)
		if gotIs != tc.wantIs || gotInline != tc.wantInline {
			t.Errorf("claudePrintFlagPrompt(%q) = (%v, %q), want (%v, %q)",
				tc.arg, gotIs, gotInline, tc.wantIs, tc.wantInline)
		}
	}
	if claudePrintFlag != "--print" || claudePrintShortFlag != "-p" {
		t.Fatal("print flag constants changed; both builders read them")
	}
}
