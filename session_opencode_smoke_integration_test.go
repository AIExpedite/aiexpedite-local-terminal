// session_opencode_smoke_integration_test.go — the LEGACY signed `session_start`
// OpenCode maintenance smoke, driven end to end through
// sessionStartArgsForCommand into StartSession against a real stub binary.
//
// This is the reported failure. A deployed publisher signs
// `run --pure --format json <marker prompt>`; before this change the device ran
// exactly those tokens, OpenCode rejected `--pure` during option parsing, and
// the run died before inference — identically before and after a CLI update, so
// no marker ever came back and the outcome collapsed into one bucket.
//
// The contract these cases pin:
//   - the request is RECOGNISED (broadly, so a mutation fails closed) and
//     PROMOTED with the private control token;
//   - the control token is consumed and never reaches a child;
//   - the child argv is the LADDER's, never the wire's — `--pure` is consumed by
//     validation;
//   - the marker prompt arrives on the managed stdin pipe, not on argv;
//   - a malformed RESERVED request fails closed with fixed text and spawns
//     nothing, while anything carrying a NON-reserved token is never maintenance
//     traffic and runs as an ordinary session.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openCodeLegacyRequestFixture is the captured deployed request. Read from
// testdata rather than invented here: a marker this repo minted would prove only
// that we recognise our own grammar.
type openCodeLegacyRequestFixture struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func loadOpenCodeLegacyRequest(t *testing.T) openCodeLegacyRequestFixture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "opencode_legacy_smoke_request.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture openCodeLegacyRequestFixture
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if fixture.Command == "" || len(fixture.Args) == 0 {
		t.Fatalf("fixture is empty: %+v", fixture)
	}
	return fixture
}

// openCodeSessionRun starts one session against the compiled stub and returns
// the argv the child received, what reached its stdin, and StartSession's error.
// The logs are absent when nothing was spawned, which is itself an assertion the
// refusal cases make.
type openCodeSessionRun struct {
	argv    string
	stdin   string
	env     string // the stub's OPENCODE_STUB_ENV_LOG line: which maintenance pins it saw
	spawned bool
	err     error
}

// openCodeSessionEndedSignal returns a channel closed on the session's
// `session_ended` frame, and the publishFn that closes it. A one-shot close
// guarded by a flag, because SessionManager may publish more than one terminal
// frame and closing a closed channel panics.
func openCodeSessionEndedSignal() (<-chan struct{}, PublishFunc) {
	ended := make(chan struct{})
	var once sync.Once
	return ended, func(res resultMsg) {
		if res.Type == "session_ended" {
			once.Do(func() { close(ended) })
		}
	}
}

