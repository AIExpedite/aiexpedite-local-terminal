// browser_contract.go — the agent's copy of the browser-control vocabulary.
//
// The Go agent cannot import @aiexpedite/shared-constants, so every wire value
// it shares with terminal-service and ai-service is pinned HERE, once, with the
// export it mirrors (shared-constants src/constants/browserControl.js and
// BROWSER_ERROR_CODES in src/constants/errorCodes.js). browser_contract_test.go
// pins the literals: a rename on either side is a data migration and an agent
// flag day, so change both or neither.
package main

import (
	"runtime"
	"time"
)

// BROWSER_DEVICE_COMMAND: the one built-in command; the operation is inside its
// single JSON argument.
const browserDeviceCommand = "__browser__"

// BROWSER_CONTROL_CAPABILITY_KEY / BROWSER_CONTROL_MIN_AGENT_PROTOCOL: what
// `capabilities.browserControl` reports. terminal-service sends commands only
// to a computer whose protocol is at least the minimum.
const (
	browserControlCapabilityKey = "browserControl"
	browserAgentProtocol        = 1
	// The params envelope version (`v`) this build reads.
	browserParamsVersion = 1
)

// BROWSER_ROUTES.
const (
	browserRouteDevTools    = "devtools"
	browserRouteAppleScript = "applescript"
)

// BROWSER_OPS.
const (
	browserOpTabs          = "tabs"
	browserOpOpen          = "open"
	browserOpRead          = "read"
	browserOpFill          = "fill"
	browserOpClick         = "click"
	browserOpNavigate      = "navigate"
	browserOpScreenshot    = "screenshot"
	browserOpPrepareSubmit = "prepareSubmit"
	browserOpSubmit        = "submit"
	browserOpStop          = "stop"
)

// BROWSER_NAVIGATE_ACTIONS.
const (
	browserNavBack    = "back"
	browserNavForward = "forward"
	browserNavReload  = "reload"
)

// BROWSER_ERROR_CODES the device reports.
const (
	browserErrDeviceOffline       = "BROWSER_DEVICE_OFFLINE"
	browserErrChromeNotReachable  = "BROWSER_CHROME_NOT_REACHABLE"
	browserErrChromeDenied        = "BROWSER_CHROME_DENIED"
	browserErrTabNotFound         = "BROWSER_TAB_NOT_FOUND"
	browserErrPageNotResponding   = "BROWSER_PAGE_NOT_RESPONDING"
	browserErrCommandTimeout      = "BROWSER_COMMAND_TIMEOUT"
	browserErrCommandInvalid      = "BROWSER_COMMAND_INVALID"
	browserErrSubmitNeedsApproval = "BROWSER_SUBMIT_NEEDS_APPROVAL"
	browserErrSubmitFormChanged   = "BROWSER_SUBMIT_FORM_CHANGED"
	browserErrScreenshotUnsupport = "BROWSER_SCREENSHOT_UNSUPPORTED"
	browserErrScreenshotNotFound  = "BROWSER_SCREENSHOT_NOT_FOUND"
)

// BROWSER_FIELD_OUTCOMES the agent itself produces (the kit produces the rest).
const (
	browserOutcomeFilled            = "filled"
	browserOutcomeFilledThenHidden  = "filled-then-hidden"
	browserOutcomeMismatch          = "mismatch"
	browserOutcomeAlreadySet        = "already-set"
	browserOutcomeNotFound          = "not-found"
	browserOutcomeNoMatch           = "no-match"
	browserOutcomeOnlyHiddenMatch   = "only-hidden-match"
	browserOutcomeAmbiguous         = "ambiguous"
	browserOutcomeOtherForm         = "other-form"
	browserOutcomeFormChanged       = "form-changed"
	browserOutcomeStoppedMidField   = "stopped-mid-field"
	browserOutcomeSkippedStopped    = "skipped-stopped"
	browserOutcomePageNotResponding = "page-not-responding"
	browserOutcomeFailed            = "failed"
	browserOutcomeRefusedHidden     = "refused-hidden"
)

// BROWSER_LIMITS.
const (
	browserMaxPlanValues    = 100
	browserMaxValueChars    = 4000
	browserMaxLabelChars    = 300
	browserMaxURLChars      = 4096
	browserMaxPageTextChars = 20000
	browserMaxReadItems     = 300
	browserMaxScreenshot    = 8 * 1024 * 1024
	browserMaxResultBytes   = 1024 * 1024
	browserCommandTTL       = 3 * time.Minute
)

// The agent's own bounds (not shared vocabulary).
const (
	// A params argument larger than this is refused (the wire contract's cap).
	browserMaxParamsBytes = 256 * 1024
	// The DevTools connection closes after this long unused: Chrome asks
	// "Allow remote debugging?" once per connection, so it is kept while used.
	browserCDPIdleClose = 10 * time.Minute
	// After a prepareSubmit the connection (and so the kit's refs, which live
	// in the page's isolated world for as long as the connection does) stays
	// open at least this long: a prepared request lives 15 minutes, plus the
	// 10 s undo window, plus the execute grace, so a late approval still finds
	// the control it approved.
	browserCDPPrepareHold = 20 * time.Minute
	// A command whose signed deadline is further away than this is clamped.
	browserMaxOpDuration = 5 * time.Minute
	// Time kept back from a command's deadline to post its result.
	browserResultMargin = 1500 * time.Millisecond
	// The most a result's own JSON may take: terminal-service stores at most
	// 900,000 bytes of it (MAX_STORED_RESULT_BYTES) and refuses a larger one,
	// so the agent trims to this, with room for encoding differences.
	browserResultBudget = 880_000
	// How long the result poster keeps retrying a 5xx / network failure.
	browserResultRetryWindow = 60 * time.Second
)

// browserControlCapability is `capabilities.browserControl`
// (isBrowserControlCapable reads `supported === true` and a numeric protocol).
type browserControlCapability struct {
	Supported bool     `json:"supported"`
	Protocol  int      `json:"protocol"`
	Routes    []string `json:"routes"`
}

func currentBrowserControlCapability() *browserControlCapability {
	return &browserControlCapability{Supported: true, Protocol: browserAgentProtocol, Routes: browserRoutesFor(runtime.GOOS)}
}

// browserRoutesFor: DevTools everywhere Chrome runs; AppleScript on macOS only.
func browserRoutesFor(goos string) []string {
	if goos == "darwin" {
		return []string{browserRouteDevTools, browserRouteAppleScript}
	}
	return []string{browserRouteDevTools}
}
