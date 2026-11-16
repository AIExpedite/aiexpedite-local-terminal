// browser_cdp.go — the DevTools route: one WebSocket to the person's Chrome.
//
// Chrome grants a DevTools client the whole browser; cdpAllowlist is the
// narrowing: every method this agent may send, refused locally otherwise. No
// Network.*, Storage.*, cookies, Browser.* or closing tabs, ever.
//
// Chrome asks "Allow remote debugging?" once per connection, so ONE connection
// is kept open while commands use it and closed after browserCDPIdleClose
// without one. Each tab gets one flattened session and one isolated world for
// the kit, kept for the connection's life, so refs from a read stay valid for
// the fill that follows.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

var cdpAllowlist = map[string]bool{
	"Target.getTargets":           true,
	"Target.attachToTarget":       true,
	"Target.detachFromTarget":     true,
	"Target.createTarget":         true,
	"Page.getFrameTree":           true,
	"Page.createIsolatedWorld":    true,
	"Page.navigate":               true,
	"Page.navigateToHistoryEntry": true,
	"Page.getNavigationHistory":   true,
	"Page.reload":                 true,
	"Page.captureScreenshot":      true,
	"Page.getLayoutMetrics":       true,
	"Runtime.evaluate":            true,
	"Input.insertText":            true,
	"Input.dispatchMouseEvent":    true,
	// Network backstop, used only between begin and end of a fill (see Guard).
	"Fetch.enable":          true,
	"Fetch.disable":         true,
	"Fetch.failRequest":     true,
	"Fetch.continueRequest": true,
}

// pageCallTimeout bounds every call that the page itself must answer. A page
// whose scripts keep its main thread busy for longer is reported as not
// responding, and the agent gives up on that page instead of waiting forever.
const pageCallTimeout = 15 * time.Second

// browserCallTimeout bounds calls that Chrome answers without the page.
const browserCallTimeout = 10 * time.Second

// cdpConsentWait is how long the WebSocket upgrade may wait while Chrome shows
// "Allow remote debugging?" (bounded further by the command's deadline).
const cdpConsentWait = 120 * time.Second

const isolatedWorldName = "aix_browser"

type notRespondingError struct {
	What  string
	After time.Duration
}

func (e *notRespondingError) Error() string {
	return fmt.Sprintf("the page did not answer %s within %s (its scripts may be stuck)", e.What, e.After)
}

func isNotResponding(err error) bool {
	var e *notRespondingError
	return errors.As(err, &e)
}

var errCDPDenied = errors.New("Chrome refused the connection: Deny was clicked, the dialog was closed, or no Chrome window was open")

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("CDP %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("CDP %d: %s", e.Code, e.Message)
}

type cdpReply struct {
	result json.RawMessage
	err    error
}

type cdpClient struct {
	ws       *websocket.Conn
	next     atomic.Int64
	mu       sync.Mutex
	pending  map[int64]chan cdpReply
	closeErr error
	onEvent  func(method, sessionID string, params json.RawMessage)
}

func dialCDP(ctx context.Context, url string, onEvent func(string, string, json.RawMessage)) (*cdpClient, error) {
	// No Origin header: Chrome rejects a WebSocket whose Origin is not its own,
	// which is what keeps web pages from driving this endpoint.
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			return nil, errCDPDenied
		}
		return nil, err
	}
	ws.SetReadLimit(64 << 20)
	c := &cdpClient{ws: ws, pending: map[int64]chan cdpReply{}, onEvent: onEvent}
	go c.readLoop()
	return c, nil
}

func (c *cdpClient) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr == nil
}

