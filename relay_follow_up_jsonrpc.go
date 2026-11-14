// File: relay_follow_up_jsonrpc.go
// -----------------------------------------------------------------------------
// Live follow-ups for Codex (`turn/steer`) and Grok Build (`_x.ai/interject`),
// device half (L1/L2 live follow-ups spec §2.3, §2.3.1, §2.8).
//
// Claude Code takes a live follow-up as a stream-json user message, and the
// stdin write returning is all the evidence there is. Codex and Grok take one as
// a JSON-RPC REQUEST and answer it, so the device builds the request itself
// (codexSteerFrame, grokInterjectFrame), writes it under the follow-up ledger
// (relay_follow_up_ledger.go) and settles the attempt from the CLI's own
// answer. Neither CLI de-duplicates the request (the same steer or interjection
// sent twice is applied twice), so that ledger is the only at-most-once guard.
//
// Each Codex and Grok child gets one liveFollowUpTracker, fed with every frame
// written through Send (outbound) and every frame read off stdout (inbound). It
// knows only what the live write needs:
//
//   - answer waiters for the ids the device itself chose (`relay-followup-…`);
//   - the open client-bound requests (Codex: any server request; Grok:
//     `session/request_permission`), so nothing is written beside an approval;
//   - Grok only: the outstanding client `session/prompt` requests, Grok's own
//     id for the one running, whether its turn has finished, the ACP session
//     the child is on, and the interjections written during each prompt.
//
// Grok runs an interjection that misses the running turn's last drain as a
// "fallback" turn of its own, after that turn (owner decision O1: keep it
// inside the turn the follow-up joined). When such a fallback is seen before
// the running prompt's answer, the tracker holds that one answer frame back
// and releases it, unchanged and under a later seq, once the fallback's turn
// has completed (§2.8).
//
// The tracker exists only on a session launched from a CLI version certified
// for live follow-ups (newLiveFollowUpTracker); on every other session it is
// nil and each method is a no-op, so an uncertified kind pays nothing.
//
// Follow-up text is never logged.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Wire values mirrored from shared-constants 3.23.578 (cliAgentPreference.js,
// codex.js, grok.js, cliRelayBrokers.js, residentKindsWire.js,
// relayFollowUps.js). Contract-pinned by relay_follow_up_jsonrpc_test.go.
const (
	// CLI_AGENT_IDS.CODEX / .GROK.
	codexCliAgentID = "codex"
	grokCliAgentID  = "grok"

	// RELAY_LIVE_FOLLOW_UP_RPC_ID_PREFIX: the JSON-RPC id of the device's live
	// write is `relay-followup-<attemptId>`.
	relayLiveFollowUpRPCIDPrefix = "relay-followup-"

	// CODEX_RPC_METHODS.
	codexMethodTurnSteer             = "turn/steer"
	codexMethodTurnCompleted         = "turn/completed"
	codexMethodServerRequestResolved = "serverRequest/resolved"

	// CODEX_STEER_ERRORS: every refused steer is -32600, told apart by text.
	codexSteerErrNoActiveTurn          = "no active turn to steer"
	codexSteerErrTurnMismatchPrefix    = "expected active turn id `"
	codexSteerErrNotSteerableReview    = "cannot steer a review turn"
	codexSteerErrNotSteerableCompact   = "cannot steer a compact turn"
	codexSteerErrNotSteerableErrorInfo = "activeTurnNotSteerable"

	// GROK_ACP_EXTENSION_METHODS.
	grokMethodInterject           = "_x.ai/interject"
	grokMethodInterjection        = "_x.ai/session/interjection"
	grokMethodQueueChanged        = "_x.ai/queue/changed"
	grokMethodSessionNotification = "_x.ai/session_notification"
	// GROK_INTERJECT_FALLBACK_PROMPT_PREFIX / GROK_SESSION_UPDATE_TURN_COMPLETED.
	grokInterjectFallbackPromptPrefix = "interject-fallback-"
	grokSessionUpdateTurnCompleted    = "turn_completed"

	// ACP methods (agentclientprotocol.com).
	acpMethodSessionNew        = "session/new"
	acpMethodSessionLoad       = "session/load"
	acpMethodSessionPrompt     = "session/prompt"
	acpMethodRequestPermission = "session/request_permission"

	// JSON-RPC 2.0 request-level error codes: the CLI refused the request
	// before acting on it.
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602

	// DEVICE_FOLLOW_UP_NOT_WRITTEN_REASON (new in 3.23.578).
	followUpReasonNoActiveTurn       = "noActiveTurn"
	followUpReasonTurnMismatch       = "turnMismatch"
	followUpReasonTurnNotSteerable   = "turnNotSteerable"
	followUpReasonMethodNotSupported = "methodNotSupported"
	followUpReasonCliRefused         = "cliRefused"

	// DEVICE_FOLLOW_UP_UNKNOWN_REASON.
	followUpReasonNoAnswer           = "noAnswer"
	followUpReasonWriteFailed        = "writeFailed"
	followUpReasonAnswerUnrecognised = "answerUnrecognised"

	// grokAnsweredInterjectionsMax bounds the interjections remembered from
	// prompts already answered (only to log a fallback that ran after its
	// prompt's answer); grokSeenFallbacksMax bounds the fallback ids already
	// matched, so a repeated queue/changed for one fallback matches once.
	grokAnsweredInterjectionsMax = 16
	grokSeenFallbacksMax         = 64
)

