package main

import (
	"sync"
	"testing"
	"time"
)

// A CLI's stdin is one pipe with several writers. Releasing the manager lock
// before the initial prompt is written (so a Quit snapshot cannot be outrun)
// removed the accidental mutual exclusion that used to keep those writers
// apart: SendInputAccepting blocked on sm.mu.RLock() until startSession
// returned, i.e. until after the initial write. stdinMu replaces it
// deliberately rather than by side effect.
//
// The real corruption happens below Go: a prompt larger than PIPE_BUF is
// several non-atomic kernel writes, so an interleaved turn splices its NDJSON
// envelope inside another line. A unit test cannot reproduce a kernel splice,
// so what it pins instead is the property that prevents it — that the second
// writer WAITS.

// blockingWriter holds its first Write until release is closed, so a second
// writer's progress (or lack of it) is observable.
type blockingWriter struct {
	entered  chan struct{}
	release  chan struct{}
	writes   chan string
	closed   bool
	firstHit bool
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
		writes:  make(chan string, 8),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if !w.firstHit {
		w.firstHit = true
		w.entered <- struct{}{}
		<-w.release
	}
	w.writes <- string(p)
	return len(p), nil
}

func (w *blockingWriter) Close() error {
	w.closed = true
	return nil
}

func TestStdin_ASecondWriterWaitsForAnInFlightWrite(t *testing.T) {
	w := newBlockingWriter()
	session := &CLISession{
		ID:            "sess-stdin",
		Stdin:         w,
		StartedAt:     time.Now(),
		Status:        "running",
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}

	// First writer: take stdinMu and block inside Write, as a large initial
	// prompt does while the kernel drains the pipe.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		session.stdinMu.Lock()
		defer session.stdinMu.Unlock()
		_, _ = session.Stdin.Write([]byte("first\n"))
	}()

	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first writer never reached Write")
	}

	// Second writer: must not touch the pipe while the first holds it.
	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		close(secondStarted)
		session.stdinMu.Lock()
		defer session.stdinMu.Unlock()
		_, _ = session.Stdin.Write([]byte("second\n"))
	}()
	<-secondStarted

	select {
	case <-secondDone:
		t.Fatal("second writer completed while the first held stdin — writes are not serialized")
	case got := <-w.writes:
		t.Fatalf("a write landed while the first writer was blocked: %q", got)
	case <-time.After(150 * time.Millisecond):
		// Correct: the second writer is parked on stdinMu.
	}

	close(w.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first writer did not finish after release")
	}
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second writer did not finish after the first released stdin")
	}

	// Whole lines, in order, neither spliced into the other.
	var got []string
	for len(got) < 2 {
		select {
		case line := <-w.writes:
			got = append(got, line)
		case <-time.After(time.Second):
			t.Fatalf("only saw %d writes: %q", len(got), got)
		}
	}
	if got[0] != "first\n" || got[1] != "second\n" {
		t.Errorf("writes arrived as %q, want [\"first\n\" \"second\n\"]", got)
	}
}

// A close must not retire the pipe under an in-flight write.
func TestStdin_CloseWaitsForAnInFlightWrite(t *testing.T) {
	w := newBlockingWriter()
	session := &CLISession{ID: "sess-close", Stdin: w, StartedAt: time.Now()}

	writing := make(chan struct{})
	go func() {
		defer close(writing)
		session.stdinMu.Lock()
		defer session.stdinMu.Unlock()
		_, _ = session.Stdin.Write([]byte("payload\n"))
	}()

	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never reached Write")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		session.stdinMu.Lock()
		defer session.stdinMu.Unlock()
		_ = session.Stdin.Close()
	}()

	select {
	case <-closed:
		t.Fatal("stdin was closed while a write was in flight")
	case <-time.After(150 * time.Millisecond):
	}
	if w.closed {
		t.Fatal("Close landed under an in-flight write")
	}

	close(w.release)
	select {
	case <-writing:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not finish")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not proceed after the write finished")
	}
	if !w.closed {
		t.Error("stdin was never closed")
	}
}

