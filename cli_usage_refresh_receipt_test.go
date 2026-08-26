package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

func TestCLIUsageRefreshReceiptDeterministicAndBounded(t *testing.T) {
	agents := []cliAgentUsage{{Provider: "z", CliAgentID: "2"}, {Provider: "a", CliAgentID: "1", Name: "Codex", Version: "1.2.3"}}
	one, normalized, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1700000000000, true, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1700000000000, true, []cliAgentUsage{agents[1], agents[0]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if one != two || normalized[0].Provider != "a" {
		t.Fatalf("receipt is not deterministic")
	}
	if strings.Contains(one, "refresh-1") || len(one) != 64 {
		t.Fatalf("receipt leaked challenge or has wrong length")
	}
	if _, _, _, err := signCLIUsageRefreshReceipt("secret", strings.Repeat("x", 257), 1, true, nil, nil); err == nil {
		t.Fatal("expected bounds rejection")
	}
}

func TestCLIUsageRefreshReceiptDomainSeparation(t *testing.T) {
	receipt, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1700000000000, false, nil, []cliAgentUsageError{{Provider: "codex", Message: "unavailable"}})
	if err != nil || receipt == "" {
		t.Fatalf("failure receipt: %v", err)
	}
	canonical, _, _, _ := canonicalCLIUsageRefreshReceipt("refresh-1", 1700000000000, false, nil, []cliAgentUsageError{{Provider: "codex", Message: "unavailable"}})
	if !strings.HasPrefix(string(canonical), cliUsageReceiptDomain) {
		t.Fatal("missing domain")
	}
}

func TestCLIUsageRefreshReceiptFormatsMetricsAsDecimalStrings(t *testing.T) {
	total := 1e-7
	remaining := math.Copysign(0, -1)
	canonical, _, _, err := canonicalCLIUsageRefreshReceipt("r1", 1, true, []cliAgentUsage{{Provider: "codex", CollectedAt: "now", Metrics: []cliAgentUsageMetric{{Kind: "quota", Total: &total, Remaining: &remaining}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/cli_usage_refresh_receipt_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Vectors []struct{ Canonical, Signature string } `json:"vectors"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Vectors) != 1 {
		t.Fatal("expected one shared vector")
	}
	if string(canonical) != vectors.Vectors[0].Canonical {
		t.Fatalf("unexpected canonical bytes: %s", canonical)
	}
	signature, _, _, err := signCLIUsageRefreshReceipt("secret", "r1", 1, true, []cliAgentUsage{{Provider: "codex", CollectedAt: "now", Metrics: []cliAgentUsageMetric{{Kind: "quota", Total: &total, Remaining: &remaining}}}}, nil)
	if err != nil || signature != vectors.Vectors[0].Signature {
		t.Fatalf("unexpected shared-vector signature: %s (%v)", signature, err)
	}
}
