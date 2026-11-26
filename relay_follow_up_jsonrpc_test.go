package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Live follow-ups for Codex (`turn/steer`) and Grok Build (`_x.ai/interject`)
// (relay_follow_up_jsonrpc.go): the device builds the request, writes it once
// under the ledger, and settles the attempt from the CLI's own answer. Frames
// below are the ones the L1/L2 spec's experiments recorded (§1.3).

/* --------------------------------------------------------------------------
   Contract pins and pure helpers
   -------------------------------------------------------------------------- */

func TestLiveFollowUpJSONRPCWireValuesArePinned(t *testing.T) {
	pins := [][2]string{
		{codexCliAgentID, "codex"},
		{grokCliAgentID, "grok"},
		{relayLiveFollowUpRPCIDPrefix, "relay-followup-"},
		{relayLiveFollowUpRPCID("att-1"), "relay-followup-att-1"},
		{codexMethodTurnSteer, "turn/steer"},
		{codexMethodTurnCompleted, "turn/completed"},
		{codexMethodServerRequestResolved, "serverRequest/resolved"},
		{codexSteerErrNoActiveTurn, "no active turn to steer"},
		{codexSteerErrTurnMismatchPrefix, "expected active turn id `"},
		{codexSteerErrNotSteerableReview, "cannot steer a review turn"},
		{codexSteerErrNotSteerableCompact, "cannot steer a compact turn"},
		{grokMethodInterject, "_x.ai/interject"},
		{grokMethodInterjection, "_x.ai/session/interjection"},
		{grokMethodQueueChanged, "_x.ai/queue/changed"},
		{grokMethodSessionNotification, "_x.ai/session_notification"},
		{grokInterjectFallbackPromptPrefix, "interject-fallback-"},
		{grokSessionUpdateTurnCompleted, "turn_completed"},
		{followUpReasonNoActiveTurn, "noActiveTurn"},
		{followUpReasonTurnMismatch, "turnMismatch"},
		{followUpReasonTurnNotSteerable, "turnNotSteerable"},
		{followUpReasonMethodNotSupported, "methodNotSupported"},
		{followUpReasonCliRefused, "cliRefused"},
		{followUpReasonNoAnswer, "noAnswer"},
		{followUpReasonWriteFailed, "writeFailed"},
		{followUpReasonAnswerUnrecognised, "answerUnrecognised"},
		// DEVICE_FOLLOW_UP_UNKNOWN_REASON.UNPLACED (shared-constants 3.23.594).
		{followUpReasonUnplaced, "unplaced"},
	}
	for _, p := range pins {
		if p[0] != p[1] {
			t.Errorf("wire value %q, want %q", p[0], p[1])
		}
	}
	// RELAY_FOLLOW_UP_LIMITS.liveAnswerTimeoutMs, pinned in SC between
	// ackOverdueMs (5 s) and attemptWindowMs less the skew margin (105 s).
	if liveFollowUpAnswerTimeout != 10*time.Second {
		t.Errorf("liveFollowUpAnswerTimeout = %s, want 10s", liveFollowUpAnswerTimeout)
	}
	if liveFollowUpAnswerTimeout >= followUpAttemptWindow-followUpSkewMargin {
		t.Errorf("the answer wait must end while the attempt can still be reconciled")
	}
	if !isRelayLiveFollowUpRPCID("relay-followup-x") || isRelayLiveFollowUpRPCID("relay-followup-") || isRelayLiveFollowUpRPCID("relay-turn-x") {
		t.Errorf("isRelayLiveFollowUpRPCID drifted from shared-constants")
	}
}

// Certified at their run versions: a Codex or Grok session launched at the
// certified version gets a tracker, and one launched below it gets none, so
// an older CLI that never passed the run is never written to live.
func TestCodexAndGrokTrackOnlyFromTheirCertifiedVersion(t *testing.T) {
	for kind, certified := range map[string]string{codexCliAgentID: "0.168.0", grokCliAgentID: "1.0.77"} {
		if liveFollowUpCertified[kind] != certified {
			t.Fatalf("%s is certified at %q, want %s", kind, liveFollowUpCertified[kind], certified)
		}
		if tr := newLiveFollowUpTracker(kind, certified); tr == nil {
			t.Fatalf("a %s session launched at its certified version %s got no tracker", kind, certified)
		}
		if tr := newLiveFollowUpTracker(kind, "0.0.1"); tr != nil {
			t.Fatalf("a %s session launched below its certified version got a tracker", kind)
		}
		if tr := newLiveFollowUpTracker(kind, ""); tr != nil {
			t.Fatalf("a %s session with an unreadable version got a tracker", kind)
		}
	}
}

// liveAgents lists Codex and Grok exactly when their INSTALLED version meets
// the certified minimum: at the run versions all three are advertised, one
// patch below their run versions only Claude Code is.
func TestLiveAgentsListCodexAndGrokOnlyAtTheirCertifiedVersions(t *testing.T) {
	dir := t.TempDir()
	ledger := newRelayFollowUpLedger(func() string { return filepath.Join(dir, relayFollowUpLedgerFileName) })
	t.Cleanup(ledger.release)
	inbox := newRelayTurnInbox(func() string { return filepath.Join(dir, relayTurnInboxFileName) })
	t.Cleanup(inbox.release)
	prev := liveFollowUpInstalledVersion
	t.Cleanup(func() { liveFollowUpInstalledVersion = prev })
	advertised := func(codex, grok string) map[string]bool {
		liveFollowUpInstalledVersion = func(id string) string {
			switch id {
			case claudeCodeCliAgentID:
				return liveFollowUpCertified[claudeCodeCliAgentID]
			case codexCliAgentID:
				return codex
			case grokCliAgentID:
				return grok
			}
			return ""
		}
		caps := newRelayFollowUpsCapabilityFor(ledger, inbox)
		if caps == nil {
			t.Fatal("no follow-up capability")
		}
		got := map[string]bool{}
		for _, id := range caps.LiveAgents {
			got[id] = true
		}
		return got
	}
	if got := advertised("0.168.0", "1.0.77"); len(got) != 3 || !got[claudeCodeCliAgentID] || !got[codexCliAgentID] || !got[grokCliAgentID] {
		t.Fatalf("at the certified versions liveAgents = %v, want claudeCode, codex and grok", got)
	}
	if got := advertised("0.167.9", "1.0.76"); len(got) != 1 || !got[claudeCodeCliAgentID] {
		t.Fatalf("below the certified versions liveAgents = %v, want claudeCode only", got)
	}
}

