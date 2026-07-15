package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsAntigravityNativeCommand(t *testing.T) {
	for _, ok := range []string{"antigravity_native_start", "antigravity_native_send", "antigravity_native_end"} {
		if !isAntigravityNativeCommand(ok) {
			t.Errorf("expected %q to be antigravity native command", ok)
		}
	}
	for _, no := range []string{"session_start", "claude_native_start", "grok_acp_start", "codex_appserver_start", ""} {
		if isAntigravityNativeCommand(no) {
			t.Errorf("did not expect %q to be antigravity native command", no)
		}
	}
}

func TestBuildAntigravityNativeArgs_PrintTakesPromptValue(t *testing.T) {
	args := buildAntigravityNativeArgs("hello world", "")
	// --print must take the prompt as its value (agy 1.1.x contract).
	if len(args) < 2 || args[0] != "--print" || args[1] != "hello world" {
		t.Fatalf("expected --print <prompt>, got %#v", args)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--continue") {
		t.Fatalf("--continue must never appear: %q", joined)
	}
}

func TestBuildAntigravityNativeArgs_ExactConversationResume(t *testing.T) {
	args := buildAntigravityNativeArgs("follow up", "abc-123-def")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--conversation") {
		t.Fatalf("expected --conversation, got %q", joined)
	}
	// Flag + value adjacent.
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--conversation" && args[i+1] == "abc-123-def" {
			found = true
		}
		if args[i] == "--continue" || args[i] == "-c" {
			t.Fatalf("must not use --continue; got %#v", args)
		}
	}
	if !found {
		t.Fatalf("expected --conversation abc-123-def in %#v", args)
	}
}

func TestBuildAntigravityNativeArgs_NeverContinueFlag(t *testing.T) {
	// Prompt text may literally contain "--continue"; that is the --print
	// value, not a CLI flag. The builder must never emit a free-standing
	// --continue / -c flag token (only --conversation for resume).
	args := buildAntigravityNativeArgs("please --continue later", "id-1")
	for i, a := range args {
		if a == "--continue" || a == "-c" {
			// Only allowed as the value of --print (index 1).
			if i == 1 && args[0] == "--print" {
				continue
			}
			t.Fatalf("argv must not include --continue as a flag: %#v", args)
		}
	}
	// Confirm resume uses exact ID flag only.
	hasConv := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--conversation" && args[i+1] == "id-1" {
			hasConv = true
		}
	}
	if !hasConv {
		t.Fatalf("expected --conversation id-1: %#v", args)
	}
}

func TestAppendAntigravityTranscript_Bounds(t *testing.T) {
	var tr []antigravityTurn
	for i := 0; i < 40; i++ {
		tr = appendAntigravityTranscript(tr, "user", strings.Repeat("u", 100))
		tr = appendAntigravityTranscript(tr, "assistant", strings.Repeat("a", 100))
	}
	if len(tr) > antigravityReplayMaxMessages {
		t.Fatalf("transcript exceeded message bound: %d > %d", len(tr), antigravityReplayMaxMessages)
	}
	if totalChars(tr) > antigravityReplayMaxChars+200 {
		// allow small overshoot on last append before trim
		t.Fatalf("transcript char bound not enforced: %d", totalChars(tr))
	}
	// Most recent turns retained.
	if tr[len(tr)-1].Role != "assistant" {
		t.Fatalf("expected last turn assistant, got %s", tr[len(tr)-1].Role)
	}
}

func TestBuildAntigravityReplayPrompt_RetainsCurrentUserTurn(t *testing.T) {
	tr := []antigravityTurn{
		{Role: "user", Content: "secret is ORANGE", At: time.Now()},
		{Role: "assistant", Content: "OK", At: time.Now()},
	}
	prompt := buildAntigravityReplayPrompt(tr, "what is the secret?")
	if !strings.Contains(prompt, "ORANGE") {
		t.Fatalf("replay must retain prior user content: %q", prompt)
	}
	if !strings.Contains(prompt, "what is the secret?") {
		t.Fatalf("replay must include current user turn: %q", prompt)
	}
	if !strings.Contains(prompt, "Answer ONLY the final user message") {
		t.Fatalf("replay must mark the final turn: %q", prompt)
	}
}

