// File: antigravity_turn_failure.go
// -----------------------------------------------------------------------------
// Turns the evidence a failed one-shot Antigravity turn left behind into the
// error the person reads: a category, a reason, a next step and a closing
// sentence (antigravity_native.go SendTurn publishes it).
//
// WHY. On AIE2 (agy 1.5.10, 2026-11-17) every first message failed with
// "Antigravity exited with code 3: {"event":"init",…": the driver published
// the first 400 characters of raw stdout, which is always the `init` event and
// its tool list. The real reason — the account's quota for the default model
// was spent — was only in agy's own log for that run. So:
//
//   - the init event, progress steps and assistant text are NEVER error text;
//   - the reason comes from the evidence sources below, highest first;
//   - the category is decided first, the first table row whose signal appears
//     in any source winning, and its template is filled from the highest
//     source that matched that row.
//
// Evidence sources, highest first (antigravityEvidenceSource):
//  1. the ERROR result event's error text;
//  2. the run's own failure line(s) in agy's log (its PID block);
//  3. an error-status step's failure sentence, or an interaction signal;
//  4. stderr (glog-formatted lines excluded — see below);
//  5. non-JSON stdout text (a plain-text diagnostic from an older/failing CLI).
//
// "You are not logged into Antigravity" is a DECOY: every agy 1.5.10 run logs
// it as a W/E line before silently signing in from the keyring, and every run
// logs a 403 PERMISSION_DENIED "insufficient authentication scopes" for a
// routing probe. Neither is ever a failure line, so signed_out / entitlement
// are only chosen from a failure source, never from an arbitrary log line —
// and glog-formatted lines on stderr are that same log, so they are not
// stderr evidence either.
//
// The category ids are a contract: shared-constants
// ANTIGRAVITY_TURN_FAILURE_CATEGORIES holds them, and
// antigravity_turn_failure_contract_test.go pins this copy to it.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Failure category ids (shared-constants ANTIGRAVITY_TURN_FAILURE_CATEGORIES).
const (
	antigravityFailureSignedOut           = "signed_out"
	antigravityFailureEntitlement         = "entitlement"
	antigravityFailureQuota               = "quota"
	antigravityFailureInvalidLaunchOption = "invalid_launch_option"
	antigravityFailureCwdProject          = "cwd_project"
	antigravityFailureInteraction         = "interaction_required"
	antigravityFailureStalled             = "stalled"
	antigravityFailureCLIDefect           = "cli_defect"
	antigravityFailureUnknown             = "unknown"
)

// Closing sentences, by the table's "fixable in this session" column.
const (
	antigravityClosingSameSession = "The session is still open — send another message to try again."
	antigravityClosingNewSession  = "This session stays open, but this fix needs a new session."
)

// antigravityShuttingDownText is the whole published text when a turn's
// process could not start because the previous turn's processes were still
// exiting. It skips the category table: no failureCategory, no closing.
const antigravityShuttingDownText = "Antigravity's previous run on this computer is still shutting down. Send the message again in a moment, or restart Antigravity on that computer."

// antigravityFailureReasonMax bounds the reason (the filled template). The
// next-step and closing sentences are never cut.
const antigravityFailureReasonMax = 400

// antigravityQuotaWrapperPrefix is the quota reason's wrapper, which the
// cloud's matchers anchor on (shared-constants cliRateLimit / providerOutage,
// shared-resources classifyCliTurnFailure). It must stay at the START of the
// published output, byte for byte.
const antigravityQuotaWrapperPrefix = "[Antigravity turn failed: "

type antigravityFailureRow struct {
	nextStep string
	sameFix  bool // the table's "fixable in this session" column
}

