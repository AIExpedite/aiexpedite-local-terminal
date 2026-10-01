package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// renewAntigravityStoredLogin's contract, without a real `agy` or keyring: the
// `agy models` child is the runner seam, the keyring is a token whose expiry
// the "child" moves.

// helperMutableAntigravityKeyring stubs the keyring with a stored login whose
// expiry the test (or a stub `agy models`) can move, and returns the setter.
func helperMutableAntigravityKeyring(t *testing.T, expiry time.Time) func(time.Time) {
	t.Helper()
	orig := antigravityKeyringReader
	t.Cleanup(func() { antigravityKeyringReader = orig })
	var mu sync.Mutex
	antigravityKeyringReader = func(context.Context) ([]byte, bool) {
		mu.Lock()
		defer mu.Unlock()
		raw, _ := json.Marshal(map[string]any{
			"access_token": "access-renew-secret", "token_type": "Bearer", "refresh_token": "never-read",
			"expiry": expiry.Format(time.RFC3339Nano),
		})
		return raw, true
	}
	return func(next time.Time) { mu.Lock(); expiry = next; mu.Unlock() }
}

// helperIsolateLoginRenewal resets the renewal's process-wide state and points
// detection at a fixed CLI, and returns a counter of `agy models` children.
// runner, when set, is the child's body; it gets the start hook's context.
func helperIsolateLoginRenewal(t *testing.T, runner func(ctx context.Context) (string, bool)) *atomic.Int32 {
	t.Helper()
	resetAntigravityLoginRenewal()
	resetCLIAgentModelProbeCache()
	origRunner, origDetected := cliAgentModelProbeRunner, antigravityLoginRenewDetectedFn
	t.Cleanup(func() {
		cliAgentModelProbeRunner, antigravityLoginRenewDetectedFn = origRunner, origDetected
		resetAntigravityLoginRenewal()
		resetCLIAgentModelProbeCache()
	})
	antigravityLoginRenewDetectedFn = func() (detectedCLIAgent, bool) {
		return detectedCLIAgent{Detected: true, Path: "/opt/agy/bin/agy", Version: "1.2.3"}, true
	}
	var spawned atomic.Int32
	cliAgentModelProbeRunner = func(ctx context.Context, executable string, _ []string, args ...string) (string, bool) {
		spawned.Add(1)
		if !strings.HasSuffix(executable, "agy") || len(args) != 1 || args[0] != "models" {
			t.Errorf("unexpected child %s %v", executable, args)
		}
		if runner != nil {
			return runner(ctx)
		}
		return realAntigravityModels, true
	}
	return &spawned
}

// The child is the agent's OWN `agy`: it is counted as running while it runs,
// its PID is registered through the start hook, and the run log it writes is
// claimed as owned — never a candidate, never owed.
func TestRenewAntigravityStoredLogin_ChildIsOwnAndItsLogIsNeverOwed(t *testing.T) {
	h := helperIsolateLogIndex(t)
	setExpiry := helperMutableAntigravityKeyring(t, time.Now().Add(-time.Minute))
	var runningDuringChild bool
	spawned := helperIsolateLoginRenewal(t, func(ctx context.Context) (string, bool) {
		runningDuringChild = antigravityOwnChildRunning()
		processStartHookFrom(ctx)(7101)
		at := time.Now()
		h.write(t, helperLogName(at), helperPIDBlock(7101), at, false)
		setExpiry(time.Now().Add(time.Hour))
		return realAntigravityModels, true
	})

	if got := renewAntigravityStoredLogin(context.Background(), time.Now()); got != antigravityLoginRenewed {
		t.Fatalf("result=%q, want renewed", got)
	}
	if spawned.Load() != 1 || !runningDuringChild {
		t.Fatalf("spawned=%d ownChildRunning=%v, want one child counted as our own", spawned.Load(), runningDuringChild)
	}
	if antigravityOwnChildRunning() {
		t.Error("the child is still counted as running after it ended")
	}
	if res := h.pass(time.Now().Add(10*time.Minute), 0); !res.owed.IsZero() {
		t.Errorf("owed=%s, want the renewal's own log never owed", res.owed)
	}
	if owned, candidates, _, _, _, _ := helperIndexCounts(); owned != 1 || candidates != 0 {
		t.Errorf("owned=%d candidates=%d, want the child's log owned", owned, candidates)
	}
}

// The result is mapped from the keyring's expiry before and after the child.
func TestRenewAntigravityStoredLogin_ResultFromTheKeyringExpiry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		before      time.Duration
		after       time.Duration // the child moves the expiry to now+after (0: leaves it)
		childOK     bool
		noCLI       bool
		want        string
		wantSpawned int32
	}{
		{name: "already valid: nothing spawned", before: time.Hour, want: antigravityLoginRenewed},
		{name: "renewed by the child", before: -time.Minute, after: time.Hour, childOK: true, want: antigravityLoginRenewed, wantSpawned: 1},
		{name: "still expired after the child", before: -time.Minute, childOK: true, want: antigravityLoginStillExpired, wantSpawned: 1},
		{name: "inside the skew after the child", before: -time.Minute, after: 5 * time.Second, childOK: true, want: antigravityLoginStillExpired, wantSpawned: 1},
		{name: "child failed or timed out", before: -time.Minute, want: antigravityLoginUnavailable, wantSpawned: 1},
		{name: "a failed child that renewed anyway", before: -time.Minute, after: time.Hour, want: antigravityLoginRenewed, wantSpawned: 1},
		{name: "no executable", before: -time.Minute, noCLI: true, want: antigravityLoginUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helperIsolateLogIndex(t)
			setExpiry := helperMutableAntigravityKeyring(t, time.Now().Add(tc.before))
			spawned := helperIsolateLoginRenewal(t, func(context.Context) (string, bool) {
				if tc.after != 0 {
					setExpiry(time.Now().Add(tc.after))
				}
				return realAntigravityModels, tc.childOK
			})
			if tc.noCLI {
				antigravityLoginRenewDetectedFn = func() (detectedCLIAgent, bool) { return detectedCLIAgent{}, false }
			}
			if got := renewAntigravityStoredLogin(context.Background(), time.Now()); got != tc.want {
				t.Errorf("result=%q, want %q", got, tc.want)
			}
			if spawned.Load() != tc.wantSpawned {
				t.Errorf("spawned=%d, want %d", spawned.Load(), tc.wantSpawned)
			}
		})
	}
}

