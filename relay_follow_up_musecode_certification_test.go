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
// `mspDriver.liveFollowUp` is certified, only at a version this passes on, and
// only when the near-end gate lost nothing (owner decision O5). Run by hand on
// a computer with Muse Code signed in and quota left, never against the
// owner's running agent or its sessions:
//
//	MUSE_LIVE_CERT=1 go test -tags muselive -run TestMuseCodeLiveFollowUpCertification -v -timeout 90m .
//
// One host, one conversation, one verdict per gate (a subtest each):
//
//  1. turnStart: a follow-up sent as a turn starts goes into THAT turn, once;
//     its redelivery (while the turn runs and after it) is answered from the
//     device's ledger and nothing is written again.
//  2. idle: with no turn running the device writes nothing (noActiveTurn).
//  3. midTool: a follow-up sent while a shell command runs goes into that
//     turn, once; one naming another turn is refused (turnMismatch). Also
//     recorded: what Muse answers to the identical steer resent while the
//     turn runs (S11).
//  4. nearEnd: the final-answer window (§4.6, O5). Follow-ups sent while the
//     turn's final answer streams. Any steer Muse ACCEPTED that no turn acted
//     on is LOST, and so is one the device reported written into the
//     running turn that ran as a later turn: either fails the gate.
//  5. stranded: after `turn/foregroundCompleted` the device writes nothing;
//     a steer written there anyway (the device's race) is recorded, and the
//     device's reading of Muse's answer must never be "written" for a steer
//     no turn acted on (S10's real-model twin).
//  6. approvals: in `promptUnmatched` Muse asks before a file write and a
//     command; a follow-up beside an open approval is refused
//     (approvalPending); a once-scoped deny holds. Decides the approval
//     model, never live follow-ups.
//  7. resume: a second host resumes the exact conversation (same session,
//     mode replayed, context kept); a gone one is the typed -32020; the
//     one-shot driver's v4 id is recorded (S18).
//
// The device launches `muse serve` as the person, so this run uses the
// person's own Muse (their sign-in, settings and session store:
// useRealUserProfile), in a fresh empty folder outside every trusted Muse
// project. Every approval is answered with the request's once-scoped deny
// unless a gate says otherwise.
//
// MUSE_LIVE_CERT_TRANSCRIPT names a folder for each host's whole transcript
// (every frame sent and read, the device's acks). MUSE_LIVE_CERT_RACE_N
// (default 10) and MUSE_LIVE_CERT_STRANDED_N (default 3) set the rounds of
// gates 4 and 5; MUSE_LIVE_CERT_EFFORT the turns' reasoningEffort (default
// "low", to spend less quota; "" sends none). Never part of `go test ./...`.

const (
	museCertTurnStartPrompt = "Count slowly from 1 to 40, one number per line. Do not use any tools."
	museCertNearEndPrompt   = "Count from 1 to 25, one number per line. Do not use any tools."
	museCertStrandPrompt    = "Reply with just the word ONE. Do not use any tools."
	museCertRiskyPrompt     = "Create a file named cert.txt containing the word CERT, then run the shell command `git --version`."
	museCertResumePrompt    = "In your very first reply in this conversation you counted from 1 up to some number. Reply with just that number."
)

func museCertToolPrompt() string {
	if runtime.GOOS == "windows" {
		return "Run the shell command `ping -n 9 127.0.0.1` and then tell me how many replies it received."
	}
	return "Run the shell command `sleep 8 && echo done` and then tell me what it printed."
}

func museCertSteer(word string) string {
	return "Also write the word " + word + " on its own line at the very end of your reply."
}

type museCertPolicy int

const (
	museCertDeny museCertPolicy = iota
	museCertApproveOnce
	museCertHold
)

// museCertApprovals answers the host's requests: a receipt at once, then a
// once-scoped decision by policy, and records what was asked and who decided.
type museCertApprovals struct {
	c *museCert

	mu       sync.Mutex
	policy   museCertPolicy
	held     []jsonRPCFrame
	asked    []string // per approval/request: subject kind, stages, judgeEscalated
	resolved []string // per approval/resolved: resolvedBy/decision
	decided  []string // per approval/decide sent: decision/scope
}

func (a *museCertApprovals) setPolicy(p museCertPolicy) {
	a.mu.Lock()
	a.policy = p
	a.mu.Unlock()
}

func (a *museCertApprovals) onFrame(f jsonRPCFrame, _ string) {
	switch {
	case f.isRequest() && (f.Method == mspServerRequestApproval || f.Method == mspServerRequestUserInput):
		receipt := `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{}}`
		if f.Method != mspServerRequestApproval {
			go func() { _ = a.c.client.send(receipt) }()
			return
		}
		var p struct {
			JudgeEscalated bool `json:"judgeEscalated"`
			Subject        struct {
				Kind   string            `json:"kind"`
				Stages []json.RawMessage `json:"stages"`
			} `json:"subject"`
		}
		_ = json.Unmarshal(f.Params, &p)
		a.mu.Lock()
		a.asked = append(a.asked, fmt.Sprintf("%s(stages=%d,judgeEscalated=%v)", p.Subject.Kind, len(p.Subject.Stages), p.JudgeEscalated))
		policy := a.policy
		if policy == museCertHold {
			a.held = append(a.held, f)
		}
		a.mu.Unlock()
		go func() {
			_ = a.c.client.send(receipt)
			switch policy {
			case museCertDeny:
				a.decide(f, mspApprovalDenied)
			case museCertApproveOnce:
				a.decide(f, mspApprovalApproved)
			}
		}()
	case f.Method == mspNotifyApprovalResolved:
		a.mu.Lock()
		a.resolved = append(a.resolved, paramString(f, "resolvedBy")+"/"+paramString(f, "decision"))
		a.mu.Unlock()
	}
}

