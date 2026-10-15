package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// newEstablishedTestFollowUpLedger is a ledger whose identity was minted an
// hour ago, so it vouches for attempts signed now (vouchesForLocked refuses
// to within the clock-skew margin of a fresh identity).
func newEstablishedTestFollowUpLedger(t *testing.T) (*relayFollowUpLedger, string) {
	t.Helper()
	l, path := newTestFollowUpLedger(t)
	l.now = func() time.Time { return time.Now().Add(-time.Hour) }
	if l.LedgerID() == "" {
		t.Fatalf("the test ledger was not taken")
	}
	l.now = time.Now
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
	if got := reloaded.Query("a4", reloaded.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowUnknown {
		t.Fatalf("an interrupted write must be unknown after restart, got %+v", got)
	}
}

func TestRowsStayUntilTheWatermarkAndTheCapRefuses(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	row, _ := l.Begin("a5", "m5", "s1", okCheck(time.Now()))
	l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
	if got := l.Query("a5", l.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowWritten {
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
	if got := other.Query("a1", owner.LedgerID(), time.Now().UnixMilli()); outcomeOf(&got) != followUpOutcomeUnknown {
		t.Fatalf("a non-owner answered a query: %+v", got)
	}
	if _, err := other.ConfirmAcked([]string{"a1"}); err == nil {
		t.Fatalf("a non-owner confirmed rows")
	}
	// The owner's row survived: the non-owner wrote nothing over it.
	reloaded := reopenFollowUpLedger(t, owner, path)
	if got := reloaded.Query("a1", reloaded.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowUnknown {
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
	l, _ := newEstablishedTestFollowUpLedger(t)
	if got := l.Query("never", "someone-elses-ledger", time.Now().UnixMilli()); got.State != followUpRowUnknown {
		t.Fatalf("a different ledger must answer unknown, got %+v", got)
	}
	got := l.Query("never", l.LedgerID(), time.Now().UnixMilli())
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
	// `followUpExpiresAt` is unsigned: stripping it must not lift the deadline,
	// which then comes from the signed ts plus the attempt window.
	stripped := commandMsg{ID: "relayturn_fu_a10", SessionID: "s1", Ts: now.Add(-3 * time.Minute).UnixMilli()}
	if !followUpTurnExpired(stripped, "fu_a10", now) {
		t.Fatalf("a stripped expiry must fall back to the signed ts deadline")
	}
	strippedFresh := commandMsg{ID: "relayturn_fu_a11", SessionID: "s1", Ts: now.UnixMilli()}
	if followUpTurnExpired(strippedFresh, "fu_a11", now) {
		t.Fatalf("a freshly signed follow-up turn was refused")
	}
	unsigned := commandMsg{ID: "relayturn_fu_a12", SessionID: "s1"}
	if followUpTurnExpired(unsigned, "fu_a12", now) {
		t.Fatalf("a dispatch with no timestamp at all has no deadline to apply")
	}
	ordinary := commandMsg{ID: "relayturn_t1", SessionID: "s1", Ts: now.Add(-time.Hour).UnixMilli(), FollowUpExpiresAt: 1}
	if followUpTurnExpired(ordinary, "t1", now) {
		t.Fatalf("an ordinary relay turn is never refused for a follow-up expiry")
	}
	frame := refusedFollowUpTurnAck(cmd, "fu_a8", "agent-1", followUpReasonExpired)
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
	if got := l.Query("never", l.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowUnknown {
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
	// A redelivery in this same process reads the STORED row, so that row must
	// be unknown too. A stored `refused` would answer notWritten and let the
	// broker resend under a new attempt, while the lost-on-restart fence could
	// still let the original be written: the person would get it twice.
	for _, id := range []string{"a1", "never", "a2"} {
		again, write := l.Begin(id, "m", "s1", okCheck(time.Now()))
		if write {
			t.Fatalf("%s: a non-durable fence was written on redelivery", id)
		}
		if again.State != followUpRowUnknown || outcomeOf(&again) != followUpOutcomeUnknown {
			t.Fatalf("%s: a redelivered non-durable fence answers %+v, want unknown", id, again)
		}
		if q := l.Query(id, l.LedgerID(), time.Now().UnixMilli()); q.State != followUpRowUnknown {
			t.Fatalf("%s: a reconciliation of a non-durable fence answers %+v, want unknown", id, q)
		}
	}
}

// A drain must not swallow the follow-up protocol: both commands correlate to
// work accepted before it began, and refusing them withholds the ack the
// broker settles or reconciles the attempt on.
func TestFollowUpProtocolCommandsPassThroughADrain(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  commandMsg
		want bool
	}{
		{"live attempt", commandMsg{Type: relayFollowUpCommandType, ID: relayFollowUpEnvelopePrefix + "att-1"}, true},
		{"reconciliation query", commandMsg{Type: relayFollowUpQueryCommandType}, true},
		{"queued follow-up turn", commandMsg{Type: "session_input", ID: relayTurnEnvelopePrefix + "fu_att-1"}, true},
		{"a new one-shot execute", commandMsg{Command: "ls"}, false},
	} {
		if got := isDrainPassThrough(tc.cmd); got != tc.want {
			t.Fatalf("%s: isDrainPassThrough = %v, want %v", tc.name, got, tc.want)
		}
		if got := isWorkStartCommand(tc.cmd); got == tc.want {
			t.Fatalf("%s: isWorkStartCommand = %v, want %v", tc.name, got, !tc.want)
		}
	}
}

func TestEditedAttemptMetadataFencesTheSignedAttemptForGood(t *testing.T) {
	l, _ := newEstablishedTestFollowUpLedger(t)
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
		FollowUpLedgerID:         l.LedgerID(),
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

// A live attempt minted under a ledger that has since been lost or replaced
// may already have been written: it is answered unknown, and neither recorded
// nor written into this ledger.
func TestALiveAttemptForAReplacedLedgerIsAnsweredUnknown(t *testing.T) {
	l, _ := newEstablishedTestFollowUpLedger(t)
	now := time.Now()
	for _, ledgerID := range []string{"the-lost-ledger", ""} {
		row, write := l.BeginForLedger(ledgerID, now.UnixMilli(), "att-1", "m1", "s1", okCheck(now))
		if write || row.State != followUpRowUnknown {
			t.Fatalf("ledger %q: write=%v row=%+v, want unknown and no write", ledgerID, write, row)
		}
		if _, ok := l.rows["att-1"]; ok {
			t.Fatalf("ledger %q: the mismatched attempt was recorded", ledgerID)
		}
	}
	if _, write := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), "att-1", "m1", "s1", okCheck(now)); !write {
		t.Fatalf("an attempt naming this ledger must be written")
	}
	// Recorded here, so a repeat answers from the row whatever ledger it names.
	if row, write := l.BeginForLedger("the-lost-ledger", now.UnixMilli(), "att-1", "m1", "s1", okCheck(now)); write || row.State != followUpRowAccepted {
		t.Fatalf("a recorded attempt: write=%v row=%+v", write, row)
	}
}

// The fence of a queued follow-up turn takes session-manager locks, while a
// manager's removeSessionIfSame holds its own lock as it releases the
// session's inbox rows. Calling the fence under the inbox lock could deadlock
// the two, so it must run outside it.
func TestTheFollowUpTurnFenceNeverRunsUnderTheInboxLock(t *testing.T) {
	inbox := newTestRelayInbox(t)
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: time.Now().UnixMilli()}
	calls := 0
	runFencedRelayTurn(context.Background(), inbox, cmd, "fu_a1", "agent-1",
		func(time.Time) string {
			calls++
			if !inbox.mu.TryLock() {
				t.Errorf("the fence ran under the inbox lock")
				return ""
			}
			inbox.mu.Unlock()
			return ""
		},
		nil,
		func(string) {},
		func(relayTurnRow) {},
		func(context.Context) {})
	if calls == 0 {
		t.Fatalf("the fence was never consulted")
	}
}

// A session that is gone when the fence is read before the claim is refused
// at the claim, exactly as when the fence ran under the lock.
func TestAQueuedFollowUpTurnReadAsGoneBeforeTheClaimIsRefused(t *testing.T) {
	inbox := newTestRelayInbox(t)
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: time.Now().UnixMilli()}
	var refusal string
	dispatched := false
	runFencedRelayTurn(context.Background(), inbox, cmd, "fu_a1", "agent-1",
		func(time.Time) string { return followUpReasonSessionNotRunning },
		func(time.Time) string { return "" },
		func(reason string) { refusal = reason },
		func(relayTurnRow) {},
		func(context.Context) { dispatched = true })
	if refusal != followUpReasonSessionNotRunning || dispatched {
		t.Fatalf("refusal=%q dispatched=%v", refusal, dispatched)
	}
	if r, ok := inbox.snapshot()[relayTurnKey{"s1", "fu_a1"}]; !ok || r.State != relayTurnStateRefused {
		t.Fatalf("a refused turn was claimed or not fenced: %+v", r)
	}
}

// runFencedTestTurn delivers one queued follow-up turn through the fenced
// claim, recording the refusal and the dispatch it led to.
func runFencedTestTurn(inbox *relayTurnInbox, cmd commandMsg, turnID string, dispatch func(context.Context)) (handled bool, refusal string, dispatched bool) {
	handled = runFencedRelayTurn(context.Background(), inbox, cmd, turnID, "agent-1",
		func(now time.Time) string { return followUpTurnRefusal(cmd, turnID, now) },
		func(now time.Time) string {
			if followUpTurnExpired(cmd, turnID, now) {
				return followUpReasonExpired
			}
			return ""
		},
		func(reason string) { refusal = reason },
		func(relayTurnRow) {},
		func(ctx context.Context) {
			dispatched = true
			if dispatch != nil {
				dispatch(ctx)
			}
		})
	return handled, refusal, dispatched
}

func TestOnlyTheInboxOwnerDecidesARefusedFollowUpTurn(t *testing.T) {
	now := time.Now()
	expired := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: now.Add(-5 * time.Minute).UnixMilli()}
	// Another process owns the inbox: it cannot tell an accepted turn from a
	// new one, so it must hand the delivery back rather than refuse it.
	other := newRelayTurnInbox(func() string { return "unused" })
	other.acquireOwnership = func() *os.File { return nil }
	if handled, refusal, dispatched := runFencedTestTurn(other, expired, "fu_a1", nil); handled || refusal != "" || dispatched {
		t.Fatalf("non-owner: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	inbox := newTestRelayInbox(t)
	// Accepted earlier: re-acked, never refused late.
	acceptTestRow(t, inbox, "s1", "fu_a1")
	if handled, refusal, dispatched := runFencedTestTurn(inbox, expired, "fu_a1", nil); !handled || refusal != "" || dispatched {
		t.Fatalf("accepted: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	// Never seen: refused without a claim, so it can never run afterwards.
	unseen := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a2", SessionID: "s1", Ts: expired.Ts}
	if handled, refusal, dispatched := runFencedTestTurn(inbox, unseen, "fu_a2", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("unseen: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if r, ok := inbox.snapshot()[relayTurnKey{"s1", "fu_a2"}]; !ok || r.State != relayTurnStateRefused {
		t.Fatalf("a refused turn was claimed or not fenced: %+v", r)
	}
}

// The fence is decided under the claim's lock, at the claim. A delivery that
// claimed before the deadline can still accept and write, so a duplicate past
// the deadline is handed back — not answered `notWritten`, which would make
// the broker queue a replacement for a turn that then runs. Once the first
// delivery settles without accepting, a redelivery is refused.
func TestAClaimedInFlightFollowUpTurnIsNotRefusedAsExpired(t *testing.T) {
	inbox := newTestRelayInbox(t)
	start := time.Now()
	clock := start
	inbox.now = func() time.Time { return clock }
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", WorkspaceID: "ws-1", UID: "u-1", Ts: start.UnixMilli()}

	var dupHandled bool
	var dupRefusal string
	var dupDispatched bool
	handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", func(context.Context) {
		// Paused after the claim, until past the deadline.
		clock = start.Add(followUpAttemptWindow + followUpSkewMargin + time.Minute)
		dupHandled, dupRefusal, dupDispatched = runFencedTestTurn(inbox, cmd, "fu_a1", nil)
	})
	// It returned past the deadline without accepting: it never wrote and now
	// never can, so it is answered at once instead of waiting for a retry.
	if !handled || refusal != followUpReasonExpired || !dispatched {
		t.Fatalf("first delivery: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if dupHandled || dupRefusal != "" || dupDispatched {
		t.Fatalf("expired duplicate of an in-flight turn: handled=%v refusal=%q dispatched=%v", dupHandled, dupRefusal, dupDispatched)
	}
	// The first delivery returned without accepting: now it is decided.
	if handled, refusal, dispatched = runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("redelivery after an unaccepted dispatch: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
}

// A manager can wait on a session lock between the claim and the acceptance
// boundary. A turn whose expiry passed meanwhile is refused at acceptance —
// the manager writes nothing and the broker may requeue — and a redelivery is
// refused at its claim, so the turn can never run after its replacement.
func TestAQueuedFollowUpTurnPastItsExpiryAtAcceptanceIsRefused(t *testing.T) {
	inbox := newTestRelayInbox(t)
	start := time.Now()
	clock := start
	inbox.now = func() time.Time { return clock }
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", WorkspaceID: "ws-1", UID: "u-1", Ts: start.UnixMilli()}

	var acceptErr error
	handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", func(ctx context.Context) {
		// Blocked on the session's stdin lock until past the deadline.
		clock = start.Add(followUpAttemptWindow + followUpSkewMargin + time.Minute)
		acceptErr = relayTurnAcceptor(ctx)()
	})
	if acceptErr == nil || !strings.Contains(acceptErr.Error(), relayAcceptanceNotRecorded) {
		t.Fatalf("an expired turn was accepted: %v", acceptErr)
	}
	if !handled || refusal != followUpReasonExpired || !dispatched {
		t.Fatalf("expired at acceptance: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if row := inbox.snapshot()[relayTurnKey{"s1", "fu_a1"}]; row.State == relayTurnStateAccepted {
		t.Fatalf("an expired turn was recorded accepted: %+v", row)
	}
	if handled, refusal, dispatched = runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("redelivery after a refused acceptance: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
}

// `followUpExpiresAt` rides outside the signature. A copy of a queued
// follow-up turn with a shortened expiry is refused as expired, and the
// broker may then queue a replacement — so the refusal is recorded, and the
// original copy, still inside its signed bound, gets the same answer instead
// of running the message a second time. The fence survives a restart and a
// watermark naming the turn, and lapses only once the signed deadline passed.
func TestARefusedFollowUpTurnStaysRefusedForEveryLaterCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), relayTurnInboxFileName)
	inbox := newRelayTurnInbox(func() string { return path })
	start := time.Now()
	clock := start
	inbox.now = func() time.Time { return clock }
	original := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", WorkspaceID: "ws-1", UID: "u-1",
		Ts: start.UnixMilli(), FollowUpExpiresAt: start.Add(followUpAttemptWindow).UnixMilli()}
	shortened := original
	shortened.FollowUpExpiresAt = start.Add(-time.Minute).UnixMilli()

	if handled, refusal, dispatched := runFencedTestTurn(inbox, shortened, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("shortened copy: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if handled, refusal, dispatched := runFencedTestTurn(inbox, original, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("original copy after the refusal: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if n := inbox.ConfirmAcked("s1", []string{"fu_a1"}); n != 0 {
		t.Fatalf("a watermark deleted the refusal fence")
	}
	inbox.ReleaseSession("s1")
	inbox.release()

	restarted := newRelayTurnInbox(func() string { return path })
	t.Cleanup(restarted.release)
	restarted.now = func() time.Time { return clock }
	if handled, refusal, dispatched := runFencedTestTurn(restarted, original, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("original copy after a restart: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}

	// Past the signed deadline every copy is expired on its own, so the row goes.
	clock = start.Add(followUpAttemptWindow + followUpSkewMargin + time.Second)
	if handled, refusal, dispatched := runFencedTestTurn(restarted, original, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("copy past the signed deadline: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if r := restarted.snapshot()[relayTurnKey{"s1", "fu_a1"}]; r.State == relayTurnStateClaimed || r.State == relayTurnStateAccepted {
		t.Fatalf("an expired turn was claimed: %+v", r)
	}
}

// A refusal that cannot be written to disk would be gone after a restart, so
// it is never answered notWritten (the broker would resend while the original
// could still run): the turn is fenced in this process and left unanswered.
func TestAnUnrecordedFollowUpTurnRefusalIsNotAnswered(t *testing.T) {
	inbox := newTestRelayInbox(t)
	inbox.writeFile = func(string, []byte) error { return errors.New("disk full") }
	start := time.Now()
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: start.UnixMilli(),
		FollowUpExpiresAt: start.Add(-time.Minute).UnixMilli()}
	for i := 0; i < 2; i++ {
		if handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != "" || dispatched {
			t.Fatalf("delivery %d: handled=%v refusal=%q dispatched=%v", i, handled, refusal, dispatched)
		}
	}
	original := cmd
	original.FollowUpExpiresAt = start.Add(followUpAttemptWindow).UnixMilli()
	if handled, refusal, dispatched := runFencedTestTurn(inbox, original, "fu_a1", nil); !handled || refusal != "" || dispatched {
		t.Fatalf("original copy: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
}

// A queued follow-up turn whose session exits after the claim but before the
// manager's acceptance boundary only gets the kind's error frame from SEND; it
// is answered notWritten/sessionNotRunning so the broker requeues at once. A
// dispatch that accepted, or that failed while its session is still there,
// is never answered that way.
func TestAQueuedFollowUpTurnWhoseSessionExitsBeforeAcceptanceIsRefused(t *testing.T) {
	resetFencedSessions(t)
	inbox := newTestRelayInbox(t)
	now := time.Now()
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s-exit", Ts: now.UnixMilli()}
	handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", func(context.Context) {
		// The session leaves between the claim and the manager's status check,
		// which refuses before invoking the acceptor.
		markSessionsFenced([]string{"s-exit"})
	})
	if !handled || refusal != followUpReasonSessionNotRunning || !dispatched {
		t.Fatalf("exited session: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}

	// Accepted, then the session went away: the turn ran, so no refusal.
	accepted := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a2", SessionID: "s-late", Ts: now.UnixMilli()}
	handled, refusal, _ = runFencedTestTurn(inbox, accepted, "fu_a2", func(ctx context.Context) {
		if err := relayTurnAcceptor(ctx)(); err != nil {
			t.Fatalf("accept: %v", err)
		}
		markSessionsFenced([]string{"s-late"})
	})
	if !handled || refusal != "" {
		t.Fatalf("accepted turn: handled=%v refusal=%q", handled, refusal)
	}

	// Failed without accepting while the session is still here: left to the
	// broker's retry, as before.
	alive := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a3", SessionID: "s-alive", Ts: now.UnixMilli()}
	if handled, refusal, _ = runFencedTestTurn(inbox, alive, "fu_a3", func(context.Context) {}); !handled || refusal != "" {
		t.Fatalf("live session: handled=%v refusal=%q", handled, refusal)
	}
}

// A queued follow-up turn for a session fenced off this computer never starts
// and is answered with an outcome, unless it was accepted already.
func TestAQueuedFollowUpTurnForAFencedSessionIsRefused(t *testing.T) {
	resetFencedSessions(t)
	markSessionsFenced([]string{"s-moved"})
	now := time.Now()
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s-moved", Ts: now.UnixMilli()}
	if got := followUpTurnRefusal(cmd, "fu_a1", now); got != followUpReasonSessionNotRunning {
		t.Fatalf("fenced refusal = %q", got)
	}
	if got := followUpTurnRefusal(commandMsg{ID: relayTurnEnvelopePrefix + "t1", SessionID: "s-moved"}, "t1", now); got != "" {
		t.Fatalf("an ordinary relay turn is left to the generic fence, got %q", got)
	}
	inbox := newTestRelayInbox(t)
	if handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != followUpReasonSessionNotRunning || dispatched {
		t.Fatalf("fenced: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	frame := refusedFollowUpTurnAck(cmd, "fu_a1", "agent-1", followUpReasonSessionNotRunning)
	if frame.FollowUpOutcome != followUpOutcomeNotWritten || frame.FollowUpReason != followUpReasonSessionNotRunning {
		t.Fatalf("refusal frame: %+v", frame)
	}
}

// A queued follow-up turn whose session is gone from this device (ended
// normally, never fenced) is refused with an outcome rather than dispatched
// into a SEND that would fail without acking the broker. A session the
// manager still holds, or a manager that is not running, is never raced.
func TestAQueuedFollowUpTurnForAMissingSessionIsRefused(t *testing.T) {
	resetFencedSessions(t)
	now := time.Now()
	prev := globalClaudeNativeManager
	t.Cleanup(func() { globalClaudeNativeManager = prev })

	cmd := commandMsg{Type: "claude_native_send", ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s-gone", Ts: now.UnixMilli()}

	// No manager at all: nothing is known, so nothing is refused here.
	globalClaudeNativeManager = nil
	if got := followUpTurnRefusal(cmd, "fu_a1", now); got != "" {
		t.Fatalf("with no manager the turn must not be refused, got %q", got)
	}

	// A live manager that does not hold the session: definitely gone.
	globalClaudeNativeManager = NewClaudeNativeManager(nil)
	if got := followUpTurnRefusal(cmd, "fu_a1", now); got != followUpReasonSessionNotRunning {
		t.Fatalf("missing session refusal = %q", got)
	}
	// An unrecognised command type stays unjudged.
	if got := followUpTurnRefusal(commandMsg{Type: "some_future_send", ID: cmd.ID, SessionID: "s-gone", Ts: cmd.Ts}, "fu_a1", now); got != "" {
		t.Fatalf("an unknown kind must not be refused, got %q", got)
	}
	// An ordinary relay turn is left to the generic paths.
	if got := followUpTurnRefusal(commandMsg{Type: "claude_native_send", ID: relayTurnEnvelopePrefix + "t1", SessionID: "s-gone"}, "t1", now); got != "" {
		t.Fatalf("an ordinary relay turn must not be refused, got %q", got)
	}

	inbox := newTestRelayInbox(t)
	if handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != followUpReasonSessionNotRunning || dispatched {
		t.Fatalf("missing session: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}

	// A generic managed session (`session_input`) is judged by the
	// SessionManager the same way.
	prevSM := globalSessionManager
	t.Cleanup(func() { globalSessionManager = prevSM })
	generic := commandMsg{Type: "session_input", ID: cmd.ID, SessionID: "s-gone", Ts: cmd.Ts}
	globalSessionManager = nil
	if got := followUpTurnRefusal(generic, "fu_a1", now); got != "" {
		t.Fatalf("with no session manager the turn must not be refused, got %q", got)
	}
	globalSessionManager = NewSessionManager(&Config{})
	if got := followUpTurnRefusal(generic, "fu_a1", now); got != followUpReasonSessionNotRunning {
		t.Fatalf("missing generic session refusal = %q", got)
	}
	globalSessionManager.sessions["s-gone"] = &CLISession{Status: "running"}
	if got := followUpTurnRefusal(generic, "fu_a1", now); got != "" {
		t.Fatalf("a held generic session must not be refused, got %q", got)
	}
	// A retained tombstone whose child exited is gone.
	globalSessionManager.sessions["s-gone"] = &CLISession{Status: "ended"}
	if got := followUpTurnRefusal(generic, "fu_a1", now); got != followUpReasonSessionNotRunning {
		t.Fatalf("ended generic session refusal = %q", got)
	}
}

// A kind session the manager still retains after its child exited (status
// "ended", before the manager removes it) is gone: its SEND would refuse it
// without ever invoking the acceptance callback. A retained session in any
// other state is never raced.
func TestAQueuedFollowUpTurnForARetainedEndedSessionIsRefused(t *testing.T) {
	resetFencedSessions(t)
	now := time.Now()
	prev := globalClaudeNativeManager
	t.Cleanup(func() { globalClaudeNativeManager = prev })
	globalClaudeNativeManager = NewClaudeNativeManager(nil)

	cmd := commandMsg{Type: "claude_native_send", ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s-ended", Ts: now.UnixMilli()}
	session := &ClaudeNativeSession{status: "running"}
	globalClaudeNativeManager.sessions["s-ended"] = session
	if got := followUpTurnRefusal(cmd, "fu_a1", now); got != "" {
		t.Fatalf("a running session must not be refused, got %q", got)
	}
	session.status = "ended"
	if got := followUpTurnRefusal(cmd, "fu_a1", now); got != followUpReasonSessionNotRunning {
		t.Fatalf("retained ended session refusal = %q", got)
	}
	inbox := newTestRelayInbox(t)
	if handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != followUpReasonSessionNotRunning || dispatched {
		t.Fatalf("retained ended session: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
}

// Tearing a session down keeps an accepted follow-up turn whose ack is not yet
// confirmed: its redelivery, finding no session, must re-ack rather than be
// refused as sessionNotRunning for a message that was already written.
func TestReleasingASessionKeepsAnAcceptedFollowUpTurn(t *testing.T) {
	resetFencedSessions(t)
	now := time.Now()
	prev := globalClaudeNativeManager
	t.Cleanup(func() { globalClaudeNativeManager = prev })
	globalClaudeNativeManager = NewClaudeNativeManager(nil)

	inbox := newTestRelayInbox(t)
	acceptTestRow(t, inbox, "s-gone", "fu_a1")
	acceptTestRow(t, inbox, "s-gone", "t1")
	inbox.ReleaseSession("s-gone")
	rows := inbox.snapshot()
	if _, ok := rows[relayTurnKey{"s-gone", "fu_a1"}]; !ok {
		t.Fatalf("the accepted follow-up turn was released")
	}
	if _, ok := rows[relayTurnKey{"s-gone", "t1"}]; ok {
		t.Fatalf("an ordinary accepted turn must still be released")
	}

	cmd := commandMsg{Type: "claude_native_send", ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s-gone", Ts: now.UnixMilli()}
	if handled, refusal, dispatched := runFencedTestTurn(inbox, cmd, "fu_a1", nil); !handled || refusal != "" || dispatched {
		t.Fatalf("redelivery after teardown: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if n := inbox.ConfirmAcked("s-gone", []string{"fu_a1"}); n != 1 {
		t.Fatalf("the watermark must still delete the retained row, deleted %d", n)
	}
}

// The generic fence gate leaves follow-up protocol commands to their own
// handlers (which answer with an outcome); everything else but END is still
// refused there.
func TestTheFenceGateLeavesFollowUpProtocolCommandsToTheirHandlers(t *testing.T) {
	resetFencedSessions(t)
	markSessionsFenced([]string{"s-moved"})
	for _, cmd := range []commandMsg{
		{Type: relayFollowUpCommandType, ID: relayFollowUpEnvelopePrefix + "att-1", SessionID: "s-moved"},
		{Type: relayFollowUpQueryCommandType, ID: "q-1", SessionID: "s-moved"},
		{Type: "claude_native_send", ID: relayTurnEnvelopePrefix + "fu_att-1", SessionID: "s-moved"},
	} {
		if refuseFencedSessionCommand(context.Background(), nil, nil, cmd, &Config{AgentID: "agent-1"}) {
			t.Fatalf("follow-up protocol command %s/%s was refused generically", cmd.Type, cmd.ID)
		}
	}
}

// A follow-up delivery delayed past the generic 60 s command-staleness window
// must still reach its handler, which emits the ack the broker waits on. Past
// its own attempt window plus the skew margin it is stale like anything else.
func TestFollowUpCommandsKeepTheirAttemptStalenessWindow(t *testing.T) {
	now := time.Now()
	window := int64((followUpAttemptWindow + followUpSkewMargin).Seconds())
	for _, tc := range []struct {
		name string
		cmd  commandMsg
		want int
	}{
		{"live attempt", commandMsg{Type: relayFollowUpCommandType, ID: relayFollowUpEnvelopePrefix + "att-1"}, int(window)},
		{"reconciliation query", commandMsg{Type: relayFollowUpQueryCommandType}, int(window)},
		{"queued follow-up turn", commandMsg{ID: relayTurnEnvelopePrefix + "fu_att-1"}, int(window)},
		{"ordinary relay turn", commandMsg{ID: relayTurnEnvelopePrefix + "t-1"}, maxCommandAgeSec},
		{"ordinary command", commandMsg{Command: "ls"}, maxCommandAgeSec},
	} {
		if got := commandMaxAgeSec(tc.cmd); got != tc.want {
			t.Fatalf("%s: commandMaxAgeSec = %d, want %d", tc.name, got, tc.want)
		}
	}
	// The future-skew bound stays at the margin the follow-up fences
	// themselves tolerate, however long the age window is.
	skew := int(followUpSkewMargin.Seconds())
	for _, tc := range []struct {
		name string
		cmd  commandMsg
		want int
	}{
		{"live attempt", commandMsg{Type: relayFollowUpCommandType, ID: relayFollowUpEnvelopePrefix + "att-1"}, skew},
		{"reconciliation query", commandMsg{Type: relayFollowUpQueryCommandType}, skew},
		{"queued follow-up turn", commandMsg{ID: relayTurnEnvelopePrefix + "fu_att-1"}, skew},
		{"ordinary relay turn", commandMsg{ID: relayTurnEnvelopePrefix + "t-1"}, maxCommandAgeSec},
		{"ordinary command", commandMsg{Command: "ls"}, maxCommandAgeSec},
	} {
		if got := commandMaxFutureSkewSec(tc.cmd); got != tc.want {
			t.Fatalf("%s: commandMaxFutureSkewSec = %d, want %d", tc.name, got, tc.want)
		}
	}
	// 90 s old: stale generically, still inside the follow-up window.
	ts := now.Add(-90 * time.Second).UnixMilli()
	if !isCommandStale(ts) {
		t.Fatalf("a 90 s old command must be generically stale")
	}
	if isCommandStaleAfter(ts, int(window), skew) {
		t.Fatalf("a 90 s old follow-up must not be stale inside its attempt window")
	}
	// Past the attempt window plus the margin, the broker has expired the
	// attempt itself, so the generic rejection applies again.
	expired := now.Add(-(time.Duration(window) + 1) * time.Second).UnixMilli()
	if !isCommandStaleAfter(expired, int(window), skew) {
		t.Fatalf("a follow-up past its window must be stale")
	}
	// A device clock far behind the broker must NOT admit a ts that far in
	// the device's future: fenceReason reads that ts plus the attempt window,
	// so the original delivery would outlive the broker's real window and
	// could run after a replacement was queued.
	future := now.Add(time.Duration(skew+30) * time.Second).UnixMilli()
	if !isCommandStaleAfter(future, int(window), skew) {
		t.Fatalf("a follow-up dated past the skew margin in the future must be stale")
	}
	if isCommandStaleAfter(now.Add(time.Duration(skew/2)*time.Second).UnixMilli(), int(window), skew) {
		t.Fatalf("a follow-up inside the skew margin must be admitted")
	}
}

// `ledgerId` rides OUTSIDE the command signature, so naming the advertised
// ledger must not be enough on its own to decide an attempt this ledger never
// recorded: an attacker able to edit a captured, validly signed delivery could
// otherwise rewrite a replaced ledger's id to the current one and have the
// attempt written (live) or fenced notWritten (a query) even though the old
// ledger may already have delivered it. The SIGNED ts decides: only an attempt
// minted after this ledger identity was, by more than the clock-skew margin,
// can be vouched for.
func TestAnAttemptMintedBeforeThisLedgerIsNeverVouchedFor(t *testing.T) {
	l, _ := newTestFollowUpLedger(t)
	now := time.Now()
	if !l.Own() {
		t.Fatalf("the test ledger was not taken")
	}
	// The ledger identity was minted after this delivery was signed.
	minted := now.Add(10 * time.Minute)
	l.mu.Lock()
	l.createdAtMs = minted.UnixMilli()
	l.mu.Unlock()

	row, write := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), "att-old", "m1", "s1", okCheck(now))
	if write || row.State != followUpRowUnknown {
		t.Fatalf("a pre-ledger attempt naming this ledger: write=%v row=%+v, want unknown and no write", write, row)
	}
	if _, ok := l.rows["att-old"]; ok {
		t.Fatalf("a pre-ledger attempt was recorded")
	}
	if got := l.Query("att-old", l.LedgerID(), now.UnixMilli()); got.State != followUpRowUnknown {
		t.Fatalf("a pre-ledger id must not be fenced notWritten, got %+v", got)
	}
	// A delivery with no ts cannot be placed against the ledger's lifetime.
	if row, write := l.BeginForLedger(l.LedgerID(), 0, "att-nots", "m1", "s1", okCheck(now)); write || row.State != followUpRowUnknown {
		t.Fatalf("an attempt with no signed ts: write=%v row=%+v", write, row)
	}
	// Minted after this ledger by at least the clock-skew margin: no earlier
	// ledger can have written it, whichever way the clocks differ.
	vouched := minted.Add(followUpSkewMargin)
	if _, write := l.BeginForLedger(l.LedgerID(), vouched.UnixMilli(), "att-new", "m1", "s1", okCheck(vouched)); !write {
		t.Fatalf("an attempt minted a skew margin after this ledger must be written")
	}
	// Inside the margin either side of the minting time, a broker clock ahead
	// of the device could date an attempt the previous ledger already wrote
	// at or after this ledger's minting: never vouched for.
	for name, ts := range map[string]time.Time{
		"at the minting time":         minted,
		"just inside the margin":      vouched.Add(-time.Millisecond),
		"inside the margin before it": minted.Add(-followUpSkewMargin),
	} {
		if row, write := l.BeginForLedger(l.LedgerID(), ts.UnixMilli(), "att-skew-"+name, "m1", "s1", okCheck(vouched)); write || row.State != followUpRowUnknown {
			t.Fatalf("an attempt %s: write=%v row=%+v, want unknown and no write", name, write, row)
		}
		if got := l.Query("att-q-"+name, l.LedgerID(), ts.UnixMilli()); got.State != followUpRowUnknown {
			t.Fatalf("an unseen id %s must not be fenced notWritten, got %+v", name, got)
		}
	}
}

// A ledger file written before the identity carried its minting time cannot
// vouch for anything older than the load: the stamp goes on at load, in the
// conservative direction.
func TestALedgerFileWithoutAMintingTimeStampsItAtLoad(t *testing.T) {
	l, path := newTestFollowUpLedger(t)
	before := time.Now().Add(-time.Hour)
	row, _ := l.Begin("a1", "m1", "s1", okCheck(time.Now()))
	l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
	// Rewrite the file in the pre-createdAt shape, keeping the identity.
	l.mu.Lock()
	old := relayFollowUpLedgerFile{Version: relayFollowUpLedgerVersion, LedgerID: l.ledgerID, Rows: []relayFollowUpRow{*l.rows["a1"]}}
	l.mu.Unlock()
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := reopenFollowUpLedger(t, l, path)
	if got := reloaded.Query("a1", reloaded.LedgerID(), before.UnixMilli()); got.State != followUpRowWritten {
		t.Fatalf("a recorded row is answered whatever the ts: %+v", got)
	}
	if got := reloaded.Query("never", reloaded.LedgerID(), before.UnixMilli()); got.State != followUpRowUnknown {
		t.Fatalf("an unseen id older than the load must be unknown, got %+v", got)
	}
	if got := reloaded.Query("never", reloaded.LedgerID(), time.Now().Add(followUpSkewMargin+time.Second).UnixMilli()); got.State != followUpRowRefused {
		t.Fatalf("an unseen id minted a skew margin after the load must be fenced, got %+v", got)
	}
}

// A refused `fu_…` row IS the fence that stops a later copy of the turn — one
// carrying a shortened unsigned `followUpExpiresAt` is refused while the
// original, still inside its signed bound, would otherwise find nothing
// recorded and run a turn the broker has already replaced. The row cap must
// therefore never evict it, and a refusal with nowhere durable to go is never
// answered notWritten.
func TestTheCapNeverEvictsALiveFollowUpRefusalFence(t *testing.T) {
	inbox := newTestRelayInbox(t)
	now := time.Now()
	// A copy whose unsigned expiry was shortened: refused and recorded.
	shortened := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: now.UnixMilli(),
		FollowUpExpiresAt: now.Add(-time.Minute).UnixMilli()}
	if handled, refusal, dispatched := runFencedTestTurn(inbox, shortened, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("shortened copy: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	// Fill the inbox to its cap with accepted rows the cap may evict.
	inbox.mu.Lock()
	for i := 0; i < relayTurnInboxMaxRows; i++ {
		key := relayTurnKey{"s-fill", fmt.Sprintf("t%d", i)}
		inbox.rows[key] = &relayTurnRow{SessionID: key.SessionID, TurnID: key.TurnID,
			State: relayTurnStateAccepted, UpdatedAt: now.UnixMilli()}
	}
	inbox.mu.Unlock()
	// Claiming a new turn evicts to make room, but never the live fence.
	fresh := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a2", SessionID: "s1", Ts: now.UnixMilli()}
	if _, _, dispatched := runFencedTestTurn(inbox, fresh, "fu_a2", nil); !dispatched {
		t.Fatalf("a fresh turn was not dispatched at the cap")
	}
	if r, ok := inbox.snapshot()[relayTurnKey{"s1", "fu_a1"}]; !ok || r.State != relayTurnStateRefused {
		t.Fatalf("the refusal fence was evicted at the cap: %+v", r)
	}
	// The original copy, inside its signed bound, still gets the recorded
	// answer and never runs.
	original := commandMsg{ID: relayTurnEnvelopePrefix + "fu_a1", SessionID: "s1", Ts: now.UnixMilli(),
		FollowUpExpiresAt: now.Add(time.Minute).UnixMilli()}
	if handled, refusal, dispatched := runFencedTestTurn(inbox, original, "fu_a1", nil); !handled || refusal != followUpReasonExpired || dispatched {
		t.Fatalf("original copy: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}

	// Now the inbox holds nothing but live fences: a further refusal cannot be
	// recorded, so it is not answered at all (the broker's deadline decides).
	full := newTestRelayInbox(t)
	// Take ownership before seeding: the first owning call reloads the file
	// and would replace rows planted beforehand.
	if full.snapshot() == nil {
		t.Fatalf("the test inbox was not taken")
	}
	full.mu.Lock()
	for i := 0; i < relayTurnInboxMaxRows; i++ {
		key := relayTurnKey{"s-fenced", fmt.Sprintf("fu_t%d", i)}
		full.rows[key] = &relayTurnRow{SessionID: key.SessionID, TurnID: key.TurnID,
			State: relayTurnStateRefused, Reason: followUpReasonExpired, UpdatedAt: now.UnixMilli(),
			FenceUntil: now.Add(time.Hour).UnixMilli()}
	}
	full.mu.Unlock()
	if handled, refusal, dispatched := runFencedTestTurn(full, shortened, "fu_a1", nil); !handled || refusal != "" || dispatched {
		t.Fatalf("no durable slot: handled=%v refusal=%q dispatched=%v", handled, refusal, dispatched)
	}
	if _, ok := full.snapshot()[relayTurnKey{"s1", "fu_a1"}]; ok {
		t.Fatalf("a refusal with no durable slot was recorded anyway")
	}
}