// runOpenCodeSessionStart starts one session against the compiled stub.
// `stdout`, when given, replaces the stub's default terminal-only frame — the
// usage rows need real `step_finish` events in the stream.
func runOpenCodeSessionStart(t *testing.T, cmd commandMsg, stdout ...string) openCodeSessionRun {
	t.Helper()
	installOpenCodeStub(t)

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	stdinLog := filepath.Join(dir, "stdin.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	envLog := filepath.Join(dir, "env.log")
	t.Setenv("OPENCODE_STUB_ENV_LOG", envLog)
	// The pins must come from the launcher, never from the test's own env.
	for _, pin := range openCodeMaintenanceEnvPins {
		t.Setenv(pin[0], "")
		os.Unsetenv(pin[0])
	}
	// A terminal frame so the session completes promptly rather than waiting out
	// its timeout.
	frames := `{"type":"session.completed"}`
	if len(stdout) > 0 {
		frames = stdout[0]
	}
	t.Setenv("OPENCODE_STUB_STDOUT", frames)

	sm := NewSessionManager(nil)
	id := fmt.Sprintf("opencode-legacy-smoke-%d", time.Now().UnixNano())
	ended, publishFn := openCodeSessionEndedSignal()

	run := openCodeSessionRun{}
	run.err = sm.StartSession(id, cmd.Command, sessionStartArgsForCommand(cmd),
		t.TempDir(), "ws", "uid", 15000, false, publishFn)
	if run.err == nil {
		select {
		case <-ended:
		case <-time.After(20 * time.Second):
			t.Fatal("session never ended")
		}
	}

	if body, err := os.ReadFile(argvLog); err == nil {
		run.argv = strings.TrimSpace(string(body))
		run.spawned = true
	}
	if body, err := os.ReadFile(stdinLog); err == nil {
		run.stdin = strings.TrimSpace(string(body))
	}
	if body, err := os.ReadFile(envLog); err == nil {
		run.env = strings.TrimSpace(string(body))
	}
	return run
}

/* --------------------------------------------------------------------------
   The reported case, and its post-rollout twin
   -------------------------------------------------------------------------- */

func TestOpenCodeLegacySmoke_DeployedPureRequestRunsTheLadderArgv(t *testing.T) {
	fixture := loadOpenCodeLegacyRequest(t)

	// Recognition: promoted, with the private control token leading the argv.
	promoted := sessionStartArgsForCommand(commandMsg{
		Type: "session_start", Command: fixture.Command, Args: fixture.Args,
	})
	if len(promoted) == 0 || promoted[0] != openCodeMaintenanceSmokeControlArg {
		t.Fatalf("the deployed request was not promoted: %q", promoted)
	}
	// Idempotent: an argv that already carries the token is left alone.
	if again := sessionStartArgsForCommand(commandMsg{
		Type: "session_start", Command: fixture.Command, Args: promoted,
	}); strings.Count(strings.Join(again, " "), openCodeMaintenanceSmokeControlArg) != 1 {
		t.Fatalf("promotion is not idempotent: %q", again)
	}

	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: fixture.Command, Args: fixture.Args,
	})
	if run.err != nil {
		t.Fatalf("the deployed request must run, not be refused: %v", run.err)
	}
	if !run.spawned {
		t.Fatal("the deployed request spawned nothing")
	}

	wantArgv := strings.Join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), " ")
	if run.argv != wantArgv {
		t.Fatalf("child argv = %q, want the ladder's %q", run.argv, wantArgv)
	}
	if strings.Contains(run.argv, "--pure") {
		t.Errorf("`--pure` reached the child: %q", run.argv)
	}
	if strings.Contains(run.argv, openCodeMaintenanceSmokeControlArg) {
		t.Errorf("the control token reached the child: %q", run.argv)
	}
	wantPrompt := fixture.Args[len(fixture.Args)-1]
	if strings.Contains(run.argv, wantPrompt) {
		t.Errorf("the marker prompt reached argv: %q", run.argv)
	}
	if run.stdin != wantPrompt {
		t.Fatalf("child stdin = %q, want the marker prompt %q", run.stdin, wantPrompt)
	}
	// A maintenance smoke must not update the CLI it is measuring.
	if run.env != "autoupdate=true title=true" {
		t.Fatalf("maintenance pins on the smoke child = %q, want both set", run.env)
	}
}

func TestOpenCodeLegacySmoke_PostRolloutShapeWithoutPureBehavesIdentically(t *testing.T) {
	// The same request once a publisher drops `--pure` — accepted for the whole
	// rollout window, so the token can go away without a flag day.
	fixture := loadOpenCodeLegacyRequest(t)
	prompt := fixture.Args[len(fixture.Args)-1]
	args := append(append([]string{}, openCodeSmokeWireRequests[1]...), prompt)

	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: "opencode", Args: args,
	})
	if run.err != nil {
		t.Fatalf("the post-rollout shape must run: %v", run.err)
	}
	if want := strings.Join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), " "); run.argv != want {
		t.Fatalf("child argv = %q, want %q", run.argv, want)
	}
	if run.stdin != prompt {
		t.Fatalf("child stdin = %q, want %q", run.stdin, prompt)
	}
}

/* --------------------------------------------------------------------------
   Refusals — reserved vocabulary only, and nothing spawns
   -------------------------------------------------------------------------- */

