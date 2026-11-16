// browser_ops.go — what each browser operation does on a route.
//
// Every op returns either a result object (the wire contract's `result`) or a
// *browserError. Page content travels only in results; it is never logged.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// browserOpRun is one operation's context: the parsed command, the route, and
// the stop flag a later `stop` command sets.
type browserOpRun struct {
	p    browserParams
	be   browserBackend
	stop *atomic.Bool
	// shot is set by screenshot: the image the dispatcher uploads before the result.
	shot *browserShot
}

// Load waits: the wire contract's "waits for load <= 15 s" (a variable so tests
// can shorten it).
var browserLoadWait = 15 * time.Second

// How long a click (or an approved submit) that navigates nowhere is watched
// before the op reports. Variables so tests can shorten them.
var (
	browserClickQuiet  = 900 * time.Millisecond
	browserSubmitQuiet = 2500 * time.Millisecond
)

func (r *browserOpRun) run(ctx context.Context) (any, error) {
	switch r.p.Op {
	case browserOpTabs:
		return r.tabs(ctx)
	case browserOpOpen:
		return r.open(ctx)
	case browserOpRead:
		return r.read(ctx)
	case browserOpFill:
		return r.fill(ctx)
	case browserOpClick:
		return r.click(ctx)
	case browserOpNavigate:
		return r.navigate(ctx)
	case browserOpScreenshot:
		return r.screenshot(ctx)
	case browserOpPrepareSubmit:
		return r.prepareSubmit(ctx)
	case browserOpSubmit:
		return r.submit(ctx)
	}
	return nil, browserErr(browserErrCommandInvalid, "unknown op")
}

func (r *browserOpRun) page(ctx context.Context) (browserPage, *kitRunner, error) {
	_, raw, _ := parseBrowserTabID(r.p.TabID)
	pg, err := r.be.Page(ctx, raw)
	if err != nil {
		return nil, nil, err
	}
	return pg, newKitRunner(pg), nil
}

type browserTabsResult struct {
	Tabs []browserTabInfo `json:"tabs"`
}

// browserMaxTabs bounds the tab list (a person with 500 tabs open still gets an answer).
const browserMaxTabs = 200

func (r *browserOpRun) tabs(ctx context.Context) (any, error) {
	all, err := r.be.Tabs(ctx)
	if err != nil {
		return nil, err
	}
	out := browserTabsResult{Tabs: []browserTabInfo{}}
	for _, t := range all {
		if !isBrowserHTTPURL(t.URL) {
			continue
		}
		t.Title = clipString(t.Title, 300)
		if len(t.URL) > browserMaxURLChars {
			t.URL = t.URL[:browserMaxURLChars]
		}
		out.Tabs = append(out.Tabs, t)
		if len(out.Tabs) >= browserMaxTabs {
			break
		}
	}
	return out, nil
}

