package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// setCodexCaptureVersion publishes the producing binary for one test.
func setCodexCaptureVersion(t *testing.T, version string) {
	t.Helper()
	codexUsageCaptureVersion.Store(version)
	t.Cleanup(func() { codexUsageCaptureVersion.Store("") })
}

// Drift is about DIFFERENCE: a reading stamped by one binary while another is
// installed. An empty stamp (first-ever or pre-stamp reading) and an empty
// detected version (a failed `--version`) say nothing either way.
func TestCodexCaptureDrift_Predicate(t *testing.T) {
	cases := []struct {
		name, stamped, detected string
		want                    bool
	}{
		{"upgrade", "codex-cli 0.149.0", "codex-cli 0.150.0", true},
		{"downgrade", "codex-cli 0.150.0", "codex-cli 0.149.0", true},
		{"same binary", "codex-cli 0.150.0", "codex-cli 0.150.0", false},
		{"first-ever reading", "", "codex-cli 0.150.0", false},
		{"version probe failed", "codex-cli 0.149.0", "", false},
		{"whitespace only differs", "codex-cli 0.150.0 ", "codex-cli 0.150.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexCaptureDrift(tc.stamped, tc.detected); got != tc.want {
				t.Fatalf("codexCaptureDrift(%q, %q) = %v, want %v", tc.stamped, tc.detected, got, tc.want)
			}
			if got := codexCaptureDriftFromView(codexCacheView{codexVersion: tc.stamped}, tc.detected); got != tc.want {
				t.Fatalf("view predicate = %v, want %v", got, tc.want)
			}
		})
	}
}

// A merge that advances an observation stamps the producing binary; one whose
// producer is unknown leaves the previous stamp in place, so a single
// unstamped write can never silently clear drift. A merge that advances
// nothing (an older reading) never restamps.
func TestCodexCaptureStamp_OnlyAKnownAdvancingMergeRestamps(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	fp := currentCodexAccountFingerprint()

	setCodexCaptureVersion(t, "codex-cli 0.149.0")
	if !captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(10, 20, now), now.Add(-time.Hour), fp) {
		t.Fatal("the first reading must land")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("stamp = %q, want the producing binary", snap.CodexVersion)
	}

	codexUsageCaptureVersion.Store("")
	if !captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(11, 21, now), now, fp) {
		t.Fatal("the newer reading must land")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("an unknown producer overwrote the stamp: %q", snap.CodexVersion)
	}

	setCodexCaptureVersion(t, "codex-cli 0.150.0")
	sparseOlder := `{"method":"token_count","timestamp":"` + now.Add(-2*time.Hour).UTC().Format(time.RFC3339Nano) +
		`","params":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_in_seconds":3600}}}}`
	if captureCodexRateLimitLineForAccount(sparseOlder, now, fp) {
		t.Fatal("an older reading must not report a capture")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("a merge that advanced nothing restamped: %q", snap.CodexVersion)
	}
}

