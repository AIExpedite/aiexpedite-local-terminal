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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// helperCaptureServer is helperAntigravityServer with two additions the capture
// tests need: a per-RPC hit counter (so "did another attempt happen?" is
// observable without waiting on the one-second resolution of observedAt) and a
// switchable identity, for the GetUserStatus-blip case.
type helperCaptureServer struct {
	srv          *httptest.Server
	quotaHits    atomic.Int64
	statusHits   atomic.Int64
	connections  atomic.Int64
	identityDown atomic.Bool
	port         string
}

func helperStartCaptureServer(t *testing.T, base, quotaJSON, statusJSON string) *helperCaptureServer {
	t.Helper()
	return helperStartCaptureServerFunc(t, base, func() string { return quotaJSON }, statusJSON)
}

// helperStartCaptureServerFunc is helperStartCaptureServer with the quota body
// resolved per request, for the tail-window test: a turn's quota is debited when
// the turn ENDS, so the reading has to be able to change after the run is
// released.
func helperStartCaptureServerFunc(t *testing.T, base string, quotaJSON func() string, statusJSON string) *helperCaptureServer {
	t.Helper()
	cs := &helperCaptureServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(antigravityQuotaRPC, func(w http.ResponseWriter, r *http.Request) {
		cs.quotaHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaJSON()))
	})
	mux.HandleFunc(antigravityStatusRPC, func(w http.ResponseWriter, r *http.Request) {
		cs.statusHits.Add(1)
		if cs.identityDown.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(statusJSON))
	})
	cs.srv = httptest.NewUnstartedServer(mux)
	cs.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			cs.connections.Add(1)
		}
	}
	cs.srv.Start()
	t.Cleanup(cs.srv.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(cs.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	cs.port = port
	helperWriteAntigravityLog(t, base, "cli-capture.log", fmt.Sprintf(
		"I0811 12:00:00.000000 42 server.go:584] Language server listening on random port at %s for HTTP\n", port))
	return cs
}

// helperIsolateAntigravityCapture points HOME, the quota cache, the capture
// tick and the post-release tail window at test-owned locations, and asserts no previous poller is still alive.
// Capture state is process-global by design (one poller for every concurrent
// `agy` run), so these tests must not run in parallel with each other.
func helperIsolateAntigravityCapture(t *testing.T, interval string) (home, cache string) {
	t.Helper()
	select {
	case <-antigravityCaptureStopped():
	default:
		t.Fatal("a capture poller from an earlier test is still running")
	}
	helperStopAntigravityRefreshSchedule()
	antigravityCaptureArms.Store(0)
	antigravityCaptureFinishes.Store(0)
	antigravityCaptureSnapshots.Store(0)
	antigravityCaptureTailProbes.Store(0)
	antigravityCaptureLastPersistedMs.Store(0)
	helperResetAntigravityLiveRuns()
	// The run-completion debt is process-global too, and a run that captures
	// nothing now owes an OUTBOUND Google read. Point the state file at the
	// test's own dir and stub that read, so no test reaches the network or the
	// developer's real agent state, and wait out any worker a test left behind.
	t.Setenv(antigravityFreshnessEnv, filepath.Join(t.TempDir(), "agy_freshness.json"))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistNoLogin })
	// The shipped 5 s retry delay would be paid by every case whose stub keeps
	// the debt unpaid past its first read; the worker reads it, so it is
	// restored only once the worker is out of flight.
	origRetry := antigravityRefreshAfterRunRetryDelay
	antigravityRefreshAfterRunRetryDelay = time.Millisecond
	t.Cleanup(func() {
		helperStopAntigravityRefreshSchedule()
		antigravityRefreshAfterRunRetryDelay = origRetry
	})
	// A debt is retired WITHOUT an attempt when `agy` is not on the machine, so
	// whether a capture test exercises the worker at all would otherwise depend
	// on the developer happening to have the CLI installed — green on a dev box,
	// red on every CI runner. Same seam as helperIsolateAntigravityFreshness; a
	// case that needs `agy` to look GONE overrides PATH itself.
	helperFakeAgyOnPath(t)

	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cache = filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	t.Setenv(antigravityCaptureIntervalEnv, interval)
	// The shipped 2s tail grace would be paid by every helperStopCapture in the
	// suite. The tail's own tests set this back to a value they can reason about.
	t.Setenv(antigravityCaptureTailEnv, "1ms")
	return home, cache
}

