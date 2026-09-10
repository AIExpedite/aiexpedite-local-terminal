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

// Edge cases around the Ship B5 parsers and the snapshot enrichment that the
// fixture-driven tests in cliagent_models_test.go do not reach: the token and
// suffix helpers, the receipt bounds applied at collection time, the
// config.toml reader, and the enrichment rules that must never overwrite what
// a provider's own parser already established.

func TestNormalizeEffortTokenAcceptsOnlyTheSharedUnion(t *testing.T) {
	for _, raw := range []string{"low", " High ", "XHIGH", "max", "ultra", "minimal", "medium"} {
		if _, ok := normalizeEffortToken(raw); !ok {
			t.Fatalf("%q is a level every CLI draws from", raw)
		}
	}
	// `auto` means "pass no flag", `deep` is Grok's private menu id, and a
	// label is not a token a CLI accepts on its flag.
	for _, raw := range []string{"auto", "deep", "extra high", "", "  ", "high-effort", "3"} {
		if got, ok := normalizeEffortToken(raw); ok {
			t.Fatalf("%q must not be reported as an effort (got %q)", raw, got)
		}
	}
}

func TestAppendEffortDedupesAndKeepsTheScaleOrderedLowToHigh(t *testing.T) {
	scale := appendEffort(nil, "max")
	scale = appendEffort(scale, "low")
	scale = appendEffort(scale, "high")
	scale = appendEffort(scale, "low")
	scale = appendEffort(scale, "minimal")
	if !reflect.DeepEqual(scale, []string{"minimal", "low", "high", "max"}) {
		t.Fatalf("scale = %v", scale)
	}
}

func TestSplitEffortSuffixOnlyFoldsAKnownLevel(t *testing.T) {
	cases := []struct{ slug, family, effort string }{
		{"gemini-3.8-flash-high", "gemini-3.8-flash", "high"},
		{"gpt-oss-120b-medium", "gpt-oss-120b", "medium"},
		{"claude-opus-4-6-thinking", "claude-opus-4-6-thinking", ""},
		{"claude-sonnet-4-6", "claude-sonnet-4-6", ""},
		{"gemini-3.8-flash-", "gemini-3.8-flash-", ""},
		{"-high", "-high", ""},
		{"high", "high", ""},
		{"model-auto", "model-auto", ""},
	}
	for _, tc := range cases {
		family, effort := splitEffortSuffix(tc.slug)
		if family != tc.family || effort != tc.effort {
			t.Fatalf("%q → (%q, %q), want (%q, %q)", tc.slug, family, effort, tc.family, tc.effort)
		}
	}
}

func TestParseAntigravityModelListEdges(t *testing.T) {
	// A slug with whitespace, an empty slug, and a row with no label are
	// skipped or kept without inventing a label; the family keeps its first
	// label with the effort suffix stripped; effort order is the shared
	// ranking regardless of the CLI's own ordering.
	out := "gemini-x-low\tGemini X (Low)\n" +
		"gemini-x-max\tGemini X (Max)\n" +
		"gemini-x-medium\tGemini X (Medium)\n" +
		"bad slug\tBroken\n" +
		"\tNo slug\n" +
		"bare-model\t\n"
	got, ok := parseAntigravityModelList(out)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	want := []cliAgentModelDetail{
		{ID: "gemini-x", Label: "Gemini X", Efforts: []string{"low", "medium", "max"}},
		{ID: "bare-model", NoEffort: true},
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("models = %#v\nwant %#v", got.Models, want)
	}
	if _, ok := parseAntigravityModelList(""); ok {
		t.Fatal("empty output is inconclusive")
	}
}

func TestParseGrokModelListAcceptsEveryBulletAndDedupes(t *testing.T) {
	out := "Available models:\n• grok-4.6 (default)\n* grok-4.5\n- grok-4.5\n  - grok-lite extra words\nnot a bullet grok-9\n"
	got, ok := parseGrokModelList(out)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	ids := []string{}
	for _, model := range got.Models {
		ids = append(ids, model.ID)
	}
	if !reflect.DeepEqual(ids, []string{"grok-4.6", "grok-4.5", "grok-lite"}) || got.DefaultModel != "grok-4.6" {
		t.Fatalf("got %#v", got)
	}
	if _, ok := parseGrokModelList("You are not authenticated.\n"); ok {
		t.Fatal("a list with no bullet rows is inconclusive")
	}
}