// decide sends approval/decide with the request's once-scoped choice for
// decision (a deny when no once-scoped approve is offered).
func (a *museCertApprovals) decide(f jsonRPCFrame, decision string) {
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
	for _, want := range []string{decision, mspApprovalDenied} {
		for _, c := range p.AvailableChoices {
			if c.Decision != want || c.Scope != mspApprovalScopeOnce {
				continue
			}
			cmd := newMuseCodeCommandID()
			a.mu.Lock()
			a.decided = append(a.decided, c.Decision+"/"+c.Scope)
			a.mu.Unlock()
			_ = a.c.client.send(`{"jsonrpc":"2.0","id":"aix-msp-decide-` + cmd + `","method":"approval/decide","params":{"commandId":"` + cmd +
				`","sessionId":"` + p.SessionID + `","approvalId":"` + p.ApprovalID + `","choiceId":` + strconv.Quote(c.ChoiceID) +
				`,"requirementId":` + string(p.CurrentRequirementID) + `}}`)
			return
		}
	}
	a.c.note("an approval offered no once-scoped " + decision + " or deny: " + string(f.Params))
}

// releaseHeld denies every held request, and every later one.
func (a *museCertApprovals) releaseHeld() {
	a.mu.Lock()
	held := a.held
	a.held, a.policy = nil, museCertDeny
	a.mu.Unlock()
	for _, f := range held {
		a.decide(f, mspApprovalDenied)
	}
}

func (a *museCertApprovals) snapshot() (asked, resolved, decided []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.asked...), append([]string(nil), a.resolved...), append([]string(nil), a.decided...)
}

// museCert is one `muse serve` host driven through the device's manager.
type museCert struct {
	m         *ACPManager
	sid       string
	cwd       string
	start     time.Time
	client    *liveCertClient
	approvals *museCertApprovals
	ledger    *relayFollowUpLedger
	acks      *ackLog
	effort    string
	// msp is the session the host serves.
	msp string

	mu  sync.Mutex
	out *os.File
	// times[i] is when the client's frame i was read.
	times []time.Time
}

// museCertBinary skips unless the run is opted in and a Muse Code the MSP
// driver runs is installed, gives Muse the person's own profile, and pins
// the binary and the certification under test.
func museCertBinary(t *testing.T) mspDriverBinary {
	t.Helper()
	if os.Getenv("MUSE_LIVE_CERT") != "1" {
		t.Skip("set MUSE_LIVE_CERT=1 to run the live certification")
	}
	useRealUserProfile(t)
	b := readInstalledMSPDriverBinary()
	if !mspDriverEligible(b) {
		t.Skipf("no Muse Code %s or newer with the MSP surface installed here (%+v)", museCodeMSPMinVersion, b)
	}
	pinMSPDriverBinary(t, b)
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID: liveFollowUpCertified[claudeCodeCliAgentID],
		museCodeCliAgentID:   b.version,
	})
	return b
}

// museCertSettings names the Muse settings that decide whether Muse asks.
func museCertSettings() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unknown home"
	}
	path := filepath.Join(home, ".config", "muse", "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return path + " (none: Muse's defaults)"
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(raw, &s) != nil {
		return path + " (unreadable)"
	}
	return path + " {permissions=" + string(s["permissions"]) + " model=" + string(s["model"]) + " reasoning_effort=" + string(s["reasoning_effort"]) + "}"
}

func museCertEffort() string {
	if v, ok := os.LookupEnv("MUSE_LIVE_CERT_EFFORT"); ok {
		return v
	}
	return "low"
}

func museCertRounds(env string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(env)); err == nil && n > 0 {
		return n
	}
	return def
}

// startMuseCert launches `muse serve` through the manager in cwd and runs
// `initialize` / `initialized` as terminal-service's handshake does.
func startMuseCert(t *testing.T, m *ACPManager, sid, cwd string, ledger *relayFollowUpLedger, acks *ackLog) *museCert {
	t.Helper()
	c := &museCert{m: m, sid: sid, cwd: cwd, start: time.Now(), ledger: ledger, acks: acks, effort: museCertEffort()}
	if dir := os.Getenv("MUSE_LIVE_CERT_TRANSCRIPT"); dir != "" {
		f, err := os.Create(filepath.Join(dir, sid+".jsonl"))
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		c.out = f
		t.Cleanup(func() { _ = f.Close() })
	}
	c.client = &liveCertClient{t: t, send: func(frame string) error {
		c.record("out", "", frame)
		return m.Send(sid, frame)
	}}
	c.approvals = &museCertApprovals{c: c}
	c.client.onFrame = c.approvals.onFrame
	publish := func(res resultMsg) {
		c.client.publish(res)
		if res.Type == museCodeMSPProfile.resultTypes.Message {
			c.mu.Lock()
			c.times = append(c.times, time.Now())
			c.mu.Unlock()
		}
		c.record("in", res.Type, res.Output)
	}
	if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", ACPStartOptions{}, publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(sid) })
	if s := m.Get(sid); s == nil || s.liveFollowUps == nil {
		t.Fatal("a host launched from a certified Muse Code has no live follow-up tracker")
	}
	r := c.client.request(`"relay-msp-init"`, mspMethodInitialize, map[string]any{
		"clientInfo": map[string]any{"name": "aiexpedite", "version": "1"}, "capabilities": map[string]any{"userInputDialogs": false}}, 60*time.Second)
	var init struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
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
	t.Logf("host %s: muse %s, sandboxAvailability=%q, schema %s", sid, init.ServerInfo.Version, init.SandboxAvailability, init.Schema.Fingerprint)
	c.client.notify(mspMethodInitialized)
	return c
}