// helperStubAntigravityCodeAssistOutcome replaces the Code Assist read the
// run-completion debt worker spends, and reports how many times it ran.
func helperStubAntigravityCodeAssistOutcome(t *testing.T, outcome func() string) *atomic.Int64 {
	t.Helper()
	orig := probeAntigravityQuotaCodeAssistFn
	t.Cleanup(func() { probeAntigravityQuotaCodeAssistFn = orig })
	var calls atomic.Int64
	probeAntigravityQuotaCodeAssistFn = func(context.Context, string, func() time.Time) string {
		calls.Add(1)
		return outcome()
	}
	return &calls
}

// helperFreshnessState reads the persisted run-completion debt.
func helperFreshnessState(t *testing.T) antigravityUsageFreshness {
	t.Helper()
	var state antigravityUsageFreshness
	readJSONFile(antigravityFreshnessPath(), &state)
	return state
}

// helperStopCapture releases a capture and waits for the poller to finish, so
// no goroutine outlives the test.
func helperStopCapture(t *testing.T, finish func()) {
	t.Helper()
	stopped := antigravityCaptureStopped()
	finish()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("capture poller did not stop after the last finish()")
	}
	// The settle runs off the poller (a short run must not wait on a longer
	// one holding it), so the debt it decides lands slightly after shutdown.
	antigravityUsageRefreshWaitIdle()
}

// helperAwaitCaptureStopped asserts a run armed a capture and waits for the
// poller to finish. antigravityCaptureStopped is never nil (an already-closed
// channel stands in before any poller exists), so "was anything armed" is
// asked of the arm counter rather than of the channel.
func helperAwaitCaptureStopped(t *testing.T, stuck, neverArmed string) {
	t.Helper()
	if antigravityCaptureArms.Load() == 0 {
		t.Fatal(neverArmed)
	}
	select {
	case <-antigravityCaptureStopped():
	case <-time.After(30 * time.Second):
		t.Fatal(stuck)
	}
}

