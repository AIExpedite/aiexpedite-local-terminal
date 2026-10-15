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
// An attempt that cannot be durably fenced (cap reached, write failed) is
// answered unknown, never notWritten: a later delivery of it could still be
// written, so the broker must not resend it under a new attempt.
//
// Exactly ONE agent process owns the ledger, through an exclusive lock on
// `relay-follow-up-ledger.json.lock` held for that process's life (the turn
// inbox's pattern): overlapping processes would otherwise each load their own
// snapshot and overwrite the other's rows. A non-owner never reads or writes
// the ledger; it hands the delivery back (Nack) so the owner answers it.
//
// Follow-up text is never logged.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Version  int    `json:"version"`
	LedgerID string `json:"ledgerId"`
	// CreatedAt (ms) is when THIS ledger identity was minted. It authenticates
	// the identity that BeginForLedger / Query use as an execution fence: the
	// `ledgerId` a delivery names rides OUTSIDE the command signature, so an
	// attacker able to alter a captured, validly signed delivery could rewrite
	// an old ledger's id to the currently advertised one and turn an answer of
	// `unknown` (never written) into a write — a message the previous ledger
	// may already have delivered. The signed `ts` cannot be rewritten, so an
	// attempt this ledger never recorded may only execute (or be fenced
	// `notWritten`) when it was minted after this ledger was, by more than
	// the clock-skew margin (vouchesForLocked), which no earlier ledger can
	// have recorded.
	CreatedAt int64              `json:"createdAt,omitempty"`
	Rows      []relayFollowUpRow `json:"rows"`
}

type relayFollowUpLedger struct {
	mu       sync.Mutex
	path     func() string
	now      func() time.Time
	loaded   bool
	ledgerID string
	// createdAtMs mirrors relayFollowUpLedgerFile.CreatedAt for the live ledger.
	createdAtMs int64
	rows        map[string]*relayFollowUpRow
	// acquireOwnership takes the ledger's cross-process lock without waiting
	// (nil while another process holds it); lockFile stays held for the life
	// of the process once taken.
	acquireOwnership func() *os.File
	lockFile         *os.File
}

func newRelayFollowUpLedger(path func() string) *relayFollowUpLedger {
	l := &relayFollowUpLedger{path: path, now: time.Now, rows: map[string]*relayFollowUpRow{}}
	l.acquireOwnership = func() *os.File {
		return tryAcquireRelayTurnInboxLock(l.path() + ".lock")
	}
	return l
}

// ownLocked makes this process the ledger's exclusive owner (the first call
// that wins the lock), then loads the file fresh under that lock. It reports
// whether this process owns the ledger; a non-owner never reads or writes it.
func (l *relayFollowUpLedger) ownLocked() bool {
	if l.lockFile != nil {
		return true
	}
	f := l.acquireOwnership()
	if f == nil {
		return false
	}
	l.lockFile = f
	l.loaded = false
	l.rows = map[string]*relayFollowUpRow{}
	l.loadLocked()
	return true
}

// Own reports whether this process owns the ledger, taking it when free.
func (l *relayFollowUpLedger) Own() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ownLocked()
}

// release gives up ownership (tests simulating a restart over the same file).
func (l *relayFollowUpLedger) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lockFile != nil {
		_ = unlockFile(l.lockFile)
		_ = l.lockFile.Close()
		l.lockFile = nil
	}
}

// globalRelayFollowUpLedger is taken only by a process that also owns the
// relay-turn inbox (gateOwnershipOn), so the two process-lifetime locks are
// never split between overlapping agent processes.
var globalRelayFollowUpLedger = newRelayFollowUpLedger(func() string {
	return filepath.Join(GetConfigDir(), relayFollowUpLedgerFileName)
}).gateOwnershipOn(func() bool { return globalRelayTurnInbox.Own() })

