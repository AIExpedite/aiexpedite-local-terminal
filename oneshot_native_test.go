package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Core lifecycle tests for oneshot_native.go, driven through the Muse Code
// spec and the stub `muse` binary (musecode_stub_test.go). These are the
// regression net for the extracted core: every invariant the OpenCode manager
// encodes (start ack, one turn at a time, fail-closed errors, bounded replay,
// id published only after success) is pinned here for the shared version.

func startMuse(t *testing.T, m *oneShotNativeManager, id, seed string) string {
	t.Helper()
	cwd := t.TempDir()
	if err := m.Start(id, cwd, "ws", "uid", seed, nil, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	return cwd
}

func TestOneShotNative_StartValidatesInput(t *testing.T) {
	m := installMuseCodeStub(t)
	cwd := t.TempDir()
	if err := m.Start("", cwd, "ws", "uid", "", nil, nil); err == nil {
		t.Fatal("an empty session id must be refused")
	}
	if err := m.Start("s", "relative/dir", "ws", "uid", "", nil, nil); err == nil {
		t.Fatal("a relative cwd must be refused")
	}
	if err := m.Start("s", filepath.Join(cwd, "missing"), "ws", "uid", "", nil, nil); err == nil {
		t.Fatal("a missing cwd must be refused")
	}
	// The cloud sends only the follow-up text on a resume; an unusable seed
	// must fail the start rather than run it in an empty conversation.
	if err := m.Start("s", cwd, "ws", "uid", "--continue", nil, nil); err == nil {
		t.Fatal("a non-UUID resume seed must be refused")
	}
	if m.Get("s") != nil {
		t.Fatal("a refused start must not register a session")
	}
}

func TestOneShotNative_StartIsIdempotentAndMintsID(t *testing.T) {
	m := installMuseCodeStub(t)
	cwd := t.TempDir()
	acks := 0
	onStarted := func() { acks++ }
	if err := m.Start("s", cwd, "ws", "uid", "", nil, onStarted); err != nil {
		t.Fatalf("start: %v", err)
	}
	first := m.Get("s").NativeSessionID
	if !isValidMuseCodeSessionID(first) {
		t.Fatalf("a caller-supplied-id CLI must get a minted UUID, got %q", first)
	}
	if err := m.Start("s", cwd, "ws", "uid", "", nil, onStarted); err != nil {
		t.Fatalf("redelivered start: %v", err)
	}
	if acks != 2 {
		t.Fatalf("redelivery must re-ack started, acks=%d", acks)
	}
	if m.Get("s").NativeSessionID != first {
		t.Fatal("redelivery must not re-mint the session id")
	}
	if m.Get("s").nativeConfirmed {
		t.Fatal("a minted id is unconfirmed until a turn under it succeeds")
	}
}

func TestOneShotNative_SendStreamsFramesAndPublishesIDAfterSuccess(t *testing.T) {
	m := installMuseCodeStub(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameUserInput, museFrameDeltaHello, museFrameDeltaWorld, museFrameTool, museFrameScheduled, museFrameCompleted))
	startMuse(t, m, "s", "")
	minted := m.Get("s").NativeSessionID

	sink := &frameSink{}
	if err := m.Send("s", "say hello", sink.publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}

	// One MESSAGE per rendered event line, then the completion. The two text
	// deltas merge into one frame; the echoed prompt (turn.input.user) and task
	// scheduling chatter are not published.
	msgs := sink.ofType("musecode_native_message")
	if len(msgs) != 4 {
		t.Fatalf("want 1 merged delta + task + terminal + completion frames, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Output, `"text":"Hello world"`) || !strings.Contains(msgs[0].Output, "run.output.delta") {
		t.Fatalf("adjacent deltas must publish as one delta record, got %s", msgs[0].Output)
	}
	for _, f := range msgs {
		if strings.Contains(f.Output, "turn.input.user") || strings.Contains(f.Output, "task.lifecycle.scheduled") {
			t.Fatalf("bookkeeping record was published: %s", f.Output)
		}
	}
	done, ok := sink.completion()
	if !ok {
		t.Fatal("missing aiexpedite.turn_complete frame")
	}
	if !strings.Contains(done.Output, `"text":"Hello world"`) {
		t.Fatalf("completion must carry the CLI's final text, got %s", done.Output)
	}
	if done.ConversationID != minted {
		t.Fatalf("completion must publish the minted id %q, got %q", minted, done.ConversationID)
	}
	if !m.Get("s").nativeConfirmed {
		t.Fatal("a successful turn confirms the id")
	}

	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "exec --json --disable-approval --session-id "+minted+" --prompt-file ") {
		t.Fatalf("unexpected argv: %s", argv)
	}
	if strings.Contains(string(argv), "say hello") {
		t.Fatal("the prompt must never appear on argv")
	}
	stdin, _ := os.ReadFile(stdinLog)
	if string(stdin) != "say hello" {
		t.Fatalf("prompt must arrive via the prompt file, got %q", stdin)
	}
	entries, _ := os.ReadDir(cliPromptTempDir("musecode-prompts"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "musecode-prompt-") {
			t.Fatalf("prompt file %s was not removed after the turn", e.Name())
		}
	}
	tr := m.Get("s").Transcript
	if len(tr) != 2 || tr[1].Content != "Hello world" {
		t.Fatalf("transcript must hold the user/assistant pair, got %#v", tr)
	}
}

