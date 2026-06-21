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
	got := buildGrokInteractiveArgs([]string{"Have", "a", "short", "conversation"}, true)
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
	}, true)
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "--model", "grok-4", "-p", "fix the bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_InlinePromptFlagValue(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"--single=hello there"}, true)
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "hello there"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildGrokInteractiveArgs_SubcommandCarveOut(t *testing.T) {
	// `grok models` is not a prompt — pass through verbatim, no injected -p.
	in := []string{"models"}
	got := buildGrokInteractiveArgs(in, true)
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
			got := buildGrokInteractiveArgs(tc.in, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_InjectsAlwaysApproveWhenOptedIn guards the
// injection of `--always-approve` on managed headless turns once the workspace
// has opted into Config.EnableGrokAlwaysApprove. Without it, Grok's default
// `ask` permission mode would prompt for tool execution / file edits, but
// StartSession closes Grok's stdin after launch and detectPromptFromJSON has
// no Grok approval branch — the prompt cannot be answered and the headless
// session stalls or fails.
func TestBuildGrokInteractiveArgs_InjectsAlwaysApproveWhenOptedIn(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"do", "the", "thing"}, true)
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

// TestBuildGrokInteractiveArgs_OmitsAlwaysApproveByDefault guards the gate:
// without Config.EnableGrokAlwaysApprove, the managed argv must NOT silently
// inject the approval bypass — the same conservative posture the Grok ACP
// path enforces. The session may stall on the first tool/file-edit prompt;
// that is the intentional opt-in tradeoff.
func TestBuildGrokInteractiveArgs_OmitsAlwaysApproveByDefault(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"do", "the", "thing"}, false)
	for _, a := range got {
		lower := strings.ToLower(a)
		if lower == "--always-approve" || lower == "--auto-approve" ||
			strings.HasPrefix(lower, "--always-approve=") ||
			strings.HasPrefix(lower, "--auto-approve=") {
			t.Fatalf("expected no approval-bypass flag when gate off, got %#v", got)
		}
	}
}

// TestBuildGrokInteractiveArgs_GateOffStripsCallerSuppliedAlwaysApprove guards
// that even a caller-supplied `--always-approve` / `--auto-approve` (any
// spelling) is dropped from flagArgs when the gate is off — otherwise a
// signed `session_start` could ferry the bypass in via argv and bypass the
// per-workspace opt-in. Mirrors the strip-by-default posture in the Grok ACP
// path's stripGrokAlwaysApprove flow.
func TestBuildGrokInteractiveArgs_GateOffStripsCallerSuppliedAlwaysApprove(t *testing.T) {
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
			got := buildGrokInteractiveArgs(tc.in, false)
			for _, a := range got {
				lower := strings.ToLower(a)
				if lower == "--always-approve" || lower == "--auto-approve" ||
					strings.HasPrefix(lower, "--always-approve=") ||
					strings.HasPrefix(lower, "--auto-approve=") {
					t.Fatalf("approval-bypass flag leaked through with gate off: %#v", got)
				}
			}
		})
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
			got := buildGrokInteractiveArgs(tc.in, true)
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

