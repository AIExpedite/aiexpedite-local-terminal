// grok_acp_args_test.go
// -----------------------------------------------------------------------------
// Tests for the grok 0.2.59 argv contract + isolated-GROK_HOME isolation that
// replaced the dead `--config`-based security-neutralizer machinery.
//
// VALIDATED CONTRACT: `grok agent --model <model> [--always-approve] stdio`.
// `grok agent` rejects --config / --permission-mode / --no-auto-update with
// "unexpected argument", and flags must precede the `stdio` subcommand.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestBuildGrokACPArgs_DefaultContract pins the validated grok 0.2.59 shape:
// `agent --model grok-build stdio` with NO --config / --permission-mode /
// --no-auto-update, flags BEFORE the `stdio` subcommand, and --always-approve
// omitted by default.
func TestBuildGrokACPArgs_DefaultContract(t *testing.T) {
	got := buildGrokACPArgs(nil, false)
	want := []string{"agent", "--model", "grok-build", "stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildGrokACPArgs(nil, false) = %#v, want %#v", got, want)
	}
}

// TestBuildGrokACPArgs_AlwaysApproveOnlyWhenEnabled pins that --always-approve
// appears (between --model grok-build and stdio) ONLY when allowAlwaysApprove
// is true.
func TestBuildGrokACPArgs_AlwaysApproveOnlyWhenEnabled(t *testing.T) {
	off := buildGrokACPArgs(nil, false)
	if grokArgsContain(off, "--always-approve") {
		t.Fatalf("--always-approve must be absent when disabled; got %#v", off)
	}

	on := buildGrokACPArgs(nil, true)
	want := []string{"agent", "--model", "grok-build", "--always-approve", "stdio"}
	if !reflect.DeepEqual(on, want) {
		t.Fatalf("buildGrokACPArgs(nil, true) = %#v, want %#v", on, want)
	}
	// --always-approve must precede the stdio subcommand (stdio takes no opts).
	idxApprove, idxStdio := -1, -1
	for i, a := range on {
		if a == "--always-approve" {
			idxApprove = i
		}
		if a == "stdio" {
			idxStdio = i
		}
	}
	if idxApprove < 0 || idxStdio < 0 || idxApprove > idxStdio {
		t.Fatalf("--always-approve must come before stdio; got %#v", on)
	}
}

// TestBuildGrokACPArgs_CallerModelOverridesDefault pins that a caller-supplied
// --model (separate-value or equals form, and the -m alias) replaces the
// grok-build default while keeping the rest of the contract intact.
func TestBuildGrokACPArgs_CallerModelOverridesDefault(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"separate_value",
			[]string{"--model", "grok-4-fast"},
			[]string{"agent", "--model", "grok-4-fast", "stdio"},
		},
		{
			"equals_form",
			[]string{"--model=grok-4-fast"},
			[]string{"agent", "--model", "grok-4-fast", "stdio"},
		},
		{
			"short_alias",
			[]string{"-m", "grok-4-fast"},
			[]string{"agent", "--model", "grok-4-fast", "stdio"},
		},
		{
			"last_model_wins",
			[]string{"--model", "first", "--model", "second"},
			[]string{"agent", "--model", "second", "stdio"},
		},
		{
			"empty_model_value_falls_back_to_default",
			[]string{"--model="},
			[]string{"agent", "--model", "grok-build", "stdio"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGrokACPArgs(%#v) = %#v, want %#v", c.args, got, c.want)
			}
		})
	}
}

// TestBuildGrokACPArgs_StripsIncompatibleFlags pins that the flags grok 0.2.59
// rejects ("unexpected argument") plus the credential / containment / approval
// side-doors and the POSIX `--` delimiter never reach the argv. Whatever the
// input, the result must be exactly the default contract.
func TestBuildGrokACPArgs_StripsIncompatibleFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"config_separate", []string{"--config", "model.api_key=xai-abc"}},
		{"config_short", []string{"-c", "approval.mode=always"}},
		{"config_equals", []string{"--config=permission_rules=Bash(*)"}},
		{"permission_mode_separate", []string{"--permission-mode", "bypassPermissions"}},
		{"permission_mode_equals", []string{"--permission-mode=bypassPermissions"}},
		{"no_auto_update", []string{"--no-auto-update"}},
		{"auto_update", []string{"--auto-update"}},
		{"api_key_separate", []string{"--api-key", "xai-abc"}},
		{"api_key_equals", []string{"--api-key=xai-abc"}},
		{"auth_method", []string{"--auth", "xai.api_key"}},
		{"cwd_separate", []string{"--cwd", "/tmp/other"}},
		{"cwd_equals", []string{"--cwd=/tmp/other"}},
		{"always_approve_injected_by_caller", []string{"--always-approve"}},
		{"end_of_options_delimiter", []string{"--", "--config", "model.api_key=x"}},
		{"duplicate_entry_tokens", []string{"agent", "stdio", "chat", "tui", "run"}},
	}
	want := []string{"agent", "--model", "grok-build", "stdio"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("buildGrokACPArgs(%#v) = %#v, want %#v (incompatible flag leaked)", c.args, got, want)
			}
		})
	}
}

