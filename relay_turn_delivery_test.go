package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A relay turn on a persistent session is accepted (durably) BEFORE it is
// written to the CLI, and a failed acceptance refuses the write outright.

type recordingWriteCloser struct {
	mu      sync.Mutex
	writes  []string
	onWrite func()
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
	}
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	return len(p), nil
}

func (w *recordingWriteCloser) Close() error { return nil }

func newRelayTestClaudeSession(id string, stdin interface {
	Write([]byte) (int, error)
	Close() error
}) *ClaudeNativeSession {
	return &ClaudeNativeSession{
		ID:            id,
		status:        "running",
		Stdin:         stdin,
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}
}

func TestClaudeRelayTurnIsAcceptedBeforeTheWrite(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	accepted := false
	stdin := &recordingWriteCloser{}
	stdin.onWrite = func() {
		if !accepted {
			t.Error("the turn reached the child's stdin before its acceptance was recorded")
		}
	}
	session := newRelayTestClaudeSession("relay-accept-order", stdin)
	if err := m.writeUserTurnAccepting(session, "make it blue", func() error { accepted = true; return nil }); err != nil {
		t.Fatalf("writeUserTurnAccepting: %v", err)
	}
	if !accepted || len(stdin.writes) != 1 {
		t.Fatalf("accepted=%v writes=%d, want true/1", accepted, len(stdin.writes))
	}
}

func TestClaudeRelayTurnUnrecordedAcceptanceRefusesTheWrite(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	stdin := &recordingWriteCloser{}
	session := newRelayTestClaudeSession("relay-accept-fail", stdin)
	err := m.writeUserTurnAccepting(session, "make it blue", func() error {
		return errors.New(relayAcceptanceNotRecorded + ": disk full")
	})
	if err == nil || !strings.Contains(err.Error(), relayAcceptanceNotRecorded) {
		t.Fatalf("err = %v, want the acceptance refusal", err)
	}
	if len(stdin.writes) != 0 {
		t.Fatalf("a turn whose acceptance was not recorded was written: %q", stdin.writes)
	}
	if session.Status() == "ended" {
		t.Fatal("a refused relay turn ended the session")
	}
}

// A refused session (ended) never reaches acceptance.
func TestClaudeRelayTurnRefusalDoesNotAccept(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	session := newRelayTestClaudeSession("relay-ended", &recordingWriteCloser{})
	session.status = "ended"
	accepted := false
	if err := m.writeUserTurnAccepting(session, "hi", func() error { accepted = true; return nil }); err == nil {
		t.Fatal("write to an ended session succeeded")
	}
	if accepted {
		t.Fatal("a refused turn was accepted")
	}
}

// blockingWriteCloser stalls every Write until Close, like a child that
// stopped draining its stdin.
type blockingWriteCloser struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (w *blockingWriteCloser) Write(p []byte) (int, error) {
	<-w.closed
	return 0, errors.New("write on closed pipe")
}

func (w *blockingWriteCloser) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

// A stalled approval write tears the session down exactly as a stalled user
// turn does: stdin closed (the abandoned write fails instead of landing
// late) and the session ended (no later turn or approval can write).
func TestClaudeSendControlTimeoutTerminatesTheSession(t *testing.T) {
	prev := claudeNativeStdinWriteBudget
	claudeNativeStdinWriteBudget = 50 * time.Millisecond
	t.Cleanup(func() { claudeNativeStdinWriteBudget = prev })

	m := NewClaudeNativeManager(nil)
	stdin := &blockingWriteCloser{closed: make(chan struct{})}
	session := newRelayTestClaudeSession("relay-control-stall", stdin)
	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	err := m.SendControl(session.ID, `{"type":"control_response","response":{"request_id":"r1","subtype":"success"}}`)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	select {
	case <-stdin.closed:
	default:
		t.Fatal("stdin left open after a stalled control write")
	}
	if session.Status() != "ended" {
		t.Fatalf("status = %q, want ended", session.Status())
	}
	if err := m.Send(session.ID, "next turn"); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("a later turn after the stall = %v, want refused", err)
	}
}
