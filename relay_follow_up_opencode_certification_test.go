//go:build opencodelive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Certification run for OpenCode on its ACP driver (L3/L4 spec §6.4) with a
// real `opencode acp` (2.2.7 or newer). OpenCode is added to
// liveFollowUpCertified, and the shared-constants row is certified, only after
// this passes on the version the device ships against. Run by hand on a
// computer with a signed-in OpenCode 2.x, never against the owner's running
// agent or its sessions:
//
//	OPENCODE_LIVE_CERT=1 go test -tags opencodelive -run TestOpenCodeLiveFollowUpCertification -v -timeout 60m .
//
// OPENCODE_LIVE_CERT_RACE_N sets how many end-of-turn steers step 3 times
// (default 50). It is never part of `go test ./...`.
//
// The run starts its OWN `opencode acp` through the device's manager — the
// device's argv, environment and ask rules — in a fresh empty folder, and
// answers every permission request reject_once unless a step says otherwise.

const (
	openCodeCertCountPrompt = "Count slowly from 1 to 30, one number per line. Do not use any tools."
	openCodeCertSteerText   = "When you reach 30, also write the word BANANA on its own line."
	openCodeCertShortPrompt = "Count from 1 to 5, one number per line. Do not use any tools."
	openCodeCertFlushText   = "Please act on the message I sent while you were working."
)

func TestOpenCodeLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE_CERT") != "1" {
		t.Skip("set OPENCODE_LIVE_CERT=1 to run the live certification")
	}
	b := readACPDriverBinary(openCodeACPProfile)
	if !acpDriverEligible(openCodeACPProfile, b) {
		t.Skipf("no OpenCode %s or newer installed as its native binary here (%+v)", openCodeACPMinVersion, b)
	}
	t.Logf("certifying opencode %s (%s)", b.version, b.path)
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID: liveFollowUpCertified[claudeCodeCliAgentID],
		opencodeCliAgentID:   b.version,
	})
	ledger, _, acks := installLiveCertLedger(t)

	const sid = "cert-opencode-1"
	m := newACPManager(nil, openCodeACPProfile)
	prev := globalOpenCodeACPManager
	globalOpenCodeACPManager = m
	t.Cleanup(func() { globalOpenCodeACPManager = prev })

	// Every permission request is answered reject_once (L3/L4 §2.4: AI
	// Expedite answers allow_once / reject_once only).
	client := &liveCertClient{t: t, send: func(frame string) error { return m.Send(sid, frame) }}
	client.onFrame = func(f jsonRPCFrame, _ string) {
		if !f.isRequest() || f.Method != acpMethodRequestPermission {
			return
		}
		go func() { _ = client.send(certPermissionAnswer(f, "reject_once")) }()
	}
	// A steer written by hand (steps 2 and 3) goes past the client fence, the
	// way the device's ledger writes one.
	steer := func(frame string) error { return m.sendDeviceFrame(sid, frame, nil) }
	cwd := t.TempDir()
	if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", ACPStartOptions{}, client.publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(sid) })
	session := m.Get(sid)
	if session.liveFollowUps == nil {
		t.Fatalf("a session launched from opencode %s has no live follow-up tracker", b.version)
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
	isChunk := func(f jsonRPCFrame) bool {
		return f.Method == "session/update" && paramString(f, "update", "sessionUpdate") == "agent_message_chunk"
	}
	isPermission := func(f jsonRPCFrame, _ string) bool { return f.isRequest() && f.Method == acpMethodRequestPermission }

	// The handshake terminal-service sends: initialize (steering advertised),
	// a session in the folder.
	r := client.request(`"relay-acp-init"`, acpMethodInitialize, map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
		"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, 60*time.Second)
	if !strings.Contains(string(r.Result), `"steering"`) || !session.liveFollowUps.steeringAdvertised() {
		t.Fatalf("initialize did not advertise steering: %s", r.Result)
	}
	r = client.request(`"relay-acp-new"`, acpMethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": []any{}}, 120*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(r.Result, &created); err != nil || created.SessionID == "" || session.liveFollowUps.acpSession() != created.SessionID {
		t.Fatalf("session/new: %s", r.Result)
	}
	acp := created.SessionID
	prompt := func(id, text string) {
		client.sendRequest(id, acpMethodSessionPrompt, map[string]any{"sessionId": acp, "prompt": []map[string]any{{"type": "text", "text": text}}})
	}

	// 1. A mid-turn steer: injected, in the running turn, its answer before
	// the turn's own, handled once.
	const p1 = `"relay-turn-cert-1"`
	prompt(p1, openCodeCertCountPrompt)
	client.waitFor("the first agent_message_chunk", 0, 120*time.Second, func(f jsonRPCFrame, _ string) bool { return isChunk(f) })
	cmd := liveCertCommand(t, ledger, sid, "cert-o1", openCodeCertSteerText, acp, "")
	want("live attempt", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	want("redelivery", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	answer := client.waitFor("the prompt's answer", 0, 5*time.Minute, isResponseTo(p1))
	if steer := client.find(0, isResponseTo(`"relay-followup-cert-o1"`)); steer < 0 || steer > answer {
		t.Fatalf("the steer's answer (%d) did not come before the turn's (%d)", steer, answer)
	}
	if n := client.count(0, isResponseTo(`"relay-followup-cert-o1"`)); n != 1 {
		t.Fatalf("one attempt reached OpenCode %d times", n)
	}
	marker := 0
	for _, line := range client.snapshot()[:answer] {
		if f, ok := parseJSONRPCFrame(line); ok && isChunk(f) && strings.Contains(line, "BANANA") {
			marker++
		}
	}
	if marker == 0 {
		t.Fatal("the steered marker never streamed inside the turn")
	}

	// 2. Idle: the device refuses before writing; a steer written by hand
	// answers promptRequired and starts no turn.
	want("idle session", deliver(liveCertCommand(t, ledger, sid, "cert-o2", "IDLE", acp, "")), followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	mark := len(client.snapshot())
	frame, _ := openCodeSteerFrame("cert-idle-steer", acp, "Reply with just IDLE.")
	if err := steer(frame); err != nil {
		t.Fatal(err)
	}
	i := client.waitFor("the idle steer's answer", mark, 30*time.Second, isResponseTo(`"cert-idle-steer"`))
	if !strings.Contains(client.snapshot()[i], openCodeSteerPromptRequired) {
		t.Fatalf("idle steer answered %s", client.snapshot()[i])
	}
	time.Sleep(3 * time.Second)
	if client.find(mark, func(f jsonRPCFrame, _ string) bool { return isChunk(f) }) >= 0 {
		t.Fatal("an idle steer started a turn")
	}

	// 3. The race (W-O6): steers written by hand around each short turn's
	// end. Held = injected after the prompt's answer; residual = injected
	// before it but never acted on (read from the transcript).
	rounds := 50
	if n, err := strconv.Atoi(os.Getenv("OPENCODE_LIVE_CERT_RACE_N")); err == nil && n > 0 {
		rounds = n
	}
	var inTurn, held, refused int
	for k := 0; k < rounds; k++ {
		mark := len(client.snapshot())
		pid := fmt.Sprintf(`"relay-turn-cert-r%d"`, k)
		prompt(pid, openCodeCertShortPrompt)
		client.waitFor("a chunk", mark, 2*time.Minute, func(f jsonRPCFrame, _ string) bool { return isChunk(f) })
		time.Sleep(time.Duration(k%10) * 150 * time.Millisecond)
		steerID := fmt.Sprintf("cert-race-%d", k)
		frame, _ := openCodeSteerFrame(steerID, acp, fmt.Sprintf("Also write RACE%d.", k))
		if err := steer(frame); err != nil {
			t.Fatal(err)
		}
		ans := client.waitFor("the steer's answer", mark, 30*time.Second, isResponseTo(strconv.Quote(steerID)))
		turn := client.waitFor("the turn's answer", mark, 2*time.Minute, isResponseTo(pid))
		switch line := client.snapshot()[ans]; {
		case !strings.Contains(line, openCodeSteerInjected):
			refused++
		case ans > turn:
			held++
		default:
			inTurn++
		}
	}
	t.Logf("race over %d rounds: injected in the turn %d, held for the next prompt %d, refused %d", rounds, inTurn, held, refused)

	// 4. A steer beside an open permission is never sent, and 6. a deny ends
	// the turn cleanly.
	mark = len(client.snapshot())
	const p4 = `"relay-turn-cert-perm"`
	prompt(p4, "Run the shell command `echo cert-permission` and tell me its output.")
	client.waitFor("a permission request", mark, 3*time.Minute, isPermission)
	time.Sleep(200 * time.Millisecond)
	// The reply to that request is already queued (reject_once); if it has not
	// been read yet, the device refuses.
	ack := deliver(liveCertCommand(t, ledger, sid, "cert-o4", "beside the approval", acp, ""))
	t.Logf("a steer beside the permission request: %s / %s", ack.FollowUpOutcome, ack.FollowUpReason)
	client.waitFor("the denied turn's answer", mark, 3*time.Minute, isResponseTo(p4))

	// 5. The flush prompt carries a held message in, once.
	if held > 0 {
		mark = len(client.snapshot())
		prompt(`"relay-turn-fu_cert-flush"`, openCodeCertFlushText)
		client.waitFor("the flush answer", mark, 3*time.Minute, isResponseTo(`"relay-turn-fu_cert-flush"`))
		t.Logf("flush answered; read the transcript for each held RACE marker exactly once")
	}

	// 7. Every risky action asks.
	for _, step := range []struct{ name, text string }{
		{"edit", "Create a file named cert.txt containing the word CERT."},
		{"shell", "Run the shell command `git --version`."},
		{"webfetch", "Fetch https://example.com and tell me its title."},
	} {
		mark = len(client.snapshot())
		pid := `"relay-turn-cert-` + step.name + `"`
		prompt(pid, step.text)
		end := client.waitFor(step.name+" answer", mark, 3*time.Minute, isResponseTo(pid))
		if client.find(mark, isPermission) < 0 || client.find(mark, isPermission) > end {
			t.Errorf("%s ran without asking", step.name)
		}
	}
	if _, err := os.Stat(cwd + string(os.PathSeparator) + "cert.txt"); err == nil {
		t.Error("a denied edit was written")
	}
	t.Logf("opencode %s: read the transcript, then certify %s", b.version, b.version)
}
