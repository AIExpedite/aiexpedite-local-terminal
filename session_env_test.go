// session_env_test.go
// -----------------------------------------------------------------------------
// Tests for sanitizeClaudeChildEnv + isClaudeCommand — the helpers that
// enforce the billing-safety policy documented in CLI_AGENT_INTEGRATION.md:
// spawned Claude Code sessions must fall back to the user's stored /login
// subscription credential, never silently bill ANTHROPIC_API_KEY /
// ANTHROPIC_AUTH_TOKEN from the parent shell.
//
// Riskiest cases first — billing-var strip, non-claude carve-out, unrelated
// ANTHROPIC_* preservation, edge inputs.
// -----------------------------------------------------------------------------

package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func envHas(env []string, name string) bool {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func TestSanitizeClaudeChildEnv_StripsBillingVarsForClaude(t *testing.T) {
	// The money-leak scenario: a user has ANTHROPIC_API_KEY in their shell
	// for unrelated SDK work. Without this strip, every spawned Claude Code
	// session silently bills the API wallet instead of the subscription.
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"ANTHROPIC_AUTH_TOKEN=tok-secret",
		"HOME=/home/u",
	}
	out, stripped := sanitizeClaudeChildEnv("claude", in)

	if envHas(out, "ANTHROPIC_API_KEY") {
		t.Errorf("ANTHROPIC_API_KEY must be stripped for claude, got %v", out)
	}
	if envHas(out, "ANTHROPIC_AUTH_TOKEN") {
		t.Errorf("ANTHROPIC_AUTH_TOKEN must be stripped for claude, got %v", out)
	}
	if !envHas(out, "PATH") || !envHas(out, "HOME") {
		t.Errorf("unrelated vars must be preserved, got %v", out)
	}

	sort.Strings(stripped)
	want := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}
	if !reflect.DeepEqual(stripped, want) {
		t.Errorf("stripped names = %v, want %v", stripped, want)
	}
}

func TestSanitizeClaudeChildEnv_PreservesBillingVarsForNonClaude(t *testing.T) {
	// Codex / gemini / arbitrary shells must keep ANTHROPIC_* — those tools
	// have unrelated auth wiring (or none) and this driver should not
	// silently rewrite their environment.
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-keep",
		"ANTHROPIC_AUTH_TOKEN=tok-keep",
	}
	for _, cmd := range []string{"codex", "gemini", "agy", "powershell", "git", ""} {
		t.Run(cmd, func(t *testing.T) {
			out, stripped := sanitizeClaudeChildEnv(cmd, in)
			if !envHas(out, "ANTHROPIC_API_KEY") || !envHas(out, "ANTHROPIC_AUTH_TOKEN") {
				t.Errorf("ANTHROPIC_* must be preserved for %q, got %v", cmd, out)
			}
			for _, s := range stripped {
				if s == "ANTHROPIC_API_KEY" || s == "ANTHROPIC_AUTH_TOKEN" {
					t.Errorf("billing var %q must not be stripped for non-claude %q", s, cmd)
				}
			}
		})
	}
}

func TestSanitizeClaudeChildEnv_AlwaysStripsClaudePrefixes(t *testing.T) {
	// CLAUDECODE / CLAUDE_* must be stripped for every command — they signal
	// nested-Claude / IDE context. Asserted for both claude and a non-claude
	// command to confirm the strip is unconditional.
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_AGENT_SDK_VERSION=0.1.0",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
	}
	for _, cmd := range []string{"claude", "codex"} {
		t.Run(cmd, func(t *testing.T) {
			out, stripped := sanitizeClaudeChildEnv(cmd, in)
			for _, name := range []string{
				"CLAUDECODE",
				"CLAUDE_CODE_ENTRYPOINT",
				"CLAUDE_AGENT_SDK_VERSION",
				"CLAUDE_CODE_OAUTH_TOKEN",
			} {
				if envHas(out, name) {
					t.Errorf("%s must be stripped (cmd=%q), got %v", name, cmd, out)
				}
				found := false
				for _, s := range stripped {
					if s == name {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s must appear in stripped list (cmd=%q), got %v", name, cmd, stripped)
				}
			}
			if !envHas(out, "PATH") {
				t.Errorf("PATH must survive, got %v", out)
			}
		})
	}
}

