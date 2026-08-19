// autoupdate_reconcile_test.go
// -----------------------------------------------------------------------------
// Reconciliation is the one thing that runs BEFORE the updater will look for a
// release, so anything that can make it fail forever is a silent, permanent
// "this machine never updates again". These tests pin the two ways out of that:
//
//   - a failure that PROVES the service has no such device is terminal, so the
//     marker is dropped immediately rather than retried; and
//   - every other failure is bounded, so no error can hold the scheduler past
//     the budget.
//
// The incident: a device's config.json was overwritten with a test fixture
// carrying pending_update_attempt_id "test-attempt" and an agent id the service
// had never issued. /device/{id}/drain/exit answered 404 NOT_FOUND on a loop for
// a day, the updater logged "Skipping check while restart reconciliation is
// pending" every six hours, and the machine sat on a stale version with
// "Automatically update" ticked.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

/* ─────────────────────────── error classification ───────────────────────── */

func TestReadServiceErrorCode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"service error code", `{"code":"NOT_FOUND","message":"Device not found"}`, "NOT_FOUND"},
		{"drain expired", `{"code":"DRAIN_EXPIRED"}`, "DRAIN_EXPIRED"},
		{"json without a code", `{"message":"Device not found"}`, ""},
		{"fastify route-not-found shape", `{"statusCode":404,"error":"Not Found","message":"Route POST:/x not found"}`, ""},
		{"an html error page from an intermediary", `<html><body>404</body></html>`, ""},
		{"empty body", ``, ""},
		{"oversized body is not parsed", `{"code":"NOT_FOUND","pad":"` + strings.Repeat("x", maxServiceErrorBody) + `"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readServiceErrorCode(strings.NewReader(tc.body)); got != tc.want {
				t.Fatalf("readServiceErrorCode(%.60q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
	if got := readServiceErrorCode(nil); got != "" {
		t.Fatalf("readServiceErrorCode(nil) = %q, want empty", got)
	}
}

// The bare form has to keep its historic wording: it is what the console shows
// and what operators grep for in agent.log.
func TestConnectivityHTTPErrorMessage(t *testing.T) {
	bare := &connectivityHTTPError{StatusCode: 503}
	if bare.Error() != "server returned status 503" {
		t.Fatalf("bare message = %q, want the unchanged legacy wording", bare.Error())
	}
	coded := &connectivityHTTPError{StatusCode: 404, Code: "NOT_FOUND"}
	if coded.Error() != "server returned status 404 (NOT_FOUND)" {
		t.Fatalf("coded message = %q", coded.Error())
	}
}

func TestIsDeviceUnknownErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"device is gone", &connectivityHTTPError{StatusCode: 404, Code: "NOT_FOUND"}, true},
		{"wrapped device is gone", fmt.Errorf("reconcile: %w", &connectivityHTTPError{StatusCode: 404, Code: "NOT_FOUND"}), true},
		{"404 with no code is a router answer, not a verdict", &connectivityHTTPError{StatusCode: 404}, false},
		{"404 with some other code", &connectivityHTTPError{StatusCode: 404, Code: "WORKSPACE_NOT_FOUND"}, false},
		{"NOT_FOUND on a different status", &connectivityHTTPError{StatusCode: 500, Code: "NOT_FOUND"}, false},
		{"expired drain", &connectivityHTTPError{StatusCode: 410, Code: "DRAIN_EXPIRED"}, false},
		{"service is down", &connectivityHTTPError{StatusCode: 503}, false},
		{"untyped text that merely mentions 404", errors.New("server returned status 404"), false},
		{"transport failure", errors.New("http: connection refused"), false},
		{"no error", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDeviceUnknownErr(tc.err); got != tc.want {
				t.Fatalf("isDeviceUnknownErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsDrainExpiredErrAcceptsTypedAndLegacyForms(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"typed 410", &connectivityHTTPError{StatusCode: 410, Code: "DRAIN_EXPIRED"}, true},
		{"typed 410 without a code", &connectivityHTTPError{StatusCode: 410}, true},
		{"legacy text", errors.New("server returned status 410"), true},
		{"legacy code text", errors.New("server returned status 410: DRAIN_EXPIRED"), true},
		{"typed device unknown is not an expiry", &connectivityHTTPError{StatusCode: 404, Code: "NOT_FOUND"}, false},
		{"typed 503", &connectivityHTTPError{StatusCode: 503}, false},
		{"no error", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDrainExpiredErr(tc.err); got != tc.want {
				t.Fatalf("isDrainExpiredErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The classification is worthless if the retry wrapper stringifies the error on
// the way out, so pin the whole path: real HTTP response → notifyDrainExit.
func TestNotifyDrainExitSurfacesTheServiceErrorCode(t *testing.T) {
	resetConnectivityState(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"Device not found"}`))
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := registeredCfg()
	err := notifyDrainExit(context.Background(), cfg, "att-1", "deferred")
	if err == nil {
		t.Fatal("notifyDrainExit must report the 404")
	}

	var httpErr *connectivityHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error %v (%T) did not survive the retry wrapper as *connectivityHTTPError", err, err)
	}
	if httpErr.StatusCode != http.StatusNotFound || httpErr.Code != "NOT_FOUND" {
		t.Fatalf("got status %d code %q, want 404/NOT_FOUND", httpErr.StatusCode, httpErr.Code)
	}
	if !isDeviceUnknownErr(err) {
		t.Fatal("a 404 NOT_FOUND from the wire must classify as device-unknown")
	}
}

