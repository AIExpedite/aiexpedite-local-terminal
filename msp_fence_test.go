package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The frames below are the ones terminal-service #500 builds
// (mspHandshake.service.js, voiceRelay.service.js), key order included.

const (
	mspFenceSession = "01a1dc92-617d-7450-a3d3-88e5dcc52575"
	mspFenceCmd1    = "01a1dc92-0000-7000-8000-000000000001"
	mspFenceCmd2    = "01a1dc92-0000-7000-8000-000000000002"
)

func newTestMSPFence(t *testing.T) (*acpClientFence, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newACPClientFence(museCodeMSPProfile), root
}

// mspSend runs a frame through the whole fence as a successful write does:
// check, admit under the stdin lock, written.
func mspSend(f *acpClientFence, frame, root string) error {
	c, err := f.check(frame, root)
	if err != nil {
		return err
	}
	if err := f.admit(c); err != nil {
		return err
	}
	f.written(c)
	return nil
}

func mspStartFrame(root string, extra string) string {
	return `{"jsonrpc":"2.0","id":"relay-msp-start","method":"session/start","params":{"commandId":"` + mspFenceCmd1 +
		`","workspaceRoot":` + strconv.Quote(root) + `,"approvalMode":"promptUnmatched"` + extra + `}}`
}

func mspStartAnswer(root string) string {
	return `{"jsonrpc":"2.0","id":"relay-msp-start","result":{"session":{"sessionId":"` + mspFenceSession +
		`","workspaceRoot":` + strconv.Quote(root) + `,"status":"idle","approvalMode":{"mode":"promptUnmatched","source":"startup"}}}}`
}

// establishMSPSession runs the handshake terminal-service sends for a fresh
// session and returns the fence holding it.
func establishMSPSession(t *testing.T) (*acpClientFence, string) {
	t.Helper()
	f, root := newTestMSPFence(t)
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"aiexpedite","version":"1"},"capabilities":{"userInputDialogs":false}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		mspStartFrame(root, `,"modelId":"muse-wonder-full-1.0"`),
	} {
		if err := mspSend(f, frame, root); err != nil {
			t.Fatalf("%s: %v", frame, err)
		}
	}
	if err := f.inboundFrame(mspStartAnswer(root), root); err != nil {
		t.Fatal(err)
	}
	if f.mspHostSession() != mspFenceSession {
		t.Fatal("the start's answer did not name the host's session")
	}
	return f, root
}

const mspApprovalRequest = `{"jsonrpc":"2.0","id":7,"method":"approval/request","params":{"approvalId":"apr-1","sessionId":"` + mspFenceSession + `","turnId":"turn-1",` +
	`"currentRequirementId":{"stage":0},"judgeEscalated":false,"subject":{"kind":"shell"},"availableChoices":[` +
	`{"choiceId":"c-once","decision":"approved","scope":"once","label":"Allow once"},` +
	`{"choiceId":"c-session","decision":"approvedForSession","scope":"session","label":"Allow for this session"},` +
	`{"choiceId":"c-always","decision":"approved","scope":"localPersistent","label":"Always allow"},` +
	`{"choiceId":"c-deny","decision":"denied","scope":"once","label":"Deny"}]}}`

func mspDecide(choiceID string) string {
	return `{"jsonrpc":"2.0","id":"aix-msp-decide-` + mspFenceCmd2 + `","method":"approval/decide","params":{"commandId":"` + mspFenceCmd2 +
		`","sessionId":"` + mspFenceSession + `","approvalId":"apr-1","choiceId":"` + choiceID + `","requirementId":{"stage":0}}}`
}

