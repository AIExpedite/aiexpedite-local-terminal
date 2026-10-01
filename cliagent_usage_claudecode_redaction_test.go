package main

// Everything the Claude owed-refresh path persists must be numeric metrics,
// timestamps, fingerprints and closed-enum labels. A probe response and a
// smoke child both sit right next to credentials, config fragments, account
// identity and private paths; none of it may reach claude_rate_limits.json,
// the published usage, or the signed refresh receipt.
//
// Mirrors cliagent_usage_codex_redaction_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeRunFreshness_PersistsAndPublishesNumericOnly(t *testing.T) {
	const (
		email        = "person.secret@example.com"
		tokenMarker  = "sk-ant-oat-LEAK-do-not-persist-7c1f"
		configMarker = `"permissions":{"allow":["Bash(rm:*)"]}`
		promptMarker = "PROMPT_MARKER_do_not_persist_9ab2"
	)
	now := time.Now()
	// The probe response carries the leaky material in fields the allow-listed
	// decode does not know about — the shape a future server version could
	// legitimately send.
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"account":{"email":%q,"token":%q},"settings":%s,
			"limits":[{"kind":"session","percent":52,"resets_at":%d}]}`,
			email, tokenMarker, "{"+configMarker+"}", now.Add(time.Hour).Unix())
	})
	homeMarker, _ := os.UserHomeDir()

	// A smoke child that answers with the marker AND spews secrets on stderr.
	resetCLISmokeState()
	t.Cleanup(resetCLISmokeState)
	stubSmokePath(t, stubClaudeBinary(t))
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(_ context.Context, _ []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)),
			[]byte(email + " " + tokenMarker + " " + configMarker + " " + promptMarker + " " + homeMarker), nil
	})

	result := runClaudeCodeSmoke(context.Background(), resolveClaudeSmokePath(), "2.1.251 (Claude Code)")
	if result.Status != cliSmokeStatusSuccess {
		t.Fatalf("fixture smoke did not pass: %+v", result)
	}
	// The smoke owes a refresh, and the trailing probe pays it against the leaky
	// fixture response — which is exactly the write this case inspects.
	waitForClaudeProbeReading(t, cache, 10*time.Second)
	claudeFreshnessWaitIdle(t)

	forbidden := []string{email, "person.secret", tokenMarker, "sk-ant-oat-LEAK",
		"permissions", "Bash(rm", promptMarker, claudeSmokeMarkerPrefix}
	if homeMarker != "" {
		forbidden = append(forbidden, homeMarker)
	}

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
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
	assertClaudeSnapshotStringsRedacted(t, "", decoded)

	// The published smoke receipt and the published usage metrics.
	published, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	metrics, err := json.Marshal(usage.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		if strings.Contains(string(published), s) {
			t.Errorf("published smoke result leaks %q", s)
		}
		if strings.Contains(string(metrics), s) {
			t.Errorf("published usage metrics leak %q", s)
		}
	}
}

var (
	claudeRedactedHex       = regexp.MustCompile(`^[0-9a-f]*$`)
	claudeRedactedTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
	// The closed vocabulary the snapshot's own string fields may hold: window
	// statuses and the writer provenance labels.
	claudeRedactedEnum = regexp.MustCompile(`^(allowed|rejected|warning|stream|statusline|probe)$`)
)

// assertClaudeSnapshotStringsRedacted walks the decoded snapshot: numbers and
// booleans are metrics / flags; every string must be an account fingerprint
// (hex), an RFC3339 timestamp, or a value from the closed enums above. Map keys
// are window ids.
func assertClaudeSnapshotStringsRedacted(t *testing.T, path string, v any) {
	t.Helper()
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			assertClaudeSnapshotStringsRedacted(t, path+"."+k, child)
		}
	case []any:
		for _, child := range val {
			assertClaudeSnapshotStringsRedacted(t, path+"[]", child)
		}
	case string:
		if !claudeRedactedHex.MatchString(val) && !claudeRedactedTimestamp.MatchString(val) &&
			!claudeRedactedEnum.MatchString(val) {
			t.Errorf("snapshot field %s holds free text %q", path, val)
		}
	}
}

// claudeRefreshOutcomeLine is the ONLY shape a Claude refresh log line may take:
// a fixed prefix, a label from the closed set, and two integers.
var claudeRefreshOutcomeLine = regexp.MustCompile(
	`^(\x1b\[\d+m)?\[claude-usage\] refresh outcome=(ok|held|timeout|unauthorized|http_error|parse_failed|not_covering|refused|scheduled|retired) attempts=\d+/\d+(\x1b\[0m)?$`)

// The ladder's persisted additions are integers, and every refresh outcome it
// logs is a closed-set label plus integers — no token, path, fingerprint or
// response body, even when the failing response carries them.
func TestClaudeRunDebtLadder_PersistsIntegersAndLogsClosedLabels(t *testing.T) {
	const tokenMarker = "sk-ant-oat-LEAK-in-error-body-41d0"
	resets := time.Now().Add(time.Hour)
	var fail atomic.Bool
	fail.Store(true)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":%q,"path":"C:\\Users\\someone\\.claude"}`, tokenMarker)
			return
		}
		fmt.Fprintf(w, `{"limits":[{"kind":"session","percent":11,"resets_at":%d},{"kind":"weekly_all","percent":22,"resets_at":%d}]}`,
			resets.Unix(), resets.Unix())
	})
	pinClaudeRunDebtLadder(t, []time.Duration{100 * time.Millisecond}, 100*time.Millisecond)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	fp := currentClaudeAccountFingerprint()
	home, _ := os.UserHomeDir()

	var snap claudeRateLimitSnapshot
	out := captureStdout(t, func() {
		triggerClaudeUsageProbeAfterRun()
		waitForClaudeDebt(t, cache, 5*time.Second)
		claudeFreshnessWaitIdle(t)
		snap = claudeCacheSnapshot(t, cache)
		fail.Store(false)
		waitForClaudeProbeReading(t, cache, 10*time.Second)
		claudeFreshnessWaitIdle(t)
	})

	if snap.NextAttemptAtMs == 0 {
		t.Fatalf("the failed attempt booked no rung: %+v", snap)
	}
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if v, ok := decoded["lastProbeWeeklyObservedAtMs"]; !ok {
		t.Error("a weekly probe reading persisted no lastProbeWeeklyObservedAtMs")
	} else if _, isNumber := v.(float64); !isNumber {
		t.Errorf("lastProbeWeeklyObservedAtMs is %T, want a number", v)
	}
	assertClaudeSnapshotStringsRedacted(t, "", decoded)
	if strings.Contains(string(raw), tokenMarker) {
		t.Error("the persisted snapshot leaks the error body")
	}

	lines := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "[claude-usage]") {
			continue
		}
		lines++
		if !claudeRefreshOutcomeLine.MatchString(strings.TrimRight(line, "\r")) {
			t.Errorf("refresh log line is not a closed label plus integers: %q", line)
		}
		for _, leak := range []string{tokenMarker, probeTestToken, "Users", fp, home} {
			if leak != "" && strings.Contains(line, leak) {
				t.Errorf("refresh log line leaks %q: %q", leak, line)
			}
		}
	}
	if lines == 0 {
		t.Fatal("the ladder logged no refresh outcome at all")
	}
}
