package main

// Tests for the bounded Claude utilization probe. Everything runs against an
// httptest server pinned through AIEXPEDITE_CLAUDE_USAGE_PROBE_URL, so no test
// here reaches the real endpoint — and the probe is opt-IN per process
// (claudeUsageProbeGate.armed), so a test that forgets to arm it makes no call
// at all rather than one to api.anthropic.com.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probeTestToken is the access token the credential fixture carries, so the
// handler can assert the Authorization header the probe actually sends.
const probeTestToken = "sk-ant-oat-test-token"

// armClaudeUsageProbe isolates the probe for one test: a private cache, a
// private Claude config dir holding a stored OAuth credential, no throttle, and
// the endpoint pinned at the given handler. Returns the cache path and a counter
// of requests the handler received.
func armClaudeUsageProbe(t *testing.T, handler http.HandlerFunc) (string, *int64) {
	t.Helper()

	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)

	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	writeClaudeProbeCredential(t, configDir, probeTestToken)

	// A non-default CLAUDE_CONFIG_DIR also keeps readClaudeCredentialsRaw off the
	// macOS Keychain, so this test reads only the fixture it just wrote.
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(claudeUsageProbeEndpointEnv, srv.URL)
	t.Setenv(claudeUsageProbeMinIntervalEnv, "0")

	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)
	t.Cleanup(resetClaudeUsageProbeGate)

	return cache, &calls
}

func writeClaudeProbeCredential(t *testing.T, configDir, token string) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","subscriptionType":"max"}}`, token)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// probeUsageJSON renders a realistic response body for the given windows.
func probeUsageJSON(windows map[string]string) string {
	parts := make([]string, 0, len(windows))
	for id, body := range windows {
		parts = append(parts, fmt.Sprintf("%q:%s", id, body))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Happy path: both windows land in the cache as real readings, stamped with the
// probe's provenance and the probe's observation time.
func TestClaudeUsageProbe_WritesFreshBuckets(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(3 * time.Hour)
	weekReset := now.Add(96 * time.Hour)

	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+probeTestToken {
			t.Errorf("Authorization=%q, want the stored access token", got)
		}
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"utilization":42,"resets_at":%q,"status":"allowed"}`,
				fiveReset.Format(time.RFC3339)),
			claudeWindowSevenDay: fmt.Sprintf(`{"used_percentage":77.5,"resets_at":%d,"status":"allowed"}`,
				weekReset.Unix()),
		}))
	})

	refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
	if !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want exactly 1", got)
	}

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected the probe to write the cache")
	}
	five := snap.Buckets[claudeWindowFiveHour]
	if five.UsedPercentage < 41.9 || five.UsedPercentage > 42.1 {
		t.Errorf("five_hour UsedPercentage=%v, want ~42 (utilization is 0..100 here)", five.UsedPercentage)
	}
	if !five.hasObservedUsage() {
		t.Error("five_hour must be recorded as an observed reading, not a heartbeat row")
	}
	if five.ObservedAtMs != now.UnixMilli() {
		t.Errorf("five_hour ObservedAtMs=%d, want %d", five.ObservedAtMs, now.UnixMilli())
	}
	if five.Source != claudeRateLimitSourceProbe {
		t.Errorf("five_hour Source=%q, want %q", five.Source, claudeRateLimitSourceProbe)
	}
	if five.ResetsAtMs != fiveReset.UnixMilli() {
		t.Errorf("five_hour ResetsAtMs=%d, want %d (RFC3339 stamp)", five.ResetsAtMs, fiveReset.UnixMilli())
	}

	week := snap.Buckets[claudeWindowSevenDay]
	if week.UsedPercentage != 77.5 {
		t.Errorf("seven_day UsedPercentage=%v, want 77.5", week.UsedPercentage)
	}
	if week.ResetsAtMs != weekReset.Unix()*1000 {
		t.Errorf("seven_day ResetsAtMs=%d, want %d (seconds normalised)", week.ResetsAtMs, weekReset.Unix()*1000)
	}
}

// Regression for the reported bug: a cache holding an old reading, plus a run
// that emits ONLY a usage-less heartbeat, must still end up with a strictly
// newer ObservedAt once the probe has run. This is the exact shape that pinned
// latestObservedAt at 2026-08-29T08:00:06.62Z through passing smokes.
func TestClaudeUsageProbe_AdvancesObservedAtAfterHeartbeatOnlyRun(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	seeded := now.Add(-4 * time.Hour)
	reset := now.Add(2 * time.Hour)

	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"utilization":60,"resets_at":%d,"status":"allowed"}`, reset.Unix()),
		}))
	})

	// Seed an old, real reading for a window that is still live.
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 55, ResetsAtMs: reset.UnixMilli(),
			ObservedAtMs: seeded.UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, seeded, "")

	// The run's only telemetry: an "allowed" heartbeat with no percentage. The
	// merge deliberately carries the prior observation time forward, so this
	// alone can never move the card.
	heartbeat := fmt.Sprintf(
		`{"type":"rate_limit_event","rateLimitInfo":{"status":"allowed","rateLimitType":"five_hour","resetsAt":%d}}`,
		reset.Unix())
	captureClaudeRateLimitLine(heartbeat, now.Add(-time.Minute))

	before := claudeCodeMetricsFromCache(now, "")[0].ObservedAt
	if before != observedAtRFC3339(seeded.UnixMilli()) {
		t.Fatalf("precondition: ObservedAt=%q, want the seeded %q — the heartbeat must not have advanced it",
			before, observedAtRFC3339(seeded.UnixMilli()))
	}

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}

	after := claudeCodeMetricsFromCache(now, "")[0]
	if after.ObservedAt != observedAtRFC3339(now.UnixMilli()) {
		t.Errorf("ObservedAt=%q, want the probe's %q (strictly newer than %q)",
			after.ObservedAt, observedAtRFC3339(now.UnixMilli()), before)
	}
	if after.Consumed == nil || *after.Consumed < 59.9 || *after.Consumed > 60.1 {
		t.Errorf("Consumed=%v, want ~60 from the probe reading", after.Consumed)
	}
}

// Every failure mode must leave the cache byte-identical — terminal-service
// delta-skips writes by hashing the payload, so a probe failure that rewrote the
// file (even with the same values) would churn a Firestore write per run.
func TestClaudeUsageProbe_FailuresLeaveCacheByteIdentical(t *testing.T) {
	oversized := strings.Repeat("x", claudeUsageProbeMaxBody+64)

	cases := []struct {
		name     string
		handler  http.HandlerFunc
		category string
	}{
		{
			name: "non-2xx",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			category: cliUsageErrorProviderUnavailable,
		},
		{
			name: "unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			category: cliUsageErrorNotAuthenticated,
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "{not json")
			},
			category: cliUsageErrorParseFailed,
		},
		{
			name: "no plottable window",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Well-formed, but every window is heartbeat-shaped (no
				// percentage), so there is nothing to observe.
				fmt.Fprint(w, `{"five_hour":{"status":"allowed","resets_at":0}}`)
			},
			category: cliUsageErrorParseFailed,
		},
		{
			name: "body exceeds the cap",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"five_hour":{"used_percentage":10},"pad":%q}`, oversized)
			},
			category: cliUsageErrorProviderUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
			cache, _ := armClaudeUsageProbe(t, tc.handler)

			seeded := now.Add(-6 * time.Hour)
			mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
				claudeWindowFiveHour: {
					UsedPercentage: 12, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
					ObservedAtMs: seeded.UnixMilli(), Status: "allowed", usageKnown: true,
				},
			}, seeded, "")
			want, err := os.ReadFile(cache)
			if err != nil {
				t.Fatal(err)
			}

			refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
			if refreshed {
				t.Error("a failed probe must not report the cache as refreshed")
			}
			if probeErr == nil || probeErr.ErrorCategory != tc.category {
				t.Errorf("probeErr=%+v, want category %q", probeErr, tc.category)
			}
			got, err := os.ReadFile(cache)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("cache changed on failure:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// A redirect must NOT be followed: the pinned-URL check vetted one location, and
// re-issuing the request would carry the subscription bearer token somewhere it
// never approved. The 3xx surfaces as an ordinary non-2xx failure instead.
func TestClaudeUsageProbe_DoesNotFollowRedirects(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	var leaked int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			atomic.AddInt64(&leaked, 1)
		}
		fmt.Fprint(w, `{"five_hour":{"used_percentage":99}}`)
	}))
	t.Cleanup(sink.Close)

	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusFound)
	})

	refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
	if refreshed {
		t.Error("a redirect must not be treated as a reading")
	}
	if probeErr == nil || probeErr.ErrorCategory != cliUsageErrorProviderUnavailable {
		t.Errorf("probeErr=%+v, want %q", probeErr, cliUsageErrorProviderUnavailable)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("origin request count=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&leaked); got != 0 {
		t.Errorf("the redirect target received %d authorized request(s); the token must never follow a 3xx", got)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("no cache should have been written; stat err=%v", err)
	}
}

// A connection the probe cannot open is a failure like any other: no cache
// write, and a category that names the transport rather than the endpoint.
func TestClaudeUsageProbe_ConnectionRefusedLeavesCacheAlone(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {})

	// Point at a closed loopback port: httptest hands out a real one, so closing
	// the server gives an address nothing is listening on.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()
	t.Setenv(claudeUsageProbeEndpointEnv, url)

	refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
	if refreshed {
		t.Error("a refused connection must not report a refresh")
	}
	if probeErr == nil {
		t.Fatal("expected a failure record for a refused connection")
	}
	if probeErr.ErrorCategory != cliUsageErrorProviderUnavailable &&
		probeErr.ErrorCategory != cliUsageErrorProviderTimeout {
		t.Errorf("category=%q, want an unavailable/timeout category", probeErr.ErrorCategory)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("no cache should have been created; stat err=%v", err)
	}
}

// Redaction: a response that carries credentials, raw config, and free-form
// vendor prose must persist NONE of it. Asserted on the serialized cache file,
// not the decoded struct, because the file is what the receipt is ultimately
// built from.
func TestClaudeUsageProbe_PersistsNoNonMetricFields(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
			"access_token": "sk-ant-oat-LEAKED",
			"refresh_token": "rt-LEAKED",
			"raw_config": {"apiKeyHelper": "/bin/leak"},
			"organization": {"name": "Vendor Prose Inc"},
			"five_hour": {
				"utilization": 0.5,
				"resets_at": %d,
				"status": "allowed but here is a long free-form vendor explanation",
				"vendor_note": "should never be persisted"
			}
		}`, now.Add(time.Hour).Unix())
	})

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"LEAKED", "access_token", "refresh_token", "raw_config",
		"apiKeyHelper", "Vendor Prose", "vendor_note", "free-form",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("cache contains %q, which must never be persisted:\n%s", forbidden, raw)
		}
	}

	// The unrecognized status prose is normalized rather than stored verbatim.
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache write")
	}
	if got := snap.Buckets[claudeWindowFiveHour].Status; got != "allowed" {
		t.Errorf("Status=%q, want the normalized %q", got, "allowed")
	}
}

