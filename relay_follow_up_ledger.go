// File: relay_follow_up_ledger.go
// -----------------------------------------------------------------------------
// Durable follow-up attempt ledger (voice / panel follow-ups, device half;
// VOICE_COMPUTER_RELAY.md "Follow-ups").
//
// A follow-up to a running coding-agent session reaches this device in one of
// two ways:
//
//   - QUEUED: an ordinary relay SEND with turn id `fu_<attemptId>` through the
//     durable turn inbox (relay_turn_inbox.go). On a device that reports
//     relayFollowUps the SEND carries `followUpExpiresAt`; a turn whose expiry
//     (minus followUpSkewMargin) has passed is REFUSED before it can start and
//     answered `relay_turn_ack { outcome: "notWritten", reason: "expired" }`.
//   - LIVE: a signed `relay_follow_up` (envelope `relayfollowup_<attemptId>`)
//     written into the RUNNING CLI as a conversational user message — never on
//     the approval channel. Only a kind certified for live follow-ups at the
//     installed version takes it (liveFollowUpCertified, empty until a
//     certification run passes); every other one answers notWritten.
//
// Every live attempt has exactly one durable row, keyed by attempt id:
//
//   accepted  persisted BEFORE any byte is written to the CLI;
//   written   the stdin write returned successfully;
//   refused   fenced: answered notWritten, and refused forever (also across
//             restarts) — the broker may then deliver the message by a new
//             attempt;
//   unknown   a write error, or a restart that found the row still accepted:
//             bytes may have reached the CLI, so the broker treats it as
//             uncertain, never as delivered or not written.
//
// A repeated attempt id returns its recorded outcome and writes nothing. Rows
// are deleted only when the broker confirms them through `ackedAttemptIds`
// (carried in the signed input of `relay_follow_up_query`); at the cap of
// unconfirmed rows new attempts are refused (ledgerFull). A query for an id
// the ledger never saw is FENCED now (refused) when the broker names this
// ledger's id — rows are kept until confirmed, so absence proves it was never
// seen; a different ledger id (file lost, reinstall) is answered unknown.
//
// Follow-up text is never logged.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Wire values mirrored from shared-constants residentKindsWire.js /
// relayFollowUps.js. Contract-pinned by relay_follow_up_ledger_test.go.
const (
	relayFollowUpCommandType      = "relay_follow_up"
	relayFollowUpQueryCommandType = "relay_follow_up_query"
	relayFollowUpAckResultType    = "relay_follow_up_ack"
	relayFollowUpEnvelopePrefix   = "relayfollowup_"
	relayFollowUpTurnPrefix       = "fu_"

	followUpOutcomeWritten    = "written"
	followUpOutcomeNotWritten = "notWritten"
	followUpOutcomeUnknown    = "unknown"

	followUpReasonExpired              = "expired"
	followUpReasonSessionNotRunning    = "sessionNotRunning"
	followUpReasonConversationMismatch = "conversationMismatch"
	followUpReasonApprovalPending      = "approvalPending"
	followUpReasonNotLiveKind          = "notLiveKind"
	followUpReasonUnseen               = "unseen"
	followUpReasonLedgerFull           = "ledgerFull"

	// CLI_AGENT_IDS.CLAUDE_CODE — the only kind whose stdin stays open between turns.
	claudeCodeCliAgentID = "claudeCode"

	followUpProcessedInCurrent = "current"
	followUpProcessedInNext    = "next"

	// RELAY_FOLLOW_UP_LIMITS.attemptWindowMs / clockSkewMarginMs.
	followUpAttemptWindow = 120 * time.Second
	followUpSkewMargin    = 15 * time.Second
	// RELAY_ACKED_ATTEMPT_IDS_MAX / RELAY_FOLLOW_UP_QUERY_MAX — over-cap is refused, never truncated.
	followUpAckedMax = 32
	followUpQueryMax = 32

	relayFollowUpLedgerFileName = "relay-follow-up-ledger.json"
	relayFollowUpLedgerVersion  = 1
	// Unconfirmed rows never age out; at this cap new attempts are refused.
	relayFollowUpLedgerMaxRows = 512

	followUpRowAccepted = "accepted"
	followUpRowWritten  = "written"
	followUpRowRefused  = "refused"
	followUpRowUnknown  = "unknown"
)

