package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBuildGrokInteractiveArgs_UnquotedPromptScreenshotCase reproduces the
// failing screenshot: `grok Have a short, simple conversation ...` tokenised so
// Grok parsed "a" as a subcommand. The builder must fold the whole prompt into
// a single `-p` value under managed streaming-json, exiting after one turn.
func TestBuildGrokInteractiveArgs_UnquotedPromptScreenshotCase(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"Have", "a", "short", "conversation"})
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "Have a short conversation"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_StripsManagedFlagsAndPassesModel(t *testing.T) {
	// User-supplied -p, --output-format (+value), --prompt-file (+value) must be
	// stripped; --model must pass through; positional words become the prompt.
	got := buildGrokInteractiveArgs([]string{
		"--model", "grok-4", "--output-format", "json", "-p", "fix the bug",
	})
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--model", "grok-4", "-p", "fix the bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_InlinePromptFlagValue(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"--single=hello there"})
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "hello there"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_SubcommandCarveOut(t *testing.T) {
	// `grok models` is not a prompt — pass through verbatim, no injected -p.
	in := []string{"models"}
	got := buildGrokInteractiveArgs(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("subcommand should be untouched: got %#v", got)
	}
}

// TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsValuedFlagValues guards
// the pre-scan that decides "is this a subcommand invocation or a prompt?":
// the scan must skip a valued flag's value so the value can't be mistaken for
// a subcommand. Without skipping, `grok --cwd sessions fix bug` would treat
// the `--cwd` value "sessions" as the `sessions` subcommand and return the
// argv untouched, sending the call down the unmanaged path instead of
// injecting `-p`.
func TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsValuedFlagValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--cwd value happens to be a subcommand name",
			in:   []string{"--cwd", "sessions", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--cwd", "sessions", "-p", "fix bug"},
		},
		{
			name: "--model value happens to be a subcommand name",
			in:   []string{"--model", "agent", "do", "thing"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--model", "agent", "-p", "do thing"},
		},
		{
			name: "-r value happens to be a subcommand name",
			in:   []string{"-r", "memory", "continue"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-r", "memory", "-p", "continue"},
		},
		{
			name: "--cwd=value equals form (single token) still routes correctly",
			in:   []string{"--cwd=sessions", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--cwd=sessions", "-p", "fix bug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_InjectsAlwaysApproveByDefault guards the
// unconditional injection of `--always-approve` on managed headless turns.
// Without it, Grok's default `ask` permission mode would prompt for tool
// execution / file edits, but StartSession closes Grok's stdin after launch
// and detectPromptFromJSON has no Grok approval branch — the prompt cannot
// be answered and the headless session stalls or fails.
func TestBuildGrokInteractiveArgs_InjectsAlwaysApproveByDefault(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"do", "the", "thing"})
	hasFlag := false
	for _, a := range got {
		if a == "--always-approve" {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		t.Fatalf("expected --always-approve in managed argv, got %#v", got)
	}
}

// TestBuildGrokInteractiveArgs_DoesNotDuplicateUserAlwaysApprove guards the
// dedupe path: when the caller already passed an approval-bypass flag (any of
// the documented spellings), the builder must not append its own copy.
func TestBuildGrokInteractiveArgs_DoesNotDuplicateUserAlwaysApprove(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "bare --always-approve", in: []string{"--always-approve", "do", "thing"}},
		{name: "--always-approve=true", in: []string{"--always-approve=true", "do", "thing"}},
		{name: "--auto-approve synonym", in: []string{"--auto-approve", "do", "thing"}},
		{name: "--auto-approve=true synonym", in: []string{"--auto-approve=true", "do", "thing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			count := 0
			for _, a := range got {
				lower := strings.ToLower(a)
				if lower == "--always-approve" || lower == "--auto-approve" ||
					strings.HasPrefix(lower, "--always-approve=") ||
					strings.HasPrefix(lower, "--auto-approve=") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("expected exactly one approval flag in argv, got %d: %#v", count, got)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_PreservesResumeAndSessionIDValues guards the
// xAI Headless & Scripting common flags `-r/--resume <ID>` and
// `-s/--session-id <ID>`: without an entry in valuedFlags the next token (the
// ID) would land in promptParts and Grok would then see `--resume -p` (the
// managed `-p` flag swallowed as the resume ID), breaking resumed sessions.
func TestBuildGrokInteractiveArgs_PreservesResumeAndSessionIDValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--resume keeps its ID and prompt is preserved",
			in:   []string{"--resume", "abc", "continue", "work"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--resume", "abc", "-p", "continue work"},
		},
		{
			name: "-r short form",
			in:   []string{"-r", "abc", "ship", "it"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-r", "abc", "-p", "ship it"},
		},
		{
			name: "--session-id keeps its ID",
			in:   []string{"--session-id", "sess-42", "next", "step"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--session-id", "sess-42", "-p", "next step"},
		},
		{
			name: "-s short form",
			in:   []string{"-s", "sess-42", "go"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-s", "sess-42", "-p", "go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsPromptFlagValues guards
// the same pre-scan against `-p`/`--single`: their value is the first word of
// the prompt, which can collide with a subcommand name (`help`, `models`,
// `sessions`, etc.). Without skipping that value the pre-scan returns the raw
// argv early, the managed `-p` folding never runs, and Grok sees only "help"
// as the prompt while the rest of the words are parsed as stray
// positionals — exactly the tokenisation failure this builder exists to fix.
func TestBuildGrokInteractiveArgs_SubcommandPreScanSkipsPromptFlagValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "-p value starts with a subcommand-name word",
			in:   []string{"-p", "help", "me", "fix", "tests"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "help me fix tests"},
		},
		{
			name: "--single value starts with a subcommand-name word",
			in:   []string{"--single", "models", "in", "this", "repo"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "models in this repo"},
		},
		{
			name: "-p value equals exactly a subcommand name",
			in:   []string{"-p", "sessions"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "sessions"},
		},
		{
			name: "-p=value inline form (single token) still routes correctly",
			in:   []string{"-p=help", "me", "out"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "help me out"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_PreservesAllowDenyRuleValues guards the xAI
// enterprise headless docs `--allow <pattern>` / `--deny <pattern>` policy
// rule flags. Without these entries in valuedFlags, the rule value lands in
// promptParts and the bare flag slots in immediately before the appended
// managed `-p`, so Grok would then consume `-p` as the rule value — dropping
// the managed prompt and/or the intended allow/deny rules.
func TestBuildGrokInteractiveArgs_PreservesAllowDenyRuleValues(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--allow keeps its pattern; prompt remains intact",
			in:   []string{"--allow", "Bash(git *)", "fix", "the", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--allow", "Bash(git *)", "-p", "fix the bug"},
		},
		{
			name: "--deny keeps its pattern; prompt remains intact",
			in:   []string{"--deny", "Bash(rm -rf *)", "ship", "it"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--deny", "Bash(rm -rf *)", "-p", "ship it"},
		},
		{
			name: "allow + deny together (xAI docs example)",
			in:   []string{"--allow", "Bash(git *)", "--deny", "Bash(rm -rf *)", "land", "the", "fix"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--allow", "Bash(git *)", "--deny", "Bash(rm -rf *)", "-p", "land the fix"},
		},
		{
			name: "caller-supplied -p with allow/deny — managed -p still wins, prompt preserved",
			in:   []string{"-p", "fix it", "--allow", "Bash(git *)", "--deny", "Bash(rm -rf *)"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--allow", "Bash(git *)", "--deny", "Bash(rm -rf *)", "-p", "fix it"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_PreservesPluginDirValue guards the xAI plugin
// docs `--plugin-dir <PATH>` separate-value flag. Without this entry in
// valuedFlags, the path lands in promptParts and the bare `--plugin-dir`
// slots in immediately before the appended managed `-p`, so Grok consumes
// `-p` as the plugin directory value and the prompt is no longer delivered.
func TestBuildGrokInteractiveArgs_PreservesPluginDirValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--plugin-dir keeps its path; prompt remains intact",
			in:   []string{"--plugin-dir", "/tmp/plugins", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--plugin-dir", "/tmp/plugins", "-p", "fix bug"},
		},
		{
			name: "caller-supplied -p with --plugin-dir — managed -p still wins, prompt preserved",
			in:   []string{"--plugin-dir", "/tmp/plugins", "-p", "fix bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--plugin-dir", "/tmp/plugins", "-p", "fix bug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_PreservesConfigValue guards the xAI
// enterprise-deployment `--config <key>=value` separate-value flag. Without
// the entry in valuedFlags, the value lands in promptParts and the bare
// `--config` slots in immediately before the appended managed `-p`, so Grok
// consumes `-p` as the config override and the prompt is no longer delivered.
func TestBuildGrokInteractiveArgs_PreservesConfigValue(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--config keeps its key=value; prompt remains intact",
			in:   []string{"--config", "log.level=debug", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--config", "log.level=debug", "-p", "fix bug"},
		},
		{
			name: "multiple --config overrides preserved before managed -p",
			in:   []string{"--config", "log.level=debug", "--config", "model.api_key=", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--config", "log.level=debug", "--config", "model.api_key=", "-p", "fix bug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_SubcommandCarveOutRequiresUnambiguousArgv
// guards the narrowed subcommand pre-scan: a leading subcommand-name word
// no longer short-circuits to verbatim argv when the trailing positional
// count is ≥ 3 — that shape is the unquoted prose prompt the headless
// builder exists to fix (`grok help me fix tests`, `grok sessions should
// persist`). Unambiguous shapes (`grok models`, `grok sessions list`) still
// carve out and forward argv untouched.
func TestBuildGrokInteractiveArgs_SubcommandCarveOutRequiresUnambiguousArgv(t *testing.T) {
	cases := []struct {
		name      string
		in        []string
		want      []string
		carvedOut bool
	}{
		{
			name:      "bare subcommand carves out",
			in:        []string{"models"},
			want:      []string{"models"},
			carvedOut: true,
		},
		{
			name:      "two-token subcommand carves out (sessions list)",
			in:        []string{"sessions", "list"},
			want:      []string{"sessions", "list"},
			carvedOut: true,
		},
		{
			name:      "two-token subcommand carves out (mcp install)",
			in:        []string{"mcp", "install"},
			want:      []string{"mcp", "install"},
			carvedOut: true,
		},
		{
			name:      "subcommand with flag also carves out",
			in:        []string{"sessions", "--json"},
			want:      []string{"sessions", "--json"},
			carvedOut: true,
		},
		{
			name:      "prose prompt leading with subcommand word folds into -p (help me fix tests)",
			in:        []string{"help", "me", "fix", "tests"},
			want:      []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "help me fix tests"},
			carvedOut: false,
		},
		{
			name:      "prose prompt with three positional words (sessions should persist)",
			in:        []string{"sessions", "should", "persist"},
			want:      []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "sessions should persist"},
			carvedOut: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_InjectsNoAutoUpdateAndDedupes guards the
// unconditional injection of `--no-auto-update` on managed headless turns and
// the strip of any caller-supplied `--no-auto-update` / `--auto-update`. Grok's
// background update worker can emit non-protocol output on stdout/stderr,
// which readOutputStream would surface as session output and pollute the
// streaming-json frame stream — mirrors the same posture in the ACP path.
func TestBuildGrokInteractiveArgs_InjectsNoAutoUpdateAndDedupes(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "no caller flag", in: []string{"do", "thing"}},
		{name: "caller --no-auto-update", in: []string{"--no-auto-update", "do", "thing"}},
		{name: "caller --no-auto-update=true", in: []string{"--no-auto-update=true", "do", "thing"}},
		{name: "caller --auto-update stripped", in: []string{"--auto-update", "do", "thing"}},
		{name: "caller --auto-update=true stripped", in: []string{"--auto-update=true", "do", "thing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in)
			noUpdateCount := 0
			autoUpdateCount := 0
			for _, a := range got {
				lower := strings.ToLower(a)
				switch {
				case lower == "--no-auto-update" || strings.HasPrefix(lower, "--no-auto-update="):
					noUpdateCount++
				case lower == "--auto-update" || strings.HasPrefix(lower, "--auto-update="):
					autoUpdateCount++
				}
			}
			if noUpdateCount != 1 {
				t.Fatalf("expected exactly one --no-auto-update, got %d: %#v", noUpdateCount, got)
			}
			if autoUpdateCount != 0 {
				t.Fatalf("expected --auto-update to be stripped, got %d: %#v", autoUpdateCount, got)
			}
		})
	}
}

func TestDetectCLITerminalEvent_Grok(t *testing.T) {
	if !detectCLITerminalEvent("grok", `{"type":"end"}`) {
		t.Fatal(`grok "end" event should be terminal`)
	}
	if detectCLITerminalEvent("grok", `{"type":"text"}`) {
		t.Fatal(`grok "text" event should NOT be terminal`)
	}
}

func TestGrokLimitStateFromFrame_Reached(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"type":         "usage_limit_reached",
		"gate_message": "You've hit your Grok limit.",
		"gate_url":     "https://grok.com/supergrok",
	}
	st, ok := grokLimitStateFromFrame(frame, now)
	if !ok || st.Severity != grokLimitReached {
		t.Fatalf("want reached, got ok=%v sev=%q", ok, st.Severity)
	}
	if st.Message == "" || !strings.Contains(st.UpgradeURL, "supergrok") {
		t.Fatalf("message/url not captured: %+v", st)
	}
}

func TestGrokLimitStateFromFrame_Approaching(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"event": "credit_limit_upsell_shown",
	}
	st, ok := grokLimitStateFromFrame(frame, now)
	if !ok || st.Severity != grokLimitApproaching {
		t.Fatalf("want approaching, got ok=%v sev=%q", ok, st.Severity)
	}
}

func TestGrokLimitStateFromFrame_NoSignal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if _, ok := grokLimitStateFromFrame(map[string]interface{}{"type": "text", "text": "hi"}, now); ok {
		t.Fatal("ordinary frame must not produce a limit state")
	}
}

// Grok ACP session-update frames carry the limit signal under
// `params.update.sessionUpdate` rather than the more generic
// `type`/`event`/`kind` keys. Without the walker recognising the
// `sessionUpdate` key, a real `usage_limit_reached` session-update frame
// passes the prefilter but returns ok=false, leaving the usage-limit card
// notice un-cached for that primary signal.
func TestGrokLimitStateFromFrame_SessionUpdate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"params": map[string]interface{}{
			"update": map[string]interface{}{
				"sessionUpdate": "usage_limit_reached",
				"gate_message":  "You've hit your Grok limit.",
				"gate_url":      "https://grok.com/supergrok",
			},
		},
	}
	st, ok := grokLimitStateFromFrame(frame, now)
	if !ok || st.Severity != grokLimitReached {
		t.Fatalf("want reached, got ok=%v sev=%q", ok, st.Severity)
	}
	if st.Message == "" || !strings.Contains(st.UpgradeURL, "supergrok") {
		t.Fatalf("message/url not captured: %+v", st)
	}

	frameSnake := map[string]interface{}{
		"params": map[string]interface{}{
			"update": map[string]interface{}{
				"session_update": "credit_limit_upsell_shown",
			},
		},
	}
	st, ok = grokLimitStateFromFrame(frameSnake, now)
	if !ok || st.Severity != grokLimitApproaching {
		t.Fatalf("want approaching from snake_case session_update, got ok=%v sev=%q", ok, st.Severity)
	}
}

// TestBuildGrokExecuteArgs_HeadlessPlainAndCarveOut verifies the EXECUTE-path
// adapter: a bare multi-word prompt becomes managed headless with PLAIN output
// (not streaming-json, which the execute path doesn't parse), and a subcommand
// passes through untouched (no injected -p).
func TestBuildGrokExecuteArgs_HeadlessPlainAndCarveOut(t *testing.T) {
	got := buildGrokExecuteArgs([]string{"Reply", "with", "exactly:", "OK"})
	// streaming-json must be swapped to plain; -p must carry the whole prompt.
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "streaming-json") {
		t.Fatalf("execute path must not use streaming-json: %#v", got)
	}
	if !strings.Contains(joined, "--output-format plain") {
		t.Fatalf("expected plain output format: %#v", got)
	}
	last2 := got[len(got)-2:]
	if last2[0] != "-p" || last2[1] != "Reply with exactly: OK" {
		t.Fatalf("prompt not folded into -p: %#v", got)
	}

	// Subcommand carve-out: unchanged, no -p injected.
	sub := buildGrokExecuteArgs([]string{"models"})
	if !reflect.DeepEqual(sub, []string{"models"}) {
		t.Fatalf("subcommand should pass through: %#v", sub)
	}
}