type browserPageInfo struct {
	TabID string `json:"tabId"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func (r *browserOpRun) open(ctx context.Context) (any, error) {
	pg, err := r.be.Open(ctx, r.p.URL)
	if err != nil {
		return nil, err
	}
	k := newKitRunner(pg)
	p, err := k.waitLoaded(ctx, "", "", browserLoadWait)
	if err != nil || p.URL == "" {
		// The tab exists; a slow or script-blocked page still gets reported.
		if info, ierr := r.tabInfo(ctx, pg); ierr == nil {
			return info, nil
		}
		return browserPageInfo{TabID: pg.TabID(), URL: r.p.URL}, nil
	}
	return browserPageInfo{TabID: pg.TabID(), URL: p.URL, Title: clipString(p.Title, 300)}, nil
}

func (r *browserOpRun) tabInfo(ctx context.Context, pg browserPage) (browserPageInfo, error) {
	_, raw, _ := parseBrowserTabID(pg.TabID())
	t, err := r.be.TabInfo(ctx, raw)
	if err != nil {
		return browserPageInfo{}, err
	}
	return browserPageInfo{TabID: pg.TabID(), URL: t.URL, Title: clipString(t.Title, 300)}, nil
}

// ---------------------------------------------------------------- read

type kitSnapshot struct {
	DocID         string `json:"docId"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"textTruncated"`
	Links         []struct {
		Ref  string `json:"ref"`
		Text string `json:"text"`
		Href string `json:"href"`
	} `json:"links"`
	Buttons []struct {
		Ref         string `json:"ref"`
		Text        string `json:"text"`
		SubmitClass bool   `json:"submitClass"`
	} `json:"buttons"`
	Fields []kitField `json:"fields"`
	Forms  []struct {
		FormRef        string `json:"formRef"`
		Name           string `json:"name"`
		SubmitControls []struct {
			Ref  string `json:"ref"`
			Text string `json:"text"`
		} `json:"submitControls"`
	} `json:"forms"`
}

type browserReadLink struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
	Href string `json:"href"`
}

type browserReadButton struct {
	Ref         string `json:"ref"`
	Text        string `json:"text"`
	SubmitClass bool   `json:"submitClass"`
}

type browserReadField struct {
	Ref          string          `json:"ref"`
	Label        string          `json:"label"`
	Kind         string          `json:"kind"`
	Required     bool            `json:"required"`
	Options      []string        `json:"options,omitempty"`
	Value        string          `json:"value,omitempty"`
	Visible      bool            `json:"visible"`
	HiddenReason string          `json:"hiddenReason,omitempty"`
	Sensitive    *kitSensitivity `json:"sensitive,omitempty"`
	FormRef      string          `json:"formRef,omitempty"`
	Disabled     bool            `json:"disabled,omitempty"`
	Invalid      bool            `json:"invalid,omitempty"`
}

type browserReadForm struct {
	FormRef        string            `json:"formRef"`
	Name           string            `json:"name"`
	SubmitControls []browserReadLink `json:"submitControls"`
}

type browserReadResult struct {
	TabID         string              `json:"tabId"`
	URL           string              `json:"url"`
	Title         string              `json:"title"`
	Text          string              `json:"text"`
	TextTruncated bool                `json:"textTruncated"`
	Links         []browserReadLink   `json:"links"`
	Buttons       []browserReadButton `json:"buttons"`
	Fields        []browserReadField  `json:"fields"`
	Forms         []browserReadForm   `json:"forms"`
}

// browserMaxOptions bounds the options listed per field.
const browserMaxOptions = 100

func shapeReadField(f kitField) browserReadField {
	out := browserReadField{
		Ref: f.Ref, Label: clipString(labelOf(f), 200), Kind: f.Kind, Required: f.Required,
		Visible: f.Visible, HiddenReason: f.HiddenReason, Sensitive: f.Sensitive, FormRef: f.FormRef,
		Disabled: f.Disabled || f.ReadOnly, Invalid: f.Invalid,
	}
	if out.Label == "" && f.Placeholder != "" {
		out.Label = clipString(f.Placeholder, 200)
	}
	for _, o := range f.Options {
		if len(out.Options) >= browserMaxOptions {
			break
		}
		out.Options = append(out.Options, clipString(o.Label, 120))
	}
	switch {
	case f.Checked != nil:
		out.Value = "unchecked"
		if *f.Checked {
			out.Value = "checked"
		}
	case f.Sensitive != nil && f.HasValue:
		out.Value = "•••" // the kit masks these already; never trust a page to keep them so
	case f.HasValue:
		out.Value = clipString(f.Value, 200)
	}
	return out
}

func shapeReadResult(tabID string, s kitSnapshot) browserReadResult {
	out := browserReadResult{
		TabID: tabID, URL: s.URL, Title: clipString(s.Title, 300), Text: s.Text, TextTruncated: s.TextTruncated,
		Links: []browserReadLink{}, Buttons: []browserReadButton{}, Fields: []browserReadField{}, Forms: []browserReadForm{},
	}
	if jsLength(out.Text) > browserMaxPageTextChars {
		out.Text = clipRunesJS(out.Text, browserMaxPageTextChars)
		out.TextTruncated = true
	}
	for _, l := range s.Links {
		if len(out.Links) >= browserMaxReadItems {
			break
		}
		out.Links = append(out.Links, browserReadLink{Ref: l.Ref, Text: clipString(l.Text, 120), Href: l.Href})
	}
	for _, b := range s.Buttons {
		if len(out.Buttons) >= browserMaxReadItems {
			break
		}
		out.Buttons = append(out.Buttons, browserReadButton{Ref: b.Ref, Text: clipString(b.Text, 80), SubmitClass: b.SubmitClass})
	}
	for _, f := range s.Fields {
		if len(out.Fields) >= browserMaxReadItems {
			break
		}
		out.Fields = append(out.Fields, shapeReadField(f))
	}
	for _, f := range s.Forms {
		if len(out.Forms) >= 50 {
			break
		}
		rf := browserReadForm{FormRef: f.FormRef, Name: clipString(f.Name, 80), SubmitControls: []browserReadLink{}}
		for _, c := range f.SubmitControls {
			rf.SubmitControls = append(rf.SubmitControls, browserReadLink{Ref: c.Ref, Text: clipString(c.Text, 80)})
		}
		out.Forms = append(out.Forms, rf)
	}
	return out
}

func clipRunesJS(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}

func (r *browserOpRun) read(ctx context.Context) (any, error) {
	pg, k, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	includeText := r.p.IncludeText == nil || *r.p.IncludeText
	var s kitSnapshot
	if _, err := k.call(ctx, "snapshot", &s, map[string]any{
		"includeText": includeText, "maxItems": browserMaxReadItems, "maxText": browserMaxPageTextChars,
	}); err != nil {
		return nil, err
	}
	return shapeReadResult(pg.TabID(), s), nil
}

// ---------------------------------------------------------------- fill

func (r *browserOpRun) fill(ctx context.Context) (any, error) {
	_, k, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	return runBrowserFill(ctx, k, r.p.Values, r.stop)
}

// ---------------------------------------------------------------- click

type browserClickResult struct {
	TabID          string `json:"tabId"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Navigated      bool   `json:"navigated"`
	BlockedSubmits int    `json:"blockedSubmits,omitempty"`
	// SubmitObserved (submit only; additive): the browser fired the approved
	// submission's submit event or the page navigated. false means the agent
	// saw no proof either way (a script-driven save sends by fetch without a
	// submit event), so it must never be read as "nothing was sent".
	SubmitObserved *bool `json:"submitObserved,omitempty"`
}

