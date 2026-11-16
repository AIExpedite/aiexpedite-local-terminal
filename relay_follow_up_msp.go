// File: relay_follow_up_msp.go
// -----------------------------------------------------------------------------
// Live follow-ups to Muse Code on its MSP driver (`muse serve`), device half
// (L5 spec §2.4, §4). A message sent while Muse works goes into the RUNNING
// turn as a `turn/steer` the device builds itself, naming that turn
// (`expectedTurnId`, signed), under the follow-up ledger
// (relay_follow_up_ledger.go), and the attempt is settled from Muse's own
// answer.
//
// Muse's ANSWER to a steer does not say where the message goes. Certification
// on Muse 1.4.20 (2026-11-16) recorded both of these:
//
//   - a steer acked `accepted` naming the running turn, which that turn
//     then finished without, and which Muse ran as the NEXT turn under the
//     steer's own commandId: every steer sent at a turn's start that used no
//     tool, and every one sent while the final answer streamed (7 of 7);
//   - a steer acked `accepted` under its own commandId and then DROPPED:
//     every one Muse read after the turn's final answer had completed (the
//     ~50 ms before `turn/foregroundCompleted`, 4 of 10, and after it).
//
// So the device judges from what Muse then DOES (mspPlacement): `current`
// only once the steer's own `userMessage` item is seen in the turn it named;
// `next` once Muse runs it as a turn of its own (that turn's `turn/started`,
// or the `userMessage` in another turn), with that turn's id on the ack
// (`followUpMspTurnId`) so terminal-service binds the follow-up's turn to it;
// `unknown` (unplaced) when neither is seen. And it never writes into the
// window where Muse drops a steer: from the final answer's `item/completed`
// on, a follow-up is refused noActiveTurn and the broker sends it as the
// next turn.
//
// What the device knows of MSP for this is limited to the JSON-RPC ids it
// chose, request/response pairing, and these facts per host, read from the
// frames going both ways:
//
//   - the session the host serves (the answer to `session/start` or
//     `session/resume`), which the signed `conversationId` must name;
//   - the running turn (a `turn/start` ack with disposition "started", or
//     `turn/started`), cleared by its own `turn/completed`; the signed
//     `expectedTurnId` must name it (else turnMismatch);
//   - whether that turn's final answer has completed (an `agentMessage`
//     item in phase `finalAnswer`) or its foreground is done
//     (`turn/foregroundCompleted`): Muse dropped steers it read after
//     either (2026-11-13 S10; 2026-11-16), so from then on the device
//     refuses noActiveTurn and the broker sends the message as the next
//     turn;
//   - where each steer it wrote ran: its `userMessage` item (by the
//     steer's commandId) in the named turn or another one, a turn started
//     under its commandId, or the named turn ending without it;
//   - the approvals the host is waiting on (`approval/request` until
//     `approval/resolved`, a newer request for the same approval, or the end
//     of its turn), and its open `userInput/request`s: nothing is written
//     beside one (approvalPending).
//
// The steer's `commandId` is a UUIDv7 DERIVED from the attempt and the
// command's signed `ts` (mspFollowUpCommandID, the mirror of shared-constants
// mspFollowUpCommandId), because Muse refuses anything but a UUIDv7: a
// redelivered attempt carries the same one. Follow-up text is never logged.
// -----------------------------------------------------------------------------

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Wire values mirrored from shared-constants 3.23.580 (museCode.js
// MUSE_MSP_METHODS, MUSE_MSP_NOTIFICATIONS, MUSE_MSP_SERVER_REQUESTS,
// MUSE_MSP_ERROR_CODES, MUSE_MSP_REJECT_REASONS, MUSE_MSP_APPROVAL_*,
// MUSE_MSP_IF_BUSY, MUSE_MSP_TURN_TERMINALS). Contract-pinned by
// relay_follow_up_jsonrpc_msp_test.go. External values: Muse's own names.
const (
	mspMethodInitialize             = "initialize"
	mspMethodInitialized            = "initialized"
	mspMethodSessionStart           = "session/start"
	mspMethodSessionResume          = "session/resume"
	mspMethodSessionSetApprovalMode = "session/setApprovalMode"
	mspMethodSessionSetModel        = "session/setModel"
	mspMethodSessionSetEffort       = "session/setReasoningEffort"
	mspMethodTurnStart              = "turn/start"
	mspMethodTurnSteer              = "turn/steer"
	mspMethodTurnInterrupt          = "turn/interrupt"
	mspMethodTurnCancel             = "turn/cancel"
	mspMethodApprovalDecide         = "approval/decide"
	mspMethodApprovalListPending    = "approval/listPending"
	mspMethodModelList              = "model/list"
	mspMethodUsageRead              = "usage/read"

	mspNotifyTurnStarted             = "turn/started"
	mspNotifyItemStarted             = "item/started"
	mspNotifyItemCompleted           = "item/completed"
	mspNotifyTurnForegroundCompleted = "turn/foregroundCompleted"
	mspNotifyTurnCompleted           = "turn/completed"
	mspNotifyApprovalResolved        = "approval/resolved"
	mspNotifyUsageChanged            = "usage/changed"
	// userInput/settled closes a userInput/request (Muse's own name; not in
	// shared-constants, which reads no user-input frames).
	mspNotifyUserInputSettled = "userInput/settled"

	mspServerRequestApproval  = "approval/request"
	mspServerRequestUserInput = "userInput/request"

	mspErrInvalidParams   = -32602
	mspErrMethodNotFound  = -32601
	mspErrOverloaded      = -32001
	mspErrSessionNotFound = -32020
	mspErrSessionInUse    = -32021
	mspErrSessionNotLoad  = -32024
	mspErrCommandRejected = -32030
	mspErrBackpressured   = -32031

	mspRejectMissingRun        = "missing_run"
	mspRejectAlreadyTerminal   = "already_terminal"
	mspRejectCommandIDConflict = "command_id_conflict"

	mspApprovalApproved  = "approved"
	mspApprovalDenied    = "denied"
	mspApprovalAbort     = "abort"
	mspApprovalScopeOnce = "once"

	mspIfBusyQueue = "queue"

	mspTurnTerminalCompleted = "completed"
	// TurnError.kind of a turn Muse refused for sign-in.
	mspTurnErrorAuthRequired = "authRequired"

	// turn/start ack dispositions; "accepted" is a steer's admission status.
	mspDispositionStarted = "started"
	mspStatusAccepted     = "accepted"

	// MUSE_MSP_ITEM_KINDS / MUSE_MSP_MESSAGE_PHASES (shared-constants 3.23.594).
	mspItemUserMessage  = "userMessage"
	mspItemAgentMessage = "agentMessage"
	mspPhaseFinalAnswer = "finalAnswer"
)

// mspAfterTurnWait is how long, after the turn a steer named has ended
// without it, the device waits for Muse to start the steer's own turn. Muse
// started it 49-76 ms after the turn's `turn/completed` on every run
// (2026-11-16). A var so tests can shorten it.
var mspAfterTurnWait = 5 * time.Second

// isMSPDriverKind reports the agent whose live follow-up rides the MSP driver.
func isMSPDriverKind(kind string) bool { return kind == museCodeCliAgentID }

/* --------------------------------------------------------------------------
   The steer's commandId
   -------------------------------------------------------------------------- */

const (
	mspFollowUpCommandIDDomain = "aix-msp-follow-up\n"
	uuidV7MaxUnixMs            = int64(1) << 48
)

// mspFollowUpCommandID mirrors shared-constants mspFollowUpCommandId: the
// `commandId` of the `turn/steer` carrying attempt attemptID, a UUIDv7 whose
// timestamp is the command's signed ts and whose other bits come from
// sha256("aix-msp-follow-up\n" + attemptID). ok=false for an unusable input.
// Both suites pin testdata/mspFollowUpCommandIdVectors.json.
func mspFollowUpCommandID(attemptID string, signedTsMs int64) (string, bool) {
	if attemptID == "" || signedTsMs < 0 || signedTsMs >= uuidV7MaxUnixMs {
		return "", false
	}
	h := sha256.Sum256([]byte(mspFollowUpCommandIDDomain + attemptID))
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(signedTsMs >> (8 * (5 - i)))
	}
	copy(b[6:], h[:10])
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32], true
}

