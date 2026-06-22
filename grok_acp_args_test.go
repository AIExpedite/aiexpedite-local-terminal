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
		{"auto_approve_alias_injected_by_caller", []string{"--auto-approve"}},
		{"auto_approve_alias_equals_form", []string{"--auto-approve=true"}},
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
			model, cleaned := sanitizeGrokACPExtraArgs(c.args, "grok-build", false)
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

	dir, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
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
	if !strings.Contains(string(cfg), `auto_update = false`) {
		t.Fatalf("config.toml missing auto_update = false: %q", cfg)
	}
	// Vendor-MCP scan must be disabled for both Cursor and Claude — without
	// these, grok scans the host's `~/.cursor/mcp.json` / `~/.claude.json`
	// at session/new and a slow vendor MCP blocks the ACP turn.
	if !strings.Contains(string(cfg), "[compat.cursor]") || !strings.Contains(string(cfg), "[compat.claude]") {
		t.Fatalf("config.toml missing [compat.cursor]/[compat.claude] sections: %q", cfg)
	}
	for _, section := range []string{"compat.cursor", "compat.claude"} {
		header := "[" + section + "]"
		idx := strings.Index(string(cfg), header)
		if idx < 0 {
			t.Fatalf("config.toml missing %s section: %q", header, cfg)
		}
		rest := string(cfg)[idx+len(header):]
		if next := strings.Index(rest, "\n["); next >= 0 {
			rest = rest[:next]
		}
		if !strings.Contains(rest, "mcps = false") {
			t.Fatalf("%s section missing mcps = false: %q", header, cfg)
		}
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

	dir, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
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

// TestBuildGrokACPArgs_StripsAPIKeyArgsUnconditionally pins that caller-
// supplied `--api-key{,-env}` / `--auth{,-method}` (and their separate-value
// tokens) NEVER reach the argv — not even when the workspace opts into
// EnableGrokAPIKeyFallback. `grok agent` rejects these flags ("unexpected
// argument"); the opt-in fallback rides XAI_API_KEY (sanitizeGrokACPEnv) and
// the persisted `[model] api_key` config.toml line (setupIsolatedGrokHome)
// instead. Both surfaces are pinned by dedicated tests
// (TestSanitizeGrokACPEnv_PreservesXAIKeyWhenFallbackEnabled and
// TestSetupIsolatedGrokHome_PreservesPersistedAPIKeyWhenFallbackEnabled).
func TestBuildGrokACPArgs_StripsAPIKeyArgsUnconditionally(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"api_key_separate_value", []string{"--api-key", "xai-abc"}},
		{"api_key_equals_form", []string{"--api-key=xai-abc"}},
		{"api_key_env_separate_value", []string{"--api-key-env", "OTHER_KEY_VAR"}},
		{"auth_method_separate_value", []string{"--auth", "xai.api_key"}},
	}
	defaultContract := []string{"agent", "--model", "grok-build", "stdio"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, defaultContract) {
				t.Fatalf("buildGrokACPArgs(%#v, false) = %#v, want %#v — api-key arg leaked into argv", c.args, got, defaultContract)
			}
			gotApprove := buildGrokACPArgs(c.args, true)
			wantApprove := []string{"agent", "--model", "grok-build", "--always-approve", "stdio"}
			if !reflect.DeepEqual(gotApprove, wantApprove) {
				t.Fatalf("buildGrokACPArgs(%#v, true) = %#v, want %#v — api-key arg leaked under always-approve gate", c.args, gotApprove, wantApprove)
			}
		})
	}
}

