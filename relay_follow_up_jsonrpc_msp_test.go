package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Live follow-ups to Muse Code on its MSP driver (L5 spec §4), against a fake
// `muse serve` over pipes. Muse Code is certified for the test only: the
// shipped liveFollowUpCertified does not list it.

const (
	mspTestSession = "01a1dc92-617d-7450-a3d3-88e5dcc52575"
	mspTestTurn    = "01a1dc92-31ef-7000-8000-000000000001"
	mspOtherTurn   = "01a1dc92-31ef-7000-8000-000000000099"
)

// newMSPLiveHarness starts a Muse Code MSP session on a fake host, through
// the handshake terminal-service sends.
func newMSPLiveHarness(t *testing.T) (*liveFollowUpHarness, *ACPSession) {
	t.Helper()
	pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322", museCodeCliAgentID: museCodeMSPMinVersion})
	h := newLiveFollowUpHarness(t, "msp1")
	m := newACPManager(nil, museCodeMSPProfile)
	prev := globalMuseCodeMSPManager
	globalMuseCodeMSPManager = m
	t.Cleanup(func() { globalMuseCodeMSPManager = prev })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &ACPSession{
		ID: h.sid, Stdin: h.child.stdinW, Stdout: h.child.stdoutR, Stderr: h.child.stderrR,
		status: "running", done: make(chan struct{}), processExited: make(chan struct{}), streamDone: make(chan struct{}),
		firstFrame: make(chan struct{}), launchedVersion: "1.4.20", WorkspaceRoot: root,
		liveFollowUps: newLiveFollowUpTrackerFor(museCodeCliAgentID), fence: newACPClientFence(museCodeMSPProfile),
	}
	m.sessions[h.sid] = s
	h.done = s.streamDone
	go m.readStream(s, h.stream.publish)
	t.Cleanup(h.finish)
	h.child.setRespond(mspHostAnswers(nil))
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"aiexpedite","version":"1"},"capabilities":{"userInputDialogs":false}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","id":"relay-msp-start","method":"session/start","params":{"commandId":"01a1dc92-0000-7000-8000-000000000001","workspaceRoot":` + strconv.Quote(root) + `,"approvalMode":"promptUnmatched"}}`,
	} {
		if err := m.Send(h.sid, frame); err != nil {
			t.Fatal(err)
		}
	}
	waitForLive(t, "the MSP session", func() bool { return s.liveFollowUps.mspSession() == mspTestSession })
	return h, s
}

// mspHostAnswers answers the handshake and every `turn/start` (started, under
// its own commandId), and hands a `turn/steer` to steer (nil: unanswered).
func mspHostAnswers(steer func(f jsonRPCFrame) []string) func(f jsonRPCFrame, line string) []string {
	return func(f jsonRPCFrame, line string) []string {
		id := string(f.ID)
		switch f.Method {
		case mspMethodInitialize:
			return []string{`{"jsonrpc":"2.0","id":` + id + `,"result":{"serverInfo":{"name":"muse","version":"1.4.20"},"sessionDurability":"durable","sandboxAvailability":"unavailable"}}`}
		case mspMethodSessionStart:
			return []string{`{"jsonrpc":"2.0","id":` + id + `,"result":{"session":{"sessionId":"` + mspTestSession + `","status":"idle","approvalMode":{"mode":"promptUnmatched"}}}}`}
		case mspMethodTurnStart:
			var p struct {
				CommandID string `json:"commandId"`
			}
			_ = json.Unmarshal(f.Params, &p)
			return []string{
				`{"jsonrpc":"2.0","id":` + id + `,"result":{"commandId":"` + p.CommandID + `","status":"accepted","turnId":"` + p.CommandID + `","startedNewTurn":true,"disposition":"started"}}`,
				`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"` + p.CommandID + `","sessionId":"` + mspTestSession + `"}}`,
			}
		case mspMethodTurnSteer:
			if steer != nil {
				return steer(f)
			}
		}
		return nil
	}
}

// startMSPTurn writes the turn `mspTestTurn` and leaves it running.
func startMSPTurn(t *testing.T, h *liveFollowUpHarness, s *ACPSession) {
	t.Helper()
	if err := globalMuseCodeMSPManager.Send(h.sid, `{"jsonrpc":"2.0","id":"relay-turn-t1","method":"turn/start","params":{"commandId":"`+mspTestTurn+`","sessionId":"`+mspTestSession+`","ifBusy":"queue","input":[{"type":"text","text":"Count slowly"}]}}`); err != nil {
		t.Fatal(err)
	}
	waitForLive(t, "the running turn", func() bool { turn, _ := s.liveFollowUps.mspTurn(); return turn == mspTestTurn })
}

func (h *liveFollowUpHarness) mspCmd(attemptID, text, expectedTurnID string) commandMsg {
	now := time.Now()
	return commandMsg{
		ID: relayFollowUpEnvelopePrefix + attemptID, Type: relayFollowUpCommandType, SessionID: h.sid,
		Input: text, Ts: now.UnixMilli(), ConversationID: mspTestSession, ExpectedTurnID: expectedTurnID,
		FollowUpID: "m-" + attemptID, FollowUpAttemptID: attemptID,
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(), FollowUpLedgerID: h.ledger.LedgerID(),
	}
}

