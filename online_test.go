// Tests for the notifyOnline / notifyOnlineIfApplicable / shared-mutex
// behavior added so the Go agent can clear `offlineSince` on tray
// Reconnect and on registered+online startup.
//
// Why these tests are the regression guard for the prod "stuck-grey-device"
// bug:
//
//   1. The prod bug existed because the Go agent never made the HTTP call
//      to /device/:id/online — there was a notifyOffline path but no
//      symmetric online path. If a future refactor accidentally drops the
//      call site (or breaks the URL path / HMAC payload shape), the unit
//      tests below fail at build/CI rather than waiting for someone to
//      notice grey dots in prod.
//
//   2. The shared mutex (notifyConnectivityMutex) is what prevents a
//      late-arriving notifyOffline from clobbering a fresh notifyOnline
//      during rapid tray toggling. The serialization test here is the
//      contract: if the mutex is removed or accidentally split per-route,
//      the test catches it.
//
//   3. The boot-path helper notifyOnlineIfApplicable encodes "only call
//      /online when registered AND not in user-toggled offline mode" — the
//      gate that keeps us from silently overriding a user's explicit
//      Disconnect across restarts. The gate tests below pin those edges.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetConnectivityState ensures tests start from a clean slate so a prior
// test's mutex state, in-memory offline flag, or shutdown guard doesn't
// leak in. Mirrors resetShutdownState() in shutdown_test.go but covers the
// connectivity-side state too.
func resetConnectivityState(t *testing.T) {
	t.Helper()
	shutdownInProgress.Store(false)
	offlineMutex.Lock()
	isOffline = false
	offlineMutex.Unlock()
	select {
	case <-offlineChan:
	default:
	}
}

// captureRequest is the test helper for assertions about what the server
// received. We log every POST under one mutex so concurrent tests see a
// consistent view, and we record the parsed body so the HMAC-signature
// shape can be verified independently.
type captureRequest struct {
	mu    sync.Mutex
	hits  []capturedHit
	delay time.Duration // set per-test to slow the handler
}

type capturedHit struct {
	path      string
	timestamp int64
	signature string
	startedAt time.Time
	endedAt   time.Time
}