func TestLiveFollowUpFramesAreSingleLineRequestsTheDeviceBuilds(t *testing.T) {
	codex, err := codexSteerFrame("relay-followup-att-1", "thread-1", "turn-1", "fu-1", "line one\nline two")
	if err != nil {
		t.Fatal(err)
	}
	wantCodex := `{"jsonrpc":"2.0","id":"relay-followup-att-1","method":"turn/steer","params":{"threadId":"thread-1","expectedTurnId":"turn-1","clientUserMessageId":"fu-1","input":[{"type":"text","text":"line one\nline two","text_elements":[]}]}}`
	if codex != wantCodex {
		t.Fatalf("codex frame\n got %s\nwant %s", codex, wantCodex)
	}
	// Any text survives the frame exactly, whatever it contains.
	odd := "a & <b> \"c\" \\ d\r\ne "
	oddFrame, err := codexSteerFrame("relay-followup-att-1", "thread-1", "turn-1", "", odd)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Params codexSteerParams `json:"params"`
	}
	if err := json.Unmarshal([]byte(oddFrame), &decoded); err != nil || len(decoded.Params.Input) != 1 || decoded.Params.Input[0].Text != odd {
		t.Fatalf("the text did not survive the frame: %v %+v", err, decoded)
	}
	if strings.Contains(oddFrame, "clientUserMessageId") {
		t.Fatalf("an empty follow-up id was sent: %s", oddFrame)
	}
	if strings.ContainsAny(oddFrame, "\r\n ") {
		t.Fatalf("a frame spans lines: %q", oddFrame)
	}
	grok, err := grokInterjectFrame("relay-followup-att-1", "acp-1", "line one\nline two", "att-1")
	if err != nil {
		t.Fatal(err)
	}
	wantGrok := `{"jsonrpc":"2.0","id":"relay-followup-att-1","method":"_x.ai/interject","params":{"sessionId":"acp-1","text":"line one\nline two","interjectionId":"att-1"}}`
	if grok != wantGrok {
		t.Fatalf("grok frame\n got %s\nwant %s", grok, wantGrok)
	}
	for _, f := range []string{codex, grok} {
		if strings.ContainsAny(f, "\r\n") {
			t.Fatalf("a frame spans lines: %q", f)
		}
	}
}

// Every row of the spec's §2.3 answer table.
func TestClassifyLiveFollowUpAnswer(t *testing.T) {
	cases := []struct {
		kind, frame, state, reason string
	}{
		// Codex (§1.3 frames).
		{codexCliAgentID, `{"id":"relay-followup-a","result":{"turnId":"01a1d7fe-4eae-7380-9393-d31ad77472b8"}}`, followUpRowWritten, ""},
		{codexCliAgentID, `{"error":{"code":-32600,"message":"no active turn to steer"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonNoActiveTurn},
		{codexCliAgentID, "{\"error\":{\"code\":-32600,\"message\":\"expected active turn id `turn-does-not-exist` but found `01a1d7fe-4eae`\"},\"id\":\"relay-followup-a\"}", followUpRowRefused, followUpReasonTurnMismatch},
		{codexCliAgentID, `{"error":{"code":-32600,"message":"cannot steer a review turn"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonTurnNotSteerable},
		{codexCliAgentID, `{"error":{"code":-32600,"message":"cannot steer a compact turn"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonTurnNotSteerable},
		{codexCliAgentID, `{"error":{"code":-32603,"message":"x","data":{"codexErrorInfo":{"activeTurnNotSteerable":{"turnKind":"compact"}}}},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonTurnNotSteerable},
		{codexCliAgentID, `{"error":{"code":-32601,"message":"Method not found"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonMethodNotSupported},
		{codexCliAgentID, `{"error":{"code":-32600,"message":"input must not be empty"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonCliRefused},
		{codexCliAgentID, `{"error":{"code":-32602,"message":"Invalid params"},"id":"relay-followup-a"}`, followUpRowRefused, followUpReasonCliRefused},
		{codexCliAgentID, `{"error":{"code":-32603,"message":"internal error"},"id":"relay-followup-a"}`, followUpRowUnknown, ""},
		{codexCliAgentID, `{"error":{"message":"no code"},"id":"relay-followup-a"}`, followUpRowUnknown, ""},
		{codexCliAgentID, `{"id":"relay-followup-a","result":{}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{codexCliAgentID, `{"id":"relay-followup-a","result":{"turnId":7}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{codexCliAgentID, `{"id":"relay-followup-a","result":null}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		// Grok (§1.3 frames).
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","result":{"result":{"status":"queued"}}}`, followUpRowWritten, ""},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","result":{"result":{"status":"dropped"}}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","result":{"status":"queued"}}`, followUpRowUnknown, followUpReasonAnswerUnrecognised},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","error":{"code":-32602,"message":"Invalid params","data":"session not found: 00000000-0000-0000-0000-000000000000"}}`, followUpRowRefused, followUpReasonConversationMismatch},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","error":{"code":-32602,"message":"Invalid params","data":"text is required"}}`, followUpRowRefused, followUpReasonCliRefused},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","error":{"code":-32601,"message":"Method not found"}}`, followUpRowRefused, followUpReasonMethodNotSupported},
		// Codex's steer texts mean nothing on Grok's wire.
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","error":{"code":-32600,"message":"no active turn to steer"}}`, followUpRowRefused, followUpReasonCliRefused},
		{grokCliAgentID, `{"jsonrpc":"2.0","id":"relay-followup-a","error":{"code":-32603,"message":"Internal error"}}`, followUpRowUnknown, ""},
	}
	for _, c := range cases {
		got := classifyLiveFollowUpAnswer(c.kind, c.frame)
		if got.State != c.state || got.Reason != c.reason {
			t.Errorf("%s %s\n got %+v, want {%s %s}", c.kind, c.frame, got, c.state, c.reason)
		}
	}
}

/* --------------------------------------------------------------------------
   The trackers
   -------------------------------------------------------------------------- */

func TestCodexTrackerOpenRequestsAndWhatClosesThem(t *testing.T) {
	tr := newLiveFollowUpTrackerFor(codexCliAgentID)
	// Any server request counts, and is closed by the answer written to it.
	tr.inbound(`{"id":41,"method":"execCommandApproval","params":{}}`)
	if !tr.approvalPending() {
		t.Fatal("an open server request is not tracked")
	}
	tr.outbound(`{"id":41,"result":{"decision":"denied"}}`).done(false)
	if !tr.approvalPending() {
		t.Fatal("an answer that never reached the child closed the request")
	}
	tr.outbound(`{"id":"41","result":{"decision":"denied"}}`).done(true)
	if !tr.approvalPending() {
		t.Fatal("the string id \"41\" closed the request numbered 41")
	}
	tr.outbound(`{"id":41,"result":{"decision":"denied"}}`).done(true)
	if tr.approvalPending() {
		t.Fatal("the answer written to the request did not close it")
	}
	// serverRequest/resolved closes one by its requestId.
	tr.inbound(`{"id":"req-7","method":"item/tool/requestUserInput","params":{}}`)
	tr.inbound(`{"method":"serverRequest/resolved","params":{"threadId":"t","requestId":"req-7"}}`)
	if tr.approvalPending() {
		t.Fatal("serverRequest/resolved did not close the request")
	}
	// A turn end closes every request of that turn.
	tr.inbound(`{"id":8,"method":"execCommandApproval","params":{}}`)
	tr.inbound(`{"id":9,"method":"applyPatchApproval","params":{}}`)
	tr.inbound(`{"method":"turn/completed","params":{"turn":{"id":"turn-1"}}}`)
	if tr.approvalPending() {
		t.Fatal("turn/completed did not close the turn's requests")
	}
	// A notification is not a request.
	tr.inbound(`{"method":"item/agentMessage/delta","params":{}}`)
	if tr.approvalPending() {
		t.Fatal("a notification counted as an open request")
	}
	// So does the child exiting.
	tr.inbound(`{"id":10,"method":"execCommandApproval","params":{}}`)
	tr.readerDone(false)
	if tr.approvalPending() {
		t.Fatal("an exited child still has an open request")
	}
}

func TestGrokTrackerFollowsTheSessionThePromptAndItsTurn(t *testing.T) {
	tr := newLiveFollowUpTrackerFor(grokCliAgentID)
	tr.outbound(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`).done(true)
	tr.inbound(`{"jsonrpc":"2.0","id":3,"result":{"sessionId":"acp-1","models":{}}}`)
	if got := tr.acpSession(); got != "acp-1" {
		t.Fatalf("acpSession = %q after session/new", got)
	}
	if tr.grokTurnRunning() {
		t.Fatal("a turn runs with no client prompt")
	}
	tr.outbound(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[]}}`).done(true)
	if tr.grokTurnRunning() {
		t.Fatal("a written prompt runs before Grok says it does")
	}
	// G7: the queue first lists the prompt, then names it running.
	tr.inbound(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"acp-1","entries":[{"id":"p-int","kind":"prompt","text":"count"}]}}`)
	if tr.grokTurnRunning() {
		t.Fatal("a queued prompt reads as running")
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"acp-1","entries":[],"runningPromptId":"p-int","runningText":"count","runningKind":"prompt"}}`)
	if !tr.grokTurnRunning() {
		t.Fatal("the running prompt is not tracked")
	}
	tr.inbound(`{"jsonrpc":"2.0","id":5,"method":"session/request_permission","params":{"sessionId":"acp-1"}}`)
	tr.inbound(`{"jsonrpc":"2.0","id":6,"method":"fs/read_text_file","params":{}}`)
	if !tr.approvalPending() {
		t.Fatal("an open permission request is not tracked")
	}
	tr.outbound(`{"jsonrpc":"2.0","id":5,"result":{"outcome":{"outcome":"selected","optionId":"reject_once"}}}`).done(true)
	if tr.approvalPending() {
		t.Fatal("only session/request_permission counts for Grok, and its answer closes it")
	}
	tr.inbound(`{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{"sessionId":"acp-1","update":{"sessionUpdate":"turn_completed","prompt_id":"p-int","stop_reason":"end_turn"}}}`)
	if tr.grokTurnRunning() {
		t.Fatal("a turn whose turn_completed was read still runs")
	}
	tr.inbound(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{}}`)
	if hold := tr.inbound(`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","_meta":{"promptId":"p-int"}}}`); hold {
		t.Fatal("an answer with no stranded interjection was held")
	}
	if tr.approvalPending() {
		t.Fatal("the prompt's answer did not close its requests")
	}
	// A prompt whose write failed is taken back, so it never binds.
	tr.outbound(`{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{}}`).done(false)
	tr.inbound(`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"entries":[],"runningPromptId":"p-other","runningText":"x"}}`)
	if tr.grokTurnRunning() {
		t.Fatal("a prompt that never reached the child reads as running")
	}
	// session/load names the session from its request, once written.
	tr.outbound(`{"jsonrpc":"2.0","id":9,"method":"session/load","params":{"sessionId":"acp-2","cwd":"/w"}}`).done(true)
	if got := tr.acpSession(); got != "acp-2" {
		t.Fatalf("acpSession = %q after session/load", got)
	}
	// A stray answer to an id nobody sent changes nothing (G14).
	tr.inbound(`{"id":"skills-reload","result":{"result":{"reloaded":1}}}`)
	if tr.approvalPending() || tr.acpSession() != "acp-2" {
		t.Fatal("a stray answer changed the tracker")
	}
}