func mspSteerAnswer(result string) func(f jsonRPCFrame) []string {
	return func(f jsonRPCFrame) []string {
		return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,` + result + `}`}
	}
}

// mspAbsorbed answers a steer as Muse does when the running turn takes it:
// accepted for that turn, then (at the turn's next model-call boundary) the
// steer's own userMessage item in that turn.
func mspAbsorbed(f jsonRPCFrame) []string {
	var p mspSteerParams
	_ = json.Unmarshal(f.Params, &p)
	return []string{mspAcceptedFor(f)[0], mspUserMessage(p.CommandID, p.ExpectedTurnID)}
}

// mspAcceptedFor is only Muse's answer: accepted for the turn the steer named.
func mspAcceptedFor(f jsonRPCFrame) []string {
	var p mspSteerParams
	_ = json.Unmarshal(f.Params, &p)
	return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"commandId":"` + p.CommandID + `","status":"accepted","turnId":"` + p.ExpectedTurnID + `"}}`}
}

// mspUserMessage is a steer's own userMessage item, in turnID.
func mspUserMessage(commandID, turnID string) string {
	return `{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"` + mspTestSession + `","item":{"itemId":"um-` + commandID + `","kind":"userMessage","turnId":"` + turnID + `","status":"completed","commandId":"` + commandID + `","steered":true}}}`
}

// mspTurnEvent is a turn notification for turnID.
func mspTurnEvent(method, turnID string) string {
	return `{"jsonrpc":"2.0","method":"` + method + `","params":{"sessionId":"` + mspTestSession + `","turnId":"` + turnID + `","terminal":"completed"}}`
}

// mspAgentMessageCompleted is an agentMessage item completing in turnID, in phase.
func mspAgentMessageCompleted(turnID, phase string) string {
	return `{"jsonrpc":"2.0","method":"item/completed","params":{"sessionId":"` + mspTestSession + `","item":{"itemId":"am-1","kind":"agentMessage","turnId":"` + turnID + `","status":"completed","text":"done","messagePhase":"` + phase + `"}}}`
}

/* --- contract pins (shared-constants 3.23.580) --- */

