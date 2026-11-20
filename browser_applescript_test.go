package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
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

// A pipe read can end in the middle of a UTF-8 character (a request carries
// the fill values, such as a name or an address). The helper keeps the bytes
// until their line is complete instead of decoding each read on its own, and a
// complete line that is not UTF-8 is answered as such without stopping it.
func TestBrowserChromeHelperDecodesSplitUTF8(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript is macOS only")
	}
	b := &asBridge{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer b.release()
	if err := b.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = b.in.Close(); b.kill() }()

	send := func(p []byte) {
		t.Helper()
		if _, err := b.in.Write(p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Pause so that the helper reads what came before on its own.
	splitSend := func(p []byte, at int) {
		t.Helper()
		send(p[:at])
		time.Sleep(300 * time.Millisecond)
		send(p[at:])
	}
	type reply struct {
		ID     int             `json:"id"`
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	next := func() reply {
		t.Helper()
		ch := make(chan []byte, 1)
		go func() {
			l, _ := b.out.ReadBytes('\n')
			ch <- l
		}()
		select {
		case l := <-ch:
			var r reply
			if err := json.Unmarshal(l, &r); err != nil {
				t.Fatalf("reply %q: %v", l, err)
			}
			return r
		case <-time.After(30 * time.Second):
			t.Fatal("no reply from the helper")
		}
		return reply{}
	}
	status := func(id int, pad string) []byte {
		line, _ := json.Marshal(map[string]any{"id": id, "op": "status", "pad": pad})
		return append(line, '\n')
	}

	// Split inside a three-byte character.
	l1 := status(1, "José, Zürich, 価格")
	splitSend(l1, bytes.Index(l1, []byte("価"))+1)
	if r := next(); r.ID != 1 || !r.OK {
		t.Fatalf("split three-byte character: %+v", r)
	}

	// A complete line and the start of the next one, split inside a four-byte character.
	l2, l3 := status(2, "first"), status(3, "smile 😀 done")
	both := append(append([]byte{}, l2...), l3...)
	splitSend(both, len(l2)+bytes.Index(l3, []byte("😀"))+2)
	if r := next(); r.ID != 2 || !r.OK {
		t.Fatalf("first of two: %+v", r)
	}
	if r := next(); r.ID != 3 || !r.OK {
		t.Fatalf("split four-byte character: %+v", r)
	}

	// A complete line that is not UTF-8 is answered, and the helper goes on.
	send([]byte("{\"id\":4,\"op\":\"status\",\"pad\":\"\xff\xfe\"}\n"))
	if r := next(); r.ID != -1 || r.OK || r.Error != "bad request encoding" {
		t.Fatalf("invalid UTF-8: %+v", r)
	}
	send(status(5, "after"))
	if r := next(); r.ID != 5 || !r.OK {
		t.Fatalf("after invalid UTF-8: %+v", r)
	}
}

// The agent's exit never waits for a request in flight (it can wait up to 150 s
// for an Apple Event or macOS's permission prompt): close does not take the
// turn, and nothing reaches a helper after it.
func TestASBridgeCloseDoesNotWaitForTheTurn(t *testing.T) {
	b, f := newFakeASBridge(t)
	if err := b.acquire(context.Background()); err != nil { // a request in flight holds the helper
		t.Fatalf("acquire: %v", err)
	}
	done := make(chan struct{})
	go func() { b.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close waited for the request in flight")
	}
	b.release()
	_, err := b.request(context.Background(), map[string]any{"op": "info", "tabId": "7"})
	var be *browserError
	if !errors.As(err, &be) || be.Code != browserErrChromeNotReachable {
		t.Fatalf("a request after close: %v", err)
	}
	if w := f.written(); len(w) != 0 {
		t.Fatalf("written after close: %q", w)
	}
}

// With the real helper: close ends a reply read in flight at once and the
// process exits, instead of the read waiting out its timeout.
func TestBrowserChromeHelperCloseInterruptsARequestInFlight(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript is macOS only")
	}
	b := &asBridge{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer b.release()
	if err := b.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	cmd := b.cmd
	// A request waiting for a reply that does not come (nothing was sent).
	read := make(chan error, 1)
	go func() { _, err := b.readReply(ctx, 1, 150*time.Second); read <- err }()
	time.Sleep(200 * time.Millisecond)
	started := time.Now()
	b.close()
	if d := time.Since(started); d > time.Second {
		t.Fatalf("close took %v", d)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the read in flight returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read in flight kept waiting after close")
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the helper kept running after close")
	}
}

func asListFixture(t *testing.T, raw string) asList {
	t.Helper()
	var l asList
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

const asTwoWindows = `{"frontmost":%s,"windows":[
 {"minimized":false,"activeTabId":"12","tabs":[{"tabId":"11","title":"Mail","url":"https://mail.example.com/"},{"tabId":"12","title":"Docs","url":"https://docs.example.com/"}]},
 {"minimized":%s,"activeTabId":"21","tabs":[{"tabId":"21","title":"News","url":"https://news.example.com/"},{"tabId":"22","title":"Shop","url":"https://shop.example.com/"}]}
]}`

// The helper's list shape: every window's active tab is active, in window order.
func TestASFocusSnapshotPerWindowActive(t *testing.T) {
	snap := asFocusSnapshot(asListFixture(t, fmt.Sprintf(asTwoWindows, "true", "false")))
	got := map[string]browserTabInfo{}
	for _, tb := range snap.Tabs {
		got[tb.TabID] = tb
	}
	if !isTrue(got["a:12"].Active) || !isTrue(got["a:21"].Active) || isTrue(got["a:11"].Active) || isTrue(got["a:22"].Active) {
		t.Fatalf("per-window active: %+v", snap.Tabs)
	}
	if got["a:12"].Window != 1 || got["a:21"].Window != 2 {
		t.Fatalf("window ordinals: %+v", snap.Tabs)
	}
	if snap.Source != browserFocusSourceWindowOrder || !snap.HasSignal {
		t.Fatalf("%+v", snap)
	}
}

// Window 1's active tab is focused only while Chrome is frontmost; otherwise it
// is the page the person left (windowOrderLast).
func TestASFocusSnapshotFrontmost(t *testing.T) {
	front := asFocusSnapshot(asListFixture(t, fmt.Sprintf(asTwoWindows, "true", "false")))
	res, berr := resolveCurrentPage(front, "", nil)
	if berr != nil || res.Tab.TabID != "a:12" || res.Basis != browserCurrentBasisFocused || front.WindowOrderLast != "" {
		t.Fatalf("%+v %v", res, berr)
	}
	back := asFocusSnapshot(asListFixture(t, fmt.Sprintf(asTwoWindows, "false", "false")))
	for _, tb := range back.Tabs {
		if isTrue(tb.Focused) {
			t.Fatalf("nothing is focused while Chrome is not in front: %+v", tb)
		}
	}
	if back.WindowOrderLast != "a:12" {
		t.Fatalf("windowOrderLast %q", back.WindowOrderLast)
	}
	res, berr = resolveCurrentPage(back, "", nil)
	if berr != nil || res.Tab.TabID != "a:12" || res.Basis != browserCurrentBasisLastActive {
		t.Fatalf("%+v %v", res, berr)
	}
	// The front window's active tab is the app's own: it is never the answer.
	res, berr = resolveCurrentPage(back, "https://docs.example.com", nil)
	if berr == nil {
		t.Fatalf("resolved %+v", res)
	}
}

// A minimized window is never visible, and never the front window.
func TestASFocusSnapshotMinimized(t *testing.T) {
	snap := asFocusSnapshot(asListFixture(t, fmt.Sprintf(asTwoWindows, "false", "true")))
	for _, tb := range snap.Tabs {
		if tb.Window == 2 && (tb.Visible == nil || *tb.Visible) {
			t.Fatalf("a minimized window's tab is visible: %+v", tb)
		}
	}
	minFront := `{"frontmost":false,"windows":[
 {"minimized":true,"activeTabId":"12","tabs":[{"tabId":"12","title":"Docs","url":"https://docs.example.com/"}]},
 {"minimized":false,"activeTabId":"21","tabs":[{"tabId":"21","title":"News","url":"https://news.example.com/"}]}]}`
	snap = asFocusSnapshot(asListFixture(t, minFront))
	if snap.WindowOrderLast != "a:21" {
		t.Fatalf("the first window not minimized leads: %q", snap.WindowOrderLast)
	}
	// Chrome frontmost with window 1 minimized: which window has focus is unknown.
	snap = asFocusSnapshot(asListFixture(t, strings.Replace(minFront, `"frontmost":false`, `"frontmost":true`, 1)))
	if snap.Complete {
		t.Fatal("focus is unknown, so the snapshot is incomplete")
	}
	if _, berr := resolveCurrentPage(snap, "", nil); berr == nil {
		t.Fatal("must not guess")
	}
}

func TestBrowserChromeHelperReportsWindows(t *testing.T) {
	for _, want := range []string{"chrome.frontmost()", "win.minimized()", "activeTabId", "windows: windows"} {
		if !strings.Contains(browserChromeHelperJS, want) {
			t.Errorf("Chrome helper list does not report %s", want)
		}
	}
}