func TestOpenCodeLegacySmoke_MalformedReservedRequestFailsClosed(t *testing.T) {
	prompt := loadOpenCodeLegacyRequest(t).Args[4]
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"a duplicated token", []string{"run", "run", "--format", "json", prompt}},
		{"a misordered wire", []string{"--format", "json", "run", prompt}},
		{"a missing json operand", []string{"run", "--format", prompt}},
		{"a promoted request with no valid prompt", []string{openCodeMaintenanceSmokeControlArg, "run", "--format", "json"}},
		{"a promoted request with a malformed marker", []string{openCodeMaintenanceSmokeControlArg, "run", "--format", "json", "just do something"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := runOpenCodeSessionStart(t, commandMsg{
				Type: "session_start", Command: "opencode", Args: tc.args,
			})
			if run.err == nil {
				t.Fatalf("%q was accepted; it must fail closed", tc.args)
			}
			if run.err.Error() != errOpenCodeSmokeRequestContract.Error() {
				t.Fatalf("%q produced a non-fixed error %q", tc.args, run.err)
			}
			if strings.Contains(run.err.Error(), prompt) {
				t.Errorf("the refusal echoed the offending token: %v", run.err)
			}
			if run.spawned {
				t.Errorf("a refused request spawned a child: %q", run.argv)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Pass-through — a non-reserved token is never maintenance traffic
   -------------------------------------------------------------------------- */

func TestOpenCodeLegacySmoke_NonReservedTrafficRunsAsAnOrdinarySession(t *testing.T) {
	prompt := loadOpenCodeLegacyRequest(t).Args[4]
	for _, tc := range []struct {
		name      string
		args      []string
		wantArgv  string
		wantStdin string
	}{
		{
			// The reserved sentence beside a caller flag: not maintenance
			// traffic, so it must run — never refuse.
			name:      "the marker sentence beside --model",
			args:      []string{"run", "--format", "json", "--model", openCodeTestModel, prompt},
			wantArgv:  "run --format json --model " + openCodeTestModel,
			wantStdin: prompt,
		},
		{
			name:      "an ordinary prompt-only session_start",
			args:      []string{"fix the failing test"},
			wantArgv:  "run --format json",
			wantStdin: "fix the failing test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := commandMsg{Type: "session_start", Command: "opencode", Args: tc.args}
			if promoted := sessionStartArgsForCommand(cmd); openCodeArgvContains(promoted, openCodeMaintenanceSmokeControlArg) {
				t.Fatalf("%q was promoted; it is not maintenance traffic", tc.args)
			}
			run := runOpenCodeSessionStart(t, cmd)
			if run.err != nil {
				t.Fatalf("an ordinary session must run: %v", run.err)
			}
			if run.argv != tc.wantArgv {
				t.Fatalf("child argv = %q, want %q", run.argv, tc.wantArgv)
			}
			if run.stdin != tc.wantStdin {
				t.Fatalf("child stdin = %q, want %q", run.stdin, tc.wantStdin)
			}
			// Ordinary sessions keep the user's update behaviour.
			if run.env != "autoupdate=unset title=unset" {
				t.Fatalf("an ordinary session carries maintenance pins: %q", run.env)
			}
		})
	}
}

func TestOpenCodeLegacySmoke_PromptPastTheOldArgvCeilingIsAccepted(t *testing.T) {
	// The 24 KiB pre-spawn refusal existed only because the prompt sat on argv.
	// It is gone with the positional, so a long brief must now simply run.
	long := strings.Repeat("a very long review brief. ", 2000)
	if len(long) <= 24*1024 {
		t.Fatalf("the fixture prompt (%d bytes) must exceed the old ceiling", len(long))
	}
	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: "opencode", Args: []string{long},
	})
	if run.err != nil {
		t.Fatalf("a prompt past the old argv ceiling was refused: %v", run.err)
	}
	if run.stdin != strings.TrimSpace(long) {
		t.Fatalf("the long prompt did not arrive intact on stdin (%d bytes delivered)", len(run.stdin))
	}
}

/* --------------------------------------------------------------------------
   The promptless start on the same transport
   --------------------------------------------------------------------------
   Moving the prompt off argv put OpenCode into codex's deferred-stdin flow: a
   session opened with NO prompt keeps its pipe open for the first SendInput,
   which then closes it. `opencode run` reads stdin to EOF before running the
   turn, so if that close is ever gated on codex — the command the mechanism's
   comments used to name exclusively — every promptless OpenCode session hangs on
   a pipe nothing closes, and the hang looks like a slow model rather than a bug.
   The stub reads stdin to EOF too, so its EXIT is the proof.
   ------------------------------------------------------------------------ */

