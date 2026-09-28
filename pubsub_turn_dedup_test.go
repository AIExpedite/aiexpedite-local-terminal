package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// The durable relay turn inbox (relay_turn_inbox.go): a relayed SEND runs at
// most once after it was accepted, is never dropped, and its row goes only on
// a signed watermark, session teardown or the TTL.

type relayTurnHarness struct {
	inbox *relayTurnInbox
	mu    sync.Mutex
	acks  []relayTurnRow
	runs  int
}

func newRelayTurnHarness(t *testing.T) *relayTurnHarness {
	return &relayTurnHarness{inbox: newTestRelayInbox(t)}
}

func (h *relayTurnHarness) deliver(cmd commandMsg, accept bool) {
	turnID, ok := relayTurnIDFromEnvelope(cmd.ID)
	if !ok {
		panic("not a relay envelope: " + cmd.ID)
	}
	runRelayTurn(context.Background(), h.inbox, cmd, turnID, "agent-1",
		func(r relayTurnRow) {
			h.mu.Lock()
			h.acks = append(h.acks, r)
			h.mu.Unlock()
		},
		func(ctx context.Context) {
			h.mu.Lock()
			h.runs++
			h.mu.Unlock()
			if accept {
				acceptRelayTurn(ctx)
			}
		})
}

// restart simulates an agent restart: a fresh process state over the same file.
func (h *relayTurnHarness) restart() {
	h.inbox = newRelayTurnInbox(h.inbox.path)
}

func relaySend(turnID string) commandMsg {
	return commandMsg{
		ID:          relayTurnEnvelopePrefix + turnID,
		Type:        "claude_native_send",
		SessionID:   "sess-1",
		WorkspaceID: "ws-1",
		UID:         "u-1",
		Input:       "run the tests",
	}
}

func TestRelayTurnEnvelopeParsing(t *testing.T) {
	if id, ok := relayTurnIDFromEnvelope("relayturn_abc"); !ok || id != "abc" {
		t.Fatalf("relayturn_abc → %q, %v", id, ok)
	}
	for _, bad := range []string{"relayturn_", "cmd-1", "", "xrelayturn_abc"} {
		if _, ok := relayTurnIDFromEnvelope(bad); ok {
			t.Fatalf("%q parsed as a relay envelope", bad)
		}
	}
}

// The device published its ack, the broker never recorded it (ack lost, broker
// restarted), and the outbox republished the SEND — after this agent restarted
// too. The retained row must turn that into a re-ack, not a second run.
func TestRelayTurnDedup_RedeliveryAfterAckReacksInsteadOfRerunning(t *testing.T) {
	h := newRelayTurnHarness(t)
	h.deliver(relaySend("t1"), true)
	if h.runs != 1 || len(h.acks) != 1 {
		t.Fatalf("first delivery: runs=%d acks=%d, want 1/1", h.runs, len(h.acks))
	}
	if r := h.inbox.snapshot()[relayTurnKey{"sess-1", "t1"}]; r.State != relayTurnStateAccepted {
		t.Fatalf("row after ack = %+v, want accepted and RETAINED", r)
	}

	h.restart()
	h.deliver(relaySend("t1"), true)
	h.deliver(relaySend("t1"), true)
	if h.runs != 1 {
		t.Fatalf("turn ran %d times, want once", h.runs)
	}
	if len(h.acks) != 3 {
		t.Fatalf("acks=%d, want the original plus one re-ack per redelivery", len(h.acks))
	}
	re := h.acks[1]
	if re.TurnID != "t1" || re.SessionID != "sess-1" || re.WorkspaceID != "ws-1" || re.AgentID != "agent-1" {
		t.Fatalf("re-ack row = %+v", re)
	}
}

// A crash between claimed and accepted is the only re-run window, and there
// the turn provably never started.
func TestRelayTurnDedup_CrashBeforeAcceptanceRedispatches(t *testing.T) {
	h := newRelayTurnHarness(t)
	decision, _ := h.inbox.Begin(relaySend("t1"), "t1", "agent-1")
	if decision != relayTurnRun {
		t.Fatalf("Begin = %v", decision)
	}
	// ... the agent dies here: no Accept, no Finish.
	h.restart()
	if r := h.inbox.snapshot()[relayTurnKey{"sess-1", "t1"}]; r.State != relayTurnStateClaimed {
		t.Fatalf("claimed row did not survive the restart: %+v", r)
	}
	h.deliver(relaySend("t1"), true)
	if h.runs != 1 || len(h.acks) != 1 {
		t.Fatalf("redelivery after a pre-acceptance crash: runs=%d acks=%d, want 1/1", h.runs, len(h.acks))
	}
}

