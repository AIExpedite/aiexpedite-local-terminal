package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A prepared submit is approved up to 15 minutes later (plus the 10 s undo and
// the execute grace): the DevTools connection, and with it the kit's refs,
// must outlive that.
func TestBrowserCDPPrepareHoldCoversALateApproval(t *testing.T) {
	const requestTTL = 15 * time.Minute // BROWSER_SUBMIT_REQUEST_TTL_MS
	const undo = 10 * time.Second       // BROWSER_SUBMIT_UNDO_MS
	if browserCDPPrepareHold < 20*time.Minute || browserCDPPrepareHold < requestTTL+undo+time.Minute {
		t.Fatalf("browserCDPPrepareHold = %v", browserCDPPrepareHold)
	}
}

// fakeCDPServer accepts DevTools-like WebSocket connections and counts the
// ones still open.
func fakeCDPServer(t *testing.T) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	open := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		open++
		mu.Unlock()
		defer func() {
			mu.Lock()
			open--
			mu.Unlock()
		}()
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), func() int {
		mu.Lock()
		defer mu.Unlock()
		return open
	}
}

func testCDPManager(url string, idle time.Duration) *cdpManager {
	return &cdpManager{idleFor: idle, dial: func(ctx context.Context, _ string, onEvent func(string, string, json.RawMessage)) (*cdpClient, error) {
		return dialCDP(ctx, url, onEvent)
	}}
}

func waitOpenConns(t *testing.T, count func() int, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for count() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open DevTools connections = %d, want %d", count(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCDPManagerHoldKeepsTheConnectionPastIdle(t *testing.T) {
	url, count := fakeCDPServer(t)
	ep := cdpEndpoint{Port: 1, WSPath: "/devtools/browser/x"}

	// Without a hold the connection closes after the idle period.
	m := testCDPManager(url, 60*time.Millisecond)
	_, release, err := m.acquire(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	waitOpenConns(t, count, 1, 2*time.Second)
	release()
	waitOpenConns(t, count, 0, 2*time.Second)

	// A hold taken while in use (the prepareSubmit path) outlives the idle
	// period, and a later op's release does not shorten it.
	m = testCDPManager(url, 60*time.Millisecond)
	_, release, err = m.acquire(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	m.holdFor(900 * time.Millisecond)
	release()
	time.Sleep(200 * time.Millisecond)
	_, release2, err := m.acquire(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	release2()
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("closed during the hold: %d open", n)
	}
	waitOpenConns(t, count, 0, 3*time.Second)

	// A hold taken while unused re-arms the idle clock.
	m = testCDPManager(url, 60*time.Millisecond)
	_, release, err = m.acquire(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	release()
	m.holdFor(600 * time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("an unused connection closed during its hold: %d open", n)
	}
	waitOpenConns(t, count, 0, 3*time.Second)
}

// The dispatcher takes the hold after a successful prepareSubmit on the
// DevTools route, before releasing the connection; never for other ops, a
// failed prepare or the AppleScript route.
func TestBrowserDispatcherHoldsAfterPrepareSubmit(t *testing.T) {
	prep := func(args []json.RawMessage) any {
		return map[string]any{"ok": true, "url": "https://ex.com/f", "origin": "https://ex.com", "title": "F", "submitRef": "f3_a",
			"submitText": "Send", "formName": "Contact", "token": "t",
			"parts":  map[string]any{"origin": "https://ex.com", "path": "/f", "form": "ff", "kind": "form", "submit": "ss", "fields": []any{}},
			"fields": []any{}}
	}
	for _, c := range []struct {
		name, op, route string
		ok, wantHold    bool
	}{
		{"devtools prepare", browserOpPrepareSubmit, browserRouteDevTools, true, true},
		{"failed prepare", browserOpPrepareSubmit, browserRouteDevTools, false, false},
		{"applescript prepare", browserOpPrepareSubmit, browserRouteAppleScript, true, false},
		{"tabs", browserOpTabs, browserRouteDevTools, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			kit := map[string]func([]json.RawMessage) any{"prepareSubmit": prep}
			if !c.ok {
				kit["prepareSubmit"] = func([]json.RawMessage) any { return map[string]any{"ok": false, "reason": "no-form"} }
			}
			h := newDispatcherHarness(t, &fakePage{id: "d:T1", kit: kit})
			h.be.route = c.route
			var mu sync.Mutex
			var events []string
			h.d.route = func(ctx context.Context, tabRoute string) (browserRouteChoice, *browserError) {
				return browserRouteChoice{Route: c.route}, nil
			}
			h.d.open = func(ctx context.Context, rc browserRouteChoice) (browserBackend, func(), error) {
				return h.be, func() { mu.Lock(); events = append(events, "release"); mu.Unlock() }, nil
			}
			h.d.hold = func(d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				if d != browserCDPPrepareHold {
					t.Errorf("hold %v", d)
				}
				events = append(events, "hold")
			}
			tab := "d:T1"
			if c.route == browserRouteAppleScript {
				tab = "a:T1"
			}
			h.send(t, map[string]any{"commandId": "c1", "op": c.op, "tabId": tab})
			h.wait(t, 1)
			mu.Lock()
			defer mu.Unlock()
			got := strings.Join(events, ",")
			want := "release"
			if c.wantHold {
				want = "hold,release"
			}
			if got != want {
				t.Fatalf("events %q, want %q (result %+v)", got, want, h.poster.byID("c1"))
			}
		})
	}
}