// One renewal at a time: a caller that arrives while one runs shares its
// result, and one inside the spacing after it is told `spaced` without a child.
func TestRenewAntigravityStoredLogin_SingleFlightAndSpacing(t *testing.T) {
	helperIsolateLogIndex(t)
	setExpiry := helperMutableAntigravityKeyring(t, time.Now().Add(-time.Minute))
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	spawned := helperIsolateLoginRenewal(t, func(context.Context) (string, bool) {
		first.Do(func() { close(entered); <-release })
		setExpiry(time.Now().Add(time.Hour))
		return realAntigravityModels, true
	})

	now := time.Now()
	results := make(chan string, 2)
	go func() { results <- renewAntigravityStoredLogin(context.Background(), now) }()
	<-entered
	go func() { results <- renewAntigravityStoredLogin(context.Background(), now.Add(time.Second)) }()
	// The second caller must be parked on the first one's flight before it ends.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for i := 0; i < 2; i++ {
		if got := <-results; got != antigravityLoginRenewed {
			t.Errorf("caller %d result=%q, want both to share renewed", i, got)
		}
	}
	if spawned.Load() != 1 {
		t.Fatalf("spawned=%d, want one child for two concurrent callers", spawned.Load())
	}

	setExpiry(time.Now().Add(-time.Minute))
	if got := renewAntigravityStoredLogin(context.Background(), now.Add(antigravityLoginRenewMinInterval-time.Second)); got != antigravityLoginSpaced {
		t.Errorf("result=%q inside the spacing, want spaced", got)
	}
	if spawned.Load() != 1 {
		t.Errorf("spawned=%d, want a spaced call to start nothing", spawned.Load())
	}
	renewAntigravityStoredLogin(context.Background(), now.Add(antigravityLoginRenewMinInterval+time.Second))
	if spawned.Load() != 2 {
		t.Errorf("spawned=%d, want a renewal once the spacing lapsed", spawned.Load())
	}
}

// The child's model list is stored under the CURRENT generation; a forced
// refresh (a cache reset) while it runs leaves its pre-reset answer unstored.
func TestRenewAntigravityStoredLogin_RefreshesTheModelCache(t *testing.T) {
	helperIsolateLogIndex(t)
	helperMutableAntigravityKeyring(t, time.Now().Add(-time.Minute))
	resetMidProbe := false
	helperIsolateLoginRenewal(t, func(context.Context) (string, bool) {
		if resetMidProbe {
			resetCLIAgentModelProbeCache()
		}
		return realAntigravityModels, true
	})
	detected, _ := antigravityLoginRenewDetectedFn()

	renewAntigravityStoredLogin(context.Background(), time.Now())
	if got, ok := lookupCLIAgentModelDiscoveryCache("antigravity", detected, time.Now()); !ok || len(got.Models) == 0 {
		t.Fatalf("models=%v ok=%v, want the renewal's list cached", got.Models, ok)
	}

	resetAntigravityLoginRenewal()
	resetCLIAgentModelProbeCache()
	resetMidProbe = true
	renewAntigravityStoredLogin(context.Background(), time.Now())
	if _, ok := lookupCLIAgentModelDiscoveryCache("antigravity", detected, time.Now()); ok {
		t.Error("a list from before a mid-probe reset was stored")
	}
}

// Nothing the renewal prints carries the token, a path or a PID.
func TestRenewAntigravityStoredLogin_LogsNoTokenBody(t *testing.T) {
	helperIsolateAntigravityFreshness(t)
	helperIsolateLogIndex(t)
	setExpiry := helperMutableAntigravityKeyring(t, time.Now().Add(-time.Minute))
	helperIsolateLoginRenewal(t, func(context.Context) (string, bool) {
		setExpiry(time.Now().Add(time.Hour))
		return realAntigravityModels, true
	})
	helperOwedDebt(t, antigravityUsageFreshness{})
	logged := captureStdout(t, func() {
		antigravityRenewLoginForDebt(helperFreshnessState(t), time.Now().Add(-time.Second).UnixMilli())
	})
	if !strings.Contains(logged, "Stored login renewal finished (renewed)") {
		t.Fatalf("log=%q, want the closed-set renewal line", logged)
	}
	helperAssertNoSecrets(t, "renewal log", logged)
	for _, forbidden := range []string{"access-renew-secret", "never-read", "/opt/agy", "agy.exe"} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("the renewal log carries %q: %q", forbidden, logged)
		}
	}
}
