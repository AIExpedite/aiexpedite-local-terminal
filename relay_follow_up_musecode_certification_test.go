//go:build muselive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Certification run for Muse Code on its MSP driver (L5 spec §4.8) with a
// real `muse serve` (Muse Code 1.4.18 or newer). Muse Code is added to
// liveFollowUpCertified, and the shared-constants row's
// `mspDriver.liveFollowUp` is certified, only after this passes on the version
// the device ships against AND step 7 lost nothing (owner decision O5). Run by
// hand on a computer with Muse Code signed in and quota left, never against
// the owner's running agent or its sessions:
//
//	MUSE_LIVE_CERT=1 go test -tags muselive -run TestMuseCodeLiveFollowUpCertification -v -timeout 90m .
//
// MUSE_LIVE_CERT_RACE_N sets how many final-answer steers step 7 times
// (default 10). It is never part of `go test ./...`.
//
// The run starts its OWN `muse serve` through the device's manager — the
// device's binary, argv and environment — in a fresh empty folder, drives the
// handshake terminal-service sends, and answers every approval with the
// request's once-scoped deny unless a step says otherwise.

const (
	museCertCountPrompt = "Run the shell command `echo cert-start`, then count slowly from 1 to 40, one number per line."
	museCertSteerText   = "When you reach 40, also write the word BANANA on its own line."
	museCertShortPrompt = "Count from 1 to 15, one number per line. Do not use any tools."
	museCertRiskyPrompt = "Create a file named cert.txt containing the word CERT, then run the shell command `git --version`."
)

// museCertApprovals answers the host's requests: a receipt at once, then the
// request's once-scoped deny (unless held), and records what was offered and
// who resolved each approval.
type museCertApprovals struct {
	t      *testing.T
	client *liveCertClient

	mu         sync.Mutex
	hold       bool
	held       []jsonRPCFrame
	requests   int
	resolvedBy []string
	offered    []string
}

func (a *museCertApprovals) onFrame(f jsonRPCFrame, _ string) {
	switch {
	case f.isRequest() && (f.Method == mspServerRequestApproval || f.Method == mspServerRequestUserInput):
		go func() { _ = a.client.send(`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{}}`) }()
		if f.Method != mspServerRequestApproval {
			return
		}
		a.mu.Lock()
		a.requests++
		a.offered = append(a.offered, string(f.Params))
		hold := a.hold
		if hold {
			a.held = append(a.held, f)
		}
		a.mu.Unlock()
		if !hold {
			go a.deny(f)
		}
	case f.Method == mspNotifyApprovalResolved:
		a.mu.Lock()
		a.resolvedBy = append(a.resolvedBy, paramString(f, "resolvedBy"))
		a.mu.Unlock()
	}
}

// deny sends approval/decide with the request's once-scoped deny choice.
func (a *museCertApprovals) deny(f jsonRPCFrame) {
	var p struct {
		ApprovalID           string          `json:"approvalId"`
		SessionID            string          `json:"sessionId"`
		CurrentRequirementID json.RawMessage `json:"currentRequirementId"`
		AvailableChoices     []struct {
			ChoiceID string `json:"choiceId"`
			Decision string `json:"decision"`
			Scope    string `json:"scope"`
		} `json:"availableChoices"`
	}
	if json.Unmarshal(f.Params, &p) != nil {
		return
	}
	for _, c := range p.AvailableChoices {
		if c.Decision == mspApprovalDenied && c.Scope == mspApprovalScopeOnce {
			cmd := newMuseCodeCommandID()
			_ = a.client.send(`{"jsonrpc":"2.0","id":"aix-msp-decide-` + cmd + `","method":"approval/decide","params":{"commandId":"` + cmd +
				`","sessionId":"` + p.SessionID + `","approvalId":"` + p.ApprovalID + `","choiceId":` + strconv.Quote(c.ChoiceID) +
				`,"requirementId":` + string(p.CurrentRequirementID) + `}}`)
			return
		}
	}
	a.t.Errorf("an approval offered no once-scoped deny: %s", f.Params)
}

func (a *museCertApprovals) releaseHeld() {
	a.mu.Lock()
	held := a.held
	a.held, a.hold = nil, false
	a.mu.Unlock()
	for _, f := range held {
		a.deny(f)
	}
}

func TestMuseCodeLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("MUSE_LIVE_CERT") != "1" {
		t.Skip("set MUSE_LIVE_CERT=1 to run the live certification")
	}
	b := readInstalledMSPDriverBinary()
	if !mspDriverEligible(b) {
		t.Skipf("no Muse Code %s or newer with the MSP surface installed here (%+v)", museCodeMSPMinVersion, b)
	}
	t.Logf("certifying Muse Code %s (%s)", b.version, b.path)
	pinMSPDriverBinary(t, b)
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID: liveFollowUpCertified[claudeCodeCliAgentID],
		museCodeCliAgentID:   b.version,
	})

	// 1. The ledger contract: a written attempt is never written again and
	// survives a restart as written.
	{
		l, path := newTestFollowUpLedger(t)
		row, write := l.Begin("cert-m0", "m-cert", "s-cert", okCheck(time.Now()))
		if !write {
			t.Fatalf("certification attempt was fenced: %+v", row)
		}
		l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
		reloaded := reopenFollowUpLedger(t, l, path)
		if got := reloaded.Query("cert-m0", reloaded.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowWritten {
			t.Fatalf("written attempt not durable: %+v", got)
		}
	}
	ledger, _, acks := installLiveCertLedger(t)

	m := newACPManager(nil, museCodeMSPProfile)
	prev := globalMuseCodeMSPManager
	globalMuseCodeMSPManager = m
	t.Cleanup(func() { globalMuseCodeMSPManager = prev })

	cwd := t.TempDir()
	startHost := func(sid string) (*liveCertClient, *museCertApprovals, *ACPSession) {
		client := &liveCertClient{t: t, send: func(frame string) error { return m.Send(sid, frame) }}
		approvals := &museCertApprovals{t: t, client: client}
		client.onFrame = approvals.onFrame
		if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", ACPStartOptions{}, client.publish); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = m.End(sid) })
		s := m.Get(sid)
		if s.liveFollowUps == nil {
			t.Fatalf("a host launched from Muse Code %s has no live follow-up tracker", b.version)
		}
		r := client.request(`"relay-msp-init"`, mspMethodInitialize, map[string]any{
			"clientInfo": map[string]any{"name": "aiexpedite", "version": "1"}, "capabilities": map[string]any{"userInputDialogs": false}}, 60*time.Second)
		var init struct {
			SessionDurability   string `json:"sessionDurability"`
			SandboxAvailability string `json:"sandboxAvailability"`
			Schema              struct {
				Fingerprint string `json:"fingerprint"`
			} `json:"schema"`
		}
		_ = json.Unmarshal(r.Result, &init)
		if init.SessionDurability != "durable" {
			t.Fatalf("the chat host is %q, not durable", init.SessionDurability)
		}
		t.Logf("host %s: sandboxAvailability=%q schema=%s", sid, init.SandboxAvailability, init.Schema.Fingerprint)
		client.notify(mspMethodInitialized)
		return client, approvals, s
	}
	deliver := func(cmd commandMsg) resultMsg {
		t.Helper()
		seen := len(acks.snapshot())
		if !handleSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-cert"}) {
			t.Fatal("the follow-up was handed back")
		}
		return lastAck(t, acks, seen)
	}
	want := func(step string, ack resultMsg, outcome, reason, processedIn string) {
		t.Helper()
		if ack.FollowUpOutcome != outcome || ack.FollowUpReason != reason || ack.FollowUpProcessedIn != processedIn {
			t.Fatalf("%s: ack {%s %s %s}, want {%s %s %s}", step, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn, outcome, reason, processedIn)
		}
	}
	itemOf := func(f jsonRPCFrame) (kind, turnID, commandID, text string) {
		var p struct {
			Item struct {
				Kind      string `json:"kind"`
				TurnID    string `json:"turnId"`
				CommandID string `json:"commandId"`
				Text      string `json:"text"`
			} `json:"item"`
		}
		_ = json.Unmarshal(f.Params, &p)
		return p.Item.Kind, p.Item.TurnID, p.Item.CommandID, p.Item.Text
	}
	completedTurn := func(turn string) func(f jsonRPCFrame, _ string) bool {
		return func(f jsonRPCFrame, _ string) bool {
			return f.Method == mspNotifyTurnCompleted && paramString(f, "turnId") == turn
		}
	}

	// 2. Start: initialize, initialized, session/start in promptUnmatched.
	const sid = "cert-musecode-1"
	client, approvals, session := startHost(sid)
	r := client.request(`"relay-msp-start"`, mspMethodSessionStart, map[string]any{
		"commandId": newMuseCodeCommandID(), "workspaceRoot": cwd, "approvalMode": museMSPChatApprovalMode}, 2*time.Minute)
	var started struct {
		Session struct {
			SessionID    string `json:"sessionId"`
			ApprovalMode struct {
				Mode string `json:"mode"`
			} `json:"approvalMode"`
		} `json:"session"`
	}
	if err := json.Unmarshal(r.Result, &started); err != nil || started.Session.SessionID == "" ||
		started.Session.ApprovalMode.Mode != museMSPChatApprovalMode || session.liveFollowUps.mspSession() != started.Session.SessionID {
		t.Fatalf("session/start: %s", r.Result)
	}
	mspSession := started.Session.SessionID
	turnStart := func(text string) string {
		turn := newMuseCodeCommandID()
		client.sendRequest(strconv.Quote("relay-turn-"+turn), mspMethodTurnStart, map[string]any{
			"commandId": turn, "sessionId": mspSession, "ifBusy": mspIfBusyQueue, "input": []map[string]any{{"type": "text", "text": text}}})
		return turn
	}

	// 3. A mid-turn steer: written into the RUNNING turn, its own userMessage
	// in that turn under the derived commandId, the marker in its answer.
	mark := len(client.snapshot())
	turn := turnStart(museCertCountPrompt)
	client.waitFor("the first agent message delta", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
		return f.Method == "item/delta"
	})
	if client.find(mark, func(f jsonRPCFrame, _ string) bool { return f.Method == mspNotifyTurnForegroundCompleted }) >= 0 {
		t.Fatal("the foreground completed before the steer could be sent; use a longer prompt")
	}
	cmd := liveCertCommand(t, ledger, sid, "cert-m1", museCertSteerText, mspSession, turn)
	want("live attempt", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	// 4. Redelivery: answered from the ledger; one steer reached Muse.
	want("redelivery", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	end := client.waitFor("the turn's completion", mark, 10*time.Minute, completedTurn(turn))
	if n := client.count(mark, isResponseTo(`"relay-followup-cert-m1"`)); n != 1 {
		t.Fatalf("one attempt reached Muse %d times", n)
	}
	steerCommandID, _ := mspFollowUpCommandID("cert-m1", cmd.Ts)
	var steered, marker bool
	for _, line := range client.snapshot()[mark:end] {
		f, ok := parseJSONRPCFrame(line)
		if !ok || f.Method != "item/completed" {
			continue
		}
		kind, itemTurn, commandID, text := itemOf(f)
		if kind == "userMessage" && itemTurn == turn && commandID == steerCommandID {
			steered = true
		}
		if kind == "agentMessage" && itemTurn == turn && strings.Contains(text, "BANANA") {
			marker = true
		}
	}
	if !steered || !marker {
		t.Fatalf("the steer was not handled in its turn: userMessage=%v marker=%v", steered, marker)
	}

	// 4b and 5. Steers written by hand while a turn runs: the identical steer
	// again (does MSP answer the recorded receipt without a second
	// userMessage? S11), and one naming another turn.
	mark = len(client.snapshot())
	turn = turnStart(museCertCountPrompt)
	client.waitFor("a delta", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool { return f.Method == "item/delta" })
	rawSteer, _ := mspSteerFrame("cert-raw-1", mspSession, turn, newMuseCodeCommandID(), "Also write RAW1.")
	for i := 0; i < 2; i++ {
		if err := m.sendDeviceFrame(sid, strings.Replace(rawSteer, `"cert-raw-1"`, fmt.Sprintf(`"cert-raw-1-%d"`, i), 1), nil); err != nil {
			t.Fatal(err)
		}
	}
	other, _ := mspSteerFrame("cert-raw-2", mspSession, newMuseCodeCommandID(), newMuseCodeCommandID(), "Also write RAW2.")
	if err := m.sendDeviceFrame(sid, other, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`"cert-raw-1-0"`, `"cert-raw-1-1"`, `"cert-raw-2"`} {
		i := client.waitFor("a raw steer's answer", mark, time.Minute, isResponseTo(id))
		t.Logf("raw steer %s answered %s", id, client.snapshot()[i])
	}
	want("wrong turn", deliver(liveCertCommand(t, ledger, sid, "cert-m5", "x", mspSession, newMuseCodeCommandID())), followUpOutcomeNotWritten, followUpReasonTurnMismatch, "")

	// 6. After the foreground, and after the turn: nothing is written.
	client.waitFor("the foreground", mark, 10*time.Minute, func(f jsonRPCFrame, _ string) bool {
		return f.Method == mspNotifyTurnForegroundCompleted && paramString(f, "turnId") == turn
	})
	want("after the foreground", deliver(liveCertCommand(t, ledger, sid, "cert-m6a", "x", mspSession, turn)), followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	client.waitFor("the turn's completion", mark, 10*time.Minute, completedTurn(turn))
	want("after the turn", deliver(liveCertCommand(t, ledger, sid, "cert-m6b", "x", mspSession, turn)), followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")

	// 7. The final-answer window: a steer during the last model call's
	// streaming. Any steer Muse accepted that its turn never acted on is LOST,
	// and fails the run (O5).
	rounds := 10
	if n, err := strconv.Atoi(os.Getenv("MUSE_LIVE_CERT_RACE_N")); err == nil && n > 0 {
		rounds = n
	}
	var handled, queued, lost int
	for k := 0; k < rounds; k++ {
		mark := len(client.snapshot())
		turn := turnStart(museCertShortPrompt)
		client.waitFor("a delta", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool { return f.Method == "item/delta" })
		time.Sleep(time.Duration(k%10) * 150 * time.Millisecond)
		word := fmt.Sprintf("RACE%d", k)
		ack := deliver(liveCertCommand(t, ledger, sid, fmt.Sprintf("cert-r%d", k), "Also write "+word+".", mspSession, turn))
		end := client.waitFor("the turn's completion", mark, 5*time.Minute, completedTurn(turn))
		if ack.FollowUpOutcome != followUpOutcomeWritten {
			queued++
			continue
		}
		acted := false
		for _, line := range client.snapshot()[mark:end] {
			if f, ok := parseJSONRPCFrame(line); ok && f.Method == "item/completed" {
				if kind, itemTurn, _, text := itemOf(f); kind == "agentMessage" && itemTurn == turn && strings.Contains(text, word) {
					acted = true
				}
			}
		}
		if acted {
			handled++
		} else {
			lost++
		}
	}
	t.Logf("final-answer window over %d rounds: handled in the turn %d, refused (queued for the next turn) %d, LOST %d", rounds, handled, queued, lost)
	if lost > 0 {
		t.Errorf("%d steers Muse accepted were never acted on: do not certify (O5)", lost)
	}

	// 8. Approvals: each risky action asks; a follow-up while one is open is
	// held; the once-scoped deny ends it cleanly; nothing persistent is sent
	// (the device's fence would refuse it anyway).
	approvals.mu.Lock()
	approvals.hold = true
	approvals.mu.Unlock()
	mark = len(client.snapshot())
	turn = turnStart(museCertRiskyPrompt)
	client.waitFor("an approval request", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
		return f.isRequest() && f.Method == mspServerRequestApproval
	})
	time.Sleep(300 * time.Millisecond)
	want("beside an approval", deliver(liveCertCommand(t, ledger, sid, "cert-m8", "x", mspSession, turn)), followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
	// 10. Ports while a sandboxed shell may be running.
	t.Logf("host endpoints during the turn: %s", museCertEndpoints(session))
	approvals.releaseHeld()
	client.waitFor("the risky turn's completion", mark, 10*time.Minute, completedTurn(turn))
	approvals.mu.Lock()
	t.Logf("approval requests %d, resolvedBy %v, permission profile: read the computer's ~/.config/muse/settings.json", approvals.requests, approvals.resolvedBy)
	if approvals.requests == 0 {
		t.Error("an edit and a shell command ran without asking in promptUnmatched (O2: record the person's profile)")
	}
	approvals.mu.Unlock()
	if _, err := os.Stat(filepath.Join(cwd, "cert.txt")); err == nil {
		t.Error("a denied edit was written")
	}

	// 9. Resume: a second host resumes the exact session, mode replayed; then
	// a v4 id the one-shot driver minted (S18), recorded.
	if err := m.End(sid); err != nil {
		t.Fatal(err)
	}
	const sid2 = "cert-musecode-2"
	client2, _, _ := startHost(sid2)
	r = client2.request(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
		"commandId": newMuseCodeCommandID(), "sessionId": mspSession, "excludeItems": true}, 2*time.Minute)
	var resumed struct {
		Session struct {
			SessionID    string `json:"sessionId"`
			ApprovalMode struct {
				Mode   string `json:"mode"`
				Source string `json:"source"`
			} `json:"approvalMode"`
		} `json:"session"`
	}
	if err := json.Unmarshal(r.Result, &resumed); err != nil || resumed.Session.SessionID != mspSession ||
		resumed.Session.ApprovalMode.Mode != museMSPChatApprovalMode {
		t.Fatalf("resume: %s", r.Result)
	}
	t.Logf("resumed %s, approval mode %s (%s)", resumed.Session.SessionID, resumed.Session.ApprovalMode.Mode, resumed.Session.ApprovalMode.Source)
	if v4 := museCertOneShotSession(t, cwd); v4 != "" {
		const sid3 = "cert-musecode-3"
		client3, _, _ := startHost(sid3)
		mark := len(client3.snapshot())
		client3.sendRequest(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
			"commandId": newMuseCodeCommandID(), "sessionId": v4, "excludeItems": true})
		i := client3.waitFor("the v4 resume's answer", mark, 2*time.Minute, isResponseTo(`"relay-msp-resume"`))
		t.Logf("S18: session/resume of the one-shot driver's v4 id %s answered %s", v4, client3.snapshot()[i])
	}
	t.Logf("Muse Code %s: read the transcript, then certify %s (device liveFollowUpCertified and shared-constants mspDriver.liveFollowUp)", b.version, b.version)
}

// museCertOneShotSession runs one `muse exec` turn the way the one-shot
// driver does, under a v4 session id it mints, and returns that id ("" when
// the run fails).
func museCertOneShotSession(t *testing.T, cwd string) string {
	t.Helper()
	id := newRandomUUID()
	cmd := exec.Command(resolveMuseCodeExecutable(), "exec", "--json", "--disable-approval", "--session-id", id, "Reply with the single word OK.")
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("S18 not recorded: muse exec failed: %v (%d bytes)", err, len(out))
		return ""
	}
	return id
}