/* --------------------------------------------------------------------------
   The frame
   -------------------------------------------------------------------------- */

type mspSteerParams struct {
	CommandID      string         `json:"commandId"`
	SessionID      string         `json:"sessionId"`
	ExpectedTurnID string         `json:"expectedTurnId"`
	Input          []acpTextBlock `json:"input"`
}

// mspSteerFrame is the `turn/steer` a Muse Code live attempt writes: the
// signed session and running turn, the derived commandId, one text part.
func mspSteerFrame(rpcID, sessionID, expectedTurnID, commandID, text string) (string, error) {
	return marshalLiveFollowUpRequest(rpcID, mspMethodTurnSteer, mspSteerParams{
		CommandID:      commandID,
		SessionID:      sessionID,
		ExpectedTurnID: expectedTurnID,
		Input:          []acpTextBlock{{Type: "text", Text: text}},
	})
}

/* --------------------------------------------------------------------------
   Settling from the answer (L5 spec §4.5)
   -------------------------------------------------------------------------- */

// classifyMSPSteerAnswer maps Muse's answer to a `turn/steer` onto the ledger.
// Written only when Muse says it took the steer for the RUNNING turn named by
// the signed expectedTurnId; even then WHERE it runs is not known from the
// answer (Muse ran such steers as the next turn too), so the caller settles
// the attempt from Muse's frames (awaitMSPPlacement). An accepted steer whose
// ack names any other turn was dropped every time it was seen (S10;
// 2026-11-16), so it is unknown. A refusal is notWritten only where nothing
// can have reached the conversation.
func classifyMSPSteerAnswer(frame, expectedTurnID string) liveFollowUpVerdict {
	var answer struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code *int64 `json:"code"`
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(frame), &answer) != nil {
		return liveFollowUpVerdict{State: followUpRowUnknown, Reason: followUpReasonAnswerUnrecognised}
	}
	if e := answer.Error; e != nil {
		if e.Code == nil {
			return liveFollowUpVerdict{State: followUpRowUnknown}
		}
		switch *e.Code {
		case mspErrCommandRejected:
			switch e.Data.Reason {
			case mspRejectMissingRun, mspRejectAlreadyTerminal:
				return refusedVerdict(followUpReasonNoActiveTurn)
			case mspRejectCommandIDConflict:
				// The commandId is this attempt's own: an earlier command
				// under it may have run.
				return liveFollowUpVerdict{State: followUpRowUnknown}
			}
			return refusedVerdict(followUpReasonCliRefused)
		case mspErrInvalidParams, mspErrSessionInUse, mspErrBackpressured, mspErrOverloaded:
			return refusedVerdict(followUpReasonCliRefused)
		case mspErrMethodNotFound:
			return refusedVerdict(followUpReasonMethodNotSupported)
		case mspErrSessionNotFound, mspErrSessionNotLoad:
			return refusedVerdict(followUpReasonConversationMismatch)
		}
		return liveFollowUpVerdict{State: followUpRowUnknown}
	}
	var r struct {
		Status string `json:"status"`
		TurnID string `json:"turnId"`
	}
	if len(answer.Result) > 0 && json.Unmarshal(answer.Result, &r) == nil &&
		r.Status == mspStatusAccepted && expectedTurnID != "" && r.TurnID == expectedTurnID {
		return liveFollowUpVerdict{State: followUpRowWritten}
	}
	return liveFollowUpVerdict{State: followUpRowUnknown, Reason: followUpReasonAnswerUnrecognised}
}