/* ──────────────────── resolveInterruptedAttempt outcomes ─────────────────── */

// reconcileRig stands up a fake terminal-service and a registered config that
// already carries an attempt marker.
type reconcileRig struct {
	cfg      *Config
	savePath string
	calls    *atomic.Int32
}

func newReconcileRig(t *testing.T, handler http.HandlerFunc) *reconcileRig {
	t.Helper()
	resetShutdownState(t)
	resetConnectivityState(t)
	resetInterruptedReconcileGuard(t)

	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := registeredCfg()
	cfg.AgentID = "agent-reconcile"
	cfg.PendingUpdateAttemptID = "att-stuck"
	cfg.PendingUpdateVersion = "v9.9.9"

	return &reconcileRig{cfg: cfg, savePath: filepath.Join(t.TempDir(), "config.json"), calls: calls}
}

func (r *reconcileRig) marker() string {
	var id string
	r.cfg.WithPersistenceLock(func() { id = r.cfg.PendingUpdateAttemptID })
	return id
}

// persistedMarker reads the marker back off disk, so a test proves the config
// was actually saved rather than only mutated in memory.
func (r *reconcileRig) persistedMarker(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r.savePath)
	if err != nil {
		t.Fatalf("config was never persisted: %v", err)
	}
	var persisted struct {
		PendingUpdateAttemptID string `json:"pending_update_attempt_id"`
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("persisted config is not JSON: %v", err)
	}
	return persisted.PendingUpdateAttemptID
}