var antigravityFailureRows = map[string]antigravityFailureRow{
	antigravityFailureSignedOut:           {"Sign in to Antigravity on that computer, then send the message again.", true},
	antigravityFailureEntitlement:         {"Fix the Antigravity account's plan and send the message again, or start a new session with another model.", true},
	antigravityFailureQuota:               {"Wait until the quota resets and send the message again, or start a new session with another model.", true},
	antigravityFailureInvalidLaunchOption: {"Start a new session with another model or effort.", false},
	antigravityFailureCwdProject:          {"Start a new session in a project folder Antigravity can open.", false},
	antigravityFailureInteraction:         {"Rephrase the task so it does not need that, then send it again.", true},
	antigravityFailureStalled:             {"Send the message again; if it repeats, restart Antigravity on that computer.", true},
	antigravityFailureCLIDefect:           {"Update Antigravity on that computer, then send the message again.", true},
	antigravityFailureUnknown:             {"Send the message again; if it repeats, check Antigravity on that computer.", true},
}

// antigravityFailureCategoryIDs lists every category id, in table order.
var antigravityFailureCategoryIDs = []string{
	antigravityFailureSignedOut,
	antigravityFailureEntitlement,
	antigravityFailureQuota,
	antigravityFailureInvalidLaunchOption,
	antigravityFailureCwdProject,
	antigravityFailureInteraction,
	antigravityFailureStalled,
	antigravityFailureCLIDefect,
	antigravityFailureUnknown,
}

// antigravityEvidenceSource names where a reason came from, highest first.
type antigravityEvidenceSource int

const (
	antigravitySourceResult antigravityEvidenceSource = iota + 1
	antigravitySourceLog
	antigravitySourceStep
	antigravitySourceStderr
	antigravitySourceStdoutText
)

func (s antigravityEvidenceSource) String() string {
	switch s {
	case antigravitySourceResult:
		return "result"
	case antigravitySourceLog:
		return "log"
	case antigravitySourceStep:
		return "step"
	case antigravitySourceStderr:
		return "stderr"
	case antigravitySourceStdoutText:
		return "stdout_text"
	}
	return "none"
}

type antigravityEvidence struct {
	source antigravityEvidenceSource
	text   string
	// interaction is set on a step-source item that is an interaction signal:
	// the tool id (ask_question / ask_permission / ask_custom_permission) or
	// the request's title when the event gives one.
	interaction string
}

// antigravityTurnEvidence is everything a failed turn left that may name its
// reason, highest source first. It never holds the init event, a progress
// step or assistant text.
type antigravityTurnEvidence struct {
	items []antigravityEvidence
}

func (ev *antigravityTurnEvidence) add(source antigravityEvidenceSource, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for _, it := range ev.items {
		if it.source == source && it.text == text {
			return
		}
	}
	ev.items = append(ev.items, antigravityEvidence{source: source, text: text})
}

func (ev *antigravityTurnEvidence) addInteraction(label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		return
	}
	ev.items = append(ev.items, antigravityEvidence{source: antigravitySourceStep, text: label, interaction: label})
}

// sorted returns the items highest source first, stable within a source.
func (ev *antigravityTurnEvidence) sorted() []antigravityEvidence {
	out := make([]antigravityEvidence, 0, len(ev.items))
	for s := antigravitySourceResult; s <= antigravitySourceStdoutText; s++ {
		for _, it := range ev.items {
			if it.source == s {
				out = append(out, it)
			}
		}
	}
	return out
}

// collectAntigravityTurnEvidence gathers a failed turn's evidence from its
// stdout lines, stderr and the run's own log failure reasons.
func collectAntigravityTurnEvidence(stdoutLines []string, stderr string, logReasons []string) antigravityTurnEvidence {
	var ev antigravityTurnEvidence
	interactions := antigravityPendingInteractions(stdoutLines)
	for _, line := range stdoutLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			ev.add(antigravitySourceStdoutText, line)
			continue
		}
		var event antigravityStreamEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			// A cut or malformed JSON line is not a diagnostic sentence, and
			// it may be the init tool list cut short: never evidence.
			continue
		}
		switch event.Event {
		case "result":
			if event.Result != nil && strings.EqualFold(event.Result.Status, "ERROR") {
				ev.add(antigravitySourceResult, event.Result.Error)
			}
		case "step_update":
			if event.StepUpdate != nil {
				if msg := event.StepUpdate.failureSentence(); msg != "" {
					ev.add(antigravitySourceStep, msg)
				}
			}
		}
	}
	for _, label := range interactions {
		ev.addInteraction(label)
	}
	for _, r := range logReasons {
		ev.add(antigravitySourceLog, r)
	}
	var stderrKept []string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || antigravityGlogRecord.MatchString(line) {
			continue
		}
		stderrKept = append(stderrKept, line)
	}
	ev.add(antigravitySourceStderr, strings.Join(stderrKept, "\n"))
	return ev
}

