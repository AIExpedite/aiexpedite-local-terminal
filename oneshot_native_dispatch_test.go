package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// dispatchOneShotNativeCommand carries the Pub/Sub lifecycle rules for every
// one-shot kind (OpenCode and the oneshot_native.go specs). Each case runs for
// both kinds so the consolidation cannot drift one of them.

type fakeOneShotTarget struct {
	startErr   error
	sendErr    error
	endErr     error
	hasSession bool
	sendCalls  int
	sendTO     time.Duration
	started    bool
}

func (f *fakeOneShotTarget) Start(_, _, _, _, _ string, _ PublishFunc, onStarted func()) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	if onStarted != nil {
		onStarted()
	}
	return nil
}

func (f *fakeOneShotTarget) Send(_, _ string, _ PublishFunc, to time.Duration) error {
	f.sendCalls++
	f.sendTO = to
	return f.sendErr
}

func (f *fakeOneShotTarget) End(string) error       { return f.endErr }
func (f *fakeOneShotTarget) HasSession(string) bool { return f.hasSession }

type dispatchResult struct {
	frames []resultMsg
	errs   []string
}

func runDispatch(cmd commandMsg, target oneShotNativeCommandTarget, kind nativeFrameKind) dispatchResult {
	var r dispatchResult
	dispatchOneShotNativeCommand(cmd, target, kind,
		func(m resultMsg) { r.frames = append(r.frames, m) },
		func(e string) { r.errs = append(r.errs, e) })
	return r
}

func TestDispatchOneShotNativeCommand(t *testing.T) {
	kinds := []nativeFrameKind{openCodeNativeKind, museCodeNativeSpec.nativeFrameKind}
	for _, kind := range kinds {
		cmd := func(suffix string) commandMsg {
			return commandMsg{ID: "c1", Type: kind.FramePrefix + "_" + suffix, SessionID: "s1", Cwd: "/w", Input: "hi", TimeoutMs: 1500}
		}
		name := kind.FramePrefix

		t.Run(name+"/uninitialized manager ends a start but not a send", func(t *testing.T) {
			r := runDispatch(cmd("start"), nil, kind)
			if len(r.errs) != 1 || !strings.Contains(r.errs[0], "manager not initialized") {
				t.Fatalf("errs=%v", r.errs)
			}
			if len(r.frames) != 1 || r.frames[0].Type != kind.frameType("ended") || r.frames[0].ExitCode != -1 {
				t.Fatalf("a start must release the cloud reservation: %#v", r.frames)
			}
			if r := runDispatch(cmd("send"), nil, kind); len(r.frames) != 0 || len(r.errs) != 1 {
				t.Fatalf("send on an uninitialized manager: %#v", r)
			}
		})

		t.Run(name+"/start acks started", func(t *testing.T) {
			r := runDispatch(cmd("start"), &fakeOneShotTarget{}, kind)
			if len(r.errs) != 0 || len(r.frames) != 1 || r.frames[0].Type != kind.frameType("started") {
				t.Fatalf("%#v", r)
			}
			if r.frames[0].Output != kind.DisplayName+" native started" {
				t.Fatalf("started output changed: %q", r.frames[0].Output)
			}
		})

		t.Run(name+"/start failure ends only when no local session exists", func(t *testing.T) {
			r := runDispatch(cmd("start"), &fakeOneShotTarget{startErr: errors.New("boom")}, kind)
			want := "failed to start " + strings.ToLower(kind.DisplayName) + " native: boom"
			if len(r.errs) != 1 || r.errs[0] != want {
				t.Fatalf("errs=%v want %q", r.errs, want)
			}
			if len(r.frames) != 1 || r.frames[0].Type != kind.frameType("ended") {
				t.Fatalf("%#v", r.frames)
			}
			r = runDispatch(cmd("start"), &fakeOneShotTarget{startErr: errors.New("probe"), hasSession: true}, kind)
			if len(r.frames) != 0 {
				t.Fatal("a live local session must keep its reservation after a start error")
			}
		})

		t.Run(name+"/whitespace send is refused before the manager", func(t *testing.T) {
			f := &fakeOneShotTarget{}
			c := cmd("send")
			c.Input = "  \n"
			r := runDispatch(c, f, kind)
			if f.sendCalls != 0 || len(r.errs) != 1 {
				t.Fatalf("calls=%d errs=%v", f.sendCalls, r.errs)
			}
		})

		t.Run(name+"/send forwards the timeout and republishes only unpublished failures", func(t *testing.T) {
			f := &fakeOneShotTarget{}
			if r := runDispatch(cmd("send"), f, kind); len(r.errs) != 0 || f.sendTO != 1500*time.Millisecond {
				t.Fatalf("errs=%v timeout=%v", r.errs, f.sendTO)
			}
			for msg, republish := range map[string]bool{
				"x native session s1 not found":                    true,
				"x native session s1 already has a turn in flight": true,
				"x native session s1 has ended":                    true,
				"session ended during turn":                        false,
				"x turn timed out":                                 false,
			} {
				r := runDispatch(cmd("send"), &fakeOneShotTarget{sendErr: errors.New(msg)}, kind)
				if (len(r.errs) == 1) != republish {
					t.Errorf("%q: republished=%v, want %v", msg, len(r.errs) == 1, republish)
				}
			}
		})

		t.Run(name+"/end outcomes", func(t *testing.T) {
			r := runDispatch(cmd("end"), &fakeOneShotTarget{}, kind)
			if len(r.frames) != 1 || r.frames[0].Type != kind.frameType("ended") || r.frames[0].Status != "success" {
				t.Fatalf("clean end: %#v", r)
			}
			r = runDispatch(cmd("end"), &fakeOneShotTarget{endErr: fmt.Errorf("drain: %w", errEndUnconfirmed)}, kind)
			if len(r.frames) != 0 || len(r.errs) != 1 {
				t.Fatalf("an unconfirmed end must never publish ended: %#v", r)
			}
			r = runDispatch(cmd("end"), &fakeOneShotTarget{endErr: fmt.Errorf("replaced: %w", errEndStaleSession)}, kind)
			if len(r.frames) != 0 || len(r.errs) != 0 {
				t.Fatalf("a stale end publishes nothing: %#v", r)
			}
			r = runDispatch(cmd("end"), &fakeOneShotTarget{endErr: errors.New("x not found")}, kind)
			if len(r.frames) != 1 || r.frames[0].Type != kind.frameType("ended") {
				t.Fatalf("an already-gone session still releases the reservation: %#v", r)
			}
		})

		t.Run(name+"/unknown type", func(t *testing.T) {
			if r := runDispatch(cmd("bogus"), &fakeOneShotTarget{}, kind); len(r.errs) != 1 {
				t.Fatalf("%#v", r)
			}
		})
	}
}