// TestBuildGrokInteractiveArgs_DisabledApproveDoesNotSuppressInjection guards
// the equals-false fix: a caller-supplied `--always-approve=false` /
// `--auto-approve=false` must NOT slip through to flagArgs (Grok would see
// the disabled flag and stall on the first tool/file-edit prompt — the
// headless `-p` turn has no approval handler) AND must NOT suppress the
// injected bare `--always-approve`. Stripping every equals-form in the
// flag-folding loop, plus dedupe-by-bare-form only, gets us both invariants:
// the managed bare flag is always present exactly once, and the disabled
// equals-form is dropped.
func TestBuildGrokInteractiveArgs_DisabledApproveDoesNotSuppressInjection(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "--always-approve=false", in: []string{"--always-approve=false", "fix", "the", "bug"}},
		{name: "--auto-approve=false", in: []string{"--auto-approve=false", "fix", "the", "bug"}},
		{name: "--Always-Approve=False mixed case", in: []string{"--Always-Approve=False", "fix", "the", "bug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			bareCount := 0
			for _, a := range got {
				lower := strings.ToLower(a)
				if strings.HasPrefix(lower, "--always-approve=") ||
					strings.HasPrefix(lower, "--auto-approve=") {
					t.Fatalf("equals-form approval flag leaked through: %#v", got)
				}
				if lower == "--always-approve" || lower == "--auto-approve" {
					bareCount++
				}
			}
			if bareCount != 1 {
				t.Fatalf("expected exactly one bare approval flag in argv, got %d: %#v", bareCount, got)
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
			got := buildGrokInteractiveArgs(tc.in, true)
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
			got := buildGrokInteractiveArgs(tc.in, true)
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
			got := buildGrokInteractiveArgs(tc.in, true)
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
			got := buildGrokInteractiveArgs(tc.in, true)
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
			got := buildGrokInteractiveArgs(tc.in, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_SubcommandCarveOutRequiresUnambiguousArgv
// guards the narrowed subcommand pre-scan. A leading subcommand-name word
// only short-circuits to verbatim argv in three unambiguous shapes:
//
//	(1) a single bare positional matching a subcommand (`grok models`);
//	(2) two positionals where the second is a recognised CLI action verb
//	    (`grok sessions list`, `grok mcp install`); or
//	(3) any positional count with a POSIX `--` separator anchoring a real
//	    subcommand grammar (covered by TestBuildGrokInteractiveArgs_
//	    SubcommandCarveOutOnDoubleDash).
//
// All other shapes — three-plus positionals without `--`, OR two positionals
// whose second word is plainly not a CLI verb (`grok help me`,
// `grok sessions stuck`) — are unquoted prose prompts the headless builder
// exists to fix and must fall through to managed `-p` delivery.
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
			name:      "two-token subcommand carves out (models list)",
			in:        []string{"models", "list"},
			want:      []string{"models", "list"},
			carvedOut: true,
		},
		{
			// xAI documents `grok agent stdio` as the ACP entrypoint
			// (https://docs.x.ai/build/cli/headless-scripting#ACP). Without
			// `stdio` in grokSubcommandActions, the 2-positional carve-out
			// would not fire and the builder would rewrite the call to
			// `grok ... -p "agent stdio"`, turning the JSON-RPC agent launch
			// into a prose prompt.
			name:      "two-token subcommand carves out (agent stdio)",
			in:        []string{"agent", "stdio"},
			want:      []string{"agent", "stdio"},
			carvedOut: true,
		},
		{
			name:      "subcommand with flag also carves out",
			in:        []string{"sessions", "--json"},
			want:      []string{"sessions", "--json"},
			carvedOut: true,
		},
		{
			name:      "two-positional prose with subcommand-word first folds into -p (help me)",
			in:        []string{"help", "me"},
			want:      []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "help me"},
			carvedOut: false,
		},
		{
			name:      "two-positional prose with subcommand-word first folds into -p (sessions stuck)",
			in:        []string{"sessions", "stuck"},
			want:      []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "sessions stuck"},
			carvedOut: false,
		},
		{
			name:      "two-positional prose with subcommand-word first folds into -p (models broken)",
			in:        []string{"models", "broken"},
			want:      []string{"--output-format", "streaming-json", "--no-auto-update", "--always-approve", "-p", "models broken"},
			carvedOut: false,
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
			got := buildGrokInteractiveArgs(tc.in, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_SubcommandCarveOutOnDoubleDash guards
// documented multi-argument Grok subcommand grammars (xAI changelog:
// `grok mcp add <name> -- <cmd> [args...]`). The POSIX `--` end-of-options
// separator is a hard CLI signal that the invocation is a real subcommand
// grammar, not prose — without this carve-out, the leading subcommand word
// plus ≥ 3 positionals would fall through to the prompt builder and be
// rewritten to `-p "mcp add filesystem npx ..."`, dropping the subcommand.
func TestBuildGrokInteractiveArgs_SubcommandCarveOutOnDoubleDash(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "mcp add with -- separator carves out verbatim (xAI changelog example)",
			in:   []string{"mcp", "add", "filesystem", "--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			want: []string{"mcp", "add", "filesystem", "--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		},
		{
			name: "mcp add with -- and a single trailing token",
			in:   []string{"mcp", "add", "fs", "--", "fs-server"},
			want: []string{"mcp", "add", "fs", "--", "fs-server"},
		},
		{
			name: "flags before subcommand with -- still carve out",
			in:   []string{"--cwd", "/tmp", "mcp", "add", "fs", "--", "npx", "server"},
			want: []string{"--cwd", "/tmp", "mcp", "add", "fs", "--", "npx", "server"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_DoubleDashInProseFoldsIntoPrompt guards the
// prose-prompt path against POSIX `--`. When the input is NOT a documented
// subcommand grammar (carved out above), a standalone `--` must NOT survive
// into flagArgs — Grok would interpret it as end-of-options and treat the
// appended `-p` as a positional, dropping the headless prompt-delivery flag
// and falling back to the interactive TUI this builder exists to avoid (e.g.
// `grok explain git checkout -- file`).
func TestBuildGrokInteractiveArgs_DoubleDashInProseFoldsIntoPrompt(t *testing.T) {
	cases := []struct {
		name       string
		in         []string
		wantPrompt string
	}{
		{
			name:       "prose prompt with -- about a shell command",
			in:         []string{"explain", "git", "checkout", "--", "file"},
			wantPrompt: "explain git checkout -- file",
		},
		{
			name:       "prose prompt with -- between words",
			in:         []string{"summarize", "the", "diff", "--", "please"},
			wantPrompt: "summarize the diff -- please",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			// `--` must never appear among the flag args: it would precede
			// the appended managed `-p` and Grok would consume the flag as a
			// positional, breaking the headless contract.
			for i, a := range got {
				if a == "--" && i+1 < len(got) && got[i+1] == "-p" {
					t.Fatalf("standalone `--` leaked before managed -p: %#v", got)
				}
			}
			// The last two args must be the managed -p flag and the folded prompt.
			if len(got) < 2 || got[len(got)-2] != "-p" {
				t.Fatalf("expected trailing `-p <prompt>`, got %#v", got)
			}
			if got[len(got)-1] != tc.wantPrompt {
				t.Fatalf("prompt mismatch: got %q, want %q (full=%#v)", got[len(got)-1], tc.wantPrompt, got)
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
			got := buildGrokInteractiveArgs(tc.in, true)
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

// TestBuildGrokInteractiveArgs_GateOffStripsPermissionBypassSurfaces guards
// the gate-off mirror of the ACP path's sanitizeGrokACPExtraArgs: when
// EnableGrokAlwaysApprove is false, the headless `-p` builder must strip
// EVERY documented permission-bypass surface, not just `--always-approve`.
// xAI's enterprise docs list three other surfaces that all skip the per-tool
// prompt gate:
//
//   - `--permission-mode bypassPermissions` (and the `bypass`/`auto`/`always`/
//     `acceptedits` synonyms isGrokPermissionModeBypassValue recognises);
//   - `--allow <pattern>` (rules are evaluated BEFORE the per-tool prompt,
//     so a single `--allow "Bash(*)"` would auto-approve matching tool calls);
//   - `--config approval.permission_mode=bypassPermissions` / `auth.method=…`
//     family (per-process config override of the same gate).
//
// Equals-form is dropped in the flag-folding loop; separate-value pairs flow
// into flagArgs and are stripped by the trailing sweeps that mirror
// stripGrokPermissionModePairs / stripGrokAllowRulePairs /
// stripGrokApprovalConfigPairs. Without these strips a signed `session_start`
// could ferry the bypass in via argv even though the ACP path refuses it.
func TestBuildGrokInteractiveArgs_GateOffStripsPermissionBypassSurfaces(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "--permission-mode bypassPermissions separate-value", in: []string{"--permission-mode", "bypassPermissions", "fix", "bug"}},
		{name: "--permission-mode=bypassPermissions equals-form", in: []string{"--permission-mode=bypassPermissions", "fix", "bug"}},
		{name: "--permission-mode bypass synonym", in: []string{"--permission-mode", "bypass", "fix", "bug"}},
		{name: "--permission-mode=auto synonym", in: []string{"--permission-mode=auto", "fix", "bug"}},
		{name: "--permission-mode acceptEdits", in: []string{"--permission-mode", "acceptEdits", "fix", "bug"}},
		{name: "--permission_mode underscore form", in: []string{"--permission_mode", "bypassPermissions", "fix", "bug"}},
		{name: "--allow separate-value", in: []string{"--allow", "Bash(*)", "fix", "bug"}},
		{name: "--allow=Bash(*) equals-form", in: []string{"--allow=Bash(*)", "fix", "bug"}},
		{name: "--allow with second rule survives gate too", in: []string{"--allow", "Bash(git *)", "--allow", "WriteFile(*)", "fix", "bug"}},
		{name: "--config approval.permission_mode=bypass", in: []string{"--config", "approval.permission_mode=bypassPermissions", "fix", "bug"}},
		{name: "--config=approval.permission_mode=bypass equals-form", in: []string{"--config=approval.permission_mode=bypassPermissions", "fix", "bug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, false)
			for i, a := range got {
				lower := strings.ToLower(a)
				switch {
				case lower == "--permission-mode" || lower == "--permission_mode":
					if i+1 < len(got) && isGrokPermissionModeBypassValue(got[i+1]) {
						t.Fatalf("permission-mode bypass pair leaked through with gate off: %#v", got)
					}
				case strings.HasPrefix(lower, "--permission-mode=") || strings.HasPrefix(lower, "--permission_mode="):
					if eq := strings.IndexByte(a, '='); eq >= 0 && isGrokPermissionModeBypassValue(a[eq+1:]) {
						t.Fatalf("permission-mode bypass equals-form leaked through with gate off: %#v", got)
					}
				case lower == "--allow" || strings.HasPrefix(lower, "--allow="):
					t.Fatalf("--allow rule leaked through with gate off: %#v", got)
				case lower == "--config" || lower == "-c":
					if i+1 < len(got) && isGrokApprovalConfigKV(got[i+1]) {
						t.Fatalf("--config approval-kv pair leaked through with gate off: %#v", got)
					}
				case strings.HasPrefix(lower, "--config=") || strings.HasPrefix(lower, "-c="):
					if eq := strings.IndexByte(a, '='); eq >= 0 && isGrokApprovalConfigKV(a[eq+1:]) {
						t.Fatalf("--config approval-kv equals-form leaked through with gate off: %#v", got)
					}
				}
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_GateOffPreservesBenignPermissionMode guards
// the inverse of the strip above: only BYPASS values are dropped. Selectors
// like `default`, `plan`, or `ask` tighten the policy (or are the default)
// and must flow through even with the gate off — same posture as the ACP
// path's stripGrokPermissionModePairs.
func TestBuildGrokInteractiveArgs_GateOffPreservesBenignPermissionMode(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "--permission-mode default flows through",
			in:   []string{"--permission-mode", "default", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--permission-mode", "default", "-p", "fix bug"},
		},
		{
			name: "--permission-mode plan flows through",
			in:   []string{"--permission-mode", "plan", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--permission-mode", "plan", "-p", "fix bug"},
		},
		{
			name: "--permission-mode=ask equals-form flows through",
			in:   []string{"--permission-mode=ask", "fix", "bug"},
			want: []string{"--output-format", "streaming-json", "--no-auto-update", "--permission-mode=ask", "-p", "fix bug"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, false)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_GateOffPreservesNonApprovalConfig guards that
// the trailing sweep only drops `--config <approval-kv>` pairs (and the
// auth-key family) when the gate is off — benign config like `log.level=debug`
// or `model.timeout=120s` must flow through unchanged.
func TestBuildGrokInteractiveArgs_GateOffPreservesNonApprovalConfig(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{"--config", "log.level=debug", "fix", "bug"}, false)
	want := []string{"--output-format", "streaming-json", "--no-auto-update", "--config", "log.level=debug", "-p", "fix bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestBuildGrokInteractiveArgs_GateOnPreservesPermissionBypassSurfaces guards
// the other side: when EnableGrokAlwaysApprove IS set the workspace has
// opted into autonomous tool execution, so the bypass surfaces flow through
// verbatim. The dedupe of the managed `--always-approve` injection is
// covered separately; here we only assert the strip does NOT fire.
func TestBuildGrokInteractiveArgs_GateOnPreservesPermissionBypassSurfaces(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		need string
	}{
		{name: "--permission-mode bypassPermissions stays", in: []string{"--permission-mode", "bypassPermissions", "fix", "bug"}, need: "bypassPermissions"},
		{name: "--allow Bash(*) stays", in: []string{"--allow", "Bash(*)", "fix", "bug"}, need: "Bash(*)"},
		{name: "--config approval.permission_mode=bypass stays", in: []string{"--config", "approval.permission_mode=bypassPermissions", "fix", "bug"}, need: "approval.permission_mode=bypassPermissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			found := false
			for _, a := range got {
				if a == tc.need {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected %q to flow through with gate on, got %#v", tc.need, got)
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_DoubleDashInProseWithSubcommandFirstWordFoldsToPrompt
// guards the narrowed `--` carve-out gate. Previously, ANY input whose first
// positional matched a known subcommand AND contained `--` short-circuited
// to verbatim argv — meaning prose prompts like
// `grok help me -- explain git checkout -- file` (where "help" collides with
// the subcommand name but "me" is plainly not a CLI action verb) would be
// passed to Grok unchanged. Grok then parses `help` as a subcommand and the
// rest as subcommand args, reintroducing the tokenization failure this
// builder exists to fix. The fix gates the `--` carve-out on the same
// subcommand-grammar shape as the no-`--` case: ONE positional before `--`,
// or two-plus positionals before `--` whose second is an action verb.
func TestBuildGrokInteractiveArgs_DoubleDashInProseWithSubcommandFirstWordFoldsToPrompt(t *testing.T) {
	cases := []struct {
		name       string
		in         []string
		wantPrompt string
	}{
		{
			name:       "help me -- explain ... (P2 reviewer case)",
			in:         []string{"help", "me", "--", "explain", "git", "checkout", "--", "file"},
			wantPrompt: "help me -- explain git checkout -- file",
		},
		{
			name:       "sessions stuck -- maybe?",
			in:         []string{"sessions", "stuck", "--", "maybe?"},
			wantPrompt: "sessions stuck -- maybe?",
		},
		{
			name:       "models broken -- I think",
			in:         []string{"models", "broken", "--", "I", "think"},
			wantPrompt: "models broken -- I think",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			// Must NOT short-circuit to raw argv: managed `-p` must wrap the prompt.
			if len(got) < 2 || got[len(got)-2] != "-p" {
				t.Fatalf("expected trailing `-p <prompt>`, got %#v", got)
			}
			if got[len(got)-1] != tc.wantPrompt {
				t.Fatalf("prompt mismatch: got %q, want %q (full=%#v)", got[len(got)-1], tc.wantPrompt, got)
			}
			// `--` must never appear directly before the managed `-p` — Grok
			// would consume the flag as a positional and the headless
			// prompt-delivery flag would be lost.
			for i, a := range got {
				if a == "--" && i+1 < len(got) && got[i+1] == "-p" {
					t.Fatalf("standalone `--` leaked before managed -p: %#v", got)
				}
			}
		})
	}
}

// TestBuildGrokInteractiveArgs_DoubleDashCarveOutStillFiresForRealSubcommandGrammar
// is the inverse: the narrowed gate must STILL admit documented
// multi-argument subcommand grammars where the positionals BEFORE the `--`
// are a real subcommand+action shape. Specifically `grok mcp add <name> --
// <cmd> [args...]` (xAI changelog example) must pass through verbatim — the
// fix narrows the gate, it does not close it.
func TestBuildGrokInteractiveArgs_DoubleDashCarveOutStillFiresForRealSubcommandGrammar(t *testing.T) {
	cases := []struct {
		name string
		in   []string
	}{
		{name: "mcp add with --", in: []string{"mcp", "add", "filesystem", "--", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"}},
		{name: "mcp -- alone (single positional before --)", in: []string{"mcp", "--", "foo"}},
		{name: "agent stdio with -- and args", in: []string{"agent", "stdio", "--", "--debug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGrokInteractiveArgs(tc.in, true)
			if !reflect.DeepEqual(got, tc.in) {
				t.Fatalf("expected verbatim passthrough, got %#v, want %#v", got, tc.in)
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
