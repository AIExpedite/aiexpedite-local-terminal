package main

// Shared-session approvals (claude_native_approval.go): which Claude tool-use
// prompts this device answers as allow itself, and which it leaves for a
// person. A voice instruction must never be answered here.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Contract pin: the wire values shared-constants CODING_SESSION_APPROVAL_DISPOSITION
// and CODING_SESSION_AUTO_APPROVAL_FIELD define.
func TestClaudeApproval_WireValuesArePinned(t *testing.T) {
	if claudeApprovalAutoAllow != "autoAllow" || claudeApprovalAskPerson != "askPerson" {
		t.Fatalf("disposition values drifted: %q %q", claudeApprovalAutoAllow, claudeApprovalAskPerson)
	}
	raw, _ := json.Marshal(resultMsg{AutoApproval: &claudeAutoApprovalMark{InstructionID: "fu_1", Disposition: claudeApprovalAutoAllow}})
	if !strings.Contains(string(raw), `"autoApproval":{"instructionId":"fu_1","disposition":"autoAllow"}`) {
		t.Fatalf("autoApproval field shape drifted: %s", raw)
	}
	raw, _ = json.Marshal(resultMsg{})
	if strings.Contains(string(raw), "autoApproval") {
		t.Fatalf("an unmarked frame must not carry autoApproval: %s", raw)
	}
}

func TestClaudeApproval_UnknownDispositionAsks(t *testing.T) {
	for _, v := range []string{"", "AUTOALLOW", "allow", "autoAllow ", "askPerson"} {
		if got := normalizeClaudeApprovalDisposition(v); got != claudeApprovalAskPerson {
			t.Errorf("normalize(%q) = %q, want askPerson", v, got)
		}
	}
	a := newClaudeApprovalState(true, "yes please")
	a.beginTurn(claudeTurnApproval{Disposition: "whatever", InstructionID: "i1"})
	if a.decide() != nil {
		t.Fatal("an unknown disposition was answered as allow")
	}
}

func TestClaudeApproval_AutoAllowTurnIsAnsweredAndMarked(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	mark := a.decide()
	if mark == nil || mark.InstructionID != "chat-1" || mark.Disposition != claudeApprovalAutoAllow {
		t.Fatalf("decide = %+v, want an autoAllow mark for chat-1", mark)
	}
}

func TestClaudeApproval_AskPersonTurnIsForwarded(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-1"})
	if a.decide() != nil {
		t.Fatal("a voice (askPerson) turn's prompt was answered by the device")
	}
}

