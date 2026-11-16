package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------- a fake page running a fake kit

type fakeKitCall struct {
	name string
	args []json.RawMessage
}

type fakePage struct {
	mu       sync.Mutex
	id       string
	trusted  bool
	kit      map[string]func(args []json.RawMessage) any
	calls    []fakeKitCall
	clicks   [][2]float64
	inserted []string
	navs     []string
	busy     atomic.Int32
	maxBusy  atomic.Int32
	evalWait time.Duration
}

func (p *fakePage) TabID() string      { return p.id }
func (p *fakePage) TrustedInput() bool { return p.trusted }

func (p *fakePage) Eval(ctx context.Context, code string) (string, error) {
	n := p.busy.Add(1)
	defer p.busy.Add(-1)
	for {
		m := p.maxBusy.Load()
		if n <= m || p.maxBusy.CompareAndSwap(m, n) {
			break
		}
	}
	if p.evalWait > 0 {
		time.Sleep(p.evalWait)
	}
	const marker = ";globalThis.__aixKit.call("
	i := strings.LastIndex(code, marker)
	if i < 0 || !strings.HasPrefix(code, browserKitJS) {
		return "", errors.New("not a kit call")
	}
	rest := strings.TrimSuffix(code[i+len(marker):], ");")
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte("["+rest+"]"), &parts); err != nil || len(parts) != 2 {
		return "", errors.New("bad kit call")
	}
	var name string
	_ = json.Unmarshal(parts[0], &name)
	var args []json.RawMessage
	_ = json.Unmarshal(parts[1], &args)
	p.mu.Lock()
	p.calls = append(p.calls, fakeKitCall{name, args})
	h := p.kit[name]
	p.mu.Unlock()
	var result any = map[string]any{"ok": true}
	if h != nil {
		result = h(args)
	}
	env, _ := json.Marshal(map[string]any{"ok": true, "result": result, "stop": false, "docId": "doc-1"})
	return string(env), nil
}

func (p *fakePage) InsertText(ctx context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inserted = append(p.inserted, text)
	return nil
}

func (p *fakePage) Click(ctx context.Context, x, y float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clicks = append(p.clicks, [2]float64{x, y})
	return nil
}
func (p *fakePage) Guard(ctx context.Context, on bool) error { return nil }
func (p *fakePage) GuardLog() []map[string]any               { return nil }
func (p *fakePage) Navigate(ctx context.Context, url string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.navs = append(p.navs, url)
	return nil
}
func (p *fakePage) History(ctx context.Context, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.navs = append(p.navs, action)
	return nil
}
func (p *fakePage) Screenshot(ctx context.Context, full bool) (browserShot, error) {
	return browserShot{Data: []byte("jpeg"), Width: 10, Height: 20, ContentType: "image/jpeg"}, nil
}

func (p *fakePage) called(name string) []fakeKitCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []fakeKitCall
	for _, c := range p.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

type fakeBackend struct {
	route string
	pages map[string]*fakePage
	tabs  []browserTabInfo
}

func (b *fakeBackend) Route() string { return b.route }
func (b *fakeBackend) Tabs(ctx context.Context) ([]browserTabInfo, error) {
	return b.tabs, nil
}
func (b *fakeBackend) Open(ctx context.Context, url string) (browserPage, error) {
	return b.pages["new"], nil
}
func (b *fakeBackend) Page(ctx context.Context, raw string) (browserPage, error) {
	if p := b.pages[raw]; p != nil {
		return p, nil
	}
	return nil, browserErr(browserErrTabNotFound, "gone")
}
func (b *fakeBackend) TabInfo(ctx context.Context, raw string) (browserTabInfo, error) {
	return browserTabInfo{TabID: "d:" + raw, URL: "https://example.com/", Title: "Example"}, nil
}

func pingOK([]json.RawMessage) any {
	return map[string]any{"docId": "doc-1", "url": "https://example.com/", "title": "Example", "readyState": "complete"}
}

func newOpRun(page *fakePage, route string, p browserParams) *browserOpRun {
	be := &fakeBackend{route: route, pages: map[string]*fakePage{"T1": page, "new": page}}
	return &browserOpRun{p: p, be: be, stop: &atomic.Bool{}}
}

// fastSettle shortens the post-click watch for tests.
func fastSettle(t *testing.T) {
	t.Helper()
	c, s := browserClickQuiet, browserSubmitQuiet
	browserClickQuiet, browserSubmitQuiet = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { browserClickQuiet, browserSubmitQuiet = c, s })
}

func browserCode(err error) string {
	var be *browserError
	if errors.As(err, &be) {
		return be.Code
	}
	return ""
}

