package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Ship B6 device half: a model the usage snapshot meters under its own window
// reports the pool it spends; every other model is left for the catalog rule.

func TestAnnotateModelPoolsFromUsage_NamesTheMeteredModelsPool(t *testing.T) {
	models := claudeModelDiscovery(realClaudeHelp).Models
	metrics := claudeCodeMetricsFromBuckets(map[string]claudeRateLimitBucket{}, time.Now())
	got := annotateModelPoolsFromUsage(models, metrics)
	byID := map[string]string{}
	for _, model := range got {
		byID[model.ID] = model.Pool
	}
	if byID[claudeModelFable] != claudeModelFable {
		t.Fatalf("fable pool = %q, want the weekly Fable window's key %q", byID[claudeModelFable], claudeModelFable)
	}
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		if byID[alias] != "" {
			t.Fatalf("%s pool = %q, want none: the shared weekly window names no model", alias, byID[alias])
		}
	}
	// The input is not mutated: the probe cache holds the raw discovery and a
	// later gather re-derives pools from ITS usage snapshot.
	for _, model := range models {
		if model.Pool != "" {
			t.Fatalf("annotate mutated the discovery: %#v", model)
		}
	}
}

func TestAnnotateModelPoolsFromUsage_MatchesCaseInsensitivelyAndKeepsAProbeSetPool(t *testing.T) {
	models := []cliAgentModelDetail{
		{ID: "gpt-5.3-codex-spark"},
		{ID: "gpt-5.3-codex"},
		{ID: "already", Pool: "Vendor pool"},
	}
	metrics := []cliAgentUsageMetric{
		{Kind: limitKindSession, Label: "5-hour session window"},
		{Kind: limitKindWeekly, Label: "GPT-5.3-Codex-Spark — Weekly quota", Model: codexPoolModelID("GPT-5.3-Codex-Spark")},
		{Kind: limitKindWeekly, Label: "other", Model: " ALREADY "},
	}
	got := annotateModelPoolsFromUsage(models, metrics)
	if got[0].Pool != "gpt-5.3-codex-spark" {
		t.Fatalf("spark pool = %q, want the lower-cased model id", got[0].Pool)
	}
	if got[1].Pool != "" {
		t.Fatalf("codex pool = %q, want none", got[1].Pool)
	}
	if got[2].Pool != "Vendor pool" {
		t.Fatalf("a pool the probe set must win: %q", got[2].Pool)
	}
	if out := annotateModelPoolsFromUsage(models, nil); out[0].Pool != "" {
		t.Fatal("no metrics, no pools")
	}
	if out := annotateModelPoolsFromUsage(nil, metrics); out != nil {
		t.Fatal("no models, nothing to annotate")
	}
}

func TestAttachCLIAgentModelDiscovery_ReportsPoolOnTheSnapshot(t *testing.T) {
	resetCLIAgentModelProbeCache()
	t.Cleanup(resetCLIAgentModelProbeCache)
	prev := cliAgentModelProbeRunner
	cliAgentModelProbeRunner = func(_ context.Context, _ string, _ []string, args ...string) (string, bool) {
		if len(args) != 1 || args[0] != "--help" {
			t.Fatalf("unexpected probe args %v", args)
		}
		return realClaudeHelp, true
	}
	t.Cleanup(func() { cliAgentModelProbeRunner = prev })
	now := time.Now()
	usage := &cliAgentUsage{
		Provider: "claudeCode",
		Metrics:  claudeCodeMetricsFromBuckets(map[string]claudeRateLimitBucket{}, now),
	}
	attachCLIAgentModelDiscovery(context.Background(), "claudecode", detectedCLIAgent{Detected: true, Path: "/bin/claude"}, usage, "", now)
	var fable *cliAgentModelDetail
	for i := range usage.ModelDetails {
		if usage.ModelDetails[i].ID == claudeModelFable {
			fable = &usage.ModelDetails[i]
		} else if usage.ModelDetails[i].Pool != "" {
			t.Fatalf("%s carries pool %q, want none", usage.ModelDetails[i].ID, usage.ModelDetails[i].Pool)
		}
	}
	if fable == nil || fable.Pool != claudeModelFable {
		t.Fatalf("fable detail = %#v, want pool %q", fable, claudeModelFable)
	}

	// The cached discovery is re-annotated per gather from THAT gather's
	// snapshot: a snapshot without the Fable window reports no pool.
	bare := &cliAgentUsage{Provider: "claudeCode"}
	attachCLIAgentModelDiscovery(context.Background(), "claudecode", detectedCLIAgent{Detected: true, Path: "/bin/claude"}, bare, "", now)
	for _, model := range bare.ModelDetails {
		if model.Pool != "" {
			t.Fatalf("%s carries pool %q without a metered window", model.ID, model.Pool)
		}
	}
}