// A pre-Contributors cache (flat Buckets only) is migrated by the merge; that
// migration is not an observation, so a merge that brings nothing newer does
// not claim the reading for the current binary.
func TestCodexCaptureStamp_LegacyMigrationIsNotAnObservation(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	fp := currentCodexAccountFingerprint()
	legacy := fmt.Sprintf(`{"updatedAt":"x","accountFingerprint":%q,"codexVersion":"codex-cli 0.149.0","buckets":{"primary":{"usedPercentage":10,"resetsAtMs":%d,"observedAtMs":%d,"windowMinutes":300}}}`,
		fp, now.Add(time.Hour).UnixMilli(), now.UnixMilli())
	if err := os.WriteFile(cache, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	setCodexCaptureVersion(t, "codex-cli 0.150.0")
	// Older than the migrated bucket, for the same slot: nothing advances.
	stale := `{"method":"token_count","timestamp":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339Nano) +
		`","params":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_in_seconds":3600}}}}`
	if captureCodexRateLimitLineForAccount(stale, now, fp) {
		t.Fatal("a merge that only migrated the legacy bucket must not report a capture")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("legacy migration restamped the reading: %q", snap.CodexVersion)
	}
}

// An authoritative full snapshot that only clears windows is still an answer
// from the installed build: it restamps, so drift is not reported against the
// build that just reported.
func TestCodexCaptureStamp_AuthoritativeClearRestamps(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	fp := currentCodexAccountFingerprint()
	setCodexCaptureVersion(t, "codex-cli 0.149.0")
	captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(10, 20, now), now.Add(-time.Minute), fp)

	setCodexCaptureVersion(t, "codex-cli 0.150.0")
	if !captureCodexRateLimitLineForAccount(`{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{}}}`, now, fp) {
		t.Fatal("an authoritative empty snapshot must report a capture")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.150.0" {
		t.Fatalf("stamp = %q, want the build that answered", snap.CodexVersion)
	}
}

// Versions are bounded rune-safely and in ONE form, so an over-long build
// string never splits a rune and never reads as drift against itself.
func TestCodexNormalizeVersion_BoundedAndStable(t *testing.T) {
	long := "codex-cli 0.150.0 " + strings.Repeat("é", 60)
	got := codexNormalizeVersion(long)
	if len(got) > codexVersionMaxBytes || !utf8.ValidString(got) {
		t.Fatalf("normalized = %q (%d bytes), want valid UTF-8 within %d", got, len(got), codexVersionMaxBytes)
	}
	setCodexCaptureVersion(t, "")
	publishCodexUsageCaptureVersion(long)
	if codexCaptureDrift(currentCodexUsageCaptureVersion(), long) {
		t.Fatal("a bounded stamp must not read as drift against its own build")
	}
}

// The two stamps stay independent: a live-probe capture under the new binary
// moves CodexVersion (clearing drift) but NOT RolloutCursorVersion, so the
// next rollout scan still resets a cursor the old binary wrote.
func TestCodexCaptureStamp_LiveCaptureDoesNotMoveTheCursorStamp(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	f.advanceCursorPast(t, now.Add(-10*time.Second), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.CodexVersion, snap.RolloutCursorVersion = "codex-cli 0.149.0", "codex-cli 0.149.0"
	})

	setCodexCaptureVersion(t, "codex-cli 0.150.0")
	captureCodexRateLimitLineForAccount(codexLiveReadEnvelope(33, 44, now), now, f.fp)

	snap := f.snapshot(t)
	if snap.CodexVersion != "codex-cli 0.150.0" || snap.RolloutCursorVersion != "codex-cli 0.149.0" {
		t.Fatalf("stamps = (%q, %q), want the reading restamped and the cursor stamp untouched", snap.CodexVersion, snap.RolloutCursorVersion)
	}
	if cursor := codexRolloutScanCursorForAccount(f.home, f.fp, now); cursor.mtimeNs != 0 {
		t.Fatalf("a cursor the previous binary wrote must read as empty, got %+v", cursor)
	}

	codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	snap = f.snapshot(t)
	if snap.RolloutCursorVersion != "codex-cli 0.150.0" || snap.RolloutHighWaterMtimeNs == now.Add(-10*time.Second).UnixNano() {
		t.Fatalf("the scan must reset the old cursor and stamp the new binary: %+v", snap)
	}
}

// The drift notice replaces the stale-run notice and inherits its gate: never
// for an upgrade alone, never while the fallback is in flight, and only
// version strings and timestamps.
func TestCodexCaptureDriftNotice_InheritsTheStaleGate(t *testing.T) {
	floor := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	state := codexRunFreshnessState{
		floor: floor, owedAt: floor.Add(time.Minute), latest: floor.Add(-time.Hour),
		owed: true, attempts: codexRefreshAfterRunMaxAttempts, codexVersion: "codex-cli 0.149.0",
	}
	const detected = "codex-cli 0.150.0"

	for _, fallback := range []string{codexFallbackUnset, codexFallbackSpent, codexFallbackSkipped} {
		state.fallback = fallback
		notice := codexRunFreshnessNotice(state, detected)
		const want = `Codex utilization was last observed 2026-09-26 09:00 UTC by Codex build "codex-cli 0.149.0"; the installed build "codex-cli 0.150.0" has not reported utilization since the most recent Codex run started (2026-09-26 10:00 UTC). It will update once that build's telemetry is captured.`
		if notice != want {
			t.Fatalf("fallback %q: notice\n got %q\nwant %q", fallback, notice, want)
		}
	}

	state.latest = time.Time{}
	if notice := codexRunFreshnessNotice(state, detected); notice != `No Codex utilization reading has been observed since Codex build "codex-cli 0.149.0"; the installed build "codex-cli 0.150.0" has not reported utilization since the most recent Codex run started (2026-09-26 10:00 UTC). It will update once that build's telemetry is captured.` {
		t.Fatalf("no-reading drift notice = %q", notice)
	}
	state.fallback = codexFallbackOutstanding
	if notice := codexRunFreshnessNotice(state, detected); notice != "" {
		t.Fatalf("no notice while the fallback is in flight, got %q", notice)
	}
	state.fallback = codexFallbackSpent
	state.attempts = 1
	if notice := codexRunFreshnessNotice(state, detected); notice != "" {
		t.Fatalf("an upgrade alone must not raise drift, got %q", notice)
	}
	state.attempts = codexRefreshAfterRunMaxAttempts
	if notice := codexRunFreshnessNotice(state, "codex-cli 0.149.0"); notice != codexStaleRunNotice(state) {
		t.Fatalf("without drift the stale-run notice stands, got %q", notice)
	}
}
