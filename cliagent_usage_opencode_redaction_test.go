// cliagent_usage_opencode_redaction_test.go — strict redaction for the OpenCode
// usage ledger, its freshness state and the published snapshot.
//
// The allowlist is deliberately spelled out here rather than derived from the
// structs: a new field must be a DECISION, and a test that walked the struct
// would approve whatever the struct grew.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Everything the two state files may hold.
var (
	openCodeLedgerAllowedKeys = map[string]bool{
		"schemaVersion": true, "generation": true, "epoch": true, "counter": true,
		"reconcileCursorMs": true, "lastSuccessfulReconcileAtMs": true,
		"lastPassOutcome": true, "lastPassStartedAtMs": true,
		"continuationDue": true, "continuationFailures": true, "continuationFirstFailureAtMs": true,
		"skipped": true, "session": true, "updatedMs": true,
		"days": true, "messages": true, "streamSessions": true, "partial": true,
		"in": true, "out": true, "reasoning": true, "cacheRead": true, "cacheWrite": true,
		"costMicros": true, "observedAtMs": true,
	}
	openCodeFreshnessAllowedKeys = map[string]bool{
		"schemaVersion": true, "runFloorMs": true, "completionMs": true, "owedAtMs": true,
		"nextAttemptAtMs": true, "lastAttemptAtMs": true, "attempts": true, "lastOutcome": true,
	}
	// The only non-numeric values allowed anywhere: a day key, a 16-hex hash,
	// and the closed outcome codes.
	openCodeDayKeyPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	openCodeHashPattern   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	openCodeOutcomeCodes  = map[string]bool{
		openCodeReconcileOK: true, openCodeReconcileNoChange: true, openCodeReconcileMore: true,
		openCodeReconcileUnsupported: true, openCodeReconcileTimeout: true,
		openCodeReconcileLaunchError: true, openCodeReconcileOffline: true,
	}
)

// auditOpenCodeJSON walks a decoded state file and fails on anything outside
// the allowlist.
func auditOpenCodeJSON(t *testing.T, label string, value any, allowedKeys map[string]bool, path string) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			// A MAP KEY is either an allowlisted field name, a day key, or a
			// 16-hex message hash.
			if !allowedKeys[key] && !openCodeDayKeyPattern.MatchString(key) && !openCodeHashPattern.MatchString(key) {
				t.Fatalf("%s%s: key %q is not on the redaction allowlist", label, path, key)
			}
			auditOpenCodeJSON(t, label, child, allowedKeys, path+"."+key)
		}
	case []any:
		for i, child := range typed {
			auditOpenCodeJSON(t, label, child, allowedKeys, fmt.Sprintf("%s[%d]", path, i))
		}
	case string:
		if !openCodeHashPattern.MatchString(typed) && !openCodeOutcomeCodes[typed] {
			t.Fatalf("%s%s: value %q is neither a 16-hex hash nor a closed outcome code", label, path, typed)
		}
	case float64, bool, nil:
		// Integers and the three documented bools.
	default:
		t.Fatalf("%s%s: unexpected value type %T", label, path, typed)
	}
}

func TestOpenCodeUsage_StateFilesHoldNothingButIntegersHashesAndClosedCodes(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeDebtFixture(t, now)

	// Seed every field a real device would write, with ids and text a leak
	// would carry.
	const (
		secretSession = "ses_super_secret_project_name"
		secretMessage = "msg_01HQZZZ_user_prompt_id"
	)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{secretSession, now.UnixMilli()}),
		oversize: map[string]bool{secretSession: true},
	}).install(t)

	handle := armOpenCodeUsageRun("native chat")
	handle.Observe(fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":%q,`+
		`"part":{"messageID":%q,"type":"step-finish","reason":"stop","cost":0.42,`+
		`"tokens":{"input":11,"output":22,"reasoning":3,"cache":{"read":4,"write":5}}}}`,
		now.UnixMilli(), secretSession, secretMessage))
	handle.Finish(false) // owes, so the freshness file is written too
	openCodeUsageRefreshWaitFor(3 * time.Second)
	reconcileOpenCodeUsageOnce(nil, now)

	for label, path := range map[string]string{
		"ledger":    openCodeUsageLedgerPath(),
		"freshness": openCodeUsageFreshnessPath(),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s was never written: %v", label, err)
		}
		text := string(raw)
		for _, secret := range []string{
			secretSession, secretMessage, "super_secret", "user_prompt",
			"native chat", "anthropic", "claude", "opencode-stub",
		} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(secret)) {
				t.Fatalf("%s leaked %q:\n%s", label, secret, text)
			}
		}
		// No absolute path fragment either.
		if strings.Contains(text, "/Users") || strings.Contains(text, "C:\\") ||
			strings.Contains(text, "/home/") || strings.Contains(text, "/var/folders") {
			t.Fatalf("%s leaked a path:\n%s", label, text)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s is not JSON: %v", label, err)
		}
		allowed := openCodeLedgerAllowedKeys
		if label == "freshness" {
			allowed = openCodeFreshnessAllowedKeys
		}
		auditOpenCodeJSON(t, label, decoded, allowed, "")
	}
}