// TestSetupIsolatedGrokHome_PreservesPersistedAPIKeyWhenFallbackEnabled pins
// the second leg of the EnableGrokAPIKeyFallback opt-in: when the user keeps
// their key in xAI's documented persistent form (`~/.grok/config.toml` with
// `[model] api_key`) and does NOT export XAI_API_KEY, the isolated home must
// carry that line over so the fallback actually works. With the gate off, the
// isolated config must stay clean (zero credential exposure).
func TestSetupIsolatedGrokHome_PreservesPersistedAPIKeyWhenFallbackEnabled(t *testing.T) {
	realHome := t.TempDir()
	body := "[model]\napi_key = \"xai-persisted\"\n[approval]\nalways_approve = true\n"
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed source config.toml: %v", err)
	}
	t.Setenv("GROK_HOME", realHome)

	on, err := setupIsolatedGrokHome(true, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome(true): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(on) })
	cfgOn, err := os.ReadFile(filepath.Join(on, "config.toml"))
	if err != nil {
		t.Fatalf("isolated config.toml not written: %v", err)
	}
	if !strings.Contains(string(cfgOn), "xai-persisted") {
		t.Fatalf("expected persisted api_key carried into isolated config; got %q", cfgOn)
	}
	if strings.Contains(string(cfgOn), "always_approve") {
		t.Fatalf("approval knob must NOT leak even when api-key fallback is on; got %q", cfgOn)
	}

	off, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome(false): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(off) })
	cfgOff, err := os.ReadFile(filepath.Join(off, "config.toml"))
	if err != nil {
		t.Fatalf("isolated config.toml not written: %v", err)
	}
	if strings.Contains(string(cfgOff), "api_key") || strings.Contains(string(cfgOff), "xai-persisted") {
		t.Fatalf("api_key must stay stripped when fallback gate is closed; got %q", cfgOff)
	}
}

// TestSetupIsolatedGrokHome_PreservesPerModelAPIKey pins the per-model leg of
// the EnableGrokAPIKeyFallback opt-in: when the user stores the fallback key in
// xAI's documented per-model form (`[model.<runtimeModel>] api_key = "..."`,
// the xAI Enterprise "API key example"), the isolated home must carry that
// section over for the model the agent actually launches under. Otherwise the
// per-session GROK_HOME isolation strips a key the user explicitly opted in to.
// Per-model match also wins over a root `[model] api_key` default — mirroring
// grok's own precedence in the un-isolated config.
func TestSetupIsolatedGrokHome_PreservesPerModelAPIKey(t *testing.T) {
	realHome := t.TempDir()
	body := "[model]\napi_key = \"xai-root-default\"\n" +
		"[model.grok-build]\napi_key = \"xai-per-model\"\n" +
		"[model.grok-other]\napi_key = \"xai-other\"\n"
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed source config.toml: %v", err)
	}
	t.Setenv("GROK_HOME", realHome)

	on, err := setupIsolatedGrokHome(true, "grok-build")
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome(true, grok-build): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(on) })
	cfgOn, err := os.ReadFile(filepath.Join(on, "config.toml"))
	if err != nil {
		t.Fatalf("isolated config.toml not written: %v", err)
	}
	got := string(cfgOn)
	if !strings.Contains(got, "[model.grok-build]") || !strings.Contains(got, "xai-per-model") {
		t.Fatalf("per-model api_key must be carried over under [model.grok-build]; got %q", got)
	}
	if strings.Contains(got, "xai-root-default") {
		t.Fatalf("per-model match must win over root [model] default; got %q", got)
	}
	if strings.Contains(got, "xai-other") || strings.Contains(got, "[model.grok-other]") {
		t.Fatalf("non-matching per-model sections must NOT leak; got %q", got)
	}
}