func notFoundDevice(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"code":"NOT_FOUND","message":"Device not found"}`))
}

// The exact shape of the incident.
func TestResolveInterruptedAttemptAbandonsAnUnknownDevice(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)

	if pending := resolveInterruptedAttemptWithPath(r.cfg, r.savePath); pending {
		t.Fatal("an unknown device must not leave reconciliation pending — that blocks every future update check")
	}
	if got := r.marker(); got != "" {
		t.Fatalf("attempt marker = %q, want it dropped", got)
	}
	if got := r.persistedMarker(t); got != "" {
		t.Fatalf("persisted attempt marker = %q, want it dropped so the next boot is clean too", got)
	}
}

// The same verdict on the post-update /online report, which is the other branch
// resolveInterruptedAttempt can take.
func TestResolveInterruptedAttemptAbandonsAnUnknownDeviceOnTheOnlineReport(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)
	r.cfg.PendingUpdateVersion = Version
	r.cfg.PendingUpdateApplied = true

	if pending := resolveInterruptedAttemptWithPath(r.cfg, r.savePath); pending {
		t.Fatal("an unknown device must not leave reconciliation pending")
	}
	if got := r.marker(); got != "" {
		t.Fatalf("attempt marker = %q, want it dropped", got)
	}
}

// Fail safe: everything that is not a proven "no such device" keeps the marker,
// because the drain it names may be real and clearing it early would strand a
// live drain on the service.
func TestResolveInterruptedAttemptKeepsTheMarkerOnRecoverableFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"service is down", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}},
		{"a 404 with no service code is a missing route, not a missing device", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"statusCode":404,"error":"Not Found","message":"Route POST:/device/x/drain/exit not found"}`))
		}},
		{"auth rejected", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"INVALID_SIGNATURE"}`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconcileRig(t, tc.handler)
			if pending := resolveInterruptedAttemptWithPath(r.cfg, r.savePath); !pending {
				t.Fatal("a recoverable failure must leave reconciliation pending so it is retried")
			}
			if got := r.marker(); got != "att-stuck" {
				t.Fatalf("attempt marker = %q, want it retained", got)
			}
		})
	}
}

/* ──────────────────────── the bounded retry budget ──────────────────────── */

// resetInterruptedReconcileGuard clears the single-flight latch and swaps in an
// instant backoff, so a test can drive the whole budget in milliseconds.
func resetInterruptedReconcileGuard(t *testing.T) {
	t.Helper()
	interruptedReconcileRunning.Store(false)

	reconcileBackoffMu.Lock()
	previous := reconcileBackoffFn
	reconcileBackoffFn = func(int) time.Duration { return time.Millisecond }
	reconcileBackoffMu.Unlock()

	t.Cleanup(func() {
		reconcileBackoffMu.Lock()
		reconcileBackoffFn = previous
		reconcileBackoffMu.Unlock()
		interruptedReconcileRunning.Store(false)
	})
}

// waitForReconcileLoopToFinish blocks until a reconciliation loop spawned by
// runAttempt has returned. Tests that trigger one must call this before they
// finish: the loop outlives the test body, and letting it run on past the
// httptest server's teardown leaks a goroutine into whatever runs next.
func waitForReconcileLoopToFinish(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !interruptedReconcileRunning.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconciliation loop did not finish within its budget")
}

// No error may hold the scheduler indefinitely, even one we cannot classify.
// Giving up is safe: terminal-service expires an unconfirmed drain after
// DRAIN_STALE_MS and resumes routing on its own.
func TestRetryInterruptedAttemptReconciliationAbandonsAfterItsBudget(t *testing.T) {
	r := newReconcileRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	retryInterruptedAttemptReconciliationAt(r.cfg, r.savePath)

	if got := r.marker(); got != "" {
		t.Fatalf("attempt marker = %q, want it abandoned once the budget ran out", got)
	}
	if got := r.persistedMarker(t); got != "" {
		t.Fatalf("persisted attempt marker = %q, want it abandoned", got)
	}
	// Each round makes notifyConnectivityMaxAttempts requests before failing.
	wantMax := int32(autoUpdateReconcileMaxAttempts * notifyConnectivityMaxAttempts)
	if got := r.calls.Load(); got == 0 || got > wantMax {
		t.Fatalf("server saw %d requests, want between 1 and %d", got, wantMax)
	}
}

// An unknown device short-circuits the budget: one round, then done.
func TestRetryInterruptedAttemptReconciliationStopsImmediatelyOnAnUnknownDevice(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)

	retryInterruptedAttemptReconciliationAt(r.cfg, r.savePath)

	if got := r.marker(); got != "" {
		t.Fatalf("attempt marker = %q, want it dropped", got)
	}
	if got := r.calls.Load(); got > notifyConnectivityMaxAttempts {
		t.Fatalf("server saw %d requests, want at most one round (%d) — a terminal verdict must not be retried",
			got, notifyConnectivityMaxAttempts)
	}
}

// runAttempt spawns one of these loops on every scheduled tick that finds a
// retained marker. Without the latch, a marker that keeps failing accumulates a
// goroutine and a fresh set of RPCs every six hours.
func TestRetryInterruptedAttemptReconciliationIsSingleFlight(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)
	interruptedReconcileRunning.Store(true)

	done := make(chan struct{})
	go func() {
		retryInterruptedAttemptReconciliationAt(r.cfg, r.savePath)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second reconciliation loop must return immediately while one is running")
	}
	if got := r.calls.Load(); got != 0 {
		t.Fatalf("second loop made %d requests, want none", got)
	}
	if got := r.marker(); got != "att-stuck" {
		t.Fatalf("second loop touched the marker (%q) despite not running", got)
	}
}

func TestRetryInterruptedAttemptReconciliationReleasesTheLatch(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)

	retryInterruptedAttemptReconciliationAt(r.cfg, r.savePath)

	if interruptedReconcileRunning.Load() {
		t.Fatal("latch still held after the loop returned — no later reconciliation could ever run")
	}
}

/* ──────────────────── drain-exit reconciliation (autoUpdater) ────────────── */

func TestExitDrainDoesNotRetryAnUnknownDevice(t *testing.T) {
	resetShutdownState(t)
	cfg := registeredCfg()
	cfg.PendingUpdateAttemptID = "att-stuck"
	r := newAutoTestRig(t, cfg)

	var calls atomic.Int32
	r.au.drainExit = func(_ context.Context, _, _ string) error {
		calls.Add(1)
		return &connectivityHTTPError{StatusCode: http.StatusNotFound, Code: "NOT_FOUND"}
	}

	r.au.exitDrain("att-stuck", "deferred", false)

	var retained string
	cfg.WithPersistenceLock(func() { retained = cfg.PendingUpdateAttemptID })
	if retained != "" {
		t.Fatalf("attempt marker = %q, want it dropped for an unknown device", retained)
	}
	// Give a retry goroutine, if one were wrongly spawned, a chance to show up.
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("drainExit called %d times, want exactly 1 — a terminal verdict must not spawn a retry loop", got)
	}
}

func TestRetryDrainExitReconciliationAbandonsAfterItsBudget(t *testing.T) {
	resetShutdownState(t)
	resetInterruptedReconcileGuard(t)
	cfg := registeredCfg()
	cfg.PendingUpdateAttemptID = "att-stuck"
	r := newAutoTestRig(t, cfg)

	var calls atomic.Int32
	r.au.drainExit = func(_ context.Context, _, _ string) error {
		calls.Add(1)
		return errors.New("server returned status 503")
	}

	r.au.retryDrainExitReconciliation("att-stuck", "deferred")

	if got := calls.Load(); got != autoUpdateReconcileMaxAttempts {
		t.Fatalf("drainExit called %d times, want the %d-round budget", got, autoUpdateReconcileMaxAttempts)
	}
	var retained string
	cfg.WithPersistenceLock(func() { retained = cfg.PendingUpdateAttemptID })
	if retained != "" {
		t.Fatalf("attempt marker = %q, want it abandoned once the budget ran out", retained)
	}
}

func TestRetryDrainExitReconciliationStopsOnAnUnknownDevice(t *testing.T) {
	resetShutdownState(t)
	resetInterruptedReconcileGuard(t)
	cfg := registeredCfg()
	cfg.PendingUpdateAttemptID = "att-stuck"
	r := newAutoTestRig(t, cfg)

	var calls atomic.Int32
	r.au.drainExit = func(_ context.Context, _, _ string) error {
		calls.Add(1)
		return &connectivityHTTPError{StatusCode: http.StatusNotFound, Code: "NOT_FOUND"}
	}

	r.au.retryDrainExitReconciliation("att-stuck", "deferred")

	if got := calls.Load(); got != 1 {
		t.Fatalf("drainExit called %d times, want 1", got)
	}
	var retained string
	cfg.WithPersistenceLock(func() { retained = cfg.PendingUpdateAttemptID })
	if retained != "" {
		t.Fatalf("attempt marker = %q, want it dropped", retained)
	}
}

// The budget spans about an hour in production, long enough for a newer attempt
// to take ownership of the marker. That one is still reconcilable, so the
// expired loop must leave it alone.
func TestRetryDrainExitReconciliationLeavesANewerAttemptAlone(t *testing.T) {
	resetShutdownState(t)
	resetInterruptedReconcileGuard(t)
	cfg := registeredCfg()
	cfg.PendingUpdateAttemptID = "att-stuck"
	r := newAutoTestRig(t, cfg)

	var calls atomic.Int32
	r.au.drainExit = func(_ context.Context, _, _ string) error {
		if calls.Add(1) == 2 {
			cfg.WithPersistenceLock(func() { cfg.PendingUpdateAttemptID = "att-newer" })
		}
		return errors.New("server returned status 503")
	}

	r.au.retryDrainExitReconciliation("att-stuck", "deferred")

	var retained string
	cfg.WithPersistenceLock(func() { retained = cfg.PendingUpdateAttemptID })
	if retained != "att-newer" {
		t.Fatalf("attempt marker = %q, want the newer attempt untouched", retained)
	}
}

/* ───────────────────────────── the wedge itself ─────────────────────────── */

// End to end: an unknown device must cost exactly one blocked tick, not every
// tick from here to reinstall.
func TestUnknownDeviceStopsBlockingUpdateChecks(t *testing.T) {
	r := newReconcileRig(t, notFoundDevice)

	rig := newAutoTestRig(t, r.cfg)
	rig.au.savePath = r.savePath
	rig.info = &UpdateInfo{Available: false}

	// Tick one: reconciliation runs and abandons the impossible marker.
	rig.au.runAttempt()
	if got := rig.checkCalls; got != 0 {
		t.Fatalf("check calls on the reconciling tick = %d, want 0", got)
	}
	if got := r.marker(); got != "" {
		t.Fatalf("attempt marker = %q after the reconciling tick, want it dropped", got)
	}

	// Tick two: nothing left to reconcile, so the updater finally checks.
	rig.au.runAttempt()
	if got := rig.checkCalls; got != 1 {
		t.Fatalf("check calls on the following tick = %d, want 1 — the updater is still wedged", got)
	}
}

// The control: a transient failure still blocks, because the marker may name a
// real drain. This is the behaviour the fix must NOT weaken.
func TestTransientFailureStillDefersTheUpdateCheck(t *testing.T) {
	r := newReconcileRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	rig := newAutoTestRig(t, r.cfg)
	rig.au.savePath = r.savePath

	rig.au.runAttempt()

	if got := rig.checkCalls; got != 0 {
		t.Fatalf("check calls = %d, want 0 while a real drain may still be outstanding", got)
	}
	if got := r.marker(); got != "att-stuck" {
		t.Fatalf("attempt marker = %q, want it retained", got)
	}

	// runAttempt handed the marker to a background retry loop; let it drain so
	// it does not outlive the fake service it is calling.
	waitForReconcileLoopToFinish(t)
}
