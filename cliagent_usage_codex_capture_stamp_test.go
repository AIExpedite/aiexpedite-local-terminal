package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// A child spawned before an upgrade keeps running the old build: its frames
// are stamped with the version pinned at spawn, not the newer one a gather
// published since, so pre-upgrade telemetry can never clear capture drift.
func TestCodexCaptureStamp_PinnedProducerOutranksThePublishedVersion(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	setCodexCaptureVersion(t, "codex-cli 0.150.0")

	if !captureCodexRateLimitLineFromProducer(codexLiveReadEnvelope(10, 20, now), now, currentCodexAccountFingerprint(), "codex-cli 0.149.0") {
		t.Fatal("the reading must land")
	}
	if snap, _ := loadCodexRateLimitSnapshot(cache); snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("stamp = %q, want the build pinned when the child was spawned", snap.CodexVersion)
	}
}

// writeCodexRolloutFromBuild writes a run rollout whose session header records
// the Codex build that wrote it, as `cli_version` does on a real rollout.
func writeCodexRolloutFromBuild(t *testing.T, base, name, build string, sessionStart, frameAt, mtime time.Time, frames []map[string]any) {
	t.Helper()
	path := writeCodexRunRollout(t, base, name, sessionStart, frameAt, mtime, true, frames)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rollout: %v", err)
	}
	header, err := json.Marshal(map[string]any{
		"timestamp": sessionStart.UTC().Format(time.RFC3339Nano),
		"type":      codexRolloutSessionMetaType,
		"payload":   map[string]any{"id": "sess-" + name, "cli_version": build},
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	body := raw[bytes.IndexByte(raw, '\n')+1:]
	if err := os.WriteFile(path, append(append(header, '\n'), body...), 0o600); err != nil {
		t.Fatalf("rewrite rollout: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes rollout: %v", err)
	}
}

// A rollout names its producer in its header; only one written by the
// installed build may carry the installed build's stamp.
func TestCodexRolloutProducerVersion(t *testing.T) {
	const installed = "codex-cli 0.150.0"
	for _, tc := range []struct{ header, installed, want string }{
		{"0.150.0", installed, installed},
		{"v0.150.0", installed, installed},
		{"0.150.0", "0.150.0", "0.150.0"},
		{"0.149.0", installed, ""},
		{"0.15", installed, ""},
		{"", installed, ""},
		{"0.150.0", "", ""},
	} {
		if got := codexRolloutProducerVersion(tc.header, tc.installed); got != tc.want {
			t.Errorf("codexRolloutProducerVersion(%q, %q) = %q, want %q", tc.header, tc.installed, got, tc.want)
		}
	}
	for line, want := range map[string]string{
		`{"type":"session_meta","payload":{"id":"s","cli_version":"0.150.0"}}`:  "0.150.0",
		`{"id":"s","timestamp":"2026-09-26T10:00:00Z","cli_version":"0.149.0"}`: "0.149.0",
		`{"type":"event_msg","payload":{"cli_version":"0.150.0"}}`:              "",
		`{"type":"session_meta","payload":{"id":"s"}}`:                          "",
		`{"cli_version":"0.150.0"}`:                                             "",
	} {
		if got := codexRolloutSessionVersionFromLine(line); got != want {
			t.Errorf("codexRolloutSessionVersionFromLine(%s) = %q, want %q", line, got, want)
		}
	}
}

// After an upgrade resets the cursor, the scan re-reads rollouts the previous
// build wrote. Their telemetry advances the reading but must not be credited
// to the installed build, or capture drift would clear on old-build evidence;
// a rollout the installed build wrote does restamp.
func TestCodexCaptureStamp_RolloutMergeKeepsItsProducer(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) {
		snap.CodexVersion, snap.RolloutCursorVersion = "codex-cli 0.149.0", "codex-cli 0.150.0"
	})
	setCodexCaptureVersion(t, "codex-cli 0.150.0")

	oldStart := now.Add(-10 * time.Minute)
	writeCodexRolloutFromBuild(t, f.home, "old", "0.149.0", oldStart, oldStart.Add(time.Minute), oldStart.Add(2*time.Minute),
		[]map[string]any{codexRateLimitFrame(21, 41, now)})
	codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	snap := f.snapshot(t)
	if got := codexLatestContributorObservation(snap.Contributors); got.Before(oldStart) {
		t.Fatalf("precondition: the old-build rollout must advance the reading, latest = %s", got)
	}
	if snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("stamp = %q after an old-build rollout, want the pre-upgrade stamp kept", snap.CodexVersion)
	}

	newStart := now.Add(-3 * time.Minute)
	writeCodexRolloutFromBuild(t, f.home, "new", "0.150.0", newStart, newStart.Add(time.Minute), newStart.Add(2*time.Minute),
		[]map[string]any{codexRateLimitFrame(22, 42, now)})
	codexReconcileFromRollout(context.Background(), f.home, f.fp, time.Now())
	if snap := f.snapshot(t); snap.CodexVersion != "codex-cli 0.150.0" {
		t.Fatalf("stamp = %q after an installed-build rollout, want the installed build", snap.CodexVersion)
	}
}

