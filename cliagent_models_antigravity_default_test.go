package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperAgyModelsArgon is `agy models` in its tab-separated shape with the
// families agy listed on 2026-11-02 (gemini-4-argon, gemini-3.8-flash,
// claude-opus-5-5, claude-sonnet-5-5, gpt-oss-120b). Not a verbatim capture:
// running `agy` here would write a run log the agent then owes a refresh for,
// and the third-party labels are assumed.
const helperAgyModelsArgon = "gemini-4-argon-high\tGemini 4 Argon (High)\n" +
	"gemini-4-argon-medium\tGemini 4 Argon (Medium)\n" +
	"gemini-4-argon-low\tGemini 4 Argon (Low)\n" +
	"gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
	"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
	"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
	"claude-opus-5-5\tClaude Opus 5.5 (Thinking)\n" +
	"claude-sonnet-5-5\tClaude Sonnet 5.5 (Thinking)\n" +
	"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n" +
	"Fetching available models...\n"

func helperAgyArgonFamilies(t *testing.T) []cliAgentModelDetail {
	t.Helper()
	discovery, ok := parseAntigravityModelList(helperAgyModelsArgon)
	if !ok || len(discovery.Models) != 5 {
		t.Fatalf("discovery=%+v ok=%v, want five families", discovery, ok)
	}
	return discovery.Models
}

func helperAgySettings(t *testing.T, home string, settings map[string]any) {
	t.Helper()
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), settings)
}

// The Argon row names gemini-4-argon, agy's own id for that family, so the
// device pools that family with no table; every other family is left to the
// catalog's rule, and a group naming a model agy does not list pools nothing.
func TestAnnotateModelPoolsFromUsage_AntigravityModelGroupPoolsItsFamily(t *testing.T) {
	metrics := antigravityQuotaMetrics(helperArgonSnapshot(t), helperArgonObservedAt)
	for _, model := range annotateModelPoolsFromUsage(helperAgyArgonFamilies(t), metrics) {
		want := ""
		if model.ID == "gemini-4-argon" {
			want = "gemini-4-argon"
		}
		if model.Pool != want {
			t.Errorf("%s pool = %q, want %q", model.ID, model.Pool, want)
		}
	}

	carbon := antigravityQuotaSnapshot{
		ObservedAt: helperArgonObservedAt.Format(time.RFC3339),
		Buckets:    []antigravityQuotaBucket{{Group: "Gemini 4 Carbon", Window: "weekly", RemainingFraction: 0, ResetTime: "2026-11-03T08:06:36Z"}},
	}
	rows := antigravityQuotaMetrics(carbon, helperArgonObservedAt)
	if len(rows) != 1 || rows[0].Model != "gemini-4-carbon" {
		t.Fatalf("rows=%+v, want the Carbon row keyed by its model", rows)
	}
	for _, model := range annotateModelPoolsFromUsage(helperAgyArgonFamilies(t), rows) {
		if model.Pool != "" {
			t.Errorf("%s pool = %q, want none: agy lists no gemini-4-carbon", model.ID, model.Pool)
		}
	}
}

func TestAntigravitySavedDefaultModel_LabelSlugAndName(t *testing.T) {
	families := helperAgyArgonFamilies(t)
	for _, tc := range []struct{ saved, want string }{
		{"Gemini 4 Argon (Medium)", "gemini-4-argon"}, // what /model saves
		{"gemini 4  argon (high)", "gemini-4-argon"},
		{"Gemini 4 Argon", "gemini-4-argon"},
		{"gemini-4-argon-medium", "gemini-4-argon"}, // a slug
		{"gemini-4-argon", "gemini-4-argon"},        // a family id
		{"GEMINI-3.8-FLASH", "gemini-3.8-flash"},
		{"Claude Opus 5.5 (Thinking)", "claude-opus-5-5"},
		{"claude-sonnet-5-5", "claude-sonnet-5-5"},
		{" GPT-OSS 120B (Medium) ", "gpt-oss-120b"},
	} {
		home := t.TempDir()
		helperAgySettings(t, home, map[string]any{"model": tc.saved})
		if got := antigravitySavedDefaultModel(home, families); got != tc.want {
			t.Errorf("saved %q resolved to %q, want %q", tc.saved, got, tc.want)
		}
	}
}

// Never guessed: a model agy cannot resolve fails a pinned print-mode run, so
// anything but one discovered family reports nothing.
func TestAntigravitySavedDefaultModel_NeverGuesses(t *testing.T) {
	families := helperAgyArgonFamilies(t)
	for _, model := range []any{"Gemini 9 Ultra (High)", "gemini-4", "Argon", "", "   ", 42, nil, map[string]any{"id": "gemini-4-argon"}} {
		home := t.TempDir()
		helperAgySettings(t, home, map[string]any{"model": model})
		if got := antigravitySavedDefaultModel(home, families); got != "" {
			t.Errorf("saved %#v resolved to %q, want nothing", model, got)
		}
	}

	// Two families answering to one label are ambiguous.
	twins := append(append([]cliAgentModelDetail{}, families...), cliAgentModelDetail{ID: "gemini-4-argon-preview", Label: "Gemini 4 Argon"})
	home := t.TempDir()
	helperAgySettings(t, home, map[string]any{"model": "Gemini 4 Argon (Medium)"})
	if got := antigravitySavedDefaultModel(home, twins); got != "" {
		t.Errorf("an ambiguous label resolved to %q, want nothing", got)
	}
	// Nothing discovered, nothing to resolve against.
	if got := antigravitySavedDefaultModel(home, nil); got != "" {
		t.Errorf("no families resolved to %q", got)
	}

	// A settings file past the read bound is not read at all.
	big := t.TempDir()
	helperAgySettings(t, big, map[string]any{"model": "gemini-4-argon", "padding": strings.Repeat("x", antigravitySettingsMaxBytes)})
	if got := antigravitySavedDefaultModel(big, families); got != "" {
		t.Errorf("an oversized settings file resolved to %q", got)
	}
	if got := antigravitySavedDefaultModel("", families); got != "" {
		t.Errorf("no home resolved to %q", got)
	}
}

