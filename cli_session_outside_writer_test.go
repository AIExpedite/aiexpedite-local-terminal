package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	owTestThread  = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	owTestOther   = "0199a1b2-c3d4-7e5f-8a9b-ffffffffffff"
	owTestSession = "aix-session-1"
)

// pinCliSessionProcesses stands in a process table for the rest of the test
// (and forgets the shared scan before and after).
func pinCliSessionProcesses(t *testing.T, procs func() []cliSessionProcess) {
	t.Helper()
	cliSessionProcessScannerMu.Lock()
	prev := cliSessionProcessScanner
	cliSessionProcessScanner = func(context.Context) ([]cliSessionProcess, bool) { return procs(), true }
	cliSessionProcessScannerMu.Unlock()
	resetCliSessionProcessSnapshot()
	t.Cleanup(func() {
		cliSessionProcessScannerMu.Lock()
		cliSessionProcessScanner = prev
		cliSessionProcessScannerMu.Unlock()
		resetCliSessionProcessSnapshot()
	})
}

// fastOutsideWriterPolls makes every watcher started in the test poll often.
func fastOutsideWriterPolls(t *testing.T) {
	t.Helper()
	cliOutsideWriterPollNanos.Store(int64(20 * time.Millisecond))
	t.Cleanup(func() { cliOutsideWriterPollNanos.Store(0) })
}

// outsideWriterSink records published frames.
type outsideWriterSink struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (s *outsideWriterSink) publish(r resultMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, r)
}

func (s *outsideWriterSink) states(ownerID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, f := range s.frames {
		if f.SessionID == ownerID && f.OutsideWriter != nil {
			out = append(out, f.OutsideWriter.State)
		}
	}
	return out
}

func (s *outsideWriterSink) last(ownerID string) *resultMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.frames) - 1; i >= 0; i-- {
		if s.frames[i].SessionID == ownerID {
			f := s.frames[i]
			return &f
		}
	}
	return nil
}

// waitForWriterState polls cond for up to d.
func waitForWriterState(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func watchRunning(key string) bool {
	cliOutsideWriterWatches.Lock()
	defer cliOutsideWriterWatches.Unlock()
	_, ok := cliOutsideWriterWatches.byKey[key]
	return ok
}

// For an engine without a status registry: a live process naming the session
// holds it open (idle), the store written within writerQuietMs by anything
// but AI Expedite's own child is busy, and neither is gone. This agent's own
// descendants and processes naming another session never count.
func TestOutsideWritersFromProcessesAndStoreWrites(t *testing.T) {
	now := time.Now()
	holder := cliSessionProcess{PID: 41, Name: "codex", Args: []string{"codex", "resume", owTestThread}}
	procs := []cliSessionProcess{
		holder,
		{PID: 42, Name: "codex", Args: []string{"codex", "resume", owTestOther}},
		{PID: 43, Name: "codex", Args: []string{"codex", "resume", owTestThread}, Own: true},
		{PID: 44, Name: "grok", Args: []string{"grok", "--resume", owTestThread}},
	}
	var current []cliSessionProcess
	pinCliSessionProcesses(t, func() []cliSessionProcess { return current })
	at := func(ago time.Duration) func() int64 { return func() int64 { return now.Add(-ago).UnixMilli() } }
	ownAt := func(ago time.Duration) func(time.Time) time.Time {
		return func(time.Time) time.Time { return now.Add(-ago) }
	}
	for _, tc := range []struct {
		name  string
		procs []cliSessionProcess
		clock func() int64
		own   func(time.Time) time.Time
		want  string
		pid   int
	}{
		{"open elsewhere, quiet", procs, at(30 * time.Second), nil, claudeOutsideWriterIdle, 41},
		{"open elsewhere, writing", procs, at(time.Second), nil, claudeOutsideWriterBusy, 41},
		{"no process, writing (an IDE's app-server)", nil, at(time.Second), nil, claudeOutsideWriterBusy, 0},
		{"no process, quiet", nil, at(30 * time.Second), nil, claudeOutsideWriterGone, 0},
		{"only AI Expedite's own child wrote", nil, at(time.Second), ownAt(2 * time.Second), claudeOutsideWriterGone, 0},
		{"someone wrote after AI Expedite's child went quiet", nil, at(time.Second), ownAt(8 * time.Second), claudeOutsideWriterBusy, 0},
		{"no store to read", procs, nil, nil, claudeOutsideWriterIdle, 41},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current = tc.procs
			resetCliSessionProcessSnapshot()
			writers := cliSessionOutsideWriters(codexWriterEvidence, owTestThread, tc.clock, tc.own, now)
			state, _, _ := claudeOutsideWriterState(writers)
			if state != tc.want {
				t.Fatalf("writers %+v read %s, want %s", writers, state, tc.want)
			}
			if tc.want != claudeOutsideWriterGone && (len(writers) != 1 || writers[0].PID != tc.pid) {
				t.Fatalf("writers = %+v, want one writer pid %d", writers, tc.pid)
			}
		})
	}
}

