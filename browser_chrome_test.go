package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestParseDevToolsActivePort(t *testing.T) {
	port, path, err := parseDevToolsActivePort("9222\n/devtools/browser/0d8f-11aa\n")
	if err != nil || port != 9222 || path != "/devtools/browser/0d8f-11aa" {
		t.Fatalf("%d %q %v", port, path, err)
	}
	if port, path, err = parseDevToolsActivePort("61000\r\n/devtools/browser/abc\r\n"); err != nil || port != 61000 || path != "/devtools/browser/abc" {
		t.Fatalf("CRLF: %d %q %v", port, path, err)
	}
	for _, bad := range []string{"", "9222", "9222\n", "x\n/devtools/browser/a", "0\n/devtools/browser/a", "70000\n/devtools/browser/a",
		"9222\n/devtools/page/a", "9222\n/devtools/browser/", "9222\n/devtools/browser/a?x=1"} {
		if _, _, err := parseDevToolsActivePort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDefaultChromeUserDataDir(t *testing.T) {
	cases := map[string]string{
		"darwin":  filepath.Join("/Users/p", "Library", "Application Support", "Google", "Chrome"),
		"linux":   filepath.Join("/Users/p", ".config", "google-chrome"),
		"windows": filepath.Join(`C:\L`, "Google", "Chrome", "User Data"),
	}
	for goos, want := range cases {
		if got := defaultChromeUserDataDir(goos, "/Users/p", `C:\L`); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestCheckChromeExecutablePath(t *testing.T) {
	if err := checkChromeExecutablePath("windows", `C:\Program Files\Google\Chrome\Application\chrome.exe`); err != nil {
		t.Error(err)
	}
	if err := checkChromeExecutablePath("linux", "/opt/google/chrome/chrome"); err != nil {
		t.Error(err)
	}
	for goos, exe := range map[string]string{
		"windows": `C:\Users\p\AppData\Local\Temp\chrome.exe`,
		"linux":   "/tmp/google/evil",
	} {
		if checkChromeExecutablePath(goos, exe) == nil {
			t.Errorf("%s: %s accepted", goos, exe)
		}
	}
}

func TestChooseBrowserRoute(t *testing.T) {
	ctx := context.Background()
	ep := cdpEndpoint{Port: 9222, WSPath: "/devtools/browser/x"}
	on := func(context.Context) (cdpEndpoint, error) { return ep, nil }
	off := func(context.Context) (cdpEndpoint, error) { return cdpEndpoint{}, errDevToolsOff }
	squatter := func(context.Context) (cdpEndpoint, error) {
		return cdpEndpoint{}, browserErr(browserErrChromeNotReachable, "not chrome")
	}
	cases := []struct {
		name, goos, tabRoute string
		discover             func(context.Context) (cdpEndpoint, error)
		route, code          string
	}{
		{"devtools on, mac", "darwin", "", on, browserRouteDevTools, ""},
		{"devtools on, windows", "windows", "", on, browserRouteDevTools, ""},
		{"devtools off, mac", "darwin", "", off, browserRouteAppleScript, ""},
		{"devtools off, windows", "windows", "", off, "", browserErrChromeNotReachable},
		{"devtools off, linux", "linux", "", off, "", browserErrChromeNotReachable},
		{"squatter, mac falls back", "darwin", "", squatter, browserRouteAppleScript, ""},
		{"squatter, windows refuses", "windows", "", squatter, "", browserErrChromeNotReachable},
		{"applescript tab stays on applescript", "darwin", browserRouteAppleScript, on, browserRouteAppleScript, ""},
		{"applescript tab elsewhere", "windows", browserRouteAppleScript, on, "", browserErrTabNotFound},
		{"devtools tab, devtools off", "darwin", browserRouteDevTools, off, "", browserErrChromeNotReachable},
		{"devtools tab, squatter", "darwin", browserRouteDevTools, squatter, "", browserErrChromeNotReachable},
	}
	for _, c := range cases {
		got, berr := chooseBrowserRoute(ctx, c.goos, c.tabRoute, c.discover)
		switch {
		case c.code != "" && (berr == nil || berr.Code != c.code):
			t.Errorf("%s: %+v %v, want %s", c.name, got, berr, c.code)
		case c.code == "" && (berr != nil || got.Route != c.route):
			t.Errorf("%s: %+v %v, want %s", c.name, got, berr, c.route)
		}
		if got.Route == browserRouteDevTools && got.Endpoint != ep {
			t.Errorf("%s: endpoint %+v", c.name, got.Endpoint)
		}
	}
}

func TestDiscoverChromeDevTools(t *testing.T) {
	origRead, origListen, origVerify := readDevToolsActivePortFile, browserPortListening, browserVerifyChromeListener
	t.Cleanup(func() {
		readDevToolsActivePortFile, browserPortListening, browserVerifyChromeListener = origRead, origListen, origVerify
	})
	file, fileErr := "9333\n/devtools/browser/abc\n", error(nil)
	listening, verifyErr := true, error(nil)
	readDevToolsActivePortFile = func(string) (string, error) { return file, fileErr }
	browserPortListening = func(int) bool { return listening }
	browserVerifyChromeListener = func(context.Context, int) (int, string, error) { return 42, "/chrome", verifyErr }

	ep, err := discoverChromeDevTools(context.Background())
	if err != nil || ep.Port != 9333 || ep.WSPath != "/devtools/browser/abc" || ep.PID != 42 || ep.wsURL() != "ws://127.0.0.1:9333/devtools/browser/abc" {
		t.Fatalf("%+v %v", ep, err)
	}
	fileErr = errors.New("missing")
	if _, err := discoverChromeDevTools(context.Background()); !errors.Is(err, errDevToolsOff) {
		t.Fatalf("no file: %v", err)
	}
	fileErr, file = nil, "9333\n"
	if _, err := discoverChromeDevTools(context.Background()); !errors.Is(err, errDevToolsOff) {
		t.Fatalf("no path: %v", err)
	}
	file, listening = "9333\n/devtools/browser/abc\n", false
	if _, err := discoverChromeDevTools(context.Background()); !errors.Is(err, errDevToolsOff) {
		t.Fatalf("stale file: %v", err)
	}
	listening, verifyErr = true, errors.New("unsigned")
	_, err = discoverChromeDevTools(context.Background())
	var be *browserError
	if !errors.As(err, &be) || be.Code != browserErrChromeNotReachable {
		t.Fatalf("squatter: %v", err)
	}
}
