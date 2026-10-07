// cliagent_usage_opencode_post_update_freshness_test.go — "after every run,
// and after every update, the card has a fresh number".
//
// The acceptance test for this feature. It covers each run kind at the seam
// every arm site funnels through, pins the SITES themselves so one cannot
// silently drift out of the set, and proves the reading survives both an agent
// restart and an OpenCode upgrade.
//
// The backend half — that a hint leads to a refresh — is covered by
// terminal-service's cliUsageObserved.route.test.js.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// openCodeRunKind is one managed way an `opencode` turn starts on this device.
type openCodeRunKind struct {
	// label is the fixed internal string the arm site passes.
	label string
	// tapped: the site reads the JSON event stream, so a clean turn is covered
	// by it. Untapped sites (execute, PTY) always owe a reconcile.
	tapped bool
}

// The six paths a managed OpenCode turn can take, plus the direct run the agent
// never spawns. Windows has no PTY spawn (pty_session_windows.go is a stub
// returning errPTYUnsupportedWindows), so it arms nothing there; the rest apply
// on every platform.
var openCodeRunKinds = []openCodeRunKind{
	{label: "native chat", tapped: true},
	{label: "pipe session", tapped: true},
	{label: "smoke", tapped: true},
	{label: "local execute", tapped: false},
	{label: "windows execute", tapped: false},
	{label: "PTY session", tapped: false},
}

// openCodeUsageFrame2 is one step_finish carrying figures, for a given session.
func openCodeUsageStepFrame(nowMs int64, session, message string, in, out int) string {
	return fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":%q,`+
		`"part":{"messageID":%q,"type":"step-finish","reason":"stop","cost":0.002,`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
		nowMs, session, message, in, out)
}

func TestOpenCodeUsage_EveryRunKindLeavesAFreshNumericRow(t *testing.T) {
	for _, kind := range openCodeRunKinds {
		t.Run(kind.label, func(t *testing.T) {
			now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
			openCodeDebtFixture(t, now)

			session := "ses_" + strings.ReplaceAll(kind.label, " ", "_")
			message := "msg_" + session
			// An untapped run's figures can only come from the reconcile, so
			// its export is what the stub answers with.
			(&openCodeCLIStub{
				sessions: sessionList(openCodeSessionRow{session, now.UnixMilli()}),
				exports: map[string]string{
					session: exportWith(session, message, now.UnixMilli(), 30, 6, "0.002"),
				},
			}).install(t)

			handle := armOpenCodeUsageRun(kind.label)
			if handle == nil {
				t.Fatal("the arm returned nil")
			}
			if kind.tapped {
				handle.Observe(openCodeUsageStepFrame(now.UnixMilli(), session, message, 30, 6))
			}
			owed := handle.Finish(kind.tapped)
			if owed != !kind.tapped {
				t.Fatalf("owed = %v for a %s run, want %v", owed, kind.label, !kind.tapped)
			}
			if !openCodeUsageRefreshWaitFor(5 * time.Second) {
				t.Fatal("the usage work never went idle")
			}

			metrics, generation, _ := openCodeLedgerMetrics(openCodeUsageNow())
			if len(metrics) == 0 {
				t.Fatalf("no metric row after a %s run", kind.label)
			}
			tokens := metrics[0]
			if tokens.Consumed == nil || *tokens.Consumed != 36 {
				t.Fatalf("tokens = %v, want 36", tokens.Consumed)
			}
			// The reading is NEVER older than the run it covers.
			observed, err := time.Parse(time.RFC3339, tokens.ObservedAt)
			if err != nil || observed.Before(now.Truncate(time.Second)) {
				t.Fatalf("observedAt = %q (err %v), want at or after the run", tokens.ObservedAt, err)
			}
			// And the backend is told, by generation, that a newer reading is
			// waiting — otherwise it would not ask until the six-hourly gather.
			if generation == nil || generation.Counter <= 0 {
				t.Fatalf("generation = %+v, want the advanced capture generation", generation)
			}
		})
	}
}

func TestOpenCodeUsage_ADirectRunIsReconciledThroughTheNudge(t *testing.T) {
	// The one kind the agent never spawns: the user's own shell or TUI. Its
	// only evidence is a session directory newer than the last pass's start.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_direct", now.UnixMilli()}),
		exports: map[string]string{
			"ses_direct": exportWith("ses_direct", "msg_direct", now.UnixMilli(), 12, 3, "0"),
		},
	}).install(t)

	data := t.TempDir()
	t.Setenv("OPENCODE_DATA", data)
	t.Setenv("XDG_DATA_HOME", "")
	if err := os.MkdirAll(data+"/storage/session", 0o755); err != nil {
		t.Fatal(err)
	}
	if !openCodeSessionStoreChangedSince(openCodeLastPassStartedAt()) {
		t.Fatal("a session directory written since the last pass must read as a change")
	}
	if !nudgeOpenCodeUsageRefresh(now) {
		t.Fatal("the nudge did not start the worker")
	}
	openCodeUsageRefreshWaitFor(5 * time.Second)

	metrics, _, _ := openCodeLedgerMetrics(now)
	if len(metrics) == 0 || metrics[0].Consumed == nil || *metrics[0].Consumed != 15 {
		t.Fatalf("metrics = %+v, want the direct run's 15 tokens", metrics)
	}
}