// Every frame terminal-service sends passes.
func TestTheMSPFenceLetsThroughWhatTerminalServiceSends(t *testing.T) {
	f, root := establishMSPSession(t)
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"relay-msp-approval-mode","method":"session/setApprovalMode","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","mode":"promptUnmatched"}}`,
		`{"jsonrpc":"2.0","id":"relay-turn-t1","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","ifBusy":"queue","input":[{"type":"text","text":"hi"}],"reasoningEffort":"low"}}`,
		`{"jsonrpc":"2.0","id":"acp-turn-t2","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","ifBusy":"queue","input":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":"relay-turn-t3","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","input":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":"i1","method":"turn/interrupt","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","turnId":"turn-1"}}`,
		`{"jsonrpc":"2.0","id":"m1","method":"session/setModel","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","model":"m"}}`,
		`{"jsonrpc":"2.0","id":"m2","method":"model/list","params":{}}`,
		`{"jsonrpc":"2.0","id":"u1","method":"usage/read"}`,
	} {
		if err := mspSend(f, frame, root); err != nil {
			t.Errorf("%s: %v", frame, err)
		}
	}
	// A host request gets its receipt, then a person's once-scoped choices.
	if err := f.inboundFrame(mspApprovalRequest, root); err != nil {
		t.Fatal(err)
	}
	if err := mspSend(f, `{"jsonrpc":"2.0","id":7,"result":{}}`, root); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	for _, choice := range []string{"c-once", "c-deny"} {
		if err := mspSend(f, mspDecide(choice), root); err != nil {
			t.Errorf("decide %s: %v", choice, err)
		}
	}
	if err := f.inboundFrame(`{"jsonrpc":"2.0","id":"q-9","method":"userInput/request","params":{"userInputId":"ui-1","turnId":"turn-1"}}`, root); err != nil {
		t.Fatal(err)
	}
	if err := mspSend(f, `{"jsonrpc":"2.0","id":"q-9","result":{}}`, root); err != nil {
		t.Fatalf("userInput receipt: %v", err)
	}
}

func TestTheMSPFenceRefusesWhatWouldLoosenTheSession(t *testing.T) {
	f, root := establishMSPSession(t)
	if err := f.inboundFrame(mspApprovalRequest, root); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Dir(root)
	cases := map[string]string{
		"a client turn/steer":            `{"jsonrpc":"2.0","id":"x","method":"turn/steer","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","expectedTurnId":"turn-1","input":[{"type":"text","text":"x"}]}}`,
		"setApprovalMode to allowAll":    `{"jsonrpc":"2.0","id":"x","method":"session/setApprovalMode","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","mode":"allowAll"}}`,
		"setApprovalMode to onRequest":   `{"jsonrpc":"2.0","id":"x","method":"session/setApprovalMode","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","mode":"onRequest"}}`,
		"turn/start ifBusy steer":        `{"jsonrpc":"2.0","id":"x","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","ifBusy":"steer","input":[]}}`,
		"turn/start ifBusy replace":      `{"jsonrpc":"2.0","id":"x","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","ifBusy":"replace","input":[]}}`,
		"turn/start with userShell":      `{"jsonrpc":"2.0","id":"x","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","input":[],"userShell":{"command":"rm"}}}`,
		"turn/start with workspaceRoots": `{"jsonrpc":"2.0","id":"x","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","input":[],"workspaceRoots":[` + strconv.Quote(outside) + `]}}`,
		"turn/start on another session":  `{"jsonrpc":"2.0","id":"x","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"01a1dc92-aaaa-7450-a3d3-88e5dcc52575","input":[]}}`,
		"a localPersistent choice":       mspDecide("c-always"),
		"a session-scoped choice":        mspDecide("c-session"),
		"a choice never offered":         mspDecide("c-made-up"),
		"a decide with feedback":         strings.Replace(mspDecide("c-once"), `"requirementId"`, `"feedback":"x","requirementId"`, 1),
		"a decide for another approval":  strings.Replace(mspDecide("c-once"), `"apr-1"`, `"apr-2"`, 1),
		"session/fork":                   `{"jsonrpc":"2.0","id":"x","method":"session/fork","params":{"sessionId":"` + mspFenceSession + `"}}`,
		"session/list":                   `{"jsonrpc":"2.0","id":"x","method":"session/list","params":{}}`,
		"session/userShell":              `{"jsonrpc":"2.0","id":"x","method":"session/userShell","params":{"sessionId":"` + mspFenceSession + `"}}`,
		"plugin/install":                 `{"jsonrpc":"2.0","id":"x","method":"plugin/install","params":{}}`,
		"userInput/answer":               `{"jsonrpc":"2.0","id":"x","method":"userInput/answer","params":{}}`,
		"another notification":           `{"jsonrpc":"2.0","method":"session/cancel","params":{}}`,
		"initialized with params":        `{"jsonrpc":"2.0","method":"initialized","params":{"x":1}}`,
		"a second, other initialize":     `{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"other","version":"1"}}}`,
		"a second conversation":          strings.Replace(mspStartFrame(root, ""), mspFenceCmd1, mspFenceCmd2, 1),
		"a resume into this host":        `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","excludeItems":true}}`,
		"a receipt with a body":          `{"jsonrpc":"2.0","id":7,"result":{"decision":"approved"}}`,
		"an error answer to a request":   `{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"no"}}`,
		"a receipt with extra fields":    `{"jsonrpc":"2.0","id":7,"result":{},"extra":1}`,
		"a receipt to no request":        `{"jsonrpc":"2.0","id":8,"result":{}}`,
		"a receipt to id \"7\"":          `{"jsonrpc":"2.0","id":"7","result":{}}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			if err := mspSend(f, frame, root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
				t.Fatalf("refusal = %v", err)
			}
		})
	}
}

// What a session/start may never carry, and what it must.
func TestTheMSPFenceHoldsTheSessionStart(t *testing.T) {
	_, root := newTestMSPFence(t)
	outside := filepath.Dir(root)
	cases := map[string]string{
		"approval mode allowAll":    strings.Replace(mspStartFrame(root, ""), "promptUnmatched", "allowAll", 1),
		"no approval mode":          strings.Replace(mspStartFrame(root, ""), `,"approvalMode":"promptUnmatched"`, "", 1),
		"a session id of its own":   mspStartFrame(root, `,"sessionId":"`+mspFenceSession+`"`),
		"a permission profile":      mspStartFrame(root, `,"permissionProfile":":unrestricted"`),
		"extra workspace roots":     mspStartFrame(root, `,"workspaceRoots":[`+strconv.Quote(root)+`]`),
		"config":                    mspStartFrame(root, `,"config":{"mcpServers":{}}`),
		"a provider":                mspStartFrame(root, `,"providerId":"echo"`),
		"a workspace outside":       mspStartFrame(outside, ""),
		"a relative workspace":      mspStartFrame("ws", ""),
		"no workspace":              `{"jsonrpc":"2.0","id":"relay-msp-start","method":"session/start","params":{"commandId":"` + mspFenceCmd1 + `","approvalMode":"promptUnmatched"}}`,
		"resume with a profile":     `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd1 + `","sessionId":"` + mspFenceSession + `","permissionProfile":"x"}}`,
		"resume with config":        `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd1 + `","sessionId":"` + mspFenceSession + `","config":{}}}`,
		"resume of no session":      `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd1 + `","sessionId":"not-a-uuid"}}`,
		"initialize asking a grant": `{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"aiexpedite","version":"1"},"capabilities":{"userInputDialogs":false,"requestedCapabilities":["userShell"]}}}`,
		"initialize with dialogs":   `{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"aiexpedite","version":"1"},"capabilities":{"userInputDialogs":true}}}`,
		"a turn before any session": `{"jsonrpc":"2.0","id":"relay-turn-t1","method":"turn/start","params":{"commandId":"` + mspFenceCmd2 + `","sessionId":"` + mspFenceSession + `","input":[]}}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			f, _ := newTestMSPFence(t)
			if err := mspSend(f, frame, root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
				t.Fatalf("refusal = %v", err)
			}
		})
	}
}

// terminal-service's own recoveries pass: a setup refused as arriving before
// `initialized` is sent again unchanged, a gone seed is followed by a fresh
// start naming it, and a redelivered SEND of the setup that worked passes.
func TestTheMSPFenceLetsTheHandshakeRecover(t *testing.T) {
	f, root := newTestMSPFence(t)
	resume := `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd1 + `","sessionId":"01a1d81f-e216-7d41-8000-000000000000","excludeItems":true}}`
	if err := mspSend(f, resume, root); err != nil {
		t.Fatal(err)
	}
	// While it is in flight, no other setup goes out.
	if err := mspSend(f, mspStartFrame(root, ""), root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("a second setup in flight: %v", err)
	}
	if err := mspSend(f, resume, root); err != nil {
		t.Fatalf("the identical resume (a redelivery): %v", err)
	}
	_ = f.inboundFrame(`{"jsonrpc":"2.0","id":"relay-msp-resume","error":{"code":-32600,"message":"not initialized","data":{"kind":"notInitialized"}}}`, root)
	if err := mspSend(f, resume, root); err != nil {
		t.Fatalf("the resume sent again after notInitialized: %v", err)
	}
	_ = f.inboundFrame(`{"jsonrpc":"2.0","id":"relay-msp-resume","error":{"code":-32020,"message":"session not found","data":{"kind":"sessionNotFound"}}}`, root)
	fresh := mspStartFrame(root, `,"previousSessionId":"01a1d81f-e216-7d41-8000-000000000000"`)
	if err := mspSend(f, fresh, root); err != nil {
		t.Fatalf("the fresh start after a gone seed: %v", err)
	}
	if err := f.inboundFrame(mspStartAnswer(root), root); err != nil || f.mspHostSession() != mspFenceSession {
		t.Fatalf("established: %v %q", err, f.mspHostSession())
	}
	if err := mspSend(f, fresh, root); err != nil {
		t.Fatalf("a redelivered start: %v", err)
	}
	if err := mspSend(f, resume, root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("a resume after the host has its session: %v", err)
	}
}