// TestBuildGrokACPArgs_PreservesUnknownGrokAgentFlags pins that flags we don't
// special-case (valid `grok agent` knobs the orchestrator may pass) flow
// through verbatim and stay BEFORE the stdio subcommand.
func TestBuildGrokACPArgs_PreservesUnknownGrokAgentFlags(t *testing.T) {
	got := buildGrokACPArgs([]string{"--reasoning-effort", "high", "--model", "grok-4"}, false)
	want := []string{"agent", "--model", "grok-4", "--reasoning-effort", "high", "stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildGrokACPArgs preserved unknown flags wrong: got %#v, want %#v", got, want)
	}
}

// TestSanitizeGrokACPExtraArgs_ExtractsModelAndStripsDangerousFlags pins the
// (model, cleaned) contract of the simplified sanitiser.
func TestSanitizeGrokACPExtraArgs_ExtractsModelAndStripsDangerousFlags(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantModel   string
		wantCleaned []string
	}{
		{
			"defaults_when_no_model",
			[]string{"--reasoning-effort", "high"},
			"grok-build",
			[]string{"--reasoning-effort", "high"},
		},
		{
			"extracts_model_drops_config_keeps_rest",
			[]string{"--model", "grok-4", "--config", "model.api_key=xai", "--reasoning-effort", "low"},
			"grok-4",
			[]string{"--reasoning-effort", "low"},
		},
		{
			"strips_all_dangerous",
			[]string{"--no-auto-update", "--api-key", "xai", "--permission-mode", "bypassPermissions", "--cwd", "/x", "--always-approve", "--"},
			"grok-build",
			[]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model, cleaned := sanitizeGrokACPExtraArgs(c.args, "grok-build")
			if model != c.wantModel {
				t.Fatalf("model = %q, want %q", model, c.wantModel)
			}
			if len(cleaned) == 0 && len(c.wantCleaned) == 0 {
				return
			}
			if !reflect.DeepEqual(cleaned, c.wantCleaned) {
				t.Fatalf("cleaned = %#v, want %#v", cleaned, c.wantCleaned)
			}
		})
	}
}

// TestSetupIsolatedGrokHome_CopiesAuthAndWritesCleanConfig pins the isolation
// setup: the temp dir gets a copy of the real auth.json plus a minimal clean
// config.toml carrying no api_key / approval knobs.
func TestSetupIsolatedGrokHome_CopiesAuthAndWritesCleanConfig(t *testing.T) {
	realHome := t.TempDir()
	const authBody = `{"token":"abc"}`
	if err := os.WriteFile(filepath.Join(realHome, "auth.json"), []byte(authBody), 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	t.Setenv("GROK_HOME", realHome)

	dir, err := setupIsolatedGrokHome()
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	if dir == realHome {
		t.Fatalf("isolated home must differ from real GROK_HOME")
	}
	auth, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatalf("isolated auth.json not written: %v", err)
	}
	if string(auth) != authBody {
		t.Fatalf("isolated auth.json content = %q", auth)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("isolated config.toml not written: %v", err)
	}
	if !strings.Contains(string(cfg), `installer = "internal"`) {
		t.Fatalf("config.toml missing clean installer line: %q", cfg)
	}
	if strings.Contains(string(cfg), "api_key") || strings.Contains(string(cfg), "approve") {
		t.Fatalf("clean config.toml must not carry api_key/approval knobs: %q", cfg)
	}
}

// TestSetupIsolatedGrokHome_MissingAuthTolerated pins that a missing real auth
// file is NOT fatal — the isolated dir is still created with a clean config so
// grok surfaces any auth error through the normal ACP flow.
func TestSetupIsolatedGrokHome_MissingAuthTolerated(t *testing.T) {
	t.Setenv("GROK_HOME", t.TempDir()) // empty: no auth.json

	dir, err := setupIsolatedGrokHome()
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome must tolerate missing auth: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	if _, err := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no auth.json when source missing; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("config.toml must exist even without auth: %v", err)
	}
}

// TestSetEnvVar_ReplacesOrAppends pins the GROK_HOME override helper.
func TestSetEnvVar_ReplacesOrAppends(t *testing.T) {
	in := []string{"PATH=/usr/bin", "GROK_HOME=/old/.grok", "HOME=/home/u"}
	got := setEnvVar(in, "GROK_HOME", "/iso/home")
	if !grokArgsContain(got, "GROK_HOME=/iso/home") {
		t.Fatalf("expected GROK_HOME replaced; got %#v", got)
	}
	if grokArgsContain(got, "GROK_HOME=/old/.grok") {
		t.Fatalf("old GROK_HOME must be gone; got %#v", got)
	}
	n := 0
	for _, e := range got {
		if strings.HasPrefix(e, "GROK_HOME=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one GROK_HOME entry; got %d (%#v)", n, got)
	}

	appended := setEnvVar([]string{"PATH=/usr/bin"}, "GROK_HOME", "/iso/home")
	if !grokArgsContain(appended, "GROK_HOME=/iso/home") {
		t.Fatalf("expected GROK_HOME appended when absent; got %#v", appended)
	}
}

// grokArgsContain reports whether args contains target exactly.
func grokArgsContain(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}