// ---------------------------------------------------------------- digest

func TestBrowserDigestCanonical(t *testing.T) {
	var parts browserDigestParts
	if err := json.Unmarshal([]byte(`{"origin":"https://ex.com","path":"/apply","form":"1a2b","kind":"form",
		"fields":[{"fp":"f1","value":"Ada"},{"fp":"f2","value":"","len":7,"sensitive":true},{"fp":"f3","value":"true"}],"submit":"9z"}`), &parts); err != nil {
		t.Fatal(err)
	}
	// The canonical is fixed byte for byte; the literal IS the assertion.
	canonical := `{"v":1,"page":"https://ex.com/apply","form":"1a2b","fields":[{"fp":"f1","value":"Ada"},{"fp":"f2","value":"","len":7,"sensitive":true},{"fp":"f3","value":"true"}],"submit":"9z"}`
	sum := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(sum[:])
	got := browserDigest(parts)
	if got != want {
		t.Fatalf("digest %s, want %s", got, want)
	}
	// A sensitive value contributes only its length, whatever the kit sent.
	parts.Fields[1].Value = "leaked?"
	if browserDigest(parts) != want {
		t.Fatal("a sensitive field's value must not reach the digest")
	}
	parts.Fields[1].Value = ""
	for name, mutate := range map[string]func(p *browserDigestParts){
		"value":  func(p *browserDigestParts) { p.Fields[0].Value = "Grace" },
		"length": func(p *browserDigestParts) { p.Fields[1].Len = 8 },
		"order":  func(p *browserDigestParts) { p.Fields[0], p.Fields[2] = p.Fields[2], p.Fields[0] },
		"page":   func(p *browserDigestParts) { p.Path = "/apply2" },
		"form":   func(p *browserDigestParts) { p.Form = "other" },
		"submit": func(p *browserDigestParts) { p.Submit = "other" },
		"added":  func(p *browserDigestParts) { p.Fields = append(p.Fields, p.Fields[0]) },
	} {
		cp := parts
		cp.Fields = append(cp.Fields[:0:0], parts.Fields...)
		mutate(&cp)
		if browserDigest(cp) == want {
			t.Errorf("%s: a change must change the digest", name)
		}
	}
	if len(want) != 64 || strings.ToLower(want) != want {
		t.Fatal("digest is lowercase hex sha256")
	}
}

// ---------------------------------------------------------------- ops

func TestBrowserTabsListsWebPagesOnly(t *testing.T) {
	be := &fakeBackend{route: browserRouteDevTools, tabs: []browserTabInfo{
		{TabID: "d:1", Title: "Mail", URL: "https://mail.example.com/"},
		{TabID: "d:2", Title: "Settings", URL: "chrome://settings/"},
		{TabID: "d:3", Title: "New Tab", URL: "chrome://newtab/"},
		{TabID: "d:4", Title: "Docs", URL: "http://docs.example.com/x"},
	}}
	r := &browserOpRun{p: browserParams{Op: browserOpTabs}, be: be, stop: &atomic.Bool{}}
	res, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tabs := res.(browserTabsResult).Tabs
	if len(tabs) != 2 || tabs[0].TabID != "d:1" || tabs[1].TabID != "d:4" {
		t.Fatalf("%+v", tabs)
	}
}

