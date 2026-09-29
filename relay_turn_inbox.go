// File: relay_turn_inbox.go
// -----------------------------------------------------------------------------
// Durable relay turn inbox (voice → coding-agent relay, device half).
//
// A voice relay hands a user turn to a CLI session on this computer through
// terminal-service's relay outbox. The outbox publishes the turn as an
// ordinary signed session SEND whose envelope id is `relayturn_<turnId>`
// (shared-constants RELAY_TURN_ENVELOPE_PREFIX / relayTurnEnvelopeId), and it
// REPUBLISHES that SEND until it sees this device's `relay_turn_ack` frame. On
// top of that, Pub/Sub itself is at-least-once. So the same turn can reach
// this device several times, and a coding agent that is told the same thing
// twice does the work twice (two commits, two PRs, two deploys).
//
// The inbox makes a relay turn run AT MOST ONCE AFTER IT WAS ACCEPTED, and
// never drops one:
//
//   - claimed  — recorded (durably) before the command is dispatched to its
//     kind's handler. A redelivery that finds a claimed row which is NOT in
//     flight in this process re-dispatches: a claimed row is only left behind
//     by a crash before acceptance, or by a handler that refused the turn
//     (session not found, input rejected) — in both cases the CLI provably
//     never started the turn, so running it now is the first run, not a second.
//   - accepted — recorded (durably) at the handler's OWNERSHIP BOUNDARY, after
//     every refusal check and BEFORE the turn can start: for a persistent
//     session (Claude, Codex, Grok, the generic PTY session) under the stdin
//     lock immediately before the SEND is written to the CLI; for a one-shot
//     kind (Antigravity, OpenCode, Muse Code) right after its session moved
//     idle→running for this turn, before anything is spawned. Only once the
//     row is on disk is the ack frame published. If the row cannot be
//     written, the handler refuses the turn (it stays claimed and unacked,
//     and the broker republishes) — an acceptance that is not durable is
//     never acked. A redelivery that finds an accepted row re-publishes the
//     ack and never re-runs. Recording before delivery means a crash in the
//     gap between the two can DROP that turn; it can never run it twice, and
//     the voice side then settles it failed, never succeeded.
//
// An accepted row is deliberately NOT deleted when its ack is published. The
// ack can be lost between this device and the broker (publish failed, broker
// crashed before recording it); the broker then republishes the SEND, and the
// retained row is the only thing that turns that redelivery into a re-ack
// instead of a second run. A row goes away only when:
//
//   - a SIGNED `ackedTurnIds` watermark on a later command for the same
//     session names it — the broker proving it recorded the ack (see
//     pubsub.go, applyRelayAckWatermark). Exactly the named rows are deleted;
//     an unsigned or edited watermark fails the command's HMAC and deletes
//     nothing;
//   - its session is torn down on this device (releaseLedgerSession — the one
//     point every manager passes when it removes a session): a turn for a
//     session that no longer exists cannot run again anyway;
//   - it outlives relayTurnInboxTTL — far longer than any outbox republish
//     window, so expiry can only drop a row nothing will redeliver.
//
// Rows are keyed (sessionID, turnId): a turn id is unique within its relay,
// and the session id scopes it so a watermark on one session can never delete
// another session's row.
//
// The file lives in the config dir beside the spawn ledger and is rewritten
// atomically (temp file + fsync + rename) on every state change, so a crash
// leaves the old or the new inbox, never a torn one. It is never an in-memory
// dedup: the redelivery that matters most is the one that arrives after the
// agent restarted.
//
// Exactly ONE agent process owns the inbox, through an exclusive lock on
// `relay-turn-inbox.json.lock` held for that process's life (the spawn
// ledger's pattern). Two agents on one config dir (Linux has no
// single-instance guard) would otherwise each keep their own rows and
// in-flight set, both run the same turn, and overwrite each other's file. A
// process that does not own the inbox hands every relay turn back to Pub/Sub
// (Nack) untouched so the owner runs it, and retries ownership on the next
// delivery in case the owner has exited.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Wire values mirrored from shared-constants residentKindsWire.js. Go cannot
// import the JS package; these are contract-pinned by
// pubsub_ack_watermark_test.go / pubsub_turn_dedup_test.go and by the Node
// signer's own tests. Change both sides or neither.
const (
	// RELAY_TURN_ENVELOPE_PREFIX
	relayTurnEnvelopePrefix = "relayturn_"
	// RELAY_ACK_WATERMARK_MAX — over-cap is REFUSED, never truncated.
	relayAckWatermarkMax = 32
	// RELAY_TURN_ACK_RESULT_TYPE
	relayTurnAckResultType = "relay_turn_ack"
)

