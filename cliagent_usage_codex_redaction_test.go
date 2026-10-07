package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Everything the run-freshness path persists or publishes must be numeric
// metrics, timestamps, fingerprints and closed-enum labels. A rollout carries
// prompt text, file paths and account identity right next to the telemetry it
// mines; none of it may reach codex_rate_limits.json, the published usage, or
// the stale notice.
func TestCodexRunFreshness_PersistsAndPublishesNumericOnly(t *testing.T) {
	const (
		email        = "person.secret@example.com"
		promptMarker = "PROMPT_MARKER_do_not_persist_7c1f"
		argvMarker   = "--dangerously-bypass-approvals-and-sandbox"
		configMarker = "model_reasoning_effort=\"high\""
	)
	now := time.Now()
	runStart := now.Add(-2 * time.Minute)
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	helperCodexAuthAt(t, f.home, email, now.Add(-3*time.Hour))
	f.fp = currentCodexAccountFingerprint()
	f.seedPreRunReading(t, now.Add(-time.Hour), now)

	leaky := []map[string]any{
		{"timestamp": runStart.UTC().Format(time.RFC3339Nano), "type": "turn_context", "payload": map[string]any{
			"cwd": f.home, "argv": []string{"codex", argvMarker}, "config": configMarker, "user": email,
		}},
		{"timestamp": runStart.UTC().Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
			"type": "message", "content": promptMarker + " rate_limits token_count",
		}},
	}
	// One stamped and one timestamp-less rollout, so both the stated and the
	// inferred write paths are covered.
	writeCodexRunRollout(t, f.home, "stamped", runStart, runStart.Add(20*time.Second), runStart.Add(30*time.Second), true,
		[]map[string]any{codexRateLimitFrame(21, 31, now)}, leaky...)
	writeCodexRunRollout(t, f.home, "unstamped", runStart.Add(time.Second), time.Time{}, runStart.Add(40*time.Second), false,
		[]map[string]any{codexRateLimitFrame(22, 32, now)}, leaky...)

	triggerCodexUsageRefreshAfterRun(runStart)
	drainCodexRunDebtLadder(t)

	raw, err := os.ReadFile(f.cache)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		email, "person.secret", promptMarker, argvMarker, "model_reasoning_effort",
		f.home, filepath.ToSlash(f.home), "rollout-", "sessions",
	}
	for _, s := range forbidden {
		if strings.Contains(string(raw), s) || strings.Contains(string(raw), strings.ReplaceAll(s, `\`, `\\`)) {
			t.Errorf("persisted snapshot leaks %q", s)
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	assertCodexSnapshotStringsRedacted(t, "", decoded)

	// The stale notice, from a debt that nothing could pay.
	codexRecordRunFreshness(f.fp, time.Now(), func(snap *codexRateLimitSnapshot) {
		codexOweRunRefresh(snap, time.Now().Add(-time.Second), time.Now())
		snap.RefreshOwedAttempts = codexRefreshAfterRunMaxAttempts
		snap.RefreshFallbackState = codexFallbackExhausted
	})
	usage, ok := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	if usage.Notice == "" {
		t.Fatal("expected the stale-run notice")
	}
	notice := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC`).ReplaceAllString(usage.Notice, "<ts>")
	if want := "Codex utilization was last observed <ts>, before the most recent Codex run started (<ts>); it will update once that run's telemetry is found."; notice != want {
		t.Fatalf("notice must be timestamps over fixed copy only:\n got %q\nwant %q", usage.Notice, want)
	}
	published, err := json.Marshal(usage.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		if strings.Contains(string(published), s) {
			t.Errorf("published metrics leak %q", s)
		}
	}
}

var (
	codexRedactedHex       = regexp.MustCompile(`^[0-9a-f]*$`)
	codexRedactedTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
)

// assertCodexSnapshotStringsRedacted walks the decoded snapshot: numbers and
// booleans are metrics / flags; every string must be a hex digest or
// fingerprint, or an RFC3339 timestamp. Map keys are window ids and limit ids.
func assertCodexSnapshotStringsRedacted(t *testing.T, path string, v any) {
	t.Helper()
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			assertCodexSnapshotStringsRedacted(t, path+"."+k, child)
		}
	case []any:
		for _, child := range val {
			assertCodexSnapshotStringsRedacted(t, path+"[]", child)
		}
	case string:
		if codexSnapshotClosedStringField(path, val) {
			return
		}
		if !codexRedactedHex.MatchString(val) && !codexRedactedTimestamp.MatchString(val) {
			t.Errorf("snapshot field %s holds free text %q", path, val)
		}
	}
}

