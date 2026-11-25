// browser_applescript.go — the AppleScript route (macOS only): Chrome's own
// `execute javascript`, through one long-lived osascript (JXA) helper.
//
// It needs "View > Developer > Allow JavaScript from Apple Events" in Chrome
// and macOS's permission for AI Expedite to control Google Chrome (the app's
// NSAppleEventsUsageDescription and the automation.apple-events entitlement).
// Chrome runs the script in its AppleScript isolated world, in the tab's main
// frame. There is no trusted input, network view or screenshot on this route.
//
// The file compiles everywhere; chooseBrowserRoute only picks it on darwin.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed browser_kit/chrome_helper.js
var browserChromeHelperJS string

type capBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.b) < c.max {
		room := c.max - len(c.b)
		if len(p) < room {
			room = len(p)
		}
		c.b = append(c.b, p[:room]...)
	}
	return len(p), nil
}

// errASTimeout: no reply in time. The helper is killed (its Apple Event may
// never come back) and the next call starts a fresh one, which needs no new
// macOS prompt once the first was allowed.
var errASTimeout = errors.New("AppleScript call timed out")

// asBridge is the one osascript helper process. Requests are serialized: one
// Apple Event at a time, for every tab. The turn is a one-slot channel, not a
// mutex, so that waiting for it ends with the caller's context: a request whose
// deadline passes while another tab's Apple Event holds the helper is never
// sent (it must not navigate or type after its command was reported as timed
// out) and does not kill the helper the other call is using.
type asBridge struct {
	turnOnce sync.Once
	turn     chan struct{}

	// procMu guards cmd and in, which close reads without the turn (an agent
	// exit never waits for a request in flight); closed refuses everything after.
	procMu sync.Mutex
	closed atomic.Bool

	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Reader
	errBuf  *capBuffer
	next    int
	broken  error
	granted bool
	started bool
}

var globalASBridge = &asBridge{}

// errASNotSent: the command's time ran out before its request reached the
// helper (another tab's call held it, or the helper was still starting).
// Nothing was sent to Chrome.
func errASNotSent() *browserError {
	return browserErr(browserErrCommandTimeout, "the browser action's time ran out before it reached Chrome; nothing was sent")
}

// acquire takes the helper's turn, or gives up when ctx ends first. A context
// that has already ended never takes the turn: select picks at random among
// ready cases, so it is checked before and again after.
func (b *asBridge) acquire(ctx context.Context) error {
	b.turnOnce.Do(func() { b.turn = make(chan struct{}, 1) })
	if ctx.Err() != nil {
		return errASNotSent()
	}
	select {
	case b.turn <- struct{}{}:
		if ctx.Err() != nil {
			<-b.turn
			return errASNotSent()
		}
		return nil
	case <-ctx.Done():
		return errASNotSent()
	}
}

func (b *asBridge) release() { <-b.turn }

// start launches the helper and waits for its ready line. It sends no Apple
// Event. The caller holds the turn.
func (b *asBridge) start(ctx context.Context) error {
	cmd := exec.Command("osascript", "-l", "JavaScript", "-e", browserChromeHelperJS)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	outp, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	eb := &capBuffer{max: 16 << 10}
	cmd.Stderr = eb
	if b.closed.Load() {
		return errASClosed()
	}
	if err := cmd.Start(); err != nil {
		return browserErr(browserErrChromeNotReachable, "could not start the AppleScript helper")
	}
	b.procMu.Lock()
	b.cmd, b.in = cmd, in
	b.procMu.Unlock()
	b.out, b.errBuf, b.next, b.broken, b.started = bufio.NewReaderSize(outp, 1<<20), eb, 0, nil, true
	if b.closed.Load() {
		// close ran while this one started: it may have missed the new process.
		b.kill()
		return errASClosed()
	}
	if _, err := b.readReply(ctx, 0, 20*time.Second); err != nil {
		b.kill()
		b.broken = err
		return browserErr(browserErrChromeNotReachable, "the AppleScript helper did not start")
	}
	return nil
}

