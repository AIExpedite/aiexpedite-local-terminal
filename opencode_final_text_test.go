package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// An OpenCode turn's completion carries `finalText`: the assistant's final
// answer, the text after the run's last tool call. `text` stays every text
// part of every step joined — what the web view and the transcript read — but
// a multi-step turn's `text` opens with the narration before each tool call,
// and voice speaks only its first 1200 characters, so the spoken reply began
// "I'll check the file.Let me run the tests…".
//
// The frames are `opencode run --format json`'s own event shapes: step_start /
// text / tool_use / step_finish, each wrapping a `part` (live on 1.x: the
// `text` event carries a whole text part, `tool_use` a finished tool part,
// `step_finish` the step's finish reason, `tool-calls` when the step called
// tools).

const (
	ocStepStart       = `{"type":"step_start","sessionID":"ses_1","part":{"type":"step-start"}}`
	ocStepFinishTools = `{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","tokens":{"input":10,"output":5}}}`
	ocStepFinishStop  = `{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","tokens":{"input":10,"output":5}}}`
	ocToolRead        = `{"type":"tool_use","sessionID":"ses_1","part":{"type":"tool","tool":"read","callID":"c1","state":{"status":"completed","input":{"filePath":"main.go"},"output":"package main"}}}`
	ocToolBash        = `{"type":"tool_use","sessionID":"ses_1","part":{"type":"tool","tool":"bash","callID":"c2","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok"}}}`
)

func ocText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "text", "sessionID": "ses_1",
		"part": map[string]string{"type": "text", "text": text, "messageID": "msg_1", "sessionID": "ses_1"},
	})
	return string(b)
}

// streamOpenCodeFrames runs frames through the manager's stdout scan.
func streamOpenCodeFrames(frames ...string) openCodeStreamState {
	m := NewOpenCodeNativeManager()
	return m.streamOpenCodeEvents(&OpenCodeNativeSession{ID: "s"},
		strings.NewReader(strings.Join(frames, "\n")+"\n"), nil, nil)
}

func TestOpenCodeFinalAnswer_IsTheTextAfterTheLastToolCall(t *testing.T) {
	cases := []struct {
		name      string
		frames    []string
		wantText  string
		wantFinal string
	}{
		{
			name:     "the live single-step turn (no tool call): all of it",
			frames:   []string{ocStepStart, ocText("hello"), ocStepFinishStop},
			wantText: "hello", wantFinal: "hello",
		},
		{
			name: "a multi-step turn: the narration before each tool call is not the answer",
			frames: []string{
				ocStepStart, ocText("I'll check the file."), ocToolRead, ocStepFinishTools,
				ocStepStart, ocText("Let me run the tests..."), ocToolBash, ocStepFinishTools,
				ocStepStart, ocText("All 12 tests pass; the fix is in main.go."), ocStepFinishStop,
			},
			wantText:  "I'll check the file.Let me run the tests...All 12 tests pass; the fix is in main.go.",
			wantFinal: "All 12 tests pass; the fix is in main.go.",
		},
		{
			name: "every text part after the last tool call is the answer",
			frames: []string{
				ocStepStart, ocText("Reading it."), ocToolRead, ocStepFinishTools,
				ocStepStart, ocText("Done."), ocText(" The bug was an off-by-one."), ocStepFinishStop,
			},
			wantText:  "Reading it.Done. The bug was an off-by-one.",
			wantFinal: "Done. The bug was an off-by-one.",
		},
		{
			name: "a step that ended in tool calls with no tool_use frame (an errored tool) is a boundary too",
			frames: []string{
				ocStepStart, ocText("Trying the build."), ocStepFinishTools,
				ocStepStart, ocText("The build failed: a dependency is missing."), ocStepFinishStop,
			},
			wantText:  "Trying the build.The build failed: a dependency is missing.",
			wantFinal: "The build failed: a dependency is missing.",
		},
		{
			name: "a run that ended on a tool call answers with its last stretch of text, never nothing",
			frames: []string{
				ocStepStart, ocText("Reading it."), ocToolRead, ocStepFinishTools,
				ocStepStart, ocText("Writing the fix now."), ocToolBash, ocStepFinishTools,
			},
			wantText:  "Reading it.Writing the fix now.",
			wantFinal: "Writing the fix now.",
		},
		{
			name: "whitespace after the last tool call is no answer",
			frames: []string{
				ocStepStart, ocText("Here is the summary."), ocToolRead, ocStepFinishTools,
				ocStepStart, ocText("\n"), ocStepFinishStop,
			},
			wantText:  "Here is the summary.",
			wantFinal: "Here is the summary.",
		},
		{
			name: "older spellings: a tool-call event and a tool part",
			frames: []string{
				`{"type":"text-delta","delta":"Looking."}`,
				`{"type":"tool-call","name":"read","text":"tool payload"}`,
				`{"type":"text-delta","delta":"Checking again."}`,
				`{"type":"message.part.updated","part":{"type":"tool","tool":"grep"}}`,
				`{"type":"text-delta","delta":"Found it."}`,
				`{"type":"session.completed"}`,
			},
			wantText:  "Looking.Checking again.Found it.",
			wantFinal: "Found it.",
		},
		{
			name:     "no text at all",
			frames:   []string{ocStepStart, ocToolRead, ocStepFinishTools},
			wantText: "", wantFinal: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := streamOpenCodeFrames(tc.frames...)
			if got := strings.TrimSpace(state.text.String()); got != tc.wantText {
				t.Fatalf("text = %q, want %q (it must stay every text part joined)", got, tc.wantText)
			}
			if got := strings.TrimSpace(state.answer.text()); got != tc.wantFinal {
				t.Fatalf("final answer = %q, want %q", got, tc.wantFinal)
			}
		})
	}
}

