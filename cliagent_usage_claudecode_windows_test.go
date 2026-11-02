package main

// Tests for the window set Claude reports (cliagent_usage_claudecode_windows.go),
// the rows built from it, the plan display name, and the two smaller fixes that
// ship with them (the same-window reset tolerance and the status line's null
// window).
//
// Every fixture here is synthetic: the account ids are made-up UUIDs and the
// addresses are example.com. The Ultra reply and the computer's cache are
// modelled on the ones observed when the defect was reported (2026-11-01) —
// their instants and percentages are kept, because they are what the defect is
// made of, and they carry nothing that identifies an account.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// Synthetic account ids for the account-binding cases.
	claudeTestAccountA = "00000000-0000-4000-8000-0000000000a1"
	claudeTestAccountB = "00000000-0000-4000-8000-0000000000b2"

	// The verified-Ultra reply's resets, as the endpoint spells them: the weekly
	// all-models reset with sub-second jitter, Fable's on the whole second.
	claudeUltraWeekReset  = "2026-11-02T20:59:59.539538+00:00"
	claudeUltraFableReset = "2026-11-02T21:00:00+00:00"
)

var (
	// claudeUltraNow is the gather instant of the replays: 28 minutes after the
	// last probe the cache below records, so a routine gather is due to probe.
	claudeUltraNow = time.UnixMilli(1793569200000).UTC() // 2026-11-01T21:40:00Z
	// The Max-era five-hour reading the defect kept publishing: 0 %, taken with
	// no reset, so it can never roll over.
	claudeUltraStaleFiveHourAt = time.UnixMilli(1793538355291).UTC() // 2026-11-01T13:05:55.291Z
	// The last probe of the weekly windows before the replay.
	claudeUltraLastProbeAt = time.UnixMilli(1793567503215).UTC() // 2026-11-01T21:11:43.215Z
	// When Claude Code last fetched usage itself (cachedUsageUtilization).
	claudeUltraClaudeCodeFetchedAt = time.UnixMilli(1793567483666).UTC() // 2026-11-01T21:11:23.666Z
)

// claudeUltraReplyJSON is the verified-Ultra usage reply: no five-hour window
// in EITHER representation, a weekly all-models limit and a separate weekly
// Fable limit — plus the null legacy windows and code-named keys a decoder must
// never read as plan windows.
func claudeUltraReplyJSON() string {
	return fmt.Sprintf(`{
		"five_hour": null,
		"seven_day": {"utilization": 44, "resets_at": %q, "limit_dollars": null, "used_dollars": null,
			"remaining_dollars": null, "locked_reason": null},
		"seven_day_oauth_apps": null, "seven_day_opus": null, "seven_day_sonnet": null,
		"seven_day_cowork": null, "seven_day_omelette": null,
		"iguana_necktie": null, "tangelo": null, "amber_gauge": null,
		"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null},
		"limits": [
			{"kind": "weekly_all", "group": "weekly", "percent": 44, "severity": "normal",
				"resets_at": %q, "scope": null, "is_active": true},
			{"kind": "weekly_scoped", "group": "weekly", "percent": 0, "severity": "normal",
				"resets_at": %q, "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null},
				"is_active": false}
		],
		"weekly_scoped_shares": [{"limit_index": 1, "used_percent_of_weekly": 0, "allowance_percent_of_weekly": 50}],
		"member_dashboard_available": false
	}`, claudeUltraWeekReset, claudeUltraWeekReset, claudeUltraFableReset)
}

// seedClaudeUltraComputerCache writes the cache an older agent left on the
// computer that reported the defect: the Max-era five_hour bucket beside the
// current weekly windows, and no window set (that agent did not know of one).
func seedClaudeUltraComputerCache(t *testing.T, cache string) {
	t.Helper()
	raw := fmt.Sprintf(`{
  "updatedAt": "2026-11-01T21:11:43Z",
  "buckets": {
    "five_hour": {"usedPercentage": 0, "resetsAtMs": 0, "observedAtMs": %d, "usageObserved": true, "source": "probe"},
    "seven_day": {"usedPercentage": 44, "resetsAtMs": 1793653199539, "observedAtMs": %d, "usageObserved": true, "source": "probe"},
    "seven_day_fable": {"usedPercentage": 0, "resetsAtMs": 1793653200000, "observedAtMs": %d, "usageObserved": true, "source": "probe"}
  },
  "lastProbeObservedAtMs": %d,
  "generationEpoch": 6161520443523029,
  "generation": 6182
}`, claudeUltraStaleFiveHourAt.UnixMilli(), claudeUltraLastProbeAt.UnixMilli(),
		claudeUltraLastProbeAt.UnixMilli(), claudeUltraLastProbeAt.UnixMilli())
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeDotJSON writes Claude Code's main config into dir and forgets the
// cached parse, so a test that rewrites it never reads the previous version.
func writeClaudeDotJSON(t *testing.T, dir string, payload any) {
	t.Helper()
	helperWriteJSON(t, filepath.Join(dir, ".claude.json"), payload)
	resetClaudeDotJSONCache()
	t.Cleanup(resetClaudeDotJSONCache)
}

// claudeUltraDotJSON is the config Claude Code writes for a verified Ultra
// login: the provider's plan name, the account, and its own cached copy of the
// usage reply (fetched for the same account).
func claudeUltraDotJSON(cachedFor string) map[string]any {
	var reply map[string]any
	_ = json.Unmarshal([]byte(claudeUltraReplyJSON()), &reply)
	return map[string]any{
		"oauthAccount": map[string]any{
			"accountUuid":     claudeTestAccountA,
			"emailAddress":    "ultra.user@example.com",
			"planDisplayName": "Ultra",
		},
		"cachedUsageUtilization": map[string]any{
			"fetchedAtMs": claudeUltraClaudeCodeFetchedAt.UnixMilli(),
			"accountUuid": cachedFor,
			"utilization": reply,
		},
	}
}

// metricSummary renders rows as "<label>|<kind>|<model>|<consumed or ?>" so a
// whole published row set can be compared in one assertion.
func metricSummary(metrics []cliAgentUsageMetric) []string {
	out := make([]string, 0, len(metrics))
	for _, m := range metrics {
		value := "?"
		if !m.Unknown && m.Consumed != nil {
			value = fmt.Sprintf("%g", *m.Consumed)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", m.Label, m.Kind, m.Model, value))
	}
	return out
}

func assertMetricSummary(t *testing.T, metrics []cliAgentUsageMetric, want ...string) {
	t.Helper()
	got := metricSummary(metrics)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n  got  %q\n  want %q", got, want)
	}
}

// requireCanonicalClaudeReceipt proves the rows sign: the receipt rejects a
// duplicate kind+label+model, an over-long string or more than 32 rows for the
// WHOLE refresh, so every row set this file builds must pass it.
func requireCanonicalClaudeReceipt(t *testing.T, usage cliAgentUsage) {
	t.Helper()
	if usage.Provider == "" {
		usage.Provider = claudeUsageProvider
	}
	if usage.CollectedAt == "" {
		usage.CollectedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := canonicalProvider(usage); err != nil {
		t.Fatalf("the rows do not canonicalise for the signed receipt: %v\n%+v", err, usage.Metrics)
	}
}

/* ─────────────────────────── the defect, replayed ─────────────────────────── */

// The reported defect, end to end through the real gather: the verified-Ultra
// reply replayed over the cache an older agent left on the computer that
// reported it. Before, the card published the Max-era five-hour row — a live
// 0 % that never rolled over, which also made every consumer read the account's
// capacity as UNKNOWN. After one gather the rows are exactly what the provider
// reports, and the plan chip says what the provider calls the plan.
func TestClaudeUltraReply_ProbeReplacesTheMaxEraRows(t *testing.T) {
	stubClaudeProbes(t, true, true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, claudeUltraReplyJSON())
	})
	seedClaudeUltraComputerCache(t, cache)
	writeClaudeDotJSON(t, os.Getenv("CLAUDE_CONFIG_DIR"), map[string]any{
		"oauthAccount": map[string]any{
			"accountUuid": claudeTestAccountA, "emailAddress": "ultra.user@example.com", "planDisplayName": "Ultra",
		},
	})

	// Precondition — the defect: three rows, the first a numeric 0 % session row.
	assertMetricSummary(t, claudeCodeMetricsFromCache(claudeUltraNow, ""),
		"5-hour session window|session||0", "Weekly quota|weekly||44", "Weekly Fable|weekly|fable|0")

	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, claudeUltraNow)
	if !ok || usage == nil {
		t.Fatal("the parser published nothing")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want the one routine probe", got)
	}
	assertMetricSummary(t, usage.Metrics, "Weekly quota|weekly||44", "Weekly Fable|weekly|fable|0")
	if usage.Plan != "Ultra" {
		t.Errorf("Plan=%q, want the provider's display name Ultra (the credential still says max)", usage.Plan)
	}
	if got, want := usage.Metrics[0].ResetAt, "2026-11-02T20:59:59Z"; got != want {
		t.Errorf("weekly ResetAt=%q, want %q", got, want)
	}
	requireCanonicalClaudeReceipt(t, *usage)

	// Hidden, not pruned: the bucket is still on disk for any reader that has no
	// set (an older binary), and the set records what the provider reported.
	snap := claudeCacheSnapshot(t, cache)
	if _, kept := snap.Buckets[claudeWindowFiveHour]; !kept {
		t.Error("the five_hour bucket was deleted; the set must hide it, never prune it")
	}
	if snap.ReportedWindowsAtMs != claudeUltraNow.UnixMilli() || len(snap.ReportedWindows) != 2 {
		t.Errorf("persisted set=%+v at %d, want seven_day + seven_day_fable at the probe instant",
			snap.ReportedWindows, snap.ReportedWindowsAtMs)
	}
	if _, ok := snap.ReportedWindows[claudeWindowSevenDayFable]; !ok {
		t.Errorf("persisted set %+v lacks the Fable window", snap.ReportedWindows)
	}
	if snap.ReportedAccount != claudeAccountRef(claudeTestAccountA) || strings.Contains(snap.ReportedAccount, claudeTestAccountA) {
		t.Errorf("ReportedAccount=%q, want the one-way digest of the account the probe ran under", snap.ReportedAccount)
	}
}