// TestSetupIsolatedGrokHome_PreservesRootAPIKeyWhenNoPerModelMatch pins that
// the root `[model] api_key` default is still honoured when no per-model
// section matches the runtime model — preserves prior behaviour for users
// whose key lives under the documented default form.
func TestSetupIsolatedGrokHome_PreservesRootAPIKeyWhenNoPerModelMatch(t *testing.T) {
	realHome := t.TempDir()
	body := "[model]\napi_key = \"xai-root-only\"\n[model.grok-other]\napi_key = \"xai-other\"\n"
	if err := os.WriteFile(filepath.Join(realHome, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed source config.toml: %v", err)
	}
	t.Setenv("GROK_HOME", realHome)

	on, err := setupIsolatedGrokHome(true, "grok-build")
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome(true, grok-build): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(on) })
	cfgOn, err := os.ReadFile(filepath.Join(on, "config.toml"))
	if err != nil {
		t.Fatalf("isolated config.toml not written: %v", err)
	}
	got := string(cfgOn)
	if !strings.Contains(got, "xai-root-only") {
		t.Fatalf("root [model] api_key must be carried over when no per-model match; got %q", got)
	}
	if strings.Contains(got, "xai-other") {
		t.Fatalf("non-matching per-model section must NOT leak; got %q", got)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedAPIKey pins the
// fail-closed behaviour for the system-level `/etc/grok/requirements.toml`
// layer (NOT redirected by GROK_HOME): when it pins `model.api_key = "..."`
// and the workspace has NOT opted into EnableGrokAPIKeyFallback, Start must
// refuse to spawn the session rather than launch with the pinned credential.
func TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedAPIKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	if err := os.WriteFile(path, []byte("[model]\napi_key = \"xai-pinned\"\n"), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when api-key pinned and fallback gate closed")
	}
	if err := detectPinnedSystemGrokRequirements(true, false); err != nil {
		t.Fatalf("expected pass when EnableGrokAPIKeyFallback=true acknowledges pin: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedAlwaysApprove mirrors
// the auth-pin path for the approval-policy axis. `always_approve = true` in
// the system layer + EnableGrokAlwaysApprove=false ⇒ refuse.
func TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedAlwaysApprove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "[tools]\nalways_approve = true  # managed\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when always_approve pinned and approval gate closed")
	}
	if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
		t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedPermissionRules
// covers the allow-list bypass surface (e.g. `permission_rules = ["Bash(*)"]`
// or `policy.allow = [...]`) that the boolean/mode-style approval pins miss.
// A non-empty allow rule auto-approves matching tools just like
// `always_approve = true`, so it must trip the same fail-closed refusal.
func TestDetectPinnedSystemGrokRequirements_RefusesOnPinnedPermissionRules(t *testing.T) {
	cases := map[string]string{
		"flat permission_rules": "permission_rules = [\"Bash(*)\"]\n",
		"section permission":    "[permission]\nrules = [\"Bash(*)\"]\nallow = [\"Read(*)\"]\n",
		"policy allow":          "[policy]\nallow = [\"Bash(*)\"]\n",
		"top-level allow":       "allow = [\"Bash(*)\"]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "requirements.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("seed pinned requirements: %v", err)
			}
			orig := grokSystemRequirementsPath
			grokSystemRequirementsPath = path
			t.Cleanup(func() { grokSystemRequirementsPath = orig })

			if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
				t.Fatalf("expected refusal when permission allow-list pinned and approval gate closed")
			}
			if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
				t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
			}
		})
	}
}