func TestMuseMSPWireValuesMatchSharedConstants(t *testing.T) {
	strs := map[string]string{
		mspMethodInitialize: "initialize", mspMethodInitialized: "initialized", mspMethodSessionStart: "session/start",
		mspMethodSessionResume: "session/resume", mspMethodSessionSetApprovalMode: "session/setApprovalMode",
		mspMethodSessionSetModel: "session/setModel", mspMethodSessionSetEffort: "session/setReasoningEffort",
		mspMethodTurnStart: "turn/start", mspMethodTurnSteer: "turn/steer", mspMethodTurnInterrupt: "turn/interrupt",
		mspMethodApprovalDecide: "approval/decide", mspMethodApprovalListPending: "approval/listPending",
		mspMethodModelList: "model/list", mspMethodUsageRead: "usage/read",
		mspNotifyTurnStarted: "turn/started", mspNotifyTurnForegroundCompleted: "turn/foregroundCompleted",
		mspNotifyTurnCompleted: "turn/completed", mspNotifyApprovalResolved: "approval/resolved", mspNotifyUsageChanged: "usage/changed",
		mspServerRequestApproval: "approval/request", mspServerRequestUserInput: "userInput/request",
		mspRejectMissingRun: "missing_run", mspRejectAlreadyTerminal: "already_terminal", mspRejectCommandIDConflict: "command_id_conflict",
		mspApprovalApproved: "approved", mspApprovalDenied: "denied", mspApprovalScopeOnce: "once",
		mspIfBusyQueue: "queue", mspTurnTerminalCompleted: "completed",
	}
	for got, want := range strs {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	codes := map[int64]int64{
		mspErrInvalidParams: -32602, mspErrMethodNotFound: -32601, mspErrOverloaded: -32001, mspErrSessionNotFound: -32020,
		mspErrSessionInUse: -32021, mspErrSessionNotLoad: -32024, mspErrCommandRejected: -32030, mspErrBackpressured: -32031,
	}
	for got, want := range codes {
		if got != want {
			t.Errorf("code %d, want %d", got, want)
		}
	}
}

// The steer's commandId is shared-constants mspFollowUpCommandId, on the
// vectors both suites pin (testdata/mspFollowUpCommandIdVectors.json, a byte
// for byte copy of shared-constants tests/fixtures/).
func TestMSPFollowUpCommandIDMatchesSharedConstantsVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/mspFollowUpCommandIdVectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			AttemptID  string `json:"attemptId"`
			SignedTsMs int64  `json:"signedTsMs"`
			CommandID  string `json:"commandId"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Vectors) < 5 {
		t.Fatalf("vectors: %v", err)
	}
	for _, v := range fixture.Vectors {
		got, ok := mspFollowUpCommandID(v.AttemptID, v.SignedTsMs)
		if !ok || got != v.CommandID {
			t.Errorf("mspFollowUpCommandID(%q, %d) = %q, want %q", v.AttemptID, v.SignedTsMs, got, v.CommandID)
		}
		if !museCodeSessionIDPattern.MatchString(got) || got[14] != '7' || !containsString([]string{"8", "9", "a", "b"}, got[19:20]) {
			t.Errorf("%q is not a UUIDv7", got)
		}
	}
	for _, bad := range []struct {
		id string
		ts int64
	}{{"", 1}, {"a", -1}, {"a", 1 << 48}} {
		if got, ok := mspFollowUpCommandID(bad.id, bad.ts); ok {
			t.Errorf("(%q, %d) = %q", bad.id, bad.ts, got)
		}
	}
}

func TestClassifyMSPSteerAnswer(t *testing.T) {
	err := func(code int, reason string) string {
		return `{"jsonrpc":"2.0","id":"r","error":{"code":` + strconv.Itoa(code) + `,"message":"m","data":{"kind":"k","reason":"` + reason + `"}}}`
	}
	cases := []struct {
		name, frame, state, reason string
	}{
		{"absorbed by the running turn", `{"id":"r","result":{"commandId":"c","status":"accepted","turnId":"` + mspTestTurn + `"}}`, followUpRowWritten, ""},
		{"accepted under its own commandId (S10)", `{"id":"r","result":{"commandId":"c","status":"accepted","turnId":"c"}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{"another status", `{"id":"r","result":{"status":"queued","turnId":"` + mspTestTurn + `"}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{"an empty result", `{"id":"r","result":{}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{"missing_run", err(-32030, "missing_run"), followUpRowRefused, followUpReasonNoActiveTurn},
		{"already_terminal", err(-32030, "already_terminal"), followUpRowRefused, followUpReasonNoActiveTurn},
		{"command_id_conflict", err(-32030, "command_id_conflict"), followUpRowUnknown, ""},
		{"another rejection", err(-32030, "something_new"), followUpRowRefused, followUpReasonCliRefused},
		{"invalid params", err(-32602, ""), followUpRowRefused, followUpReasonCliRefused},
		{"no turn/steer", err(-32601, ""), followUpRowRefused, followUpReasonMethodNotSupported},
		{"session not found", err(-32020, ""), followUpRowRefused, followUpReasonConversationMismatch},
		{"session not loaded", err(-32024, ""), followUpRowRefused, followUpReasonConversationMismatch},
		{"session in use", err(-32021, ""), followUpRowRefused, followUpReasonCliRefused},
		{"backpressured", err(-32031, ""), followUpRowRefused, followUpReasonCliRefused},
		{"overloaded", err(-32001, ""), followUpRowRefused, followUpReasonCliRefused},
		{"internal", err(-32603, ""), followUpRowUnknown, ""},
		{"an approval error", err(-32050, ""), followUpRowUnknown, ""},
		{"not JSON", `nope`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
	}
	for _, c := range cases {
		if v := classifyMSPSteerAnswer(c.frame, mspTestTurn); v.State != c.state || v.Reason != c.reason {
			t.Errorf("%s: %+v, want {%s %s}", c.name, v, c.state, c.reason)
		}
	}
	if v := classifyMSPSteerAnswer(`{"id":"r","result":{"status":"accepted","turnId":""}}`, ""); v.State == followUpRowWritten {
		t.Error("an answer is written with no running turn to compare against")
	}
}

/* --- live, over a fake host --- */

func TestAMuseCodeLiveFollowUpWritesOneSteerAndIsWrittenOnItsAnswer(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(mspAbsorbed))
	cmd := h.mspCmd("msp-1", "When you reach 30, also write BANANA.", mspTestTurn)
	h.handle(cmd)
	h.handle(cmd)
	steers := h.child.framesWithMethod(mspMethodTurnSteer)
	if len(steers) != 1 {
		t.Fatalf("want exactly one turn/steer, got %d", len(steers))
	}
	var p mspSteerParams
	if err := json.Unmarshal(steers[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	want, _ := mspFollowUpCommandID("msp-1", cmd.Ts)
	if id, _ := steers[0].stringID(); id != "relay-followup-msp-1" || p.CommandID != want || p.SessionID != mspTestSession ||
		p.ExpectedTurnID != mspTestTurn || len(p.Input) != 1 || p.Input[0].Type != "text" || p.Input[0].Text != cmd.Input {
		t.Fatalf("steer = id %s %+v (commandId want %s)", id, p, want)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	wantAck(t, acks[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	for _, a := range acks {
		if a.FollowUpMSPTurnID != mspTestTurn {
			t.Fatalf("followUpMspTurnId = %q, want the turn it joined %q", a.FollowUpMSPTurnID, mspTestTurn)
		}
	}
}

// Muse acks a steer for the running turn, finishes that turn without it, and
// runs it as the next turn under the steer's own commandId (certification,
// 2026-11-16: every steer sent at a turn's start that used no tool, and
// every one sent while the final answer streamed). The device answers only
// once it sees that: written, `next`, naming that turn; never `current`.
func TestAMuseCodeSteerMuseRunsAsTheNextTurnIsWrittenNextWithThatTurn(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(mspAcceptedFor))
	cmd := h.mspCmd("msp-next", "Also write STEERED.", mspTestTurn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handle(cmd)
	}()
	waitForLive(t, "Muse's answer", func() bool { return s.liveFollowUps.mspWatched("relay-followup-msp-next") })
	time.Sleep(50 * time.Millisecond)
	// Accepted, not yet seen running: in flight, so a redelivery and a query
	// publish nothing.
	h.handle(cmd)
	h.query("msp-next")
	if acks := h.acks.snapshot(); len(acks) != 0 {
		t.Fatalf("answered before the steer was seen running: %+v", acks)
	}
	commandID, _ := mspFollowUpCommandID("msp-next", cmd.Ts)
	h.child.emit(mspAgentMessageCompleted(mspTestTurn, mspPhaseFinalAnswer))
	h.child.emit(mspTurnEvent(mspNotifyTurnCompleted, mspTestTurn))
	h.child.emit(mspTurnEvent(mspNotifyTurnStarted, commandID))
	h.child.emit(mspUserMessage(commandID, commandID))
	<-done
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInNext)
	if acks[0].FollowUpMSPTurnID != commandID {
		t.Fatalf("followUpMspTurnId = %q, want the steer's own turn %q", acks[0].FollowUpMSPTurnID, commandID)
	}
	// The ledger answers a redelivery the same, turn included.
	h.handle(cmd)
	if a := h.acks.snapshot()[1]; a.FollowUpOutcome != followUpOutcomeWritten || a.FollowUpProcessedIn != followUpProcessedInNext || a.FollowUpMSPTurnID != commandID {
		t.Fatalf("redelivery answered %+v", a)
	}
	if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 1 {
		t.Fatalf("%d steers written", n)
	}
}

// Seen only as the steer's userMessage in another turn (no turn/started read
// for it): still the next turn, that one.
func TestAMuseCodeSteerSeenInAnotherTurnIsWrittenNext(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(func(f jsonRPCFrame) []string {
		var p mspSteerParams
		_ = json.Unmarshal(f.Params, &p)
		return []string{mspAcceptedFor(f)[0], mspTurnEvent(mspNotifyTurnCompleted, mspTestTurn), mspUserMessage(p.CommandID, mspOtherTurn)}
	}))
	h.handle(h.mspCmd("msp-other", "x", mspTestTurn))
	a := h.acks.snapshot()[0]
	wantAck(t, a, followUpOutcomeWritten, "", followUpProcessedInNext)
	if a.FollowUpMSPTurnID != mspOtherTurn {
		t.Fatalf("followUpMspTurnId = %q", a.FollowUpMSPTurnID)
	}
}

// The turn the steer named ends without it and Muse starts no turn of its
// own (what Muse did with every steer it dropped): unknown, never written.
func TestAMuseCodeSteerNeverSeenRunningIsUnknownUnplaced(t *testing.T) {
	prev := mspAfterTurnWait
	mspAfterTurnWait = 50 * time.Millisecond
	t.Cleanup(func() { mspAfterTurnWait = prev })
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(func(f jsonRPCFrame) []string {
		return []string{mspAcceptedFor(f)[0], mspTurnEvent(mspNotifyTurnCompleted, mspTestTurn)}
	}))
	cmd := h.mspCmd("msp-lost", "x", mspTestTurn)
	h.handle(cmd)
	a := h.acks.snapshot()[0]
	wantAck(t, a, followUpOutcomeUnknown, followUpReasonUnplaced, "")
	if a.FollowUpMSPTurnID != "" {
		t.Fatalf("an unplaced steer named a turn: %q", a.FollowUpMSPTurnID)
	}
	// Seen running after all, while the broker can still take it: lifted.
	commandID, _ := mspFollowUpCommandID("msp-lost", cmd.Ts)
	h.child.emit(mspTurnEvent(mspNotifyTurnStarted, commandID))
	waitForLive(t, "the late placement", func() bool { return len(h.acks.snapshot()) == 2 })
	b := h.acks.snapshot()[1]
	wantAck(t, b, followUpOutcomeWritten, "", followUpProcessedInNext)
	if b.FollowUpMSPTurnID != commandID {
		t.Fatalf("late followUpMspTurnId = %q", b.FollowUpMSPTurnID)
	}
}

// The host exits after accepting a steer, before it is seen running.
func TestAMuseCodeSteerWhoseHostExitsBeforeItRunsIsUnknown(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(mspAcceptedFor))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handle(h.mspCmd("msp-exit", "x", mspTestTurn))
	}()
	waitForLive(t, "Muse's answer", func() bool { return s.liveFollowUps.mspWatched("relay-followup-msp-exit") })
	time.Sleep(50 * time.Millisecond)
	s.liveFollowUps.readerDone(false)
	<-done
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeUnknown, followUpReasonNoAnswer, "")
}