// liveFollowUpAnswerTimeout is RELAY_FOLLOW_UP_LIMITS.liveAnswerTimeoutMs: how
// long the device waits for the CLI's answer to a live write before it answers
// the attempt `unknown`. A var so tests can shorten it.
var liveFollowUpAnswerTimeout = 10 * time.Second

// grokHeldAnswerMax caps how long a Grok prompt answer is held for a stranded
// interjection's fallback turn (§2.8). Grok relay turns have no per-turn
// timeout of their own. A var so tests can shorten it.
var grokHeldAnswerMax = time.Hour

// grokStrandedAfterAnswer counts fallback turns of our interjections that
// started after their prompt's answer had already been published: the one
// order the hold cannot cover (§2.8 "Not held"). The certification run reads
// it; nothing else does.
var grokStrandedAfterAnswer atomic.Int64

func relayLiveFollowUpRPCID(attemptID string) string {
	return relayLiveFollowUpRPCIDPrefix + attemptID
}

func isRelayLiveFollowUpRPCID(id string) bool {
	return strings.HasPrefix(id, relayLiveFollowUpRPCIDPrefix) && len(id) > len(relayLiveFollowUpRPCIDPrefix)
}

/* --------------------------------------------------------------------------
   Frames the device builds
   -------------------------------------------------------------------------- */

type liveFollowUpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type codexSteerParams struct {
	ThreadID            string           `json:"threadId"`
	ExpectedTurnID      string           `json:"expectedTurnId"`
	ClientUserMessageID string           `json:"clientUserMessageId,omitempty"`
	Input               []codexTextInput `json:"input"`
}

type codexTextInput struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	TextElements []json.RawMessage `json:"text_elements"`
}

type grokInterjectParams struct {
	SessionID      string `json:"sessionId"`
	Text           string `json:"text"`
	InterjectionID string `json:"interjectionId"`
}

// codexSteerFrame is the `turn/steer` a Codex live attempt writes: the signed
// thread, turn and text, and the follow-up id as clientUserMessageId (which
// Codex echoes on the steered userMessage item but never de-duplicates on).
func codexSteerFrame(rpcID, threadID, expectedTurnID, followUpID, text string) (string, error) {
	return marshalLiveFollowUpRequest(rpcID, codexMethodTurnSteer, codexSteerParams{
		ThreadID:            threadID,
		ExpectedTurnID:      expectedTurnID,
		ClientUserMessageID: followUpID,
		Input:               []codexTextInput{{Type: "text", Text: text, TextElements: []json.RawMessage{}}},
	})
}