func TestSanitizeClaudeChildEnv_DoesNotStripUnrelatedAnthropicVars(t *testing.T) {
	// Only the two precedence-bearing vars are part of the billing strip.
	// ANTHROPIC_BASE_URL / ANTHROPIC_MODEL / ANTHROPIC_LOG would surprise a
	// user if they vanished silently — keep them.
	in := []string{
		"ANTHROPIC_BASE_URL=https://example",
		"ANTHROPIC_MODEL=claude-opus",
		"ANTHROPIC_LOG=debug",
	}
	out, stripped := sanitizeClaudeChildEnv("claude", in)
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_LOG"} {
		if !envHas(out, name) {
			t.Errorf("%s must be preserved (not a billing precedence var), got %v", name, out)
		}
	}
	if len(stripped) != 0 {
		t.Errorf("nothing should be stripped, got %v", stripped)
	}
}

func TestSanitizeClaudeChildEnv_EmptyInput(t *testing.T) {
	out, stripped := sanitizeClaudeChildEnv("claude", nil)
	if len(out) != 0 {
		t.Errorf("empty input → empty output, got %v", out)
	}
	if len(stripped) != 0 {
		t.Errorf("empty input → no stripped vars, got %v", stripped)
	}
}

func TestSanitizeClaudeChildEnv_CaseInsensitiveMatching(t *testing.T) {
	// Env names on Windows can come through with mixed casing; the strip
	// must be case-insensitive on the NAME (matching the existing
	// strings.ToUpper convention in the helper).
	in := []string{
		"anthropic_api_key=sk-ant-lower",
		"Anthropic_Auth_Token=tok-mixed",
		"claudecode=1",
		"Claude_Code_Entrypoint=cli",
	}
	out, _ := sanitizeClaudeChildEnv("claude", in)
	if len(out) != 0 {
		t.Errorf("case-variant billing/claude vars must all be stripped, got %v", out)
	}
}

func TestSanitizeClaudeChildEnv_MalformedEntryWithoutEquals(t *testing.T) {
	// Defensive: a stray entry without `=` (shouldn't happen for os.Environ()
	// but possible if callers pass in handcrafted lists) must not panic and
	// must not appear in the stripped-names log line (would log the whole
	// value).
	in := []string{"BAREWORD", "PATH=/usr/bin"}
	out, stripped := sanitizeClaudeChildEnv("claude", in)
	if !envHas(out, "PATH") {
		t.Errorf("PATH must survive, got %v", out)
	}
	for _, s := range stripped {
		if !strings.Contains(s, "_") {
			// Sanity: every legit stripped name in our matchers contains `_`
			// (ANTHROPIC_*, CLAUDE_*, CLAUDECODE). A bare "BAREWORD" leaking
			// in here would mean the log line is exposing junk.
			t.Errorf("unexpected stripped name %q from malformed entry", s)
		}
	}
}

func TestPrepareClaudeChildEnv_PinsCliEntrypointForClaude(t *testing.T) {
	// A spawned claude session must self-identify as an interactive CLI
	// session (CLAUDE_CODE_ENTRYPOINT=cli) — the launch shape this driver
	// uses — even when this agent inherited a different entrypoint (e.g.
	// "claude-vscode" from a host IDE). The inherited value is stripped by
	// sanitize, then the canonical "cli" is pinned. Load-bearing for the
	// 2026-06-15 Agent-SDK-credit-pool split (see CLI_AGENT_INTEGRATION.md).
	in := []string{
		"PATH=/usr/bin",
		"CLAUDE_CODE_ENTRYPOINT=claude-vscode",
	}
	out, stripped := prepareClaudeChildEnv("claude", in)

	var vals []string
	for _, e := range out {
		if strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			vals = append(vals, strings.TrimPrefix(e, "CLAUDE_CODE_ENTRYPOINT="))
		}
	}
	if len(vals) != 1 || vals[0] != "cli" {
		t.Errorf("claude child env must pin exactly CLAUDE_CODE_ENTRYPOINT=cli, got %v (full=%v)", vals, out)
	}

	// The inherited entrypoint must still be recorded as stripped so the
	// audit log line reflects what was removed.
	found := false
	for _, s := range stripped {
		if s == "CLAUDE_CODE_ENTRYPOINT" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("inherited CLAUDE_CODE_ENTRYPOINT should be recorded as stripped, got %v", stripped)
	}
}

