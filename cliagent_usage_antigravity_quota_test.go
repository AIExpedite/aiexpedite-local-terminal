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
	"sync"
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

// A payload whose windows we cannot map is not an observation: treating it as
// one would overwrite the last usable cached reading with all-Unknown rows the
// moment the provider renames a window.
func TestFetchAntigravityQuota_UnrecognizedWindowsAreNotASnapshot(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-fortnightly","window":"fortnightly","remainingFraction":0.5,"resetTime":"2126-08-14T00:00:00Z"}]}]}}`,
		helperStatusJSON)

	if _, ok := fetchAntigravityQuota(context.Background(), base, time.Now()); ok {
		t.Errorf("a response with no plottable bucket must not count as an observation")
	}
}

// The quota RPC can succeed while GetUserStatus does not. That reading is fine
// to DISPLAY (it came from the server about to run the work) but must not be
// cached: we cannot say whose pool it is, and an unscoped snapshot would be
// replayed under the next unidentified account.
func TestAntigravityUsageParser_DoesNotCacheQuotaWithoutServerIdentity(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	helperAntigravityServer(t, base, helperQuotaJSON, "") // GetUserStatus 500s
	// A stale identity from a previous login, exactly what must NOT be adopted.
	helperWriteJSON(t, filepath.Join(base, "settings.json"), map[string]any{
		"email": "previous-login@example.com",
	})

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 3 {
		t.Errorf("the live reading should still display, got %+v", usage.Metrics)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Errorf("an unattributable reading must not be cached")
	}
	// settings.json's account is from a previous login and cannot be assumed to
	// own the quota the server just reported — publishing under it would attach
	// this reading to that account's fingerprint across every device.
	if usage.Account != "" || usage.AccountFingerprint != "" {
		t.Errorf("account/fingerprint=%q/%q, want unattributed",
			usage.Account, usage.AccountFingerprint)
	}
}

// Even a cache left by an older build must not be replayed unscoped.
func TestLoadAntigravityQuotaSnapshot_RefusesUnscopedCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	body, _ := json.Marshal(antigravityQuotaSnapshot{
		ObservedAt: "2026-08-11T09:00:00Z",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4},
		},
	})
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if _, ok := loadAntigravityQuotaSnapshot(""); ok {
		t.Errorf("an unscoped snapshot must never be replayed")
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

func TestAntigravityQuotaMetrics_CapsSignedRefreshMetrics(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	snap := antigravityQuotaSnapshot{ObservedAt: now.Format(time.RFC3339)}
	for i := 0; i < cliUsageMaxMetricsPerProvider+2; i++ {
		snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{
			Group:             fmt.Sprintf("Group %02d Models", i),
			Window:            "weekly",
			RemainingFraction: 0.5,
			ResetTime:         "2026-08-19T00:00:00Z",
		})
	}

	metrics := antigravityQuotaMetrics(snap, now)
	if len(metrics) != cliUsageMaxMetricsPerProvider {
		t.Fatalf("rendered %d metrics, want signed-refresh cap %d", len(metrics), cliUsageMaxMetricsPerProvider)
	}
	if metrics[len(metrics)-1].Label != "Group 31 — Weekly quota" {
		t.Fatalf("last retained metric = %q, want stable first %d entries", metrics[len(metrics)-1].Label, cliUsageMaxMetricsPerProvider)
	}
	if _, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1, true, []cliAgentUsage{{Provider: "antigravity", Metrics: metrics}}, nil); err != nil {
		t.Fatalf("bounded Antigravity metrics rejected from signed refresh: %v", err)
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

// settings.json usually names nobody, so scoping the replay by the current
// fingerprint alone would leave the between-runs cache permanently unreachable.
// Replay under the producer's identity instead — the card then names the account
// the reading belongs to.
func TestAntigravityUsageParser_ReplaysCacheUnderItsProducerWhenSettingsHasNoIdentity(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	fingerprint := fingerprintAccount("antigravity", "ada@example.com")
	body, _ := json.Marshal(antigravityQuotaSnapshot{
		ObservedAt:         "2026-08-11T09:00:00Z",
		AccountFingerprint: fingerprint,
		Account:            "ada@example.com",
		Plan:               "Pro",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	})
	if err := os.WriteFile(cache, body, 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	// No settings.json at all: the account lives in the OS keyring.
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Fatalf("expected the cached bucket to plot, got %+v", usage.Metrics)
	}
	if usage.Account != "ada@example.com" || usage.AccountFingerprint != fingerprint {
		t.Errorf("account/fingerprint=%q/%q, want the producing identity",
			usage.Account, usage.AccountFingerprint)
	}
	if usage.Plan != "Pro" {
		t.Errorf("Plan=%q, want the producer's plan", usage.Plan)
	}
	if usage.Metrics[0].ObservedAt != "2026-08-11T09:00:00Z" {
		t.Errorf("ObservedAt=%q, want the original observation", usage.Metrics[0].ObservedAt)
	}
}

// A settings file that names a DIFFERENT account still blocks the replay: the
// scoped load fails and the producer fallback is only for an unknown identity.
func TestAntigravityUsageParser_DoesNotReplayProducerCacheUnderAConflictingAccount(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	body, _ := json.Marshal(antigravityQuotaSnapshot{
		ObservedAt:         "2026-08-11T09:00:00Z",
		AccountFingerprint: fingerprintAccount("antigravity", "someone-else@example.com"),
		Account:            "someone-else@example.com",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2126-08-14T00:00:00Z"},
		},
	})
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
	if usage.Account != "ada@example.com" {
		t.Errorf("Account=%q, want the settings identity to stand", usage.Account)
	}
}