// Skip conditions: each one must short-circuit before any request is issued.
func TestClaudeUsageProbe_SkipConditions(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ok := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"five_hour":{"used_percentage":10,"resets_at":0}}`)
	}

	t.Run("disable_claude_usage_probe", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, ok)
		SetClaudeUsageProbeDisabled(true)
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now); refreshed {
			t.Error("opt-out must skip the probe")
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})

	t.Run("no stored credential", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, ok)
		if err := os.Remove(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")); err != nil {
			t.Fatal(err)
		}
		if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); refreshed || probeErr != nil {
			t.Errorf("refreshed=%v err=%+v, want a silent skip", refreshed, probeErr)
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})

	t.Run("unarmed process makes no call", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, ok)
		resetClaudeUsageProbeGate() // leaves the gate unarmed
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now); refreshed {
			t.Error("an unarmed process must not probe")
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})

	// The endpoint override is a test seam, not a redirect knob: the request
	// carries the user's subscription bearer token, so anything able to set the
	// env var must not be able to aim it at a host of its choosing. Every
	// non-loopback override — http OR https — is refused outright rather than
	// falling back to the real endpoint, so an ignored override fails visibly.
	for _, override := range []string{
		"http://usage.example.com/api/oauth/usage",
		"https://usage.example.com/api/oauth/usage",
		"https://api.anthropic.com.evil.test/api/oauth/usage",
		"file:///etc/passwd",
		"://nonsense",
	} {
		t.Run("non-loopback override refused: "+override, func(t *testing.T) {
			_, calls := armClaudeUsageProbe(t, ok)
			t.Setenv(claudeUsageProbeEndpointEnv, override)
			refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
			if refreshed {
				t.Error("the bearer token must never be sent to an overridden remote host")
			}
			if probeErr == nil || probeErr.ErrorCategory != cliUsageErrorProviderUnavailable {
				t.Errorf("probeErr=%+v, want %q", probeErr, cliUsageErrorProviderUnavailable)
			}
			if got := atomic.LoadInt64(calls); got != 0 {
				t.Errorf("request count=%d, want 0", got)
			}
		})
	}

	// An unset override resolves to the real, hard-coded HTTPS endpoint — the
	// only host this probe may ever reach in production.
	t.Run("unset override resolves to the pinned endpoint", func(t *testing.T) {
		t.Setenv(claudeUsageProbeEndpointEnv, "")
		if got := claudeUsageProbeURL(); got != claudeUsageProbeEndpoint {
			t.Errorf("claudeUsageProbeURL()=%q, want %q", got, claudeUsageProbeEndpoint)
		}
	})
}

// Throttle: back-to-back runs inside the minimum interval issue one request; a
// user-initiated refresh bypasses it; concurrent callers collapse to one.
func TestClaudeUsageProbe_ThrottleAndSingleFlight(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	body := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":10,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	}

	t.Run("minimum interval collapses back-to-back runs", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

		if refreshed, _ := runClaudeUsageProbe(context.Background(), now); !refreshed {
			t.Fatal("first probe should run")
		}
		if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now.Add(time.Second)); refreshed || probeErr != nil {
			t.Errorf("second probe inside the interval: refreshed=%v err=%+v, want a silent skip", refreshed, probeErr)
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want 1", got)
		}

		// A user-initiated refresh bypasses the LOCAL timer. It cannot bypass the
		// shared-cache check, so clear the observation the first probe wrote —
		// otherwise this would be asserting the dedupe away rather than the timer.
		// (TestClaudeUsageProbe_SharedObservationSuppressesDuplicate covers that.)
		if err := os.Remove(os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE")); err != nil {
			t.Fatal(err)
		}
		SetClaudeUsageForceProbe(true)
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now.Add(2*time.Second)); !refreshed {
			t.Error("a forced probe must bypass the minimum interval")
		}
		if got := atomic.LoadInt64(calls); got != 2 {
			t.Errorf("request count=%d, want 2", got)
		}
	})

	t.Run("concurrent callers collapse to one request", func(t *testing.T) {
		release := make(chan struct{})
		_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			<-release
			body(w, r)
		})

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = runClaudeUsageProbe(context.Background(), now)
			}()
		}
		// Give the losers time to hit the latch and return before the winner's
		// request completes.
		time.Sleep(50 * time.Millisecond)
		close(release)
		wg.Wait()

		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want exactly 1 (single-flight)", got)
		}
	})
}

// refreshClaudeUsageIfStale is the routine-gather gate: a fresh cache is left
// alone, a stale one is probed, and a forced refresh probes regardless.
func TestRefreshClaudeUsageIfStale_Gating(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	body := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":33,"resets_at":%d}}`, reset.Unix())
	}

	t.Run("fresh observation is not probed", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		if refreshClaudeUsageIfStale(context.Background(), now, now.Add(-time.Minute), probeTestToken, "") {
			t.Error("a reading a minute old must not spend a probe")
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})

	t.Run("stale observation is probed", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		if !refreshClaudeUsageIfStale(context.Background(), now, now.Add(-claudeUsageProbeStaleAfter-time.Minute), probeTestToken, "") {
			t.Error("a reading older than the staleness TTL must be refreshed")
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want 1", got)
		}
	})

	// A cache that has never held a real reading — the state a fresh install or
	// an env-authenticated device is in — is stale by definition.
	t.Run("never-observed cache is probed", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
			t.Error("a cache with no observation at all must be refreshed")
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want 1", got)
		}
	})

	// The minimum interval belongs to the probe's own gate, not to this helper —
	// a stale observation on every gather must still yield one request per
	// interval, not one per gather.
	t.Run("repeated stale gathers collapse onto the minimum interval", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")
		stale := now.Add(-claudeUsageProbeStaleAfter - time.Minute)

		if !refreshClaudeUsageIfStale(context.Background(), now, stale, probeTestToken, "") {
			t.Fatal("the first stale gather should probe")
		}
		for i := 0; i < 3; i++ {
			if refreshClaudeUsageIfStale(context.Background(), now.Add(time.Duration(i+1)*time.Second), stale, probeTestToken, "") {
				t.Errorf("gather %d inside the minimum interval must not probe", i)
			}
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want 1", got)
		}
	})

	t.Run("a forced refresh probes a fresh observation", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		SetClaudeUsageForceProbe(true)
		if !refreshClaudeUsageIfStale(context.Background(), now, now.Add(-time.Minute), probeTestToken, "") {
			t.Error("a user-initiated refresh must probe regardless of age")
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Errorf("request count=%d, want 1", got)
		}
	})

	// Every remaining gate lives in the probe's begin(); this helper must not
	// re-derive them and must not probe when they refuse.
	t.Run("an opted-out agent is never probed", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		SetClaudeUsageProbeDisabled(true)
		if refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
			t.Error("opt-out must hold even for a never-observed cache")
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})
}

// latestClaudeObservation must ignore heartbeat-only rows: their ObservedAtMs
// records when a reset time was seen, not when a percentage was measured, so
// counting them would report the card as fresh in the exact state the staleness
// check exists to detect.
func TestLatestClaudeObservation_IgnoresHeartbeatOnlyRows(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-3 * time.Hour)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ObservedAtMs: observed.UnixMilli(),
			UsageObserved: usageObservedPtr(true),
		},
		claudeWindowSevenDay: {
			ObservedAtMs: now.UnixMilli(), UsageObserved: usageObservedPtr(false),
		},
	}
	if got := latestClaudeObservation(buckets); !got.Equal(observed.UTC()) {
		t.Errorf("latestClaudeObservation=%v, want %v (the heartbeat row must not count)", got, observed.UTC())
	}
	if got := latestClaudeObservation(map[string]claudeRateLimitBucket{}); !got.IsZero() {
		t.Errorf("empty cache should report the zero time, got %v", got)
	}
}

// Freshness is a PER-ROW question. The status-line hook refreshes only
// five_hour/seven_day, so a scalar "newest reading anywhere" lets frequent
// interactive renders report the whole card as fresh while another displayed row
// sits at an hours-old reading — the probe is then suppressed for the full
// staleness TTL and that row never advances.
func TestStalestClaudeRowObservation_FreshRowCannotMaskAStaleOne(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-6 * time.Hour)
	buckets := map[string]claudeRateLimitBucket{
		// What a status-line render leaves behind.
		claudeWindowFiveHour: {
			UsedPercentage: 40, ObservedAtMs: fresh.UnixMilli(),
			UsageObserved: usageObservedPtr(true),
		},
		// The weekly row it cannot refresh.
		claudeWindowSevenDayOpus: {
			UsedPercentage: 88, ObservedAtMs: stale.UnixMilli(),
			UsageObserved: usageObservedPtr(true),
		},
	}
	if got := latestClaudeObservation(buckets); !got.Equal(fresh.UTC()) {
		t.Fatalf("precondition: latestClaudeObservation=%v, want the fresh row %v", got, fresh.UTC())
	}
	if got := stalestClaudeRowObservation(buckets); !got.Equal(stale.UTC()) {
		t.Errorf("stalestClaudeRowObservation=%v, want the stale weekly row %v", got, stale.UTC())
	}
}