func TestCliSessionArgvNamesTheSession(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"codex", "resume", owTestThread}, true},
		{[]string{"agy", "--conversation=" + owTestThread}, true},
		{[]string{owTestThread}, false}, // argv[0] is the program
		{[]string{"codex", "resume", owTestThread + "x"}, false},
		{[]string{"codex", "--prompt", "about " + owTestThread}, false},
	} {
		if got := cliSessionArgvNames(tc.argv, owTestThread); got != tc.want {
			t.Errorf("cliSessionArgvNames(%v) = %v, want %v", tc.argv, got, tc.want)
		}
	}
	if cliSessionProcessName("/opt/homebrew/bin/Codex.EXE") != "codex" || !museCodeWriterEvidence.isAgentProcess("muse-bin-1.4.20-r8619.1") {
		t.Fatal("process names are compared lower-case, without .exe, Muse's build by prefix")
	}
}

// The watcher of a Grok session AI Expedite loaded reports every change the
// other program makes — open, writing, quiet, closed — in the one frame
// shape, naming this agent and AI Expedite's own session.
func TestOutsideWriterWatcherPublishesAnEnginesChanges(t *testing.T) {
	fastOutsideWriterPolls(t)
	home := t.TempDir()
	t.Setenv("GROK_HOME", home)
	folder := filepath.Join(home, "sessions", grokSessionCwdDirName("/w/app"), owTestThread)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	updates := filepath.Join(folder, "updates.jsonl")
	if err := os.WriteFile(updates, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setWritten := func(ago time.Duration) {
		at := time.Now().Add(-ago)
		for _, f := range grokSessionFiles {
			_ = os.Chtimes(filepath.Join(folder, f), at, at)
		}
	}
	setWritten(time.Minute)
	var mu sync.Mutex
	holding := true
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		mu.Lock()
		defer mu.Unlock()
		if !holding {
			return nil
		}
		return []cliSessionProcess{{PID: 77, Name: "grok", Args: []string{"grok", "--resume", owTestThread}}}
	})

	sink := &outsideWriterSink{}
	stop := make(chan struct{})
	defer close(stop)
	owner := cliOutsideWriterOwner{ID: owTestSession + "-grok", WorkspaceID: "ws1", UID: "u1"}
	if !startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine: cliSessionEngineGrok, sessionID: owTestThread, owner: owner, agentID: "agent-9", stop: stop,
	}, sink.publish) {
		t.Fatal("watcher not started")
	}
	waitForWriterState(t, 3*time.Second, "idle", func() bool { return len(sink.states(owner.ID)) == 1 })
	setWritten(0)
	waitForWriterState(t, 3*time.Second, "busy", func() bool { return len(sink.states(owner.ID)) == 2 })
	setWritten(time.Minute)
	waitForWriterState(t, 3*time.Second, "idle again", func() bool { return len(sink.states(owner.ID)) == 3 })
	mu.Lock()
	holding = false
	mu.Unlock()
	resetCliSessionProcessSnapshot()
	waitForWriterState(t, 3*time.Second, "gone", func() bool { return len(sink.states(owner.ID)) == 4 })

	want := []string{claudeOutsideWriterIdle, claudeOutsideWriterBusy, claudeOutsideWriterIdle, claudeOutsideWriterGone}
	if got := sink.states(owner.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	f := sink.last(owner.ID)
	if f.Type != claudeOutsideWriterResultType || f.AgentID != "agent-9" || f.UID != "u1" || f.WorkspaceID != "ws1" || f.ID != owner.ID || f.BootID != agentBootID {
		t.Fatalf("frame = %+v, want it to name this agent and AI Expedite's session", f)
	}
}