// liveFollowUpCertified lists the CLIs whose installed version passed the
// live follow-up certification (relay_follow_up_claude_certification_test.go)
// with the minimum version it passed at. EMPTY until a run passes: every live
// attempt is then answered notWritten/notLiveKind and the broker queues.
var liveFollowUpCertified = map[string]string{}

type relayFollowUpRow struct {
	AttemptID   string `json:"attemptId"`
	FollowUpID  string `json:"followUpId,omitempty"`
	SessionID   string `json:"sessionID,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	ProcessedIn string `json:"processedIn,omitempty"`
	UpdatedAt   int64  `json:"updatedAt"`
}

type relayFollowUpLedgerFile struct {
	Version  int                `json:"version"`
	LedgerID string             `json:"ledgerId"`
	Rows     []relayFollowUpRow `json:"rows"`
}

type relayFollowUpLedger struct {
	mu       sync.Mutex
	path     func() string
	now      func() time.Time
	loaded   bool
	ledgerID string
	rows     map[string]*relayFollowUpRow
}

func newRelayFollowUpLedger(path func() string) *relayFollowUpLedger {
	return &relayFollowUpLedger{path: path, now: time.Now, rows: map[string]*relayFollowUpRow{}}
}

var globalRelayFollowUpLedger = newRelayFollowUpLedger(func() string {
	return filepath.Join(GetConfigDir(), relayFollowUpLedgerFileName)
})

func newLedgerID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("l%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// loadLocked reads the ledger once. A row still `accepted` was being written
// when the process stopped: the bytes may have reached the CLI, so it becomes
// `unknown` for good.
func (l *relayFollowUpLedger) loadLocked() {
	if l.loaded {
		return
	}
	l.loaded = true
	data, err := os.ReadFile(l.path())
	if err == nil {
		var f relayFollowUpLedgerFile
		if json.Unmarshal(data, &f) == nil && f.LedgerID != "" {
			l.ledgerID = f.LedgerID
			changed := false
			for i := range f.Rows {
				r := f.Rows[i]
				if r.State == followUpRowAccepted {
					r.State = followUpRowUnknown
					changed = true
				}
				l.rows[r.AttemptID] = &r
			}
			if changed {
				_ = l.persistLocked()
			}
			return
		}
	}
	l.ledgerID = newLedgerID()
	_ = l.persistLocked()
}

func (l *relayFollowUpLedger) persistLocked() error {
	f := relayFollowUpLedgerFile{Version: relayFollowUpLedgerVersion, LedgerID: l.ledgerID}
	for _, r := range l.rows {
		f.Rows = append(f.Rows, *r)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeRelayTurnInboxAtomic(l.path(), data)
}

// LedgerID is reported in the relayFollowUps capability.
func (l *relayFollowUpLedger) LedgerID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	return l.ledgerID
}

// outcomeOf answers a row as the broker reads it.
func outcomeOf(r *relayFollowUpRow) string {
	switch r.State {
	case followUpRowWritten:
		return followUpOutcomeWritten
	case followUpRowRefused:
		return followUpOutcomeNotWritten
	default:
		return followUpOutcomeUnknown
	}
}

// followUpAttemptCheck is everything a new live attempt is fenced on, read
// before the row is created. Pure inputs, so the rules are testable.
type followUpAttemptCheck struct {
	Now             time.Time
	ExpiresAtMs     int64
	SignedTsMs      int64
	LiveKind        bool
	SessionRunning  bool
	ConversationOK  bool
	ApprovalPending bool
}

// fenceReason is why a new attempt must be refused, or "" to write it. The
// expiry is the EARLIER of the broker's `expiresAt` and the signed `ts` plus
// the attempt window, so an edited (unsigned) expiresAt cannot extend it.
func (c followUpAttemptCheck) fenceReason() string {
	expires := c.ExpiresAtMs
	if c.SignedTsMs > 0 {
		bound := c.SignedTsMs + followUpAttemptWindow.Milliseconds()
		if expires <= 0 || bound < expires {
			expires = bound
		}
	}
	if expires <= 0 || c.Now.UnixMilli() > expires-followUpSkewMargin.Milliseconds() {
		return followUpReasonExpired
	}
	if !c.LiveKind {
		return followUpReasonNotLiveKind
	}
	if !c.SessionRunning {
		return followUpReasonSessionNotRunning
	}
	if !c.ConversationOK {
		return followUpReasonConversationMismatch
	}
	if c.ApprovalPending {
		return followUpReasonApprovalPending
	}
	return ""
}

// Begin decides a live attempt. It returns the recorded row for a repeated id
// (write=false), a refused row for a fenced one, or an `accepted` row already
// on disk (write=true) — only then may the caller write to the CLI.
func (l *relayFollowUpLedger) Begin(attemptID, followUpID, sessionID string, check followUpAttemptCheck) (relayFollowUpRow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if r := l.rows[attemptID]; r != nil {
		return *r, false
	}
	now := l.now().UnixMilli()
	r := &relayFollowUpRow{AttemptID: attemptID, FollowUpID: followUpID, SessionID: sessionID, UpdatedAt: now}
	if len(l.rows) >= relayFollowUpLedgerMaxRows {
		// Not recorded: there is no room, and nothing was fenced.
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowRefused, Reason: followUpReasonLedgerFull}, false
	}
	if reason := check.fenceReason(); reason != "" {
		r.State = followUpRowRefused
		r.Reason = reason
		l.rows[attemptID] = r
		if err := l.persistLocked(); err != nil {
			fmt.Printf("%s[follow-up] ledger write failed for a refusal: %v%s\n", colorYellow, err, colorReset)
			return notDurableFence(*r), false
		}
		return *r, false
	}
	r.State = followUpRowAccepted
	l.rows[attemptID] = r
	if err := l.persistLocked(); err != nil {
		// Not durable → never written: refuse instead (fenced in memory too).
		r.State = followUpRowRefused
		r.Reason = followUpReasonLedgerFull
		return *r, false
	}
	return *r, true
}

// Finish records the write's outcome: written, or unknown on any write error
// (bytes may have reached the CLI).
func (l *relayFollowUpLedger) Finish(attemptID string, writeErr error, processedIn string) relayFollowUpRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	r := l.rows[attemptID]
	if r == nil {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	if writeErr != nil {
		r.State = followUpRowUnknown
	} else {
		r.State = followUpRowWritten
		r.ProcessedIn = processedIn
	}
	r.UpdatedAt = l.now().UnixMilli()
	_ = l.persistLocked()
	return *r
}

// Query answers one attempt id: its recorded outcome; an id never seen is
// fenced now when the broker names THIS ledger, else it is unknown.
func (l *relayFollowUpLedger) Query(attemptID, brokerLedgerID string) relayFollowUpRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if r := l.rows[attemptID]; r != nil {
		return *r
	}
	if brokerLedgerID == "" || brokerLedgerID != l.ledgerID {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	if len(l.rows) >= relayFollowUpLedgerMaxRows {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	r := &relayFollowUpRow{AttemptID: attemptID, State: followUpRowRefused, Reason: followUpReasonUnseen, UpdatedAt: l.now().UnixMilli()}
	l.rows[attemptID] = r
	if err := l.persistLocked(); err != nil {
		return notDurableFence(*r)
	}
	return *r
}

// notDurableFence is the answer for a fence that did not reach disk. This
// process still refuses the id (the row stays in memory), but after a restart
// the fence is gone and a redelivered envelope could be written. A
// `notWritten` answer would let the broker resend under a new attempt, and
// the person would get the message twice. So the answer is `unknown`: the
// person decides.
func notDurableFence(r relayFollowUpRow) relayFollowUpRow {
	r.State = followUpRowUnknown
	r.Reason = ""
	return r
}

// ConfirmAcked deletes exactly the rows the broker confirmed. An accepted
// row (a write in progress) is kept whatever the watermark says.
func (l *relayFollowUpLedger) ConfirmAcked(attemptIDs []string) (int, error) {
	if len(attemptIDs) > followUpAckedMax {
		return 0, fmt.Errorf("ackedAttemptIds carries %d ids, more than the %d allowed", len(attemptIDs), followUpAckedMax)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	n := 0
	for _, id := range attemptIDs {
		if r := l.rows[id]; r != nil && r.State != followUpRowAccepted {
			delete(l.rows, id)
			n++
		}
	}
	if n > 0 {
		_ = l.persistLocked()
	}
	return n, nil
}

/* --------------------------------------------------------------------------
   Commands (called from pubsub.go handleSessionCommand)
   -------------------------------------------------------------------------- */

// relayFollowUpPayload is the unsigned metadata a live attempt carries; the
// text rides in the signed `input`, the attempt id in the signed envelope id.
type relayFollowUpPayload struct {
	AttemptID  string `json:"attemptId"`
	FollowUpID string `json:"followUpId"`
	ExpiresAt  int64  `json:"expiresAt"`
	LedgerID   string `json:"ledgerId"`
}

// relayFollowUpQuery is the SIGNED input of relay_follow_up_query.
type relayFollowUpQuery struct {
	AttemptIDs      []string `json:"attemptIds"`
	LedgerID        string   `json:"ledgerId"`
	AckedAttemptIDs []string `json:"ackedAttemptIds"`
}

func relayFollowUpAttemptIDFromEnvelope(id string) (string, bool) {
	if !strings.HasPrefix(id, relayFollowUpEnvelopePrefix) || len(id) <= len(relayFollowUpEnvelopePrefix) {
		return "", false
	}
	return id[len(relayFollowUpEnvelopePrefix):], true
}

func relayFollowUpAckFrame(cmd commandMsg, agentID string, r relayFollowUpRow, ledgerID string) resultMsg {
	return resultMsg{
		ID:                  relayFollowUpEnvelopePrefix + r.AttemptID,
		WorkspaceID:         cmd.WorkspaceID,
		UID:                 cmd.UID,
		AgentID:             agentID,
		Status:              "success",
		Ts:                  time.Now().UnixMilli(),
		Version:             Version,
		BootID:              agentBootID,
		Type:                relayFollowUpAckResultType,
		SessionID:           cmd.SessionID,
		FollowUpAttemptID:   r.AttemptID,
		FollowUpID:          r.FollowUpID,
		FollowUpOutcome:     outcomeOf(&r),
		FollowUpReason:      r.Reason,
		FollowUpProcessedIn: r.ProcessedIn,
		FollowUpLedgerID:    ledgerID,
	}
}

// liveFollowUpKind reports whether this session's kind is certified for live
// follow-ups at its installed version. Only Claude Code can ever be: its
// stdin stays open between turns.
func liveFollowUpKind(cliAgentID string) bool {
	_, ok := liveFollowUpCertified[cliAgentID]
	return ok
}

// handleRelayFollowUpCommand answers a live attempt or a reconciliation query.
// It always publishes an answer per attempt; it never interrupts, ends or
// restarts the running turn.
func handleRelayFollowUpCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) {
	agentID := cmd.AgentID
	if agentID == "" && cfg != nil {
		agentID = cfg.AgentID
	}
	ledger := globalRelayFollowUpLedger
	publish := func(r relayFollowUpRow) {
		pubCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := publishMsg(pubCtx, topic, relayFollowUpAckFrame(cmd, agentID, r, ledger.LedgerID())); err != nil {
			fmt.Printf("%s[follow-up] failed to publish the answer for %s: %v%s\n", colorRed, r.AttemptID, err, colorReset)
		}
	}

	if cmd.Type == relayFollowUpQueryCommandType {
		var q relayFollowUpQuery
		if err := json.Unmarshal([]byte(cmd.Input), &q); err != nil || len(q.AttemptIDs) > followUpQueryMax {
			fmt.Printf("%s[follow-up] malformed or over-cap query refused%s\n", colorYellow, colorReset)
			return
		}
		if _, err := ledger.ConfirmAcked(q.AckedAttemptIDs); err != nil {
			fmt.Printf("%s[follow-up] %v%s\n", colorYellow, err, colorReset)
		}
		for _, id := range q.AttemptIDs {
			publish(ledger.Query(id, q.LedgerID))
		}
		return
	}

	attemptID, ok := relayFollowUpAttemptIDFromEnvelope(cmd.ID)
	if !ok {
		return
	}
	p := relayFollowUpPayload{
		AttemptID:  cmd.FollowUpAttemptID,
		FollowUpID: cmd.FollowUpID,
		ExpiresAt:  cmd.FollowUpAttemptExpiresAt,
		LedgerID:   cmd.FollowUpLedgerID,
	}
	if p.AttemptID != "" && p.AttemptID != attemptID {
		// The unsigned metadata must name the signed attempt; anything else is refused.
		publish(relayFollowUpRow{AttemptID: attemptID, State: followUpRowRefused, Reason: followUpReasonConversationMismatch})
		return
	}
	session := (*ClaudeNativeSession)(nil)
	if globalClaudeNativeManager != nil {
		session = globalClaudeNativeManager.Get(cmd.SessionID)
	}
	check := followUpAttemptCheck{
		Now:            time.Now(),
		ExpiresAtMs:    p.ExpiresAt,
		SignedTsMs:     cmd.Ts,
		LiveKind:       session != nil && liveFollowUpKind(claudeCodeCliAgentID),
		SessionRunning: session != nil && session.Status() == "running",
		// A relay session id is bound to one CLI conversation (a resume is a
		// new session id), so the session being this one is the match.
		ConversationOK:  session != nil,
		ApprovalPending: session != nil && session.pendingApprovals.Load() > 0,
	}
	row, write := ledger.Begin(attemptID, p.FollowUpID, cmd.SessionID, check)
	if !write {
		publish(row)
		return
	}
	// Read before the write: a `result` already read means the running turn
	// ended, so the agent takes this message as a new turn.
	processedIn := followUpProcessedInCurrent
	if session.turnSettled.Load() {
		processedIn = followUpProcessedInNext
	}
	err := globalClaudeNativeManager.SendAccepting(cmd.SessionID, cmd.Input, nil)
	publish(ledger.Finish(attemptID, err, processedIn))
}

// followUpTurnExpired reports whether a queued follow-up turn (`fu_…`) must be
// refused before it starts: its expiry (bounded by the signed ts) has passed.
// A SEND without `followUpExpiresAt` (a legacy-device dispatch) never expires.
func followUpTurnExpired(cmd commandMsg, turnID string, now time.Time) bool {
	if !strings.HasPrefix(turnID, relayFollowUpTurnPrefix) || cmd.FollowUpExpiresAt <= 0 {
		return false
	}
	return followUpAttemptCheck{Now: now, ExpiresAtMs: cmd.FollowUpExpiresAt, SignedTsMs: cmd.Ts, LiveKind: true,
		SessionRunning: true, ConversationOK: true}.fenceReason() == followUpReasonExpired
}

// refusedFollowUpTurnAck answers an expired queued follow-up turn: it never ran.
func refusedFollowUpTurnAck(cmd commandMsg, turnID, agentID string) resultMsg {
	frame := relayTurnAckFrame(relayTurnRow{
		SessionID:   cmd.SessionID,
		TurnID:      turnID,
		EnvelopeID:  cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		AgentID:     agentID,
		UID:         cmd.UID,
	})
	frame.FollowUpOutcome = followUpOutcomeNotWritten
	frame.FollowUpReason = followUpReasonExpired
	return frame
}