/* --------------------------------------------------------------------------
   Ledger additions
   -------------------------------------------------------------------------- */

func TestAnAttemptInFlightIsMarkedOnEveryAnswerUntilItSettles(t *testing.T) {
	l, _ := newEstablishedTestFollowUpLedger(t)
	now := time.Now()
	row, write := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), "a1", "m1", "s1", okCheck(now))
	if !write || row.inFlight {
		t.Fatalf("first delivery: write=%v row=%+v", write, row)
	}
	if again, w := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), "a1", "m1", "s1", okCheck(now)); w || !again.inFlight {
		t.Fatalf("a redelivery in flight is not marked: write=%v row=%+v", w, again)
	}
	if q := l.Query("a1", l.LedgerID(), now.UnixMilli()); !q.inFlight {
		t.Fatalf("a query in flight is not marked: %+v", q)
	}
	l.Settle("a1", followUpRowWritten, "", followUpProcessedInCurrent)
	if q := l.Query("a1", l.LedgerID(), now.UnixMilli()); q.inFlight || q.State != followUpRowWritten {
		t.Fatalf("a settled attempt: %+v", q)
	}
	// Every other settling path clears it too.
	for _, settle := range []func(id string){
		func(id string) { l.Finish(id, nil, followUpProcessedInCurrent) },
		func(id string) { l.RefuseAccepted(id, followUpReasonApprovalPending) },
		func(id string) { l.EndInFlight(id) },
	} {
		id := fmt.Sprintf("b-%d", time.Now().UnixNano())
		if _, w := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), id, "m", "s1", okCheck(now)); !w {
			t.Fatal("not accepted")
		}
		settle(id)
		if q := l.Query(id, l.LedgerID(), now.UnixMilli()); q.inFlight {
			t.Fatalf("still in flight after settling: %+v", q)
		}
	}
}

func TestSettleIsDurableAndOnlyLeavesAccepted(t *testing.T) {
	l, path := newEstablishedTestFollowUpLedger(t)
	now := time.Now()
	for _, id := range []string{"w", "r", "u"} {
		if _, w := l.BeginForLedger(l.LedgerID(), now.UnixMilli(), id, "m-"+id, "s1", okCheck(now)); !w {
			t.Fatalf("%s not accepted", id)
		}
	}
	l.Settle("w", followUpRowWritten, "", followUpProcessedInCurrent)
	l.Settle("r", followUpRowRefused, followUpReasonTurnMismatch, "")
	l.Settle("u", followUpRowUnknown, followUpReasonNoAnswer, "")
	// A second settle changes nothing: only an accepted row moves.
	if got := l.Settle("r", followUpRowWritten, "", followUpProcessedInCurrent); got.State != followUpRowRefused {
		t.Fatalf("a refused row was re-settled: %+v", got)
	}
	reloaded := reopenFollowUpLedger(t, l, path)
	id := reloaded.LedgerID()
	if r := reloaded.Query("w", id, now.UnixMilli()); r.State != followUpRowWritten || r.ProcessedIn != followUpProcessedInCurrent {
		t.Fatalf("written row after restart: %+v", r)
	}
	if r := reloaded.Query("r", id, now.UnixMilli()); r.State != followUpRowRefused || r.Reason != followUpReasonTurnMismatch {
		t.Fatalf("refused row after restart: %+v", r)
	}
	if r := reloaded.Query("u", id, now.UnixMilli()); r.State != followUpRowUnknown || r.Reason != followUpReasonNoAnswer {
		t.Fatalf("unknown row after restart: %+v", r)
	}
	// Late evidence lifts unknown to written, and never touches another state.
	if r, ok := reloaded.LateWritten("u", "m-u", "s1", followUpProcessedInCurrent, ""); !ok || r.State != followUpRowWritten || r.Reason != "" {
		t.Fatalf("late success on an unknown row: ok=%v %+v", ok, r)
	}
	if _, ok := reloaded.LateWritten("r", "m-r", "s1", followUpProcessedInCurrent, ""); ok {
		t.Fatal("a late success lifted a refused row")
	}
	// A row the broker confirmed away is still answered written, unstored.
	if _, err := reloaded.ConfirmAcked([]string{"w"}); err != nil {
		t.Fatal(err)
	}
	if r, ok := reloaded.LateWritten("w", "m-w", "s1", followUpProcessedInCurrent, ""); !ok || r.State != followUpRowWritten || r.FollowUpID != "m-w" {
		t.Fatalf("late success for a confirmed row: ok=%v %+v", ok, r)
	}
	again := reopenFollowUpLedger(t, reloaded, path)
	if r := again.Query("u", again.LedgerID(), now.UnixMilli()); r.State != followUpRowWritten {
		t.Fatalf("the late upgrade is not durable: %+v", r)
	}
}