// bind makes the client fail the given (sub)test.
func (c *museCert) bind(t *testing.T) { c.client.t = t }

func (c *museCert) record(dir, typ, frame string) {
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
	b, _ := json.Marshal(line)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.out.Write(append(b, '\n'))
}

func (c *museCert) note(text string) { c.record("note", "", text) }

func (c *museCert) frames() int { return len(c.client.snapshot()) }

func (c *museCert) frame(i int) string { return c.client.snapshot()[i] }

// at is when frame i was read.
func (c *museCert) at(i int) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.times) {
		return time.Time{}
	}
	return c.times[i]
}

func (c *museCert) session() *ACPSession { return c.m.Get(c.sid) }

// newSession starts the conversation as terminal-service does
// (`session/start`, promptUnmatched, no sessionId).
func (c *museCert) newSession(t *testing.T) {
	t.Helper()
	r := c.client.request(`"relay-msp-start"`, mspMethodSessionStart, map[string]any{
		"commandId": newMuseCodeCommandID(), "workspaceRoot": c.cwd, "approvalMode": museMSPChatApprovalMode}, 2*time.Minute)
	var started struct {
		Session struct {
			SessionID    string `json:"sessionId"`
			ModelID      string `json:"modelId"`
			ApprovalMode struct {
				Mode string `json:"mode"`
			} `json:"approvalMode"`
		} `json:"session"`
	}
	if err := json.Unmarshal(r.Result, &started); err != nil || started.Session.SessionID == "" ||
		started.Session.ApprovalMode.Mode != museMSPChatApprovalMode || c.session().liveFollowUps.mspSession() != started.Session.SessionID {
		t.Fatalf("session/start: %s", r.Result)
	}
	c.msp = started.Session.SessionID
	t.Logf("session %s, model %s, approval mode %s", c.msp, started.Session.ModelID, started.Session.ApprovalMode.Mode)
}

// turn sends a `turn/start` as terminal-service does and checks it started
// at once; it returns the turn id and the frame count before it.
func (c *museCert) turn(t *testing.T, text string) (string, int) {
	t.Helper()
	mark := c.frames()
	turn := newMuseCodeCommandID()
	params := map[string]any{"commandId": turn, "sessionId": c.msp, "ifBusy": mspIfBusyQueue,
		"input": []map[string]any{{"type": "text", "text": text}}}
	if c.effort != "" {
		params["reasoningEffort"] = c.effort
	}
	id := strconv.Quote("relay-turn-" + turn)
	c.client.sendRequest(id, mspMethodTurnStart, params)
	i := c.client.waitFor("the turn/start ack", mark, 2*time.Minute, isResponseTo(id))
	f := mustParseMuseFrame(t, c.frame(i))
	var ack struct {
		TurnID      string `json:"turnId"`
		Disposition string `json:"disposition"`
	}
	if len(f.Error) > 0 || json.Unmarshal(f.Result, &ack) != nil || ack.Disposition != mspDispositionStarted || ack.TurnID != turn {
		t.Fatalf("turn/start did not start its turn: %s", c.frame(i))
	}
	return turn, mark
}

func (c *museCert) cmd(t *testing.T, attemptID, text, expectedTurn string) commandMsg {
	t.Helper()
	return liveCertCommand(t, c.ledger, c.sid, attemptID, text, c.msp, expectedTurn)
}

// deliver hands a signed relay_follow_up to the device and returns its ack.
func (c *museCert) deliver(t *testing.T, cmd commandMsg) resultMsg {
	t.Helper()
	seen := len(c.acks.snapshot())
	if !handleSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-cert"}) {
		t.Fatal("the follow-up was handed back")
	}
	ack := lastAck(t, c.acks, seen)
	c.note(fmt.Sprintf("ack %s %s/%s/%s", cmd.FollowUpAttemptID, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn))
	return ack
}

func turnEvent(method, turn string) func(f jsonRPCFrame, _ string) bool {
	return func(f jsonRPCFrame, _ string) bool { return f.Method == method && paramString(f, "turnId") == turn }
}

func (c *museCert) completed(t *testing.T, mark int, turn string, timeout time.Duration) int {
	t.Helper()
	return c.client.waitFor("the turn's completion", mark, timeout, turnEvent(mspNotifyTurnCompleted, turn))
}

// settle waits until the host runs no turn.
func (c *museCert) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if running, _ := c.session().liveFollowUps.mspTurn(); running == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the host still runs a turn after 5 minutes")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// drainStray gives a turn the follow-up may have started after `from` time
// to show, then waits for every such turn to end; it returns their ids.
func (c *museCert) drainStray(t *testing.T, from int) []string {
	t.Helper()
	time.Sleep(4 * time.Second)
	c.settle(t)
	var started []string
	for _, line := range c.client.snapshot()[from:] {
		if f, ok := parseJSONRPCFrame(line); ok && f.Method == mspNotifyTurnStarted {
			started = append(started, paramString(f, "turnId"))
		}
	}
	return started
}