// gateOwnershipOn makes the ledger takeable only while owns() reports this
// process owns its companion lock (the relay-turn inbox). Both are held for
// the life of the process once taken, so acquiring the ledger first and then
// failing on the inbox would split them between two overlapping processes
// (Linux has no single-instance guard across an automatic update): neither
// could advertise relayFollowUps, and the ownership watcher would never see
// one process holding both. Lock order is ledger.mu -> inbox.mu; the inbox
// never calls into the ledger.
func (l *relayFollowUpLedger) gateOwnershipOn(owns func() bool) *relayFollowUpLedger {
	acquire := l.acquireOwnership
	l.acquireOwnership = func() *os.File {
		if !owns() {
			return nil
		}
		return acquire()
	}
	return l
}

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
			l.createdAtMs = f.CreatedAt
			changed := false
			if l.createdAtMs <= 0 {
				// A file written before the identity carried its minting
				// time. Stamp it NOW: that is the conservative direction —
				// every attempt minted before this load is answered unknown
				// instead of being written or fenced on an identity this
				// ledger cannot vouch for.
				l.createdAtMs = l.now().UnixMilli()
				changed = true
			}
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
	l.createdAtMs = l.now().UnixMilli()
	_ = l.persistLocked()
}

func (l *relayFollowUpLedger) persistLocked() error {
	f := relayFollowUpLedgerFile{Version: relayFollowUpLedgerVersion, LedgerID: l.ledgerID, CreatedAt: l.createdAtMs}
	for _, r := range l.rows {
		f.Rows = append(f.Rows, *r)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeRelayTurnInboxAtomic(l.path(), data)
}

// LedgerID is reported in the relayFollowUps capability; "" from a non-owner,
// so a query naming it is answered unknown rather than fenced.
func (l *relayFollowUpLedger) LedgerID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return ""
	}
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
	return l.beginLocked(attemptID, followUpID, sessionID, check)
}

// BeginForLedger is Begin for a delivery that names the ledger its broker
// last saw, with the delivery's SIGNED ts. An attempt this ledger never
// recorded, addressed to another ledger (the file was lost, corrupted or
// replaced since) or minted before this ledger existed, may already have been
// written under the old one, so it is answered unknown — like Query — and
// neither recorded nor written.
func (l *relayFollowUpLedger) BeginForLedger(brokerLedgerID string, signedTsMs int64, attemptID, followUpID, sessionID string, check followUpAttemptCheck) (relayFollowUpRow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}, false
	}
	if r := l.rows[attemptID]; r == nil && !l.vouchesForLocked(brokerLedgerID, signedTsMs) {
		return relayFollowUpRow{AttemptID: attemptID, FollowUpID: followUpID, SessionID: sessionID, State: followUpRowUnknown}, false
	}
	return l.beginLocked(attemptID, followUpID, sessionID, check)
}

// vouchesForLocked reports whether THIS ledger can speak for an attempt it has
// no row for: whether its absence proves the attempt never executed.
//
// Two conditions, and the second is the authenticated one:
//   - the delivery names this ledger. `ledgerId` is UNSIGNED, so this alone
//     is only a fail-safe narrowing: rewriting it to the advertised id would
//     otherwise promote an attempt from `unknown` to written (live) or to a
//     `notWritten` fence (a query), either of which can deliver the person's
//     message twice when the previous ledger had already written it.
//   - the delivery was minted after this ledger identity was, by MORE than
//     the clock-skew margin. `ts` is SIGNED (signaturePayload), so it cannot
//     be rewritten without invalidating the HMAC. The broker's clock and this
//     device's differ, so the margin is applied in the conservative
//     direction: an attempt an earlier ledger wrote at device time w was
//     minted no later than w + followUpSkewMargin on the broker's clock (the
//     same bound every follow-up expiry fence already relies on, and the one
//     isCommandStaleAfter enforces on a future-dated ts), and w precedes this
//     ledger's minting, so its ts is below createdAt + margin. Only a ts at
//     or after that point proves no earlier ledger can have seen the attempt.
//     The cost is that attempts minted within the margin of a fresh ledger
//     are answered unknown — rare (a lost or replaced ledger file) and on the
//     safe side.
//
// A delivery with no ts at all cannot be placed against the ledger's lifetime
// and is never vouched for; every `fu_…`/`relayfollowup_` producer signs one.
func (l *relayFollowUpLedger) vouchesForLocked(brokerLedgerID string, signedTsMs int64) bool {
	if brokerLedgerID == "" || brokerLedgerID != l.ledgerID {
		return false
	}
	if signedTsMs <= 0 || l.createdAtMs <= 0 {
		return false
	}
	return signedTsMs >= l.createdAtMs+followUpSkewMargin.Milliseconds()
}

