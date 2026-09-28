package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestOneShotNative_CachedDetectionFailureDoesNotDisableResume(t *testing.T) {
	m := installMuseCodeStub(t) // stub `--version` prints 1.4.0; no .muse-version beside it
	exe := resolveMuseCodeExecutable()
	// A cold detection probe that timed out is cached as "" under the shared key.
	cachedProbeVersionFunc(exe, func() string { return "" })
	if err := m.probeCapability(); err != nil {
		t.Fatal(err)
	}
	if !m.supportsNativeResume() {
		t.Fatal("the capability probe must run its own --version, not inherit a cached detection failure")
	}
}

func TestOneShotNative_VersionProbeIsBoundedForLauncherTrees(t *testing.T) {
	// A launcher whose child hangs and inherits the output pipe — the Windows
	// `muse.cmd` -> PowerShell shape. Killing only the direct child used to
	// leave CombinedOutput blocked on the pipe well past the deadline.
	dir := t.TempDir()
	var launcher string
	if runtime.GOOS == "windows" {
		launcher = filepath.Join(dir, "muse.cmd")
		if err := os.WriteFile(launcher, []byte("@echo off\r\nping -n 60 127.0.0.1 >nul\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		launcher = filepath.Join(dir, "muse")
		if err := os.WriteFile(launcher, []byte("#!/bin/sh\nsleep 60 &\nwait\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := oneShotVersionProbeTimeout
	oneShotVersionProbeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { oneShotVersionProbeTimeout = prev })

	spec := *museCodeNativeSpec
	spec.ResolveExecutable = func() string { return launcher }
	m := newOneShotNativeManager(&spec)
	start := time.Now()
	if _, err := m.probeVersionUncached(); err == nil {
		t.Fatal("a probe that timed out must report the CLI as not runnable")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("probe took %v; the deadline must bound the whole launcher tree", elapsed)
	}
}

// The stale reaper samples under m.mu and ends after releasing it. If the
// sampled session was ended and its id reused by a replacement Start in that
// window, the reap must refuse (staleEndError) rather than end the replacement.
func TestOneShotNative_StaleReapSparesAReplacementUnderTheSameID(t *testing.T) {
	m := installMuseCodeStub(t)
	cwd := startMuse(t, m, "s", "")
	sampled := m.Get("s")
	if err := m.End("s"); err != nil {
		t.Fatalf("end: %v", err)
	}
	if err := m.Start("s", cwd, "ws", "uid", "", nil, nil); err != nil {
		t.Fatalf("replacement start: %v", err)
	}
	replacement := m.Get("s")

	if err := m.endIfSame("s", sampled); !errors.Is(err, errEndStaleSession) {
		t.Fatalf("a reap of a replaced session must be refused as stale, got %v", err)
	}
	if m.Get("s") != replacement || replacement.Status() == "ended" {
		t.Fatal("the replacement session must survive the stale reap")
	}
	if err := m.endIfSame("s", replacement); err != nil {
		t.Fatalf("ending the sampled session itself must still work: %v", err)
	}
}

func TestOneShotShimScript_KeepsVariableOperandsInTheEnvironment(t *testing.T) {
	prompt := `C:\Users\A B\%TEMP%\aix muse\prompt-1.txt`
	args := buildMuseCodeNativeArgs("3f2b8c1e-6a4d-4e2f-9b7a-1c2d3e4f5a6b", prompt)
	script, env, ok := oneShotShimScript(`C:\Users\A B\muse.cmd`, args, []string{"PATH=x"})
	if !ok {
		t.Fatal("the Muse Code turn argv must render as a shim script")
	}
	want := `"%AIX_ONESHOT_SHIM_PATH%" exec --json --disable-approval --session-id 3f2b8c1e-6a4d-4e2f-9b7a-1c2d3e4f5a6b --prompt-file "%AIX_ONESHOT_SHIM_ARG_6%"`
	if script != want {
		t.Fatalf("script:\n got %s\nwant %s", script, want)
	}
	if strings.Contains(script, prompt) || strings.Contains(script, "muse.cmd") {
		t.Fatal("paths must ride in the environment, never in the script text")
	}
	envSet := map[string]bool{}
	for _, kv := range env {
		envSet[kv] = true
	}
	for _, kv := range []string{"PATH=x", `AIX_ONESHOT_SHIM_PATH=C:\Users\A B\muse.cmd`, "AIX_ONESHOT_SHIM_ARG_6=" + prompt} {
		if !envSet[kv] {
			t.Fatalf("env missing %q: %v", kv, env)
		}
	}

	if script, _, ok := oneShotShimScript("muse.cmd", []string{"--version"}, nil); !ok || script != `"%AIX_ONESHOT_SHIM_PATH%" --version` {
		t.Fatalf("version probe script = %q, ok=%v", script, ok)
	}
	for _, bad := range []string{"", `a"b`, "a\nb"} {
		if _, _, ok := oneShotShimScript("muse.cmd", []string{"--prompt-file", bad}, nil); ok {
			t.Fatalf("operand %q cannot survive cmd.exe quoting and must be refused", bad)
		}
	}
}

// The completion frame is the one place a full assistant turn leaves the
// device; a tool that echoed an inherited credential must not be republished
// verbatim there just because the streamed deltas were already redacted.
func TestOneShotCompletionFrameRedactsCredentialShapedText(t *testing.T) {
	secret := "mk-live-0123456789abcdef"
	frame := oneShotCompletionFrame("done. META_API_KEY="+secret+"\n", false)
	if strings.Contains(frame, secret) {
		t.Fatalf("completion frame leaked the credential: %q", frame)
	}
	var payload struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Final bool   `json:"final"`
	}
	if err := json.Unmarshal([]byte(frame), &payload); err != nil {
		t.Fatalf("completion frame is not JSON: %v", err)
	}
	if payload.Type != "aiexpedite.turn_complete" || !payload.Final {
		t.Fatalf("completion envelope changed shape: %+v", payload)
	}
	if !strings.Contains(payload.Text, "[REDACTED]") {
		t.Fatalf("expected a redaction marker in %q", payload.Text)
	}
	// Ordinary assistant text is untouched.
	plain := oneShotCompletionFrame("hello world", true)
	if !strings.Contains(plain, "hello world") || !strings.Contains(plain, "replayRecovery") {
		t.Fatalf("plain completion frame altered: %q", plain)
	}
}

// endIfSame frees the id before the reaper publishes its ended frame, and that
// frame names the session only by the logical id. A replacement Start must not
// be admitted into that window, or the stale frame would release the live
// session's cloud reservation.
func TestOneShotNative_StaleReapReservesTheIDThroughEndedPublication(t *testing.T) {
	m := installMuseCodeStub(t)
	cwd := startMuse(t, m, "s", "")
	sampled := m.Get("s")

	admitted := make(chan error, 1)
	var published []resultMsg
	publish := func(msg resultMsg) {
		// Racing Start, as the cloud would once the id looked free.
		admitted <- m.Start("s", cwd, "ws", "uid", "", nil, nil)
		published = append(published, msg)
	}

	m.reapStaleSession("s", sampled, "ws", "uid", publish)

	if err := <-admitted; err == nil {
		t.Fatal("a Start racing the ended publication must be refused while the id is reserved")
	}
	if len(published) != 1 || published[0].Type != m.spec.frameType("ended") {
		t.Fatalf("the reap must still publish exactly one ended frame, got %+v", published)
	}
	if m.Get("s") != nil {
		t.Fatal("the reaped session must not be replaced inside the reserved window")
	}
	// The reservation is released once publication is done.
	if err := m.Start("s", cwd, "ws", "uid", "", nil, nil); err != nil {
		t.Fatalf("a Start after the ended frame must be admitted: %v", err)
	}
}

// A one-shot session must be visible to the spawn ledger the same way the
// resident kinds are: a logical entry between turns (so a restart can certify
// it ended rather than fence the cloud reservation), a tracked PID for the
// turn's child (so the next boot can reap a survivor), and no record once the
// session is released.
func TestOneShotNative_TurnsAndSessionAreRecordedInTheSpawnLedger(t *testing.T) {
	dir := t.TempDir()
	ledger := newTestLedger(t, dir, "boot-1")
	var mu sync.Mutex
	var tracked []int
	inner := ledger.startToken
	ledger.startToken = func(pid int) (string, error) {
		mu.Lock()
		tracked = append(tracked, pid)
		mu.Unlock()
		return inner(pid)
	}
	prev := globalSpawnLedger
	globalSpawnLedger = ledger
	t.Cleanup(func() { globalSpawnLedger = prev })

	m := installMuseCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameDeltaHello, museFrameCompleted))
	startMuse(t, m, "s", "")

	entries := readLedgerFile(t, dir).Entries
	if len(entries) != 1 || entries[0].SessionID != "s" || !entries[0].Logical {
		t.Fatalf("start must open a logical ledger entry, got %+v", entries)
	}

	sink := &frameSink{}
	if err := m.Send("s", "say hello", sink.publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	mu.Lock()
	turnPIDs := len(tracked)
	mu.Unlock()
	if turnPIDs != 1 {
		t.Fatalf("the turn's child must be tracked in the ledger, tracked=%d", turnPIDs)
	}
	entries = readLedgerFile(t, dir).Entries
	if len(entries) != 1 || !entries[0].Logical {
		t.Fatalf("the logical entry must survive the turn, got %+v", entries)
	}
	// The child exited and its tree is proven empty, so the turn's PID is gone
	// while the logical session stays.
	if len(entries[0].PIDs) != 0 {
		t.Fatalf("a completed turn must untrack its PID, got %+v", entries[0].PIDs)
	}

	if err := m.End("s"); err != nil {
		t.Fatalf("end: %v", err)
	}
	if entries = readLedgerFile(t, dir).Entries; len(entries) != 0 {
		t.Fatalf("End must release the ledger entry, got %+v", entries)
	}
}

// A child that never starts must not leave a pending spawn behind: the next
// boot would read it as "may have leaked a process" forever.
func TestOneShotNative_FailedStartAbortsThePendingSpawn(t *testing.T) {
	dir := t.TempDir()
	ledger := newTestLedger(t, dir, "boot-1")
	prev := globalSpawnLedger
	globalSpawnLedger = ledger
	t.Cleanup(func() { globalSpawnLedger = prev })

	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")
	// Point the spec at a path that cannot be executed, so cmd.Start fails
	// after beginSessionSpawn recorded the pending spawn.
	missing := filepath.Join(t.TempDir(), "not-installed")
	// A copy: the spec is a package-level singleton shared by every manager.
	spec := *m.spec
	spec.ResolveExecutable = func() string { return missing }
	m.spec = &spec

	if err := m.Send("s", "say hello", (&frameSink{}).publish, time.Minute); err == nil {
		t.Fatal("a turn whose child cannot start must be an error")
	}
	entries := readLedgerFile(t, dir).Entries
	if len(entries) != 1 || entries[0].PendingSpawns != 0 || len(entries[0].PIDs) != 0 {
		t.Fatalf("a failed start must abort the pending spawn, got %+v", entries)
	}
}