func TestNewFenceReasonsComeAfterTheExistingOnes(t *testing.T) {
	now := time.Now()
	c := okCheck(now)
	c.EmptyText = true
	if got := c.fenceReason(); got != followUpReasonCliRefused {
		t.Fatalf("empty text: %q", got)
	}
	c.LiveKind = false
	if got := c.fenceReason(); got != followUpReasonNotLiveKind {
		t.Fatalf("an uncertified kind must answer notLiveKind first, got %q", got)
	}
	c = okCheck(now)
	c.NoActiveTurn = true
	if got := c.fenceReason(); got != followUpReasonNoActiveTurn {
		t.Fatalf("no active turn: %q", got)
	}
	c.ApprovalPending = true
	if got := c.fenceReason(); got != followUpReasonApprovalPending {
		t.Fatalf("an open approval outranks no active turn, got %q", got)
	}
}

/* --------------------------------------------------------------------------
   End to end: the handler, a manager, and a fake child over pipes
   -------------------------------------------------------------------------- */

// fakeJSONRPCChild stands in for `codex app-server` / `grok agent stdio`: it
// records every frame the device writes and answers with respond.
type fakeJSONRPCChild struct {
	stdinR, stdoutR, stderrR *io.PipeReader
	stdinW, stdoutW, stderrW *io.PipeWriter

	mu      sync.Mutex
	frames  []string
	respond func(f jsonRPCFrame, line string) []string
}

func newFakeJSONRPCChild(t *testing.T) *fakeJSONRPCChild {
	t.Helper()
	c := &fakeJSONRPCChild{}
	c.stdinR, c.stdinW = io.Pipe()
	c.stdoutR, c.stdoutW = io.Pipe()
	c.stderrR, c.stderrW = io.Pipe()
	go func() {
		sc := bufio.NewScanner(c.stdinR)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			c.mu.Lock()
			c.frames = append(c.frames, line)
			respond := c.respond
			c.mu.Unlock()
			if respond == nil {
				continue
			}
			f, _ := parseJSONRPCFrame(line)
			for _, out := range respond(f, line) {
				c.emit(out)
			}
		}
	}()
	return c
}

func (c *fakeJSONRPCChild) setRespond(fn func(f jsonRPCFrame, line string) []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.respond = fn
}

func (c *fakeJSONRPCChild) emit(line string) { _, _ = io.WriteString(c.stdoutW, line+"\n") }

// exit closes the child's output, as a child that exits does.
func (c *fakeJSONRPCChild) exit() {
	_ = c.stdoutW.Close()
	_ = c.stderrW.Close()
}

func (c *fakeJSONRPCChild) framesWithMethod(method string) []jsonRPCFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []jsonRPCFrame
	for _, line := range c.frames {
		if f, ok := parseJSONRPCFrame(line); ok && f.Method == method {
			out = append(out, f)
		}
	}
	return out
}

// streamLog captures the session's stream frames (chunks).
type streamLog struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (s *streamLog) publish(res resultMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, res)
}

func (s *streamLog) snapshot() []resultMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resultMsg(nil), s.frames...)
}

// indexOf is the position of the first message frame containing marker, or -1.
func (s *streamLog) indexOf(marker string) int {
	for i, f := range s.snapshot() {
		if f.Type == "grok_acp_message" && strings.Contains(f.Output, marker) {
			return i
		}
	}
	return -1
}

// hasOutput reports whether any stream frame, of whatever type, contains
// marker.
func (s *streamLog) hasOutput(marker string) bool {
	for _, f := range s.snapshot() {
		if strings.Contains(f.Output, marker) {
			return true
		}
	}
	return false
}

func (s *streamLog) countOf(marker string) int {
	n := 0
	for _, f := range s.snapshot() {
		if f.Type == "grok_acp_message" && strings.Contains(f.Output, marker) {
			n++
		}
	}
	return n
}

// ackLog captures the device's relay_follow_up_ack frames.
type ackLog struct {
	mu   sync.Mutex
	acks []resultMsg
}

func (a *ackLog) snapshot() []resultMsg {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]resultMsg(nil), a.acks...)
}

func captureFollowUpAcks(t *testing.T) *ackLog {
	t.Helper()
	log := &ackLog{}
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.acks = append(log.acks, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })
	return log
}

func waitForLive(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pinLiveFollowUpKinds certifies exactly the kinds given, for one test.
func pinLiveFollowUpKinds(t *testing.T, kinds map[string]string) {
	t.Helper()
	prev := liveFollowUpCertified
	liveFollowUpCertified = kinds
	t.Cleanup(func() { liveFollowUpCertified = prev })
}

func shortenLiveFollowUpAnswerTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := liveFollowUpAnswerTimeout
	liveFollowUpAnswerTimeout = d
	t.Cleanup(func() { liveFollowUpAnswerTimeout = prev })
}

type liveFollowUpHarness struct {
	t      *testing.T
	ledger *relayFollowUpLedger
	child  *fakeJSONRPCChild
	stream *streamLog
	acks   *ackLog
	sid    string
	done   chan struct{}
}

func newLiveFollowUpHarness(t *testing.T, sid string) *liveFollowUpHarness {
	t.Helper()
	resetFencedSessions(t)
	l, _ := newEstablishedTestFollowUpLedger(t)
	prevLedger := globalRelayFollowUpLedger
	globalRelayFollowUpLedger = l
	t.Cleanup(func() { globalRelayFollowUpLedger = prevLedger })
	prevClaude := globalClaudeNativeManager
	globalClaudeNativeManager = NewClaudeNativeManager(nil)
	t.Cleanup(func() { globalClaudeNativeManager = prevClaude })
	return &liveFollowUpHarness{t: t, ledger: l, child: newFakeJSONRPCChild(t), stream: &streamLog{}, acks: captureFollowUpAcks(t), sid: sid}
}

// finish ends the fake child and waits for the reader, before the swapped
// globals are restored.
func (h *liveFollowUpHarness) finish() {
	h.child.exit()
	_ = h.child.stdinW.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("readStream did not finish")
	}
}

func newCodexLiveHarness(t *testing.T, tracked bool) (*liveFollowUpHarness, *CodexAppServerSession) {
	t.Helper()
	return newCodexLiveHarnessAt(t, tracked, "0.168.0")
}