func TestOneShotNative_FailedTurnIsAnErrorAndRotatesUnconfirmedID(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameFailed402))
	t.Setenv("OPENCODE_STUB_EXIT", "1")
	startMuse(t, m, "s", "")
	minted := m.Get("s").NativeSessionID

	sink := &frameSink{}
	if err := m.Send("s", "hi", sink.publish, time.Minute); err == nil {
		t.Fatal("a run.terminal.failed turn must fail")
	}
	errs := sink.ofType("musecode_native_error")
	if len(errs) != 1 || errs[0].Output != "[Muse Code turn failed: billing_not_configured (status 402): Billing is not configured for this account.]" {
		t.Fatalf("unexpected error frames: %#v", errs)
	}
	if _, ok := sink.completion(); ok {
		t.Fatal("a failed turn must never publish a completion")
	}
	s := m.Get("s")
	if len(s.Transcript) != 0 {
		t.Fatal("a failed turn must not reach the transcript")
	}
	if s.NativeSessionID == minted || !isValidMuseCodeSessionID(s.NativeSessionID) {
		t.Fatal("an unconfirmed id must rotate after a failed turn so the next one does not resume a hidden turn")
	}
	if s.Status() != "idle" {
		t.Fatalf("the logical session survives a failed turn, status=%s", s.Status())
	}
}

func TestOneShotNative_EmptyCompletionIsNotSuccess(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(`not json at all`))
	startMuse(t, m, "s", "")
	sink := &frameSink{}
	if err := m.Send("s", "hi", sink.publish, time.Minute); err == nil {
		t.Fatal("an empty completion must not be treated as success")
	}
	if len(sink.ofType("musecode_native_error")) != 1 {
		t.Fatal("expected one error frame")
	}
}

func TestOneShotNative_TurnTimeoutIsReported(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "60000")
	startMuse(t, m, "s", "")
	sink := &frameSink{}
	if err := m.Send("s", "hi", sink.publish, 1500*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	errs := sink.ofType("musecode_native_error")
	if len(errs) != 1 || !strings.Contains(errs[0].Output, "timed out") {
		t.Fatalf("a timeout must publish a terminal error frame, got %#v", errs)
	}
	if m.Get("s") == nil {
		t.Fatal("a timed-out turn must not destroy the logical session")
	}
}

func TestOneShotNative_EndMidTurnKillsTheChildAndDrains(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "60000")
	startMuse(t, m, "s", "")
	sink := &frameSink{}
	done := make(chan error, 1)
	go func() { done <- m.Send("s", "hi", sink.publish, time.Minute) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		s := m.Get("s")
		s.mu.Lock()
		running := s.activeProcess != nil
		s.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := m.End("s"); err != nil {
		t.Fatalf("end: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("End must kill the process tree, not wait out the child")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled turn must not report success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send did not return after End")
	}
	if _, ok := sink.completion(); ok {
		t.Fatal("late output must not be promoted to a completion after End")
	}
	if m.Get("s") != nil {
		t.Fatal("End removes the logical session")
	}
	if err := m.End("s"); err == nil || errors.Is(err, errEndUnconfirmed) {
		t.Fatalf("a second End reports not found, got %v", err)
	}
}

