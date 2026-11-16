package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBrowserPoster struct {
	mu      sync.Mutex
	results []browserResultBody
	shots   []string
	shotErr error
	// gate, when set, holds every PostResult until it is closed (a slow or
	// retrying delivery).
	gate chan struct{}
}

func (f *fakeBrowserPoster) PostResult(ctx context.Context, body browserResultBody) error {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, body)
	return nil
}

func (f *fakeBrowserPoster) PostScreenshot(ctx context.Context, id string, data []byte, ct string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots = append(f.shots, id+"|"+ct)
	return f.shotErr
}

func (f *fakeBrowserPoster) byID(id string) []browserResultBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []browserResultBody
	for _, r := range f.results {
		if r.CommandID == id {
			out = append(out, r)
		}
	}
	return out
}

func (d *browserDispatcher) registerForTest(id string) *atomic.Bool {
	s := &atomic.Bool{}
	d.mu.Lock()
	d.active[id] = s
	d.mu.Unlock()
	return s
}

func (f *fakeBrowserPoster) shotList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.shots...)
}

func (f *fakeBrowserPoster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.results)
}

type dispatcherHarness struct {
	d      *browserDispatcher
	poster *fakeBrowserPoster
	be     *fakeBackend
	done   chan string
	routeE *browserError
}

func newDispatcherHarness(t *testing.T, page *fakePage) *dispatcherHarness {
	t.Helper()
	h := &dispatcherHarness{poster: &fakeBrowserPoster{}, done: make(chan string, 64)}
	h.be = &fakeBackend{route: browserRouteDevTools, pages: map[string]*fakePage{"T1": page, "T2": page, "new": page},
		tabs: []browserTabInfo{{TabID: "d:T1", URL: "https://ex.com/", Title: "Ex"}}}
	d := newBrowserDispatcher()
	d.poster = func() browserPoster { return h.poster }
	d.route = func(ctx context.Context, tabRoute string) (browserRouteChoice, *browserError) {
		if h.routeE != nil {
			return browserRouteChoice{}, h.routeE
		}
		return browserRouteChoice{Route: browserRouteDevTools}, nil
	}
	d.open = func(ctx context.Context, c browserRouteChoice) (browserBackend, func(), error) {
		return h.be, func() {}, nil
	}
	d.done = func(id string) { h.done <- id }
	h.d = d
	return h
}

func (h *dispatcherHarness) send(t *testing.T, params map[string]any) {
	t.Helper()
	if _, ok := params["v"]; !ok {
		params["v"] = 1
	}
	if _, ok := params["deadlineMs"]; !ok {
		params["deadlineMs"] = time.Now().Add(time.Minute).UnixMilli()
	}
	h.d.handle(browserCmd(t, params))
}

func (h *dispatcherHarness) wait(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("waited for %d results, got %d", n, i)
		}
	}
}

func TestBrowserDispatcherRunsAndPosts(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.send(t, map[string]any{"commandId": "c1", "op": "tabs"})
	h.wait(t, 1)
	r := h.poster.byID("c1")
	if len(r) != 1 || !r[0].OK || r[0].Route != browserRouteDevTools || r[0].V != 1 {
		t.Fatalf("%+v", r)
	}
	raw, _ := json.Marshal(r[0].Result)
	if !strings.Contains(string(raw), `"tabId":"d:T1"`) {
		t.Fatalf("result %s", raw)
	}
	waitActiveCount(t, h.d, 0)
}