// A handler that REFUSED the turn (session not found, input rejected) never
// accepts: no ack, the row stays claimed, and a redelivery tries again.
func TestRelayTurnDedup_RefusedTurnIsNotAcked(t *testing.T) {
	h := newRelayTurnHarness(t)
	h.deliver(relaySend("t1"), false)
	if len(h.acks) != 0 {
		t.Fatalf("a refused turn was acked: %+v", h.acks)
	}
	h.deliver(relaySend("t1"), true)
	if h.runs != 2 || len(h.acks) != 1 {
		t.Fatalf("runs=%d acks=%d, want 2/1", h.runs, len(h.acks))
	}
}

// A duplicate delivered while this process is still dispatching the turn is
// dropped; the running dispatch acks when it accepts.
func TestRelayTurnDedup_ConcurrentDuplicateIsDropped(t *testing.T) {
	h := newRelayTurnHarness(t)
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runRelayTurn(context.Background(), h.inbox, relaySend("t1"), "t1", "agent-1",
			func(r relayTurnRow) { h.mu.Lock(); h.acks = append(h.acks, r); h.mu.Unlock() },
			func(ctx context.Context) {
				h.mu.Lock()
				h.runs++
				h.mu.Unlock()
				close(started)
				<-release
				acceptRelayTurn(ctx)
			})
	}()
	<-started
	h.deliver(relaySend("t1"), true)
	close(release)
	<-done
	if h.runs != 1 || len(h.acks) != 1 {
		t.Fatalf("runs=%d acks=%d, want 1/1", h.runs, len(h.acks))
	}
}

// Acceptance fires at most once per delivery even if a handler calls it twice.
func TestRelayTurnDedup_AcceptIsIdempotent(t *testing.T) {
	h := newRelayTurnHarness(t)
	runRelayTurn(context.Background(), h.inbox, relaySend("t1"), "t1", "agent-1",
		func(r relayTurnRow) { h.acks = append(h.acks, r) },
		func(ctx context.Context) { acceptRelayTurn(ctx); acceptRelayTurn(ctx) })
	if len(h.acks) != 1 {
		t.Fatalf("acks=%d, want 1", len(h.acks))
	}
}

// A row goes only once a watermark confirms it; teardown clears the rest.
func TestRelayTurnDedup_RowLifecycle(t *testing.T) {
	h := newRelayTurnHarness(t)
	h.deliver(relaySend("t1"), true)
	h.deliver(relaySend("t2"), true)
	other := relaySend("t9")
	other.SessionID = "sess-2"
	h.deliver(other, true)

	// Publishing the ack (and re-acking) never removes the row.
	h.deliver(relaySend("t1"), true)
	if _, ok := h.inbox.snapshot()[relayTurnKey{"sess-1", "t1"}]; !ok {
		t.Fatal("row deleted by an ack publish")
	}

	// The watermark confirms t1 only.
	if n := applyRelayAckWatermark(h.inbox, commandMsg{SessionID: "sess-1", AckedTurnIds: []string{"t1"}}, true); n != 1 {
		t.Fatalf("watermark deleted %d rows, want 1", n)
	}
	rows := h.inbox.snapshot()
	if _, ok := rows[relayTurnKey{"sess-1", "t1"}]; ok {
		t.Fatal("confirmed row survived")
	}
	if _, ok := rows[relayTurnKey{"sess-1", "t2"}]; !ok {
		t.Fatal("unconfirmed row deleted")
	}

	// Teardown of sess-1 (through the path every manager takes) clears t2.
	prev := globalRelayTurnInbox
	globalRelayTurnInbox = h.inbox
	t.Cleanup(func() { globalRelayTurnInbox = prev })
	releaseLedgerSession("sess-1")
	rows = h.inbox.snapshot()
	if _, ok := rows[relayTurnKey{"sess-1", "t2"}]; ok {
		t.Fatal("teardown left a row of the ended session")
	}
	if _, ok := rows[relayTurnKey{"sess-2", "t9"}]; !ok {
		t.Fatal("teardown of sess-1 deleted sess-2's row")
	}
	h.restart()
	if got := len(h.inbox.snapshot()); got != 1 {
		t.Fatalf("after restart the inbox holds %d rows, want 1", got)
	}
}