func (c *cdpClient) readLoop() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.closeErr = fmt.Errorf("DevTools connection closed: %w", err)
			for id, ch := range c.pending {
				ch <- cdpReply{err: c.closeErr}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		var m struct {
			ID        int64           `json:"id"`
			Result    json.RawMessage `json:"result"`
			Error     *cdpError       `json:"error"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- cdpReply{err: m.Error}
				} else {
					ch <- cdpReply{result: m.Result}
				}
			}
			continue
		}
		if m.Method != "" && c.onEvent != nil {
			c.onEvent(m.Method, m.SessionID, m.Params)
		}
	}
}

func (c *cdpClient) Call(ctx context.Context, sessionID, method string, params any, out any) error {
	return c.callWithin(ctx, sessionID, method, params, out, 0)
}

func (c *cdpClient) callWithin(ctx context.Context, sessionID, method string, params any, out any, limit time.Duration) error {
	if !cdpAllowlist[method] {
		return fmt.Errorf("refused locally: CDP method %s is not on the allowlist", method)
	}
	id := c.next.Add(1)
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if limit == 0 {
		limit = browserCallTimeout
		if sessionID != "" {
			limit = pageCallTimeout
		}
	}
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ch := make(chan cdpReply, 1)
	c.mu.Lock()
	if c.closeErr != nil {
		c.mu.Unlock()
		return c.closeErr
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.Write(cctx, websocket.MessageText, data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-cctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err() // the caller gave up, not the page
		}
		if sessionID != "" {
			return &notRespondingError{What: method, After: limit}
		}
		return fmt.Errorf("Chrome did not answer %s within %s", method, limit)
	}
}

func (c *cdpClient) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "done")
}

// ---------------------------------------------------------------- the connection

// cdpConn is one DevTools connection and the tab sessions attached over it.
type cdpConn struct {
	c     *cdpClient
	ep    cdpEndpoint
	mu    sync.Mutex
	pages map[string]*cdpPage // targetId -> page
}

func (cn *cdpConn) onEvent(method, sessionID string, params json.RawMessage) {
	switch method {
	case "Target.detachedFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)
		cn.mu.Lock()
		for id, pg := range cn.pages {
			if pg.sessionID == p.SessionID {
				pg.detached.Store(true)
				delete(cn.pages, id)
			}
		}
		cn.mu.Unlock()
	case "Fetch.requestPaused":
		cn.mu.Lock()
		var page *cdpPage
		for _, pg := range cn.pages {
			if pg.sessionID == sessionID {
				page = pg
			}
		}
		cn.mu.Unlock()
		if page == nil {
			return
		}
		var p struct {
			RequestID string `json:"requestId"`
			FrameID   string `json:"frameId"`
			Request   struct {
				URL    string `json:"url"`
				Method string `json:"method"`
			} `json:"request"`
		}
		_ = json.Unmarshal(params, &p)
		go page.answerPaused(p.RequestID, p.FrameID, p.Request.Method, p.Request.URL)
	}
}

type cdpTarget struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

func (cn *cdpConn) targets(ctx context.Context) ([]cdpTarget, error) {
	var r struct {
		TargetInfos []cdpTarget `json:"targetInfos"`
	}
	if err := cn.c.Call(ctx, "", "Target.getTargets", nil, &r); err != nil {
		return nil, err
	}
	var out []cdpTarget
	for _, t := range r.TargetInfos {
		if t.Type == "page" {
			out = append(out, t)
		}
	}
	return out, nil
}

// page returns the attached session for a target, attaching on first use.
func (cn *cdpConn) page(ctx context.Context, targetID string) (*cdpPage, error) {
	cn.mu.Lock()
	if pg := cn.pages[targetID]; pg != nil && !pg.detached.Load() {
		cn.mu.Unlock()
		return pg, nil
	}
	cn.mu.Unlock()
	targets, err := cn.targets(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, t := range targets {
		if t.TargetID == targetID {
			found = true
		}
	}
	if !found {
		return nil, browserErr(browserErrTabNotFound, "that tab is closed or is not a web page; list the tabs again")
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := cn.c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &a); err != nil {
		return nil, err
	}
	pg := &cdpPage{conn: cn, targetID: targetID, sessionID: a.SessionID}
	if err := pg.ensureWorld(ctx); err != nil && !isNotResponding(err) {
		_ = cn.c.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": a.SessionID}, nil)
		return nil, err
	}
	cn.mu.Lock()
	if old := cn.pages[targetID]; old != nil && !old.detached.Load() {
		cn.mu.Unlock()
		_ = cn.c.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": a.SessionID}, nil)
		return old, nil
	}
	cn.pages[targetID] = pg
	cn.mu.Unlock()
	return pg, nil
}

// ---------------------------------------------------------------- the manager

// cdpManager owns the ONE connection. acquire hands it out (dialling when there
// is none, or when Chrome restarted on another endpoint); release starts the
// idle clock when nobody uses it.
type cdpManager struct {
	mu      sync.Mutex
	conn    *cdpConn
	users   int
	dialing chan struct{}
	idle    *time.Timer
	idleFor time.Duration
	dial    func(ctx context.Context, url string, onEvent func(string, string, json.RawMessage)) (*cdpClient, error)
}

var globalCDPManager = &cdpManager{idleFor: browserCDPIdleClose, dial: dialCDP}

func (m *cdpManager) acquire(ctx context.Context, ep cdpEndpoint) (*cdpConn, func(), error) {
	for {
		m.mu.Lock()
		if m.conn != nil && (!m.conn.c.alive() || m.conn.ep.Port != ep.Port || m.conn.ep.WSPath != ep.WSPath) {
			if m.users == 0 || !m.conn.c.alive() {
				old := m.conn
				m.conn = nil
				go old.c.close()
			}
		}
		if m.conn != nil && m.conn.ep.Port == ep.Port && m.conn.ep.WSPath == ep.WSPath {
			m.users++
			if m.idle != nil {
				m.idle.Stop()
				m.idle = nil
			}
			cn := m.conn
			m.mu.Unlock()
			return cn, m.releaser(), nil
		}
		if m.dialing != nil {
			ch := m.dialing
			m.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		m.dialing = ch
		m.mu.Unlock()

		cn := &cdpConn{ep: ep, pages: map[string]*cdpPage{}}
		dctx, cancel := context.WithTimeout(ctx, cdpConsentWait)
		c, err := m.dial(dctx, ep.wsURL(), cn.onEvent)
		cancel()
		m.mu.Lock()
		m.dialing = nil
		close(ch)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		cn.c = c
		if m.conn != nil {
			old := m.conn
			go old.c.close()
		}
		m.conn = cn
		m.users++
		m.mu.Unlock()
		return cn, m.releaser(), nil
	}
}

func (m *cdpManager) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.users > 0 {
				m.users--
			}
			if m.users == 0 && m.conn != nil {
				if m.idle != nil {
					m.idle.Stop()
				}
				m.idle = time.AfterFunc(m.idleFor, m.closeIfIdle)
			}
		})
	}
}

func (m *cdpManager) closeIfIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.users == 0 && m.conn != nil {
		m.conn.c.close()
		m.conn = nil
	}
	m.idle = nil
}

// closeAll drops the connection now (agent shutdown).
func (m *cdpManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idle != nil {
		m.idle.Stop()
		m.idle = nil
	}
	if m.conn != nil {
		m.conn.c.close()
		m.conn = nil
	}
}

// ---------------------------------------------------------------- one tab

type cdpPage struct {
	conn      *cdpConn
	targetID  string
	sessionID string
	worldMu   sync.Mutex
	frameID   string
	contextID int64
	detached  atomic.Bool
	guardMu   sync.Mutex
	guardLog  []map[string]any
}

func (p *cdpPage) TabID() string      { return makeBrowserTabID(browserRouteDevTools, p.targetID) }
func (p *cdpPage) TrustedInput() bool { return true }

func (p *cdpPage) ensureWorld(ctx context.Context) error {
	var ft struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.conn.c.Call(ctx, p.sessionID, "Page.getFrameTree", nil, &ft); err != nil {
		return err
	}
	var w struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if err := p.conn.c.Call(ctx, p.sessionID, "Page.createIsolatedWorld",
		map[string]any{"frameId": ft.FrameTree.Frame.ID, "worldName": isolatedWorldName, "grantUniveralAccess": false}, &w); err != nil {
		return err
	}
	p.worldMu.Lock()
	p.frameID, p.contextID = ft.FrameTree.Frame.ID, w.ExecutionContextID
	p.worldMu.Unlock()
	return nil
}

func (p *cdpPage) Eval(ctx context.Context, code string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if p.detached.Load() {
			return "", browserErr(browserErrTabNotFound, "the tab was closed")
		}
		p.worldMu.Lock()
		contextID := p.contextID
		p.worldMu.Unlock()
		var r struct {
			Result struct {
				Type  string `json:"type"`
				Value any    `json:"value"`
			} `json:"result"`
			ExceptionDetails *struct {
				Text      string `json:"text"`
				Exception *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		err := p.conn.c.Call(ctx, p.sessionID, "Runtime.evaluate",
			map[string]any{"expression": code, "contextId": contextID, "returnByValue": true, "awaitPromise": false}, &r)
		if err != nil {
			if isNotResponding(err) || ctx.Err() != nil {
				return "", err
			}
			lastErr = err
			// A navigation destroys the isolated world: make a new one in the new document.
			if strings.Contains(err.Error(), "context") || contextID == 0 {
				sleepCtx(ctx, 150)
				if e := p.ensureWorld(ctx); e != nil {
					lastErr = e
				}
				continue
			}
			return "", err
		}
		if r.ExceptionDetails != nil {
			// Never the description: an exception's text can quote page content.
			return "", errors.New("the kit raised an exception in the page")
		}
		s, ok := r.Result.Value.(string)
		if !ok {
			return "", fmt.Errorf("unexpected result type %q", r.Result.Type)
		}
		return s, nil
	}
	return "", lastErr
}

func (p *cdpPage) InsertText(ctx context.Context, text string) error {
	return p.conn.c.Call(ctx, p.sessionID, "Input.insertText", map[string]any{"text": text}, nil)
}

// Click presses and releases the left button at a point of the viewport (CSS
// pixels): a trusted click, as if the person did it.
func (p *cdpPage) Click(ctx context.Context, x, y float64) error {
	for _, ev := range []map[string]any{
		{"type": "mouseMoved", "x": x, "y": y, "button": "none", "buttons": 0},
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": 1},
	} {
		if err := p.conn.c.Call(ctx, p.sessionID, "Input.dispatchMouseEvent", ev, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *cdpPage) Navigate(ctx context.Context, url string) error {
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.conn.c.Call(ctx, p.sessionID, "Page.navigate", map[string]any{"url": url}, &r); err != nil {
		return err
	}
	return nil // a failed load still shows Chrome's error page in the tab
}

func (p *cdpPage) History(ctx context.Context, action string) error {
	if action == browserNavReload {
		return p.conn.c.Call(ctx, p.sessionID, "Page.reload", map[string]any{}, nil)
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int64 `json:"id"`
		} `json:"entries"`
	}
	if err := p.conn.c.Call(ctx, p.sessionID, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	i := h.CurrentIndex - 1
	if action == browserNavForward {
		i = h.CurrentIndex + 1
	}
	if i < 0 || i >= len(h.Entries) {
		return browserErr(browserErrCommandInvalid, "there is no page to go %s to", action)
	}
	return p.conn.c.Call(ctx, p.sessionID, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
}

// Screenshot captures the viewport, or the whole page (capped), as JPEG; the
// quality steps down until the image fits BROWSER_LIMITS.MAX_SCREENSHOT_BYTES.
func (p *cdpPage) Screenshot(ctx context.Context, fullPage bool) (browserShot, error) {
	base := map[string]any{"format": "jpeg"}
	if fullPage {
		var m struct {
			CSSContentSize struct {
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"cssContentSize"`
		}
		if err := p.conn.c.Call(ctx, p.sessionID, "Page.getLayoutMetrics", nil, &m); err != nil {
			return browserShot{}, err
		}
		w, h := m.CSSContentSize.Width, m.CSSContentSize.Height
		if w <= 0 || h <= 0 {
			fullPage = false
		} else {
			if w > 4096 {
				w = 4096
			}
			if h > 16384 {
				h = 16384
			}
			base["captureBeyondViewport"] = true
			base["clip"] = map[string]any{"x": 0, "y": 0, "width": w, "height": h, "scale": 1}
		}
	}
	for _, q := range []int{80, 60, 40} {
		params := map[string]any{}
		for k, v := range base {
			params[k] = v
		}
		params["quality"] = q
		var r struct {
			Data string `json:"data"`
		}
		if err := p.conn.c.callWithin(ctx, p.sessionID, "Page.captureScreenshot", params, &r, 30*time.Second); err != nil {
			return browserShot{}, err
		}
		img, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			return browserShot{}, errors.New("Chrome returned an unreadable screenshot")
		}
		if len(img) <= browserMaxScreenshot {
			return newBrowserShot(img, "image/jpeg")
		}
		if clip, ok := base["clip"].(map[string]any); ok {
			clip["scale"] = 0.5
		}
	}
	return browserShot{}, errors.New("the page is too large to capture")
}

