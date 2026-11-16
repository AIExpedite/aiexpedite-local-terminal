package main

// Opt-in end-to-end checks of the approved submit against a THROWAWAY headless
// Chrome (see browser_smoke_test.go for the harness and how to run it):
//
//	AIX_BROWSER_SMOKE=1 go test -run TestBrowserSmokeSubmit -v .
//
//   - a submitter's formaction changed after approval refuses the submit (the
//     destination is part of what the person approved);
//   - the browser's own validation refusing the submission reports "nothing
//     was submitted";
//   - a script-driven save (a submit-class type=button posting by fetch, no
//     submit event, no navigation) is NOT reported as unsent;
//   - a hidden input changed after approval refuses the submit;
//   - without trusted input (the AppleScript route's path), the approved
//     control is clicked from the page, so its own onclick runs;
//   - an option matched by its value ("DE") verifies against its label;
//   - a control inside an open shadow root can be clicked;
//   - an approved control whose onclick calls form.submit() (no submit event)
//     posts the approved form, once.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const smokeSubmitPage = `<!doctype html><html><head><title>Submit cases</title></head><body>
<form id="dest" method="post" action="/submit">
  <label for="a">Alpha</label><input id="a" name="a" value="one">
  <input type="hidden" id="amount" name="amount" value="10.00">
  <button id="go" type="submit">Send alpha</button>
</form>
<form id="req" method="post" action="/submit">
  <label for="b">Beta</label><input id="b" name="b" required>
  <button id="go2" type="submit">Send beta</button>
</form>
<form id="ajax" onsubmit="return false">
  <label for="c">Gamma</label><input id="c" name="c" value="three">
  <button id="save" type="button">Save</button>
</form>
<form id="handled" method="post" action="/native">
  <label for="d">Delta</label><input id="d" name="d" value="four">
  <button id="send4" type="submit">Send delta</button>
</form>
<form id="geo">
  <label for="country">Country</label><select id="country" name="country"><option value="">Choose</option><option value="DE">Germany</option><option value="FR">France</option></select>
</form>
<div id="shadowhost"></div>
<form id="scripted" method="post" action="/posted5">
  <label for="e">Epsilon</label><input id="e" name="e" value="five">
  <button id="send5" type="button">Send epsilon</button>