// The same computer with no probe at all — the probe opted out, or a process
// that never armed it (offline, credential expired, not probed since the
// upgrade): Claude Code's own cached copy of the reply in ~/.claude.json names
// the window set, at no request. Values still come only from the agent's own
// cache.
func TestClaudeUltraReply_ClaudeCodesCachedReplyNamesTheWindowsWithoutAProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		disarm func()
	}{
		{"probe opted out", func() { SetClaudeUsageProbeDisabled(true) }},
		{"probe never armed", resetClaudeUsageProbeGate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubClaudeProbes(t, true, true)
			cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, claudeUltraReplyJSON())
			})
			tc.disarm()
			seedClaudeUltraComputerCache(t, cache)
			writeClaudeDotJSON(t, os.Getenv("CLAUDE_CONFIG_DIR"), claudeUltraDotJSON(claudeTestAccountA))

			usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, claudeUltraNow)
			if !ok || usage == nil {
				t.Fatal("the parser published nothing")
			}
			if got := atomic.LoadInt64(calls); got != 0 {
				t.Fatalf("request count=%d, want none", got)
			}
			assertMetricSummary(t, usage.Metrics, "Weekly quota|weekly||44", "Weekly Fable|weekly|fable|0")
			if want := observedAtRFC3339(claudeUltraLastProbeAt.UnixMilli()); usage.Metrics[0].ObservedAt != want {
				t.Errorf("weekly ObservedAt=%q, want the agent's own reading %q — Claude Code's copy names windows, never values",
					usage.Metrics[0].ObservedAt, want)
			}
			if usage.Plan != "Ultra" || usage.Account != "ultra.user@example.com" {
				t.Errorf("Plan=%q Account=%q, want Ultra / the dotfile label", usage.Plan, usage.Account)
			}
			requireCanonicalClaudeReceipt(t, *usage)
			// Never persisted: the next probe's own set supersedes it.
			if snap := claudeCacheSnapshot(t, cache); snap.ReportedWindowsAtMs != 0 {
				t.Errorf("Claude Code's set was persisted (%d); it must be re-read each gather", snap.ReportedWindowsAtMs)
			}
		})
	}
}

// Claude Code's cached reply is ignored — silently, and without costing the
// rest of the config file — unless it is readable AND belongs to the account
// signed in now. Each case falls back to today's three rows.
func TestClaudeCodesCachedReply_IgnoredUnlessItDescribesTheSignedInAccount(t *testing.T) {
	cases := map[string]map[string]any{
		"another account's reply": claudeUltraDotJSON(claudeTestAccountB),
		"no signed-in account": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["oauthAccount"] = map[string]any{"emailAddress": "ultra.user@example.com", "planDisplayName": "Ultra"}
			return m
		}(),
		"fetchedAtMs changed type": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["cachedUsageUtilization"].(map[string]any)["fetchedAtMs"] = "soon"
			return m
		}(),
		"utilization changed type": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["cachedUsageUtilization"].(map[string]any)["utilization"] = "not a reply"
			return m
		}(),
		"reply percent changed type": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["cachedUsageUtilization"].(map[string]any)["utilization"] = map[string]any{
				"limits": []any{map[string]any{"kind": "weekly_all", "percent": "44"}},
			}
			return m
		}(),
		"the whole block changed type": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["cachedUsageUtilization"] = []any{1, 2, 3}
			return m
		}(),
		"fetched implausibly far in the future": func() map[string]any {
			m := claudeUltraDotJSON(claudeTestAccountA)
			m["cachedUsageUtilization"].(map[string]any)["fetchedAtMs"] = claudeUltraNow.Add(24 * time.Hour).UnixMilli()
			return m
		}(),
	}
	for name, dot := range cases {
		t.Run(name, func(t *testing.T) {
			stubClaudeProbes(t, true, true)
			cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {})
			resetClaudeUsageProbeGate()
			seedClaudeUltraComputerCache(t, cache)
			writeClaudeDotJSON(t, os.Getenv("CLAUDE_CONFIG_DIR"), dot)

			usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, claudeUltraNow)
			if !ok || usage == nil {
				t.Fatal("the parser published nothing")
			}
			assertMetricSummary(t, usage.Metrics,
				"5-hour session window|session||0", "Weekly quota|weekly||44", "Weekly Fable|weekly|fable|0")
			if usage.Account != "ultra.user@example.com" {
				t.Errorf("Account=%q — a bad cached reply must not cost the rest of the config file", usage.Account)
			}
		})
	}
}

