package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// tokenRequestRecorder is a fake /auth/token that records each request body.
type tokenRequestRecorder struct {
	mu       sync.Mutex
	payloads []map[string]any
	arrived  chan struct{}
}

func newTokenRequestRecorder(t *testing.T) (*tokenRequestRecorder, *httptest.Server) {
	t.Helper()
	rec := &tokenRequestRecorder{arrived: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		rec.mu.Lock()
		rec.payloads = append(rec.payloads, payload)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":   "oidc-token",
			"expires_in": 3600,
			"token_type": "Bearer",
		})
		rec.arrived <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (rec *tokenRequestRecorder) snapshot() []map[string]any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]map[string]any(nil), rec.payloads...)
}

func (rec *tokenRequestRecorder) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(rec.snapshot()) < n {
		select {
		case <-rec.arrived:
		case <-deadline:
			t.Fatalf("got %d token requests, want %d", len(rec.snapshot()), n)
		}
	}
}

// isolateMachineInfo clears the process-global cache and the pending re-send so
// each test starts from "agent just launched, first gather still running".
func isolateMachineInfo(t *testing.T) {
	t.Helper()
	if !drainMachineInfoGathers(10 * time.Second) {
		t.Fatal("a background machine-info gather did not drain")
	}
	machineInfoMu.Lock()
	saved := machineInfoCache
	machineInfoCache = nil
	machineInfoMu.Unlock()
	tokenSourceAwaitingMachineInfo.Store(nil)
	savedLast := lastTokenSource.Swap(nil)
	t.Cleanup(func() {
		machineInfoMu.Lock()
		machineInfoCache = saved
		machineInfoMu.Unlock()
		tokenSourceAwaitingMachineInfo.Store(nil)
		lastTokenSource.Store(savedLast)
	})
}

func baselineMachineInfo() *MachineInfo {
	return &MachineInfo{
		Architecture: "amd64",
		CPU:          &cpuInfo{Cores: 8},
		CollectedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

// The first /auth/token after pairing raced the startup gather and went out
// bare, so terminal-service never started the computer's onboarding until the
// next token refresh (prod AIX3, 2026-09-25).
func TestTokenRequestSentBeforeFirstGather_IsResentOnceWithMachineInfo(t *testing.T) {
	isolateMachineInfo(t)
	rec, srv := newTokenRequestRecorder(t)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})

	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	if first := rec.snapshot()[0]; first["cpu"] != nil || first["collectedAt"] != nil {
		t.Fatalf("first request carried machine info before any gather: %v", first)
	}

	storeMachineInfo(baselineMachineInfo())
	rec.waitFor(t, 2)
	resent := rec.snapshot()[1]
	if resent["cpu"] == nil || resent["collectedAt"] == nil {
		t.Fatalf("re-sent request lacks the machine-info baseline: %v", resent)
	}

	// A later gather (the 6h refresh, a readiness gather) must not re-send again.
	storeMachineInfo(baselineMachineInfo())
	time.Sleep(200 * time.Millisecond)
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("token requests=%d after a second gather, want 2 (one re-send only)", got)
	}
}

func TestTokenRequestCarryingMachineInfo_LeavesNothingToResend(t *testing.T) {
	isolateMachineInfo(t)
	rec, srv := newTokenRequestRecorder(t)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})

	// Bare, then with data (e.g. a Pub/Sub token refresh after the gather):
	// the second request already delivered the baseline.
	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	machineInfoMu.Lock()
	machineInfoCache = baselineMachineInfo()
	machineInfoMu.Unlock()
	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	if tokenSourceAwaitingMachineInfo.Load() != nil {
		t.Fatal("a request that carried machine info left a re-send pending")
	}

	storeMachineInfo(baselineMachineInfo())
	time.Sleep(200 * time.Millisecond)
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("token requests=%d, want 2 (no re-send)", got)
	}
}

// Codex P2 on #178: a gather that lands after getOIDCToken reads a nil cache
// but before the re-send is armed found nothing pending, and the bare request
// was never repeated. The re-send is armed before the read now.
func TestGatherLandingRightAfterBareRead_StillResends(t *testing.T) {
	isolateMachineInfo(t)
	rec, srv := newTokenRequestRecorder(t)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})

	testHookAfterMachineInfoRead = func() {
		testHookAfterMachineInfoRead = nil
		storeMachineInfo(baselineMachineInfo())
	}
	t.Cleanup(func() { testHookAfterMachineInfoRead = nil })

	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	rec.waitFor(t, 2)
	var carried bool
	for _, p := range rec.snapshot() {
		if p["cpu"] != nil && p["collectedAt"] != nil {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("no token request carried machine info after the gather landed: %v", rec.snapshot())
	}
}

