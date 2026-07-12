// File: headless_env_test.go
// Tests for the non-interactive headless env overlay, test-runner profile
// detection, PTY eligibility guardrails, and effective-command unwrapping.
package main

import (
	"strings"
	"testing"
)

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

func TestIsPTYIneligibleCommand_GitAndTestRunnersForcedOffPTY(t *testing.T) {
	ineligible := []string{"git fetch --all", "git", "pytest", "npm test", "jest"}
	eligible := []string{"agy", "antigravity chat", "node repl.js", "bash"}
	for _, c := range ineligible {
		if !isPTYIneligibleCommand(c) {
			t.Errorf("isPTYIneligibleCommand(%q) = false, want true", c)
		}
	}
	for _, c := range eligible {
		if isPTYIneligibleCommand(c) {
			t.Errorf("isPTYIneligibleCommand(%q) = true, want false", c)
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