func TestEachMuseCodeSteerAnswerSettlesTheAttempt(t *testing.T) {
	cases := []struct {
		name    string
		answer  func(f jsonRPCFrame) []string
		outcome string
		reason  string
	}{
		{"accepted under its own commandId", func(f jsonRPCFrame) []string {
			var p mspSteerParams
			_ = json.Unmarshal(f.Params, &p)
			return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"commandId":"` + p.CommandID + `","status":"accepted","turnId":"` + p.CommandID + `"}}`}
		}, followUpOutcomeUnknown, followUpReasonAnswerUnrecognised},
		{"missing_run", mspSteerAnswer(`"error":{"code":-32030,"message":"rejected","data":{"kind":"commandRejected","reason":"missing_run"}}`), followUpOutcomeNotWritten, followUpReasonNoActiveTurn},
		{"already_terminal", mspSteerAnswer(`"error":{"code":-32030,"message":"rejected","data":{"kind":"commandRejected","reason":"already_terminal"}}`), followUpOutcomeNotWritten, followUpReasonNoActiveTurn},
		{"command_id_conflict", mspSteerAnswer(`"error":{"code":-32030,"message":"rejected","data":{"kind":"commandRejected","reason":"command_id_conflict"}}`), followUpOutcomeUnknown, ""},
		{"backpressured", mspSteerAnswer(`"error":{"code":-32031,"message":"capacity","data":{"kind":"backpressured","retryable":true}}`), followUpOutcomeNotWritten, followUpReasonCliRefused},
		{"an older build", mspSteerAnswer(`"error":{"code":-32601,"message":"method not found"}`), followUpOutcomeNotWritten, followUpReasonMethodNotSupported},
		{"session not found", mspSteerAnswer(`"error":{"code":-32020,"message":"session not found","data":{"kind":"sessionNotFound"}}`), followUpOutcomeNotWritten, followUpReasonConversationMismatch},
		{"internal", mspSteerAnswer(`"error":{"code":-32603,"message":"boom"}`), followUpOutcomeUnknown, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, s := newMSPLiveHarness(t)
			startMSPTurn(t, h, s)
			h.child.setRespond(mspHostAnswers(c.answer))
			cmd := h.mspCmd(fmt.Sprintf("msp-e%d", i), "x", mspTestTurn)
			h.handle(cmd)
			h.handle(cmd)
			if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 1 {
				t.Fatalf("%d steers written", n)
			}
			acks := h.acks.snapshot()
			if len(acks) != 2 {
				t.Fatalf("acks %+v", acks)
			}
			wantAck(t, acks[0], c.outcome, c.reason, "")
			if acks[1].FollowUpOutcome != c.outcome {
				t.Fatalf("the redelivery was answered %s", acks[1].FollowUpOutcome)
			}
		})
	}
}

func TestAMuseCodeSteerWithNoAnswerIsUnknownThenLiftedByALateSuccess(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 50*time.Millisecond)
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.handle(h.mspCmd("msp-late", "text", mspTestTurn))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeUnknown, followUpReasonNoAnswer, "")
	steer := h.child.framesWithMethod(mspMethodTurnSteer)[0]
	// Muse's late answer alone says nothing of where the steer runs.
	h.child.emit(mspAcceptedFor(steer)[0])
	time.Sleep(100 * time.Millisecond)
	if n := len(h.acks.snapshot()); n != 1 {
		t.Fatalf("a late answer lifted the attempt before the steer was seen running: %+v", h.acks.snapshot())
	}
	for _, line := range mspAbsorbed(steer)[1:] {
		h.child.emit(line)
	}
	waitForLive(t, "the late upgrade", func() bool { return len(h.acks.snapshot()) == 2 })
	wantAck(t, h.acks.snapshot()[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	if got := h.acks.snapshot()[1].FollowUpMSPTurnID; got != mspTestTurn {
		t.Fatalf("followUpMspTurnId = %q, want %q", got, mspTestTurn)
	}
}

// A late answer that names another turn never lifts the attempt.
func TestALateMuseCodeAnswerNamingAnotherTurnStaysUnknown(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 50*time.Millisecond)
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.handle(h.mspCmd("msp-late2", "text", mspTestTurn))
	h.child.emit(`{"jsonrpc":"2.0","id":"relay-followup-msp-late2","result":{"status":"accepted","turnId":"` + mspOtherTurn + `"}}`)
	time.Sleep(100 * time.Millisecond)
	if acks := h.acks.snapshot(); len(acks) != 1 {
		t.Fatalf("a late answer for another turn lifted the attempt: %+v", acks)
	}
}

// The device writes nothing where Muse would drop it or a person is deciding.
func TestAMuseCodeFollowUpIsNotWrittenWhereItCouldNotLand(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(h *liveFollowUpHarness, s *ACPSession)
		expected string
		reason   string
	}{
		{"no turn running", func(h *liveFollowUpHarness, s *ACPSession) {}, mspTestTurn, followUpReasonNoActiveTurn},
		{"after the foreground completed (S10)", func(h *liveFollowUpHarness, s *ACPSession) {
			startMSPTurn(t, h, s)
			h.child.emit(`{"jsonrpc":"2.0","method":"turn/foregroundCompleted","params":{"turnId":"` + mspTestTurn + `","sessionId":"` + mspTestSession + `"}}`)
			waitForLive(t, "the foreground", func() bool { _, done := s.liveFollowUps.mspTurn(); return done })
		}, mspTestTurn, followUpReasonNoActiveTurn},
		{"after the final answer completed", func(h *liveFollowUpHarness, s *ACPSession) {
			startMSPTurn(t, h, s)
			h.child.emit(mspAgentMessageCompleted(mspTestTurn, mspPhaseFinalAnswer))
			waitForLive(t, "the final answer", func() bool { _, done := s.liveFollowUps.mspTurn(); return done })
		}, mspTestTurn, followUpReasonNoActiveTurn},
		{"after the turn completed", func(h *liveFollowUpHarness, s *ACPSession) {
			startMSPTurn(t, h, s)
			h.child.emit(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"` + mspTestTurn + `","terminal":"completed"}}`)
			waitForLive(t, "the turn's end", func() bool { turn, _ := s.liveFollowUps.mspTurn(); return turn == "" })
		}, mspTestTurn, followUpReasonNoActiveTurn},
		{"another turn running", func(h *liveFollowUpHarness, s *ACPSession) { startMSPTurn(t, h, s) }, mspOtherTurn, followUpReasonTurnMismatch},
		{"an approval open", func(h *liveFollowUpHarness, s *ACPSession) {
			startMSPTurn(t, h, s)
			h.child.emit(`{"jsonrpc":"2.0","id":7,"method":"approval/request","params":{"approvalId":"apr-1","turnId":"` + mspTestTurn + `","availableChoices":[]}}`)
			waitForLive(t, "the approval", s.liveFollowUps.approvalPending)
		}, mspTestTurn, followUpReasonApprovalPending},
		{"a question open", func(h *liveFollowUpHarness, s *ACPSession) {
			startMSPTurn(t, h, s)
			h.child.emit(`{"jsonrpc":"2.0","id":8,"method":"userInput/request","params":{"userInputId":"ui-1","turnId":"` + mspTestTurn + `"}}`)
			waitForLive(t, "the question", s.liveFollowUps.approvalPending)
		}, mspTestTurn, followUpReasonApprovalPending},
		{"no expected turn signed", func(h *liveFollowUpHarness, s *ACPSession) { startMSPTurn(t, h, s) }, "", followUpReasonConversationMismatch},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, s := newMSPLiveHarness(t)
			c.setup(h, s)
			h.child.setRespond(mspHostAnswers(mspAbsorbed))
			h.handle(h.mspCmd(fmt.Sprintf("msp-f%d", i), "x", c.expected))
			if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 0 {
				t.Fatalf("%d steers written", n)
			}
			wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, c.reason, "")
		})
	}
}

