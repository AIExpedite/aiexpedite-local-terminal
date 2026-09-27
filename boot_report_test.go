// Tests for the /online boot report, its signature, fenced sessions, and the
// boot id on pongs and session error frames (deregister.go,
// fenced_sessions.go, pubsub.go).
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// Known vector, computed with terminal-service's reference implementation
// (buildBootReportSignedMessage in src/utils/hmac.util.js, Node crypto):
//
//	sorted = ["s-a","s-b","sess_3"]; digest = sha256hex("s-a,s-b,sess_3")
//	HMAC-SHA256("secret-xyz", message) as lowercase hex
const (
	vectorAgentID   = "agent-1"
	vectorTimestamp = int64(1727000000000)
	vectorBootID    = "6f1c1f5e-0d0e-4b7a-9c4e-1b2a3c4d5e6f"
	vectorSecret    = "secret-xyz"
	vectorMessage   = "agent-1:1727000000000:6f1c1f5e-0d0e-4b7a-9c4e-1b2a3c4d5e6f:493323ece3a31eacec6e99725a860ec6546e76a0e9cddd6fea27f33127f88577"
	vectorSignature = "8298ff3d30a4b125ed6cfbd41e304865489ad56596abb35d2474a06f1a84ef0d"
	// Empty list: the digest of "".
	vectorEmptyMessage   = "agent-1:1727000000000:boot-2:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	vectorEmptySignature = "081571f70d4dc1ab4251c7ebab9bf736855a8bc5814733a1aa85d2ced15b9ce4"
)

func TestBootReportSignatureMatchesServerVector(t *testing.T) {
	msg := buildBootReportSignedMessage(vectorAgentID, vectorTimestamp, vectorBootID, []string{"s-b", "sess_3", "s-a"})
	if msg != vectorMessage {
		t.Fatalf("message\n got %s\nwant %s", msg, vectorMessage)
	}
	if sig := generateHMAC(msg, vectorSecret); sig != vectorSignature {
		t.Fatalf("signature %s, want %s", sig, vectorSignature)
	}
	empty := buildBootReportSignedMessage(vectorAgentID, vectorTimestamp, "boot-2", nil)
	if empty != vectorEmptyMessage || generateHMAC(empty, vectorSecret) != vectorEmptySignature {
		t.Fatalf("empty list: %s", empty)
	}
	// The caller's slice is never reordered.
	in := []string{"b", "a"}
	_ = buildBootReportSignedMessage("x", 1, "y", in)
	if in[0] != "b" {
		t.Fatalf("input list was sorted in place")
	}
}

// expectedBootReportSignature rebuilds the signature from the documented
// format, independently of buildBootReportSignedMessage.
func expectedBootReportSignature(secret, agentID string, ts int64, bootID string, reaped []string) string {
	sorted := append([]string(nil), reaped...)
	sort.Strings(sorted)
	digest := sha256.Sum256([]byte(strings.Join(sorted, ",")))
	msg := agentID + ":" + strconv.FormatInt(ts, 10) + ":" + bootID + ":" + hex.EncodeToString(digest[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

type onlineCapture struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
}

func (c *onlineCapture) server(t *testing.T, status func(n int) int, answer string) *httptest.Server {
	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("body: %v", err)
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		code := status(int(atomic.AddInt32(&n, 1)))
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = w.Write([]byte(answer))
		}
	}))
}

func bodyStringList(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(string))
	}
	return out
}

func withBootReport(t *testing.T, r bootReport) {
	t.Helper()
	prev := bootReportForOnline
	bootReportForOnline = func(context.Context) bootReport { return r }
	t.Cleanup(func() { bootReportForOnline = prev })
}

func withOnlineAccepted(t *testing.T) *[]*onlineResponse {
	t.Helper()
	prev := onOnlineAccepted
	var got []*onlineResponse
	var mu sync.Mutex
	onOnlineAccepted = func(resp *onlineResponse) {
		mu.Lock()
		got = append(got, resp)
		mu.Unlock()
	}
	t.Cleanup(func() { onOnlineAccepted = prev })
	return &got
}

