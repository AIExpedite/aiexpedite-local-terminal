package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeASHelper stands in for the osascript helper: it records every request
// line and answers each one with ok.
type fakeASHelper struct {
	mu    sync.Mutex
	lines []string
	pw    *io.PipeWriter
}

func (f *fakeASHelper) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.lines = append(f.lines, string(p))
	f.mu.Unlock()
	var req struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(bytes.TrimSpace(p), &req) == nil {
		go fmt.Fprintf(f.pw, "{\"id\":%d,\"ok\":true,\"result\":\"done\"}\n", req.ID)
	}
	return len(p), nil
}

func (f *fakeASHelper) Close() error { return nil }

func (f *fakeASHelper) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func newFakeASBridge(t *testing.T) (*asBridge, *fakeASHelper) {
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	f := &fakeASHelper{pw: pw}
	return &asBridge{in: f, out: bufio.NewReader(pr), started: true, granted: true}, f
}

// Requests on different tabs share one helper. A request whose deadline passes
// while another call holds the helper is never written to it (a navigation or
// a fill must not reach Chrome after its command was reported as timed out),
// and it leaves the helper the other call is using alone.
func TestASBridgeQueuedRequestHonorsItsDeadline(t *testing.T) {
	b, f := newFakeASBridge(t)
	exec := func(ctx context.Context) error {
		_, err := b.request(ctx, map[string]any{"op": "exec", "tabId": "7", "code": "location.assign('https://example.com/')"})
		return err
	}
	isBusy := func(err error) bool {
		var be *browserError
		return errors.As(err, &be) && be.Code == browserErrCommandTimeout && strings.Contains(be.Message, "nothing was sent")
	}

	// A context that has already ended never takes the turn, even when it is free.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 50; i++ {
		if err := exec(ended); !isBusy(err) {
			t.Fatalf("ended context: err = %v", err)
		}
		if w := f.written(); len(w) != 0 {
			t.Fatalf("ended context: sent to the helper: %q", w)
		}
	}

	// Another tab's Apple Event holds the helper past this request's deadline.
	if err := b.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	started := time.Now()
	err := exec(ctx)
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("waited %v for the helper after the deadline", waited)
	}
	if !isBusy(err) {
		t.Fatalf("queued past its deadline: err = %v", err)
	}
	b.release()

	if w := f.written(); len(w) != 0 {
		t.Fatalf("sent to the helper after the deadline: %q", w)
	}
	if !b.started || b.broken != nil {
		t.Fatalf("the helper was killed (started=%v broken=%v)", b.started, b.broken)
	}

	// The helper still serves the next request, and the refused ones took no id.
	ok, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	res, err := b.request(ok, map[string]any{"op": "info", "tabId": "7"})
	if err != nil || string(res) != `"done"` {
		t.Fatalf("next request: %s, %v", res, err)
	}
	if w := f.written(); len(w) != 1 || !strings.Contains(w[0], `"id":1`) {
		t.Fatalf("written = %q", w)
	}
}