// Every engine's frame is the one terminal-service already reads: the
// golden fixture's shape, byte for byte, whoever's session it reports.
func TestOutsideWriterFrameIsTheSameForEveryEngine(t *testing.T) {
	fixture := readOutsideWriterFixture(t)
	frame := cliSessionOutsideWriterFrame(cliOutsideWriterOwner{ID: "ts1", WorkspaceID: "ws1", UID: "u1"}, "agent1", cliSessionOutsideWriter{
		State: claudeOutsideWriterBusy, Entrypoint: claudeEntrypointVSCode, Editor: cliSessionSourceVSCode, At: 1790000000000,
	}, 1790000000123)
	raw, _ := json.Marshal(frame)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	got["version"], got["bootId"] = fixture.Frame["version"], fixture.Frame["bootId"]
	if !reflect.DeepEqual(got, fixture.Frame) {
		t.Fatalf("frame drifted from the fixture: %s", raw)
	}
}

// A frame published while the Pub/Sub link was down — the `idle` that would
// release a held message — is never lost for good: a new connection gets
// every watcher's current state again, and every later change.
func TestOutsideWriterReSendsTheCurrentStateOnReconnect(t *testing.T) {
	fastOutsideWriterPolls(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	var mu sync.Mutex
	holding := true
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		mu.Lock()
		defer mu.Unlock()
		if !holding {
			return nil
		}
		return []cliSessionProcess{{PID: 88, Name: "codex", Args: []string{"codex", "resume", owTestThread}}}
	})
	oldLink := &outsideWriterSink{}
	stop := make(chan struct{})
	defer close(stop)
	owner := cliOutsideWriterOwner{ID: owTestSession + "-reconnect", WorkspaceID: "ws1", UID: "u1"}
	startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine: cliSessionEngineCodex, sessionID: owTestThread, owner: owner, agentID: "agent-1", stop: stop,
	}, oldLink.publish)
	// A second session nobody else holds: it reports nothing, so a reconnect
	// has nothing of it to repeat.
	quiet := cliOutsideWriterOwner{ID: owTestSession + "-alone", WorkspaceID: "ws1", UID: "u1"}
	startCliSessionOutsideWriterWatcher(cliOutsideWriterTarget{
		engine: cliSessionEngineCodex, sessionID: owTestOther, owner: quiet, agentID: "agent-1", stop: stop,
	}, oldLink.publish)
	waitForWriterState(t, 3*time.Second, "idle on the old link", func() bool { return len(oldLink.states(owner.ID)) == 1 })
	idle := oldLink.last(owner.ID)

	newLink := &outsideWriterSink{}
	if n := resendCliSessionOutsideWriterStates(newLink.publish); n < 1 {
		t.Fatalf("re-sent %d frames, want the running watcher's", n)
	}
	again := newLink.last(owner.ID)
	if again == nil || again.OutsideWriter.State != claudeOutsideWriterIdle || again.OutsideWriter.At != idle.OutsideWriter.At {
		t.Fatalf("re-sent %+v, want the same idle report (same `at`) on the new link", again)
	}
	if newLink.last(quiet.ID) != nil {
		t.Fatal("a watcher that never reported anything was re-sent")
	}

	mu.Lock()
	holding = false
	mu.Unlock()
	resetCliSessionProcessSnapshot()
	waitForWriterState(t, 3*time.Second, "gone on the new link", func() bool {
		s := newLink.states(owner.ID)
		return len(s) == 2 && s[1] == claudeOutsideWriterGone
	})
	if got := oldLink.states(owner.ID); len(got) != 1 {
		t.Fatalf("the closed link got %v after the reconnect", got)
	}
}