// Guard: while a fill runs, pause every document request of the tab. The kit
// already cancels page-initiated navigations of the main frame, so a main-frame
// GET that still arrives here was let through on purpose (the person clicked a
// link) and continues. Everything else (a form posted into a hidden iframe, an
// iframe sent somewhere, a main-frame POST) is failed and logged. Chrome drops
// the interception by itself if this connection goes away.
func (p *cdpPage) Guard(ctx context.Context, on bool) error {
	if on {
		return p.conn.c.Call(ctx, p.sessionID, "Fetch.enable", map[string]any{
			"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}},
		}, nil)
	}
	return p.conn.c.Call(ctx, p.sessionID, "Fetch.disable", nil, nil)
}

func (p *cdpPage) answerPaused(id, frameID, method, rawURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.worldMu.Lock()
	mainFrame := frameID != "" && frameID == p.frameID
	p.worldMu.Unlock()
	if mainFrame && method == "GET" {
		_ = p.conn.c.Call(ctx, p.sessionID, "Fetch.continueRequest", map[string]any{"requestId": id}, nil)
		return
	}
	_ = p.conn.c.Call(ctx, p.sessionID, "Fetch.failRequest", map[string]any{"requestId": id, "errorReason": "BlockedByClient"}, nil)
	where := ""
	if u, err := neturl.Parse(rawURL); err == nil {
		where = u.Scheme + "://" + u.Host + u.Path
	}
	p.guardMu.Lock()
	if len(p.guardLog) < 50 {
		p.guardLog = append(p.guardLog, map[string]any{"atMs": time.Now().UnixMilli(), "method": method, "url": where, "mainFrame": mainFrame})
	}
	p.guardMu.Unlock()
}