// An approval closes on approval/resolved: a later attempt in the same turn is
// written.
func TestAResolvedMuseCodeApprovalNoLongerHoldsFollowUps(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(mspAbsorbed))
	h.child.emit(`{"jsonrpc":"2.0","id":7,"method":"approval/request","params":{"approvalId":"apr-1","turnId":"` + mspTestTurn + `","availableChoices":[]}}`)
	waitForLive(t, "the approval", s.liveFollowUps.approvalPending)
	h.handle(h.mspCmd("msp-a1", "x", mspTestTurn))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
	// A newer stage of the same approval replaces it; its resolution clears it.
	h.child.emit(`{"jsonrpc":"2.0","id":9,"method":"approval/request","params":{"approvalId":"apr-1","turnId":"` + mspTestTurn + `","availableChoices":[]}}`)
	h.child.emit(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"apr-1","resolvedBy":"user"}}`)
	waitForLive(t, "the resolution", func() bool { return !s.liveFollowUps.approvalPending() })
	h.handle(h.mspCmd("msp-a2", "x", mspTestTurn))
	wantAck(t, h.acks.snapshot()[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
}

// A follow-up naming another conversation is refused, nothing written.
func TestAMuseCodeFollowUpToAnotherConversationIsRefused(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	cmd := h.mspCmd("msp-c", "x", mspTestTurn)
	cmd.ConversationID = "01a1dc92-0000-7000-8000-00000000dead"
	h.handle(cmd)
	if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 0 {
		t.Fatalf("%d steers written", n)
	}
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonConversationMismatch, "")
}

// Empty text never goes out (Muse would refuse it as invalid params).
func TestAnEmptyMuseCodeFollowUpIsRefusedBeforeItIsWritten(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.handle(h.mspCmd("msp-empty", "  \n", mspTestTurn))
	if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 0 {
		t.Fatalf("%d steers written", n)
	}
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonCliRefused, "")
}

// L1/L2's in-flight rule: a redelivery while the answer is pending publishes
// nothing; the first delivery answers it.
func TestAMuseCodeFollowUpRedeliveredWhileItsAnswerIsPendingPublishesNothing(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	h.child.setRespond(mspHostAnswers(nil))
	cmd := h.mspCmd("msp-w", "wait for it", mspTestTurn)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handle(cmd)
	}()
	waitForLive(t, "the steer", func() bool { return len(h.child.framesWithMethod(mspMethodTurnSteer)) == 1 })
	h.handle(cmd)
	h.query("msp-w")
	if acks := h.acks.snapshot(); len(acks) != 0 {
		t.Fatalf("an in-flight attempt was answered from the ledger: %+v", acks)
	}
	for _, line := range mspAbsorbed(h.child.framesWithMethod(mspMethodTurnSteer)[0]) {
		h.child.emit(line)
	}
	<-done
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
}

// As shipped, Muse Code is not certified: a session gets no tracker and every
// live attempt is refused notLiveKind, nothing written.
func TestAnUncertifiedMuseCodeSessionTakesNoLiveFollowUp(t *testing.T) {
	h, s := newMSPLiveHarness(t)
	startMSPTurn(t, h, s)
	pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322"})
	s.liveFollowUps = newLiveFollowUpTracker(museCodeCliAgentID, s.launchedVersion)
	if s.liveFollowUps != nil {
		t.Fatal("an uncertified Muse session got a tracker")
	}
	h.handle(h.mspCmd("msp-u", "x", mspTestTurn))
	if n := len(h.child.framesWithMethod(mspMethodTurnSteer)); n != 0 {
		t.Fatalf("%d steers written", n)
	}
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNotLiveKind, "")
}

// The tracker's own reading of the host: the session from the setup answer,
// the turn from the ack (or turn/started), cleared by its own turn/completed.
func TestTheMSPTrackerFollowsTheHost(t *testing.T) {
	tr := newLiveFollowUpTrackerFor(museCodeCliAgentID)
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"sessionId":"` + mspTestSession + `"}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-msp-resume","error":{"code":-32020,"message":"gone"}}`)
	if tr.mspSession() != "" {
		t.Fatal("a refused resume named the session")
	}
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-msp-start","method":"session/start","params":{}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-msp-start","result":{"session":{"sessionId":"` + mspTestSession + `"}}}`)
	if tr.mspSession() != mspTestSession {
		t.Fatal("the start's answer did not name the session")
	}
	// A queued turn does not run until turn/started says so.
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-turn-q","method":"turn/start","params":{}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-turn-q","result":{"turnId":"q","disposition":"queued"}}`)
	if turn, _ := tr.mspTurn(); turn != "" {
		t.Fatalf("a queued turn reads as running: %q", turn)
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"q"}}`)
	if turn, _ := tr.mspTurn(); turn != "q" {
		t.Fatalf("turn/started: %q", turn)
	}
	// Another turn's completion leaves it running.
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"other"}}`)
	if turn, _ := tr.mspTurn(); turn != "q" {
		t.Fatal("another turn's completion ended this one")
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/foregroundCompleted","params":{"turnId":"other"}}`)
	if _, done := tr.mspTurn(); done {
		t.Fatal("another turn's foreground ended this one's")
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"q"}}`)
	if turn, _ := tr.mspTurn(); turn != "" {
		t.Fatal("the turn's own completion did not end it")
	}
	// A turn/start whose write failed is forgotten.
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-turn-x","method":"turn/start","params":{}}`).done(false)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-turn-x","result":{"turnId":"x","disposition":"started"}}`)
	if turn, _ := tr.mspTurn(); turn != "" {
		t.Fatal("an unwritten turn/start's answer started a turn")
	}
	// The host's exit closes everything.
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"y"}}`)
	tr.inbound(`{"jsonrpc":"2.0","id":3,"method":"approval/request","params":{"approvalId":"a","turnId":"y"}}`)
	tr.readerDone(false)
	if turn, _ := tr.mspTurn(); turn != "" || tr.approvalPending() {
		t.Fatal("state survived the host's exit")
	}
}

// The stdin-boundary half of the fence, on the tracker itself: whatever the
// earlier check saw, nothing is admitted once the foreground is done, into
// another turn, or beside an open request; an admitted write keeps the turn it
// named for reading Muse's answer.
func TestTheMSPTrackerAdmitsALiveWriteOnlyIntoTheRunningTurn(t *testing.T) {
	fence := func(s liveWriteState) string {
		c := okCheck(time.Now())
		c.SessionRunning = !s.Closed
		c.ApprovalPending = s.ApprovalPending
		c.NoActiveTurn = s.NoActiveTurn
		c.TurnMismatch = s.TurnMismatch
		return c.fenceReason()
	}
	tr := newLiveFollowUpTrackerFor(museCodeCliAgentID)
	if ch, reason := tr.admitLiveWriteFor("relay-followup-a", "a", "t", mspTestTurn, fence); ch != nil || reason != followUpReasonNoActiveTurn {
		t.Fatalf("no turn: %q", reason)
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/started","params":{"turnId":"` + mspTestTurn + `"}}`)
	if ch, reason := tr.admitLiveWriteFor("relay-followup-b", "b", "t", mspOtherTurn, fence); ch != nil || reason != followUpReasonTurnMismatch {
		t.Fatalf("another turn: %q", reason)
	}
	tr.inbound(`{"jsonrpc":"2.0","id":4,"method":"approval/request","params":{"approvalId":"apr","turnId":"` + mspTestTurn + `"}}`)
	if ch, reason := tr.admitLiveWriteFor("relay-followup-c", "c", "t", mspTestTurn, fence); ch != nil || reason != followUpReasonApprovalPending {
		t.Fatalf("beside an approval: %q", reason)
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"apr"}}`)
	ch, reason := tr.admitLiveWriteFor("relay-followup-d", "d", "t", mspTestTurn, fence)
	if ch == nil || reason != "" {
		t.Fatalf("into the running turn: %q", reason)
	}
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-followup-d","result":{"status":"accepted","turnId":"` + mspTestTurn + `"}}`)
	if a := <-ch; a.ExpectedTurnID != mspTestTurn || classifyLiveFollowUpAnswerFor(museCodeCliAgentID, a).State != followUpRowWritten {
		t.Fatalf("answer %+v", a)
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"turn/foregroundCompleted","params":{"turnId":"` + mspTestTurn + `"}}`)
	if ch, reason := tr.admitLiveWriteFor("relay-followup-e", "e", "t", mspTestTurn, fence); ch != nil || reason != followUpReasonNoActiveTurn {
		t.Fatalf("after the foreground: %q", reason)
	}
}

// Muse dropped every steer it read after the turn's final answer had
// completed and before `turn/foregroundCompleted` (2026-11-16, 4 of 10):
// from the final answer's item/completed the turn takes no live write. A
// commentary message, or another turn's final answer, closes nothing; the
// next turn opens again.
func TestTheMSPTrackerClosesTheTurnAtItsFinalAnswer(t *testing.T) {
	fence := func(s liveWriteState) string {
		c := okCheck(time.Now())
		c.SessionRunning = !s.Closed
		c.ApprovalPending = s.ApprovalPending
		c.NoActiveTurn = s.NoActiveTurn
		c.TurnMismatch = s.TurnMismatch
		return c.fenceReason()
	}
	tr := newLiveFollowUpTrackerFor(museCodeCliAgentID)
	tr.inbound(mspTurnEvent(mspNotifyTurnStarted, mspTestTurn))
	tr.inbound(mspAgentMessageCompleted(mspTestTurn, "commentary"))
	tr.inbound(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"kind":"agentMessage","turnId":"` + mspTestTurn + `","messagePhase":"finalAnswer"}}}`)
	tr.inbound(mspAgentMessageCompleted(mspOtherTurn, mspPhaseFinalAnswer))
	if ch, reason := tr.admitLiveWriteFor("relay-followup-a", "a", "t", mspTestTurn, fence); ch == nil || reason != "" {
		t.Fatalf("while the final answer streams: %q", reason)
	}
	tr.forget("relay-followup-a")
	tr.inbound(mspAgentMessageCompleted(mspTestTurn, mspPhaseFinalAnswer))
	if _, closing := tr.mspTurn(); !closing {
		t.Fatal("the final answer did not close the turn")
	}
	if ch, reason := tr.admitLiveWriteFor("relay-followup-b", "b", "t", mspTestTurn, fence); ch != nil || reason != followUpReasonNoActiveTurn {
		t.Fatalf("after the final answer: %q", reason)
	}
	tr.inbound(mspTurnEvent(mspNotifyTurnCompleted, mspTestTurn))
	tr.inbound(mspTurnEvent(mspNotifyTurnStarted, mspOtherTurn))
	if ch, reason := tr.admitLiveWriteFor("relay-followup-c", "c", "t", mspOtherTurn, fence); ch == nil || reason != "" {
		t.Fatalf("the next turn: %q", reason)
	}
}

