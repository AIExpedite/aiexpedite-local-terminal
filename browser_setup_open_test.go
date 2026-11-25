package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Every destination maps to exactly one fixed argv, and no argv ever carries a
// Chrome switch: the agent never turns remote debugging on, never picks a
// profile and never automates Chrome. It only shows the person the page.
func TestSetupOpenArgvIsFixed(t *testing.T) {
	const chrome = `C:\Program Files\Google\Chrome\Application\chrome.exe`
	cases := []struct {
		goos, dest string
		want       []string
	}{
		{"windows", browserSetupChromeRemoteDebugging, []string{chrome, "chrome://inspect/#remote-debugging"}},
		{"darwin", browserSetupChromeRemoteDebugging, []string{chrome, "chrome://inspect/#remote-debugging"}},
		{"linux", browserSetupChromeRemoteDebugging, []string{chrome, "chrome://inspect/#remote-debugging"}},
		{"darwin", browserSetupMacOSAutomation, []string{"open", setupMacOSAutomationURL}},
	}
	for _, c := range cases {
		got, berr := setupOpenArgv(c.goos, c.dest, chrome)
		if berr != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s %s: %v %v", c.goos, c.dest, got, berr)
		}
		for _, arg := range got {
			for _, banned := range []string{"--remote-debugging", "--user-data-dir", "--enable-automation", "--profile-directory", "--"} {
				if strings.HasPrefix(arg, banned) {
					t.Fatalf("argv carries a switch %q", arg)
				}
			}
		}
	}
}

func TestSetupOpenArgvRefusesAnythingElse(t *testing.T) {
	for _, c := range []struct{ goos, dest string }{
		{"windows", browserSetupMacOSAutomation},
		{"linux", browserSetupMacOSAutomation},
		{"darwin", "chrome://settings"},
		{"darwin", "https://evil.test/"},
		{"windows", ""},
		{"windows", "openAnything"},
	} {
		if _, berr := setupOpenArgv(c.goos, c.dest, "chrome"); berr == nil || berr.Code != browserErrCommandInvalid {
			t.Fatalf("%s %q: %v", c.goos, c.dest, berr)
		}
	}
	if _, berr := setupOpenArgv("windows", browserSetupChromeRemoteDebugging, ""); berr == nil {
		t.Fatal("no Chrome binary must refuse")
	}
}

// The params parser accepts only an allowlisted code and drops every other
// field: a URL is never read for a setup open.
func TestOpenSetupParamsAllowlistAndIgnoreURL(t *testing.T) {
	p := browserParams{V: 1, CommandID: "c1", Op: browserOpOpenSetup, DeadlineMs: 1, Destination: browserSetupChromeRemoteDebugging, URL: "https://evil.test/", TabID: "d:T1"}
	if berr := validateBrowserParams(&p); berr != nil {
		t.Fatal(berr)
	}
	if p.URL != "" || p.TabID != "" || p.Destination != browserSetupChromeRemoteDebugging {
		t.Fatalf("fields kept: %+v", p)
	}
	for _, dest := range []string{"", "chrome://inspect/#remote-debugging", "https://example.com", "macosautomation"} {
		q := browserParams{V: 1, CommandID: "c2", Op: browserOpOpenSetup, Destination: dest}
		if berr := validateBrowserParams(&q); berr == nil || berr.Code != browserErrCommandInvalid {
			t.Fatalf("%q accepted", dest)
		}
	}
}

func TestChromeCandidates(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	noLook := func(string) (string, error) { return "", errors.New("none") }
	same := func(p string) (string, error) { return p, nil }

	saved := windowsChromeAppPaths
	defer func() { windowsChromeAppPaths = saved }()
	windowsChromeAppPaths = func() []string { return []string{`D:\Apps\Google\Chrome\Application\chrome.exe`} }
	win := chromeExecutableCandidates("windows", env(map[string]string{"ProgramFiles": `C:\Program Files`, "LocalAppData": `C:\Users\a\AppData\Local`}), noLook, same)
	want := []string{
		`D:\Apps\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Users\a\AppData\Local\Google\Chrome\Application\chrome.exe`,
	}
	if !reflect.DeepEqual(win, want) {
		t.Fatalf("windows: %v", win)
	}

	mac := chromeExecutableCandidates("darwin", env(map[string]string{"HOME": "/Users/a"}), noLook, same)
	if mac[0] != "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" || !strings.HasPrefix(mac[1], "/Users/a/Applications/Google Chrome.app") {
		t.Fatalf("darwin: %v", mac)
	}

	// Debian: google-chrome-stable -> /opt/google/chrome/google-chrome (a
	// wrapper script); the executed binary is its sibling `chrome`.
	look := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	resolve := func(p string) (string, error) { return "/opt/google/chrome/google-chrome", nil }
	linux := chromeExecutableCandidates("linux", env(nil), look, resolve)
	for _, c := range linux {
		if c != "/opt/google/chrome/chrome" {
			t.Fatalf("linux candidate %q", c)
		}
	}

	// A PATH entry resolving outside Google's install directory is never a
	// candidate, even though its path would pass the name-and-path check.
	planted := func(p string) (string, error) { return "/home/a/bin/google/chrome", nil }
	for _, c := range chromeExecutableCandidates("linux", env(nil), look, planted) {
		if c != "/opt/google/chrome/chrome" {
			t.Fatalf("planted PATH binary became a candidate: %q", c)
		}
	}
}