func TestClaudeApproval_NeverAnswersWithoutThePromptToolLaunch(t *testing.T) {
	a := newClaudeApprovalState(false, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	if a.decide() != nil {
		t.Fatal("answered a prompt on a session not launched with --permission-prompt-tool stdio")
	}
	if claudeLaunchAsksEachAction([]string{"--permission-prompt-tool", "stdio", "--dangerously-skip-permissions"}) {
		t.Error("an auto-approve launch must never count as asking each action")
	}
	if claudeLaunchAsksEachAction([]string{"--permission-prompt-tool", "mcp__x__y"}) {
		t.Error("only the stdio prompt tool reaches this device")
	}
	if !claudeLaunchAsksEachAction([]string{"--output-format", "stream-json", "--permission-prompt-tool", "stdio"}) ||
		!claudeLaunchAsksEachAction([]string{"--permission-prompt-tool=stdio"}) {
		t.Error("the stdio prompt-tool launch was not recognised")
	}
}

// A signed askPerson arriving mid-turn makes the NEXT prompt ask; an allowing
// one arriving mid-turn (a redelivered or late SEND) never raises an asking turn.
func TestClaudeApproval_MidTurnOnlyLowers(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-2"})
	if a.decide() != nil {
		t.Fatal("a voice follow-up into a chat turn left the turn allowing")
	}
	if d, id := a.current(); d != claudeApprovalAskPerson || id != "voice-2" {
		t.Fatalf("current = %s/%s, want askPerson/voice-2", d, id)
	}
	// A redelivered chat SEND does not change the running turn.
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	if a.decide() != nil {
		t.Fatal("an allowing SEND raised a running asking turn")
	}
}

// A turn settling returns to the default, except after an asking instruction
// was written mid-turn: Claude may run it as the next turn, which asks too and
// stays open until its own result.
func TestClaudeApproval_SettleReturnsToDefaultUnlessAnAskWasCarried(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-1"})
	a.settle()
	if a.decide() == nil {
		t.Fatal("a settled voice turn left the chat session asking (it opened no follow-up)")
	}

	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-2"})
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-3"})
	a.settle()
	if a.decide() != nil {
		t.Fatal("a voice follow-up written mid-turn was allowed as the next turn")
	}
	if d, id := a.current(); d != claudeApprovalAskPerson || id != "voice-3" {
		t.Fatalf("carried turn = %s/%s, want askPerson/voice-3", d, id)
	}
	a.settle() // the carried voice turn's own result
	if a.decide() == nil {
		t.Fatal("the carried ask outlived its own turn")
	}
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-4"})
	if a.decide() == nil {
		t.Fatal("a chat SEND opening its own turn did not allow")
	}
}

// A chat SEND arriving while a carried voice turn is (or may be) running joins
// that turn and never raises it: the voice turn's later prompts still ask and
// are never attributed to the chat instruction. Only that turn's own result
// ends it.
func TestClaudeApproval_ChatSendCannotRaiseACarriedVoiceTurn(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-2"})
	a.settle() // chat-1's result; Claude now runs voice-2 as the next turn

	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-3"})
	if a.decide() != nil {
		t.Fatal("a chat SEND during the queued voice turn auto-approved its prompts")
	}
	if d, id := a.current(); d != claudeApprovalAskPerson || id != "voice-2" {
		t.Fatalf("running turn = %s/%s, want askPerson/voice-2", d, id)
	}

	a.settle() // voice-2's result; chat-3 runs next under the default
	if a.decide() == nil {
		t.Fatal("the chat turn after the voice turn did not allow")
	}

	// A further voice follow-up written into the carried turn chains: the
	// turn after it asks as well.
	b := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	b.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	b.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-2"})
	b.settle()
	b.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-3"})
	b.settle()
	if d, id := b.current(); d != claudeApprovalAskPerson || id != "voice-3" {
		t.Fatalf("chained carry = %s/%s, want askPerson/voice-3", d, id)
	}
	b.settle()
	if b.decide() == nil {
		t.Fatal("the chained carry outlived its own turn")
	}
}

// A SEND with no disposition inherits the most recent attributed turn's, or the
// session's default when there is none.
func TestClaudeApproval_UnattributedSendInherits(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAutoAllow)
	a.beginTurn(claudeTurnApproval{})
	if a.decide() == nil {
		t.Fatal("an unattributed first SEND did not take the autoAllow default")
	}
	a.settle()
	a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-1"})
	a.settle()
	a.beginTurn(claudeTurnApproval{InstructionID: "x"})
	if a.decide() != nil {
		t.Fatal("an unattributed SEND after a voice turn did not inherit askPerson")
	}

	// A relay-launched session with no approvalDefault asks, as before.
	relay := newClaudeApprovalState(true, "")
	relay.beginTurn(claudeTurnApproval{})
	if relay.decide() != nil {
		t.Fatal("a START with no approvalDefault allowed a prompt")
	}
}