// The legacy ~/.agy/config.json answers only on a machine with no modern
// settings file, the same tree Parse falls back to.
func TestAntigravitySavedDefaultModel_LegacyConfigOnlyWithoutModernSettings(t *testing.T) {
	families := helperAgyArgonFamilies(t)
	home := t.TempDir()
	helperWriteJSON(t, filepath.Join(home, ".agy", "config.json"), map[string]any{"model": "gemini-3.8-flash-high"})
	if got := antigravitySavedDefaultModel(home, families); got != "gemini-3.8-flash" {
		t.Errorf("legacy config resolved to %q, want gemini-3.8-flash", got)
	}
	helperAgySettings(t, home, map[string]any{"artifactReviewMode": "auto"})
	if got := antigravitySavedDefaultModel(home, families); got != "" {
		t.Errorf("with a modern settings file naming no model the legacy one answered %q", got)
	}
}

// settings.json sits beside credentials-adjacent keys; only `model` is decoded,
// so a value of any other key — whatever its type — can neither break the read
// nor reach the snapshot. The identity keys hold types a broader decode target
// (antigravityConfig's strings) would refuse, failing the whole read.
func TestAntigravitySavedDefaultModel_DecodesOnlyTheModelKey(t *testing.T) {
	home := t.TempDir()
	helperAgySettings(t, home, map[string]any{
		"model":               "Gemini 4 Argon (Medium)",
		"apiKey":              "sk-live-DO-NOT-LEAK-0987654321",
		"oauth":               map[string]any{"refresh_token": "1//refresh-DO-NOT-LEAK", "expiry": 1730000000},
		"email":               map[string]any{"address": "ada@example.com"},
		"plan":                7,
		"trustedWorkspaces":   []any{"C:\\work\\secret-repo"},
		"toolExecutionPolicy": map[string]any{"allow": []any{"*"}},
	})
	if got := antigravitySavedDefaultModel(home, helperAgyArgonFamilies(t)); got != "gemini-4-argon" {
		t.Fatalf("resolved %q, want gemini-4-argon", got)
	}
}

// End to end on the snapshot: the Argon family carries its pool, the saved
// /model choice is the agent's reported default, and nothing else from
// settings.json is published.
func TestAttachCLIAgentModelDiscovery_AntigravityArgonPoolAndSavedDefault(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(_ context.Context, _ string, _ []string, args ...string) (string, bool) {
		if len(args) != 1 || args[0] != "models" {
			t.Fatalf("unexpected probe args %v", args)
		}
		return helperAgyModelsArgon, true
	}
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	home := t.TempDir()
	helperAgySettings(t, home, map[string]any{
		"model":  "Gemini 4 Argon (Medium)",
		"apiKey": "sk-live-DO-NOT-LEAK-0987654321",
		"email":  "ada@example.com",
	})

	usage := &cliAgentUsage{Provider: "antigravity", Metrics: antigravityQuotaMetrics(helperArgonSnapshot(t), helperArgonObservedAt)}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.2.3"}, usage, home, helperArgonObservedAt)
	if usage.Model != "gemini-4-argon" {
		t.Errorf("Model=%q, want the saved /model choice", usage.Model)
	}
	pools := map[string]string{}
	for _, model := range usage.ModelDetails {
		pools[model.ID] = model.Pool
	}
	if len(pools) != 5 || pools["gemini-4-argon"] != "gemini-4-argon" || pools["gemini-3.8-flash"] != "" ||
		pools["claude-opus-5-5"] != "" || pools["gpt-oss-120b"] != "" {
		t.Errorf("pools=%v, want only gemini-4-argon pooled", pools)
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sk-live", "ada@", "apiKey"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the snapshot carries %q: %s", leak, raw)
		}
	}
	if _, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1, true, []cliAgentUsage{*usage}, nil); err != nil {
		t.Fatalf("the snapshot was rejected from the signed refresh: %v", err)
	}

	// A model the account does not run never becomes the default: no list,
	// no guess (the probe cache is reset so the runner answers nothing).
	resetCLIAgentModelProbeCache()
	cliAgentModelProbeRunner = func(context.Context, string, []string, ...string) (string, bool) { return "", false }
	bare := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery(context.Background(), "antigravity", detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.2.3"}, bare, home, helperArgonObservedAt)
	if bare.Model != "" || len(bare.ModelDetails) != 0 {
		t.Errorf("without a discovered list: Model=%q details=%+v, want nothing", bare.Model, bare.ModelDetails)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")); err != nil {
		t.Fatalf("settings.json must be left as it was: %v", err)
	}
}