// TestDetectPinnedSystemGrokRequirements_EmptyPermissionRulesPasses guards
// the explicit-clear case: `permission_rules = []` is the operator deliberately
// blanking the system layer, the same way `api_key = ""` is treated as a clear
// for the auth path.
func TestDetectPinnedSystemGrokRequirements_EmptyPermissionRulesPasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = []\n[policy]\nallow = []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed empty allow-list requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("empty allow-list must not trigger refusal: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_MissingFileTolerated pins the
// best-effort contract: when there is no system requirements file at all the
// scan returns nil (a missing file is the common case on non-managed hosts).
func TestDetectPinnedSystemGrokRequirements_MissingFileTolerated(t *testing.T) {
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = filepath.Join(t.TempDir(), "does-not-exist.toml")
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("missing system requirements file must be tolerated: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_BenignFilePasses guards against the
// fail-closed scan over-refusing on a benign system file that names neither
// api_key nor approval pins.
func TestDetectPinnedSystemGrokRequirements_BenignFilePasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "[models]\ndefault = \"grok-build\"\n[xai]\napi_base_url = \"https://api.x.ai\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed benign requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("benign requirements file must not trigger refusal: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnLegacyBarePatternAllowRule
// pins the allow-rule axis the boolean/mode-style scan does NOT catch: a
// system-layer `permission_rules = ["Bash(*)"]` (the documented legacy bare-
// pattern allow shortcut) MUST refuse to spawn when EnableGrokAlwaysApprove
// is false, because the isolated GROK_HOME does not redirect /etc/grok and
// Grok would otherwise auto-approve matching tools behind the workspace's
// back.
func TestDetectPinnedSystemGrokRequirements_RefusesOnLegacyBarePatternAllowRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = [\"Bash(*)\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when bare-pattern allow rule pinned and approval gate closed")
	}
	if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
		t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnMultilineAllowActionRule
// pins the multi-line accumulation path: `permission_rules` whose array body
// spans multiple lines must be reassembled before classification — otherwise
// the first-line value `[` would be misread as a benign empty-shape opener
// and an explicit `{action = "allow", pattern = "..."}` on the next line
// would slip past the gate.
func TestDetectPinnedSystemGrokRequirements_RefusesOnMultilineAllowActionRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = [\n  { action = \"allow\", pattern = \"Bash(*)\" }\n]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when multi-line allow-action rule pinned and approval gate closed")
	}
}

// TestDetectPinnedSystemGrokRequirements_PassesOnDenyOnlyMultilineRule guards
// the inverse of the previous case: an MDM-style deny-only
// `permission_rules` (deny takes precedence in xAI's policy model) MUST NOT
// trigger refusal — wiping it would weaken host security in pursuit of a
// non-existent allow.
func TestDetectPinnedSystemGrokRequirements_PassesOnDenyOnlyMultilineRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = [\n  { action = \"deny\", pattern = \"Bash(rm -rf*)\" }\n]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed deny-only requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("deny-only permission_rules must NOT trigger refusal: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnPolicyAllowKey covers the
// `policy.allow` / `permissions.allow` / `tools.allow` cousins. Any non-empty
// value is by definition an allow list and must refuse to spawn when the
// approval gate is closed.
func TestDetectPinnedSystemGrokRequirements_RefusesOnPolicyAllowKey(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"policy_allow", "policy.allow = \"Bash(*)\"\n"},
		{"permissions_allow", "permissions.allow = \"Bash(*)\"\n"},
		{"tools_allow", "tools.allow = \"Bash(*)\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "requirements.toml")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatalf("seed pinned requirements: %v", err)
			}
			orig := grokSystemRequirementsPath
			grokSystemRequirementsPath = path
			t.Cleanup(func() { grokSystemRequirementsPath = orig })

			if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
				t.Fatalf("expected refusal for non-empty %s pin", c.name)
			}
			if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
				t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
			}
		})
	}
}

// TestDetectPinnedSystemGrokRequirements_QuoteAwareInlineCommentStrip pins the
// quote-aware `#` strip: a pinned allow rule whose pattern legitimately
// contains a `#` (e.g. `pattern = "Bash(#magic)"`) must survive comment
// stripping intact and still be classified as an allow. A naive
// IndexByte('#') strip would chop the value at the in-pattern `#` and the
// downstream classifier would never see the `action = "allow"` token.
func TestDetectPinnedSystemGrokRequirements_QuoteAwareInlineCommentStrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = [{ action = \"allow\", pattern = \"Bash(#magic)\" }] # mdm\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal — quote-aware strip must keep the in-pattern `#` and still flag the allow rule")
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnSectionFormPermissionRules
// pins the section-header path: `[permission]\nrules = ["Bash(*)"]` is a
// legal TOML rewrite of the flat `permission.rules = [...]` form, so the
// scanner must qualify the unqualified `rules` key with the active section
// header — otherwise a system-layer allow rule would skip the gate and let
// auto-approve happen behind the workspace's back.
func TestDetectPinnedSystemGrokRequirements_RefusesOnSectionFormPermissionRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "[permission]\nrules = [\"Bash(*)\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed pinned requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when section-form [permission]\\nrules = [...] pinned and approval gate closed")
	}
	if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
		t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_PassesOnDenyOnlyFlatRule guards the