func (b *asBridge) kill() {
	b.procMu.Lock()
	old := b.cmd
	b.procMu.Unlock()
	if old == nil || old.Process == nil {
		return
	}
	_ = old.Process.Kill()
	go func() { _ = old.Wait() }()
	b.started = false
}

func errASClosed() *browserError {
	return browserErr(browserErrChromeNotReachable, "the desktop app is shutting down")
}

// close ends the helper at once, for the agent's exit. It never waits for the
// turn: a request in flight can wait up to 150 s (a slow Apple Event, macOS's
// first-use permission prompt), so it is interrupted instead. Killing the
// process ends that request's reply read, so it returns an error and releases
// the turn itself; nothing starts a helper again afterwards.
func (b *asBridge) close() {
	b.closed.Store(true)
	b.procMu.Lock()
	cmd, in := b.cmd, b.in
	b.procMu.Unlock()
	if in != nil {
		_ = in.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

type asReplyError struct {
	Msg  string
	Code int
}

func (e *asReplyError) Error() string { return fmt.Sprintf("AppleScript error %d", e.Code) }

// friendlyAppleScriptError maps the helper's error to a BROWSER_* code. The
// raw message is used only to classify; it is never logged or returned.
func friendlyAppleScriptError(msg string, code int) *browserError {
	switch {
	case code == -1743 || strings.Contains(msg, "Not authorized to send Apple events"):
		return browserErr(browserErrChromeDenied, "macOS has not allowed AI Expedite to control Google Chrome: allow it in System Settings > Privacy & Security > Automation")
	case strings.Contains(msg, "Executing JavaScript through AppleScript is turned off"), strings.Contains(msg, "JavaScript through AppleScript"):
		return browserErr(browserErrChromeNotReachable, "in Chrome, turn on View > Developer > Allow JavaScript from Apple Events (or turn on remote debugging from the setup card in the chat)")
	case strings.Contains(msg, "chrome-not-running"):
		return browserErr(browserErrChromeNotReachable, "Google Chrome is not running")
	case strings.Contains(msg, "tab-not-found"):
		return browserErr(browserErrTabNotFound, "that tab is closed; list the tabs again")
	case strings.Contains(msg, "DevTools are not allowed"):
		return browserErr(browserErrChromeNotReachable, "Chrome's policy blocks scripts on this page")
	}
	return browserErr(browserErrChromeNotReachable, "Chrome did not accept the AppleScript request (error %d)", code)
}

func (b *asBridge) readReply(ctx context.Context, want int, timeout time.Duration) (json.RawMessage, error) {
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	out := b.out
	go func() {
		l, err := out.ReadBytes('\n')
		ch <- res{l, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			b.broken = errors.New("the AppleScript helper exited")
			return nil, b.broken
		}
		var rep struct {
			ID     int             `json:"id"`
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
			Code   int             `json:"code"`
		}
		if err := json.Unmarshal(r.line, &rep); err != nil {
			b.kill()
			b.broken = errors.New("bad reply from the AppleScript helper")
			return nil, b.broken
		}
		if rep.ID != want {
			b.kill()
			b.broken = errors.New("the AppleScript helper answered out of order")
			return nil, b.broken
		}
		if !rep.OK {
			return nil, &asReplyError{Msg: rep.Error, Code: rep.Code}
		}
		return rep.Result, nil
	case <-timer.C:
		b.kill()
		b.broken = errASTimeout
		return nil, errASTimeout
	case <-ctx.Done():
		b.kill()
		b.broken = errASTimeout
		return nil, ctx.Err()
	}
}

func (b *asBridge) request(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if err := b.acquire(ctx); err != nil {
		return nil, err
	}
	defer b.release()
	if b.closed.Load() {
		return nil, errASClosed()
	}
	if !b.started || b.broken != nil {
		if b.started {
			b.kill()
		}
		if err := b.start(ctx); err != nil {
			return nil, err
		}
	}
	// Starting the helper takes time; nothing reaches Chrome after the deadline.
	if ctx.Err() != nil {
		return nil, errASNotSent()
	}
	b.next++
	req["id"] = b.next
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := b.in.Write(append(data, '\n')); err != nil {
		b.kill()
		b.broken = errors.New("the AppleScript helper is gone")
		return nil, browserErr(browserErrChromeNotReachable, "the AppleScript helper stopped")
	}
	op, _ := req["op"].(string)
	timeout := browserCallTimeout
	if op == "exec" {
		timeout = pageCallTimeout // the page itself must answer
	}
	if !b.granted {
		// The first Apple Event can sit behind macOS's "wants to control Google Chrome" prompt.
		timeout = 150 * time.Second
	}
	res, err := b.readReply(ctx, b.next, timeout)
	var re *asReplyError
	switch {
	case err == nil:
		b.granted = true
		return res, nil
	case errors.As(err, &re):
		b.granted = b.granted || re.Code != -1743
		return nil, friendlyAppleScriptError(re.Msg, re.Code)
	case errors.Is(err, errASTimeout):
		if !b.granted {
			return nil, browserErr(browserErrChromeDenied, "macOS's permission for AI Expedite to control Google Chrome was not answered")
		}
		if op == "exec" {
			return nil, &notRespondingError{What: "execute javascript", After: timeout}
		}
		return nil, browserErr(browserErrChromeNotReachable, "Chrome did not answer the AppleScript %q call in time", op)
	}
	return nil, err
}

// ---------------------------------------------------------------- the route

type asBackend struct {
	b *asBridge
}

func (a *asBackend) Route() string { return browserRouteAppleScript }

type asTab struct {
	TabID   string `json:"tabId"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Loading bool   `json:"loading"`
}

// asList is the helper's `list` answer: Chrome's windows front to back.
type asList struct {
	Frontmost bool `json:"frontmost"`
	Windows   []struct {
		Minimized   bool    `json:"minimized"`
		ActiveTabID string  `json:"activeTabId"`
		Tabs        []asTab `json:"tabs"`
	} `json:"windows"`
}

func (a *asBackend) list(ctx context.Context) (asList, error) {
	var l asList
	res, err := a.b.request(ctx, map[string]any{"op": "list"})
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(res, &l); err != nil {
		return l, errors.New("unreadable tab list")
	}
	return l, nil
}

// asFocusSnapshot reads focus from window order. Window 1 is the front window:
// its active tab has focus exactly when Chrome is the frontmost app (and the
// window is not minimized). When Chrome is not in front, that tab is the page
// the person left (WindowOrderLast). A minimized window shows nothing; whether
// another window covers one is not reported, so Visible is left unknown there.
func asFocusSnapshot(l asList) browserFocusSnapshot {
	snap := browserFocusSnapshot{Source: browserFocusSourceWindowOrder, Complete: true, HasSignal: true}
	front := true // the first window not minimized
	for i, w := range l.Windows {
		lead := front && !w.Minimized
		if lead {
			front = false
		}
		for _, t := range w.Tabs {
			if !browserRawTabIDPattern.MatchString(t.TabID) {
				continue
			}
			id := makeBrowserTabID(browserRouteAppleScript, t.TabID)
			active := t.TabID == w.ActiveTabID
			info := browserTabInfo{TabID: id, Title: t.Title, URL: t.URL, Active: boolPtr(active), Window: i + 1}
			switch {
			case active && lead && l.Frontmost && i > 0:
				// Window 1 is minimized: which window macOS focused is not known.
				snap.Complete = false
			default:
				info.Focused = boolPtr(active && lead && l.Frontmost)
			}
			if w.Minimized || !active {
				info.Visible = boolPtr(false)
			}
			if active && lead && !l.Frontmost {
				snap.WindowOrderLast = id
			}
			snap.Tabs = append(snap.Tabs, info)
		}
	}
	return snap
}

func (a *asBackend) Tabs(ctx context.Context) ([]browserTabInfo, error) {
	l, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	return asFocusSnapshot(l).Tabs, nil
}

// FocusSnapshot costs the same Apple Events as Tabs: always cheap.
func (a *asBackend) FocusSnapshot(ctx context.Context, cheapOnly bool) (browserFocusSnapshot, error) {
	l, err := a.list(ctx)
	if err != nil {
		return browserFocusSnapshot{}, err
	}
	return asFocusSnapshot(l), nil
}

func (a *asBackend) FocusMemory() *browserFocusMemory {
	return browserFocusMemoryFor(browserRouteAppleScript)
}

func (a *asBackend) Open(ctx context.Context, url string) (browserPage, error) {
	res, err := a.b.request(ctx, map[string]any{"op": "open", "url": url})
	if err != nil {
		return nil, err
	}
	var t asTab
	if err := json.Unmarshal(res, &t); err != nil || !browserRawTabIDPattern.MatchString(t.TabID) {
		return nil, errors.New("Chrome did not say which tab it opened")
	}
	return &asPage{b: a.b, raw: t.TabID}, nil
}

func (a *asBackend) Page(ctx context.Context, rawTabID string) (browserPage, error) {
	if _, err := a.b.request(ctx, map[string]any{"op": "info", "tabId": rawTabID}); err != nil {
		return nil, err
	}
	return &asPage{b: a.b, raw: rawTabID}, nil
}

func (a *asBackend) TabInfo(ctx context.Context, rawTabID string) (browserTabInfo, error) {
	res, err := a.b.request(ctx, map[string]any{"op": "info", "tabId": rawTabID})
	if err != nil {
		return browserTabInfo{}, err
	}
	var t asTab
	_ = json.Unmarshal(res, &t)
	return browserTabInfo{TabID: makeBrowserTabID(browserRouteAppleScript, rawTabID), Title: t.Title, URL: t.URL}, nil
}

type asPage struct {
	b   *asBridge
	raw string
}

func (p *asPage) TabID() string      { return makeBrowserTabID(browserRouteAppleScript, p.raw) }
func (p *asPage) TrustedInput() bool { return false }

func (p *asPage) Eval(ctx context.Context, code string) (string, error) {
	res, err := p.b.request(ctx, map[string]any{"op": "exec", "tabId": p.raw, "code": code})
	if err != nil {
		return "", err
	}
	var s *string
	if err := json.Unmarshal(res, &s); err != nil || s == nil {
		return "", errors.New("unexpected AppleScript result")
	}
	return *s, nil
}

func (p *asPage) InsertText(ctx context.Context, text string) error { return errBrowserUnsupported }
func (p *asPage) Click(ctx context.Context, x, y float64) error     { return errBrowserUnsupported }
func (p *asPage) Guard(ctx context.Context, on bool) error          { return errBrowserUnsupported }
func (p *asPage) GuardLog() []map[string]any                        { return nil }
func (p *asPage) GuardWrites(ctx context.Context, on bool) error {
	return errBrowserUnsupported
}
func (p *asPage) Screenshot(ctx context.Context, full bool) (browserShot, error) {
	return browserShot{}, browserErr(browserErrScreenshotUnsupport, "screenshots need Chrome's remote debugging: turn it on from the setup card in the chat")
}

func (p *asPage) Navigate(ctx context.Context, url string) error {
	_, err := p.b.request(ctx, map[string]any{"op": "navigate", "tabId": p.raw, "url": url})
	return err
}

func (p *asPage) History(ctx context.Context, action string) error {
	_, err := p.b.request(ctx, map[string]any{"op": action, "tabId": p.raw})
	return err
}
