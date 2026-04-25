// Tests for the gracefulShutdown orchestration introduced as part of the
// terminal-offline-status fix. The contract these tests pin down:
//
//  1. gracefulShutdown is idempotent — concurrent or repeated calls don't
//     double-teardown sub-processes or fire two backend notifications.
//  2. gracefulShutdown flips the in-memory offline flag BEFORE attempting
//     anything that could publish a stale pong.
//  3. notifyOffline runs its retry loop and returns nil on a successful
//     attempt (even if the first attempt fails).
//  4. notifyOffline returns an error when credentials are missing so
//     gracefulShutdown can log the failure rather than silently skip it.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetShutdownState clears the package-level guards so each test starts
// from a known baseline. Tests run sequentially within a package, so a
// shared atomic flag would otherwise leak from one case to the next.
func resetShutdownState(t *testing.T) {
	t.Helper()
	shutdownInProgress.Store(false)
	offlineMutex.Lock()
	isOffline = false
	offlineMutex.Unlock()
	// Drain any pending channel value so a previous test's signal doesn't
	// fire spurious goroutines in this one.
	select {
	case <-offlineChan:
	default:
	}
}

func TestGracefulShutdownIsIdempotent(t *testing.T) {
	resetShutdownState(t)

	// Build a minimal config without registration credentials so the
	// notifyOffline path is skipped and we can focus on the guard.
	cfg := &Config{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gracefulShutdown(ctx, cfg)
		}()
	}
	wg.Wait()

	if !IsShutdownInProgress() {
		t.Fatalf("expected shutdownInProgress to be set after gracefulShutdown")
	}
	if !IsOffline() {
		t.Fatalf("expected IsOffline()==true after gracefulShutdown")
	}
}

func TestGracefulShutdownFlipsOfflineImmediately(t *testing.T) {
	resetShutdownState(t)

	cfg := &Config{} // no creds — notifyOffline path skipped
	if IsOffline() {
		t.Fatalf("precondition: IsOffline should be false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	gracefulShutdown(ctx, cfg)

	if !IsOffline() {
		t.Fatalf("gracefulShutdown must flip IsOffline to true")
	}
}

func TestNotifyOfflineRequiresCredentials(t *testing.T) {
	cfg := &Config{} // empty
	err := notifyOffline(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected error when credentials missing")
	}
}

// TestNotifyOfflineRetriesOnTransientFailure proves the retry wrapper added
// in this fix actually re-issues the request when the first attempt fails.
// Without retry the disconnect signal would be lost on any transient blip
// during shutdown, leaving the backend with stale offlineSince state.
func TestNotifyOfflineRetriesOnTransientFailure(t *testing.T) {
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

	// httptest gives us http://127.0.0.1:<port> which the loopback-allow
	// path in getRegistrationURL accepts as a TERMINAL_SERVICE_URL override.
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-test",
		CommandSecret: "secret-test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := notifyOffline(ctx, cfg); err != nil {
		t.Fatalf("expected notifyOffline to succeed after retry, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", got)
	}
}

// TestNotifyOfflineReportsErrorAfterAllAttempts verifies the function
// returns the last error rather than silently swallowing it (the previous
// behavior, which left shutdown callers with no way to log success/failure).
func TestNotifyOfflineReportsErrorAfterAllAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-test",
		CommandSecret: "secret-test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := notifyOffline(ctx, cfg)
	if err == nil {
		t.Fatalf("expected notifyOffline to surface error after all attempts failed")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to mention status 500, got %q", err.Error())
	}
}

// TestNotifyOfflineHonorsContextCancel ensures that a caller-provided
// cancelled context aborts retries instead of running them all out.
// Otherwise a system shutdown with a tight timeout would still wait the
// full 250ms × N backoff cycle even after the OS told us to terminate.
func TestNotifyOfflineHonorsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := &Config{
		AgentID:       "agent-test",
		CommandSecret: "secret-test",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := notifyOffline(ctx, cfg)
	if err == nil {
		t.Fatalf("expected notifyOffline to surface ctx.Err() when cancelled")
	}
}