// antigravityStreamEvent is the subset of an agy stream-json event the failure
// path reads. Assistant text (`text_delta`, `response`) is deliberately absent.
type antigravityStreamEvent struct {
	Event  string `json:"event"`
	Result *struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"result"`
	StepUpdate *antigravityStepUpdate `json:"step_update"`
}

type antigravityStepUpdate struct {
	StepIndex *int   `json:"step_index"`
	State     string `json:"state"`
	StepType  string `json:"step_type"`
	ToolName  string `json:"tool_name"`
	// Title fields a permission or question step may carry. Unverified on
	// agy 1.5.10 — see antigravityInteractionStopMode.
	Title    string `json:"title"`
	Question string `json:"question"`
	// Failure sentence fields an error step may carry.
	Error        string `json:"error"`
	ErrorMessage string `json:"error_message"`
}

// failureSentence is the failure sentence of a step whose status is error —
// never a progress step's content.
func (s *antigravityStepUpdate) failureSentence() string {
	if !strings.EqualFold(s.State, "ERROR") && s.StepType != "error_message" {
		return ""
	}
	if msg := strings.TrimSpace(s.Error); msg != "" {
		return msg
	}
	return strings.TrimSpace(s.ErrorMessage)
}

// antigravityInteractionTools are the interactive tools stream mode cannot
// answer.
var antigravityInteractionTools = map[string]bool{
	"ask_question":          true,
	"ask_permission":        true,
	"ask_custom_permission": true,
}

func isAntigravityPermissionTool(name string) bool {
	return name == "ask_permission" || name == "ask_custom_permission"
}

// interactionLabel is the tool id, or the request's title when it gives one.
func (s *antigravityStepUpdate) interactionLabel() string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	if q := strings.TrimSpace(s.Question); q != "" {
		return q
	}
	return s.ToolName
}

// antigravityPendingInteractions lists the interaction signals a stream left
// unresolved: an ask_question step, or a permission step with neither a DONE
// for its step index nor any later step (where the auto-approve would show).
func antigravityPendingInteractions(lines []string) []string {
	type pending struct {
		label string
		index int
	}
	var open []pending
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, "step_update") {
			continue
		}
		var event antigravityStreamEvent
		if json.Unmarshal([]byte(line), &event) != nil || event.Event != "step_update" || event.StepUpdate == nil {
			continue
		}
		s := event.StepUpdate
		idx := -1
		if s.StepIndex != nil {
			idx = *s.StepIndex
		}
		// Any later step resolves every earlier permission request.
		kept := open[:0]
		for _, p := range open {
			if p.index == idx && idx >= 0 && !strings.EqualFold(s.State, "DONE") {
				kept = append(kept, p) // the same step, still waiting
			}
		}
		open = kept
		if antigravityInteractionTools[s.ToolName] && !strings.EqualFold(s.State, "DONE") {
			already := false
			for _, p := range open {
				if p.index == idx && idx >= 0 {
					already = true
				}
			}
			if !already {
				open = append(open, pending{label: s.interactionLabel(), index: idx})
			}
		}
	}
	out := make([]string, 0, len(open))
	for _, p := range open {
		out = append(out, p.label)
	}
	return out
}

/* --------------------------------------------------------------------------
   Category signals
   -------------------------------------------------------------------------- */

var (
	antigravitySignedOutSignal = regexp.MustCompile(`(?i)\bnot logged in|\bunauthenticated\b|\btoken\b[^.\n]{0,60}\b(missing|expired)\b`)
	// A bare PERMISSION_DENIED is never entitlement: every 1.5.10 run logs a
	// 403 "insufficient authentication scopes" PERMISSION_DENIED for a
	// routing probe, and a tool or sandbox denial is not a plan problem. Nor
	// is quota prose ("Please upgrade your subscription to increase your
	// limits"), which the quota row owns.
	antigravityEntitlementSignal  = regexp.MustCompile(`(?i)subscription_required|\bnot entitled\b|\bentitlement\b|(?:not|isn't) (?:available|included) (?:on|in|with|for) (?:your|this|the current) (?:plan|tier|subscription)|\brequires? (?:a |an )?(?:paid |higher |pro |ultra |different )?(?:plan|tier)\b`)
	antigravityLaunchOptionSignal = regexp.MustCompile(`(?i)\b(?:unknown|unsupported|invalid|unrecognized|unrecognised|not supported|not available)\b[^.\n]{0,40}\b(model|effort)\b|\b(model|effort)\b[^.\n]{0,40}\b(?:is unknown|is unsupported|is invalid|not found|not supported|is not available|unrecognized|unrecognised)\b|flag provided but not defined: -{1,2}(model|effort)\b`)
	antigravityCwdProjectSignal   = regexp.MustCompile(`(?i)\bworking directory\b|\bworkspace trust\b|\b(?:not|un)trusted (?:workspace|folder|directory|project)\b|\bworkspace is not trusted\b|\bproject (?:root|folder|directory)\b|\bnot a (?:git )?repository\b|\bchdir\b`)
)

// antigravityLaunchOptionName names the setting a launch-option reason
// rejects: "effort" when it names effort, "model" otherwise.
func antigravityLaunchOptionName(reason string) string {
	m := antigravityLaunchOptionSignal.FindStringSubmatch(reason)
	for _, g := range m[1:] {
		if strings.EqualFold(g, "effort") {
			return "effort"
		}
	}
	return "model"
}

/* --------------------------------------------------------------------------
   Classification
   -------------------------------------------------------------------------- */

// antigravityTurnFailure is a classified failed turn.
type antigravityTurnFailure struct {
	category string
	reason   string // the filled reason template, unbounded and unredacted
	source   antigravityEvidenceSource
}

// antigravityFailureContext is what the templates fill from besides evidence.
type antigravityFailureContext struct {
	computer string
	cwd      string
	model    string // the START's --model value, "" for agy's default
	exitCode int
}

// classifyAntigravityTurnFailure picks the category and reason. stopCause is
// the category of a turn the driver itself stopped (interaction_required /
// stalled), which is kept as is: the exit its stop caused is not classified
// again. stopDetail is the interaction's tool or title.
func classifyAntigravityTurnFailure(ev antigravityTurnEvidence, stopCause, stopDetail string, fc antigravityFailureContext) antigravityTurnFailure {
	switch stopCause {
	case antigravityFailureStalled:
		return antigravityTurnFailure{category: stopCause, reason: "Antigravity produced nothing for 3 minutes after starting."}
	case antigravityFailureInteraction:
		return antigravityTurnFailure{category: stopCause, reason: antigravityInteractionReason(stopDetail), source: antigravitySourceStep}
	}

	items := ev.sorted()
	first := func(match func(antigravityEvidence) bool) (antigravityEvidence, bool) {
		for _, it := range items {
			if match(it) {
				return it, true
			}
		}
		return antigravityEvidence{}, false
	}
	failureSource := func(it antigravityEvidence) bool {
		// signed_out / entitlement: a failure source only (result, the run's
		// failure line, stderr), never a progress step or stray stdout.
		return it.source == antigravitySourceResult || it.source == antigravitySourceLog || it.source == antigravitySourceStderr
	}

	if it, ok := first(func(it antigravityEvidence) bool {
		return failureSource(it) && antigravitySignedOutSignal.MatchString(it.text)
	}); ok {
		computer := fc.computer
		if computer == "" {
			computer = "this computer"
		}
		return antigravityTurnFailure{category: antigravityFailureSignedOut, reason: fmt.Sprintf("Antigravity is not signed in on %s.", computer), source: it.source}
	}
	if it, ok := first(func(it antigravityEvidence) bool {
		return failureSource(it) && antigravityEntitlementSignal.MatchString(it.text)
	}); ok {
		what := fc.model
		if what == "" {
			what = "Antigravity"
		}
		return antigravityTurnFailure{category: antigravityFailureEntitlement, reason: fmt.Sprintf("This Antigravity account cannot use %s: %s", what, it.text), source: it.source}
	}
	if it, ok := first(func(it antigravityEvidence) bool {
		return it.interaction == "" && isAntigravityQuotaReason(it.text)
	}); ok {
		return antigravityTurnFailure{category: antigravityFailureQuota, reason: antigravityQuotaWrapperPrefix + it.text + "]", source: it.source}
	}
	if it, ok := first(func(it antigravityEvidence) bool {
		return it.interaction == "" && antigravityLaunchOptionSignal.MatchString(it.text)
	}); ok {
		return antigravityTurnFailure{category: antigravityFailureInvalidLaunchOption, reason: fmt.Sprintf("Antigravity rejected the %s setting: %s", antigravityLaunchOptionName(it.text), it.text), source: it.source}
	}
	if it, ok := first(func(it antigravityEvidence) bool {
		return it.interaction == "" && antigravityCwdProjectSignal.MatchString(it.text)
	}); ok {
		return antigravityTurnFailure{category: antigravityFailureCwdProject, reason: fmt.Sprintf("Antigravity could not use the folder %s: %s", fc.cwd, it.text), source: it.source}
	}
	if it, ok := first(func(it antigravityEvidence) bool { return it.interaction != "" }); ok {
		return antigravityTurnFailure{category: antigravityFailureInteraction, reason: antigravityInteractionReason(it.interaction), source: it.source}
	}
	if len(items) > 0 {
		it := items[0]
		return antigravityTurnFailure{category: antigravityFailureCLIDefect, reason: "Antigravity failed: " + it.text, source: it.source}
	}
	return antigravityTurnFailure{
		category: antigravityFailureUnknown,
		reason:   fmt.Sprintf("Antigravity stopped with exit code %d before reporting a reason, so this message did not finish.", fc.exitCode),
	}
}

func antigravityInteractionReason(label string) string {
	if strings.TrimSpace(label) == "" {
		label = "a question"
	}
	return fmt.Sprintf("Antigravity asked %q, which can't be answered in this unattended session.", label)
}

// composeAntigravityFailureText is the published output: the reason, redacted
// and bounded, then the next-step and closing sentences, whole.
//
// The reason is bounded INSIDE its template's placeholder, so the template's
// fixed text survives a long CLI reason — above all the quota wrapper, which
// keeps its closing "]" and stays the first bytes of the output.
func composeAntigravityFailureText(f antigravityTurnFailure) string {
	row, ok := antigravityFailureRows[f.category]
	if !ok {
		row = antigravityFailureRows[antigravityFailureCLIDefect]
	}
	closing := antigravityClosingSameSession
	if !row.sameFix {
		closing = antigravityClosingNewSession
	}
	return boundAntigravityFailureReason(redactAgentSecrets(f.reason), f.category) + " " + row.nextStep + " " + closing
}

// boundAntigravityFailureReason cuts a reason to antigravityFailureReasonMax
// characters. A quota wrapper is cut inside its brackets.
func boundAntigravityFailureReason(reason, category string) string {
	if utf8.RuneCountInString(reason) <= antigravityFailureReasonMax {
		return reason
	}
	if category == antigravityFailureQuota && strings.HasPrefix(reason, antigravityQuotaWrapperPrefix) && strings.HasSuffix(reason, "]") {
		inner := strings.TrimSuffix(strings.TrimPrefix(reason, antigravityQuotaWrapperPrefix), "]")
		room := antigravityFailureReasonMax - utf8.RuneCountInString(antigravityQuotaWrapperPrefix) - 1 // "]"
		return antigravityQuotaWrapperPrefix + clipString(inner, room) + "]"
	}
	return clipString(reason, antigravityFailureReasonMax)
}

// antigravityLaunchModel is the START's --model value, "" when agy's default
// model runs.
func antigravityLaunchModel(launchArgs []string) string {
	for i := 0; i+1 < len(launchArgs); i++ {
		if launchArgs[i] == "--model" {
			return launchArgs[i+1]
		}
	}
	return ""
}
