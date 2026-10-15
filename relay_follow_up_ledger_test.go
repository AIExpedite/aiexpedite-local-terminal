package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// The follow-up attempt ledger (relay_follow_up_ledger.go): an attempt is
// written at most once, a fenced id is refused forever, a write whose outcome
// is unknown stays unknown, and rows go only when the broker confirms them.

func newTestFollowUpLedger(t *testing.T) (*relayFollowUpLedger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), relayFollowUpLedgerFileName)
	l := newRelayFollowUpLedger(func() string { return path })
	t.Cleanup(l.release)
	return l, path
}

// reopenFollowUpLedger simulates a restart over the same file: the old
// process lets go of the lock, the new one takes it and reloads.
func reopenFollowUpLedger(t *testing.T, old *relayFollowUpLedger, path string) *relayFollowUpLedger {
	t.Helper()
	old.release()
	l := newRelayFollowUpLedger(func() string { return path })
	t.Cleanup(l.release)
	return l
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
	reloaded := reopenFollowUpLedger(t, l, path)
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
	reloaded := reopenFollowUpLedger(t, l, path)
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
	// Not recorded, so not fenced: answered unknown, never notWritten — the
	// broker must not resend under a new attempt while this id could still land.
	row, write := l.Begin("a6", "m6", "s1", okCheck(time.Now()))
	if write || row.Reason != followUpReasonLedgerFull || outcomeOf(&row) != followUpOutcomeUnknown {
		t.Fatalf("at the cap: write=%v row=%+v", write, row)
	}
	if _, kept := l.rows["a6"]; kept {
		t.Fatalf("an over-cap attempt was recorded")
	}
}

func TestOnlyOneProcessOwnsTheLedger(t *testing.T) {
	owner, path := newTestFollowUpLedger(t)
	if _, write := owner.Begin("a1", "m1", "s1", okCheck(time.Now())); !write {
		t.Fatalf("the owner could not accept an attempt")
	}
	// A second agent process over the same file never reads or writes it.
	other := newRelayFollowUpLedger(func() string { return path })
	t.Cleanup(other.release)
	if other.Own() || other.LedgerID() != "" {
		t.Fatalf("a second process took the ledger")
	}
	if row, write := other.Begin("a2", "m2", "s1", okCheck(time.Now())); write || outcomeOf(&row) != followUpOutcomeUnknown {
		t.Fatalf("a non-owner decided an attempt: write=%v row=%+v", write, row)
	}
	if got := other.Query("a1", owner.LedgerID()); outcomeOf(&got) != followUpOutcomeUnknown {
		t.Fatalf("a non-owner answered a query: %+v", got)
	}
	if _, err := other.ConfirmAcked([]string{"a1"}); err == nil {
		t.Fatalf("a non-owner confirmed rows")
	}
	// The owner's row survived: the non-owner wrote nothing over it.
	reloaded := reopenFollowUpLedger(t, owner, path)
	if got := reloaded.Query("a1", reloaded.LedgerID()); got.State != followUpRowUnknown {
		t.Fatalf("the owner's accepted row was lost: %+v", got)
	}
	if _, kept := reloaded.rows["a2"]; kept {
		t.Fatalf("the non-owner's attempt reached the file")
	}
}

