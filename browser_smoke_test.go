package main

// An opt-in end-to-end run of the DevTools route against a THROWAWAY headless
// Google Chrome this test launches itself, with its own --user-data-dir and
// --remote-debugging-port=0 (the flag the agent itself never uses: here it
// stands in for the person ticking "Allow remote debugging", without the
// consent dialog). It never touches the person's own Chrome or profile, and
// kills only the Chrome it started.
//
//	AIX_BROWSER_SMOKE=1 go test -run TestBrowserSmoke -v .
//
// CHROME_PATH overrides the Chrome binary (default: the macOS app bundle).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const smokeFormPage = `<!doctype html><html><head><title>Smoke form</title></head><body>
<h1>Contact us</h1>
<p>Fill this in and we will get back to you.</p>
<a id="next" href="/page2">Read about us</a>
<form id="contact" method="post" action="/submit">
  <label for="name">Full name</label><input id="name" name="name">
  <label for="email">Email</label><input id="email" name="email" type="email">
  <label for="topic">Topic</label><select id="topic" name="topic"><option value="">Choose</option><option>Sales</option><option>Support</option></select>
  <label><input type="checkbox" name="news" id="news"> Send me news</label>
  <label for="pw">Password</label><input id="pw" name="pw" type="password">
  <input type="hidden" name="csrf" value="tok">
  <button type="submit">Send message</button>
