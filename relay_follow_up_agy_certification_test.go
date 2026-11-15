//go:build agylive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Certification runs for Antigravity on its ACP driver (L3/L4 spec §6.4) with
// a real `agy acp` (1.5.4 or newer). Run by hand on a computer with a
// signed-in agy, never against the owner's running agent or its sessions:
//
//	AGY_LIVE_CERT=1 go test -tags agylive -run TestAntigravity -v -timeout 60m .
//
// Three tests, three questions, three verdicts:
//
//   - TestAntigravityLiveFollowUpCertification: does a message sent while agy
//     works go into the running turn, exactly once, with the outcome the
//     device reports (§6.4 agy-2, agy-3)? Antigravity is added to
//     liveFollowUpCertified, and its shared-constants acpDriver.liveFollowUp
//     is certified, at the version this passes on, and only then.
//   - TestAntigravityACPApprovals: does agy ask before a file write and a
//     non-trivial command (W-A4, agy-1)? It decides Antigravity's approval
//     model on this driver, never its live follow-ups: when agy does not ask,
//     "only exact resume and live follow-ups ship" (§6.4 agy-1).
//   - TestAntigravityACPResumeFacts: what agy answers to a reattach (W-A9,
//     agy-4, agy-5). It decides the resume path, not a follow-up.
//
// The device launches `agy acp` as the person, so these runs use the
// person's own agy: their sign-in and their agy settings (the suite's sandbox
// home is undone for agy, test_environment_test.go useRealUserProfile).
// AGY_LIVE_CERT_DEFAULT_SETTINGS=1 keeps the sandbox home instead, so agy
// reads none of the person's settings (its sign-in stays readable where it
// lives outside the home, such as Windows Credential Manager).
//
// Every run works in a fresh folder outside every agy trustedWorkspaces entry.
// AGY_LIVE_CERT_TRUSTED_DIR names an EMPTY folder inside one, to repeat the
// approvals there. AGY_LIVE_CERT_CONVERSATION names a conversation the one-shot
// `--conversation` created, for agy-4 (else the run makes one with `agy
// --print`). AGY_LIVE_CERT_TRANSCRIPT names a folder for each session's whole
// transcript: every frame sent and read, the device's annotations, its acks.
//
// The launch is the device's own (`agy acp`, a fresh per-launch
// ANTIGRAVITY_SERVER_CSRF_TOKEN). The listeners' token check is NOT tested
// here, because AI Expedite never connects to them: re-run the port check of
// aixwt/rs/_notes/design/AGY_ACP_PORT_PROBE.md on every agy version this is
// certified against (local/documents/security/ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md).