func TestClaudeApproval_FailedWriteRestores(t *testing.T) {
	a := newClaudeApprovalState(true, claudeApprovalAskPerson)
	prev := a.beginTurn(claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"})
	a.restore(prev)
	if a.decide() != nil {
		t.Fatal("a turn whose text never reached the child still allows")
	}
}

func TestClaudeApproval_FollowUpWithoutDispositionAsks(t *testing.T) {
	got := claudeFollowUpApprovalOf(commandMsg{InstructionID: "fu_1"})
	if got.Disposition != claudeApprovalAskPerson || got.InstructionID != "fu_1" {
		t.Fatalf("follow-up approval = %+v, want askPerson/fu_1", got)
	}
}

func TestClaudeCanUseToolRequest(t *testing.T) {
	id, input, ok := claudeCanUseToolRequest(`{"type":"control_request","request_id":"req_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`)
	if !ok || id != "req_1" || string(input) != `{"command":"ls"}` {
		t.Fatalf("parse = %q %s %v", id, input, ok)
	}
	for _, line := range []string{
		`{"type":"control_request","request_id":"r","request":{"subtype":"interrupt","note":"can_use_tool"}}`,
		`{"type":"control_request","request":{"subtype":"can_use_tool"}}`,
		`{"type":"assistant","text":"control_request can_use_tool"}`,
		`not json control_request can_use_tool`,
	} {
		if _, _, ok := claudeCanUseToolRequest(line); ok {
			t.Errorf("not a can_use_tool prompt, parsed as one: %s", line)
		}
	}
}

// The allow is the exact answer a person's Allow writes: the tool input passed
// back unchanged as updatedInput.
func TestClaudeAllowControlResponse_Shape(t *testing.T) {
	line, err := claudeAllowControlResponse("req_9", json.RawMessage(`{"command":"rm -rf build"}`))
	if err != nil {
		t.Fatalf("claudeAllowControlResponse: %v", err)
	}
	var frame struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior     string          `json:"behavior"`
				UpdatedInput json.RawMessage `json:"updatedInput"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("unmarshal %s: %v", line, err)
	}
	if frame.Type != "control_response" || frame.Response.Subtype != "success" || frame.Response.RequestID != "req_9" ||
		frame.Response.Response.Behavior != "allow" || string(frame.Response.Response.UpdatedInput) != `{"command":"rm -rf build"}` {
		t.Fatalf("allow answer = %s", line)
	}
	if strings.Contains(line, "\n") {
		t.Fatal("the answer must be one NDJSON line")
	}
	empty, _ := claudeAllowControlResponse("req_10", nil)
	if !strings.Contains(empty, `"updatedInput":{}`) {
		t.Fatalf("a missing input must pass back {}: %s", empty)
	}
}

// A redelivered relay turn runs under the disposition its FIRST claimed
// delivery carried, whatever the copy says.
func TestRelayTurnInbox_PinsTheFirstDeliverysApproval(t *testing.T) {
	inbox := newTestRelayInbox(t)
	first := commandMsg{ID: relayTurnEnvelopePrefix + "t1", SessionID: "s1", ApprovalDisposition: claudeApprovalAskPerson, InstructionID: "t1"}
	if decision, _ := inbox.Begin(first, "t1", "agent-1"); decision != relayTurnRun {
		t.Fatalf("first Begin = %v, want run", decision)
	}
	// The handler refused before acceptance (the row stays claimed).
	inbox.Finish("s1", "t1")

	copyCmd := first
	copyCmd.ApprovalDisposition = claudeApprovalAutoAllow
	copyCmd.InstructionID = "other"
	var got claudeTurnApproval
	ran := runRelayTurn(context.Background(), inbox, copyCmd, "t1", "agent-1", func(relayTurnRow) {}, func(ctx context.Context) {
		got = claudeSendApproval(ctx, copyCmd)
	})
	if !ran {
		t.Fatal("redelivery was handed back")
	}
	if got.Disposition != claudeApprovalAskPerson || got.InstructionID != "t1" {
		t.Fatalf("redelivered turn ran under %+v, want the first delivery's askPerson/t1", got)
	}
	row := inbox.snapshot()[relayTurnKey{"s1", "t1"}]
	if row.ApprovalDisposition != claudeApprovalAskPerson || row.InstructionID != "t1" {
		t.Fatalf("row = %+v", row)
	}

	// A plain (non-relay) SEND reads its own signed fields.
	plain := commandMsg{ApprovalDisposition: claudeApprovalAutoAllow, InstructionID: "chat-1"}
	if a := claudeSendApproval(context.Background(), plain); a.Disposition != claudeApprovalAutoAllow || a.InstructionID != "chat-1" {
		t.Fatalf("plain SEND approval = %+v", a)
	}
}

func TestSignedApprovalFields_SitBetweenCwdFenceAndReadOnly(t *testing.T) {
	cmd := signedExecIDFixture()
	cmd.CwdFence = `C:\repo`
	cmd.ApprovalDefault = claudeApprovalAutoAllow
	cmd.ApprovalDisposition = claudeApprovalAskPerson
	cmd.InstructionID = "fu_1"
	cmd.ReadOnly = true
	canonical, _ := canonicalAndHMAC(t, cmd)
	order := []string{`"cwdFence"`, `"approvalDefault"`, `"approvalDisposition"`, `"instructionId"`, `"readOnly"`}
	last := -1
	for _, f := range order {
		i := strings.Index(canonical, f)
		if i < 0 {
			t.Fatalf("%s missing from %s", f, canonical)
		}
		if i <= last {
			t.Fatalf("canonical order must be %v, got %s", order, canonical)
		}
		last = i
	}
	// Absent fields leave every other command's canonical byte-identical.
	plain, _ := canonicalAndHMAC(t, signedExecIDFixture())
	if strings.Contains(plain, "approval") || strings.Contains(plain, "instructionId") {
		t.Fatalf("absent approval fields reached the canonical: %s", plain)
	}
	// Upgrading a voice turn to autoAllow breaks the signature.
	_, askHMAC := canonicalAndHMAC(t, cmd)
	cmd.ApprovalDisposition = claudeApprovalAutoAllow
	if _, allowHMAC := canonicalAndHMAC(t, cmd); allowHMAC == askHMAC {
		t.Fatal("approvalDisposition is not covered by the HMAC")
	}
}

func TestRelayOriginApprovals_CapabilityIsReportedAndNeverOmitted(t *testing.T) {
	if !newMachineCapabilities().RelayOriginApprovals {
		t.Error("this build answers chat-turn prompts itself; it must report relayOriginApprovals")
	}
	encoded, _ := json.Marshal(&capabilitiesInfo{})
	if !strings.Contains(string(encoded), `"relayOriginApprovals":false`) {
		t.Errorf("a false RelayOriginApprovals must still serialise; got %s", encoded)
	}
}

/* -------------------------------------------------------------------------- */
/* Integration: the real manager against a mock Claude that asks               */
/* -------------------------------------------------------------------------- */

// runMockClaudeNativeAsk mimics a Claude launched with --permission-prompt-tool
// stdio: for each user turn it asks permission for one tool use (input
// `{"command":<the turn's text>}`), waits for the control_response, echoes it
// back as a `mock_control` system frame, and ends the turn with a result.
func runMockClaudeNativeAsk() {
	reader := bufio.NewReader(os.Stdin)
	n := 0
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && strings.Contains(trimmed, `"type":"user"`) {
			var env struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			_ = json.Unmarshal([]byte(trimmed), &env)
			n++
			fmt.Printf(`{"type":"control_request","request_id":"req_%d","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":%s}}}`+"\n",
				n, jsonEscapeString(env.Message.Content))
			answer, aerr := reader.ReadString('\n')
			fmt.Printf(`{"type":"system","subtype":"mock_control","answer":%s}`+"\n", jsonEscapeString(strings.TrimSpace(answer)))
			fmt.Printf(`{"type":"result","subtype":"success","result":%s}`+"\n", jsonEscapeString("done "+env.Message.Content))
			if aerr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	os.Exit(0)
}

type capturedFrames struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (c *capturedFrames) publish(r resultMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, r)
}

func (c *capturedFrames) find(pred func(resultMsg) bool) (resultMsg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.frames {
		if pred(f) {
			return f, true
		}
	}
	return resultMsg{}, false
}

func (c *capturedFrames) waitFor(t *testing.T, what string, pred func(resultMsg) bool) resultMsg {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f, ok := c.find(pred); ok {
			return f
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return resultMsg{}
}

func outputHas(sub string) func(resultMsg) bool {
	return func(r resultMsg) bool { return r.Type == "claude_native_message" && strings.Contains(r.Output, sub) }
}

// A chat (autoAllow) turn's prompt is answered by the device with the request's
// own input and the chunk is marked; a voice (askPerson) turn in the SAME
// session is forwarded unanswered and waits for a person.
func TestClaudeNativeLifecycle_ChatTurnAllowedLocallyVoiceTurnAsks(t *testing.T) {
	skipIfUnsupportedOS(t)
	tmpDir := installMockClaude(t, "claude-native-ask")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-shared-%d", time.Now().UnixNano())
	var c capturedFrames
	err := m.StartForAgentWithApproval("", id, tmpDir, []string{"--permission-prompt-tool", "stdio"}, "list files", "ws", "uid",
		claudeStartApproval{Default: claudeApprovalAutoAllow, InstructionID: "chat-1"}, c.publish, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	prompt := c.waitFor(t, "the chat turn's prompt", outputHas(`"req_1"`))
	if prompt.AutoApproval == nil || prompt.AutoApproval.InstructionID != "chat-1" || prompt.AutoApproval.Disposition != claudeApprovalAutoAllow {
		t.Fatalf("chat turn's prompt not marked as auto-allowed: %+v", prompt.AutoApproval)
	}
	answer := c.waitFor(t, "the allow reached Claude", outputHas("mock_control"))
	if !strings.Contains(answer.Output, `\"behavior\":\"allow\"`) || !strings.Contains(answer.Output, `\"updatedInput\":{\"command\":\"list files\"}`) {
		t.Fatalf("allow answer = %s", answer.Output)
	}
	c.waitFor(t, "chat turn result", outputHas("done list files"))
	session := m.Get(id)
	if n := session.pendingApprovals.Load(); n != 0 {
		t.Fatalf("an answered prompt left %d approvals open", n)
	}

	// A voice turn in the same session: forwarded, left for a person.
	if err := m.SendTurn(id, "delete the build folder", nil, claudeTurnApproval{Disposition: claudeApprovalAskPerson, InstructionID: "voice-2"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	voicePrompt := c.waitFor(t, "the voice turn's prompt", outputHas(`"req_2"`))
	if voicePrompt.AutoApproval != nil {
		t.Fatalf("a voice turn's prompt was answered by the device: %+v", voicePrompt.AutoApproval)
	}
	deadline := time.Now().Add(5 * time.Second)
	for session.pendingApprovals.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if session.pendingApprovals.Load() != 1 {
		t.Fatal("the voice prompt is not counted as waiting on a person")
	}
	time.Sleep(200 * time.Millisecond)
	if _, answered := c.find(func(r resultMsg) bool {
		return r.Type == "claude_native_message" && strings.Contains(r.Output, "mock_control") && strings.Contains(r.Output, "req_2")
	}); answered {
		t.Fatal("the voice prompt was answered without a person")
	}
	// The person denies it.
	if err := m.SendControl(id, `{"type":"control_response","response":{"subtype":"success","request_id":"req_2","response":{"behavior":"deny","message":"no"}}}`); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	c.waitFor(t, "the person's deny reached Claude", func(r resultMsg) bool {
		return r.Type == "claude_native_message" && strings.Contains(r.Output, "mock_control") && strings.Contains(r.Output, "req_2") && strings.Contains(r.Output, "deny")
	})
}

// A session launched auto-approving (no stdio prompt tool) is never answered
// by the device, whatever its default says.
func TestClaudeNativeLifecycle_NoPromptToolNeverAnswered(t *testing.T) {
	skipIfUnsupportedOS(t)
	tmpDir := installMockClaude(t, "claude-native-ask")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-legacy-%d", time.Now().UnixNano())
	var c capturedFrames
	err := m.StartForAgentWithApproval("", id, tmpDir, nil, "list files", "ws", "uid",
		claudeStartApproval{Default: claudeApprovalAutoAllow, InstructionID: "chat-1"}, c.publish, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })
	prompt := c.waitFor(t, "the prompt", outputHas(`"req_1"`))
	if prompt.AutoApproval != nil {
		t.Fatalf("an auto-approve launch's prompt was answered: %+v", prompt.AutoApproval)
	}
}

// The disposition is applied under stdinMu BEFORE the text reaches the child:
// a prompt the voice follow-up raises can never be read under the chat turn's
// allow.
func TestClaudeApproval_AppliedBeforeTheTextIsWritten(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	stdin := &recordingWriteCloser{}
	session := newRelayTestClaudeSession("approval-order", stdin)
	session.approvals = newClaudeApprovalState(true, claudeApprovalAutoAllow)
	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	if err := m.SendTurn(session.ID, "build it", nil, claudeTurnApproval{Disposition: claudeApprovalAutoAllow, InstructionID: "chat-1"}); err != nil {
		t.Fatalf("chat SendTurn: %v", err)
	}
	var atWrite string
	stdin.onWrite = func() { atWrite, _ = session.approvals.current() }
	followUp := claudeFollowUpApprovalOf(commandMsg{ApprovalDisposition: claudeApprovalAskPerson, InstructionID: "fu_voice"})
	if err := m.SendTurn(session.ID, "and delete prod", nil, followUp); err != nil {
		t.Fatalf("voice follow-up SendTurn: %v", err)
	}
	if atWrite != claudeApprovalAskPerson {
		t.Fatalf("disposition when the follow-up text was written = %q, want askPerson", atWrite)
	}
}