// Where a steer ran, from Muse's frames: its userMessage in the named turn
// (or naming no turn, while that one runs), a turn started under its
// commandId, or the named turn ending without it.
func TestTheMSPTrackerPlacesASteerFromWhatMuseDoes(t *testing.T) {
	const cmdA, cmdB, cmdC = "01a1dc92-aaaa-7000-8000-00000000000a", "01a1dc92-bbbb-7000-8000-00000000000b", "01a1dc92-cccc-7000-8000-00000000000c"
	tr := newLiveFollowUpTrackerFor(museCodeCliAgentID)
	tr.inbound(mspTurnEvent(mspNotifyTurnStarted, mspTestTurn))
	tr.mspWatchSteer("a", cmdA, mspTestTurn)
	tr.mspWatchSteer("b", cmdB, mspTestTurn)
	tr.mspWatchSteer("c", cmdC, mspTestTurn)
	far := time.Now().Add(time.Minute)
	tr.inbound(`{"jsonrpc":"2.0","method":"item/started","params":{"item":{"kind":"userMessage","commandId":"` + cmdA + `"}}}`)
	if p, ok := tr.awaitMSPPlacement("a", far, true); !ok || p.processedIn != followUpProcessedInCurrent || p.turnID != mspTestTurn {
		t.Fatalf("a: %+v %v", p, ok)
	}
	// Another steer's message places nothing for b; its own completion repeated places once.
	tr.inbound(mspUserMessage(cmdA, mspTestTurn))
	tr.inbound(mspTurnEvent(mspNotifyTurnCompleted, mspTestTurn))
	tr.inbound(mspTurnEvent(mspNotifyTurnStarted, cmdB))
	if p, ok := tr.awaitMSPPlacement("b", far, true); !ok || p.processedIn != followUpProcessedInNext || p.turnID != cmdB {
		t.Fatalf("b: %+v %v", p, ok)
	}
	prev := mspAfterTurnWait
	mspAfterTurnWait = 20 * time.Millisecond
	t.Cleanup(func() { mspAfterTurnWait = prev })
	if p, ok := tr.awaitMSPPlacement("c", far, true); ok {
		t.Fatalf("c was placed: %+v", p)
	}
	// Without the after-turn rule it waits to the deadline.
	start := time.Now()
	if _, ok := tr.awaitMSPPlacement("c", time.Now().Add(60*time.Millisecond), false); ok || time.Since(start) < 50*time.Millisecond {
		t.Fatal("c: gave up before its deadline")
	}
	tr.forget("c")
	if tr.mspWatched("c") {
		t.Fatal("forget kept the watch")
	}
}

// mspWatched reports whether the tracker still follows the steer written
// under rpcID.
func (t *liveFollowUpTracker) mspWatched(rpcID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.msp != nil && t.msp.watches[rpcID] != nil
}
