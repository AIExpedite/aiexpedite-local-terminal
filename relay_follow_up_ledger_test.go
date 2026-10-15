package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The follow-up attempt ledger (relay_follow_up_ledger.go): an attempt is
// written at most once, a fenced id is refused forever, a write whose outcome
// is unknown stays unknown, and rows go only when the broker confirms them.

func newTestFollowUpLedger(t *testing.T) (*relayFollowUpLedger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), relayFollowUpLedgerFileName)
	return newRelayFollowUpLedger(func() string { return path }), path
}

func okCheck(now time.Time) followUpAttemptCheck {
	return followUpAttemptCheck{
		Now:            now,
		ExpiresAtMs:    now.Add(time.Minute).UnixMilli(),
		SignedTsMs:     now.UnixMilli(),
		LiveKind:       true,
		SessionRunning: true,
		ConversationOK: true,
	}
}

func TestFollowUpWireValuesArePinned(t *testing.T) {
	pins := map[string]string{
		relayFollowUpCommandType:      "relay_follow_up",
		relayFollowUpQueryCommandType: "relay_follow_up_query",
		relayFollowUpAckResultType:    "relay_follow_up_ack",
		relayFollowUpEnvelopePrefix:   "relayfollowup_",
		relayFollowUpTurnPrefix:       "fu_",
		followUpOutcomeNotWritten:     "notWritten",
		followUpReasonApprovalPending: "approvalPending",
	}
	for got, want := range pins {
		if got != want {
			t.Errorf("wire value %q, want %q", got, want)
		}
	}
	if followUpAckedMax != 32 || followUpAttemptWindow != 120*time.Second {
		t.Errorf("limits drifted from shared-constants")
	}
	if len(liveFollowUpCertified) != 0 {
		t.Errorf("no CLI is certified for live follow-ups until its certification run passes")
	}
}

func TestFenceReasons(t *testing.T) {
	now := time.Now()
	cases := map[string]func(c *followUpAttemptCheck){
		followUpReasonExpired:              func(c *followUpAttemptCheck) { c.ExpiresAtMs = now.Add(10 * time.Second).UnixMilli() },
		followUpReasonNotLiveKind:          func(c *followUpAttemptCheck) { c.LiveKind = false },
		followUpReasonSessionNotRunning:    func(c *followUpAttemptCheck) { c.SessionRunning = false },
		followUpReasonConversationMismatch: func(c *followUpAttemptCheck) { c.ConversationOK = false },
		followUpReasonApprovalPending:      func(c *followUpAttemptCheck) { c.ApprovalPending = true },
	}
	for want, mutate := range cases {
		c := okCheck(now)
		mutate(&c)
		if got := c.fenceReason(); got != want {
			t.Errorf("fenceReason = %q, want %q", got, want)
		}
	}
	if got := okCheck(now).fenceReason(); got != "" {
		t.Errorf("a fresh attempt is fenced as %q", got)
	}
	// An (unsigned) expiresAt far in the future is still bounded by the signed ts.
	c := okCheck(now)
	c.SignedTsMs = now.Add(-3 * time.Minute).UnixMilli()
	c.ExpiresAtMs = now.Add(time.Hour).UnixMilli()
	if got := c.fenceReason(); got != followUpReasonExpired {
		t.Errorf("an edited expiry extended the attempt: %q", got)
	}
}

func TestAnExpiredOrFencedAttemptIsNeverWrittenAndRefusedForever(t *testing.T) {
	l, path := newTestFollowUpLedger(t)
	c := okCheck(time.Now())
	c.ApprovalPending = true
	row, write := l.Begin("a1", "m1", "s1", c)
	if write || outcomeOf(&row) != followUpOutcomeNotWritten || row.Reason != followUpReasonApprovalPending {
		t.Fatalf("fenced attempt: write=%v row=%+v", write, row)
	}
	// Even once the approval is answered, the same id stays refused — also after a restart.
	reloaded := newRelayFollowUpLedger(func() string { return path })
	row, write = reloaded.Begin("a1", "m1", "s1", okCheck(time.Now()))
	if write || row.State != followUpRowRefused {
		t.Fatalf("a refused id was accepted later: write=%v row=%+v", write, row)
	}
}

func TestAWriteIsAtMostOnceAndAnErrorIsUnknown(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	row, write := l.Begin("a2", "m2", "s1", okCheck(time.Now()))
	if !write || row.State != followUpRowAccepted {
		t.Fatalf("first attempt: write=%v row=%+v", write, row)
	}
	if got := l.Finish("a2", errors.New("broken pipe"), followUpProcessedInCurrent); got.State != followUpRowUnknown {
		t.Fatalf("a failed write must be unknown, got %+v", got)
	}
	if _, again := l.Begin("a2", "m2", "s1", okCheck(time.Now())); again {
		t.Fatalf("a repeated attempt id was written again")
	}
	row, _ = l.Begin("a3", "m3", "s1", okCheck(time.Now()))
	if got := l.Finish(row.AttemptID, nil, followUpProcessedInNext); got.State != followUpRowWritten || got.ProcessedIn != followUpProcessedInNext {
		t.Fatalf("a clean write: %+v", got)
	}
}

func TestACrashBetweenAcceptedAndWrittenIsUnknownAfterRestart(t *testing.T) {
	l, path := newTestFollowUpLedger(t)
	if _, write := l.Begin("a4", "m4", "s1", okCheck(time.Now())); !write {
		t.Fatalf("expected an accepted row")
	}
	// The process dies here, before Finish.
	reloaded := newRelayFollowUpLedger(func() string { return path })
	if got := reloaded.Query("a4", reloaded.LedgerID()); got.State != followUpRowUnknown {
		t.Fatalf("an interrupted write must be unknown after restart, got %+v", got)
	}
}