func TestAntigravityLiveFollowUpCertification(t *testing.T) {
	b := agyCertBinary(t)
	t.Logf("certifying agy %s (%s), settings: %s", b.version, b.path, agyCertSettings())
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID:  liveFollowUpCertified[claudeCodeCliAgentID],
		antigravityCliAgentID: b.version,
	})
	ledger, _, acks := installLiveCertLedger(t)
	m := newACPManager(nil, antigravityACPProfile)
	prev := globalAntigravityACPManager
	globalAntigravityACPManager = m
	t.Cleanup(func() { globalAntigravityACPManager = prev })

	c := startAgyCert(t, m, "cert-agy-live", t.TempDir())
	c.newSession()
	if s := m.Get(c.sid); s == nil || s.liveFollowUps == nil {
		t.Fatalf("a session launched from agy %s has no live follow-up tracker", b.version)
	}
	cmd := func(attemptID, text string) commandMsg {
		return liveCertCommand(t, ledger, c.sid, attemptID, text, c.acp, "")
	}

	// 1. A message sent as agy starts on a turn, no tool running: the running
	// prompt answers at once (end_turn), published with the device's
	// acpHandoverTo; the follow-up is taken once, into the same turn, and its
	// own prompt's answer ends that turn.
	c.handoverStep(acks, cmd, "1",
		"Count slowly from 1 to 40, one number per line. Do not use any tools.",
		"STEERED", func(f jsonRPCFrame, line string) bool {
			kind, _ := acpUpdateOf(line)
			return kind == "user_message_chunk"
		})

	// 2. The same while a command runs, after a model step of the turn has
	// finished (W-A5's shape): the early answer then carries that step's
	// usage, and is still the handover.
	tool := "Run the shell command `ping -n 6 127.0.0.1` and then tell me how many replies it received."
	if runtime.GOOS != "windows" {
		tool = "Run the shell command `sleep 6 && echo done` and then tell me what it printed."
	}
	c.handoverStep(acks, cmd, "2", tool, "TOOLSTEER", func(f jsonRPCFrame, line string) bool {
		kind, _ := acpUpdateOf(line)
		return kind == "tool_call"
	})

	// 3. Never into an idle session: with no prompt running the device writes
	// nothing and answers notWritten/noActiveTurn, and the broker sends the
	// message as the next turn instead.
	mark := c.frames()
	ack := c.deliver(acks, cmd("cert-3", "Reply with just the word IDLE."))
	if ack.FollowUpOutcome != followUpOutcomeNotWritten || ack.FollowUpReason != followUpReasonNoActiveTurn {
		t.Errorf("step 3: an attempt on an idle session: %+v", ack)
	}
	time.Sleep(3 * time.Second)
	if n := c.countEcho(mark, "IDLE"); n != 0 {
		t.Errorf("step 3: agy received the refused follow-up (%d echoes)", n)
	}
	if n := c.client.count(mark, isResponseTo(strconv.Quote(relayFollowUpPromptRPCID("cert-3")))); n != 0 {
		t.Errorf("step 3: the refused follow-up's prompt was answered %d times", n)
	}

	// 4. The window at a turn's end (agy-3): short turns, each with a
	// follow-up sent later into it. Whatever the timing, the device's answer
	// must match what agy did:
	//   written/current: the running prompt answered end_turn, published as
	//     handed over to the follow-up's prompt, whose answer ends the turn;
	//   written/next: the running prompt ended for a reason of its own (not
	//     end_turn), and the follow-up ran as the next turn under its own id;
	//   notWritten/noActiveTurn: the turn's answer was read before the write,
	//     and agy never received the message.
	offsets := []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, time.Second,
		1500 * time.Millisecond, 2 * time.Second, 3 * time.Second, 4 * time.Second, 6 * time.Second, 8 * time.Second}
	var current, next, idle int
	for k, d := range offsets {
		n := fmt.Sprintf("r%d", k)
		attemptID := "cert-" + n
		pid := `"relay-turn-cert-` + n + `"`
		fuID := relayFollowUpPromptRPCID(attemptID)
		fu := strconv.Quote(fuID)
		mark := c.prompt(pid, "Reply with just the word ONE.")
		c.client.waitFor("round "+n+": the prompt's echo", mark, 2*time.Minute, func(f jsonRPCFrame, line string) bool {
			kind, _ := acpUpdateOf(line)
			return kind == "user_message_chunk" || isResponseTo(pid)(f, line)
		})
		time.Sleep(d)
		ack := c.deliver(acks, cmd(attemptID, "Now reply with just the word TWO."))
		pAt := c.client.waitFor("round "+n+": the prompt's answer", mark, 3*time.Minute, isResponseTo(pid))
		pOK, pUsage, pStop := agyAnswerOf(c.frame(pAt))
		handedTo := c.handoverAt(pAt)
		outcome := fmt.Sprintf("%s/%s%s", ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn)
		switch {
		case ack.FollowUpOutcome == followUpOutcomeWritten && ack.FollowUpProcessedIn == followUpProcessedInCurrent:
			current++
			if !pOK || pStop != acpStopReasonEndTurn || handedTo != fuID {
				t.Errorf("round %s: written/current, but the running prompt's answer was not a handover to %s (acpHandoverTo %q): %s", n, fuID, handedTo, c.frame(pAt))
			}
			c.awaitFollowUpAnswer(n, mark, fu)
		case ack.FollowUpOutcome == followUpOutcomeWritten && ack.FollowUpProcessedIn == followUpProcessedInNext:
			next++
			if (pOK && pStop == acpStopReasonEndTurn) || handedTo != "" {
				t.Errorf("round %s: written/next, but the running prompt's answer was a handover (acpHandoverTo %q): %s", n, handedTo, c.frame(pAt))
			}
			c.awaitFollowUpAnswer(n, mark, fu)
		case ack.FollowUpOutcome == followUpOutcomeNotWritten && ack.FollowUpReason == followUpReasonNoActiveTurn:
			idle++
			if !pOK || !pUsage {
				t.Errorf("round %s: the turn ended without usage: %s", n, c.frame(pAt))
			}
			time.Sleep(2 * time.Second)
			if c.countEcho(mark, "TWO") != 0 || c.client.count(mark, isResponseTo(fu)) != 0 {
				t.Errorf("round %s: agy received a follow-up the device answered notWritten", n)
			}
		default:
			t.Errorf("round %s (+%s): unexpected answer %s", n, d, outcome)
			time.Sleep(10 * time.Second)
		}
		t.Logf("round %s (+%s after the echo): %s (the running prompt's answer: usage %v)", n, d, outcome, pUsage)
	}
	t.Logf("turn-end window over %d rounds: handed over %d, next turn %d, after the turn %d", len(offsets), current, next, idle)
	t.Logf("agy %s: read the transcript, re-run the port check, then certify %s", b.version, b.version)
}