// fakeTurnState is a one-shot manager's view of one session.
type fakeTurnState struct {
	mu             sync.Mutex
	alive, running bool
	lastTurnEnded  time.Time
}

// read is the session's turn state, as a bound oneShotSessionTurnState.
func (f *fakeTurnState) read() (bool, bool, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive, f.running, f.lastTurnEnded
}

func (f *fakeTurnState) TurnStateOf(string) oneShotSessionTurnState { return f.read }

// A turn of AI Expedite's own that starts and ends between two watcher polls
// is still its own: when it ran comes from the session, never from what a
// poll saw, so the store write it made reads as nobody else's — while a
// write past that turn and its flush slack is another program's.
func TestOneShotOwnTurnBetweenPollsIsNeverAnOutsideWriter(t *testing.T) {
	now := time.Now()
	ended := now.Add(-2 * time.Second)
	state := &fakeTurnState{alive: true, lastTurnEnded: ended}
	own := oneShotOwnActiveAt(state.read)
	if cliSessionStoreWrittenByOther(ended.Add(-time.Second).UnixMilli(), now, own) {
		t.Fatal("the store write of a turn no poll saw running read as another writer's")
	}
	later := ended.Add(cliSessionOwnWriteSlack + time.Second)
	if !cliSessionStoreWrittenByOther(later.UnixMilli(), later.Add(time.Second), own) {
		t.Fatal("a write after AI Expedite's turn and its flush slack did not read as another writer's")
	}
	state.mu.Lock()
	state.running = true
	state.mu.Unlock()
	if cliSessionStoreWrittenByOther(later.UnixMilli(), later.Add(time.Second), own) {
		t.Fatal("a write while a turn of AI Expedite's own runs read as another writer's")
	}
	if !cliSessionStoreWrittenByOther(now.Add(-time.Second).UnixMilli(), now, oneShotOwnActiveAt((&fakeTurnState{alive: true}).read)) {
		t.Fatal("before any turn of its own, a fresh store write must be another writer's")
	}
}