type kitClickBegin struct {
	OK     bool    `json:"ok"`
	Reason string  `json:"reason"`
	Detail string  `json:"detail"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	DocID  string  `json:"docId"`
}

func (r *browserOpRun) click(ctx context.Context) (any, error) {
	pg, k, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	var b kitClickBegin
	if _, err := k.call(ctx, "clickBegin", &b, r.p.Ref); err != nil {
		return nil, err
	}
	if !b.OK {
		switch b.Reason {
		case "submit-class":
			return nil, browserErr(browserErrSubmitNeedsApproval, "that control would submit or save; prepare the form with prepareSubmit so the person can approve it")
		case "hidden":
			return nil, browserErr(browserErrCommandInvalid, "that control cannot be seen on the page (%s); it was not clicked", clipString(b.Detail, 60))
		}
		return nil, browserErr(browserErrCommandInvalid, "no element with that ref on the page; read the page again")
	}
	if pg.TrustedInput() {
		err = pg.Click(ctx, b.X, b.Y)
	} else {
		var now struct {
			OK bool `json:"ok"`
		}
		_, err = k.call(ctx, "clickNow", &now, r.p.Ref)
		if err == nil && !now.OK {
			err = browserErr(browserErrCommandInvalid, "the control went away before it was clicked")
		}
	}
	if err != nil {
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, _ = k.call(endCtx, "clickEnd", nil)
		cancel()
		return nil, err
	}
	p, navigated := k.settle(ctx, b.DocID, browserClickQuiet, browserLoadWait)
	out := browserClickResult{TabID: pg.TabID(), URL: p.URL, Title: clipString(p.Title, 300), Navigated: navigated}
	if !navigated && !k.hung {
		var e struct {
			BlockedSubmits []json.RawMessage `json:"blockedSubmits"`
		}
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, _ = k.call(endCtx, "clickEnd", &e)
		cancel()
		out.BlockedSubmits = len(e.BlockedSubmits)
	}
	if out.URL == "" {
		if info, ierr := r.tabInfo(ctx, pg); ierr == nil {
			out.URL, out.Title = info.URL, info.Title
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- navigate

func (r *browserOpRun) navigate(ctx context.Context) (any, error) {
	pg, k, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	old, oldURL := "", ""
	if p, err := k.ping(ctx); err == nil {
		old, oldURL = p.DocID, p.URL
	}
	if r.p.Action == browserNavReload {
		oldURL = "" // a reload stays at its URL: only a new document counts
	}
	if r.p.Action != "" {
		err = pg.History(ctx, r.p.Action)
	} else {
		err = pg.Navigate(ctx, r.p.URL)
	}
	if err != nil {
		return nil, err
	}
	p, _ := k.waitLoaded(ctx, old, oldURL, browserLoadWait)
	if p.URL == "" {
		if info, ierr := r.tabInfo(ctx, pg); ierr == nil {
			return info, nil
		}
	}
	return browserPageInfo{TabID: pg.TabID(), URL: p.URL, Title: clipString(p.Title, 300)}, nil
}

// ---------------------------------------------------------------- screenshot

type browserScreenshotResult struct {
	TabID       string `json:"tabId"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"contentType"`
}

func (r *browserOpRun) screenshot(ctx context.Context) (any, error) {
	if r.be.Route() != browserRouteDevTools {
		return nil, browserErr(browserErrScreenshotUnsupport, "screenshots need Chrome's remote debugging: turn it on at chrome://inspect/#remote-debugging")
	}
	pg, _, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	shot, err := pg.Screenshot(ctx, r.p.FullPage)
	if err != nil {
		return nil, err
	}
	r.shot = &shot
	return browserScreenshotResult{TabID: pg.TabID(), Width: shot.Width, Height: shot.Height, Bytes: len(shot.Data), ContentType: shot.ContentType}, nil
}

// ---------------------------------------------------------------- prepare and submit

// browserDigestParts is what the kit reads for a form's digest.
type browserDigestParts struct {
	Origin string `json:"origin"`
	Path   string `json:"path"`
	Form   string `json:"form"`
	Fields []struct {
		FP        string `json:"fp"`
		Value     string `json:"value"`
		Len       int    `json:"len"`
		Sensitive bool   `json:"sensitive"`
	} `json:"fields"`
	// Data is every name/value pair the submission carries (hidden inputs
	// included), in tree order; a sensitive value contributes only its length.
	Data []struct {
		Name      string `json:"name"`
		Value     string `json:"value"`
		Len       int    `json:"len"`
		Sensitive bool   `json:"sensitive"`
	} `json:"data"`
	Submit string `json:"submit"`
	// Kind is "form" for a real <form>, "virtual" for a control outside any
	// form. Not part of the digest (the form fingerprint already covers it).
	Kind string `json:"kind"`
}

// browserDigest is the lowercase hex SHA-256 of the canonical JSON of a form:
// page origin + path, the form's identity fingerprint (its destination
// included), every field's fingerprint and current value in DOM order, every
// name/value pair the submission carries (hidden inputs included), and the
// submit control's fingerprint (its destination overrides included). The canonical form is fixed here, field by field, so the digest
// the person approved and the one recomputed before the submit can only differ
// when the form did.
func browserDigest(p browserDigestParts) string {
	type canonField struct {
		FP    string `json:"fp"`
		Value string `json:"value"`
		Len   int    `json:"len,omitempty"`
		S     bool   `json:"sensitive,omitempty"`
	}
	type canonDatum struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Len   int    `json:"len,omitempty"`
		S     bool   `json:"sensitive,omitempty"`
	}
	type canon struct {
		V      int          `json:"v"`
		Page   string       `json:"page"`
		Form   string       `json:"form"`
		Fields []canonField `json:"fields"`
		Data   []canonDatum `json:"data,omitempty"`
		Submit string       `json:"submit"`
	}
	c := canon{V: 1, Page: p.Origin + p.Path, Form: p.Form, Fields: []canonField{}, Submit: p.Submit}
	for _, d := range p.Data {
		cd := canonDatum{Name: d.Name, Value: d.Value}
		if d.Sensitive {
			cd = canonDatum{Name: d.Name, Len: d.Len, S: true}
		}
		c.Data = append(c.Data, cd)
	}
	for _, f := range p.Fields {
		cf := canonField{FP: f.FP, Value: f.Value}
		if f.Sensitive {
			cf = canonField{FP: f.FP, Len: f.Len, S: true}
		}
		c.Fields = append(c.Fields, cf)
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type kitPrepare struct {
	OK         bool               `json:"ok"`
	Reason     string             `json:"reason"`
	Detail     string             `json:"detail"`
	Candidates []browserReadLink  `json:"candidates"`
	DocID      string             `json:"docId"`
	URL        string             `json:"url"`
	Origin     string             `json:"origin"`
	Title      string             `json:"title"`
	SubmitRef  string             `json:"submitRef"`
	SubmitText string             `json:"submitText"`
	FormName   string             `json:"formName"`
	Parts      browserDigestParts `json:"parts"`
	Token      string             `json:"token"`
	Fields     []struct {
		Label     string `json:"label"`
		Kind      string `json:"kind"`
		Value     string `json:"value"`
		Required  bool   `json:"required"`
		Hidden    bool   `json:"hidden"`
		Sensitive bool   `json:"sensitive"`
	} `json:"fields"`
}

type browserSubmitField struct {
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	Required  bool   `json:"required"`
	Hidden    bool   `json:"hidden,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

type browserPrepareResult struct {
	TabID      string               `json:"tabId"`
	URL        string               `json:"url"`
	Origin     string               `json:"origin"`
	Title      string               `json:"title"`
	Digest     string               `json:"digest"`
	SubmitRef  string               `json:"submitRef"`
	SubmitText string               `json:"submitText"`
	FormName   string               `json:"formName"`
	Fields     []browserSubmitField `json:"fields"`
}

func prepareRefusal(reason, detail string, cands []browserReadLink) *browserError {
	list := ""
	for i, c := range cands {
		if i >= 5 {
			break
		}
		if list != "" {
			list += "; "
		}
		list += fmt.Sprintf("%q (ref %s)", clipString(c.Text, 40), c.Ref)
	}
	switch reason {
	case "not-found":
		return browserErr(browserErrCommandInvalid, "no element with that ref on the page; read the page again")
	case "not-submit-control":
		return browserErr(browserErrCommandInvalid, "that ref is not a form or a control that submits one")
	case "ambiguous-submit":
		return browserErr(browserErrCommandInvalid, "that form has several submit controls; name one: %s", list)
	case "no-submit":
		return browserErr(browserErrCommandInvalid, "that form has no visible submit control")
	case "ambiguous-form":
		return browserErr(browserErrCommandInvalid, "the page has several forms; name one by its ref: %s", list)
	case "no-form":
		return browserErr(browserErrCommandInvalid, "the page has no form with a visible submit control")
	case "hidden":
		return browserErr(browserErrCommandInvalid, "the submit control cannot be seen on the page (%s)", clipString(detail, 60))
	}
	return browserErr(browserErrCommandInvalid, "the form could not be prepared")
}

func (r *browserOpRun) prepareSubmit(ctx context.Context) (any, error) {
	pg, k, err := r.page(ctx)
	if err != nil {
		return nil, err
	}
	var p kitPrepare
	var refArg any
	if r.p.Ref != "" {
		refArg = r.p.Ref
	}
	if _, err := k.call(ctx, "prepareSubmit", &p, refArg); err != nil {
		return nil, err
	}
	if !p.OK {
		return nil, prepareRefusal(p.Reason, p.Detail, p.Candidates)
	}
	out := browserPrepareResult{
		TabID: pg.TabID(), URL: p.URL, Origin: p.Origin, Title: clipString(p.Title, 300), Digest: browserDigest(p.Parts),
		SubmitRef: p.SubmitRef, SubmitText: clipString(p.SubmitText, 80), FormName: clipString(p.FormName, 80),
		Fields: []browserSubmitField{},
	}
	for _, f := range p.Fields {
		v := clipString(f.Value, 500)
		if f.Sensitive && v != "" {
			v = "•••"
		}
		out.Fields = append(out.Fields, browserSubmitField{Label: clipString(f.Label, 120), Kind: f.Kind, Value: v, Required: f.Required, Hidden: f.Hidden, Sensitive: f.Sensitive})
	}
	return out, nil
}

type kitSubmitCheck struct {
	OK     bool               `json:"ok"`
	Reason string             `json:"reason"`
	DocID  string             `json:"docId"`
	Parts  browserDigestParts `json:"parts"`
	Token  string             `json:"token"`
}

type kitArm struct {
	OK        bool    `json:"ok"`
	Reason    string  `json:"reason"`
	Detail    string  `json:"detail"`
	DocID     string  `json:"docId"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Submitted bool    `json:"submitted"`
}

func formChanged(msg string) *browserError {
	return browserErr(browserErrSubmitFormChanged, "%s Nothing was submitted.", msg)
}

// submit runs a submit the person approved: re-read the form, refuse unless its
// digest is the one they approved, then press exactly the approved control with
// the kit's guard lifted for that one submission.
func (r *browserOpRun) submit(ctx context.Context) (any, error) {
	pg, k, err := r.page(ctx)
	if err != nil {
		var be *browserError
		if errors.As(err, &be) && be.Code == browserErrTabNotFound {
			return nil, formChanged("The tab with the form was closed.")
		}
		return nil, err
	}
	var chk kitSubmitCheck
	if _, err := k.call(ctx, "submitCheck", &chk, r.p.SubmitRef); err != nil {
		return nil, err
	}
	if !chk.OK {
		return nil, formChanged("The form is no longer on the page.")
	}
	if browserDigest(chk.Parts) != r.p.Digest {
		return nil, formChanged("The form changed after it was approved.")
	}
	how := "click"
	if !pg.TrustedInput() {
		how = "scriptClick"
	}
	var arm kitArm
	if _, err := k.call(ctx, "armSubmit", &arm, r.p.SubmitRef, chk.Token, how); err != nil {
		return nil, err
	}
	if !arm.OK {
		if arm.Reason == "hidden" {
			return nil, formChanged("The submit control is covered or hidden now.")
		}
		return nil, formChanged("The form changed after it was approved.")
	}
	if pg.TrustedInput() {
		if err := pg.Click(ctx, arm.X, arm.Y); err != nil {
			endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = k.call(endCtx, "submitEnd", nil)
			cancel()
			return nil, err
		}
	}
	p, navigated := k.settle(ctx, arm.DocID, browserSubmitQuiet, browserLoadWait)
	observed := navigated
	out := browserClickResult{TabID: pg.TabID(), URL: p.URL, Title: clipString(p.Title, 300), Navigated: navigated, SubmitObserved: &observed}
	if !navigated && !k.hung {
		var e struct {
			Used     bool `json:"used"`
			Invalid  bool `json:"invalid"`
			Requests int  `json:"requests"`
		}
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, endErr := k.call(endCtx, "submitEnd", &e)
		cancel()
		if endErr == nil && !e.Used && e.Invalid && e.Requests == 0 {
			// The browser's own validation refused it (it fired `invalid`, no
			// submit event, and the page sent no request of its own meanwhile):
			// the one outcome that proves nothing was sent.
			return nil, browserErr(browserErrSubmitFormChanged,
				"The page did not accept the submission (it shows a validation message). Nothing was submitted.")
		}
		// No submit event and no navigation is NOT proof that nothing was sent:
		// a control whose script saves by fetch fires neither. Report the click
		// as done, with submitObserved false, rather than invite a second send.
		observed = endErr == nil && e.Used
	}
	if out.URL == "" {
		if info, ierr := r.tabInfo(ctx, pg); ierr == nil {
			out.URL, out.Title = info.URL, info.Title
		}
	}
	return out, nil
}
