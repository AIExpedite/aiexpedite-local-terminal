// browser_params.go — reading and checking one signed `__browser__` command.
//
// The parameters travel as ONE JSON object in the signed Args[0], like the
// WhatsApp commands, so the command signature canonical is unchanged on both
// sides. Tolerant reader: fields this build does not know are ignored; the
// shape of the ones it uses is checked, and a command that cannot be run as
// asked is answered with BROWSER_COMMAND_INVALID rather than guessed at.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type browserPlanValue struct {
	Field     string `json:"field"`
	Value     string `json:"value"`
	Confirmed bool   `json:"confirmed,omitempty"`
}

type browserParams struct {
	V           int                `json:"v"`
	CommandID   string             `json:"commandId"`
	Op          string             `json:"op"`
	DeadlineMs  int64              `json:"deadlineMs"`
	TabID       string             `json:"tabId,omitempty"`
	URL         string             `json:"url,omitempty"`
	Action      string             `json:"action,omitempty"`
	Ref         string             `json:"ref,omitempty"`
	IncludeText *bool              `json:"includeText,omitempty"`
	FullPage    bool               `json:"fullPage,omitempty"`
	Values      []browserPlanValue `json:"values,omitempty"`
	SubmitRef   string             `json:"submitRef,omitempty"`
	Digest      string             `json:"digest,omitempty"`
	RequestID   string             `json:"requestId,omitempty"`
	// AppOrigin (current): the AI Expedite app's own origin; its tabs are
	// never "this page".
	AppOrigin string `json:"appOrigin,omitempty"`
	// Destination (openSetup): a setup destination CODE from the compiled-in
	// allowlist (browser_setup_open.go). Never a URL.
	Destination string `json:"destination,omitempty"`
}

// browserError is a failure reported to the cloud: a BROWSER_* code and a
// short, person-readable message. Messages never carry page text or values.
type browserError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Candidates: the pages a `current` refusal offers the person to choose from.
	Candidates []browserCurrentCandidate `json:"candidates,omitempty"`
}

func (e *browserError) Error() string { return e.Code + ": " + e.Message }

func browserErr(code, format string, a ...any) *browserError {
	return &browserError{Code: code, Message: fmt.Sprintf(format, a...)}
}

