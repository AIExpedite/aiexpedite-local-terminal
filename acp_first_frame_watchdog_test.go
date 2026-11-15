package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// The first-frame watchdog's clock starts at the first REQUEST written to the
// child, never at the spawn. Found live in dev on 2026-11-15 (F7): the chat
// panel starts Grok when it is picked and runs the handshake with the first
// typed turn, so a first message more than 45 s after the pick met a child
// the watchdog had already killed, with an error that blamed the sign-in.
// Exercised on the OpenCode profile's mock CLI; the watchdog is the family's,
// so the same code runs for Grok, Antigravity and Muse Code.

// testFirstFrameWindow is the shortened watchdog window these tests run with.
const testFirstFrameWindow = 750 * time.Millisecond

func startWatchdogMock(t *testing.T, stall string) (*ACPManager, string, *frameLog) {
	t.Helper()
	if stall != "" {
		t.Setenv(mockACPStallEnv, stall)
	}
	// The window is shortened BEFORE the start, so a watchdog armed at the
	// spawn would run with it too.
	return startMockACPDriverWith(t, openCodeACPProfile, "2.2.7", func(m *ACPManager) {
		m.firstFrameTimeout = testFirstFrameWindow
	})
}

const watchdogInitialize = `{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{"protocolVersion":1}}`

// assertStillRunning: nothing failed the session, and it is still up.
func assertStillRunning(t *testing.T, m *ACPManager, id string, log *frameLog, when string) {
	t.Helper()
	if errs := log.ofType("opencode_acp_error"); len(errs) != 0 {
		t.Fatalf("%s: the session was failed: %+v", when, errs)
	}
	if ended := log.ofType("opencode_acp_ended"); len(ended) != 0 {
		t.Fatalf("%s: the session ended: %+v", when, ended)
	}
	if s := m.Get(id); s == nil || s.Status() != "running" {
		t.Fatalf("%s: the session is not running", when)
	}
}

// The bug: idle well past the window before the first message, then the
// handshake. Nothing is killed, before or after.
func TestACPFirstFrameWatchdogWaitsForTheFirstMessage(t *testing.T) {
	m, id, log := startWatchdogMock(t, "")

	time.Sleep(3 * testFirstFrameWindow)
	assertStillRunning(t, m, id, log, "idle before the first message")

	if err := m.Send(id, watchdogInitialize); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "opencode_acp_message", `"relay-acp-init"`)
	time.Sleep(2 * testFirstFrameWindow)
	assertStillRunning(t, m, id, log, "after the handshake")
}

// A real handshake timeout still fails the session, with the truth: which
// request went unanswered and for how long, and no sign-in guess. The clock
// starts at the request, so the idle stretch before it costs nothing.
func TestACPFirstFrameWatchdogFailsAnUnansweredHandshakeHonestly(t *testing.T) {
	m, id, log := startWatchdogMock(t, mockACPStallSilent)

	time.Sleep(2 * testFirstFrameWindow)
	assertStillRunning(t, m, id, log, "idle before the first message")

	sent := time.Now()
	if err := m.Send(id, watchdogInitialize); err != nil {
		t.Fatal(err)
	}
	got := waitForFrame(t, log, "opencode_acp_error", "did not answer")
	if waited := time.Since(sent); waited < testFirstFrameWindow {
		t.Fatalf("failed %v after the request, inside its %v window", waited, testFirstFrameWindow)
	}
	want := "opencode did not answer `initialize` within 750ms: it has written no protocol frame since it started. " +
		"Session terminated. To see what it is waiting for, run `opencode` in a terminal on this computer."
	if got.Output != want || got.Status != "error" || got.SessionID != id {
		t.Fatalf("stall error = %+v\nwant output %q", got, want)
	}
	waitForFrame(t, log, "opencode_acp_ended", "opencode acp ended")
	if n := len(log.ofType("opencode_acp_error")); n != 1 {
		t.Fatalf("%d error frames, want 1", n)
	}
}

// When the CLI said something on stderr, its own words are the evidence, so
// the error quotes them.
func TestACPFirstFrameWatchdogQuotesTheCLIsStderr(t *testing.T) {
	const said = "waiting for the browser to finish the sign-in"
	m, id, log := startWatchdogMock(t, said)

	if err := m.Send(id, watchdogInitialize); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "opencode_acp_stderr", said)
	got := waitForFrame(t, log, "opencode_acp_error", "did not answer")
	if !strings.Contains(got.Output, `Its last line on stderr: "`+said+`".`) {
		t.Fatalf("stall error %q does not quote the CLI's stderr", got.Output)
	}
}

