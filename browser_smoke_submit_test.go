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
//     submit event, no navigation) is NOT reported as unsent.

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
<script>
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
}
