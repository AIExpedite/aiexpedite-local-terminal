package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   opencode_auth_refusal_test.go — OpenCode's ProviderAuthError record: matched
   by shared-constants openCodeAuthRefusal's rule, and reported in the turn's
   error frame as a sentence plus the record rebuilt from the provider id alone
   — never OpenCode's own message. The vectors mirror shared-constants
   tests/opencode.test.js.
   ------------------------------------------------------------------------ */

// openCodeSignedOutLine is the captured whole output of a signed-out turn
// (prod 2026-10-15/16), as shared-constants pins it.
const openCodeSignedOutLine = `{"type":"error","timestamp":1792148521225,"sessionID":"ses_ebb9fc7efffehtZ25Sse0aW6Uh","error":{"name":"ProviderAuthError","data":{"providerID":"google","message":"Google Generative AI API key is missing. Pass it using the 'apiKey' parameter or the GOOGLE_GENERATIVE_AI_API_KEY environment variable."}}}`

// openCodeAuthErrorLine is shared-constants' authErrorLine: a ProviderAuthError
// record carrying data (omitted when nil).
func openCodeAuthErrorLine(t *testing.T, data any) string {
	t.Helper()
	record := map[string]any{"type": "error", "sessionID": "ses_x", "error": map[string]any{"name": "ProviderAuthError"}}
	if data != nil {
		record["error"].(map[string]any)["data"] = data
	}
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenCodeProviderAuthRefusal_MatchesTheCapturedRecord(t *testing.T) {
	if len(openCodeSignedOutLine) != 303 {
		t.Fatalf("captured line is %d bytes, want the 303 shared-constants pins", len(openCodeSignedOutLine))
	}
	for _, line := range []string{openCodeSignedOutLine, "  " + openCodeSignedOutLine + "  \r"} {
		if id, ok := openCodeProviderAuthRefusal(line); !ok || id != "google" {
			t.Fatalf("refusal(%q) = %q, %v; want google", line, id, ok)
		}
	}
}

func TestOpenCodeProviderAuthRefusal_IgnoresOtherErrorsProseQuotedRecordsAndJunk(t *testing.T) {
	quoted, _ := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"text": openCodeSignedOutLine}})
	for _, line := range []string{
		`{"type":"error","error":{"name":"APIError","data":{"message":"x"}}}`,
		`{"type":"text","error":{"name":"ProviderAuthError"}}`,
		string(quoted),
		"opencode failed with ProviderAuthError: Google Generative AI API key is missing.",
		"The previous turn ended with a ProviderAuthError (Google Generative AI API key missing). If you can continue, please retry.",
		`{"type":"error"`,
		`{"type":"error","error":{"name":"ProviderAuthError"`,
		`{"type":"error","error":"ProviderAuthError"}`,
		`{"type":"error","error":{"name":7},"x":"ProviderAuthError"}`,
		`{"type":["error"],"error":{"name":"ProviderAuthError"}}`,
		`[{"type":"error","error":{"name":"ProviderAuthError"}}]`,
		"[]",
		"null",
		"",
	} {
		if id, ok := openCodeProviderAuthRefusal(line); ok {
			t.Errorf("refusal(%q) = %q, matched; want no match", line, id)
		}
	}
}

// A provider id the pattern does not admit is dropped — the record still
// refuses, without a name — and the message is never read into anything.
func TestOpenCodeProviderAuthRefusal_KeepsOnlyAWellFormedProviderID(t *testing.T) {
	for _, providerID := range []any{"../../home/alice", strings.Repeat("a", 41), "", "two words", 7, nil, map[string]any{}} {
		data := map[string]any{"providerID": providerID, "message": "/Users/alice"}
		if id, ok := openCodeProviderAuthRefusal(openCodeAuthErrorLine(t, data)); !ok || id != "" {
			t.Errorf("providerID %#v: refusal = %q, %v; want a nameless refusal", providerID, id, ok)
		}
	}
	for _, line := range []string{
		openCodeAuthErrorLine(t, nil),
		`{"type":"error","error":{"name":"ProviderAuthError","data":"not an object"}}`,
		`{"type":"error","error":{"name":"ProviderAuthError","data":null}}`,
	} {
		if id, ok := openCodeProviderAuthRefusal(line); !ok || id != "" {
			t.Errorf("refusal(%q) = %q, %v; want a nameless refusal", line, id, ok)
		}
	}
	forty := strings.Repeat("a", 40)
	if id, ok := openCodeProviderAuthRefusal(openCodeAuthErrorLine(t, map[string]any{"providerID": forty})); !ok || id != forty {
		t.Fatalf("refusal = %q, %v; want the 40-character id", id, ok)
	}
	for _, id := range []string{"google", "github-copilot", "amazon-bedrock", "x.ai", "my_provider"} {
		if got, ok := openCodeProviderAuthRefusal(openCodeAuthErrorLine(t, map[string]any{"providerID": id})); !ok || got != id {
			t.Errorf("refusal for %q = %q, %v", id, got, ok)
		}
	}
}