</form>
</body></html>`

const smokePage2 = `<!doctype html><html><head><title>About</title></head><body><h1>About us</h1><p>We make things.</p></body></html>`

func TestBrowserSmoke(t *testing.T) {
	chromePath := smokeChromePath(t)

	var subMu sync.Mutex
	var submissions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, smokeFormPage)
		case "/page2":
			fmt.Fprint(w, smokePage2)
		case "/submit":
			_ = r.ParseForm()
			subMu.Lock()
			submissions = append(submissions, r.Form.Encode())
			subMu.Unlock()
			fmt.Fprint(w, `<!doctype html><html><head><title>Thanks</title></head><body>Thanks!</body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

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
		r := &browserOpRun{p: p, be: be, stop: stop}
		res, err := r.run(ctx)
		if r.shot != nil {
			t.Logf("screenshot %dx%d %d bytes", r.shot.Width, r.shot.Height, len(r.shot.Data))
		}
		return res, err
	}
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	// open
	res, err := run(browserParams{Op: browserOpOpen, URL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	opened := res.(browserPageInfo)
	if opened.Title != "Smoke form" {
		t.Fatalf("open: %s", asJSON(opened))
	}
	tab := opened.TabID

	// tabs
	res, err = run(browserParams{Op: browserOpTabs})
	if err != nil || !strings.Contains(asJSON(res), tab) {
		t.Fatalf("tabs: %v %s", err, asJSON(res))
	}

	// read
	res, err = run(browserParams{Op: browserOpRead, TabID: tab})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rd := res.(browserReadResult)
	t.Logf("read: %d links, %d buttons, %d fields, %d forms, text %d chars", len(rd.Links), len(rd.Buttons), len(rd.Fields), len(rd.Forms), len(rd.Text))
	if !strings.Contains(rd.Text, "Contact us") || len(rd.Forms) != 1 || len(rd.Forms[0].SubmitControls) != 1 {
		t.Fatalf("read: %s", asJSON(rd))
	}
	var submitRef, linkRef string
	for _, b := range rd.Buttons {
		if b.Text == "Send message" && b.SubmitClass {
			submitRef = b.Ref
		}
	}
	for _, l := range rd.Links {
		if l.Text == "Read about us" {
			linkRef = l.Ref
		}
	}
	if submitRef == "" || linkRef == "" {
		t.Fatalf("read: no submit button or link: %s", asJSON(rd))
	}
	for _, f := range rd.Fields {
		if f.Kind == "text" && f.Label == "Password" && f.Sensitive == nil {
			t.Fatalf("password not sensitive: %s", asJSON(f))
		}
	}

	// click on a submit-class control is refused
	if _, err := run(browserParams{Op: browserOpClick, TabID: tab, Ref: submitRef}); !isBrowserCode(err, browserErrSubmitNeedsApproval) {
		t.Fatalf("click submit: %v", err)
	}

	// fill
	res, err = run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{
		{Field: "Full name", Value: "Ada Lovelace"},
		{Field: "Email", Value: "ada@example.com"},
		{Field: "Topic", Value: "Support"},
		{Field: "Send me news", Value: "yes"},
		{Field: "Password", Value: "hunter2"},
		{Field: "Phone", Value: "123"},
	}})
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	fr := res.(browserFillResult)
	t.Logf("fill: %s", asJSON(fr))
	want := []string{"filled", "filled", "filled", "filled", "refused-sensitive", "no-match"}
	for i, o := range fr.Outcomes {
		if o.Outcome != want[i] {
			t.Errorf("fill outcome %d (%s) = %s, want %s", i, o.Field, o.Outcome, want[i])
		}
	}
	if fr.Filled != 4 {
		t.Errorf("filled = %d", fr.Filled)
	}

	// screenshot
	res, err = run(browserParams{Op: browserOpScreenshot, TabID: tab})
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if sr := res.(browserScreenshotResult); sr.Bytes == 0 || sr.ContentType != "image/jpeg" || sr.Width == 0 {
		t.Fatalf("screenshot: %s", asJSON(sr))
	}
	if _, err = run(browserParams{Op: browserOpScreenshot, TabID: tab, FullPage: true}); err != nil {
		t.Fatalf("full screenshot: %v", err)
	}

	// prepareSubmit (no ref: the page's one form)
	res, err = run(browserParams{Op: browserOpPrepareSubmit, TabID: tab})
	if err != nil {
		t.Fatalf("prepareSubmit: %v", err)
	}
	prep := res.(browserPrepareResult)
	t.Logf("prepare: %s", asJSON(prep))
	if prep.SubmitText != "Send message" || len(prep.Digest) != 64 || prep.FormName == "" {
		t.Fatalf("prepare: %s", asJSON(prep))
	}
	for _, f := range prep.Fields {
		if f.Label == "Password" && f.Value != "" {
			t.Fatalf("password value leaked into the summary: %q", f.Value)
		}
	}
	// prepare again: the same form gives the same digest
	res2, _ := run(browserParams{Op: browserOpPrepareSubmit, TabID: tab, Ref: submitRef})
	if res2.(browserPrepareResult).Digest != prep.Digest {
		t.Fatal("digest not stable")
	}

	// a stale digest submits nothing
	if _, err := run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: strings.Repeat("0", 64), RequestID: "r1"}); !isBrowserCode(err, browserErrSubmitFormChanged) {
		t.Fatalf("stale submit: %v", err)
	}
	// the form changes after approval: nothing is submitted
	if _, err := run(browserParams{Op: browserOpFill, TabID: tab, Values: []browserPlanValue{{Field: "Full name", Value: "Grace Hopper"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r2"}); !isBrowserCode(err, browserErrSubmitFormChanged) {
		t.Fatalf("changed form submitted: %v", err)
	}
	subMu.Lock()
	n := len(submissions)
	subMu.Unlock()
	if n != 0 {
		t.Fatalf("%d submission(s) before approval", n)
	}

	// prepare again and submit what was approved
	res, err = run(browserParams{Op: browserOpPrepareSubmit, TabID: tab})
	if err != nil {
		t.Fatal(err)
	}
	prep = res.(browserPrepareResult)
	res, err = run(browserParams{Op: browserOpSubmit, TabID: tab, SubmitRef: prep.SubmitRef, Digest: prep.Digest, RequestID: "r3"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	sub := res.(browserClickResult)
	if !sub.Navigated || sub.Title != "Thanks" {
		t.Fatalf("submit: %s", asJSON(sub))
	}
	subMu.Lock()
	got := append([]string(nil), submissions...)
	subMu.Unlock()
	if len(got) != 1 || !strings.Contains(got[0], "name=Grace+Hopper") || !strings.Contains(got[0], "topic=Support") {
		t.Fatalf("submissions: %v", got)
	}

	// navigate back, click a link, reload
	if res, err = run(browserParams{Op: browserOpNavigate, TabID: tab, Action: browserNavBack}); err != nil {
		t.Fatalf("back: %v", err)
	}
	t.Logf("back: %s", asJSON(res))
	res, err = run(browserParams{Op: browserOpRead, TabID: tab, IncludeText: new(bool)})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.(browserReadResult).Links {
		if l.Text == "Read about us" {
			linkRef = l.Ref
		}
	}
	res, err = run(browserParams{Op: browserOpClick, TabID: tab, Ref: linkRef})
	if err != nil {
		t.Fatalf("click link: %v", err)
	}
	if cr := res.(browserClickResult); !cr.Navigated || cr.Title != "About" {
		t.Fatalf("click link: %s", asJSON(cr))
	}
	if res, err = run(browserParams{Op: browserOpNavigate, TabID: tab, Action: browserNavReload}); err != nil || res.(browserPageInfo).Title != "About" {
		t.Fatalf("reload: %v %s", err, asJSON(res))
	}
	if res, err = run(browserParams{Op: browserOpNavigate, TabID: tab, URL: srv.URL + "/"}); err != nil || res.(browserPageInfo).Title != "Smoke form" {
		t.Fatalf("navigate url: %v %s", err, asJSON(res))
	}
	subMu.Lock()
	n = len(submissions)
	subMu.Unlock()
	if n != 1 {
		t.Fatalf("%d submissions, want exactly 1", n)
	}
}

func isBrowserCode(err error, code string) bool {
	var be *browserError
	return errors.As(err, &be) && be.Code == code
}

// launchSmokeChrome starts a throwaway headless Chrome with its own profile and
// points discovery at it; the test's cleanup kills it and removes the profile.
func launchSmokeChrome(t *testing.T, chromePath string) {
	t.Helper()
	launchSmokeChromeWith(t, chromePath, false)
}

// launchSmokeChromeWith launches the throwaway Chrome; headed shows real
// windows (focus and visibility need them: browser_smoke_current_test.go).
func launchSmokeChromeWith(t *testing.T, chromePath string, headed bool) {
	t.Helper()
	root := os.Getenv("AIX_BROWSER_SMOKE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	profile, err := os.MkdirTemp(root, "chrome-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--user-data-dir=" + profile, "--remote-debugging-port=0",
		"--no-first-run", "--no-default-browser-check", "about:blank"}
	if runtime.GOOS == "darwin" {
		args = append([]string{"--use-mock-keychain"}, args...)
	}
	if !headed {
		args = append([]string{"--headless=new", "--disable-gpu"}, args...)
	}
	cmd := exec.Command(chromePath, args...)
	cmd.Env = smokeChromeEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		globalCDPManager.closeAll()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(profile)
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort")); err == nil && strings.Count(string(b), "\n") >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the smoke Chrome wrote no DevToolsActivePort")
		}
		time.Sleep(100 * time.Millisecond)
	}
	prev := browserChromeUserDataDirOverride
	browserChromeUserDataDirOverride = profile
	t.Cleanup(func() { browserChromeUserDataDirOverride = prev })

}

// smokeChromeEnv is the environment for the throwaway Chrome: the suite's own,
// but with the profile, app-data and temp variables TestMain sandboxed put
// back. On Windows a Chrome launched under the sandboxed USERPROFILE hands the
// launch to the person's own running Chrome instead of starting on its
// --user-data-dir. The throwaway profile is still the only one it uses.
func smokeChromeEnv() []string {
	real := map[string]string{
		"HOME": realHomeAtStartup, "USERPROFILE": realHomeAtStartup, "APPDATA": realAppDataAtStartup,
		"TEMP": realTempAtStartup, "TMP": realTempAtStartup, "TMPDIR": realTempAtStartup,
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if v, ok := real[strings.ToUpper(key)]; ok && v != "" {
			continue
		}
		env = append(env, kv)
	}
	for key, v := range real {
		if v != "" {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func smokeChromePath(t *testing.T) string {
	t.Helper()
	if os.Getenv("AIX_BROWSER_SMOKE") != "1" {
		t.Skip("set AIX_BROWSER_SMOKE=1 to run against a throwaway headless Chrome")
	}
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		chromePath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	if _, err := os.Stat(chromePath); err != nil {
		t.Skipf("no Chrome at %s", chromePath)
	}
	return chromePath
}
