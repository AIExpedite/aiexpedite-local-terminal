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
// Apple Event at a time.
type asBridge struct {
	mu      sync.Mutex
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

// start launches the helper and waits for its ready line. It sends no Apple Event.
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
	if err := cmd.Start(); err != nil {
		return browserErr(browserErrChromeNotReachable, "could not start the AppleScript helper")
	}
	b.cmd, b.in, b.out, b.errBuf, b.next, b.broken, b.started = cmd, in, bufio.NewReaderSize(outp, 1<<20), eb, 0, nil, true
	if _, err := b.readReply(ctx, 0, 20*time.Second); err != nil {
		b.kill()
		b.broken = err
		return browserErr(browserErrChromeNotReachable, "the AppleScript helper did not start")
	}
	return nil
}

func (b *asBridge) kill() {
	if b.cmd == nil || b.cmd.Process == nil {
		return
	}
	_ = b.cmd.Process.Kill()
	old := b.cmd
	go func() { _ = old.Wait() }()
	b.started = false
}

func (b *asBridge) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.in != nil {
		_ = b.in.Close()
	}
	b.kill()
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
		return browserErr(browserErrChromeNotReachable, "in Chrome, turn on View > Developer > Allow JavaScript from Apple Events (or turn on remote debugging at chrome://inspect/#remote-debugging)")
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started || b.broken != nil {
		if b.started {
			b.kill()
		}
		if err := b.start(ctx); err != nil {
			return nil, err
		}
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
	Active  bool   `json:"active"`
	Loading bool   `json:"loading"`
}

func (a *asBackend) Tabs(ctx context.Context) ([]browserTabInfo, error) {
	res, err := a.b.request(ctx, map[string]any{"op": "list"})
	if err != nil {
		return nil, err
	}
	var tabs []asTab
	if err := json.Unmarshal(res, &tabs); err != nil {
		return nil, errors.New("unreadable tab list")
	}
	out := make([]browserTabInfo, 0, len(tabs))
	for _, t := range tabs {
		if !browserRawTabIDPattern.MatchString(t.TabID) {
			continue
		}
		out = append(out, browserTabInfo{TabID: makeBrowserTabID(browserRouteAppleScript, t.TabID), Title: t.Title, URL: t.URL, Active: t.Active})
	}
	return out, nil
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
	return browserShot{}, browserErr(browserErrScreenshotUnsupport, "screenshots need Chrome's remote debugging: turn it on at chrome://inspect/#remote-debugging")
}

func (p *asPage) Navigate(ctx context.Context, url string) error {
	_, err := p.b.request(ctx, map[string]any{"op": "navigate", "tabId": p.raw, "url": url})
	return err
}

func (p *asPage) History(ctx context.Context, action string) error {
	_, err := p.b.request(ctx, map[string]any{"op": action, "tabId": p.raw})
	return err
}