// The turn error is a sentence for a person, then the record rebuilt from the
// provider id alone as the LAST line — the line the cloud reads off a failed
// turn's output — and it survives the publish redaction unchanged.
func TestOpenCodeAuthRefusalTurnError(t *testing.T) {
	for _, c := range []struct {
		providerID, want string
	}{
		{"google", "OpenCode could not use its model: no credentials for provider google on this computer.\n" +
			`{"type":"error","error":{"name":"ProviderAuthError","data":{"providerID":"google"}}}`},
		{"", "OpenCode could not use its model: no credentials for its provider on this computer.\n" +
			`{"type":"error","error":{"name":"ProviderAuthError"}}`},
		// An id the pattern does not admit is never quoted.
		{"../../home/alice", "OpenCode could not use its model: no credentials for its provider on this computer.\n" +
			`{"type":"error","error":{"name":"ProviderAuthError"}}`},
	} {
		got := openCodeAuthRefusalTurnError(c.providerID)
		if got != c.want {
			t.Fatalf("turn error for %q =\n%s\nwant\n%s", c.providerID, got, c.want)
		}
		if redactAgentSecrets(got) != got {
			t.Fatalf("redaction changed the turn error: %q", redactAgentSecrets(got))
		}
		last := got[strings.LastIndex(got, "\n")+1:]
		wantID := c.providerID
		if !openCodeAuthProviderIDPattern.MatchString(wantID) {
			wantID = ""
		}
		if id, ok := openCodeProviderAuthRefusal(last); !ok || id != wantID {
			t.Fatalf("the last line %q reads back as %q, %v; want %q", last, id, ok, wantID)
		}
	}
}

/* --------------------------------------------------------------------------
   End to end through the compiled stub (opencode_stub_test.go).
   -------------------------------------------------------------------------- */

func TestOpenCodeNativeManager_AProviderAuthErrorNamesTheProviderAndNeverItsMessage(t *testing.T) {
	// The failed turn arms and settles a usage run: keep it in this test's ledger.
	openCodeUsageFixture(t, 5301)
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", openCodeSignedOutLine+`\n`)
	t.Setenv("OPENCODE_STUB_EXIT", "1")

	m := NewOpenCodeNativeManager()
	id := "sess-auth-refused"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second)
	if err == nil {
		t.Fatal("a turn refused for a missing key must fail")
	}
	last := frames[len(frames)-1]
	if last.Type != "opencode_native_error" || last.Status != "error" {
		t.Fatalf("last frame = %#v, want a terminal opencode_native_error", last)
	}
	want := openCodeAuthRefusalTurnError("google")
	if last.Output != want || err.Error() != want {
		t.Fatalf("turn error = %q (err %q), want %q", last.Output, err.Error(), want)
	}
	for _, leak := range []string{"Generative AI", "GOOGLE_GENERATIVE_AI_API_KEY", "apiKey", "ses_ebb9fc7efffehtZ25Sse0aW6Uh", "exited with code"} {
		if strings.Contains(last.Output, leak) {
			t.Fatalf("turn error %q carries %q", last.Output, leak)
		}
	}
	// The record itself is still forwarded as the stream event it is.
	if frames[0].Type != "opencode_native_message" || !strings.Contains(frames[0].Output, "ProviderAuthError") {
		t.Fatalf("first frame = %#v, want the streamed record", frames[0])
	}
	if len(sess.Transcript) != 0 || sess.NativeSessionID != "" {
		t.Fatalf("a refused turn reached the transcript (%#v) or the native id (%q)", sess.Transcript, sess.NativeSessionID)
	}
}

// The record is evidence only on a turn that FAILS: one that completes keeps
// its completion, and a record quoted inside a text event is not the record.
func TestOpenCodeNativeManager_TheRecordCountsOnlyForAFailedTurn(t *testing.T) {
	openCodeUsageFixture(t, 5302)
	installOpenCodeStub(t)

	t.Run("a turn that completes", func(t *testing.T) {
		t.Setenv("OPENCODE_STUB_STDOUT", openCodeSignedOutLine+`\n{"type":"text","text":"done anyway"}\n`)
		t.Setenv("OPENCODE_STUB_EXIT", "0")
		m := NewOpenCodeNativeManager()
		injectOpenCodeSession(t, m, "sess-auth-completed", t.TempDir())
		var frames []resultMsg
		if err := m.Send("sess-auth-completed", "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err != nil {
			t.Fatalf("a completed turn failed: %v", err)
		}
		if last := frames[len(frames)-1]; last.Type != "opencode_native_message" || !strings.Contains(last.Output, "done anyway") {
			t.Fatalf("last frame = %#v, want the completion", last)
		}
	})

	t.Run("a quoted record in a failed turn", func(t *testing.T) {
		quoted, _ := json.Marshal(map[string]any{"type": "text", "text": "it said " + openCodeSignedOutLine})
		t.Setenv("OPENCODE_STUB_STDOUT", string(quoted)+`\n`)
		t.Setenv("OPENCODE_STUB_EXIT", "1")
		m := NewOpenCodeNativeManager()
		injectOpenCodeSession(t, m, "sess-auth-quoted", t.TempDir())
		var frames []resultMsg
		if err := m.Send("sess-auth-quoted", "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err == nil {
			t.Fatal("a non-zero exit must fail the turn")
		}
		last := frames[len(frames)-1]
		if last.Type != "opencode_native_error" || !strings.HasPrefix(last.Output, "OpenCode exited with code 1") {
			t.Fatalf("last frame = %#v, want the generic failure", last)
		}
	})
}
