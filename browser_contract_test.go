package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Unit tests never reach the person's own Chrome: discovery reads a dir that
// does not exist unless a test points it somewhere (the opt-in smoke tests
// point it at their own throwaway headless Chrome).
func init() {
	browserChromeUserDataDirOverride = filepath.Join(os.TempDir(), "aix-browser-tests-no-chrome")
}

// Contract pins. The Go agent cannot import shared-constants, so these literals
// are its copy of BROWSER_DEVICE_COMMAND, BROWSER_OPS, BROWSER_ROUTES,
// BROWSER_NAVIGATE_ACTIONS, BROWSER_ERROR_CODES, BROWSER_FIELD_OUTCOMES and
// BROWSER_LIMITS (shared-constants src/constants/browserControl.js and
// errorCodes.js). A rename on either side is a data migration and an agent flag
// day: change both or neither.
func TestBrowserContractPins(t *testing.T) {
	pins := [][2]string{
		{browserDeviceCommand, "__browser__"},
		{browserControlCapabilityKey, "browserControl"},
		{browserRouteDevTools, "devtools"},
		{browserRouteAppleScript, "applescript"},
		{browserOpTabs, "tabs"},
		{browserOpOpen, "open"},
		{browserOpRead, "read"},
		{browserOpFill, "fill"},
		{browserOpClick, "click"},
		{browserOpNavigate, "navigate"},
		{browserOpScreenshot, "screenshot"},
		{browserOpPrepareSubmit, "prepareSubmit"},
		{browserOpSubmit, "submit"},
		{browserOpStop, "stop"},
		{browserNavBack, "back"},
		{browserNavForward, "forward"},
		{browserNavReload, "reload"},
		{browserErrDeviceOffline, "BROWSER_DEVICE_OFFLINE"},
		{browserErrChromeNotReachable, "BROWSER_CHROME_NOT_REACHABLE"},
		{browserErrChromeDenied, "BROWSER_CHROME_DENIED"},
		{browserErrTabNotFound, "BROWSER_TAB_NOT_FOUND"},
		{browserErrPageNotResponding, "BROWSER_PAGE_NOT_RESPONDING"},
		{browserErrCommandTimeout, "BROWSER_COMMAND_TIMEOUT"},
		{browserErrCommandInvalid, "BROWSER_COMMAND_INVALID"},
		{browserErrSubmitNeedsApproval, "BROWSER_SUBMIT_NEEDS_APPROVAL"},
		{browserErrSubmitFormChanged, "BROWSER_SUBMIT_FORM_CHANGED"},
		{browserErrScreenshotUnsupport, "BROWSER_SCREENSHOT_UNSUPPORTED"},
		{browserErrScreenshotNotFound, "BROWSER_SCREENSHOT_NOT_FOUND"},
		{browserOutcomeFilled, "filled"},
		{browserOutcomeFilledThenHidden, "filled-then-hidden"},
		{browserOutcomeMismatch, "mismatch"},
		{browserOutcomeAlreadySet, "already-set"},
		{browserOutcomeNotFound, "not-found"},
		{browserOutcomeNoMatch, "no-match"},
		{browserOutcomeOnlyHiddenMatch, "only-hidden-match"},
		{browserOutcomeAmbiguous, "ambiguous"},
		{browserOutcomeOtherForm, "other-form"},
		{browserOutcomeFormChanged, "form-changed"},
		{browserOutcomeStoppedMidField, "stopped-mid-field"},
		{browserOutcomeSkippedStopped, "skipped-stopped"},
		{browserOutcomePageNotResponding, "page-not-responding"},
		{browserOutcomeFailed, "failed"},
		{browserOutcomeRefusedHidden, "refused-hidden"},
	}
	for _, p := range pins {
		if p[0] != p[1] {
			t.Errorf("pinned value drifted: %q != %q", p[0], p[1])
		}
	}
	limits := map[string][2]int{
		"MAX_PLAN_VALUES":      {browserMaxPlanValues, 100},
		"MAX_VALUE_CHARS":      {browserMaxValueChars, 4000},
		"MAX_LABEL_CHARS":      {browserMaxLabelChars, 300},
		"MAX_URL_CHARS":        {browserMaxURLChars, 4096},
		"MAX_PAGE_TEXT_CHARS":  {browserMaxPageTextChars, 20000},
		"MAX_READ_ITEMS":       {browserMaxReadItems, 300},
		"MAX_SCREENSHOT_BYTES": {browserMaxScreenshot, 8 * 1024 * 1024},
		"MAX_RESULT_BYTES":     {browserMaxResultBytes, 1024 * 1024},
		"COMMAND_TTL_MS":       {int(browserCommandTTL.Milliseconds()), 180000},
	}
	for name, v := range limits {
		if v[0] != v[1] {
			t.Errorf("BROWSER_LIMITS.%s drifted: %d != %d", name, v[0], v[1])
		}
	}
}