func TestPrepareClaudeChildEnv_NoEntrypointForNonClaude(t *testing.T) {
	// codex / gemini / shells must not be tagged with claude's entrypoint —
	// the pin is claude-specific.
	in := []string{"PATH=/usr/bin"}
	for _, cmd := range []string{"codex", "gemini", "agy", "git", ""} {
		t.Run(cmd, func(t *testing.T) {
			out, _ := prepareClaudeChildEnv(cmd, in)
			if envHas(out, "CLAUDE_CODE_ENTRYPOINT") {
				t.Errorf("non-claude %q must not get CLAUDE_CODE_ENTRYPOINT, got %v", cmd, out)
			}
		})
	}
}

func TestPrepareClaudeChildEnv_PinsCliWhenAbsent(t *testing.T) {
	// Even with no inherited entrypoint at all, claude must end up pinned to
	// cli rather than relying on claude's default-when-unset.
	out, _ := prepareClaudeChildEnv("claude", []string{"PATH=/usr/bin"})
	if !envHas(out, "CLAUDE_CODE_ENTRYPOINT") {
		t.Errorf("claude child env must pin CLAUDE_CODE_ENTRYPOINT=cli, got %v", out)
	}
}

func TestIsClaudeCommand(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"claude", true},
		{"Claude", true},
		{"CLAUDE", true},
		{"claude.exe", true},
		{"claude.cmd", true},
		{"claude.bat", true},
		{"claude.ps1", true},
		{"/usr/local/bin/claude", true},
		{`C:\Users\u\AppData\Roaming\npm\claude.cmd`, true},
		{"./claude", true},
		{"codex", false},
		{"gemini", false},
		{"agy", false},
		// Matches the router's HasPrefix("claude") behaviour — any future
		// `claude-edge` / `claude-next` variant gets the same env policy as
		// the canonical claude binary, so it can't silently regain API-key
		// billing.
		{"claude-edge", true},
		{"claude-next.exe", true},
		{"myclaude", false}, // prefix doesn't match — not routed to claude
		{"", false},
	}
	for _, tc := range cases {
		got := isClaudeCommand(tc.in)
		if got != tc.want {
			t.Errorf("isClaudeCommand(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsCodexCommand(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"codex", true},
		{"Codex", true},
		{"codex.exe", true},
		{"codex.cmd", true},
		{"/usr/local/bin/codex", true},
		{`C:\Users\u\AppData\Roaming\npm\codex.cmd`, true},
		{"./codex", true},
		{"codex-next", true},
		{"claude", false},
		{"gemini", false},
		{"agy", false},
		{"mycodex", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isCodexCommand(tc.in)
		if got != tc.want {
			t.Errorf("isCodexCommand(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsRelativeExplicitPath(t *testing.T) {
	// resolveExecutable must skip exec.LookPath for cwd-relative paths so
	// the binary is resolved against the session's cwd at exec time, not
	// the agent's. Bare names (PATH lookup), absolute Unix paths, and
	// Windows drive-letter paths are NOT cwd-relative.
	cases := []struct {
		in   string
		want bool
	}{
		{"./claude", true},
		{"../tools/claude", true},
		{"bin/claude", true},
		{`.\claude.cmd`, true},
		{`tools\claude.exe`, true},
		{"/opt/claude-nightly/claude", false},
		{"/usr/local/bin/claude", false},
		{`C:\tools\claude.cmd`, false},
		{`c:\tools\claude.cmd`, false},
		{"claude", false}, // bare name → PATH lookup, not explicit path
		{"claude.exe", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isRelativeExplicitPath(tc.in); got != tc.want {
			t.Errorf("isRelativeExplicitPath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestResolveExecutable_PreservesCwdRelativePath(t *testing.T) {
	// A relative explicit path like `./claude` must be returned unchanged
	// so exec.Command + proc.Dir resolves it against the session's cwd.
	// Running exec.LookPath here would resolve against the AGENT's cwd
	// and silently launch the wrong binary (or none) when the agent and
	// session live in different directories — the pre-fix bug Codex flagged.
	cases := []string{
		"./claude",
		"../bin/claude",
		"bin/claude",
		`.\claude.cmd`,
	}
	for _, in := range cases {
		if got := resolveExecutable(in); got != in {
			t.Errorf("resolveExecutable(%q) = %q, want %q (cwd-relative paths must pass through unchanged)",
				in, got, in)
		}
	}
}