// The watcher of a resumed one-shot session: the store write of a turn of AI
// Expedite's own that ran entirely between two polls publishes nothing (no
// `busy` holding the next message), while a write another program makes once
// that turn is long over reads busy.
func TestOneShotWatcherNeverReportsItsOwnUnsampledTurn(t *testing.T) {
	fastOutsideWriterPolls(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	root := t.TempDir()
	t.Setenv("OPENCODE_DATA", root)
	const conversation = "ses_0123456789abcdefABCDEF"
	writeOpenCodeLegacySession(t, root, conversation, "Resumed", "/w/app", time.Now().Add(-time.Second))
	state := &fakeTurnState{alive: true, lastTurnEnded: time.Now()}
	sink := &outsideWriterSink{}
	cmd := commandMsg{SessionID: owTestSession + "-unsampled", WorkspaceID: "ws1", UID: "u1", AgentID: "agent-1", ConversationID: conversation}
	if !watchOneShotResumedSession(cliSessionEngineOpenCode, cmd, state, sink.publish) {
		t.Fatal("watcher not started")
	}
	key := cliSessionEngineOpenCode + "|" + cmd.SessionID + "|" + conversation
	t.Cleanup(func() {
		state.mu.Lock()
		state.alive = false
		state.mu.Unlock()
		waitForWriterState(t, 3*time.Second, "the watcher to end", func() bool { return !watchRunning(key) })
	})
	time.Sleep(10 * 20 * time.Millisecond) // ten polls
	if got := sink.states(cmd.SessionID); len(got) != 0 {
		t.Fatalf("states = %v, want nothing: the only write was AI Expedite's own turn's", got)
	}
	state.mu.Lock()
	state.lastTurnEnded = time.Now().Add(-8 * time.Second)
	state.mu.Unlock()
	writeOpenCodeLegacySession(t, root, conversation, "Resumed", "/w/app", time.Now())
	waitForWriterState(t, 3*time.Second, "busy", func() bool {
		s := sink.states(cmd.SessionID)
		return len(s) == 1 && s[0] == claudeOutsideWriterBusy
	})
}

// Every one-shot manager records a turn's end as the turn itself ends — its
// child has exited, and before the session reads idle — so after a real turn
// (a stub CLI) that no watcher poll ever saw running, the session says when
// it ended, and the last write its child made just before exiting is AI
// Expedite's own.
func TestOneShotTurnsRecordWhenTheyEnded(t *testing.T) {
	check := func(t *testing.T, mgr oneShotTurnState, id, doneMark string, send func() error) {
		t.Helper()
		state := mgr.TurnStateOf(id)
		if state == nil {
			t.Fatalf("no session %s to read", id)
		}
		if _, _, ended := state(); !ended.IsZero() {
			t.Fatalf("a session that never ran a turn reports one ended at %v", ended)
		}
		before := time.Now()
		if err := send(); err != nil {
			t.Fatalf("turn: %v", err)
		}
		after := time.Now()
		alive, running, ended := state()
		if !alive || running {
			t.Fatalf("after the turn: alive=%v running=%v, want an idle live session", alive, running)
		}
		if ended.Before(before.Truncate(time.Millisecond)) || ended.After(after) {
			t.Fatalf("turn ended at %v, want between %v and %v", ended, before, after)
		}
		fi, err := os.Stat(doneMark)
		if err != nil {
			t.Fatalf("the stub's last write: %v", err)
		}
		if cliSessionStoreWrittenByOther(fi.ModTime().UnixMilli(), time.Now(), oneShotOwnActiveAt(state)) {
			t.Fatal("the last write of AI Expedite's own turn read as another writer's")
		}
	}
	t.Run("opencode", func(t *testing.T) {
		installOpenCodeStub(t)
		t.Setenv("OPENCODE_DATA", t.TempDir())
		done := filepath.Join(t.TempDir(), "done.log")
		t.Setenv("OPENCODE_STUB_DONE_LOG", done)
		t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"session.created","sessionID":"ses_turnended"}\n{"type":"text","text":"ok"}\n`)
		m := NewOpenCodeNativeManager()
		injectOpenCodeSession(t, m, "sess-turn-ended", t.TempDir())
		check(t, m, "sess-turn-ended", done, func() error {
			return m.Send("sess-turn-ended", "hello", func(resultMsg) {}, time.Minute)
		})
	})
	t.Run("musecode", func(t *testing.T) {
		m := installMuseCodeStub(t)
		done := filepath.Join(t.TempDir(), "done.log")
		t.Setenv("OPENCODE_STUB_DONE_LOG", done)
		t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameUserInput, museFrameDeltaHello, museFrameDeltaWorld, museFrameCompleted))
		startMuse(t, m, "s-turn-ended", "")
		check(t, m, "s-turn-ended", done, func() error {
			return m.Send("s-turn-ended", "say hello", func(resultMsg) {}, time.Minute)
		})
	})
	t.Run("antigravity", func(t *testing.T) {
		done := filepath.Join(t.TempDir(), "done.log")
		installFakeAgy(t, initEventScript()+echoLines(successResultEvent)+"touch '"+done+"'\n")
		m := NewAntigravityNativeManager(nil)
		s := injectAntigravitySession(t, m, "s-turn-ended", "")
		var fl agyFrameLog
		check(t, m, s.ID, done, func() error { return m.Send(s.ID, "hello", fl.publish, 10*time.Second) })
	})
}

// One watcher per (engine, AI Expedite session, session): a redelivered
// resume starts none, and a one-shot session's watcher ends with it, without
// a frame (its end says nothing about the other writer).
func TestOutsideWriterStartsOnceAndEndsWithItsSession(t *testing.T) {
	fastOutsideWriterPolls(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{{PID: 5, Name: "opencode", Args: []string{"opencode", "--session", "ses_0123456789abcdefABCDEF"}}}
	})
	t.Setenv("OPENCODE_DATA", t.TempDir())
	sink := &outsideWriterSink{}
	state := &fakeTurnState{alive: true}
	cmd := commandMsg{SessionID: owTestSession + "-oneshot", WorkspaceID: "ws1", UID: "u1", AgentID: "agent-1", ConversationID: "ses_0123456789abcdefABCDEF"}
	if !watchOneShotResumedSession(cliSessionEngineOpenCode, cmd, state, sink.publish) {
		t.Fatal("watcher not started")
	}
	if watchOneShotResumedSession(cliSessionEngineOpenCode, cmd, state, sink.publish) {
		t.Fatal("a second watcher started for the same session")
	}
	key := cliSessionEngineOpenCode + "|" + cmd.SessionID + "|" + cmd.ConversationID
	waitForWriterState(t, 3*time.Second, "idle", func() bool { return len(sink.states(cmd.SessionID)) == 1 })
	state.mu.Lock()
	state.alive = false
	state.mu.Unlock()
	waitForWriterState(t, 3*time.Second, "the watcher to end", func() bool { return !watchRunning(key) })
	if got := sink.states(cmd.SessionID); len(got) != 1 {
		t.Fatalf("states = %v, want nothing published for the session's own end", got)
	}
	if watchOneShotResumedSession(cliSessionEngineOpenCode, commandMsg{SessionID: "s"}, state, sink.publish) {
		t.Fatal("a start that resumes nothing started a watcher")
	}
}

// A one-shot watcher is bound to the session its START registered, never to
// that session's id, which a manager reuses once the session ends. A
// replacement that continues the same conversation gets its own watcher at
// once (the ended session's never stands in for it), and once a replacement
// continues another conversation, the watcher of the first one unwinds
// instead of reporting that conversation's writer against the replacement.
func TestOneShotWatcherIsBoundToItsSessionNotItsID(t *testing.T) {
	fastOutsideWriterPolls(t)
	const convA, convB = "0199b000-0000-7000-8000-0000000000a1", "0199b000-0000-7000-8000-0000000000b2"
	// Another program holds both conversations open.
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{
			{PID: 61, Name: "muse", Args: []string{"muse", "--session-id", convA}},
			{PID: 62, Name: "muse", Args: []string{"muse", "--session-id", convB}},
		}
	})
	m := installMuseCodeStub(t)
	const id = owTestSession + "-reused-id"
	sink := &outsideWriterSink{}
	keyOf := func(conversation string) string { return cliSessionEngineMuseCode + "|" + id + "|" + conversation }
	watch := func(conversation string) bool {
		return watchOneShotResumedSession(cliSessionEngineMuseCode, commandMsg{
			SessionID: id, WorkspaceID: "ws", UID: "uid", AgentID: "agent-1", ConversationID: conversation,
		}, m, sink.publish)
	}
	cwd := startMuse(t, m, id, convA)
	replace := func(conversation string) {
		t.Helper()
		if err := m.End(id); err != nil {
			t.Fatalf("end: %v", err)
		}
		if err := m.Start(id, cwd, "ws", "uid", conversation, nil, nil); err != nil {
			t.Fatalf("replacement start: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = m.End(id)
		waitForWriterState(t, 3*time.Second, "every watcher to end", func() bool {
			return !watchRunning(keyOf(convA)) && !watchRunning(keyOf(convB))
		})
	})

	if !watch(convA) {
		t.Fatal("watcher not started")
	}
	first := m.TurnStateOf(id)
	waitForWriterState(t, 3*time.Second, "A held open elsewhere", func() bool { return len(sink.states(id)) == 1 })

	replace(convA)
	if alive, _, _ := first(); alive {
		t.Fatal("the ended session read as alive through the replacement under its id")
	}
	if alive, _, _ := m.TurnStateOf(id)(); !alive {
		t.Fatal("the replacement does not read as alive")
	}
	if !watch(convA) {
		t.Fatal("the replacement's START was refused its watcher: the ended session's stood in for it")
	}

	replace(convB)
	if !watch(convB) {
		t.Fatal("the replacement continuing B started no watcher")
	}
	waitForWriterState(t, 3*time.Second, "the watcher of A to unwind", func() bool { return !watchRunning(keyOf(convA)) })
}

func TestCliSessionResumedIDReadsOnlyResumeFrames(t *testing.T) {
	for _, tc := range []struct {
		frame   string
		methods []string
		want    string
	}{
		{`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"` + owTestThread + `","cwd":"/w","mcpServers":[]}}`, acpResumeMethods(grokACPProfile), owTestThread},
		{`{"jsonrpc":"2.0","id":5,"method":"session/resume","params":{"sessionId":"` + owTestThread + `","cwd":"/w"}}`, acpResumeMethods(openCodeACPProfile), owTestThread},
		{`{"jsonrpc":"2.0","id":5,"method":"session/new","params":{"cwd":"/w"}}`, acpResumeMethods(grokACPProfile), ""},
		{`{"jsonrpc":"2.0","id":5,"method":"session/resume","params":{"commandId":"c","sessionId":"` + owTestThread + `"}}`, acpResumeMethods(museCodeMSPProfile), owTestThread},
		{`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"` + owTestThread + `"}}`, acpResumeMethods(museCodeMSPProfile), ""},
		{`{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"` + owTestThread + `"}}`, []string{codexMethodThreadResume}, owTestThread},
		{`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{}}`, []string{codexMethodThreadResume}, ""},
		{`not json`, []string{codexMethodThreadResume}, ""},
	} {
		if got := cliSessionResumedID(tc.frame, tc.methods...); got != tc.want {
			t.Errorf("cliSessionResumedID(%s) = %q, want %q", tc.frame, got, tc.want)
		}
	}
}

// The ACP family and the Codex app-server start the watcher of the session a
// frame they wrote resumes, for as long as their child runs.
func TestResumeFramesStartTheirSessionsWatcher(t *testing.T) {
	fastOutsideWriterPolls(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	t.Setenv("GROK_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	sink := &outsideWriterSink{}

	acp := &ACPSession{ID: owTestSession + "-acp", WorkspaceID: "ws", UID: "u", processExited: make(chan struct{}), publish: sink.publish}
	newACPManager(nil, grokACPProfile).noteACPResumeWritten(acp, `{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"`+owTestThread+`","cwd":"/w","mcpServers":[]}}`)
	acpKey := cliSessionEngineGrok + "|" + acp.ID + "|" + owTestThread
	if !watchRunning(acpKey) {
		t.Fatal("a Grok session/load started no watcher")
	}

	codex := &CodexAppServerSession{ID: owTestSession + "-codex", WorkspaceID: "ws", UID: "u", processExited: make(chan struct{}), publish: sink.publish}
	NewCodexAppServerManager(nil).noteCodexResumeWritten(codex, `{"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"threadId":"`+owTestThread+`"}}`)
	codexKey := cliSessionEngineCodex + "|" + codex.ID + "|" + owTestThread
	if !watchRunning(codexKey) {
		t.Fatal("a Codex thread/resume started no watcher")
	}
	NewCodexAppServerManager(nil).noteCodexResumeWritten(codex, `{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"`+owTestOther+`"}}`)
	if watchRunning(cliSessionEngineCodex + "|" + codex.ID + "|" + owTestOther) {
		t.Fatal("a turn/start started a watcher")
	}

	close(acp.processExited)
	close(codex.processExited)
	waitForWriterState(t, 3*time.Second, "the watchers to end with their children", func() bool {
		return !watchRunning(acpKey) && !watchRunning(codexKey)
	})
}

