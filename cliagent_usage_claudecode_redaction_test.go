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

// waitForClaudeProbeReading polls until the probe has persisted a reading. The
// trailing probe is asynchronous by design, so polling is the only honest way
// to observe it.
func waitForClaudeProbeReading(t *testing.T, cache string, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok && snap.LastProbeObservedAtMs != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the smoke's trailing probe never persisted a reading")
}