func TestAntigravityACPApprovals(t *testing.T) {
	b := agyCertBinary(t)
	t.Logf("agy %s, settings: %s", b.version, agyCertSettings())
	m := newACPManager(nil, antigravityACPProfile)
	folders := []string{t.TempDir()}
	if trusted := os.Getenv("AGY_LIVE_CERT_TRUSTED_DIR"); trusted != "" {
		folders = append(folders, trusted)
	}
	for n, cwd := range folders {
		c := startAgyCert(t, m, fmt.Sprintf("cert-agy-ask-%d", n), cwd)
		c.newSession()
		for _, step := range []struct{ name, text string }{
			{"write", "Create a file named cert.txt containing the word CERT."},
			{"command", "Run the shell command `git --version` and tell me its output."},
		} {
			pid := `"relay-turn-ask-` + step.name + `"`
			mark := c.prompt(pid, step.text)
			end := c.client.waitFor(step.name+" answer", mark, 5*time.Minute, isResponseTo(pid))
			at := c.client.find(mark, func(f jsonRPCFrame, _ string) bool {
				return f.isRequest() && f.Method == acpMethodRequestPermission
			})
			if at < 0 || at > end {
				t.Errorf("%s in %s: agy did not ask before it", step.name, cwd)
			} else {
				t.Logf("%s in %s: agy asked (%s)", step.name, cwd, permissionTitleOf(c.frame(at)))
			}
		}
		if _, err := os.Stat(filepath.Join(cwd, "cert.txt")); err == nil {
			t.Errorf("a write the run denied (or was never asked about) landed in %s", cwd)
		}
		_ = m.End(c.sid)
	}
}

