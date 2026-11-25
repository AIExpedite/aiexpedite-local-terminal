// browser_chrome.go — finding the person's own Chrome and choosing how to reach it.
//
// DevTools route: Chrome's consent-gated remote debugging ("Allow remote
// debugging for this browser instance" at chrome://inspect/#remote-debugging,
// Chrome 144+). Chrome then writes DevToolsActivePort into its user-data dir:
// line 1 the port, line 2 the browser endpoint path (/devtools/browser/<id>).
// The agent reads that file in the DEFAULT profile's dir, checks the port is
// listened on by Google Chrome itself (on macOS: codesigned by Google), and
// dials ws://127.0.0.1:<port><path>. It never passes --remote-debugging-port
// and never launches Chrome to control it (openSetup only starts Chrome on its
// own setup page, browser_setup_open.go).
//
// AppleScript route (macOS only): Chrome's `execute javascript`, used when
// remote debugging is off.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// googleTeamID is Google LLC's Apple Developer Team ID. Google Chrome is signed with it.
const googleTeamID = "EQHXZ8M8AV"

// cdpEndpoint is where Chrome's DevTools server listens, and what was checked about it.
type cdpEndpoint struct {
	Port   int
	WSPath string
	PID    int
	Exe    string
}

func (e cdpEndpoint) wsURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d%s", e.Port, e.WSPath)
}

// errDevToolsOff: remote debugging is not on (no DevToolsActivePort, or nothing
// listens on the port it names). Not a failure: the AppleScript route may serve.
var errDevToolsOff = errors.New("Chrome's remote debugging is off")

// browserChromeUserDataDirOverride points discovery at another user-data dir.
// Tests (and the opt-in smoke test against a throwaway headless Chrome) only.
var browserChromeUserDataDirOverride string

// defaultChromeUserDataDir is the default profile dir of Google Chrome (stable).
func defaultChromeUserDataDir(goos, home, localAppData string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	case "windows":
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(localAppData, "Google", "Chrome", "User Data")
	default:
		return filepath.Join(home, ".config", "google-chrome")
	}
}

func chromeUserDataDir() string {
	if browserChromeUserDataDirOverride != "" {
		return browserChromeUserDataDirOverride
	}
	home, _ := os.UserHomeDir()
	return defaultChromeUserDataDir(runtime.GOOS, home, os.Getenv("LOCALAPPDATA"))
}

// parseDevToolsActivePort reads the file's two lines: the port, and the browser
// endpoint path. Without the path Chrome never answers the WebSocket, so a file
// without it reads as "off".
func parseDevToolsActivePort(content string) (int, string, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(content), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return 0, "", errors.New("DevToolsActivePort has no browser path")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, "", errors.New("unreadable DevToolsActivePort port")
	}
	path := strings.TrimSpace(lines[1])
	const prefix = "/devtools/browser/"
	if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) || len(path) > 200 ||
		strings.ContainsAny(path, " ?#\t") {
		return 0, "", errors.New("unreadable DevToolsActivePort path")
	}
	return port, path, nil
}

// Seams (tests replace them).
var (
	readDevToolsActivePortFile = func(dir string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
		if err != nil {
			return "", err
		}
		if len(b) > 4096 {
			return "", errors.New("DevToolsActivePort is too large")
		}
		return string(b), nil
	}
	browserPortListening = func(port int) bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	browserVerifyChromeListener = verifyChromeListener
)

// chromeVerifyCache remembers a verified listener (pid + port), so codesign
// runs once per Chrome process rather than once per command.
var chromeVerifyCache struct {
	mu   sync.Mutex
	port int
	pid  int
	exe  string
	at   time.Time
}

// discoverChromeDevTools returns the endpoint of the person's Chrome, or
// errDevToolsOff, or a *browserError when something that is not Google Chrome
// holds the port.
func discoverChromeDevTools(ctx context.Context) (cdpEndpoint, error) {
	content, err := readDevToolsActivePortFile(chromeUserDataDir())
	if err != nil {
		return cdpEndpoint{}, errDevToolsOff
	}
	port, path, err := parseDevToolsActivePort(content)
	if err != nil {
		return cdpEndpoint{}, errDevToolsOff
	}
	if !browserPortListening(port) {
		return cdpEndpoint{}, errDevToolsOff // a file left behind by a Chrome that quit
	}
	pid, exe, err := browserVerifyChromeListener(ctx, port)
	if err != nil {
		return cdpEndpoint{}, browserErr(browserErrChromeNotReachable, "the program on Chrome's debugging port is not Google Chrome (%v)", err)
	}
	return cdpEndpoint{Port: port, WSPath: path, PID: pid, Exe: exe}, nil
}