func TestRelayTurnDedup_TTLExpiresRows(t *testing.T) {
	h := newRelayTurnHarness(t)
	base := time.Now()
	h.inbox.now = func() time.Time { return base }
	h.deliver(relaySend("t1"), true)

	h.restart()
	h.inbox.now = func() time.Time { return base.Add(relayTurnInboxTTL - time.Minute) }
	if len(h.inbox.snapshot()) != 1 {
		t.Fatal("row expired before the TTL")
	}
	h.restart()
	h.inbox.now = func() time.Time { return base.Add(relayTurnInboxTTL + time.Minute) }
	if len(h.inbox.snapshot()) != 0 {
		t.Fatal("row outlived the TTL")
	}
}

// The ack frame carries exactly what the broker's results subscriber reads.
func TestRelayTurnAckFrameShape(t *testing.T) {
	var got []resultMsg
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		got = append(got, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })

	publishRelayTurnAck(nil, relayTurnRow{SessionID: "sess-1", TurnID: "t1", EnvelopeID: "relayturn_t1",
		WorkspaceID: "ws-1", AgentID: "agent-1", UID: "u-1"})
	if len(got) != 1 {
		t.Fatalf("published %d frames", len(got))
	}
	raw, _ := json.Marshal(got[0])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	want := map[string]string{"type": "relay_turn_ack", "sessionID": "sess-1", "relayTurnId": "t1",
		"workspaceID": "ws-1", "agentId": "agent-1"}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("frame %s = %v, want %q (frame %s)", k, m[k], v, raw)
		}
	}
}

/* ---- the per-kind handlers own acceptance ---- */

type fakeRelayOneShot struct {
	fakeOneShotTarget
	refuse bool
}

func (f *fakeRelayOneShot) SendTurn(_, _ string, _ PublishFunc, _ time.Duration, onAccepted func()) error {
	f.sendCalls++
	if f.refuse {
		return errors.New("native session s1 not found")
	}
	if onAccepted != nil {
		onAccepted()
	}
	return nil
}

func TestRelayTurnOneShotDispatchAcceptsOnlyWhenOwned(t *testing.T) {
	kind := openCodeNativeKind
	cmd := commandMsg{ID: "relayturn_t1", Type: kind.FramePrefix + "_send", SessionID: "s1", Input: "hi"}
	accepted := 0
	onAccepted := func() { accepted++ }
	noop := func(resultMsg) {}
	noErr := func(string) {}

	dispatchOneShotNativeCommandAccepting(cmd, &fakeRelayOneShot{}, kind, noop, noErr, onAccepted)
	if accepted != 1 {
		t.Fatalf("owned turn: accepted=%d", accepted)
	}
	dispatchOneShotNativeCommandAccepting(cmd, &fakeRelayOneShot{refuse: true}, kind, noop, noErr, onAccepted)
	if accepted != 1 {
		t.Fatalf("refused turn was accepted")
	}
	// A target without SendTurn owns the turn once Send returned nil.
	dispatchOneShotNativeCommandAccepting(cmd, &fakeOneShotTarget{}, kind, noop, noErr, onAccepted)
	if accepted != 2 {
		t.Fatalf("plain target: accepted=%d, want 2", accepted)
	}
	dispatchOneShotNativeCommandAccepting(cmd, &fakeOneShotTarget{sendErr: errors.New("x not found")}, kind, noop, noErr, onAccepted)
	if accepted != 2 {
		t.Fatalf("plain target with a failed Send was accepted")
	}
}

// The real one-shot manager refuses an unknown session BEFORE it owns the
// turn, so a relay turn aimed at it is never acked.
func TestRelayTurnOneShotManagerRefusalDoesNotAccept(t *testing.T) {
	mgr := NewMuseCodeNativeManager()
	accepted := false
	err := mgr.SendTurn("no-such-session", "hi", func(resultMsg) {}, time.Second, func() { accepted = true })
	if err == nil {
		t.Fatal("SendTurn to an unknown session succeeded")
	}
	if accepted {
		t.Fatal("a refused turn was accepted")
	}
}