func TestANonOwnerHandsTheFollowUpBack(t *testing.T) {
	owner, path := newTestFollowUpLedger(t)
	owner.Own()
	other := newRelayFollowUpLedger(func() string { return path })
	t.Cleanup(other.release)
	prevLedger := globalRelayFollowUpLedger
	globalRelayFollowUpLedger = other
	t.Cleanup(func() { globalRelayFollowUpLedger = prevLedger })
	var answers []resultMsg
	prevPublish := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		answers = append(answers, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prevPublish })

	now := time.Now()
	cmd := commandMsg{
		ID:                       relayFollowUpEnvelopePrefix + "att-1",
		Type:                     relayFollowUpCommandType,
		SessionID:                "s1",
		Ts:                       now.UnixMilli(),
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(),
	}
	if handleRelayFollowUpCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1"}) {
		t.Fatalf("a non-owner must hand the delivery back (Nack)")
	}
	if len(answers) != 0 {
		t.Fatalf("a non-owner answered: %+v", answers)
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

func TestAFenceThatDidNotReachDiskIsAnsweredUnknown(t *testing.T) {
	// A ledger path inside a regular file: every write fails.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newRelayFollowUpLedger(func() string { return filepath.Join(blocker, relayFollowUpLedgerFileName) })
	lockPath := filepath.Join(t.TempDir(), relayFollowUpLedgerFileName+".lock")
	l.acquireOwnership = func() *os.File { return tryAcquireRelayTurnInboxLock(lockPath) }
	t.Cleanup(l.release)
	c := okCheck(time.Now())
	c.ApprovalPending = true
	row, write := l.Begin("a1", "m1", "s1", c)
	if write || row.State != followUpRowUnknown {
		t.Fatalf("a refusal that is not durable must answer unknown, got write=%v row=%+v", write, row)
	}
	if got := l.Query("never", l.LedgerID()); got.State != followUpRowUnknown {
		t.Fatalf("an unseen-id fence that is not durable must answer unknown, got %+v", got)
	}
	// This process still never writes either id.
	if _, again := l.Begin("a1", "m1", "s1", okCheck(time.Now())); again {
		t.Fatalf("a fenced id was written")
	}
	if _, again := l.Begin("never", "m", "s1", okCheck(time.Now())); again {
		t.Fatalf("a fenced unseen id was written")
	}
	// An accepted row that could not be persisted is never written, and is
	// answered unknown too: only this process remembers the fence.
	row, write = l.Begin("a2", "m2", "s1", okCheck(time.Now()))
	if write || row.State != followUpRowUnknown {
		t.Fatalf("an acceptance that is not durable must answer unknown, got write=%v row=%+v", write, row)
	}
}

func TestEditedAttemptMetadataFencesTheSignedAttemptForGood(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	prevLedger := globalRelayFollowUpLedger
	globalRelayFollowUpLedger = l
	t.Cleanup(func() { globalRelayFollowUpLedger = prevLedger })
	var answers []resultMsg
	prevPublish := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		answers = append(answers, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prevPublish })

	now := time.Now()
	cmd := commandMsg{
		ID:                       relayFollowUpEnvelopePrefix + "att-1",
		Type:                     relayFollowUpCommandType,
		SessionID:                "s1",
		Ts:                       now.UnixMilli(),
		FollowUpAttemptID:        "someone-else",
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(),
	}
	handleRelayFollowUpCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1"})
	if len(answers) != 1 || answers[0].FollowUpOutcome != followUpOutcomeNotWritten {
		t.Fatalf("an edited delivery must be refused, got %+v", answers)
	}
	// Recorded in the ledger: the genuine delivery of the same attempt, which
	// would otherwise pass every check, is refused too.
	if row, write := l.Begin("att-1", "m1", "s1", okCheck(now)); write || row.State != followUpRowRefused {
		t.Fatalf("a refused attempt was written later: write=%v row=%+v", write, row)
	}
}

func TestOnlyTheInboxOwnerDecidesAnExpiredFollowUpTurn(t *testing.T) {
	inbox := newTestRelayInbox(t)
	acceptTestRow(t, inbox, "s1", "fu_a1")
	if owner, accepted := relayTurnAcceptance(inbox, "s1", "fu_a1"); !owner || !accepted {
		t.Fatalf("owner: owner=%v accepted=%v", owner, accepted)
	}
	if owner, accepted := relayTurnAcceptance(inbox, "s1", "fu_a2"); !owner || accepted {
		t.Fatalf("owner, unseen turn: owner=%v accepted=%v", owner, accepted)
	}
	// Another process owns the inbox: it cannot tell an accepted turn from a
	// new one, so it must hand the delivery back rather than refuse it.
	other := newRelayTurnInbox(func() string { return "unused" })
	other.acquireOwnership = func() *os.File { return nil }
	if owner, accepted := relayTurnAcceptance(other, "s1", "fu_a1"); owner || accepted {
		t.Fatalf("non-owner: owner=%v accepted=%v", owner, accepted)
	}
}