const (
	relayTurnInboxFileName = "relay-turn-inbox.json"
	relayTurnInboxVersion  = 1
	// relayTurnInboxTTL is conservative on purpose: expiring a row early is
	// the one way the inbox could let a redelivery run twice.
	relayTurnInboxTTL = 24 * time.Hour
	// relayTurnInboxMaxRows bounds the file. The broker admits one open turn
	// per relay and the watermark confirms rows continuously, so a live inbox
	// holds a handful of rows; the cap only matters for a broker that never
	// sends watermarks (an older terminal-service) on a very busy device.
	relayTurnInboxMaxRows = 2048

	relayTurnStateClaimed  = "claimed"
	relayTurnStateAccepted = "accepted"

	// relayAcceptanceNotRecorded prefixes the error a manager returns when it
	// refused a relay turn because its acceptance could not be made durable.
	// The one-shot handlers match it to publish the error frame themselves.
	relayAcceptanceNotRecorded = "relay turn acceptance could not be recorded"
)

// relayTurnIDFromEnvelope returns the relay turnId carried by a session
// command's envelope id (mirrors shared-constants relayTurnIdFromEnvelope:
// the prefix must be followed by at least one character).
func relayTurnIDFromEnvelope(id string) (string, bool) {
	if !strings.HasPrefix(id, relayTurnEnvelopePrefix) || len(id) <= len(relayTurnEnvelopePrefix) {
		return "", false
	}
	return id[len(relayTurnEnvelopePrefix):], true
}

type relayTurnKey struct {
	SessionID string
	TurnID    string
}

// relayTurnRow is one persisted inbox row. WorkspaceID / AgentID / UID /
// EnvelopeID are what a re-ack needs to rebuild the frame after a restart.
type relayTurnRow struct {
	SessionID   string `json:"sessionID"`
	TurnID      string `json:"turnId"`
	State       string `json:"state"`
	EnvelopeID  string `json:"envelopeId,omitempty"`
	WorkspaceID string `json:"workspaceID,omitempty"`
	AgentID     string `json:"agentId,omitempty"`
	UID         string `json:"uid,omitempty"`
	ClaimedAt   int64  `json:"claimedAt"`
	AcceptedAt  int64  `json:"acceptedAt,omitempty"`
	UpdatedAt   int64  `json:"updatedAt"`
}

type relayTurnInboxFile struct {
	Version int             `json:"version"`
	Rows    []*relayTurnRow `json:"rows"`
}

// relayTurnDecision is what Begin tells the dispatcher to do.
type relayTurnDecision int

const (
	// relayTurnRun: the row is (now) claimed and this delivery owns it —
	// dispatch, then Finish.
	relayTurnRun relayTurnDecision = iota
	// relayTurnReack: the turn was already accepted — re-publish the ack,
	// never re-run.
	relayTurnReack
	// relayTurnInFlight: another delivery of the same turn is being
	// dispatched by this process right now and will ack when it accepts.
	relayTurnInFlight
	// relayTurnNotOwner: another agent process owns the inbox — hand the
	// delivery back (Nack) untouched so the owner runs it.
	relayTurnNotOwner
)

type relayTurnInbox struct {
	mu        sync.Mutex
	path      func() string
	loaded    bool
	rows      map[relayTurnKey]*relayTurnRow
	inFlight  map[relayTurnKey]bool
	now       func() time.Time
	writeFile func(path string, data []byte) error
	// acquireOwnership takes the inbox's cross-process lock without waiting
	// (nil while another process holds it); lockFile stays held for the life
	// of the process once taken.
	acquireOwnership func() *os.File
	lockFile         *os.File
}