func (c *captureRequest) handler(t *testing.T, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var parsed struct {
			Timestamp int64  `json:"timestamp"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("failed to parse body: %v — raw=%q", err, string(body))
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Hold the response open if a delay was requested (used for the
		// mutex-serialization test). Sleeping inside the handler keeps the
		// HTTP client blocked, so we can observe whether two callers
		// overlap or are forced to take turns.
		c.mu.Lock()
		delay := c.delay
		c.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}

		c.mu.Lock()
		c.hits = append(c.hits, capturedHit{
			path:      r.URL.Path,
			timestamp: parsed.Timestamp,
			signature: parsed.Signature,
			startedAt: startedAt,
			endedAt:   time.Now(),
		})
		c.mu.Unlock()
		w.WriteHeader(status)
	}
}

func (c *captureRequest) snapshot() []capturedHit {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedHit, len(c.hits))
	copy(out, c.hits)
	return out
}

// ── notifyOnline tests ────────────────────────────────────────────────────

// TestNotifyOnlineRequiresCredentials pins the same guard notifyOffline has:
// callers without AgentID/CommandSecret get a clean error rather than firing
// an unauthenticated request that would 401 on the server.
func TestNotifyOnlineRequiresCredentials(t *testing.T) {
	resetConnectivityState(t)

	if err := notifyOnline(context.Background(), nil); err == nil {
		t.Fatalf("expected error for nil cfg")
	}
	if err := notifyOnline(context.Background(), &Config{}); err == nil {
		t.Fatalf("expected error for empty cfg")
	}
	if err := notifyOnline(context.Background(), &Config{AgentID: "x"}); err == nil {
		t.Fatalf("expected error for cfg missing CommandSecret")
	}
}

// TestNotifyOnlineHappyPath verifies the request reaches /device/:id/online
// with a parseable HMAC payload on the very first try when the server
// responds 200.
func TestNotifyOnlineHappyPath(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-online-1",
		CommandSecret: "secret-online-1",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := notifyOnline(ctx, cfg); err != nil {
		t.Fatalf("expected notifyOnline to succeed, got %v", err)
	}

	hits := cap.snapshot()
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(hits))
	}
	if got, want := hits[0].path, "/device/agent-online-1/online"; got != want {
		t.Fatalf("expected path %q, got %q", want, got)
	}
	// Signature shape — full validation happens server-side in the
	// terminal-service tests; here we just confirm the client populated
	// the field at all.
	if hits[0].signature == "" {
		t.Fatalf("expected non-empty signature in payload")
	}
	if hits[0].timestamp == 0 {
		t.Fatalf("expected non-zero timestamp in payload")
	}
}

// TestNotifyOnlineRetriesOnTransientFailure proves the retry wrapper
// re-issues the request when the first attempt fails. Without retry, a
// transient network blip during boot would leave the device wrongly
// stuck offline until the next boot cycle.
func TestNotifyOnlineRetriesOnTransientFailure(t *testing.T) {
	resetConnectivityState(t)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-retry-1",
		CommandSecret: "secret-retry",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := notifyOnline(ctx, cfg); err != nil {
		t.Fatalf("expected notifyOnline to succeed after retry, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", got)
	}
}

// TestNotifyOnlineReportsErrorAfterAllAttempts ensures the function
// surfaces a useful error when retries are exhausted, rather than
// silently swallowing it. Boot-path callers wrap this in a goroutine and
// log on error — they can't log what they don't see.
func TestNotifyOnlineReportsErrorAfterAllAttempts(t *testing.T) {
	resetConnectivityState(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-fail-1", CommandSecret: "secret"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := notifyOnline(ctx, cfg)
	if err == nil {
		t.Fatalf("expected error after all attempts failed")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to mention status 500, got %q", err.Error())
	}
}

// TestNotifyOnlineHonorsContextCancel ensures a cancelled context stops
// retries promptly. Boot path uses a 5s budget; if the network is fully
// broken we'd otherwise spend 4s+ on the second attempt's HTTP timeout
// before the boot goroutine returns.
func TestNotifyOnlineHonorsContextCancel(t *testing.T) {
	resetConnectivityState(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-ctx-1", CommandSecret: "secret"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	if err := notifyOnline(ctx, cfg); err == nil {
		t.Fatalf("expected ctx.Err() when ctx is pre-cancelled")
	}
}

// TestNotifyOnlineHMACSignatureShape asserts the request payload is a
// valid HMAC-SHA256 of "<agentId>:<timestamp>" using cfg.CommandSecret —
// matches what the backend's revoke.route.js verifies. Catches anyone
// changing the signed-string format without updating both sides.
func TestNotifyOnlineHMACSignatureShape(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-sig-1", CommandSecret: "shared-secret-xyz"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := notifyOnline(ctx, cfg); err != nil {
		t.Fatalf("notifyOnline failed: %v", err)
	}

	hits := cap.snapshot()
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}

	// Recompute the expected signature using the same primitive the
	// production code uses. If the message format ever drifts, this
	// fails immediately rather than producing a 401 only in prod.
	expected := generateHMAC(
		// Use the captured timestamp the agent actually sent — recomputing
		// against time.Now() would race with the network round-trip.
		fmtSigInput(cfg.AgentID, hits[0].timestamp),
		cfg.CommandSecret,
	)
	if hits[0].signature != expected {
		t.Fatalf("signature mismatch:\n  got      %s\n  expected %s", hits[0].signature, expected)
	}
}

// ── notifyOnlineIfApplicable (boot-path gate) tests ──────────────────────

// TestNotifyOnlineIfApplicableSkipsWhenUnregistered: the StartAgent boot
// path fires this unconditionally; the helper must skip when AgentID is
// empty so we don't fire an unauthenticated 401-bound request on every
// pre-registration boot.
func TestNotifyOnlineIfApplicableSkipsWhenUnregistered(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{} // not registered
	notifyOnlineIfApplicable(context.Background(), cfg)

	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("expected zero requests when unregistered, got %d", got)
	}
}

// TestNotifyOnlineIfApplicableSkipsWhenOffline: when the user has
// explicitly disconnected (cfg.OfflineMode==true), we must NOT silently
// override their choice on next boot. This is the safeguard that lets
// users keep a device persistently offline through restarts.
func TestNotifyOnlineIfApplicableSkipsWhenOffline(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-offline-1",
		CommandSecret: "secret",
		OfflineMode:   true,
	}
	notifyOnlineIfApplicable(context.Background(), cfg)

	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("expected zero requests when OfflineMode=true, got %d", got)
	}
}

// TestNotifyOnlineIfApplicableFiresWhenRegisteredAndOnline: this is the
// path that actually fixes the prod bug. A registered device with
// OfflineMode=false on boot must call /online so any stale offlineSince
// from a previous shutdown is cleared.
func TestNotifyOnlineIfApplicableFiresWhenRegisteredAndOnline(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-boot-1",
		CommandSecret: "secret",
		OfflineMode:   false,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	notifyOnlineIfApplicable(ctx, cfg)

	hits := cap.snapshot()
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 request on registered+online boot, got %d", len(hits))
	}
	if got, want := hits[0].path, "/device/agent-boot-1/online"; got != want {
		t.Fatalf("expected path %q, got %q", want, got)
	}
}

// TestNotifyOnlineIfApplicableSwallowsErrors: a transient backend failure
// must not panic or block boot. The helper logs and returns; the next
// shutdown→restart cycle (or a manual tray Reconnect) recovers.
func TestNotifyOnlineIfApplicableSwallowsErrors(t *testing.T) {
	resetConnectivityState(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-swallow-1", CommandSecret: "secret"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Must not panic, must return.
	notifyOnlineIfApplicable(ctx, cfg)
}

// ── Shared-mutex serialization test ──────────────────────────────────────

// TestNotifyConnectivityMutexSerializesOfflineAndOnline is the regression
// test for the rapid-toggle race: if the user clicks Disconnect →
// Reconnect quickly enough that both RPCs are in flight, OR if a
// graceful-shutdown notifyOffline races with a boot notifyOnline (e.g.
// during auto-update overlap), the wire order MUST match the call order
// or the device ends up wrongly stuck.
//
// notifyConnectivityMutex enforces that. We verify by holding the
// server's response open for 200ms on the first call, firing the second
// call concurrently, and confirming the second call's start time is
// after the first call's end time — i.e., they did not overlap on the
// server side.
func TestNotifyConnectivityMutexSerializesOfflineAndOnline(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{delay: 200 * time.Millisecond}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-mutex-1", CommandSecret: "secret"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fire offline first, then online from a goroutine. With the mutex,
	// the second call blocks on Lock() until the first finishes, so the
	// server sees them sequentially.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = notifyOffline(ctx, cfg)
	}()
	// Tiny gap so the first goroutine grabs the mutex before the second
	// one tries — without this we'd be measuring goroutine-scheduling
	// jitter rather than the mutex's effect.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		_ = notifyOnline(ctx, cfg)
	}()
	wg.Wait()

	hits := cap.snapshot()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	// Find the offline hit and the online hit by path. With the mutex
	// working, the second call's startedAt is at least one server-delay
	// later than the first call's startedAt — because the second client
	// is blocked on the mutex until the first server response returns.
	// Without the mutex, both server-side handlers would start within
	// goroutine-scheduling jitter of each other (~ms) and run in
	// parallel. We assert the gap is at least ~80% of the configured
	// delay; a conservative threshold below `delay` itself absorbs any
	// scheduler/clock noise but is still vastly larger than the
	// "broken mutex" case.
	var offlineHit, onlineHit *capturedHit
	for i := range hits {
		switch {
		case strings.HasSuffix(hits[i].path, "/offline"):
			offlineHit = &hits[i]
		case strings.HasSuffix(hits[i].path, "/online"):
			onlineHit = &hits[i]
		}
	}
	if offlineHit == nil || onlineHit == nil {
		t.Fatalf("expected one /offline and one /online hit; got paths %v", []string{hits[0].path, hits[1].path})
	}

	gap := onlineHit.startedAt.Sub(offlineHit.startedAt)
	const minGap = 160 * time.Millisecond // 80% of the 200ms handler delay
	if gap < minGap {
		t.Fatalf("expected online to start at least %v after offline (mutex serialization), got gap=%v.\n  offline: %v → %v\n  online:  %v → %v",
			minGap, gap,
			offlineHit.startedAt, offlineHit.endedAt,
			onlineHit.startedAt, onlineHit.endedAt)
	}
}

// TestNotifyConnectivityResetsBudgetAfterLock pins the regression flagged
// by the Codex bot review on PR #9: rapid Disconnect→Reconnect (or any
// pair of state-flip RPCs back-to-back) must not let the first call's
// retries-while-holding-the-mutex eat into the second call's HTTP budget.
//
// Without the post-lock budget reset, the second call would block on the
// mutex while its caller-provided context expired, then abort on the
// first per-attempt cancellation check — silently dropping the user's
// most recent intent. With the reset, the second call gets a fresh
// budget for HTTP work once it owns the mutex, and the user's later
// click reaches the backend.
//
// Test mechanic: the server delays only the FIRST request long enough
// that a second caller's tight 200ms context expires while waiting on
// the mutex. After the first call releases, the second call must
// still succeed (proving its budget was reset).
func TestNotifyConnectivityResetsBudgetAfterLock(t *testing.T) {
	resetConnectivityState(t)

	var firstHit atomic.Bool
	cap := &captureRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow first request, instant on every subsequent call.
		if firstHit.CompareAndSwap(false, true) {
			time.Sleep(800 * time.Millisecond)
		}
		// Reuse the captureRequest helper's recording machinery so we
		// still assert on the path/timestamp/signature shape.
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Timestamp int64  `json:"timestamp"`
			Signature string `json:"signature"`
		}
		_ = json.Unmarshal(body, &parsed)
		cap.mu.Lock()
		cap.hits = append(cap.hits, capturedHit{
			path:      r.URL.Path,
			timestamp: parsed.Timestamp,
			signature: parsed.Signature,
			startedAt: time.Now(),
			endedAt:   time.Now(),
		})
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-budget-reset-1", CommandSecret: "secret"}

	// Fire a slow notifyOffline that will hold the mutex for ~800ms.
	// Use a generous 5s parent ctx so this call itself doesn't get cut
	// short — we're testing what happens to the SECOND call.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := notifyOffline(ctx, cfg); err != nil {
			t.Errorf("first notifyOffline failed: %v", err)
		}
	}()

	// Brief delay so the first goroutine grabs the mutex before the
	// second one tries to enter — without this we'd be measuring
	// goroutine scheduling, not the deadline-reset behavior.
	time.Sleep(50 * time.Millisecond)

	// Second call: 200ms parent ctx. It will expire while we're still
	// blocked on the mutex (~750ms left of the first call's hold).
	// Without the post-lock reset, the per-attempt ctx check would fire
	// immediately after lock acquisition and return ctx.Err(). With the
	// reset, the call proceeds with a fresh 5s budget for HTTP work.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := notifyOnline(ctx, cfg); err != nil {
		t.Fatalf("expected notifyOnline to succeed despite parent ctx expiring during mutex wait; got %v after %v",
			err, time.Since(start))
	}
	wg.Wait()

	// Sanity: the call should have waited for the first call's mutex
	// hold (~750ms remaining at start of second call's wait), proving
	// it actually went through the queue rather than racing past it.
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("expected notifyOnline to wait for mutex (~750ms), but completed in %v — mutex serialization broken?", elapsed)
	}

	// Both calls must have reached the server.
	hits := cap.snapshot()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits (1 offline + 1 online), got %d", len(hits))
	}
	var sawOffline, sawOnline bool
	for _, h := range hits {
		if strings.HasSuffix(h.path, "/offline") {
			sawOffline = true
		}
		if strings.HasSuffix(h.path, "/online") {
			sawOnline = true
		}
	}
	if !sawOffline || !sawOnline {
		t.Fatalf("expected one /offline and one /online hit; got paths %+v", hits)
	}
}

// TestNotifyConnectivityRejectsPreCancelledContext pins the OTHER half
// of the budget-reset contract: a caller passing an already-cancelled
// context must still abort early — we don't want a stale, doomed call
// to queue up on the mutex behind a slow predecessor. The early-cancel
// check at the top of notifyConnectivity preserves this even though we
// reset the deadline post-lock.
func TestNotifyConnectivityRejectsPreCancelledContext(t *testing.T) {
	resetConnectivityState(t)

	cap := &captureRequest{}
	srv := httptest.NewServer(cap.handler(t, http.StatusOK))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{AgentID: "agent-precancel-1", CommandSecret: "secret"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	if err := notifyOnline(ctx, cfg); err == nil {
		t.Fatalf("expected error for pre-cancelled ctx")
	}

	// And critically, the request must NOT have reached the server —
	// the early-cancel check fast-paths out before mutex.Lock().
	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("expected zero requests for pre-cancelled ctx, got %d", got)
	}
}

// fmtSigInput recomputes the canonical "<agentId>:<timestamp>" string used
// as HMAC input — kept here as a tiny helper so the signature-shape test
// reads the same as the production code without forcing an export.
func fmtSigInput(agentID string, timestamp int64) string {
	return fmt.Sprintf("%s:%d", agentID, timestamp)
}