func TestBrowserReadShapesAndMasks(t *testing.T) {
	yes := true
	page := &fakePage{id: "d:T1", kit: map[string]func([]json.RawMessage) any{
		"snapshot": func(args []json.RawMessage) any {
			return map[string]any{"docId": "doc-1", "url": "https://ex.com/", "title": "Ex", "text": "Hello", "textTruncated": false,
				"links":   []any{map[string]any{"ref": "f1_a", "text": "Home", "href": "https://ex.com/"}},
				"buttons": []any{map[string]any{"ref": "f2_a", "text": "Send", "submitClass": true}},
				"fields": []any{
					map[string]any{"ref": "f3_a", "kind": "text", "label": "Password", "visible": true, "hasValue": true, "value": "hunter2", "sensitive": map[string]any{"level": "never", "kind": "password"}},
					map[string]any{"ref": "f4_a", "kind": "checkbox", "label": "News", "visible": true, "checked": &yes},
					map[string]any{"ref": "f5_a", "kind": "select", "label": "Topic", "visible": true, "hasValue": true, "value": "Sales", "options": []any{map[string]any{"label": "Sales"}, map[string]any{"label": "Support"}}},
					map[string]any{"ref": "f6_a", "kind": "text", "label": "aria name", "visibleLabel": "Shown", "labelMismatch": true, "visible": false, "hiddenReason": "covered"},
				},
				"forms": []any{map[string]any{"formRef": "f9_a", "name": "Contact", "submitControls": []any{map[string]any{"ref": "f2_a", "text": "Send"}}}},
			}
		},
	}}
	r := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpRead, TabID: "d:T1"})
	res, err := r.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rd := res.(browserReadResult)
	if rd.Fields[0].Value != "•••" || rd.Fields[1].Value != "checked" || len(rd.Fields[2].Options) != 2 || rd.Fields[3].Label != "Shown" || rd.Fields[3].Visible {
		t.Fatalf("%+v", rd.Fields)
	}
	if len(rd.Buttons) != 1 || !rd.Buttons[0].SubmitClass || len(rd.Forms) != 1 || rd.Forms[0].SubmitControls[0].Ref != "f2_a" {
		t.Fatalf("%+v %+v", rd.Buttons, rd.Forms)
	}
	var opts map[string]any
	_ = json.Unmarshal(page.called("snapshot")[0].args[0], &opts)
	if opts["includeText"] != true || opts["maxItems"] != float64(browserMaxReadItems) || opts["maxText"] != float64(browserMaxPageTextChars) {
		t.Fatalf("snapshot options %v", opts)
	}
}

func TestBrowserClick(t *testing.T) {
	fastSettle(t)
	newPage := func(reason string, trusted bool) *fakePage {
		return &fakePage{id: "d:T1", trusted: trusted, kit: map[string]func([]json.RawMessage) any{
			"clickBegin": func([]json.RawMessage) any {
				if reason != "" {
					return map[string]any{"ok": false, "reason": reason, "detail": "covered"}
				}
				return map[string]any{"ok": true, "x": 10.5, "y": 20, "docId": "doc-1"}
			},
			"clickNow": func([]json.RawMessage) any { return map[string]any{"ok": true} },
			"ping":     pingOK,
		}}
	}
	for reason, code := range map[string]string{"submit-class": browserErrSubmitNeedsApproval, "hidden": browserErrCommandInvalid, "not-found": browserErrCommandInvalid} {
		page := newPage(reason, true)
		_, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpClick, TabID: "d:T1", Ref: "f1_a"}).run(context.Background())
		if browserCode(err) != code || len(page.clicks) != 0 {
			t.Errorf("%s: %v, clicks %v", reason, err, page.clicks)
		}
	}
	page := newPage("", true)
	res, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpClick, TabID: "d:T1", Ref: "f1_a"}).run(context.Background())
	if err != nil || len(page.clicks) != 1 || page.clicks[0] != [2]float64{10.5, 20} || len(page.called("clickEnd")) != 1 {
		t.Fatalf("trusted click: %v %v", err, page.clicks)
	}
	if cr := res.(browserClickResult); cr.Navigated || cr.URL != "https://example.com/" {
		t.Fatalf("%+v", cr)
	}
	page = newPage("", false)
	if _, err := newOpRun(page, browserRouteAppleScript, browserParams{Op: browserOpClick, TabID: "a:T1", Ref: "f1_a"}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(page.clicks) != 0 || len(page.called("clickNow")) != 1 {
		t.Fatal("the AppleScript route clicks from the page")
	}
}

func TestBrowserPrepareSubmit(t *testing.T) {
	parts := map[string]any{"origin": "https://ex.com", "path": "/f", "form": "ff", "kind": "form", "submit": "ss",
		"fields": []any{map[string]any{"fp": "a", "value": "Ada"}, map[string]any{"fp": "b", "value": "", "len": 4, "sensitive": true}}}
	page := &fakePage{id: "d:T1", kit: map[string]func([]json.RawMessage) any{
		"prepareSubmit": func(args []json.RawMessage) any {
			if string(args[0]) == `"f2_a"` {
				return map[string]any{"ok": false, "reason": "ambiguous-form", "candidates": []any{
					map[string]any{"ref": "f8_a", "text": "Login"}, map[string]any{"ref": "f9_a", "text": "Search"}}}
			}
			return map[string]any{"ok": true, "url": "https://ex.com/f", "origin": "https://ex.com", "title": "F", "submitRef": "f3_a",
				"submitText": "Send", "formName": "Contact", "parts": parts, "token": "t",
				"fields": []any{map[string]any{"label": "Name", "kind": "text", "value": "Ada", "required": true},
					map[string]any{"label": "PIN", "kind": "text", "value": "1234", "sensitive": true}}}
		},
	}}
	_, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpPrepareSubmit, TabID: "d:T1", Ref: "f2_a"}).run(context.Background())
	if browserCode(err) != browserErrCommandInvalid || !strings.Contains(err.Error(), "f8_a") {
		t.Fatalf("ambiguous: %v", err)
	}
	res, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpPrepareSubmit, TabID: "d:T1"}).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pr := res.(browserPrepareResult)
	var dp browserDigestParts
	raw, _ := json.Marshal(parts)
	_ = json.Unmarshal(raw, &dp)
	if pr.Digest != browserDigest(dp) || pr.SubmitRef != "f3_a" || pr.Fields[1].Value != "•••" || pr.Origin != "https://ex.com" {
		t.Fatalf("%+v", pr)
	}
	// No ref: the kit gets null and picks the page's one form.
	if string(page.called("prepareSubmit")[1].args[0]) != "null" {
		t.Fatalf("args %s", page.called("prepareSubmit")[1].args[0])
	}
}