func TestIsOpenCodeToolBoundary(t *testing.T) {
	for _, line := range []string{
		ocToolRead,
		ocStepFinishTools,
		`{"type":"step-finish","reason":"tool_calls"}`,
		`{"type":"tool-call","name":"read"}`,
		`{"type":"message.part.updated","part":{"type":"tool","tool":"bash"}}`,
		// A field typed differently in some release must not hide the boundary.
		`{"type":"tool_use","message":"a string where an object was","part":{"type":"tool"}}`,
	} {
		if !isOpenCodeToolBoundary(line) {
			t.Errorf("%s must mark a tool call", line)
		}
	}
	for _, line := range []string{
		ocStepStart,
		ocStepFinishStop,
		ocText("use the tool"),
		`{"type":"text","part":{"type":"text","text":"x"},"reason":"tool-calls"}`,
		`{"type":"error","error":{"name":"ProviderAuthError"}}`,
		`{"type":"session.completed"}`,
		`Error: Session not found`,
		`{"type":`,
	} {
		if isOpenCodeToolBoundary(line) {
			t.Errorf("%s must not mark a tool call", line)
		}
	}
}

// End to end: the completion frame of a real (stub) multi-step turn.
func TestOpenCodeNativeSend_CompletionCarriesTheFinalAnswer(t *testing.T) {
	// The step_finish frames carry tokens, so the turn commits real OpenCode
	// usage: into this test's own usage cache, never the process-wide one a
	// later propagator start would read back and announce.
	openCodeUsageFixture(t, 1601)
	installOpenCodeStub(t)
	frames := []string{
		ocStepStart, ocText("I'll check the file."), ocToolRead, ocStepFinishTools,
		ocStepStart, ocText("Let me run the tests..."), ocToolBash, ocStepFinishTools,
		ocStepStart, ocText("All 12 tests pass."), ocStepFinishStop,
	}
	t.Setenv("OPENCODE_STUB_STDOUT", strings.Join(frames, "\\n")+"\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-final"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	var completion resultMsg
	if err := m.Send(id, "fix it", func(r resultMsg) {
		if strings.Contains(r.Output, `"aiexpedite.turn_complete"`) {
			completion = r
		}
	}, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	openCodeUsageInFlight.Wait()
	var payload struct {
		Type      string  `json:"type"`
		Text      string  `json:"text"`
		FinalText *string `json:"finalText"`
		Final     bool    `json:"final"`
	}
	if err := json.Unmarshal([]byte(completion.Output), &payload); err != nil {
		t.Fatalf("completion frame %q is not JSON: %v", completion.Output, err)
	}
	if payload.Text != "I'll check the file.Let me run the tests...All 12 tests pass." || !payload.Final {
		t.Fatalf("text changed: %+v", payload)
	}
	if payload.FinalText == nil || *payload.FinalText != "All 12 tests pass." {
		t.Fatalf("finalText = %v, want the answer after the last tool call (frame %s)", payload.FinalText, completion.Output)
	}
	// The transcript (replayed to the model after a lost session) keeps the
	// whole turn, as before.
	if got := sess.Transcript[len(sess.Transcript)-1].Content; got != payload.Text {
		t.Fatalf("transcript = %q, want the whole text", got)
	}
}

func TestOpenCodeCompletionFrame_RedactsTheFinalAnswer(t *testing.T) {
	secret := "mk-live-0123456789abcdef"
	frame := openCodeCompletionFrame("Checking .env.Found it: META_API_KEY="+secret,
		"Found it: META_API_KEY="+secret, false)
	if strings.Contains(frame, secret) {
		t.Fatalf("completion frame leaked the credential: %s", frame)
	}
	var payload struct {
		Text      string `json:"text"`
		FinalText string `json:"finalText"`
	}
	if err := json.Unmarshal([]byte(frame), &payload); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(payload.FinalText, "[REDACTED]") || !strings.HasPrefix(payload.FinalText, "Found it: ") {
		t.Fatalf("finalText must be redacted like text, got %q", payload.FinalText)
	}
}

// Muse Code's completion is unchanged: its `text` is the CLI's own
// `run.terminal.completed` text, not a device-side join of every step.
func TestOneShotCompletionFrame_HasNoFinalText(t *testing.T) {
	if frame := oneShotCompletionFrame("hello world", false); strings.Contains(frame, "finalText") {
		t.Fatalf("the Muse Code completion frame changed shape: %s", frame)
	}
}

// finalText repeats the tail of text, so a reply that fits one publish on its
// own must not fail because of it: it goes out without finalText (a reader
// then takes text, as from an older device). Only a reply too big even
// without it is a turn error, as before.
func TestWithOpenCodeCompletion_DropsTheFinalAnswerBeforeFailingATurn(t *testing.T) {
	base := resultMsg{ID: "s", Type: "opencode_native_message", Status: "success"}

	msg, err := withOpenCodeCompletion(base, "narration.answer", "answer", false)
	if err != nil || !strings.Contains(msg.Output, `"finalText":"answer"`) {
		t.Fatalf("a small turn carries finalText: %v %s", err, msg.Output)
	}

	// Prose, not one long token: the redactor masks an opaque run of letters,
	// which would shrink the frame and hide the case under test.
	const sentence = "all good. "
	big := strings.Repeat(sentence, openCodeNativeMaxPublishSize*6/10/len(sentence))
	msg, err = withOpenCodeCompletion(base, big, big, false)
	if err != nil {
		t.Fatalf("a turn that fits without finalText must still complete: %v", err)
	}
	if strings.Contains(msg.Output, "finalText") || !strings.Contains(msg.Output, `"text":"`+sentence) {
		t.Fatal("the oversize finalText must be dropped and text kept")
	}
	if openCodeNativeEnvelopePublishable(msg) != nil {
		t.Fatal("the completion it returns must be publishable")
	}

	huge := strings.Repeat(sentence, openCodeNativeMaxPublishSize/len(sentence)+1)
	if _, err := withOpenCodeCompletion(base, huge, "", false); err == nil {
		t.Fatal("a completion too big even without finalText is still a turn error")
	}
}