func TestAntigravityTurnPrompt_ReplaysWhenNoNativeIDButHistory(t *testing.T) {
	tr := []antigravityTurn{
		{Role: "user", Content: "secret is ORANGE", At: time.Now()},
		{Role: "assistant", Content: "OK", At: time.Now()},
	}

	// First turn: no native ID, no history -> bare prompt, no replay.
	if p, replay := antigravityTurnPrompt("", nil, "hi"); replay || p != "hi" {
		t.Fatalf("first turn should send bare prompt without replay, got prompt=%q replay=%v", p, replay)
	}

	// Follow-up with a captured native ID -> native resume, bare prompt.
	if p, replay := antigravityTurnPrompt("native-123", tr, "what is the secret?"); replay || p != "what is the secret?" {
		t.Fatalf("native-resume turn should send bare prompt, got prompt=%q replay=%v", p, replay)
	}

	// Follow-up where capture failed (no native ID) but history exists -> replay
	// so context is not silently lost.
	p, replay := antigravityTurnPrompt("", tr, "what is the secret?")
	if !replay {
		t.Fatalf("expected replay when history exists but no native ID")
	}
	if !strings.Contains(p, "ORANGE") || !strings.Contains(p, "what is the secret?") {
		t.Fatalf("replay prompt must carry prior context and current turn, got %q", p)
	}
}