</form>
<script>
var sr = document.getElementById('shadowhost').attachShadow({ mode: 'open' });
sr.innerHTML = '<button id="toggle" type="button">Toggle details</button>';
sr.getElementById('toggle').addEventListener('click', function () { document.getElementById('shadowhost').setAttribute('data-toggled', 'yes'); });
document.getElementById('send5').addEventListener('click', function () { document.getElementById('scripted').submit(); });
document.getElementById('send4').addEventListener('click', function (e) {
  e.preventDefault();
  fetch('/save2', { method: 'POST', body: document.getElementById('d').value });
});
document.getElementById('save').addEventListener('click', function () {
  fetch('/save', { method: 'POST', body: document.getElementById('c').value });
});
</script>
</body></html>`

func TestBrowserSmokeSubmit(t *testing.T) {
	chromePath := smokeChromePath(t)

	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, smokeSubmitPage)
		case "/save":
			fmt.Fprint(w, "saved")
		default:
			fmt.Fprint(w, `<!doctype html><html><head><title>Posted</title></head><body>ok</body></html>`)
		}
	}))
	defer srv.Close()
	hit := func(k string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[k]
	}

	launchSmokeChrome(t, chromePath)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	choice, berr := chooseBrowserRoute(ctx, "darwin", "", discoverChromeDevTools)
	if berr != nil || choice.Route != browserRouteDevTools {
		t.Fatalf("route: %+v %v", choice, berr)
	}
	be, release, err := openBrowserBackend(ctx, choice)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stop := &atomic.Bool{}
	run := func(p browserParams) (any, error) {
		t.Helper()
		return (&browserOpRun{p: p, be: be, stop: stop}).run(ctx)
	}

	res, err := run(browserParams{Op: browserOpOpen, URL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tab := res.(browserPageInfo).TabID
	res, err = run(browserParams{Op: browserOpRead, TabID: tab})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	refs := map[string]string{}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}
	if refs["Send alpha"] == "" || refs["Send beta"] == "" || refs["Save"] == "" {
		t.Fatalf("buttons: %+v", res.(browserReadResult).Buttons)
	}
	if refs["Toggle details"] == "" || refs["Send epsilon"] == "" {
		t.Fatalf("buttons: %+v", res.(browserReadResult).Buttons)
	}

	// An option matched by its value is shown by its label. The fill's closing
	// message lingers over the top of the page; the next steps act right away,
	// so they also prove it no longer reads as covering the controls under it.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Country", Value: "DE"}}})
	if err != nil {
		t.Fatalf("fill select: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeFilled {
		t.Fatalf("fill select by value: %+v", o)
	}
	_, raw, _ := parseBrowserTabID(tab)
	pg, err := be.Page(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(ref string) browserPrepareResult {
		t.Helper()
		res, err := run(browserParams{Op: browserOpPrepareSubmit, TabID: tab, Ref: ref})
		if err != nil {
			t.Fatalf("prepare %s: %v", ref, err)
		}
		return res.(browserPrepareResult)
	}

	// 1. The destination changes after approval: refused, nothing posted anywhere.
	prep := prepare(refs["Send alpha"])
	if _, err := pg.Eval(ctx, `document.getElementById('go').setAttribute('formaction', '/elsewhere'); 'ok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r1"}); !isBrowserCode(err, browserErrSubmitFormChanged) {
		t.Fatalf("formaction changed after approval: %v", err)
	}
	// The form's own action changing is refused the same way.
	if _, err := pg.Eval(ctx, `document.getElementById('go').removeAttribute('formaction'); 'ok'`); err != nil {
		t.Fatal(err)
	}
	prep = prepare(refs["Send alpha"])
	if _, err := pg.Eval(ctx, `document.getElementById('dest').setAttribute('action', '/elsewhere'); 'ok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r2"}); !isBrowserCode(err, browserErrSubmitFormChanged) {
		t.Fatalf("form action changed after approval: %v", err)
	}
	if n := hit("POST /elsewhere") + hit("POST /submit"); n != 0 {
		t.Fatalf("%d post(s) after a refused submit", n)
	}

	// 1b. A hidden input changes after approval: refused.
	if _, err := pg.Eval(ctx, `document.getElementById('dest').setAttribute('action', '/submit'); 'ok'`); err != nil {
		t.Fatal(err)
	}
	prep = prepare(refs["Send alpha"])
	if _, err := pg.Eval(ctx, `document.getElementById('amount').value = '1000.00'; 'ok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r2b"}); !isBrowserCode(err, browserErrSubmitFormChanged) {
		t.Fatalf("hidden input changed after approval: %v", err)
	}
	if n := hit("POST /submit"); n != 0 {
		t.Fatalf("%d post(s) after a hidden value changed", n)
	}

	// 2. The browser's validation refuses it: reported as not submitted.
	prep = prepare(refs["Send beta"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r3"})
	if !isBrowserCode(err, browserErrSubmitFormChanged) || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("required field empty: %v", err)
	}
	if n := hit("POST /submit"); n != 0 {
		t.Fatalf("%d post(s) past the browser's validation", n)
	}

	// 3. A script-driven save: no submit event, no navigation, yet it was sent.
	prep = prepare(refs["Save"])
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r4"})
	if err != nil {
		t.Fatalf("script-driven save reported as failed: %v", err)
	}
	cr := res.(browserClickResult)
	if cr.Navigated || cr.SubmitObserved == nil || *cr.SubmitObserved {
		t.Fatalf("script-driven save: %+v", cr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for hit("POST /save") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := hit("POST /save"); n != 1 {
		t.Fatalf("save posted %d time(s), want 1", n)
	}

	// 4. No trusted input (the AppleScript route's path): the kit clicks the
	// approved control from the page, so a submit button's own onclick (which
	// cancels the native post and saves by fetch) runs, as for a person.
	ub := untrustedBackend{be}
	prep = prepare(refs["Send delta"])
	res, err = (&browserOpRun{p: browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r5"},
		be: ub, stop: stop}).run(ctx)
	if err != nil {
		t.Fatalf("script click: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for hit("POST /save2") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if hit("POST /save2") != 1 || hit("POST /native") != 0 {
		t.Fatalf("script click: save2=%d native=%d (%+v)", hit("POST /save2"), hit("POST /native"), res)
	}

	// 5. A control inside an open shadow root is clicked, not reported covered.
	if _, err := run(browserParams{Op: browserOpClick, TabID: tab, Ref: refs["Toggle details"]}); err != nil {
		t.Fatalf("shadow click: %v", err)
	}
	if v, err := pg.Eval(ctx, `document.getElementById('shadowhost').getAttribute('data-toggled') || 'no'`); err != nil || !strings.Contains(v, "yes") {
		t.Fatalf("shadow click had no effect: %q %v", v, err)
	}

	// 6. The approved control's onclick calls form.submit(): no submit event,
	// yet the approved form posts to its own action, once.
	prep = prepare(refs["Send epsilon"])
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r6"})
	if err != nil {
		t.Fatalf("scripted submit: %v", err)
	}
	if cr := res.(browserClickResult); !cr.Navigated || cr.Title != "Posted" || hit("POST /posted5") != 1 {
		t.Fatalf("scripted submit: %+v posted5=%d", cr, hit("POST /posted5"))
	}
}

// untrustedBackend hands out pages without trusted input, as the AppleScript
// route does, over a real DevTools page.
type untrustedBackend struct{ browserBackend }

func (u untrustedBackend) Page(ctx context.Context, raw string) (browserPage, error) {
	pg, err := u.browserBackend.Page(ctx, raw)
	if err != nil {
		return nil, err
	}
	return untrustedPage{pg}, nil
}

type untrustedPage struct{ browserPage }

func (untrustedPage) TrustedInput() bool { return false }
