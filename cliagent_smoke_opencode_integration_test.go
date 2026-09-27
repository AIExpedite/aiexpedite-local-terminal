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
