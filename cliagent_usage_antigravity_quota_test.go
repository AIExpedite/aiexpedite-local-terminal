package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperAntigravityServer stands up a loopback stub answering the two RPCs the
// real language server exposes, and writes a CLI log naming its port — the exact
// discovery path the parser uses on a real machine.
func helperAntigravityServer(t *testing.T, base, quotaJSON, statusJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(antigravityQuotaRPC, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaJSON))
	})
	mux.HandleFunc(antigravityStatusRPC, func(w http.ResponseWriter, r *http.Request) {
		if statusJSON == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	helperWriteAntigravityLog(t, base, "cli-20260811_120000.log", fmt.Sprintf(
		"I0811 12:00:00.000000 42 server.go:584] Language server listening on random port at %s for HTTP\n",
		port,
	))
	return srv
}

func helperWriteAntigravityLog(t *testing.T, base, name, body string) {
	t.Helper()
	dir := antigravityLogDir(base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const helperQuotaJSON = `{"response":{"groups":[
  {"displayName":"Gemini Models","buckets":[
    {"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":0.75,"resetTime":"2126-08-14T14:17:13Z"},
    {"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":0.9,"resetTime":"2126-08-12T04:37:41Z"}]},
  {"displayName":"Claude and GPT models","buckets":[
    {"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":1,"resetTime":"2126-08-19T01:11:09Z"}]}]}}`

const helperStatusJSON = `{"userStatus":{"name":"Ada Lovelace","email":"ada@example.com",
  "planStatus":{"planInfo":{"planName":"Pro"}}}}`

func TestFetchAntigravityQuota_ReadsLoopbackServerDiscoveredFromLogs(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, helperQuotaJSON, helperStatusJSON)

	now := time.Date(2026, 8, 11, 12, 5, 0, 0, time.UTC)
	snap, ok := fetchAntigravityQuota(context.Background(), base, now)
	if !ok {
		t.Fatalf("expected a quota snapshot")
	}
	if len(snap.Buckets) != 3 {
		t.Fatalf("len(buckets)=%d, want 3", len(snap.Buckets))
	}
	if snap.Account != "ada@example.com" || snap.Plan != "Pro" {
		t.Errorf("account/plan=%q/%q, want ada@example.com/Pro", snap.Account, snap.Plan)
	}
	if snap.ObservedAt != now.UTC().Format(time.RFC3339) {
		t.Errorf("ObservedAt=%q, want the fetch time", snap.ObservedAt)
	}
}

func TestFetchAntigravityQuota_NoServerRunning(t *testing.T) {
	base := t.TempDir()
	// A log naming a port nothing is listening on — the normal state between runs.
	helperWriteAntigravityLog(t, base, "cli-old.log",
		"server.go:584] Language server listening on random port at 1 for HTTP\n")

	if _, ok := fetchAntigravityQuota(context.Background(), base, time.Now()); ok {
		t.Errorf("expected no snapshot when nothing answers")
	}
}

func TestFetchAntigravityQuota_NoLogsIsNotAnError(t *testing.T) {
	if _, ok := fetchAntigravityQuota(context.Background(), t.TempDir(), time.Now()); ok {
		t.Errorf("expected no snapshot without logs")
	}
}

// An empty quota response must not be treated as a successful read: overwriting
// the cache with it would erase a good reading whenever a signed-out server answers.
func TestFetchAntigravityQuota_EmptyGroupsIsNotASnapshot(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, `{"response":{"groups":[]}}`, helperStatusJSON)

	if _, ok := fetchAntigravityQuota(context.Background(), base, time.Now()); ok {
		t.Errorf("an empty quota payload must not count as an observation")
	}
}

func TestDiscoverAntigravityHTTPPorts_PrefersNewestLogAndLastLine(t *testing.T) {
	base := t.TempDir()
	helperWriteAntigravityLog(t, base, "cli-old.log",
		"server.go:584] Language server listening on random port at 11111 for HTTP\n")
	time.Sleep(20 * time.Millisecond)
	helperWriteAntigravityLog(t, base, "cli-new.log",
		"server.go:576] Language server listening on random port at 22221 for HTTPS (gRPC)\n"+
			"server.go:584] Language server listening on random port at 22222 for HTTP\n"+
			"server.go:584] Language server listening on random port at 33333 for HTTP\n")

	ports := discoverAntigravityHTTPPorts(base)
	if len(ports) < 3 {
		t.Fatalf("ports=%v, want at least 3 candidates", ports)
	}
	if ports[0] != 33333 {
		t.Errorf("ports[0]=%d, want the newest log's last restart", ports[0])
	}
	for _, p := range ports {
		if p == 22221 {
			t.Errorf("the HTTPS/gRPC port must not be probed over plain HTTP: %v", ports)
		}
	}
}

// The transport refuses anything off loopback, so a doctored log naming a remote
// host can never turn a local quota read into an outbound request.
func TestAntigravityLoopbackClient_RefusesNonLoopback(t *testing.T) {
	client := antigravityLoopbackClient()
	req, err := http.NewRequest(http.MethodPost, "http://93.184.216.34:80"+antigravityQuotaRPC, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := client.Do(req); err == nil {
		t.Errorf("expected the loopback guard to refuse a public address")
	}
}

func TestAntigravityQuotaMetrics_OrdersSessionBeforeWeeklyWithinGroup(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	snap := antigravityQuotaSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.75, ResetTime: "2026-08-14T00:00:00Z"},
			{Group: "Gemini Models", Window: "5h", RemainingFraction: 0.9, ResetTime: "2026-08-11T16:00:00Z"},
			{Group: "Claude and GPT models", Window: "weekly", RemainingFraction: 1, ResetTime: "2026-08-19T00:00:00Z"},
		},
	}

	metrics := antigravityQuotaMetrics(snap, now)
	if len(metrics) != 3 {
		t.Fatalf("len(metrics)=%d, want 3", len(metrics))
	}
	want := []string{
		"Gemini — 5-hour session window",
		"Gemini — Weekly quota",
		"Claude and GPT — Weekly quota",
	}
	for i, label := range want {
		if metrics[i].Label != label {
			t.Errorf("metrics[%d].Label=%q, want %q", i, metrics[i].Label, label)
		}
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed < 9.99 || *metrics[0].Consumed > 10.01 {
		t.Errorf("Consumed=%v, want ~10 (1 - remainingFraction)", metrics[0].Consumed)
	}
}