// The real process table: a child of this agent that names a session is
// found, and is this agent's own — never another writer.
func TestProcessScanFindsTheAgentsOwnChildAsOwn(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process table read only on win/linux/darwin")
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	mock := filepath.Join(dir, name)
	if err := copyTestBinary(testExe, mock); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(mock, "resume", owTestThread)
	cmd.Env = setEnvVar(os.Environ(), mockCLIEnvVar, "stdin-deaf-hang")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	var found *cliSessionProcess
	waitForWriterState(t, 20*time.Second, "the child in the process table", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		procs, _ := defaultScanCliSessionProcesses(ctx)
		for i := range procs {
			if procs[i].PID == cmd.Process.Pid {
				found = &procs[i]
				return true
			}
		}
		return false
	})
	if found.Name != "codex" || !cliSessionArgvNames(found.Args, owTestThread) || !found.Own {
		t.Fatalf("process = %+v, want codex naming the thread, marked this agent's own", found)
	}
	resetCliSessionProcessSnapshot()
	t.Cleanup(resetCliSessionProcessSnapshot)
	for _, w := range cliSessionProcessWriters(codexWriterEvidence, owTestThread) {
		if w.PID == cmd.Process.Pid {
			t.Fatalf("this agent's own child counted as another writer: %+v", w)
		}
	}
	if !strings.HasSuffix(strings.ToLower(filepath.Base(found.Args[0])), "codex") && !strings.Contains(strings.ToLower(found.Args[0]), "codex") {
		t.Fatalf("argv[0] = %q", found.Args[0])
	}
}