func TestAntigravityACPResumeFacts(t *testing.T) {
	b := agyCertBinary(t)
	t.Logf("agy %s, settings: %s", b.version, agyCertSettings())
	m := newACPManager(nil, antigravityACPProfile)

	// agy-5: a conversation agy does not have. terminal-service resumes with
	// session/resume, then session/load, and reads a definitive "gone" answer
	// (shared-resources isDefinitiveSessionLoadGone) as a lost context.
	c := startAgyCert(t, m, "cert-agy-missing", t.TempDir())
	const missing = "00000000-0000-4000-8000-000000000000"
	for _, step := range []struct{ id, method string }{
		{`"relay-acp-resume"`, acpMethodSessionResume},
		{`"relay-acp-load"`, acpMethodSessionLoad},
	} {
		mark := c.frames()
		c.client.sendRequest(step.id, step.method, map[string]any{"sessionId": missing, "cwd": c.cwd, "mcpServers": []any{}})
		at := c.client.waitFor(step.method+" answer", mark, 2*time.Minute, isResponseTo(step.id))
		frame := c.frame(at)
		ok, _, _ := agyAnswerOf(frame)
		switch {
		case ok:
			t.Errorf("%s of a conversation agy does not have answered SUCCESS, so a reattach to a lost conversation is not recognised as gone: %.300s", step.method, frame)
		case !acpSessionGone(frame):
			t.Errorf("%s of a missing conversation: the error is not one the gone patterns read: %s", step.method, frame)
		default:
			t.Logf("%s of a missing conversation reads as gone: %s", step.method, frame)
		}
	}
	_ = m.End(c.sid)

	// agy-4: a conversation the one-shot driver made, resumed on the ACP
	// driver in the same folder.
	dir := t.TempDir()
	conv := os.Getenv("AGY_LIVE_CERT_CONVERSATION")
	if conv == "" {
		conv = oneShotAgyConversation(t, b.path, dir)
	}
	r := startAgyCert(t, m, "cert-agy-resume", dir)
	resume := r.client.request(`"relay-acp-resume"`, acpMethodSessionResume, map[string]any{"sessionId": conv, "cwd": dir, "mcpServers": []any{}}, 2*time.Minute)
	t.Logf("session/resume of the one-shot conversation %s: %.300s", conv, resume.Result)
	r.acp = conv
	const pid = `"relay-turn-resume"`
	mark := r.prompt(pid, "What single word did my previous message ask you to reply with? Reply with just that word.")
	end := r.client.waitFor("the resumed turn's answer", mark, 3*time.Minute, isResponseTo(pid))
	reply := r.replyText(mark, end)
	if !strings.Contains(strings.ToUpper(reply), "PINEAPPLE") {
		t.Errorf("the resumed conversation lost its earlier turn: %q", reply)
	} else {
		t.Logf("the resumed conversation remembers its one-shot turn: %q", reply)
	}
}

/* --------------------------------------------------------------------------
   Driving one `agy acp` session
   -------------------------------------------------------------------------- */

// agyCert is one `agy acp` session driven through the device's own manager.
type agyCert struct {
	t      *testing.T
	m      *ACPManager
	sid    string
	cwd    string
	acp    string
	client *liveCertClient
	start  time.Time

	mu sync.Mutex
	// handovers is the device's acpHandoverTo on a published frame, by the
	// frame's index in client's list.
	handovers map[int]string
	out       *os.File
}

// agyCertBinary skips unless the run is opted in and an agy the driver runs
// is installed, and gives agy the person's own profile unless
// AGY_LIVE_CERT_DEFAULT_SETTINGS=1.
func agyCertBinary(t *testing.T) acpDriverBinary {
	t.Helper()
	if os.Getenv("AGY_LIVE_CERT") != "1" {
		t.Skip("set AGY_LIVE_CERT=1 to run the live certification")
	}
	if os.Getenv("AGY_LIVE_CERT_DEFAULT_SETTINGS") != "1" {
		useRealUserProfile(t)
	}
	b := readACPDriverBinary(antigravityACPProfile)
	if !acpDriverEligible(antigravityACPProfile, b) {
		t.Skipf("no agy %s or newer installed here (%+v)", antigravityACPMinVersion, b)
	}
	return b
}

// agyCertSettings names the agy settings this run reads, for the record:
// only the keys that decide whether agy asks.
func agyCertSettings() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unknown home"
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return path + " (none: agy's defaults)"
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(raw, &s) != nil {
		return path + " (unreadable)"
	}
	keys := []string{"toolExecutionPolicy", "artifactReviewMode", "trustedWorkspaces"}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v, ok := s[k]; ok {
			parts = append(parts, k+"="+string(v))
		}
	}
	return path + " {" + strings.Join(parts, " ") + "}"
}

