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

// Verbatim `agy models` (1.1.27, macOS, 2026-09-05). The progress line is
// what the CLI prints while it fetches; the effort rides in the slug.
const realAntigravityModels = "gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
	"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
	"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
	"gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n" +
	"gemini-3.1-pro-low\tGemini 3.1 Pro (Low)\n" +
	"claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n" +
	"claude-opus-4-6-thinking\tClaude Opus 4.6 (Thinking)\n" +
	"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n" +
	"Fetching available models...\n"

// Verbatim `grok models` (Grok Build 1.0.13, logged out).
const realGrokModelsLoggedOut = "You are not authenticated.\n\nDefault model: grok-4.6\n\nAvailable models:\n  * grok-4.6 (default)\n  - grok-4.5\n"

// The `--effort` and `--model` lines of `claude --help` (2.1.260).
const realClaudeHelp = "  --effort <level>                      Effort level for the current session\n" +
	"                                        (low, medium, high, xhigh, max)\n" +
	"  --model <model>                       Model for the current session. Provide\n" +
	"                                        an alias for the latest model (e.g.\n" +
	"                                        'sonnet' or 'opus') or a model's full name\n"

func TestParseAntigravityModelListFoldsEffortSuffixes(t *testing.T) {
	got, ok := parseAntigravityModelList(realAntigravityModels)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	if !got.Exhaustive {
		t.Fatal("agy models is the whole list")
	}
	want := []cliAgentModelDetail{
		{ID: "gemini-3.8-flash", Label: "Gemini 3.8 Flash", Efforts: []string{"low", "medium", "high"}},
		{ID: "gemini-3.1-pro", Label: "Gemini 3.1 Pro", Efforts: []string{"low", "high"}},
		{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)", NoEffort: true},
		{ID: "claude-opus-4-6-thinking", Label: "Claude Opus 4.6 (Thinking)", NoEffort: true},
		{ID: "gpt-oss-120b", Label: "GPT-OSS 120B", Efforts: []string{"medium"}},
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("models = %#v\nwant %#v", got.Models, want)
	}
}

func TestParseAntigravityModelListInconclusiveWithoutRows(t *testing.T) {
	if _, ok := parseAntigravityModelList("Fetching available models...\nError: not signed in\n"); ok {
		t.Fatal("chatter without a tab-separated row must be inconclusive")
	}
}