func TestResolveChromeExecutableVerifies(t *testing.T) {
	exists := func(string) bool { return true }
	ctx := context.Background()
	// The real verifier on the real install paths (non-macOS: name and path).
	if got, err := resolveChromeExecutable(ctx, "windows", []string{`C:\Windows\notepad.exe`, `C:\Program Files\Google\Chrome\Application\chrome.exe`}, exists, verifyChromeExecutable); err != nil || got != `C:\Program Files\Google\Chrome\Application\chrome.exe` {
		t.Fatalf("windows: %q %v", got, err)
	}
	if got, err := resolveChromeExecutable(ctx, "linux", []string{"/usr/bin/chromium", "/opt/google/chrome/chrome"}, exists, verifyChromeExecutable); err != nil || got != "/opt/google/chrome/chrome" {
		t.Fatalf("linux: %q %v", got, err)
	}
	// macOS: the code-signature check stubbed to pass for Google's bundle only.
	macVerify := func(_ context.Context, _ string, exe string) error {
		if strings.Contains(exe, "Google Chrome.app") {
			return nil
		}
		return errors.New("not signed by Google")
	}
	mac := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if got, err := resolveChromeExecutable(ctx, "darwin", []string{"/Applications/Evil.app/Contents/MacOS/chrome", mac}, exists, macVerify); err != nil || got != mac {
		t.Fatalf("darwin: %q %v", got, err)
	}
	// A non-Chrome binary is never chosen; nothing found is "".
	if got, err := resolveChromeExecutable(ctx, "linux", []string{"/usr/bin/firefox"}, exists, verifyChromeExecutable); err != nil || got != "" {
		t.Fatalf("non-chrome accepted: %q %v", got, err)
	}
	if got, err := resolveChromeExecutable(ctx, "linux", []string{"/opt/google/chrome/chrome"}, func(string) bool { return false }, verifyChromeExecutable); err != nil || got != "" {
		t.Fatalf("missing file accepted: %q %v", got, err)
	}
}

func TestSetupOpenerOutcomes(t *testing.T) {
	var started [][]string
	o := &browserSetupOpener{
		goos: "windows",
		findChrome: func(context.Context) (string, error) {
			return `C:\Program Files\Google\Chrome\Application\chrome.exe`, nil
		},
		start: func(_ context.Context, argv []string) error { started = append(started, argv); return nil },
	}
	res, berr := o.open(context.Background(), browserSetupChromeRemoteDebugging)
	if berr != nil || res["outcome"] != browserSetupOutcomeOpened || len(started) != 1 || started[0][1] != setupChromeRemoteDebuggingAddress {
		t.Fatalf("%v %v %v", res, berr, started)
	}
	o.findChrome = func(context.Context) (string, error) { return "", nil }
	res, berr = o.open(context.Background(), browserSetupChromeRemoteDebugging)
	if berr != nil || res["outcome"] != browserSetupOutcomeChromeNotInstalled || len(started) != 1 {
		t.Fatalf("not installed: %v %v", res, berr)
	}
	o.findChrome = func(context.Context) (string, error) { return "chrome.exe", nil }
	o.start = func(context.Context, []string) error { return errors.New("exec failed") }
	if _, berr = o.open(context.Background(), browserSetupChromeRemoteDebugging); berr == nil || !strings.Contains(berr.Message, "exec failed") {
		t.Fatalf("launch failure: %v", berr)
	}
	if _, berr = o.open(context.Background(), "https://evil.test"); berr == nil || berr.Code != browserErrCommandInvalid {
		t.Fatalf("unknown: %v", berr)
	}
}

