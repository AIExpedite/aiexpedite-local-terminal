package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// Verbatim events from agy 1.2.6 (`--output-format stream-json`), captured on a
// Windows 11 desktop on 2026-09-18. The same run was then resumed from a second
// process with `--conversation <id>` and answered from the first turn's context
// (`num_turns: 2`, same conversation_id) — which is the behaviour this file
// exists to reach from the cloud.
const (
	agyProbeConversationID = "ea663263-95c7-4849-b49a-06225d5969d0"
	agyProbeInit           = `{"event":"init","conversation_id":"ea663263-95c7-4849-b49a-06225d5969d0","init":{"cwd":"C:\\repo","tools":["ask_permission"]}}`
	agyProbeStep           = `{"event":"step_update","step_update":{"conversation_id":"ea663263-95c7-4849-b49a-06225d5969d0","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"PONG"}}`
	agyProbeResult         = `{"event":"result","result":{"conversation_id":"ea663263-95c7-4849-b49a-06225d5969d0","status":"SUCCESS","response":"PONG\n","num_turns":1}}`
)

func TestExtractCliConversationID(t *testing.T) {
	cases := []struct {
		name, command, line, want string
	}{
		{"agy init event", "agy", agyProbeInit, agyProbeConversationID},
		{"agy step_update (init frame lost)", "agy", agyProbeStep, agyProbeConversationID},
		{"agy result event", "agy", agyProbeResult, agyProbeConversationID},
		{"agy by explicit windows path", `C:\Users\me\AppData\Local\agy\agy.exe`, agyProbeInit, agyProbeConversationID},
		{"opencode top-level sessionID", "opencode", `{"type":"step_start","sessionID":"ses_7f3aK2pQ9xLmN4","part":{"type":"step-start"}}`, "ses_7f3aK2pQ9xLmN4"},
		// parseOpenCodeEventLine falls back to info.id, which is a MESSAGE id on
		// some events. First id wins for the session, so it must not be adopted.
		{"opencode message id is not a session id", "opencode", `{"type":"message.updated","info":{"id":"msg_9aB2cD3eF4gH5i"}}`, ""},
		{"plain text", "agy", "Compiling…", ""},
		{"malformed json", "agy", `{"event":"init","conversation_id":`, ""},
		// The id becomes a process ARGUMENT.
		{"flag-shaped id is refused", "agy", `{"event":"init","conversation_id":"--dangerously-skip-permissions"}`, ""},
		{"id with whitespace is refused", "agy", `{"event":"init","conversation_id":"abcd efgh ijkl"}`, ""},
		// Resident CLIs are not one-shot; their follow-ups are typed into stdin.
		{"claude is not captured", "claude", `{"type":"system","subtype":"init","session_id":"ea663263-95c7-4849-b49a-06225d5969d0"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCliConversationID(tc.command, tc.line); got != tc.want {
				t.Fatalf("extractCliConversationID(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func TestCLISessionConversationCaptureIsFirstWinsAndPublishedOnce(t *testing.T) {
	s := &CLISession{Command: "agy"}

	if got := s.takeUnpublishedCliConversationID(); got != "" {
		t.Fatalf("nothing captured yet, got %q", got)
	}
	s.noteCliConversationID("plain text before the stream starts")
	s.noteCliConversationID(agyProbeInit)
	// A different id later in the same process must not replace the first.
	s.noteCliConversationID(`{"event":"step_update","step_update":{"conversation_id":"11111111-2222-3333-4444-555555555555"}}`)

	if got := s.takeUnpublishedCliConversationID(); got != agyProbeConversationID {
		t.Fatalf("first take = %q, want %q", got, agyProbeConversationID)
	}
	// Once per session on stream frames: the results path reads the session
	// document whenever a frame carries an id.
	if got := s.takeUnpublishedCliConversationID(); got != "" {
		t.Fatalf("second take = %q, want empty", got)
	}
	// …but session_ended always repeats it.
	if got := s.capturedCliConversationID(); got != agyProbeConversationID {
		t.Fatalf("captured = %q, want %q", got, agyProbeConversationID)
	}
}

func TestApplyCliResumeSeedAntigravity(t *testing.T) {
	callerArgs := []string{"--effort", "medium", "finish the review"}
	shaped, prompt := buildAntigravityStreamingArgs(callerArgs)
	if prompt == nil || *prompt != "finish the review" {
		t.Fatalf("fixture: prompt must ride stdin, got %v", prompt)
	}

	seeded, err := applyCliResumeSeed("agy", callerArgs, shaped, agyProbeConversationID, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"--dangerously-skip-permissions",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--conversation", agyProbeConversationID,
		"--effort", "medium",
	}
	if !reflect.DeepEqual(seeded, want) {
		t.Fatalf("seeded argv =\n  %v\nwant\n  %v", seeded, want)
	}
	// The input slice is not mutated (StartSession still holds it).
	if strings.Contains(strings.Join(shaped, " "), "--conversation") {
		t.Fatalf("applyCliResumeSeed mutated its input: %v", shaped)
	}
}

func TestApplyCliResumeSeedOpenCode(t *testing.T) {
	callerArgs := []string{"--model", "anthropic/claude-sonnet-4-5", "fix the failing test"}
	shaped := buildOpenCodeInteractiveArgs(callerArgs)

	seeded, err := applyCliResumeSeed("opencode", callerArgs, shaped, "ses_7f3aK2pQ9xLmN4", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"run", "--format", "json",
		"--session", "ses_7f3aK2pQ9xLmN4",
		"--model", "anthropic/claude-sonnet-4-5",
		"fix the failing test",
	}
	if !reflect.DeepEqual(seeded, want) {
		t.Fatalf("seeded argv =\n  %v\nwant\n  %v", seeded, want)
	}
}

// Session control is server-owned: a caller-typed --session is still stripped,
// so only the signed seed can put the flag on the command line.
func TestCallerTypedOpenCodeSessionFlagIsStillStripped(t *testing.T) {
	shaped := buildOpenCodeInteractiveArgs([]string{"--session", "ses_attackerChosen1", "do it"})
	if strings.Contains(strings.Join(shaped, " "), "--session") {
		t.Fatalf("caller --session survived shaping: %v", shaped)
	}
}

func TestApplyCliResumeSeedFailsClosed(t *testing.T) {
	agyArgs := []string{"continue"}
	agyShaped, _ := buildAntigravityStreamingArgs(agyArgs)
	callerResume := []string{"--conversation", "11111111-2222-3333-4444-555555555555", "continue"}
	callerResumeShaped, _ := buildAntigravityStreamingArgs(callerResume)

	cases := []struct {
		name, command  string
		caller, shaped []string
		id             string
		tty            bool
		wantErr        string
	}{
		{"flag-shaped id", "agy", agyArgs, agyShaped, "--print", false, "not a valid CLI argument"},
		{"empty id", "agy", agyArgs, agyShaped, "", false, "not a valid CLI argument"},
		{"tty session", "agy", agyArgs, agyShaped, agyProbeConversationID, true, "tty"},
		{"resident CLI", "claude", agyArgs, agyArgs, agyProbeConversationID, false, "one-shot"},
		{"agy diagnostic", "agy", []string{"--version"}, []string{"--version"}, agyProbeConversationID, false, "not a managed agy turn"},
		{"opencode diagnostic", "opencode", []string{"models"}, []string{"models"}, "ses_7f3aK2pQ9xLmN4", false, "not an opencode run"},
		{"caller already typed a conversation", "agy", callerResume, callerResumeShaped, agyProbeConversationID, false, "already present"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyCliResumeSeed(tc.command, tc.caller, tc.shaped, tc.id, tc.tty)
			if err == nil {
				t.Fatalf("expected a refusal, got argv %v", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// The seed rides the SIGNED conversationId field. A tampered id must not verify.
func TestSessionStartConversationSeedIsSigned(t *testing.T) {
	secret := "test-secret"
	cmd := commandMsg{
		ID:             "cmd-1",
		Command:        "agy",
		Args:           []string{"finish the review"},
		Ts:             1_700_000_000_000,
		Type:           "session_start",
		SessionID:      "sess-1",
		ConversationID: agyProbeConversationID,
	}
	// Signed the way Node's signCommand does (see verify_sig_test.go).
	nodeJSON, err := nodeJSONStringify(signaturePayload{
		ID:             cmd.ID,
		Command:        cmd.Command,
		Args:           cmd.Args,
		Ts:             cmd.Ts,
		Type:           cmd.Type,
		SessionID:      cmd.SessionID,
		ConversationID: cmd.ConversationID,
	})
	if err != nil {
		t.Fatalf("nodeJSONStringify failed: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nodeJSON)
	cmd.Signature = hex.EncodeToString(mac.Sum(nil))
	if !verifySignature(cmd, secret) {
		t.Fatal("fixture: a correctly signed seeded start must verify")
	}
	cmd.ConversationID = "11111111-2222-3333-4444-555555555555"
	if verifySignature(cmd, secret) {
		t.Fatal("a swapped conversationId verified — the seed is not covered by the signature")
	}
}