func TestAntigravityQuotaMetrics_RolledOverWindowIsUnobservable(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	snap := antigravityQuotaSnapshot{
		ObservedAt: now.Add(-6 * time.Hour).Format(time.RFC3339),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "5h", RemainingFraction: 0.02, ResetTime: "2026-08-11T09:00:00Z"},
		},
	}

	m := antigravityQuotaMetrics(snap, now)[0]
	if !m.Unknown || m.Consumed != nil || m.ResetAt != "" {
		t.Errorf("a refilled window must plot nothing, got %+v", m)
	}
	if m.ObservedAt == "" {
		t.Errorf("observation time must survive so the card can age the row")
	}
}

func TestAntigravityQuotaMetrics_SkipsUnknownWindow(t *testing.T) {
	now := time.Now()
	snap := antigravityQuotaSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "fortnightly", RemainingFraction: 0.5},
		},
	}
	if metrics := antigravityQuotaMetrics(snap, now); len(metrics) != 0 {
		t.Errorf("an unrecognized window must not be plotted under a guessed kind: %+v", metrics)
	}
}

// Between `agy` runs the card replays the cached reading with its ORIGINAL
// observation time, so the age shown is the age of the provider's figure.
func TestAntigravityUsageParser_ReplaysCachedSnapshotWhenServerIsDown(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	observed := "2026-08-11T09:00:00Z"
	snap := antigravityQuotaSnapshot{
		ObservedAt:         observed,
		AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
		Account:            "ada@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	body, _ := json.Marshal(snap)
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"),
		map[string]any{"email": "ada@example.com"})

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Fatalf("expected the cached bucket to plot, got %+v", usage.Metrics)
	}
	if usage.Metrics[0].ObservedAt != observed {
		t.Errorf("ObservedAt=%q, want the original observation %q", usage.Metrics[0].ObservedAt, observed)
	}
	if usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 60 {
		t.Errorf("Consumed=%v, want 60", usage.Metrics[0].Consumed)
	}
}

// A snapshot captured under a different signed-in account must not be shown as
// this one's — the same scoping rule the Claude and Codex caches apply.
func TestAntigravityUsageParser_IgnoresCacheFromAnotherAccount(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	snap := antigravityQuotaSnapshot{
		ObservedAt:         "2026-08-11T09:00:00Z",
		AccountFingerprint: fingerprintAccount("antigravity", "someone-else@example.com"),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.1, ResetTime: "2126-08-14T00:00:00Z"},
		},
	}
	body, _ := json.Marshal(snap)
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"),
		map[string]any{"email": "ada@example.com"})

	usage, _ := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	for _, m := range usage.Metrics {
		if !m.Unknown {
			t.Errorf("another account's cache must not be plotted: %+v", m)
		}
	}
}

// A live read wins over the cache AND replaces it, so the next offline gather
// replays the newer figure.
func TestAntigravityUsageParser_FreshReadOverwritesCache(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	helperAntigravityServer(t, base, helperQuotaJSON, helperStatusJSON)

	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !ok {
		t.Fatalf("Parse failed")
	}
	if usage.Account != "ada@example.com" || usage.Plan != "Pro" {
		t.Errorf("account/plan=%q/%q, want the language server's identity", usage.Account, usage.Plan)
	}
	if len(usage.Metrics) != 3 {
		t.Fatalf("len(metrics)=%d, want 3 live rows", len(usage.Metrics))
	}

	var persisted antigravityQuotaSnapshot
	if !readJSONFile(cache, &persisted) {
		t.Fatalf("expected the fresh reading to be cached")
	}
	if persisted.AccountFingerprint != fingerprintAccount("antigravity", "ada@example.com") {
		t.Errorf("cache fingerprint=%q, want the account the quota was captured under",
			persisted.AccountFingerprint)
	}
	if len(persisted.Buckets) != 3 {
		t.Errorf("cached buckets=%d, want 3", len(persisted.Buckets))
	}
}