// Architecture: the setup-open path never edits Chrome's preferences, never
// touches macOS privacy settings and never reuses the default-browser opener.
func TestSetupOpenSourceNeverChangesSettings(t *testing.T) {
	raw, err := os.ReadFile("browser_setup_open.go")
	if err != nil {
		t.Fatal(err)
	}
	// Comments describe what is forbidden; only code is checked.
	var code strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		code.WriteString(line + "\n")
	}
	src := code.String()
	for _, banned := range []string{"Preferences", "Local State", "tccutil", "--remote-debugging", "--user-data-dir", "--enable-automation", "openBrowser(", "WriteFile", "OpenFile", "defaults write"} {
		if strings.Contains(src, banned) {
			t.Errorf("browser_setup_open.go code contains %q", banned)
		}
	}
}

// The dispatcher runs openSetup without choosing a route (no DevTools
// discovery, no AppleScript) and posts its outcome; a duplicate is ignored.
func TestBrowserDispatcherOpenSetup(t *testing.T) {
	h := newDispatcherHarness(t, &fakePage{id: "d:T1"})
	routeCalls := 0
	h.d.route = func(ctx context.Context, tabRoute string) (browserRouteChoice, *browserError) {
		routeCalls++
		return browserRouteChoice{}, browserErr(browserErrChromeNotReachable, "off")
	}
	var dests []string
	h.d.setup = func(_ context.Context, dest string) (map[string]any, *browserError) {
		dests = append(dests, dest)
		if dest == browserSetupMacOSAutomation {
			return map[string]any{"outcome": browserSetupOutcomeChromeNotInstalled}, nil
		}
		return map[string]any{"outcome": browserSetupOutcomeOpened}, nil
	}
	h.send(t, map[string]any{"commandId": "s1", "op": "openSetup", "destination": "chromeRemoteDebugging", "url": "https://evil.test"})
	h.wait(t, 1)
	h.send(t, map[string]any{"commandId": "s1", "op": "openSetup", "destination": "chromeRemoteDebugging"})
	h.send(t, map[string]any{"commandId": "s2", "op": "openSetup", "destination": "macosAutomation"})
	h.send(t, map[string]any{"commandId": "s3", "op": "openSetup", "destination": "chrome://settings"})
	h.wait(t, 2)
	if routeCalls != 0 {
		t.Fatalf("openSetup chose a route %d times", routeCalls)
	}
	if r := h.poster.byID("s1"); len(r) != 1 || !r[0].OK || r[0].Result.(map[string]any)["outcome"] != "opened" {
		t.Fatalf("s1: %+v", r)
	}
	if r := h.poster.byID("s2"); len(r) != 1 || r[0].Result.(map[string]any)["outcome"] != "chrome_not_installed" {
		t.Fatalf("s2: %+v", r)
	}
	if r := h.poster.byID("s3"); len(r) != 1 || r[0].OK || r[0].Error.Code != browserErrCommandInvalid {
		t.Fatalf("s3: %+v", r)
	}
	if !reflect.DeepEqual(dests, []string{"chromeRemoteDebugging", "macosAutomation"}) {
		t.Fatalf("setup ran for %v", dests)
	}
}

// startAndWatch re-runs this test binary as the launched process: an early
// non-zero exit is an error carrying stderr, an early clean exit (Chrome handing
// off to its running instance) and a process still running past the grace
// window (a fresh Chrome) are both "opened".
func TestStartAndWatchReportsEarlyFailures(t *testing.T) {
	if mode := os.Getenv("AIX_SETUP_LAUNCH_HELPER"); mode != "" {
		switch mode {
		case "fail":
			fmt.Fprint(os.Stderr, "profile is locked")
			os.Exit(21)
		case "ok":
			os.Exit(0)
		case "linger":
			time.Sleep(time.Second)
			os.Exit(0)
		}
	}
	run := func(mode string, grace time.Duration) error {
		t.Setenv("AIX_SETUP_LAUNCH_HELPER", mode)
		return startAndWatch(context.Background(), []string{os.Args[0], "-test.run=^TestStartAndWatchReportsEarlyFailures$"}, grace)
	}
	if err := run("fail", 10*time.Second); err == nil || !strings.Contains(err.Error(), "profile is locked") || !strings.Contains(err.Error(), "21") {
		t.Fatalf("early failure: %v", err)
	}
	if err := run("ok", 10*time.Second); err != nil {
		t.Fatalf("clean hand-off: %v", err)
	}
	began := time.Now()
	if err := run("linger", 200*time.Millisecond); err != nil || time.Since(began) > 900*time.Millisecond {
		t.Fatalf("still running: %v after %v", err, time.Since(began))
	}
	// Let the lingering helper exit: Windows cannot delete a running test binary.
	time.Sleep(time.Second)
	if err := startAndWatch(context.Background(), []string{"aix-no-such-binary-xyz"}, time.Second); err == nil {
		t.Fatal("missing binary accepted")
	}
}