// startAgyCert launches `agy acp` in cwd and initializes it. Every permission
// agy asks for is denied, so nothing it asks about runs.
func startAgyCert(t *testing.T, m *ACPManager, sid, cwd string) *agyCert {
	t.Helper()
	c := &agyCert{t: t, m: m, sid: sid, cwd: cwd, start: time.Now(), handovers: map[int]string{}}
	if dir := os.Getenv("AGY_LIVE_CERT_TRANSCRIPT"); dir != "" {
		f, err := os.Create(filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+"-"+sid+".jsonl"))
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		c.out = f
		t.Cleanup(func() { _ = f.Close() })
	}
	c.client = &liveCertClient{t: t, send: func(frame string) error {
		c.record("out", "", frame, "")
		return m.Send(sid, frame)
	}}
	c.client.onFrame = func(f jsonRPCFrame, _ string) {
		if f.isRequest() && f.Method == acpMethodRequestPermission {
			go func() { _ = c.client.send(certPermissionAnswer(f, "reject_once")) }()
		}
	}
	publish := func(res resultMsg) {
		c.mu.Lock()
		if res.Type == antigravityACPProfile.resultTypes.Message {
			c.client.publish(res)
			if res.AcpHandoverTo != "" {
				c.handovers[len(c.client.snapshot())-1] = res.AcpHandoverTo
			}
		}
		c.mu.Unlock()
		c.record("in", res.Type, res.Output, res.AcpHandoverTo)
	}
	if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", ACPStartOptions{}, publish); err != nil {
		t.Fatalf("start in %s: %v", cwd, err)
	}
	t.Cleanup(func() { _ = m.End(sid) })
	c.client.request(`"relay-acp-init"`, acpMethodInitialize, map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
		"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, 60*time.Second)
	return c
}

// newSession opens a fresh conversation (`session/new`, as terminal-service's
// handshake does) and checks the device follows the ACP session it is on.
func (c *agyCert) newSession() {
	c.t.Helper()
	r := c.client.request(`"relay-acp-new"`, acpMethodSessionNew, map[string]any{"cwd": c.cwd, "mcpServers": []any{}}, 2*time.Minute)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(r.Result, &created); err != nil || created.SessionID == "" {
		c.t.Fatalf("session/new: %s", r.Result)
	}
	if s := c.m.Get(c.sid); s != nil && s.liveFollowUps != nil && s.liveFollowUps.acpSession() != created.SessionID {
		c.t.Fatalf("the device follows ACP session %q, agy answered %q", s.liveFollowUps.acpSession(), created.SessionID)
	}
	c.acp = created.SessionID
}

// record appends one line to the transcript.
func (c *agyCert) record(dir, typ, frame, handoverTo string) {
	if c.out == nil {
		return
	}
	line := map[string]any{"ms": time.Since(c.start).Milliseconds(), "dir": dir}
	if typ != "" {
		line["type"] = typ
	}
	if json.Valid([]byte(frame)) {
		line["frame"] = json.RawMessage(frame)
	} else if frame != "" {
		line["text"] = frame
	}
	if handoverTo != "" {
		line["acpHandoverTo"] = handoverTo
	}
	b, _ := json.Marshal(line)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.out.Write(append(b, '\n'))
}

func (c *agyCert) frames() int { return len(c.client.snapshot()) }

func (c *agyCert) frame(i int) string { return c.client.snapshot()[i] }

func (c *agyCert) handoverAt(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handovers[i]
}

// prompt sends a `session/prompt` and returns the frame count before it.
func (c *agyCert) prompt(id, text string) int {
	c.t.Helper()
	mark := c.frames()
	c.client.sendRequest(id, acpMethodSessionPrompt, map[string]any{"sessionId": c.acp, "prompt": []map[string]any{{"type": "text", "text": text}}})
	return mark
}

// deliver hands a signed relay_follow_up to the device and returns its ack.
func (c *agyCert) deliver(acks *ackLog, cmd commandMsg) resultMsg {
	c.t.Helper()
	seen := len(acks.snapshot())
	if !handleSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-cert"}) {
		c.t.Fatal("the follow-up was handed back")
	}
	ack := lastAck(c.t, acks, seen)
	c.record("ack", "", fmt.Sprintf("%s %s/%s/%s", cmd.FollowUpAttemptID, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn), "")
	return ack
}