func newRelayTurnInbox(path func() string) *relayTurnInbox {
	b := &relayTurnInbox{
		path:      path,
		rows:      make(map[relayTurnKey]*relayTurnRow),
		inFlight:  make(map[relayTurnKey]bool),
		now:       time.Now,
		writeFile: writeRelayTurnInboxAtomic,
	}
	b.acquireOwnership = func() *os.File {
		return tryAcquireRelayTurnInboxLock(b.path() + ".lock")
	}
	return b
}

// tryAcquireRelayTurnInboxLock takes the inbox lock file exclusively, or
// returns nil at once when another agent process holds it. It never waits:
// the caller hands the delivery back to Pub/Sub, which is its own retry.
func tryAcquireRelayTurnInboxLock(path string) *os.File {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Printf("%s[relay-inbox] Cannot create the inbox lock dir: %v%s\n", colorYellow, err, colorReset)
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Printf("%s[relay-inbox] Cannot open the inbox lock: %v%s\n", colorYellow, err, colorReset)
		return nil
	}
	if ok, _ := tryLockFileExclusive(f); !ok {
		_ = f.Close()
		return nil
	}
	return f
}

// ownLocked makes this process the inbox's exclusive owner (the first call
// that wins the lock), then loads the file fresh under that lock. It reports
// whether this process owns the inbox; a non-owner never reads or writes it.
func (b *relayTurnInbox) ownLocked() bool {
	if b.lockFile != nil {
		return true
	}
	f := b.acquireOwnership()
	if f == nil {
		return false
	}
	b.lockFile = f
	b.loaded = false
	b.rows = make(map[relayTurnKey]*relayTurnRow)
	b.loadLocked()
	return true
}

// release gives up ownership (tests simulating a restart over the same file).
func (b *relayTurnInbox) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lockFile != nil {
		_ = unlockFile(b.lockFile)
		_ = b.lockFile.Close()
		b.lockFile = nil
	}
}

// globalRelayTurnInbox is the agent's inbox, in the config dir.
var globalRelayTurnInbox = newRelayTurnInbox(func() string {
	return filepath.Join(GetConfigDir(), relayTurnInboxFileName)
})

// writeRelayTurnInboxAtomic writes via the spawn ledger's temp+fsync+rename
// helper (same directory, same crash guarantees).
func writeRelayTurnInboxAtomic(path string, data []byte) error {
	return writeLedgerFileAtomic(path, data)
}

// loadLocked reads the file once. A missing file starts empty; an unreadable
// one is logged and starts empty — the inbox cannot then dedup turns from
// before the corruption, which is no worse than an agent without an inbox,
// and the outbox's own publish state still bounds redelivery.
func (b *relayTurnInbox) loadLocked() {
	if b.loaded {
		return
	}
	b.loaded = true
	raw, err := os.ReadFile(b.path())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("%s[relay-inbox] Could not read the relay turn inbox: %v%s\n", colorYellow, err, colorReset)
		}
		return
	}
	var data relayTurnInboxFile
	if err := json.Unmarshal(raw, &data); err != nil {
		fmt.Printf("%s[relay-inbox] Relay turn inbox unreadable, starting empty: %v%s\n", colorYellow, err, colorReset)
		return
	}
	for _, r := range data.Rows {
		if r == nil || r.SessionID == "" || r.TurnID == "" {
			continue
		}
		if r.State != relayTurnStateClaimed && r.State != relayTurnStateAccepted {
			continue
		}
		b.rows[relayTurnKey{r.SessionID, r.TurnID}] = r
	}
	if b.pruneExpiredLocked() {
		_ = b.persistLocked()
	}
}