func TestOnlineCarriesSignedBootReport(t *testing.T) {
	resetConnectivityState(t)
	withBootReport(t, bootReport{
		BootID:            "boot-new",
		PreviousBootID:    "boot-old",
		SessionsReaped:    []string{"s-2", "s-1"},
		SessionsSurviving: []string{"s-9"},
	})
	accepted := withOnlineAccepted(t)

	c := &onlineCapture{}
	srv := c.server(t, func(int) int { return http.StatusOK }, `{"success":true,"fencedSessionIds":["s-f"]}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-boot", CommandSecret: "secret-boot"}

	if err := notifyOnline(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 1 {
		t.Fatalf("requests = %d", len(c.bodies))
	}
	b := c.bodies[0]
	if b["bootId"] != "boot-new" || b["previousBootId"] != "boot-old" {
		t.Fatalf("boot ids: %v", b)
	}
	reaped := bodyStringList(t, b["sessionsReaped"])
	if !reflect.DeepEqual(bodyStringList(t, b["sessionsSurviving"]), []string{"s-9"}) {
		t.Fatalf("sessionsSurviving: %v", b["sessionsSurviving"])
	}
	ts := int64(b["timestamp"].(float64))
	// The ordinary signature is unchanged ...
	if b["signature"] != generateHMAC("agent-boot:"+strconv.FormatInt(ts, 10), "secret-boot") {
		t.Fatalf("request signature changed")
	}
	// ... and the boot report signature covers the list as sent, with the
	// SAME timestamp.
	want := expectedBootReportSignature("secret-boot", "agent-boot", ts, "boot-new", reaped)
	if b["bootReportSignature"] != want {
		t.Fatalf("bootReportSignature = %v, want %s", b["bootReportSignature"], want)
	}

	if len(*accepted) != 1 {
		t.Fatalf("accepted callbacks = %d", len(*accepted))
	}
	resp := (*accepted)[0]
	if !reflect.DeepEqual(resp.sentReaped, reaped) || !reflect.DeepEqual(resp.FencedSessionIDs, []string{"s-f"}) {
		t.Fatalf("accepted response: %+v", resp)
	}
}

func TestOnlineWithVersionAlsoCarriesBootReport(t *testing.T) {
	resetConnectivityState(t)
	withBootReport(t, bootReport{BootID: "boot-v", SessionsReaped: []string{}, SessionsSurviving: []string{}})
	withOnlineAccepted(t)
	c := &onlineCapture{}
	srv := c.server(t, func(int) int { return http.StatusOK }, `{}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-v", CommandSecret: "secret-v"}
	if err := notifyOnlineWithVersion(context.Background(), cfg, "v9", "attempt-1"); err != nil {
		t.Fatal(err)
	}
	b := c.bodies[0]
	if b["bootId"] != "boot-v" || b["version"] != "v9" || b["attemptId"] != "attempt-1" {
		t.Fatalf("body: %v", b)
	}
	if _, ok := b["previousBootId"]; ok {
		t.Fatalf("an unknown previous boot must be omitted")
	}
	ts := int64(b["timestamp"].(float64))
	if b["bootReportSignature"] != expectedBootReportSignature("secret-v", "agent-v", ts, "boot-v", nil) {
		t.Fatalf("empty-list signature wrong")
	}
}

func TestOfflineAndDrainCarryNoBootReport(t *testing.T) {
	resetConnectivityState(t)
	withBootReport(t, bootReport{BootID: "boot-z", SessionsReaped: []string{"s"}})
	accepted := withOnlineAccepted(t)
	c := &onlineCapture{}
	srv := c.server(t, func(int) int { return http.StatusOK }, `{"fencedSessionIds":["x"]}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-z", CommandSecret: "secret-z"}
	if err := notifyOffline(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := notifyDrain(context.Background(), cfg, "a", "v"); err != nil {
		t.Fatal(err)
	}
	for _, b := range c.bodies {
		for _, k := range []string{"bootId", "sessionsReaped", "bootReportSignature"} {
			if _, ok := b[k]; ok {
				t.Fatalf("%s sent on a non-online call: %v", k, b)
			}
		}
	}
	if len(*accepted) != 0 {
		t.Fatalf("a non-online answer was applied as /online")
	}
}

func TestExtraFieldsCannotForgeTheBootReport(t *testing.T) {
	payload := map[string]any{}
	for _, k := range []string{"bootId", "sessionsReaped", "bootReportSignature", "previousBootId", "sessionsSurviving"} {
		if !isBootReportField(k) {
			t.Fatalf("%s is not protected", k)
		}
	}
	cfg := &Config{AgentID: "a", CommandSecret: "s"}
	sent := addBootReport(payload, cfg, 5, bootReport{BootID: "b", SessionsReaped: []string{"z", "y"}})
	if !reflect.DeepEqual(sent, []string{"z", "y"}) || payload["bootReportSignature"] != expectedBootReportSignature("s", "a", 5, "b", sent) {
		t.Fatalf("addBootReport: %v", payload)
	}
	if len(addBootReportPayload(bootReport{})) != 0 {
		t.Fatalf("a report without a boot id adds nothing")
	}
}

func addBootReportPayload(r bootReport) map[string]any {
	p := map[string]any{}
	addBootReport(p, &Config{AgentID: "a", CommandSecret: "s"}, 1, r)
	return p
}

// TestReapedSessionsKeptUntilAnOnlineIsAccepted drives the real ledger
// through a failing then an accepting /online.
func TestReapedSessionsKeptUntilAnOnlineIsAccepted(t *testing.T) {
	resetConnectivityState(t)
	resetFencedSessions(t)
	dir := t.TempDir()
	writeLedgerFixture(t, dir, ledgerFileData{
		BootID: "boot-a",
		Entries: []*ledgerEntry{
			{SessionID: "s-dead", BootID: "boot-a", PIDs: []ledgerProcess{{PID: 1, StartTime: "x"}}},
		},
	})
	l := newTestLedger(t, dir, "boot-b")
	l.probe = func(ledgerProcess) processProbeResult { return processGone }
	l.RunBootReap()
	// The real ledger path, on a private ledger (never the package global,
	// which other tests' background sessions may be using).
	prevReport, prevAccepted := bootReportForOnline, onOnlineAccepted
	bootReportForOnline = func(ctx context.Context) bootReport { return l.Report(ctx) }
	onOnlineAccepted = func(resp *onlineResponse) { applyOnlineAccepted(l, resp) }
	t.Cleanup(func() { bootReportForOnline, onOnlineAccepted = prevReport, prevAccepted })

	var failing atomic.Bool
	failing.Store(true)
	c := &onlineCapture{}
	srv := c.server(t, func(int) int {
		if failing.Load() {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}, `{"success":true}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-k", CommandSecret: "secret-k"}

	if err := notifyOnline(context.Background(), cfg); err == nil {
		t.Fatalf("expected the failing /online to error")
	}
	if got := l.Report(context.Background()).SessionsReaped; !reflect.DeepEqual(got, []string{"s-dead"}) {
		t.Fatalf("a rejected /online dropped the proof: %v", got)
	}
	failing.Store(false)
	if err := notifyOnline(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	last := c.bodies[len(c.bodies)-1]
	if !reflect.DeepEqual(bodyStringList(t, last["sessionsReaped"]), []string{"s-dead"}) || last["previousBootId"] != "boot-a" {
		t.Fatalf("accepted body: %v", last)
	}
	if got := l.Report(context.Background()).SessionsReaped; len(got) != 0 {
		t.Fatalf("an accepted report must drop the entry: %v", got)
	}
	// The next /online no longer lists it.
	if err := notifyOnline(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := bodyStringList(t, c.bodies[len(c.bodies)-1]["sessionsReaped"]); len(got) != 0 {
		t.Fatalf("reaped session re-sent after ack: %v", got)
	}
}

func resetFencedSessions(t *testing.T) {
	t.Helper()
	clear := func() {
		fencedSessions.Lock()
		fencedSessions.ids = make(map[string]struct{})
		fencedSessions.order = nil
		fencedSessions.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// TestFencedSessionsAreRefusedAndEndedFirst: the fence is in force before the
// /online call returns, every command but END is refused, and each fenced
// session is ended.
func TestFencedSessionsAreRefusedAndEndedFirst(t *testing.T) {
	resetConnectivityState(t)
	resetFencedSessions(t)
	withBootReport(t, bootReport{BootID: "boot-f", SessionsReaped: []string{}, SessionsSurviving: []string{}})

	endedCh := make(chan string, 4)
	prevEnd := endFencedSession
	endFencedSession = func(id string) {
		if !isSessionFenced(id) {
			t.Errorf("session %s ended before it was fenced", id)
		}
		endedCh <- id
	}
	t.Cleanup(func() { endFencedSession = prevEnd })

	c := &onlineCapture{}
	srv := c.server(t, func(int) int { return http.StatusOK }, `{"fencedSessionIds":["s-moved","s-parked"]}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	if err := notifyOnline(context.Background(), &Config{AgentID: "agent-f", CommandSecret: "secret-f"}); err != nil {
		t.Fatal(err)
	}
	// Fenced synchronously, before notifyOnline returned.
	if !isSessionFenced("s-moved") || !isSessionFenced("s-parked") || isSessionFenced("s-other") {
		t.Fatalf("fence set wrong")
	}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-endedCh:
			got[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("fenced sessions not ended: %v", got)
		}
	}
	if !got["s-moved"] || !got["s-parked"] {
		t.Fatalf("ended = %v", got)
	}

	// A repeated /online does not end them again.
	if err := notifyOnline(context.Background(), &Config{AgentID: "agent-f", CommandSecret: "secret-f"}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-endedCh:
		t.Fatalf("session %s ended twice", id)
	case <-time.After(200 * time.Millisecond):
	}

	// The entry gate: input refused, END let through, other sessions untouched.
	var published []resultMsg
	prevPublish := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		published = append(published, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prevPublish })
	cfg := &Config{AgentID: "agent-f"}

	for _, typ := range []string{"codex_appserver_send", "session_input", "claude_native_start", "grok_acp_send"} {
		if !refuseFencedSessionCommand(context.Background(), nil, nil, commandMsg{ID: "c", Type: typ, SessionID: "s-moved"}, cfg) {
			t.Fatalf("%s for a fenced session was not refused", typ)
		}
	}
	for _, typ := range []string{"codex_appserver_end", "session_end", "claude_native_end", "grok_acp_end", "antigravity_native_end", "opencode_native_end"} {
		if refuseFencedSessionCommand(context.Background(), nil, nil, commandMsg{Type: typ, SessionID: "s-moved"}, cfg) {
			t.Fatalf("%s must be let through", typ)
		}
	}
	if refuseFencedSessionCommand(context.Background(), nil, nil, commandMsg{Type: "codex_appserver_send", SessionID: "s-other"}, cfg) {
		t.Fatalf("an unfenced session was refused")
	}
	if len(published) != 4 {
		t.Fatalf("published %d refusals", len(published))
	}
	r := published[0]
	if r.Status != "denied" || r.RejectionReason != fencedSessionRejectionCode || r.SessionID != "s-moved" ||
		r.Type != "codex_appserver_error" || r.BootID != agentBootID {
		t.Fatalf("refusal frame: %+v", r)
	}
}

func TestPongCarriesBootID(t *testing.T) {
	res := pongResult(commandMsg{ID: "p1", AgentID: "a", UID: "u", WorkspaceID: "w"})
	if res.BootID == "" || res.BootID != agentBootID || res.Output != "pong" || res.ID != "p1" {
		t.Fatalf("pong: %+v", res)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"bootId":"`+agentBootID+`"`) {
		t.Fatalf("bootId not on the wire: %s", raw)
	}
}

func TestSessionErrorFramesCarryBootID(t *testing.T) {
	var published []resultMsg
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		published = append(published, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })
	cmd := commandMsg{ID: "c", SessionID: "s"}
	publishSessionError(context.Background(), nil, cmd, "session s not found")
	publishCodexAppServerError(context.Background(), nil, cmd, "x")
	publishClaudeNativeError(context.Background(), nil, cmd, "x")
	publishAntigravityNativeError(context.Background(), nil, cmd, "x")
	publishOpenCodeNativeError(context.Background(), nil, cmd, "x")
	publishGrokACPError(context.Background(), nil, cmd, "x", "")
	if len(published) != 6 {
		t.Fatalf("published %d", len(published))
	}
	for _, r := range published {
		if r.BootID != agentBootID {
			t.Fatalf("%s frame without bootId", r.Type)
		}
	}
}

func resetFenceReportForTest(t *testing.T) {
	t.Helper()
	resetFenceReport()
	t.Cleanup(resetFenceReport)
}

// TestSessionCommandsWaitForTheFenceReport: until an /online fence report is
// accepted, every session command but END is held and then Nacked.
func TestSessionCommandsWaitForTheFenceReport(t *testing.T) {
	resetFenceReportForTest(t)
	ctx := context.Background()
	input := commandMsg{Type: "codex_appserver_send", SessionID: "s1"}
	start := commandMsg{Type: "claude_native_start", SessionID: "s2"}

	if !holdForFenceReport(ctx, nil, input, 20*time.Millisecond) || !holdForFenceReport(ctx, nil, start, 20*time.Millisecond) {
		t.Fatalf("a session command ran before the fence report")
	}
	if holdForFenceReport(ctx, nil, commandMsg{Type: "codex_appserver_end", SessionID: "s1"}, 20*time.Millisecond) {
		t.Fatalf("END must never wait")
	}

	// A waiting command is released the moment the report lands.
	released := make(chan bool, 1)
	go func() { released <- holdForFenceReport(ctx, nil, input, 5*time.Second) }()
	time.Sleep(50 * time.Millisecond)
	markFenceReportApplied()
	select {
	case held := <-released:
		if held {
			t.Fatalf("command still held after the report")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("waiting command not released")
	}

	// Going offline forgets it: the next connection must learn it again.
	resetFenceReport()
	if !holdForFenceReport(ctx, nil, input, 20*time.Millisecond) {
		t.Fatalf("fence report survived a disconnect")
	}
}

func TestAcceptedOnlineAppliesTheFenceReport(t *testing.T) {
	resetConnectivityState(t)
	resetFencedSessions(t)
	resetFenceReportForTest(t)
	withBootReport(t, bootReport{BootID: "boot-g", SessionsReaped: []string{}, SessionsSurviving: []string{}})
	c := &onlineCapture{}
	var ok atomic.Bool
	srv := c.server(t, func(int) int {
		if ok.Load() {
			return http.StatusOK
		}
		return http.StatusBadGateway
	}, `{}`)
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-g", CommandSecret: "secret-g"}

	if err := notifyOnline(context.Background(), cfg); err == nil || fenceReportApplied() {
		t.Fatalf("a rejected /online must not apply the fence report")
	}
	ok.Store(true)
	if err := notifyOnline(context.Background(), cfg); err != nil || !fenceReportApplied() {
		t.Fatalf("an accepted /online must apply the fence report (err %v)", err)
	}
}

// TestEnsureFenceReportRetriesUntilAccepted: a failed boot / reconnect
// /online cannot hold session commands for the whole connection.
func TestEnsureFenceReportRetriesUntilAccepted(t *testing.T) {
	resetConnectivityState(t)
	resetFenceReportForTest(t)
	prevDelays, prevSend := fenceReportRetryDelays, sendOnlineForFenceReport
	fenceReportRetryDelays = []time.Duration{10 * time.Millisecond}
	var calls atomic.Int32
	done := make(chan struct{})
	sendOnlineForFenceReport = func(context.Context, *Config) error {
		if calls.Add(1) < 3 {
			return io.ErrUnexpectedEOF
		}
		markFenceReportApplied()
		close(done)
		return nil
	}
	t.Cleanup(func() { fenceReportRetryDelays, sendOnlineForFenceReport = prevDelays, prevSend })

	ensureFenceReport(&Config{AgentID: "a", CommandSecret: "s"})
	ensureFenceReport(&Config{AgentID: "a", CommandSecret: "s"}) // single-flight
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("fence report never retried to acceptance (calls = %d)", calls.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for fenceWorkerRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fenceWorkerRunning() {
		t.Fatalf("retry loop did not stop once applied")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}

	// Unregistered: nothing is sent.
	resetFenceReport()
	calls.Store(0)
	ensureFenceReport(&Config{})
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("an unregistered agent sent /online")
	}
}

// TestReconnectResetsTheFenceReport: the first connection keeps a report its
// boot /online already applied; every later connection forgets it.
func TestReconnectResetsTheFenceReport(t *testing.T) {
	resetFenceReportForTest(t)
	prev := fenceReportConnections.Load()
	t.Cleanup(func() { fenceReportConnections.Store(prev) })
	unregistered := &Config{} // ensureFenceReport sends nothing for it

	fenceReportConnections.Store(0)
	markFenceReportApplied()
	beginFenceReportConnection(unregistered)
	if !fenceReportApplied() {
		t.Fatalf("the first connection discarded the boot report")
	}
	beginFenceReportConnection(unregistered)
	if fenceReportApplied() {
		t.Fatalf("a reconnect kept the previous connection's fence report")
	}
	if holdForFenceReport(context.Background(), nil, commandMsg{Type: "session_signal", SessionID: "s"}, 10*time.Millisecond) {
		t.Fatalf("a signal (cancel) must never wait")
	}
}

// TestUnreadableOnlineAnswerAppliesNoFences: a 200 whose body cannot be read
// or parsed completely may be missing a fenced session, so it neither applies
// the fence report nor acks the reaped sessions it carried.
func TestUnreadableOnlineAnswerAppliesNoFences(t *testing.T) {
	for _, body := range []string{"", `{"fencedSessionIds":["s-1"`, "<html>proxy</html>"} {
		resetConnectivityState(t)
		resetFencedSessions(t)
		resetFenceReportForTest(t)
		dir := t.TempDir()
		writeLedgerFixture(t, dir, ledgerFileData{BootID: "boot-u", Entries: []*ledgerEntry{
			{SessionID: "s-gone", BootID: "boot-u", State: ledgerStateReaped, PIDs: []ledgerProcess{}},
		}})
		l := newTestLedger(t, dir, "boot-v")
		l.RunBootReap()
		prevReport, prevAccepted := bootReportForOnline, onOnlineAccepted
		bootReportForOnline = func(ctx context.Context) bootReport { return l.Report(ctx) }
		onOnlineAccepted = func(resp *onlineResponse) { applyOnlineAccepted(l, resp) }

		c := &onlineCapture{}
		srv := c.server(t, func(int) int { return http.StatusOK }, body)
		t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
		err := notifyOnline(context.Background(), &Config{AgentID: "agent-u", CommandSecret: "secret-u"})
		srv.Close()
		bootReportForOnline, onOnlineAccepted = prevReport, prevAccepted
		if err != nil {
			t.Fatalf("body %q: a 200 is still online: %v", body, err)
		}
		if fenceReportApplied() {
			t.Fatalf("body %q: an unreadable answer applied the fence report", body)
		}
		if got := l.Report(context.Background()).SessionsReaped; !reflect.DeepEqual(got, []string{"s-gone"}) {
			t.Fatalf("body %q: an unreadable answer acked the reaped sessions: %v", body, got)
		}
	}
}

// TestFenceWorkerHandsOffAcrossReconnects: a reset racing a worker that is
// just stopping never leaves the new connection without a worker.
func TestFenceWorkerHandsOffAcrossReconnects(t *testing.T) {
	resetConnectivityState(t)
	resetFenceReportForTest(t)
	prevDelays, prevSend := fenceReportRetryDelays, sendOnlineForFenceReport
	fenceReportRetryDelays = []time.Duration{time.Millisecond}
	sendOnlineForFenceReport = func(context.Context, *Config) error {
		markFenceReportApplied()
		return nil
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for fenceWorkerRunning() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		fenceReportRetryDelays, sendOnlineForFenceReport = prevDelays, prevSend
	})
	cfg := &Config{AgentID: "a", CommandSecret: "s"}
	for i := 0; i < 200; i++ {
		resetFenceReport()
		ensureFenceReport(cfg)
		deadline := time.Now().Add(2 * time.Second)
		for !fenceReportApplied() {
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: the new connection was left without a fence worker", i)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}
}

// TestOnlineAnswerFromAnEarlierConnectionDoesNotOpenTheGate: an /online sent
// before a reconnect may answer with fences that predate a Move made during
// the reconnect, so it never applies the new connection's fence report.
func TestOnlineAnswerFromAnEarlierConnectionDoesNotOpenTheGate(t *testing.T) {
	resetConnectivityState(t)
	resetFencedSessions(t)
	resetFenceReportForTest(t)
	withBootReport(t, bootReport{BootID: "boot-gen", SessionsReaped: []string{}, SessionsSurviving: []string{}})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-release
		_, _ = w.Write([]byte(`{"fencedSessionIds":[]}`))
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)
	cfg := &Config{AgentID: "agent-gen", CommandSecret: "secret-gen"}

	done := make(chan error, 1)
	go func() { done <- notifyOnline(context.Background(), cfg) }()
	time.Sleep(200 * time.Millisecond) // the request is in flight
	resetFenceReport()                 // a reconnect starts a new generation
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if fenceReportApplied() {
		t.Fatalf("an answer to a request sent before the reconnect opened the gate")
	}
	// A request sent under the new generation does.
	if err := notifyOnline(context.Background(), cfg); err != nil || !fenceReportApplied() {
		t.Fatalf("a current-generation answer must apply (err %v)", err)
	}
}
