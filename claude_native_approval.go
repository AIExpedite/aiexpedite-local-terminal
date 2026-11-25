package main

// Shared-session approvals for chat-direct Claude Code sessions (shared-constants
// codingSessionInstructionOrigin.js, VOICE_COMPUTER_RELAY.md "Shared sessions").
//
// One Claude session can now take instructions from chat, from the person typing
// in its own composer, and from voice. It is launched with
// `--permission-prompt-tool stdio` either way, so every tool use reaches this
// device as a `can_use_tool` control_request. How that prompt is answered
// follows the ORIGIN of the instruction whose turn raised it, which the server
// signs into each command:
//
//   - `approvalDefault` on the START: the session's own default (a chat start
//     signs autoAllow, a relay start askPerson). Absent → askPerson, today's
//     relay behaviour.
//   - `approvalDisposition` + `instructionId` on a SEND, a relay turn and a
//     `relay_follow_up`.
//
// While the open turn is `autoAllow` (a chat or typed instruction), this device
// answers the prompt as allow ITSELF — the same control_response a person's
// Allow writes, with `updatedInput` equal to the request's input — and marks the
// published chunk with `autoApproval: { instructionId, disposition }`, which is
// the audit record of that answer. A chat turn therefore runs exactly as fast as
// an auto-approving launch did. Any other turn forwards the prompt unanswered,
// for a person to Allow or Deny on the approval card or the dock chip.
//
// Rules, all decided under the session's stdin lock so a prompt and a turn
// write never interleave:
//
//   - A SEND opening a turn sets the turn's disposition. A SEND with none
//     inherits the most recent attributed turn's, else approvalDefault.
//   - While a turn is open its disposition can only be LOWERED (autoAllow →
//     askPerson), never raised: a signed askPerson live follow-up lowers it
//     before its text is written, and a redelivered or late SEND can never
//     turn an asking turn into an allowing one.
//   - A turn settling (Claude's `result`) returns the disposition to the
//     session's default, so a turn Claude starts by itself follows the START —
//     unless an asking instruction was written mid-turn: Claude may run it as
//     the next turn, so that turn is treated as OPEN and asking until its own
//     `result`. Nothing on stdout says when a queued instruction starts, so a
//     SEND arriving meanwhile is written into that turn (lower only) and can
//     never raise it. Every instruction written into an open turn is queued
//     and each `result` retires one, so several queued voice instructions
//     each keep their own turn asking. A queue holding only chat or typed
//     instructions keeps the oldest one's turn open under its own allow and
//     instruction id, so its prompts are answered and attributed to it even
//     in a session whose default asks. The session returns to its default
//     only once the queue is empty. If Claude had in fact merged queued
//     instructions into one turn, the turn state runs one `result` behind:
//     later turns ask more than they needed to after a queued ask, and after
//     a queued allow a turn Claude starts by itself before the next `result`
//     follows that chat instruction instead of the default. A voice
//     instruction written meanwhile still lowers it before its text lands.
//   - A redelivered relay turn runs under the approval its FIRST claimed
//     delivery carried (the turn inbox row pins it, relay_turn_inbox.go).
//   - Anything but the literal "autoAllow" asks, and nothing is ever answered
//     on a session not launched with the stdio prompt tool or for any
//     control_request subtype other than can_use_tool.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Wire values mirrored from shared-constants CODING_SESSION_APPROVAL_DISPOSITION.
// Go cannot import the JS package; pinned by claude_native_approval_test.go.
const (
	claudeApprovalAutoAllow = "autoAllow"
	claudeApprovalAskPerson = "askPerson"
)

// normalizeClaudeApprovalDisposition is the tolerant reader: anything but the
// literal autoAllow asks.
func normalizeClaudeApprovalDisposition(v string) string {
	if v == claudeApprovalAutoAllow {
		return claudeApprovalAutoAllow
	}
	return claudeApprovalAskPerson
}

// claudeAutoApprovalMark is the `autoApproval` field (shared-constants
// CODING_SESSION_AUTO_APPROVAL_FIELD) on a control_request chunk this device
// answered itself.
type claudeAutoApprovalMark struct {
	InstructionID string `json:"instructionId,omitempty"`
	Disposition   string `json:"disposition"`
}

// claudeTurnApproval is what one instruction says about the approvals its turn
// raises. An empty Disposition means the command carried none (inherit).
type claudeTurnApproval struct {
	Disposition   string
	InstructionID string
}

// claudeTurnApprovalOf reads a SEND's signed fields.
func claudeTurnApprovalOf(cmd commandMsg) claudeTurnApproval {
	return claudeTurnApproval{Disposition: cmd.ApprovalDisposition, InstructionID: cmd.InstructionID}
}