func (l *relayFollowUpLedger) beginLocked(attemptID, followUpID, sessionID string, check followUpAttemptCheck) (relayFollowUpRow, bool) {
	if !l.ownLocked() {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}, false
	}
	if r := l.rows[attemptID]; r != nil {
		return *r, false
	}
	now := l.now().UnixMilli()
	r := &relayFollowUpRow{AttemptID: attemptID, FollowUpID: followUpID, SessionID: sessionID, UpdatedAt: now}
	if len(l.rows) >= relayFollowUpLedgerMaxRows {
		// Not recorded: there is no room, so nothing is fenced and a later
		// delivery of this id could still be written — unknown, not notWritten.
		fmt.Printf("%s[follow-up] ledger full: attempt %s answered unknown%s\n", colorYellow, attemptID, colorReset)
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown, Reason: followUpReasonLedgerFull}, false
	}
	if reason := check.fenceReason(); reason != "" {
		r.State = followUpRowRefused
		r.Reason = reason
		l.rows[attemptID] = r
		if err := l.persistLocked(); err != nil {
			fmt.Printf("%s[follow-up] ledger write failed for a refusal: %v%s\n", colorYellow, err, colorReset)
			return l.markNotDurableLocked(r), false
		}
		return *r, false
	}
	r.State = followUpRowAccepted
	l.rows[attemptID] = r
	if err := l.persistLocked(); err != nil {
		// Not durable → never written, and the fence would live in memory
		// only: keep the stored row unknown too, so a redelivery in this
		// process is answered uncertain rather than notWritten.
		return l.markNotDurableLocked(r), false
	}
	return *r, true
}

// RefuseAccepted fences an `accepted` attempt that the stdin boundary refused
// before writing a byte (refuseAtStdin): the row becomes `refused` — answered
// notWritten, and refused for good like any fence. A row in any other state is
// returned unchanged. A fence that does not reach disk is downgraded to
// unknown (markNotDurableLocked).
func (l *relayFollowUpLedger) RefuseAccepted(attemptID, reason string) relayFollowUpRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	r := l.rows[attemptID]
	if r == nil {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	if r.State != followUpRowAccepted {
		return *r
	}
	r.State = followUpRowRefused
	r.Reason = reason
	r.UpdatedAt = l.now().UnixMilli()
	if err := l.persistLocked(); err != nil {
		fmt.Printf("%s[follow-up] ledger write failed for a refusal: %v%s\n", colorYellow, err, colorReset)
		return l.markNotDurableLocked(r)
	}
	return *r
}

// Finish records the write's outcome: written, or unknown on any write error
// (bytes may have reached the CLI).
func (l *relayFollowUpLedger) Finish(attemptID string, writeErr error, processedIn string) relayFollowUpRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
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
// fenced now when this ledger can vouch for its absence (vouchesForLocked),
// else it is unknown.
func (l *relayFollowUpLedger) Query(attemptID, brokerLedgerID string, signedTsMs int64) relayFollowUpRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	if r := l.rows[attemptID]; r != nil {
		return *r
	}
	if !l.vouchesForLocked(brokerLedgerID, signedTsMs) {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	if len(l.rows) >= relayFollowUpLedgerMaxRows {
		return relayFollowUpRow{AttemptID: attemptID, State: followUpRowUnknown}
	}
	r := &relayFollowUpRow{AttemptID: attemptID, State: followUpRowRefused, Reason: followUpReasonUnseen, UpdatedAt: l.now().UnixMilli()}
	l.rows[attemptID] = r
	if err := l.persistLocked(); err != nil {
		return l.markNotDurableLocked(r)
	}
	return *r
}