// flat (single-line) deny-only form. Before the lineMentionsGrokApprovalPin
// scoping fix, ANY non-empty `permission_rules` value tripped the broad
// refusal, including `permission_rules = [{action = "deny", ...}]` —
// weakening host security to wipe a deny policy that doesn't even allow
// anything. The structured switch's grokPermissionRulesValueHasAllowAction
// classifier now owns this surface and must let deny-only through.
func TestDetectPinnedSystemGrokRequirements_PassesOnDenyOnlyFlatRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "permission_rules = [{ action = \"deny\", pattern = \"Bash(rm -rf*)\" }]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed deny-only requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("flat deny-only permission_rules must NOT trigger refusal: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_PassesOnSectionFormDenyOnlyRule
// covers the section-header rewrite of the deny-only case: both the section
// qualification AND the deny-vs-allow classification must compose so an MDM
// policy `[permission]\nrules = [{action = "deny", ...}]` stays intact.
func TestDetectPinnedSystemGrokRequirements_PassesOnSectionFormDenyOnlyRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.toml")
	body := "[permission]\nrules = [{ action = \"deny\", pattern = \"Bash(rm -rf*)\" }]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed deny-only section requirements: %v", err)
	}
	orig := grokSystemRequirementsPath
	grokSystemRequirementsPath = path
	t.Cleanup(func() { grokSystemRequirementsPath = orig })

	if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
		t.Fatalf("section-form deny-only permission_rules must NOT trigger refusal: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnFullBypassValueSet pins the
// lockstep contract between the requirements scanner and isGrokApprovalConfigKV
// / isGrokPermissionModeBypassValue. The argv classifier accepts dashed long
// forms (`approval.mode = "always-approve"`) and the claude-style
// `acceptEdits` / `bypassPermissions` selectors on `permission_mode`; the
// requirements gate must too, or a managed host can pin one of those values in
// `/etc/grok/requirements.toml` and route around the per-tool prompt while
// EnableGrokAlwaysApprove is false. The cases mirror the value buckets
// documented in isGrokPermissionModeBypassValue.
func TestDetectPinnedSystemGrokRequirements_RefusesOnFullBypassValueSet(t *testing.T) {
	cases := map[string]string{
		"approval.mode always-approve":      "approval.mode = \"always-approve\"\n",
		"approval.mode auto-approve":        "approval.mode = \"auto-approve\"\n",
		"approval_mode legacy long-form":    "approval_mode = \"always-approve\"\n",
		"permission_mode acceptEdits":       "permission_mode = \"acceptEdits\"\n",
		"permission_mode accept-edits":      "permission_mode = \"accept-edits\"\n",
		"permission_mode bypassPermissions": "permission_mode = \"bypassPermissions\"\n",
		"permission_mode always-approve":    "permission_mode = \"always-approve\"\n",
		"ui.permission_mode acceptEdits":    "[ui]\npermission_mode = \"acceptEdits\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "requirements.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("seed pinned requirements: %v", err)
			}
			orig := grokSystemRequirementsPath
			grokSystemRequirementsPath = path
			t.Cleanup(func() { grokSystemRequirementsPath = orig })

			if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
				t.Fatalf("expected refusal when bypass-value pinned and approval gate closed")
			}
			if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
				t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
			}
		})
	}
}