// grokInterjectFrame is the `_x.ai/interject` a Grok live attempt writes into
// the signed ACP session, with the attempt id as interjectionId.
func grokInterjectFrame(rpcID, acpSessionID, text, attemptID string) (string, error) {
	return marshalLiveFollowUpRequest(rpcID, grokMethodInterject, grokInterjectParams{
		SessionID:      acpSessionID,
		Text:           text,
		InterjectionID: attemptID,
	})
}

// marshalLiveFollowUpRequest is one JSONL frame: json.Marshal escapes every
// newline inside a string, so the frame is always a single line.
func marshalLiveFollowUpRequest(rpcID, method string, params any) (string, error) {
	b, err := json.Marshal(liveFollowUpRequest{JSONRPC: "2.0", ID: rpcID, Method: method, Params: params})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

/* --------------------------------------------------------------------------
   Reading the CLI's answer
   -------------------------------------------------------------------------- */

// liveFollowUpVerdict is a ledger row state (written, refused, unknown) and
// its reason.
type liveFollowUpVerdict struct {
	State  string
	Reason string
}

type jsonRPCAnswerError struct {
	Code    *int64          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// classifyLiveFollowUpAnswer maps the CLI's JSON-RPC answer to a live write
// onto the ledger (spec §2.3 "Settling from the answer"). A refusal is only
// ever a request-level one (-32600 / -32601 / -32602, or Codex's own "cannot
// steer" error): nothing reached the conversation, so the attempt is fenced
// for good. Anything else stays `unknown`, because bytes reached a CLI that
// may have acted on them.
func classifyLiveFollowUpAnswer(kind, frame string) liveFollowUpVerdict {
	var answer struct {
		Result json.RawMessage     `json:"result"`
		Error  *jsonRPCAnswerError `json:"error"`
	}
	if json.Unmarshal([]byte(frame), &answer) != nil {
		return liveFollowUpVerdict{State: followUpRowUnknown, Reason: followUpReasonAnswerUnrecognised}
	}
	if e := answer.Error; e != nil {
		if kind == codexCliAgentID && (bytes.Contains(e.Data, []byte(codexSteerErrNotSteerableErrorInfo)) ||
			strings.Contains(e.Message, codexSteerErrNotSteerableReview) ||
			strings.Contains(e.Message, codexSteerErrNotSteerableCompact)) {
			return refusedVerdict(followUpReasonTurnNotSteerable)
		}
		if e.Code == nil {
			return liveFollowUpVerdict{State: followUpRowUnknown}
		}
		switch *e.Code {
		case jsonRPCMethodNotFound:
			return refusedVerdict(followUpReasonMethodNotSupported)
		case jsonRPCInvalidRequest:
			if kind == codexCliAgentID {
				switch {
				case e.Message == codexSteerErrNoActiveTurn:
					return refusedVerdict(followUpReasonNoActiveTurn)
				case strings.HasPrefix(e.Message, codexSteerErrTurnMismatchPrefix):
					return refusedVerdict(followUpReasonTurnMismatch)
				}
			}
			return refusedVerdict(followUpReasonCliRefused)
		case jsonRPCInvalidParams:
			if kind == grokCliAgentID && bytes.Contains(e.Data, []byte("session not found")) {
				return refusedVerdict(followUpReasonConversationMismatch)
			}
			return refusedVerdict(followUpReasonCliRefused)
		}
		return liveFollowUpVerdict{State: followUpRowUnknown}
	}
	if len(answer.Result) == 0 || string(answer.Result) == "null" {
		return liveFollowUpVerdict{State: followUpRowUnknown, Reason: followUpReasonAnswerUnrecognised}
	}
	switch kind {
	case codexCliAgentID:
		var r struct {
			TurnID json.RawMessage `json:"turnId"`
		}
		var turnID string
		if json.Unmarshal(answer.Result, &r) == nil && json.Unmarshal(r.TurnID, &turnID) == nil {
			return liveFollowUpVerdict{State: followUpRowWritten}
		}
	case grokCliAgentID:
		// `{"result":{"status":"queued"}}`, nested twice (G3).
		var r struct {
			Result struct {
				Status string `json:"status"`
			} `json:"result"`
		}
		if json.Unmarshal(answer.Result, &r) == nil && r.Result.Status == "queued" {
			return liveFollowUpVerdict{State: followUpRowWritten}
		}
	}
	return liveFollowUpVerdict{State: followUpRowUnknown, Reason: followUpReasonAnswerUnrecognised}
}

func refusedVerdict(reason string) liveFollowUpVerdict {
	return liveFollowUpVerdict{State: followUpRowRefused, Reason: reason}
}

/* --------------------------------------------------------------------------
   The per-child tracker
   -------------------------------------------------------------------------- */

// jsonRPCFrame is the envelope of one JSON-RPC frame; the parts are kept raw.
type jsonRPCFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func parseJSONRPCFrame(line string) (jsonRPCFrame, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return jsonRPCFrame{}, false
	}
	var f jsonRPCFrame
	if json.Unmarshal([]byte(trimmed), &f) != nil {
		return jsonRPCFrame{}, false
	}
	return f, true
}

func (f jsonRPCFrame) hasID() bool { return len(f.ID) > 0 && string(f.ID) != "null" }

// isRequest: a method and an id (a notification has no id).
func (f jsonRPCFrame) isRequest() bool { return f.Method != "" && f.hasID() }

// isResponse: an id, no method, and a result or an error.
func (f jsonRPCFrame) isResponse() bool {
	return f.Method == "" && f.hasID() && (len(f.Result) > 0 || len(f.Error) > 0)
}

// idKey is the id's canonical JSON, so the number 7 and the string "7" are
// different requests, as they are on the wire.
func (f jsonRPCFrame) idKey() string { return jsonRPCIDKey(f.ID) }

func jsonRPCIDKey(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// stringID is the id when it is a JSON string.
func (f jsonRPCFrame) stringID() (string, bool) {
	var s string
	if json.Unmarshal(f.ID, &s) != nil {
		return "", false
	}
	return s, true
}

// liveFollowUpAnswer is what an answer waiter receives: the CLI's response
// frame, or exited when the child's stdout ended first.
type liveFollowUpAnswer struct {
	Frame  string
	Exited bool
}

// liveWriteState is the tracker's part of the stdin-boundary fence, read
// under its lock in the same step that admits the write.
type liveWriteState struct {
	Closed          bool
	ApprovalPending bool
	NoActiveTurn    bool
}

type grokInterjection struct {
	attemptID string
	text      string
	matched   bool
}

// grokTrackedPrompt is one client `session/prompt` from the moment it is
// written until its answer is published.
type grokTrackedPrompt struct {
	rpcKey string
	// internalID is Grok's own id for the prompt (`queue/changed`
	// runningPromptId while it runs; the answer's `_meta.promptId`, G13).
	internalID string
	// finished: its turn_completed (or its answer) has been read. A new
	// interjection is refused noActiveTurn from then on.
	finished      bool
	interjections []grokInterjection
	// fallbacks are the fallback turns, matched to this prompt's
	// interjections, whose turn_completed has not been read yet.
	fallbacks map[string]struct{}
	// answered: the answer was read while a fallback was pending, so it is
	// held in heldFrame until the last one completes.
	answered  bool
	heldFrame string
	heldTimer *time.Timer
}

type liveFollowUpTracker struct {
	kind string

	mu      sync.Mutex
	closed  bool
	waiters map[string]chan liveFollowUpAnswer
	open    map[string]struct{}

	// Grok only.
	acpSessionID string
	pendingNew   map[string]struct{}
	prompts      []*grokTrackedPrompt
	answered     []grokInterjection
	seen         []string
	released     []string

	// publishMu orders every publish the tracker makes (a released answer, the
	// cap, the flush at the end) against the reader detaching, which happens
	// before the session's publish queue is closed. Lock order: publishMu, then mu.
	publishMu sync.Mutex
	publish   func(string) bool
	detached  bool
}

// newLiveFollowUpTracker is the tracker for a session launched from version
// of cliAgentID, or nil when that version is not certified for live
// follow-ups: such a session never takes a live write, so it tracks nothing.
func newLiveFollowUpTracker(cliAgentID, launchedVersion string) *liveFollowUpTracker {
	if !liveFollowUpVersionCertified(cliAgentID, launchedVersion) {
		return nil
	}
	return newLiveFollowUpTrackerFor(cliAgentID)
}

func newLiveFollowUpTrackerFor(cliAgentID string) *liveFollowUpTracker {
	return &liveFollowUpTracker{
		kind:       cliAgentID,
		waiters:    map[string]chan liveFollowUpAnswer{},
		open:       map[string]struct{}{},
		pendingNew: map[string]struct{}{},
	}
}

// liveFollowUpOutbound is what outbound learned about a frame before it is
// written; done applies the rest once the write's outcome is known.
type liveFollowUpOutbound struct {
	t           *liveFollowUpTracker
	responseKey string
	loadSession string
	promptKey   string
	newKey      string
}

// outbound reads a frame about to be written to the child. A client
// `session/prompt` and `session/new` are recorded BEFORE the write, so the
// reader can never see Grok's answer to them first; done(false) takes them
// back when the write failed.
func (t *liveFollowUpTracker) outbound(frame string) liveFollowUpOutbound {
	if t == nil {
		return liveFollowUpOutbound{}
	}
	f, ok := parseJSONRPCFrame(frame)
	if !ok {
		return liveFollowUpOutbound{}
	}
	out := liveFollowUpOutbound{t: t}
	if f.isResponse() {
		out.responseKey = f.idKey()
		return out
	}
	if t.kind != grokCliAgentID || !f.isRequest() {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch f.Method {
	case acpMethodSessionPrompt:
		out.promptKey = f.idKey()
		t.prompts = append(t.prompts, &grokTrackedPrompt{rpcKey: out.promptKey, fallbacks: map[string]struct{}{}})
	case acpMethodSessionNew:
		out.newKey = f.idKey()
		t.pendingNew[out.newKey] = struct{}{}
	case acpMethodSessionLoad:
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(f.Params, &p) == nil {
			out.loadSession = p.SessionID
		}
	}
	return out
}

// done applies the write's outcome: an answer the device wrote to a
// client-bound request closes it, a `session/load` names the ACP session the
// child is on; a failed write takes back what outbound recorded.
func (o liveFollowUpOutbound) done(written bool) {
	t := o.t
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !written {
		if o.promptKey != "" {
			t.removePromptLocked(o.promptKey)
		}
		if o.newKey != "" {
			delete(t.pendingNew, o.newKey)
		}
		return
	}
	if o.responseKey != "" {
		delete(t.open, o.responseKey)
	}
	if o.loadSession != "" {
		t.acpSessionID = o.loadSession
	}
}

func (t *liveFollowUpTracker) removePromptLocked(key string) {
	for i, p := range t.prompts {
		if p.rpcKey == key {
			t.prompts = append(t.prompts[:i], t.prompts[i+1:]...)
			return
		}
	}
}

// inbound reads one frame off the child's stdout before it is published. It
// reports whether the frame is held back (a Grok prompt answer waiting for a
// stranded interjection's fallback turn); every other frame is published
// verbatim as before.
func (t *liveFollowUpTracker) inbound(frame string) (hold bool) {
	if t == nil {
		return false
	}
	f, ok := parseJSONRPCFrame(frame)
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case f.isResponse():
		if id, ok := f.stringID(); ok && isRelayLiveFollowUpRPCID(id) {
			if ch := t.waiters[id]; ch != nil {
				delete(t.waiters, id)
				ch <- liveFollowUpAnswer{Frame: strings.TrimSpace(frame)}
			}
			return false
		}
		if t.kind == grokCliAgentID {
			return t.grokResponseLocked(f, frame)
		}
	case f.isRequest():
		if t.kind == codexCliAgentID || f.Method == acpMethodRequestPermission {
			t.open[f.idKey()] = struct{}{}
		}
	case f.Method != "":
		switch t.kind {
		case codexCliAgentID:
			t.codexNotificationLocked(f)
		case grokCliAgentID:
			t.grokNotificationLocked(f)
		}
	}
	return false
}

func (t *liveFollowUpTracker) codexNotificationLocked(f jsonRPCFrame) {
	switch f.Method {
	case codexMethodTurnCompleted:
		// A turn end closes every request of that turn.
		t.open = map[string]struct{}{}
	case codexMethodServerRequestResolved:
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(f.Params, &p) == nil && len(p.RequestID) > 0 {
			delete(t.open, jsonRPCIDKey(p.RequestID))
		}
	}
}

func (t *liveFollowUpTracker) grokResponseLocked(f jsonRPCFrame, frame string) bool {
	key := f.idKey()
	if _, ok := t.pendingNew[key]; ok {
		delete(t.pendingNew, key)
		var r struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(f.Result, &r) == nil && r.SessionID != "" {
			t.acpSessionID = r.SessionID
		}
		return false
	}
	for i, p := range t.prompts {
		if p.rpcKey != key || p.answered {
			continue
		}
		// The prompt's answer ends its turn: its requests are closed with it.
		t.open = map[string]struct{}{}
		p.finished = true
		if len(p.fallbacks) > 0 {
			p.answered = true
			p.heldFrame = strings.TrimSpace(frame)
			p.heldTimer = time.AfterFunc(grokHeldAnswerMax, func() { t.releaseHeldPrompt(p) })
			fmt.Printf("%s[follow-up] holding a Grok prompt answer until %d stranded interjection(s) finish%s\n",
				colorCyan, len(p.fallbacks), colorReset)
			return true
		}
		t.prompts = append(t.prompts[:i], t.prompts[i+1:]...)
		t.rememberAnsweredLocked(p)
		return false
	}
	return false
}

func (t *liveFollowUpTracker) grokNotificationLocked(f jsonRPCFrame) {
	switch f.Method {
	case grokMethodQueueChanged:
		var p struct {
			RunningPromptID string `json:"runningPromptId"`
			RunningText     string `json:"runningText"`
		}
		if json.Unmarshal(f.Params, &p) == nil && p.RunningPromptID != "" {
			t.grokRunningLocked(p.RunningPromptID, p.RunningText)
		}
	case grokMethodSessionNotification:
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				PromptID      string `json:"prompt_id"`
			} `json:"update"`
		}
		if json.Unmarshal(f.Params, &p) == nil && p.Update.SessionUpdate == grokSessionUpdateTurnCompleted && p.Update.PromptID != "" {
			t.grokTurnCompletedLocked(p.Update.PromptID)
		}
	}
}