type museCertItem struct {
	ItemID       string `json:"itemId"`
	Kind         string `json:"kind"`
	TurnID       string `json:"turnId"`
	CommandID    string `json:"commandId"`
	Text         string `json:"text"`
	Status       string `json:"status"`
	MessagePhase string `json:"messagePhase"`
}

func museItemOf(f jsonRPCFrame) (museCertItem, bool) {
	if f.Method != "item/started" && f.Method != "item/completed" {
		return museCertItem{}, false
	}
	var p struct {
		Item museCertItem `json:"item"`
	}
	if json.Unmarshal(f.Params, &p) != nil {
		return museCertItem{}, false
	}
	return p.Item, true
}

// completedItems are the items completed in frames [from, to).
func (c *museCert) completedItems(from, to int) []museCertItem {
	frames := c.client.snapshot()
	if to > len(frames) {
		to = len(frames)
	}
	var out []museCertItem
	for _, line := range frames[from:to] {
		if f, ok := parseJSONRPCFrame(line); ok && f.Method == "item/completed" {
			if it, ok := museItemOf(f); ok {
				out = append(out, it)
			}
		}
	}
	return out
}

// agentText is turn's agent messages, joined.
func (c *museCert) agentText(from, to int, turn string) string {
	var parts []string
	for _, it := range c.completedItems(from, to) {
		if it.Kind == "agentMessage" && it.TurnID == turn {
			parts = append(parts, it.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// steered are the userMessage items carrying a follow-up's derived commandId.
func (c *museCert) steered(from, to int, commandID string) []museCertItem {
	var out []museCertItem
	for _, it := range c.completedItems(from, to) {
		if it.Kind == "userMessage" && it.CommandID == commandID {
			out = append(out, it)
		}
	}
	return out
}

// writes counts Muse's answers to the device's request for an attempt: one
// per write.
func (c *museCert) writes(from int, attemptID string) int {
	return c.client.count(from, isResponseTo(strconv.Quote(relayLiveFollowUpRPCID(attemptID))))
}

func (c *museCert) answerTo(from int, id string) string {
	if i := c.client.find(from, isResponseTo(id)); i >= 0 {
		return c.frame(i)
	}
	return ""
}

// itemKindAt is the kind and phase of the item a delta at or after from
// appends to, from its item/started.
func (c *museCert) itemKindAt(from int, itemID string) (kind, phase string) {
	for _, line := range c.client.snapshot()[from:] {
		if f, ok := parseJSONRPCFrame(line); ok && f.Method == "item/started" {
			if it, ok := museItemOf(f); ok && it.ItemID == itemID {
				return it.Kind, it.MessagePhase
			}
		}
	}
	return "", ""
}

// whereWent says where a follow-up's text went: into turn ("turn"), into a
// later turn ("later:<id>"), or nowhere ("none").
func (c *museCert) whereWent(from, to int, turn, commandID, word string) string {
	if strings.Contains(c.agentText(from, to, turn), word) {
		return "turn"
	}
	for _, it := range c.completedItems(from, to) {
		if it.Kind == "agentMessage" && it.TurnID != turn && strings.Contains(it.Text, word) {
			return "later:" + it.TurnID
		}
	}
	for _, it := range c.steered(from, to, commandID) {
		if it.TurnID != turn {
			return "later:" + it.TurnID
		}
	}
	return "none"
}

func mustParseMuseFrame(t *testing.T, line string) jsonRPCFrame {
	t.Helper()
	f, ok := parseJSONRPCFrame(line)
	if !ok {
		t.Fatalf("not a JSON-RPC frame: %s", line)
	}
	return f
}

func museWantAck(t *testing.T, step string, ack resultMsg, outcome, reason, processedIn string) bool {
	t.Helper()
	if ack.FollowUpOutcome != outcome || ack.FollowUpReason != reason || ack.FollowUpProcessedIn != processedIn {
		t.Errorf("%s: ack {%s %s %s}, want {%s %s %s}", step, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn, outcome, reason, processedIn)
		return false
	}
	return true
}

func TestMuseCodeLiveFollowUpCertification(t *testing.T) {
	b := museCertBinary(t)
	t.Logf("certifying Muse Code %s (%s), effort %q, settings: %s", b.version, b.path, museCertEffort(), museCertSettings())

	// The ledger contract: a written attempt is never written again and
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
	c := startMuseCert(t, m, "cert-musecode-1", cwd, ledger, acks)
	c.newSession(t)
	var lastTurn string

	t.Run("1 turnStart", func(t *testing.T) {
		c.bind(t)
		c.settle(t)
		turn, mark := c.turn(t, museCertTurnStartPrompt)
		lastTurn = turn
		startedAt := c.client.waitFor("turn/started", mark, 2*time.Minute, turnEvent(mspNotifyTurnStarted, turn))
		cmd := c.cmd(t, "cert-1", museCertSteer("STEERED"), turn)
		t0 := time.Now()
		ack := c.deliver(t, cmd)
		took := time.Since(t0)
		// Redelivery while the turn runs: the ledger answers.
		again := c.deliver(t, cmd)
		end := c.completed(t, mark, turn, 10*time.Minute)
		// And after it.
		after := c.deliver(t, cmd)
		stray := c.drainStray(t, end)
		derived, _ := mspFollowUpCommandID("cert-1", cmd.Ts)
		t.Logf("turn start: steer sent %d frames after turn/started, answered in %s: %s", c.frames()-startedAt, took, c.answerTo(mark, strconv.Quote(relayLiveFollowUpRPCID("cert-1"))))
		museWantAck(t, "live attempt", ack, followUpOutcomeWritten, "", followUpProcessedInCurrent)
		museWantAck(t, "redelivery during the turn", again, followUpOutcomeWritten, "", followUpProcessedInCurrent)
		museWantAck(t, "redelivery after the turn", after, followUpOutcomeWritten, "", followUpProcessedInCurrent)
		if n := c.writes(mark, "cert-1"); n != 1 {
			t.Errorf("one attempt reached Muse %d times", n)
		}
		um := c.steered(mark, c.frames(), derived)
		if len(um) != 1 || um[0].TurnID != turn {
			t.Errorf("the steer's userMessage (commandId %s): %+v, want once in turn %s", derived, um, turn)
		}
		if where := c.whereWent(mark, c.frames(), turn, derived, "STEERED"); where != "turn" {
			t.Errorf("the steer was not acted on in its turn (%s); reply: %q", where, c.agentText(mark, c.frames(), turn))
		}
		if len(stray) != 0 {
			t.Errorf("turns started after the turn: %v, want none", stray)
		}
	})

	t.Run("2 idle", func(t *testing.T) {
		c.bind(t)
		c.settle(t)
		expected := lastTurn
		if expected == "" {
			expected = newMuseCodeCommandID()
		}
		mark := c.frames()
		ack := c.deliver(t, c.cmd(t, "cert-idle", "Reply with just the word IDLE.", expected))
		time.Sleep(3 * time.Second)
		museWantAck(t, "idle session", ack, followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
		if n := c.writes(mark, "cert-idle"); n != 0 {
			t.Errorf("a refused attempt reached Muse %d times", n)
		}
		if i := c.client.find(mark, func(f jsonRPCFrame, _ string) bool { return f.Method == mspNotifyTurnStarted }); i >= 0 {
			t.Errorf("a turn started on an idle session: %s", c.frame(i))
		}
	})

	t.Run("3 midTool", func(t *testing.T) {
		c.bind(t)
		c.settle(t)
		c.approvals.setPolicy(museCertApproveOnce)
		defer c.approvals.setPolicy(museCertDeny)
		turn, mark := c.turn(t, museCertToolPrompt())
		lastTurn = turn
		var toolItem string
		at := c.client.waitFor("the command's toolCall", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
			if turnEvent(mspNotifyTurnCompleted, turn)(f, "") {
				return true
			}
			it, ok := museItemOf(f)
			if ok && f.Method == "item/started" && it.Kind == "toolCall" && (it.TurnID == turn || it.TurnID == "") {
				toolItem = it.ItemID
				return true
			}
			return false
		})
		if toolItem == "" {
			t.Fatalf("the turn ended before a command ran: %s", c.frame(at))
		}
		// Past its approval (answered approve-once), with the command under way.
		deadline := time.Now().Add(2 * time.Minute)
		for c.session().liveFollowUps.approvalPending() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)
		toolDone := func(f jsonRPCFrame, _ string) bool {
			it, ok := museItemOf(f)
			return ok && f.Method == "item/completed" && it.ItemID == toolItem
		}
		if i := c.client.find(at, toolDone); i >= 0 {
			t.Fatalf("the command finished before the follow-up could be sent: %s", c.frame(i))
		}
		t.Logf("host endpoints while the command runs: %s", museCertEndpoints(c.session()))
		cmd := c.cmd(t, "cert-3", museCertSteer("TOOLSTEER"), turn)
		ack := c.deliver(t, cmd)
		derived, _ := mspFollowUpCommandID("cert-3", cmd.Ts)
		// S11: the identical steer resent raw while the turn runs.
		dup, _ := mspSteerFrame("cert-raw-dup", c.msp, turn, derived, cmd.Input)
		c.record("out", "raw", dup)
		if err := c.m.sendDeviceFrame(c.sid, dup, nil); err != nil {
			t.Fatal(err)
		}
		// Another turn named: refused, nothing written.
		wrong := c.deliver(t, c.cmd(t, "cert-3w", "Reply with just the word WRONG.", newMuseCodeCommandID()))
		end := c.completed(t, mark, turn, 10*time.Minute)
		stray := c.drainStray(t, end)
		cmdDone := c.client.find(at, toolDone)
		um := c.steered(mark, c.frames(), derived)
		t.Logf("mid-tool: steer answered %s", c.answerTo(mark, strconv.Quote(relayLiveFollowUpRPCID("cert-3"))))
		t.Logf("S11: the identical steer resent while the turn ran answered %s; userMessages with its commandId: %d", c.answerTo(mark, `"cert-raw-dup"`), len(um))
		museWantAck(t, "mid-tool attempt", ack, followUpOutcomeWritten, "", followUpProcessedInCurrent)
		museWantAck(t, "wrong turn", wrong, followUpOutcomeNotWritten, followUpReasonTurnMismatch, "")
		if n := c.writes(mark, "cert-3"); n != 1 {
			t.Errorf("one attempt reached Muse %d times", n)
		}
		if n := c.writes(mark, "cert-3w"); n != 0 {
			t.Errorf("a refused attempt reached Muse %d times", n)
		}
		if len(um) == 0 || um[0].TurnID != turn {
			t.Errorf("the steer's userMessage: %+v, want in turn %s", um, turn)
		}
		if cmdDone < 0 {
			t.Errorf("the command never completed")
		}
		if where := c.whereWent(mark, c.frames(), turn, derived, "TOOLSTEER"); where != "turn" {
			t.Errorf("the steer was not acted on in its turn (%s); reply: %q", where, c.agentText(mark, c.frames(), turn))
		}
		if len(stray) != 0 {
			t.Errorf("turns started after the turn: %v, want none", stray)
		}
	})

	t.Run("4 nearEnd", func(t *testing.T) {
		c.bind(t)
		rounds := museCertRounds("MUSE_LIVE_CERT_RACE_N", 10)
		var handled, queued, lost, misreported, unknownHandled, unknownLater int
		// The rounds' offsets after the final answer starts streaming span
		// that stream: round 0 at its start, the last just past its end,
		// scaled to the stream round 0 measured.
		stream := time.Second
		for k := 0; k < rounds; k++ {
			c.settle(t)
			turn, mark := c.turn(t, museCertNearEndPrompt)
			lastTurn = turn
			var answerItem string
			first := c.client.waitFor("the final answer streaming", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
				if f.Method != "item/delta" {
					return false
				}
				id := paramString(f, "itemId")
				if kind, phase := c.itemKindAt(mark, id); kind == "agentMessage" && phase != "commentary" {
					answerItem = id
					return true
				}
				return false
			})
			var offset time.Duration
			if k > 0 && rounds > 1 {
				offset = stream * 11 / 10 * time.Duration(k) / time.Duration(rounds-1)
			}
			time.Sleep(offset)
			attempt := fmt.Sprintf("cert-r%d", k)
			word := fmt.Sprintf("RACE%d", k)
			cmd := c.cmd(t, attempt, museCertSteer(word), turn)
			sentAt := time.Now()
			answerDone := c.client.find(first, func(f jsonRPCFrame, _ string) bool {
				it, ok := museItemOf(f)
				return ok && f.Method == "item/completed" && it.ItemID == answerItem
			}) >= 0
			fgBefore := c.client.find(mark, turnEvent(mspNotifyTurnForegroundCompleted, turn)) >= 0
			ack := c.deliver(t, cmd)
			end := c.completed(t, mark, turn, 5*time.Minute)
			c.drainStray(t, end)
			// The answer's stream, measured on this round.
			doneAt := c.client.find(first, func(f jsonRPCFrame, _ string) bool {
				it, ok := museItemOf(f)
				return ok && f.Method == "item/completed" && it.ItemID == answerItem
			})
			var streamed time.Duration
			if doneAt >= 0 {
				streamed = c.at(doneAt).Sub(c.at(first))
				if k == 0 && streamed > 0 {
					stream = streamed
				}
			}
			fgAt := c.client.find(mark, turnEvent(mspNotifyTurnForegroundCompleted, turn))
			derived, _ := mspFollowUpCommandID(attempt, cmd.Ts)
			answer := c.answerTo(mark, strconv.Quote(relayLiveFollowUpRPCID(attempt)))
			accepted := answer != "" && strings.Contains(answer, `"result"`)
			where := c.whereWent(mark, c.frames(), turn, derived, word)
			var outcome string
			switch {
			case ack.FollowUpOutcome == followUpOutcomeNotWritten:
				outcome = "queued (" + ack.FollowUpReason + ")"
				queued++
				if c.writes(mark, attempt) != 0 {
					t.Errorf("round %d: a refused attempt reached Muse", k)
				}
			case accepted && where == "none":
				outcome = "LOST"
				lost++
			case ack.FollowUpOutcome == followUpOutcomeWritten && where == "turn":
				outcome = "handled in the turn"
				handled++
			case ack.FollowUpOutcome == followUpOutcomeWritten:
				outcome = "MISREPORTED: written/current but " + where
				misreported++
			case where == "turn":
				outcome = "unknown, handled in the turn"
				unknownHandled++
			default:
				outcome = "unknown, " + where
				unknownLater++
			}
			timeline := fmt.Sprintf("answer streamed %s; sent %s after its first delta; answer complete before the send: %v; foreground before the send: %v",
				streamed.Round(time.Millisecond), sentAt.Sub(c.at(first)).Round(time.Millisecond), answerDone, fgBefore)
			if fgAt >= 0 {
				timeline += fmt.Sprintf("; foreground %s and completion %s after the first delta",
					c.at(fgAt).Sub(c.at(first)).Round(time.Millisecond), c.at(end).Sub(c.at(first)).Round(time.Millisecond))
			}
			t.Logf("round %d (+%s): ack %s/%s; Muse answered %s; %s [%s]",
				k, offset.Round(time.Millisecond), ack.FollowUpOutcome, ack.FollowUpReason, answer, outcome, timeline)
		}
		t.Logf("near-end window over %d rounds: handled in the turn %d, queued %d, LOST %d, misreported %d, unknown-but-handled %d, unknown-and-later %d",
			rounds, handled, queued, lost, misreported, unknownHandled, unknownLater)
		if lost > 0 || misreported > 0 {
			t.Errorf("%d steers Muse accepted were never acted on and %d were misreported: do not certify (O5)", lost, misreported)
		}
	})

	t.Run("5 stranded", func(t *testing.T) {
		c.bind(t)
		rounds := museCertRounds("MUSE_LIVE_CERT_STRANDED_N", 3)
		for k := 0; k < rounds; k++ {
			c.settle(t)
			turn, mark := c.turn(t, museCertStrandPrompt)
			lastTurn = turn
			at := c.client.waitFor("the foreground", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
				return turnEvent(mspNotifyTurnForegroundCompleted, turn)(f, "") || turnEvent(mspNotifyTurnCompleted, turn)(f, "")
			})
			fg := turnEvent(mspNotifyTurnForegroundCompleted, turn)(mustParseMuseFrame(t, c.frame(at)), "")
			// Written straight to stdin, as if the device had admitted it just
			// before it read the foreground.
			word := fmt.Sprintf("STRAND%d", k)
			rawCmd := newMuseCodeCommandID()
			rawID := fmt.Sprintf(`"cert-strand-%d"`, k)
			raw, _ := mspSteerFrame(strings.Trim(rawID, `"`), c.msp, turn, rawCmd, museCertSteer(word))
			c.record("out", "raw", raw)
			if err := c.m.sendDeviceFrame(c.sid, raw, nil); err != nil {
				t.Fatal(err)
			}
			// Through the device: refused, nothing written.
			attempt := fmt.Sprintf("cert-s%d", k)
			ack := c.deliver(t, c.cmd(t, attempt, museCertSteer("X"+word), turn))
			end := c.completed(t, mark, turn, 5*time.Minute)
			ansAt := c.client.waitFor("the raw steer's answer", mark, time.Minute, isResponseTo(rawID))
			completedBefore := ansAt > end
			c.drainStray(t, end)
			answer := c.frame(ansAt)
			verdict := classifyMSPSteerAnswer(answer, turn)
			where := c.whereWent(mark, c.frames(), turn, rawCmd, word)
			t.Logf("round %d (after the foreground: %v; Muse answered after turn/completed: %v): Muse answered %s; the device would read %s/%s; the text went: %s",
				k, fg, completedBefore, answer, verdict.State, verdict.Reason, where)
			museWantAck(t, fmt.Sprintf("round %d: after the foreground", k), ack, followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
			if n := c.writes(mark, attempt); n != 0 {
				t.Errorf("round %d: a refused attempt reached Muse %d times", k, n)
			}
			if verdict.State == followUpRowWritten && where != "turn" {
				t.Errorf("round %d: the device would report written a steer that went %s", k, where)
			}
		}
	})

	t.Run("6 approvals", func(t *testing.T) {
		c.bind(t)
		c.settle(t)
		c.approvals.setPolicy(museCertHold)
		defer c.approvals.releaseHeld()
		turn, mark := c.turn(t, museCertRiskyPrompt)
		lastTurn = turn
		at := c.client.waitFor("an approval request", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
			return (f.isRequest() && f.Method == mspServerRequestApproval) || turnEvent(mspNotifyTurnCompleted, turn)(f, "")
		})
		asked := mustParseMuseFrame(t, c.frame(at)).isRequest()
		if asked {
			time.Sleep(300 * time.Millisecond)
			ack := c.deliver(t, c.cmd(t, "cert-6", museCertSteer("HELD"), turn))
			museWantAck(t, "beside an open approval", ack, followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
			if n := c.writes(mark, "cert-6"); n != 0 {
				t.Errorf("an attempt beside an open approval reached Muse %d times", n)
			}
			c.approvals.releaseHeld()
		}
		c.completed(t, mark, turn, 10*time.Minute)
		c.drainStray(t, mark)
		requests, resolved, decided := c.approvals.snapshot()
		_, statErr := os.Stat(filepath.Join(c.cwd, "cert.txt"))
		t.Logf("approvals: asked %v; resolved %v; decisions sent %v; cert.txt written: %v; reply: %q",
			requests, resolved, decided, statErr == nil, c.agentText(mark, c.frames(), turn))
		for _, line := range c.client.snapshot()[mark:] {
			if f, ok := parseJSONRPCFrame(line); ok && f.isResponse() && strings.HasPrefix(strings.Trim(string(f.ID), `"`), "aix-msp-decide-") && len(f.Error) > 0 {
				t.Errorf("approval/decide refused: %s", line)
			}
		}
		for _, d := range decided {
			if !strings.HasSuffix(d, "/"+mspApprovalScopeOnce) {
				t.Errorf("a decision other than once-scoped was sent: %s", d)
			}
		}
		if !asked {
			t.Errorf("Muse wrote a file and ran a command without asking in %s (%s)", museMSPChatApprovalMode, museCertSettings())
		}
		if statErr == nil {
			t.Error("a denied edit was written")
		}
	})

	t.Run("7 resume", func(t *testing.T) {
		c.bind(t)
		c.settle(t)
		if err := m.End(c.sid); err != nil {
			t.Fatal(err)
		}
		c2 := startMuseCert(t, m, "cert-musecode-2", cwd, ledger, acks)
		r := c2.client.request(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
			"commandId": newMuseCodeCommandID(), "sessionId": c.msp, "excludeItems": true}, 2*time.Minute)
		var resumed struct {
			Session struct {
				SessionID    string `json:"sessionId"`
				ApprovalMode struct {
					Mode   string `json:"mode"`
					Source string `json:"source"`
				} `json:"approvalMode"`
			} `json:"session"`
		}
		if err := json.Unmarshal(r.Result, &resumed); err != nil || resumed.Session.SessionID != c.msp ||
			resumed.Session.ApprovalMode.Mode != museMSPChatApprovalMode || c2.session().liveFollowUps.mspSession() != c.msp {
			t.Fatalf("resume: %s", r.Result)
		}
		t.Logf("resumed %s, approval mode %s (%s)", resumed.Session.SessionID, resumed.Session.ApprovalMode.Mode, resumed.Session.ApprovalMode.Source)
		c2.msp = c.msp
		turn, mark := c2.turn(t, museCertResumePrompt)
		c2.completed(t, mark, turn, 5*time.Minute)
		reply := c2.agentText(mark, c2.frames(), turn)
		t.Logf("resumed conversation's reply: %q", reply)
		if !strings.Contains(reply, "40") {
			t.Errorf("the resumed conversation lost its context: %q", reply)
		}
		if err := m.End(c2.sid); err != nil {
			t.Fatal(err)
		}

		// A conversation that is gone is a typed error, never a new session.
		c3 := startMuseCert(t, m, "cert-musecode-3", cwd, ledger, acks)
		mark = c3.frames()
		gone := newMuseCodeCommandID()
		c3.client.sendRequest(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
			"commandId": newMuseCodeCommandID(), "sessionId": gone, "excludeItems": true})
		i := c3.client.waitFor("the gone resume's answer", mark, 2*time.Minute, isResponseTo(`"relay-msp-resume"`))
		var e struct {
			Error *struct {
				Code int64 `json:"code"`
				Data struct {
					Kind string `json:"kind"`
				} `json:"data"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(c3.frame(i)), &e)
		if e.Error == nil || e.Error.Code != mspErrSessionNotFound || e.Error.Data.Kind != "sessionNotFound" {
			t.Errorf("resume of a gone session answered %s, want -32020 sessionNotFound", c3.frame(i))
		}
		_ = m.End(c3.sid)

		// S18, recorded: the one-shot driver's v4 conversation id.
		if v4 := museCertOneShotSession(t, cwd); v4 != "" {
			c4 := startMuseCert(t, m, "cert-musecode-4", cwd, ledger, acks)
			mark := c4.frames()
			c4.client.sendRequest(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
				"commandId": newMuseCodeCommandID(), "sessionId": v4, "excludeItems": true})
			i := c4.client.waitFor("the v4 resume's answer", mark, 2*time.Minute, isResponseTo(`"relay-msp-resume"`))
			t.Logf("S18: session/resume of the one-shot driver's v4 id %s answered %s", v4, c4.frame(i))
			_ = m.End(c4.sid)
		}
	})
	t.Logf("Muse Code %s: read the transcript, then certify %s (device liveFollowUpCertified and shared-constants mspDriver.liveFollowUp) only if every gate passed", b.version, b.version)
}

// TestMuseCodeMSPHandshakeSmoke checks the harness with no model call: a
// host starts and opens a session in promptUnmatched; a second host's resume
// of that turnless session is recorded (Muse 1.4.20 persists a session only
// at its first turn, so it answers -32020); a third answers a gone session
// -32020.
func TestMuseCodeMSPHandshakeSmoke(t *testing.T) {
	b := museCertBinary(t)
	t.Logf("Muse Code %s (%s), settings: %s", b.version, b.path, museCertSettings())
	ledger, _, acks := installLiveCertLedger(t)
	m := newACPManager(nil, museCodeMSPProfile)
	prev := globalMuseCodeMSPManager
	globalMuseCodeMSPManager = m
	t.Cleanup(func() { globalMuseCodeMSPManager = prev })
	cwd := t.TempDir()
	c := startMuseCert(t, m, "smoke-musecode-1", cwd, ledger, acks)
	c.newSession(t)
	if err := m.End(c.sid); err != nil {
		t.Fatal(err)
	}
	c2 := startMuseCert(t, m, "smoke-musecode-2", cwd, ledger, acks)
	mark := c2.frames()
	c2.client.sendRequest(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
		"commandId": newMuseCodeCommandID(), "sessionId": c.msp, "excludeItems": true})
	i := c2.client.waitFor("the turnless resume's answer", mark, 2*time.Minute, isResponseTo(`"relay-msp-resume"`))
	t.Logf("resume of the turnless session answered %s; the device follows %q", c2.frame(i), c2.session().liveFollowUps.mspSession())
	_ = m.End(c2.sid)
	c3 := startMuseCert(t, m, "smoke-musecode-3", cwd, ledger, acks)
	mark = c3.frames()
	c3.client.sendRequest(`"relay-msp-resume"`, mspMethodSessionResume, map[string]any{
		"commandId": newMuseCodeCommandID(), "sessionId": newMuseCodeCommandID(), "excludeItems": true})
	i = c3.client.waitFor("the gone resume's answer", mark, 2*time.Minute, isResponseTo(`"relay-msp-resume"`))
	t.Logf("gone resume answered %s", c3.frame(i))
	if !strings.Contains(c3.frame(i), "-32020") {
		t.Errorf("a gone session was not -32020")
	}
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
				"\"procs: \" + (($all | Where-Object { $ids -contains $_.ProcessId } | ForEach-Object { $_.Name }) -join ','); "+
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
