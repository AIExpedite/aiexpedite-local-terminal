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
	finish := startAntigravityQuotaCapture("test run").Finish
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

	first := startAntigravityQuotaCapture("run one").Finish
	stopped := antigravityCaptureStopped()
	second := startAntigravityQuotaCapture("run two").Finish
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

	short := startAntigravityQuotaCapture("short run").Finish
	long := startAntigravityQuotaCapture("long run").Finish
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

	finish := startAntigravityQuotaCapture("immediate run").Finish
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

	finish := startAntigravityQuotaCapture("late-server run").Finish
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

	finish := startAntigravityQuotaCapture("blippy run").Finish
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

	finish := startAntigravityQuotaCapture("memo run").Finish
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

	finish := startAntigravityQuotaCapture("restart run").Finish
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

	finish := startAntigravityQuotaCapture("capped run").Finish

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

	finish := startAntigravityQuotaCapture("redaction run").Finish
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

	finish := startAntigravityQuotaCapture("tail run").Finish
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

	finish := startAntigravityQuotaCapture("no-server run").Finish
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
	release := armAntigravityCaptureForCommand("test", "git", []string{"status"}).Finish
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
	}).Finish
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
	// The log index, its candidate floors and the in-memory exhaustion
	// evidence are process-global too.
	resetAntigravityLogIndex()
	resetAntigravityExhaustionEvidence()
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
		finish := startAntigravityQuotaCapture("gated run").Finish
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
				finish := startAntigravityQuotaCapture("build check").Finish
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
		finish := startAntigravityQuotaCapture("stamp run").Finish
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
		finish := startAntigravityQuotaCapture("unprobed run").Finish
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

// The handle contract: a non-agy command's handle is nil and every method is a
// no-op; Finish is idempotent and stops a running wrapper resolver.
func TestAntigravityRunCapture_HandleContract(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	var none *antigravityRunCapture
	none.SetPID(1)
	none.SetWrapper(1)
	none.Finish()
	if armAntigravityCaptureForCommand("test", "git", []string{"status"}) != nil {
		t.Error("a non-agy command got a capture handle")
	}

	origScan, origEvery := antigravityAncestryScan, antigravityWrapperScanEvery
	defer func() { antigravityAncestryScan, antigravityWrapperScanEvery = origScan, origEvery }()
	var scans atomic.Int64
	antigravityAncestryScan = func(int) ([]ProcessInfo, bool) { scans.Add(1); return nil, true }
	antigravityWrapperScanEvery = 5 * time.Millisecond

	capture := startAntigravityQuotaCapture("contract run")
	capture.SetWrapper(100)
	time.Sleep(30 * time.Millisecond)
	helperStopCapture(t, capture.Finish)
	capture.Finish()
	if got := antigravityCaptureFinishes.Load(); got != 1 {
		t.Errorf("finishes=%d, want Finish idempotent", got)
	}
	after := scans.Load()
	time.Sleep(30 * time.Millisecond)
	if scans.Load() != after || after == 0 {
		t.Errorf("scans=%d then %d, want the resolver to have run and stopped at Finish", after, scans.Load())
	}
}

// The wrapper resolver takes the first agy in the wrapper's tree that started
// after the capture floor, retains it, marks it managed and stops scanning.
func TestAntigravityRunCapture_ResolvesTheWrappedAgy(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	origScan, origEvery := antigravityAncestryScan, antigravityWrapperScanEvery
	defer func() { antigravityAncestryScan, antigravityWrapperScanEvery = origScan, origEvery }()
	antigravityWrapperScanEvery = 5 * time.Millisecond
	var scans atomic.Int64
	antigravityAncestryScan = func(root int) ([]ProcessInfo, bool) {
		scans.Add(1)
		if root != 300 {
			return nil, false
		}
		return []ProcessInfo{
			{PID: 301, ParentPID: 300, Name: "powershell.exe", StartTime: time.Now()},
			{PID: 302, ParentPID: 300, Name: "agy.exe", StartTime: time.Now().Add(-time.Hour)}, // before the floor: a persistent host's older child
			{PID: 303, ParentPID: 301, Name: "agy.exe", StartTime: time.Now().Add(time.Second)},
		}, true
	}
	capture := startAntigravityQuotaCapture("wrapped run")
	capture.SetWrapper(300)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		pid := capture.pid
		capture.mu.Unlock()
		if pid != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	capture.mu.Lock()
	pid := capture.pid
	capture.mu.Unlock()
	if pid != 303 {
		t.Errorf("resolved pid=%d, want the agy started after the floor (303)", pid)
	}
	stopped := scans.Load()
	time.Sleep(30 * time.Millisecond)
	if scans.Load() != stopped {
		t.Error("the resolver kept scanning after resolving")
	}
	lockAntigravityLogIndex()
	managed := antigravityPIDIn(antigravityLogIndex.managedPIDs, 303, "", time.Time{})
	unlockAntigravityLogIndex()
	if !managed {
		t.Error("the resolved agy was not marked managed")
	}
	helperStopCapture(t, capture.Finish)
}

