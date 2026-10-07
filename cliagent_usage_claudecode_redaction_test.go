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

// A failing debt attempt carrying a sentinel token, a sentinel response body
// and a prose Retry-After: none of the three reaches the snapshot, the device
// log line or the published usage, and the schedule fields it adds are numbers.
func TestClaudeRunDebtSchedule_PersistsLogsAndPublishesNumericOnly(t *testing.T) {
	const (
		tokenSentinel = "sk-ant-oat-SENTINEL-never-persist-4e1a"
		bodySentinel  = "BODY_SENTINEL_never_persist_77c0"
		proseRetry    = "please wait until the PROSE_SENTINEL quota frees up"
	)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", proseRetry)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, bodySentinel)
	})
	writeClaudeProbeCredential(t, os.Getenv("CLAUDE_CONFIG_DIR"), tokenSentinel)
	now := time.Now()
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
	claudeOweRunRefresh(now.Add(-time.Minute))

	logged := captureStdout(t, func() {
		if result := claudeRunDebtAttemptAt(now, claudeDebtTriggerRun); result.code != claudeProbeHTTP429 {
			t.Errorf("result=%+v, want http_429", result)
		}
	})
	// A credential wait is persisted beside the rung on the next booking.
	claudeUsageProbe.noteAuthWait(claudeCredStamp{modNs: 1, size: 2})
	claudeBookRunDebtRung("", now.Add(-time.Minute), now, claudeRungFree, 0)

	forbidden := []string{tokenSentinel, "SENTINEL", bodySentinel, proseRetry, "PROSE_SENTINEL", os.Getenv("CLAUDE_CONFIG_DIR")}
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	published, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		for surface, text := range map[string]string{"snapshot": string(raw), "log": logged, "published usage": string(published)} {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("%s leaks %q", surface, s)
			}
		}
	}
	if !strings.Contains(logged, "issued http_429") {
		t.Errorf("log line %q does not carry the fixed result code", logged)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	assertClaudeSnapshotStringsRedacted(t, "", decoded)
	for _, field := range []string{"nextAttemptAtMs", "authWaitCredStampNs", "authWaitCredSize", "refreshOwedAttempts", "attemptClaimedUntilMs"} {
		if v, present := decoded[field]; present {
			if _, isNumber := v.(float64); !isNumber {
				t.Errorf("%s=%v (%T), want a number", field, v, v)
			}
		}
	}
	for _, field := range []string{"nextAttemptAtMs", "authWaitCredStampNs", "authWaitCredSize"} {
		if _, present := decoded[field]; !present {
			t.Errorf("%s was not persisted", field)
		}
	}
}

// A failing probe with NO debt owed leaves the cache byte-identical; a failing
// attempt on an owed debt leaves the bucket bytes identical and changes only
// the bounded schedule fields — RefreshOwedAttempts and NextAttemptAtMs.
func TestClaudeRunDebtSchedule_FailureTouchesOnlyScheduleFields(t *testing.T) {
	t.Run("no debt", func(t *testing.T) {
		cache, calls := armClaudeUsageProbe(t, unreachableProbeHandler)
		seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
		before, _ := os.ReadFile(cache)
		refreshClaudeUsageIfStale(context.Background(), time.Now(), time.Time{}, probeTestToken, "")
		if atomic.LoadInt64(calls) != 1 {
			t.Fatal("precondition: the stale gather should probe")
		}
		if after, _ := os.ReadFile(cache); string(after) != string(before) {
			t.Errorf("a failed probe with no debt changed the cache:\n got %s\nwant %s", after, before)
		}
	})
	t.Run("owed debt", func(t *testing.T) {
		cache, _ := armClaudeUsageProbe(t, unreachableProbeHandler)
		now := time.Now()
		seedClaudeProbeReading(t, cache, now.Add(-time.Hour))
		claudeOweRunRefresh(now.Add(-time.Minute))
		before := claudeCacheSnapshot(t, cache)
		bucketsBefore, _ := json.Marshal(before.Buckets)

		if result := claudeRunDebtAttemptAt(now, claudeDebtTriggerRun); result.code != claudeProbeHTTP5xx {
			t.Fatalf("result=%+v, want http_5xx", result)
		}

		after := claudeCacheSnapshot(t, cache)
		if bucketsAfter, _ := json.Marshal(after.Buckets); string(bucketsAfter) != string(bucketsBefore) {
			t.Errorf("bucket bytes changed:\n got %s\nwant %s", bucketsAfter, bucketsBefore)
		}
		if after.RefreshOwedAttempts != 1 || after.NextAttemptAtMs == 0 {
			t.Errorf("attempts=%d rung=%d, want the slot spent and a rung booked", after.RefreshOwedAttempts, after.NextAttemptAtMs)
		}
		after.RefreshOwedAttempts, after.NextAttemptAtMs = before.RefreshOwedAttempts, before.NextAttemptAtMs
		after.Buckets, before.Buckets = nil, nil
		if a, b := mustJSON(t, after), mustJSON(t, before); string(a) != string(b) {
			t.Errorf("fields beyond the schedule changed:\n got %s\nwant %s", a, b)
		}
	})
}