func TestOpenCodeSession_PromptlessStartClosesStdinOnTheFirstSendInput(t *testing.T) {
	installOpenCodeStub(t)

	dir := t.TempDir()
	stdinLog := filepath.Join(dir, "stdin.log")
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"session.completed"}`)

	sm := NewSessionManager(nil)
	id := fmt.Sprintf("opencode-promptless-%d", time.Now().UnixNano())
	ended, publishFn := openCodeSessionEndedSignal()

	// No prompt: only a flag. This is the shape the chat-direct flow opens on
	// model selection, before the user has said anything.
	if err := sm.StartSession(id, "opencode", []string{"--model", openCodeTestModel},
		t.TempDir(), "ws", "uid", 30000, false, publishFn); err != nil {
		t.Fatalf("promptless start failed: %v", err)
	}

	// Through the accessor, not the map: sm.sessions is guarded by sm.mu and the
	// manager's readOutputStream / waitForExit goroutines are live by now, with
	// removeSession writing that map on exit. An unguarded read here is a data
	// race the detector would eventually surface as an intermittent CI failure.
	session := sm.GetSession(id)
	if session == nil {
		t.Fatal("session was not registered")
	}
	session.mu.Lock()
	deferred := session.deferredStdinClose
	session.mu.Unlock()
	if !deferred {
		t.Fatal("a promptless opencode start must defer its stdin close, not close at start")
	}

	// The child must still be waiting: nothing has been delivered yet.
	select {
	case <-ended:
		t.Fatal("the session ended before any prompt was sent — stdin was closed at start")
	case <-time.After(300 * time.Millisecond):
	}

	if err := sm.SendInput(id, "implement the feature"); err != nil {
		t.Fatalf("SendInput failed: %v", err)
	}

	// Only reachable if SendInput closed the pipe: the stub reads stdin to EOF.
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the session never ended — the first SendInput did not close stdin")
	}

	delivered, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("the prompt never reached the child: %v", err)
	}
	if got := strings.TrimSpace(string(delivered)); got != "implement the feature" {
		t.Fatalf("child stdin = %q, want the SendInput prompt", got)
	}

	// A second SendInput must not double-close the already-closed pipe.
	session.mu.Lock()
	stillDeferred := session.deferredStdinClose
	session.mu.Unlock()
	if stillDeferred {
		t.Error("deferredStdinClose was not cleared, so a second SendInput would double-close")
	}
}

/* --------------------------------------------------------------------------
   Pipe-session usage capture
   -------------------------------------------------------------------------- */

// openCodePipeUsageFrames is a two-step turn's worth of `step_finish` events
// plus the terminal frame, spelled for the stub's "\n"-escaped stdout env.
func openCodePipeUsageFrames(nowMs int64) string {
	step := func(reason string, in, out int) string {
		return fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":"ses_pipe",`+
			`"part":{"messageID":"msg_pipe","type":"step-finish","reason":%q,"cost":0.001,`+
			`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
			nowMs, reason, in, out)
	}
	return strings.Join([]string{
		step("tool-calls", 10, 2),
		step("stop", 20, 4),
		`{"type":"session.completed","timestamp":` + fmt.Sprintf("%d", nowMs) + `}`,
	}, "\\n") + "\\n"
}

// A clean pipe turn is COVERED by its own stream: the card gets a number and
// nothing owes a reconcile.
func TestOpenCodeSession_ACleanPipeTurnIsCoveredByItsStream(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	// The settle must not reach a CLI: a covered run owes nothing, and a stub
	// that fatals proves it.
	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: "opencode", Args: []string{"implement the feature"},
	}, openCodePipeUsageFrames(now.UnixMilli()))
	if run.err != nil || !run.spawned {
		t.Fatalf("the session did not run: err=%v spawned=%v", run.err, run.spawned)
	}
	if !openCodeUsageRefreshWaitFor(5 * time.Second) {
		t.Fatal("the usage settle never went idle")
	}

	// 10+2 then 20+4, one messageID.
	tokens, cost, rows := todayTotals(t, now)
	if tokens != 36 || rows != 1 || cost != 2000 {
		t.Fatalf("tokens=%d cost=%d rows=%d, want 36/2000/1", tokens, cost, rows)
	}
	var freshness openCodeUsageFreshness
	readJSONFile(openCodeUsageFreshnessPath(), &freshness)
	if freshness.owed() {
		t.Fatalf("freshness = %+v, want nothing owed for a covered turn", freshness)
	}
}

// A pipe turn whose stream never announced the turn's end still spent tokens we
// could not read, so it owes one bounded reconcile.
func TestOpenCodeSession_APipeTurnWithNoTerminalFrameOwesAReconcile(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	// A reconcile that answers "nothing changed", so the debt is observable
	// before it is paid rather than the pass reaching a real install.
	(&openCodeCLIStub{sessions: "[]"}).install(t)
	// Drop the terminal frame: the turn's output stops mid-stream.
	frames := strings.Split(openCodePipeUsageFrames(now.UnixMilli()), "\\n")[0] + "\\n"

	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: "opencode", Args: []string{"implement the feature"},
	}, frames)
	if run.err != nil || !run.spawned {
		t.Fatalf("the session did not run: err=%v spawned=%v", run.err, run.spawned)
	}
	openCodeUsageRefreshWaitFor(5 * time.Second)

	// What the stream DID report still counts — the reconcile is for the rest.
	if tokens, _, _ := todayTotals(t, now); tokens != 12 {
		t.Fatalf("tokens = %d, want the 12 the stream reported", tokens)
	}
	ledger := readOpenCodeUsageLedger()
	if ledger.LastPassOutcome == "" {
		t.Fatal("the owed reconcile never ran")
	}
}

// A session opened and closed WITHOUT a prompt ran no turn, so it must owe
// nothing: the chat-direct flow opens an opencode session on model selection,
// and settling every abandoned one as an owed reconcile would spend a bounded
// pass — a `session list` plus up to three `export` calls — on a run that never
// happened, once per abandoned chat.
func TestOpenCodeSession_APromptlessSessionOwesNothing(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	// Any reconcile would be a bug: the fixture's stub fatals if one spawns.

	run := runOpenCodeSessionStart(t, commandMsg{
		// No prompt token, so buildOpenCodeInteractiveArgs yields a nil
		// stdinPrompt and the child holds stdin open for a first message that
		// never comes.
		Type: "session_start", Command: "opencode", Args: []string{"--model", "anthropic/claude-sonnet-4-5"},
	})
	if run.err != nil || !run.spawned {
		t.Fatalf("the session did not run: err=%v spawned=%v", run.err, run.spawned)
	}
	if !openCodeUsageRefreshWaitFor(5 * time.Second) {
		t.Fatal("the usage settle never went idle")
	}

	var freshness openCodeUsageFreshness
	readJSONFile(openCodeUsageFreshnessPath(), &freshness)
	if freshness.owed() {
		t.Fatalf("freshness = %+v, want nothing owed for a session that ran no turn", freshness)
	}
	// And no floor is left behind for the next process to adopt as an
	// interrupted run.
	if freshness.RunFloorMs != 0 {
		t.Fatalf("runFloorMs = %d, want the withdrawn run to leave none", freshness.RunFloorMs)
	}
	if _, _, rows := todayTotals(t, now); rows != 0 {
		t.Fatalf("rows = %d, want none", rows)
	}
}

// A shell-wrapped launch carries its prompt in the script and runs at once, so
// no stdin delivery is ever recorded for it. It must still settle as a run —
// here an uncovered one, since its stream is not tapped — rather than being
// withdrawn as a promptless session that ran no turn.
func TestOpenCodeSession_AShellWrappedRunOwesAReconcile(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)
	// A reconcile that answers "nothing changed", so the owed pass is
	// observable rather than reaching a real install.
	(&openCodeCLIStub{sessions: "[]"}).install(t)

	run := runOpenCodeSessionStart(t, commandMsg{
		Type: "session_start", Command: "sh",
		Args: []string{"-c", "opencode run 'implement the feature' </dev/null"},
	}, openCodePipeUsageFrames(now.UnixMilli()))
	if run.err != nil || !run.spawned {
		t.Fatalf("the session did not run: err=%v spawned=%v", run.err, run.spawned)
	}
	if !openCodeUsageRefreshWaitFor(5 * time.Second) {
		t.Fatal("the usage settle never went idle")
	}

	if ledger := readOpenCodeUsageLedger(); ledger.LastPassOutcome == "" {
		t.Fatal("the wrapped run was withdrawn: its owed reconcile never ran")
	}
}
