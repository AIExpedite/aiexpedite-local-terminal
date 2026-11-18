package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// The AIE2 block (agy 1.5.10, 2026-11-17, conversation 515395b5…), redacted
// to its shape: the decoy "not logged in" warnings, the silent keyring
// sign-in, the 403 scope probe, then the quota failure.
const aie2LogBlock = `I1117 12:55:44.101000   7712 main.go:88] Starting language server process with pid 7712
I1117 12:55:44.201000   7712 print.go:51] Print mode: --dangerously-skip-permissions set, auto-approving all tool permissions
W1117 12:55:44.301000   7712 auth.go:140] You are not logged into Antigravity.
E1117 12:55:44.302000   7712 auth.go:141] You are not logged into Antigravity.
I1117 12:55:44.901000   7712 auth.go:201] silent auth succeeded
W1117 12:55:45.001000   7712 routing.go:77] AICode routing probe failed: PERMISSION_DENIED (code 403): Request had insufficient authentication scopes.
I1117 12:55:45.401000   7712 conversation.go:33] Created conversation 515395b5-22ff-4ee1-b279-f4c81f9cd90d
E1117 12:55:47.801000   7712 executor.go:212] agent executor error: generating and executing: RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 80h46m28s.
E1117 12:55:47.802000   7712 print.go:97] Print mode: run ended with error and no response: RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 80h46m28s.
`

const aie2QuotaReason = `RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 80h46m28s.`

func aie2InitLine() string {
	tools := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		tools = append(tools, fmt.Sprintf(`"browser_tool_number_%02d"`, i))
	}
	return `{"event":"init","conversation_id":"515395b5-22ff-4ee1-b279-f4c81f9cd90d","init":{"cwd":"C:\\aiexpedite","tools":["ask_custom_permission","ask_permission","ask_question",` + strings.Join(tools, ",") + `]}}`
}

func classifyFor(stdout []string, stderr string, logReasons []string, exit int) antigravityTurnFailure {
	ev := collectAntigravityTurnEvidence(stdout, stderr, logReasons)
	return classifyAntigravityTurnFailure(ev, "", "", antigravityFailureContext{computer: "AIE2", cwd: `C:\aiexpedite`, exitCode: exit})
}

func TestAntigravityRunFailureReasons_AIE2Block(t *testing.T) {
	got := antigravityRunFailureReasons([]byte(aie2LogBlock))
	if len(got) != 1 || got[0] != aie2QuotaReason {
		t.Fatalf("reasons = %q, want the one quota reason (both 1.5.10 shapes fold to it)", got)
	}
}

func TestClassifyAntigravityTurnFailure_AIE2IsQuotaNotSignedOutOrEntitlement(t *testing.T) {
	logReasons := antigravityRunFailureReasons([]byte(aie2LogBlock))
	f := classifyFor([]string{aie2InitLine()}, "", logReasons, 3)
	if f.category != antigravityFailureQuota {
		t.Fatalf("category = %q, want quota", f.category)
	}
	text := composeAntigravityFailureText(f)
	want := "[Antigravity turn failed: " + aie2QuotaReason + "] " +
		"Wait until the quota resets and send the message again, or start a new session with another model. " +
		"The session is still open — send another message to try again."
	if text != want {
		t.Fatalf("text = %q\nwant   %q", text, want)
	}
	if strings.Count(text, antigravityQuotaWrapperPrefix) != 1 || !strings.HasPrefix(text, antigravityQuotaWrapperPrefix) {
		t.Fatalf("the quota wrapper must lead the output exactly once: %q", text)
	}
	if strings.Contains(text, `"event"`) || strings.Contains(text, "tools") {
		t.Fatalf("init output leaked into the error: %q", text)
	}
}

func TestClassifyAntigravityTurnFailure_InitThenExit3IsUnknown(t *testing.T) {
	f := classifyFor([]string{aie2InitLine()}, "", nil, 3)
	if f.category != antigravityFailureUnknown {
		t.Fatalf("category = %q, want unknown", f.category)
	}
	want := "Antigravity stopped with exit code 3 before reporting a reason, so this message did not finish. " +
		"Send the message again; if it repeats, check Antigravity on that computer. " +
		"The session is still open — send another message to try again."
	if got := composeAntigravityFailureText(f); got != want {
		t.Fatalf("text = %q\nwant   %q", got, want)
	}
}