// TestBuildGrokACPArgs_StripsCallerAllowRuleUnderGateOff pins the gate-off
// strip of caller-supplied `--allow <pattern>` / `--allow=<pattern>` so a
// signed grok_acp_start cannot ferry an autonomous-approval allow rule
// through extras when EnableGrokAlwaysApprove is false. Mirrors the raw
// `session_start` path's stripGrokAllowRulePairs trailing sweep.
func TestBuildGrokACPArgs_StripsCallerAllowRuleUnderGateOff(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"separate_value", []string{"--allow", "Bash(*)"}},
		{"equals_form", []string{"--allow=Bash(*)"}},
		{"multiple_separate", []string{"--allow", "Bash(git *)", "--allow", "WriteFile(*)"}},
	}
	defaultContract := []string{"agent", "--model", "grok-build", "stdio"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, defaultContract) {
				t.Fatalf("buildGrokACPArgs(%#v, false, false) = %#v, want %#v — caller --allow leaked past gate", c.args, got, defaultContract)
			}
		})
	}
}

// TestBuildGrokACPArgs_PreservesCallerAllowRuleUnderGateOn pins the inverse:
// when EnableGrokAlwaysApprove is true the caller has opted into autonomous
// approval, so `--allow` rules flow through verbatim. Also pins that
// `--deny` survives on both sides of the gate (policy-tightening, never
// stripped).
func TestBuildGrokACPArgs_PreservesCallerAllowRuleUnderGateOn(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"allow_separate",
			[]string{"--allow", "Bash(git *)"},
			[]string{"agent", "--model", "grok-build", "--always-approve", "--allow", "Bash(git *)", "stdio"},
		},
		{
			"allow_equals",
			[]string{"--allow=Bash(*)"},
			[]string{"agent", "--model", "grok-build", "--always-approve", "--allow=Bash(*)", "stdio"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, true)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGrokACPArgs(%#v, true, false) = %#v, want %#v", c.args, got, c.want)
			}
		})
	}

	denyArgs := []string{"--deny", "Bash(rm -rf *)"}
	wantOn := []string{"agent", "--model", "grok-build", "--always-approve", "--deny", "Bash(rm -rf *)", "stdio"}
	if got := buildGrokACPArgs(denyArgs, true); !reflect.DeepEqual(got, wantOn) {
		t.Fatalf("--deny stripped under gate-on: got %#v, want %#v", got, wantOn)
	}
	wantOff := []string{"agent", "--model", "grok-build", "--deny", "Bash(rm -rf *)", "stdio"}
	if got := buildGrokACPArgs(denyArgs, false); !reflect.DeepEqual(got, wantOff) {
		t.Fatalf("--deny stripped under gate-off: got %#v, want %#v", got, wantOff)
	}
}

// TestDetectPinnedSystemGrokRequirements_ScansManagedConfigPath pins the
// fail-closed behaviour for the second system-level layer
// `/etc/grok/managed_config.toml`. It is NOT redirected by GROK_HOME, so a
// pin there must trip the same refusal as `/etc/grok/requirements.toml`.
func TestDetectPinnedSystemGrokRequirements_ScansManagedConfigPath(t *testing.T) {
	emptyDir := t.TempDir()
	emptyPath := filepath.Join(emptyDir, "requirements.toml")
	if err := os.WriteFile(emptyPath, []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty requirements: %v", err)
	}
	origReq := grokSystemRequirementsPath
	grokSystemRequirementsPath = emptyPath
	t.Cleanup(func() { grokSystemRequirementsPath = origReq })

	cases := map[string]string{
		"managed_config pins api_key":                "[model]\napi_key = \"xai-mc\"\n",
		"managed_config pins always_approve":         "[tools]\nalways_approve = true\n",
		"managed_config pins permission_rules allow": "permission_rules = [\"Bash(*)\"]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "managed_config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("seed managed_config: %v", err)
			}
			origMC := grokSystemManagedConfigPath
			grokSystemManagedConfigPath = path
			t.Cleanup(func() { grokSystemManagedConfigPath = origMC })

			if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
				t.Fatalf("expected refusal when managed_config pins bypass posture and gate closed")
			}
			if err := detectPinnedSystemGrokRequirements(true, true); err != nil {
				t.Fatalf("expected pass when both gates open: %v", err)
			}
		})
	}
}