// codexRedactedVersion is the shape of a `codex --version` first line.
var codexRedactedVersion = regexp.MustCompile(`^(codex(-cli)? )?v?\d+(\.\d+)*([-+][0-9A-Za-z.-]+)?$`)

// codexSnapshotClosedStringField admits the snapshot's closed, locally derived
// string fields: the two binary stamps (version-shaped only) and the fallback
// state (its enum only).
func codexSnapshotClosedStringField(path, val string) bool {
	switch path {
	case ".codexVersion", ".rolloutCursorVersion":
		return codexRedactedVersion.MatchString(val)
	case ".refreshFallbackState":
		switch val {
		case codexFallbackOutstanding, codexFallbackDeferred, codexFallbackExhausted:
			return true
		}
	}
	return false
}

// The smoke's own turn now reaches the cache through the capture path. Its
// stdout carries the account, the CODEX_HOME path and prompt text right next
// to the rate-limit frame; nothing but numeric windows (and the closed stamps)
// may land in codex_rate_limits.json.
func TestRunCodexSmoke_CapturePersistsNumericOnly(t *testing.T) {
	const (
		email        = "smoke.secret@example.com"
		accountID    = "acct-secret-7f3a"
		promptMarker = "PROMPT_MARKER_smoke_do_not_persist"
	)
	f, path := codexSmokeCaptureEnv(t)
	cache := f.cache
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		leaky, _ := json.Marshal(map[string]any{
			"type": "event_msg",
			"payload": map[string]any{
				"type": "token_count", "account_id": accountID, "email": email, "cwd": f.home,
				"prompt":      promptMarker,
				"rate_limits": codexRateLimitFrame(71, 72, time.Now()),
			},
		})
		frames := append(codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), append(leaky, '\n')...)
		return frames, nil, nil
	})

	runCodexSmoke(context.Background(), path, codexSmokeTestVersion)
	drainCodexRunDebtLadder(t)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{email, "smoke.secret", accountID, promptMarker, f.home, filepath.ToSlash(f.home), codexSmokeMarkerPrefix} {
		if strings.Contains(string(raw), s) || strings.Contains(string(raw), strings.ReplaceAll(s, `\`, `\`)) {
			t.Errorf("persisted snapshot leaks %q", s)
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	assertCodexSnapshotStringsRedacted(t, "", decoded)
	if session := codexSessionMetric(t, codexMetricsFromCache(time.Now(), f.fp)); session.Consumed == nil || *session.Consumed != 71 {
		t.Fatalf("the numeric window must still land: %+v", session)
	}
}

// The capture-drift notice names only the two version strings and timestamps:
// never a path, an account, or vendor text.
func TestCodexCaptureDriftNotice_CarriesNoPathAccountOrVendorText(t *testing.T) {
	const email = "drift.secret@example.com"
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	helperCodexAuthAt(t, f.home, email, now.Add(-3*time.Hour))
	f.fp = currentCodexAccountFingerprint()
	f.seedPreRunReading(t, now.Add(-time.Hour), now)
	codexRecordRunFreshness(f.fp, now, func(snap *codexRateLimitSnapshot) { snap.CodexVersion = "codex-cli 0.149.0" })
	state := f.oweExhaustedDebt(t, now.Add(-2*time.Minute), now.Add(-time.Minute))
	codexSetRefreshFallback(f.fp, state.debtID(), codexFallbackExhausted)

	usage, _ := codexUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Version: "codex-cli 0.150.0", Path: ""}, time.Now())

	for _, s := range []string{email, "drift.secret", f.home, filepath.ToSlash(f.home), f.fp, "auth.json", "sessions", "rollout-"} {
		if strings.Contains(usage.Notice, s) {
			t.Errorf("drift notice leaks %q: %q", s, usage.Notice)
		}
	}
	shape := regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC`).ReplaceAllString(usage.Notice, "<ts>")
	if want := `Codex utilization was last observed <ts> by Codex build "codex-cli 0.149.0"; the installed build "codex-cli 0.150.0" has not reported utilization since the most recent Codex run started (<ts>). It will update once that build's telemetry is captured.`; shape != want {
		t.Fatalf("drift notice must be versions and timestamps over fixed copy only:\n got %q\nwant %q", usage.Notice, want)
	}
}

// The schedule's and the nudge's log lines carry fixed labels, counters and
// durations only — never a path, an account, a fingerprint or rollout text.
func TestCodexRefreshSchedule_LogsCarryCountersOnly(t *testing.T) {
	const email = "schedule.secret@example.com"
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-3*time.Hour))
	helperCodexAuthAt(t, f.home, email, now.Add(-3*time.Hour))
	f.fp = currentCodexAccountFingerprint()
	observed := now.Add(-time.Hour)
	f.seedPreRunReading(t, observed, now)
	rollout := now.Add(-2 * codexForcedReconcileMinInterval).Truncate(time.Millisecond)

	logged := captureStdout(t, func() {
		// The nudge's "refresh owed" line, then the schedule's rung line.
		nudgeCodexUsageRefresh(f.home, f.fp, now, codexRolloutNudgeEvidence{newest: rollout}, observed)
		drainCodexRunDebtLadder(t)
	})

	if !strings.Contains(logged, "[cli-usage]") {
		t.Fatalf("expected the refresh log lines, got %q", logged)
	}
	for _, s := range []string{
		email, "schedule.secret", f.home, filepath.ToSlash(f.home), f.fp,
		"auth.json", "rollout-", "codex_rate_limits.json",
	} {
		if strings.Contains(logged, s) {
			t.Errorf("refresh schedule log leaks %q:\n%s", s, logged)
		}
	}
}