func (p *cdpPage) GuardLog() []map[string]any {
	p.guardMu.Lock()
	defer p.guardMu.Unlock()
	out := p.guardLog
	p.guardLog = nil
	return out
}

// ---------------------------------------------------------------- the route

type cdpBackend struct {
	conn *cdpConn
}

func (b *cdpBackend) Route() string { return browserRouteDevTools }

func (b *cdpBackend) Tabs(ctx context.Context) ([]browserTabInfo, error) {
	targets, err := b.conn.targets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]browserTabInfo, 0, len(targets))
	for _, t := range targets {
		out = append(out, browserTabInfo{TabID: makeBrowserTabID(browserRouteDevTools, t.TargetID), Title: t.Title, URL: t.URL})
	}
	return out, nil
}

func (b *cdpBackend) Open(ctx context.Context, url string) (browserPage, error) {
	var r struct {
		TargetID string `json:"targetId"`
	}
	if err := b.conn.c.Call(ctx, "", "Target.createTarget", map[string]any{"url": url}, &r); err != nil {
		return nil, err
	}
	return b.conn.page(ctx, r.TargetID)
}

func (b *cdpBackend) Page(ctx context.Context, rawTabID string) (browserPage, error) {
	return b.conn.page(ctx, rawTabID)
}

// TabInfo reads a tab's current URL and title from Chrome (no page call).
func (b *cdpBackend) TabInfo(ctx context.Context, rawTabID string) (browserTabInfo, error) {
	targets, err := b.conn.targets(ctx)
	if err != nil {
		return browserTabInfo{}, err
	}
	for _, t := range targets {
		if t.TargetID == rawTabID {
			return browserTabInfo{TabID: makeBrowserTabID(browserRouteDevTools, t.TargetID), Title: t.Title, URL: t.URL}, nil
		}
	}
	return browserTabInfo{}, browserErr(browserErrTabNotFound, "the tab was closed")
}
