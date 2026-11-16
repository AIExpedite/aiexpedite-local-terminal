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
//     posts the approved form, once;
//   - an approved submit button in a form inside an open shadow root submits
//     (its click and submit events are retargeted or not composed);
//   - a form with a control named "elements" still lists its submit button;
//   - a field the page repurposes during the highlight delay is not filled;
//   - an approved button whose own click handler changes a value it sends is
//     stopped, while a token written into an empty hidden input is allowed;
//   - an unapproved click whose handler posts a new form into a hidden iframe
//     is blocked by the network backstop;
//   - an approved button whose handler natively submits a SECOND form to the
//     same action with other values is refused (the outgoing data counts);
//   - an unapproved click whose handler sends a GET form with data into a
//     hidden iframe is blocked by the network backstop;
//   - a value a blur handler clears is reported mismatch, not filled;
//   - same-named form-less radios in two shadow roots are two groups;
//   - an option inside a disabled <optgroup> is never chosen.

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
<form id="shadowed" method="post" action="/posted8"><label for="el">Elements</label><input id="el" name="elements" value="x"><button type="submit">Send eta</button></form>
<form id="clearing"><label for="nu">Nu</label><input id="nu" name="nu"></form>
<form id="retheta"><label for="th">Theta</label><input id="th" name="theta"></form>
<form id="tamper" method="post" action="/posted9"><label for="io">Iota</label><input id="io" name="iota" value="nine"><input type="hidden" id="amt9" name="amount" value="10.00"><button id="send9" type="submit">Send iota</button></form>
<form id="token" method="post" action="/posted10"><label for="ka">Kappa</label><input id="ka" name="kappa" value="ten"><input type="hidden" id="tok10" name="captcha" value=""><button id="send10" type="submit">Send kappa</button></form>
<button id="sneaky" type="button">Show more</button>
<form id="forge" method="post" action="/posted11"><label for="mu">Mu</label><input id="mu" name="mu" value="eleven"><input type="hidden" name="amount" value="10.00"><button id="send11" type="submit">Send mu</button></form>
<button id="peek" type="button">More options</button>
<div id="radios1"></div><div id="radios2"></div>
<form id="plans"><label for="plan">Plan</label><select id="plan" name="plan"><option value="">Choose</option><optgroup label="Old" disabled><option>Legacy</option></optgroup><option>Basic</option></select></form>
<div id="shadowhost"></div>
<div id="shadowform"></div>
<form id="scripted" method="post" action="/posted5">
  <label for="e">Epsilon</label><input id="e" name="e" value="five">
  <button id="send5" type="button">Send epsilon</button>
