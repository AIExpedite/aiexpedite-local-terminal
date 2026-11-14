package main

// A Muse Code resume whose session is gone (coding-agent parity plan, W2.3).
//
// Muse Code creates a session for an id it has no record of, a gone one
// included, and exits 0 with no warning (1.4.18). The run's first record is
// the only sign: `runtime.command.accepted` with `sequence` 1 in a session the
// run created, the session's run count in one it resumed. These records are
// the 1.4.18 shape (captured 2026-11-13), with the stream id swapped in.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// museAcceptedRecord is the first record of a `muse exec --json` run.
func museAcceptedRecord(sessionID string, sequence int) string {
	return `{"schema_version":1,"id":"018f0000-0000-7000-8000-00000000c350","stream":{"kind":"session","id":"` + sessionID +
		`"},"sequence":` + strconv.Itoa(sequence) +
		`,"recorded_at":1780531400000000,"record_type":"reconciliation","durability":"durable","causation_id":"01a1d802-1a98-7b73-8488-70beb711e7fd","payload_type":"runtime.command.accepted","payload_schema_version":1,"payload":{"kind":"command_accepted","command_id":"01a1d802-1a98-7b73-8488-70beb711e7fd","client_id":null,"command_kind":"turn.submit"}}`
}

const museGoneSeed = "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"

// museGoneHarness: a seeded (so confirmed) Muse session with history, the
// stub counting its runs and logging each run's argv and stdin.
type museGoneHarness struct {
	t                         *testing.T
	m                         *oneShotNativeManager
	runLog, argvLog, stdinLog string
}

func newMuseGoneHarness(t *testing.T, seed string) *museGoneHarness {
	t.Helper()
	h := &museGoneHarness{t: t, m: installMuseCodeStub(t)}
	dir := t.TempDir()
	h.runLog = filepath.Join(dir, "runs.log")
	h.argvLog = filepath.Join(dir, "argv.log")
	h.stdinLog = filepath.Join(dir, "stdin.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", h.runLog)
	t.Setenv("OPENCODE_STUB_ARGV_LOG", h.argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", h.stdinLog)
	startMuse(t, h.m, "s", seed)
	s := h.m.Get("s")
	s.Transcript = appendOneShotTranscript(nil, "user", "earlier question")
	s.Transcript = appendOneShotTranscript(s.Transcript, "assistant", "earlier answer")
	return h
}

func (h *museGoneHarness) send() *frameSink {
	h.t.Helper()
	sink := &frameSink{}
	if err := h.m.Send("s", "follow up", sink.publish, time.Minute); err != nil {
		h.t.Fatalf("send: %v", err)
	}
	return sink
}

func (h *museGoneHarness) runs() int {
	data, _ := os.ReadFile(h.runLog)
	return strings.Count(string(data), "\n")
}

func TestMuseCode_GoneSessionIsRecoveredByReplayNotAnsweredBlank(t *testing.T) {
	h := newMuseGoneHarness(t, museGoneSeed)
	// Run 1: Muse has no session under the seed, creates one (sequence 1)
	// and would answer the follow-up with no context, exiting 0.
	t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
	t.Setenv("OPENCODE_STUB_FIRST_EXIT", "0")
	t.Setenv("OPENCODE_STUB_FIRST_STDOUT", museStubStdout(
		museAcceptedRecord(museGoneSeed, 1),
		museDeltaRecord("ANSWER WITHOUT CONTEXT"),
		museCompletedRecord("ANSWER WITHOUT CONTEXT"),
	))
	// Run 2: the replay.
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
	sink := h.send()

	if h.runs() != 2 {
		t.Fatalf("a gone session must be recovered by exactly one replay, runs=%d", h.runs())
	}
	for _, f := range sink.all() {
		if strings.Contains(f.Output, "ANSWER WITHOUT CONTEXT") {
			t.Fatalf("nothing of the run in the recreated session may be published:\n%s", museFramesDump(sink.all()))
		}
	}
	stdin, _ := os.ReadFile(h.stdinLog)
	if !strings.Contains(string(stdin), "earlier answer") {
		t.Fatal("the replay must carry the bounded transcript")
	}
	done, ok := sink.completion()
	if !ok || !strings.Contains(done.Output, `"replayRecovery":true`) {
		t.Fatalf("the completion must say the context was replayed (replayRecovery), got %#v", done)
	}
	if done.ConversationID == museGoneSeed || !isValidMuseCodeSessionID(done.ConversationID) {
		t.Fatalf("the replay runs under a fresh id, got %q", done.ConversationID)
	}
	argv, _ := os.ReadFile(h.argvLog)
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], museGoneSeed) || !strings.Contains(lines[1], done.ConversationID) {
		t.Fatalf("run 1 resumes the seed, run 2 the fresh id:\n%s", argv)
	}
}

