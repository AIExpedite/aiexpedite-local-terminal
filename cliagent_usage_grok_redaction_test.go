// cliagent_usage_grok_redaction_test.go
// -----------------------------------------------------------------------------
// The two places Grok's local file contents could cross to Pub/Sub:
//
//  1. persistGrokManagedBillingSnapshot, which APPENDS into the user's own
//     provider-owned unified.jsonl, and
//  2. canonicalCLIUsageRefreshReceipt, which signs the gathered snapshot.
//
// Both are allowlist-only by construction. These tests fail if either one ever
// starts carrying a credential, raw config, prompt text, or any non-metric log
// field across that boundary.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every sentinel below is planted somewhere inside the isolated Grok home. None
// may appear in the persistent log, the published usage, or the signed receipt.
var grokRedactionSentinels = []string{
	"credential-sentinel",
	"refresh-token-sentinel",
	"api-key-sentinel",
	"prompt-sentinel",
	"tool-result-sentinel",
	"raw-config-sentinel",
}

func assertNoGrokSentinels(t *testing.T, what string, body []byte) {
	t.Helper()
	for _, sentinel := range grokRedactionSentinels {
		if strings.Contains(string(body), sentinel) {
			t.Fatalf("%s leaked %q: %s", what, sentinel, body)
		}
	}
}

func TestGrokManagedBillingMergeAndReceiptCarryOnlyAllowlistedFields(t *testing.T) {
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := t.TempDir()
	t.Setenv("GROK_HOME", persistent)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	// A realistic isolated home: credentials, a config carrying an api_key, and
	// a log full of prompts and tool-result telemetry around the one record we
	// are allowed to read.
	helperWriteJSON(t, filepath.Join(isolated, "auth.json"), map[string]any{
		"user_id":       "acct-1",
		"access_token":  "credential-sentinel",
		"refresh_token": "refresh-token-sentinel",
	})
	if err := os.WriteFile(filepath.Join(isolated, "config.toml"),
		[]byte("[model]\napi_key = \"api-key-sentinel\"\nraw = \"raw-config-sentinel\"\n"), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated, `{"ts":"2026-08-19T11:57:00Z","msg":"chat: user turn","ctx":{"prompt":"prompt-sentinel"}}`)
	helperAppendGrokLogLine(t, isolated,
		`{"ts":"2026-08-19T11:58:00Z","msg":"billing: fetched credits config",`+
			`"access_token":"credential-sentinel","prompt":"prompt-sentinel",`+
			`"ctx":{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2126-08-24T22:28:32Z"},`+
			`"rawConfig":"raw-config-sentinel"},"subscriptionTier":"SuperGrok",`+
			`"toolResult":{"text":"tool-result-sentinel"}}}`)
	helperAppendGrokLogLine(t, isolated, `{"ts":"2026-08-19T11:59:00Z","msg":"tool: result","ctx":{"text":"tool-result-sentinel"}}`)

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, false)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingPersisted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingPersisted)
	}

	merged, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	assertNoGrokSentinels(t, "the merged provider log", merged)
	// Positively pin the shape too: only the normalized record may be written.
	if !strings.Contains(string(merged), grokBillingLogMessage) ||
		!strings.Contains(string(merged), "USAGE_PERIOD_TYPE_WEEKLY") {
		t.Fatalf("the normalized record was not merged: %s", merged)
	}
	for _, forbidden := range []string{"rawConfig", "toolResult", "access_token", "prompt"} {
		if strings.Contains(string(merged), forbidden) {
			t.Fatalf("merged log carried a non-allowlisted key %q: %s", forbidden, merged)
		}
	}

	usage, ok := grokUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatal("Parse failed")
	}
	if usage.Metrics[0].ObservedAt != "2026-08-19T11:58:00Z" {
		t.Fatalf("ObservedAt = %q, want the merged observation", usage.Metrics[0].ObservedAt)
	}
	published, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	assertNoGrokSentinels(t, "the published cliAgentUsage", published)

	// The signed body is the last boundary: nothing local may ride along, and a
	// diagnostic message attached to an error must not become signed content.
	body, _, _, err := canonicalCLIUsageRefreshReceipt("refresh-1", now.UnixMilli(), true,
		[]cliAgentUsage{*usage},
		[]cliAgentUsageError{{Provider: "grok", Message: "credential-sentinel"}})
	if err != nil {
		t.Fatalf("canonical receipt: %v", err)
	}
	assertNoGrokSentinels(t, "the canonical receipt body", body)
	// Guard against a vacuous assertion: the grok provider must actually be in
	// the signed body for its absence of sentinels to mean anything.
	if !strings.Contains(string(body), "grok") {
		t.Fatalf("the signed body does not include the grok provider: %s", body)
	}
}

// The run-completion refresh debt (cliagent_usage_grok_freshness.go) is a
// third surface: a state file on disk and device log lines. A full debt cycle
// against a loopback endpoint — one failed read whose body carries a sentinel,
// then a paid one — must leave only integers, the hashed fingerprint and closed
// codes in the file, and only fixed labels and counters in the log.
func TestGrokRunDebtStateAndLogsCarryOnlyMetrics(t *testing.T) {
	home := isolateGrok(t)
	grokEnableRunRefresh(t)
	email := "ada-sentinel@example.com"
	writeGrokAuth(t, home, "credential-sentinel", email, time.Now().Add(5*time.Hour))
	failFirst := true
	grokBillingServer(t, func(string) (int, string) {
		if failFirst {
			failFirst = false
			return http.StatusInternalServerError, `{"error":"tool-result-sentinel prompt-sentinel"}`
		}
		return http.StatusOK, strings.Replace(grokFixtureBody(time.Now().Add(72*time.Hour)),
			`"isUnifiedBillingUser":true`, `"isUnifiedBillingUser":true,"rawConfig":"raw-config-sentinel"`, 1)
	})

	var midCycle []byte
	logged := captureStdout(t, func() {
		floor := armGrokUsageRunFloor(time.Now())
		grokUsageRunSettled(floor)
		grokUsageRefreshWaitFor(10 * time.Second)
		midCycle, _ = os.ReadFile(grokUsageFreshnessPath())
		grokStartRunDebtWorker(true)
		grokUsageRefreshWaitFor(10 * time.Second)
	})

	if len(midCycle) == 0 {
		t.Fatal("no debt state was written after the failed read")
	}
	var fields map[string]any
	if err := json.Unmarshal(midCycle, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"schemaVersion": true, "runFloorMs": true, "completionMs": true, "owedAtMs": true,
		"nextAttemptAtMs": true, "lastAttemptAtMs": true, "attempts": true,
		"accountFingerprint": true, "runFloorAccount": true, "lastOutcome": true,
	}
	for key := range fields {
		if !allowed[key] {
			t.Errorf("state file carries a non-allowlisted field %q", key)
		}
	}
	if fields["lastOutcome"] != grokLiveOutcomeHTTPError {
		t.Errorf("lastOutcome = %v, want the closed code", fields["lastOutcome"])
	}
	final, _ := os.ReadFile(grokUsageFreshnessPath())
	for what, body := range map[string][]byte{"state file": midCycle, "final state file": final, "device log": []byte(logged)} {
		assertNoGrokSentinels(t, what, body)
		for _, secret := range []string{email, "user-" + email, home, filepath.ToSlash(home)} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("%s leaked %q", what, secret)
			}
		}
	}
	if !strings.Contains(logged, "grok refresh: ok") {
		t.Fatalf("the paid read was not logged as its closed outcome: %q", logged)
	}
}