</form>
<script>
[['radios1', 'Newsletter'], ['radios2', 'Terms']].forEach(function (x) {
  var r = document.getElementById(x[0]).attachShadow({ mode: 'open' });
  r.innerHTML = '<fieldset><legend>' + x[1] + '</legend><label><input type="radio" name="agree" value="yes"> Yes</label><label><input type="radio" name="agree" value="no"> No</label></fieldset>';
});
document.getElementById('nu').addEventListener('blur', function () { this.value = ''; });
document.getElementById('send9').addEventListener('click', function () { document.getElementById('amt9').value = '1000.00'; });
document.getElementById('send10').addEventListener('click', function () { document.getElementById('tok10').value = 'tok-abc'; });
document.getElementById('sneaky').addEventListener('click', function () {
  var f = document.createElement('iframe'); f.name = 'sink'; f.style.display = 'none'; document.body.appendChild(f);
  var fm = document.createElement('form'); fm.method = 'post'; fm.action = '/sneaky'; fm.target = 'sink';
  var i = document.createElement('input'); i.name = 'x'; i.value = 'y'; fm.appendChild(i); document.body.appendChild(fm);
  fm.submit();
});
document.getElementById('send11').addEventListener('click', function (e) {
  e.preventDefault();
  var fm = document.createElement('form'); fm.method = 'post'; fm.action = '/posted11';
  [['mu', 'eleven'], ['amount', '9999']].forEach(function (p) { var i = document.createElement('input'); i.type = 'hidden'; i.name = p[0]; i.value = p[1]; fm.appendChild(i); });
  document.body.appendChild(fm);
  HTMLFormElement.prototype.submit.call(fm);
});
document.getElementById('peek').addEventListener('click', function () {
  var f = document.createElement('iframe'); f.name = 'sink2'; f.style.display = 'none'; document.body.appendChild(f);
  var fm = document.createElement('form'); fm.method = 'get'; fm.action = '/peek'; fm.target = 'sink2';
  var i = document.createElement('input'); i.name = 'q'; i.value = 'secret'; fm.appendChild(i); document.body.appendChild(fm);
  fm.submit();
});
var sr = document.getElementById('shadowhost').attachShadow({ mode: 'open' });
sr.innerHTML = '<button id="toggle" type="button">Toggle details</button>';
var sf = document.getElementById('shadowform').attachShadow({ mode: 'open' });
sf.innerHTML = '<form method="post" action="/posted7"><label for="z">Zeta</label><input id="z" name="z" value="six"><button type="submit">Send zeta</button></form>';
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
	// A control named "elements" shadows form.elements: the submit button is
	// still found, and the form can be prepared by its ref.
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	etaForm := ""
	for _, f := range res.(browserReadResult).Forms {
		for _, c := range f.SubmitControls {
			if c.Text == "Send eta" {
				etaForm = f.FormRef
			}
		}
	}
	if etaForm == "" {
		t.Fatalf("the form with a control named elements lists no submit control: %+v", res.(browserReadResult).Forms)
	}
	if r, err := run(browserParams{Op: browserOpPrepareSubmit, TabID: tab, Ref: etaForm}); err != nil || r.(browserPrepareResult).SubmitText != "Send eta" {
		t.Fatalf("prepare by form ref: %v %+v", err, r)
	}
	// Same-named radios in two shadow roots are two groups; a disabled
	// optgroup's option is never chosen.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{
		{Field: "Newsletter", Value: "Yes"}, {Field: "Terms", Value: "No"}, {Field: "Plan", Value: "Legacy"},
	}})
	if err != nil {
		t.Fatalf("fill radios/plan: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 3 || o[0].Outcome != browserOutcomeFilled || o[1].Outcome != browserOutcomeFilled || o[2].Outcome == browserOutcomeFilled {
		t.Fatalf("shadow radio groups / disabled optgroup: %+v", o)
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

	// The page repurposes the planned field during the highlight delay: it is
	// reported form-changed and nothing is written into it.
	if _, err := pg.Eval(ctx, `setTimeout(function () { document.getElementById('th').setAttribute('name', 'notes'); }, 250); 'ok'`); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Theta", Value: "hello"}}})
	if err != nil {
		t.Fatalf("fill theta: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeFormChanged {
		t.Fatalf("repurposed field: %+v", o)
	}
	if v, _ := pg.Eval(ctx, `document.getElementById('th').value || 'empty'`); !strings.Contains(v, "empty") {
		t.Fatalf("a repurposed field was written: %q", v)
	}

	// A field whose blur handler clears the value: the read-back comes after
	// the blur, so it is reported as not holding the value, never as filled.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Nu", Value: "hello"}}})
	if err != nil {
		t.Fatalf("fill nu: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("a value the blur handler cleared: %+v", o)
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

	// 1a. The destination and identities reach the digest whole, never as a
	// 32-bit fingerprint two destinations could share; and an arm needs the
	// exact reading its check kept.
	if _, err := pg.Eval(ctx, `document.getElementById('go').setAttribute('formaction', '/pay?recipient=329599'); 'ok'`); err != nil {
		t.Fatal(err)
	}
	kr := newKitRunner(pg)
	var chk kitSubmitCheck
	if _, err := kr.call(ctx, "submitCheck", &chk, refs["Send alpha"]); err != nil || !chk.OK {
		t.Fatalf("submitCheck: %+v %v", chk, err)
	}
	if !strings.Contains(chk.Parts.Submit, "/pay?recipient=329599") || !strings.Contains(chk.Parts.Form, "/elsewhere") {
		t.Fatalf("destinations not kept whole: form %q submit %q", chk.Parts.Form, chk.Parts.Submit)
	}
	var arm kitArm
	if _, err := kr.call(ctx, "armSubmit", &arm, refs["Send alpha"], "not-the-token", "click"); err != nil || arm.OK {
		t.Fatalf("arm with a foreign token: %+v %v", arm, err)
	}
	if _, err := pg.Eval(ctx, `document.getElementById('go').removeAttribute('formaction'); 'ok'`); err != nil {
		t.Fatal(err)
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

	// 4b. The approved button's own click handler changes a value it sends.
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}
	prep = prepare(refs["Send iota"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r9"})
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted9") != 0 {
		t.Fatalf("changed during activation: %v posted9=%d", err, hit("POST /posted9"))
	}

	// 4c. An unapproved click whose handler posts a new form into a hidden
	// iframe: the network backstop fails it.
	if _, err := run(browserParams{Op: browserOpClick, TabID: tab, Ref: refs["Show more"]}); err != nil {
		t.Fatalf("click show more: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := hit("POST /sneaky"); n != 0 {
		t.Fatalf("an unapproved click posted a form %d time(s)", n)
	}

	// 4d. An approved submit button whose click handler cancels the approved
	// post and natively submits a SECOND form to the same action with other
	// values (amount 9999): the outgoing data is not what was approved, so
	// nothing reaches the server and the submit reports the form changed.
	prep = prepare(refs["Send mu"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r11"})
	time.Sleep(500 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted11") != 0 {
		t.Fatalf("forged second form: %v posted11=%d", err, hit("POST /posted11"))
	}

	// 4e. An unapproved, non-submit-class button whose handler submits a GET
	// form carrying data into a new hidden iframe: no GET with that data
	// reaches the server.
	if refs["More options"] == "" {
		t.Fatalf("no More options button: %+v", refs)
	}
	if _, err := run(browserParams{Op: browserOpClick, TabID: tab, Ref: refs["More options"]}); err != nil {
		t.Fatalf("click more options: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := hit("GET /peek"); n != 0 {
		t.Fatalf("an unapproved click sent a GET form into an iframe %d time(s)", n)
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

	// 7. A submit button in a form inside an open shadow root.
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	zeta := ""
	for _, b := range res.(browserReadResult).Buttons {
		if b.Text == "Send zeta" {
			zeta = b.Ref
		}
	}
	if zeta == "" {
		t.Fatalf("no shadow-root submit button: %+v", res.(browserReadResult).Buttons)
	}
	prep = prepare(zeta)
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r7"})
	if err != nil {
		t.Fatalf("shadow-root submit: %v", err)
	}
	if cr := res.(browserClickResult); !cr.Navigated || cr.Title != "Posted" || hit("POST /posted7") != 1 {
		t.Fatalf("shadow-root submit: %+v posted7=%d", cr, hit("POST /posted7"))
	}

	// 8. An anti-bot token written into an empty hidden input on the way out
	// is allowed: the approved values are unchanged.
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	kappa := ""
	for _, b := range res.(browserReadResult).Buttons {
		if b.Text == "Send kappa" {
			kappa = b.Ref
		}
	}
	prep = prepare(kappa)
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r10"})
	if err != nil {
		t.Fatalf("token submit: %v", err)
	}
	if cr := res.(browserClickResult); !cr.Navigated || hit("POST /posted10") != 1 {
		t.Fatalf("token submit: %+v posted10=%d", cr, hit("POST /posted10"))
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