// grokRunningLocked reads a `queue/changed` naming the prompt that runs.
func (t *liveFollowUpTracker) grokRunningLocked(promptID, text string) {
	if !strings.HasPrefix(promptID, grokInterjectFallbackPromptPrefix) {
		// A client prompt began running: bind Grok's id to the oldest one
		// written and not yet bound.
		for _, p := range t.prompts {
			if p.internalID == promptID {
				return
			}
		}
		for _, p := range t.prompts {
			if p.internalID == "" && !p.finished && !p.answered {
				p.internalID = promptID
				return
			}
		}
		return
	}
	for _, id := range t.seen {
		if id == promptID {
			return
		}
	}
	// A fallback turn: an interjection of ours that missed its turn's last
	// drain, matched by its text to the first one not yet matched, in order.
	for _, p := range t.prompts {
		for i := range p.interjections {
			in := &p.interjections[i]
			if !in.matched && in.text == text {
				in.matched = true
				p.fallbacks[promptID] = struct{}{}
				t.seeFallbackLocked(promptID)
				return
			}
		}
	}
	for i := range t.answered {
		in := &t.answered[i]
		if !in.matched && in.text == text {
			in.matched = true
			t.seeFallbackLocked(promptID)
			grokStrandedAfterAnswer.Add(1)
			fmt.Printf("%s[follow-up] stranded interjection ran after its turn's answer (attempt %s)%s\n",
				colorYellow, in.attemptID, colorReset)
			return
		}
	}
}

