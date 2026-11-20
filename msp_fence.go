// File: msp_fence.go
// -----------------------------------------------------------------------------
// The client-frame fence of Muse Code's MSP driver (`muse serve`, L5 spec
// §2.3): the MSP half of acpClientFence (acp_fence.go), chosen by the
// profile's protocol. Every frame a SEND carries is checked BEFORE it reaches
// the host, and one outside the fence is refused with a typed
// `musecode_msp_error` (errorCode ACP_FRAME_REFUSED) and never written.
//
// terminal-service builds every frame these sessions get (its MSP handshake,
// the `turn/start` it wraps a person's text in, a person's `approval/decide`,
// the receipts for the host's requests), so the fence never gets in its way.
// It exists so that nothing else can: a frame that loosens whether Muse asks,
// saves a rule the person did not make, widens where the session works, or
// opens a second conversation in the same host.
//
//   - Requests: only the methods below, each with only the params named. One
//     host serves ONE conversation: `session/start` / `session/resume` are
//     admitted only until one has been answered with a session (a frame
//     identical to one already written is a redelivery and passes: Muse's own
//     `commandId` idempotency answers it again), and every later frame must
//     name that session.
//       initialize               once (an identical redelivery passes); only
//                                `clientInfo` and `capabilities.userInputDialogs`
//                                false: no requested capability grants
//       session/start            `workspaceRoot` inside the session's own start
//                                folder; `approvalMode` exactly promptUnmatched;
//                                never a `sessionId` (only the host mints one),
//                                `permissionProfile`, `workspaceRoots` or `config`
//       session/resume           a UUID `sessionId`; never `permissionProfile` or
//                                `config`. The answer's `session.workspaceRoot`
//                                must be inside the start folder, else the host
//                                is ended (msp inbound) and the answer never sent
//       session/setApprovalMode  only to promptUnmatched (never allowAll)
//       turn/start               `ifBusy` absent or "queue"; never `userShell`
//                                or `workspaceRoots`
//       approval/decide          an approval the host asked for and has not
//                                resolved, with one of ITS choices whose scope
//                                is "once" and decision approved, denied or
//                                abort (Muse 1.4.20's "Reject": it rejects that
//                                one call and the turn goes on). Never a
//                                feedback text.
//       session/setModel, session/setReasoningEffort, turn/interrupt,
//       turn/cancel, model/list, usage/read, approval/listPending
//   - Refused by name: `turn/steer` (only the device's follow-up ledger writes
//     one, sendDeviceFrame), and every other method: session/fork, sideChat,
//     userShell, plugin/*, skill/*, subagent/*, workflow/*, goal/*, session/list.
//   - The one notification: `initialized`.
//   - Responses: only the presentation receipt `{"result":{}}`, and only to an
//     `approval/request` or `userInput/request` the host made and that has not
//     had one. The decision is never a response: it is `approval/decide`.
//
// What this guarantees: AI Expedite never selects a mode that runs actions
// unasked, never saves a session-wide or persistent rule, never widens the
// workspace, and never re-creates a gone conversation under its old id. The
// person's own Muse settings can still allow an action (musecode_msp_driver.go).
//
// The fence reads the envelope and the params named above, never prompt text.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// mspFenceRequestParams are the params each method a client may send can
// carry. Anything else in params is refused.
var mspFenceRequestParams = map[string][]string{
	mspMethodInitialize:             {"clientInfo", "capabilities"},
	mspMethodSessionStart:           {"commandId", "workspaceRoot", "approvalMode", "modelId", "previousSessionId"},
	mspMethodSessionResume:          {"commandId", "sessionId", "excludeItems", "modelId"},
	mspMethodSessionSetApprovalMode: {"commandId", "sessionId", "mode"},
	mspMethodSessionSetModel:        {"commandId", "sessionId", "model"},
	mspMethodSessionSetEffort:       {"commandId", "sessionId", "reasoningEffort"},
	mspMethodTurnStart:              {"commandId", "sessionId", "ifBusy", "input", "reasoningEffort"},
	mspMethodTurnInterrupt:          {"commandId", "sessionId", "turnId"},
	mspMethodTurnCancel:             {"commandId", "sessionId", "turnId"},
	mspMethodApprovalDecide:         {"commandId", "sessionId", "approvalId", "choiceId", "requirementId"},
	mspMethodApprovalListPending:    {"sessionId"},
	mspMethodModelList:              {"sessionId"},
	mspMethodUsageRead:              {},
}