func TestParseCodexModelsCacheVisibilityAndDefaults(t *testing.T) {
	raw := `{"models":[
	  {"slug":"gpt-old","display_name":"Old","default_reasoning_level":"medium","priority":2},
	  {"slug":"gpt-new","display_name":"New","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"deep"}],"default_reasoning_level":"medium","visibility":"LIST","priority":1},
	  {"slug":"gpt-new","display_name":"Duplicate","visibility":"list","priority":0},
	  {"slug":"","display_name":"Nameless","visibility":"list"}
	]}`
	var cache codexModelsCacheFile
	if err := json.Unmarshal([]byte(raw), &cache); err != nil {
		t.Fatal(err)
	}
	got, ok := parseCodexModelsCache(cache)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	want := []cliAgentModelDetail{
		// Visibility compares case-insensitively; `deep` is outside the shared
		// union; a default level outside the kept scale is not reported.
		{ID: "gpt-new", Label: "New", Efforts: []string{"low", "high"}},
		// Unset visibility (older caches) is listed; no levels at all means
		// the CLI said this model takes no effort flag.
		{ID: "gpt-old", Label: "Old", NoEffort: true},
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("models = %#v\nwant %#v", got.Models, want)
	}
	if _, ok := parseCodexModelsCache(codexModelsCacheFile{}); ok {
		t.Fatal("an empty cache is inconclusive, not an empty exhaustive list")
	}
}

func TestReadCodexConfiguredModelReadsOnlyTheTopLevelTable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	if got := readCodexConfiguredModel(home); got != "" {
		t.Fatalf("no config.toml must read as no configured model, got %q", got)
	}
	// A profile's model is not the machine default; the top-level line wins
	// even when a comment mentions `model` first.
	toml := "# model = \"commented-out\"\nmodel_reasoning_effort = \"high\"\nmodel = \"gpt-6-astra\"\n[profiles.fast]\nmodel = \"gpt-5.4-mini\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readCodexConfiguredModel(home); got != "gpt-6-astra" {
		t.Fatalf("configured = %q", got)
	}
	// Only a profile table: nothing at the top level.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[profiles.fast]\nmodel = \"gpt-5.4-mini\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readCodexConfiguredModel(home); got != "" {
		t.Fatalf("a profile-only config must read as no default, got %q", got)
	}
	// CODEX_HOME redirects the lookup away from ~/.codex.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "config.toml"), []byte("model = \"gpt-elsewhere\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", other)
	if got := readCodexConfiguredModel(home); got != "gpt-elsewhere" {
		t.Fatalf("CODEX_HOME config = %q", got)
	}
}

func TestBoundedModelDetailsApplyTheReceiptCapsAtCollectionTime(t *testing.T) {
	long := cliAgentModelDetail{ID: "x", Label: strings.Repeat("l", 257), Efforts: make([]string, cliUsageMaxEffortsPerModel+3)}
	for i := range long.Efforts {
		long.Efforts[i] = "low"
	}
	got := boundedModelDetail(long)
	if got.Label != "" {
		t.Fatal("an over-long label is dropped, not truncated into a different label")
	}
	if len(got.Efforts) != cliUsageMaxEffortsPerModel {
		t.Fatalf("efforts = %d, want the receipt cap %d", len(got.Efforts), cliUsageMaxEffortsPerModel)
	}
	many := make([]cliAgentModelDetail, cliUsageMaxModelsPerProvider+5)
	for i := range many {
		many[i] = cliAgentModelDetail{ID: strings.Repeat("m", 1) + string(rune('a'+i%26)) + strings.Repeat("-", i/26)}
	}
	kept := boundedModelDetails(many)
	if len(kept) != cliUsageMaxModelsPerProvider || kept[0].ID != many[0].ID {
		t.Fatalf("kept %d models, first %q — the CLI's order must survive the cut", len(kept), kept[0].ID)
	}
	// Bounded output always passes the receipt canonicalizer, so a
	// pathological CLI answer can never invalidate a refresh.
	agent := cliAgentUsage{Provider: "codex", CollectedAt: "now", ModelDetails: boundedModelDetails([]cliAgentModelDetail{got})}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{agent}, nil); err != nil {
		t.Fatalf("bounded details must canonicalize: %v", err)
	}
}