func (t *liveFollowUpTracker) seeFallbackLocked(promptID string) {
	t.seen = append(t.seen, promptID)
	if len(t.seen) > grokSeenFallbacksMax {
		t.seen = t.seen[len(t.seen)-grokSeenFallbacksMax:]
	}
}

// grokTurnCompletedLocked reads a `turn_completed` session notification.
func (t *liveFollowUpTracker) grokTurnCompletedLocked(promptID string) {
	if !strings.HasPrefix(promptID, grokInterjectFallbackPromptPrefix) {
		for _, p := range t.prompts {
			if p.internalID == promptID {
				p.finished = true
			}
		}
		return
	}
	for i, p := range t.prompts {
		if _, ok := p.fallbacks[promptID]; !ok {
			continue
		}
		delete(p.fallbacks, promptID)
		if len(p.fallbacks) == 0 && p.answered {
			// The last of its fallbacks finished: the held answer goes out
			// right after this frame, under a later seq.
			if p.heldTimer != nil {
				p.heldTimer.Stop()
			}
			t.released = append(t.released, p.heldFrame)
			t.prompts = append(t.prompts[:i], t.prompts[i+1:]...)
			t.rememberAnsweredLocked(p)
			fmt.Printf("%s[follow-up] releasing a held Grok prompt answer%s\n", colorCyan, colorReset)
		}
		return
	}
}