// claudeFollowUpApprovalOf reads a relay_follow_up's signed fields. A live
// follow-up never inherits: no disposition asks.
func claudeFollowUpApprovalOf(cmd commandMsg) claudeTurnApproval {
	return claudeTurnApproval{
		Disposition:   normalizeClaudeApprovalDisposition(cmd.ApprovalDisposition),
		InstructionID: cmd.InstructionID,
	}
}

// claudeApprovalState is one session's approval disposition, guarded by mu.
// beginTurn, restore and decide are called with the session's stdinMu held, so
// a turn write and a prompt answer never interleave; settle (the stdout reader,
// on a `result`) is not, and only ever returns the session to its default.
type claudeApprovalState struct {
	mu sync.Mutex
	// promptTool: the child was launched with `--permission-prompt-tool stdio`
	// and without the auto-approve flag. Never true → nothing is ever answered.
	promptTool  bool
	defaultDisp string
	turnOpen    bool
	disposition string
	instruction string
	// lastAttributed is the most recent turn whose command named a
	// disposition; a SEND naming none inherits it.
	lastAttributed *claudeTurnApproval
	// queued: every instruction written while a turn was open, oldest first,
	// not yet retired by a `result`. Claude may take each into the running
	// turn or run it as a later one, and nothing tells the two apart, so
	// while any queued instruction asks, settle keeps the turn open and
	// asking: those turns ask too, and a SEND arriving before their own
	// `result` cannot raise them.
	queued []claudeTurnApproval
}

func newClaudeApprovalState(promptTool bool, approvalDefault string) *claudeApprovalState {
	def := normalizeClaudeApprovalDisposition(approvalDefault)
	return &claudeApprovalState{promptTool: promptTool, defaultDisp: def, disposition: def}
}

// claudeApprovalSnapshot restores a state whose turn write failed.
type claudeApprovalSnapshot struct {
	turnOpen       bool
	disposition    string
	instruction    string
	lastAttributed *claudeTurnApproval
	queued         []claudeTurnApproval
}

// beginTurn records that an instruction is about to be written to the child,
// and returns the state before it so a failed write can be undone. Called
// under stdinMu immediately before the write.
func (a *claudeApprovalState) beginTurn(t claudeTurnApproval) claudeApprovalSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := claudeApprovalSnapshot{a.turnOpen, a.disposition, a.instruction, a.lastAttributed, append([]claudeTurnApproval(nil), a.queued...)}
	var resolved string
	if t.Disposition == "" {
		resolved = a.defaultDisp
		if a.lastAttributed != nil {
			resolved = a.lastAttributed.Disposition
		}
	} else {
		resolved = normalizeClaudeApprovalDisposition(t.Disposition)
		a.lastAttributed = &claudeTurnApproval{Disposition: resolved, InstructionID: t.InstructionID}
	}
	switch {
	case !a.turnOpen:
		// A turn of its own: its instruction decides.
		a.disposition, a.instruction = resolved, t.InstructionID
		a.queued = nil
	default:
		// Written into an open turn: queued until a `result` retires it, and
		// it can only lower the running turn, never raise it.
		a.queued = append(a.queued, claudeTurnApproval{Disposition: resolved, InstructionID: t.InstructionID})
		if resolved == claudeApprovalAskPerson && a.disposition != claudeApprovalAskPerson {
			a.disposition, a.instruction = claudeApprovalAskPerson, t.InstructionID
		}
	}
	a.turnOpen = true
	return prev
}

// restore undoes a beginTurn whose write never reached the child.
func (a *claudeApprovalState) restore(prev claudeApprovalSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turnOpen, a.disposition, a.instruction, a.lastAttributed = prev.turnOpen, prev.disposition, prev.instruction, prev.lastAttributed
	a.queued = prev.queued
}

// settle ends the open turn (Claude's `result`) and retires the oldest queued
// instruction, which Claude may now be running as the next turn. While that
// one or any later queued instruction asks, the turn stays OPEN and asking,
// attributed to the oldest asking one, until further `result`s retire them:
// writing another instruction to stdin does not mean a queued one has ended,
// so a chat SEND meanwhile only joins and cannot turn their prompts into
// allowed ones. When every queued instruction allows, the oldest one's turn
// stays OPEN under its own allow and instruction id, so a chat instruction
// Claude runs as a separate turn auto-approves even in a session whose
// default asks, and its marks name it. Only with nothing queued does the
// disposition return to the session's default.
func (a *claudeApprovalState) settle() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queued) == 0 {
		a.turnOpen = false
		a.disposition, a.instruction = a.defaultDisp, ""
		a.queued = nil
		return
	}
	next := a.queued[0]
	for _, q := range a.queued {
		if q.Disposition == claudeApprovalAskPerson {
			next = q
			break
		}
	}
	a.turnOpen = true
	a.disposition, a.instruction = next.Disposition, next.InstructionID
	a.queued = a.queued[1:]
}

