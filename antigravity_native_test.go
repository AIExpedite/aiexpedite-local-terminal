package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

// A replay built from a large history must fit the SEND limit (24KB), not the
// larger 48KB transcript bound — otherwise Send rejects it and recovery fails
// for histories between the two limits. The final user turn must survive.
func TestBuildAntigravityReplayPrompt_FitsSendLimit(t *testing.T) {
	var tr []antigravityTurn
	// ~40KB of prior history: over the 24KB send limit, under the 48KB bound.
	for i := 0; i < 20; i++ {
		tr = append(tr,
			antigravityTurn{Role: "user", Content: strings.Repeat("u", 1000), At: time.Now()},
			antigravityTurn{Role: "assistant", Content: strings.Repeat("a", 1000), At: time.Now()},
		)
	}
	final := "FINAL_USER_QUESTION_marker"
	prompt := buildAntigravityReplayPrompt(tr, final)

	if len(prompt) > antigravityNativeMaxPromptBytes {
		t.Fatalf("replay prompt %d bytes exceeds send limit %d", len(prompt), antigravityNativeMaxPromptBytes)
	}
	if !strings.Contains(prompt, final) {
		t.Fatalf("replay dropped the final user turn: %q", prompt[:min(200, len(prompt))])
	}
	if !strings.Contains(prompt, "Answer ONLY the final user message") {
		t.Fatalf("replay dropped the preamble instructions")
	}
	if !utf8.ValidString(prompt) {
		t.Fatalf("replay produced invalid UTF-8")
	}
}

// When the current user prompt is near the 24KB send limit, prior history must
// be dropped whole-turn (or the prompt must fail closed oversized) — never
// byte-slice into the middle of the current "User: ..." turn.
func TestBuildAntigravityReplayPrompt_NeverTruncatesCurrentTurn(t *testing.T) {
	const preamble = "You are continuing an AIExpedite Antigravity Chat conversation after native resume failed. " +
		"Prior turns (oldest first) follow. Treat them as history only. " +
		"Answer ONLY the final user message.\n\n"
	budget := antigravityNativeMaxPromptBytes - len(preamble)
	// Near-limit current turn: leaves only a few dozen bytes for history.
	// Prefix/suffix markers prove the full current turn survived (not a tail).
	markerHead := "HEAD_MARKER_xyz"
	markerTail := "TAIL_MARKER_xyz"
	// "User: " + text + "\n" should be budget-80 so some history would have fit
	// under a naïve total-size check, but a large prior turn still forces drop.
	overhead := len("User: ") + len("\n") + len(markerHead) + len(markerTail)
	midLen := budget - 80 - overhead
	if midLen < 100 {
		t.Fatalf("test setup: budget too small (%d)", budget)
	}
	final := markerHead + strings.Repeat("Q", midLen) + markerTail

	tr := []antigravityTurn{
		{Role: "user", Content: strings.Repeat("OLD_HISTORY_SHOULD_DROP_", 200), At: time.Now()},
		{Role: "assistant", Content: strings.Repeat("OLD_ASSIST_", 200), At: time.Now()},
	}
	prompt := buildAntigravityReplayPrompt(tr, final)

	if !strings.Contains(prompt, markerHead) {
		t.Fatalf("replay truncated leading bytes of current user turn")
	}
	if !strings.Contains(prompt, markerTail) {
		t.Fatalf("replay lost trailing bytes of current user turn")
	}
	// Full current text must appear as a single contiguous "User: ..." line.
	wantLine := "User: " + final + "\n"
	if !strings.Contains(prompt, wantLine) {
		t.Fatalf("current turn was rewritten or split; want contiguous %q… in prompt", wantLine[:min(60, len(wantLine))])
	}
	if strings.Contains(prompt, "OLD_HISTORY_SHOULD_DROP_") {
		// History may remain only if it fits; with our sizes it must not.
		t.Fatalf("expected oversized prior history to be dropped, still present")
	}
	if len(prompt) > antigravityNativeMaxPromptBytes {
		t.Fatalf("replay prompt %d exceeds send limit %d", len(prompt), antigravityNativeMaxPromptBytes)
	}

	// Near-limit bare prompt: preamble + final alone exceed the limit. Must
	// still keep the full current turn (callers fail closed on size).
	huge := strings.Repeat("H", antigravityNativeMaxPromptBytes-10)
	oversized := buildAntigravityReplayPrompt(tr, huge)
	if !strings.Contains(oversized, "User: "+huge+"\n") {
		t.Fatalf("oversized current turn must still appear in full for fail-closed callers")
	}
	if len(oversized) <= antigravityNativeMaxPromptBytes {
		t.Fatalf("expected oversized replay so Send rejects rather than truncating user text")
	}
}