// handoverStep sends task as prompt P, delivers one live follow-up asking for
// marker once ready is seen after P (before P's answer), and checks agy took
// it into P's turn exactly once (§6.4 agy-2).
func (c *agyCert) handoverStep(acks *ackLog, mkCmd func(attemptID, text string) commandMsg, n, task, marker string, ready func(jsonRPCFrame, string) bool) {
	t := c.t
	t.Helper()
	pid := `"relay-turn-cert-` + n + `"`
	mark := c.prompt(pid, task)
	at := c.client.waitFor("step "+n+": the turn under way", mark, 3*time.Minute, func(f jsonRPCFrame, line string) bool {
		return ready(f, line) || isResponseTo(pid)(f, line)
	})
	if f, _ := parseJSONRPCFrame(c.frame(at)); isResponseTo(pid)(f, "") {
		t.Fatalf("step %s: the turn ended before the follow-up could be sent: %s", n, c.frame(at))
	}
	attemptID := "cert-" + n
	followUp := mkCmd(attemptID, "Also end your reply with the word "+marker+".")
	ack := c.deliver(acks, followUp)
	if ack.FollowUpOutcome == followUpOutcomeNotWritten && ack.FollowUpReason == followUpReasonApprovalPending {
		// agy asked about the tool and the fence held the message beside the
		// open permission, as it must. Once the run has denied it, a new
		// attempt goes in.
		t.Logf("step %s: held beside an open permission (approvalPending), as the fence requires; retrying once it is answered", n)
		time.Sleep(2 * time.Second)
		attemptID += "b"
		followUp = mkCmd(attemptID, "Also end your reply with the word "+marker+".")
		ack = c.deliver(acks, followUp)
	}
	if ack.FollowUpOutcome != followUpOutcomeWritten {
		t.Fatalf("step %s: the follow-up into a running turn was not written: %s/%s", n, ack.FollowUpOutcome, ack.FollowUpReason)
	}
	if ack.FollowUpProcessedIn != followUpProcessedInCurrent {
		t.Errorf("step %s: the follow-up into a running turn answered written/%s, want written/%s", n, ack.FollowUpProcessedIn, followUpProcessedInCurrent)
	}
	// Pub/Sub delivers at least once: a redelivery is answered from the
	// ledger and writes nothing.
	if again := c.deliver(acks, followUp); again.FollowUpOutcome != ack.FollowUpOutcome || again.FollowUpProcessedIn != ack.FollowUpProcessedIn {
		t.Errorf("step %s: the redelivered attempt answered %s/%s, the first %s/%s", n, again.FollowUpOutcome, again.FollowUpProcessedIn, ack.FollowUpOutcome, ack.FollowUpProcessedIn)
	}
	fuID := relayFollowUpPromptRPCID(attemptID)
	fu := strconv.Quote(fuID)
	early := c.client.waitFor("step "+n+": the running prompt's answer", mark, time.Minute, isResponseTo(pid))
	earlyOK, earlyUsage, earlyStop := agyAnswerOf(c.frame(early))
	if !earlyOK || earlyStop != acpStopReasonEndTurn {
		t.Errorf("step %s: the running prompt's answer is not a handover (end_turn): %s", n, c.frame(early))
	}
	if got := c.handoverAt(early); got != fuID {
		t.Errorf("step %s: the running prompt's answer was published with acpHandoverTo %q, want %q", n, got, fuID)
	}
	final := c.client.waitFor("step "+n+": the follow-up prompt's answer", mark, 5*time.Minute, isResponseTo(fu))
	if final < early {
		t.Errorf("step %s: the follow-up prompt answered before the prompt it took over", n)
	}
	if ok, usage, stop := agyAnswerOf(c.frame(final)); !ok || !usage || stop != "end_turn" {
		t.Errorf("step %s: the turn's final answer (stopReason %q, usage %v): %s", n, stop, usage, c.frame(final))
	}
	// Anything a duplicate write would have produced arrives in this window.
	time.Sleep(3 * time.Second)
	if got := c.countEcho(mark, marker); got != 1 {
		t.Errorf("step %s: agy received the follow-up %d times, want once", n, got)
	}
	if got := c.client.count(mark, isResponseTo(fu)); got != 1 {
		t.Errorf("step %s: the follow-up prompt was answered %d times, want once", n, got)
	}
	reply := c.replyText(mark, final)
	if !strings.Contains(reply, marker) {
		t.Errorf("step %s: the turn's reply never acted on the follow-up (no %s): %q", n, marker, reply)
	}
	t.Logf("step %s: handed over to %s (the early answer carried usage: %v); the turn ended under it; the reply acted on it once (%d chars)", n, fuID, earlyUsage, len(reply))
}