// A Max account — Claude Code's /usage keeps its three meters, and the reply
// still carries both representations (Sept 2026): the rows are exactly the
// three the card always had, with their frozen identities.
func TestClaudeMaxReply_KeepsTheThreeRows(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour).Format(time.RFC3339)
	week := now.Add(96 * time.Hour).Format(time.RFC3339)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{
			"five_hour": {"utilization": 12, "resets_at": %q},
			"seven_day": {"utilization": 30, "resets_at": %q},
			"seven_day_opus": null,
			"limits": [
				{"kind": "session", "group": "session", "percent": 12, "resets_at": %q, "scope": null, "is_active": false},
				{"kind": "weekly_all", "group": "weekly", "percent": 30, "resets_at": %q, "scope": null, "is_active": true},
				{"kind": "weekly_scoped", "group": "weekly", "percent": 5, "resets_at": %q,
					"scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
			]
		}`, reset, week, reset, week, week)
	})
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now, false); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}
	rows := claudeCodeMetricsFromCache(now, "")
	assertMetricSummary(t, rows, "5-hour session window|session||12", "Weekly quota|weekly||30", "Weekly Fable|weekly|fable|5")
	if snap := claudeCacheSnapshot(t, cache); len(snap.ReportedWindows) != 3 {
		t.Errorf("persisted set=%+v, want the three windows the reply names", snap.ReportedWindows)
	}
	requireCanonicalClaudeReceipt(t, cliAgentUsage{Metrics: rows})
}

/* ─────────────────────────── generic windows ─────────────────────────── */

// frontendWindowScope mirrors the Coding capacity dialog's windowScopeOf
// (frontend codingCapacityWindows.js) closely enough to pin the label grammar
// the generic rows rely on: the row's model when it names one; otherwise every
// word after the first, unless they are all generic words, in which case the
// row is the all-models budget (""). A label that broke the grammar would move
// a row onto the wrong scope — or take the all-models headline away.
func frontendWindowScope(m cliAgentUsageMetric) string {
	if m.Model != "" {
		return m.Model
	}
	if i := strings.Index(m.Label, " — "); i > 0 {
		return strings.TrimSpace(m.Label[:i])
	}
	words := strings.Fields(m.Label)
	if len(words) < 2 {
		return ""
	}
	generic := map[string]bool{"quota": true, "credits": true, "usage": true, "limit": true, "window": true}
	for _, word := range words[1:] {
		if !generic[strings.ToLower(word)] {
			return strings.Join(words[1:], " ")
		}
	}
	return ""
}

// Windows this agent never modelled become rows of their own: the period from
// the group (or the kind's prefix), a label in the grammar the dialog reads,
// and a model only where the scope names one (a surface or an unknown scope is
// a shared constraint). In the server's order, after the modelled rows.
func TestClaudeGenericWindows_KindsScopesLabelsAndModels(t *testing.T) {
	body := `{"limits": [
		{"kind": "daily_all", "group": "daily", "percent": 10},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 20,
			"scope": {"model": {"id": null, "display_name": "Haiku 5"}, "surface": null}},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 30,
			"scope": {"model": null, "surface": {"display_name": "Claude Code"}}},
		{"kind": "weekly_burst", "group": "weekly", "percent": 40},
		{"kind": "something_new", "percent": 50},
		{"kind": "monthly_spend", "group": "monthly"},
		{"kind": "daily_scoped", "group": "daily", "percent": 60,
			"scope": {"model": {"id": "claude-sonnet-5-1", "display_name": "Sonnet 5.1"}}},
		{"kind": "daily_scoped", "group": "daily", "percent": 61,
			"scope": {"model": {"display_name": "Claude Opus 5"}}},
		{"kind": "hint", "group": "hint", "percent": 62},
		{"kind": "weekly", "group": "weekly", "percent": 63},
		{"kind": "weekly_scoped_v2", "group": "weekly", "percent": 64,
			"scope": {"model": {"display_name": "Fable"}}},
		{"kind": "five_hour_extra", "group": "five_hour", "percent": 65},
		{"kind": "weekly_all", "group": "weekly", "percent": 70},
		{"kind": "daily_all", "group": "daily", "percent": 99}
	]}`
	var decoded claudeUsageProbeResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	buckets, set := claudeUsageReplyWindows(decoded, now.UnixMilli())
	if set == nil {
		t.Fatal("a list of readable entries is authoritative")
	}
	wantIDs := []string{
		"limit:daily_all", "limit:weekly_scoped:model:haiku_5", "limit:weekly_scoped:surface:claude_code",
		"limit:weekly_burst", "limit:something_new", "limit:monthly_spend",
		"limit:daily_scoped:model:claude_sonnet_5_1", "limit:daily_scoped:model:claude_opus_5",
		"limit:hint", "limit:weekly", "limit:weekly_scoped_v2:model:fable", "limit:five_hour_extra",
		claudeWindowSevenDay,
	}
	gotIDs := make([]string, 0, len(set.Windows))
	for _, w := range set.Windows {
		gotIDs = append(gotIDs, w.ID)
	}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("set ids:\n  got  %v\n  want %v (server order; the duplicate daily_all keeps the first)", gotIDs, wantIDs)
	}
	if got := buckets["limit:daily_all"].UsedPercentage; got != 10 {
		t.Errorf("limit:daily_all=%v, want the FIRST entry's 10", got)
	}
	if _, present := buckets["limit:monthly_spend"]; present {
		t.Error("an entry without a percentage wrote a bucket; it must only join the set")
	}

	rows := claudeCodeMetricsFromWindowSet(buckets, set, now)
	assertMetricSummary(t, rows,
		"Weekly quota|weekly||70",
		"Daily quota|daily||10",
		"Weekly Haiku 5|weekly|haiku|20",
		"Weekly Claude Code|weekly||30",
		"Weekly burst|weekly||40",
		"Something new|something_new||50",
		"Monthly spend|monthly||?",
		"Daily Sonnet 5.1|daily|sonnet|60",
		"Daily Claude Opus 5|daily|opus|61",
		"Hint quota|provider_hint||62",
		"Weekly quota limit|weekly||63",
		"Weekly Fable limit|weekly|fable|64",
		"Five-hour extra|session||65",
	)
	for _, row := range rows {
		if row.Kind == "hint" {
			t.Errorf("row %q claims kind hint, which shared-constants reserves", row.Label)
		}
	}
	// The grammar the dialog reads (rule 4 / the frontend coupling).
	for label, wantScope := range map[string]string{
		"Weekly quota":       "",
		"Daily quota":        "",
		"Weekly quota limit": "",
		"Weekly Haiku 5":     "haiku",
		"Weekly Claude Code": "Claude Code",
		"Weekly Fable limit": "fable",
	} {
		for _, row := range rows {
			if row.Label == label {
				if got := frontendWindowScope(row); got != wantScope {
					t.Errorf("%q reads as scope %q in the capacity dialog, want %q", label, got, wantScope)
				}
			}
		}
	}
	requireCanonicalClaudeReceipt(t, cliAgentUsage{Metrics: rows})

	// The pool annotation can key a model-scoped window: Haiku's row names the
	// alias `claude --model` lists, so the haiku model gets the haiku pool.
	pooled := annotateModelPoolsFromUsage(claudeModelAliases, rows)
	for _, m := range pooled {
		if m.ID == "haiku" && m.Pool != "haiku" {
			t.Errorf("haiku pool=%q, want haiku from the Weekly Haiku 5 row", m.Pool)
		}
		if m.ID == "opus" && m.Pool != "opus" {
			t.Errorf("opus pool=%q, want opus from the Daily Claude Opus 5 row", m.Pool)
		}
		if m.ID == "sonnet" && m.Pool != "sonnet" {
			t.Errorf("sonnet pool=%q, want sonnet from the Daily Sonnet 5.1 row, whose scope names a full model id", m.Pool)
		}
	}
}

// A model-scoped window's pool key is the alias the device discovers whether
// or not the provider fills scope.model.id: every reply seen so far sends
// `id: null`, and a key that became the full id once it did would pool nothing
// (the device lists only the aliases) — a spent window no longer reaching the
// routes that run its model — and would split the row's identity between two
// spellings. An id that names no alias is still the key.
func TestClaudeWindowModelKey_IsTheAliasWithOrWithoutTheModelID(t *testing.T) {
	for _, tc := range []struct{ id, name, want string }{
		{"", "Sonnet 5.1", "sonnet"},
		{"claude-sonnet-5-1", "Sonnet 5.1", "sonnet"},
		{"claude-sonnet-5-1", "", "sonnet"},
		{"CLAUDE-FABLE-5", "Fable", "fable"},
		{"", "Claude Haiku 5", "haiku"},
		{"", "Opus+", "opus"},
		{"claude-nova-2", "Nova 2", "claude-nova-2"},
		{"", "Nova 2", "nova"},
		{"", "4o mini", "4o mini"},
		{"", "", ""},
	} {
		if got := claudeWindowModelKey(tc.id, tc.name); got != tc.want {
			t.Errorf("claudeWindowModelKey(%q, %q)=%q, want %q", tc.id, tc.name, got, tc.want)
		}
	}
	withID := claudeGenericWindow("daily_scoped", "daily", "claude-sonnet-5-1", "Sonnet 5.1", "")
	withoutID := claudeGenericWindow("daily_scoped", "daily", "", "Sonnet 5.1", "")
	if withID.Kind+"|"+withID.Label+"|"+withID.Model != withoutID.Kind+"|"+withoutID.Label+"|"+withoutID.Model {
		t.Errorf("the row's identity moved when the provider filled the id: %+v vs %+v", withID, withoutID)
	}
}

// A generic row's metric kind and the first word of its label name the same
// period: the group decides both (a group beginning with a period word is that
// period), the kind's own prefix speaks only when there is no group, and a
// group that names no period is its own kind — never a period the label does
// not show, which the card would age the row by.
func TestClaudeGenericWindows_KindAndLabelNameTheSamePeriod(t *testing.T) {
	for _, tc := range []struct{ kind, group, wantKind, wantLabel string }{
		{"daily_all", "", "daily", "Daily quota"},
		{"weekly_burst", "weekly", "weekly", "Weekly burst"},
		{"burst", "weekly_extra", "weekly", "Weekly-extra burst"},
		{"daily_rolling", "rolling", "rolling", "Rolling daily rolling"},
		{"rolling_all", "rolling", "rolling", "Rolling quota"},
	} {
		w := claudeGenericWindow(tc.kind, tc.group, "", "", "")
		if w.Kind != tc.wantKind || w.Label != tc.wantLabel {
			t.Errorf("claudeGenericWindow(%q, group %q) = kind %q label %q, want %q / %q",
				tc.kind, tc.group, w.Kind, w.Label, tc.wantKind, tc.wantLabel)
		}
	}
}

// Derived strings are bounded and printable however the vendor spells them:
// control and bidi runes dropped, whitespace collapsed, labels clamped to 64
// runes and 256 bytes, the pool key to 64 bytes, and two non-Latin names that
// would both slug to nothing still get distinct ids.
func TestClaudeGenericWindows_DerivedStringsAreBounded(t *testing.T) {
	long := strings.Repeat("Ω", 200)
	w := claudeGenericWindow("weekly_scoped", "weekly", "", "Haiku\n\u202e 5\t\x00", "")
	if w.Label != "Weekly Haiku 5" || w.Model != "haiku" {
		t.Errorf("window=%+v, want the label and model with control runes dropped", w)
	}
	long1 := claudeGenericWindow("daily_scoped", "daily", "", long, "")
	if n := len([]rune(long1.Label)); n > claudeWindowLabelMaxRunes || len(long1.Label) > claudeWindowLabelMaxBytes {
		t.Errorf("label of %d runes / %d bytes, want at most %d / %d", n, len(long1.Label),
			claudeWindowLabelMaxRunes, claudeWindowLabelMaxBytes)
	}
	if len(long1.Model) > cliUsageMaxModelPoolLength {
		t.Errorf("model key of %d bytes, want at most %d", len(long1.Model), cliUsageMaxModelPoolLength)
	}
	a := claudeGenericWindow("daily_scoped", "daily", "", "Модель", "")
	b := claudeGenericWindow("daily_scoped", "daily", "", "Другая", "")
	if a.ID == b.ID || !strings.HasPrefix(a.ID, "limit:daily_scoped:model:h") {
		t.Errorf("non-Latin names collapsed or lost their id: %q vs %q", a.ID, b.ID)
	}
}

// The rows a window set builds are capped at the receipt's 32 and unique by
// kind+label+model — a refresh that broke either rule would fail for every
// provider, not only Claude.
func TestClaudeGenericWindows_CappedAndDeduplicated(t *testing.T) {
	limits := []string{`{"kind":"session","percent":1}`, `{"kind":"weekly_all","percent":2}`}
	for i := 0; i < 40; i++ {
		limits = append(limits, fmt.Sprintf(`{"kind":"meter_%02d","group":"weekly","percent":%d}`, i, i))
	}
	var decoded claudeUsageProbeResponse
	if err := json.Unmarshal([]byte(`{"limits":[`+strings.Join(limits, ",")+`]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	buckets, set := claudeUsageReplyWindows(decoded, now.UnixMilli())
	rows := claudeCodeMetricsFromWindowSet(buckets, set, now)
	if len(rows) != cliUsageMaxMetricsPerProvider {
		t.Errorf("%d rows, want the receipt's cap of %d", len(rows), cliUsageMaxMetricsPerProvider)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		identity := row.Kind + "\x00" + row.Label + "\x00" + row.Model
		if seen[identity] {
			t.Errorf("duplicate row identity %q", row.Label)
		}
		seen[identity] = true
	}
	requireCanonicalClaudeReceipt(t, cliAgentUsage{Metrics: rows})
	if rows[0].Label != claudeSessionLabel || rows[1].Label != claudeWeeklyLabel {
		t.Errorf("the modelled rows lost their places to the cap: %v", metricSummary(rows[:2]))
	}

	// Two different kinds that derive the same row identity: the later is dropped.
	twins := []string{`{"kind":"weekly_twin","group":"weekly","percent":1}`, `{"kind":"twin","group":"weekly","percent":2}`}
	if err := json.Unmarshal([]byte(`{"limits":[`+strings.Join(twins, ",")+`]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	buckets, set = claudeUsageReplyWindows(decoded, now.UnixMilli())
	assertMetricSummary(t, claudeCodeMetricsFromWindowSet(buckets, set, now), "Weekly twin|weekly||1")
}

// A new window is diagnosable from agent.log: its id is logged once per
// process — never its value, reset or anything else from the reply.
func TestLogClaudeGenericWindowsOnce(t *testing.T) {
	claudeGenericWindowsLogged.mu.Lock()
	claudeGenericWindowsLogged.seen = nil
	claudeGenericWindowsLogged.mu.Unlock()
	t.Cleanup(func() {
		claudeGenericWindowsLogged.mu.Lock()
		claudeGenericWindowsLogged.seen = nil
		claudeGenericWindowsLogged.mu.Unlock()
	})
	var decoded claudeUsageProbeResponse
	_ = json.Unmarshal([]byte(`{"limits":[{"kind":"brand_new_meter","group":"daily","percent":37.5,"resets_at":1893456000}]}`), &decoded)
	first := captureStdout(t, func() { claudeUsageReplyWindows(decoded, 1) })
	second := captureStdout(t, func() { claudeUsageReplyWindows(decoded, 2) })
	if !strings.Contains(first, "limit:brand_new_meter") {
		t.Errorf("first sighting logged %q, want the window id", first)
	}
	if strings.Contains(first, "37.5") || strings.Contains(first, "1893456000") {
		t.Errorf("the log line carries a value: %q", first)
	}
	if second != "" {
		t.Errorf("second sighting logged %q, want nothing", second)
	}
}

/* ─────────────────────────── authority ─────────────────────────── */

// `limits: []` is the server saying the account has no meters: a SUCCESS that
// persists an empty set and advances the probe evidence — no rows, no backoff,
// no probe on every gather, and a receipt that still signs.
func TestClaudeUsageProbe_EmptyLimitsIsAnAuthoritativeAnswer(t *testing.T) {
	stubClaudeProbes(t, true, true)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"limits": [], "five_hour": null, "seven_day": null}`)
	})
	now := time.Now()
	// An earlier plan's reading, which the empty answer must hide.
	seedClaudeProbeReading(t, cache, now.Add(-time.Hour))

	usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, now)
	if !ok || usage == nil {
		t.Fatal("the parser published nothing")
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
	if len(usage.Metrics) != 0 {
		t.Errorf("rows=%v, want none for an account with no meters", metricSummary(usage.Metrics))
	}
	snap := claudeCacheSnapshot(t, cache)
	if snap.ReportedWindowsAtMs != now.UnixMilli() || snap.ReportedWindows != nil {
		t.Errorf("set=%+v at %d, want an empty set recorded at the probe instant", snap.ReportedWindows, snap.ReportedWindowsAtMs)
	}
	if snap.LastProbeObservedAtMs != now.UnixMilli() {
		t.Errorf("LastProbeObservedAtMs=%d, want the probe instant %d", snap.LastProbeObservedAtMs, now.UnixMilli())
	}
	claudeUsageProbe.mu.Lock()
	failures := claudeUsageProbe.failures
	claudeUsageProbe.mu.Unlock()
	if failures != 0 {
		t.Errorf("failure streak=%d, want 0: an empty list is an answer, not a broken endpoint", failures)
	}
	requireCanonicalClaudeReceipt(t, *usage)

	// No probe loop: the next gathers inside the staleness TTL ask nothing.
	for _, later := range []time.Duration{time.Minute, 5 * time.Minute} {
		_, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, now.Add(later))
		if !ok {
			t.Fatal("the parser published nothing")
		}
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d after two more gathers inside the TTL, want still 1", got)
	}
}

