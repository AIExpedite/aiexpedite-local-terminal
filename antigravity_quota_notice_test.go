package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Verbatim from ~/.gemini/antigravity-cli/cli.log on 2026-09-18 (agy 1.2.6,
// Windows), while the stream-json output carried only text-less
// `error_message` steps.
const (
	agyQuotaLogLine = `I0918 10:02:56.745522     460 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 1h27m57s.), retrying in 4s`
	agyQuotaReason  = `RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 1h27m57s.`
	agyErrorStep    = `{"event":"step_update","step_update":{"conversation_id":"ea663263-95c7-4849-b49a-06225d5969d0","step_index":7,"state":"DONE","step_type":"error_message","duration_seconds":0}}`
)

// A `now` in the same local day as the fixture's stamp, after it.
func agyLogNow() time.Time {
	return time.Date(time.Now().Year(), time.September, 18, 10, 30, 0, 0, time.Local)
}

func TestParseAntigravityRunFailureLine(t *testing.T) {
	now := agyLogNow()
	at, reason, ok := parseAntigravityRunFailureLine(agyQuotaLogLine, now)
	if !ok {
		t.Fatal("the captured cli.log line did not parse")
	}
	if reason != agyQuotaReason {
		t.Fatalf("reason = %q", reason)
	}
	want := time.Date(now.Year(), time.September, 18, 10, 2, 56, 745522000, time.Local)
	if !at.Equal(want) {
		t.Fatalf("at = %v, want %v", at, want)
	}
	// A stamp that would be in the future belongs to last year (a lookup that
	// straddles New Year).
	janNow := time.Date(now.Year(), time.January, 2, 0, 0, 0, 0, time.Local)
	at2, _, _ := parseAntigravityRunFailureLine(agyQuotaLogLine, janNow)
	if at2.Year() != now.Year()-1 {
		t.Fatalf("a future-dated stamp should step back a year, got %v", at2)
	}
	// A stamp only HOURS ahead of now is last year’s too: on January 1 a line
	// dated January 2 was written 364 days ago, not tomorrow.
	newYear := time.Date(now.Year(), time.January, 1, 12, 0, 0, 0, time.Local)
	at3, _, _ := parseAntigravityRunFailureLine(
		`I0102 10:02:56.745522     460 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Resets in 1h27m57s.), retrying in 4s`,
		newYear,
	)
	if !at3.Before(newYear) {
		t.Fatalf("a stamp ahead of now must date to last year, got %v (now %v)", at3, newYear)
	}
	// … but a line agy wrote a moment ago, on a log clock a hair ahead of
	// ours, stays in the current year.
	skewed := parseSkewedNow(t, now)
	if skewed.Year() != now.Year() {
		t.Fatalf("a stamp inside the tolerated drift must stay in this year, got %v", skewed)
	}
	for _, line := range []string{
		"",
		"I0918 10:02:56.517054     312 quota_manager.go:45] doRefreshQuota: starting reload (force=true)",
		"plain text",
	} {
		if _, _, ok := parseAntigravityRunFailureLine(line, now); ok {
			t.Fatalf("%q must not parse as a run failure", line)
		}
	}
}

// parseSkewedNow parses a line stamped one second ahead of `now` — the drift
// antigravityLogClockSkew tolerates — and returns its parsed time.
func parseSkewedNow(t *testing.T, now time.Time) time.Time {
	t.Helper()
	ahead := now.Add(time.Second)
	line := fmt.Sprintf(
		"I%02d%02d %02d:%02d:%02d.000000     460 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Resets in 1h.), retrying in 4s",
		int(ahead.Month()), ahead.Day(), ahead.Hour(), ahead.Minute(), ahead.Second(),
	)
	at, _, ok := parseAntigravityRunFailureLine(line, now)
	if !ok {
		t.Fatalf("%q did not parse", line)
	}
	return at
}

func TestIsAntigravityQuotaReason(t *testing.T) {
	for _, r := range []string{agyQuotaReason, "RESOURCE_EXHAUSTED", "HTTP (code 429): slow down", "Daily quota exceeded"} {
		if !isAntigravityQuotaReason(r) {
			t.Fatalf("%q should read as a quota reason", r)
		}
	}
	for _, r := range []string{"connection reset by peer", "tool execution failed: exit status 1", "UNAVAILABLE (code 503): try later"} {
		if isAntigravityQuotaReason(r) {
			t.Fatalf("%q must not read as a quota reason", r)
		}
	}
}

func TestAntigravityResetWindow(t *testing.T) {
	if d, ok := antigravityResetWindow(agyQuotaReason); !ok || d != time.Hour+27*time.Minute+57*time.Second {
		t.Fatalf("window = %v, %v", d, ok)
	}
	if d, ok := antigravityResetWindow("quota reached. Resets in 1h 5m."); !ok || d != time.Hour+5*time.Minute {
		t.Fatalf("spaced window = %v, %v", d, ok)
	}
	if _, ok := antigravityResetWindow("quota reached, no clock"); ok {
		t.Fatal("a reason with no clock must report none")
	}
}

func writeAgyLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.log")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFindAntigravityQuotaFailureInLog(t *testing.T) {
	now := agyLogNow()
	spawnBefore := now.Add(-40 * time.Minute) // 09:50, before the 10:02 line
	spawnAfter := now.Add(-20 * time.Minute)  // 10:10, after it

	t.Run("finds the newest quota reason written since spawn", func(t *testing.T) {
		path := writeAgyLog(t,
			"I0918 09:00:00.000000     1 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): stale, earlier run. Resets in 3h.), retrying in 4s",
			agyQuotaLogLine,
			"I0918 10:03:00.902803     460 run.go:395] Run: attempt 2 failed (RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 1h27m53s.), retrying in 7.5s",
		)
		got := findAntigravityQuotaFailureInLog(path, spawnBefore, now)
		if got == "" || got[len(got)-9:] != "1h27m53s." {
			t.Fatalf("got %q, want the attempt-2 reason", got)
		}
	})

	t.Run("ignores lines older than this process", func(t *testing.T) {
		path := writeAgyLog(t, agyQuotaLogLine)
		if got := findAntigravityQuotaFailureInLog(path, spawnAfter, now); got != "" {
			t.Fatalf("a line from before spawn must not be adopted, got %q", got)
		}
	})

	t.Run("ignores failures that are not quota", func(t *testing.T) {
		path := writeAgyLog(t, "I0918 10:02:56.745522     460 run.go:395] Run: attempt 1 failed (UNAVAILABLE (code 503): upstream connect error), retrying in 4s")
		if got := findAntigravityQuotaFailureInLog(path, spawnBefore, now); got != "" {
			t.Fatalf("a transient failure must not be reported as quota, got %q", got)
		}
	})

	t.Run("a sub-two-minute reset is a throttle agy rides out itself", func(t *testing.T) {
		path := writeAgyLog(t, "I0918 10:02:56.745522     460 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Rate limited. Resets in 45s.), retrying in 4s")
		if got := findAntigravityQuotaFailureInLog(path, spawnBefore, now); got != "" {
			t.Fatalf("a throttle must not cut the turn, got %q", got)
		}
	})

	t.Run("a quota reason with no clock still counts", func(t *testing.T) {
		path := writeAgyLog(t, "I0918 10:02:56.745522     460 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Individual quota reached.), retrying in 4s")
		if got := findAntigravityQuotaFailureInLog(path, spawnBefore, now); got == "" {
			t.Fatal("a clockless quota reason must still be reported")
		}
	})

	t.Run("a record whose reset window already elapsed is spent", func(t *testing.T) {
		// 10:00 + 15m is 10:15, before `now`: that quota came back ahead of this
		// lookup, so the line must not cut a later turn short — the case a raw
		// session idling between its open and its first prompt hits.
		path := writeAgyLog(t, "I0918 10:00:00.000000     1 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 15m.), retrying in 4s")
		if got := findAntigravityQuotaFailureInLog(path, spawnBefore, now); got != "" {
			t.Fatalf("an expired quota record must not be adopted, got %q", got)
		}
		// Still inside its window (10:00 + 3h), so it is still the live quota.
		live := writeAgyLog(t, "I0918 10:00:00.000000     1 run.go:395] Run: attempt 1 failed (RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 3h.), retrying in 4s")
		if got := findAntigravityQuotaFailureInLog(live, spawnBefore, now); got == "" {
			t.Fatal("a record still inside its reset window must be reported")
		}
	})

	t.Run("missing log", func(t *testing.T) {
		if got := findAntigravityQuotaFailureInLog(filepath.Join(t.TempDir(), "nope.log"), spawnBefore, now); got != "" {
			t.Fatalf("got %q", got)
		}
		if got := findAntigravityQuotaFailureInLog("", spawnBefore, now); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestIsAntigravityErrorMessageStep(t *testing.T) {
	if !isAntigravityErrorMessageStep(agyErrorStep) {
		t.Fatal("the captured error step must be recognised")
	}
	for _, line := range []string{
		agyProbeInit, agyProbeStep, agyProbeResult, "plain error_message text", `{"event":"step_update","step_update":{"step_type":"tool"}}`,
	} {
		if isAntigravityErrorMessageStep(line) {
			t.Fatalf("%q is not an error step", line)
		}
	}
}

func TestQuotaFailureFromErrorStepIsOncePerManagedSession(t *testing.T) {
	// Point the lookup at a log with the captured line by running the session
	// "before" it in the same local day.
	path := writeAgyLog(t, agyQuotaLogLine)
	orig := antigravityCliLogPathOverride
	antigravityCliLogPathOverride = path
	defer func() { antigravityCliLogPathOverride = orig }()

	now := agyLogNow()
	s := &CLISession{Command: "agy", antigravityManagedStream: true, StartedAt: now.Add(-40 * time.Minute)}
	// Uses time.Now() for `now`; the fixture stamp is a fixed calendar day, so
	// pin the clock the lookup compares against.
	antigravityQuotaClock = func() time.Time { return now }
	defer func() { antigravityQuotaClock = time.Now }()

	if got := s.quotaFailureFromErrorStep(agyProbeStep); got != "" {
		t.Fatalf("a text delta is not an error step, got %q", got)
	}
	if got := s.quotaFailureFromErrorStep(agyErrorStep); got != agyQuotaReason {
		t.Fatalf("first error step: got %q", got)
	}
	if got := s.quotaFailureFromErrorStep(agyErrorStep); got != "" {
		t.Fatalf("the notice is published once per session, got %q again", got)
	}
	raw := &CLISession{Command: "agy", StartedAt: now.Add(-40 * time.Minute)} // not a managed stream (e.g. `agy --version`)
	if got := raw.quotaFailureFromErrorStep(agyErrorStep); got != "" {
		t.Fatalf("an unmanaged invocation never publishes a notice, got %q", got)
	}
}

// agy 1.5.10 (AIE2, 2026-11-17) does not retry a quota failure: it logs the
// executor error and a Print-mode line, then exits 3. Both are run-failure
// lines carrying the same reason, and both are quota.
func TestParseAntigravityRunFailureLine_Agy1510Shapes(t *testing.T) {
	now := time.Date(time.Now().Year(), time.November, 17, 13, 0, 0, 0, time.Local)
	for _, line := range []string{
		`E1117 12:55:47.801000   7712 executor.go:212] agent executor error: generating and executing: ` + aie2QuotaReason,
		`E1117 12:55:47.802000   7712 print.go:97] Print mode: run ended with error and no response: ` + aie2QuotaReason,
	} {
		_, reason, ok := parseAntigravityRunFailureLine(line, now)
		if !ok || reason != aie2QuotaReason || !isAntigravityQuotaReason(reason) {
			t.Fatalf("line %q → (%q, %v)", line, reason, ok)
		}
	}
	// The decoys every run writes are not failure lines.
	for _, line := range []string{
		`W1117 12:55:44.301000   7712 auth.go:140] You are not logged into Antigravity.`,
		`W1117 12:55:45.001000   7712 routing.go:77] AICode routing probe failed: PERMISSION_DENIED (code 403): Request had insufficient authentication scopes.`,
	} {
		if _, _, ok := parseAntigravityRunFailureLine(line, now); ok {
			t.Fatalf("decoy %q parsed as a failure line", line)
		}
	}
}

// The in-run quota follow reads the run's own PID block: a neighbour's quota
// line in the same file never stops this turn.
func TestAntigravityMonitor_QuotaOnlyFromOwnBlock(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli")
	if err := os.MkdirAll(antigravityLogDir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := antigravityRunLogBase
	antigravityRunLogBase = func() string { return base }
	resetAntigravityLogIndex()
	t.Cleanup(func() { antigravityRunLogBase = prev; resetAntigravityLogIndex() })
	now := time.Now()
	stamp := now.Format("0102 15:04:05")
	body := fmt.Sprintf("I%s.000000 301 main.go:1] Starting language server process with pid 301\n"+
		"E%s.000000 301 print.go:97] Print mode: run ended with error and no response: %s\n"+
		"I%s.000000 302 main.go:1] Starting language server process with pid 302\n", stamp, stamp, aie2QuotaReason, stamp)
	name := "cli-" + now.In(antigravityLogZone()).Format("20060102_150405") + ".log"
	if err := os.WriteFile(filepath.Join(antigravityLogDir(base), name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for pid, want := range map[int]string{302: "", 301: aie2QuotaReason} {
		got := ""
		mon := newAntigravityTurnMonitor("s", now.Add(-time.Second), 1024, nil, func(r string) { got = r })
		mon.pid = pid
		mon.followLog(now)
		if got != want {
			t.Fatalf("pid %d: quota = %q, want %q", pid, got, want)
		}
	}
}
