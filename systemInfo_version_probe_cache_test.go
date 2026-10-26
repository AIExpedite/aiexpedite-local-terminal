package main

// The version cache must not remember a probe that never got a fair answer.
// A `--version` that timed out on a cold binary (the first spawn after a CLI
// or agent update, scanned by real-time AV on Windows) used to be cached as a
// negative for the binary's (path, mtime, size), so the version read "" until
// the binary changed again -- and the Antigravity Code Assist User-Agent and
// the Grok gather both read it.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// helperVersionProbeTarget writes a stand-in binary whose (path, mtime, size)
// keys the cache. cachedProbeVersionOutcomeFunc never spawns it: the probe
// under test is the stub handed in.
func helperVersionProbeTarget(t *testing.T) string {
	t.Helper()
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// helperReplaceVersionProbeTarget rewrites the stand-in as an update would: new
// bytes, new size, a later mtime.
func helperReplaceVersionProbeTarget(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// helperScriptedProbe answers from a script, one outcome per call, and counts
// the calls (each one stands for a spawned child).
func helperScriptedProbe(script ...versionProbeOutcome) (func() (string, versionProbeOutcome), *atomic.Int64) {
	var calls atomic.Int64
	return func() (string, versionProbeOutcome) {
		n := int(calls.Add(1)) - 1
		outcome := script[len(script)-1]
		if n < len(script) {
			outcome = script[n]
		}
		if outcome == versionProbeOK {
			return "cli 1.2.3", versionProbeOK
		}
		return "", outcome
	}, &calls
}

func TestCachedProbeVersionOutcome_TransientTimeoutIsNotCached(t *testing.T) {
	path := helperVersionProbeTarget(t)
	probe, calls := helperScriptedProbe(versionProbeTransient, versionProbeOK)

	if got := cachedProbeVersionOutcomeFunc(path, probe); got != "" {
		t.Fatalf("first (cold) probe = %q, want \"\"", got)
	}
	if _, ok := lookupCachedProbeVersion(path); ok {
		t.Fatal("a timed-out probe was cached; the version would read \"\" until the binary changes again")
	}
	if got := cachedProbeVersionOutcomeFunc(path, probe); got != "cli 1.2.3" {
		t.Fatalf("second probe = %q, want the version the warm binary answers", got)
	}
	if v, ok := lookupCachedProbeVersion(path); !ok || v != "cli 1.2.3" {
		t.Fatalf("cache holds %q (ok=%v), want only the answered version", v, ok)
	}
	if got := cachedProbeVersionOutcomeFunc(path, probe); got != "cli 1.2.3" || calls.Load() != 2 {
		t.Fatalf("third call = %q after %d probes, want a cache hit", got, calls.Load())
	}
}

func TestCachedProbeVersionOutcome_StableFailureIsStillCached(t *testing.T) {
	path := helperVersionProbeTarget(t)
	probe, calls := helperScriptedProbe(versionProbeFailed)

	for i := 0; i < 3; i++ {
		if got := cachedProbeVersionOutcomeFunc(path, probe); got != "" {
			t.Fatalf("call %d = %q, want \"\"", i, got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("probes=%d, want 1: a binary that answered badly must not re-spawn on every gather", calls.Load())
	}
	if v, ok := lookupCachedProbeVersion(path); !ok || v != "" {
		t.Fatalf("cache = %q (ok=%v), want the cached negative", v, ok)
	}
}

// A string-only probe cannot say why it answered "", so nothing moves for it:
// its "" is cached as a stable failure, as it always was.
func TestCachedProbeVersionFunc_StringProbeKeepsCachingEmpty(t *testing.T) {
	path := helperVersionProbeTarget(t)
	var calls atomic.Int64
	probe := func() string { calls.Add(1); return "" }

	cachedProbeVersionFunc(path, probe)
	cachedProbeVersionFunc(path, probe)
	if calls.Load() != 1 {
		t.Fatalf("probes=%d, want the string-only negative cached after one", calls.Load())
	}
}

// A transient that never clears (a binary the agent is genuinely denied) must
// not cost a child on every gather forever: after versionProbeTransientLimit in
// a row for the same binary it is cached. A replaced binary starts over.
func TestCachedProbeVersionOutcome_RepeatedTransientsArePromoted(t *testing.T) {
	path := helperVersionProbeTarget(t)
	probe, calls := helperScriptedProbe(versionProbeTransient)

	for i := 0; i < versionProbeTransientLimit; i++ {
		if _, ok := lookupCachedProbeVersion(path); ok {
			t.Fatalf("cached after %d transients, want only after %d", i, versionProbeTransientLimit)
		}
		cachedProbeVersionOutcomeFunc(path, probe)
	}
	if v, ok := lookupCachedProbeVersion(path); !ok || v != "" {
		t.Fatalf("after %d transients cache = %q (ok=%v), want a cached failure", versionProbeTransientLimit, v, ok)
	}
	cachedProbeVersionOutcomeFunc(path, probe)
	if calls.Load() != int64(versionProbeTransientLimit) {
		t.Fatalf("probes=%d, want the promoted failure served from the cache", calls.Load())
	}

	// The update lands: new mtime and size, a new key, a fresh count.
	helperReplaceVersionProbeTarget(t, path, "v2-longer")
	cachedProbeVersionOutcomeFunc(path, probe)
	if _, ok := lookupCachedProbeVersion(path); ok {
		t.Fatal("a replaced binary inherited the old binary's transient count")
	}
	if calls.Load() != int64(versionProbeTransientLimit)+1 {
		t.Fatalf("probes=%d, want the replaced binary probed again", calls.Load())
	}
}

// forgetCachedProbeVersion (OpenCode's launcher-failure expiry) drops the
// transient count with the cached readings, so it cannot promote early.
func TestCachedProbeVersionOutcome_ForgetResetsTheTransientCount(t *testing.T) {
	path := helperVersionProbeTarget(t)
	probe, _ := helperScriptedProbe(versionProbeTransient)

	for i := 0; i < versionProbeTransientLimit-1; i++ {
		cachedProbeVersionOutcomeFunc(path, probe)
	}
	forgetCachedProbeVersion(path)
	cachedProbeVersionOutcomeFunc(path, probe)
	if _, ok := lookupCachedProbeVersion(path); ok {
		t.Fatal("forgetCachedProbeVersion left the transient count behind")
	}
}

// The existing race guard: a probe that raced a binary replacement lands
// nothing, whatever its outcome.
func TestCachedProbeVersionOutcome_ReplacedDuringProbeLeavesTheMapUntouched(t *testing.T) {
	for _, outcome := range []versionProbeOutcome{versionProbeOK, versionProbeFailed, versionProbeTransient} {
		path := helperVersionProbeTarget(t)
		got := cachedProbeVersionOutcomeFunc(path, func() (string, versionProbeOutcome) {
			helperReplaceVersionProbeTarget(t, path, "replaced-mid-probe")
			if outcome == versionProbeOK {
				return "cli 1.2.3", outcome
			}
			return "", outcome
		})
		if outcome == versionProbeOK && got != "cli 1.2.3" {
			t.Fatalf("outcome %d: caller got %q, want the reading returned uncached", outcome, got)
		}
		versionProbeMu.Lock()
		entries, transients := len(versionProbeCache), len(versionProbeTransients)
		versionProbeMu.Unlock()
		if entries != 0 || transients != 0 {
			t.Fatalf("outcome %d: map has %d entries / %d transient counts, want none for a raced probe",
				outcome, entries, transients)
		}
	}
}

// The classification of a real child, end to end through the shipped probe.
func TestProbeVersionArgsWithEnvOutcome_ClassifiesTheChild(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	run := func(mode string) (string, versionProbeOutcome) {
		return probeVersionArgsWithEnvOutcome(testExe, setEnvVar(os.Environ(), mockCLIEnvVar, mode), "--version")
	}
	if v, outcome := run("grok-version-v1"); outcome != versionProbeOK || v != "grok 1.0.3" {
		t.Errorf("answering child = %q/%d, want ok", v, outcome)
	}
	if v, outcome := run("stderr-and-exit"); outcome != versionProbeFailed || v != "" {
		t.Errorf("non-zero exit = %q/%d, want a stable failure", v, outcome)
	}
	if _, outcome := probeVersionArgsWithEnvOutcome(filepath.Join(t.TempDir(), "missing"), nil, "--version"); outcome != versionProbeFailed {
		t.Errorf("missing binary = %d, want a stable failure", outcome)
	}
	// A child that never answers inside machineInfoProbeTimeout: the shape of
	// a cold binary under an AV scan. Costs the 3 s probe budget once.
	if v, outcome := run("grok-smoke-hang"); outcome != versionProbeTransient || v != "" {
		t.Errorf("hung child = %q/%d, want transient", v, outcome)
	}
}

func TestClassifyVersionProbeError(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := classifyVersionProbeError(expired, errors.New("signal: killed")); got != versionProbeTransient {
		t.Errorf("deadline hit = %d, want transient", got)
	}
	if got := classifyVersionProbeError(context.Background(), context.DeadlineExceeded); got != versionProbeTransient {
		t.Errorf("DeadlineExceeded = %d, want transient", got)
	}
	if got := classifyVersionProbeError(context.Background(), exec.ErrNotFound); got != versionProbeFailed {
		t.Errorf("not found = %d, want a stable failure", got)
	}
}