// markNotDurableLocked downgrades a fence that did not reach disk, in the
// stored row as well as in the answer. The fence would otherwise live in
// memory only: after a restart it is gone and a redelivered envelope could be
// written. A `notWritten` answer would let the broker resend under a new
// attempt, and the person would get the message twice — so every answer for
// this id, now and after a redelivery, is `unknown`: the person decides.
func (l *relayFollowUpLedger) markNotDurableLocked(r *relayFollowUpRow) relayFollowUpRow {
	r.State = followUpRowUnknown
	r.Reason = ""
	r.UpdatedAt = l.now().UnixMilli()
	return *r
}

// ConfirmAcked deletes exactly the rows the broker confirmed. An accepted
// row (a write in progress) is kept whatever the watermark says.
func (l *relayFollowUpLedger) ConfirmAcked(attemptIDs []string) (int, error) {
	if len(attemptIDs) > followUpAckedMax {
		return 0, fmt.Errorf("ackedAttemptIds carries %d ids, more than the %d allowed", len(attemptIDs), followUpAckedMax)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownLocked() {
		return 0, fmt.Errorf("another agent process owns the follow-up ledger")
	}
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
//
// Membership in liveFollowUpCertified is not on its own certification: the
// run passes against ONE version, and an older build of the same CLI was
// never exercised. So the installed version is compared against the recorded
// minimum, and anything that cannot be established — an unparseable minimum,
// an unreadable or unparseable installed version — fails closed to queued
// delivery (Codex P2 on #219).
func liveFollowUpKind(cliAgentID string) bool {
	return liveFollowUpVersionCertified(cliAgentID, liveFollowUpInstalledVersion(cliAgentID))
}

// liveFollowUpVersionCertified reports whether version (major.minor.patch)
// meets the certified minimum for cliAgentID. An empty version, or an
// unparseable minimum, fails closed.
func liveFollowUpVersionCertified(cliAgentID, version string) bool {
	minVersion, ok := liveFollowUpCertified[cliAgentID]
	if !ok || semverRe.FindStringSubmatch(minVersion) == nil {
		return false
	}
	return version != "" && compareSemver(version, minVersion) >= 0
}

// claudeLaunchVersion reads the version of the Claude binary a session is
// about to spawn, plus the (path, mtime, size) identity it was read from, so
// confirmClaudeLaunchVersion can check that exact binary is what started. It
// probes only while Claude Code is certified for live follow-ups at all, so an
// uncertified build never pays a `--version` spawn per session start.
func claudeLaunchVersion(executable string) (string, versionProbeKey) {
	if _, ok := liveFollowUpCertified[claudeCodeCliAgentID]; !ok || executable == "" {
		return "", versionProbeKey{}
	}
	cachedProbeVersion(executable)
	v, key, ok := peekCachedProbeVersionIdentity(executable)
	m := semverRe.FindStringSubmatch(v)
	if !ok || m == nil {
		return "", versionProbeKey{}
	}
	return m[0], key
}

// confirmClaudeLaunchVersion keeps the version claudeLaunchVersion read only
// when the binary on disk still has the identity it was read from once the
// child has started. An upgrade landing between the probe and the spawn makes
// the launched build unknown, which fails closed to queued delivery.
func confirmClaudeLaunchVersion(executable, version string, key versionProbeKey) string {
	if version == "" {
		return ""
	}
	info, err := os.Stat(executable)
	if err != nil || key != (versionProbeKey{Path: executable, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}) {
		return ""
	}
	return version
}

// liveFollowUpInstalledVersion is the installed major.minor.patch of a
// certified CLI, or "" when it cannot be read. A var so the ledger tests and
// the certification run can pin a version instead of spawning the real CLI.
// The probe is the shared (path, mtime, size) cache, so an accepted attempt
// spawns `--version` only after the binary actually changed.
var liveFollowUpInstalledVersion = func(cliAgentID string) string {
	if cliAgentID != claudeCodeCliAgentID {
		return ""
	}
	path := resolveClaudePath()
	if path == "" {
		return ""
	}
	m := semverRe.FindStringSubmatch(cachedProbeVersion(path))
	if m == nil {
		return ""
	}
	return m[0]
}

// handleRelayFollowUpCommand answers a live attempt or a reconciliation query.
// It always publishes an answer per attempt; it never interrupts, ends or
// restarts the running turn. It returns false only when another agent process
// owns the ledger: the caller must Nack so Pub/Sub hands it to the owner.
func handleRelayFollowUpCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) bool {
	ledger := globalRelayFollowUpLedger
	if !ledger.Own() {
		fmt.Printf("%s[follow-up] Another agent process owns the follow-up ledger — handing %s back%s\n", colorYellow, cmd.ID, colorReset)
		return false
	}
	agentID := cmd.AgentID
	if agentID == "" && cfg != nil {
		agentID = cfg.AgentID
	}
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
			return true
		}
		if _, err := ledger.ConfirmAcked(q.AckedAttemptIDs); err != nil {
			fmt.Printf("%s[follow-up] %v%s\n", colorYellow, err, colorReset)
		}
		for _, id := range q.AttemptIDs {
			publish(ledger.Query(id, q.LedgerID, cmd.Ts))
		}
		return true
	}

	attemptID, ok := relayFollowUpAttemptIDFromEnvelope(cmd.ID)
	if !ok {
		return true
	}
	p := relayFollowUpPayload{
		AttemptID:  cmd.FollowUpAttemptID,
		FollowUpID: cmd.FollowUpID,
		ExpiresAt:  cmd.FollowUpAttemptExpiresAt,
		LedgerID:   cmd.FollowUpLedgerID,
	}
	// The unsigned metadata must name the signed attempt. A mismatch is
	// refused through the ledger like any fence, so the attempt id stays
	// refused: a later, genuine delivery of it is never written after the
	// broker was told it was not.
	metadataMatches := p.AttemptID == "" || p.AttemptID == attemptID
	session := (*ClaudeNativeSession)(nil)
	if globalClaudeNativeManager != nil {
		session = globalClaudeNativeManager.Get(cmd.SessionID)
	}
	check := followUpAttemptCheck{
		Now:         time.Now(),
		ExpiresAtMs: p.ExpiresAt,
		SignedTsMs:  cmd.Ts,
		// Gated on the version the running child was LAUNCHED from, not the
		// one installed now: an in-place upgrade must not make a session
		// started from an older, uncertified build live-eligible (Codex P2
		// on #219).
		LiveKind:    session != nil && liveFollowUpVersionCertified(claudeCodeCliAgentID, session.launchedVersion),
		// A fenced session (moved or max-parked off this computer) takes
		// nothing but its END, so it is not running for a follow-up.
		SessionRunning: session != nil && session.Status() == "running" && !isSessionFenced(cmd.SessionID),
		// A relay session id is bound to one CLI conversation (a resume is a
		// new session id), so the session being this one is the match.
		ConversationOK:  session != nil && metadataMatches,
		ApprovalPending: session != nil && session.pendingApprovals.Load() > 0,
	}
	row, write := ledger.BeginForLedger(p.LedgerID, cmd.Ts, attemptID, p.FollowUpID, cmd.SessionID, check)
	if !write {
		publish(row)
		return true
	}
	// The fence is decided again at the stdin boundary, under stdinMu right
	// before the write: SendAccepting can wait there behind another stdin
	// write (up to its write timeout), and the check above may pass up to the
	// skew margin before the broker's expiry, so without this the message
	// could be written after the broker expired the attempt and queued a
	// replacement. processedIn is read there too: a `result` already read
	// means the running turn ended, so the agent takes this message as a new
	// turn.
	processedIn := followUpProcessedInCurrent
	var refusedAtStdin *relayFollowUpRow
	err := globalClaudeNativeManager.SendAccepting(cmd.SessionID, cmd.Input, func() error {
		recheck := check
		recheck.Now = time.Now()
		recheck.ApprovalPending = session.pendingApprovals.Load() > 0
		// The session's own state is re-read here too, not carried over from
		// the pre-lock snapshot: /online records a fenced id BEFORE it ends
		// the session asynchronously, so a session fenced (moved or
		// max-parked) off this computer while this write waited on stdinMu
		// would otherwise still be written to (Codex P2 on #219). Both reads
		// are leaf locks — the session's own mutex, already taken under
		// stdinMu by the writer, and the fenced-id set — so neither can
		// deadlock against the manager here.
		recheck.SessionRunning = session.Status() == "running" && !isSessionFenced(cmd.SessionID)
		if reason := recheck.fenceReason(); reason != "" {
			row := ledger.RefuseAccepted(attemptID, reason)
			refusedAtStdin = &row
			return errFollowUpRefusedAtStdin
		}
		if session.turnSettled.Load() {
			processedIn = followUpProcessedInNext
		}
		return nil
	})
	if refusedAtStdin != nil {
		publish(*refusedAtStdin)
		return true
	}
	publish(ledger.Finish(attemptID, err, processedIn))
	return true
}

