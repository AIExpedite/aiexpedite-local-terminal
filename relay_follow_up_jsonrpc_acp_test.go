package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Live follow-ups on the ACP drivers (L3/L4 spec §2.6), against a fake CLI
// over pipes. The kinds are certified for the test only: the shipped
// liveFollowUpCertified lists neither.

const acpTestSession = "ses_1"

// newACPLiveHarness starts a session of profile on a fake child. steering is
// what the fake OpenCode's initialize answer advertises.
func newACPLiveHarness(t *testing.T, profile *acpAgentProfile, steering bool) (*liveFollowUpHarness, *ACPSession) {
	t.Helper()
	version := "2.2.7"
	if profile == antigravityACPProfile {
		version = "1.5.4"
	}
	pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322", profile.cliAgentID: version})
	h := newLiveFollowUpHarness(t, "acp1")
	m := newACPManager(nil, profile)
	prevOC, prevAG := globalOpenCodeACPManager, globalAntigravityACPManager
	if profile == openCodeACPProfile {
		globalOpenCodeACPManager = m
	} else {
		globalAntigravityACPManager = m
	}
	t.Cleanup(func() { globalOpenCodeACPManager, globalAntigravityACPManager = prevOC, prevAG })
	s := &ACPSession{
		ID: h.sid, Stdin: h.child.stdinW, Stdout: h.child.stdoutR, Stderr: h.child.stderrR,
		status: "running", done: make(chan struct{}), processExited: make(chan struct{}), streamDone: make(chan struct{}),
		firstFrame: make(chan struct{}), launchedVersion: version,
		liveFollowUps: newLiveFollowUpTrackerFor(profile.cliAgentID), fence: newACPClientFence(profile),
	}
	m.sessions[h.sid] = s
	h.done = s.streamDone
	go m.readStream(s, h.stream.publish)
	t.Cleanup(h.finish)
	initResult := `{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`
	if steering {
		initResult = `{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"_meta":{"steering":{"supported":true}}}`
	}
	h.child.setRespond(func(f jsonRPCFrame, line string) []string {
		switch f.Method {
		case acpMethodInitialize:
			return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":` + initResult + `}`}
		case acpMethodSessionNew:
			return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"sessionId":"` + acpTestSession + `"}}`}
		}
		return nil
	})
	if err := m.Send(h.sid, `{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{"protocolVersion":1}}`); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(h.sid, `{"jsonrpc":"2.0","id":"relay-acp-new","method":"session/new","params":{"mcpServers":[]}}`); err != nil {
		t.Fatal(err)
	}
	waitForLive(t, "the ACP session", func() bool { return s.liveFollowUps.acpSession() == acpTestSession })
	if steering {
		waitForLive(t, "the steering advertisement", s.liveFollowUps.steeringAdvertised)
	}
	return h, s
}

// startACPPrompt writes the client prompt `relay-turn-t1` and leaves it running.
func startACPPrompt(t *testing.T, m *ACPManager, h *liveFollowUpHarness, s *ACPSession) {
	t.Helper()
	if err := m.Send(h.sid, `{"jsonrpc":"2.0","id":"relay-turn-t1","method":"session/prompt","params":{"sessionId":"`+acpTestSession+`","prompt":[{"type":"text","text":"Count slowly"}]}}`); err != nil {
		t.Fatal(err)
	}
	waitForLive(t, "the running prompt", s.liveFollowUps.clientTurnRunning)
}

func (h *liveFollowUpHarness) acpCmd(attemptID, text string) commandMsg {
	now := time.Now()
	return commandMsg{
		ID: relayFollowUpEnvelopePrefix + attemptID, Type: relayFollowUpCommandType, SessionID: h.sid,
		Input: text, Ts: now.UnixMilli(), ConversationID: acpTestSession,
		FollowUpID: "m-" + attemptID, FollowUpAttemptID: attemptID,
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(), FollowUpLedgerID: h.ledger.LedgerID(),
	}
}

// messageWithID is the first published MESSAGE frame whose CLI line answers id.
func (h *liveFollowUpHarness) messageWithID(typ, id string) (resultMsg, bool) {
	for _, f := range h.stream.snapshot() {
		if f.Type != typ {
			continue
		}
		if rpc, ok := parseJSONRPCFrame(f.Output); ok && rpc.isResponse() {
			if got, _ := rpc.stringID(); got == id {
				return f, true
			}
		}
	}
	return resultMsg{}, false
}

const promptT1Answer = `{"jsonrpc":"2.0","id":"relay-turn-t1","result":{"stopReason":"end_turn","usage":{"totalTokens":120}}}`

/* --- OpenCode: `_session/steering` --- */

func answerSteersWith(frames func(id string) []string) func(f jsonRPCFrame, line string) []string {
	return func(f jsonRPCFrame, line string) []string {
		if f.Method == openCodeMethodSteering {
			id, _ := f.stringID()
			return frames(id)
		}
		return nil
	}
}

func TestAnOpenCodeSteerAnsweredInjectedInTheTurnIsWrittenCurrent(t *testing.T) {
	h, s := newACPLiveHarness(t, openCodeACPProfile, true)
	startACPPrompt(t, globalOpenCodeACPManager, h, s)
	h.child.setRespond(answerSteersWith(func(id string) []string {
		return []string{`{"jsonrpc":"2.0","id":"` + id + `","result":{"outcome":"injected"}}`}
	}))
	cmd := h.acpCmd("oc-1", "Also write BANANA.")
	h.handle(cmd)
	h.handle(cmd)
	steers := h.child.framesWithMethod(openCodeMethodSteering)
	if len(steers) != 1 {
		t.Fatalf("want exactly one _session/steering, got %d", len(steers))
	}
	var p acpPromptParams
	_ = json.Unmarshal(steers[0].Params, &p)
	if id, _ := steers[0].stringID(); id != "relay-followup-oc-1" || p.SessionID != acpTestSession ||
		len(p.Prompt) != 1 || p.Prompt[0].Type != "text" || p.Prompt[0].Text != cmd.Input {
		t.Fatalf("steer = id %s %+v", id, p)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	wantAck(t, acks[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	waitForLive(t, "the steer answer", func() bool { _, ok := h.messageWithID("opencode_acp_message", "relay-followup-oc-1"); return ok })
	if f, _ := h.messageWithID("opencode_acp_message", "relay-followup-oc-1"); f.AcpSteerOutcome != followUpOutcomeWritten || f.AcpHandoverTo != "" {
		t.Fatalf("steer answer annotations = %q / %q", f.AcpSteerOutcome, f.AcpHandoverTo)
	}
}

// W-O6: a steer that lands after its turn's answer is held for the next
// prompt. The device reads the two answers in stdout order.
func TestAnOpenCodeSteerAnsweredAfterItsTurnEndedIsHeldForTheNextPrompt(t *testing.T) {
	h, s := newACPLiveHarness(t, openCodeACPProfile, true)
	startACPPrompt(t, globalOpenCodeACPManager, h, s)
	h.child.setRespond(answerSteersWith(func(id string) []string {
		return []string{promptT1Answer, `{"jsonrpc":"2.0","id":"` + id + `","result":{"outcome":"injected"}}`}
	}))
	h.handle(h.acpCmd("oc-2", "late"))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeWritten, "", followUpProcessedInNextPrompt)
}

func TestAnOpenCodeSteerIsSettledFromItsAnswer(t *testing.T) {
	cases := []struct {
		name, answer, outcome, reason, annotation string
	}{
		{"promptRequired", `"result":{"outcome":"promptRequired","reason":"noRunningTurn"}`, followUpOutcomeNotWritten, followUpReasonNoActiveTurn, followUpOutcomeNotWritten},
		{"an older build", `"error":{"code":-32601,"message":"\"Method not found\": _session/steering"}`, followUpOutcomeNotWritten, followUpReasonMethodNotSupported, followUpOutcomeNotWritten},
		{"a session it does not have", `"error":{"code":-32602,"message":"session not found: ses_1"}`, followUpOutcomeNotWritten, followUpReasonConversationMismatch, followUpOutcomeNotWritten},
		{"invalid params", `"error":{"code":-32602,"message":"bad prompt"}`, followUpOutcomeNotWritten, followUpReasonCliRefused, followUpOutcomeNotWritten},
		{"an internal error", `"error":{"code":-32603,"message":"boom"}`, followUpOutcomeUnknown, "", followUpOutcomeUnknown},
		{"an unknown outcome", `"result":{"outcome":"queued"}`, followUpOutcomeUnknown, followUpReasonAnswerUnrecognised, followUpOutcomeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, s := newACPLiveHarness(t, openCodeACPProfile, true)
			startACPPrompt(t, globalOpenCodeACPManager, h, s)
			h.child.setRespond(answerSteersWith(func(id string) []string {
				return []string{`{"jsonrpc":"2.0","id":"` + id + `",` + c.answer + `}`}
			}))
			h.handle(h.acpCmd("oc-c", "text"))
			wantAck(t, h.acks.snapshot()[0], c.outcome, c.reason, "")
			waitForLive(t, "the steer answer", func() bool { _, ok := h.messageWithID("opencode_acp_message", "relay-followup-oc-c"); return ok })
			if f, _ := h.messageWithID("opencode_acp_message", "relay-followup-oc-c"); f.AcpSteerOutcome != c.annotation {
				t.Fatalf("annotation = %q, want %q", f.AcpSteerOutcome, c.annotation)
			}
		})
	}
}

func TestAnOpenCodeSteerWithNoAnswerIsUnknownThenLiftedByALateSuccess(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 50*time.Millisecond)
	h, s := newACPLiveHarness(t, openCodeACPProfile, true)
	startACPPrompt(t, globalOpenCodeACPManager, h, s)
	h.child.setRespond(nil)
	h.handle(h.acpCmd("oc-late", "text"))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeUnknown, followUpReasonNoAnswer, "")
	h.child.emit(`{"jsonrpc":"2.0","id":"relay-followup-oc-late","result":{"outcome":"injected"}}`)
	waitForLive(t, "the late upgrade", func() bool { return len(h.acks.snapshot()) == 2 })
	wantAck(t, h.acks.snapshot()[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	if f, _ := h.messageWithID("opencode_acp_message", "relay-followup-oc-late"); f.AcpSteerOutcome != followUpOutcomeWritten {
		t.Fatalf("late success annotation = %q", f.AcpSteerOutcome)
	}
}

// A late REFUSAL cannot lift an unknown: the chunk says what the ledger kept.
func TestALateOpenCodeRefusalIsAnnotatedUnknown(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 50*time.Millisecond)
	h, s := newACPLiveHarness(t, openCodeACPProfile, true)
	startACPPrompt(t, globalOpenCodeACPManager, h, s)
	h.child.setRespond(nil)
	h.handle(h.acpCmd("oc-lr", "text"))
	h.child.emit(`{"jsonrpc":"2.0","id":"relay-followup-oc-lr","result":{"outcome":"promptRequired"}}`)
	waitForLive(t, "the late answer", func() bool { _, ok := h.messageWithID("opencode_acp_message", "relay-followup-oc-lr"); return ok })
	if f, _ := h.messageWithID("opencode_acp_message", "relay-followup-oc-lr"); f.AcpSteerOutcome != followUpOutcomeUnknown {
		t.Fatalf("late refusal annotation = %q", f.AcpSteerOutcome)
	}
	if len(h.acks.snapshot()) != 1 {
		t.Fatal("a late refusal changed the answer")
	}
}

func TestAnOpenCodeSteerIsFencedBeforeAnythingIsWritten(t *testing.T) {
	t.Run("no turn running", func(t *testing.T) {
		h, _ := newACPLiveHarness(t, openCodeACPProfile, true)
		h.handle(h.acpCmd("oc-idle", "idle"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	})
	t.Run("after the turn's answer", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, true)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		h.child.emit(promptT1Answer)
		waitForLive(t, "the turn to end", func() bool { return !s.liveFollowUps.clientTurnRunning() })
		h.handle(h.acpCmd("oc-after", "after"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	})
	t.Run("an open permission request", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, true)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		h.child.emit(`{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"ses_1","options":[{"optionId":"once","kind":"allow_once"}]}}`)
		waitForLive(t, "the permission request", s.liveFollowUps.approvalPending)
		h.handle(h.acpCmd("oc-perm", "beside an approval"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
	})
	t.Run("steering not advertised", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, false)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		h.handle(h.acpCmd("oc-nosteer", "x"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNotLiveKind, "")
	})
	t.Run("another ACP session", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, true)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		cmd := h.acpCmd("oc-other", "x")
		cmd.ConversationID = "ses_other"
		h.handle(cmd)
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonConversationMismatch, "")
	})
	t.Run("empty text", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, true)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		h.handle(h.acpCmd("oc-empty", "  "))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonCliRefused, "")
	})
	t.Run("uncertified", func(t *testing.T) {
		h, s := newACPLiveHarness(t, openCodeACPProfile, true)
		startACPPrompt(t, globalOpenCodeACPManager, h, s)
		pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322"})
		h.handle(h.acpCmd("oc-uncert", "x"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNotLiveKind, "")
	})
	// In every case nothing reached the CLI.
}

func TestNoOpenCodeSteerIsWrittenWhenFenced(t *testing.T) {
	h, _ := newACPLiveHarness(t, openCodeACPProfile, true)
	h.handle(h.acpCmd("oc-none", "idle"))
	if n := len(h.child.framesWithMethod(openCodeMethodSteering)); n != 0 {
		t.Fatalf("%d steers written into an idle session", n)
	}
}

/* --- Antigravity: a second `session/prompt` that takes the turn over --- */

// The fake agy answers the running prompt the moment the follow-up prompt
// arrives (W-A5), with `usage` (a normal end) or without (a handover).
func agyAnswersRunningPrompt(usage bool) func(f jsonRPCFrame, line string) []string {
	return func(f jsonRPCFrame, line string) []string {
		if id, _ := f.stringID(); f.Method == acpMethodSessionPrompt && strings.HasPrefix(id, "relay-turn-fu_") {
			if usage {
				return []string{promptT1Answer}
			}
			return []string{`{"result":{"stopReason":"end_turn"},"id":"relay-turn-t1"}`}
		}
		return nil
	}
}

func TestAnAntigravityFollowUpTakesTheRunningTurnOver(t *testing.T) {
	h, s := newACPLiveHarness(t, antigravityACPProfile, false)
	startACPPrompt(t, globalAntigravityACPManager, h, s)
	h.child.setRespond(agyAnswersRunningPrompt(false))
	cmd := h.acpCmd("ag-1", "Also end with STEERED.")
	h.handle(cmd)
	h.handle(cmd)
	prompts := h.child.framesWithMethod(acpMethodSessionPrompt)
	if len(prompts) != 2 {
		t.Fatalf("want the turn's prompt and ONE follow-up prompt, got %d", len(prompts))
	}
	var p acpPromptParams
	_ = json.Unmarshal(prompts[1].Params, &p)
	if id, _ := prompts[1].stringID(); id != "relay-turn-fu_ag-1" || p.SessionID != acpTestSession || p.Prompt[0].Text != cmd.Input {
		t.Fatalf("follow-up prompt = id %s %+v", id, p)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	waitForLive(t, "the handed-over answer", func() bool { _, ok := h.messageWithID("antigravity_acp_message", "relay-turn-t1"); return ok })
	f, _ := h.messageWithID("antigravity_acp_message", "relay-turn-t1")
	if f.AcpHandoverTo != "relay-turn-fu_ag-1" || f.AcpSteerOutcome != "" {
		t.Fatalf("handover annotation = %q / %q", f.AcpHandoverTo, f.AcpSteerOutcome)
	}
	if !strings.Contains(f.Output, `"stopReason":"end_turn"`) || strings.Contains(f.Output, "acpHandoverTo") {
		t.Fatalf("the CLI's line was edited: %s", f.Output)
	}
	// The turn carries on under the follow-up's prompt.
	if !s.liveFollowUps.clientTurnRunning() {
		t.Fatal("the turn ended at the handed-over answer")
	}
}

// The race: the running prompt ended normally before agy took the new one,
// so agy runs it as the next turn, settled under its own id.
func TestAnAntigravityFollowUpAfterANormalEndRunsAsTheNextTurn(t *testing.T) {
	h, s := newACPLiveHarness(t, antigravityACPProfile, false)
	startACPPrompt(t, globalAntigravityACPManager, h, s)
	h.child.setRespond(agyAnswersRunningPrompt(true))
	h.handle(h.acpCmd("ag-2", "next"))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeWritten, "", followUpProcessedInNext)
	waitForLive(t, "the turn's answer", func() bool { _, ok := h.messageWithID("antigravity_acp_message", "relay-turn-t1"); return ok })
	if f, _ := h.messageWithID("antigravity_acp_message", "relay-turn-t1"); f.AcpHandoverTo != "" {
		t.Fatalf("a normal end was annotated as a handover: %q", f.AcpHandoverTo)
	}
}

func TestAnAntigravityFollowUpWithNoAnswerYetJoinsTheRunningTurn(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 50*time.Millisecond)
	h, s := newACPLiveHarness(t, antigravityACPProfile, false)
	startACPPrompt(t, globalAntigravityACPManager, h, s)
	h.child.setRespond(nil)
	h.handle(h.acpCmd("ag-3", "x"))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	// Its answer still comes: the turn is handed over.
	h.child.emit(`{"result":{"stopReason":"end_turn"},"id":"relay-turn-t1"}`)
	waitForLive(t, "the handed-over answer", func() bool { _, ok := h.messageWithID("antigravity_acp_message", "relay-turn-t1"); return ok })
	if f, _ := h.messageWithID("antigravity_acp_message", "relay-turn-t1"); f.AcpHandoverTo != "relay-turn-fu_ag-3" {
		t.Fatalf("handover annotation = %q", f.AcpHandoverTo)
	}
}

func TestAnAntigravityFollowUpIsNeverAPromptIntoAnIdleSession(t *testing.T) {
	h, _ := newACPLiveHarness(t, antigravityACPProfile, false)
	h.handle(h.acpCmd("ag-idle", "idle"))
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	if n := len(h.child.framesWithMethod(acpMethodSessionPrompt)); n != 0 {
		t.Fatalf("%d prompts written into an idle session", n)
	}
}

func TestAcpHandoverAnswer(t *testing.T) {
	for frame, want := range map[string]bool{
		`{"result":{"stopReason":"end_turn"},"id":"p1"}`:                           true,
		`{"result":{"stopReason":"end_turn","usage":{"totalTokens":1}},"id":"p1"}`: false,
		`{"error":{"code":-32603,"message":"x"},"id":"p1"}`:                        false,
		`{"result":null,"id":"p1"}`:                                                false,
		`not json`:                                                                 false,
	} {
		if got := acpHandoverAnswer(frame); got != want {
			t.Errorf("%s: %v, want %v", frame, got, want)
		}
	}
}

// A session whose `initialize` came back without steering, or whose load and
// resume named a session, keeps the right ACP session id.
func TestTheACPTrackerFollowsTheSessionSetup(t *testing.T) {
	tr := newLiveFollowUpTrackerFor(opencodeCliAgentID)
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-acp-resume","method":"session/resume","params":{"sessionId":"ses_old"}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-acp-resume","error":{"code":-32602,"message":"session not found: ses_old"}}`)
	if tr.acpSession() != "" {
		t.Fatalf("a failed resume set the session to %q", tr.acpSession())
	}
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-acp-load","method":"session/load","params":{"sessionId":"ses_2"}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-acp-load","result":{}}`)
	if tr.acpSession() != "ses_2" {
		t.Fatalf("session = %q after a load", tr.acpSession())
	}
	tr.outbound(`{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":"relay-acp-init","result":{"_meta":{"steering":{"supported":false}}}}`)
	if tr.steeringAdvertised() {
		t.Fatal("steering read as advertised")
	}
	// A prompt whose write failed is no turn.
	tr.outbound(`{"jsonrpc":"2.0","id":"p","method":"session/prompt","params":{}}`).done(false)
	if tr.clientTurnRunning() {
		t.Fatal("an unwritten prompt reads as a running turn")
	}
}

// The stdin-boundary half of the idle fence: with no client prompt running,
// the tracker admits no live write, whatever the earlier check saw.
func TestTheACPTrackerAdmitsNoLiveWriteWithoutARunningPrompt(t *testing.T) {
	for _, kind := range []string{opencodeCliAgentID, antigravityCliAgentID} {
		tr := newLiveFollowUpTrackerFor(kind)
		fence := func(s liveWriteState) string {
			if s.NoActiveTurn {
				return followUpReasonNoActiveTurn
			}
			return ""
		}
		if ch, reason := tr.admitLiveWrite("relay-followup-x", "x", "t", fence); ch != nil || reason != followUpReasonNoActiveTurn {
			t.Fatalf("%s: admitted with no prompt running (reason %q)", kind, reason)
		}
		tr.outbound(`{"jsonrpc":"2.0","id":"p","method":"session/prompt","params":{}}`).done(true)
		if ch, reason := tr.admitLiveWrite("relay-followup-y", "y", "t", fence); ch == nil || reason != "" {
			t.Fatalf("%s: refused with a prompt running (reason %q)", kind, reason)
		}
	}
}