// helperAwaitSnapshot polls the cache until a snapshot is present whose
// observedAt is strictly after `after` (zero time = any snapshot).
func helperAwaitSnapshot(t *testing.T, cache string, after time.Time, why string) antigravityQuotaSnapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var snap antigravityQuotaSnapshot
		if readJSONFile(cache, &snap) {
			if observed, err := time.Parse(time.RFC3339, snap.ObservedAt); err == nil && observed.After(after) {
				return snap
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The core of the bug: nothing ever wrote a snapshot observed DURING a run, so
// the post-run refresh could only replay a day-old one.
func TestAntigravityQuotaCapture_WritesSnapshotObservedDuringTheRun(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	// RFC3339 has one-second resolution, so the run window has to be compared
	// against the truncated start instant.
	start := time.Now().UTC().Truncate(time.Second)
	finish := startAntigravityQuotaCapture("test run")
	snap := helperAwaitSnapshot(t, cache, time.Time{}, "the first mid-run capture")
	helperStopCapture(t, finish)
	end := time.Now().UTC()

	observed, err := time.Parse(time.RFC3339, snap.ObservedAt)
	if err != nil {
		t.Fatalf("observedAt=%q is not RFC3339: %v", snap.ObservedAt, err)
	}
	if observed.Before(start) || observed.After(end) {
		t.Errorf("observedAt=%s outside the run window [%s, %s]", observed, start, end)
	}
	if snap.AccountFingerprint != fingerprintAccount("antigravity", "ada@example.com") {
		t.Errorf("fingerprint=%q, want the server-named account's", snap.AccountFingerprint)
	}
	if len(snap.Buckets) != 3 {
		t.Errorf("buckets=%d, want the three plottable rows", len(snap.Buckets))
	}
	if snap.SchemaVersion != antigravityQuotaSchemaVersion {
		t.Errorf("schemaVersion=%d, want %d", snap.SchemaVersion, antigravityQuotaSchemaVersion)
	}
}

// One poller serves every concurrent `agy` run, and it may only stop once the
// LAST of them has released it — otherwise a second turn starting mid-run would
// silently lose its capture.
func TestAntigravityQuotaCapture_SingleFlightUntilLastFinish(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	first := startAntigravityQuotaCapture("run one")
	stopped := antigravityCaptureStopped()
	second := startAntigravityQuotaCapture("run two")
	if antigravityCaptureStopped() != stopped {
		t.Fatal("the second arm started a second poller instead of joining the first")
	}
	helperAwaitSnapshot(t, cache, time.Time{}, "a capture while both runs are armed")

	first()
	// finish() is idempotent: a double release must not drop the refcount below
	// the still-armed second run (nor double-close the stop channel).
	first()
	select {
	case <-stopped:
		t.Fatal("poller stopped while a run was still armed")
	case <-time.After(200 * time.Millisecond):
	}

	helperStopCapture(t, second)
	if got := antigravityCaptureArms.Load(); got != 2 {
		t.Errorf("arms=%d, want 2", got)
	}
	// The doubled release is swallowed by the idempotence guard, so exactly one
	// finish is recorded per armed run.
	if got := antigravityCaptureFinishes.Load(); got != 2 {
		t.Errorf("finishes=%d, want 2 (one per armed run, double release ignored)", got)
	}
	// Both runs had a reading of their own, so neither settle left a debt.
	antigravityUsageRefreshWaitIdle()
	if state := helperFreshnessState(t); state.RefreshOwedAtMs != 0 {
		t.Errorf("state=%+v, want no refresh debt when both runs captured", state)
	}
}

// Settling is PER RUN, not per poller exit. A short run that captured nothing
// must owe its refresh the moment it finishes — even while a second, still
// armed run holds the shared poller open — and the doubled release must not
// settle it twice.
func TestAntigravityQuotaCapture_SettlesOncePerRunNotPerPollerExit(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperIsolateAntigravityGate(t)
	// No server: nothing can capture, so every run finishes owing a refresh.
	reads := helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistNoLogin })

	short := startAntigravityQuotaCapture("short run")
	long := startAntigravityQuotaCapture("long run")
	stopped := antigravityCaptureStopped()
	short()
	short() // idempotent: the second release must not settle a second time

	deadline := time.Now().Add(30 * time.Second)
	for helperFreshnessState(t).RefreshOwedAtMs == 0 {
		if time.Now().After(deadline) {
			helperStopCapture(t, long)
			t.Fatal("the short run's debt waited for the poller to exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("the poller stopped while a run was still armed")
	default:
	}
	antigravityUsageRefreshWaitIdle()
	// no_login stops attempting after one read, so a double settle would show
	// up as a second read of the same debt.
	if got := reads.Load(); got != 1 {
		t.Errorf("reads=%d, want exactly one per settled run", got)
	}
	helperStopCapture(t, long)
}

// The first probe must happen at ARM time, not one interval later. The quota
// server belongs to the child, so a turn that finishes inside a single tick has
// no post-exit second chance — waiting for the first tick is what left
// latestObservedAt stale for fast successful runs.
func TestAntigravityQuotaCapture_ProbesImmediatelyOnArm(t *testing.T) {
	// An hour-long tick guarantees the polling loop never fires, and the
	// assertion runs while the capture is still armed, so the only attempt that
	// can have produced this snapshot is the one taken at arm time.
	home, cache := helperIsolateAntigravityCapture(t, "1h")
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("immediate run")
	snap := helperAwaitSnapshot(t, cache, time.Time{}, "the arm-time probe")
	if snap.AccountFingerprint == "" {
		t.Errorf("arm-time snapshot is not attributable: %+v", snap)
	}
	helperStopCapture(t, finish)
}

// A server that binds after the capture is armed must be discovered on a live
// polling tick. finish() normally runs after Wait, when the in-process server is
// already gone, so correctness must never depend on a post-exit final probe.
func TestAntigravityQuotaCapture_DiscoversServerThatBindsAfterArm(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")

	finish := startAntigravityQuotaCapture("late-server run")
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("nothing was listening at arm time; no snapshot should exist yet")
	}

	snap := helperAwaitSnapshot(t, cache, time.Time{}, "a live probe after the server bound")
	if snap.ObservedAt == "" || snap.AccountFingerprint == "" {
		t.Errorf("live snapshot is not attributable: %+v", snap)
	}
	helperStopCapture(t, finish)
}

// A GetUserStatus blip must cost one tick of freshness, not the whole run's: the
// reading is unattributable so it cannot be cached, but the next tick — on the
// same memoized port — must still succeed.
func TestAntigravityQuotaCapture_RetriesAfterAnIdentityBlip(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	server := helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)
	server.identityDown.Store(true)

	finish := startAntigravityQuotaCapture("blippy run")
	defer helperStopCapture(t, finish)

	// Let several ticks land while identity is down.
	deadline := time.Now().Add(30 * time.Second)
	for server.statusHits.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("poller never reached the identity RPC")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("an unattributable reading must not be cached")
	}

	server.identityDown.Store(false)
	snap := helperAwaitSnapshot(t, cache, time.Time{}, "a capture once identity returned")
	if snap.Account != "ada@example.com" {
		t.Errorf("account=%q, want the recovered server identity", snap.Account)
	}
}