// A row that was never observed is NOT infinitely stale. "Unobserved" and
// "stale" are indistinguishable from the cache, and a comfortably-unused Fable
// quota legitimately never reports — counting it would make every gather on
// every ordinary account probe forever.
func TestStalestClaudeRowObservation_UnobservedRowIsNotStale(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-time.Minute)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ObservedAtMs: observed.UnixMilli(),
			UsageObserved: usageObservedPtr(true),
		},
		claudeWindowSevenDay: {
			UsedPercentage: 12, ObservedAtMs: observed.UnixMilli(),
			UsageObserved: usageObservedPtr(true),
		},
		// Fable: present as a heartbeat row only — a reset was seen, no percentage.
		claudeWindowSevenDayFable: {
			ObservedAtMs: now.UnixMilli(), UsageObserved: usageObservedPtr(false),
		},
	}
	if got := stalestClaudeRowObservation(buckets); !got.Equal(observed.UTC()) {
		t.Errorf("stalestClaudeRowObservation=%v, want %v (an unobserved row must not pin the snapshot stale)",
			got, observed.UTC())
	}
	if got := stalestClaudeRowObservation(map[string]claudeRateLimitBucket{}); !got.IsZero() {
		t.Errorf("empty cache should report the zero time, got %v", got)
	}
}

// A row still older than the newest PROBE-sourced reading in the cache is one
// the endpoint demonstrably does not supply — probing again cannot move it, so
// it must stop counting or the snapshot reads stale on every gather forever.
// Rows that probe DID write are stamped exactly at its instant and keep counting.
func TestStalestClaudeRowObservation_ExcludesRowsTheProbeCannotSupply(t *testing.T) {
	probedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	old := probedAt.Add(-48 * time.Hour)
	// A stream event from days ago naming a window the endpoint never returns.
	orphan := claudeRateLimitBucket{
		UsedPercentage: 5, ObservedAtMs: old.UnixMilli(),
		UsageObserved: usageObservedPtr(true), Source: claudeRateLimitSourceStream,
	}
	probed := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ObservedAtMs: probedAt.UnixMilli(),
			UsageObserved: usageObservedPtr(true), Source: claudeRateLimitSourceProbe,
		},
		claudeWindowSevenDay: {
			UsedPercentage: 12, ObservedAtMs: probedAt.UnixMilli(),
			UsageObserved: usageObservedPtr(true), Source: claudeRateLimitSourceProbe,
		},
		claudeWindowSevenDayFable: orphan,
	}
	if got := stalestClaudeRowObservation(probed); !got.Equal(probedAt.UTC()) {
		t.Errorf("stalestClaudeRowObservation=%v, want the probe's own instant %v", got, probedAt.UTC())
	}

	// With no probe-sourced reading on record nothing is excluded — a legacy
	// cache, or a device that has never probed, must still report itself stale.
	never := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ObservedAtMs: probedAt.UnixMilli(),
			UsageObserved: usageObservedPtr(true), Source: claudeRateLimitSourceStatusLine,
		},
		claudeWindowSevenDayFable: orphan,
	}
	if got := stalestClaudeRowObservation(never); !got.Equal(old.UTC()) {
		t.Errorf("stalestClaudeRowObservation=%v, want the 48h-old row %v while no probe is on record", got, old.UTC())
	}
}

// The cross-process dedupe has to answer the same per-row question the staleness
// TTL does. A status-line render answers only five_hour/seven_day, so treating it
// as "someone already observed" would let interactive renders suppress the probe
// the weekly-split row depends on — the reported staleness, reintroduced through
// the dedupe.
func TestClaudeUsageProbeObservedSince_StatusLineRenderCannotCoverAStaleWeeklyRow(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	resetClaudeUsageProbeGate()
	t.Cleanup(resetClaudeUsageProbeGate)

	now := time.Now()
	fresh := now.Add(-2 * time.Second)
	stale := now.Add(-6 * time.Hour)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ResetsAtMs: now.Add(3 * time.Hour).UnixMilli(),
			ObservedAtMs: fresh.UnixMilli(), usageKnown: true,
		},
	}, fresh, "", claudeRateLimitSourceStatusLine)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDayOpus: {
			UsedPercentage: 91, ResetsAtMs: now.Add(96 * time.Hour).UnixMilli(),
			ObservedAtMs: stale.UnixMilli(), usageKnown: true,
		},
	}, stale, "", claudeRateLimitSourceStream)

	if claudeUsageProbeObservedSince("", now.Add(-time.Minute)) {
		t.Error("a status-line render must not stand in for the weekly row it cannot refresh")
	}
}

// The forced-refresh join has to outlast the WHOLE probe, not just its request.
// A probe that spends its full HTTP timeout and then contends for the cache lock
// persists after the request deadline; a join that gave up there would fall
// through, be refused by the still-held single-flight slot, and sign the
// pre-probe cache milliseconds before the fresh reading lands.
//
// Pinned as an invariant rather than driven behaviourally: reproducing lock
// contention needs a second PROCESS holding the sibling .lock file, which no
// unit test here spawns.
func TestClaudeUsageProbeJoinTimeoutCoversPersistence(t *testing.T) {
	want := claudeUsageProbeTimeout + 2*claudeRateLimitCacheLockWait
	if claudeUsageProbeJoinTimeout < want {
		t.Errorf("claudeUsageProbeJoinTimeout=%v, want at least %v (request timeout + the cache-lock waits the merge can spend)",
			claudeUsageProbeJoinTimeout, want)
	}
}

// A probe reading must not leak any probe-only field into the signed receipt:
// canonicalCLIUsageRefreshReceipt carries observedAt and the numbers, nothing
// else the probe touched.
func TestCLIUsageRefreshReceipt_CarriesProbeObservationWithoutProbeFields(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":64,"resets_at":%d,"status":"allowed"}}`,
			now.Add(time.Hour).Unix())
	})
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	}

	usage := []cliAgentUsage{{
		Provider:   "claudeCode",
		Name:       "Claude Code",
		DataSource: "rate_limit_event",
		Metrics:    claudeCodeMetricsFromCache(now, ""),
	}}
	canonical, _, _, err := canonicalCLIUsageRefreshReceipt("refresh-1", now.UnixMilli(), true, usage, nil)
	if err != nil {
		t.Fatalf("canonicalCLIUsageRefreshReceipt: %v", err)
	}
	if !strings.Contains(string(canonical), observedAtRFC3339(now.UnixMilli())) {
		t.Errorf("canonical receipt should carry the advanced observation %q:\n%s",
			observedAtRFC3339(now.UnixMilli()), canonical)
	}
	for _, forbidden := range []string{claudeRateLimitSourceProbe, "source", "accessToken"} {
		if strings.Contains(string(canonical), forbidden) {
			t.Errorf("canonical receipt leaked %q:\n%s", forbidden, canonical)
		}
	}
}

// The gather path must reuse the credential the parser already decoded rather
// than reading the credential store a second time. This is not cosmetic: on
// macOS readClaudeCredentialsRaw shells out to `security` under a 3s timeout,
// and GatherCLIAgentUsageOnly runs every provider SERIALLY under one shared 10s
// context — a second spawn could push the Claude parser past the whole budget
// and drop every provider ordered after it from the signed refresh.
//
// Asserted by deleting the on-disk credential first: a probe that still succeeds
// can only have used the token it was handed.
func TestRefreshClaudeUsageIfStale_UsesTheCallersCredential(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	var seenAuth string
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":48,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	})
	if err := os.Remove(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")); err != nil {
		t.Fatal(err)
	}

	if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, "handed-in-token", "") {
		t.Fatal("the probe must run from the caller-supplied credential, with no second read")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
	if seenAuth != "Bearer handed-in-token" {
		t.Errorf("Authorization=%q, want the caller-supplied token", seenAuth)
	}
	if snap, ok := loadClaudeRateLimitSnapshot(cache); !ok || snap.Buckets[claudeWindowFiveHour].UsedPercentage != 48 {
		t.Errorf("probe reading did not land: ok=%v buckets=%+v", ok, snap.Buckets)
	}

}