func TestClassifyAntigravityTurnFailure_OneCasePerCategory(t *testing.T) {
	errResult := func(msg string) string {
		return fmt.Sprintf(`{"event":"result","result":{"status":"ERROR","error":%q}}`, msg)
	}
	cases := []struct {
		name     string
		stdout   []string
		stderr   string
		log      []string
		category string
		reason   string
	}{
		{"signed_out from stderr beats a generic ERROR result", []string{aie2InitLine(), errResult("turn failed")},
			"Error: not logged in. Run agy login.", nil, antigravityFailureSignedOut, "Antigravity is not signed in on AIE2."},
		{"signed_out from the run's log failure line", nil, "",
			[]string{"UNAUTHENTICATED: sign-in token expired"}, antigravityFailureSignedOut, "Antigravity is not signed in on AIE2."},
		{"entitlement", []string{errResult("SUBSCRIPTION_REQUIRED: model not available on your plan")}, "", nil,
			antigravityFailureEntitlement, "This Antigravity account cannot use Antigravity: SUBSCRIPTION_REQUIRED: model not available on your plan"},
		{"quota from the result", []string{errResult("RESOURCE_EXHAUSTED (code 429): quota")}, "", nil,
			antigravityFailureQuota, "[Antigravity turn failed: RESOURCE_EXHAUSTED (code 429): quota]"},
		{"invalid model", nil, "error: unknown model \"gemini-9\"", nil,
			antigravityFailureInvalidLaunchOption, "Antigravity rejected the model setting: error: unknown model \"gemini-9\""},
		{"invalid effort", nil, "flag provided but not defined: -effort", nil,
			antigravityFailureInvalidLaunchOption, "Antigravity rejected the effort setting: flag provided but not defined: -effort"},
		{"cwd / workspace trust", []string{errResult("workspace is not trusted")}, "", nil,
			antigravityFailureCwdProject, `Antigravity could not use the folder C:\aiexpedite: workspace is not trusted`},
		{"interaction left unresolved in the stream", []string{
			`{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"ask_question"}}`,
		}, "", nil, antigravityFailureInteraction, `Antigravity asked "ask_question", which can't be answered in this unattended session.`},
		{"cli_defect from an unrecognised log line", nil, "", []string{"internal: nil pointer in planner"},
			antigravityFailureCLIDefect, "Antigravity failed: internal: nil pointer in planner"},
		{"cli_defect from stderr", nil, "panic: something odd", nil,
			antigravityFailureCLIDefect, "Antigravity failed: panic: something odd"},
		{"cli_defect from plain stdout", []string{"Error: cannot open database"}, "", nil,
			antigravityFailureCLIDefect, "Antigravity failed: Error: cannot open database"},
		{"unknown", []string{aie2InitLine()}, "", nil, antigravityFailureUnknown,
			"Antigravity stopped with exit code 7 before reporting a reason, so this message did not finish."},
		{"a recognised log line gets its row's template", nil, "", []string{"not logged in"},
			antigravityFailureSignedOut, "Antigravity is not signed in on AIE2."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := classifyFor(tc.stdout, tc.stderr, tc.log, 7)
			if f.category != tc.category || f.reason != tc.reason {
				t.Fatalf("got (%q, %q)\nwant (%q, %q)", f.category, f.reason, tc.category, tc.reason)
			}
		})
	}
	// stalled is the driver's own stop.
	f := classifyAntigravityTurnFailure(antigravityTurnEvidence{}, antigravityFailureStalled, "", antigravityFailureContext{exitCode: -1})
	if f.category != antigravityFailureStalled || f.reason != "Antigravity produced nothing for 3 minutes after starting." {
		t.Fatalf("stalled = %+v", f)
	}
}

func TestClassifyAntigravityTurnFailure_ToolPermissionDeniedIsNotEntitlement(t *testing.T) {
	for _, stderr := range []string{
		"PERMISSION_DENIED (code 403): Request had insufficient authentication scopes.",
		"tool run_command: PERMISSION_DENIED: sandbox refused write to /etc/hosts",
	} {
		f := classifyFor(nil, stderr, nil, 1)
		if f.category == antigravityFailureEntitlement {
			t.Fatalf("%q classified as entitlement", stderr)
		}
	}
}

func TestClassifyAntigravityTurnFailure_DecoyLogLinesOnStderrAreIgnored(t *testing.T) {
	stderr := "W1117 12:55:44.301000   7712 auth.go:140] You are not logged into Antigravity.\n" +
		"E1117 12:55:44.302000   7712 auth.go:141] You are not logged into Antigravity."
	f := classifyFor([]string{aie2InitLine()}, stderr, nil, 3)
	if f.category != antigravityFailureUnknown {
		t.Fatalf("glog decoys on stderr must not be evidence, got %+v", f)
	}
}

func TestClassifyAntigravityTurnFailure_StopCauseIsKept(t *testing.T) {
	ev := collectAntigravityTurnEvidence([]string{aie2InitLine()}, "killed", nil)
	f := classifyAntigravityTurnFailure(ev, antigravityFailureInteraction, "Allow edits outside the folder?", antigravityFailureContext{exitCode: -1})
	if f.category != antigravityFailureInteraction ||
		f.reason != `Antigravity asked "Allow edits outside the folder?", which can't be answered in this unattended session.` {
		t.Fatalf("stop cause must win over the exit's evidence: %+v", f)
	}
}