// persistLocked mirrors the inbox to disk and returns (and logs) a failure.
// Only Accept treats a failure as fatal — an acceptance that never reached
// the disk must not be acked. Every other write is best-effort: the
// in-memory rows still dedup every redelivery this process sees.
func (b *relayTurnInbox) persistLocked() error {
	rows := make([]*relayTurnRow, 0, len(b.rows))
	for _, r := range b.rows {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].UpdatedAt != rows[j].UpdatedAt {
			return rows[i].UpdatedAt < rows[j].UpdatedAt
		}
		if rows[i].SessionID != rows[j].SessionID {
			return rows[i].SessionID < rows[j].SessionID
		}
		return rows[i].TurnID < rows[j].TurnID
	})
	raw, err := json.Marshal(relayTurnInboxFile{Version: relayTurnInboxVersion, Rows: rows})
	if err != nil {
		fmt.Printf("%s[relay-inbox] Could not encode the relay turn inbox: %v%s\n", colorRed, err, colorReset)
		return err
	}
	if err := b.writeFile(b.path(), raw); err != nil {
		fmt.Printf("%s[relay-inbox] Could not write the relay turn inbox: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}

// pruneExpiredLocked drops rows past the TTL (never one in flight) and
// reports whether anything changed.
func (b *relayTurnInbox) pruneExpiredLocked() bool {
	cutoff := b.now().Add(-relayTurnInboxTTL).UnixMilli()
	changed := false
	for k, r := range b.rows {
		if r.UpdatedAt < cutoff && !b.inFlight[k] {
			delete(b.rows, k)
			changed = true
		}
	}
	return changed
}

// evictForCapLocked makes room for one more row: claimed rows first (a
// claimed row that is not in flight holds a turn that never started, so
// losing it can at worst let a redelivery run it — once), then the oldest
// accepted rows.
func (b *relayTurnInbox) evictForCapLocked() {
	for len(b.rows) >= relayTurnInboxMaxRows {
		var victim *relayTurnRow
		for k, r := range b.rows {
			if b.inFlight[k] {
				continue
			}
			if victim == nil ||
				(r.State == relayTurnStateClaimed && victim.State != relayTurnStateClaimed) ||
				(r.State == victim.State && r.UpdatedAt < victim.UpdatedAt) {
				victim = r
			}
		}
		if victim == nil {
			return
		}
		fmt.Printf("%s[relay-inbox] Inbox full — evicting %s row %s/%s%s\n",
			colorYellow, victim.State, victim.SessionID, victim.TurnID, colorReset)
		delete(b.rows, relayTurnKey{victim.SessionID, victim.TurnID})
	}
}

// Begin records a delivery of a relay turn and says what to do with it. On
// relayTurnRun the caller MUST call Finish once dispatch returns.
func (b *relayTurnInbox) Begin(cmd commandMsg, turnID, agentID string) (relayTurnDecision, relayTurnRow) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ownLocked() {
		return relayTurnNotOwner, relayTurnRow{}
	}
	changed := b.pruneExpiredLocked()
	key := relayTurnKey{cmd.SessionID, turnID}
	if r := b.rows[key]; r != nil {
		if r.State == relayTurnStateAccepted {
			if changed {
				_ = b.persistLocked()
			}
			return relayTurnReack, *r
		}
		if b.inFlight[key] {
			if changed {
				_ = b.persistLocked()
			}
			return relayTurnInFlight, *r
		}
	}
	now := b.now().UnixMilli()
	r := b.rows[key]
	if r == nil {
		b.evictForCapLocked()
		r = &relayTurnRow{SessionID: cmd.SessionID, TurnID: turnID, ClaimedAt: now}
		b.rows[key] = r
	}
	r.State = relayTurnStateClaimed
	r.EnvelopeID = cmd.ID
	r.WorkspaceID = cmd.WorkspaceID
	r.AgentID = agentID
	r.UID = cmd.UID
	r.UpdatedAt = now
	b.inFlight[key] = true
	_ = b.persistLocked()
	return relayTurnRun, *r
}

// Accept moves a claimed row to accepted and persists it BEFORE the caller
// lets the turn start or publishes the ack. It returns the row and true on
// the transition; false with a nil error when the row is unknown or already
// accepted. A failed write returns an error and leaves the row claimed in
// memory as well: the acceptance is not durable, so the caller must refuse
// the turn — neither run it nor ack it.
func (b *relayTurnInbox) Accept(sessionID, turnID string) (relayTurnRow, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ownLocked() {
		return relayTurnRow{}, false, fmt.Errorf("%s for turn %s: another agent process owns the inbox", relayAcceptanceNotRecorded, turnID)
	}
	r := b.rows[relayTurnKey{sessionID, turnID}]
	if r == nil || r.State == relayTurnStateAccepted {
		return relayTurnRow{}, false, nil
	}
	prev := *r
	now := b.now().UnixMilli()
	r.State = relayTurnStateAccepted
	r.AcceptedAt = now
	r.UpdatedAt = now
	if err := b.persistLocked(); err != nil {
		*r = prev
		return relayTurnRow{}, false, fmt.Errorf("%s for turn %s: %w", relayAcceptanceNotRecorded, turnID, err)
	}
	return *r, true, nil
}

// Finish ends this process's dispatch of a turn. The row itself stays
// (claimed or accepted) — see the file header for why.
func (b *relayTurnInbox) Finish(sessionID, turnID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inFlight, relayTurnKey{sessionID, turnID})
}

