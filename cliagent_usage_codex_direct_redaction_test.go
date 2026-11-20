package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The direct-run detector reads a rollout written by a run nothing armed —
// prompt text, a pasted bearer token, the account and private paths right next
// to the telemetry. What it logs is one fixed label, and what reaches the cache
// is numeric windows only.
func TestCodexDirectScan_LogsAFixedLabelAndPersistsNumericOnly(t *testing.T) {
	const (
		email        = "person.direct@example.com"
		bearer       = "Bearer sk-proj-DIRECTSCANSECRET0123456789"
		promptMarker = "PROMPT_MARKER_direct_scan_9e2a"
	)
	now := time.Now()
	h := codexDirectFixture(t, now)
	helperCodexAuthAt(t, h.f.home, email, now.Add(-3*time.Hour))
	h.f.fp = currentCodexAccountFingerprint()
	h.f.seedPreRunReading(t, now.Add(-time.Hour), now)
	privatePath := filepath.Join(h.f.home, "private", "project")
	h.endToEnd()

	rollout := now.Add(-time.Minute)
	writeCodexRunRollout(t, h.f.home, "leaky", now.Add(-5*time.Minute), rollout, rollout, true,
		[]map[string]any{codexRateLimitFrame(61, 62, now)},
		map[string]any{"timestamp": rollout.UTC().Format(time.RFC3339Nano), "type": "turn_context", "payload": map[string]any{
			"cwd": privatePath, "user": email, "authorization": bearer,
		}},
		map[string]any{"timestamp": rollout.UTC().Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
			"type": "message", "content": promptMarker + " rate_limits token_count " + bearer,
		}},
	)

	out := captureStdout(t, func() {
		codexDirectScanAtStartup()
		waitCodexUsageRefreshIdle(t)
	})
	if !strings.Contains(out, "[cli-usage] codex direct run: reconciled") {
		t.Fatalf("the scan must log its fixed label, got %q", out)
	}
	forbidden := []string{email, "person.direct", bearer, "sk-proj-", promptMarker, privatePath,
		filepath.ToSlash(privatePath), h.f.home, "rollout-", "sessions", h.f.fp}
	for _, s := range forbidden {
		if strings.Contains(out, s) {
			t.Errorf("the log leaks %q: %q", s, out)
		}
	}

	if s := codexSessionMetric(t, codexMetricsFromCache(now, h.f.fp)); s.Consumed == nil || *s.Consumed != 61 {
		t.Fatalf("session = %+v, want the direct run's 61%%", s)
	}
	raw, err := os.ReadFile(h.f.cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden[:len(forbidden)-1] {
		if strings.Contains(string(raw), s) || strings.Contains(string(raw), strings.ReplaceAll(s, `\`, `\\`)) {
			t.Errorf("persisted snapshot leaks %q", s)
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	assertCodexSnapshotStringsRedacted(t, "", decoded)
}