// rememberAnsweredLocked keeps an answered prompt's unmatched interjections,
// only so a fallback of one that starts after the answer can be counted.
func (t *liveFollowUpTracker) rememberAnsweredLocked(p *grokTrackedPrompt) {
	for _, in := range p.interjections {
		if !in.matched {
			t.answered = append(t.answered, in)
		}
	}
	if len(t.answered) > grokAnsweredInterjectionsMax {
		t.answered = t.answered[len(t.answered)-grokAnsweredInterjectionsMax:]
	}
}

// runningPromptLocked is the client prompt whose turn is running: written,
// bound to Grok's running id, and neither finished nor answered.
func (t *liveFollowUpTracker) runningPromptLocked() *grokTrackedPrompt {
	for _, p := range t.prompts {
		if p.internalID != "" && !p.finished && !p.answered {
			return p
		}
	}
	return nil
}

// approvalPending reports an open client-bound request on this child.
func (t *liveFollowUpTracker) approvalPending() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.open) > 0
}

// grokTurnRunning reports whether a client prompt's turn is running (Grok).
func (t *liveFollowUpTracker) grokTurnRunning() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runningPromptLocked() != nil
}

// acpSession is the ACP session the Grok child is on ("" before one is known).
func (t *liveFollowUpTracker) acpSession() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.acpSessionID
}

