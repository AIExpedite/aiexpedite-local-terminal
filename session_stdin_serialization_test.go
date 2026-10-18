package main

import (
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