func TestOpenCodeUsage_LedgerHoldsTheHashedSessionNotTheId(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	const sessionID = "ses_a_project_the_user_would_recognise"

	handle := armOpenCodeUsageRun("unit")
	handle.Observe(fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":%q,`+
		`"part":{"messageID":"msg_1","type":"step-finish","tokens":{"input":5}}}`,
		now.UnixMilli(), sessionID))
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	day := readOpenCodeUsageLedger().Days[openCodeDayKey(now)]
	if len(day.StreamSessions) != 1 || day.StreamSessions[0] != openCodeUsageHash(sessionID, "") {
		t.Fatalf("streamSessions = %v, want one 16-hex hash", day.StreamSessions)
	}
	for key := range day.Messages {
		if !openCodeHashPattern.MatchString(key) {
			t.Fatalf("message key %q is not a 16-hex hash", key)
		}
	}
}

func TestOpenCodeUsage_SnapshotCarriesNoIdsOrPaths(t *testing.T) {
	now := openCodeFixtureRunAt
	openCodeDebtFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	observeFixture(t, handle, "run_two_steps.jsonl")
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)
	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		return openCodeLedgerEdit{Changed: openCodeMarkTodayPartial(l, now)}
	})

	metrics, generation, partial := openCodeLedgerMetrics(now)
	if !partial || generation == nil || len(metrics) == 0 {
		t.Fatalf("metrics=%+v generation=%+v partial=%v", metrics, generation, partial)
	}
	// Only the three wire fields this feature adds are in scope here; the
	// snapshot's pre-existing identity fields (account, models, path,
	// accountFingerprint) are owned by the readiness probe.
	payload, err := json.Marshal(struct {
		Metrics    []cliAgentUsageMetric `json:"metrics"`
		Notice     string                `json:"notice"`
		Generation *cliUsageGeneration   `json:"usageGeneration"`
	}{metrics, openCodeUsagePartialNotice, generation})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, secret := range []string{"ses_", "msg_", "/Users", "anthropic", "prt_"} {
		if strings.Contains(text, secret) {
			t.Fatalf("the snapshot leaked %q:\n%s", secret, text)
		}
	}
	// The labels and units are locally authored constants, not vendor text.
	for _, metric := range metrics {
		if metric.Label != "Tokens today" && metric.Label != "Cost today" {
			t.Fatalf("unexpected label %q", metric.Label)
		}
		if metric.Model != "" {
			t.Fatalf("a usage counter must name no model: %+v", metric)
		}
	}
}

func TestOpenCodeUsage_LogLinesCarryFixedLabelsOnly(t *testing.T) {
	// logOpenCodeUsage is the only logger this feature has, and every call site
	// passes a closed outcome code plus counters.
	for _, call := range []func(){
		func() { logOpenCodeUsage("owed attempts=%d", 2) },
		func() { logOpenCodeUsage("%s attempts=%d exported=%d remaining=%d", openCodeReconcileOK, 1, 2, 3) },
		func() { logOpenCodeUsage("scheduled attempts=%d", 1) },
		func() { logOpenCodeUsage("resumed attempts=%d", 1) },
		func() { logOpenCodeUsage("resumed continuation") },
		func() { logOpenCodeUsage("shutdown drain timed out inFlight=%d", 0) },
	} {
		call()
	}
	// The real assertion is structural: the only formatter is this one
	// function, and every verb it is given is a number or a closed code. A
	// grep-style guard keeps a future call site from passing vendor text.
	source, err := os.ReadFile("cliagent_usage_opencode_freshness.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(source), "\n") {
		if !strings.Contains(line, "logOpenCodeUsage(") {
			continue
		}
		if strings.Contains(line, "sessionID") || strings.Contains(line, "messageID") ||
			strings.Contains(line, "path") || strings.Contains(line, "stdout") ||
			strings.Contains(line, "stderr") {
			t.Fatalf("a log call site names vendor data: %s", strings.TrimSpace(line))
		}
	}
}
