package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Codex round 5 on #147: a Codex models cache is a floor even when its writer
// matches the installed build, and a scale made only of levels this build does
// not recognise is UNKNOWN, not "takes no effort".

func TestCodexCacheIsNeverExhaustiveEvenFromTheInstalledBuild(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	// Same client_version as the installed CLI, configured model listed: the
	// strongest case for "complete", and still only a floor — the catalog is
	// server-side and only a `codex` run refreshes this file.
	cache := `{"client_version":"0.153.4","models":[{"slug":"gpt-6-astra","visibility":"list","supported_reasoning_levels":[{"effort":"high"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model = \"gpt-6-astra\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := discoverCodexModels(home, "codex-cli 0.153.4")
	if !ok || got.Exhaustive {
		t.Fatalf("got ok=%v exhaustive=%v — a Codex cache must never veto a pin", ok, got.Exhaustive)
	}
	if got.DefaultModel != "gpt-6-astra" || len(got.Models) != 1 {
		t.Fatalf("got %#v", got)
	}
}

func TestParseCodexModelsCacheUnknownLevelsLeaveTheScaleUnknown(t *testing.T) {
	raw := `{"models":[
	  {"slug":"gpt-future","display_name":"Future","supported_reasoning_levels":[{"effort":"deep"},{"effort":"extreme"}],"default_reasoning_level":"deep","visibility":"list","priority":1},
	  {"slug":"gpt-flagless","display_name":"Flagless","supported_reasoning_levels":[],"visibility":"list","priority":2},
	  {"slug":"gpt-mixed","display_name":"Mixed","supported_reasoning_levels":[{"effort":"deep"},{"effort":"high"}],"default_reasoning_level":"deep","visibility":"list","priority":3}
	]}`
	var cache codexModelsCacheFile
	if err := json.Unmarshal([]byte(raw), &cache); err != nil {
		t.Fatal(err)
	}
	got, ok := parseCodexModelsCache(cache)
	if !ok || len(got.Models) != 3 {
		t.Fatalf("got ok=%v %#v", ok, got)
	}
	future, flagless, mixed := got.Models[0], got.Models[1], got.Models[2]
	// Only unrecognised levels: the CLI DOES take an effort flag, we just do
	// not know the menu — Efforts nil and NoEffort false, so the resolver
	// neither clamps nor strips.
	if future.Efforts != nil || future.NoEffort || future.DefaultEffort != "" {
		t.Fatalf("unknown-only scale must stay unknown: %#v", future)
	}
	// Genuinely no levels listed: the model refuses the flag.
	if !flagless.NoEffort || flagless.Efforts != nil {
		t.Fatalf("an empty level list is no-effort: %#v", flagless)
	}
	// A mix keeps the recognised part; an unrecognised default is not reported.
	if len(mixed.Efforts) != 1 || mixed.Efforts[0] != "high" || mixed.NoEffort || mixed.DefaultEffort != "" {
		t.Fatalf("mixed scale: %#v", mixed)
	}
}