// Codex P2 on #219: a token request that already carried machine info clears
// the bare-request arm, so a capability added to the cache afterwards (the
// follow-up ledger becoming owned once an overlapping process exits) was never
// re-sent. The capability-added path repeats the request unconditionally.
func TestCapabilityAddedAfterAFullRequest_IsResent(t *testing.T) {
	isolateMachineInfo(t)
	rec, srv := newTokenRequestRecorder(t)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})

	info := baselineMachineInfo()
	info.Capabilities = &capabilitiesInfo{RelayTurnInbox: true}
	machineInfoMu.Lock()
	machineInfoCache = info
	machineInfoMu.Unlock()
	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	if tokenSourceAwaitingMachineInfo.Load() != nil {
		t.Fatal("a request that carried machine info left a re-send pending")
	}

	updated := *info
	updated.Capabilities = &capabilitiesInfo{
		RelayTurnInbox: true,
		RelayFollowUps: &relayFollowUpsCapability{AttemptExpiry: true, LedgerID: "ledger-1"},
	}
	machineInfoMu.Lock()
	machineInfoCache = &updated
	machineInfoMu.Unlock()
	onMachineCapabilityAdded()

	rec.waitFor(t, 2)
	caps, _ := rec.snapshot()[1]["capabilities"].(map[string]any)
	followUps, _ := caps["relayFollowUps"].(map[string]any)
	if followUps["ledgerId"] != "ledger-1" {
		t.Fatalf("re-sent request lacks the added capability: %v", rec.snapshot()[1])
	}
}

// Before any token request there is nothing to repeat: the first request reads
// the updated cache itself.
func TestCapabilityAddedBeforeAnyRequest_SendsNothing(t *testing.T) {
	isolateMachineInfo(t)
	rec, _ := newTokenRequestRecorder(t)
	onMachineCapabilityAdded()
	time.Sleep(200 * time.Millisecond)
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("token requests=%d, want 0", got)
	}
}

// Codex P2 on #219: the capability re-send ran concurrently with an older
// in-flight request that had snapshotted the capability-less cache, so the
// older one could land last and overwrite the update. Requests on a token
// source are serialized now: the re-send goes out only after the older request
// completes, and it reads the updated cache.
func TestCapabilityResend_WaitsForTheInFlightOlderRequest(t *testing.T) {
	isolateMachineInfo(t)
	var (
		mu       sync.Mutex
		payloads []map[string]any
	)
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	completed := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		payloads = append(payloads, payload)
		n := len(payloads)
		mu.Unlock()
		if n == 1 {
			close(firstArrived)
			<-releaseFirst
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "oidc-token", "expires_in": 3600, "token_type": "Bearer"})
		completed <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(payloads)
	}

	info := baselineMachineInfo()
	info.Capabilities = &capabilitiesInfo{RelayTurnInbox: true}
	machineInfoMu.Lock()
	machineInfoCache = info
	machineInfoMu.Unlock()
	go func() {
		if _, err := ts.getOIDCToken(); err != nil {
			t.Errorf("getOIDCToken failed: %v", err)
		}
	}()
	select {
	case <-firstArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the first token request never arrived")
	}

	updated := *info
	updated.Capabilities = &capabilitiesInfo{
		RelayTurnInbox: true,
		RelayFollowUps: &relayFollowUpsCapability{AttemptExpiry: true, LedgerID: "ledger-1"},
	}
	machineInfoMu.Lock()
	machineInfoCache = &updated
	machineInfoMu.Unlock()
	onMachineCapabilityAdded()

	time.Sleep(200 * time.Millisecond)
	if got := count(); got != 1 {
		t.Fatalf("token requests=%d while the older request was in flight, want 1 (re-send must wait)", got)
	}
	close(releaseFirst)
	for i := 0; i < 2; i++ {
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d token requests completed, want 2", i)
		}
	}
	mu.Lock()
	last := payloads[len(payloads)-1]
	mu.Unlock()
	caps, _ := last["capabilities"].(map[string]any)
	followUps, _ := caps["relayFollowUps"].(map[string]any)
	if followUps["ledgerId"] != "ledger-1" {
		t.Fatalf("the request that completed last lacks the added capability: %v", last)
	}
}