// Log scanning, not the RPC, is what makes a probe expensive. Once a port has
// answered it must be reused for the life of the run — provable by removing the
// logs entirely and watching attempts continue.
func TestAntigravityQuotaCapture_MemoizesThePortAcrossAttempts(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	server := helperStartCaptureServer(t, base, helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("memo run")
	defer helperStopCapture(t, finish)
	helperAwaitSnapshot(t, cache, time.Time{}, "the first capture")

	// Discovery is now impossible: the log no longer advertises any HTTP port.
	// Only the memoized port can keep the probes flowing.
	helperWriteAntigravityLog(t, base, "cli-capture.log",
		"I0811 12:00:01.000000 42 server.go:100] nothing to see here\n")
	before := server.quotaHits.Load()
	deadline := time.Now().Add(30 * time.Second)
	for server.quotaHits.Load() < before+3 {
		if time.Now().After(deadline) {
			t.Fatalf("attempts stopped after the port line vanished (hits %d → %d) — the port was not memoized",
				before, server.quotaHits.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.connections.Load(); got != 1 {
		t.Errorf("poller opened %d TCP connections across repeated probes, want one reused transport", got)
	}
}

// An RPC failure on the memoized port must force re-discovery, which is how a
// mid-run server restart on a new port is picked up.
func TestAntigravityQuotaCapture_RediscoversAfterTheMemoizedPortDies(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	first := helperStartCaptureServer(t, base, helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("restart run")
	defer helperStopCapture(t, finish)
	helperAwaitSnapshot(t, cache, time.Time{}, "the first capture")

	// The first server goes away and a second one comes up on a fresh port,
	// under a different account so the new reading is unambiguous.
	first.srv.Close()
	// The replacement server rewrites the same log in place, so the dead port is
	// no longer discoverable — only the memo (which must fail and clear) and the
	// new line remain.
	helperStartCaptureServer(t, base, helperQuotaJSON,
		`{"userStatus":{"email":"grace@example.com","planStatus":{"planInfo":{"planName":"Ultra"}}}}`)

	deadline := time.Now().Add(30 * time.Second)
	for {
		var snap antigravityQuotaSnapshot
		if readJSONFile(cache, &snap) && snap.Account == "grace@example.com" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture never rediscovered the restarted server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Log discovery is hard-capped, but a known-good port must keep receiving cheap
// reads until the run ends. Otherwise the last observation for a run longer
// than the discovery window could be hours stale when the child exits.
func TestAntigravityQuotaCapture_KeepsPollingMemoizedPortAfterDiscoveryCap(t *testing.T) {
	home, _ := helperIsolateAntigravityCapture(t, "1ms")
	server := helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("capped run")

	// The quota RPC is issued once per tick. Reaching beyond the discovery cap
	// proves the memoized-port tail loop did not park with the process alive.
	deadline := time.Now().Add(90 * time.Second)
	for server.quotaHits.Load() <= antigravityCaptureMaxAttempts+5 {
		if time.Now().After(deadline) {
			t.Fatalf("polling stopped at %d hits; want reads beyond discovery cap %d",
				server.quotaHits.Load(), antigravityCaptureMaxAttempts)
		}
		time.Sleep(5 * time.Millisecond)
	}

	helperStopCapture(t, finish)
	stoppedAt := server.quotaHits.Load()
	time.Sleep(20 * time.Millisecond)
	if got := server.quotaHits.Load(); got != stoppedAt {
		t.Errorf("polling continued after finish: hits %d → %d", stoppedAt, got)
	}
}

// Everything persisted must stay inside the allowlist: no discovered port, no
// log text, no settings/config fields.
func TestAntigravityQuotaCapture_PersistsOnlyTheAllowlistedFields(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	server := helperStartCaptureServer(t, base, helperQuotaJSON, helperStatusJSON)
	// A settings file whose contents must never reach the cache.
	helperWriteJSON(t, filepath.Join(base, "settings.json"), map[string]any{
		"email":       "previous-login@example.com",
		"apiKey":      "sk-super-secret",
		"mcpServers":  map[string]any{"local": "http://127.0.0.1:9999"},
		"workspaceId": "ws-123",
	})

	finish := startAntigravityQuotaCapture("redaction run")
	helperAwaitSnapshot(t, cache, time.Time{}, "a capture to inspect")
	helperStopCapture(t, finish)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	for _, forbidden := range []string{server.port, "sk-super-secret", "mcpServers", "ws-123",
		"previous-login@example.com", "listening on random port"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("persisted cache leaked %q:\n%s", forbidden, raw)
		}
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("cache is not a JSON object: %v", err)
	}
	allowedTop := map[string]bool{
		"schemaVersion": true, "observedAt": true, "observedAtMs": true, "accountFingerprint": true,
		"account": true, "plan": true, "buckets": true,
	}
	for key := range decoded {
		if !allowedTop[key] {
			t.Errorf("unexpected persisted field %q", key)
		}
	}
	var buckets []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["buckets"], &buckets); err != nil {
		t.Fatalf("buckets are not objects: %v", err)
	}
	allowedBucket := map[string]bool{
		"bucketId": true, "group": true, "displayName": true,
		"window": true, "remainingFraction": true, "resetTime": true,
	}
	for _, bucket := range buckets {
		for key := range bucket {
			if !allowedBucket[key] {
				t.Errorf("unexpected persisted bucket field %q", key)
			}
		}
	}
}

// The tick override is a TEST seam, not an operator knob: garbage must fall back
// to the shipped interval rather than producing a hot loop or a stalled poller.
func TestAntigravityCapturePollIntervalValue_RejectsUnusableOverrides(t *testing.T) {
	for _, raw := range []string{"", "nonsense", "0s", "-5s"} {
		t.Setenv(antigravityCaptureIntervalEnv, raw)
		if got := antigravityCapturePollIntervalValue(); got != antigravityCapturePollInterval {
			t.Errorf("override %q → %v, want the default %v", raw, got, antigravityCapturePollInterval)
		}
	}
	t.Setenv(antigravityCaptureIntervalEnv, "25ms")
	if got := antigravityCapturePollIntervalValue(); got != 25*time.Millisecond {
		t.Errorf("override 25ms → %v", got)
	}
}

// The quota a turn spends is debited at the END of the turn, and the execute
// path releases the capture the instant Wait returns — while the language
// server is still up and still answering. Without the post-release tail window,
// the reading the card shows is the one taken BEFORE the turn's own debit
// landed, so a successful run could still publish an under-reported pool.
func TestAntigravityQuotaCapture_TailProbesAfterLastFinish(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "1h")
	t.Setenv(antigravityCaptureTailEnv, "3s")
	base := filepath.Join(home, ".gemini", "antigravity-cli")

	var quotaJSON atomic.Value
	quotaJSON.Store(helperQuotaJSON)
	server := helperStartCaptureServerFunc(t, base, func() string {
		return quotaJSON.Load().(string)
	}, helperStatusJSON)

	finish := startAntigravityQuotaCapture("tail run")
	helperAwaitSnapshot(t, cache, time.Time{}, "the arm-time capture")

	// observedAt has one-second resolution, so the release instant has to fall
	// in a strictly later second than every in-run probe for the assertion below
	// to be able to tell them apart.
	time.Sleep(1100 * time.Millisecond)
	// The turn's debit lands only now, as the run is released — the real-world
	// ordering this window exists for.
	quotaJSON.Store(helperQuotaJSONDebited)
	releaseAt := time.Now().UTC().Truncate(time.Second)
	hitsBeforeRelease := server.quotaHits.Load()

	helperStopCapture(t, finish)
	if elapsed := time.Since(releaseAt); elapsed > 3*time.Second+2*time.Second {
		t.Errorf("poller took %s to exit after the last finish — the tail window is not bounded", elapsed)
	}

	var snap antigravityQuotaSnapshot
	if !readJSONFile(cache, &snap) {
		t.Fatal("no snapshot after the tail window")
	}
	observed, err := time.Parse(time.RFC3339, snap.ObservedAt)
	if err != nil {
		t.Fatalf("tail observedAt=%q is not RFC3339: %v", snap.ObservedAt, err)
	}
	// Every in-run probe happened strictly before releaseAt, so a reading at or
	// after it can only have been taken by the tail.
	if observed.Before(releaseAt) {
		t.Errorf("observedAt=%s predates the release at %s — nothing probed after finish()", observed, releaseAt)
	}
	if got := helperBucketFraction(t, snap, "gemini-5h"); got != 0.6 {
		t.Errorf("gemini-5h remainingFraction=%v, want the post-turn debited 0.6 — the tail reading was not the one persisted", got)
	}
	if got := antigravityCaptureTailProbes.Load(); got == 0 {
		t.Error("tailProbes=0, want at least one probe after the last release")
	}
	if got := server.quotaHits.Load(); got <= hitsBeforeRelease {
		t.Errorf("quotaHits=%d, unchanged from %d at release — the server saw no tail probe", got, hitsBeforeRelease)
	}
}

// helperBucketFraction returns one bucket's remaining fraction, so a test can
// assert WHICH reading was persisted rather than only that one was.
func helperBucketFraction(t *testing.T, snap antigravityQuotaSnapshot, bucketID string) float64 {
	t.Helper()
	for _, b := range snap.Buckets {
		if b.BucketID == bucketID {
			return b.RemainingFraction
		}
	}
	t.Fatalf("bucket %q missing from snapshot %+v", bucketID, snap)
	return 0
}

// The tail window is cheap reads of an ALREADY-KNOWN port. A run that never
// found a server must not start scanning logs at the very moment it is being
// torn down — that is the expensive half of a probe, paid on the teardown path,
// for a server that by construction is not there.
func TestAntigravityQuotaCapture_TailProbeNeverRediscovers(t *testing.T) {
	_, cache := helperIsolateAntigravityCapture(t, "1h")
	t.Setenv(antigravityCaptureTailEnv, "3s")
	// No server, and no log advertising one: the arm-time probe finds nothing
	// and no port is ever memoized.

	finish := startAntigravityQuotaCapture("no-server run")
	released := time.Now()
	helperStopCapture(t, finish)

	if elapsed := time.Since(released); elapsed > 2*time.Second {
		t.Errorf("release took %s — the tail ran despite there being no memoized port", elapsed)
	}
	if got := antigravityCaptureTailProbes.Load(); got != 0 {
		t.Errorf("tailProbes=%d, want 0 with no memoized port", got)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Error("a snapshot was written with no server up")
	}
}

// Every spawn site arms through armAntigravityCaptureForCommand rather than
// pairing its own classifier check with startAntigravityQuotaCapture. A command
// that never starts `agy` must arm nothing, and releasing that no-op must be
// safe — a site that has to remember a nil check is a site that will forget one.
func TestArmAntigravityCaptureForCommand_NoOpForOtherCommands(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")

	// Capture state is process-global, so an earlier test's finished poller may
	// still be the retained handle. Only a CHANGE means a poller was started.
	before := antigravityCaptureStopped()
	release := armAntigravityCaptureForCommand("test", "git", []string{"status"})
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 for a command that never spawns agy", got)
	}
	if antigravityCaptureStopped() != before {
		t.Error("a poller was started for a non-agy command")
	}
	release()
	release() // idempotent, like the real release
	if got := antigravityCaptureFinishes.Load(); got != 0 {
		t.Errorf("finishes=%d, want 0 — nothing was armed", got)
	}
}

// The matching half of the same contract: a wrapped Windows payload arms
// exactly once and releases exactly once.
func TestArmAntigravityCaptureForCommand_ArmsWrappedAgyOnce(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")

	release := armAntigravityCaptureForCommand("test", "powershell.exe", []string{
		"-NoProfile", "-NonInteractive", "-EncodedCommand",
		encodeForPowerShell(`Set-Location C:\tmp; & 'C:\t\agy.cmd' -p "hi"`),
	})
	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Fatalf("arms=%d, want exactly one for a wrapped agy payload", got)
	}
	helperStopCapture(t, release)
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want exactly one", got)
	}
}

// helperResetAntigravityLiveRuns drops the armed-run registry. It is
// process-global, and a test that arms a floor without settling it would
// otherwise pin the next test's crash marker.
func helperResetAntigravityLiveRuns() {
	antigravityLiveRunsMu.Lock()
	antigravityLiveRuns = map[int64]int{}
	antigravityLiveRunsMu.Unlock()
}

// A build the gate marker already knows refuses loopback reads gets NO poller:
// every probe would be a log scan for a read guaranteed to be refused. The run
// still arms its floor and still settles gated, so the Code Assist route that
// does answer on such a build pays it.
func TestAntigravityQuotaCapture_GatedBuildStartsNoPoller(t *testing.T) {
	home, _ := helperIsolateAntigravityCapture(t, "20ms")
	helperIsolateAntigravityGate(t)
	noteAntigravityQuotaGate("", time.Now().Add(-time.Minute))
	// A live-looking server whose port the log names: a poller would find it.
	server := helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"), helperQuotaJSON, helperStatusJSON)
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	logged := captureStdout(t, func() {
		finish := startAntigravityQuotaCapture("gated run")
		antigravityCaptureMu.Lock()
		refs := antigravityCaptureRefs
		antigravityCaptureMu.Unlock()
		if refs != 0 {
			t.Errorf("refs=%d, want no poller joined for a gated build", refs)
		}
		helperStopCapture(t, finish)
	})

	if got := antigravityCaptureArms.Load(); got != 1 {
		t.Errorf("arms=%d, want the run armed once", got)
	}
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want the run released once", got)
	}
	if got := server.quotaHits.Load() + server.statusHits.Load(); got != 0 {
		t.Errorf("loopback RPCs=%d, want none on a gated build", got)
	}
	if got := antigravityCaptureTailProbes.Load(); got != 0 {
		t.Errorf("tailProbes=%d, want none", got)
	}
	// The poller's close-out line reports its discovery attempts; no poller,
	// no line — and so no discovery at all.
	if strings.Contains(logged, "Capture finished for gated run") {
		t.Errorf("a poller ran for a gated build: %q", logged)
	}
	state := helperFreshnessState(t)
	if state.RefreshOwedAtMs == 0 || !state.Gated {
		t.Errorf("state=%+v, want the run's debt owed and marked gated", state)
	}
	// antigravityCaptureStopped stays answerable with no poller armed.
	select {
	case <-antigravityCaptureStopped():
	case <-time.After(5 * time.Second):
		t.Error("antigravityCaptureStopped hung with no poller armed")
	}
}

