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
	waitCodexUsageRefreshIdle(t)

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
		case codexFallbackOutstanding, codexFallbackSpent, codexFallbackSkipped:
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
	waitCodexUsageRefreshIdle(t)

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
	codexSetRefreshFallback(f.fp, state.debtID(), codexFallbackSpent)

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