// errFollowUpRefusedAtStdin aborts a live write whose fence failed at the
// stdin boundary; nothing was written, and the ledger already holds the fence.
var errFollowUpRefusedAtStdin = errors.New("follow-up attempt refused at the stdin boundary")

// followUpTurnExpired reports whether a queued follow-up turn (`fu_…`) must be
// refused before it starts: its expiry (bounded by the signed ts) has passed.
//
// `followUpExpiresAt` rides OUTSIDE the signature, so a missing or stripped
// value must not lift the deadline — a `fu_…` SEND is admitted for the whole
// attempt window plus the skew margin (commandMaxAgeSec), which outlives the
// broker's own expiry, so an unfenced delivery could run a message the broker
// has already expired and replaced. When the field is absent the deadline
// therefore comes from the SIGNED ts plus the attempt window, the same bound
// fenceReason applies. Only a command with no timestamp at all (the
// backwards-compatible case isCommandStaleAfter also admits) has no deadline
// to apply; a `fu_…` turn id is only ever minted by a follow-up-aware broker,
// which always signs a ts.
func followUpTurnExpired(cmd commandMsg, turnID string, now time.Time) bool {
	if !strings.HasPrefix(turnID, relayFollowUpTurnPrefix) {
		return false
	}
	if cmd.FollowUpExpiresAt <= 0 && cmd.Ts <= 0 {
		return false
	}
	return followUpAttemptCheck{Now: now, ExpiresAtMs: cmd.FollowUpExpiresAt, SignedTsMs: cmd.Ts, LiveKind: true,
		SessionRunning: true, ConversationOK: true}.fenceReason() == followUpReasonExpired
}