func TestOneShotNative_ConcurrentTurnIsRefused(t *testing.T) {
	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")
	s := m.Get("s")
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if err := m.Send("s", "hi", (&frameSink{}).publish, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "already has a turn in flight") {
		t.Fatalf("a second turn must be refused, got %v", err)
	}
}

func TestOneShotNative_BelowVersionFloorReplaysAndNeverPublishesAnID(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_VERSION", "muse 1.1.9")
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
	startMuse(t, m, "s", "")

	for _, prompt := range []string{"first", "second"} {
		sink := &frameSink{}
		if err := m.Send("s", prompt, sink.publish, time.Minute); err != nil {
			t.Fatalf("send %s: %v", prompt, err)
		}
		done, _ := sink.completion()
		if done.ConversationID != "" {
			t.Fatal("below the floor no id may be published")
		}
	}
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "--session-id") {
		t.Fatalf("below the floor --session-id must never be passed: %s", argv)
	}
	stdin, _ := os.ReadFile(stdinLog)
	if !strings.Contains(string(stdin), "Prior turns") || !strings.Contains(string(stdin), "User: first") ||
		!strings.HasSuffix(string(stdin), "User: second\n") {
		t.Fatalf("the follow-up must replay the transcript, got %q", stdin)
	}
}

func TestOneShotNative_SeededResumeUsesTheSeedAndSkipsReplay(t *testing.T) {
	m := installMuseCodeStub(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
	seed := "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"
	startMuse(t, m, "s", seed)
	sink := &frameSink{}
	if err := m.Send("s", "follow up", sink.publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "--session-id "+seed) {
		t.Fatalf("a seeded session resumes exactly the seed: %s", argv)
	}
	if done, _ := sink.completion(); done.ConversationID != seed {
		t.Fatalf("the seed stays the conversation id, got %q", done.ConversationID)
	}
}

func TestOneShotNative_MissingSessionDegradesToOneReplay(t *testing.T) {
	m := installMuseCodeStub(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
	t.Setenv("OPENCODE_STUB_FIRST_STDOUT", `Error: session not found\n`)
	t.Setenv("OPENCODE_STUB_FIRST_EXIT", "1")
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
	seed := "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"
	startMuse(t, m, "s", seed)
	s := m.Get("s")
	s.Transcript = appendOneShotTranscript(nil, "user", "earlier question")
	s.Transcript = appendOneShotTranscript(s.Transcript, "assistant", "earlier answer")

	sink := &frameSink{}
	if err := m.Send("s", "follow up", sink.publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	runs, _ := os.ReadFile(runLog)
	if strings.Count(string(runs), "\n") != 2 {
		t.Fatalf("a missing session must replay exactly once, runs=%q", runs)
	}
	stdin, _ := os.ReadFile(stdinLog)
	if !strings.Contains(string(stdin), "earlier answer") {
		t.Fatal("the replay must carry the bounded transcript")
	}
	done, ok := sink.completion()
	if !ok || !strings.Contains(done.Output, `"replayRecovery":true`) {
		t.Fatalf("the completion must carry the replay marker, got %#v", done)
	}
	if done.ConversationID == seed || !isValidMuseCodeSessionID(done.ConversationID) {
		t.Fatalf("the replay runs under a fresh id, got %q", done.ConversationID)
	}
}

func TestOneShotNative_ReplayPromptRespectsBothBounds(t *testing.T) {
	var tr []oneShotTurn
	for i := 0; i < 40; i++ {
		tr = appendOneShotTranscript(tr, "user", "q")
	}
	if len(tr) != oneShotReplayMaxMessages {
		t.Fatalf("message bound: got %d", len(tr))
	}
	big := strings.Repeat("x", oneShotReplayMaxChars/2)
	tr = appendOneShotTranscript(nil, "user", big)
	tr = appendOneShotTranscript(tr, "assistant", big)
	tr = appendOneShotTranscript(tr, "user", big)
	if oneShotTranscriptChars(tr) > oneShotReplayMaxChars {
		t.Fatalf("char bound: got %d", oneShotTranscriptChars(tr))
	}
	// The current turn is never truncated, even when history must go.
	huge := strings.Repeat("y", oneShotNativeMaxPromptBytes)
	prompt := buildOneShotReplayPrompt("P\n", []oneShotTurn{{Role: "user", Content: "old"}}, huge)
	if !strings.HasSuffix(prompt, "User: "+huge+"\n") || strings.Contains(prompt, "old") {
		t.Fatal("the final user turn must survive intact and push out history")
	}
}

func TestOneShotNative_StripsUnrelatedProviderCredentials(t *testing.T) {
	env := []string{
		"PATH=/bin", "META_API_KEY=keep", "anthropic_api_key=x", "CLAUDE_CODE_OAUTH_TOKEN=x",
		"OPENAI_API_KEY=x", "XAI_API_KEY=x", "GROK_TOKEN=x", "CODEX_HOME=x", "HOME=/home/u",
	}
	got := stripEnvPrefixes(env, museCodeUnrelatedStripped)
	want := []string{"PATH=/bin", "META_API_KEY=keep", "HOME=/home/u"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOneShotNative_ParseVersionTriple(t *testing.T) {
	for in, want := range map[string]string{
		"muse 1.4.0 (1.4.0-R4161.1)": "1.4.0",
		"1.3.0 (1.3.0-R3401.1)\n":    "1.3.0",
		"v1.2.1":                     "1.2.1",
		"development build":          "",
	} {
		if got := parseCLIVersionTriple(in); got != want {
			t.Errorf("parseCLIVersionTriple(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRandomUUIDIsAValidV4(t *testing.T) {
	a, b := newRandomUUID(), newRandomUUID()
	if !isValidMuseCodeSessionID(a) || a == b || a[14] != '4' {
		t.Fatalf("bad uuids %q %q", a, b)
	}
}

func TestOneShotNative_SeedBelowVersionFloorFailsClosed(t *testing.T) {
	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_VERSION", "muse 1.1.9")
	err := m.Start("s", t.TempDir(), "ws", "uid", "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("a seed the CLI cannot honour must refuse the start, got %v", err)
	}
	if m.Get("s") != nil {
		t.Fatal("a refused start must not register a session")
	}
	// Without a seed the same binary still starts (replay covers follow-ups).
	if err := m.Start("s", t.TempDir(), "ws", "uid", "", nil, nil); err != nil {
		t.Fatalf("an unseeded start below the floor must still work: %v", err)
	}
}

func TestDeltaCoalescer(t *testing.T) {
	render := func(text string) string { return "D:" + text }
	var (
		mu  sync.Mutex
		out []string
	)
	emit := func(line string) { mu.Lock(); out = append(out, line); mu.Unlock() }
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), out...) }

	t.Run("a non-delta line flushes buffered text ahead of itself", func(t *testing.T) {
		out = nil
		c := newDeltaCoalescer(render, emit)
		c.add("Hel")
		c.add("lo")
		c.publish("TOOL")
		c.close()
		if got := strings.Join(snapshot(), "|"); got != "D:Hello|TOOL" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("the size bound flushes without waiting", func(t *testing.T) {
		out = nil
		c := newDeltaCoalescer(render, emit)
		c.add(strings.Repeat("x", oneShotDeltaFlushBytes))
		if len(snapshot()) != 1 {
			t.Fatal("a full buffer must flush immediately")
		}
		c.close()
	})

	t.Run("the timer flushes a stalled stream, and close flushes the rest once", func(t *testing.T) {
		out = nil
		c := newDeltaCoalescer(render, emit)
		c.add("thinking")
		deadline := time.Now().Add(5 * time.Second)
		for len(snapshot()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := snapshot(); len(got) != 1 || got[0] != "D:thinking" {
			t.Fatalf("stalled text must surface within the flush delay, got %q", got)
		}
		c.add("tail")
		c.close()
		time.Sleep(2 * oneShotDeltaFlushDelay)
		if got := strings.Join(snapshot(), "|"); got != "D:thinking|D:tail" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no renderer means no coalescing", func(t *testing.T) {
		if newDeltaCoalescer(nil, emit).add("x") {
			t.Fatal("a spec without DeltaFrame must publish deltas as-is")
		}
	})
}