// A notification asks for no answer, so it does not start the clock.
func TestACPFirstFrameWatchdogIgnoresANotification(t *testing.T) {
	m, id, log := startWatchdogMock(t, "")

	if err := m.Send(id, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"ses_1"}}`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * testFirstFrameWindow)
	assertStillRunning(t, m, id, log, "after a notification")

	if err := m.Send(id, watchdogInitialize); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "opencode_acp_message", `"relay-acp-init"`)
}

// Only a request arms it, once, and only on a session that has a publisher
// (a hand-built test session has none, and must never get a watchdog that
// would publish into nothing).
func TestACPFirstFrameWatchdogArmsOnTheFirstRequestOnly(t *testing.T) {
	m := newACPManager(nil, openCodeACPProfile)
	m.firstFrameTimeout = time.Hour
	session := func(publish PublishFunc) *ACPSession {
		s := &ACPSession{ID: "arm", status: "running", done: make(chan struct{}), firstFrame: make(chan struct{}), publish: publish}
		close(s.done) // the watchdog goroutine, once started, returns at once
		return s
	}
	publish := func(resultMsg) {}

	unpublished := session(nil)
	m.armFirstFrameWatchdog(unpublished, watchdogInitialize)
	if unpublished.watchdogArmed.Load() {
		t.Error("a session with no publisher armed the watchdog")
	}

	s := session(publish)
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"session/cancel","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`{"jsonrpc":"2.0","id":null,"method":"initialize"}`,
		`not json`,
	} {
		m.armFirstFrameWatchdog(s, frame)
		if s.watchdogArmed.Load() {
			t.Fatalf("%s armed the watchdog", frame)
		}
	}
	m.armFirstFrameWatchdog(s, watchdogInitialize)
	if !s.watchdogArmed.Load() {
		t.Fatal("the first request did not arm the watchdog")
	}
}

// sharedResourcesCredentialText is shared-resources cliTurnCompletion.util.js
// CREDENTIAL_TEXT (5.8.x). voice-service runs the device's error text through
// it and says a sign-in on a match, so the device's own words must never match
// it: only the CLI's quoted stderr may. A copy, used only to prove a negative.
var sharedResourcesCredentialText = regexp.MustCompile(`(?i)(invalid api key|please run /login|run /login|not logged in|logged out|log ?in required|sign ?in required|\b(?:log|sign) ?in again\b|authentication[ _](?:failed|required|error)|oauth token (has )?expired|token (has )?expired)`)

func TestACPFirstAnswerStallNeverClaimsASignIn(t *testing.T) {
	for _, p := range []*acpAgentProfile{grokACPProfile, openCodeACPProfile, antigravityACPProfile, museCodeMSPProfile} {
		got := acpFirstAnswerStall(p.cliName, "initialize", acpFamilyFirstFrameTimeout, nil)
		if sharedResourcesCredentialText.MatchString(got) || strings.Contains(strings.ToLower(got), "sign") {
			t.Errorf("%s: %q reads as a sign-in", p.cliAgentID, got)
		}
		if !strings.HasPrefix(got, p.cliName+" did not answer `initialize` within 45s") {
			t.Errorf("%s: %q", p.cliAgentID, got)
		}
	}
	grok := acpFirstAnswerStall("grok", "initialize", 45*time.Second, nil)
	if grok != "grok did not answer `initialize` within 45s: it has written no protocol frame since it started. "+
		"Session terminated. To see what it is waiting for, run `grok` in a terminal on this computer." {
		t.Errorf("grok stall = %q", grok)
	}
	said := "Error: not logged in"
	if quoted := acpFirstAnswerStall("grok", "initialize", 45*time.Second, &said); !sharedResourcesCredentialText.MatchString(quoted) {
		t.Errorf("the CLI's own sign-in words were lost: %q", quoted)
	}
	empty := ""
	if acpFirstAnswerStall("grok", "initialize", 45*time.Second, &empty) != grok {
		t.Error("an empty stderr line was quoted")
	}
}