// classifyLiveFollowUpAnswerFor is the verdict on an answer a waiter
// received: Muse's needs the running turn the steer named.
func classifyLiveFollowUpAnswerFor(kind string, a liveFollowUpAnswer) liveFollowUpVerdict {
	if isMSPDriverKind(kind) {
		return classifyMSPSteerAnswer(a.Frame, a.ExpectedTurnID)
	}
	return classifyLiveFollowUpAnswer(kind, a.Frame)
}

/* --------------------------------------------------------------------------
   The tracker's MSP mode
   -------------------------------------------------------------------------- */

// mspTrackerState is the MSP half of a liveFollowUpTracker (guarded by its mu).
type mspTrackerState struct {
	// setup / turnStarts: request id keys written and not answered yet.
	setup      map[string]struct{}
	turnStarts map[string]struct{}
	// sessionID is the session the host serves.
	sessionID string
	// runningTurnID is the turn running now ("" when none);
	// foregroundDone: its `turn/foregroundCompleted` has been read;
	// answerDone: its final answer (an agentMessage in phase finalAnswer)
	// has completed. Either one closes the turn to live writes.
	runningTurnID  string
	foregroundDone bool
	answerDone     bool
	// approvals / userInputs: the host's open requests, by approvalId /
	// userInputId, each with the turn it was asked under ("" when unnamed).
	approvals  map[string]string
	userInputs map[string]string
	// expected is the running turn each live waiter's steer named.
	expected map[string]string
	// watches are the steers the device wrote, by JSON-RPC id, until their
	// attempt settles: where each one ran (mspPlaceLocked).
	watches map[string]*mspSteerWatch
}