// A setup that never reached the host is released: terminal-service may send
// another.
func TestTheMSPFenceReleasesASetupThatWasNeverWritten(t *testing.T) {
	f, root := newTestMSPFence(t)
	c, err := f.check(mspStartFrame(root, ""), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.admit(c); err != nil {
		t.Fatal(err)
	}
	f.release(c)
	other := strings.Replace(mspStartFrame(root, ""), mspFenceCmd1, mspFenceCmd2, 1)
	if err := mspSend(f, other, root); err != nil {
		t.Fatalf("after a failed write: %v", err)
	}
}

// A receipt goes once: the second to the same request is refused, and an
// approval stays decidable until the host resolves it.
func TestTheMSPFenceClosesARequestOnItsReceiptAndAnApprovalOnItsResolution(t *testing.T) {
	f, root := establishMSPSession(t)
	if err := f.inboundFrame(mspApprovalRequest, root); err != nil {
		t.Fatal(err)
	}
	if err := mspSend(f, `{"jsonrpc":"2.0","id":7,"result":{}}`, root); err != nil {
		t.Fatal(err)
	}
	if err := mspSend(f, `{"jsonrpc":"2.0","id":7,"result":{}}`, root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("a second receipt: %v", err)
	}
	if err := mspSend(f, mspDecide("c-once"), root); err != nil {
		t.Fatal(err)
	}
	_ = f.inboundFrame(`{"jsonrpc":"2.0","method":"approval/resolved","params":{"approvalId":"apr-1","resolvedBy":"user","decision":"approved"}}`, root)
	if err := mspSend(f, mspDecide("c-once"), root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("a decide after the approval resolved: %v", err)
	}
	// The next stage of a shell pipeline replaces the request, with its own
	// choices; the end of its turn closes it.
	stage2 := strings.Replace(strings.Replace(mspApprovalRequest, `"id":7`, `"id":8`, 1), `"c-once"`, `"c-once-2"`, 1)
	_ = f.inboundFrame(stage2, root)
	if err := mspSend(f, mspDecide("c-once-2"), root); err != nil {
		t.Fatalf("stage 2: %v", err)
	}
	_ = f.inboundFrame(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"turn-1","terminal":"completed"}}`, root)
	if err := mspSend(f, mspDecide("c-once-2"), root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("a decide after its turn ended: %v", err)
	}
}

// The resumed conversation must work inside the session's folder; one that
// names another folder, or none, is refused (the host is then ended).
func TestTheMSPFenceHoldsAResumedConversationToTheFolder(t *testing.T) {
	resume := `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"` + mspFenceCmd1 + `","sessionId":"` + mspFenceSession + `","excludeItems":true}}`
	answer := func(workspaceRoot string) string {
		return `{"jsonrpc":"2.0","id":"relay-msp-resume","result":{"session":{"sessionId":"` + mspFenceSession + `","workspaceRoot":` + workspaceRoot + `},"history":{"mode":"none"},"pendingRequests":[]}}`
	}
	f, root := newTestMSPFence(t)
	inside := filepath.Join(root, "sub")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, ws := range map[string]string{
		"another folder": strconv.Quote(filepath.Dir(root)),
		"no folder":      "null",
		"a relative one": `"ws"`,
	} {
		t.Run(name, func(t *testing.T) {
			f, root := newTestMSPFence(t)
			if err := mspSend(f, resume, root); err != nil {
				t.Fatal(err)
			}
			if err := f.inboundFrame(answer(ws), root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
				t.Fatalf("refusal = %v", err)
			}
			if f.mspHostSession() != "" {
				t.Fatal("a refused resume became the host's session")
			}
		})
	}
	if err := mspSend(f, resume, root); err != nil {
		t.Fatal(err)
	}
	if err := f.inboundFrame(answer(strconv.Quote(inside)), root); err != nil || f.mspHostSession() != mspFenceSession {
		t.Fatalf("a resume inside the folder: %v", err)
	}
}

// A frame the profile's own ACP fence would pass is not an MSP frame.
func TestTheMSPFenceIsChosenByTheProfile(t *testing.T) {
	if newACPClientFence(openCodeACPProfile).msp != nil || newACPClientFence(museCodeMSPProfile).msp == nil {
		t.Fatal("the fence does not follow the profile's protocol")
	}
	f, root := newTestMSPFence(t)
	if err := mspSend(f, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":`+strconv.Quote(root)+`,"mcpServers":[]}}`, root); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("an ACP session/new reached a Muse host: %v", err)
	}
}

// Send refuses a batch before any fence, as for every driver.
func TestAMuseCodeMSPBatchIsRefused(t *testing.T) {
	m := newACPManager(nil, museCodeMSPProfile)
	m.sessions["msp"] = &ACPSession{ID: "msp", status: "running", fence: newACPClientFence(museCodeMSPProfile)}
	if err := m.Send("msp", `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}]`); err == nil {
		t.Fatal("a batch reached the fence")
	}
}
