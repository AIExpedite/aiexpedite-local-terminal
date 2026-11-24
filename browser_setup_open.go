// browser_setup_open.go — the `openSetup` op: open a FIXED browser-control
// setup destination on this computer (BROWSER_CONTROL.md "Opening the setup
// page").
//
// The cloud sends only a destination CODE from a closed set
// (shared-constants BROWSER_SETUP_DESTINATIONS), never a URL. Each code maps to
// one compiled-in action:
//
//   - chromeRemoteDebugging: start the installed, verified Google Chrome (not
//     the default browser) with chrome://inspect/#remote-debugging as its ONLY
//     argument. Chrome's process singleton hands the address to the running
//     instance and profile, or Chrome starts with it when closed.
//   - macosAutomation (macOS only): `open` System Settings on Privacy &
//     Security > Automation.
//
// Anything else is refused. This file never passes a Chrome switch
// (--remote-debugging-*, --user-data-dir, automation flags), never writes
// Chrome's Preferences / Local State, never clicks or ticks anything and never
// touches macOS privacy (TCC) settings: the person turns remote debugging on
// and answers Chrome's prompt themselves. It never uses openBrowser (main.go):
// the OS URL handler may not be Chrome and does not open chrome:// reliably.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// BROWSER_SETUP_DESTINATIONS (shared-constants browserControl.js).
const (
	browserSetupChromeRemoteDebugging = "chromeRemoteDebugging"
	browserSetupMacOSAutomation       = "macosAutomation"
)

// BROWSER_SETUP_OPEN_OUTCOMES the device reports (`skipped` is the cloud's own;
// a launch failure is reported as an error, with its reason).
const (
	browserSetupOutcomeOpened             = "opened"
	browserSetupOutcomeChromeNotInstalled = "chrome_not_installed"
)

// CHROME_REMOTE_DEBUGGING_ADDRESS (shared-constants), pinned by the contract test.
const setupChromeRemoteDebuggingAddress = "chrome://inspect/#remote-debugging"

// The System Settings pane for Privacy & Security > Automation (macOS 13+).
const setupMacOSAutomationURL = "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_Automation"

func isBrowserSetupDestination(code string) bool {
	return code == browserSetupChromeRemoteDebugging || code == browserSetupMacOSAutomation
}

// setupOpenArgv is the exact argv for a destination on an OS. `chrome` is the
// verified Chrome binary (chromeRemoteDebugging only).
func setupOpenArgv(goos, destination, chrome string) ([]string, *browserError) {
	switch destination {
	case browserSetupChromeRemoteDebugging:
		if chrome == "" {
			return nil, browserErr(browserErrCommandInvalid, "Google Chrome was not found")
		}
		return []string{chrome, setupChromeRemoteDebuggingAddress}, nil
	case browserSetupMacOSAutomation:
		if goos != "darwin" {
			return nil, browserErr(browserErrCommandInvalid, "that setup page exists only on macOS")
		}
		return []string{"open", setupMacOSAutomationURL}, nil
	}
	return nil, browserErr(browserErrCommandInvalid, "unknown setup destination")
}

// windowsChromeAppPaths reads the registry's App Paths\chrome.exe (HKCU, then
// HKLM). Set on Windows (browser_setup_open_windows.go); nil elsewhere.
var windowsChromeAppPaths = func() []string { return nil }

// chromeExecutableCandidates lists where Google Chrome is installed, most
// specific first. `env` and `lookPath` are seams for tests.
func chromeExecutableCandidates(goos string, env func(string) string, lookPath func(string) (string, error), evalSymlinks func(string) (string, error)) []string {
	var out []string
	switch goos {
	case "windows":
		out = append(out, windowsChromeAppPaths()...)
		for _, root := range []string{env("ProgramFiles"), env("ProgramFiles(x86)"), env("LocalAppData")} {
			if root != "" {
				out = append(out, root+`\Google\Chrome\Application\chrome.exe`)
			}
		}
	case "darwin":
		const inApp = "Google Chrome.app/Contents/MacOS/Google Chrome"
		out = append(out, "/Applications/"+inApp)
		if home := env("HOME"); home != "" {
			out = append(out, strings.TrimSuffix(home, "/")+"/Applications/"+inApp)
		}
	default:
		out = append(out, "/opt/google/chrome/chrome")
		for _, name := range []string{"google-chrome-stable", "google-chrome"} {
			p, err := lookPath(name)
			if err != nil {
				continue
			}
			if resolved, err := evalSymlinks(p); err == nil {
				p = resolved
			}
			// Debian's wrapper script (/opt/google/chrome/google-chrome) execs
			// the sibling binary: that binary is what is verified and run.
			if strings.HasPrefix(p, "/opt/google/chrome/") {
				p = "/opt/google/chrome/chrome"
			}
			out = append(out, p)
		}
	}
	return out
}

// resolveChromeExecutable returns the first candidate that exists and passes
// verifyChromeExecutable (Google's code signature on macOS, the install path
// elsewhere), or "" when Chrome is not installed.
func resolveChromeExecutable(ctx context.Context, goos string, candidates []string, exists func(string) bool, verify func(context.Context, string, string) error) string {
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if !exists(c) {
			continue
		}
		if err := verify(ctx, goos, c); err != nil {
			continue
		}
		return c
	}
	return ""
}

// browserSetupOpener runs one setup open. Its seams are replaced in tests.
type browserSetupOpener struct {
	goos       string
	findChrome func(ctx context.Context) string
	start      func(argv []string) error
}

func newBrowserSetupOpener() *browserSetupOpener {
	return &browserSetupOpener{
		goos: runtime.GOOS,
		findChrome: func(ctx context.Context) string {
			return resolveChromeExecutable(ctx, runtime.GOOS,
				chromeExecutableCandidates(runtime.GOOS, os.Getenv, exec.LookPath, filepath.EvalSymlinks),
				fileExists, verifyChromeExecutable)
		},
		start: startDetached,
	}
}

// startDetached starts argv with no shell, no extra arguments and the agent's
// own environment, and reaps it in the background.
func startDetached(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// open runs the destination's fixed action. The result is `{outcome}`.
func (o *browserSetupOpener) open(ctx context.Context, destination string) (map[string]any, *browserError) {
	if !isBrowserSetupDestination(destination) {
		return nil, browserErr(browserErrCommandInvalid, "unknown setup destination")
	}
	chrome := ""
	if destination == browserSetupChromeRemoteDebugging {
		chrome = o.findChrome(ctx)
		if chrome == "" {
			return map[string]any{"outcome": browserSetupOutcomeChromeNotInstalled}, nil
		}
	}
	argv, berr := setupOpenArgv(o.goos, destination, chrome)
	if berr != nil {
		return nil, berr
	}
	if err := o.start(argv); err != nil {
		return nil, browserErr(browserErrChromeNotReachable, "the setup page could not be opened: %s", trimErr(err))
	}
	return map[string]any{"outcome": browserSetupOutcomeOpened}, nil
}

func trimErr(err error) string {
	s := fmt.Sprint(err)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