func TestParseGrokModelListLoggedOut(t *testing.T) {
	got, ok := parseGrokModelList(realGrokModelsLoggedOut)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	want := cliAgentModelDiscovery{
		Models:       []cliAgentModelDetail{{ID: "grok-4.6"}, {ID: "grok-4.5"}},
		Exhaustive:   true,
		DefaultModel: "grok-4.6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestParseGrokModelListReadsEffortLevelsOnTheLine(t *testing.T) {
	out := "Available models:\n  * grok-4.6 (default) [efforts: low, medium, high, xhigh]\n  - grok-4.5 (efforts: high)\n"
	got, ok := parseGrokModelList(out)
	if !ok {
		t.Fatal("expected a conclusive parse")
	}
	if !reflect.DeepEqual(got.Models[0].Efforts, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("grok-4.6 efforts = %v", got.Models[0].Efforts)
	}
	if !reflect.DeepEqual(got.Models[1].Efforts, []string{"high"}) {
		t.Fatalf("grok-4.5 efforts = %v", got.Models[1].Efforts)
	}
	if got.Models[0].NoEffort || got.Models[1].NoEffort {
		t.Fatal("a named scale is never 'no effort'")
	}
}

func TestParseCodexModelsCacheKeepsListedModelsInPriorityOrder(t *testing.T) {
	raw := `{"models":[
	  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}],"default_reasoning_level":"low","visibility":"list","priority":6},
	  {"slug":"gpt-reserve","display_name":"GPT-Reserve","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}],"default_reasoning_level":"medium","visibility":"hide","priority":3},
	  {"slug":"gpt-6-astra","display_name":"GPT-6-Astra","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}],"default_reasoning_level":"medium","visibility":"list","priority":1}
	]}`
	var cache codexModelsCacheFile
	if err := json.Unmarshal([]byte(raw), &cache); err != nil {
		t.Fatal(err)
	}
	got, ok := parseCodexModelsCache(cache)
	if !ok || !got.Exhaustive {
		t.Fatalf("expected an exhaustive parse, got ok=%v %#v", ok, got)
	}
	want := []cliAgentModelDetail{
		{ID: "gpt-6-astra", Label: "GPT-6-Astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "medium"},
		{ID: "gpt-5.6-sol", Label: "GPT-5.6-Sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
	}
	if !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("models = %#v\nwant %#v", got.Models, want)
	}
}

func TestDiscoverCodexModelsReadsCodexHome(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6-astra","supported_reasoning_levels":[{"effort":"high"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	got, ok := discoverCodexModels(home)
	if !ok || len(got.Models) != 1 || got.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("got ok=%v %#v", ok, got)
	}
	if _, ok := discoverCodexModels(t.TempDir()); ok {
		t.Fatal("a missing cache is inconclusive, not empty")
	}
}

func TestClaudeModelDiscoveryReadsScaleFromHelp(t *testing.T) {
	got := claudeModelDiscovery(realClaudeHelp)
	if got.Exhaustive {
		t.Fatal("the alias set is a floor, not the whole list")
	}
	ids := []string{}
	for _, model := range got.Models {
		ids = append(ids, model.ID)
		if !reflect.DeepEqual(model.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
			t.Fatalf("%s efforts = %v", model.ID, model.Efforts)
		}
	}
	if !reflect.DeepEqual(ids, []string{"fable", "opus", "sonnet", "haiku"}) {
		t.Fatalf("aliases = %v", ids)
	}
	bare := claudeModelDiscovery("no effort flag here")
	if bare.Models[0].Efforts != nil || bare.Models[0].NoEffort {
		t.Fatal("an unparsed help leaves the scale unknown, not empty")
	}
}

func TestAttachCLIAgentModelDiscoveryEnrichesAndCaches(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	calls := 0
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(executable string, env []string, args ...string) (string, bool) {
		calls++
		if !strings.HasSuffix(executable, "agy") || len(args) != 1 || args[0] != "models" {
			t.Fatalf("unexpected probe %s %v", executable, args)
		}
		return realAntigravityModels, true
	}
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })

	detected := detectedCLIAgent{Detected: true, Path: "/usr/local/bin/agy", Version: "1.1.27"}
	now := time.Now()
	usage := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery("antigravity", detected, usage, "", now)
	if len(usage.ModelDetails) != 5 || usage.ModelsExhaustive == nil || !*usage.ModelsExhaustive {
		t.Fatalf("snapshot not enriched: %#v", usage)
	}
	if !reflect.DeepEqual(usage.Models, []string{"gemini-3.8-flash", "gemini-3.1-pro", "claude-sonnet-4-6", "claude-opus-4-6-thinking", "gpt-oss-120b"}) {
		t.Fatalf("models = %v", usage.Models)
	}
	again := &cliAgentUsage{Provider: "antigravity"}
	attachCLIAgentModelDiscovery("antigravity", detected, again, "", now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("probe ran %d times inside the TTL", calls)
	}
	attachCLIAgentModelDiscovery("antigravity", detected, again, "", now.Add(cliAgentModelProbeTTL+time.Second))
	if calls != 2 {
		t.Fatalf("probe did not re-run after the TTL (%d)", calls)
	}
}

func TestAttachCLIAgentModelDiscoveryLeavesUnknownAgentsAlone(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(string, []string, ...string) (string, bool) {
		t.Fatal("no probe exists for this agent")
		return "", false
	}
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	usage := &cliAgentUsage{Provider: "somethingElse"}
	attachCLIAgentModelDiscovery("somethingElse", detectedCLIAgent{Detected: true, Path: "/bin/x"}, usage, "", time.Now())
	if usage.ModelDetails != nil || usage.ModelsExhaustive != nil {
		t.Fatalf("unexpected enrichment: %#v", usage)
	}
}