// followUpTurnAcceptRefusal is the part of followUpTurnRefusal decided AGAIN
// at the acceptance boundary, or "" to accept. A manager can wait on a
// session lock between the claim and acceptance, so both of these can land in
// between: the attempt's expiry passes (the broker may already have queued a
// replacement), or the session is fenced off this computer — /online records a
// fenced id BEFORE it ends the session asynchronously, so a dispatch can
// otherwise win the session lock and write into a CLI that has already moved
// (Codex P2 on #219).
//
// Only checks that take NO session-manager lock belong here, because the
// manager calls the acceptor while holding its own: the expiry is pure, and
// isSessionFenced takes only the fenced-id set's own leaf lock.
func followUpTurnAcceptRefusal(cmd commandMsg, turnID string, now time.Time) string {
	if !strings.HasPrefix(turnID, relayFollowUpTurnPrefix) {
		return ""
	}
	if followUpTurnExpired(cmd, turnID, now) {
		return followUpReasonExpired
	}
	if isSessionFenced(cmd.SessionID) {
		return followUpReasonSessionNotRunning
	}
	return ""
}

// followUpTurnRefusal is why a queued follow-up turn (`fu_…`) that is neither
// accepted nor in flight must not start, or "" to run it: its expiry passed
// (followUpTurnExpired), its session was fenced off this computer (moved or
// max-parked), whose CLI must take nothing more but its END, or the session
// is gone from this device altogether. It takes session-manager locks, so
// runFencedRelayTurn reads it before the claim, never under the inbox lock.
func followUpTurnRefusal(cmd commandMsg, turnID string, now time.Time) string {
	if !strings.HasPrefix(turnID, relayFollowUpTurnPrefix) {
		return ""
	}
	if followUpTurnExpired(cmd, turnID, now) {
		return followUpReasonExpired
	}
	if isSessionFenced(cmd.SessionID) || followUpTurnTargetGone(cmd) {
		return followUpReasonSessionNotRunning
	}
	return ""
}