// helperInstalledAgy puts an `agy` binary on PATH and, when version is
// non-empty, records it in the version-probe cache as the card's detection
// would have — the run path reads that cache and never spawns --version.
func helperInstalledAgy(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	if version != "" {
		cachedProbeVersionFunc(antigravityExecutablePath(), func() string { return version })
	}
}

// The run path compares the marker with the installed build: a versioned
// marker skips the poller only for that same build. A self-update (a new
// version, or a binary changed since its last probe) gets its first re-probe
// instead of riding the old build's refusal for up to the recheck window.
func TestAntigravityQuotaCapture_GateMarkerIsPerInstalledBuild(t *testing.T) {
	for _, tc := range []struct {
		name       string
		marker     string
		installed  string
		wantPolled bool
	}{
		{name: "same build stays gated", marker: "1.2.2", installed: "1.2.2", wantPolled: false},
		{name: "an upgraded build is re-probed", marker: "1.2.2", installed: "1.3.0", wantPolled: true},
		{name: "an unprobed build is re-probed", marker: "1.2.2", installed: "", wantPolled: true},
		{name: "an unversioned marker covers whatever is installed", marker: "", installed: "1.3.0", wantPolled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperIsolateAntigravityCapture(t, "20ms")
			helperIsolateAntigravityGate(t)
			helperInstalledAgy(t, tc.installed)
			noteAntigravityQuotaGate(tc.marker, time.Now().Add(-time.Minute))
			helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

			captureStdout(t, func() {
				finish := startAntigravityQuotaCapture("build check")
				antigravityCaptureMu.Lock()
				polled := antigravityCaptureRefs == 1
				antigravityCaptureMu.Unlock()
				if polled != tc.wantPolled {
					t.Errorf("poller started=%v, want %v", polled, tc.wantPolled)
				}
				helperStopCapture(t, finish)
			})
		})
	}
}