// An already-cancelled gather must not burn the throttle slot: the next caller
// would otherwise be refused for a full minute because of a probe that never
// left the process. The shared 10s budget makes this reachable — Claude is one
// of several providers parsed serially under it.
func TestClaudeUsageProbe_CancelledContextDoesNotBurnTheThrottle(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":51,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if refreshClaudeUsageIfStale(dead, now, time.Time{}, probeTestToken, "") {
		t.Fatal("a cancelled gather must not probe")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0", got)
	}

	// The slot was never claimed, so a live caller one second later still runs
	// even though the minimum interval is a full minute.
	if !refreshClaudeUsageIfStale(context.Background(), now.Add(time.Second), time.Time{}, probeTestToken, "") {
		t.Error("the cancelled attempt must not have consumed the throttle slot")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

// The gather selects ParseContext through a TYPE ASSERTION on the value held in
// the parser registry, and a failed assertion is silent — the gather would fall
// back to Parse, whose context.Background() ignores the 10s budget every
// provider shares serially, letting the probe spend its full timeout after that
// budget is gone. Assert on the REGISTRY entry, not the concrete type: the
// registry value is what actually flows through runProviderParseSafely.
func TestClaudeCodeParser_RegistryEntryHonorsTheGatherDeadline(t *testing.T) {
	parser, ok := cliAgentUsageParserIndex()["claudeCode"]
	if !ok {
		t.Fatal("claudeCode parser is not registered")
	}
	contextParser, ok := parser.(cliAgentUsageContextParser)
	if !ok {
		t.Fatal("the registered claudeCode parser no longer satisfies cliAgentUsageContextParser — " +
			"the gather would silently fall back to Parse and stop honoring its deadline")
	}
	// Deliberately asserted on the registry entry rather than on a literal: the
	// registry stores POINTERS (&claudeCodeUsageParser{}), so a value-type
	// assertion would pass while proving nothing about what actually flows
	// through runProviderParseSafely.

	// And prove the deadline actually reaches the probe: with an already-expired
	// context, ParseContext must not issue a request.
	now := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":12,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	})
	stubClaudeProbes(t, true, true)

	expired, cancel := context.WithDeadline(context.Background(), now.Add(-time.Minute))
	defer cancel()
	if _, ok := contextParser.ParseContext(expired, t.TempDir(), detectedCLIAgent{Detected: true}, now); !ok {
		t.Fatal("ParseContext should still produce a usage entry on an expired context")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Errorf("probe request count=%d on an expired gather context, want 0", got)
	}
}

// An absent status must stay absent. bucketFromInfo leaves it empty for a stream
// event that omits it, so inventing "allowed" here would make the same window
// read differently depending on which writer observed it last.
func TestClaudeUsageProbeStatus_Normalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"allowed", "allowed"},
		{"rejected", claudeRateLimitStatusRejected},
		{"REJECTED", claudeRateLimitStatusRejected},
		{"  rejected  ", claudeRateLimitStatusRejected},
		{"allowed, but here is a long free-form vendor explanation nobody asked for", "allowed"},
		{strings.Repeat("x", 4096), "allowed"},
		{"réjeté — accentué, multi-byte, and long enough to have been truncated mid-rune", "allowed"},
	}
	for _, tc := range cases {
		if got := claudeUsageProbeStatus(tc.in); got != tc.want {
			t.Errorf("claudeUsageProbeStatus(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// A probe window carrying no status writes the same shape a stream capture would
// for the same window — asserted on the serialized cache, where an invented
// "allowed" would show up as a `status` key the stream writer never emits.
func TestClaudeUsageProbe_AbsentStatusIsNotPersisted(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":37,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	})

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"status"`) {
		t.Errorf("a window with no status must not gain one:\n%s", raw)
	}
	snap, _ := loadClaudeRateLimitSnapshot(cache)
	if got := snap.Buckets[claudeWindowFiveHour]; got.Status != "" || got.UsedPercentage != 37 {
		t.Errorf("bucket=%+v, want the reading with an empty status", got)
	}
}

// A PERSISTENT failure must back off. A transient one clears on the next
// attempt, but a persistent one does not — and it is reachable: a cache that
// never receives a reading never produces an observation, so the staleness check
// reports stale on EVERY gather. Paired with a flat minimum interval, an
// endpoint whose shape drifted away from the allow-list decode would be retried
// ~60 times/hour/device forever. The doubling settles that at the cap.
func TestClaudeUsageProbe_PersistentFailureBacksOff(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		// The un-self-healing shape: a 200 whose body the allow-list cannot plot.
		fmt.Fprint(w, `{"unknown_window":{"utilization":0.5}}`)
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "1000") // 1s base

	// First failure: retried after the flat minimum.
	if _, probeErr := runClaudeUsageProbe(context.Background(), now); probeErr == nil {
		t.Fatal("an unplottable body must be reported as a failure")
	}
	if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(1500*time.Millisecond)); probeErr == nil {
		t.Fatal("the first retry should still happen at the flat minimum")
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Fatalf("request count=%d, want 2", got)
	}

	// Two failures in: the interval has doubled, so the same 1.5s gap is refused.
	if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(3*time.Second)); probeErr != nil {
		t.Errorf("probe inside the backed-off interval should be a silent skip, got %+v", probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("request count=%d, want still 2 — the backoff must hold", got)
	}

	// Past the doubled interval it retries again.
	if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(6*time.Second)); probeErr == nil {
		t.Error("the probe must still retry once the backed-off interval elapses")
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Errorf("request count=%d, want 3", got)
	}
}

// A user-initiated refresh must cut through the backoff — the whole point of the
// force flag is that someone is watching the card right now.
func TestClaudeUsageProbe_ForceBypassesTheBackoff(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

	for i := 0; i < 3; i++ {
		SetClaudeUsageForceProbe(true)
		if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(time.Duration(i)*time.Second)); probeErr == nil {
			t.Fatalf("probe %d should have failed", i)
		}
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Errorf("request count=%d, want 3 — a forced refresh must bypass the backoff", got)
	}
}

// A success clears the streak, so one bad stretch cannot leave a healthy agent
// throttled at the cap.
func TestClaudeUsageProbe_SuccessClearsTheBackoff(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	var healthy atomic.Bool
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"five_hour":{"used_percentage":22,"resets_at":%d}}`, now.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "1000")

	// Two failures, so the interval is now 4s.
	if _, probeErr := runClaudeUsageProbe(context.Background(), now); probeErr == nil {
		t.Fatal("expected the first failure")
	}
	if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(2*time.Second)); probeErr == nil {
		t.Fatal("expected the second failure")
	}

	healthy.Store(true)
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now.Add(10*time.Second)); !refreshed || probeErr != nil {
		t.Fatalf("recovery probe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("request count=%d, want 3", got)
	}

	// Streak cleared: the flat 1s minimum applies again, not the backed-off one.
	if refreshed, _ := runClaudeUsageProbe(context.Background(), now.Add(12*time.Second)); !refreshed {
		t.Error("a success must clear the failure streak and restore the flat minimum")
	}
	if got := atomic.LoadInt64(calls); got != 4 {
		t.Errorf("request count=%d, want 4", got)
	}
}

// The backoff is bounded, and never shorter than the operator's configured
// minimum even when that minimum exceeds the cap.
func TestClaudeUsageProbeGate_IntervalBounds(t *testing.T) {
	t.Run("saturates at the cap", func(t *testing.T) {
		g := &claudeUsageProbeGate{failures: claudeUsageProbeMaxFailureStreak}
		if got := g.interval(); got != claudeUsageProbeMaxInterval {
			t.Errorf("interval=%v, want the %v cap", got, claudeUsageProbeMaxInterval)
		}
	})
	t.Run("no failures means the flat minimum", func(t *testing.T) {
		g := &claudeUsageProbeGate{}
		if got := g.interval(); got != claudeUsageProbeMinInterval {
			t.Errorf("interval=%v, want %v", got, claudeUsageProbeMinInterval)
		}
	})
	t.Run("a configured minimum above the cap is never shortened", func(t *testing.T) {
		t.Setenv(claudeUsageProbeMinIntervalEnv, "7200000") // 2h, above the 30m cap
		g := &claudeUsageProbeGate{failures: 8}
		if got := g.interval(); got != 2*time.Hour {
			t.Errorf("interval=%v, want the operator's 2h — the cap must not shorten it", got)
		}
	})
	t.Run("streak counter is bounded", func(t *testing.T) {
		g := &claudeUsageProbeGate{}
		for i := 0; i < claudeUsageProbeMaxFailureStreak*4; i++ {
			g.finish(claudeUsageProbeFailure(cliUsageErrorParseFailed), false, time.Time{})
		}
		if g.failures > claudeUsageProbeMaxFailureStreak {
			t.Errorf("failures=%d, want it capped at %d", g.failures, claudeUsageProbeMaxFailureStreak)
		}
	})
}

// resetClaudeUsageProbeGate must not return while a probe is still in flight.
//
// This is what keeps `go test` off the network. triggerClaudeUsageProbeAfterRun
// runs the probe on a goroutine, and probeClaudeUsage resolves the endpoint
// AFTER claiming the gate — so a goroutine descheduled between those two points
// outlives its test, resumes once t.Setenv has restored
// AIEXPEDITE_CLAUDE_USAGE_PROBE_URL, resolves the REAL endpoint, and sends the
// fixture token to api.anthropic.com. Draining in the reset seam closes that
// window, because cleanup runs it before the env restore.
func TestResetClaudeUsageProbeGate_DrainsInFlightProbe(t *testing.T) {
	release := make(chan struct{})
	served := make(chan struct{})
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		close(served)
		<-release
		fmt.Fprint(w, `{"five_hour":{"used_percentage":9,"resets_at":0}}`)
	})
	// Release the handler however this test exits. Without it a t.Fatal below
	// leaves the handler blocked, httptest's Close waits for it, and the FAILURE
	// path hangs instead of reporting — a hanging test in CI is worse than a
	// failing one. Registered after armClaudeUsageProbe so t.Cleanup's LIFO order
	// runs this before that helper's srv.Close.
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHandler)

	go func() {
		_, _ = runClaudeUsageProbe(context.Background(), time.Now())
	}()

	// Wait until the probe is genuinely mid-request, which is exactly the state
	// the drain has to cover.
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never reached the handler")
	}

	resetReturned := make(chan struct{})
	go func() {
		resetClaudeUsageProbeGate()
		close(resetReturned)
	}()

	select {
	case <-resetReturned:
		t.Fatal("resetClaudeUsageProbeGate returned while a probe was still in flight — " +
			"an escaping goroutine would resolve the real endpoint after cleanup restores the env")
	case <-time.After(150 * time.Millisecond):
		// Still blocked, as required.
	}

	releaseHandler()
	select {
	case <-resetReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("resetClaudeUsageProbeGate did not return after the probe finished")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

// The drain must be bounded: a probe that never releases the latch cannot be
// allowed to hang the whole suite in a cleanup.
//
// The real deadline is shortened here rather than waited out. CI runs this
// package under `go test -race -timeout 5m`, and sleeping the production five
// seconds to assert one boolean is wall-clock the whole suite has to pay for.
// Saving and restoring the package var is the same seam stubClaudeProbes uses.
func TestResetClaudeUsageProbeGate_DrainIsBounded(t *testing.T) {
	originalTimeout := claudeUsageProbeDrainTimeout
	claudeUsageProbeDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { claudeUsageProbeDrainTimeout = originalTimeout })

	// A latch nothing will ever release — the wedged-probe case.
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.inFlight = true
	claudeUsageProbe.mu.Unlock()
	t.Cleanup(func() {
		claudeUsageProbe.mu.Lock()
		claudeUsageProbe.inFlight = false
		claudeUsageProbe.mu.Unlock()
	})

	start := time.Now()
	done := make(chan struct{})
	go func() {
		resetClaudeUsageProbeGate()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resetClaudeUsageProbeGate never gave up on a wedged probe")
	}
	// Pin that the bound is the configured deadline, not some unrelated path
	// returning early — otherwise this test would still pass if the drain loop
	// were deleted entirely.
	if elapsed := time.Since(start); elapsed < claudeUsageProbeDrainTimeout {
		t.Errorf("gave up after %v, before the %v deadline — the drain did not run",
			elapsed, claudeUsageProbeDrainTimeout)
	}
}