// followUpTurnTargetGone reports that this device DEFINITELY no longer holds
// the session a queued follow-up turn names — it ended normally rather than
// being fenced. Dispatching it anyway would fail inside the kind's SEND, which
// publishes a kind-specific error frame without ever invoking the acceptance
// callback, so the broker would get neither an acceptance nor an
// outcome-bearing `relay_turn_ack` and could not settle or requeue the
// person's message until its own deadline. Refusing here answers
// notWritten/sessionNotRunning instead, and the broker requeues at once.
//
// A session the manager still retains after its child exited (status "ended":
// waitForExit flips it before artifact collection and the terminal frame, and
// a generic session can linger as a tombstone) is gone too, because every
// kind's SEND refuses an ended session before invoking the callback.
//
// It only ever reports a session the owning manager is live and certain about.
// An unrecognised command type, an uninitialised manager, or a session the
// manager still holds in any other state (including one that is merely
// starting or idle) reads as present, so a session that can still accept the
// turn is never raced.
func followUpTurnTargetGone(cmd commandMsg) bool {
	if cmd.SessionID == "" {
		return false
	}
	switch {
	case isClaudeNativeCommand(cmd.Type):
		return globalClaudeNativeManager != nil && managedSessionGone(globalClaudeNativeManager.Get(cmd.SessionID))
	case isCodexAppServerCommand(cmd.Type):
		return globalCodexAppServerManager != nil && managedSessionGone(globalCodexAppServerManager.Get(cmd.SessionID))
	case isGrokACPCommand(cmd.Type):
		return globalGrokACPManager != nil && managedSessionGone(globalGrokACPManager.Get(cmd.SessionID))
	case isAntigravityNativeCommand(cmd.Type):
		return globalAntigravityNativeManager != nil && managedSessionGone(globalAntigravityNativeManager.Get(cmd.SessionID))
	case isOpenCodeNativeCommand(cmd.Type):
		return globalOpenCodeNativeManager != nil && managedSessionGone(globalOpenCodeNativeManager.Get(cmd.SessionID))
	case isMuseCodeNativeCommand(cmd.Type):
		return globalMuseCodeNativeManager != nil && managedSessionGone(globalMuseCodeNativeManager.Get(cmd.SessionID))
	case cmd.Type == "session_input":
		return globalSessionManager != nil && cliSessionGone(globalSessionManager.GetSession(cmd.SessionID))
	default:
		return false
	}
}

// managedSessionGone reports that a kind manager's Get found no session, or a
// retained one whose child has exited.
func managedSessionGone[T any, S interface {
	*T
	Status() string
}](session S) bool {
	return session == nil || session.Status() == "ended"
}

// cliSessionGone is managedSessionGone for a generic managed session, whose
// status is guarded by its own mutex.
func cliSessionGone(session *CLISession) bool {
	if session == nil {
		return true
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.Status == "ended"
}

// isFollowUpProtocolCommand reports whether a command belongs to the follow-up
// protocol: a live attempt, a reconciliation query, or a queued follow-up turn
// (`relayturn_fu_…`). These carry the 120 s attempt window rather than the
// generic 60 s command-staleness window.
func isFollowUpProtocolCommand(cmd commandMsg) bool {
	if cmd.Type == relayFollowUpCommandType || cmd.Type == relayFollowUpQueryCommandType {
		return true
	}
	turnID, ok := relayTurnIDFromEnvelope(cmd.ID)
	return ok && strings.HasPrefix(turnID, relayFollowUpTurnPrefix)
}

// refusedFollowUpTurnAck answers a refused queued follow-up turn (expired, or
// its session fenced): it never ran.
func refusedFollowUpTurnAck(cmd commandMsg, turnID, agentID, reason string) resultMsg {
	frame := relayTurnAckFrame(relayTurnRow{
		SessionID:   cmd.SessionID,
		TurnID:      turnID,
		EnvelopeID:  cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		AgentID:     agentID,
		UID:         cmd.UID,
	})
	frame.FollowUpOutcome = followUpOutcomeNotWritten
	frame.FollowUpReason = reason
	return frame
}