// ConfirmAcked deletes exactly the rows named by a VERIFIED watermark for
// sessionID and returns how many it deleted. An id with no row (already
// expired, torn down, or confirmed by an earlier watermark) is a no-op. A row
// that is claimed and in flight is left alone: the broker cannot have
// recorded an ack this device has not published yet, so naming it is a
// broker bug, and deleting it would reopen the double-run window.
func (b *relayTurnInbox) ConfirmAcked(sessionID string, turnIDs []string) int {
	if sessionID == "" || len(turnIDs) == 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ownLocked() {
		return 0
	}
	deleted := 0
	for _, id := range turnIDs {
		key := relayTurnKey{sessionID, id}
		r := b.rows[key]
		if r == nil {
			continue
		}
		if r.State != relayTurnStateAccepted && b.inFlight[key] {
			continue
		}
		delete(b.rows, key)
		deleted++
	}
	if deleted > 0 {
		_ = b.persistLocked()
	}
	return deleted
}

// ReleaseSession deletes every row of a session torn down on this device.
func (b *relayTurnInbox) ReleaseSession(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ownLocked() {
		return
	}
	changed := false
	for k := range b.rows {
		if k.SessionID == sessionID && !b.inFlight[k] {
			delete(b.rows, k)
			changed = true
		}
	}
	if changed {
		_ = b.persistLocked()
	}
}

// snapshot returns a copy of the current rows (tests, diagnostics); nil when
// another process owns the inbox.
func (b *relayTurnInbox) snapshot() map[relayTurnKey]relayTurnRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ownLocked() {
		return nil
	}
	out := make(map[relayTurnKey]relayTurnRow, len(b.rows))
	for k, r := range b.rows {
		out[k] = *r
	}
	return out
}

/* --------------------------------------------------------------------------
   Acceptance plumbing
   -------------------------------------------------------------------------- */

// relayTurnAcceptKey carries the per-delivery acceptance callback through the
// dispatch ctx. Context (not a per-session registry) binds it to THIS
// delivery: a non-relay send racing on the same session can never fire it.
type relayTurnAcceptKey struct{}

func withRelayTurnAcceptance(ctx context.Context, accept func() error) context.Context {
	return context.WithValue(ctx, relayTurnAcceptKey{}, accept)
}

// relayTurnAcceptor returns the acceptance callback of the relay turn being
// dispatched on ctx, or nil for an ordinary command. Handlers hand it to their
// manager, which calls it at its ownership boundary — after every refusal
// check, before the turn can start — and refuses the turn when it returns an
// error (the acceptance could not be made durable).
func relayTurnAcceptor(ctx context.Context) func() error {
	if ctx == nil {
		return nil
	}
	if f, ok := ctx.Value(relayTurnAcceptKey{}).(func() error); ok {
		return f
	}
	return nil
}

// acceptRelayDelivery is how a persistent-session manager calls the (possibly
// nil) acceptor under its stdin lock, immediately before writing the turn.
func acceptRelayDelivery(accept func() error) error {
	if accept == nil {
		return nil
	}
	return accept()
}

// relayTurnAckFrame is the result frame the broker's results subscriber reads:
// `{ type: "relay_turn_ack", sessionID, relayTurnId, workspaceID, agentId }`.
func relayTurnAckFrame(r relayTurnRow) resultMsg {
	id := r.EnvelopeID
	if id == "" {
		id = relayTurnEnvelopePrefix + r.TurnID
	}
	return resultMsg{
		ID:          id,
		WorkspaceID: r.WorkspaceID,
		UID:         r.UID,
		AgentID:     r.AgentID,
		Status:      "success",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		BootID:      agentBootID,
		Type:        relayTurnAckResultType,
		SessionID:   r.SessionID,
		RelayTurnID: r.TurnID,
	}
}

/* --------------------------------------------------------------------------
   Dispatch and watermark (called from pubsub.go)
   -------------------------------------------------------------------------- */