// A session launched from an explicit side-by-side path may run a different
// build than the detected install: it stamps only a version probed for that
// exact binary, never the published install's.
func TestCodexCaptureVersionForLaunch(t *testing.T) {
	setCodexCaptureVersion(t, "codex-cli 0.150.0")
	if got := codexCaptureVersionForLaunch("codex", "/usr/local/bin/codex"); got != "codex-cli 0.150.0" {
		t.Fatalf("PATH launch = %q, want the published install", got)
	}

	side := filepath.Join(t.TempDir(), "codex-side")
	if err := os.WriteFile(side, []byte("binary"), 0o700); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if got := codexCaptureVersionForLaunch(side, side); got != "" {
		t.Fatalf("unprobed explicit path = %q, want unknown", got)
	}
	cachedProbeVersionFunc(side, func() string { return "codex-cli 0.151.0" })
	if got := codexCaptureVersionForLaunch(side, side); got != "codex-cli 0.151.0" {
		t.Fatalf("probed explicit path = %q, want that binary's own version", got)
	}
}

// A gather that detected the PRE-upgrade build and was overtaken by one that
// published the new build must not roll the process-global stamp back: later
// captures made by the new binary would be stamped old, and the rollout cursor
// reset against the wrong version. The version-probe cache's binary identity
// is what settles it — no spawn.
func TestPublishCodexUsageCaptureVersionFrom_StaleDetectionCannotRollBack(t *testing.T) {
	t.Cleanup(resetVersionProbeCache)
	setCodexCaptureVersion(t, "")

	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// Nothing has probed this exact binary, so the offered version cannot be
	// tied to the file on disk and is refused rather than published on trust.
	publishCodexUsageCaptureVersionFrom(path, "codex-cli 0.149.0")
	if got := currentCodexUsageCaptureVersion(); got != "" {
		t.Fatalf("unprobed binary published %q, want no stamp", got)
	}

	// A version probed for the binary AS IT IS publishes.
	cachedProbeVersionFunc(path, func() string { return "codex-cli 0.149.0" })
	publishCodexUsageCaptureVersionFrom(path, "codex-cli 0.149.0")
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.149.0" {
		t.Fatalf("probed binary published %q, want the offered version", got)
	}

	// The upgrade: the binary as it is now reports the new build, and the
	// gather that detected it published first.
	if err := os.WriteFile(path, []byte("binary-v2"), 0o700); err != nil {
		t.Fatalf("rewrite binary: %v", err)
	}
	cachedProbeVersionFunc(path, func() string { return "codex-cli 0.150.0" })
	publishCodexUsageCaptureVersionFrom(path, "codex-cli 0.150.0")
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.150.0" {
		t.Fatalf("installed build published %q, want it to publish", got)
	}

	// The slow pre-upgrade gather finally reaches its publish.
	publishCodexUsageCaptureVersionFrom(path, "codex-cli 0.149.0")
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.150.0" {
		t.Fatalf("stale detection rolled the stamp back to %q, want the installed build", got)
	}

	// An absent path (detection reported no path) keeps the old unconditional
	// behaviour rather than dropping every stamp.
	publishCodexUsageCaptureVersionFrom("", "codex-cli 0.151.0")
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.151.0" {
		t.Fatalf("pathless publish = %q, want it to publish", got)
	}
}