// TestDetectPinnedSystemGrokRequirements_RefusesOnClaudeManagedAllowRule pins
// that an imported Claude `managed-settings.json` with a non-empty
// `permissions.allow` array trips the same fail-closed refusal under
// gate-off — the per-session isolated GROK_HOME does not relocate these
// MDM-managed files, so Grok's enterprise loader still imports the rules
// before the per-tool prompt.
func TestDetectPinnedSystemGrokRequirements_RefusesOnClaudeManagedAllowRule(t *testing.T) {
	emptyDir := t.TempDir()
	emptyReq := filepath.Join(emptyDir, "requirements.toml")
	if err := os.WriteFile(emptyReq, []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty requirements: %v", err)
	}
	emptyMC := filepath.Join(emptyDir, "managed_config.toml")
	if err := os.WriteFile(emptyMC, []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty managed_config: %v", err)
	}
	origReq := grokSystemRequirementsPath
	origMC := grokSystemManagedConfigPath
	grokSystemRequirementsPath = emptyReq
	grokSystemManagedConfigPath = emptyMC
	t.Cleanup(func() {
		grokSystemRequirementsPath = origReq
		grokSystemManagedConfigPath = origMC
	})

	dir := t.TempDir()
	claudePath := filepath.Join(dir, "managed-settings.json")
	if err := os.WriteFile(claudePath, []byte(`{"permissions":{"allow":["Bash(*)"]}}`), 0o600); err != nil {
		t.Fatalf("seed claude managed-settings: %v", err)
	}
	origFn := claudeManagedSettingsPathsFn
	claudeManagedSettingsPathsFn = func() []string { return []string{claudePath} }
	t.Cleanup(func() { claudeManagedSettingsPathsFn = origFn })

	if err := detectPinnedSystemGrokRequirements(false, false); err == nil {
		t.Fatalf("expected refusal when claude managed-settings pins permissions.allow and approval gate closed")
	}
	if err := detectPinnedSystemGrokRequirements(false, true); err != nil {
		t.Fatalf("expected pass when EnableGrokAlwaysApprove=true acknowledges pin: %v", err)
	}
}

// TestDetectPinnedSystemGrokRequirements_PassesOnEmptyClaudeAllowList guards
// the empty-list path: present-but-empty / absent `permissions.allow` must
// NOT refuse — only non-empty rules count, otherwise we over-fail-closed for
// hosts that ship the file for non-allow-related settings.
func TestDetectPinnedSystemGrokRequirements_PassesOnEmptyClaudeAllowList(t *testing.T) {
	emptyDir := t.TempDir()
	emptyReq := filepath.Join(emptyDir, "requirements.toml")
	if err := os.WriteFile(emptyReq, []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty requirements: %v", err)
	}
	emptyMC := filepath.Join(emptyDir, "managed_config.toml")
	if err := os.WriteFile(emptyMC, []byte(""), 0o600); err != nil {
		t.Fatalf("seed empty managed_config: %v", err)
	}
	origReq := grokSystemRequirementsPath
	origMC := grokSystemManagedConfigPath
	grokSystemRequirementsPath = emptyReq
	grokSystemManagedConfigPath = emptyMC
	t.Cleanup(func() {
		grokSystemRequirementsPath = origReq
		grokSystemManagedConfigPath = origMC
	})

	cases := map[string]string{
		"empty allow array":    `{"permissions":{"allow":[]}}`,
		"no permissions key":   `{"otherKey":true}`,
		"whitespace-only rule": `{"permissions":{"allow":["  "]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			claudePath := filepath.Join(dir, "managed-settings.json")
			if err := os.WriteFile(claudePath, []byte(body), 0o600); err != nil {
				t.Fatalf("seed claude managed-settings: %v", err)
			}
			origFn := claudeManagedSettingsPathsFn
			claudeManagedSettingsPathsFn = func() []string { return []string{claudePath} }
			t.Cleanup(func() { claudeManagedSettingsPathsFn = origFn })

			if err := detectPinnedSystemGrokRequirements(false, false); err != nil {
				t.Fatalf("expected pass on benign claude managed-settings (%s): %v", name, err)
			}
		})
	}
}