var (
	browserCommandIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	browserRefPattern       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	browserDigestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// isBrowserStopCommand: a `__browser__` whose op is stop. It passes a drain and
// the rate limiter, so a person can always stop the agent.
func isBrowserStopCommand(cmd commandMsg) bool {
	if cmd.Command != browserDeviceCommand || len(cmd.Args) != 1 || len(cmd.Args[0]) > browserMaxParamsBytes {
		return false
	}
	var p struct {
		Op string `json:"op"`
	}
	return json.Unmarshal([]byte(cmd.Args[0]), &p) == nil && p.Op == browserOpStop
}

// parseBrowserCommand reads the params. commandID is returned whenever it could
// be read, so a refusal can still be posted against it; an empty commandID
// means nothing can be reported (the command is only logged).
func parseBrowserCommand(cmd commandMsg, now time.Time) (p browserParams, commandID string, berr *browserError) {
	if len(cmd.Args) != 1 {
		return p, "", browserErr(browserErrCommandInvalid, "expected one JSON argument")
	}
	if len(cmd.Args[0]) > browserMaxParamsBytes {
		return p, "", browserErr(browserErrCommandInvalid, "the command is larger than %d bytes", browserMaxParamsBytes)
	}
	if err := json.Unmarshal([]byte(cmd.Args[0]), &p); err != nil {
		// Read the id alone, so a mistyped field can still be answered.
		var idOnly struct {
			CommandID string `json:"commandId"`
		}
		if json.Unmarshal([]byte(cmd.Args[0]), &idOnly) == nil && browserCommandIDPattern.MatchString(idOnly.CommandID) {
			return p, idOnly.CommandID, browserErr(browserErrCommandInvalid, "the command's fields have the wrong types")
		}
		return p, "", browserErr(browserErrCommandInvalid, "the argument is not a JSON object")
	}
	if !browserCommandIDPattern.MatchString(p.CommandID) {
		return p, "", browserErr(browserErrCommandInvalid, "missing or malformed commandId")
	}
	if p.V != browserParamsVersion {
		return p, p.CommandID, browserErr(browserErrCommandInvalid, "unsupported command version %d", p.V)
	}
	if p.DeadlineMs <= 0 {
		p.DeadlineMs = now.Add(browserCommandTTL).UnixMilli()
	}
	if berr := validateBrowserParams(&p); berr != nil {
		return p, p.CommandID, berr
	}
	return p, p.CommandID, nil
}

func validateBrowserParams(p *browserParams) *browserError {
	needTab := func() *browserError {
		if p.TabID == "" {
			return browserErr(browserErrCommandInvalid, "%s needs a tabId from tabs or open", p.Op)
		}
		if _, _, ok := parseBrowserTabID(p.TabID); !ok {
			return browserErr(browserErrTabNotFound, "that tabId is not one this computer gave out; list the tabs again")
		}
		return nil
	}
	switch p.Op {
	case browserOpTabs, browserOpStop:
		return nil
	case browserOpOpenSetup:
		if !isBrowserSetupDestination(p.Destination) {
			return browserErr(browserErrCommandInvalid, "unknown setup destination")
		}
		// Every other field is ignored: a URL is never read for a setup open.
		*p = browserParams{V: p.V, CommandID: p.CommandID, Op: p.Op, DeadlineMs: p.DeadlineMs, Destination: p.Destination}
		return nil
	case browserOpCurrent:
		// A malformed origin is dropped, not refused: the op still runs.
		p.AppOrigin = normalizeBrowserOrigin(p.AppOrigin)
		return nil
	case browserOpOpen:
		u, err := validateBrowserURL(p.URL)
		if err != nil {
			return browserErr(browserErrCommandInvalid, "%v", err)
		}
		p.URL = u
		return nil
	case browserOpRead:
		return needTab()
	case browserOpScreenshot:
		return needTab()
	case browserOpClick:
		if e := needTab(); e != nil {
			return e
		}
		if !browserRefPattern.MatchString(p.Ref) {
			return browserErr(browserErrCommandInvalid, "click needs a ref from read")
		}
		return nil
	case browserOpNavigate:
		if e := needTab(); e != nil {
			return e
		}
		if (p.URL == "") == (p.Action == "") {
			return browserErr(browserErrCommandInvalid, "navigate needs either a url or an action (back, forward, reload)")
		}
		if p.Action != "" {
			switch p.Action {
			case browserNavBack, browserNavForward, browserNavReload:
				return nil
			}
			return browserErr(browserErrCommandInvalid, "unknown navigate action")
		}
		u, err := validateBrowserURL(p.URL)
		if err != nil {
			return browserErr(browserErrCommandInvalid, "%v", err)
		}
		p.URL = u
		return nil
	case browserOpFill:
		if e := needTab(); e != nil {
			return e
		}
		if len(p.Values) == 0 || len(p.Values) > browserMaxPlanValues {
			return browserErr(browserErrCommandInvalid, "fill needs 1 to %d values", browserMaxPlanValues)
		}
		for i, v := range p.Values {
			field := strings.TrimSpace(v.Field)
			if field == "" || jsLength(v.Field) > browserMaxLabelChars {
				return browserErr(browserErrCommandInvalid, "value %d: field must be 1 to %d characters", i+1, browserMaxLabelChars)
			}
			if jsLength(v.Value) > browserMaxValueChars {
				return browserErr(browserErrCommandInvalid, "value %d: at most %d characters", i+1, browserMaxValueChars)
			}
			p.Values[i].Field = field
		}
		return nil
	case browserOpPrepareSubmit:
		if e := needTab(); e != nil {
			return e
		}
		if p.Ref != "" && !browserRefPattern.MatchString(p.Ref) {
			return browserErr(browserErrCommandInvalid, "malformed ref")
		}
		return nil
	case browserOpSubmit:
		if e := needTab(); e != nil {
			return e
		}
		if !browserRefPattern.MatchString(p.SubmitRef) || !browserDigestPattern.MatchString(p.Digest) || !browserCommandIDPattern.MatchString(p.RequestID) {
			return browserErr(browserErrCommandInvalid, "submit needs submitRef, digest and requestId")
		}
		return nil
	}
	return browserErr(browserErrCommandInvalid, "unknown op")
}

// jsLength counts as JavaScript's String.length does (UTF-16 code units), which
// is how terminal-service and ai-service measure BROWSER_LIMITS.
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// validateBrowserURL accepts only an absolute http(s) URL with a host and no
// credentials in it, at most BROWSER_LIMITS.MAX_URL_CHARS long.
func validateBrowserURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("a url is required")
	}
	if jsLength(s) > browserMaxURLChars || !utf8.ValidString(s) {
		return "", fmt.Errorf("the url is longer than %d characters", browserMaxURLChars)
	}
	if strings.ContainsAny(s, "\x00\r\n\t") {
		return "", fmt.Errorf("the url has control characters")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("the url does not parse")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("only http and https urls can be opened")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("the url has no host")
	}
	if u.User != nil {
		return "", fmt.Errorf("urls with a user name or password are not opened")
	}
	// The URL goes to Chrome as written (Go would percent-encode an
	// internationalised host, which Chrome then has to undo).
	return s, nil
}

// browserMaxAppOriginChars: BROWSER_LIMITS.MAX_APP_ORIGIN_CHARS.
const browserMaxAppOriginChars = 200

// normalizeBrowserOrigin returns a bare http(s) origin ("https://host[:port]",
// an optional trailing "/"), lowercased, or "" for anything else.
func normalizeBrowserOrigin(raw string) string {
	if raw == "" || len(raw) > browserMaxAppOriginChars || strings.ContainsAny(raw, "?# ") || strings.ContainsFunc(raw, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	s := strings.ToLower(u.Scheme)
	if s != "http" && s != "https" {
		return ""
	}
	return s + "://" + strings.ToLower(u.Host)
}

// isBrowserHTTPURL: a tab worth listing (tabs lists http/https tabs only).
func isBrowserHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && u.Host != ""
}

// Tab ids are opaque to the cloud: "<route letter>:<the route's own id>". The
// route letter keeps a tab on the route that found it: a DevTools target id
// means nothing to AppleScript, and the other way round.
const (
	browserTabPrefixDevTools    = "d:"
	browserTabPrefixAppleScript = "a:"
	browserMaxTabIDChars        = 200
)

var browserRawTabIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,190}$`)

func makeBrowserTabID(route, raw string) string {
	if route == browserRouteAppleScript {
		return browserTabPrefixAppleScript + raw
	}
	return browserTabPrefixDevTools + raw
}

func parseBrowserTabID(id string) (route, raw string, ok bool) {
	if len(id) > browserMaxTabIDChars {
		return "", "", false
	}
	switch {
	case strings.HasPrefix(id, browserTabPrefixDevTools):
		route, raw = browserRouteDevTools, id[len(browserTabPrefixDevTools):]
	case strings.HasPrefix(id, browserTabPrefixAppleScript):
		route, raw = browserRouteAppleScript, id[len(browserTabPrefixAppleScript):]
	default:
		return "", "", false
	}
	if !browserRawTabIDPattern.MatchString(raw) {
		return "", "", false
	}
	return route, raw, true
}