// Claude Code refreshing its own copy of the same "no meters" answer is no
// news about OUR values: the probe's answer is still the freshest word on
// them, so a gather inside the staleness TTL must not go back to the endpoint
// because the copy is newer. Read that way, a meter-less account was re-probed
// after every refresh Claude Code made — a loop bounded only by Claude Code's
// own refetch rate. The TTL itself still applies.
func TestClaudeUsageProbe_EmptyLimitsStaysFreshWhileClaudeCodeRefreshesItsCopy(t *testing.T) {
	stubClaudeProbes(t, true, true)
	_, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"limits": [], "five_hour": null, "seven_day": null}`)
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	now := time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)
	claudeCodeFetched := func(at time.Time) {
		t.Helper()
		writeClaudeDotJSON(t, configDir, map[string]any{
			"oauthAccount": map[string]any{"accountUuid": claudeTestAccountA},
			"cachedUsageUtilization": map[string]any{
				"fetchedAtMs": at.UnixMilli(), "accountUuid": claudeTestAccountA,
				"utilization": map[string]any{"limits": []any{}, "five_hour": nil},
			},
		})
	}
	gather := func(at time.Time) {
		t.Helper()
		usage, ok := claudeCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, at)
		if !ok || usage == nil {
			t.Fatal("the parser published nothing")
		}
		if len(usage.Metrics) != 0 {
			t.Errorf("rows=%v, want none for an account with no meters", metricSummary(usage.Metrics))
		}
	}

	claudeCodeFetched(now.Add(-time.Minute))
	gather(now)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("request count=%d, want the one routine probe", got)
	}
	for _, step := range []struct{ fetched, gathered time.Duration }{
		{2 * time.Minute, 3 * time.Minute},
		{5 * time.Minute, 6 * time.Minute},
		{8 * time.Minute, 9 * time.Minute},
	} {
		claudeCodeFetched(now.Add(step.fetched))
		gather(now.Add(step.gathered))
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("request count=%d after three gathers inside the TTL, each after Claude Code refreshed its copy; want still 1", got)
	}
	gather(now.Add(claudeUsageProbeStaleAfter + time.Minute))
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Errorf("request count=%d past the staleness TTL, want 2: the probe's answer ages like any reading", got)
	}
}

// An answer that carries a window set and no reading at all is still the
// probe's whole answer about the account, so it pays what a reading pays: the
// debt a finished run left standing — on disk, in the same write, and in
// memory — and the persisted credential wait for the token it just proved.
// Unpaid, a meter-less account would re-request after every run up to the
// debt's cap; a wait left on disk would be restored at the next start and
// refuse a token the endpoint just accepted.
func TestClaudeUsageProbe_EmptyLimitsPaysTheRunDebtAndProvesTheCredential(t *testing.T) {
	const emptyAnswer = `{"limits": []}`
	t.Run("a standing run debt", func(t *testing.T) {
		stubClaudeProbes(t, true, true)
		cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, emptyAnswer)
		})
		now := time.Now()
		runEnded := now.Add(-time.Minute)
		claudeUsageProbe.recordOwed(runEnded)
		if _, onDisk := claudeOweRunRefresh(runEnded); !onDisk {
			t.Fatal("precondition: the run's debt did not reach the cache")
		}
		if _, ok := (claudeCodeUsageParser{}).ParseContext(context.Background(), "", detectedCLIAgent{Detected: true}, now); !ok {
			t.Fatal("the parser published nothing")
		}
		if got := atomic.LoadInt64(calls); got != 1 {
			t.Fatalf("request count=%d, want the one probe the debt is owed", got)
		}
		if snap := claudeCacheSnapshot(t, cache); snap.RefreshOwedAtMs != 0 || snap.RefreshOwedAttempts != 0 {
			t.Errorf("durable debt=%d (attempts %d), want it paid by the write that carried the answer",
				snap.RefreshOwedAtMs, snap.RefreshOwedAttempts)
		}
		if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
			t.Errorf("in-memory debt=%v, want it settled by the answer", owed)
		}
	})

	stamp := claudeCredStamp{modNs: 100, size: 10}
	for _, tc := range []struct {
		name     string
		wait     claudeCredStamp
		wantKept bool
	}{
		{"a wait for the credential it proved", stamp, false},
		{"a wait for another credential", claudeCredStamp{modNs: 200, size: 10}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, emptyAnswer)
			})
			seed, _ := json.Marshal(claudeRateLimitSnapshot{
				Buckets:             map[string]claudeRateLimitBucket{},
				AuthWaitCredStampNs: tc.wait.modNs, AuthWaitCredSize: tc.wait.size,
			})
			if err := os.WriteFile(cache, seed, 0o600); err != nil {
				t.Fatal(err)
			}
			// A click: the one request a persisted wait does not refuse.
			id := claudeUsageProbeIdentity{token: probeTestToken, credStamp: stamp}
			r, refreshed, _ := probeClaudeUsageResult(context.Background(), time.Now(),
				func() claudeUsageProbeIdentity { return id }, time.Time{}, true)
			if !refreshed || r.code != claudeProbeOK {
				t.Fatalf("result=%+v refreshed=%v, want the empty list read as a successful answer", r, refreshed)
			}
			snap := claudeCacheSnapshot(t, cache)
			if kept := snap.AuthWaitCredStampNs == tc.wait.modNs && snap.AuthWaitCredSize == tc.wait.size; kept != tc.wantKept {
				t.Errorf("persisted wait=%d/%d, want kept=%v", snap.AuthWaitCredStampNs, snap.AuthWaitCredSize, tc.wantKept)
			}
		})
	}
}

// The same empty list BESIDE readable legacy windows is a server that has not
// populated the list yet, not an account with no meters: not authoritative, so
// today's three rows and no set.
func TestClaudeUsageProbe_EmptyLimitsBesideLegacyReadingsIsNotAuthoritative(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"limits": [], "five_hour": {"utilization": 20, "resets_at": %d}}`, now.Add(time.Hour).Unix())
	})
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now, false); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.ReportedWindowsAtMs != 0 {
		t.Errorf("a set was persisted (%+v) for a reply that is not authoritative", snap.ReportedWindows)
	}
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""),
		"5-hour session window|session||20", "Weekly quota|weekly||?", "Weekly Fable|weekly|fable|?")
}