// A refusal the poller records is stamped with the installed build when the
// card has probed it, so a later upgrade is not covered by it.
func TestAntigravityQuotaCapture_PollerStampsTheInstalledBuild(t *testing.T) {
	home, _ := helperIsolateAntigravityCapture(t, "20ms")
	gatePath := helperIsolateAntigravityGate(t)
	helperInstalledAgy(t, "1.2.2")
	_, hits := helperGatedAntigravityServer(t, filepath.Join(home, ".gemini", "antigravity-cli"))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	captureStdout(t, func() {
		finish := startAntigravityQuotaCapture("stamp run")
		deadline := time.Now().Add(10 * time.Second)
		for hits.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		helperStopCapture(t, finish)
	})

	var gate antigravityQuotaGate
	if !readJSONFile(gatePath, &gate) {
		t.Fatal("the poller recorded no refusal")
	}
	if gate.Version != "1.2.2" {
		t.Errorf("marker version=%q, want the installed build 1.2.2", gate.Version)
	}
}

// A refusal on a build the card has not probed (cold cache, or a binary changed
// since its probe) persists nothing: an unversioned marker would cover every
// later build for the recheck window. The poller still parks for this run.
func TestAntigravityQuotaCapture_UnprobedBuildPersistsNoMarker(t *testing.T) {
	home, _ := helperIsolateAntigravityCapture(t, "20ms")
	gatePath := helperIsolateAntigravityGate(t)
	helperInstalledAgy(t, "")
	_, hits := helperGatedAntigravityServer(t, filepath.Join(home, ".gemini", "antigravity-cli"))
	helperStubAntigravityCodeAssistOutcome(t, func() string { return liveProbeOutcomeCodeAssistHTTPError })

	var atRefusal int64
	logged := captureStdout(t, func() {
		finish := startAntigravityQuotaCapture("unprobed run")
		deadline := time.Now().Add(10 * time.Second)
		for hits.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		atRefusal = hits.Load()
		time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms
		if extra := hits.Load() - atRefusal; extra > 1 {
			t.Errorf("%d further RPCs after the refusal, want the poller parked", extra)
		}
		helperStopCapture(t, finish)
	})

	if atRefusal == 0 {
		t.Fatal("the poller never reached the gated server")
	}
	if !strings.Contains(logged, "refuses loopback quota reads") {
		t.Errorf("the refusal was not logged: %q", logged)
	}
	if _, err := os.Stat(gatePath); !os.IsNotExist(err) {
		t.Errorf("an unprobed build's refusal wrote a marker (stat err=%v)", err)
	}
}