// awaitFollowUpAnswer waits for a written follow-up's own prompt answer (a
// turn's end, with usage).
func (c *agyCert) awaitFollowUpAnswer(n string, mark int, fu string) {
	c.t.Helper()
	at := c.client.waitFor("round "+n+": the follow-up prompt's answer", mark, 3*time.Minute, isResponseTo(fu))
	if ok, usage, _ := agyAnswerOf(c.frame(at)); !ok || !usage {
		c.t.Errorf("round %s: the follow-up's turn ended without usage: %s", n, c.frame(at))
	}
	if c.countEcho(mark, "TWO") != 1 {
		c.t.Errorf("round %s: agy received the follow-up %d times, want once", n, c.countEcho(mark, "TWO"))
	}
}

// countEcho counts agy's echoes of a prompt (user_message_chunk) containing
// needle, from frame `from` on: agy echoes every prompt it takes (W-A8).
func (c *agyCert) countEcho(from int, needle string) int {
	return c.client.count(from, func(_ jsonRPCFrame, line string) bool {
		kind, text := acpUpdateOf(line)
		return kind == "user_message_chunk" && strings.Contains(text, needle)
	})
}

// replyText joins the agent text of frames [from, to).
func (c *agyCert) replyText(from, to int) string {
	var b strings.Builder
	frames := c.client.snapshot()
	for i := from; i < to && i < len(frames); i++ {
		if kind, text := acpUpdateOf(frames[i]); kind == "agent_message_chunk" {
			b.WriteString(text)
		}
	}
	return b.String()
}

// acpUpdateOf is a `session/update` notification's update kind and its text.
func acpUpdateOf(line string) (kind, text string) {
	var f struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Content       json.RawMessage `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &f) != nil || f.Method != "session/update" {
		return "", ""
	}
	var block struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(f.Params.Update.Content, &block) == nil {
		return f.Params.Update.SessionUpdate, block.Text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(f.Params.Update.Content, &blocks)
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(bl.Text)
	}
	return f.Params.Update.SessionUpdate, b.String()
}

// agyAnswerOf reads a prompt answer: a success, whether it carries `usage`,
// and its stopReason.
func agyAnswerOf(line string) (ok, usage bool, stopReason string) {
	var a struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(line), &a) != nil || len(a.Error) > 0 || len(a.Result) == 0 {
		return false, false, ""
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(a.Result, &r) != nil || r == nil {
		return false, false, ""
	}
	_, usage = r["usage"]
	_ = json.Unmarshal(r["stopReason"], &stopReason)
	return true, usage, stopReason
}

// permissionTitleOf is a permission request's tool title, for the record.
func permissionTitleOf(line string) string {
	var f struct {
		Params struct {
			ToolCall struct {
				Title string `json:"title"`
				Kind  string `json:"kind"`
			} `json:"toolCall"`
		} `json:"params"`
	}
	_ = json.Unmarshal([]byte(line), &f)
	return f.Params.ToolCall.Kind + ": " + f.Params.ToolCall.Title
}

// acpSessionGonePatterns mirrors shared-resources ACP_SESSION_GONE_PATTERNS
// (cliTurnCompletion.util.js), which isDefinitiveSessionLoadGone reads.
var acpSessionGonePatterns = regexp.MustCompile(`\b(unknown session|session not found|no such session|session does not exist|session (has been )?deleted|conversation not found|conversation does not exist)\b`)

func acpSessionGone(line string) bool {
	var f struct {
		Error struct {
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(line), &f) != nil {
		return false
	}
	return acpSessionGonePatterns.MatchString(strings.ToLower(f.Error.Message + " " + string(f.Error.Data)))
}

var agyConversationIDRe = regexp.MustCompile(`"conversation_id"\s*:\s*"([0-9a-fA-F-]{36})"`)

// oneShotAgyConversation runs one turn through the one-shot `agy --print`
// in dir, the way the one-shot driver does, and returns its conversation id.
func oneShotAgyConversation(t *testing.T, agy, dir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, agy, "--print", "Reply with just the word PINEAPPLE.", "--output-format", "stream-json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	m := agyConversationIDRe.FindSubmatch(out)
	if m == nil {
		t.Fatalf("the one-shot agy reported no conversation id (err %v): %.400s", err, out)
	}
	t.Logf("one-shot conversation %s", m[1])
	return string(m[1])
}
