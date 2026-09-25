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
	"sync/atomic"
	"testing"
	"time"
)

// Antigravity CLI ≥ 1.2.2 answers every loopback RPC without its per-run CSRF
// token with 401 {"code":"unauthenticated","message":"missing CSRF token"}.
// These pin what the agent does about it: recognise the refusal as a property
// of the build, stop paying for reads it cannot take, keep the last reading
// with its true age, and tell the card why.

const antigravityCSRFRefusal = `{"code":"unauthenticated","message":"missing CSRF token"}`

func helperIsolateAntigravityGate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agy_gate.json")
	t.Setenv(antigravityQuotaGateEnv, path)
	return path
}

// helperGatedAntigravityServer is a language server of a gated build: every
// RPC is refused, and the CLI log names its port exactly like a real run.
func helperGatedAntigravityServer(t *testing.T, base string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(antigravityCSRFRefusal))
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	helperWriteAntigravityLog(t, base, "cli-gated.log", fmt.Sprintf(
		"I0915 09:26:36.273955 98 server.go:1569] Starting language server process with pid 4242\n"+
			"I0915 09:26:36.294274 98 server.go:637] Language server listening on random port at %s for HTTP\n", port))
	return srv, &hits
}

func TestAntigravityPostJSONOutcome_OnlyTheCSRFRefusalIsGated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gated":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(antigravityCSRFRefusal))
		case "/gated-by-message":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"permission_denied","message":"invalid CSRF token"}`))
		case "/plain-401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		case "/ok":
			_, _ = w.Write([]byte(`{"x":1}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)
	client := antigravityLoopbackClient()
	defer client.CloseIdleConnections()

	cases := map[string]antigravityRPCOutcome{
		"/gated":            antigravityRPCGated,
		"/gated-by-message": antigravityRPCGated,
		"/plain-401":        antigravityRPCFailed,
		"/ok":               antigravityRPCOK,
		"/boom":             antigravityRPCFailed,
	}
	for path, want := range cases {
		var out map[string]any
		if got := antigravityPostJSONOutcome(context.Background(), client, port, path, &out); got != want {
			t.Errorf("%s: outcome=%v, want %v", path, got, want)
		}
	}
	// The boolean wrapper keeps its meaning for every existing caller.
	var out map[string]any
	if antigravityPostJSON(context.Background(), client, port, "/gated", &out) {
		t.Error("a refusal must not read as success")
	}
}

func TestAntigravityQuotaGate_AppliesToTheSameBuildUntilRechecked(t *testing.T) {
	helperIsolateAntigravityGate(t)
	now := time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC)
	if _, ok := antigravityQuotaGateFor("1.2.3", now); ok {
		t.Fatal("no marker yet, nothing should be gated")
	}
	noteAntigravityQuotaGate("1.2.3", now)

	if gate, ok := antigravityQuotaGateFor("1.2.3", now.Add(time.Hour)); !ok || gate.Version != "1.2.3" {
		t.Errorf("same build an hour later: gate=%+v ok=%v, want gated", gate, ok)
	}
	if _, ok := antigravityQuotaGateFor("", now.Add(time.Hour)); !ok {
		t.Error("an unknown installed version must still honour the marker (the refusal was seen on whatever is installed)")
	}
	if _, ok := antigravityQuotaGateFor("1.2.4", now.Add(time.Hour)); ok {
		t.Error("a newer build must be tried again")
	}
	if _, ok := antigravityQuotaGateFor("1.2.3", now.Add(antigravityQuotaGateRecheck+time.Minute)); ok {
		t.Error("the marker must expire so a same-version fix heals itself")
	}

	// The poller, which does not know the build, must not erase the version
	// the live probe recorded.
	noteAntigravityQuotaGate("", now.Add(2*time.Hour))
	if gate, ok := antigravityQuotaGateFor("1.2.3", now.Add(3*time.Hour)); !ok || gate.Version != "1.2.3" {
		t.Errorf("after an unversioned note: gate=%+v ok=%v, want the recorded build kept", gate, ok)
	}
	if _, ok := antigravityQuotaGateFor("1.2.4", now.Add(3*time.Hour)); ok {
		t.Error("the kept version must still release a newer build")
	}

	clearAntigravityQuotaGate()
	if _, ok := antigravityQuotaGateFor("1.2.3", now.Add(time.Hour)); ok {
		t.Error("a cleared marker must gate nothing")
	}
}

