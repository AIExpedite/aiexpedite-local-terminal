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
		if !codexRedactedHex.MatchString(val) && !codexRedactedTimestamp.MatchString(val) {
			t.Errorf("snapshot field %s holds free text %q", path, val)
		}
	}
}