func TestOpenCodeUsage_ASelfUpdateCarriesTheReadingAndThePendingWorkAcross(t *testing.T) {
	// The agent hands off mid-run: the floor is on disk, unsettled. The next
	// process owes one reconcile for that run, and the reading the PREVIOUS
	// process already committed is republished under a fresh epoch so the
	// backend (which may have applied the old one) fetches it again.
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	(&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_before", now.UnixMilli()}),
		exports: map[string]string{
			"ses_before": exportWith("ses_before", "msg_before", now.UnixMilli(), 40, 10, "0"),
		},
	}).install(t)

	// A pre-update run that DID commit a reading…
	committed := armOpenCodeUsageRun("smoke")
	committed.Observe(openCodeUsageStepFrame(now.UnixMilli(), "ses_before", "msg_before", 40, 10))
	committed.Finish(true)
	// …and one the hand-off cut off before it settled.
	armOpenCodeUsageRunFloor(now.Add(-time.Second))
	openCodeUsageRefreshWaitFor(5 * time.Second)

	before := readOpenCodeUsageLedger()
	if before.Generation.Counter == 0 {
		t.Fatal("the pre-update run committed nothing")
	}

	// "The new process": a fresh epoch, the live-run map forgotten, the files
	// untouched.
	resetOpenCodeUsageLedgerForTests()
	openCodeResetRunState()
	openCodeUsageRefreshEnabled.Store(true)

	adoptAndPayOwedOpenCodeRunDebt(now.Add(time.Second))
	openCodeUsageRefreshWaitFor(5 * time.Second)

	// The reading survived…
	metrics, _, _ := openCodeLedgerMetrics(now)
	if len(metrics) == 0 || metrics[0].Consumed == nil || *metrics[0].Consumed != 50 {
		t.Fatalf("metrics = %+v, want the pre-update reading carried across", metrics)
	}
	// …and the interrupted run's reconcile ran rather than being forgotten.
	if outcome := readOpenCodeUsageLedger().LastPassOutcome; outcome == "" {
		t.Fatal("the interrupted run's reconcile never ran")
	}
	// …and it republishes under THIS process's epoch, so a backend that already
	// applied the old generation asks again.
	if rotated, refused := openCodeRotateGenerationEpoch(now); !rotated || refused {
		t.Fatalf("rotate = %v/%v, want the ledger moved onto this epoch", rotated, refused)
	}
	after := readOpenCodeUsageLedger().Generation
	if after.Epoch == before.Generation.Epoch {
		t.Fatalf("generation epoch %d did not change across the hand-off", after.Epoch)
	}
	if openCodeUsageRecoveryGeneration() == nil {
		t.Fatal("the startup recovery must hint a ledger whose today rows are numeric")
	}
}

/* ─────────────────────────── the arm-site invariant ────────────────────── */

// openCodeArmSites names every file that must put an `opencode` run on the
// usage ledger, and the label it passes. A site that stops arming is a run
// whose tokens vanish from the card, which is exactly the failure this feature
// fixes — and exactly the failure the Antigravity Windows execute path shipped
// with, because nothing pinned its site list.
var openCodeArmSites = map[string]string{
	"opencode_native.go":         "native chat",
	"session.go":                 "pipe session",
	"cliagent_smoke_opencode.go": "smoke",
	"pty_session_unix.go":        "PTY session",
}

func TestOpenCodeUsage_EveryArmSiteStillArms(t *testing.T) {
	for file, label := range openCodeArmSites {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(source)
		if !strings.Contains(text, "armOpenCodeUsageRun(") && !strings.Contains(text, "armOpenCodeUsageForCommand(") {
			t.Fatalf("%s no longer arms the OpenCode usage capture", file)
		}
		if !strings.Contains(text, fmt.Sprintf("%q", label)) {
			t.Fatalf("%s no longer passes the label %q", file, label)
		}
	}
	// pubsub.go carries BOTH execute transports, and the Windows chain is armed
	// at function entry because it has no single post-Start hook.
	pubsub, err := os.ReadFile("pubsub.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{`"local execute"`, `"windows execute"`} {
		if !strings.Contains(string(pubsub), "armOpenCodeUsageForCommand("+label) {
			t.Fatalf("pubsub.go no longer arms the %s path", label)
		}
	}
	// And startup / shutdown still own the lifecycle.
	agent, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"SetOpenCodeUsageRefreshEnabled(true)", "payOwedOpenCodeUsageRefresh()"} {
		if !strings.Contains(string(agent), call) {
			t.Fatalf("agent.go no longer calls %s — nothing would ever arm", call)
		}
	}
	shutdown, err := os.ReadFile("shutdown.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"stopOpenCodeRunDebtRetry()", "drainOpenCodeUsageWrites(ctx)"} {
		if !strings.Contains(string(shutdown), call) {
			t.Fatalf("shutdown.go no longer calls %s — a hand-off would lose the debt", call)
		}
	}
}

func TestOpenCodeUsage_TheRefreshClickRunsOneForcedPass(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeDebtFixture(t, now)
	stub := (&openCodeCLIStub{
		sessions: sessionList(openCodeSessionRow{"ses_click", now.UnixMilli()}),
		exports: map[string]string{
			"ses_click": exportWith("ses_click", "msg_click", now.UnixMilli(), 5, 1, "0"),
		},
	}).install(t)

	// A click reconciles with nothing owed and no continuation booked, which is
	// how it recovers a layout the discovery walk cannot see.
	if outcome := probeOpenCodeUsageLive(context.Background()); outcome != liveProbeOutcomeOK {
		t.Fatalf("live probe = %q, want ok", outcome)
	}
	if calls := stub.exportCount(); calls != 1 {
		t.Fatalf("exports = %d, want exactly one", calls)
	}
	if tokens, _, _ := todayTotals(t, now); tokens != 6 {
		t.Fatalf("tokens = %d, want the clicked reading's 6", tokens)
	}
}