func TestClassifyAntigravityTurnFailure_ResolvedPermissionIsNotInteraction(t *testing.T) {
	stdout := []string{
		`{"event":"step_update","step_update":{"step_index":3,"state":"ACTIVE","step_type":"tool","tool_name":"ask_permission"}}`,
		`{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"tool","tool_name":"ask_permission"}}`,
		`{"event":"step_update","step_update":{"step_index":4,"state":"ACTIVE","step_type":"tool","tool_name":"edit_file"}}`,
	}
	f := classifyFor(stdout, "", nil, 1)
	if f.category != antigravityFailureUnknown {
		t.Fatalf("an auto-approved permission is not a signal: %+v", f)
	}
}

func TestComposeAntigravityFailureText_BoundsOnlyTheReason(t *testing.T) {
	long := strings.Repeat("x", 1000)
	f := classifyFor([]string{fmt.Sprintf(`{"event":"result","result":{"status":"ERROR","error":%q}}`, long)}, "", nil, 1)
	text := composeAntigravityFailureText(f)
	next := "Update Antigravity on that computer, then send the message again."
	if !strings.HasSuffix(text, " "+next+" "+antigravityClosingSameSession) {
		t.Fatalf("next step and closing must stay whole: %q", text[len(text)-200:])
	}
	reason := strings.TrimSuffix(text, " "+next+" "+antigravityClosingSameSession)
	if n := utf8.RuneCountInString(reason); n > antigravityFailureReasonMax {
		t.Fatalf("reason is %d chars, want ≤ %d", n, antigravityFailureReasonMax)
	}
	if strings.Contains(text, "exit code") {
		t.Fatalf("only unknown carries an exit code: %q", text)
	}

	q := classifyFor(nil, "", []string{"RESOURCE_EXHAUSTED (code 429): " + long}, 3)
	qt := composeAntigravityFailureText(q)
	qr := strings.SplitN(qt, "] ", 2)[0] + "]"
	if !strings.HasPrefix(qt, antigravityQuotaWrapperPrefix) || utf8.RuneCountInString(qr) > antigravityFailureReasonMax {
		t.Fatalf("a long quota reason must be cut inside its wrapper: %q", qt[:80])
	}
}

func TestComposeAntigravityFailureText_NoCategoriesEndWithNewSessionSentence(t *testing.T) {
	for cat, row := range antigravityFailureRows {
		text := composeAntigravityFailureText(antigravityTurnFailure{category: cat, reason: "r."})
		wantClosing := antigravityClosingSameSession
		if !row.sameFix {
			wantClosing = antigravityClosingNewSession
		}
		if !strings.HasSuffix(text, wantClosing) {
			t.Fatalf("%s: %q", cat, text)
		}
	}
	for _, cat := range []string{antigravityFailureInvalidLaunchOption, antigravityFailureCwdProject} {
		if antigravityFailureRows[cat].sameFix {
			t.Fatalf("%s is fixable only in a new session", cat)
		}
	}
}

func TestClassifyAntigravityTurnFailure_AssistantTextNeverAppears(t *testing.T) {
	stdout := []string{
		aie2InitLine(),
		`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"SECRET ASSISTANT WORDS"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"SECRET ASSISTANT WORDS"}}`,
	}
	f := classifyFor(stdout, "", nil, 2)
	if text := composeAntigravityFailureText(f); strings.Contains(text, "SECRET") || f.category != antigravityFailureUnknown {
		t.Fatalf("assistant text leaked: %+v %q", f, text)
	}
}

func TestAntigravityDiagnosticEvents_ExcludeInitPromptAndAssistantText(t *testing.T) {
	events := antigravityDiagnosticEvents([]string{
		aie2InitLine(),
		`{"event":"user","message":{"content":"MY PROMPT"}}`,
		`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"ASSISTANT"}}`,
		`{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","args":{"cmd":"export TOKEN=abc"}}}`,
		`{"event":"result","result":{"status":"ERROR","error":"boom"}}`,
	})
	raw := fmt.Sprintf("%+v", events)
	for _, banned := range []string{"browser_tool", "MY PROMPT", "ASSISTANT", "TOKEN"} {
		if strings.Contains(raw, banned) {
			t.Fatalf("diagnostics kept %q: %s", banned, raw)
		}
	}
	if len(events) != 3 {
		t.Fatalf("events = %s", raw)
	}
}

func TestRecordAntigravityTurnFailure_PrunesToNewest(t *testing.T) {
	dir := t.TempDir()
	prev := antigravityTurnDiagnosticsDir
	antigravityTurnDiagnosticsDir = func() string { return dir }
	t.Cleanup(func() { antigravityTurnDiagnosticsDir = prev })
	for i := 0; i < antigravityDiagnosticsKeep+5; i++ {
		recordAntigravityTurnFailure(antigravityTurnDiagnostics{SessionID: fmt.Sprintf("s%02d", i), ExitCode: 3, Category: antigravityFailureUnknown, Source: "none"})
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != antigravityDiagnosticsKeep {
		t.Fatalf("kept %d files, want %d", len(entries), antigravityDiagnosticsKeep)
	}
}