// A long-running session's log must not be slurped whole, and the port has to
// survive being logged at the head of a file whose tail is far away.
func TestDiscoverAntigravityHTTPPorts_ReadsBoundedHeadAndTail(t *testing.T) {
	base := t.TempDir()
	filler := strings.Repeat("I0811 12:00:00.000000 42 chatter: noise noise noise\n", 12000)
	helperWriteAntigravityLog(t, base, "cli-big.log",
		"server.go:584] Language server listening on random port at 44441 for HTTP\n"+
			filler+
			"server.go:584] Language server listening on random port at 44442 for HTTP\n")

	info, err := os.Stat(filepath.Join(antigravityLogDir(base), "cli-big.log"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() <= 2*antigravityLogScanBytes {
		t.Fatalf("fixture must exceed the scan window, got %d bytes", info.Size())
	}

	ports := discoverAntigravityHTTPPorts(base)
	if len(ports) != 2 {
		t.Fatalf("ports=%v, want both the startup and restart ports", ports)
	}
	// The restart (tail) is the newer listener and must be probed first.
	if ports[0] != 44442 || ports[1] != 44441 {
		t.Errorf("ports=%v, want [44442 44441]", ports)
	}
}

// A bucket with an absent or null remainingFraction decodes to 0 in a plain
// float64 — i.e. "100% consumed". It is not a reading and must not count as an
// observation, or a malformed payload would replace a good cached one.
func TestFetchAntigravityQuota_SkipsBucketsWithoutARemainingFraction(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-weekly","window":"weekly","resetTime":"2126-08-14T00:00:00Z"},
	  {"bucketId":"gemini-5h","window":"5h","remainingFraction":null,"resetTime":"2126-08-12T04:37:41Z"}]}]}}`,
		helperStatusJSON)

	if _, ok := fetchAntigravityQuota(context.Background(), base, time.Now()); ok {
		t.Errorf("buckets with no usable fraction must not count as an observation")
	}
}

// An out-of-range fraction is equally unusable.
func TestFetchAntigravityQuota_SkipsOutOfRangeFraction(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":42,"resetTime":"2126-08-14T00:00:00Z"}]}]}}`,
		helperStatusJSON)

	if _, ok := fetchAntigravityQuota(context.Background(), base, time.Now()); ok {
		t.Errorf("a fraction outside 0..1 must not be plotted")
	}
}

// A valid bucket alongside a malformed one still reads, carrying only the usable
// row into the cache.
func TestFetchAntigravityQuota_KeepsValidBucketsBesideMalformedOnes(t *testing.T) {
	base := t.TempDir()
	helperAntigravityServer(t, base, `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-weekly","window":"weekly","resetTime":"2126-08-14T00:00:00Z"},
	  {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"resetTime":"2126-08-12T04:37:41Z"}]}]}}`,
		helperStatusJSON)

	snap, ok := fetchAntigravityQuota(context.Background(), base, time.Now())
	if !ok {
		t.Fatalf("expected the usable bucket to yield a snapshot")
	}
	if len(snap.Buckets) != 1 || snap.Buckets[0].Window != "5h" {
		t.Errorf("buckets=%+v, want only the 5h row", snap.Buckets)
	}
}

// Concurrent savers must never expose a partial cache: a shared temp name lets
// one writer truncate bytes another is about to rename into place.
func TestSaveAntigravityQuotaSnapshot_ConcurrentWritesStayIntact(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			saveAntigravityQuotaSnapshot(antigravityQuotaSnapshot{
				ObservedAt:         "2026-08-11T09:00:00Z",
				AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
				Account:            "ada@example.com",
				Buckets: []antigravityQuotaBucket{
					{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4,
						ResetTime: "2126-08-14T00:00:00Z"},
				},
			})
		}(i)
	}
	wg.Wait()

	var persisted antigravityQuotaSnapshot
	if !readJSONFile(cache, &persisted) {
		t.Fatalf("cache must be readable after concurrent writes")
	}
	if len(persisted.Buckets) != 1 {
		t.Errorf("persisted=%+v, want one intact bucket", persisted)
	}
	// No temp files may survive.
	matches, _ := filepath.Glob(cache + ".tmp.*")
	if len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}
}

// A legacy ~/.agy install keeps its logs under ~/.agy/log. Quota discovery must
// follow the install that is actually in use, not the modern path.
func TestAntigravityUsageParser_DiscoversQuotaFromLegacyInstall(t *testing.T) {
	home := t.TempDir()
	legacyBase := filepath.Join(home, ".agy")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", filepath.Join(t.TempDir(), "agyq.json"))
	// Legacy config selects the ~/.agy tree; its log names the live server.
	helperWriteJSON(t, filepath.Join(legacyBase, "config.json"), map[string]any{})
	helperAntigravityServer(t, legacyBase, helperQuotaJSON, helperStatusJSON)

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if usage.DataSource != "~/.agy" {
		t.Errorf("DataSource=%q, want ~/.agy", usage.DataSource)
	}
	if len(usage.Metrics) != 3 {
		t.Fatalf("expected the legacy install's live rows, got %+v", usage.Metrics)
	}
	if usage.Account != "ada@example.com" {
		t.Errorf("Account=%q, want the server identity", usage.Account)
	}
}

// The reverse case: a modern install whose settings.json does not exist yet must
// still be probed, rather than the parser giving up on the configured tree.
func TestAntigravityUsageParser_DiscoversQuotaWithoutASettingsFile(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", filepath.Join(t.TempDir(), "agyq.json"))
	helperAntigravityServer(t, base, helperQuotaJSON, helperStatusJSON)

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true},
		time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatalf("Parse failed")
	}
	if len(usage.Metrics) != 3 {
		t.Fatalf("expected live rows without a settings file, got %+v", usage.Metrics)
	}
}