// The resolver's budget is bounded: at most antigravityWrapperMaxScans scans.
func TestAntigravityRunCapture_ResolverBudget(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperResolverSchedule(t, time.Millisecond, time.Millisecond)
	origScan, origMax := antigravityAncestryScan, antigravityWrapperMaxScans
	defer func() { antigravityAncestryScan, antigravityWrapperMaxScans = origScan, origMax }()
	antigravityWrapperMaxScans = 4
	var scans atomic.Int64
	antigravityAncestryScan = func(int) ([]ProcessInfo, bool) { scans.Add(1); return nil, true }
	capture := startAntigravityQuotaCapture("unresolved run")
	capture.SetWrapper(400)
	time.Sleep(50 * time.Millisecond)
	helperStopCapture(t, capture.Finish)
	if got := scans.Load(); got != 4 {
		t.Errorf("scans=%d, want the budget of 4", got)
	}
}

func helperResolverSchedule(t *testing.T, ramp, every time.Duration) {
	t.Helper()
	origRamp, origEvery := antigravityWrapperRampEvery, antigravityWrapperScanEvery
	t.Cleanup(func() { antigravityWrapperRampEvery, antigravityWrapperScanEvery = origRamp, origEvery })
	antigravityWrapperRampEvery, antigravityWrapperScanEvery = ramp, every
}

// A wrapped agy that fails fast is over long before one steady interval. The
// resolver scans at once and through its opening ramp, so it still sees agy
// alive — here only on the third scan — without waiting a steady interval.
func TestAntigravityRunCapture_ResolverRampCatchesAFastRun(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperResolverSchedule(t, 10*time.Millisecond, time.Hour)
	origScan := antigravityAncestryScan
	defer func() { antigravityAncestryScan = origScan }()
	var scans atomic.Int64
	antigravityAncestryScan = func(int) ([]ProcessInfo, bool) {
		if scans.Add(1) < 3 {
			return []ProcessInfo{{PID: 700, Name: "powershell.exe"}}, true
		}
		return []ProcessInfo{{PID: 700, Name: "powershell.exe"}, {PID: 701, ParentPID: 700, Name: "agy.exe"}}, true
	}
	capture := startAntigravityQuotaCapture("fast wrapped run")
	capture.SetWrapper(700)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		pid := capture.pid
		capture.mu.Unlock()
		if pid == 701 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	capture.mu.Lock()
	pid := capture.pid
	capture.mu.Unlock()
	if pid != 701 {
		t.Errorf("resolved pid=%d after %d scans, want the ramp to resolve the fast agy (701)", pid, scans.Load())
	}
	helperStopCapture(t, capture.Finish)
}