// admitLiveWrite is the tracker's half of the stdin-boundary fence: under its
// lock it reads its state, asks fence for a refusal, and only when there is
// none registers the answer waiter for rpcID and (Grok) records the
// interjection on the running prompt — so a turn cannot finish between the
// check and the record. Called under the session's stdinMu, right before the
// write.
func (t *liveFollowUpTracker) admitLiveWrite(rpcID, attemptID, text string, fence func(liveWriteState) string) (<-chan liveFollowUpAnswer, string) {
	if t == nil {
		return nil, fence(liveWriteState{Closed: true})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := liveWriteState{Closed: t.closed, ApprovalPending: len(t.open) > 0}
	var running *grokTrackedPrompt
	if t.kind == grokCliAgentID {
		running = t.runningPromptLocked()
		state.NoActiveTurn = running == nil
	}
	if reason := fence(state); reason != "" {
		return nil, reason
	}
	ch := make(chan liveFollowUpAnswer, 1)
	t.waiters[rpcID] = ch
	if running != nil {
		running.interjections = append(running.interjections, grokInterjection{attemptID: attemptID, text: text})
	}
	return ch, ""
}

// forget drops the answer waiter for rpcID (a failed write, or the late
// window ended). An answer read afterwards is still forwarded as a frame.
func (t *liveFollowUpTracker) forget(rpcID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiters, rpcID)
}

