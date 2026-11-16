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
//   - an approved button sharing its name with a filled field submits;
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
<form id="xi" method="post" action="/posted12"><label for="xa">Xi</label><input id="xa" name="action" value="edit"><button id="send12" type="submit" name="action" value="save">Save xi</button></form>
<form id="clearing"><label for="nu">Nu</label><input id="nu" name="nu"></form>
<form id="retheta"><label for="th">Theta</label><input id="th" name="theta"></form>
<form id="tamper" method="post" action="/posted9"><label for="io">Iota</label><input id="io" name="iota" value="nine"><input type="hidden" id="amt9" name="amount" value="10.00"><button id="send9" type="submit">Send iota</button></form>
<form id="token" method="post" action="/posted10"><label for="ka">Kappa</label><input id="ka" name="kappa" value="ten"><input type="hidden" id="tok10" name="captcha" value=""><button id="send10" type="submit">Send kappa</button></form>
<button id="sneaky" type="button">Show more</button>
<form id="forge" method="post" action="/posted11"><label for="mu">Mu</label><input id="mu" name="mu" value="eleven"><input type="hidden" name="amount" value="10.00"><button id="send11" type="submit">Send mu</button></form>
<button id="peek" type="button">More options</button>
<form id="omicron" method="post" action="/posted13"><label for="om">Omicron</label><input id="om" name="omicron" value="thirteen"><button id="send13" type="submit">Send omicron</button></form>
<iframe name="sink14" style="display:none"></iframe>
<form id="pi" method="post" action="/posted14"><label for="pi1">Pi</label><input id="pi1" name="pi" value="fourteen"><input type="hidden" id="amt14" name="amount" value="10.00"><button id="send14" type="submit" formtarget="sink14">Send pi</button></form>
<div id="shadowform2"></div>
<form id="tau" method="get" action="/got16"><label for="tx">Tau notes</label><textarea id="tx" name="tau">first line
second line</textarea><button id="send16" type="submit">Send tau</button></form>
<form id="upsilon" method="post" action="/posted18"><label for="up">Upsilon</label><input id="up" name="upsilon" value="eighteen"><input type="hidden" name="item[]" value="A"><input type="hidden" id="q18a" name="qty[]" value="1"><input type="hidden" name="item[]" value="B"><input type="hidden" id="q18b" name="qty[]" value="2"><button id="send18" type="submit">Send upsilon</button></form>
<form id="chi" method="post" action="/posted19"><label for="c1x">Chi first</label><input id="c1x" name="action" value="save"><label for="c2x">Chi second</label><input id="c2x" name="action" value="edit"><button type="submit" name="action" value="save">Save chi</button></form>
<form id="phi"><label for="sa">Signed amount</label><input id="sa" name="signed"><label for="da">Decimal amount</label><input id="da" name="decimal"><label for="acct">Tracking reference</label><input id="acct" name="tracking"><label for="pr">Unit price</label><input id="pr" name="price"></form>
<form id="omega"><label for="cw">Code word</label><input id="cw" name="codeword"><fieldset><legend>Answer</legend><label><input type="radio" name="ans" id="ansNo" value="no"> No</label><label><input type="radio" name="ans" id="ansNs" value="notsure"> Not sure</label></fieldset><fieldset><legend>Updates</legend><label><input type="radio" name="upd" value="agree"> I agree to receive occasional product updates and understand that I can unsubscribe at any time from my settings page</label><label><input type="radio" name="upd" value="decline"> No thanks</label></fieldset></form>
<iframe name="sink21" style="display:none"></iframe>
<form id="ups21" method="post" action="/posted21"><label for="k21">Kappa one</label><input id="k21" name="kappa" value="twentyone"><input type="hidden" id="amt21" name="amount" value="10.00"><button type="submit">Send kappa one</button></form>
<form id="ups22" method="post" action="/posted22"><label for="k22">Kappa two</label><input id="k22" name="kappa" value="twentytwo"><input type="hidden" id="amt22" name="amount" value="10.00"><button type="submit">Send kappa two</button></form>
<form id="ups23" method="post" action="/posted23"><label for="k23">Kappa three</label><input id="k23" name="kappa" value="twentythree"><button type="submit">Send kappa three</button></form>
<form id="cities"><label for="cb24">Shipping city</label><input id="cb24" name="shipcity" role="combobox" aria-autocomplete="list" aria-expanded="false" aria-controls="lb24"><ul id="lb24" role="listbox" style="display:none"><li role="option">Boston</li><li role="option">Denver</li></ul></form>
<form id="tiers"><label for="tier">Tier</label><select id="tier" name="tier"><option value="">Choose</option><option value="cheap">Standard</option><option value="expensive">Standard</option></select></form>
<form id="psi" method="get" action="/got20"><label for="ps">Psi</label><input id="ps" name="psi" value="twenty"><button id="send20" type="button">Send psi</button></form>
<form id="rho" method="post" action="/posted15"><label for="rh">Rho</label><input id="rh" name="rho" value="fifteen"><button id="send15" type="submit" formtarget="sink14">Send rho</button></form>
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
document.getElementById('send13').addEventListener('click', function (e) {
  e.preventDefault();
  var fm = document.createElement('form'); fm.method = 'get'; fm.action = '/elsewhere13';
  var i = document.createElement('input'); i.name = 'q'; i.value = 'stolen'; fm.appendChild(i); document.body.appendChild(fm);
  HTMLFormElement.prototype.submit.call(fm);
});
document.getElementById('pi').addEventListener('submit', function () { document.getElementById('amt14').value = '9999.00'; });
document.getElementById('send18').addEventListener('click', function () { var a = document.getElementById('q18a'), b = document.getElementById('q18b'), t = a.value; a.value = b.value; b.value = t; });
document.getElementById('sa').addEventListener('blur', function () { this.value = this.value.replace('-', ''); });
document.getElementById('da').addEventListener('blur', function () { this.value = this.value.replace('.', ','); });
document.getElementById('acct').addEventListener('blur', function () { this.value = this.value.replace(/3$/, '2'); });
document.getElementById('pr').addEventListener('blur', function () { this.value = this.value.replace(/,/g, '').replace(/0+$/, ''); });
document.getElementById('cw').addEventListener('focus', function () { this.type = 'password'; });
document.getElementById('ansNo').addEventListener('change', function () { document.getElementById('ansNs').checked = true; });
['21', '22'].forEach(function (n) {
  document.getElementById('ups' + n).addEventListener('submit', function () { this.target = 'sink21'; document.getElementById('amt' + n).value = '9999.00'; });
});
document.getElementById('ups23').addEventListener('submit', function (e) { if (e.submitter) e.submitter.setAttribute('formtarget', 'sink21'); e.stopImmediatePropagation(); });
document.getElementById('tier').addEventListener('change', function () { if (this.value === 'cheap') this.value = 'expensive'; });
document.getElementById('cb24').addEventListener('focus', function () { this.type = 'password'; });
document.getElementById('send20').addEventListener('click', function () {
  var f = document.getElementById('psi');
  HTMLFormElement.prototype.submit.call(f);
  setTimeout(function () { HTMLFormElement.prototype.submit.call(f); }, 400);
});
var sf2 = document.getElementById('shadowform2').attachShadow({ mode: 'open' });
sf2.innerHTML = '<form method="post" action="/posted17" target="sink14"><label for="sg">Sigma</label><input id="sg" name="sigma" value="seventeen"><input type="hidden" id="amt17" name="amount" value="10.00"><button type="submit">Send sigma</button></form>';
sf2.querySelector('form').addEventListener('submit', function () { sf2.getElementById('amt17').value = '9999.00'; });
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
		case "/got20":
			w.WriteHeader(http.StatusNoContent)
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

	// 4d2. The approved button shares its name with a filled field
	// (action=edit + button action=save): the unchanged form submits.
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}
	prep = prepare(refs["Save xi"])
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r12"})
	if err != nil || hit("POST /posted12") != 1 {
		t.Fatalf("submitter sharing a field's name: %v posted12=%d", err, hit("POST /posted12"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d3. The approved button's click handler natively submits its own GET
	// form to ANOTHER address: refused, nothing reaches either address.
	prep = prepare(refs["Send omicron"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r13"})
	time.Sleep(300 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("GET /elsewhere13") != 0 || hit("POST /posted13") != 0 {
		t.Fatalf("handler's GET form elsewhere: %v elsewhere13=%d posted13=%d", err, hit("GET /elsewhere13"), hit("POST /posted13"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d4. An approved button aimed at an iframe (formtarget) whose FORM's own
	// submit handler changes a hidden amount after the capture-phase check:
	// refused, nothing posted.
	prep = prepare(refs["Send pi"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r14"})
	time.Sleep(300 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted14") != 0 {
		t.Fatalf("formtarget + submit handler change: %v posted14=%d", err, hit("POST /posted14"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d5. The same iframe-targeted button with no handler changing anything:
	// the approved form posts, once.
	prep = prepare(refs["Send rho"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r15"})
	deadline = time.Now().Add(5 * time.Second)
	for hit("POST /posted15") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || hit("POST /posted15") != 1 {
		t.Fatalf("iframe-targeted approved submit: %v posted15=%d", err, hit("POST /posted15"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d6. An approved form inside an open shadow root, aimed at an iframe
	// (target), whose own submit handler changes a hidden amount: refused.
	prep = prepare(refs["Send sigma"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r17"})
	time.Sleep(300 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted17") != 0 {
		t.Fatalf("shadow-root form aimed at an iframe: %v posted17=%d", err, hit("POST /posted17"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d7. An approved GET form with a multiline textarea (the submission sends
	// CRLF, the DOM reads LF): the unchanged form submits, once.
	prep = prepare(refs["Send tau"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r16"})
	deadline = time.Now().Add(5 * time.Second)
	for hit("GET /got16") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || hit("GET /got16") != 1 {
		t.Fatalf("multiline GET form: %v got16=%d", err, hit("GET /got16"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d8. Repeated names pair by position: a click handler that swaps the two
	// quantities (item[]=A qty[]=1, item[]=B qty[]=2 -> qty 2,1) is refused.
	prep = prepare(refs["Send upsilon"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r18"})
	time.Sleep(300 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted18") != 0 {
		t.Fatalf("reordered repeated values: %v posted18=%d", err, hit("POST /posted18"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	// 4d10. Repeated fields whose values include the button's own (action=save,
	// action=edit, then a button action=save): the unchanged form submits once.
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}
	prep = prepare(refs["Save chi"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r19"})
	deadline = time.Now().Add(5 * time.Second)
	for hit("POST /posted19") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || hit("POST /posted19") != 1 {
		t.Fatalf("button value among repeated values: %v posted19=%d", err, hit("POST /posted19"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}

	// 4d11. The approved GET form's handler submits it natively twice against an
	// endpoint answering 204 (the page stays): only the first one goes.
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}
	prep = prepare(refs["Send psi"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r20"})
	time.Sleep(1500 * time.Millisecond)
	if err != nil || hit("GET /got20") != 1 {
		t.Fatalf("approved GET form submitted twice: %v got20=%d", err, hit("GET /got20"))
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	// 4d12. A page that turns a decimal point into a thousands separator: mismatch.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Decimal amount", Value: "1.234"}}})
	if err != nil {
		t.Fatalf("fill decimal amount: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("1.234 shown as 1,234 must read as a mismatch: %+v", o)
	}

	// 4d15. A page that changes the last digit of a long identifier (past 2^53, where
	// floating point cannot tell the two apart): mismatch.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Tracking reference", Value: "9007199254740993"}}})
	if err != nil {
		t.Fatalf("fill tracking reference: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("a changed digit of a long number must read as a mismatch: %+v", o)
	}
	// 4d16. The same number written differently (1,200.50 shown as 1200.5): filled.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Unit price", Value: "1,200.50"}}})
	if err != nil {
		t.Fatalf("fill unit price: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeFilled {
		t.Fatalf("1,200.50 shown as 1200.5 is the same number: %+v", o)
	}

	// 4d13. A field that turns into a password field when focused: refused, nothing typed.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Code word", Value: "abc123"}}})
	if err != nil {
		t.Fatalf("fill code word: %v", err)
	}
	// Refused either as sensitive or because its identity changed (the type is part of its
	// fingerprint): what matters is that nothing was typed into it.
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || (o[0].Outcome != "refused-sensitive" && o[0].Outcome != browserOutcomeFormChanged) {
		t.Fatalf("a field that becomes a password on focus must be refused: %+v", o)
	}
	if v, err := pg.Eval(ctx, `JSON.stringify(document.getElementById('cw').value)`); err != nil || v != `""` {
		t.Fatalf("the refused field was written: %q %v", v, err)
	}
	// 4d22. An editable combobox that turns into a password field when focused: refused, nothing typed.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Shipping city", Value: "Boston"}}})
	if err != nil {
		t.Fatalf("fill shipping city: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || (o[0].Outcome != "refused-sensitive" && o[0].Outcome != browserOutcomeFormChanged) {
		t.Fatalf("a combobox that becomes a password on focus must be refused: %+v", o)
	}
	if v, err := pg.Eval(ctx, `JSON.stringify(document.getElementById('cb24').value)`); err != nil || v != `""` {
		t.Fatalf("the refused combobox was written: %q %v", v, err)
	}
	// 4d14. A radio whose handler selects "Not sure" when "No" was asked: mismatch.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Answer", Value: "No"}}})
	if err != nil {
		t.Fatalf("fill answer: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("a radio switched by the page must read as a mismatch: %+v", o)
	}

	// 4d17. A radio option whose label is longer than the option list shows (80
	// characters), chosen by its value: filled, not a mismatch against the clipped label.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Updates", Value: "agree"}}})
	if err != nil {
		t.Fatalf("fill updates: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeFilled {
		t.Fatalf("a long radio label chosen by value must read as filled: %+v", o)
	}
	if v, err := pg.Eval(ctx, `JSON.stringify(document.querySelector('input[name=upd]:checked') && document.querySelector('input[name=upd]:checked').value)`); err != nil || v != `"agree"` {
		t.Fatalf("checked updates option: %q %v", v, err)
	}

	// 4d9. A page that strips the minus sign of a filled amount: mismatch, not filled.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Signed amount", Value: "-10"}}})
	if err != nil {
		t.Fatalf("fill signed amount: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("a stripped minus sign must read as a mismatch: %+v", o)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
	}

	// 4d18. Two options share a label ("Standard" at two prices) and the page switches from
	// the chosen one to the other: mismatch, not filled.
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Tier", Value: "cheap"}}})
	if err != nil {
		t.Fatalf("fill tier: %v", err)
	}
	if o := res.(browserFillResult).Outcomes; len(o) != 1 || o[0].Outcome != browserOutcomeMismatch {
		t.Fatalf("a select switched to another option with the same label must read as a mismatch: %+v", o)
	}

	// 4d19. The approved form's own submit handler, after the capture-phase check, aims the
	// form at a hidden iframe and raises a hidden amount: stopped, nothing sent (trusted click).
	prep = prepare(refs["Send kappa one"])
	_, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r21"})
	time.Sleep(800 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted21") != 0 {
		t.Fatalf("re-aimed and changed in a submit handler (trusted): %v posted21=%d", err, hit("POST /posted21"))
	}
	// 4d20. The same through a script click (the AppleScript route's path).
	prep = prepare(refs["Send kappa two"])
	_, err = (&browserOpRun{p: browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r22"},
		be: ub, stop: stop}).run(ctx)
	time.Sleep(800 * time.Millisecond)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || hit("POST /posted22") != 0 {
		t.Fatalf("re-aimed and changed in a submit handler (script click): %v posted22=%d", err, hit("POST /posted22"))
	}
	// 4d21. A submit handler that re-aims the button itself and stops the event, so nothing of
	// the kit runs after it. Script click: the browser planned the post into the frame before
	// the kit could look, so the result says it may have been sent, never "submitted".
	prep = prepare(refs["Send kappa three"])
	_, err = (&browserOpRun{p: browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r23"},
		be: ub, stop: stop}).run(ctx)
	if !isBrowserCode(err, browserErrSubmitFormChanged) || !strings.Contains(err.Error(), "may have been sent") {
		t.Fatalf("re-aimed with the event stopped (script click) must be reported: %v", err)
	}
	// Trusted click: the pin observer undoes the re-aiming as soon as the handler returns, so the
	// approved data posts to the page itself and the page navigates.
	before23 := hit("POST /posted23")
	prep = prepare(refs["Send kappa three"])
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r23b"})
	if err != nil || hit("POST /posted23") != before23+1 || !res.(browserClickResult).Navigated {
		t.Fatalf("re-aimed with the event stopped (trusted): %v posted23 %d -> %d %+v", err, before23, hit("POST /posted23"), res)
	}
	if _, err := run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.(browserReadResult).Buttons {
		refs[b.Text] = b.Ref
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
