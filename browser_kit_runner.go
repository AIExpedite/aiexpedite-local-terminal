// browser_kit_runner.go — the seam between the routes and the in-page kit.
//
// Both routes run the same kit (browser_kit/kit.js) in an isolated world of
// the tab's main frame. Every call sends the kit source; the kit's version
// guard makes re-sending it a no-op once it is loaded in the document.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"time"
)

//go:embed browser_kit/kit.js
var browserKitJS string

var errBrowserUnsupported = errors.New("not supported on this route")

// errGaveUp is returned for every kit call after a page stopped responding:
// the agent does not keep knocking on a page that is stuck.
var errGaveUp = errors.New("gave up on this page: it stopped responding")

type browserTabInfo struct {
	TabID string `json:"tabId"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Active: the tab showing in its window. Focused: the tab with keyboard
	// focus. Visible: its document is visible. Nil = unknown, never false.
	Active  *bool `json:"active,omitempty"`
	Focused *bool `json:"focused,omitempty"`
	Visible *bool `json:"visible,omitempty"`
	// Window: an ordinal within this listing, frontmost first, when known.
	Window int `json:"window,omitempty"`
}

// browserBackend is one route to Chrome.
type browserBackend interface {
	Route() string
	Tabs(ctx context.Context) ([]browserTabInfo, error)
	Open(ctx context.Context, url string) (browserPage, error)
	Page(ctx context.Context, rawTabID string) (browserPage, error)
	TabInfo(ctx context.Context, rawTabID string) (browserTabInfo, error)
	// FocusSnapshot reports every tab with what Chrome says about focus (see
	// browser_focus.go). cheapOnly: return errBrowserFocusNotCheap rather than
	// probe every page (the tabs op never pays the full probe).
	FocusSnapshot(ctx context.Context, cheapOnly bool) (browserFocusSnapshot, error)
	// FocusMemory: the activations recorded for this Chrome instance.
	FocusMemory() *browserFocusMemory
}

// browserPage is one tab on a route.
type browserPage interface {
	TabID() string
	// TrustedInput: the route can type and click as the person would
	// (DevTools Input domain). AppleScript cannot.
	TrustedInput() bool
	Eval(ctx context.Context, code string) (string, error)
	InsertText(ctx context.Context, text string) error
	Click(ctx context.Context, x, y float64) error
	Guard(ctx context.Context, on bool) error
	// GuardWrites: the backstop around an unapproved click. Every document
	// request that is not a GET (a form posted anywhere, a main-frame or an
	// iframe POST) is failed and logged; GETs (a link, an iframe loading) go on.
	GuardWrites(ctx context.Context, on bool) error
	GuardLog() []map[string]any
	Navigate(ctx context.Context, url string) error
	History(ctx context.Context, action string) error
	Screenshot(ctx context.Context, fullPage bool) (browserShot, error)
}

type browserShot struct {
	Data        []byte
	Width       int
	Height      int
	ContentType string
}

func newBrowserShot(data []byte, contentType string) (browserShot, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return browserShot{}, errors.New("Chrome returned an unreadable screenshot")
	}
	return browserShot{Data: data, Width: cfg.Width, Height: cfg.Height, ContentType: contentType}, nil
}

type kitEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Stop   bool            `json:"stop"`
	StopAt int64           `json:"stopAt"`
	DocID  string          `json:"docId"`
}

// kitRunner sends one kit call per Eval.
type kitRunner struct {
	tr        browserPage
	src       string
	lastStop  bool
	lastDocID string
	hung      bool // the page stopped responding; no more calls go to it
}

func newKitRunner(p browserPage) *kitRunner { return &kitRunner{tr: p, src: browserKitJS} }

func (k *kitRunner) call(ctx context.Context, name string, out any, args ...any) (*kitEnvelope, error) {
	if args == nil {
		args = []any{}
	}
	a, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if k.hung {
		return nil, errGaveUp
	}
	n, _ := json.Marshal(name)
	code := k.src + "\n;globalThis.__aixKit.call(" + string(n) + ", " + string(a) + ");"
	raw, err := k.tr.Eval(ctx, code)
	if err != nil {
		if isNotResponding(err) {
			k.hung = true
		}
		return nil, err
	}
	var env kitEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, errors.New("unreadable reply from the page")
	}
	k.lastStop, k.lastDocID = env.Stop, env.DocID
	if !env.OK {
		// The kit's error text is a stack trace from the page world; it stays local.
		return &env, fmt.Errorf("kit %s failed", name)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return &env, fmt.Errorf("kit %s result: %w", name, err)
		}
	}
	return &env, nil
}

type kitPing struct {
	DocID      string `json:"docId"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	ReadyState string `json:"readyState"`
}

func (k *kitRunner) ping(ctx context.Context) (kitPing, error) {
	var p kitPing
	_, err := k.call(ctx, "ping", &p)
	return p, err
}

// waitLoaded waits until the tab shows a fully loaded document other than
// oldDoc, or the same document at another URL (a script-driven page that
// navigated within itself), at most `limit`. With no oldDoc any loaded page
// will do. Returns the last ping (best effort: a page that is slow to finish
// loading still answers with what it has).
func (k *kitRunner) waitLoaded(ctx context.Context, oldDoc, oldURL string, limit time.Duration) (kitPing, error) {
	deadline := time.Now().Add(limit)
	var last kitPing
	var lastErr error
	for time.Now().Before(deadline) && ctx.Err() == nil {
		p, err := k.ping(ctx)
		if err != nil {
			lastErr = err
			if k.hung {
				return last, err
			}
			sleepCtx(ctx, 250)
			continue
		}
		last, lastErr = p, nil
		moved := p.DocID != oldDoc || (oldURL != "" && p.URL != oldURL)
		if moved && p.ReadyState == "complete" && p.URL != "about:blank" {
			return p, nil
		}
		sleepCtx(ctx, 250)
	}
	if last.DocID == "" && lastErr != nil {
		return last, lastErr
	}
	return last, nil
}

// settle waits after a click: until the tab shows a new loaded document, or
// for `quiet` when nothing navigates. navigated reports a new document.
func (k *kitRunner) settle(ctx context.Context, oldDoc string, quiet, limit time.Duration) (kitPing, bool) {
	start := time.Now()
	deadline := start.Add(limit)
	var last kitPing
	for time.Now().Before(deadline) && ctx.Err() == nil {
		sleepCtx(ctx, 200)
		p, err := k.ping(ctx)
		if err != nil {
			if k.hung {
				return last, false
			}
			continue // a navigation in progress: the old world is gone, the new one not ready
		}
		last = p
		if p.DocID != oldDoc {
			if p.ReadyState == "complete" {
				return p, true
			}
			continue
		}
		if time.Since(start) >= quiet {
			return p, false
		}
	}
	return last, last.DocID != "" && last.DocID != oldDoc
}

func sleepCtx(ctx context.Context, ms int) {
	if ms <= 0 {
		return
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
