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

func TestCLIUsageRefreshReceiptRedactsLocalErrorMessages(t *testing.T) {
	canonical, _, normalized, err := canonicalCLIUsageRefreshReceipt("refresh-1", 1, false, nil, []cliAgentUsageError{{
		Provider: "codex", Message: `parse failed at C:\\Users\\private\\credentials.json: token=secret`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || normalized[0].ErrorCategory != cliUsageErrorParseFailed || normalized[0].Message != "" {
		t.Fatalf("unexpected normalized errors: %#v", normalized)
	}
	text := string(canonical)
	if strings.Contains(text, "credentials") || strings.Contains(text, "secret") || strings.Contains(text, "message") {
		t.Fatalf("canonical receipt leaked local error text: %s", text)
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
	if len(vectors.Vectors) != 3 {
		t.Fatal("expected three shared vectors")
	}
	if string(canonical) != vectors.Vectors[0].Canonical {
		t.Fatalf("unexpected canonical bytes: %s", canonical)
	}
	signature, _, _, err := signCLIUsageRefreshReceipt("secret", "r1", 1, true, []cliAgentUsage{{Provider: "codex", CollectedAt: "now", Metrics: []cliAgentUsageMetric{{Kind: "quota", Total: &total, Remaining: &remaining}}}}, nil)
	if err != nil || signature != vectors.Vectors[0].Signature {
		t.Fatalf("unexpected shared-vector signature: %s (%v)", signature, err)
	}

	noticeAgent := []cliAgentUsage{{
		Provider:       "codex",
		CollectedAt:    "now",
		Notice:         "line\u2028separator",
		NoticeSeverity: "warning",
		NoticeURL:      "https://example.test",
	}}
	noticeCanonical, _, _, err := canonicalCLIUsageRefreshReceipt("r2", 2, true, noticeAgent, nil)
	if err != nil || string(noticeCanonical) != vectors.Vectors[1].Canonical {
		t.Fatalf("unexpected notice canonical bytes: %s (%v)", noticeCanonical, err)
	}
	noticeSignature, _, _, err := signCLIUsageRefreshReceipt("secret", "r2", 2, true, noticeAgent, nil)
	if err != nil || noticeSignature != vectors.Vectors[1].Signature {
		t.Fatalf("unexpected notice shared-vector signature: %s (%v)", noticeSignature, err)
	}
}

func TestCLIUsageRefreshResultAlwaysIncludesErrorsArray(t *testing.T) {
	success := true
	payload, err := json.Marshal(resultMsg{
		Type:      "__cli_usage_refresh_result__",
		Success:   &success,
		CliAgents: []cliAgentUsage{},
		Errors:    []cliAgentUsageError{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"errors":[]`) {
		t.Fatalf("successful receipt omitted errors array: %s", payload)
	}
}

func TestSafeCLIUsageErrorCategoryAcceptsClosedEnum(t *testing.T) {
	// Every member of the closed enum must survive canonicalization unchanged.
	// `protocol` is the newest member: before it existed, a device reporting a
	// broken CLI invocation contract had its category collapsed to
	// internal_error, which is why the smoke failure was indistinguishable
	// from a crash in our own collector.
	for _, category := range []string{
		cliUsageErrorProviderTimeout, cliUsageErrorProviderUnavailable,
		cliUsageErrorNotAuthenticated, cliUsageErrorParseFailed,
		cliUsageErrorCollectionFailed, cliUsageErrorReceiptBounds,
		cliUsageErrorProtocol, cliUsageErrorInternal,
	} {
		got := safeCLIUsageErrorCategory(cliAgentUsageError{Provider: "claudeCode", ErrorCategory: category})
		if got != category {
			t.Errorf("safeCLIUsageErrorCategory(%q) = %q, want it preserved", category, got)
		}
	}
	if got := safeCLIUsageErrorCategory(cliAgentUsageError{ErrorCategory: "made_up"}); got != cliUsageErrorInternal {
		t.Errorf("unknown category = %q, want internal_error", got)
	}
}

func TestSafeCLIUsageErrorCategoryInfersProtocolFromLegacyText(t *testing.T) {
	// A producer that predates the closed enum only sends free text. These are
	// the exact phrasings Claude 2.1.x uses when it rejects the invocation.
	for _, message := range []string{
		"Error: --input-format=stream-json requires output-format=stream-json.",
		"claude exited 1 with a protocol violation",
		"unknown option '--input-format'",
	} {
		if got := safeCLIUsageErrorCategory(cliAgentUsageError{Provider: "claudeCode", Message: message}); got != cliUsageErrorProtocol {
			t.Errorf("safeCLIUsageErrorCategory(%q) = %q, want protocol", message, got)
		}
	}
}

func TestCLIUsageRefreshReceiptProtocolErrorCarriesNoMessage(t *testing.T) {
	canonical, _, normalized, err := canonicalCLIUsageRefreshReceipt("r3", 3, false, nil,
		[]cliAgentUsageError{{
			Provider:      "claudeCode",
			ErrorCategory: cliUsageErrorProtocol,
			Message:       "Error: --input-format=stream-json requires output-format=stream-json.",
		}})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 1 || normalized[0].ErrorCategory != cliUsageErrorProtocol || normalized[0].Message != "" {
		t.Fatalf("unexpected normalized errors: %#v", normalized)
	}
	text := string(canonical)
	if strings.Contains(text, "stream-json") || strings.Contains(text, "message") {
		t.Fatalf("canonical receipt leaked the vendor diagnostic: %s", text)
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
	if len(vectors.Vectors) != 3 {
		t.Fatal("expected three shared vectors")
	}
	if text != vectors.Vectors[2].Canonical {
		t.Fatalf("unexpected protocol canonical bytes: %s", text)
	}
	signature, _, _, err := signCLIUsageRefreshReceipt("secret", "r3", 3, false, nil,
		[]cliAgentUsageError{{Provider: "claudeCode", ErrorCategory: cliUsageErrorProtocol}})
	if err != nil || signature != vectors.Vectors[2].Signature {
		t.Fatalf("unexpected protocol shared-vector signature: %s (%v)", signature, err)
	}
}