// The fields this change persists — the capture generation and the early-read
// reservation — are integers only, and the receipt addition is two integers.
func TestCodexGenerationAndEarlyRead_PersistIntegersOnly(t *testing.T) {
	withCodexGenerationEpoch(t, 4503599627370497)
	r := newCodexFencedRun(t)
	stubCodexFallbackRead(t, noReadingFallback)
	codexPayRunRefresh(r.f.home, r.f.fp)

	raw, err := os.ReadFile(r.f.cache)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generationEpoch", "generation", "earlyReadFloorMs", "earlyReadOwedAtMs"} {
		v, ok := fields[key]
		if !ok {
			t.Fatalf("expected %q in the persisted snapshot", key)
		}
		if n, isNum := v.(float64); !isNum || n != float64(int64(n)) {
			t.Fatalf("%q = %#v, want an integer", key, v)
		}
	}

	canonical, _, _, err := canonicalCLIUsageRefreshReceipt("r", 1, true, []cliAgentUsage{{
		Provider: "codex", CollectedAt: "now", UsageGeneration: &cliUsageGeneration{Epoch: 4503599627370497, Counter: 9},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"usageGeneration":{"epoch":4503599627370497,"counter":9}`) {
		t.Fatalf("receipt addition is not two integers: %s", canonical)
	}
}

// Propagator log lines are fixed labels: never a path, an account, an email, a
// fingerprint, a thread id or a credit balance.
func TestCLIUsageHint_LogsCarryFixedLabelsOnly(t *testing.T) {
	const email = "hint.secret@example.com"
	withCodexGenerationEpoch(t, 777)
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	helperCodexAuthAt(t, f.home, email, now.Add(-time.Hour))
	f.fp = currentCodexAccountFingerprint()
	rec, cfg := propagatorFixture(t)
	rec.status = func(cliUsageObservedHint) int { return 409 }

	logged := captureStdout(t, func() {
		captureCodexRateLimitLineForAccount(codexCurrentBuildTokenCount(t, "codex", 27, now.Add(96*time.Hour), now), now, f.fp)
		startCLIUsagePropagator(cfg)
		waitHints(t, rec, 2, cliUsageHintSpacing/2)
		stopCLIUsagePropagator()
	})

	label := regexp.MustCompile(`^\[cli-usage\] usage hint: (sent|followup_sent|deferred|skipped_offline|skipped_unregistered|awaiting_rotation|rotation_retry|dropped|dropped_applied|dropped_rejected|dropped_retryable|refused_retryable|refused|failed_\d+)( provider=(codex|claudeCode))?$`)
	ansi := regexp.MustCompile("\x1b\\[[0-9;]*m")
	hintLines := 0
	for _, line := range strings.Split(ansi.ReplaceAllString(logged, ""), "\n") {
		if !strings.Contains(line, "usage hint") {
			continue
		}
		hintLines++
		if !label.MatchString(strings.TrimSpace(line)) {
			t.Errorf("hint log line is not a fixed label: %q", line)
		}
	}
	if hintLines == 0 {
		t.Fatalf("expected usage hint log lines, got %q", logged)
	}
	for _, s := range []string{email, "hint.secret", f.home, filepath.ToSlash(f.home), f.fp, "123.45"} {
		if strings.Contains(logged, s) {
			t.Errorf("propagator log leaks %q:\n%s", s, logged)
		}
	}
}