// runRelayTurn is the one inbox check every relayed session command passes
// (handleSessionCommand, before per-kind routing). dispatch runs the kind's
// handler with a ctx carrying the acceptance callback; the handler's manager
// calls it at its ownership boundary, which persists `accepted` and only then
// publishes the ack. publishAck is injected so tests can observe the frames
// without a Pub/Sub topic. It returns false only when another agent process
// owns the inbox: the caller must Nack so Pub/Sub hands the turn to the owner.
func runRelayTurn(ctx context.Context, inbox *relayTurnInbox, cmd commandMsg, turnID, agentID string,
	publishAck func(relayTurnRow), dispatch func(context.Context)) bool {
	decision, row := inbox.Begin(cmd, turnID, agentID)
	switch decision {
	case relayTurnNotOwner:
		fmt.Printf("%s[relay-inbox] Another agent process owns the relay turn inbox — handing turn %s on %s back%s\n",
			colorYellow, turnID, cmd.SessionID, colorReset)
		return false
	case relayTurnReack:
		// Accepted earlier (this boot or a previous one): the broker has not
		// recorded our ack yet. Say it again; never run the turn again.
		fmt.Printf("%s[relay-inbox] Turn %s on %s already accepted — re-acking, not re-running%s\n",
			colorYellow, turnID, cmd.SessionID, colorReset)
		publishAck(row)
		return true
	case relayTurnInFlight:
		// A concurrent redelivery of a turn this process is dispatching
		// right now; that dispatch acks when it accepts.
		fmt.Printf("%s[relay-inbox] Turn %s on %s is already being dispatched — dropping the duplicate delivery%s\n",
			colorYellow, turnID, cmd.SessionID, colorReset)
		return true
	}
	defer inbox.Finish(cmd.SessionID, turnID)

	var once sync.Once
	var acceptErr error
	accept := func() error {
		once.Do(func() {
			r, ok, err := inbox.Accept(cmd.SessionID, turnID)
			if err != nil {
				acceptErr = err
				return
			}
			if ok {
				publishAck(r)
			}
		})
		return acceptErr
	}
	dispatch(withRelayTurnAcceptance(ctx, accept))
	return true
}

// publishRelayTurnAck publishes the ack frame the same way session frames
// are published: a fresh context, because the ack of a one-shot turn fires
// from inside a Send that may outlive the Pub/Sub callback's ctx. A failed
// publish is only logged — the row stays accepted, and the broker's outbox
// republishes the SEND, which re-acks from the inbox.
func publishRelayTurnAck(topic *pubsub.Publisher, r relayTurnRow) {
	pubCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := publishMsg(pubCtx, topic, relayTurnAckFrame(r)); err != nil {
		fmt.Printf("%s[relay-inbox] Failed to publish the ack for turn %s on %s: %v%s\n",
			colorRed, r.TurnID, r.SessionID, err, colorReset)
	}
}

// checkRelayAckWatermark refuses an over-cap watermark. Never truncate: the
// broker would keep believing it confirmed rows this device kept, and a
// signer and verifier that cut in different places disagree on the HMAC.
func checkRelayAckWatermark(cmd commandMsg) error {
	if len(cmd.AckedTurnIds) > relayAckWatermarkMax {
		return fmt.Errorf("ackedTurnIds carries %d ids, more than the %d allowed",
			len(cmd.AckedTurnIds), relayAckWatermarkMax)
	}
	return nil
}

// applyRelayAckWatermark deletes exactly the inbox rows the watermark names
// for the command's session. signed must be true only when the command's HMAC
// (which covers ackedTurnIds) verified; otherwise nothing is deleted.
func applyRelayAckWatermark(inbox *relayTurnInbox, cmd commandMsg, signed bool) int {
	if len(cmd.AckedTurnIds) == 0 || cmd.SessionID == "" {
		return 0
	}
	if !signed {
		fmt.Printf("%s[relay-inbox] Ignoring an unsigned ack watermark on %s%s\n",
			colorYellow, cmd.SessionID, colorReset)
		return 0
	}
	if checkRelayAckWatermark(cmd) != nil {
		return 0
	}
	return inbox.ConfirmAcked(cmd.SessionID, cmd.AckedTurnIds)
}