// A persistent host can hold a detached agy an earlier command started in the
// same second as this run's floor. Subsecond creation times keep it out: the
// resolver takes only the descendant created after the floor itself.
func TestAntigravityRunCapture_ResolverSkipsSameSecondEarlierDescendant(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	origScan, origEvery, origNow := antigravityAncestryScan, antigravityWrapperScanEvery, antigravityUsageFreshnessNow
	defer func() {
		antigravityAncestryScan, antigravityWrapperScanEvery, antigravityUsageFreshnessNow = origScan, origEvery, origNow
	}()
	antigravityWrapperScanEvery = 5 * time.Millisecond
	floor := time.Now().Truncate(time.Second).Add(500 * time.Millisecond)
	antigravityUsageFreshnessNow = func() time.Time { return floor }
	antigravityAncestryScan = func(int) ([]ProcessInfo, bool) {
		return []ProcessInfo{
			{PID: 900, Name: "powershell.exe"},
			{PID: 901, ParentPID: 900, Name: "agy.exe", StartTime: floor.Add(-200 * time.Millisecond)},
			{PID: 902, ParentPID: 900, Name: "agy.exe", StartTime: floor.Add(200 * time.Millisecond)},
		}, true
	}

	capture := startAntigravityQuotaCapture("persistent host")
	capture.SetWrapper(900)
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for pid == 0 && time.Now().Before(deadline) {
		capture.mu.Lock()
		pid = capture.pid
		capture.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if pid != 902 {
		t.Errorf("resolved pid=%d, want 902 (created after the floor), not the same-second 901", pid)
	}
	helperStopCapture(t, capture.Finish)
}

// A spawn site hands SetStarted the child it started: a direct `agy` is the
// run's PID at once, with no scan; a shell wrapper is walked, and a Unix shell
// that exec'd agy in place resolves to the wrapper PID itself.
func TestAntigravityRunCapture_SetStartedDirectAndWrapped(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	origScan, origEvery := antigravityAncestryScan, antigravityWrapperScanEvery
	defer func() { antigravityAncestryScan, antigravityWrapperScanEvery = origScan, origEvery }()
	antigravityWrapperScanEvery = 5 * time.Millisecond
	var scans atomic.Int64
	antigravityAncestryScan = func(root int) ([]ProcessInfo, bool) {
		scans.Add(1)
		switch root {
		case 500: // `bash -c "cd x && agy …"`: the shell forks agy
			return []ProcessInfo{{PID: 500, Name: "bash"}, {PID: 501, ParentPID: 500, Name: "agy"}}, true
		case 600: // `bash -c "agy …"`: the shell execs agy in place
			return []ProcessInfo{{PID: 600, Name: "agy"}}, true
		case 700: // `powershell -c "antigravity …"`: the supported alias
			return []ProcessInfo{{PID: 700, Name: "powershell.exe"}, {PID: 701, ParentPID: 700, Name: "Antigravity.exe"}}, true
		}
		return nil, true
	}
	pidOf := func(c *antigravityRunCapture) int {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			c.mu.Lock()
			pid := c.pid
			c.mu.Unlock()
			if pid != 0 {
				return pid
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0
	}

	direct := startAntigravityQuotaCapture("direct run")
	direct.SetStarted("/usr/local/bin/agy", 400)
	if got := pidOf(direct); got != 400 || scans.Load() != 0 {
		t.Errorf("direct agy pid=%d scans=%d, want 400 with no scan", got, scans.Load())
	}
	helperStopCapture(t, direct.Finish)

	for _, tc := range []struct{ root, want int }{{500, 501}, {600, 600}, {700, 701}} {
		wrapped := startAntigravityQuotaCapture("wrapped run")
		wrapped.SetStarted("bash", tc.root)
		if got := pidOf(wrapped); got != tc.want {
			t.Errorf("wrapper %d resolved pid=%d, want %d", tc.root, got, tc.want)
		}
		lockAntigravityLogIndex()
		wrapperManaged := tc.root != tc.want && antigravityPIDIn(antigravityLogIndex.managedPIDs, tc.root, "", time.Time{})
		unlockAntigravityLogIndex()
		if wrapperManaged {
			t.Errorf("the wrapper shell %d was recorded as the managed agy", tc.root)
		}
		helperStopCapture(t, wrapped.Finish)
	}
}

// The resolver takes agy under either supported name, and nothing that merely
// starts the same way.
func TestIsAntigravityProcessName(t *testing.T) {
	for name, want := range map[string]bool{
		"agy": true, "agy.exe": true, "AGY.EXE": true,
		"antigravity": true, "Antigravity.exe": true,
		"agy-helper.exe": false, "antigravity-updater": false, "powershell.exe": false, "": false,
	} {
		if got := isAntigravityProcessName(name); got != want {
			t.Errorf("isAntigravityProcessName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Every spawn site that can start a wrapper hands the capture the started
// command through SetStarted, so a shell is resolved rather than recorded as
// agy. Only the native path, which always execs agy itself, calls SetPID.
func TestAntigravityCaptureSpawnSitesUseSetStarted(t *testing.T) {
	for _, file := range []string{"session.go", "pty_session_unix.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if !strings.Contains(string(src), ".SetStarted(") {
			t.Errorf("%s no longer hands its started child to SetStarted", file)
		}
		if strings.Contains(string(src), ".SetPID(") {
			t.Errorf("%s calls SetPID directly, recording a possible wrapper as agy", file)
		}
	}
}

// Finish must not hold the command's return path open behind a scan that never
// answers (a wedged WMI/CIM provider): it waits at most the grace, then settles
// the run without the resolver's PID. A PID the stalled scan reports after that
// is dropped — the run is over, and the PID may already be somebody else's.
func TestAntigravityRunCapture_FinishBoundsTheResolverWait(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperResolverSchedule(t, time.Millisecond, time.Millisecond)
	origScan, origGrace := antigravityAncestryScan, antigravityWrapperFinishGrace
	defer func() { antigravityAncestryScan, antigravityWrapperFinishGrace = origScan, origGrace }()
	antigravityWrapperFinishGrace = 50 * time.Millisecond

	release := make(chan struct{})
	scanning := make(chan struct{}, 1)
	antigravityAncestryScan = func(int) ([]ProcessInfo, bool) {
		select {
		case scanning <- struct{}{}:
		default:
		}
		<-release // a provider that never answers
		return []ProcessInfo{{PID: 801, Name: "agy.exe"}}, true
	}
	defer close(release)

	capture := startAntigravityQuotaCapture("wedged scan")
	capture.SetWrapper(800)
	select {
	case <-scanning:
	case <-time.After(5 * time.Second):
		t.Fatal("the resolver never started its first scan")
	}

	returned := make(chan struct{})
	go func() { defer close(returned); capture.Finish() }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Finish blocked on the stalled ancestry scan instead of bounding the wait")
	}

	release <- struct{}{} // let the stalled scan report its PID, too late
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		pid := capture.pid
		capture.mu.Unlock()
		if pid != 0 {
			t.Fatalf("pid=%d recorded after Finish, want the late scan dropped", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A transport that fails over re-roots the resolver. The superseded resolver's
// scan was already in flight; when it returns late with the failed
// transport's agy it must not win over the fallback's.
func TestAntigravityRunCapture_SupersededResolverRecordsNothing(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperResolverSchedule(t, time.Millisecond, time.Millisecond)
	origScan := antigravityAncestryScan
	defer func() { antigravityAncestryScan = origScan }()

	releaseOld := make(chan struct{})
	oldScanning := make(chan struct{}, 1)
	antigravityAncestryScan = func(root int) ([]ProcessInfo, bool) {
		if root == 900 {
			select {
			case oldScanning <- struct{}{}:
			default:
			}
			<-releaseOld
			return []ProcessInfo{{PID: 901, Name: "agy.exe"}}, true
		}
		return nil, true // the fallback's agy is not up yet
	}

	capture := startAntigravityQuotaCapture("failover")
	defer capture.Finish()
	capture.SetWrapper(900)
	select {
	case <-oldScanning:
	case <-time.After(5 * time.Second):
		t.Fatal("the first resolver never started its scan")
	}
	capture.SetWrapper(950) // the transport fell back
	close(releaseOld)

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		capture.mu.Lock()
		pid := capture.pid
		capture.mu.Unlock()
		if pid != 0 {
			t.Fatalf("pid=%d recorded by the superseded resolver, want none", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A transport that fails after its resolver already found agy is retried on a
// new wrapper (persistent-host restart, then the fallback). The re-root must
// drop the failed attempt's PID and resolve the retried agy, or the run that
// actually completes is never marked managed and its evidence is missed.
func TestAntigravityRunCapture_ReRootReplacesAResolvedPID(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperResolverSchedule(t, time.Millisecond, time.Millisecond)
	origScan := antigravityAncestryScan
	defer func() { antigravityAncestryScan = origScan }()
	antigravityAncestryScan = func(root int) ([]ProcessInfo, bool) {
		switch root {
		case 900:
			return []ProcessInfo{{PID: 901, Name: "agy.exe"}}, true
		case 950:
			return []ProcessInfo{{PID: 951, Name: "agy.exe"}}, true
		}
		return nil, true
	}
	waitPID := func(capture *antigravityRunCapture, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			capture.mu.Lock()
			pid := capture.pid
			capture.mu.Unlock()
			if pid == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("capture never resolved pid %d", want)
	}

	capture := startAntigravityQuotaCapture("failover after resolve")
	capture.SetWrapper(900)
	waitPID(capture, 901)
	capture.SetWrapper(950) // the host failed; the command is retried
	waitPID(capture, 951)

	at := time.Now().Add(time.Second)
	lockAntigravityLogIndex()
	retried := antigravityPIDIn(antigravityLogIndex.managedPIDs, 951, "", time.Time{})
	failedLive := antigravityPIDIn(antigravityLogIndex.managedPIDs, 901, "", at)
	unlockAntigravityLogIndex()
	if !retried {
		t.Error("the retried agy was not marked managed")
	}
	if failedLive {
		t.Error("the failed attempt's agy is still held as a live managed run")
	}
	helperStopCapture(t, capture.Finish)
}