// museCertEndpoints lists the TCP and UDP endpoints of the host and every
// process under it (P1, R4): a listener is a `Listen` / `LISTEN` row.
func museCertEndpoints(s *ACPSession) string {
	if s == nil || s.Process == nil || s.Process.Process == nil {
		return "(no process)"
	}
	pid := strconv.Itoa(s.Process.Process.Pid)
	var out []byte
	var err error
	if runtime.GOOS == "windows" {
		out, err = exec.Command("powershell", "-NoProfile", "-Command",
			"$ids=@("+pid+"); $all=Get-CimInstance Win32_Process; $grew=$true; "+
				"while($grew){$grew=$false; foreach($p in $all){ if(($ids -contains $p.ParentProcessId) -and -not ($ids -contains $p.ProcessId)){ $ids+=$p.ProcessId; $grew=$true } } }; "+
				"Get-NetTCPConnection -ErrorAction SilentlyContinue | Where-Object { $ids -contains $_.OwningProcess } | ForEach-Object { \"tcp $($_.OwningProcess) $($_.LocalAddress):$($_.LocalPort) -> $($_.RemoteAddress):$($_.RemotePort) $($_.State)\" }; "+
				"Get-NetUDPEndpoint -ErrorAction SilentlyContinue | Where-Object { $ids -contains $_.OwningProcess } | ForEach-Object { \"udp $($_.OwningProcess) $($_.LocalAddress):$($_.LocalPort)\" }").CombinedOutput()
	} else {
		// The host leads its own process group (killProcessTree): -g lists
		// every member's sockets.
		out, err = exec.Command("lsof", "-a", "-g", pid, "-i", "-n", "-P").CombinedOutput()
	}
	if err != nil && len(out) == 0 {
		return "(could not list: " + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}