// mspPlacement is where Muse ran a steer: in the turn it named (`current`),
// or as a turn of its own (`next`); turnID is that turn. exited: the host
// went away before either was seen.
type mspPlacement struct {
	processedIn string
	turnID      string
	exited      bool
}

// mspSteerWatch follows one written steer until Muse is seen running it.
type mspSteerWatch struct {
	commandID string
	expected  string
	// placed receives the placement once (cap 1); sent says it has.
	placed chan mspPlacement
	sent   bool
	// ended is closed when the named turn ended without the steer.
	ended  chan struct{}
	closed bool
}

func newMSPTrackerState() *mspTrackerState {
	return &mspTrackerState{
		setup:      map[string]struct{}{},
		turnStarts: map[string]struct{}{},
		approvals:  map[string]string{},
		userInputs: map[string]string{},
		expected:   map[string]string{},
		watches:    map[string]*mspSteerWatch{},
	}
}

// mspOutbound records, before the write, the requests whose answers the
// tracker reads: a session setup (it names the session) and a `turn/start`
// (its ack names the turn that started).
func (t *liveFollowUpTracker) mspOutbound(f jsonRPCFrame, out liveFollowUpOutbound) liveFollowUpOutbound {
	key := f.idKey()
	t.mu.Lock()
	defer t.mu.Unlock()
	switch f.Method {
	case mspMethodSessionStart, mspMethodSessionResume:
		t.msp.setup[key] = struct{}{}
		out.mspSetupKey = key
	case mspMethodTurnStart:
		t.msp.turnStarts[key] = struct{}{}
		out.mspTurnKey = key
	}
	return out
}

// mspResponseLocked reads an answer to a request mspOutbound recorded.
func (t *liveFollowUpTracker) mspResponseLocked(f jsonRPCFrame) {
	st := t.msp
	key := f.idKey()
	success := len(f.Error) == 0 && len(f.Result) > 0
	if _, ok := st.setup[key]; ok {
		delete(st.setup, key)
		var r struct {
			Session struct {
				SessionID string `json:"sessionId"`
			} `json:"session"`
		}
		if success && json.Unmarshal(f.Result, &r) == nil && r.Session.SessionID != "" {
			st.sessionID = r.Session.SessionID
		}
		return
	}
	if _, ok := st.turnStarts[key]; ok {
		delete(st.turnStarts, key)
		var r struct {
			TurnID      string `json:"turnId"`
			Disposition string `json:"disposition"`
		}
		if success && json.Unmarshal(f.Result, &r) == nil && r.Disposition == mspDispositionStarted && r.TurnID != "" {
			t.mspTurnStartedLocked(r.TurnID)
		}
	}
}