// The approval gate must display/allow the same argv runOneShot executes,
// including the leading --dangerously-skip-permissions, so a narrowed allowlist
// (e.g. `agy --print *`) cannot approve a command that silently runs with
// auto-approved tool permissions.
func TestAntigravityNativeGateArgs_MatchExecutedShape(t *testing.T) {
	gate := buildAntigravityNativeGateArgs()
	if len(gate) == 0 || gate[0] != "--dangerously-skip-permissions" {
		t.Fatalf("gate argv must lead with --dangerously-skip-permissions, got %#v", gate)
	}
	// Gate must equal the first-turn launch shape (dangerous flags + native args
	// with the prompt placeholder, no --conversation).
	want := append(antigravityDangerousFlags(), buildAntigravityNativeArgs("<prompt>", "")...)
	if strings.Join(gate, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("gate argv %#v drifted from executed shape %#v", gate, want)
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

func TestCaptureAntigravityNativeID_RequiresCwdMappedNewID(t *testing.T) {
	// When last_conversations still maps this cwd to a pre-existing ID, a new
	// global DB (other cwd / local TUI) must NOT be adopted. Fail closed so
	// Send falls back to bounded transcript replay.
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
	unrelatedID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	_ = os.WriteFile(filepath.Join(base, "cache", "last_conversations.json"),
		[]byte(`{"`+cwd+`":"`+staleID+`"}`), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", staleID+".db"), []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", unrelatedID+".db"), []byte("other-cwd"), 0o644)
	before := map[string]struct{}{staleID: {}}
	got := captureAntigravityNativeID(cwd, before)
	if got != "" {
		t.Fatalf("capture must not adopt unrelated new DB; got %q", got)
	}
}

func TestCaptureAntigravityNativeID_AcceptsNewCwdMappedID(t *testing.T) {
	// Happy path with beforeIDs: last_conversations maps this cwd to an ID that
	// was not in the pre-turn snapshot.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	base := filepath.Join(tmp, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(filepath.Join(base, "cache"), 0o755)
	_ = os.MkdirAll(filepath.Join(base, "conversations"), 0o755)
	cwd := "/tmp/proj-capture"
	oldID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	newID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	_ = os.WriteFile(filepath.Join(base, "cache", "last_conversations.json"),
		[]byte(`{"`+cwd+`":"`+newID+`"}`), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", oldID+".db"), []byte("old"), 0o644)
	_ = os.WriteFile(filepath.Join(base, "conversations", newID+".db"), []byte("new"), 0o644)
	before := map[string]struct{}{oldID: {}}
	got := captureAntigravityNativeID(cwd, before)
	if got != newID {
		t.Fatalf("capture = %q, want cwd-mapped new ID %q", got, newID)
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

func TestCaptureAntigravityNativeID_NoCwdMappingDoesNotAdoptGlobalDB(t *testing.T) {
	// Without a last_conversations entry for this cwd, a new global DB must
	// not be adopted (it may belong to another workspace).
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
	if got != "" {
		t.Fatalf("capture without cwd mapping must be empty (got %q); do not adopt global DB %q", got, newID)
	}
}

func TestAntigravityNativeManager_StartSendIsolation(t *testing.T) {
	// Unit-level: two sessions with different IDs never share native IDs
	// when we set them independently (no real agy needed).
	m := NewAntigravityNativeManager()
	cwd := t.TempDir()
	if err := m.Start("sess-a", cwd, "ws", "uid", nil, nil); err != nil {
		// probeAntigravityNativeCapability may fail if agy missing — skip.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not runnable") ||
			strings.Contains(err.Error(), "below minimum") || strings.Contains(err.Error(), "unsupported") {
			t.Skipf("agy not available for integration-style unit test: %v", err)
		}
		t.Fatalf("start a: %v", err)
	}
	if err := m.Start("sess-b", cwd, "ws", "uid", nil, nil); err != nil {
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

func TestAntigravityNativeEnvelopePublishable_AcceptsNormalMessage(t *testing.T) {
	msg := resultMsg{
		ID:     "sess",
		UID:    "uid",
		Output: "hello from antigravity",
		Status: "success",
		Type:   "antigravity_native_message",
	}
	if err := antigravityNativeEnvelopePublishable(msg); err != nil {
		t.Fatalf("expected publishable envelope: %v", err)
	}
}

func TestAntigravityNativeEnvelopePublishable_RejectsJSONInflatedPayload(t *testing.T) {
	// Backslashes double under JSON string escaping. ~5.5 MiB of "\" becomes
	// >10 MiB marshaled while still under the raw 8 MiB stdout capture cap —
	// the failure mode Codex called out for silent Pub/Sub rejection.
	raw := strings.Repeat("\\", 5_500_000)
	if len(raw) > antigravityNativeMaxStdout {
		t.Fatalf("test payload must stay under stdout cap; got %d", len(raw))
	}
	msg := resultMsg{
		ID:     "sess",
		UID:    "uid",
		Output: raw,
		Status: "success",
		Type:   "antigravity_native_message",
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(encoded) <= antigravityNativeMaxPublishSize {
		t.Fatalf("test setup: expected marshaled size > %d, got %d", antigravityNativeMaxPublishSize, len(encoded))
	}
	if err := antigravityNativeEnvelopePublishable(msg); err == nil {
		t.Fatal("expected oversize envelope to be rejected")
	} else if !strings.Contains(err.Error(), "publishable limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSetActiveProcess_CancelsWhenAlreadyEnded(t *testing.T) {
	// End-before-registration race: session is already ended when the one-shot
	// process finally stores activeCancel. setActiveProcess must invoke cancel
	// immediately so tools cannot keep running after stop.
	s := &AntigravityNativeSession{status: "ended"}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "30", "127.0.0.1")
	} else {
		cmd = exec.Command("sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start long-running process: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	var cancelled atomic.Bool
	cancel := func() {
		cancelled.Store(true)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	s.setActiveProcess(cmd, cancel)
	if !cancelled.Load() {
		t.Fatal("expected cancel to run when session already ended")
	}

	// Process should exit promptly after kill.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatal("process still running after ended-session cancel")
	}
}

func TestSetActiveProcess_DoesNotCancelWhenRunning(t *testing.T) {
	s := &AntigravityNativeSession{status: "running"}
	var cancelled atomic.Bool
	s.setActiveProcess(nil, func() { cancelled.Store(true) })
	if cancelled.Load() {
		t.Fatal("cancel must not run for non-ended session")
	}
	if s.activeCancel == nil {
		t.Fatal("activeCancel should be stored")
	}
}

func TestBeginTurn_DoesNotReviveEndedSession(t *testing.T) {
	// End wins the race after Send's pre-check: beginTurn must refuse to
	// overwrite "ended" with "running", otherwise setActiveProcess/runOneShot
	// stop seeing the cancellation and an agy turn survives a Stop.
	ended := &AntigravityNativeSession{status: "ended"}
	if ended.beginTurn() {
		t.Fatal("beginTurn must return false for an ended session")
	}
	if got := ended.Status(); got != "ended" {
		t.Fatalf("ended status was overwritten to %q", got)
	}

	// Normal idle→running transition still works.
	idle := &AntigravityNativeSession{status: "idle"}
	if !idle.beginTurn() {
		t.Fatal("beginTurn must return true for an idle session")
	}
	if got := idle.Status(); got != "running" {
		t.Fatalf("expected running, got %q", got)
	}
}

func TestSanitizeAntigravityEnv_StripsCredentialsCaseInsensitive(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-upper",
		"OpenAI_API_KEY=sk-mixed",
		"anthropic_api_key=sk-lower",
		"xai_api_key=xai-lower",
		"CODEX_API_KEY=codex-upper",
		// OAuth / session tokens must be stripped too, not just *_API_KEY.
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-token",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"anthropic_auth_token=auth-lower",
		"CLAUDECODE=1",
		"SAFE_VAR=keep",
		"HOME=/tmp",
	}
	out := sanitizeAntigravityEnv(env)
	joined := strings.Join(out, "\n")
	for _, secret := range []string{
		"ANTHROPIC_API_KEY=",
		"OpenAI_API_KEY=",
		"anthropic_api_key=",
		"xai_api_key=",
		"CODEX_API_KEY=",
		"CLAUDE_CODE_OAUTH_TOKEN=",
		"ANTHROPIC_AUTH_TOKEN=",
		"anthropic_auth_token=",
		"CLAUDECODE=",
	} {
		for _, e := range out {
			if strings.HasPrefix(strings.ToUpper(e), strings.ToUpper(secret)) {
				t.Fatalf("credential %q must be stripped; got env %v", secret, out)
			}
		}
	}
	if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "SAFE_VAR=") || !strings.Contains(joined, "HOME=") {
		t.Fatalf("harmless vars must be preserved: %v", out)
	}
}

func TestAntigravityNativeManager_StartIsIdempotent(t *testing.T) {
	// Pub/Sub redelivery of antigravity_native_start must re-ack without
	// erroring (which would previously emit antigravity_native_ended and
	// desync cloud vs local session).
	m := NewAntigravityNativeManager()
	cwd := t.TempDir()

	var acks int
	onStarted := func() { acks++ }

	if err := m.Start("sess-idem", cwd, "ws", "uid", nil, onStarted); err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not runnable") ||
			strings.Contains(err.Error(), "below minimum") || strings.Contains(err.Error(), "unsupported") {
			t.Skipf("agy not available for capability probe: %v", err)
		}
		t.Fatalf("first start: %v", err)
	}
	if acks != 1 {
		t.Fatalf("expected 1 started ack after first start, got %d", acks)
	}
	if m.Get("sess-idem") == nil {
		t.Fatal("session missing after first start")
	}

	if err := m.Start("sess-idem", cwd, "ws", "uid", nil, onStarted); err != nil {
		t.Fatalf("duplicate start must be idempotent nil, got %v", err)
	}
	if acks != 2 {
		t.Fatalf("expected second started ack on redelivery, got %d", acks)
	}
	if m.Get("sess-idem") == nil {
		t.Fatal("session must still exist after idempotent re-start")
	}
	_ = m.End("sess-idem")
}

func TestAntigravityNativeManager_EndStaleSessionsPublishesEnded(t *testing.T) {
	// Idle GC must emit antigravity_native_ended so the cloud releases
	// reservations when antigravity_native_end never arrives (6h expiry /
	// dropped end command). Unlike Claude/Codex/Grok there is no long-lived
	// process whose exit path publishes ended.
	m := NewAntigravityNativeManager()
	cwd := t.TempDir()

	var captured []resultMsg
	publishFn := func(res resultMsg) {
		captured = append(captured, res)
	}

	// Bypass Start's capability probe: inject a session directly with an old
	// StartedAt so the test does not depend on an installed agy binary.
	id := "sess-stale-gc"
	m.mu.Lock()
	m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: "ws-stale",
		UID:         "uid-stale",
		StartedAt:   time.Now().Add(-7 * time.Hour),
		status:      "idle",
		publishFn:   publishFn,
	}
	m.mu.Unlock()

	m.endStaleSessions(6 * time.Hour)

	if m.Get(id) != nil {
		t.Fatal("stale session must be removed after GC")
	}
	if len(captured) != 1 {
		t.Fatalf("expected exactly one antigravity_native_ended frame, got %d: %#v", len(captured), captured)
	}
	got := captured[0]
	if got.Type != "antigravity_native_ended" {
		t.Fatalf("type=%q want antigravity_native_ended", got.Type)
	}
	if got.SessionID != id || got.ID != id {
		t.Fatalf("session identity mismatch: id=%q session=%q", got.ID, got.SessionID)
	}
	if got.WorkspaceID != "ws-stale" || got.UID != "uid-stale" {
		t.Fatalf("routing fields lost: workspace=%q uid=%q", got.WorkspaceID, got.UID)
	}
	if got.Status != "success" || got.ExitCode != 0 {
		t.Fatalf("status/exit: status=%q exit=%d", got.Status, got.ExitCode)
	}
	if !strings.Contains(got.Output, "expired") && !strings.Contains(got.Output, "stale") {
		t.Fatalf("ended output should mention expiry/stale: %q", got.Output)
	}

	// Fresh sessions must not be reaped or published.
	freshID := "sess-fresh"
	m.mu.Lock()
	m.sessions[freshID] = &AntigravityNativeSession{
		ID:          freshID,
		Cwd:         cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
		publishFn:   publishFn,
	}
	m.mu.Unlock()
	m.endStaleSessions(6 * time.Hour)
	if m.Get(freshID) == nil {
		t.Fatal("fresh session must not be reaped")
	}
	if len(captured) != 1 {
		t.Fatalf("fresh session must not emit ended; captured=%d", len(captured))
	}
	_ = m.End(freshID)
}

func TestAntigravityNativeManager_StartIdempotentSkipsCapabilityProbe(t *testing.T) {
	// Duplicate antigravity_native_start must re-ack even when the capability
	// cache is a recent failure (expired TTL / upgrade blip). Probing first
	// would return an error and desync cloud retry state from a live session.
	m := NewAntigravityNativeManager()
	cwd := t.TempDir()
	id := "sess-skip-probe"

	m.mu.Lock()
	m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
	}
	m.mu.Unlock()

	antigravityCapabilityMu.Lock()
	prevOK := antigravityCapabilityOK
	prevErr := antigravityCapabilityErr
	prevChecked := antigravityCapabilityChecked
	antigravityCapabilityOK = false
	antigravityCapabilityErr = fmt.Errorf("Antigravity CLI not found or not runnable: install agy ≥ %s", antigravityNativeMinVersion)
	antigravityCapabilityChecked = time.Now()
	antigravityCapabilityMu.Unlock()
	t.Cleanup(func() {
		antigravityCapabilityMu.Lock()
		antigravityCapabilityOK = prevOK
		antigravityCapabilityErr = prevErr
		antigravityCapabilityChecked = prevChecked
		antigravityCapabilityMu.Unlock()
	})

	var acks int
	if err := m.Start(id, cwd, "ws", "uid", nil, func() { acks++ }); err != nil {
		t.Fatalf("existing session must re-ack without capability probe: %v", err)
	}
	if acks != 1 {
		t.Fatalf("expected one started ack, got %d", acks)
	}
	if m.Get(id) == nil {
		t.Fatal("session must still be registered")
	}
	_ = m.End(id)
}

func TestAntigravityNativeManager_SendNonzeroExitWithStdoutIsError(t *testing.T) {
	// Auth/quota/invalid-flag failures often print diagnostics to stdout and
	// exit non-zero. Those must publish antigravity_native_error — never success
	// with the diagnostic appended as an assistant transcript turn.
	if runtime.GOOS == "windows" {
		t.Skip("shell shim for fake agy is unix-oriented")
	}

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, "agy")
	// Always fail with stdout diagnostic so PATH lookup of "agy" is deterministic.
	script := "#!/bin/sh\necho 'Error: authentication required — please run agy login'\nexit 2\n"
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := NewAntigravityNativeManager()
	cwd := t.TempDir()
	id := "sess-nonzero-stdout"

	// Bypass Start probe — inject session directly (same pattern as stale GC test).
	m.mu.Lock()
	m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
	}
	m.mu.Unlock()

	var frames []resultMsg
	publishFn := func(res resultMsg) {
		frames = append(frames, res)
	}

	err := m.Send(id, "hello", publishFn, 10*time.Second)
	if err == nil {
		t.Fatal("expected Send to fail on non-zero agy exit")
	}
	if !strings.Contains(err.Error(), "exited with code 2") {
		t.Fatalf("error should mention exit code: %v", err)
	}

	var sawError, sawSuccess bool
	for _, f := range frames {
		if f.Type == "antigravity_native_error" {
			sawError = true
		}
		if f.Type == "antigravity_native_message" && f.Status == "success" {
			sawSuccess = true
		}
	}
	if !sawError {
		t.Fatalf("expected antigravity_native_error frame, got %#v", frames)
	}
	if sawSuccess {
		t.Fatalf("must not publish success for non-zero exit: %#v", frames)
	}

	sess := m.Get(id)
	if sess == nil {
		t.Fatal("session should remain registered after failed turn")
	}
	if len(sess.Transcript) != 0 {
		t.Fatalf("failed turn must not append transcript, got %#v", sess.Transcript)
	}
	_ = m.End(id)
}

func TestAntigravityNativeManager_NativeIDNotAdoptedOnNonzeroExit(t *testing.T) {
	// A run that creates/updates the cwd conversation mapping and then exits
	// non-zero must NOT adopt the native conversation ID: the failed turn is
	// rejected without a transcript append, so resuming it later would replay a
	// hidden turn the UI never saw. Capture must run only after exitCode == 0.
	if runtime.GOOS == "windows" {
		t.Skip("shell shim for fake agy is unix-oriented")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(filepath.Join(base, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	newID := "cccccccc-cccc-cccc-cccc-cccccccccccc"

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, "agy")
	// Create the cwd->conversation mapping + DB (as a real first turn would), then
	// fail. Paths are baked in so the shim needs no env/pwd resolution.
	script := fmt.Sprintf("#!/bin/sh\n"+
		"printf '{\"%s\":\"%s\"}' > '%s'\n"+
		": > '%s'\n"+
		"echo 'Error: quota exceeded after creating conversation'\n"+
		"exit 2\n",
		cwd, newID,
		filepath.Join(base, "cache", "last_conversations.json"),
		filepath.Join(base, "conversations", newID+".db"))
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := NewAntigravityNativeManager()
	id := "sess-nonzero-capture"
	m.mu.Lock()
	m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
	}
	m.mu.Unlock()

	err := m.Send(id, "hello", func(resultMsg) {}, 10*time.Second)
	if err == nil {
		t.Fatal("expected Send to fail on non-zero agy exit")
	}

	sess := m.Get(id)
	if sess == nil {
		t.Fatal("session should remain registered after failed turn")
	}
	if sess.NativeConversationID != "" {
		t.Fatalf("must not adopt native conversation ID on non-zero exit; got %q", sess.NativeConversationID)
	}
	if len(sess.Transcript) != 0 {
		t.Fatalf("failed turn must not append transcript, got %#v", sess.Transcript)
	}
	_ = m.End(id)
}

func TestAntigravityNativeManager_TimeoutReapsProcessTree(t *testing.T) {
	// agy can spawn tool children that inherit the stdout/stderr pipes and outlive
	// it. If the timeout only kills agy, such a child keeps the pipe open and the
	// concurrent drain blocks long past the turn timeout (the unix orphan scanner
	// is a no-op). Starting agy as its own process-group leader and killing the
	// whole group on timeout must reap the child so the drain unblocks promptly.
	if runtime.GOOS == "windows" {
		t.Skip("process-group reaping is unix-specific; Windows uses job objects tracked separately")
	}

	binDir := t.TempDir()
	fakeAgy := filepath.Join(binDir, "agy")
	script := "#!/bin/sh\nsleep 60 &\necho started\nexit 0\n"
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := NewAntigravityNativeManager()
	cwd := t.TempDir()
	id := "sess-timeout-reap"
	m.mu.Lock()
	m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
	}
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- m.Send(id, "hello", func(resultMsg) {}, 1*time.Second)
	}()

	// turnTimeout (1s) + graceful kill wait (~3s) ≈ 4s. If the child were not
	// reaped, the drain would stay blocked on the inherited pipe for ~60s.
	select {
	case <-done:
		// Returned promptly — the process tree was reaped and the drain unblocked.
	case <-time.After(20 * time.Second):
		t.Fatal("Send blocked past the timeout — agy child was not reaped (drain stuck on inherited pipe)")
	}
	_ = m.End(id)
}