func TestMergeGrokDiscoveryFallsBackToTheModelLevelDefault(t *testing.T) {
	// No `default: true` in the menu: the model-level `reasoning_effort`
	// names the default when it is on the kept scale; a supports=false model
	// with no menu is a no-effort model; a listed model the cache does not
	// know keeps its unknown scale; a menu entry with an empty value falls
	// back to its id.
	raw := `{"grok_version":"1.0.13","models":{
	  "grok-a":{"info":{"id":"grok-a","name":"Grok A","reasoning_effort":"high","reasoning_efforts":[{"id":"low","value":"low"},{"id":"high","value":""}]}},
	  "grok-b":{"info":{"id":"grok-b","name":"Grok B","supports_reasoning_effort":false,"reasoning_efforts":[]}},
	  "grok-c":{"info":{"id":"grok-c","name":"Grok C","reasoning_effort":"ultra","reasoning_efforts":[{"id":"low","value":"low"}]}}
	}}`
	var cache grokModelsCacheFile
	if err := json.Unmarshal([]byte(raw), &cache); err != nil {
		t.Fatal(err)
	}
	listed := cliAgentModelDiscovery{Exhaustive: true, DefaultModel: "grok-a", Models: []cliAgentModelDetail{{ID: "grok-a"}, {ID: "grok-unknown"}}}
	got := mergeGrokDiscovery(listed, true, cache, true, "grok 1.0.13")
	want := []cliAgentModelDetail{
		{ID: "grok-a", Label: "Grok A", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
		{ID: "grok-unknown"},
		{ID: "grok-b", Label: "Grok B", NoEffort: true},
		{ID: "grok-c", Label: "Grok C", Efforts: []string{"low"}},
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("models = %#v\nwant %#v", got.Models, want)
	}
	if !got.Exhaustive || got.DefaultModel != "grok-a" {
		t.Fatalf("got exhaustive=%v default=%q", got.Exhaustive, got.DefaultModel)
	}
	// A scale the list itself named is never overwritten by the cache.
	own := cliAgentModelDiscovery{Models: []cliAgentModelDetail{{ID: "grok-a", Efforts: []string{"max"}}}}
	if m := mergeGrokDiscovery(own, true, cache, true, "grok 1.0.13").Models[0]; !reflect.DeepEqual(m.Efforts, []string{"max"}) || m.Label != "Grok A" {
		t.Fatalf("list-named scale must win, label still enriched: %#v", m)
	}
}

func TestAttachCLIAgentModelDiscoveryNeverOverwritesTheParsersOwnModelFields(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(string, []string, ...string) (string, bool) { return realAntigravityModels, true }
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })

	usage := &cliAgentUsage{Provider: "antigravity", Model: "gemini-3.1-pro", Models: []string{"gemini-3.1-pro"}}
	attachCLIAgentModelDiscovery("antigravity", detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.27"}, usage, "", time.Now())
	if usage.Model != "gemini-3.1-pro" || !reflect.DeepEqual(usage.Models, []string{"gemini-3.1-pro"}) {
		t.Fatalf("the parser's model and list must survive enrichment: %#v", usage)
	}
	if len(usage.ModelDetails) != 5 || usage.ModelsExhaustive == nil || !*usage.ModelsExhaustive {
		t.Fatalf("details still attached: %#v", usage)
	}
	// A nil snapshot and an OpenCode snapshot without models are no-ops.
	attachCLIAgentModelDiscovery("antigravity", detectedCLIAgent{Detected: true}, nil, "", time.Now())
	empty := &cliAgentUsage{Provider: "opencode"}
	attachCLIAgentModelDiscovery("opencode", detectedCLIAgent{Detected: true}, empty, "", time.Now())
	if empty.ModelDetails != nil || empty.ModelsExhaustive != nil {
		t.Fatalf("OpenCode with no enumerated models must stay untouched: %#v", empty)
	}
}

func TestModelDiscoveryCacheIsKeyedByBinaryAndReset(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	calls := 0
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(string, []string, ...string) (string, bool) {
		calls++
		return realAntigravityModels, true
	}
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	now := time.Now()
	old := detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.27"}
	upgraded := detectedCLIAgent{Detected: true, Path: "/bin/agy", Version: "1.1.28"}
	attachCLIAgentModelDiscovery("antigravity", old, &cliAgentUsage{}, "", now)
	attachCLIAgentModelDiscovery("antigravity", old, &cliAgentUsage{}, "", now)
	if calls != 1 {
		t.Fatalf("same binary inside the TTL probed %d times", calls)
	}
	// A new version (or path) is a different binary: its list is asked for.
	attachCLIAgentModelDiscovery("antigravity", upgraded, &cliAgentUsage{}, "", now)
	if calls != 2 {
		t.Fatalf("an upgraded binary must be re-probed (%d)", calls)
	}
	// A forced usage refresh empties the cache so a fresh login is seen now.
	resetCLIAgentModelProbeCache()
	attachCLIAgentModelDiscovery("antigravity", upgraded, &cliAgentUsage{}, "", now)
	if calls != 3 {
		t.Fatalf("reset must force a re-probe (%d)", calls)
	}
	// An inconclusive answer is cached too, so a broken CLI is not spawned
	// every gather cycle.
	cliAgentModelProbeRunner = func(string, []string, ...string) (string, bool) { calls++; return "", false }
	resetCLIAgentModelProbeCache()
	attachCLIAgentModelDiscovery("antigravity", old, &cliAgentUsage{}, "", now)
	attachCLIAgentModelDiscovery("antigravity", old, &cliAgentUsage{}, "", now.Add(time.Minute))
	if calls != 4 {
		t.Fatalf("an inconclusive probe must be cached for the TTL (%d)", calls)
	}
}

func TestClaudeModelDiscoveryScaleIsPerAliasCopy(t *testing.T) {
	got := claudeModelDiscovery(realClaudeHelp)
	got.Models[0].Efforts[0] = "tampered"
	if got.Models[1].Efforts[0] == "tampered" {
		t.Fatal("aliases must not share one scale slice")
	}
	if claudeModelAliases[0].Efforts != nil {
		t.Fatal("the alias table itself must stay scale-free")
	}
}

func TestRunCLIAgentModelProbeRejectsAnEmptyExecutable(t *testing.T) {
	if out, ok := runCLIAgentModelProbe("   ", nil, "models"); ok || out != "" {
		t.Fatal("an empty executable is inconclusive")
	}
}