/* -------------------------------------------------------------------------- */
/* Response-shape coverage: the limits[] representation                       */
/* -------------------------------------------------------------------------- */

// The current payload carries windows in a `limits[]` array, not as top-level
// objects. Decoding only the legacy shape is how this probe silently becomes a
// no-op — it would return "no plottable window" forever while the endpoint
// answered perfectly well, leaving latestObservedAt exactly as stale as the bug
// this branch exists to fix.
func TestClaudeUsageProbe_DecodesLimitsArray(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(2 * time.Hour)
	weekReset := now.Add(72 * time.Hour)

	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
			"limits": [
				{"type":"session","percent":31.5,"resets_at":%q,"status":"allowed"},
				{"type":"weekly_all","percent":64,"resets_at":%q,"status":"allowed"},
				{"type":"weekly_scoped","model":"claude-opus-4","percent":88,"resets_at":%q},
				{"type":"weekly_scoped","model":"claude-sonnet-4","percent":12,"resets_at":%q},
				{"type":"weekly_scoped","model":"fable-5","percent":7,"resets_at":%q},
				{"type":"some_future_meter","percent":99,"resets_at":%q},
				{"type":"weekly_scoped","model":"an-unmodelled-model","percent":50,"resets_at":%q}
			]
		}`,
			fiveReset.Format(time.RFC3339), weekReset.Format(time.RFC3339),
			weekReset.Format(time.RFC3339), weekReset.Format(time.RFC3339),
			weekReset.Format(time.RFC3339), weekReset.Format(time.RFC3339),
			weekReset.Format(time.RFC3339))
	})

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache write")
	}

	for _, tc := range []struct {
		window string
		want   float64
	}{
		{claudeWindowFiveHour, 31.5},
		{claudeWindowSevenDay, 64},
		{claudeWindowSevenDayOpus, 88},
		{claudeWindowSevenDaySonnet, 12},
		{claudeWindowSevenDayFable, 7},
	} {
		got, present := snap.Buckets[tc.window]
		if !present {
			t.Errorf("%s missing from the cache; got %+v", tc.window, snap.Buckets)
			continue
		}
		if got.UsedPercentage != tc.want {
			t.Errorf("%s UsedPercentage=%v, want %v", tc.window, got.UsedPercentage, tc.want)
		}
		if got.ResetsAtMs == 0 {
			t.Errorf("%s lost its reset stamp", tc.window)
		}
	}
	// Unmodelled entries are dropped, not guessed into a row.
	if len(snap.Buckets) != 5 {
		t.Errorf("cache holds %d windows, want 5 — unrecognized limits[] entries must be dropped: %+v",
			len(snap.Buckets), snap.Buckets)
	}

	// The Fable row is reachable ONLY through weekly_scoped on this shape, which
	// is the specific row a legacy-only decoder would leave permanently blank.
	fable := claudeCodeMetricsFromCache(now, "")[2]
	if fable.Unknown || fable.Consumed == nil || *fable.Consumed != 7 {
		t.Errorf("weekly Fable row=%+v, want an observed 7%%", fable)
	}
}

// Legacy top-level windows remain a fallback, so a rollback (or an account still
// served the old shape) keeps working.
func TestClaudeUsageProbe_FallsBackToLegacyWindows(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)

	t.Run("limits absent", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"five_hour":{"used_percentage":25,"resets_at":%d}}`, reset.Unix())
		})
		if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
			t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
		}
		snap, _ := loadClaudeRateLimitSnapshot(cache)
		if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 25 {
			t.Errorf("five_hour=%v, want 25 from the legacy field", got)
		}
	})

	t.Run("limits null and legacy windows null", func(t *testing.T) {
		_, _ = armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"limits":null,"five_hour":null,"seven_day":null}`)
		})
		refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
		if refreshed {
			t.Error("an all-null payload is not an observation")
		}
		if probeErr == nil || probeErr.ErrorCategory != cliUsageErrorParseFailed {
			t.Errorf("probeErr=%+v, want %q", probeErr, cliUsageErrorParseFailed)
		}
	})

	t.Run("limits wins where both describe a window", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{
				"limits":[{"type":"session","percent":70,"resets_at":%d}],
				"five_hour":{"used_percentage":10,"resets_at":%d}
			}`, reset.Unix(), reset.Unix())
		})
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now); !refreshed {
			t.Fatal("expected a reading")
		}
		snap, _ := loadClaudeRateLimitSnapshot(cache)
		if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 70 {
			t.Errorf("five_hour=%v, want 70 — the maintained shape wins", got)
		}
	})
}

/* -------------------------------------------------------------------------- */
/* Percentage scale                                                           */
/* -------------------------------------------------------------------------- */