// mspSessionScopedMethods must name the session the host serves.
var mspSessionScopedMethods = []string{
	mspMethodSessionSetApprovalMode, mspMethodSessionSetModel, mspMethodSessionSetEffort,
	mspMethodTurnStart, mspMethodTurnInterrupt, mspMethodTurnCancel, mspMethodApprovalDecide,
	mspMethodApprovalListPending,
}

// mspFenceState is the MSP fence's own record of the host. Guarded by the
// fence's mu. The host's open requests awaiting a receipt are the fence's
// `open` map (acp_fence.go), keyed by JSON-RPC id.
type mspFenceState struct {
	// initCanon is the `initialize` written (or being written), canonical.
	initCanon string
	// pendingKey / pendingCanon / pendingMethod: a session setup written and
	// not answered yet.
	pendingKey, pendingCanon, pendingMethod string
	// sessionID is the conversation this host serves, from the first setup
	// answered with one; setupCanon is that setup.
	sessionID  string
	setupCanon string
	// approvals are the approvals the host asked for and has not resolved, by
	// approvalId.
	approvals     map[string]mspFenceApproval
	approvalOrder []string
}

type mspFenceApproval struct {
	turnID  string
	choices map[string]mspFenceChoice
}

type mspFenceChoice struct {
	decision string
	scope    string
}

// mspFenceStep is what a passed check still has to settle at the write.
type mspFenceStep int

const (
	mspStepNone mspFenceStep = iota
	mspStepInitialize
	mspStepSetup
	mspStepReceipt
)

func newMSPFenceState() *mspFenceState {
	return &mspFenceState{approvals: map[string]mspFenceApproval{}}
}

// canonicalJSONRPC is a frame's compact form, the key "identical frame" is
// compared on.
func canonicalJSONRPC(frame string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(strings.TrimSpace(frame))) != nil {
		return strings.TrimSpace(frame)
	}
	return buf.String()
}