// A terminal frame must never overtake the START it belongs to.
//
// sm.mu used to give this ordering for free: session_started is published only
// after StartSession RETURNS, so holding the manager lock for the whole of
// StartSession meant nothing could publish a terminal frame first. Releasing it
// early — so a tray Quit's snapshot is not starved by a wedged stdin write —
// removed that guarantee, and a child exiting during the initial prompt
// delivery would let session_ended arrive before session_started, leaving the
// server tracking a dead session as live.
func TestStartAck_TerminalFrameWaitsForTheStartAcknowledgement(t *testing.T) {
	session := &CLISession{ID: "sess-ack", startAck: make(chan struct{}), startAckExpected: true}

	waited := make(chan struct{})
	go func() {
		session.awaitStartAck()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("the terminal frame did not wait for the start acknowledgement")
	case <-time.After(120 * time.Millisecond):
	}

	session.MarkStartAcknowledged()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal frame did not proceed after the start was acknowledged")
	}
}

// Marking is idempotent, because the success and failure paths can both reach
// it, and a double close would panic.
func TestStartAck_MarkingIsIdempotent(t *testing.T) {
	session := &CLISession{ID: "sess-twice", startAck: make(chan struct{}), startAckExpected: true}
	session.MarkStartAcknowledged()
	session.MarkStartAcknowledged()
	session.awaitStartAck() // returns immediately, no panic
}

// A session with no barrier (a struct a test built directly) must not block.
func TestStartAck_AbsentBarrierDoesNotBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		(&CLISession{ID: "sess-nobarrier"}).awaitStartAck()
		var nilSession *CLISession
		nilSession.awaitStartAck()
		nilSession.MarkStartAcknowledged()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitStartAck blocked without a barrier")
	}
}

// The wait happens ONLY when someone undertook to answer the start. Every
// caller but the Pub/Sub handler publishes no start frame, so there is nothing
// a terminal frame could overtake — and making those callers wait is what cost
// 130 seconds of suite time and pushed a timing test over its budget.
func TestStartAck_NoWaitWhenNobodyWillAnswer(t *testing.T) {
	session := &CLISession{ID: "sess-unexpected", startAck: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		session.awaitStartAck()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waited for an acknowledgement nobody undertook to send")
	}
}

// The expectation is declared before the session exists, and consumed when it
// is built — so a later session reusing the id does not inherit a promise
// nobody made for it.
func TestStartAck_ExpectationIsConsumedNotReused(t *testing.T) {
	sm := &SessionManager{sessions: map[string]*CLISession{}}
	if sm.consumeStartAckExpectation("sess-x") {
		t.Fatal("an undeclared id reported an expectation")
	}
	sm.ExpectStartAck("sess-x")
	if !sm.consumeStartAckExpectation("sess-x") {
		t.Fatal("a declared id did not report its expectation")
	}
	if sm.consumeStartAckExpectation("sess-x") {
		t.Fatal("the expectation survived being consumed")
	}
	// An empty id is not a session.
	sm.ExpectStartAck("")
	if sm.consumeStartAckExpectation("") {
		t.Fatal("an empty id recorded an expectation")
	}
}

// The ordering the barrier exists for, end to end over the two publishes: a
// terminal frame must never be the first of the pair to go out.
//
// This is the test that would have caught closing the barrier on
// startSession's RETURN rather than after the start publish: returning is not
// publishing, and the frames are published from different goroutines.
func TestStartAck_StartFrameIsAlwaysPublishedFirst(t *testing.T) {
	session := &CLISession{ID: "sess-order", startAck: make(chan struct{}), startAckExpected: true}

	var order []string
	var mu sync.Mutex
	record := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}

	// The terminal publisher, as waitForExit runs it: on its own goroutine,
	// gated on the barrier.
	published := make(chan struct{})
	go func() {
		session.awaitStartAck()
		record("session_ended")
		close(published)
	}()

	// The handler: publish, THEN mark. Marking before the publish is the bug.
	time.Sleep(50 * time.Millisecond) // let the terminal publisher reach the gate
	record("session_started")
	session.MarkStartAcknowledged()

	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal frame never published")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "session_started" || order[1] != "session_ended" {
		t.Fatalf("frames published in the order %v, want [session_started session_ended]", order)
	}
}
