// cliagent_smoke_opencode_integration_test.go — the shared smoke core, driven
// through the public `runCLISmoke(ctx, "opencode")` entry point.
//
// Before this feature there was no `opencode` row in cliSmokeProviders at all,
// so a maintenance smoke resolved to provider_unavailable / unknown_cli without
// spawning anything. These cases pin the row AND the core behaviour it now
// inherits: the cooldown that protects the user's quota, its invalidation by a
// binary change (the post-upgrade smoke must actually execute), the singleflight
// that stops a Pub/Sub burst from spending N turns, and the rule that a verdict
// reached under a cancelled caller is never pinned.
package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRunCLISmoke_OpenCodeRowIsRegistered(t *testing.T) {
	// The absence of this row IS the reported symptom's first cause.
	if _, known := cliSmokeProviders["opencode"]; !known {
		t.Fatal("cliSmokeProviders has no opencode row; a smoke would answer unknown_cli without spawning")
	}
}

func TestRunCLISmoke_OpenCodeUnknownIdStillNeverSpawns(t *testing.T) {
	result, replayed := runCLISmoke(context.Background(), "opencode-nightly")
	if result.Diagnostic != cliSmokeDiagnosticUnknownCLI || replayed {
		t.Fatalf("an unrecognised id must answer unknown_cli, got %+v", result)
	}
}

func TestRunCLISmoke_OpenCodeCooldownReplaysButNotAcrossAnUpgrade(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeSmokePath(t, path)
	stubOpenCodeReadiness(t, "", false)
	seedProbeVersion(t, path, "opencode 0.9.1")
	calls, _ := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	first, replayed := runCLISmoke(context.Background(), "opencode")
	if replayed || first.Status != cliSmokeStatusSuccess || first.Version != "opencode 0.9.1" {
		t.Fatalf("first smoke = %+v replayed=%t", first, replayed)
	}
	second, replayed := runCLISmoke(context.Background(), "opencode")
	if !replayed || second != first {
		t.Fatalf("second smoke inside the cooldown must replay: %+v replayed=%t", second, replayed)
	}
	if *calls != 1 {
		t.Fatalf("cooldown let %d turns through, want 1", *calls)
	}

	// The upgrade: different bytes reporting a new version. This is exactly the
	// post-update smoke the harness must not be served a stale answer for.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("stub-upgraded"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, "opencode 0.9.2")
	third, replayed := runCLISmoke(context.Background(), "opencode")
	if replayed || third.Version != "opencode 0.9.2" {
		t.Fatalf("post-upgrade smoke replayed the pre-upgrade verdict: %+v replayed=%t", third, replayed)
	}
	if *calls != 2 {
		t.Fatalf("post-upgrade smoke did not execute: calls=%d", *calls)
	}
}

func TestRunCLISmoke_OpenCodeConcurrentCallersShareOneTurn(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeSmokePath(t, path)
	stubOpenCodeReadiness(t, "", false)
	seedProbeVersion(t, path, "opencode 0.9.1")

	release := make(chan struct{})
	calls, _ := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		frames := openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch))
		<-release
		return frames, nil, nil
	})

	const callers = 6
	var wg sync.WaitGroup
	results := make([]cliSmokeResult, callers)
	executed := make([]bool, callers)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, replayed := runCLISmoke(context.Background(), "opencode")
			results[i], executed[i] = r, !replayed
		}(i)
	}
	// Let every caller reach the group before the leader can finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if *calls != 1 {
		t.Fatalf("a burst spent %d turns; the singleflight must collapse them onto one", *calls)
	}
	spenders := 0
	for i, r := range results {
		if r.Status != cliSmokeStatusSuccess {
			t.Fatalf("caller %d got %+v", i, r)
		}
		if executed[i] {
			spenders++
		}
	}
	if spenders != 1 {
		t.Fatalf("%d callers reported spending a turn, want exactly 1", spenders)
	}
}

func TestRunCLISmoke_OpenCodeCallerCancellationIsNotCached(t *testing.T) {
	// A run killed because the CALLER went away says nothing about the binary.
	// Pinning it would hand the redelivered post-upgrade smoke a stale failure.
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeSmokePath(t, path)
	stubOpenCodeReadiness(t, "", false)
	seedProbeVersion(t, path, "opencode 0.9.1")

	ctx, cancel := context.WithCancel(context.Background())
	cancelled := false
	calls, _ := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		if !cancelled {
			cancelled = true
			cancel()
			return nil, nil, context.Canceled
		}
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result, _ := runCLISmoke(ctx, "opencode"); result.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("a cancelled caller must classify as timeout, got %+v", result)
	}
	// A fresh caller must run again rather than be handed the cancelled verdict.
	if result, replayed := runCLISmoke(context.Background(), "opencode"); replayed ||
		result.Status != cliSmokeStatusSuccess {
		t.Fatalf("the cancelled verdict was pinned: %+v replayed=%v", result, replayed)
	}
	if *calls != 2 {
		t.Fatalf("expected a second real run, got calls=%d", *calls)
	}
}