// newCodexLiveHarnessAt starts a Codex session launched from launchedVersion.
// Its launch fields are set in the literal, before its reader runs.
func newCodexLiveHarnessAt(t *testing.T, tracked bool, launchedVersion string) (*liveFollowUpHarness, *CodexAppServerSession) {
	t.Helper()
	pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322", codexCliAgentID: "0.168.0"})
	h := newLiveFollowUpHarness(t, "cx1")
	m := NewCodexAppServerManager(nil)
	prev := globalCodexAppServerManager
	globalCodexAppServerManager = m
	t.Cleanup(func() { globalCodexAppServerManager = prev })
	var tracker *liveFollowUpTracker
	if tracked {
		tracker = newLiveFollowUpTrackerFor(codexCliAgentID)
	}
	s := &CodexAppServerSession{
		ID: h.sid, Stdin: h.child.stdinW, Stdout: h.child.stdoutR, Stderr: h.child.stderrR,
		status: "running", done: make(chan struct{}), processExited: make(chan struct{}), streamDone: make(chan struct{}),
		launchedVersion: launchedVersion, liveFollowUps: tracker,
	}
	m.sessions[h.sid] = s
	h.done = s.streamDone
	go m.readStream(s, h.stream.publish)
	t.Cleanup(h.finish)
	return h, s
}

func (h *liveFollowUpHarness) codexCmd(attemptID, text string) commandMsg {
	now := time.Now()
	return commandMsg{
		ID: relayFollowUpEnvelopePrefix + attemptID, Type: relayFollowUpCommandType, SessionID: h.sid,
		Input: text, Ts: now.UnixMilli(), ConversationID: "thread-1", ExpectedTurnID: "turn-1",
		FollowUpID: "m-" + attemptID, FollowUpAttemptID: attemptID,
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(), FollowUpLedgerID: h.ledger.LedgerID(),
	}
}

func (h *liveFollowUpHarness) handle(cmd commandMsg) {
	h.t.Helper()
	if !handleRelayFollowUpCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1"}) {
		h.t.Fatal("the follow-up was handed back")
	}
}

func (h *liveFollowUpHarness) query(attemptIDs ...string) {
	h.t.Helper()
	input, _ := json.Marshal(relayFollowUpQuery{AttemptIDs: attemptIDs, LedgerID: h.ledger.LedgerID()})
	h.handle(commandMsg{ID: "q-" + attemptIDs[0], Type: relayFollowUpQueryCommandType, SessionID: h.sid, Ts: time.Now().UnixMilli(), Input: string(input)})
}

// answerFollowUpsWith answers each live write the device sends with frame(id).
func answerFollowUpsWith(frame func(id string) string) func(f jsonRPCFrame, line string) []string {
	return func(f jsonRPCFrame, line string) []string {
		if id, ok := f.stringID(); ok && isRelayLiveFollowUpRPCID(id) && f.Method != "" {
			return []string{frame(id)}
		}
		return nil
	}
}

func codexTurnAccepted(id string) string { return `{"id":"` + id + `","result":{"turnId":"turn-1"}}` }

func wantAck(t *testing.T, ack resultMsg, outcome, reason, processedIn string) {
	t.Helper()
	if ack.Type != relayFollowUpAckResultType || ack.FollowUpOutcome != outcome || ack.FollowUpReason != reason || ack.FollowUpProcessedIn != processedIn {
		t.Fatalf("ack = {%s %s %s %s}, want {%s %s %s}", ack.Type, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn, outcome, reason, processedIn)
	}
}