// capabilities.browserControl is what isBrowserControlCapable reads:
// `supported === true` and a numeric protocol >= BROWSER_CONTROL_MIN_AGENT_PROTOCOL (1).
func TestBrowserControlCapabilityShape(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  `{"supported":true,"protocol":1,"routes":["devtools","applescript"]}`,
		"windows": `{"supported":true,"protocol":1,"routes":["devtools"]}`,
		"linux":   `{"supported":true,"protocol":1,"routes":["devtools"]}`,
	} {
		raw, _ := json.Marshal(&browserControlCapability{Supported: true, Protocol: browserAgentProtocol, Routes: browserRoutesFor(goos)})
		if string(raw) != want {
			t.Errorf("%s: %s, want %s", goos, raw, want)
		}
	}
	// newMachineCapabilities carries it: systemInfo_capabilities_test.go pins
	// the whole capabilities object, browserControl included.
	if c := currentBrowserControlCapability(); !c.Supported || c.Protocol != 1 || c.Routes[0] != browserRouteDevTools {
		t.Fatalf("%+v", c)
	}
}

// The kit and the Chrome helper are embedded, and every kit call the agent
// makes exists in the kit.
func TestBrowserKitEmbedded(t *testing.T) {
	if !strings.Contains(browserKitJS, "var VERSION = 'aix-1.0.27'") {
		t.Fatal("kit version missing")
	}
	for _, name := range []string{"ping", "scan", "snapshot", "fingerprint", "check", "begin", "end", "status", "fillStep",
		"clickBegin", "clickNow", "clickEnd", "prepareSubmit", "submitCheck", "armSubmit", "submitEnd"} {
		if !strings.Contains(browserKitJS, name+": ") && !strings.Contains(browserKitJS, " "+name+": "+name) {
			t.Errorf("kit does not export %s", name)
		}
	}
	if strings.Contains(browserKitJS, "harness") {
		t.Error("the POC's test-harness calls must not ship in the agent's kit")
	}
	for _, op := range []string{"'status'", "'list'", "'open'", "'info'", "'navigate'", "'back'", "'forward'", "'reload'", "'exec'"} {
		if !strings.Contains(browserChromeHelperJS, op) {
			t.Errorf("Chrome helper does not handle %s", op)
		}
	}
	if !strings.Contains(browserChromeHelperJS, "chrome.running()") {
		t.Error("Chrome helper must check Chrome is running before any Apple Event (it never launches Chrome)")
	}
}

// The DevTools allowlist is the narrowing of what Chrome grants: nothing that
// reads cookies, storage or the network, nothing that closes a tab.
func TestBrowserCDPAllowlist(t *testing.T) {
	for m := range cdpAllowlist {
		for _, banned := range []string{"Network.", "Storage.", "Browser.", "Cookie", "Target.closeTarget", "Runtime.callFunctionOn", "Page.setDownloadBehavior"} {
			if strings.Contains(m, banned) {
				t.Errorf("%s must not be allowed", m)
			}
		}
	}
	for _, need := range []string{"Target.createTarget", "Page.navigate", "Page.navigateToHistoryEntry", "Page.getNavigationHistory",
		"Page.reload", "Page.captureScreenshot", "Page.getLayoutMetrics", "Input.dispatchMouseEvent", "Runtime.evaluate"} {
		if !cdpAllowlist[need] {
			t.Errorf("%s missing from the allowlist", need)
		}
	}
	c := &cdpClient{pending: map[int64]chan cdpReply{}}
	if err := c.Call(t.Context(), "", "Network.getCookies", nil, nil); err == nil || !strings.Contains(err.Error(), "refused locally") {
		t.Fatalf("Network.getCookies: %v", err)
	}
}

// The AppleScript helper starts (one ready line, no Apple Event sent) and,
// when the agent goes away (stdin closes), exits instead of spinning.
func TestBrowserChromeHelperStartsAndExitsOnEOF(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript is macOS only")
	}
	b := &asBridge{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.acquire(ctx); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	err := b.start(ctx)
	b.release()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cmd := b.cmd
	_ = b.in.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the helper kept running after stdin closed")
	}
}