// A partial read never removes a row: an entry we could not identify (a scope
// of the wrong shape, no kind at all) or a list longer than the schema allows
// makes the reply not authoritative, while its readable entries still land.
func TestClaudeUsageReplyWindows_PartialRepliesAreNotAuthoritative(t *testing.T) {
	over := make([]string, 0, claudeUsageProbeMaxLimits+1)
	for i := 0; i <= claudeUsageProbeMaxLimits; i++ {
		over = append(over, fmt.Sprintf(`{"kind":"meter_%d","percent":1}`, i))
	}
	for name, body := range map[string]string{
		"malformed scope": `{"limits":[{"kind":"weekly_all","percent":44},
			{"kind":"weekly_scoped","percent":5,"scope":["not","an","object"]}]}`,
		"malformed model part": `{"limits":[{"kind":"weekly_all","percent":44},
			{"kind":"weekly_scoped","percent":5,"scope":{"model":17}}]}`,
		"entry without a kind":          `{"limits":[{"kind":"weekly_all","percent":44},{"percent":5}]}`,
		"longer than the schema allows": `{"limits":[{"kind":"weekly_all","percent":44},` + strings.Join(over, ",") + `]}`,
		"limits null":                   `{"limits":null,"seven_day":{"utilization":44}}`,
		"limits absent":                 `{"seven_day":{"utilization":44}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var decoded claudeUsageProbeResponse
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("a partial reply must still decode: %v", err)
			}
			buckets, set := claudeUsageReplyWindows(decoded, 1)
			if set != nil {
				t.Errorf("set=%+v, want none for a partial reply", set)
			}
			if got := buckets[claudeWindowSevenDay].UsedPercentage; got != 44 {
				t.Errorf("seven_day=%v, want the readable entry still read", got)
			}
		})
	}
	// A malformed scope on a kind whose window the scope never decided costs
	// nothing: session is session whatever its scope looks like.
	var decoded claudeUsageProbeResponse
	_ = json.Unmarshal([]byte(`{"limits":[{"kind":"session","percent":9,"scope":[1]},{"kind":"weekly_all","percent":44}]}`), &decoded)
	if buckets, set := claudeUsageReplyWindows(decoded, 1); set == nil || buckets[claudeWindowFiveHour].UsedPercentage != 9 {
		t.Errorf("session with an odd scope: buckets=%+v set=%+v, want it read and the reply authoritative", buckets, set)
	}
}

// Absence is believed only when BOTH representations omit a window: a reply
// whose limits[] has no session entry while its legacy five_hour still carries
// a reading — the shape an unverified Ultra account's temporary five-hour limit
// would take if the server reported it only there — keeps the session row,
// because that legacy window joins the set. Left out, the window's fresh
// reading would land at the set's own instant, outside the set, and be hidden
// while a live limit exists: the over-admit direction. A legacy window sent as
// null, or without a readable value, names nothing and stays out.
func TestClaudeUsageReplyWindows_ALegacyReadingJoinsTheSet(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour).Unix()
	for _, tc := range []struct {
		name, fiveHour string
		named          bool
		want           []string
	}{
		{"a readable legacy five_hour", fmt.Sprintf(`{"utilization": 20, "resets_at": %d}`, reset), true,
			[]string{"5-hour session window|session||20", "Weekly quota|weekly||44"}},
		{"a null legacy five_hour", `null`, false, []string{"Weekly quota|weekly||44"}},
		{"a legacy five_hour without a value", fmt.Sprintf(`{"resets_at": %d}`, reset), false,
			[]string{"Weekly quota|weekly||44"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoded claudeUsageProbeResponse
			body := fmt.Sprintf(`{"limits":[{"kind":"weekly_all","group":"weekly","percent":44}],"five_hour":%s}`, tc.fiveHour)
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatal(err)
			}
			buckets, set := claudeUsageReplyWindows(decoded, now.UnixMilli())
			if set == nil {
				t.Fatal("a non-empty limits[] of readable entries is authoritative")
			}
			named := false
			for _, w := range set.Windows {
				named = named || w.ID == claudeWindowFiveHour
			}
			if named != tc.named {
				t.Errorf("five_hour in the set=%v, want %v (set %+v)", named, tc.named, set.Windows)
			}
			assertMetricSummary(t, claudeCodeMetricsFromWindowSet(buckets, set, now), tc.want...)
		})
	}
}

/* ─────────────────────────── evidence and order ─────────────────────────── */

// Newer evidence wins: a window the last authoritative reply omitted shows
// again as soon as something observes it AFTER that reply — an unverified
// account given a temporary session limit, say — without waiting for the next
// probe. A reading older than the reply stays hidden.
func TestClaudeWindowSet_NewerStreamEvidenceShowsAHiddenWindowAgain(t *testing.T) {
	setAt := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	week := setAt.Add(48 * time.Hour)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"weekly_all","group":"weekly","percent":44,"resets_at":%d}]}`, week.Unix())
	})
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 0, ObservedAtMs: setAt.Add(-5 * time.Hour).UnixMilli(), usageKnown: true},
	}, setAt.Add(-5*time.Hour), "")
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), setAt, false); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}
	assertMetricSummary(t, claudeCodeMetricsFromCache(setAt, ""), "Weekly quota|weekly||44")

	streamAt := setAt.Add(20 * time.Minute)
	line := fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"five_hour","resets_at":%d,"utilization":0.37}}`,
		streamAt.Add(4*time.Hour).Unix())
	captureClaudeRateLimitLine(line, streamAt)
	assertMetricSummary(t, claudeCodeMetricsFromCache(streamAt, ""),
		"5-hour session window|session||37", "Weekly quota|weekly||44")
}

// "Weekly quota" aggregates over the windows the set SHOWS, not over every
// bucket the cache still holds: a per-model weekly window the provider no
// longer reports — a seven_day_opus reading left at 90 % without a reset,
// which never rolls over — must not drive the all-models row (and with it the
// whole account) to "limit reached". A per-model window the set does name
// still counts, worst case, as it always has.
func TestClaudeWindowSet_WeeklyQuotaAggregatesOnlyTheShownWindows(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	setAt := now.Add(-time.Minute)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {UsedPercentage: 30, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
			ObservedAtMs: setAt.UnixMilli(), UsageObserved: usageObservedPtr(true)},
		claudeWindowSevenDayOpus: {UsedPercentage: 90, ObservedAtMs: now.Add(-48 * time.Hour).UnixMilli(),
			UsageObserved: usageObservedPtr(true)},
	}
	// Precondition: with no set, the aggregate takes the stale window's value.
	assertMetricSummary(t, claudeCodeMetricsFromBuckets(buckets, now),
		"5-hour session window|session||?", "Weekly quota|weekly||90", "Weekly Fable|weekly|fable|?")

	weeklyOnly := &claudeWindowSet{AtMs: setAt.UnixMilli(), Windows: []claudeReportedWindow{{ID: claudeWindowSevenDay}}}
	assertMetricSummary(t, claudeCodeMetricsFromWindowSet(buckets, weeklyOnly, now), "Weekly quota|weekly||30")

	withOpus := &claudeWindowSet{AtMs: setAt.UnixMilli(), Windows: []claudeReportedWindow{
		{ID: claudeWindowSevenDay}, {ID: claudeWindowSevenDayOpus, Order: 1},
	}}
	assertMetricSummary(t, claudeCodeMetricsFromWindowSet(buckets, withOpus, now), "Weekly quota|weekly||90")
}

// Two agent channels on one machine answer the same account-scoped question,
// so the merged view takes the NEWER set, wherever it is stored; the import
// carries a newer pinned set into the own cache without moving the own
// generation — the generation versions readings, and none was imported.
func TestClaudeWindowSet_DualChannelNewestSetWins(t *testing.T) {
	ownCache := filepath.Join(t.TempDir(), "own", "rl.json")
	pinnedCache := filepath.Join(t.TempDir(), "pinned", "rl.json")
	configDir := t.TempDir()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", ownCache)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinnedCache) +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	writeSnap := func(path string, snap claudeRateLimitSnapshot) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(snap)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bucket := func(used float64, at time.Time) claudeRateLimitBucket {
		return claudeRateLimitBucket{UsedPercentage: used, ResetsAtMs: t0.Add(72 * time.Hour).UnixMilli(),
			ObservedAtMs: at.UnixMilli(), UsageObserved: usageObservedPtr(true), Source: claudeRateLimitSourceProbe}
	}
	own := claudeRateLimitSnapshot{
		Buckets:             map[string]claudeRateLimitBucket{claudeWindowSevenDay: bucket(40, t1)},
		ReportedWindows:     map[string]claudeReportedWindow{claudeWindowSevenDay: {}},
		ReportedWindowsAtMs: t1.UnixMilli(), LastProbeObservedAtMs: t1.UnixMilli(),
		GenerationEpoch: 77, Generation: 5,
	}

	t.Run("the pinned set is newer", func(t *testing.T) {
		writeSnap(ownCache, own)
		writeSnap(pinnedCache, claudeRateLimitSnapshot{
			Buckets: map[string]claudeRateLimitBucket{claudeWindowSevenDay: bucket(39, t0)},
			ReportedWindows: map[string]claudeReportedWindow{
				claudeWindowSevenDay: {Order: 0}, claudeWindowSevenDayFable: {Order: 1},
			},
			ReportedWindowsAtMs: t2.UnixMilli(), LastProbeObservedAtMs: t2.UnixMilli(),
		})
		assertMetricSummary(t, claudeCodeMetricsFromCache(t2, ""), "Weekly quota|weekly||40", "Weekly Fable|weekly|fable|?")

		if g := importPinnedClaudeObservations(""); g != nil {
			t.Errorf("the import committed generation %+v; a set alone must not move it", g)
		}
		got := claudeCacheSnapshot(t, ownCache)
		if got.ReportedWindowsAtMs != t2.UnixMilli() || len(got.ReportedWindows) != 2 {
			t.Errorf("own set=%+v at %d, want the pinned set carried across", got.ReportedWindows, got.ReportedWindowsAtMs)
		}
		if got.GenerationEpoch != 77 || got.Generation != 5 || got.LastProbeObservedAtMs != t1.UnixMilli() {
			t.Errorf("generation %d/%d, probe evidence %d: a carried set must change neither",
				got.GenerationEpoch, got.Generation, got.LastProbeObservedAtMs)
		}
		// With the pinned file gone, the own cache alone still hides what the
		// newer answer omitted.
		_ = os.Remove(pinnedCache)
		assertMetricSummary(t, claudeCodeMetricsFromCache(t2, ""), "Weekly quota|weekly||40", "Weekly Fable|weekly|fable|?")
	})

	t.Run("the own set is newer", func(t *testing.T) {
		writeSnap(ownCache, own)
		writeSnap(pinnedCache, claudeRateLimitSnapshot{
			Buckets: map[string]claudeRateLimitBucket{claudeWindowFiveHour: bucket(80, t0)},
			ReportedWindows: map[string]claudeReportedWindow{
				claudeWindowFiveHour: {Order: 0}, claudeWindowSevenDay: {Order: 1},
			},
			ReportedWindowsAtMs: t0.UnixMilli(),
		})
		assertMetricSummary(t, claudeCodeMetricsFromCache(t2, ""), "Weekly quota|weekly||40")
		importPinnedClaudeObservations("")
		if got := claudeCacheSnapshot(t, ownCache); got.ReportedWindowsAtMs != t1.UnixMilli() {
			t.Errorf("own set moved to %d; an older pinned set must never replace it", got.ReportedWindowsAtMs)
		}
	})
}

// A claude.ai credential carries no account, so the cache cannot see a /login
// to another account; the set is bound to the account ~/.claude.json named
// when it was read, and ignored — today's rows, the fail-safe direction — once
// another account is signed in.
func TestClaudeWindowSet_BoundToTheAccountItWasReadUnder(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	week := now.Add(48 * time.Hour)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"limits":[{"kind":"weekly_all","group":"weekly","percent":44,"resets_at":%d}]}`, week.Unix())
	})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	writeClaudeDotJSON(t, configDir, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeTestAccountA}})
	if refreshed, probeErr := runClaudeUsageProbe(context.Background(), now, false); !refreshed || probeErr != nil {
		t.Fatalf("refreshed=%v err=%+v", refreshed, probeErr)
	}
	if got := claudeCacheSnapshot(t, cache).ReportedAccount; got != claudeAccountRef(claudeTestAccountA) {
		t.Fatalf("ReportedAccount=%q, want the digest of the account the probe ran under", got)
	}
	setRows := []string{"Weekly quota|weekly||44"}
	today := []string{"5-hour session window|session||?", "Weekly quota|weekly||44", "Weekly Fable|weekly|fable|?"}

	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""), setRows...)
	writeClaudeDotJSON(t, configDir, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeTestAccountB}})
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""), today...)
	// Until something describes the account signed in now: Claude Code's own
	// cached reply for B names B's windows, though it is OLDER than A's set.
	// Labels only — the values keep their own provenance (the cache's readings,
	// whichever set names the windows).
	writeClaudeDotJSON(t, configDir, map[string]any{
		"oauthAccount": map[string]any{"accountUuid": claudeTestAccountB},
		"cachedUsageUtilization": map[string]any{
			"fetchedAtMs": now.Add(-time.Hour).UnixMilli(), "accountUuid": claudeTestAccountB,
			"utilization": map[string]any{"limits": []any{
				map[string]any{"kind": "weekly_all", "group": "weekly", "percent": 12},
				map[string]any{"kind": "weekly_scoped", "group": "weekly", "percent": 3,
					"scope": map[string]any{"model": map[string]any{"id": nil, "display_name": "Fable"}, "surface": nil}},
			}},
		},
	})
	labels := []string{}
	for _, m := range claudeCodeMetricsFromCache(now, "") {
		labels = append(labels, m.Label)
	}
	if got, want := strings.Join(labels, ","), claudeWeeklyLabel+","+claudeFableLabel; got != want {
		t.Errorf("rows after /login with B's own cached reply: %s, want %s", got, want)
	}
	// Unknown on either side is not a mismatch.
	writeClaudeDotJSON(t, configDir, map[string]any{"oauthAccount": map[string]any{"emailAddress": "someone@example.com"}})
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""), setRows...)
	writeClaudeDotJSON(t, configDir, map[string]any{"oauthAccount": map[string]any{"accountUuid": claudeTestAccountA}})
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""), setRows...)
}