func TestRowsStayUntilTheWatermarkAndTheCapRefuses(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	row, _ := l.Begin("a5", "m5", "s1", okCheck(time.Now()))
	l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
	if got := l.Query("a5", l.LedgerID()); got.State != followUpRowWritten {
		t.Fatalf("row gone before confirmation: %+v", got)
	}
	if n, err := l.ConfirmAcked([]string{"a5"}); err != nil || n != 1 {
		t.Fatalf("ConfirmAcked = %d, %v", n, err)
	}
	over := make([]string, followUpAckedMax+1)
	if _, err := l.ConfirmAcked(over); err == nil {
		t.Fatalf("an over-cap watermark must be refused, never truncated")
	}
	for i := 0; i < relayFollowUpLedgerMaxRows; i++ {
		l.rows[string(rune(0x4e00+i))] = &relayFollowUpRow{State: followUpRowWritten}
	}
	if row, write := l.Begin("a6", "m6", "s1", okCheck(time.Now())); write || row.Reason != followUpReasonLedgerFull {
		t.Fatalf("at the cap: write=%v row=%+v", write, row)
	}
}

func TestAnUnseenIdIsFencedOnlyForThisLedger(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	if got := l.Query("never", "someone-elses-ledger"); got.State != followUpRowUnknown {
		t.Fatalf("a different ledger must answer unknown, got %+v", got)
	}
	got := l.Query("never", l.LedgerID())
	if got.State != followUpRowRefused || got.Reason != followUpReasonUnseen {
		t.Fatalf("an unseen id must be fenced now, got %+v", got)
	}
	// Fenced for good: a late write of it is refused.
	if _, write := l.Begin("never", "m", "s1", okCheck(time.Now())); write {
		t.Fatalf("a fenced id was written")
	}
}

func TestTheLedgerFileHoldsNoFollowUpText(t *testing.T) {
	l, path := newTestFollowUpLedger(t)
	row, _ := l.Begin("a7", "m7", "s1", okCheck(time.Now()))
	l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if json.Unmarshal(data, &f) != nil {
		t.Fatalf("ledger is not JSON")
	}
	for _, field := range []string{"text", "input"} {
		if containsKey(f, field) {
			t.Fatalf("the ledger stores %q", field)
		}
	}
}

func containsKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == key || containsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if containsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func TestAQueuedFollowUpTurnPastItsExpiryIsRefused(t *testing.T) {
	now := time.Now()
	cmd := commandMsg{ID: "relayturn_fu_a8", SessionID: "s1", Ts: now.Add(-3 * time.Minute).UnixMilli(), FollowUpExpiresAt: now.Add(-time.Minute).UnixMilli()}
	if !followUpTurnExpired(cmd, "fu_a8", now) {
		t.Fatalf("an expired follow-up turn must be refused")
	}
	fresh := commandMsg{ID: "relayturn_fu_a9", SessionID: "s1", Ts: now.UnixMilli(), FollowUpExpiresAt: now.Add(time.Minute).UnixMilli()}
	if followUpTurnExpired(fresh, "fu_a9", now) {
		t.Fatalf("a fresh follow-up turn was refused")
	}
	legacy := commandMsg{ID: "relayturn_fu_a10", SessionID: "s1", Ts: now.Add(-time.Hour).UnixMilli()}
	if followUpTurnExpired(legacy, "fu_a10", now) {
		t.Fatalf("a dispatch without an expiry (legacy) never expires")
	}
	ordinary := commandMsg{ID: "relayturn_t1", SessionID: "s1", Ts: now.Add(-time.Hour).UnixMilli(), FollowUpExpiresAt: 1}
	if followUpTurnExpired(ordinary, "t1", now) {
		t.Fatalf("an ordinary relay turn is never refused for a follow-up expiry")
	}
	frame := refusedFollowUpTurnAck(cmd, "fu_a8", "agent-1")
	if frame.Type != relayTurnAckResultType || frame.RelayTurnID != "fu_a8" || frame.FollowUpOutcome != followUpOutcomeNotWritten || frame.FollowUpReason != followUpReasonExpired {
		t.Fatalf("refusal frame: %+v", frame)
	}
}

func TestTheCapabilityListsNoLiveAgentButReportsTheLedger(t *testing.T) {
	cap := newRelayFollowUpsCapability()
	if cap.LiveAgents == nil || len(cap.LiveAgents) != 0 || !cap.AttemptExpiry || cap.LedgerID == "" {
		t.Fatalf("capability: %+v", cap)
	}
	data, _ := json.Marshal(cap)
	if string(data) == "" || !json.Valid(data) {
		t.Fatalf("capability does not marshal")
	}
}

func TestAnsweringAnApprovalNeverDrivesTheCountBelowZero(t *testing.T) {
	var s ClaudeNativeSession
	s.answerPendingApproval()
	if got := s.pendingApprovals.Load(); got != 0 {
		t.Fatalf("an answer with nothing open left %d", got)
	}
	s.pendingApprovals.Add(2)
	s.answerPendingApproval()
	if got := s.pendingApprovals.Load(); got != 1 {
		t.Fatalf("one answer of two open left %d", got)
	}
	// The reader's reset at a turn's result racing an answer.
	s.pendingApprovals.Store(0)
	s.answerPendingApproval()
	s.pendingApprovals.Add(1)
	if got := s.pendingApprovals.Load(); got != 1 {
		t.Fatalf("an approval after a raced reset reads %d open, want 1", got)
	}
}