// attachPublisher gives the tracker the reader's own publish (Grok), through
// which a held answer is released under a fresh seq.
func (t *liveFollowUpTracker) attachPublisher(publish func(string) bool) {
	if t == nil {
		return
	}
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	t.publish = publish
}

// publishReleased publishes the answers released by the frame just
// published. False only when a publish failed (the session is failing).
func (t *liveFollowUpTracker) publishReleased() bool {
	if t == nil {
		return true
	}
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	if t.detached || t.publish == nil {
		return true
	}
	t.mu.Lock()
	frames := t.released
	t.released = nil
	t.mu.Unlock()
	for _, frame := range frames {
		if !t.publish(frame) {
			return false
		}
	}
	return true
}

// releaseHeldPrompt releases one held answer at the cap (grokHeldAnswerMax).
func (t *liveFollowUpTracker) releaseHeldPrompt(held *grokTrackedPrompt) {
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	if t.detached || t.publish == nil {
		return
	}
	t.mu.Lock()
	frame := ""
	for i, p := range t.prompts {
		if p == held && p.answered {
			frame = p.heldFrame
			t.prompts = append(t.prompts[:i], t.prompts[i+1:]...)
			t.rememberAnsweredLocked(p)
			break
		}
	}
	t.mu.Unlock()
	if frame == "" {
		return
	}
	fmt.Printf("%s[follow-up] releasing a Grok prompt answer held for %s%s\n", colorYellow, grokHeldAnswerMax, colorReset)
	t.publish(frame)
}

// readerDone is called once the child's stdout reader stops. Every answer
// waiter learns the child is gone (its attempt is answered unknown) and every
// open request is dropped. With flush (the reader reached EOF) any held or
// released answer is published first, so before the session's ended frame;
// after a fatal stop nothing more is published. Idempotent.
func (t *liveFollowUpTracker) readerDone(flush bool) {
	if t == nil {
		return
	}
	t.publishMu.Lock()
	defer t.publishMu.Unlock()
	if t.detached {
		return
	}
	t.detached = true
	t.mu.Lock()
	frames := t.released
	t.released = nil
	for _, p := range t.prompts {
		if p.heldTimer != nil {
			p.heldTimer.Stop()
		}
		if p.answered {
			frames = append(frames, p.heldFrame)
		}
	}
	t.prompts = nil
	t.closed = true
	t.open = map[string]struct{}{}
	waiters := t.waiters
	t.waiters = map[string]chan liveFollowUpAnswer{}
	t.mu.Unlock()
	for _, ch := range waiters {
		ch <- liveFollowUpAnswer{Exited: true}
	}
	if !flush || t.publish == nil {
		return
	}
	for _, frame := range frames {
		if !t.publish(frame) {
			return
		}
	}
}