// The two sources of the window set — the probe's persisted set and Claude
// Code's own cached reply — are each a full statement of the account's
// windows at their instant, so the NEWER one decides which rows exist, within
// the account binding and the future-skew guard. Both directions matter: an
// older probe set left in charge keeps a window the plan has dropped (the
// phantom session row this design removes), and an older Claude Code copy left
// in charge hides one the latest probe reported.
func TestClaudeEffectiveWindowSet_TheNewerSourceWins(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// Readings older than every set, so a window shows only when the set in
	// force names it (never through the newer-evidence rule).
	before := now.Add(-3 * time.Hour)
	buckets := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 15, ResetsAtMs: now.Add(2 * time.Hour).UnixMilli(),
			ObservedAtMs: before.UnixMilli(), UsageObserved: usageObservedPtr(true)},
		claudeWindowSevenDay: {UsedPercentage: 40, ResetsAtMs: now.Add(72 * time.Hour).UnixMilli(),
			ObservedAtMs: before.UnixMilli(), UsageObserved: usageObservedPtr(true)},
	}
	withSession := []claudeReportedWindow{{ID: claudeWindowFiveHour}, {ID: claudeWindowSevenDay, Order: 1}}
	weeklyOnly := []claudeReportedWindow{{ID: claudeWindowSevenDay}}
	set := func(at time.Time, account string, windows []claudeReportedWindow) *claudeWindowSet {
		return &claudeWindowSet{AtMs: at.UnixMilli(), Account: claudeAccountRef(account), Windows: windows}
	}
	sessionRows := []string{"5-hour session window|session||15", "Weekly quota|weekly||40"}
	weeklyRows := []string{"Weekly quota|weekly||40"}
	today := []string{"5-hour session window|session||15", "Weekly quota|weekly||40", "Weekly Fable|weekly|fable|?"}

	for _, tc := range []struct {
		name              string
		probe, claudeCode *claudeWindowSet
		signedIn          string
		want              []string
	}{
		{"Claude Code's newer set drops a window the probe's older set named",
			set(now.Add(-time.Hour), claudeTestAccountA, withSession), set(now.Add(-30*time.Minute), claudeTestAccountA, weeklyOnly),
			claudeTestAccountA, weeklyRows},
		{"the probe's newer set names a window Claude Code's older set lacked",
			set(now.Add(-time.Hour), claudeTestAccountA, withSession), set(now.Add(-2*time.Hour), claudeTestAccountA, weeklyOnly),
			claudeTestAccountA, sessionRows},
		{"after a /login to another account, that account's own set wins though it is older",
			set(now.Add(-time.Hour), claudeTestAccountA, withSession), set(now.Add(-2*time.Hour), claudeTestAccountB, weeklyOnly),
			claudeTestAccountB, weeklyRows},
		{"a set Claude Code claims to have fetched past the skew guard is ignored",
			set(now.Add(-time.Hour), claudeTestAccountA, withSession), set(now.Add(claudeWindowSetSkew+time.Minute), claudeTestAccountA, weeklyOnly),
			claudeTestAccountA, sessionRows},
		{"only a set read under another account: today's rows",
			set(now.Add(-time.Hour), claudeTestAccountA, weeklyOnly), nil,
			claudeTestAccountB, today},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := claudeRateLimitView{buckets: buckets, reported: tc.probe,
				dot: claudeDotJSONFacts{accountUUID: tc.signedIn, usageWindows: tc.claudeCode}}
			assertMetricSummary(t, claudeCodeMetricsFromView(view, now), tc.want...)
		})
	}
}

// Only a NEWER set replaces the persisted one (two channels' probes landing out
// of order cannot walk it backwards), and an account-fingerprint transition
// drops it with the rest of the previous account's state.
func TestClaudeWindowSet_OnlyANewerSetReplacesThePersistedOne(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t1 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	reading := func(at time.Time) map[string]claudeRateLimitBucket {
		return map[string]claudeRateLimitBucket{claudeWindowSevenDay: {UsedPercentage: 10, ObservedAtMs: at.UnixMilli(), usageKnown: true}}
	}
	newer := &claudeWindowSet{AtMs: t1.Add(time.Minute).UnixMilli(), Windows: []claudeReportedWindow{{ID: claudeWindowSevenDay}}}
	older := &claudeWindowSet{AtMs: t1.UnixMilli(), Windows: []claudeReportedWindow{{ID: claudeWindowFiveHour}, {ID: claudeWindowSevenDay, Order: 1}}}
	if _, err := mergeClaudeRateLimitCacheProbeReading(context.Background(), cache, reading(t1.Add(time.Minute)), newer, t1.Add(time.Minute), "fp-A", nil, claudeCredStamp{}); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeClaudeRateLimitCacheProbeReading(context.Background(), cache, reading(t1), older, t1, "fp-A", nil, claudeCredStamp{}); err != nil {
		t.Fatal(err)
	}
	if snap := claudeCacheSnapshot(t, cache); snap.ReportedWindowsAtMs != newer.AtMs || len(snap.ReportedWindows) != 1 {
		t.Errorf("set=%+v at %d, want the newer set kept", snap.ReportedWindows, snap.ReportedWindowsAtMs)
	}
	// A writer for ANOTHER account resets the previous account's state, the set
	// included — the bucket it writes is the new account's first reading.
	mergeClaudeRateLimitCacheFromSource(cache, reading(t1.Add(2*time.Minute)), t1.Add(2*time.Minute), "fp-B", claudeRateLimitSourceStream)
	if snap := claudeCacheSnapshot(t, cache); snap.ReportedWindowsAtMs != 0 || snap.ReportedWindows != nil || snap.ReportedAccount != "" {
		t.Errorf("set=%+v at %d survived an account transition", snap.ReportedWindows, snap.ReportedWindowsAtMs)
	}
}