// verifyChromeListener makes sure the process listening on the port is Google
// Chrome, so page contents and form values never reach a program squatting on
// the debugging port.
func verifyChromeListener(ctx context.Context, port int) (int, string, error) {
	pid, err := pidListeningOn(ctx, port)
	if err != nil {
		return 0, "", err
	}
	chromeVerifyCache.mu.Lock()
	if chromeVerifyCache.port == port && chromeVerifyCache.pid == pid && time.Since(chromeVerifyCache.at) < time.Hour {
		exe := chromeVerifyCache.exe
		chromeVerifyCache.mu.Unlock()
		return pid, exe, nil
	}
	chromeVerifyCache.mu.Unlock()
	exe, err := chromeExecutableOf(ctx, pid)
	if err != nil {
		return pid, "", err
	}
	if err := verifyChromeExecutable(ctx, runtime.GOOS, exe); err != nil {
		return pid, exe, err
	}
	chromeVerifyCache.mu.Lock()
	chromeVerifyCache.port, chromeVerifyCache.pid, chromeVerifyCache.exe, chromeVerifyCache.at = port, pid, exe, time.Now()
	chromeVerifyCache.mu.Unlock()
	return pid, exe, nil
}

func browserRunOut(ctx context.Context, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	hideWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}

func pidListeningOn(ctx context.Context, port int) (int, error) {
	if runtime.GOOS == "darwin" {
		out, err := browserRunOut(ctx, "lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fp")
		if err != nil {
			return 0, fmt.Errorf("nothing is listening on port %d", port)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "p") {
				if pid, err := strconv.Atoi(l[1:]); err == nil {
					return pid, nil
				}
			}
		}
		return 0, fmt.Errorf("could not tell which process listens on port %d", port)
	}
	conns, err := gnet.ConnectionsWithContext(ctx, "tcp")
	if err != nil {
		return 0, fmt.Errorf("could not list listening ports: %w", err)
	}
	for _, c := range conns {
		if c.Status == "LISTEN" && int(c.Laddr.Port) == port && c.Pid > 0 {
			return int(c.Pid), nil
		}
	}
	return 0, fmt.Errorf("could not tell which process listens on port %d", port)
}

func chromeExecutableOf(ctx context.Context, pid int) (string, error) {
	if runtime.GOOS == "darwin" {
		exe, err := browserRunOut(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid))
		exe = strings.TrimSpace(exe)
		if err != nil || exe == "" {
			return "", fmt.Errorf("cannot read the executable of process %d", pid)
		}
		return exe, nil
	}
	proc, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return "", fmt.Errorf("cannot read process %d", pid)
	}
	exe, err := proc.ExeWithContext(ctx)
	if err != nil || exe == "" {
		return "", fmt.Errorf("cannot read the executable of process %d", pid)
	}
	return exe, nil
}

// verifyChromeExecutable: on macOS, the code signature of the app bundle must be
// Google's Chrome; elsewhere the executable must be Google Chrome's chrome
// binary by name and install path.
func verifyChromeExecutable(ctx context.Context, goos, exe string) error {
	if goos == "darwin" {
		app := exe
		if i := strings.Index(exe, ".app/"); i >= 0 {
			app = exe[:i+4]
		}
		req := fmt.Sprintf(`=anchor apple generic and identifier "com.google.Chrome" and certificate leaf[subject.OU] = "%s"`, googleTeamID)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "codesign", "--verify", "-R", req, app).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s is not Google Chrome signed by Google: %s", filepath.Base(app), strings.TrimSpace(string(out)))
		}
		return nil
	}
	return checkChromeExecutablePath(goos, exe)
}

func checkChromeExecutablePath(goos, exe string) error {
	base := strings.ToLower(filepath.Base(exe))
	if goos == "windows" {
		base = strings.ToLower(exe[strings.LastIndexAny(exe, `\/`)+1:])
	}
	want := "chrome"
	if goos == "windows" {
		want = "chrome.exe"
	}
	if base != want || !strings.Contains(strings.ToLower(exe), "google") {
		return fmt.Errorf("%s is not Google Chrome", base)
	}
	return nil
}

// browserRouteChoice is the route one command runs on.
type browserRouteChoice struct {
	Route    string
	Endpoint cdpEndpoint
}

// chooseBrowserRoute: a tab keeps the route that found it; otherwise DevTools
// when remote debugging is on, else AppleScript on macOS, else
// BROWSER_CHROME_NOT_REACHABLE.
func chooseBrowserRoute(ctx context.Context, goos, tabRoute string, discover func(context.Context) (cdpEndpoint, error)) (browserRouteChoice, *browserError) {
	if tabRoute == browserRouteAppleScript {
		if goos != "darwin" {
			return browserRouteChoice{}, browserErr(browserErrTabNotFound, "that tab is not on this computer; list the tabs again")
		}
		return browserRouteChoice{Route: browserRouteAppleScript}, nil
	}
	ep, err := discover(ctx)
	if err == nil {
		return browserRouteChoice{Route: browserRouteDevTools, Endpoint: ep}, nil
	}
	var be *browserError
	if errors.As(err, &be) && (tabRoute == browserRouteDevTools || goos != "darwin") {
		return browserRouteChoice{}, be
	}
	if tabRoute == browserRouteDevTools {
		return browserRouteChoice{}, browserErr(browserErrChromeNotReachable,
			"Chrome's remote debugging is off now, so that tab cannot be reached; turn it on from the setup card in the chat, or list the tabs again")
	}
	if goos == "darwin" {
		return browserRouteChoice{Route: browserRouteAppleScript}, nil
	}
	return browserRouteChoice{}, browserErr(browserErrChromeNotReachable,
		"Chrome is not running, or its remote debugging is off: the setup card in the chat opens Chrome's setup page; tick \"Allow remote debugging for this browser instance\" there")
}