func (t *liveFollowUpTracker) mspTurnStartedLocked(turnID string) {
	if t.msp.runningTurnID == turnID {
		return
	}
	t.msp.runningTurnID = turnID
	t.msp.foregroundDone = false
	t.msp.answerDone = false
}

// mspRequestLocked reads a request the host makes of the client.
func (t *liveFollowUpTracker) mspRequestLocked(f jsonRPCFrame) {
	var p struct {
		ApprovalID  string `json:"approvalId"`
		UserInputID string `json:"userInputId"`
		TurnID      string `json:"turnId"`
	}
	_ = json.Unmarshal(f.Params, &p)
	switch f.Method {
	case mspServerRequestApproval:
		// Keyed by approvalId, so a later stage of the same approval
		// replaces the earlier one rather than adding to it.
		id := p.ApprovalID
		if id == "" {
			id = "rpc:" + f.idKey()
		}
		t.msp.approvals[id] = p.TurnID
	case mspServerRequestUserInput:
		id := p.UserInputID
		if id == "" {
			id = "rpc:" + f.idKey()
		}
		t.msp.userInputs[id] = p.TurnID
	}
}

// mspNotificationLocked reads a host notification.
func (t *liveFollowUpTracker) mspNotificationLocked(f jsonRPCFrame) {
	st := t.msp
	var p struct {
		TurnID      string `json:"turnId"`
		ApprovalID  string `json:"approvalId"`
		UserInputID string `json:"userInputId"`
	}
	_ = json.Unmarshal(f.Params, &p)
	switch f.Method {
	case mspNotifyTurnStarted:
		if p.TurnID != "" {
			t.mspTurnStartedLocked(p.TurnID)
			// A turn started under a steer's own commandId is that steer,
			// run as a turn of its own.
			t.mspPlaceLocked(p.TurnID, p.TurnID)
		}
	case mspNotifyItemStarted, mspNotifyItemCompleted:
		var ip struct {
			Item struct {
				Kind         string `json:"kind"`
				TurnID       string `json:"turnId"`
				CommandID    string `json:"commandId"`
				MessagePhase string `json:"messagePhase"`
			} `json:"item"`
		}
		if json.Unmarshal(f.Params, &ip) != nil {
			return
		}
		item := ip.Item
		turn := item.TurnID
		if turn == "" {
			turn = st.runningTurnID
		}
		switch {
		case item.Kind == mspItemUserMessage && item.CommandID != "":
			// A steer's own message, in the turn that took it.
			t.mspPlaceLocked(item.CommandID, turn)
		case f.Method == mspNotifyItemCompleted && item.Kind == mspItemAgentMessage &&
			item.MessagePhase == mspPhaseFinalAnswer && turn != "" && turn == st.runningTurnID:
			st.answerDone = true
		}
	case mspNotifyTurnForegroundCompleted:
		if p.TurnID != "" && p.TurnID == st.runningTurnID {
			st.foregroundDone = true
		}
	case mspNotifyTurnCompleted:
		ended := p.TurnID
		if ended == "" {
			ended = st.runningTurnID
		}
		if ended == st.runningTurnID {
			st.runningTurnID, st.foregroundDone, st.answerDone = "", false, false
		}
		// A steer that named this turn and was not seen in it: Muse either
		// runs it as a turn of its own next, or never does.
		for _, w := range st.watches {
			if ended != "" && w.expected == ended && !w.sent && !w.closed {
				w.closed = true
				close(w.ended)
			}
		}
		// The turn's requests end with it; one that named no turn ends with
		// the turn that was running. Another turn's stay open (fail safe:
		// an open request only ever refuses a live write).
		for _, open := range []map[string]string{st.approvals, st.userInputs} {
			for id, turn := range open {
				if turn == ended || (turn == "" && ended != "") {
					delete(open, id)
				}
			}
		}
	case mspNotifyApprovalResolved:
		delete(st.approvals, p.ApprovalID)
	case mspNotifyUserInputSettled:
		delete(st.userInputs, p.UserInputID)
	}
}