// The same cache and set always produce byte-identical rows, in the server's
// order: terminal-service delta-skips by hashing the payload (it sorts object
// keys, NOT arrays), so rows built in map order would churn a Firestore write
// and a history document on every refresh.
func TestClaudeWindowRows_AreDeterministic(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	limits := []string{`{"kind":"session","percent":1}`, `{"kind":"weekly_all","percent":2}`}
	for i := 0; i < 20; i++ {
		limits = append(limits, fmt.Sprintf(`{"kind":"z_meter_%02d","group":"daily","percent":%d}`, 19-i, i))
	}
	var decoded claudeUsageProbeResponse
	if err := json.Unmarshal([]byte(`{"limits":[`+strings.Join(limits, ",")+`]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	buckets, set := claudeUsageReplyWindows(decoded, now.UnixMilli())
	// Through the persisted (map-keyed) form, as a later read sees it.
	reloaded := claudeSnapshotWindowSet(claudeRateLimitSnapshot{
		ReportedWindows: set.persisted(), ReportedWindowsAtMs: set.AtMs,
	})
	first, _ := json.Marshal(claudeCodeMetricsFromWindowSet(buckets, reloaded, now))
	for i := 0; i < 50; i++ {
		again, _ := json.Marshal(claudeCodeMetricsFromWindowSet(buckets, claudeSnapshotWindowSet(claudeRateLimitSnapshot{
			ReportedWindows: set.persisted(), ReportedWindowsAtMs: set.AtMs,
		}), now))
		if string(again) != string(first) {
			t.Fatalf("rows differ between two builds of the same cache:\n%s\n%s", first, again)
		}
	}
	rows := claudeCodeMetricsFromWindowSet(buckets, reloaded, now)
	if rows[2].Label != "Daily z meter 19" || rows[len(rows)-1].Label != "Daily z meter 00" {
		t.Errorf("generic rows are not in the server's order: first %q, last %q", rows[2].Label, rows[len(rows)-1].Label)
	}
}

// A cache written before window sets existed — or re-marshalled by an older
// binary, which drops them — reads exactly as it always did: three rows, and
// the round trip adds nothing. A generic bucket such a binary left behind is
// not mistaken for one of the modelled windows either.
func TestClaudeWindowSet_OlderCacheWithoutTheNewFields(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 12, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), usageKnown: true},
	}, now, "")
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"reportedWindows", "reportedWindowsAtMs", "reportedAccount"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("a cache with no set carries %q:\n%s", field, raw)
		}
	}
	var snap claudeRateLimitSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	again, _ := json.MarshalIndent(snap, "", "  ")
	if string(again) != string(raw) {
		t.Errorf("round trip changed the cache:\n got %s\nwant %s", again, raw)
	}
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""),
		"5-hour session window|session||12", "Weekly quota|weekly||?", "Weekly Fable|weekly|fable|?")

	// An older binary dropped the set but kept the generic buckets.
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		"limit:daily_scoped:model:fable": {UsedPercentage: 90, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: now.UnixMilli(), usageKnown: true},
	}, now, "")
	assertMetricSummary(t, claudeCodeMetricsFromCache(now, ""),
		"5-hour session window|session||12", "Weekly quota|weekly||?", "Weekly Fable|weekly|fable|?")
}

// Freshness follows the rows that are SHOWN: a hidden window's stale reading
// can neither make the snapshot read stale nor pass for fresh, and an account
// whose probe-reported windows carry no readable value is as fresh as that
// probe — never "nothing observed", which would probe on every gather.
func TestStalestClaudeRowObservation_FollowsTheWindowSet(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	probedAt := now.Add(-2 * time.Minute)
	observed := func(at time.Time) claudeRateLimitBucket {
		return claudeRateLimitBucket{UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(), ObservedAtMs: at.UnixMilli(), UsageObserved: usageObservedPtr(true)}
	}

	// A status-line five_hour reading an hour old, newer than the last probe, on
	// an account whose window set (here Claude Code's own, 30 minutes old) has
	// no five-hour window: shown, it would make the snapshot an hour stale; hidden,
	// the weekly row is what the card shows and what freshness follows.
	staleProbe := now.Add(-2 * time.Hour)
	shownOnly := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: observed(now.Add(-time.Hour)),
		claudeWindowSevenDay: observed(now.Add(-10 * time.Minute)),
	}
	withoutSet := claudeRateLimitView{buckets: shownOnly, probedAtMs: staleProbe.UnixMilli()}
	if got := stalestClaudeRowObservation(withoutSet, now); !got.Equal(now.Add(-time.Hour)) {
		t.Fatalf("precondition: freshness=%v, want the session row's %v", got, now.Add(-time.Hour))
	}
	withSet := withoutSet
	withSet.dot = claudeDotJSONFacts{usageWindows: &claudeWindowSet{AtMs: now.Add(-30 * time.Minute).UnixMilli(),
		Windows: []claudeReportedWindow{{ID: claudeWindowSevenDay}}}}
	if got := stalestClaudeRowObservation(withSet, now); !got.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("freshness=%v, want the shown weekly row's %v", got, now.Add(-10*time.Minute))
	}

	buckets := map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: observed(now.Add(-9 * time.Hour)),
		claudeWindowSevenDay: observed(probedAt),
	}
	empty := claudeRateLimitView{buckets: buckets, probedAtMs: probedAt.UnixMilli(),
		reported: &claudeWindowSet{AtMs: probedAt.UnixMilli()}}
	if got := stalestClaudeRowObservation(empty, now); !got.Equal(probedAt) {
		t.Errorf("no meters: freshness=%v, want the probe that said so (%v)", got, probedAt)
	}
	unreadable := claudeRateLimitView{buckets: map[string]claudeRateLimitBucket{}, probedAtMs: probedAt.UnixMilli(),
		reported: &claudeWindowSet{AtMs: probedAt.UnixMilli(), Windows: []claudeReportedWindow{{ID: claudeWindowSevenDay}}}}
	if got := stalestClaudeRowObservation(unreadable, now); !got.Equal(probedAt) {
		t.Errorf("no readable value: freshness=%v, want the probe's %v", got, probedAt)
	}
	// A set read from Claude Code's own cache, with no probe since, says nothing
	// about how fresh OUR values are: probe if you can.
	ccOnly := claudeRateLimitView{buckets: map[string]claudeRateLimitBucket{}, probedAtMs: probedAt.UnixMilli(),
		dot: claudeDotJSONFacts{usageWindows: &claudeWindowSet{AtMs: now.Add(-time.Minute).UnixMilli(),
			Windows: []claudeReportedWindow{{ID: claudeWindowSevenDay}}}}}
	if got := stalestClaudeRowObservation(ccOnly, now); !got.IsZero() {
		t.Errorf("Claude Code's set alone: freshness=%v, want zero (probe if you can)", got)
	}
	// Claude Code's NEWER copy of the probe's own "no meters" answer changes
	// nothing about how fresh our values are: the rows follow the copy, the
	// freshness still follows the probe's own set.
	newerCopy := empty
	newerCopy.dot = claudeDotJSONFacts{usageWindows: &claudeWindowSet{AtMs: now.Add(-time.Minute).UnixMilli()}}
	if got := stalestClaudeRowObservation(newerCopy, now); !got.Equal(probedAt) {
		t.Errorf("a newer copy of the empty answer: freshness=%v, want the probe's %v", got, probedAt)
	}
	// A probe set read under another account says nothing about the account
	// signed in now, so it vouches for nothing: probe that account.
	otherAccount := claudeRateLimitView{buckets: map[string]claudeRateLimitBucket{}, probedAtMs: probedAt.UnixMilli(),
		reported: &claudeWindowSet{AtMs: probedAt.UnixMilli(), Account: claudeAccountRef(claudeTestAccountA)},
		dot:      claudeDotJSONFacts{accountUUID: claudeTestAccountB}}
	if got := stalestClaudeRowObservation(otherAccount, now); !got.IsZero() {
		t.Errorf("a set read under another account: freshness=%v, want zero (probe if you can)", got)
	}
}

/* ─────────────────────────── the plan chip ─────────────────────────── */

func TestClaudePlanName_ProviderDisplayNameWithFallback(t *testing.T) {
	for _, tc := range []struct{ display, subscription, want string }{
		{"Ultra", "max", "Ultra"},
		{"Max", "max", "Max"},
		{"Team Premium", "team", "Team Premium"},
		{"Pro+", "pro", "Pro+"},
		{"", "max", "max"},
		{"", "", ""},
		{"Ultra!", "max", "max"},
		{" Ultra", "max", "max"},
		{"Ultra ", "max", "max"},
		{"Ul\ntra", "max", "max"},
		{strings.Repeat("U", 41), "max", "max"},
		{strings.Repeat("U", 40), "max", strings.Repeat("U", 40)},
	} {
		if got := claudePlanName(tc.display, tc.subscription); got != tc.want {
			t.Errorf("claudePlanName(%q, %q)=%q, want %q", tc.display, tc.subscription, got, tc.want)
		}
	}
}

// The plan chip through the real parser: the provider's display name from the
// config dir's ~/.claude.json wins over the credential's code, every way that
// file can fail falls back to the code exactly as before, and a credential
// with no subscription login reports no plan at all.
func TestClaudeCodeUsageParser_PlanFromProviderDisplayName(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	subscription := map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "sk-ant-oat-access", "refreshToken": "sk-ant-ort-refresh",
		"expiresAt": now.Add(8 * time.Hour).UnixMilli(), "subscriptionType": "max",
	}}
	parse := func(t *testing.T, home string) *cliAgentUsage {
		t.Helper()
		usage, ok := claudeCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
		if !ok || usage == nil {
			t.Fatal("the parser published nothing")
		}
		return usage
	}
	setup := func(t *testing.T, credential map[string]any, dot any) string {
		t.Helper()
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		seedClaudeRateLimitCache(t, nil, time.Time{}, "")
		stubClaudeProbes(t, true, true)
		home := t.TempDir()
		helperWriteJSON(t, filepath.Join(home, ".claude", ".credentials.json"), credential)
		switch body := dot.(type) {
		case nil:
		case string:
			if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			resetClaudeDotJSONCache()
		default:
			writeClaudeDotJSON(t, home, body)
		}
		return home
	}

	for _, tc := range []struct {
		name string
		dot  any
		want string
	}{
		{"display name wins", map[string]any{"oauthAccount": map[string]any{"planDisplayName": "Ultra"}}, "Ultra"},
		{"invalid display name falls back", map[string]any{"oauthAccount": map[string]any{"planDisplayName": "Ultra!!"}}, "max"},
		{"non-string display name falls back", map[string]any{"oauthAccount": map[string]any{"planDisplayName": map[string]any{"x": 1}}}, "max"},
		{"missing dotfile falls back", nil, "max"},
		{"unreadable dotfile falls back", "{not json", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parse(t, setup(t, subscription, tc.dot)).Plan; got != tc.want {
				t.Errorf("Plan=%q, want %q", got, tc.want)
			}
		})
	}

	t.Run("a type change costs only the plan", func(t *testing.T) {
		home := setup(t, subscription, map[string]any{"oauthAccount": map[string]any{
			"emailAddress": "ada@example.com", "planDisplayName": 7, "accountUuid": []any{},
		}})
		if usage := parse(t, home); usage.Account != "ada@example.com" || usage.Plan != "max" {
			t.Errorf("Account=%q Plan=%q, want the label kept and the plan falling back", usage.Account, usage.Plan)
		}
	})

	t.Run("no subscription login reports no plan", func(t *testing.T) {
		home := setup(t, map[string]any{"email": "ada@example.com"},
			map[string]any{"oauthAccount": map[string]any{"planDisplayName": "Ultra"}})
		if got := parse(t, home).Plan; got != "" {
			t.Errorf("Plan=%q, want empty for an API-key install whatever the dotfile says", got)
		}
	})

	t.Run("CLAUDE_CONFIG_DIR's dotfile is the one read", func(t *testing.T) {
		home := setup(t, subscription, map[string]any{"oauthAccount": map[string]any{"planDisplayName": "Pro"}})
		profile := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", profile)
		helperWriteJSON(t, filepath.Join(profile, ".credentials.json"), subscription)
		writeClaudeDotJSON(t, profile, map[string]any{"oauthAccount": map[string]any{"planDisplayName": "Ultra"}})
		if got := parse(t, home).Plan; got != "Ultra" {
			t.Errorf("Plan=%q, want Ultra from $CLAUDE_CONFIG_DIR/.claude.json, not the home dotfile's Pro", got)
		}
	})
}

// Claude Code rewrites ~/.claude.json constantly. A read that lands mid-write
// is a fact about the read, not the account, so the last good parse of the
// same file stands rather than flipping the plan chip (and the window set) for
// one gather; a file that is really gone reads as gone.
func TestClaudeDotJSONFacts_ATornReadKeepsTheLastGoodParse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeClaudeDotJSON(t, dir, map[string]any{"oauthAccount": map[string]any{
		"planDisplayName": "Ultra", "accountUuid": claudeTestAccountA,
	}})
	if got := claudeDotJSONFactsFor(""); got.planDisplayName != "Ultra" || got.accountUUID != claudeTestAccountA {
		t.Fatalf("facts=%+v, want the written plan and account", got)
	}
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"oauthAccount":{"planDispl`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := claudeDotJSONFactsFor(""); got.planDisplayName != "Ultra" {
		t.Errorf("a torn read gave %+v, want the last good parse kept", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := claudeDotJSONFactsFor(""); got.planDisplayName != "" || got.accountUUID != "" {
		t.Errorf("a removed file gave %+v, want nothing", got)
	}
}

/* ─────────────────────────── reset tolerance ─────────────────────────── */

func TestClaudeSameResetWindow_ToleratesSubSecondJitter(t *testing.T) {
	probe := int64(1793653199539) // 20:59:59.539, the endpoint's spelling
	for _, tc := range []struct {
		other int64
		want  bool
	}{
		{probe, true},
		{1793653200000, true}, // 21:00:00, the stream's spelling of the same reset
		{probe + 60_000, true},
		{probe - 60_000, true},
		{probe + 60_001, false},
		{probe + 5*3600_000, false}, // the next window of the same id
	} {
		if got := claudeSameResetWindow(probe, tc.other); got != tc.want {
			t.Errorf("claudeSameResetWindow(%d, %d)=%v, want %v", probe, tc.other, got, tc.want)
		}
	}
}

// The weekly row flapped to UNKNOWN after every run: the probe stamps the reset
// with sub-second jitter, the run's usage-less heartbeat with whole seconds,
// and an exact comparison took the heartbeat for a NEW window. Within the
// tolerance it is the same window, so the reading is carried — with the reset
// it was observed with — and a real rollover still is not.
func TestClaudeRateLimitMerge_HeartbeatWithinTheJitterKeepsTheReading(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	probedAt := time.UnixMilli(1793567503215).UTC()
	probeReset := int64(1793653199539)
	if _, err := mergeClaudeRateLimitCacheProbeScoped(context.Background(), cache, map[string]claudeRateLimitBucket{
		claudeWindowSevenDay: {UsedPercentage: 44, ResetsAtMs: probeReset, ObservedAtMs: probedAt.UnixMilli(), usageKnown: true},
	}, probedAt, "", nil, claudeCredStamp{}); err != nil {
		t.Fatal(err)
	}
	runAt := probedAt.Add(10 * time.Minute)
	captureClaudeRateLimitLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"seven_day","resets_at":1793653200}}`, runAt)
	b := claudeCacheSnapshot(t, cache).Buckets[claudeWindowSevenDay]
	if !b.hasObservedUsage() || b.UsedPercentage != 44 || b.ObservedAtMs != probedAt.UnixMilli() || b.ResetsAtMs != probeReset {
		t.Errorf("bucket=%+v, want the probe's 44%% carried with its own observation and reset", b)
	}
	if weekly := claudeCodeMetricsFromCache(runAt, "")[1]; weekly.Unknown || weekly.Consumed == nil || *weekly.Consumed != 44 {
		t.Errorf("weekly row=%+v, want a numeric 44%%, not Unknown", weekly)
	}

	// The next window is a different window: recorded without a reading.
	nextWeek := time.UnixMilli(probeReset).Add(7 * 24 * time.Hour)
	captureClaudeRateLimitLine(fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rate_limit_type":"seven_day","resets_at":%d}}`,
		nextWeek.Unix()), runAt.Add(time.Minute))
	if b := claudeCacheSnapshot(t, cache).Buckets[claudeWindowSevenDay]; b.hasObservedUsage() {
		t.Errorf("bucket=%+v, want the new window recorded as unobserved", b)
	}
}