// The OAuth usage endpoint reports percentages, NOT the stream's 0..1 fraction.
// Scaling a small value by 100 would turn a genuine 0.5% reading just after a
// reset into 50%, and 1% into 100% — reporting a nearly-empty quota as half or
// fully consumed.
func TestClaudeUsageProbeBucket_UtilizationIsAPercentage(t *testing.T) {
	for _, tc := range []struct {
		raw  float64
		want float64
	}{
		{0, 0},
		{0.5, 0.5},
		{1, 1},
		{42, 42},
		{99.9, 99.9},
		{100, 100},
		{140, 100}, // clamped
		{-5, 0},    // clamped
	} {
		util := tc.raw
		bucket, ok := claudeUsageProbeBucket(claudeUsageProbeWindow{Utilization: &util}, 0)
		if !ok {
			t.Errorf("utilization %v produced no reading", tc.raw)
			continue
		}
		if bucket.UsedPercentage != tc.want {
			t.Errorf("utilization %v -> %v, want %v", tc.raw, bucket.UsedPercentage, tc.want)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Daemon environment must not suppress the probe                             */
/* -------------------------------------------------------------------------- */

// Both launch paths strip CLAUDE_* and ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
// before spawning Claude, so a spawned run burns the STORED subscription login
// no matter what the daemon inherited. Skipping the probe on the daemon's own
// environment would leave a tray agent started from a shell that exports those
// vars running Claude against the stored account and never refreshing it.
func TestClaudeUsageProbe_InheritedEnvCredentialsDoNotSuppressTheProbe(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	for _, envVar := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK",
	} {
		t.Run(envVar, func(t *testing.T) {
			cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"limits":[{"type":"session","percent":44,"resets_at":%d}]}`,
					now.Add(time.Hour).Unix())
			})
			t.Setenv(envVar, "inherited-from-the-developer-shell")

			if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
				t.Fatalf("refreshed=%v err=%+v — the daemon environment says nothing about "+
					"which credential the spawned Claude used", refreshed, probeErr)
			}
			if got := atomic.LoadInt64(calls); got != 1 {
				t.Errorf("request count=%d, want 1", got)
			}
			snap, _ := loadClaudeRateLimitSnapshot(cache)
			if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 44 {
				t.Errorf("five_hour=%v, want the stored account reading", got)
			}
		})
	}

	// The stripping this relies on is real, not assumed.
	t.Run("the launch paths do strip those vars", func(t *testing.T) {
		env := []string{
			"ANTHROPIC_API_KEY=sk-secret",
			"ANTHROPIC_AUTH_TOKEN=tok",
			"CLAUDE_CODE_OAUTH_TOKEN=oauth",
			"CLAUDE_CODE_USE_BEDROCK=1",
			"PATH=/usr/bin",
		}
		filtered, _ := prepareClaudeChildEnv("claude", env)
		for _, e := range filtered {
			upper := strings.ToUpper(e)
			if strings.HasPrefix(upper, "ANTHROPIC_API_KEY=") ||
				strings.HasPrefix(upper, "ANTHROPIC_AUTH_TOKEN=") ||
				strings.HasPrefix(upper, "CLAUDE_CODE_OAUTH_TOKEN=") ||
				strings.HasPrefix(upper, "CLAUDE_CODE_USE_BEDROCK=") {
				t.Errorf("env credential survived into the child: %q", e)
			}
		}
	})
}

/* -------------------------------------------------------------------------- */
/* Persistence failures, rate-limit backpressure, cross-process dedupe        */
/* -------------------------------------------------------------------------- */

// A cache the probe cannot write is NOT a refresh. Reporting success would tell
// the caller to re-read unchanged data, clear the failure backoff, throttle the
// retry, and sign a receipt claiming an observation that was never persisted.
func TestClaudeUsageProbe_UnwritableCacheIsNotARefresh(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"type":"session","percent":33,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})

	// Point the cache at a path that cannot be created: an existing REGULAR file
	// standing where the parent directory would have to be.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(blocker, "sub", "rl.json"))

	refreshed, probeErr := runClaudeUsageProbe(context.Background(), now)
	if refreshed {
		t.Error("an unwritable cache must not be reported as a refresh")
	}
	if probeErr == nil || probeErr.ErrorCategory != cliUsageErrorCollectionFailed {
		t.Errorf("probeErr=%+v, want %q", probeErr, cliUsageErrorCollectionFailed)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

// A 429 must be honored, not merely counted as a failure. This endpoint is
// account-scoped and shared with every other poller on the account, so pressing
// on through explicit backpressure is how a fleet degrades it for everyone —
// including Claude's own /usage panel.
func TestClaudeUsageProbe_HonorsRetryAfter(t *testing.T) {
	now := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	if _, probeErr := runClaudeUsageProbe(context.Background(), now); probeErr == nil {
		t.Fatal("a 429 must be reported as a failure")
	}
	// Even a user-initiated refresh must respect an explicit hold.
	SetClaudeUsageForceProbe(true)
	if refreshed, _ := runClaudeUsageProbe(context.Background(), now.Add(30*time.Second)); refreshed {
		t.Error("probed again inside the Retry-After window")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1 — Retry-After was ignored", got)
	}

	// Past the hold it resumes.
	SetClaudeUsageForceProbe(true)
	if _, probeErr := runClaudeUsageProbe(context.Background(), now.Add(3*time.Minute)); probeErr == nil {
		t.Error("expected the probe to resume after the Retry-After window")
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("request count=%d, want 2", got)
	}
}

func TestRetryAfterDeadline(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if got := retryAfterDeadline("", now); !got.IsZero() {
		t.Errorf("empty header -> %v, want zero", got)
	}
	if got := retryAfterDeadline("not-a-value", now); !got.IsZero() {
		t.Errorf("garbage header -> %v, want zero", got)
	}
	if got := retryAfterDeadline("0", now); !got.IsZero() {
		t.Errorf("zero delta -> %v, want zero", got)
	}
	if got := retryAfterDeadline("90", now); !got.Equal(now.Add(90 * time.Second)) {
		t.Errorf("delta-seconds -> %v, want %v", got, now.Add(90*time.Second))
	}
	httpDate := now.Add(5 * time.Minute).UTC().Format(http.TimeFormat)
	if got := retryAfterDeadline(httpDate, now); got.IsZero() || got.Before(now) {
		t.Errorf("HTTP-date -> %v, want a future instant", got)
	}
	// A hostile value cannot park the probe for days.
	if got := retryAfterDeadline("999999999", now); !got.Equal(now.Add(claudeUsageProbeMaxRetryAfter)) {
		t.Errorf("oversized delta -> %v, want the %v cap", got, claudeUsageProbeMaxRetryAfter)
	}
	// A past date is not a hold.
	past := now.Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := retryAfterDeadline(past, now); !got.IsZero() {
		t.Errorf("past date -> %v, want zero", got)
	}
}

/* -------------------------------------------------------------------------- */
/* Shared-cache dedupe: baseline semantics                                    */
/* -------------------------------------------------------------------------- */

// A suppressed duplicate must not wedge the process. begin() sets the
// single-flight latch, so any early return that skips the deferred finish leaves
// it stuck and every future probe is rejected forever — a permanent outage, not
// a missed sample.
//
// Deliberately does NOT call resetClaudeUsageProbeGate between attempts: that
// seam is test-only with no production caller, and using it here is exactly what
// would hide the defect.
func TestClaudeUsageProbe_SuppressedDuplicateDoesNotWedgeTheGate(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":50,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "1000")

	// Another writer observed AFTER this run finished, so the post-run probe is a
	// genuine duplicate and is suppressed.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 21, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(2 * time.Second).UnixMilli(), usageKnown: true,
		},
	}, now.Add(2*time.Second), "", claudeRateLimitSourceProbe)

	if refreshed, _ := runClaudeUsageProbe(context.Background(), now); refreshed {
		t.Fatal("precondition: an observation newer than the run should suppress the probe")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0", got)
	}

	// A later run must still be able to probe. Without the latch released on the
	// suppressed path, this is rejected forever.
	later := now.Add(10 * time.Minute)
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), later); !refreshed {
		t.Fatalf("gate is wedged: the suppressed duplicate left inFlight set, so no probe can run "+
			"again in this process (refreshed=%v err=%+v)", refreshed, probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

// The acceptance criterion, against the dedupe. An observation recorded shortly
// BEFORE a run must never suppress the post-run probe: the run consumed quota
// after that reading was taken, so the reading cannot stand in for it. Getting
// this wrong would let a status-line render moments before a headless run cancel
// the probe that run earned — latestObservedAt would not advance, which is the
// exact defect this file exists to fix.
func TestClaudeUsageProbe_RecentPreRunObservationDoesNotSuppressPostRunProbe(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":73,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

	// A status-line render five seconds before the run finished — well inside the
	// 60s interval, so an interval-based dedupe would swallow the probe.
	preRun := now.Add(-5 * time.Second)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 40, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: preRun.UnixMilli(), usageKnown: true,
		},
	}, preRun, "", claudeRateLimitSourceStatusLine)

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v — a reading taken BEFORE the run cannot substitute "+
			"for the probe that run earned", refreshed, probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}

	after := claudeCodeMetricsFromCache(now, "")[0]
	if after.ObservedAt != observedAtRFC3339(now.UnixMilli()) {
		t.Errorf("ObservedAt=%q, want the post-run probe %q", after.ObservedAt, observedAtRFC3339(now.UnixMilli()))
	}
	if after.Consumed == nil || *after.Consumed != 73 {
		t.Errorf("Consumed=%v, want 73 from the post-run reading", after.Consumed)
	}
}

// The routine gather still dedupes on the interval — that is what keeps a second
// agent channel from doubling the request rate in steady state.
func TestRefreshClaudeUsageIfStale_DedupesAgainstAnotherWriter(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":50,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")

	// Another agent channel observed 10s ago.
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 21, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(-10 * time.Second).UnixMilli(), usageKnown: true,
		},
	}, now.Add(-10*time.Second), "", claudeRateLimitSourceProbe)

	// A stale-looking gather (latest passed as zero) still stands down, because
	// the SHARED cache says the account was just measured.
	if refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
		t.Error("a routine gather must stand down when another writer just observed")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Errorf("request count=%d, want 0", got)
	}

	// A user-initiated refresh is not deduped — somebody is looking at the card.
	SetClaudeUsageForceProbe(true)
	if !refreshClaudeUsageIfStale(context.Background(), now.Add(time.Second), time.Time{}, probeTestToken, "") {
		t.Error("a forced refresh must not be suppressed by the shared-cache check")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}

/* -------------------------------------------------------------------------- */
/* Real limits[] shape, and one credential read per probe                     */
/* -------------------------------------------------------------------------- */

// The live payload keys the discriminator as `kind` and carries the model in a
// nested scope OBJECT. Modelling `scope` as a string is not merely a missed
// mapping: encoding/json rejects the WHOLE response on the type mismatch, so a
// single mis-modelled field takes every window down with it.
func TestClaudeUsageProbe_DecodesLiveLimitsShape(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)

	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
			"limits": [
				{"kind":"session","percent":31.5,"resets_at":%q,"status":"allowed"},
				{"kind":"weekly_all","percent":64,"resets_at":%q},
				{"kind":"weekly_scoped","scope":{"model":{"display_name":"Claude Opus 4"}},"percent":88,"resets_at":%q},
				{"kind":"weekly_scoped","scope":{"model":{"display_name":"Claude Sonnet 4"}},"percent":12,"resets_at":%q},
				{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":7,"resets_at":%q},
				{"kind":"weekly_scoped","scope":{"model":{"display_name":"Some Future Model"}},"percent":50,"resets_at":%q},
				{"kind":"some_future_meter","percent":99,"resets_at":%q}
			]
		}`,
			reset.Format(time.RFC3339), reset.Format(time.RFC3339), reset.Format(time.RFC3339),
			reset.Format(time.RFC3339), reset.Format(time.RFC3339), reset.Format(time.RFC3339),
			reset.Format(time.RFC3339))
	})

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("runClaudeUsageProbe: refreshed=%v err=%+v", refreshed, probeErr)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected a cache write")
	}
	for _, tc := range []struct {
		window string
		want   float64
	}{
		{claudeWindowFiveHour, 31.5},
		{claudeWindowSevenDay, 64},
		{claudeWindowSevenDayOpus, 88},
		{claudeWindowSevenDaySonnet, 12},
		{claudeWindowSevenDayFable, 7},
	} {
		got, present := snap.Buckets[tc.window]
		if !present {
			t.Errorf("%s missing; got %+v", tc.window, snap.Buckets)
			continue
		}
		if got.UsedPercentage != tc.want {
			t.Errorf("%s=%v, want %v", tc.window, got.UsedPercentage, tc.want)
		}
	}
	if len(snap.Buckets) != 5 {
		t.Errorf("cache holds %d windows, want 5 — unmodelled entries must drop: %+v",
			len(snap.Buckets), snap.Buckets)
	}
	// Fable is reachable only through weekly_scoped on this shape.
	if fable := claudeCodeMetricsFromCache(now, "")[2]; fable.Unknown || fable.Consumed == nil || *fable.Consumed != 7 {
		t.Errorf("weekly Fable row=%+v, want an observed 7%%", fable)
	}
}

// No shape of the scope/model fields may fail the enclosing decode. A field we
// cannot interpret must cost one row, never the whole response.
func TestClaudeUsageProbeLabel_ToleratesEveryShape(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{`"Fable"`, "Fable"},
		{`{"model":{"display_name":"Fable"}}`, "Fable"},
		{`{"model":"Opus"}`, "Opus"},
		{`{"display_name":"Sonnet"}`, "Sonnet"},
		{`{"name":"opus-4"}`, "opus-4"},
		{`null`, ""},
		{`12345`, ""},
		{`["unexpected","array"]`, ""},
		{`{"model":{"unknown_key":1}}`, ""},
	} {
		var label claudeUsageProbeLabel
		if err := label.UnmarshalJSON([]byte(tc.raw)); err != nil {
			t.Errorf("%s returned an error (%v) — it must never fail the enclosing decode", tc.raw, err)
		}
		if label.Label != tc.want {
			t.Errorf("%s -> %q, want %q", tc.raw, label.Label, tc.want)
		}
	}

	// End to end: an unreadable scope drops its own entry and nothing else.
	var decoded claudeUsageProbeResponse
	body := `{"limits":[
		{"kind":"session","percent":10},
		{"kind":"weekly_scoped","scope":["not","an","object"],"percent":20},
		{"kind":"weekly_all","percent":30}
	]}`
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("a malformed scope must not fail the whole response: %v", err)
	}
	buckets := claudeUsageProbeBuckets(decoded, time.Unix(0, 0))
	if len(buckets) != 2 {
		t.Errorf("got %d windows, want 2 (session + weekly_all survive): %+v", len(buckets), buckets)
	}
}

// One credential read per probe. On a default macOS config that read shells out
// to `security` under a 3s timeout, and the providers share a 10s budget
// serially — deriving the fingerprint separately would put two or three of those
// around a single network call.
func TestClaudeUsageProbe_ReadsTheCredentialStoreOnce(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":18,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})

	// Force the Keychain path (default config dir) and count the reads.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var reads int64
	original := claudeKeychainReader
	claudeKeychainReader = func() ([]byte, bool) {
		atomic.AddInt64(&reads, 1)
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, probeTestToken)), true
	}
	t.Cleanup(func() { claudeKeychainReader = original })

	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&reads); got != 1 {
		t.Errorf("credential store read %d times, want exactly 1 per probe", got)
	}
}

