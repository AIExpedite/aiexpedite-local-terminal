package main

import (
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

// helperCaptureServer is helperAntigravityServer with two additions the capture
// tests need: a per-RPC hit counter (so "did another attempt happen?" is
// observable without waiting on the one-second resolution of observedAt) and a
// switchable identity, for the GetUserStatus-blip case.
type helperCaptureServer struct {
	srv          *httptest.Server
	quotaHits    atomic.Int64
	statusHits   atomic.Int64
	identityDown atomic.Bool
	port         string
}

func helperStartCaptureServer(t *testing.T, base, quotaJSON, statusJSON string) *helperCaptureServer {
	t.Helper()
	cs := &helperCaptureServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(antigravityQuotaRPC, func(w http.ResponseWriter, r *http.Request) {
		cs.quotaHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(quotaJSON))
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
	cs.srv = httptest.NewServer(mux)
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

// helperIsolateAntigravityCapture points HOME, the quota cache and the capture
// tick at test-owned locations, and asserts no previous poller is still alive.
// Capture state is process-global by design (one poller for every concurrent
// `agy` run), so these tests must not run in parallel with each other.
func helperIsolateAntigravityCapture(t *testing.T, interval string) (home, cache string) {
	t.Helper()
	if ch := antigravityCaptureStopped(); ch != nil {
		select {
		case <-ch:
		default:
			t.Fatal("a capture poller from an earlier test is still running")
		}
	}
	antigravityCaptureArms.Store(0)
	antigravityCaptureFinishes.Store(0)
	antigravityCaptureSnapshots.Store(0)

	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cache = filepath.Join(t.TempDir(), "agyq.json")
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", cache)
	t.Setenv(antigravityCaptureIntervalEnv, interval)
	return home, cache
}

// helperStopCapture releases a capture and waits for the poller — including its
// exit-grace attempt — to finish, so no goroutine outlives the test.
func helperStopCapture(t *testing.T, finish func()) {
	t.Helper()
	stopped := antigravityCaptureStopped()
	finish()
	if stopped == nil {
		return
	}
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("capture poller did not stop after the last finish()")
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
}

// The acceptance signal ("latestObservedAt advances after run") is carried by
// the exit-grace read for any run too short to tick: the socket can still answer
// for a beat after the process is reaped.
func TestAntigravityQuotaCapture_ExitGraceTakesTheFinalReading(t *testing.T) {
	// An hour-long tick guarantees the polling loop never fires, so the only
	// attempt that can produce this snapshot is the exit-grace one.
	home, cache := helperIsolateAntigravityCapture(t, "1h")
	helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("short run")
	helperStopCapture(t, finish)

	var snap antigravityQuotaSnapshot
	if !readJSONFile(cache, &snap) {
		t.Fatal("the exit-grace attempt produced no snapshot")
	}
	if snap.ObservedAt == "" || snap.AccountFingerprint == "" {
		t.Errorf("exit-grace snapshot is not attributable: %+v", snap)
	}
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

// The polling loop is hard-capped so a very long run cannot turn into unbounded
// background work on the user's machine. The exit-grace attempt is deliberately
// outside the cap, so at most one further probe may land.
func TestAntigravityQuotaCapture_StopsAtTheAttemptCap(t *testing.T) {
	home, _ := helperIsolateAntigravityCapture(t, "1ms")
	server := helperStartCaptureServer(t, filepath.Join(home, ".gemini", "antigravity-cli"),
		helperQuotaJSON, helperStatusJSON)

	finish := startAntigravityQuotaCapture("capped run")

	// Wait for the probe count to go quiet — that is the cap taking effect. The
	// quota RPC is issued exactly once per attempt (the port is memoized after
	// the first success), so hits and attempts are the same number.
	deadline := time.Now().Add(90 * time.Second)
	var settled int64
	for {
		before := server.quotaHits.Load()
		time.Sleep(500 * time.Millisecond)
		settled = server.quotaHits.Load()
		if settled == before && settled > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("polling never settled — the attempt cap is not enforced")
		}
	}
	if settled > antigravityCaptureMaxAttempts {
		t.Errorf("%d probes, want at most the cap %d", settled, antigravityCaptureMaxAttempts)
	}

	helperStopCapture(t, finish)
	if got := server.quotaHits.Load(); got > antigravityCaptureMaxAttempts+1 {
		t.Errorf("%d probes after the exit-grace read, want at most cap+1 (%d)",
			got, antigravityCaptureMaxAttempts+1)
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
		"schemaVersion": true, "observedAt": true, "accountFingerprint": true,
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