func TestACodexLiveFollowUpWritesOneSteerAndIsWrittenOnItsAnswer(t *testing.T) {
	h, _ := newCodexLiveHarness(t, true)
	h.child.setRespond(answerFollowUpsWith(codexTurnAccepted))
	h.handle(h.codexCmd("att-1", "When you reach 30, also write BANANA."))

	steers := h.child.framesWithMethod(codexMethodTurnSteer)
	if len(steers) != 1 {
		t.Fatalf("want exactly one turn/steer, got %d", len(steers))
	}
	var p codexSteerParams
	if err := json.Unmarshal(steers[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	if id, _ := steers[0].stringID(); id != "relay-followup-att-1" || p.ThreadID != "thread-1" || p.ExpectedTurnID != "turn-1" ||
		p.ClientUserMessageID != "m-att-1" || len(p.Input) != 1 || p.Input[0].Text != "When you reach 30, also write BANANA." {
		t.Fatalf("steer = id %s %+v", id, p)
	}
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("want one ack, got %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	if r := h.ledger.Query("att-1", h.ledger.LedgerID(), time.Now().UnixMilli()); r.State != followUpRowWritten {
		t.Fatalf("ledger row %+v", r)
	}
}

// Codex does not de-duplicate a steer (C5): the ledger must.
func TestACodexLiveFollowUpDeliveredTwiceWritesOneSteer(t *testing.T) {
	h, _ := newCodexLiveHarness(t, true)
	h.child.setRespond(answerFollowUpsWith(codexTurnAccepted))
	cmd := h.codexCmd("att-2", "again?")
	h.handle(cmd)
	h.handle(cmd)
	if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 1 {
		t.Fatalf("a redelivered attempt wrote %d steers", n)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("want two acks, got %d", len(acks))
	}
	wantAck(t, acks[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
}

// A redelivery or a query while the answer is pending publishes nothing; the
// in-flight path then answers once.
func TestALiveFollowUpRedeliveredOrQueriedWhileItsAnswerIsPendingPublishesNothing(t *testing.T) {
	h, _ := newCodexLiveHarness(t, true)
	cmd := h.codexCmd("att-3", "wait for it")
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handle(cmd)
	}()
	waitForLive(t, "the steer", func() bool { return len(h.child.framesWithMethod(codexMethodTurnSteer)) == 1 })
	h.handle(cmd)
	h.query("att-3")
	if acks := h.acks.snapshot(); len(acks) != 0 {
		t.Fatalf("an in-flight attempt was answered from the ledger: %+v", acks)
	}
	h.child.emit(codexTurnAccepted("relay-followup-att-3"))
	<-done
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("want one ack from the in-flight path, got %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 1 {
		t.Fatalf("%d steers written", n)
	}
	// Settled, it is answered from the ledger again.
	h.query("att-3")
	if acks := h.acks.snapshot(); len(acks) != 2 || acks[1].FollowUpOutcome != followUpOutcomeWritten {
		t.Fatalf("a settled attempt's query: %+v", acks)
	}
}

func TestEachCodexRefusalIsNotWrittenAndRefusedForever(t *testing.T) {
	cases := []struct {
		answer, outcome, reason string
	}{
		{`{"error":{"code":-32600,"message":"no active turn to steer"},"id":"%s"}`, followUpOutcomeNotWritten, followUpReasonNoActiveTurn},
		{"{\"error\":{\"code\":-32600,\"message\":\"expected active turn id `turn-1` but found `turn-2`\"},\"id\":\"%s\"}", followUpOutcomeNotWritten, followUpReasonTurnMismatch},
		{`{"error":{"code":-32600,"message":"cannot steer a compact turn"},"id":"%s"}`, followUpOutcomeNotWritten, followUpReasonTurnNotSteerable},
		{`{"error":{"code":-32601,"message":"Method not found"},"id":"%s"}`, followUpOutcomeNotWritten, followUpReasonMethodNotSupported},
		{`{"error":{"code":-32602,"message":"Invalid params"},"id":"%s"}`, followUpOutcomeNotWritten, followUpReasonCliRefused},
		{`{"error":{"code":-32603,"message":"Internal error"},"id":"%s"}`, followUpOutcomeUnknown, ""},
		{`{"result":{"accepted":true},"id":"%s"}`, followUpOutcomeUnknown, followUpReasonAnswerUnrecognised},
	}
	for i, c := range cases {
		t.Run(c.reason+c.outcome, func(t *testing.T) {
			h, _ := newCodexLiveHarness(t, true)
			h.child.setRespond(answerFollowUpsWith(func(id string) string { return fmt.Sprintf(c.answer, id) }))
			cmd := h.codexCmd(fmt.Sprintf("att-e%d", i), "x")
			h.handle(cmd)
			h.handle(cmd)
			if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 1 {
				t.Fatalf("%d steers written", n)
			}
			acks := h.acks.snapshot()
			if len(acks) != 2 {
				t.Fatalf("acks %+v", acks)
			}
			for _, a := range acks {
				wantAck(t, a, c.outcome, c.reason, "")
			}
		})
	}
}

// No answer within the timeout: unknown, then a late success lifts it to
// written and is published again.
func TestACodexLiveFollowUpWithNoAnswerIsUnknownUntilALateSuccess(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 150*time.Millisecond)
	h, s := newCodexLiveHarness(t, true)
	h.handle(h.codexCmd("att-4", "late"))
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeUnknown, followUpReasonNoAnswer, "")
	h.child.emit(codexTurnAccepted("relay-followup-att-4"))
	waitForLive(t, "the late ack", func() bool { return len(h.acks.snapshot()) == 2 })
	wantAck(t, h.acks.snapshot()[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	if r := h.ledger.Query("att-4", h.ledger.LedgerID(), time.Now().UnixMilli()); r.State != followUpRowWritten {
		t.Fatalf("ledger row %+v", r)
	}
	waitForLive(t, "the late waiter to be dropped", func() bool {
		s.liveFollowUps.mu.Lock()
		defer s.liveFollowUps.mu.Unlock()
		return len(s.liveFollowUps.waiters) == 0
	})
}

// A late error leaves the row unknown.
func TestALateCodexRefusalLeavesTheAttemptUnknown(t *testing.T) {
	shortenLiveFollowUpAnswerTimeout(t, 100*time.Millisecond)
	h, _ := newCodexLiveHarness(t, true)
	h.handle(h.codexCmd("att-5", "late"))
	h.child.emit(`{"error":{"code":-32600,"message":"no active turn to steer"},"id":"relay-followup-att-5"}`)
	time.Sleep(200 * time.Millisecond)
	if acks := h.acks.snapshot(); len(acks) != 1 {
		t.Fatalf("a late error was published: %+v", acks)
	}
	if r := h.ledger.Query("att-5", h.ledger.LedgerID(), time.Now().UnixMilli()); r.State != followUpRowUnknown {
		t.Fatalf("ledger row %+v", r)
	}
}

func TestACodexLiveFollowUpWhoseChildExitsFirstIsUnknown(t *testing.T) {
	h, _ := newCodexLiveHarness(t, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handle(h.codexCmd("att-6", "gone"))
	}()
	waitForLive(t, "the steer", func() bool { return len(h.child.framesWithMethod(codexMethodTurnSteer)) == 1 })
	h.child.exit()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an exited child left the attempt waiting for its timeout")
	}
	acks := h.acks.snapshot()
	if len(acks) != 1 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeUnknown, followUpReasonNoAnswer, "")
}

// An open server request on the child refuses the attempt with nothing
// written; the answer written to it through Send clears it.
func TestACodexLiveFollowUpBesideAnOpenServerRequestIsNotWritten(t *testing.T) {
	h, s := newCodexLiveHarness(t, true)
	h.child.setRespond(answerFollowUpsWith(codexTurnAccepted))
	h.child.emit(`{"id":41,"method":"execCommandApproval","params":{"command":["ls"]}}`)
	waitForLive(t, "the open request", s.liveFollowUps.approvalPending)
	h.handle(h.codexCmd("att-7", "beside an approval"))
	if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 0 {
		t.Fatalf("a steer was written beside an open approval")
	}
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
	if err := globalCodexAppServerManager.Send(h.sid, `{"id":41,"result":{"decision":"denied"}}`); err != nil {
		t.Fatal(err)
	}
	h.handle(h.codexCmd("att-8", "after it"))
	if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 1 {
		t.Fatalf("want one steer once the request was answered, got %d", n)
	}
	wantAck(t, h.acks.snapshot()[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
}

func TestACodexLiveFollowUpIsFencedBeforeAnythingIsWritten(t *testing.T) {
	cases := map[string]struct {
		tracked bool
		mutate  func(c *commandMsg)
		reason  string
	}{
		"no tracker (uncertified launch)": {false, func(c *commandMsg) {}, followUpReasonNotLiveKind},
		"no signed expectedTurnId":        {true, func(c *commandMsg) { c.ExpectedTurnID = "" }, followUpReasonConversationMismatch},
		"no signed conversationId":        {true, func(c *commandMsg) { c.ConversationID = "" }, followUpReasonConversationMismatch},
		"whitespace-only text":            {true, func(c *commandMsg) { c.Input = " \n\t" }, followUpReasonCliRefused},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, _ := newCodexLiveHarness(t, c.tracked)
			h.child.setRespond(answerFollowUpsWith(codexTurnAccepted))
			cmd := h.codexCmd("att-f", "text")
			c.mutate(&cmd)
			h.handle(cmd)
			h.child.mu.Lock()
			n := len(h.child.frames)
			h.child.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d frames written", n)
			}
			wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, c.reason, "")
		})
	}
}

// A session launched before an in-place upgrade stays queued: Codex is gated
// on its launched version too.
func TestACodexLiveFollowUpIsGatedOnTheLaunchedVersion(t *testing.T) {
	h, _ := newCodexLiveHarnessAt(t, true, "0.167.9")
	h.handle(h.codexCmd("att-v", "text"))
	if n := len(h.child.framesWithMethod(codexMethodTurnSteer)); n != 0 {
		t.Fatalf("a session launched below the minimum was written to")
	}
	wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonNotLiveKind, "")
}

/* --- Grok --- */

func newGrokLiveHarness(t *testing.T) (*liveFollowUpHarness, *GrokACPSession) {
	t.Helper()
	pinLiveFollowUpKinds(t, map[string]string{claudeCodeCliAgentID: "2.1.322", grokCliAgentID: "1.0.77"})
	h := newLiveFollowUpHarness(t, "gk1")
	m := NewGrokACPManager(nil)
	prev := globalGrokACPManager
	globalGrokACPManager = m
	t.Cleanup(func() { globalGrokACPManager = prev })
	s := &GrokACPSession{
		ID: h.sid, Stdin: h.child.stdinW, Stdout: h.child.stdoutR, Stderr: h.child.stderrR,
		status: "running", done: make(chan struct{}), processExited: make(chan struct{}), streamDone: make(chan struct{}),
		firstFrame: make(chan struct{}), launchedVersion: "1.0.77", liveFollowUps: newLiveFollowUpTrackerFor(grokCliAgentID),
	}
	m.sessions[h.sid] = s
	h.done = s.streamDone
	go m.readStream(s, h.stream.publish)
	t.Cleanup(h.finish)
	// The fake Grok: session/new answers the ACP session, a prompt starts
	// running (G7), and an interjection is queued and broadcast (G3, G4).
	h.child.setRespond(func(f jsonRPCFrame, line string) []string {
		switch f.Method {
		case acpMethodSessionNew:
			return []string{`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"sessionId":"acp-1"}}`}
		case acpMethodSessionPrompt:
			return []string{`{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"acp-1","entries":[],"runningPromptId":"p-int-1","runningText":"Count slowly","runningKind":"prompt"}}`}
		case grokMethodInterject:
			var p grokInterjectParams
			_ = json.Unmarshal(f.Params, &p)
			return []string{
				`{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"result":{"status":"queued"}}}`,
				`{"jsonrpc":"2.0","method":"_x.ai/session/interjection","params":{"sessionId":"acp-1","text":` + jsonStringLiteral(p.Text) + `,"interjectionId":"` + p.InterjectionID + `"}}`,
			}
		}
		return nil
	})
	return h, s
}