// A new session under an id whose last session just ended is watched at once:
// the old watcher, still unwinding, never keeps the new one from starting.
func TestOutsideWriterReplacesTheWatcherOfAnEndedSession(t *testing.T) {
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	t.Setenv("CODEX_HOME", t.TempDir())
	sink := &outsideWriterSink{}
	target := func(stop chan struct{}) cliOutsideWriterTarget {
		return cliOutsideWriterTarget{
			engine: cliSessionEngineCodex, sessionID: owTestThread, agentID: "agent-1", stop: stop,
			owner: cliOutsideWriterOwner{ID: owTestSession + "-reused", WorkspaceID: "ws", UID: "u"},
		}
	}
	first := make(chan struct{})
	if !startCliSessionOutsideWriterWatcher(target(first), sink.publish) {
		t.Fatal("first watcher not started")
	}
	if startCliSessionOutsideWriterWatcher(target(first), sink.publish) {
		t.Fatal("a redelivered resume of the running session started a second watcher")
	}
	close(first)
	second := make(chan struct{})
	defer close(second)
	if !startCliSessionOutsideWriterWatcher(target(second), sink.publish) {
		t.Fatal("the new session's watcher was refused while the ended one unwound")
	}
}

// A store read that fails (0) between two good ones must not read as quiet: the
// last good write time stands until writerQuietMs passes, as a real quiet does.
func TestLastGoodStoreClockSurvivesAFailedRead(t *testing.T) {
	now := time.Now()
	written := now.Add(-2 * time.Second).UnixMilli()
	reads := []int64{written, 0}
	i := 0
	clock := lastGoodStoreClock(func() int64 {
		v := reads[i]
		if i < len(reads)-1 {
			i++
		}
		return v
	})
	if got := clock(); got != written {
		t.Fatalf("first read = %d, want %d", got, written)
	}
	if got := clock(); got != written {
		t.Fatalf("failed read = %d, want the last good %d", got, written)
	}
	if !cliSessionStoreWrittenByOther(clock(), now, nil) {
		t.Fatal("a failed read turned a session written 2 s ago quiet")
	}
	if cliSessionStoreWrittenByOther(clock(), now.Add(cliSessionWriterQuiet), nil) {
		t.Fatal("the last good write must still go quiet after writerQuietMs")
	}
	if got := lastGoodStoreClock(func() int64 { return 0 })(); got != 0 {
		t.Fatalf("never read = %d, want 0 (unknown)", got)
	}
}