// mspAdmitStateLocked is the MSP part of the stdin-boundary fence: an open
// request, no running turn (or one whose final answer is done, or only its
// background checks), another turn.
func (t *liveFollowUpTracker) mspAdmitStateLocked(state *liveWriteState, expectedTurnID string) {
	st := t.msp
	state.ApprovalPending = state.ApprovalPending || len(st.approvals) > 0 || len(st.userInputs) > 0
	state.NoActiveTurn = st.runningTurnID == "" || st.foregroundDone || st.answerDone
	state.TurnMismatch = !state.NoActiveTurn && st.runningTurnID != expectedTurnID
}

// mspPlaceLocked records where the steer whose commandId is commandID ran:
// in the turn it named, or (any other turn) as a turn of its own.
func (t *liveFollowUpTracker) mspPlaceLocked(commandID, turnID string) {
	if commandID == "" || turnID == "" {
		return
	}
	for _, w := range t.msp.watches {
		if w.commandID != commandID || w.sent {
			continue
		}
		p := mspPlacement{processedIn: followUpProcessedInNext, turnID: turnID}
		if turnID == w.expected {
			p.processedIn = followUpProcessedInCurrent
		}
		w.sent = true
		w.placed <- p
	}
}

// mspWatchSteer follows the steer about to be written under rpcID until its
// attempt settles. Called under the session's stdinMu, after the fence
// admitted it and before the write, so no frame about it can come first.
func (t *liveFollowUpTracker) mspWatchSteer(rpcID, commandID, expectedTurnID string) {
	if t == nil || t.msp == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msp.watches[rpcID] = &mspSteerWatch{
		commandID: commandID,
		expected:  expectedTurnID,
		placed:    make(chan mspPlacement, 1),
		ended:     make(chan struct{}),
	}
}

// awaitMSPPlacement waits for where Muse ran the steer written under rpcID,
// until deadline. With afterTurn, it also gives up mspAfterTurnWait after the
// turn the steer named ended without it and no turn of its own started.
// ok=false: not seen.
func (t *liveFollowUpTracker) awaitMSPPlacement(rpcID string, deadline time.Time, afterTurn bool) (mspPlacement, bool) {
	if t == nil || t.msp == nil {
		return mspPlacement{}, false
	}
	t.mu.Lock()
	w := t.msp.watches[rpcID]
	t.mu.Unlock()
	if w == nil {
		return mspPlacement{}, false
	}
	wait := time.Until(deadline)
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var ended <-chan struct{}
	if afterTurn {
		ended = w.ended
	}
	var grace <-chan time.Time
	for {
		select {
		case p := <-w.placed:
			return p, true
		case <-ended:
			ended = nil
			g := time.NewTimer(mspAfterTurnWait)
			defer g.Stop()
			grace = g.C
		case <-grace:
			return mspPlacement{}, false
		case <-timer.C:
			// A placement read in the same instant still counts.
			select {
			case p := <-w.placed:
				return p, true
			default:
			}
			return mspPlacement{}, false
		}
	}
}

// mspSession is the session the Muse host serves ("" before one is known).
func (t *liveFollowUpTracker) mspSession() string {
	if t == nil || t.msp == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msp.sessionID
}

// mspTurn is the turn running on the Muse host and whether it is closed to
// live writes: its final answer has completed or its foreground is done.
func (t *liveFollowUpTracker) mspTurn() (turnID string, closing bool) {
	if t == nil || t.msp == nil {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msp.runningTurnID, t.msp.foregroundDone || t.msp.answerDone
}

// mspPendingLocked reports an open approval or user-input request.
func (t *liveFollowUpTracker) mspPendingLocked() bool {
	return t.msp != nil && (len(t.msp.approvals) > 0 || len(t.msp.userInputs) > 0)
}