func TestMuseCode_ResumedSessionIsNotReplayed(t *testing.T) {
	h := newMuseGoneHarness(t, museGoneSeed)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(
		museAcceptedRecord(museGoneSeed, 2),
		museFrameDeltaHello,
		museFrameDeltaWorld,
		museFrameCompleted,
	))
	sink := h.send()

	if h.runs() != 1 {
		t.Fatalf("a resumed session runs once, runs=%d", h.runs())
	}
	done, ok := sink.completion()
	if !ok || strings.Contains(done.Output, "replayRecovery") {
		t.Fatalf("a resumed session is no recovery, got %#v", done)
	}
	if done.ConversationID != museGoneSeed {
		t.Fatalf("the seed stays the conversation, got %q", done.ConversationID)
	}
	// The run streams on past its first record exactly as before.
	if got := museStreamedDeltaText(t, sink.all()); got != "Hello world" {
		t.Fatalf("a resumed run's text must stream as before, got %q:\n%s", got, museFramesDump(sink.all()))
	}
}

// A new chat's first turn creates its minted session: sequence 1 there is
// expected, never a gone session.
func TestMuseCode_FirstTurnOfANewChatIsNotAGoneSession(t *testing.T) {
	m := installMuseCodeStub(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)
	startMuse(t, m, "s", "")
	minted := m.Get("s").NativeSessionID
	if !isValidMuseCodeSessionID(minted) {
		t.Fatalf("expected a minted id, got %q", minted)
	}
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museAcceptedRecord(minted, 1), museFrameCompleted))
	sink := &frameSink{}
	if err := m.Send("s", "hello", sink.publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	runs, _ := os.ReadFile(runLog)
	if strings.Count(string(runs), "\n") != 1 {
		t.Fatalf("the first turn runs once, runs=%q", runs)
	}
	if done, _ := sink.completion(); strings.Contains(done.Output, "replayRecovery") || done.ConversationID != minted {
		t.Fatalf("the first turn is no recovery and keeps its id, got %#v", done)
	}
}

// Only the run's FIRST record says whether the session was created.
func TestMuseCode_OnlyTheFirstRecordIsRead(t *testing.T) {
	h := newMuseGoneHarness(t, museGoneSeed)
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(
		museAcceptedRecord(museGoneSeed, 3),
		museAcceptedRecord(museGoneSeed, 1),
		museFrameCompleted,
	))
	h.send()
	if h.runs() != 1 {
		t.Fatalf("a record after the first must not start a recovery, runs=%d", h.runs())
	}
}

func TestMuseCodeRunCreatedSession(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"created":                   {museAcceptedRecord(museGoneSeed, 1), true},
		"id in another case":        {museAcceptedRecord(strings.ToUpper(museGoneSeed), 1), true},
		"resumed":                   {museAcceptedRecord(museGoneSeed, 2), false},
		"another session's stream":  {museAcceptedRecord("11111111-1111-4111-8111-111111111111", 1), false},
		"not the accepted record":   {strings.Replace(museAcceptedRecord(museGoneSeed, 1), "runtime.command.accepted", "run.lifecycle.started", 1), false},
		"not the session stream":    {strings.Replace(museAcceptedRecord(museGoneSeed, 1), `"kind":"session"`, `"kind":"exec.usage"`, 1), false},
		"no sequence":               {strings.Replace(museAcceptedRecord(museGoneSeed, 1), `"sequence":1,`, "", 1), false},
		"assistant text quoting it": {museDeltaRecord(`{"payload_type":"runtime.command.accepted","sequence":1,"stream":{"kind":"session","id":"` + museGoneSeed + `"}}`), false},
		"not JSON":                  {"muse: workspace root: C:\\x", false},
	} {
		if got := museCodeRunCreatedSession(tc.line, museGoneSeed); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