// checkMSP decides whether frame may be written to the host. root is the
// session's symlink-resolved start folder ("" only on a test fixture).
func (f *acpClientFence) checkMSP(frame, root string) (acpFenceCheck, error) {
	rpc, ok := parseJSONRPCFrame(frame)
	if !ok {
		return acpFenceCheck{}, f.refuse("not a JSON-RPC object")
	}
	if rpc.Method == "" {
		if !rpc.isResponse() {
			return acpFenceCheck{}, f.refuse("neither a request, a notification nor a response")
		}
		return f.checkMSPReceipt(rpc, frame)
	}
	if !rpc.hasID() {
		if rpc.Method != mspMethodInitialized {
			return acpFenceCheck{}, f.refuse("notification %q is not one a client may send", rpc.Method)
		}
		if !mspParamsEmpty(rpc.Params) {
			return acpFenceCheck{}, f.refuse("%s carries params", rpc.Method)
		}
		return acpFenceCheck{}, nil
	}
	if rpc.Method == mspMethodTurnSteer {
		return acpFenceCheck{}, f.refuse("turn/steer is written only by this computer's follow-up ledger")
	}
	allowed, known := mspFenceRequestParams[rpc.Method]
	if !known {
		return acpFenceCheck{}, f.refuse("request %q is not one a client may send", rpc.Method)
	}
	params := map[string]json.RawMessage{}
	if len(rpc.Params) > 0 && string(rpc.Params) != "null" {
		if err := json.Unmarshal(rpc.Params, &params); err != nil || params == nil {
			return acpFenceCheck{}, f.refuse("%s params are not an object", rpc.Method)
		}
	}
	for name := range params {
		if !containsString(allowed, name) {
			return acpFenceCheck{}, f.refuse("%s may not carry %s", rpc.Method, name)
		}
	}
	str := func(name string) (string, bool) {
		var s string
		raw, present := params[name]
		if !present || json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	canon := canonicalJSONRPC(frame)

	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.msp
	if containsString(mspSessionScopedMethods, rpc.Method) {
		id, _ := str("sessionId")
		if st.sessionID == "" || id != st.sessionID {
			return acpFenceCheck{}, f.refuse("%s names a session this host does not serve", rpc.Method)
		}
	}
	switch rpc.Method {
	case mspMethodInitialize:
		if err := mspCheckInitializeParams(params); err != "" {
			return acpFenceCheck{}, f.refuse("initialize %s", err)
		}
		if st.initCanon != "" && st.initCanon != canon {
			return acpFenceCheck{}, f.refuse("initialize was already sent to this host")
		}
		return acpFenceCheck{mspStep: mspStepInitialize, mspCanon: canon}, nil
	case mspMethodSessionStart:
		wr, ok := str("workspaceRoot")
		if !ok {
			return acpFenceCheck{}, f.refuse("session/start names no workspaceRoot")
		}
		if root != "" {
			if err := checkMSPWorkspaceRoot("session/start workspaceRoot", wr, root); err != nil {
				return acpFenceCheck{}, f.refuse("%v", err)
			}
		}
		if mode, _ := str("approvalMode"); mode != museMSPChatApprovalMode {
			return acpFenceCheck{}, f.refuse("session/start must select approval mode %s", museMSPChatApprovalMode)
		}
		return f.checkMSPSetupLocked(rpc, canon)
	case mspMethodSessionResume:
		id, _ := str("sessionId")
		if !museCodeSessionIDPattern.MatchString(id) {
			return acpFenceCheck{}, f.refuse("session/resume names no session id")
		}
		return f.checkMSPSetupLocked(rpc, canon)
	case mspMethodSessionSetApprovalMode:
		if mode, _ := str("mode"); mode != museMSPChatApprovalMode {
			return acpFenceCheck{}, f.refuse("session/setApprovalMode may only select %s", museMSPChatApprovalMode)
		}
	case mspMethodTurnStart:
		if _, present := params["ifBusy"]; present {
			if ifBusy, _ := str("ifBusy"); ifBusy != mspIfBusyQueue {
				return acpFenceCheck{}, f.refuse("turn/start may only queue behind a running turn (ifBusy %q)", mspIfBusyQueue)
			}
		}
	case mspMethodApprovalDecide:
		approvalID, _ := str("approvalId")
		choiceID, _ := str("choiceId")
		approval, open := st.approvals[approvalID]
		if !open {
			return acpFenceCheck{}, f.refuse("approval/decide names an approval the host is not asking about")
		}
		choice, offered := approval.choices[choiceID]
		if !offered {
			return acpFenceCheck{}, f.refuse("approval/decide selects a choice the request did not offer")
		}
		if choice.scope != mspApprovalScopeOnce || !mspFenceAnswerDecision(choice.decision) {
			return acpFenceCheck{}, f.refuse("approval/decide selects a %s %s choice (only a once-scoped approve, deny or reject)", choice.scope, choice.decision)
		}
	case mspMethodModelList:
		if id, present := str("sessionId"); present && id != st.sessionID {
			return acpFenceCheck{}, f.refuse("model/list names a session this host does not serve")
		}
	}
	return acpFenceCheck{}, nil
}

// mspFenceAnswerDecision reports whether a person's `approval/decide` may
// select a choice with this decision (its scope must also be "once"): an
// approve, or one of shared-constants MUSE_MSP_DENY_ONCE_DECISIONS. Muse Code
// 1.4.20 offers no `denied` choice on a command approval; its "Reject" is
// `abort`, scope "once", which rejects that one tool call (the item ends
// `rejected`) while the turn goes on. Never a decision that saves a rule
// (`approvedPolicyAmendment`, `deniedPolicyAmendment`) or grants the session.
// Reported to terminal-service as MSP_DRIVERS protocol 2
// (mspDriversProtocol): an app at protocol 1 refused `abort`, so it is never
// sent one.
func mspFenceAnswerDecision(decision string) bool {
	switch decision {
	case mspApprovalApproved, mspApprovalDenied, mspApprovalAbort:
		return true
	}
	return false
}

// mspCheckInitializeParams refuses an `initialize` asking for anything but
// the client's name and "no dialogs": a requested capability would grant the
// client host powers (a user shell, session MCP servers) the fence never sees.
func mspCheckInitializeParams(params map[string]json.RawMessage) string {
	raw, present := params["capabilities"]
	if !present || string(raw) == "null" {
		return ""
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil || caps == nil {
		return "capabilities are not an object"
	}
	for name, v := range caps {
		if name != "userInputDialogs" {
			return "may not ask for " + name
		}
		var dialogs bool
		if json.Unmarshal(v, &dialogs) != nil || dialogs {
			return "may only declare userInputDialogs false"
		}
	}
	return ""
}

// checkMSPSetupLocked admits a `session/start` or `session/resume` only
// while the host serves no conversation and no other setup is in flight; an
// identical frame (a redelivery) passes either way.
func (f *acpClientFence) checkMSPSetupLocked(rpc jsonRPCFrame, canon string) (acpFenceCheck, error) {
	st := f.msp
	switch {
	case st.sessionID != "" && canon != st.setupCanon:
		return acpFenceCheck{}, f.refuse("%s: this host already serves a conversation", rpc.Method)
	case st.sessionID == "" && st.pendingKey != "" && canon != st.pendingCanon:
		return acpFenceCheck{}, f.refuse("%s: another session setup is still in flight on this host", rpc.Method)
	}
	return acpFenceCheck{mspStep: mspStepSetup, mspCanon: canon, mspKey: rpc.idKey(), mspMethod: rpc.Method}, nil
}

// checkMSPReceipt admits only the presentation receipt, to a host request
// that is open.
func (f *acpClientFence) checkMSPReceipt(rpc jsonRPCFrame, frame string) (acpFenceCheck, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(frame), &top) != nil {
		return acpFenceCheck{}, f.refuse("a response that is not an object")
	}
	for name := range top {
		if name != "jsonrpc" && name != "id" && name != "result" {
			return acpFenceCheck{}, f.refuse("a response may only be the presentation receipt")
		}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(rpc.Result, &result) != nil || result == nil || len(result) != 0 {
		return acpFenceCheck{}, f.refuse("a response may only be the presentation receipt (an empty result)")
	}
	key := rpc.idKey()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, open := f.open[key]; !open {
		return acpFenceCheck{}, f.refuse("a response to id %s, which is not an open host request", key)
	}
	return acpFenceCheck{responseKey: key, mspStep: mspStepReceipt}, nil
}

// admitMSP re-decides, under the session's stdin lock and immediately before
// the write, what a concurrent write could have changed since checkMSP, and
// reserves it: the `initialize`, the session setup in flight, a receipt.
func (f *acpClientFence) admitMSP(c acpFenceCheck) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.msp
	switch c.mspStep {
	case mspStepInitialize:
		if st.initCanon != "" && st.initCanon != c.mspCanon {
			return f.refuse("initialize was already sent to this host")
		}
		st.initCanon = c.mspCanon
	case mspStepSetup:
		switch {
		case st.sessionID != "":
			if c.mspCanon != st.setupCanon {
				return f.refuse("%s: this host already serves a conversation", c.mspMethod)
			}
		case st.pendingKey != "":
			if c.mspCanon != st.pendingCanon {
				return f.refuse("%s: another session setup is still in flight on this host", c.mspMethod)
			}
		default:
			st.pendingKey, st.pendingCanon, st.pendingMethod = c.mspKey, c.mspCanon, c.mspMethod
		}
	case mspStepReceipt:
		if _, open := f.open[c.responseKey]; !open {
			return f.refuse("a response to id %s, which is not an open host request", c.responseKey)
		}
	}
	return nil
}

// settleMSP records what a write's outcome completes. An `initialize` or a
// setup that never reached the host is released, so terminal-service can
// send it again.
func (f *acpClientFence) settleMSP(c acpFenceCheck, wrote bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.msp
	switch {
	case wrote && c.responseKey != "":
		delete(f.open, c.responseKey)
	case !wrote && c.mspStep == mspStepInitialize && st.initCanon == c.mspCanon && st.sessionID == "":
		st.initCanon = ""
	case !wrote && c.mspStep == mspStepSetup && st.pendingKey == c.mspKey && st.pendingCanon == c.mspCanon:
		st.pendingKey, st.pendingCanon, st.pendingMethod = "", "", ""
	}
}

// inboundMSP reads one frame off the host's stdout before it is published.
// It returns an error only for the answer to a `session/resume` whose
// conversation works outside the session's start folder: that answer is never
// published and the host is ended (acp_family.go).
func (f *acpClientFence) inboundMSP(line, root string) error {
	rpc, ok := parseJSONRPCFrame(line)
	if !ok {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.msp
	switch {
	case rpc.isResponse():
		if st.pendingKey == "" || rpc.idKey() != st.pendingKey {
			return nil
		}
		method, canon := st.pendingMethod, st.pendingCanon
		st.pendingKey, st.pendingCanon, st.pendingMethod = "", "", ""
		if len(rpc.Error) > 0 {
			return nil
		}
		var r struct {
			Session struct {
				SessionID     string  `json:"sessionId"`
				WorkspaceRoot *string `json:"workspaceRoot"`
			} `json:"session"`
		}
		if json.Unmarshal(rpc.Result, &r) != nil || r.Session.SessionID == "" {
			return nil
		}
		if method == mspMethodSessionResume && root != "" {
			if r.Session.WorkspaceRoot == nil || *r.Session.WorkspaceRoot == "" {
				return f.refuse("the resumed conversation names no workspace folder, so it cannot be held to this session's folder %q", root)
			}
			if err := checkMSPWorkspaceRoot("the resumed conversation's workspaceRoot", *r.Session.WorkspaceRoot, root); err != nil {
				return f.refuse("%v", err)
			}
		}
		st.sessionID, st.setupCanon = r.Session.SessionID, canon
	case rpc.isRequest():
		if rpc.Method != mspServerRequestApproval && rpc.Method != mspServerRequestUserInput {
			return nil
		}
		f.recordOpenLocked(rpc.idKey(), acpOpenRequest{method: rpc.Method})
		if rpc.Method == mspServerRequestApproval {
			f.recordMSPApprovalLocked(rpc.Params)
		}
	case rpc.Method == mspNotifyApprovalResolved:
		var p struct {
			ApprovalID string `json:"approvalId"`
		}
		if json.Unmarshal(rpc.Params, &p) == nil && p.ApprovalID != "" {
			delete(st.approvals, p.ApprovalID)
		}
	case rpc.Method == mspNotifyTurnCompleted:
		var p struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(rpc.Params, &p)
		for id, a := range st.approvals {
			if p.TurnID == "" || a.turnID == "" || a.turnID == p.TurnID {
				delete(st.approvals, id)
			}
		}
	}
	return nil
}

// recordMSPApprovalLocked keeps an `approval/request`'s choices under its
// approvalId; a later request for the same approval (the next stage of a
// shell pipeline, or a re-issue) replaces it.
func (f *acpClientFence) recordMSPApprovalLocked(params json.RawMessage) {
	var p struct {
		ApprovalID       string `json:"approvalId"`
		TurnID           string `json:"turnId"`
		AvailableChoices []struct {
			ChoiceID string `json:"choiceId"`
			Decision string `json:"decision"`
			Scope    string `json:"scope"`
		} `json:"availableChoices"`
	}
	if json.Unmarshal(params, &p) != nil || p.ApprovalID == "" {
		return
	}
	st := f.msp
	a := mspFenceApproval{turnID: p.TurnID, choices: map[string]mspFenceChoice{}}
	for _, c := range p.AvailableChoices {
		if c.ChoiceID != "" {
			a.choices[c.ChoiceID] = mspFenceChoice{decision: c.Decision, scope: c.Scope}
		}
	}
	if _, seen := st.approvals[p.ApprovalID]; !seen {
		st.approvalOrder = append(st.approvalOrder, p.ApprovalID)
	}
	st.approvals[p.ApprovalID] = a
	// The same bound as the open requests: drop the oldest still listed.
	if len(st.approvalOrder) > acpFenceOpenRequestsMax {
		kept := st.approvalOrder[:0]
		for _, id := range st.approvalOrder {
			if _, open := st.approvals[id]; open {
				kept = append(kept, id)
			}
		}
		st.approvalOrder = kept
	}
	for len(st.approvals) > acpFenceOpenRequestsMax && len(st.approvalOrder) > 0 {
		delete(st.approvals, st.approvalOrder[0])
		st.approvalOrder = st.approvalOrder[1:]
	}
}

// checkMSPWorkspaceRoot requires path to resolve inside root, the session's
// own start folder (the check ACP setup frames get, checkACPSetupCwd). A
// Windows extended-length path to a local drive (`\\?\C:\...`) is the same
// folder as its plain form (localExtendedLengthPath).
func checkMSPWorkspaceRoot(what, path, root string) error {
	path = localExtendedLengthPath(path)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q is not an absolute path", what, path)
	}
	resolved, err := resolveCwdForContainment(path)
	if err != nil {
		return fmt.Errorf("%s %q could not be safely resolved: %w", what, path, err)
	}
	if !resolvedPathWithin(root, resolved) {
		return fmt.Errorf("%s %q is outside the session's folder %q (the directory the session was started in)", what, resolved, root)
	}
	return nil
}

// localExtendedLengthPath is path without its Windows extended-length prefix
// when it names a folder on a local drive (`\\?\C:\dir` → `C:\dir`), and path
// unchanged otherwise. Muse 1.4.20 reports the workspace folder of a
// conversation `muse exec` started in that form (2026-11-16, S18), so the
// fence read the session's own folder as another one and ended the host.
// Windows does not normalise a path behind the prefix, so one whose plain form
// would differ once cleaned (`.` or `..` components, doubled separators) is
// left as it is and refused.
func localExtendedLengthPath(path string) string {
	const prefix = `\\?\`
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || len(rest) < 3 || rest[1] != ':' || rest[2] != '\\' {
		return path
	}
	if d := rest[0] | 0x20; d < 'a' || d > 'z' {
		return path
	}
	if strings.Contains(rest, `\\`) || strings.Contains(rest, "/") {
		return path
	}
	for _, part := range strings.Split(rest[3:], `\`) {
		if part == "." || part == ".." {
			return path
		}
	}
	return rest
}

// mspParamsEmpty: absent, null or an empty object.
func mspParamsEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && len(m) == 0
}

// mspHostSession is the conversation the host serves ("" before a setup was
// answered). Tests read it.
func (f *acpClientFence) mspHostSession() string {
	if f == nil || f.msp == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msp.sessionID
}