func TestBoundedModelDetail_DropsAnOverLongPool(t *testing.T) {
	long := strings.Repeat("p", cliUsageMaxModelPoolLength+1)
	got := boundedModelDetail(cliAgentModelDetail{ID: "m", Pool: long})
	if got.Pool != "" {
		t.Fatalf("pool = %q, want dropped", got.Pool)
	}
	ok := boundedModelDetail(cliAgentModelDetail{ID: "m", Pool: strings.Repeat("p", cliUsageMaxModelPoolLength)})
	if len(ok.Pool) != cliUsageMaxModelPoolLength {
		t.Fatalf("a pool at the bound must survive: %q", ok.Pool)
	}
}

func TestCLIUsageReceiptCarriesModelPool(t *testing.T) {
	agent := cliAgentUsage{
		Provider: "claudeCode", CollectedAt: "now",
		ModelDetails: []cliAgentModelDetail{
			{ID: "fable", Label: "Fable", Efforts: []string{"low", "medium", "high"}, Pool: "fable"},
			{ID: "opus", Label: "Opus", Efforts: []string{"low", "medium", "high"}},
		},
		ModelsExhaustive: authBoolPtr(false),
		Metrics: []cliAgentUsageMetric{
			{Kind: limitKindWeekly, Label: "Weekly Fable", Unit: "%", Model: "fable", Unknown: true},
		},
	}
	canonical, _, _, err := canonicalCLIUsageRefreshReceipt("r5", 5, true, []cliAgentUsage{agent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// `pool` is the LAST detail key — the Go struct order the verifier mirrors.
	want := `"modelDetails":[{"id":"fable","label":"Fable","efforts":["low","medium","high"],"pool":"fable"},{"id":"opus","label":"Opus","efforts":["low","medium","high"]}],"modelsExhaustive":false`
	if !strings.Contains(string(canonical), want) {
		t.Fatalf("canonical = %s", canonical)
	}

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
	if len(vectors.Vectors) != 5 {
		t.Fatal("expected five shared vectors")
	}
	vector := vectors.Vectors[4]
	if vector.Name != "model-details-with-pool" || string(canonical) != vector.Canonical {
		t.Fatalf("shared vector %q canonical mismatch: %s", vector.Name, canonical)
	}
	signature, _, _, err := signCLIUsageRefreshReceipt("secret", "r5", 5, true, []cliAgentUsage{agent}, nil)
	if err != nil || signature != vector.Signature {
		t.Fatalf("shared vector %q signature mismatch: %s (%v)", vector.Name, signature, err)
	}

	long := agent
	long.ModelDetails = []cliAgentModelDetail{{ID: "x", Pool: strings.Repeat("p", cliUsageMaxModelPoolLength+1)}}
	if _, _, _, err := canonicalCLIUsageRefreshReceipt("r5", 5, true, []cliAgentUsage{long}, nil); err == nil {
		t.Fatal("an over-long pool must be rejected")
	}
}
