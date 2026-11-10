package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_config_model_test.go — the device-level default
   model OpenCode is configured with, read from opencode.jsonc as well as
   opencode.json, both as JSONC. A computer configured only through
   opencode.jsonc used to report no default at all.
   ------------------------------------------------------------------------ */

// openCodeConfigHome is a temporary home with XDG_CONFIG_HOME unset, so only
// the files a case writes are read.
func openCodeConfigHome(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	return t.TempDir()
}

func writeOpenCodeConfig(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A real-shaped opencode.jsonc: a schema URL, comments of both kinds, trailing
// commas, and a provider baseURL whose `//` must survive the comment strip.
const openCodeJSONCWithComments = `{
  "$schema": "https://opencode.ai/config.json",
  // OpenCode's own default for every run without --model.
  "model": "google/gemini-2.5-pro", /* the one without a key */
  "provider": {
    "ollama": {
      "options": { "baseURL": "http://localhost:11434/v1", }, // trailing commas
    },
  },
}
`

func TestReadOpenCodeConfiguredModel_ReadsOpenCodeJSONC(t *testing.T) {
	home := openCodeConfigHome(t)
	writeOpenCodeConfig(t, filepath.Join(home, ".config", "opencode"), "opencode.jsonc", openCodeJSONCWithComments)
	if got := readOpenCodeConfiguredModel(home); got != "google/gemini-2.5-pro" {
		t.Fatalf("model = %q, want the opencode.jsonc default", got)
	}
}

// OpenCode reads opencode.json and then opencode.jsonc over it in one
// directory, so the .jsonc file's model wins; a .jsonc without one leaves the
// .json file's.
func TestReadOpenCodeConfiguredModel_JSONCOverridesJSONInOneDirectory(t *testing.T) {
	home := openCodeConfigHome(t)
	dir := filepath.Join(home, ".config", "opencode")
	writeOpenCodeConfig(t, dir, "opencode.json", `{"model": "anthropic/claude-sonnet-4-5"}`)
	writeOpenCodeConfig(t, dir, "opencode.jsonc", `{"model": "opencode/big-pickle", // chosen
}`)
	if got := readOpenCodeConfiguredModel(home); got != "opencode/big-pickle" {
		t.Fatalf("model = %q, want the .jsonc file's", got)
	}
	writeOpenCodeConfig(t, dir, "opencode.jsonc", `{
  // no model here
  "theme": "system",
}`)
	if got := readOpenCodeConfiguredModel(home); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("model = %q, want the .json file's when the .jsonc names none", got)
	}
	writeOpenCodeConfig(t, dir, "opencode.jsonc", `{"model": "opencode/big-pickle" /* never closed`)
	if got := readOpenCodeConfiguredModel(home); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("model = %q, want a malformed .jsonc skipped", got)
	}
}

// opencode.json is read as JSONC too, as OpenCode reads it.
func TestReadOpenCodeConfiguredModel_ReadsCommentsInOpenCodeJSON(t *testing.T) {
	home := openCodeConfigHome(t)
	writeOpenCodeConfig(t, filepath.Join(home, ".config", "opencode"), "opencode.json", openCodeJSONCWithComments)
	if got := readOpenCodeConfiguredModel(home); got != "google/gemini-2.5-pro" {
		t.Fatalf("model = %q, want opencode.json read as JSONC", got)
	}
}

// Directories keep their order: XDG_CONFIG_HOME first, then ~/.config, then
// the older ~/.opencode and the home directory itself — each with its .jsonc.
func TestReadOpenCodeConfiguredModel_DirectoryOrder(t *testing.T) {
	home := openCodeConfigHome(t)
	xdg := t.TempDir()
	writeOpenCodeConfig(t, filepath.Join(home, ".opencode"), "opencode.jsonc", `{"model": "legacy/model",}`)
	if got := readOpenCodeConfiguredModel(home); got != "legacy/model" {
		t.Fatalf("model = %q, want the ~/.opencode .jsonc", got)
	}
	writeOpenCodeConfig(t, filepath.Join(home, ".config", "opencode"), "opencode.json", `{"model": "config/model"}`)
	if got := readOpenCodeConfiguredModel(home); got != "config/model" {
		t.Fatalf("model = %q, want ~/.config before ~/.opencode", got)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeOpenCodeConfig(t, filepath.Join(xdg, "opencode"), "opencode.jsonc", `{"model": "xdg/model"}`)
	if got := readOpenCodeConfiguredModel(home); got != "xdg/model" {
		t.Fatalf("model = %q, want XDG_CONFIG_HOME first", got)
	}
	empty := openCodeConfigHome(t)
	writeOpenCodeConfig(t, empty, "opencode.jsonc", `{"model": "home/model"}`)
	if got := readOpenCodeConfiguredModel(empty); got != "home/model" {
		t.Fatalf("model = %q, want the home directory's own .jsonc", got)
	}
	if got := readOpenCodeConfiguredModel(openCodeConfigHome(t)); got != "" {
		t.Fatalf("model = %q with no config, want none", got)
	}
}

// The case that hid OpenCode's own pick: many models listed, the default only
// in opencode.jsonc. The card now reports it as the device default.
func TestOpenCodeParser_ReportsTheDefaultFromOpenCodeJSONC(t *testing.T) {
	openCodeUsageFixture(t, 5201)
	home := openCodeConfigHome(t)
	writeOpenCodeConfig(t, filepath.Join(home, ".config", "opencode"), "opencode.jsonc", openCodeJSONCWithComments)
	stubOpenCodeReadiness(t, "google/gemini-2.5-pro\nopencode/big-pickle\nopencode/grok-code\n", true)
	usage, ok := openCodeUsageParser{}.ParseContext(context.Background(), home, detectedCLIAgent{Path: "opencode"}, openCodeDirectTestNoon)
	if !ok || usage == nil {
		t.Fatal("no usage")
	}
	if usage.Model != "google/gemini-2.5-pro" {
		t.Fatalf("model = %q, want the configured default from opencode.jsonc", usage.Model)
	}
}