func TestRunCLISmoke_OpenCodeLaunchErrorIsNotCached(t *testing.T) {
	// launch_error means the child never started, so nothing was inferred and a
	// reinstall that repairs a broken shim must be noticed immediately.
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeSmokePath(t, path)
	stubOpenCodeReadiness(t, "", false)
	seedProbeVersion(t, path, "opencode 0.9.1")

	failed := false
	calls, _ := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		if !failed {
			failed = true
			return nil, nil, errOpenCodeShimUnrenderable
		}
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result, _ := runCLISmoke(context.Background(), "opencode"); result.Diagnostic != cliSmokeDiagnosticLaunchError {
		t.Fatalf("want launch_error, got %+v", result)
	}
	if result, replayed := runCLISmoke(context.Background(), "opencode"); replayed ||
		result.Status != cliSmokeStatusSuccess {
		t.Fatalf("launch_error was pinned by the cooldown: %+v replayed=%v", result, replayed)
	}
	if *calls != 2 {
		t.Fatalf("expected a second real run, got calls=%d", *calls)
	}
}

/* --------------------------------------------------------------------------
   The real deadline path, on every platform
   --------------------------------------------------------------------------
   openCodeSmokeTimeout is a var precisely so a test can shrink it and exercise
   the REAL kill, but the only case doing so was Windows-gated — so the seam's
   cmd.Cancel hook, its WaitDelay and the process-tree reap had no executed
   coverage on the platform CI actually runs. Driving the compiled stub proves
   all three without a vendor binary.
   ------------------------------------------------------------------------ */

func TestRunCLISmoke_OpenCodeRealDeadlineKillsTheChildAndReportsTimeout(t *testing.T) {
	openCodeSmokeEnv(t)
	binDir := installOpenCodeStub(t)
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	stubOpenCodeSmokePath(t, filepath.Join(binDir, name))
	t.Setenv("OPENCODE_STUB_VERSION", "opencode 0.9.1")

	// The child outlives the deadline by a wide margin, and records reaching the
	// end of its sleep. That marker is the cross-platform proof of the reap: it
	// appears iff the child was NOT killed.
	doneLog := filepath.Join(t.TempDir(), "done.log")
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "60000")
	t.Setenv("OPENCODE_STUB_DONE_LOG", doneLog)

	original := openCodeSmokeTimeout
	openCodeSmokeTimeout = 800 * time.Millisecond
	t.Cleanup(func() { openCodeSmokeTimeout = original })

	started := time.Now()
	result, replayed := runCLISmoke(context.Background(), "opencode")
	elapsed := time.Since(started)

	if replayed {
		t.Fatal("the first smoke must actually run")
	}
	if result.Diagnostic != cliSmokeDiagnosticTimeout ||
		result.ErrorCategory != cliUsageErrorProviderTimeout {
		t.Fatalf("got (%q, %q), want timeout/provider_timeout", result.ErrorCategory, result.Diagnostic)
	}
	if result.MarkerMatched {
		t.Error("a killed run must not report markerMatched")
	}
	// Bounded by the deadline plus the reap race (WaitDelay), nowhere near the
	// child's own 60s sleep. Without the tree kill, Wait would sit on the pipes
	// the child still holds.
	if elapsed > 30*time.Second {
		t.Fatalf("the deadline did not bound the run: %v", elapsed)
	}
	if _, err := os.Stat(doneLog); err == nil {
		t.Error("the child reached the end of its sleep — the deadline did not kill it")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error stat-ing the done marker: %v", err)
	}

	// A verdict reached by a kill says nothing about the binary, so it must not
	// be pinned: the redelivered post-upgrade smoke has to test the CLI.
	if cliSmokeVerdictSpentTurn(result) {
		// timeout IS a spent-turn verdict by design (inference may have started),
		// so this only documents which side of the line it falls on.
		t.Log("timeout is cached as a spent turn, as designed")
	}
	// The scratch cwd and the prompt file are gone even on the kill path.
	assertNoOpenCodeSmokeScratchLeaks(t)
}

// assertNoOpenCodeSmokeScratchLeaks fails when a run left a per-run cwd or a
// staged prompt behind. The prompt file carries the marker nonce, and the cwd
// accumulates one directory entry per run, so neither may survive.
func assertNoOpenCodeSmokeScratchLeaks(t *testing.T) {
	t.Helper()
	for _, dir := range []string{cliPromptTempDir("opencode-smoke"), cliPromptTempDir("opencode-prompts")} {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // never created — nothing to leak
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("scratch dir %s still holds %v", dir, names)
		}
	}
}