// The narrow race the identity check exists for: a probe whose binary is
// REPLACED while it runs answers for a file that is gone, so it records nothing
// and cannot delete what a probe of the new build already recorded. Its own
// stale reading then finds no entry for the installed binary and is refused,
// rather than rolling the stamp back to a build that is no longer there.
func TestProbeRacingABinaryReplacement_LeavesNoStaleStamp(t *testing.T) {
	t.Cleanup(resetVersionProbeCache)
	resetVersionProbeCache()
	setCodexCaptureVersion(t, "codex-cli 0.150.0")

	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("binary-v1"), 0o700); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// The slow pre-upgrade probe: the upgrade lands mid-probe, and a probe of
	// the NEW build completes first and records its answer.
	stale := cachedProbeVersionFunc(path, func() string {
		if err := os.WriteFile(path, []byte("binary-v2-longer"), 0o700); err != nil {
			t.Fatalf("rewrite binary: %v", err)
		}
		cachedProbeVersionFunc(path, func() string { return "codex-cli 0.150.0" })
		return "codex-cli 0.149.0"
	})
	if stale != "codex-cli 0.149.0" {
		t.Fatalf("racing probe returned %q, want its own reading handed back uncached", stale)
	}

	// The new build's entry survived: the racing probe must not prune it.
	if got, ok := peekCachedProbeVersion(path); !ok || got != "codex-cli 0.150.0" {
		t.Fatalf("installed binary caches (%q, %v), want the new build's own probe", got, ok)
	}

	publishCodexUsageCaptureVersionFrom(path, stale)
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.150.0" {
		t.Fatalf("stale racing probe rolled the stamp back to %q", got)
	}
}

// The startup replay and the live fallback resolve the version outside a
// gather, so they must pass the same identity check: a probe that raced a
// binary replacement hands its stale reading back uncached, and publishing it
// unchecked would roll the stamp back to a build that is gone.
func TestCodexResolveCaptureVersion_RefusesAStaleReading(t *testing.T) {
	t.Cleanup(resetVersionProbeCache)
	resetVersionProbeCache()
	setCodexCaptureVersion(t, "codex-cli 0.150.0")

	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("binary-v2"), 0o700); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	cachedProbeVersionFunc(path, func() string { return "codex-cli 0.150.0" })

	original := codexInstalledVersion
	t.Cleanup(func() { codexInstalledVersion = original })

	codexInstalledVersion = func() (string, string) { return path, "codex-cli 0.149.0" }
	codexResolveCaptureVersion()
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.150.0" {
		t.Fatalf("stale resolved version rolled the stamp back to %q", got)
	}

	// A reading the cache holds for the binary as it is now still publishes.
	setCodexCaptureVersion(t, "")
	codexInstalledVersion = func() (string, string) { return path, "codex-cli 0.150.0" }
	codexResolveCaptureVersion()
	if got := currentCodexUsageCaptureVersion(); got != "codex-cli 0.150.0" {
		t.Fatalf("resolved installed version = %q, want it published", got)
	}
}

// The live probe's child is a long-lived process like a managed session: its
// reading is stamped with the build launched, not with whatever a concurrent
// gather published while the request was in flight.
func TestCodexLiveProbeConverse_StampsTheLaunchedBuild(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	setCodexCaptureVersion(t, "codex-cli 0.149.0")
	pinned := currentCodexUsageCaptureVersion()

	stdin, stdout, methods := fakeCodexAppServer(t, codexReadFixture(now))
	// The upgrade lands while the child is mid-request.
	codexUsageCaptureVersion.Store("codex-cli 0.150.0")
	if got := codexLiveProbeConverse(stdin, stdout, currentCodexAccountFingerprint(), pinned); got != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	<-methods

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected cache")
	}
	if snap.CodexVersion != "codex-cli 0.149.0" {
		t.Fatalf("stamp = %q, want the build the probe launched", snap.CodexVersion)
	}
}
