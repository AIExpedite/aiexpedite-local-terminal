package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Grok 1.0.13 uses an explicit empty --tools value for the maintenance
// no-tools smoke. The empty argv token must remain attached to --tools; if the
// builder treats it as prompt text, Grok consumes the managed -p as the tool
// list and exits with a protocol error before emitting the marker.
func TestBuildGrokInteractiveArgs_PreservesNoToolsSmokeContract(t *testing.T) {
	got := buildGrokInteractiveArgs([]string{
		grokMaintenanceSmokeControlArg,
		"--tools", "", "--disable-web-search", "--no-subagents",
		"--max-turns", "1", "--verbatim", "return the marker",
	}, false)
	want := []string{
		"--output-format", "streaming-json", "--no-auto-update",
		"--tools", "", "--disable-web-search", "--no-subagents",
		"--max-turns", "1", "--verbatim", "-p", "return the marker",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no-tools smoke argv = %#v, want %#v", got, want)
	}
}

func TestExtractGrokMaintenanceSmokeControl_IsExplicitAndNeverForwarded(t *testing.T) {
	ordinary := []string{"--tools", "", "marker"}
	cleaned, requested := extractGrokMaintenanceSmokeControl(ordinary)
	if requested || !reflect.DeepEqual(cleaned, ordinary) {
		t.Fatalf("ordinary no-tools request became maintenance smoke: requested=%t cleaned=%#v", requested, cleaned)
	}

	controlled := []string{grokMaintenanceSmokeControlArg, "--tools", "", "marker"}
	cleaned, requested = extractGrokMaintenanceSmokeControl(controlled)
	if !requested || !reflect.DeepEqual(cleaned, ordinary) {
		t.Fatalf("explicit maintenance control not consumed: requested=%t cleaned=%#v", requested, cleaned)
	}
	for _, arg := range buildGrokInteractiveArgs(controlled, false) {
		if arg == grokMaintenanceSmokeControlArg {
			t.Fatalf("internal maintenance control reached Grok argv")
		}
	}
}

func TestSessionStartArgsForCommand_PromotesSerializedMaintenanceSmoke(t *testing.T) {
	const payload = `{
		"id":"maintenance-smoke-1",
		"type":"session_start",
		"sessionID":"grok-smoke-session",
		"command":"grok",
		"args":["--tools","","--disable-web-search","--no-subagents","--max-turns","1","--verbatim","Return exactly this marker and nothing else: AIEXPEDITE_GROK_SMOKE_MARKER_7F3C2A"],
		"ts":1770000000000
	}`
	var cmd commandMsg
	if err := json.Unmarshal([]byte(payload), &cmd); err != nil {
		t.Fatalf("unmarshal serialized maintenance command: %v", err)
	}
	const commandSecret = "maintenance-dispatch-secret"
	signedPayload, err := json.Marshal(signaturePayload{
		ID: cmd.ID, Command: cmd.Command, Args: cmd.Args, Ts: cmd.Ts,
		Type: cmd.Type, SessionID: cmd.SessionID,
	})
	if err != nil {
		t.Fatalf("marshal signed maintenance payload: %v", err)
	}
	cmd.Signature = generateHMAC(string(signedPayload), commandSecret)
	wirePayload, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal wire maintenance command: %v", err)
	}
	var received commandMsg
	if err := json.Unmarshal(wirePayload, &received); err != nil {
		t.Fatalf("unmarshal wire maintenance command: %v", err)
	}
	if !verifySignature(received, commandSecret) {
		t.Fatal("serialized maintenance args were not covered by the command signature")
	}
	cmd = received
	original := append([]string(nil), cmd.Args...)
	dispatched := sessionStartArgsForCommand(cmd)
	cleaned, promoted := extractGrokMaintenanceSmokeControl(dispatched)
	if !promoted {
		t.Fatalf("serialized production smoke was not promoted: %#v", dispatched)
	}
	if !reflect.DeepEqual(cleaned, original) {
		t.Fatalf("promotion changed signed smoke args: got %#v, want %#v", cleaned, original)
	}
	if !reflect.DeepEqual(cmd.Args, original) {
		t.Fatalf("dispatch mutated the deserialized signed args: got %#v, want %#v", cmd.Args, original)
	}

	ordinary := commandMsg{Command: "grok", Args: []string{"--tools", "", "ordinary no-tools prompt"}}
	if got := sessionStartArgsForCommand(ordinary); !reflect.DeepEqual(got, ordinary.Args) {
		t.Fatalf("ordinary no-tools request was promoted: %#v", got)
	}

	conflicting := cmd
	conflicting.Args = append(append([]string(nil), cmd.Args...), "--tools", "Bash")
	dispatchedConflicting := sessionStartArgsForCommand(conflicting)
	cleanedConflicting, promoted := extractGrokMaintenanceSmokeControl(dispatchedConflicting)
	if !promoted {
		t.Fatalf("malformed reserved smoke was not promoted for fail-closed validation: %#v", conflicting.Args)
	}
	if err := validateGrokMaintenanceSmokeRequestArgs(cleanedConflicting); err == nil {
		t.Fatalf("conflicting serialized smoke contract was accepted: %#v", cleanedConflicting)
	}
}

