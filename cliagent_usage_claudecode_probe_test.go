package main

// Tests for the bounded Claude utilization probe. Everything runs against an
// httptest server pinned through AIEXPEDITE_CLAUDE_USAGE_PROBE_URL, so no test
// here reaches the real endpoint — and the probe is opt-IN per process
// (claudeUsageProbeGate.armed), so a test that forgets to arm it makes no call
// at all rather than one to api.anthropic.com.

import (
	"context"
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
			claudeWindowFiveHour: fmt.Sprintf(`{"utilization":0.42,"resets_at":%q,"status":"allowed"}`,
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
		t.Errorf("five_hour UsedPercentage=%v, want ~42 (utilization 0.42)", five.UsedPercentage)
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
			claudeWindowFiveHour: fmt.Sprintf(`{"utilization":0.6,"resets_at":%d,"status":"allowed"}`, reset.Unix()),
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

	t.Run("env credential outranks the stored login", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, ok)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
		if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now); refreshed || probeErr != nil {
			t.Errorf("refreshed=%v err=%+v, want a silent skip", refreshed, probeErr)
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0 — env-account usage has no card here", got)
		}
	})

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

		// A user-initiated refresh bypasses the interval — exactly once.
		SetClaudeUsageForceProbe(true)
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now.Add(2*time.Second)); !refreshed {
			t.Error("a forced probe must bypass the minimum interval")
		}
		if refreshed, _ := runClaudeUsageProbe(context.Background(), now.Add(3*time.Second)); refreshed {
			t.Error("the force flag must be consumed by one probe")
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
		if refreshClaudeUsageIfStale(context.Background(), now, now.Add(-time.Minute), probeTestToken) {
			t.Error("a reading a minute old must not spend a probe")
		}
		if got := atomic.LoadInt64(calls); got != 0 {
			t.Errorf("request count=%d, want 0", got)
		}
	})

	t.Run("stale observation is probed", func(t *testing.T) {
		_, calls := armClaudeUsageProbe(t, body)
		if !refreshClaudeUsageIfStale(context.Background(), now, now.Add(-claudeUsageProbeStaleAfter-time.Minute), probeTestToken) {
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
		if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken) {
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

		if !refreshClaudeUsageIfStale(context.Background(), now, stale, probeTestToken) {
			t.Fatal("the first stale gather should probe")
		}
		for i := 0; i < 3; i++ {
			if refreshClaudeUsageIfStale(context.Background(), now.Add(time.Duration(i+1)*time.Second), stale, probeTestToken) {
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
		if !refreshClaudeUsageIfStale(context.Background(), now, now.Add(-time.Minute), probeTestToken) {
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
		if refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, probeTestToken) {
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

	if !refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, "handed-in-token") {
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

	// The env-auth guard still applies to the caller-supplied path — a token in
	// hand must not bypass the rule that env-account usage has no card here.
	resetClaudeUsageProbeGate()
	SetClaudeUsageProbeDisabled(false)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "env-token")
	if refreshClaudeUsageIfStale(context.Background(), now, time.Time{}, "handed-in-token") {
		t.Error("env credentials outrank the stored login even when a token was passed in")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want still 1", got)
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
	if refreshClaudeUsageIfStale(dead, now, time.Time{}, probeTestToken) {
		t.Fatal("a cancelled gather must not probe")
	}
	if got := atomic.LoadInt64(calls); got != 0 {
		t.Fatalf("request count=%d, want 0", got)
	}

	// The slot was never claimed, so a live caller one second later still runs
	// even though the minimum interval is a full minute.
	if !refreshClaudeUsageIfStale(context.Background(), now.Add(time.Second), time.Time{}, probeTestToken) {
		t.Error("the cancelled attempt must not have consumed the throttle slot")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d, want 1", got)
	}
}