func jsonStringLiteral(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// openSession runs session/new; startPrompt sends the client prompt (id 4)
// and waits until Grok names it running.
func (h *liveFollowUpHarness) openGrokSession(s *GrokACPSession) {
	h.t.Helper()
	if err := globalGrokACPManager.Send(h.sid, `{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/w","mcpServers":[]}}`); err != nil {
		h.t.Fatal(err)
	}
	waitForLive(h.t, "the ACP session", func() bool { return s.liveFollowUps.acpSession() == "acp-1" })
}

func (h *liveFollowUpHarness) startGrokPrompt(s *GrokACPSession) {
	h.t.Helper()
	if err := globalGrokACPManager.Send(h.sid, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"Count slowly"}]}}`); err != nil {
		h.t.Fatal(err)
	}
	waitForLive(h.t, "the running prompt", s.liveFollowUps.grokTurnRunning)
}

func (h *liveFollowUpHarness) grokCmd(attemptID, text string) commandMsg {
	now := time.Now()
	return commandMsg{
		ID: relayFollowUpEnvelopePrefix + attemptID, Type: relayFollowUpCommandType, SessionID: h.sid,
		Input: text, Ts: now.UnixMilli(), ConversationID: "acp-1",
		FollowUpID: "m-" + attemptID, FollowUpAttemptID: attemptID,
		FollowUpAttemptExpiresAt: now.Add(time.Minute).UnixMilli(), FollowUpLedgerID: h.ledger.LedgerID(),
	}
}

const (
	grokP1TurnCompleted  = `{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{"sessionId":"acp-1","update":{"sessionUpdate":"turn_completed","prompt_id":"p-int-1","stop_reason":"end_turn"}}}`
	grokP1PromptComplete = `{"jsonrpc":"2.0","method":"_x.ai/session/prompt_complete","params":{"sessionId":"acp-1","promptId":"p-int-1","stopReason":"end_turn"}}`
	grokSessionsIdle     = `{"jsonrpc":"2.0","method":"_x.ai/sessions/changed","params":{"upserted":[{"sessionId":"acp-1","activity":"idle"}]}}`
	grokP1Answer         = `{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn","_meta":{"sessionId":"acp-1","promptId":"p-int-1"}}}`
	grokQueueEmpty       = `{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"acp-1","entries":[]}}`
	grokFallbackID       = "interject-fallback-01a1d7fe-70e0-7071-889b-9da30180d9cf"
	grokFallbackChunk    = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"FALLBACK-REPLY"}},"_meta":{"promptId":"` + grokFallbackID + `"}}}`
	grokFallbackDone     = `{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{"sessionId":"acp-1","update":{"sessionUpdate":"turn_completed","prompt_id":"` + grokFallbackID + `","stop_reason":"end_turn"}}}`
	grokSentinel         = `{"jsonrpc":"2.0","method":"_x.ai/settings/update","params":{"sentinel":"SENTINEL-%d"}}`
)

func grokFallbackRunning(text string) string {
	return `{"jsonrpc":"2.0","method":"_x.ai/queue/changed","params":{"sessionId":"acp-1","entries":[],"runningPromptId":"` + grokFallbackID + `","runningText":` + jsonStringLiteral(text) + `,"runningKind":"prompt"}}`
}

// sentinel emits a marker frame and waits until it is published, so every
// frame emitted before it has been read and published (or held).
func (h *liveFollowUpHarness) sentinel(n int) {
	h.t.Helper()
	marker := fmt.Sprintf("SENTINEL-%d", n)
	h.child.emit(fmt.Sprintf(grokSentinel, n))
	waitForLive(h.t, marker, func() bool { return h.stream.indexOf(marker) >= 0 })
}

func TestAGrokLiveFollowUpIsInterjectedOnceAndWrittenOnItsAnswer(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	cmd := h.grokCmd("att-g1", "When you reach 30, also write BANANA.")
	h.handle(cmd)
	h.handle(cmd)
	interjects := h.child.framesWithMethod(grokMethodInterject)
	if len(interjects) != 1 {
		t.Fatalf("want exactly one _x.ai/interject, got %d", len(interjects))
	}
	var p grokInterjectParams
	_ = json.Unmarshal(interjects[0].Params, &p)
	if id, _ := interjects[0].stringID(); id != "relay-followup-att-g1" || p.SessionID != "acp-1" || p.InterjectionID != "att-g1" || p.Text != cmd.Input {
		t.Fatalf("interject = id %s %+v", id, p)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("acks %+v", acks)
	}
	wantAck(t, acks[0], followUpOutcomeWritten, "", followUpProcessedInCurrent)
	wantAck(t, acks[1], followUpOutcomeWritten, "", followUpProcessedInCurrent)
}

// Never into an idle session (G8): no outstanding prompt, or one whose turn
// has completed.
func TestAGrokLiveFollowUpWithNoRunningTurnIsNotWritten(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.handle(h.grokCmd("att-g2", "idle"))
	h.startGrokPrompt(s)
	h.child.emit(grokP1TurnCompleted)
	waitForLive(t, "the turn to finish", func() bool { return !s.liveFollowUps.grokTurnRunning() })
	h.handle(h.grokCmd("att-g3", "after the turn"))
	if n := len(h.child.framesWithMethod(grokMethodInterject)); n != 0 {
		t.Fatalf("%d interjections written with no running turn", n)
	}
	acks := h.acks.snapshot()
	if len(acks) != 2 {
		t.Fatalf("acks %+v", acks)
	}
	for _, a := range acks {
		wantAck(t, a, followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	}
}

func TestAGrokLiveFollowUpIsFencedBeforeAnythingIsWritten(t *testing.T) {
	t.Run("open permission request", func(t *testing.T) {
		h, s := newGrokLiveHarness(t)
		h.openGrokSession(s)
		h.startGrokPrompt(s)
		h.child.emit(`{"jsonrpc":"2.0","id":11,"method":"session/request_permission","params":{"sessionId":"acp-1"}}`)
		waitForLive(t, "the permission request", s.liveFollowUps.approvalPending)
		h.handle(h.grokCmd("att-g4", "beside an approval"))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
		if n := len(h.child.framesWithMethod(grokMethodInterject)); n != 0 {
			t.Fatal("an interjection was written beside an open permission request")
		}
	})
	t.Run("another ACP session", func(t *testing.T) {
		h, s := newGrokLiveHarness(t)
		h.openGrokSession(s)
		h.startGrokPrompt(s)
		cmd := h.grokCmd("att-g5", "elsewhere")
		cmd.ConversationID = "acp-other"
		h.handle(cmd)
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonConversationMismatch, "")
		if n := len(h.child.framesWithMethod(grokMethodInterject)); n != 0 {
			t.Fatal("an interjection was written into another ACP session")
		}
	})
	t.Run("empty text", func(t *testing.T) {
		h, s := newGrokLiveHarness(t)
		h.openGrokSession(s)
		h.startGrokPrompt(s)
		h.handle(h.grokCmd("att-g6", "   "))
		wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, followUpReasonCliRefused, "")
		if n := len(h.child.framesWithMethod(grokMethodInterject)); n != 0 {
			t.Fatal("an empty interjection was written (Grok would run it, G11)")
		}
	})
}

func TestGrokRefusalsMapToTheirReasons(t *testing.T) {
	for _, c := range []struct{ answer, reason string }{
		{`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}`, followUpReasonMethodNotSupported},
		{`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"Invalid params","data":"session not found: acp-1"}}`, followUpReasonConversationMismatch},
	} {
		t.Run(c.reason, func(t *testing.T) {
			h, s := newGrokLiveHarness(t)
			h.openGrokSession(s)
			h.startGrokPrompt(s)
			h.child.setRespond(func(f jsonRPCFrame, line string) []string {
				if f.Method == grokMethodInterject {
					return []string{fmt.Sprintf(c.answer, string(f.ID))}
				}
				return nil
			})
			h.handle(h.grokCmd("att-gr", "x"))
			wantAck(t, h.acks.snapshot()[0], followUpOutcomeNotWritten, c.reason, "")
		})
	}
}

// Drained (G6/G7): the interjection joined the running turn, so the prompt's
// answer is forwarded at once.
func TestAGrokPromptAnswerIsForwardedAtOnceWhenTheInterjectionDrained(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	h.handle(h.grokCmd("att-d", "drained text"))
	for _, f := range []string{grokQueueEmpty, grokP1TurnCompleted, grokP1PromptComplete, grokSessionsIdle, grokP1Answer} {
		h.child.emit(f)
	}
	waitForLive(t, "the prompt answer", func() bool { return h.stream.indexOf(`"id":4,"result"`) >= 0 })
	if n := h.stream.countOf(`"id":4,"result"`); n != 1 {
		t.Fatalf("the answer was published %d times", n)
	}
}

// Stranded (G10 then G8): a fallback of our interjection is seen before the
// prompt's answer, so the answer is held until that fallback's turn_completed
// and then published, once, with a higher seq.
func TestAGrokPromptAnswerIsHeldUntilAStrandedInterjectionsFallbackCompletes(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	h.handle(h.grokCmd("att-s", "stranded text"))
	for _, f := range []string{grokQueueEmpty, grokP1TurnCompleted, grokFallbackRunning("stranded text"),
		grokP1PromptComplete, grokSessionsIdle, grokP1Answer, grokFallbackChunk} {
		h.child.emit(f)
	}
	h.sentinel(1)
	if h.stream.indexOf(`"id":4,"result"`) >= 0 {
		t.Fatal("the prompt answer was published while its stranded interjection's fallback still ran")
	}
	h.child.emit(grokFallbackDone)
	waitForLive(t, "the released answer", func() bool { return h.stream.indexOf(`"id":4,"result"`) >= 0 })
	frames := h.stream.snapshot()
	answer, done := h.stream.indexOf(`"id":4,"result"`), h.stream.indexOf(`"prompt_id":"`+grokFallbackID+`"`)
	if done < 0 || answer < done || frames[answer].Seq <= frames[done].Seq {
		t.Fatalf("the answer (index %d) must follow the fallback's turn_completed (index %d) with a higher seq", answer, done)
	}
	if frames[answer].Output != grokP1Answer {
		t.Fatalf("the held answer was edited: %s", frames[answer].Output)
	}
	if n := h.stream.countOf(`"id":4,"result"`); n != 1 {
		t.Fatalf("the answer was published %d times", n)
	}
	// Seqs stay gapless: the held frame took no seq while it was held.
	seen := map[int]bool{}
	for _, f := range frames {
		seen[f.Seq] = true
	}
	for i := 1; i <= len(frames); i++ {
		if !seen[i] {
			t.Fatalf("seq %d is missing from %d frames", i, len(frames))
		}
	}
}

// The child exits while an answer is held: the answer is published before the
// reader ends, so before grok_acp_ended (whose seq waitForExit takes only
// after streamDone).
func TestAHeldGrokPromptAnswerIsPublishedBeforeTheReaderEnds(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	h.handle(h.grokCmd("att-x", "exit text"))
	for _, f := range []string{grokP1TurnCompleted, grokFallbackRunning("exit text"), grokP1Answer} {
		h.child.emit(f)
	}
	h.sentinel(2)
	if h.stream.indexOf(`"id":4,"result"`) >= 0 {
		t.Fatal("not held")
	}
	h.child.exit()
	select {
	case <-s.streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not end")
	}
	frames := h.stream.snapshot()
	if i := h.stream.indexOf(`"id":4,"result"`); i != len(frames)-1 {
		t.Fatalf("the held answer is not the last frame before the end (index %d of %d)", i, len(frames))
	}
}

func TestAHeldGrokPromptAnswerIsReleasedAtTheCap(t *testing.T) {
	prev := grokHeldAnswerMax
	grokHeldAnswerMax = 150 * time.Millisecond
	t.Cleanup(func() { grokHeldAnswerMax = prev })
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	h.handle(h.grokCmd("att-c", "cap text"))
	for _, f := range []string{grokP1TurnCompleted, grokFallbackRunning("cap text"), grokP1Answer} {
		h.child.emit(f)
	}
	waitForLive(t, "the answer at the cap", func() bool { return h.stream.indexOf(`"id":4,"result"`) >= 0 })
	// Its fallback finishing later releases nothing more.
	h.child.emit(grokFallbackDone)
	h.sentinel(3)
	if n := h.stream.countOf(`"id":4,"result"`); n != 1 {
		t.Fatalf("the answer was published %d times", n)
	}
}

// The order the hold cannot cover (§2.8 "Not held"): a fallback that starts
// after its prompt's answer was published is only counted.
func TestAGrokFallbackAfterItsPromptsAnswerIsCounted(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	h.handle(h.grokCmd("att-a", "after answer text"))
	before := grokStrandedAfterAnswer.Load()
	for _, f := range []string{grokP1TurnCompleted, grokP1Answer, grokFallbackRunning("after answer text")} {
		h.child.emit(f)
	}
	h.sentinel(4)
	if h.stream.indexOf(`"id":4,"result"`) < 0 {
		t.Fatal("an answer with nothing stranded was held")
	}
	if got := grokStrandedAfterAnswer.Load(); got != before+1 {
		t.Fatalf("grokStrandedAfterAnswer = %d, want %d", got, before+1)
	}
}

// Grok's text-only frames the hold reads never change the frames forwarded.
func TestGrokFramesTheTrackerReadsAreForwardedVerbatim(t *testing.T) {
	h, s := newGrokLiveHarness(t)
	h.openGrokSession(s)
	h.startGrokPrompt(s)
	frames := []string{grokQueueEmpty, `{"jsonrpc":"2.0","id":"skills-reload","result":{"result":{"reloaded":1}}}`, grokP1TurnCompleted, grokP1Answer}
	for _, f := range frames {
		h.child.emit(f)
	}
	h.sentinel(5)
	for _, f := range frames {
		if h.stream.countOf(f) != 1 {
			t.Fatalf("frame not forwarded verbatim once: %s", f)
		}
	}
}