// TestRunCLIUsageLiveProbes_GatedBuildSkipsTheSpawn: the first click that meets
// the refusal records it; later clicks on the same build spawn no `agy` at all
// (no model turn, no 20 s wait) and still report the outcome.
func TestRunCLIUsageLiveProbes_GatedBuildSkipsTheSpawn(t *testing.T) {
	gatePath := helperIsolateAntigravityGate(t)
	stubLiveProbes(t)
	var agyCalls int32
	probeAntigravityQuotaLiveFn = func(context.Context, string, string) string {
		atomic.AddInt32(&agyCalls, 1)
		return liveProbeOutcomeGated
	}
	resetCooldown := func() {
		cliUsageLiveProbeMu.Lock()
		cliUsageLiveProbeLastDone, cliUsageLiveProbeLast = time.Time{}, nil
		cliUsageLiveProbeMu.Unlock()
	}

	// A gated click reports the Code Assist route's outcome (stubbed here).
	if out := runCLIUsageLiveProbes(context.Background()); out["antigravity"] != liveProbeOutcomeCodeAssistNoLogin {
		t.Fatalf("first click: antigravity=%q, want the Code Assist outcome", out["antigravity"])
	}
	if atomic.LoadInt32(&agyCalls) != 1 {
		t.Fatalf("first click must probe once, got %d", agyCalls)
	}
	if _, err := os.Stat(gatePath); err != nil {
		t.Fatalf("the refusal was not recorded: %v", err)
	}

	resetCooldown()
	if out := runCLIUsageLiveProbes(context.Background()); out["antigravity"] != liveProbeOutcomeCodeAssistNoLogin {
		t.Fatalf("second click: antigravity=%q, want the Code Assist outcome", out["antigravity"])
	}
	if atomic.LoadInt32(&agyCalls) != 1 {
		t.Errorf("second click on a gated build spawned agy again (calls=%d)", agyCalls)
	}

	// A reading — from a build that answers — clears the record.
	clearAntigravityQuotaGate()
	probeAntigravityQuotaLiveFn = func(context.Context, string, string) string {
		atomic.AddInt32(&agyCalls, 1)
		return liveProbeOutcomeOK
	}
	resetCooldown()
	if out := runCLIUsageLiveProbes(context.Background()); out["antigravity"] != liveProbeOutcomeOK {
		t.Fatalf("after clearing: antigravity=%q, want ok", out["antigravity"])
	}
	if _, err := os.Stat(gatePath); !os.IsNotExist(err) {
		t.Errorf("a successful reading must leave no marker (stat err=%v)", err)
	}
}

// TestAntigravityUsageParser_GatedBuildCarriesANoticeAndKeepsTheReading: the
// card keeps the last reading with its original observedAt and gets a banner
// naming the build; a different build gets no banner.
func TestAntigravityUsageParser_GatedBuildCarriesANoticeAndKeepsTheReading(t *testing.T) {
	helperIsolateAntigravityGate(t)
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)

	observed := "2026-09-12T03:04:14Z"
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
		t.Fatal(err)
	}
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"),
		map[string]any{"email": "ada@example.com"})
	now := time.Date(2026, 9, 15, 13, 30, 0, 0, time.UTC)
	noteAntigravityQuotaGate("1.2.3", now.Add(-time.Hour))

	// A finished run on this build owes a refresh
	// (cliagent_usage_antigravity_freshness.go). Its arm sits BELOW the gate's,
	// and a gated debt carries no wording of its own, so the banner must still
	// be the gate's — two sources for one banner would drift.
	t.Setenv(antigravityFreshnessEnv, filepath.Join(t.TempDir(), "agy_freshness.json"))
	helperWriteJSON(t, antigravityFreshnessPath(), antigravityUsageFreshness{
		SchemaVersion:      antigravityFreshnessSchema,
		RefreshOwedFloorMs: now.Add(-time.Minute).UnixMilli(),
		RefreshOwedAtMs:    now.Add(-time.Minute).UnixMilli(),
		Attempts:           antigravityRefreshAfterRunMaxAttempts,
		Gated:              true,
		Outcome:            liveProbeOutcomeCodeAssistHTTPError,
	})

	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "1.2.3"}, now)
	if !ok {
		t.Fatal("Parse failed")
	}
	if usage.NoticeSeverity != "warning" || !strings.Contains(usage.Notice, "1.2.3") || !strings.Contains(usage.Notice, "2026-09-12 03:04") {
		t.Errorf("notice=%q severity=%q, want a warning naming the build and the last reading", usage.Notice, usage.NoticeSeverity)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown || usage.Metrics[0].ObservedAt != observed {
		t.Errorf("metrics=%+v, want the cached reading kept with its original observedAt", usage.Metrics)
	}

	usage, _ = antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "1.2.4"}, now)
	if usage.Notice != "" {
		t.Errorf("a newer build carried the old build's notice: %q", usage.Notice)
	}
}

// TestAntigravityQuotaCapture_StopsAtTheFirstRefusal: a real run on a gated
// build must not scan the CLI's logs for 15 minutes collecting 401s. The first
// refusal records the gate and the poller parks until the run ends.
func TestAntigravityQuotaCapture_StopsAtTheFirstRefusal(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	gatePath := helperIsolateAntigravityGate(t)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	_, hits := helperGatedAntigravityServer(t, base)

	finish := startAntigravityQuotaCapture("test gated run")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(gatePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refusal was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	atGate := hits.Load()
	time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms
	if extra := hits.Load() - atGate; extra > 1 {
		t.Errorf("%d further RPCs after the refusal was recorded, want the poller parked", extra)
	}
	helperStopCapture(t, finish)

	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("a refused read must not write a snapshot (stat err=%v)", err)
	}
	if got := antigravityCaptureSnapshots.Load(); got != 0 {
		t.Errorf("snapshots=%d, want 0", got)
	}
}