func TestBrowserSubmit(t *testing.T) {
	fastSettle(t)
	parts := map[string]any{"origin": "https://ex.com", "path": "/f", "form": "ff", "kind": "form", "submit": "ss",
		"fields": []any{map[string]any{"fp": "a", "value": "Ada"}}}
	raw, _ := json.Marshal(parts)
	var dp browserDigestParts
	_ = json.Unmarshal(raw, &dp)
	approved := browserDigest(dp)

	type setup struct {
		check    any
		arm      any
		used     bool
		invalid  bool
		requests int
		trusted  bool
	}
	run := func(s setup, digest string) (*fakePage, any, error) {
		page := &fakePage{id: "d:T1", trusted: s.trusted, kit: map[string]func([]json.RawMessage) any{
			"submitCheck": func([]json.RawMessage) any { return s.check },
			"armSubmit":   func([]json.RawMessage) any { return s.arm },
			"submitEnd": func([]json.RawMessage) any {
				return map[string]any{"ok": true, "used": s.used, "invalid": s.invalid, "requests": s.requests}
			},
			"ping": pingOK,
		}}
		res, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpSubmit, TabID: "d:T1", SubmitRef: "f3_a",
			Digest: digest, RequestID: "r1"}).run(context.Background())
		return page, res, err
	}
	okCheck := map[string]any{"ok": true, "docId": "doc-1", "parts": parts, "token": "tok"}
	okArm := map[string]any{"ok": true, "docId": "doc-1", "x": 5, "y": 6}

	// The form changed: nothing is armed, nothing is clicked.
	page, _, err := run(setup{check: okCheck, arm: okArm, used: true, trusted: true}, strings.Repeat("0", 64))
	if browserCode(err) != browserErrSubmitFormChanged || len(page.called("armSubmit")) != 0 || len(page.clicks) != 0 {
		t.Fatalf("changed: %v", err)
	}
	// The control is gone.
	page, _, err = run(setup{check: map[string]any{"ok": false, "reason": "not-found"}, trusted: true}, approved)
	if browserCode(err) != browserErrSubmitFormChanged || len(page.called("armSubmit")) != 0 {
		t.Fatalf("gone: %v", err)
	}
	// Changed between the check and the arm.
	page, _, err = run(setup{check: okCheck, arm: map[string]any{"ok": false, "reason": "changed"}, trusted: true}, approved)
	if browserCode(err) != browserErrSubmitFormChanged || len(page.clicks) != 0 {
		t.Fatalf("raced: %v", err)
	}
	// Approved and unchanged: one trusted click on the control.
	page, res, err := run(setup{check: okCheck, arm: okArm, used: true, trusted: true}, approved)
	if err != nil || len(page.clicks) != 1 || page.clicks[0] != [2]float64{5, 6} {
		t.Fatalf("submit: %v %v", err, page.clicks)
	}
	if res.(browserClickResult).TabID != "d:T1" {
		t.Fatalf("%+v", res)
	}
	var arm []json.RawMessage = page.called("armSubmit")[0].args
	if string(arm[0]) != `"f3_a"` || string(arm[1]) != `"tok"` || string(arm[2]) != `"click"` {
		t.Fatalf("armSubmit args %s", arm)
	}
	// AppleScript: the kit submits with requestSubmit; no trusted click exists.
	page, _, err = run(setup{check: okCheck, arm: map[string]any{"ok": true, "docId": "doc-1", "submitted": true}, used: true}, approved)
	if err != nil || len(page.clicks) != 0 || string(page.called("armSubmit")[0].args[2]) != `"requestSubmit"` {
		t.Fatalf("applescript: %v", err)
	}
	if so := res.(browserClickResult).SubmitObserved; so == nil || !*so {
		t.Fatalf("an observed submit event must report submitObserved: %+v", res)
	}
	// The browser's validation refused it (an invalid event, no submit event,
	// nothing navigated): the one case that proves nothing was sent.
	_, _, err = run(setup{check: okCheck, arm: okArm, used: false, invalid: true, trusted: true}, approved)
	if browserCode(err) != browserErrSubmitFormChanged || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("refused: %v", err)
	}
	// A validation refusal while the page sent a request of its own (a script
	// that checks validity and saves by fetch anyway) proves nothing either.
	_, res, err = run(setup{check: okCheck, arm: okArm, used: false, invalid: true, requests: 1, trusted: true}, approved)
	if err != nil {
		t.Fatalf("invalid plus a request reported as not submitted: %v", err)
	}
	// No submit event, no navigation, no validation refusal: a script-driven
	// save (a submit-class type=button posting by fetch) looks exactly like
	// this, so the agent must not claim nothing was sent.
	_, res, err = run(setup{check: okCheck, arm: okArm, used: false, trusted: true}, approved)
	if err != nil {
		t.Fatalf("an unobserved outcome was reported as not submitted: %v", err)
	}
	if cr := res.(browserClickResult); cr.SubmitObserved == nil || *cr.SubmitObserved || cr.Navigated {
		t.Fatalf("unobserved: %+v", cr)
	}
	raw, _ = json.Marshal(res)
	if !strings.Contains(string(raw), `"submitObserved":false`) {
		t.Fatalf("result %s", raw)
	}
}