func TestGrokArgsRequestNoTools_RequiresExplicitEmptyValue(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{buildGrokInteractiveArgs([]string{"--tools", "", "marker"}, false), true},
		{buildGrokInteractiveArgs([]string{"--tools=", "marker"}, false), true},
		{buildGrokInteractiveArgs([]string{"--tools", "Bash", "marker"}, false), false},
		{buildGrokInteractiveArgs([]string{"--tools", "", "--tools", "Bash", "marker"}, false), false},
		{buildGrokInteractiveArgs([]string{"--tools=", "--tools", "", "marker"}, false), false},
		{buildGrokInteractiveArgs([]string{"--disable-web-search", "marker"}, false), false},
		{[]string{"--tools", "", "models"}, false},
	}
	for _, tc := range tests {
		if got := grokArgsRequestNoTools(tc.args); got != tc.want {
			t.Errorf("grokArgsRequestNoTools(%#v) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestValidateGrokMaintenanceSmokeContract_RequiresAllSafetyControls(t *testing.T) {
	prompt := grokMaintenanceSmokePromptPrefix + "TEST_MARKER"
	valid := buildGrokInteractiveArgs([]string{
		"--tools", "", "--disable-web-search", "--no-subagents",
		"--max-turns", "1", "--verbatim", prompt,
	}, false)
	if err := validateGrokMaintenanceSmokeContract(valid); err != nil {
		t.Fatalf("valid maintenance contract rejected: %v", err)
	}

	for _, missing := range []string{"--disable-web-search", "--no-subagents", "--verbatim", "--max-turns"} {
		trimmed := make([]string, 0, len(valid))
		dropValue := false
		for _, arg := range valid {
			if dropValue {
				dropValue = false
				continue
			}
			if arg == missing {
				dropValue = missing == "--max-turns"
				continue
			}
			trimmed = append(trimmed, arg)
		}
		if err := validateGrokMaintenanceSmokeContract(trimmed); err == nil {
			t.Errorf("contract missing %s was accepted: %#v", missing, trimmed)
		}
	}

	for _, duplicate := range [][]string{
		{"--max-turns", "1"},
		{"--max-turns=5"},
		{"--disable-web-search"},
		{"--no-subagents"},
		{"--verbatim"},
	} {
		conflicting := append(append([]string(nil), valid...), duplicate...)
		if err := validateGrokMaintenanceSmokeContract(conflicting); err == nil {
			t.Errorf("contract with duplicate/conflicting controls was accepted: %#v", conflicting)
		}
	}
	dangling := append(append([]string(nil), valid...), "--max-turns")
	if err := validateGrokMaintenanceSmokeContract(dangling); err == nil {
		t.Errorf("contract with dangling --max-turns was accepted: %#v", dangling)
	}
}

func TestValidateGrokMaintenanceSmokeRequestArgs_RejectsEveryExtraOption(t *testing.T) {
	prompt := grokMaintenanceSmokePromptPrefix + "TEST_MARKER"
	valid := []string{
		"--tools", "", "--disable-web-search", "--no-subagents",
		"--max-turns", "1", "--verbatim", prompt,
	}
	if err := validateGrokMaintenanceSmokeRequestArgs(valid); err != nil {
		t.Fatalf("canonical request rejected: %v", err)
	}

	tests := [][]string{
		{"--json-schema", `{"type":"string","secret":"schema-sentinel"}`},
		{"--system-prompt-override", "system-prompt-sentinel"},
		{"--rules", "rules-sentinel"},
		{"--model", "provider-sentinel"},
		{"--sandbox", "sandbox-sentinel"},
		{"--debug-file", filepath.Join(t.TempDir(), "debug-path-sentinel")},
		{"--always-approve"},
		{"--auto-approve"},
		{"--permission-mode", "bypassPermissions"},
		{"--allow", "MCPTool(*)"},
	}
	for _, extra := range tests {
		candidate := append([]string(nil), valid[:len(valid)-1]...)
		candidate = append(candidate, extra...)
		candidate = append(candidate, prompt)
		err := validateGrokMaintenanceSmokeRequestArgs(candidate)
		if err == nil {
			t.Errorf("maintenance request with extra option was accepted: %#v", extra)
			continue
		}
		for _, sentinel := range []string{
			"schema-sentinel", "system-prompt-sentinel", "rules-sentinel", "provider-sentinel",
			"sandbox-sentinel", "debug-path-sentinel", "bypassPermissions", "MCPTool",
		} {
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("maintenance rejection leaked %q: %v", sentinel, err)
			}
		}
	}
}

func TestStartSession_GrokNoToolsRejectsExternalLoaders(t *testing.T) {
	tests := []struct {
		args       []string
		wantFlag   string
		secretText string
	}{
		{[]string{"--tools", "", "--plugin-dir=" + filepath.Join(t.TempDir(), "private-plugin"), "marker"}, "--plugin-dir", "private-plugin"},
		{[]string{"--tools=", "--config=model.api_key='credential-sentinel'", "marker"}, "--config", "credential-sentinel"},
		{[]string{"--tools", "", "--agent=private-agent-path", "marker"}, "--agent", "private-agent-path"},
		{[]string{"--tools", "", `--agents={"worker":{"token":"credential-sentinel"}}`, "marker"}, "--agents", "credential-sentinel"},
		{[]string{"--tools", "", "--cwd=" + filepath.Join(t.TempDir(), "project-path-sentinel"), "marker"}, "--cwd", "project-path-sentinel"},
		{[]string{"--tools", "", "--resume=session-path-sentinel", "marker"}, "--resume", "session-path-sentinel"},
		{[]string{"--tools", "", "--worktree=project-path-sentinel", "marker"}, "--worktree", "project-path-sentinel"},
	}
	for i, tc := range tests {
		sm := NewSessionManager(nil)
		args := append([]string{grokMaintenanceSmokeControlArg}, tc.args...)
		err := sm.StartSession("grok-no-tools-loader", "grok", args, t.TempDir(), "ws", "uid", 1000, false, func(resultMsg) {})
		if err == nil || !strings.Contains(err.Error(), "cannot use external-loader or workspace/session") {
			t.Errorf("case %d StartSession(%#v) error = %v, want external-loader rejection", i, args, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantFlag) {
			t.Errorf("case %d error %q does not identify canonical flag %q", i, err, tc.wantFlag)
		}
		if strings.Contains(err.Error(), tc.secretText) || strings.Contains(err.Error(), "model.api_key") || strings.Contains(err.Error(), `"worker"`) {
			t.Errorf("case %d rejection leaked loader value: %q", i, err)
		}
	}
}

func TestSanitizeGrokMaintenanceSmokeEnv_StripsExtensionOverrides(t *testing.T) {
	got := sanitizeGrokMaintenanceSmokeEnv([]string{
		"PATH=keep-me",
		"XAI_API_KEY=credential-sentinel",
		"GROK_CODE_XAI_API_KEY=alternate-credential-sentinel",
		"GROK_AUTH_PROVIDER_ACCESS_TOKEN=provider-credential-sentinel",
		"GROK_OIDC_ISSUER=https://oidc-issuer-sentinel.invalid",
		"GROK_OIDC_CLIENT_ID=oidc-client-sentinel",
		"GROK_OIDC_AUDIENCE=oidc-audience-sentinel",
		"GROK_AGENT=agent-path-sentinel",
		"GROK_DEFAULT_MODEL=model-sentinel",
		"GROK_MODEL=alternate-model-sentinel",
		"GROK_MODELS_BASE_URL=https://models-base-sentinel.invalid",
		"GROK_MODELS_LIST_URL=https://models-list-sentinel.invalid",
		"GROK_XAI_API_BASE_URL=https://xai-base-sentinel.invalid",
		"GROK_API_BASE_URL=https://api-base-sentinel.invalid",
		"XAI_API_BASE_URL=https://alternate-xai-base-sentinel.invalid",
		"GROK_CURSOR_MCPS_ENABLED=1",
		"GROK_CLAUDE_MCPS_ENABLED=1",
		"GROK_CURSOR_AGENTS_ENABLED=1",
		"GROK_CLAUDE_HOOKS_ENABLED=1",
		"GROK_CODEX_MCPS_ENABLED=1",
		"GROK_MANAGED_MCPS_ENABLED=1",
		"GROK_WORKSPACE_TOOL_DEFS_ENABLED=1",
		"GROK_CONFIG_PATH=config-path-sentinel",
		"GROK_MANAGED_CONFIG_URL=managed-config-sentinel",
		"GROK_PLUGIN_ROOT=plugin-path-sentinel",
		"GROK_WORKSPACE_ROOT=workspace-path-sentinel",
		"GROK_WORKSPACE_SERVER_SKILLS_DIR=skills-path-sentinel",
		"GROK_SUBAGENTS=1",
		"GROK_WEB_FETCH=1",
		"GROK_MEMORY=1",
		"GROK_LSP_TOOLS=1",
		"GROK_SANDBOX=host-sandbox-sentinel",
		"GROK_SANDBOX_AUTO_ALLOW_BASH=1",
		"GROK_LOG_FILE=external-log-path-sentinel",
		"GROK_FUTURE_EXECUTION_OVERRIDE=future-override-sentinel",
		"RUST_LOG=xai_grok=debug-stderr-sentinel",
		"RUST_BACKTRACE=1",
		"RUST_LIB_BACKTRACE=1",
		"OTEL_LOGS_EXPORTER=console-output-sentinel",
		"OTEL_METRICS_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-endpoint-sentinel.invalid",
		"OTEL_EXPORTER_OTLP_HEADERS=Authorization=otel-credential-sentinel",
		"OTEL_EXPORTER_OTLP_CERTIFICATE=otel-ca-path-sentinel",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=otel-client-cert-path-sentinel",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY=otel-client-key-path-sentinel",
		"OTEL_LOG_USER_PROMPTS=1",
		"OTEL_LOG_TOOL_DETAILS=1",
		"OTEL_FUTURE_EXPORT_OVERRIDE=future-otel-sentinel",
	})
	values := make(map[string]string, len(got))
	for _, entry := range got {
		name, value, _ := strings.Cut(entry, "=")
		values[strings.ToUpper(name)] = value
	}
	if values["PATH"] != "keep-me" {
		t.Fatalf("safe environment was not preserved: %#v", got)
	}
	for _, stripped := range []string{
		"XAI_API_KEY", "GROK_CODE_XAI_API_KEY", "GROK_AUTH_PROVIDER_ACCESS_TOKEN",
		"GROK_OIDC_ISSUER", "GROK_OIDC_CLIENT_ID", "GROK_OIDC_AUDIENCE",
		"GROK_AGENT", "GROK_SUBAGENTS", "GROK_WEB_FETCH",
		"GROK_MEMORY", "GROK_LSP_TOOLS", "GROK_CONFIG_PATH", "GROK_PLUGIN_ROOT",
		"GROK_MANAGED_CONFIG_URL", "GROK_WORKSPACE_ROOT", "GROK_WORKSPACE_SERVER_SKILLS_DIR",
		"GROK_DEFAULT_MODEL", "GROK_MODELS_BASE_URL", "GROK_MODELS_LIST_URL",
		"GROK_MODEL", "GROK_XAI_API_BASE_URL", "GROK_API_BASE_URL", "XAI_API_BASE_URL",
		"GROK_SANDBOX", "GROK_SANDBOX_AUTO_ALLOW_BASH", "GROK_LOG_FILE",
		"GROK_FUTURE_EXECUTION_OVERRIDE", "RUST_LOG", "RUST_BACKTRACE", "RUST_LIB_BACKTRACE",
		"OTEL_LOGS_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS", "OTEL_FUTURE_EXPORT_OVERRIDE",
	} {
		if _, ok := values[stripped]; ok {
			t.Errorf("maintenance environment retained %s: %#v", stripped, got)
		}
	}
	for _, pinned := range []string{
		"GROK_CURSOR_SKILLS_ENABLED", "GROK_CURSOR_RULES_ENABLED", "GROK_CURSOR_AGENTS_ENABLED",
		"GROK_CURSOR_MCPS_ENABLED", "GROK_CURSOR_HOOKS_ENABLED", "GROK_CURSOR_SESSIONS_ENABLED",
		"GROK_CLAUDE_SKILLS_ENABLED", "GROK_CLAUDE_RULES_ENABLED", "GROK_CLAUDE_AGENTS_ENABLED",
		"GROK_CLAUDE_MCPS_ENABLED", "GROK_CLAUDE_HOOKS_ENABLED", "GROK_CLAUDE_SESSIONS_ENABLED",
		"GROK_CODEX_SKILLS_ENABLED", "GROK_CODEX_RULES_ENABLED", "GROK_CODEX_AGENTS_ENABLED",
		"GROK_CODEX_MCPS_ENABLED", "GROK_CODEX_HOOKS_ENABLED", "GROK_CODEX_SESSIONS_ENABLED",
		"GROK_MANAGED_MCPS_ENABLED", "GROK_MANAGED_MCP_GATEWAY_TOOLS_ENABLED",
		"GROK_WORKSPACE_TOOL_DEFS_ENABLED", "GROK_WORKSPACE_TOOL_STATE_ENABLED",
	} {
		if values[pinned] != "0" {
			t.Errorf("maintenance environment %s = %q, want 0", pinned, values[pinned])
		}
	}
	encoded := strings.Join(got, "\n")
	for _, secret := range []string{
		"credential-sentinel", "alternate-credential-sentinel", "provider-credential-sentinel",
		"oidc-issuer-sentinel", "oidc-client-sentinel", "oidc-audience-sentinel",
		"agent-path-sentinel", "config-path-sentinel", "managed-config-sentinel", "plugin-path-sentinel",
		"workspace-path-sentinel", "skills-path-sentinel", "model-sentinel", "alternate-model-sentinel", "models-base-sentinel",
		"models-list-sentinel", "xai-base-sentinel", "api-base-sentinel", "alternate-xai-base-sentinel",
		"host-sandbox-sentinel", "external-log-path-sentinel", "future-override-sentinel", "debug-stderr-sentinel",
		"console-output-sentinel", "otel-endpoint-sentinel", "otel-credential-sentinel", "otel-ca-path-sentinel",
		"otel-client-cert-path-sentinel", "otel-client-key-path-sentinel", "future-otel-sentinel",
	} {
		if strings.Contains(encoded, secret) {
			t.Errorf("maintenance environment retained %q: %s", secret, encoded)
		}
	}
}

func TestDetectGrokMaintenanceSmokeSystemConfig_FailsClosedOnCredentialsAndTools(t *testing.T) {
	requirementsPath := filepath.Join(t.TempDir(), "requirements.toml")
	managedPath := filepath.Join(t.TempDir(), "managed_config.toml")
	origRequirementsPath := grokSystemRequirementsPath
	origManagedPath := grokSystemManagedConfigPath
	origClaudePaths := claudeManagedSettingsPathsFn
	grokSystemRequirementsPath = requirementsPath
	grokSystemManagedConfigPath = managedPath
	claudeManagedSettingsPathsFn = func() []string { return nil }
	t.Cleanup(func() {
		grokSystemRequirementsPath = origRequirementsPath
		grokSystemManagedConfigPath = origManagedPath
		claudeManagedSettingsPathsFn = origClaudePaths
	})
	if err := os.WriteFile(requirementsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		body         string
		wantCategory string
	}{
		{"system API key", "[model]\napi_key = 'credential-sentinel'\n", ""},
		{"table auth provider command", "[auth]\nauth_provider_command = 'credential-sentinel'\n", ""},
		{"table OIDC issuer", "[auth.oidc]\nissuer = 'https://oidc-issuer-sentinel.invalid'\n", ""},
		{"dotted OIDC client", `auth.oidc.client_id = "oidc-client-sentinel"` + "\n", ""},
		{"inline OIDC issuer", `auth = { oidc = { issuer = "https://oidc-inline-sentinel.invalid" } }` + "\n", ""},
		{"quoted OIDC client", `"auth"."oidc"."client_id" = "oidc-quoted-sentinel"` + "\n", ""},
		{"dotted custom model endpoint", `models.custom.base_url = "https://raw-config-sentinel.invalid"` + "\n", ""},
		{"inline provider headers", `models = { custom = { extra_headers = { Authorization = "credential-sentinel" } } }` + "\n", ""},
		{"quoted default model", `"models" = { "default" = "customer-secret-name" }` + "\n", ""},
		{"system plugin", "[plugins]\nenabled = ['private-plugin']\n", "plugin"},
		{"system MCP", "[mcp_servers.customer-secret-name]\ncommand = 'raw-config-sentinel'\n", "mcp"},
		{"vendor MCP override", "[compat.cursor]\nmcps = true\n", "vendor-mcp"},
		{"inline vendor MCP", "compat = { cursor = { mcps = true } }\n", "vendor-mcp"},
		{"inline approval", `approval = { mode = "always" }` + "\n", ""},
		{"inline documented UI approval", `ui = { permission_mode = "always-approve" }` + "\n", ""},
		{"inline documented permission allow", `permission = { rules = [{ action = "allow", tool = "MCPTool" }] }` + "\n", ""},
		{"top-level allow list", `allow = ["MCPTool(*)"]` + "\n", ""},
		{"quoted inline UI approval", `"ui" = { "permission_mode" = "acceptEdits" }` + "\n", ""},
		{"quoted inline permission allow", `"permission" = { "rules" = [{ "action" = "allow", "tool" = "MCPTool" }] }` + "\n", ""},
		{"quoted inline vendor MCP", `"compat" = { "cursor" = { "mcps" = true } }` + "\n", "vendor-mcp"},
		{"inline plugin", `plugins = { enabled = ["customer-secret-name"] }` + "\n", "plugin"},
		{"inline MCP", `mcp_servers = { "customer-secret-name" = { command = "raw-config-sentinel" } }` + "\n", "mcp"},
		{"table external telemetry", "[telemetry]\notel_enabled = true\n", "telemetry"},
		{"dotted console telemetry", `telemetry.otel_logs_exporter = "console-output-sentinel"` + "\n", "telemetry"},
		{"inline OTLP telemetry", `telemetry = { otel_metrics_exporter = "otlp", otel_endpoint = "https://otel-endpoint-sentinel.invalid" }` + "\n", "telemetry"},
		{"inline prompt telemetry", `telemetry = { otel_log_user_prompts = true }` + "\n", "telemetry"},
		{"inline tool-detail telemetry", `telemetry = { otel_log_tool_details = true }` + "\n", "telemetry"},
		{"expanded telemetry enable", "[telemetry]\notel_enabled = '$OTEL_ENABLE_SENTINEL'\n", "telemetry"},
		{"quoted telemetry certificate", `"telemetry"."otel_certificate" = "otel-ca-path-sentinel"` + "\n", "telemetry"},
		{"quoted telemetry client certificate", `"telemetry"."otel_client_certificate" = "otel-client-cert-path-sentinel"` + "\n", "telemetry"},
		{"quoted telemetry client key", `"telemetry"."otel_client_key" = "otel-client-key-path-sentinel"` + "\n", "telemetry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(managedPath, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13")
			if err == nil {
				t.Fatalf("system config was accepted: %s", tc.body)
			}
			if tc.wantCategory != "" && !strings.Contains(err.Error(), `"`+tc.wantCategory+`"`) {
				t.Fatalf("system-config refusal %q omitted fixed category %q", err, tc.wantCategory)
			}
			for _, secret := range []string{"credential-sentinel", "private-plugin", "raw-config-sentinel", "customer-secret-name", "customer_secret_name", "oidc-issuer-sentinel", "oidc-client-sentinel", "oidc-inline-sentinel", "oidc-quoted-sentinel", "console-output-sentinel", "otel-endpoint-sentinel", "otel-ca-path-sentinel", "otel-client-cert-path-sentinel", "otel-client-key-path-sentinel", "OTEL_ENABLE_SENTINEL"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("system-config refusal leaked %q: %v", secret, err)
				}
			}
			if strings.Contains(err.Error(), requirementsPath) || strings.Contains(err.Error(), managedPath) {
				t.Fatalf("system-config refusal leaked a source path: %v", err)
			}
		})
	}

	if err := os.WriteFile(managedPath, []byte("[compat.cursor]\nmcps = false\n[compat.claude]\nmcps = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err != nil {
		t.Fatalf("explicit system vendor-MCP disables must remain valid: %v", err)
	}
	if err := os.WriteFile(managedPath, []byte("compat = { cursor = { mcps = false }, claude = { mcps = false } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err != nil {
		t.Fatalf("inline system vendor-MCP disables must remain valid: %v", err)
	}
	if err := os.WriteFile(managedPath, []byte(`permission = { rules = [{ action = "deny", tool = "MCPTool", allow = "matcher-only" }] }`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err != nil {
		t.Fatalf("inline deny-only permission rules must remain valid: %v", err)
	}
	if err := os.WriteFile(managedPath, []byte("[telemetry]\notel_enabled = false\notel_metrics_exporter = 'none'\notel_logs_exporter = 'none'\notel_log_user_prompts = false\notel_log_tool_details = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err != nil {
		t.Fatalf("explicitly disabled external telemetry must remain valid: %v", err)
	}

	for _, emptyConfig := range []struct {
		name string
		body string
	}{
		{"empty mcp_servers table", "mcp_servers = {}\n"},
		{"empty mcp_servers section", "[mcp_servers]\n"},
		{"empty mcps array", "mcps = []\n"},
		{"empty mcpservers section", "[mcpservers]\n"},
		{"disabled mcp_servers section", "[mcp_servers]\nenabled = false\n"},
		{"empty plugins table", "plugins = {}\n"},
		{"empty plugins section", "[plugins]\n"},
		{"empty plugins enabled array inline", "plugins = { enabled = [] }\n"},
		{"empty plugins enabled array section", "[plugins]\nenabled = []\n"},
		{"disabled plugins section", "[plugins]\nenabled = false\n"},
		// A disabled entry keeps its full body in the managed layer; the flag,
		// not the retained command/args/env/path, decides whether it can load.
		{"disabled mcp server with retained command", "[mcp_servers.customer-secret-name]\nenabled = false\ncommand = 'raw-config-sentinel'\n"},
		{"disabled mcp server with retained args", "[mcp_servers.customer-secret-name]\ndisabled = true\ncommand = 'raw-config-sentinel'\nargs = ['raw-config-sentinel']\n"},
		{"inline disabled mcp server with retained command", `mcp_servers = { "customer-secret-name" = { enabled = false, command = "raw-config-sentinel" } }` + "\n"},
		{"disabled mcp array entry with retained command", `mcps = [{ enabled = false, command = "raw-config-sentinel" }]` + "\n"},
		{"disabled plugin with retained path", "[plugins.customer-secret-name]\nenabled = false\npath = 'raw-config-sentinel'\n"},
		{"disabled vendor mcp parent", "[compat.cursor]\nenabled = false\nmcps = true\n"},
	} {
		t.Run(emptyConfig.name, func(t *testing.T) {
			if err := os.WriteFile(managedPath, []byte(emptyConfig.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err != nil {
				t.Fatalf("empty/disabled tool configuration %q was unexpectedly rejected: %v", emptyConfig.body, err)
			}
		})
	}

	// Suppressing a disabled definition must not suppress its siblings, and an
	// enablement that cannot be proven false lexically (environment expansion)
	// must keep failing closed.
	for _, live := range []struct {
		name         string
		body         string
		wantCategory string
	}{
		{"enabled sibling of disabled mcp server", "[mcp_servers.disabled-one]\nenabled = false\ncommand = 'raw-config-sentinel'\n\n[mcp_servers.customer-secret-name]\ncommand = 'raw-config-sentinel'\n", "mcp"},
		{"expanded mcp enablement", "[mcp_servers.customer-secret-name]\nenabled = '$MCP_ENABLE_SENTINEL'\ncommand = 'raw-config-sentinel'\n", "mcp"},
		{"enabled plugin sibling of disabled plugin", "[plugins.disabled-one]\nenabled = false\npath = 'raw-config-sentinel'\n\n[plugins.customer-secret-name]\nenabled = true\npath = 'raw-config-sentinel'\n", "plugin"},
		{"contradictory mcp enablement pair", "[mcp_servers.customer-secret-name]\nenabled = false\ndisabled = false\ncommand = 'raw-config-sentinel'\n", "mcp"},
		{"contradictory mcp disablement pair", "[mcp_servers.customer-secret-name]\nenabled = true\ndisabled = true\ncommand = 'raw-config-sentinel'\n", "mcp"},
		{"expanded enablement beside explicit disable", "[mcp_servers.customer-secret-name]\nenabled = '$MCP_ENABLE_SENTINEL'\ndisabled = true\ncommand = 'raw-config-sentinel'\n", "mcp"},
	} {
		t.Run(live.name, func(t *testing.T) {
			if err := os.WriteFile(managedPath, []byte(live.body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13")
			if err == nil {
				t.Fatalf("live tool configuration was accepted: %s", live.body)
			}
			if !strings.Contains(err.Error(), `"`+live.wantCategory+`"`) {
				t.Fatalf("refusal %q omitted fixed category %q", err, live.wantCategory)
			}
			for _, secret := range []string{"raw-config-sentinel", "customer-secret-name", "customer_secret_name", "MCP_ENABLE_SENTINEL"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("refusal leaked %q: %v", secret, err)
				}
			}
		})
	}

	// A disabled block still exposes readable credentials and provider
	// overrides, so those findings keep failing closed independently of whether
	// the definition can load a tool.
	for _, sensitive := range []struct {
		name string
		body string
	}{
		{"credential inside disabled mcp server", "[mcp_servers.customer-secret-name]\nenabled = false\napi_key = 'credential-sentinel'\n"},
		{"provider override inside disabled mcp server", "[mcp_servers.customer-secret-name]\nenabled = false\nbase_url = 'https://provider-sentinel.invalid'\n"},
	} {
		t.Run(sensitive.name, func(t *testing.T) {
			if err := os.WriteFile(managedPath, []byte(sensitive.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err == nil {
				t.Fatalf("sensitive value inside a disabled definition was accepted: %s", sensitive.body)
			}
		})
	}

	if err := os.WriteFile(managedPath, []byte(strings.Repeat("#", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); err == nil || !strings.Contains(err.Error(), "inspection limit") {
		t.Fatalf("oversized system config must fail closed, got %v", err)
	}
}

func TestInspectGrokSystemConfigSemantic_AppliesOnlyMatchingVersionOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed_config.toml")
	tests := []struct {
		name           string
		runtimeVersion string
		minimum        string
		maximum        string
		wantTool       bool
	}{
		{"1.0.5 active", "grok 1.0.5", "1.0.0", "1.0.9", true},
		{"1.0.5 historical inactive", "grok 1.0.5", "0.9.0", "1.0.4", false},
		{"1.0.5 future inactive", "grok 1.0.5", "1.0.6", "1.0.12", false},
		{"1.0.13 active", "grok 1.0.13", "1.0.10", "1.0.20", true},
		{"1.0.13 historical inactive", "grok 1.0.13", "1.0.0", "1.0.12", false},
		{"1.0.13 future inactive", "grok 1.0.13", "1.0.14", "2.0.0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `version_overrides = [{ minimum_version = "` + tc.minimum +
				`", maximum_version = "` + tc.maximum +
				`", mcp_servers = { "sentinel-server" = { command = "raw-config-sentinel" } } }]` + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			finding, ok, err := inspectGrokSystemConfigSemantic(path, tc.runtimeVersion)
			if err != nil || !ok {
				t.Fatalf("inspect override: ok=%v finding=%+v err=%v", ok, finding, err)
			}
			if got := finding.toolCategory != ""; got != tc.wantTool {
				t.Fatalf("tool finding = %+v, want tool=%v", finding, tc.wantTool)
			}
		})
	}

	// Matching patches replace their base-layer value before classification;
	// walking both the base and patch independently would reject this safe pin.
	if err := os.WriteFile(path, []byte(
		`compat = { cursor = { mcps = true } }`+"\n"+
			`version_overrides = [{ minimum_version = "1.0.10", maximum_version = "1.0.20", compat = { cursor = { mcps = false } } }]`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	finding, ok, err := inspectGrokSystemConfigSemantic(path, "grok 1.0.13")
	if err != nil || !ok || finding.toolCategory != "" {
		t.Fatalf("effective override did not replace base value: ok=%v finding=%+v err=%v", ok, finding, err)
	}
	if err := os.WriteFile(path, []byte(
		`compat = { cursor = { mcps = true } }`+"\n"+
			`version_overrides = [{ minimum_version = "1.0.10", maximum_version = "1.0.20", Compat = { cursor = { mcps = false } } }]`+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	finding, ok, err = inspectGrokSystemConfigSemantic(path, "grok 1.0.13")
	if err != nil || !ok || finding.toolCategory != "vendor-mcp" {
		t.Fatalf("lookalike override hid the effective base setting: ok=%v finding=%+v err=%v", ok, finding, err)
	}

	for name, body := range map[string]string{
		"missing maximum": `version_overrides = [{ minimum_version = "1.0.0", mcp_servers = {} }]`,
		"invalid minimum": `version_overrides = [{ minimum_version = "not-semver", maximum_version = "2.0.0", mcp_servers = {} }]`,
		"reversed range":  `version_overrides = [{ minimum_version = "2.0.0", maximum_version = "1.0.0", mcp_servers = {} }]`,
		"ambiguous minimum": `version_overrides = [{ minimum_version = "1.0.0", minimum-version = "1.0.1", ` +
			`maximum_version = "2.0.0", mcp_servers = {} }]`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := inspectGrokSystemConfigSemantic(path, "grok 1.0.13"); err == nil || !strings.Contains(err.Error(), "cannot be evaluated safely") {
				t.Fatalf("unsafe selector was not rejected generically: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name           string
		runtimeVersion string
		minimum        string
		maximum        string
		wantTelemetry  bool
	}{
		{"1.0.5 active telemetry", "grok 1.0.5", "1.0.0", "1.0.9", true},
		{"1.0.5 future inactive telemetry", "grok 1.0.5", "1.0.6", "1.0.20", false},
		{"1.0.13 historical inactive telemetry", "grok 1.0.13", "1.0.0", "1.0.12", false},
		{"1.0.13 active telemetry", "grok 1.0.13", "1.0.10", "1.0.20", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `version_overrides = [{ minimum_version = "` + tc.minimum +
				`", maximum_version = "` + tc.maximum +
				`", telemetry = { otel_enabled = true, otel_logs_exporter = "console" } }]` + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			finding, ok, err := inspectGrokSystemConfigSemantic(path, tc.runtimeVersion)
			if err != nil || !ok {
				t.Fatalf("inspect telemetry override: ok=%v finding=%+v err=%v", ok, finding, err)
			}
			if got := finding.toolCategory == "telemetry"; got != tc.wantTelemetry {
				t.Fatalf("telemetry finding = %+v, want telemetry=%v", finding, tc.wantTelemetry)
			}
		})
	}
}

func TestStartSession_GrokMaintenanceSmokeRunsSystemPreflight(t *testing.T) {
	requirementsPath := filepath.Join(t.TempDir(), "requirements.toml")
	managedPath := filepath.Join(t.TempDir(), "managed_config.toml")
	if err := os.WriteFile(requirementsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, []byte("[mcp_servers.private]\ncommand = 'raw-config-sentinel'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	origRequirementsPath := grokSystemRequirementsPath
	origManagedPath := grokSystemManagedConfigPath
	origClaudePaths := claudeManagedSettingsPathsFn
	origVersionProbe := grokMaintenanceSmokeVersionProbeFn
	grokSystemRequirementsPath = requirementsPath
	grokSystemManagedConfigPath = managedPath
	claudeManagedSettingsPathsFn = func() []string { return nil }
	grokMaintenanceSmokeVersionProbeFn = func(string) string { return "grok 1.0.13" }
	t.Cleanup(func() {
		grokSystemRequirementsPath = origRequirementsPath
		grokSystemManagedConfigPath = origManagedPath
		claudeManagedSettingsPathsFn = origClaudePaths
		grokMaintenanceSmokeVersionProbeFn = origVersionProbe
	})

	sm := NewSessionManager(nil)
	err := sm.StartSession("grok-system-tools", "grok", []string{
		grokMaintenanceSmokeControlArg, "--tools", "", "--disable-web-search",
		"--no-subagents", "--max-turns", "1", "--verbatim", grokMaintenanceSmokePromptPrefix + "TEST_MARKER",
	}, t.TempDir(), "ws", "uid", 1000, false, func(resultMsg) {})
	if err == nil || !strings.Contains(err.Error(), "system-config preflight") {
		t.Fatalf("StartSession system-tool config error = %v, want preflight refusal", err)
	}
	if strings.Contains(err.Error(), "raw-config-sentinel") {
		t.Fatalf("StartSession system-tool refusal leaked raw config: %v", err)
	}
}

func stubGrokMaintenanceSmokePreflight(t *testing.T) {
	t.Helper()
	origRequirementsPath := grokSystemRequirementsPath
	origManagedPath := grokSystemManagedConfigPath
	origClaudePaths := claudeManagedSettingsPathsFn
	origVersionProbe := grokMaintenanceSmokeVersionProbeFn
	grokSystemRequirementsPath = filepath.Join(t.TempDir(), "requirements.toml")
	grokSystemManagedConfigPath = filepath.Join(t.TempDir(), "managed_config.toml")
	claudeManagedSettingsPathsFn = func() []string { return nil }
	grokMaintenanceSmokeVersionProbeFn = func(string) string { return "grok 1.0.13" }
	t.Cleanup(func() {
		grokSystemRequirementsPath = origRequirementsPath
		grokSystemManagedConfigPath = origManagedPath
		claudeManagedSettingsPathsFn = origClaudePaths
		grokMaintenanceSmokeVersionProbeFn = origVersionProbe
	})
}

func grokMaintenanceSmokeStartArgs() []string {
	return []string{
		grokMaintenanceSmokeControlArg, "--tools", "", "--disable-web-search",
		"--no-subagents", "--max-turns", "1", "--verbatim", grokMaintenanceSmokePromptPrefix + "TEST_MARKER",
	}
}

func TestStartSession_GrokMaintenanceSmokeRefusesUnauthenticatedHome(t *testing.T) {
	stubGrokMaintenanceSmokePreflight(t)
	emptyHome := t.TempDir()
	t.Setenv("GROK_HOME", emptyHome)
	t.Setenv("XAI_API_KEY", "credential-sentinel-api-key")

	sm := NewSessionManager(nil)
	err := sm.StartSession("grok-smoke-no-auth", "grok", grokMaintenanceSmokeStartArgs(), t.TempDir(), "ws", "uid", 1000, false, func(resultMsg) {})
	typed := grokAuthErrorFrom(err)
	if typed == nil || typed.Code != grokNotAuthenticatedCode {
		t.Fatalf("StartSession unauthenticated smoke error = %v, want GROK_NOT_AUTHENTICATED", err)
	}
	if strings.Contains(err.Error(), "credential-sentinel") || strings.Contains(err.Error(), emptyHome) {
		t.Fatalf("unauthenticated smoke refusal leaked secret or path: %v", err)
	}
	if _, exists := sm.sessions["grok-smoke-no-auth"]; exists {
		t.Fatal("unauthenticated smoke must not register a session")
	}
}

func TestResolveGrokMaintenanceSmokeExecutable_AbsolutizesRelativePath(t *testing.T) {
	cwd := t.TempDir()
	for _, rel := range []string{"./grok", filepath.Join("bin", "grok")} {
		got := resolveGrokMaintenanceSmokeExecutable(rel, cwd)
		want, err := filepath.Abs(filepath.Join(cwd, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("relative smoke executable %q = %q, want %q", rel, got, want)
		}
	}

	abs := filepath.Join(cwd, "installed", "grok")
	if got := resolveGrokMaintenanceSmokeExecutable(abs, t.TempDir()); got != abs {
		t.Fatalf("absolute smoke executable = %q, want unchanged %q", got, abs)
	}
}

func TestStartSession_GrokMaintenanceSmokeProbesRelativeBinaryAgainstCallerCwd(t *testing.T) {
	stubGrokMaintenanceSmokePreflight(t)
	callerCwd := t.TempDir()
	var probed string
	grokMaintenanceSmokeVersionProbeFn = func(executable string) string {
		probed = executable
		return "grok 1.0.13"
	}
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("XAI_API_KEY", "")

	sm := NewSessionManager(nil)
	_ = sm.StartSession("grok-smoke-relpath", "./grok", grokMaintenanceSmokeStartArgs(), callerCwd, "ws", "uid", 1000, false, func(resultMsg) {})
	want, err := filepath.Abs(filepath.Join(callerCwd, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if probed != want {
		t.Fatalf("version probe executable = %q, want caller-cwd absolute %q", probed, want)
	}
}

func TestStartSession_GrokMaintenanceSmokeAuthenticatedHomeDoesNotReturnAuthError(t *testing.T) {
	stubGrokMaintenanceSmokePreflight(t)
	enableTestGrokLogin(t)

	sm := NewSessionManager(nil)
	err := sm.StartSession("grok-smoke-auth", "./grok", grokMaintenanceSmokeStartArgs(), t.TempDir(), "ws", "uid", 1000, false, func(resultMsg) {})
	if grokAuthErrorFrom(err) != nil {
		t.Fatalf("authenticated smoke returned GROK_NOT_AUTHENTICATED: %v", err)
	}
	if err == nil {
		t.Fatal("expected StartSession to fail when the relative grok binary is missing")
	}
	if _, exists := sm.sessions["grok-smoke-auth"]; exists {
		t.Fatal("missing executable must not register a session")
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

func TestCaptureGrokUsageLimitLine_RemainsNoticeOnly(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	t.Setenv("AIEXPEDITE_GROK_LIMIT_CACHE", cache)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	for _, line := range []string{
		`{"ts":"2026-08-17T23:02:12Z","msg":"billing: fetched credits config","ctx":{"config":{"creditUsagePercent":33}}}`,
		`{"type":"tool_result","credit_limit_percent":42,"used":12,"credits":50,"credential":"secret"}`,
	} {
		captureGrokUsageLimitLine(line, now)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("billing-shaped and generic numeric ACP frames must not write the notice cache: %v", err)
	}
}

func TestWriteGrokUsageLimitState_ReachedWinsUntilTTL(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "grok_usage_limit.json")
	fingerprint := "account-fingerprint"
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	writeGrokUsageLimitState(cache, grokUsageLimitState{
		Severity:     grokLimitReached,
		ObservedAt:   now.Format(time.RFC3339),
		ObservedAtMs: now.UnixMilli(),
	}, fingerprint)
	later := now.Add(time.Minute)
	writeGrokUsageLimitState(cache, grokUsageLimitState{
		Severity:     grokLimitApproaching,
		ObservedAt:   later.Format(time.RFC3339),
		ObservedAtMs: later.UnixMilli(),
	}, fingerprint)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var got grokUsageLimitState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Severity != grokLimitReached || got.ObservedAtMs != later.UnixMilli() {
		t.Fatalf("fresh approaching notice downgraded reached state: %+v", got)
	}

	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	writeGrokUsageLimitState(cache, grokUsageLimitState{ObservedAtMs: later.Add(time.Minute).UnixMilli()}, fingerprint)
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("severity-empty notice write must be rejected")
	}
}