// The capture generation, the hint, the startup recovery and every failure
// outcome log fixed labels and nothing else; the hint body is provider plus
// integers. The account fingerprint stays inside the local cache.
func TestClaudeUsageHint_LogsFixedLabelsAndSendsNoIdentity(t *testing.T) {
	const (
		secret  = "hint-secret-LEAK-4e1a"
		agentID = "agent-LEAK-9f2b"
		account = "person.secret@example.com"
	)
	rec, cfg := propagatorFixture(t)
	cfg.AgentID, cfg.CommandSecret = agentID, secret
	cache := t.TempDir() + "/claude_rate_limits.json"
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	fingerprint := fingerprintAccount(claudeUsageProvider, account)
	calls := 0
	rec.status = func(cliUsageObservedHint) int {
		calls++
		if calls == 1 {
			return 0 // a transport failure, then accepted
		}
		return 202
	}

	logs := captureStdout(t, func() {
		observed := time.Now()
		mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
			claudeWindowFiveHour: {UsedPercentage: 30, ResetsAtMs: observed.Add(time.Hour).UnixMilli(), ObservedAtMs: observed.UnixMilli(), usageKnown: true},
		}, observed, fingerprint, claudeRateLimitSourceStream)
		startCLIUsagePropagator(cfg) // the startup recovery reads the same commit
		waitHints(t, rec, 2, cliUsageHintSpacing)

		g := gen(5, 1)
		ctx, reservation := withCLIUsageReceiptReservation(context.Background())
		observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
		settleCLIUsageReceipt(reservation, false, nil) // a failed receipt
		ctx, reservation = withCLIUsageReceiptReservation(context.Background())
		g2 := gen(5, 2)
		observeCLIUsageGeneration(ctx, claudeUsageProvider, &g2)
		// The published receipt carried a newer generation than the released
		// one: its single confirmation stands for both.
		settleCLIUsageReceipt(reservation, true, []cliAgentUsage{{Provider: claudeUsageProvider, UsageGeneration: &g2}})
		waitHints(t, rec, 3, 2*cliUsageHintSpacing)
		stopCLIUsagePropagator()
	})

	label := regexp.MustCompile(`^\[cli-usage\] usage hint: (sent|followup_sent|failed_\d+|deferred|dropped|skipped_offline|skipped_unregistered|awaiting_rotation|rotation_retry|receipt_released|confirmation_pending)$`)
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(line, ""))
		if !strings.Contains(line, "usage hint") {
			continue
		}
		if !label.MatchString(line) {
			t.Fatalf("unexpected hint log line %q", line)
		}
	}
	home, _ := os.UserHomeDir()
	for _, leaked := range []string{secret, agentID, account, fingerprint, cache, home} {
		if leaked != "" && strings.Contains(logs, leaked) {
			t.Fatalf("logs carry %q:\n%s", leaked, logs)
		}
	}
	for _, h := range rec.all() {
		body, _ := json.Marshal(h.hint)
		var fields map[string]any
		_ = json.Unmarshal(body, &fields)
		if len(fields) != 5 || fields["provider"] != claudeUsageProvider {
			t.Fatalf("hint body = %s, want timestamp, signature, provider and the two integers only", body)
		}
		for _, leaked := range []string{secret, account, fingerprint, cache} {
			if strings.Contains(string(body), leaked) {
				t.Fatalf("hint body carries %q: %s", leaked, body)
			}
		}
	}
	if raw, _ := os.ReadFile(cache); !strings.Contains(string(raw), fingerprint) || strings.Contains(string(raw), account) {
		t.Fatalf("the cache must scope by the one-way fingerprint and never hold the raw account: %s", raw)
	}
}