// The gather path supplies both values from the read the parser already did, so
// a probe there touches the credential store zero further times.
func TestRefreshClaudeUsageIfStale_MakesNoCredentialRead(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":18,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})

	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var reads int64
	original := claudeKeychainReader
	claudeKeychainReader = func() ([]byte, bool) {
		atomic.AddInt64(&reads, 1)
		return nil, false
	}
	t.Cleanup(func() { claudeKeychainReader = original })

	if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
		t.Fatal("expected the gather-path probe to run from the supplied credential")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
	if got := atomic.LoadInt64(&reads); got != 0 {
		t.Errorf("credential store read %d times on the gather path, want 0 — the parser already read it", got)
	}
}

/* -------------------------------------------------------------------------- */
/* The throttle may DELAY a post-run probe, never discard it                  */
/* -------------------------------------------------------------------------- */

// A probe shortly BEFORE a run must not make that run's refresh vanish.
// begin() checks the process-wide minimum interval before any baseline is
// consulted, so a routine gather thirty seconds earlier would otherwise drop the
// post-run probe outright — and the gather path then treats that pre-run reading
// as fresh for the whole staleness TTL, leaving latestObservedAt behind the run
// for minutes. That is the reported defect, reintroduced by the rate bound.
func TestClaudeUsageProbe_ThrottledPostRunProbeStillLands(t *testing.T) {
	base := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":77,"resets_at":%d}]}`,
			base.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "400")

	// A routine gather probes successfully just before the run.
	if !refreshClaudeUsageIfStale(context.Background(), time.Now(), time.Time{}, probeTestToken, "") {
		t.Fatal("precondition: the pre-run gather should probe")
	}
	if latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")).IsZero() {
		t.Fatal("precondition: expected a pre-run observation")
	}

	// The run completes INSIDE the throttle window.
	runCompleted := time.Now()
	triggerClaudeUsageProbeAfterRun()

	waitForObservationAfter(t, runCompleted, 8*time.Second,
		"latestObservedAt never advanced past the run — the throttle discarded the post-run probe")
	if got := atomic.LoadInt64(calls); got > 3 {
		t.Errorf("request count=%d, want a small bounded number", got)
	}
}

// A burst of runs inside one throttle window is one DEBT, not one probe each:
// the newest baseline subsumes the older ones and only one trailing timer runs.
func TestClaudeUsageProbe_TrailingProbesCoalesceAcrossRuns(t *testing.T) {
	base := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":61,"resets_at":%d}]}`,
			base.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "600")

	if !refreshClaudeUsageIfStale(context.Background(), time.Now(), time.Time{}, probeTestToken, "") {
		t.Fatal("precondition: the first probe should run")
	}

	// Eight runs finish inside the window.
	last := time.Now()
	for i := 0; i < 8; i++ {
		last = time.Now()
		triggerClaudeUsageProbeAfterRun()
		time.Sleep(5 * time.Millisecond)
	}

	waitForObservationAfter(t, last, 8*time.Second, "the coalesced trailing probe never landed")
	// One pre-run probe plus one trailing probe. Anything near eight means the
	// runs each bought their own request.
	if got := atomic.LoadInt64(calls); got > 3 {
		t.Errorf("request count=%d, want <= 3 — a burst of runs must coalesce onto one trailing probe", got)
	}
}

// When the trailing probe is too far out to hold a timer for — a deep backoff or
// a long Retry-After — the debt is still recorded, and the next routine gather
// pays it instead of honoring the staleness TTL.
func TestRefreshClaudeUsageIfStale_PaysAnOutstandingPostRunDebt(t *testing.T) {
	now := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":52,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})

	// A reading from just now: comfortably inside the staleness TTL, so an
	// unaware gather would stand down.
	preRun := now.Add(-time.Second)
	mergeClaudeRateLimitCacheFromSource(os.Getenv("AIEXPEDITE_CLAUDE_RL_CACHE"),
		map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				UsedPercentage: 30, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
				ObservedAtMs: preRun.UnixMilli(), usageKnown: true,
			},
		}, preRun, "", claudeRateLimitSourceProbe)

	// Record a debt for a run that finished after that reading.
	runCompleted := now
	claudeUsageProbe.recordOwed(runCompleted)

	if !refreshClaudeUsageIfStale(context.Background(), now.Add(time.Second),
		latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")), probeTestToken, "") {
		t.Fatal("a gather must pay an outstanding post-run debt rather than trusting the staleness TTL")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
	if latest := latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")); !latest.After(runCompleted) {
		t.Errorf("observation %v did not advance past the run %v", latest, runCompleted)
	}
	// Debt paid: the next gather goes back to the ordinary TTL.
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("debt still outstanding (%v) after a successful probe", owed)
	}
}

// An observation that arrives from another writer between the run and the
// trailing probe settles the debt without spending a request.
func TestClaudeUsageProbe_DebtSettledByAnotherWriterSkipsTheTrailingProbe(t *testing.T) {
	now := time.Now()
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":12,"resets_at":%d}]}`,
			now.Add(time.Hour).Unix())
	})

	runCompleted := now
	// The status-line hook records a reading after the run finished.
	after := runCompleted.Add(50 * time.Millisecond)
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 45, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: after.UnixMilli(), usageKnown: true,
		},
	}, after, "", claudeRateLimitSourceStatusLine)

	claudeUsageProbeAfterRun(runCompleted)
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Errorf("request count=%d, want 0 — another writer already answered for this run", got)
	}
}

// waitForObservationAfter polls the merged cache until an observation newer than
// `baseline` appears. The trailing probe is asynchronous by design, so polling
// is the only honest way to assert it ran.
func waitForObservationAfter(t *testing.T, baseline time.Time, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if latest := latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")); latest.After(baseline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s (baseline %s)", msg, baseline.UTC().Format(time.RFC3339Nano))
}

/* -------------------------------------------------------------------------- */
/* Debt survives failure; timer slot is never leaked; one credential read      */
/* -------------------------------------------------------------------------- */

// countClaudeCredentialReads forces the Keychain path and counts reads of the
// credential store, returning the counter. On a default macOS config each read
// spawns `security` under a 3s timeout, inside a 10s budget shared serially by
// every provider — so the count is a real cost, not bookkeeping.
func countClaudeCredentialReads(t *testing.T) *int64 {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	var reads int64
	original := claudeKeychainReader
	claudeKeychainReader = func() ([]byte, bool) {
		atomic.AddInt64(&reads, 1)
		return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q}}`, probeTestToken)), true
	}
	t.Cleanup(func() { claudeKeychainReader = original })
	return &reads
}

// The post-run wrapper must not resolve credentials during per-run preflight.
// A burst that coalesces its HTTP requests but still pays a Keychain spawn per
// run has only moved the cost.
func TestClaudeUsageProbeAfterRun_ReadsCredentialsOncePerActualProbe(t *testing.T) {
	base := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":39,"resets_at":%d}]}`,
			base.Add(time.Hour).Unix())
	})
	t.Setenv(claudeUsageProbeMinIntervalEnv, "500")
	reads := countClaudeCredentialReads(t)

	// One run probes immediately; nine more land inside the throttle window and
	// must coalesce onto a single trailing probe.
	var last time.Time
	for i := 0; i < 10; i++ {
		last = time.Now()
		claudeUsageProbeAfterRunAsyncForTest(last)
		time.Sleep(3 * time.Millisecond)
	}
	waitForObservationAfter(t, last, 8*time.Second, "the coalesced trailing probe never landed")

	if got := atomic.LoadInt64(calls); got > 2 {
		t.Errorf("request count=%d, want <= 2 (immediate + one trailing)", got)
	}
	// One read per probe that actually happened — never one per run.
	if got := atomic.LoadInt64(reads); got > 2 {
		t.Errorf("credential store read %d times across 10 runs, want <= 2 — the per-run "+
			"preflight must not resolve credentials", got)
	}
}

// A debt must be recorded BEFORE the attempt, so a probe that is refused
// (offline) or that fails (network/decode/cache) still leaves the run's refresh
// owed. Otherwise a recent pre-run reading suppresses routine recovery for the
// whole staleness TTL.
func TestClaudeUsageProbeAfterRun_DebtSurvivesRefusalAndFailure(t *testing.T) {
	now := time.Now()

	t.Run("offline refusal keeps the debt", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"limits":[{"kind":"session","percent":10}]}`)
		})
		SetOffline(true)
		t.Cleanup(func() { SetOffline(false) })

		completed := time.Now()
		claudeUsageProbeAfterRun(completed)
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Fatalf("request count=%d, want 0 while offline", got)
		}
		owed := claudeUsageProbe.owedObservation()
		if owed.IsZero() || owed.Before(completed) {
			t.Errorf("owed=%v, want the run completion retained across an offline refusal", owed)
		}
	})

	t.Run("transient failure keeps the debt, and a later gather pays it", func(t *testing.T) {
		var healthy atomic.Bool
		cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
			if !healthy.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":66,"resets_at":%d}]}`,
				now.Add(time.Hour).Unix())
		})
		t.Setenv(claudeUsageProbeMinIntervalEnv, "0")

		// A fresh pre-run reading: an unaware gather would trust it for 10 minutes.
		preRun := time.Now().Add(-time.Second)
		mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {
				UsedPercentage: 30, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
				ObservedAtMs: preRun.UnixMilli(), usageKnown: true,
			},
		}, preRun, "", claudeRateLimitSourceProbe)

		completed := time.Now()
		claudeUsageProbeAfterRun(completed) // 500s, fails
		if got := atomic.LoadInt64(calls); got == 0 {
			t.Fatal("expected the post-run probe to be attempted")
		}
		if owed := claudeUsageProbe.owedObservation(); owed.IsZero() {
			t.Fatal("a failed probe must leave the debt standing")
		}

		// The endpoint recovers; the next routine gather must pay the debt rather
		// than trusting the recent pre-run reading.
		healthy.Store(true)
		latest := latestClaudeObservation(loadMergedClaudeRateLimitBuckets(""))
		if !refreshClaudeUsageIfStale(context.Background(), time.Now(), latest, probeTestToken, "") {
			t.Fatal("the gather must pay the outstanding debt despite a recent pre-run reading")
		}
		if got := latestClaudeObservation(loadMergedClaudeRateLimitBuckets("")); !got.After(completed) {
			t.Errorf("observation %v did not advance past the run %v", got, completed)
		}
		if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
			t.Errorf("debt %v still outstanding after a successful probe", owed)
		}
	})
}

