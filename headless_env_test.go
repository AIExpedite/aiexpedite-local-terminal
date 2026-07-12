// File: headless_env_test.go
// Tests for the non-interactive headless env overlay, test-runner profile
// detection, PTY eligibility guardrails, and effective-command unwrapping.
package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// clearAmbientTestRunnerEnv removes any inherited test-runner env keys (notably
// CI, which GitHub Actions exports as "true") for the duration of a test so the
// wiring assertions below observe only what hardenNonAgentCommand adds — not
// what the CI runner already put in os.Environ(). Without this these tests are
// non-hermetic on a CI runner: an ambient CI=true both masks the "non-test
// command gets no CI" check and blocks testRunnerEnvDefaults from filling CI=1
// (it only fills MISSING keys). Values are restored via t.Cleanup.
func clearAmbientTestRunnerEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CI", "FORCE_COLOR", "NO_COLOR", "PYTHONUNBUFFERED"} {
		key := key
		if v, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { os.Setenv(key, v) })
		} else {
			t.Cleanup(func() { os.Unsetenv(key) })
		}
		os.Unsetenv(key)
	}
}

func TestHardenNonAgentCommand_AppliesEnvOverlayToCmd(t *testing.T) {
	// The security-critical wiring: hardenNonAgentCommand must actually stamp
	// the authoritative non-interactive overlay onto the spawned command's env.
	clearAmbientTestRunnerEnv(t)
	c := exec.Command("git", "status")
	hardenNonAgentCommand(c, "git status")
	m := envMap(c.Env)
	if m["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT not forced on cmd env: %q", m["GIT_TERMINAL_PROMPT"])
	}
	if m["GIT_EDITOR"] != "true" || m["GCM_INTERACTIVE"] != "never" {
		t.Errorf("expected editor/credential hardening on cmd env, got %v", m)
	}
	// A non-test command must NOT get the test-runner niceties.
	if _, present := m["CI"]; present {
		t.Errorf("non-test command should not receive CI=1")
	}
}

func TestHardenNonAgentCommand_TestRunnerGetsCIDefaults(t *testing.T) {
	clearAmbientTestRunnerEnv(t)
	c := exec.Command("bash", "-c", "pytest -q")
	hardenNonAgentCommand(c, effectiveCommandLine("bash", []string{"-c", "pytest -q"}))
	m := envMap(c.Env)
	if m["CI"] != "1" || m["PYTHONUNBUFFERED"] != "1" {
		t.Errorf("test-runner command should get CI/unbuffering defaults, got %v", m)
	}
	// Safety overlay still applies underneath the test profile.
	if m["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("safety overlay missing on test-runner command: %v", m)
	}
}

func TestHardenNonAgentCommand_PreservesPreSanitizedEnv(t *testing.T) {
	// When the caller has already sanitized c.Env (as StartSession does via
	// prepareClaudeChildEnv, stripping CLAUDE_* from a utility session), that
	// filtered env must be the base — hardening layers on top instead of
	// reverting to os.Environ() and reintroducing the stripped variables.
	clearAmbientTestRunnerEnv(t)
	c := exec.Command("git", "status")
	c.Env = []string{"PATH=/usr/bin", "SAFE=keep"} // pre-sanitized: no CLAUDE_*
	t.Setenv("CLAUDE_NESTED_MARKER", "leaked")     // present in os.Environ()
	hardenNonAgentCommand(c, "git status")
	m := envMap(c.Env)
	if _, leaked := m["CLAUDE_NESTED_MARKER"]; leaked {
		t.Errorf("hardening reintroduced a stripped env var from os.Environ(): %v", m)
	}
	if m["SAFE"] != "keep" {
		t.Errorf("pre-sanitized caller env not preserved as base: %v", m)
	}
	// Authoritative overlay still wins on top of the preserved base.
	if m["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("overlay must still apply over the preserved base: %v", m)
	}
}

func envMap(kvs []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func TestHeadlessEnvOverlay_ForcesNonInteractiveKeys(t *testing.T) {
	m := envMap(headlessEnvOverlay())
	want := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_MERGE_AUTOEDIT":  "no",
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GCM_INTERACTIVE":     "never",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("overlay[%s] = %q, want %q", k, m[k], v)
		}
	}
	// Askpass helper should be wired so ssh/credential prompts fail fast.
	if m["GIT_ASKPASS"] == "" || m["SSH_ASKPASS"] == "" {
		t.Errorf("expected GIT_ASKPASS and SSH_ASKPASS to be set, got %v", m)
	}
	if m["SSH_ASKPASS_REQUIRE"] != "force" {
		t.Errorf("expected SSH_ASKPASS_REQUIRE=force, got %q", m["SSH_ASKPASS_REQUIRE"])
	}
}

func TestTestRunnerEnvDefaults_OnlyFillsMissing(t *testing.T) {
	// CI is already set by the caller; it must be preserved (not overridden).
	base := []string{"CI=already", "PATH=/usr/bin"}
	m := envMap(testRunnerEnvDefaults(base))
	if _, present := m["CI"]; present {
		t.Errorf("CI should not be re-added when caller already set it: %v", m)
	}
	if m["FORCE_COLOR"] != "0" || m["NO_COLOR"] != "1" || m["PYTHONUNBUFFERED"] != "1" {
		t.Errorf("expected quiet/unbuffered defaults, got %v", m)
	}
}