func TestBrowserScreenshotNeedsDevTools(t *testing.T) {
	page := &fakePage{id: "a:T1"}
	_, err := newOpRun(page, browserRouteAppleScript, browserParams{Op: browserOpScreenshot, TabID: "a:T1"}).run(context.Background())
	if browserCode(err) != browserErrScreenshotUnsupport {
		t.Fatalf("%v", err)
	}
	r := newOpRun(&fakePage{id: "d:T1"}, browserRouteDevTools, browserParams{Op: browserOpScreenshot, TabID: "d:T1"})
	res, err := r.run(context.Background())
	if err != nil || r.shot == nil || res.(browserScreenshotResult).Bytes != 4 || res.(browserScreenshotResult).ContentType != "image/jpeg" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestBrowserNavigate(t *testing.T) {
	var docs atomic.Int32
	page := &fakePage{id: "d:T1", kit: map[string]func([]json.RawMessage) any{"ping": func([]json.RawMessage) any {
		// Every ping after the first sees a new document: the navigation landed.
		return map[string]any{"docId": fmt.Sprintf("doc-%d", docs.Add(1)), "url": "https://a.b/", "title": "A", "readyState": "complete"}
	}}}
	if _, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpNavigate, TabID: "d:T1", Action: browserNavBack}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpNavigate, TabID: "d:T1", URL: "https://a.b/"}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(page.navs, ",") != "back,https://a.b/" {
		t.Fatalf("%v", page.navs)
	}
	_, err := newOpRun(page, browserRouteDevTools, browserParams{Op: browserOpRead, TabID: "d:GONE"}).run(context.Background())
	if browserCode(err) != browserErrTabNotFound {
		t.Fatalf("closed tab: %v", err)
	}
}

func TestClassifyBrowserError(t *testing.T) {
	ctx := context.Background()
	expired, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	cases := []struct {
		err  error
		ctx  context.Context
		code string
	}{
		{browserErr(browserErrTabNotFound, "x"), ctx, browserErrTabNotFound},
		{&notRespondingError{What: "x", After: time.Second}, ctx, browserErrPageNotResponding},
		{errGaveUp, ctx, browserErrPageNotResponding},
		{errCDPDenied, ctx, browserErrChromeDenied},
		{context.DeadlineExceeded, expired, browserErrCommandTimeout},
		{errors.New("kit snapshot failed"), ctx, browserErrPageNotResponding},
		{errors.New("DevTools connection closed: EOF"), ctx, browserErrChromeNotReachable},
	}
	for _, c := range cases {
		if got := classifyBrowserError(c.err, c.ctx); got.Code != c.code {
			t.Errorf("%v: %s, want %s", c.err, got.Code, c.code)
		}
	}
	if got := classifyBrowserOpenError(errCDPDenied, ctx); got.Code != browserErrChromeDenied {
		t.Errorf("declined: %s", got.Code)
	}
	if got := classifyBrowserOpenError(context.DeadlineExceeded, expired); got.Code != browserErrChromeDenied {
		t.Errorf("unanswered dialog: %s", got.Code)
	}
}