// decide returns the mark to answer a can_use_tool prompt with, or nil when a
// person must answer it.
func (a *claudeApprovalState) decide() *claudeAutoApprovalMark {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.promptTool || a.disposition != claudeApprovalAutoAllow {
		return nil
	}
	return &claudeAutoApprovalMark{InstructionID: a.instruction, Disposition: claudeApprovalAutoAllow}
}

// current is the open turn's disposition (tests, diagnostics).
func (a *claudeApprovalState) current() (disposition, instructionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disposition, a.instruction
}

// claudeLaunchAsksEachAction reports whether the final Claude argv asks every
// tool use over stdio (the prompt tool is `stdio` and the auto-approve flag is
// absent): the only launch whose prompts this device may answer.
func claudeLaunchAsksEachAction(args []string) bool {
	stdio := false
	for i, a := range args {
		if a == "--dangerously-skip-permissions" {
			return false
		}
		if a == "--permission-prompt-tool" && i+1 < len(args) && args[i+1] == "stdio" {
			stdio = true
		}
		if a == "--permission-prompt-tool=stdio" {
			stdio = true
		}
	}
	return stdio
}

// claudeCanUseToolRequest parses a stdout line as a `can_use_tool`
// control_request: its request id and raw tool input. ok is false for every
// other frame, including every other control_request subtype.
func claudeCanUseToolRequest(line string) (requestID string, input json.RawMessage, ok bool) {
	if !strings.Contains(line, `"control_request"`) || !strings.Contains(line, `"can_use_tool"`) {
		return "", nil, false
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string          `json:"subtype"`
			Input   json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return "", nil, false
	}
	if frame.Type != "control_request" || frame.Request.Subtype != "can_use_tool" || frame.RequestID == "" {
		return "", nil, false
	}
	return frame.RequestID, frame.Request.Input, true
}

// claudeAllowControlResponse is the control_response a person's Allow writes
// (terminal-service relayApprovalCommand): allow, with the tool input passed
// back unchanged as `updatedInput`. A missing or non-object input sends `{}`.
func claudeAllowControlResponse(requestID string, input json.RawMessage) (string, error) {
	updated := json.RawMessage(`{}`)
	if trimmed := bytes.TrimSpace(input); len(trimmed) > 0 && trimmed[0] == '{' {
		updated = trimmed
	}
	frame := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response": map[string]any{
				"behavior":     "allow",
				"updatedInput": updated,
			},
		},
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return "", fmt.Errorf("encode allow control_response: %w", err)
	}
	// The same single-line validation a person's answer passes.
	return claudeControlResponseLine(string(raw))
}

// autoAnswerCanUseTool answers a can_use_tool prompt as allow when the
// session's open turn is autoAllow, and returns the mark the published chunk
// carries; nil when the prompt is left for a person (or the write failed, in
// which case it is not answered and is published as a live approval). The
// decision is read under stdinMu, so a live follow-up that lowered the turn
// before this prompt was answered is always seen.
func (m *ClaudeNativeManager) autoAnswerCanUseTool(session *ClaudeNativeSession, requestID string, input json.RawMessage) *claudeAutoApprovalMark {
	if session.approvals == nil {
		return nil
	}
	session.stdinMu.Lock()
	defer session.stdinMu.Unlock()
	if session.Status() == "ended" {
		return nil
	}
	mark := session.approvals.decide()
	if mark == nil {
		return nil
	}
	line, err := claudeAllowControlResponse(requestID, input)
	if err != nil {
		fmt.Printf("%s[claude-native] %s: could not build the allow answer for %s: %v — leaving it for a person%s\n",
			colorRed, session.ID, requestID, err, colorReset)
		return nil
	}
	if err := writeClaudeControlLocked(session, line); err != nil {
		fmt.Printf("%s[claude-native] %s: allow answer for %s not written: %v%s\n",
			colorRed, session.ID, requestID, err, colorReset)
		return nil
	}
	fmt.Printf("%s[claude-native] %s: allowed %s for a chat turn (instruction %q)%s\n",
		colorGreen, session.ID, requestID, mark.InstructionID, colorReset)
	return mark
}