/* ─────────────────────────── status line ─────────────────────────── */

// A window the payload sends as null — or without a percentage — has nothing
// to show; decoded as a plain value it printed "5h 0%", a full session quota
// for an account (verified Ultra) that has no session window at all.
func TestDefaultStatusLine_ShowsOnlyWindowsWithAReading(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"model":{"display_name":"Opus"},"rate_limits":{"five_hour":null,"seven_day":{"used_percentage":44.4,"resets_at":1793653200}}}`, "Opus · 7d 44%"},
		{`{"rate_limits":{"five_hour":{"resets_at":1793653200},"seven_day":{"used_percentage":81}}}`, "7d 81%"},
		{`{"rate_limits":{"seven_day":{"used_percentage":81},"limits":[{"kind":"weekly_all"}]}}`, "7d 81%"},
		{`{"rate_limits":{"five_hour":{"used_percentage":24},"seven_day":{"used_percentage":81}}}`, "5h 24% · 7d 81%"},
		{`{"rate_limits":null}`, ""},
	} {
		if got := defaultStatusLine([]byte(tc.payload)); got != tc.want {
			t.Errorf("defaultStatusLine(%s)=%q, want %q", tc.payload, got, tc.want)
		}
	}
	// The capture side never recorded a null window either.
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	clearClaudeEnvAuth(t) // env-auth would make the hook skip the write
	captureClaudeRateLimitsFromStatusline([]byte(`{"rate_limits":{"five_hour":null,"seven_day":{"used_percentage":44,"resets_at":1793653200}}}`), time.Now())
	if _, present := claudeCacheSnapshot(t, cache).Buckets[claudeWindowFiveHour]; present {
		t.Error("a null five_hour was captured as a window")
	}
}
