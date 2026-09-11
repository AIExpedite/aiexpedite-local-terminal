package main

import (
	"strings"
	"testing"
)

// Codex round 7 on #147: `grok models` runs under the same configuration
// surface as an ACP session — endpoint overrides and config path included —
// not under the maintenance smoke's "strip every GROK_*" sanitizer.

func TestGrokModelListEnvKeepsTheSessionsRoutingConfiguration(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"GROK_HOME=/custom/grok",
		"GROK_MODELS_LIST_URL=https://enterprise.example/v1/models",
		"GROK_MODELS_BASE_URL=https://enterprise.example/v1",
		"GROK_API_BASE_URL=https://enterprise.example",
		"XAI_API_BASE_URL=https://enterprise.example/xai",
		"GROK_CONFIG_PATH=/etc/grok/config.toml",
		"GROK_CURSOR_SKILLS_ENABLED=1",
		// What the maintenance smoke exists to keep away from a headless
		// child: a raw-diagnostics sink outside the isolated home and an
		// execution-path override. Routing config is restored; these are not.
		"GROK_LOG_FILE=/tmp/raw-grok-diagnostics.log",
		"GROK_FUTURE_EXECUTION_OVERRIDE=future-execution-sentinel",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector",
		"RUST_LOG=debug",
		"CLAUDE_CODE_OAUTH_TOKEN=leak",
		"XAI_API_KEY=xai-secret",
	}
	prev := shutdownConfig
	t.Cleanup(func() { shutdownConfig = prev })
	shutdownConfig = nil
	got := sanitizeGrokModelListEnv(env)
	kept := map[string]string{}
	for _, entry := range got {
		if name, value, found := strings.Cut(entry, "="); found {
			kept[name] = value
		}
	}
	// Everything an ACP session would see, the list sees too.
	for _, name := range []string{"GROK_HOME", "GROK_MODELS_LIST_URL", "GROK_MODELS_BASE_URL", "GROK_API_BASE_URL", "XAI_API_BASE_URL", "GROK_CONFIG_PATH", "PATH"} {
		if kept[name] == "" {
			t.Fatalf("%s must reach the list probe exactly as it reaches a session: %#v", name, got)
		}
	}
	// What a session strips, the list strips: cross-agent identity, the
	// non-opted-in key, and the telemetry / log noise the smoke drops too.
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "XAI_API_KEY", "OTEL_EXPORTER_OTLP_ENDPOINT", "RUST_LOG", "GROK_LOG_FILE", "GROK_FUTURE_EXECUTION_OVERRIDE"} {
		if _, present := kept[name]; present {
			t.Fatalf("%s must not reach the list probe: %#v", name, got)
		}
	}
	// The integration switches are forced off, never merely inherited.
	if kept["GROK_CURSOR_SKILLS_ENABLED"] != "0" {
		t.Fatalf("integration switch not neutralised: %q", kept["GROK_CURSOR_SKILLS_ENABLED"])
	}
	for _, name := range grokNeutralisedIntegrationSwitches {
		if kept[name] != "0" {
			t.Fatalf("%s must be pinned to 0: %q", name, kept[name])
		}
	}
	// Opted in: the key rides along, as sanitizeGrokACPEnv would forward it.
	shutdownConfig = &Config{EnableGrokAPIKeyFallback: true}
	if !containsString(sanitizeGrokModelListEnv(env), "XAI_API_KEY=xai-secret") {
		t.Fatal("the opted-in key must reach the list probe")
	}
}