func TestRedactAntigravitySecrets(t *testing.T) {
	in := "Authorization: Bearer supersecrettoken12345 and api_key=abcdefghijklmnop"
	out := redactAntigravitySecrets(in)
	if strings.Contains(out, "supersecrettoken12345") {
		t.Fatalf("token not redacted: %q", out)
	}
	if strings.Contains(out, "abcdefghijklmnop") {
		t.Fatalf("api key not redacted: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("expected REDACTED marker: %q", out)
	}
}

func TestCaptureAntigravityNativeID_PrefersNewDBOverLastConversations(t *testing.T) {
	// Concurrent first turns: last_conversations may point at another chat's
	// ID for the same cwd; when beforeIDs is provided we must prefer the new DB.
	tmp := t.TempDir()
	// Redirect both HOME (Unix) and USERPROFILE (Windows) so os.UserHomeDir
	// resolves to the temp fixture on every platform, not just Unix.
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	base := filepath.Join(tmp, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(filepath.Join(base, "cache"), 0o755)
	_ = os.MkdirAll(filepath.Join(base, "conversations"), 0o755)
	cwd := "/tmp/shared-cwd"
	staleID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	newID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	_ = os.WriteFile(filepath.Join(base, "cache", "last_conversations.json"),
		[]byte(`{"`+cwd+`":"`+staleID+`"}`), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", staleID+".db"), []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", newID+".db"), []byte("new"), 0o644)
	// beforeIDs includes stale so only newID is "new"
	before := map[string]struct{}{staleID: {}}
	got := captureAntigravityNativeID(cwd, before)
	if got != newID {
		t.Fatalf("capture with beforeIDs = %q, want new %q (not last_conversations %q)", got, newID, staleID)
	}
}

func TestCaptureAntigravityNativeID_FromLastConversations(t *testing.T) {
	// Point home at a temp dir. Redirect both HOME (Unix) and USERPROFILE
	// (Windows) so os.UserHomeDir resolves to the fixture on every platform.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	base := filepath.Join(tmp, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(base, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := "/tmp/proj-a"
	id := "11111111-2222-3333-4444-555555555555"
	// Write last_conversations + empty db file.
	if err := os.WriteFile(filepath.Join(base, "cache", "last_conversations.json"),
		[]byte(`{"`+cwd+`":"`+id+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "conversations", id+".db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := captureAntigravityNativeID(cwd, nil)
	if got != id {
		t.Fatalf("captureAntigravityNativeID = %q, want %q", got, id)
	}
}

func TestCaptureAntigravityNativeID_NewDBFallback(t *testing.T) {
	tmp := t.TempDir()
	// Redirect both HOME (Unix) and USERPROFILE (Windows) so os.UserHomeDir
	// resolves to the temp fixture on every platform, not just Unix.
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	base := filepath.Join(tmp, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	before := map[string]struct{}{"old-id": {}}
	_ = os.WriteFile(filepath.Join(base, "old-id.db"), []byte("o"), 0o644)
	newID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	_ = os.WriteFile(filepath.Join(base, newID+".db"), []byte("n"), 0o644)

	got := captureAntigravityNativeID("/no/mapping", before)
	if got != newID {
		t.Fatalf("fallback capture = %q, want %q", got, newID)
	}
}

func TestAntigravityNativeManager_StartSendIsolation(t *testing.T) {
	// Unit-level: two sessions with different IDs never share native IDs
	// when we set them independently (no real agy needed).
	m := NewAntigravityNativeManager()
	cwd := t.TempDir()
	if err := m.Start("sess-a", cwd, "ws", "uid", nil); err != nil {
		// probeAntigravityNativeCapability may fail if agy missing — skip.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not runnable") ||
			strings.Contains(err.Error(), "below minimum") || strings.Contains(err.Error(), "unsupported") {
			t.Skipf("agy not available for integration-style unit test: %v", err)
		}
		t.Fatalf("start a: %v", err)
	}
	if err := m.Start("sess-b", cwd, "ws", "uid", nil); err != nil {
		t.Fatalf("start b: %v", err)
	}
	sa, sb := m.Get("sess-a"), m.Get("sess-b")
	if sa == nil || sb == nil {
		t.Fatal("sessions missing")
	}
	sa.NativeConversationID = "native-a"
	sb.NativeConversationID = "native-b"
	if sa.NativeConversationID == sb.NativeConversationID {
		t.Fatal("sessions must not share native conversation IDs")
	}
	_ = m.End("sess-a")
	_ = m.End("sess-b")
	if m.Get("sess-a") != nil || m.Get("sess-b") != nil {
		t.Fatal("ended sessions must be removed")
	}
}

func TestCompareSemver(t *testing.T) {
	if compareSemver("1.1.1", "1.1.1") != 0 {
		t.Fatal("equal")
	}
	if compareSemver("1.1.0", "1.1.1") >= 0 {
		t.Fatal("1.1.0 should be < 1.1.1")
	}
	if compareSemver("1.2.0", "1.1.1") <= 0 {
		t.Fatal("1.2.0 should be > 1.1.1")
	}
}

func TestLooksLikeMissingConversation(t *testing.T) {
	if !looksLikeMissingConversation("", "conversation not found") {
		t.Fatal("expected match")
	}
	if looksLikeMissingConversation("hello", "ok") {
		t.Fatal("false positive")
	}
}

func TestBuildAntigravityNativeArgs_RejectsOversizedPromptAtSend(t *testing.T) {
	// Size gate is in Send; ensure constant is sane relative to CreateProcess budget.
	if antigravityNativeMaxPromptBytes <= 0 || antigravityNativeMaxPromptBytes >= 32*1024 {
		t.Fatalf("max prompt bytes should be positive and under 32KB, got %d", antigravityNativeMaxPromptBytes)
	}
}

func TestCaptureLimited_DrainsAfterTruncate(t *testing.T) {
	// Truncation must keep reading so a producer writing past the limit cannot
	// block forever on a full pipe (would deadlock cmd.Wait in runOneShot).
	pr, pw := io.Pipe()
	const limit = 64
	done := make(chan *limitedBuffer, 1)
	go func() {
		done <- captureLimited(pr, limit)
	}()

	// Write more than limit; producer must not hang after consumer truncates.
	payload := strings.Repeat("x", limit*4)
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(pw, payload)
		_ = pw.Close()
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("writer blocked or failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer hung — captureLimited did not drain after truncate")
	}

	lb := <-done
	if !lb.trunc {
		t.Fatal("expected truncation")
	}
	if lb.b.Len() != limit {
		t.Fatalf("captured %d bytes, want %d", lb.b.Len(), limit)
	}
}

func TestLooksLikeMissingConversation_DoesNotMatchGenericErrors(t *testing.T) {
	// Generic failures must NOT trigger replay (would double model cost).
	if looksLikeMissingConversation("permission denied", "exit status 1") {
		t.Fatal("generic errors must not look like missing conversation")
	}
	if looksLikeMissingConversation("timeout waiting for model", "") {
		t.Fatal("timeout must not look like missing conversation")
	}
	if !looksLikeMissingConversation("", "Error: conversation not found") {
		t.Fatal("expected missing-conversation match")
	}
	// Successful exit with prose mentioning the phrase is handled by the
	// Send-path exitCode != 0 gate (not this helper alone).
}

func TestRedactAntigravitySecrets_PreservesShortDiagnostics(t *testing.T) {
	uuid := "11111111-2222-3333-4444-555555555555"
	out := redactAntigravitySecrets("session " + uuid + " failed")
	if !strings.Contains(out, uuid) {
		t.Fatalf("UUID-length diagnostics should not be redacted: %q", out)
	}
	token := "Authorization: Bearer " + strings.Repeat("a", 40)
	out2 := redactAntigravitySecrets(token)
	if strings.Contains(out2, strings.Repeat("a", 40)) {
		t.Fatalf("bearer token should be redacted: %q", out2)
	}
}

// Ensure legacy one-shot builder is unchanged (regression guard).
func TestLegacyBuildAntigravityInteractiveArgs_Unchanged(t *testing.T) {
	args := buildAntigravityInteractiveArgs([]string{"do the task"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--print") {
		t.Fatalf("legacy one-shot must still use --print: %q", joined)
	}
	if !strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Fatalf("legacy one-shot still uses permission skip: %q", joined)
	}
	// Prompt remains positional on the legacy path.
	if args[len(args)-1] != "do the task" {
		t.Fatalf("legacy path keeps positional prompt: %#v", args)
	}
}
