package main

// Everything the Claude owed-refresh path persists must be numeric metrics,
// timestamps, fingerprints and closed-enum labels. A probe response and a
// smoke child both sit right next to credentials, config fragments, account
// identity and private paths; none of it may reach claude_rate_limits.json,
// the published usage, or the signed refresh receipt.
//
// One exception, and only one: a window this agent never modelled persists
// the row it renders as — reportedWindows.<limit:…>.{kind,label,model} —
// because limits[] carries no label and a later read has nothing but the cache
// to build that row from. claudeGenericWindow derives those three strings from
// the entry's kind, group and scope display names alone, printable and
// bounded, and they are the very row the card publishes (new windows travel as
// free-text kind / label / model: the receipt has no other field for them).
// assertClaudeSnapshotStringsRedacted admits them there and nowhere else, and
// TestClaudeGenericWindow_PersistsOnlyTheRowItRendersAs pins their values.
//
// Mirrors cliagent_usage_codex_redaction_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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
	// The one place free text may sit (see the file header): a generic window's
	// row. Window ids are slugs, so the path splits unambiguously on ".".
	claudeRedactedGenericRow = regexp.MustCompile(`^\.reportedWindows\.limit:[a-z0-9_:]+\.(kind|label|model)$`)
)

// assertClaudeSnapshotStringsRedacted walks the decoded snapshot: numbers and
// booleans are metrics / flags; every string must be an account fingerprint
// (hex), an RFC3339 timestamp, or a value from the closed enums above — except
// a generic window's derived row, which must be bounded and printable. Map keys
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
		if claudeRedactedGenericRow.MatchString(path) {
			if len(val) > claudeWindowLabelMaxBytes || utf8.RuneCountInString(val) > claudeWindowLabelMaxRunes ||
				claudeWindowDisplayText(val) != val {
				t.Errorf("snapshot field %s holds %q, which is not a bounded, printable row string", path, val)
			}
			return
		}
		if !claudeRedactedHex.MatchString(val) && !claudeRedactedTimestamp.MatchString(val) &&
			!claudeRedactedEnum.MatchString(val) {
			t.Errorf("snapshot field %s holds free text %q", path, val)
		}
	}
}