func TestIsTestRunnerCommand(t *testing.T) {
	yes := []string{
		"jest", "jest --ci", "vitest run", "pytest -q tests/",
		"python -m pytest", "npm test", "pnpm run test", "yarn test",
		"npx jest src", "bats test.bats", "tox",
	}
	no := []string{
		"git status", "bash deploy.sh", "node server.js", "npm run build",
		"pytest-helper", "", "echo test",
	}
	for _, c := range yes {
		if !isTestRunnerCommand(c) {
			t.Errorf("isTestRunnerCommand(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if isTestRunnerCommand(c) {
			t.Errorf("isTestRunnerCommand(%q) = true, want false", c)
		}
	}
}

func TestIsPTYEligibleCommand_AllowlistOnly(t *testing.T) {
	type tc struct {
		cmd  string
		args []string
	}
	// Only recognized resident TUI agents are PTY-eligible; robust to path/suffix.
	// Direct argv: punctuation inside a literal prompt/argument is NOT chaining, so
	// an eligible agent still rides the PTY even when an argument contains `;`/`&`/`|`.
	eligible := []tc{
		{"agy", nil},
		{"antigravity", []string{"chat"}},
		{"agy", []string{"--brief", "x"}},
		{"/usr/local/bin/agy", nil},
		{"agy.exe", []string{"run"}},
		// Windows launcher shims must normalize the same as commandBaseName —
		// otherwise a tty=true agy.cmd skips PTY routing and falls back to pipes.
		{"agy.cmd", []string{"run"}},
		{"antigravity.cmd", nil},
		{"agy.bat", nil},
		{"agy.ps1", nil},
		{`C:\tools\agy.cmd`, []string{"go"}},
		{"agy", []string{"--print", "fix A; then B"}},
		{"agy", []string{"--message", "use a && b || c | d"}},
		// A bash-wrapped SINGLE agent invocation is still eligible.
		{"bash", []string{"-c", "agy --brief x"}},
	}
	// Everything else stays on the hardened pipe path even with tty=true — this
	// is the security guardrail: unsigned tty can't flip a utility onto a PTY.
	ineligible := []tc{
		{"git", []string{"fetch", "--all"}}, {"git", nil},
		{"/usr/bin/git", []string{"fetch"}}, {"git.exe", []string{"pull"}},
		{"pytest", nil}, {"npm", []string{"test"}}, {"jest", nil},
		{"bash", nil}, {"bash", []string{"-c", "echo hi"}}, {"sh", nil},
		{"powershell", nil}, {"pwsh", nil},
		{"ssh", []string{"user@host"}}, {"node", []string{"repl.js"}},
		{"claude", []string{"--print", "x"}}, {"codex", nil}, {"grok", nil}, {"gemini", nil},
		// A shell-wrapped payload that chains a non-agent command after the agent
		// must NOT ride the PTY on its first token — the trailing git/test-runner
		// would inherit the controlling terminal and bypass headless hardening.
		{"bash", []string{"-c", "agy && git push"}},
		{"bash", []string{"-c", "agy && npm test"}},
		{"bash", []string{"-c", "agy || git pull"}},
		{"bash", []string{"-c", "agy; git status"}},
		{"bash", []string{"-c", "agy | tee out.log"}},
		{"bash", []string{"-c", "agy & git fetch"}},
		{"bash", []string{"-c", "agy $(git rev-parse HEAD)"}},
		{"bash", []string{"-c", "agy `git rev-parse HEAD`"}},
		// Process substitution / redirection launch or reach another process
		// without any of the chaining operators above — still must not ride the
		// PTY on the agent's first token.
		{"bash", []string{"-c", "agy <(git credential fill)"}},
		{"bash", []string{"-c", "agy >(git hash-object -w --stdin)"}},
		{"bash", []string{"-c", "agy > /dev/tty"}},
		{"bash", []string{"-c", "agy < /dev/tty"}},
		{"", nil},
	}
	for _, c := range eligible {
		if !isPTYEligibleCommand(c.cmd, c.args) {
			t.Errorf("isPTYEligibleCommand(%q, %v) = false, want true", c.cmd, c.args)
		}
	}
	for _, c := range ineligible {
		if isPTYEligibleCommand(c.cmd, c.args) {
			t.Errorf("isPTYEligibleCommand(%q, %v) = true, want false", c.cmd, c.args)
		}
	}
}

func TestEffectiveCommandLine_UnwrapsBashDashC(t *testing.T) {
	got := effectiveCommandLine("bash", []string{"-c", "pytest -q && echo ok"})
	if got != "pytest -q && echo ok" {
		t.Errorf("unwrap bash -c: got %q", got)
	}
	// A wrapped test runner must be detected through the shell wrapper.
	if !isTestRunnerCommand(effectiveCommandLine("bash", []string{"-c", "pytest tests/"})) {
		t.Errorf("expected wrapped pytest to be detected as a test runner")
	}
	// Direct command with args joins normally.
	if got := effectiveCommandLine("git", []string{"status", "-s"}); got != "git status -s" {
		t.Errorf("direct join: got %q", got)
	}
}