func TestAttachCLIAgentModelDiscoveryReshapesOpenCode(t *testing.T) {
	usage := &cliAgentUsage{Provider: "opencode", Models: []string{"ollama/qwen3-coder:30b", "opencode/big-pickle"}}
	attachCLIAgentModelDiscovery("opencode", detectedCLIAgent{Detected: true}, usage, "", time.Now())
	want := []cliAgentModelDetail{{ID: "ollama/qwen3-coder:30b"}, {ID: "opencode/big-pickle"}}
	if !reflect.DeepEqual(usage.ModelDetails, want) || usage.ModelsExhaustive == nil || !*usage.ModelsExhaustive {
		t.Fatalf("got %#v", usage)
	}
}

func TestAttachCLIAgentModelDiscoveryInconclusiveProbeReportsNothing(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(string, []string, ...string) (string, bool) { return "", false }
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	usage := &cliAgentUsage{Provider: "grok"}
	attachCLIAgentModelDiscovery("grok", detectedCLIAgent{Detected: true, Path: "/bin/grok"}, usage, "", time.Now())
	if usage.ModelDetails != nil || usage.Models != nil || usage.ModelsExhaustive != nil {
		t.Fatalf("an inconclusive probe must leave the snapshot untouched: %#v", usage)
	}
}

func TestCLIUsageReceiptCarriesModelDetails(t *testing.T) {
	agent := cliAgentUsage{
		Provider: "antigravity", CollectedAt: "now",
		Models:           []string{"gemini-3.8-flash", "claude-sonnet-4-6"},
		ModelDetails:     []cliAgentModelDetail{{ID: "gemini-3.8-flash", Label: "Gemini 3.8 Flash", Efforts: []string{"low", "medium", "high"}}, {ID: "claude-sonnet-4-6", NoEffort: true}},
		ModelsExhaustive: authBoolPtr(true),
	}
	canonical, _, _, err := canonicalCLIUsageRefreshReceipt("r4", 4, true, []cliAgentUsage{agent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `"models":["gemini-3.8-flash","claude-sonnet-4-6"],"modelDetails":[{"id":"gemini-3.8-flash","label":"Gemini 3.8 Flash","efforts":["low","medium","high"]},{"id":"claude-sonnet-4-6","noEffort":true}],"modelsExhaustive":true,"collectedAt":"now"`
	if !strings.Contains(string(canonical), want) {
		t.Fatalf("canonical = %s", canonical)
	}
	// The shared cross-language vector (testdata/…_vectors.json, mirrored in
	// terminal-service) pins the byte layout the verifier reproduces.
	data, err := os.ReadFile("testdata/cli_usage_refresh_receipt_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Vectors []struct{ Name, Canonical, Signature string } `json:"vectors"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	vector := vectors.Vectors[3]
	withDefault := agent
	withDefault.ModelDetails = []cliAgentModelDetail{{ID: "gemini-3.8-flash", Label: "Gemini 3.8 Flash", Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"}, {ID: "claude-sonnet-4-6", NoEffort: true}}
	vectorCanonical, _, _, err := canonicalCLIUsageRefreshReceipt("r4", 4, true, []cliAgentUsage{withDefault}, nil)
	if err != nil || string(vectorCanonical) != vector.Canonical {
		t.Fatalf("shared vector %q canonical mismatch: %s (%v)", vector.Name, vectorCanonical, err)
	}
	signature, _, _, err := signCLIUsageRefreshReceipt("secret", "r4", 4, true, []cliAgentUsage{withDefault}, nil)
	if err != nil || signature != vector.Signature {
		t.Fatalf("shared vector %q signature mismatch: %s (%v)", vector.Name, signature, err)
	}

	dup := agent
	dup.ModelDetails = []cliAgentModelDetail{{ID: "x"}, {ID: "x"}}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r4", 4, true, []cliAgentUsage{dup}, nil); err == nil {
		t.Fatal("duplicate model ids must be rejected")
	}
	long := agent
	long.ModelDetails = []cliAgentModelDetail{{ID: "x", Efforts: make([]string, cliUsageMaxEffortsPerModel+1)}}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r4", 4, true, []cliAgentUsage{long}, nil); err == nil {
		t.Fatal("an over-long scale must be rejected")
	}
}