// A long-wait debt records without reserving the timer. Reserving a slot that no
// timer will ever release would permanently disable trailing probes: every later
// run would see the slot held and decline to schedule.
func TestClaudeUsageProbeAfterRun_LongWaitDoesNotLeakTheTimerSlot(t *testing.T) {
	base := time.Now()
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":48,"resets_at":%d}]}`,
			base.Add(time.Hour).Unix())
	})

	// An interval far beyond the trailing-wait cap, so the debt is recorded but
	// no timer is created.
	t.Setenv(claudeUsageProbeMinIntervalEnv, "3600000") // 1h
	if !refreshClaudeUsageIfStale(context.Background(), time.Now(), time.Time{}, probeTestToken, "") {
		t.Fatal("precondition: the first probe should run")
	}
	claudeUsageProbeAfterRun(time.Now()) // wait ~1h > cap: record only

	if claudeUsageProbe.trailingScheduled {
		t.Error("the long-wait path must not reserve a timer slot it will never release")
	}

	// A gather pays the long-wait debt.
	SetClaudeUsageForceProbe(true)
	if !refreshClaudeUsageIfStale(context.Background(), time.Now(), time.Time{}, probeTestToken, "") {
		t.Fatal("a forced gather should pay the long-wait debt")
	}

	// Now a normal throttled run must still be able to schedule a trailing probe.
	// With the slot leaked, this never fires.
	t.Setenv(claudeUsageProbeMinIntervalEnv, "400")
	last := time.Now()
	claudeUsageProbeAfterRunAsyncForTest(last)
	waitForObservationAfter(t, last, 8*time.Second,
		"no trailing probe was scheduled after a long-wait debt — the timer slot leaked")
	if got := atomic.LoadInt64(calls); got > 4 {
		t.Errorf("request count=%d, want a small bounded number", got)
	}
}

// claudeUsageProbeAfterRunAsyncForTest mirrors what triggerClaudeUsageProbeAfterRun
// does in production — run the post-run path on its own goroutine — without the
// armed-gate precheck the tests set up explicitly.
func claudeUsageProbeAfterRunAsyncForTest(completedAt time.Time) {
	go func() {
		defer func() { _ = recover() }()
		claudeUsageProbeAfterRun(completedAt)
	}()
}

// A user-initiated refresh that lands while a post-run probe is already on the
// wire must JOIN it, not be turned away by the single-flight latch. Being turned
// away is what let the Refresh button sign a receipt from the buckets loaded
// before the answer arrived — reporting the pre-run observation while the fresh
// one landed milliseconds later.
func TestClaudeUsageProbe_ForcedRefreshJoinsInFlightProbe(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	var entered sync.Once
	inFlight := make(chan struct{})
	release := make(chan struct{})

	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		entered.Do(func() { close(inFlight) })
		<-release
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"used_percentage":33,"resets_at":%d,"status":"allowed"}`,
				now.Add(3*time.Hour).Unix()),
		}))
	})

	// The post-run probe, still awaiting its response.
	go func() { _, _ = runClaudeUsageProbe(context.Background(), now) }()
	<-inFlight

	// The user presses Refresh mid-flight.
	SetClaudeUsageForceProbe(true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
		t.Fatal("a forced refresh must join the in-flight probe and report the fresh reading")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1 — the forced refresh must join, not duplicate", got)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok || snap.Buckets[claudeWindowFiveHour].ObservedAtMs != now.UnixMilli() {
		t.Error("the joined probe's reading must be on disk before the refresh reports success")
	}
	if claudeUsageProbe.forced() {
		t.Error("the join must consume the force flag, not leave a bypass pending")
	}
}

// Joining is not blind deference: an in-flight probe that writes nothing leaves
// the forced refresh to issue its own request, the slot now free.
func TestClaudeUsageProbe_ForcedRefreshRetriesAfterFailedInFlightProbe(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	var entered sync.Once
	inFlight := make(chan struct{})
	release := make(chan struct{})

	var seen int64
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&seen, 1) == 1 { // the in-flight probe: fail it
			entered.Do(func() { close(inFlight) })
			<-release
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"used_percentage":61,"resets_at":%d,"status":"allowed"}`,
				now.Add(3*time.Hour).Unix()),
		}))
	})

	go func() { _, _ = runClaudeUsageProbe(context.Background(), now) }()
	<-inFlight

	SetClaudeUsageForceProbe(true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken, "") {
		t.Fatal("a forced refresh must fall through to its own probe when the joined one wrote nothing")
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("request count=%d, want 2 — the failed probe must not stand in for the refresh", got)
	}
	snap, _ := loadClaudeRateLimitSnapshot(cache)
	if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 61 {
		t.Errorf("UsedPercentage=%v, want the forced probe's 61", got)
	}
}

// A joined probe that started BEFORE the run does not pay the run's debt. It
// persisted a reading, but a PRE-run one: settling with it would sign the
// pre-run observation into the receipt and leave the already-scheduled trailing
// probe to exit finding nothing owed — the freeze the feature exists to fix,
// reintroduced by the join.
func TestClaudeUsageProbe_ForcedRefreshDoesNotSettleRunDebtWithPreRunJoin(t *testing.T) {
	runAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	preRun := runAt.Add(-30 * time.Second) // the in-flight probe's observation
	postRun := runAt.Add(2 * time.Second)  // our own, after the join is refused

	var entered sync.Once
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var seen int64
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&seen, 1) == 1 { // the pre-run probe, still on the wire
			entered.Do(func() { close(inFlight) })
			<-release
			fmt.Fprint(w, probeUsageJSON(map[string]string{
				claudeWindowFiveHour: fmt.Sprintf(`{"used_percentage":21,"resets_at":%d,"status":"allowed"}`,
					runAt.Add(3*time.Hour).Unix()),
			}))
			return
		}
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"used_percentage":73,"resets_at":%d,"status":"allowed"}`,
				runAt.Add(3*time.Hour).Unix()),
		}))
	})

	// A probe that was already in flight when the run finished.
	go func() { _, _ = runClaudeUsageProbe(context.Background(), preRun) }()
	<-inFlight

	// The run completes, then the user presses Refresh mid-flight.
	claudeUsageProbe.recordOwed(runAt)
	SetClaudeUsageForceProbe(true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if !refreshClaudeUsageIfStale(context.Background(), postRun, time.Time{}, probeTestToken, "") {
		t.Fatal("the forced refresh must still produce a reading of its own")
	}

	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("request count=%d, want 2 — a pre-run reading cannot answer a post-run refresh", got)
	}
	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("no snapshot on disk")
	}
	if got := snap.Buckets[claudeWindowFiveHour].ObservedAtMs; got != postRun.UnixMilli() {
		t.Errorf("ObservedAtMs=%d, want the post-run %d", got, postRun.UnixMilli())
	}
	if got := claudeUsageProbe.owedObservation(); !got.IsZero() {
		t.Errorf("owedObservation=%s, want the debt settled by the post-run reading", got)
	}
}

// The converse, so the guard above is a check on the OBSERVATION and not a
// blanket refusal to join while a debt is outstanding: a probe whose reading is
// newer than the run pays it, with one request.
func TestClaudeUsageProbe_ForcedRefreshSettlesRunDebtWithPostRunJoin(t *testing.T) {
	runAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	postRun := runAt.Add(time.Second)

	var entered sync.Once
	inFlight := make(chan struct{})
	release := make(chan struct{})
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		entered.Do(func() { close(inFlight) })
		<-release
		fmt.Fprint(w, probeUsageJSON(map[string]string{
			claudeWindowFiveHour: fmt.Sprintf(`{"used_percentage":49,"resets_at":%d,"status":"allowed"}`,
				runAt.Add(3*time.Hour).Unix()),
		}))
	})

	claudeUsageProbe.recordOwed(runAt)
	go func() { _, _ = runClaudeUsageProbe(context.Background(), postRun) }()
	<-inFlight

	SetClaudeUsageForceProbe(true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if !refreshClaudeUsageIfStale(context.Background(), postRun, time.Time{}, probeTestToken, "") {
		t.Fatal("a post-run reading already on the wire must answer the refresh")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1 — the joined reading covers the debt", got)
	}
	if got := claudeUsageProbe.owedObservation(); !got.IsZero() {
		t.Errorf("owedObservation=%s, want the debt settled by the joined reading", got)
	}
	snap, _ := loadClaudeRateLimitSnapshot(cache)
	if got := snap.Buckets[claudeWindowFiveHour].UsedPercentage; got != 49 {
		t.Errorf("UsedPercentage=%v, want the joined probe's 49", got)
	}
}

// The join is bounded by the caller's context: a cancelled gather must not sit
// waiting on a probe whose answer belongs to the next read.
func TestClaudeUsageProbe_ForcedRefreshJoinHonorsContext(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	var entered sync.Once
	inFlight := make(chan struct{})
	release := make(chan struct{})
	// A plain defer, NOT t.Cleanup: cleanups run AFTER the test body returns and
	// LIFO, so armClaudeUsageProbe's httptest shutdown would go first and block
	// forever on the handler still parked here.
	defer close(release)

	_, _ = armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		entered.Do(func() { close(inFlight) })
		<-release
	})

	go func() { _, _ = runClaudeUsageProbe(context.Background(), now) }()
	<-inFlight

	SetClaudeUsageForceProbe(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if refreshClaudeUsageIfStale(ctx, now, time.Time{}, probeTestToken, "") {
		t.Error("a cancelled gather must not report a refresh")
	}
	if elapsed := time.Since(start); elapsed > claudeUsageProbeJoinTimeout {
		t.Errorf("join took %v, want it abandoned as soon as the context was done", elapsed)
	}
}