// waitActiveCount: the op's goroutine leaves active just after its result
// posted, so the count settles a moment after the done hook fires.
func waitActiveCount(t *testing.T, d *browserDispatcher, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for d.ActiveCount() != want {
		if time.Now().After(deadline) {
			t.Fatalf("ActiveCount = %d, want %d", d.ActiveCount(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A result still being delivered holds an update's drain: the message was
// acked, so replacing the agent then would lose it (a submitted form's result
// among them). Refusals posted from the Pub/Sub callback count too.
func TestBrowserDispatcherResultDeliveryIsDrainWork(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.poster.gate = make(chan struct{})
	h.send(t, map[string]any{"commandId": "c1", "op": "tabs"})
	h.send(t, map[string]any{"commandId": "c2", "op": "no-such-op"})
	if n := h.d.ActiveCount(); n < 1 {
		t.Fatalf("ActiveCount = %d right after dispatch", n)
	}
	// The tabs op itself finishes at once; its result is still being posted.
	time.Sleep(100 * time.Millisecond)
	if n := h.d.ActiveCount(); n != 2 {
		t.Fatalf("ActiveCount = %d while two results are undelivered, want 2", n)
	}
	close(h.poster.gate)
	h.wait(t, 2)
	waitActiveCount(t, h.d, 0)
}

func TestBrowserDispatcherRedeliveryRunsOnce(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.send(t, map[string]any{"commandId": "c1", "op": "tabs"})
	h.send(t, map[string]any{"commandId": "c1", "op": "tabs"})
	h.wait(t, 1)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.poster.byID("c1")); n != 1 {
		t.Fatalf("%d results for one command", n)
	}
}

func TestBrowserDispatcherRefusals(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.send(t, map[string]any{"commandId": "bad", "op": "open", "url": "javascript:x"})
	h.send(t, map[string]any{"commandId": "late", "op": "tabs", "deadlineMs": time.Now().Add(-time.Second).UnixMilli()})
	h.wait(t, 2)
	if r := h.poster.byID("bad"); len(r) != 1 || r[0].OK || r[0].Error.Code != browserErrCommandInvalid {
		t.Fatalf("invalid: %+v", r)
	}
	if r := h.poster.byID("late"); len(r) != 1 || r[0].Error.Code != browserErrCommandTimeout {
		t.Fatalf("late: %+v", r)
	}
	// No usable commandId: nothing to answer.
	h.d.handle(commandMsg{Command: browserDeviceCommand, Args: []string{"{}"}})
	time.Sleep(50 * time.Millisecond)
	if n := h.poster.count(); n != 2 {
		t.Fatalf("%d results", n)
	}
	// Chrome cannot be reached: the route's own code.
	h.routeE = browserErr(browserErrChromeNotReachable, "off")
	h.send(t, map[string]any{"commandId": "off", "op": "read", "tabId": "d:T1"})
	h.wait(t, 1)
	if r := h.poster.byID("off"); len(r) != 1 || r[0].Error.Code != browserErrChromeNotReachable {
		t.Fatalf("unreachable: %+v", r)
	}
}

// A submit request is sent to the page at most once, whatever the cloud sends.
func TestBrowserDispatcherSubmitsARequestOnce(t *testing.T) {
	page := &fakePage{id: "d:T1", trusted: true, kit: map[string]func([]json.RawMessage) any{
		"submitCheck": func([]json.RawMessage) any { return map[string]any{"ok": false, "reason": "not-found"} },
	}}
	h := newDispatcherHarness(t, page)
	digest := strings.Repeat("b", 64)
	h.send(t, map[string]any{"commandId": "s1", "op": "submit", "tabId": "d:T1", "submitRef": "f1_a", "digest": digest, "requestId": "req-1"})
	h.send(t, map[string]any{"commandId": "s2", "op": "submit", "tabId": "d:T1", "submitRef": "f1_a", "digest": digest, "requestId": "req-1"})
	h.wait(t, 2)
	if len(page.called("submitCheck")) != 1 {
		t.Fatalf("the page saw %d submit attempts", len(page.called("submitCheck")))
	}
	if r := h.poster.byID("s2"); len(r) != 1 || r[0].Error.Code != browserErrCommandInvalid {
		t.Fatalf("second: %+v", r)
	}
}

// Ops on one tab run one at a time.
func TestBrowserDispatcherSerializesATab(t *testing.T) {
	page := &fakePage{id: "d:T1", evalWait: 30 * time.Millisecond, kit: map[string]func([]json.RawMessage) any{
		"snapshot": func([]json.RawMessage) any { return map[string]any{"docId": "doc-1", "url": "https://ex.com/"} },
	}}
	h := newDispatcherHarness(t, page)
	for i := 0; i < 4; i++ {
		h.send(t, map[string]any{"commandId": "r" + string(rune('0'+i)), "op": "read", "tabId": "d:T1"})
	}
	h.wait(t, 4)
	if page.maxBusy.Load() != 1 {
		t.Fatalf("%d ops ran on one tab at once", page.maxBusy.Load())
	}
}

// stop answers at once and tells every op in flight to stop.
func TestBrowserDispatcherStop(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	running := h.d.registerForTest("fill-1")
	h.send(t, map[string]any{"commandId": "stop-1", "op": "stop"})
	h.wait(t, 1)
	r := h.poster.byID("stop-1")
	if len(r) != 1 || !r[0].OK || r[0].Route != "" {
		t.Fatalf("%+v", r)
	}
	if raw, _ := json.Marshal(r[0].Result); string(raw) != `{"stopped":true}` {
		t.Fatalf("result %s", raw)
	}
	if !running.Load() {
		t.Fatal("the running op was not told to stop")
	}
}

func TestBrowserScreenshotUploadedBeforeResult(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.send(t, map[string]any{"commandId": "shot", "op": "screenshot", "tabId": "d:T1"})
	h.wait(t, 1)
	if shots := h.poster.shotList(); len(shots) != 1 || shots[0] != "shot|image/jpeg" {
		t.Fatalf("shots %v", shots)
	}
	if r := h.poster.byID("shot"); len(r) != 1 || !r[0].OK {
		t.Fatalf("%+v", r)
	}
	h.poster.mu.Lock()
	h.poster.shotErr = &browserPostError{Status: 413}
	h.poster.mu.Unlock()
	h.send(t, map[string]any{"commandId": "shot2", "op": "screenshot", "tabId": "d:T1"})
	h.wait(t, 1)
	if r := h.poster.byID("shot2"); len(r) != 1 || r[0].Error.Code != browserErrScreenshotNotFound {
		t.Fatalf("%+v", r)
	}
}

func TestBrowserRefusedWhileDraining(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	h.d.refuseWhileDraining(browserCmd(t, map[string]any{"v": 1, "commandId": "d1", "op": "read", "tabId": "d:T1"}))
	h.wait(t, 1)
	if r := h.poster.byID("d1"); len(r) != 1 || r[0].Error.Code != browserErrDeviceOffline {
		t.Fatalf("%+v", r)
	}
}