// A window this agent never modelled is the one place vendor text reaches the
// cache (see the file header): the row it renders as, and nothing else from the
// reply — not the entry's other fields, not its scope's, not the body around
// them. What the cache holds is exactly claudeGenericWindow's derivation, and
// the card publishes the same strings.
func TestClaudeGenericWindow_PersistsOnlyTheRowItRendersAs(t *testing.T) {
	const (
		email       = "person.secret@example.com"
		tokenMarker = "sk-ant-oat-LEAK-do-not-persist-7c1f"
		proseMarker = "PROSE_MARKER_do_not_persist_51d0"
	)
	now := time.Now()
	reset := now.Add(time.Hour).Unix()
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"account":{"email":%q,"token":%q},"note":%q,"limits":[
			{"kind":"session","percent":52,"resets_at":%d},
			{"kind":"weekly_scoped","group":"weekly","percent":5,"resets_at":%d,"severity":%q,"owner":%q,
				"scope":{"model":{"id":null,"display_name":"Haiku 5","granted_by":%q},"surface":null,"note":%q}},
			{"kind":"daily_all","group":"daily","percent":7,"resets_at":%d,"description":%q,
				"scope":{"model":null,"surface":{"display_name":"Claude Code","admin":%q}}}
		]}`, email, tokenMarker, proseMarker, reset, reset, proseMarker, email, email, tokenMarker, reset, proseMarker, email)
	})
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now, false); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}

	forbidden := []string{email, "person.secret", tokenMarker, "sk-ant-oat-LEAK", proseMarker, "PROSE_MARKER"}
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		if strings.Contains(string(raw), s) {
			t.Errorf("persisted snapshot leaks %q", s)
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	assertClaudeSnapshotStringsRedacted(t, "", decoded)

	snap := claudeCacheSnapshot(t, cache)
	for id, want := range map[string]struct{ kind, label, model string }{
		"limit:weekly_scoped:model:haiku_5":   {"weekly", "Weekly Haiku 5", "haiku"},
		"limit:daily_all:surface:claude_code": {"daily", "Daily Claude Code", ""},
	} {
		got, ok := snap.ReportedWindows[id]
		if !ok {
			t.Errorf("the set lacks %s: %+v", id, snap.ReportedWindows)
			continue
		}
		if got.Kind != want.kind || got.Label != want.label || got.Model != want.model {
			t.Errorf("%s persisted {%q %q %q}, want {%q %q %q}", id, got.Kind, got.Label, got.Model, want.kind, want.label, want.model)
		}
	}
	if got := snap.ReportedWindows[claudeWindowFiveHour]; got.Kind != "" || got.Label != "" || got.Model != "" {
		t.Errorf("the modelled session window persisted a row %+v; it keeps its id only", got)
	}

	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{}, now)
	if !ok {
		t.Fatal("parse failed")
	}
	assertMetricSummary(t, usage.Metrics,
		"5-hour session window|session||52", "Weekly Haiku 5|weekly|haiku|5", "Daily Claude Code|daily||7")
	metrics, err := json.Marshal(usage.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		if strings.Contains(string(metrics), s) {
			t.Errorf("published usage metrics leak %q", s)
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

// The SessionEnd hook sits next to the ending session's id, transcript path,
// cwd and whatever the user's prompt was. With hostile stdin it prints nothing,
// and its only change to the cache is the integer debt fields; the resident
// adoption logs fixed labels only.
func TestClaudeRunEndHook_WritesNumbersOnlyAndPrintsNothing(t *testing.T) {
	const (
		tokenSentinel  = "sk-ant-oat-RUNEND-never-persist-51d3"
		emailSentinel  = "runend.person@example.com"
		promptSentinel = "PROMPT_RUNEND_never_persist_0c4e"
	)
	cache, _ := armClaudeDirectRunTest(t, claudeBothWindowsHandler)
	seedClaudeProbeReading(t, cache, time.Now().Add(-time.Hour))
	before := claudeSnapshotMap(t, cache)

	transcript := filepath.Join(t.TempDir(), "TRANSCRIPT_SENTINEL", "session.jsonl")
	stdin := fmt.Sprintf(`{"session_id":"sess-SENTINEL-1","transcript_path":%q,"cwd":%q,"hook_event_name":"SessionEnd","reason":"other","prompt":%q,"token":%q,"email":%q}`,
		transcript, filepath.Dir(transcript), promptSentinel, tokenSentinel, emailSentinel)
	stdout, stderr := runClaudeRunEndHookWithStdio(t, stdin)
	if stdout != "" || stderr != "" {
		t.Fatalf("the hook printed stdout=%q stderr=%q, want nothing", stdout, stderr)
	}

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{tokenSentinel, emailSentinel, promptSentinel, "SENTINEL", transcript} {
		if strings.Contains(string(raw), s) {
			t.Errorf("the cache leaks %q", s)
		}
	}
	after := claudeSnapshotMap(t, cache)
	debtFields := map[string]bool{"refreshOwedAtMs": true, "refreshOwedAttempts": true, "nextAttemptAtMs": true}
	for k, v := range after {
		if fmt.Sprint(before[k]) == fmt.Sprint(v) {
			continue
		}
		if !debtFields[k] {
			t.Errorf("the hook changed %s", k)
		}
		if _, isNumber := v.(float64); !isNumber {
			t.Errorf("%s=%v (%T), want a number", k, v, v)
		}
	}
	if _, owed := after["refreshOwedAtMs"]; !owed {
		t.Fatal("the hook wrote no debt")
	}

	logged := captureStdout(t, func() {
		if !adoptObservedClaudeRunDebt(time.Now()) {
			t.Error("the debt was not adopted")
		}
		claudeFreshnessWaitIdle(t)
	})
	if !strings.Contains(logged, "[claude-usage] observed run refresh adopted") {
		t.Errorf("adoption log %q lacks its fixed label", logged)
	}
	for _, s := range []string{probeTestToken, os.Getenv("CLAUDE_CONFIG_DIR"), cache} {
		if strings.Contains(logged, s) {
			t.Errorf("adoption log leaks %q", s)
		}
	}
}

// runClaudeRunEndHookWithStdio runs the subcommand body with stdin fed from a
// file and stdout / stderr captured.
func runClaudeRunEndHookWithStdio(t *testing.T, stdin string) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "stdin")
	if err := os.WriteFile(in, []byte(stdin), 0o600); err != nil {
		t.Fatal(err)
	}
	inFile, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer inFile.Close()
	outFile, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inFile, outFile, errFile
	runClaudeRunEndHook()
	os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr
	_ = outFile.Close()
	_ = errFile.Close()
	o, _ := os.ReadFile(outFile.Name())
	e, _ := os.ReadFile(errFile.Name())
	return string(o), string(e)
}

func claudeSnapshotMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The Claude cold start after an update (cliagent_usage_post_update_cold_start_test.go):
// the transient version-probe notice names the binary only — never the
// config dir, the install path or the credential sitting next to it.
func TestClaudeColdStartVersionProbe_LogsNoSecrets(t *testing.T) {
	const tokenSentinel = "sk-ant-oat-SENTINEL-cold-start-never-log-5b2d"
	armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	writeClaudeProbeCredential(t, os.Getenv("CLAUDE_CONFIG_DIR"), tokenSentinel)
	binDir := installMockClaude(t, "claude-heartbeat-result")
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	helperColdVersionProbeOnce(t, "claude", claudeColdStartVersion)

	logged := captureStdout(t, func() { cachedProbeVersion(filepath.Join(binDir, name)) })
	if !strings.Contains(logged, "version probe transient for "+name) {
		t.Errorf("the transient version probe was not logged by binary name:\n%s", logged)
	}
	for _, s := range []string{tokenSentinel, "SENTINEL", os.Getenv("CLAUDE_CONFIG_DIR"), binDir} {
		if s != "" && strings.Contains(logged, s) {
			t.Errorf("cold-start log leaks %q:\n%s", s, logged)
		}
	}
}
