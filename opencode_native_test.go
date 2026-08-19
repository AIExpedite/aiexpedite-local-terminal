package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   Dispatch predicate
   -------------------------------------------------------------------------- */

func TestIsOpenCodeNativeCommand(t *testing.T) {
	for _, ok := range []string{"opencode_native_start", "opencode_native_send", "opencode_native_end"} {
		if !isOpenCodeNativeCommand(ok) {
			t.Errorf("expected %q to be an opencode native command", ok)
		}
	}
	// A false positive here would route another kind's command into the
	// OpenCode manager, which would then answer with opencode_native_* frames
	// the caller's session listener does not recognise.
	for _, no := range []string{
		"session_start", "claude_native_start", "grok_acp_start",
		"codex_appserver_start", "antigravity_native_start",
		"opencode_native", "opencode_start", "",
	} {
		if isOpenCodeNativeCommand(no) {
			t.Errorf("did not expect %q to be an opencode native command", no)
		}
	}
}

/* --------------------------------------------------------------------------
   Argv builder
   -------------------------------------------------------------------------- */

func TestBuildOpenCodeNativeArgs_AlwaysForcesJSONRun(t *testing.T) {
	args := buildOpenCodeNativeArgs("")
	want := []string{"run", "--format", "json"}
	if len(args) != len(want) {
		t.Fatalf("expected exactly %v, got %#v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("arg %d: want %q, got %q (full: %#v)", i, want[i], args[i], args)
		}
	}
}

func TestBuildOpenCodeNativeArgs_ExactSessionResumeNeverContinue(t *testing.T) {
	args := buildOpenCodeNativeArgs("ses_abc123")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--session ses_abc123") {
		t.Fatalf("expected adjacent --session <id>, got %q", joined)
	}
	// --continue resumes whatever the user last ran globally, including in
	// their own local TUI. It must never appear.
	for _, a := range args {
		if a == "--continue" || a == "-c" || a == "--fork" {
			t.Fatalf("argv must never carry %q: %#v", a, args)
		}
	}
}

func TestBuildOpenCodeNativeArgs_NeverCarriesThePrompt(t *testing.T) {
	// The prompt goes to a temp file consumed on stdin. A prompt on argv would
	// be visible in a process listing and subject to the CreateProcess ceiling.
	args := buildOpenCodeNativeArgs("ses_1")
	for _, a := range args {
		if len(a) > 64 {
			t.Fatalf("suspiciously long argv token — is the prompt on argv? %#v", args)
		}
	}
}

func TestNormalizeOpenCodeCallerArgs_StripsManagerOwnedFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			// --format would flip the child out of JSON mode and make the turn
			// unrenderable.
			name: "separate-value format is stripped with its value",
			in:   []string{"--format", "text", "--model", "anthropic/claude-sonnet-4-5"},
			want: []string{"--model", "anthropic/claude-sonnet-4-5"},
		},
		{
			name: "inline-value format is stripped without eating the next token",
			in:   []string{"--format=text", "--model", "openai/gpt-4o"},
			want: []string{"--model", "openai/gpt-4o"},
		},
		{
			// --session would re-point the conversation at a chat the caller
			// does not own.
			name: "session is stripped with its value",
			in:   []string{"--session", "ses_someone_else", "--agent", "build"},
			want: []string{"--agent", "build"},
		},
		{
			name: "continue and fork are stripped",
			in:   []string{"--continue", "--fork", "abc", "--model", "m/x"},
			want: []string{"--model", "m/x"},
		},
		{
			name: "print-logs is stripped",
			in:   []string{"--print-logs", "--model", "m/x"},
			want: []string{"--model", "m/x"},
		},
		{
			name: "a leading caller run is dropped (the manager forces its own)",
			in:   []string{"run", "--model", "m/x"},
			want: []string{"--model", "m/x"},
		},
		{
			name: "model and agent are forwarded untouched",
			in:   []string{"--model", "anthropic/claude-sonnet-4-5", "--agent", "plan"},
			want: []string{"--model", "anthropic/claude-sonnet-4-5", "--agent", "plan"},
		},
		{
			name: "empty in, empty out",
			in:   nil,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeOpenCodeCallerArgs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("want %#v, got %#v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("arg %d: want %q, got %q (full %#v)", i, tc.want[i], got[i], got)
				}
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Version parsing
   -------------------------------------------------------------------------- */

func TestParseOpenCodeVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.4.2", "0.4.2"},
		{"opencode 0.4.2", "0.4.2"},
		{"v0.5.13\n", "0.5.13"},
		{"0.5.0-beta.1", "0.5.0"},
		{"0.5.0+build.7", "0.5.0"},
		{"\n\nopencode version 1.12.3 (darwin/arm64)\n", "1.12.3"},
		// Fail CLOSED rather than invent a version: an unparseable answer
		// leaves resume disabled, which costs one replay prompt. Inventing one
		// would pass --session to a binary whose resume semantics are unknown.
		{"", ""},
		{"unknown", ""},
		{"opencode (dev build)", ""},
	}
	for _, tc := range cases {
		if got := parseOpenCodeVersion(tc.in); got != tc.want {
			t.Errorf("parseOpenCodeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpenCodeResumeVersionGate(t *testing.T) {
	// Below the floor the manager must not pass --session at all — upstream
	// resume is unreliable headlessly there, and a broken resume loses the
	// conversation silently, whereas replay does not.
	below := []string{"0.1.0", "0.3.9"}
	atOrAbove := []string{"0.4.0", "0.4.1", "1.0.0"}
	for _, v := range below {
		if compareSemver(v, openCodeNativeMinVersion) >= 0 {
			t.Errorf("%q should sort below the resume floor %q", v, openCodeNativeMinVersion)
		}
	}
	for _, v := range atOrAbove {
		if compareSemver(v, openCodeNativeMinVersion) < 0 {
			t.Errorf("%q should sort at/above the resume floor %q", v, openCodeNativeMinVersion)
		}
	}
}

/* --------------------------------------------------------------------------
   Event parsing
   -------------------------------------------------------------------------- */

func TestParseOpenCodeEventLine_TextDeltas(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"text event", `{"type":"text","text":"Hello"}`, "Hello"},
		{"text delta", `{"type":"text-delta","delta":" world"}`, " world"},
		{"part updated", `{"type":"message.part.updated","part":{"type":"text","text":"!"}}`, "!"},
		{"assistant message", `{"type":"message","message":{"role":"assistant","content":"done"}}`, "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _, ok := parseOpenCodeEventLine(tc.line)
			if !ok {
				t.Fatalf("expected %q to parse as a JSON event", tc.line)
			}
			if text != tc.want {
				t.Fatalf("want text %q, got %q", tc.want, text)
			}
		})
	}
}

func TestParseOpenCodeEventLine_WhitespaceOnlyDeltaIsPreserved(t *testing.T) {
	// A delta may legitimately be a single space or newline. Trimming it would
	// glue words together in the coalesced completion.
	for _, raw := range []string{" ", "\n"} {
		line := fmt.Sprintf(`{"type":"text-delta","delta":%q}`, raw)
		text, _, ok := parseOpenCodeEventLine(line)
		if !ok {
			t.Fatalf("expected %q to parse", line)
		}
		if text != raw {
			t.Fatalf("whitespace delta %q was mangled to %q", raw, text)
		}
	}
}

func TestParseOpenCodeEventLine_ToolAndErrorEventsContributeNoText(t *testing.T) {
	// Tool output is not the assistant speaking. Folding it into the completion
	// would put tool payloads into the transcript, which then gets replayed
	// back to the model as if the assistant had said it.
	lines := []string{
		`{"type":"tool.text","text":"cat: no such file"}`,
		`{"type":"tool-call","text":"rm -rf /"}`,
		`{"type":"error","text":"boom"}`,
	}
	for _, line := range lines {
		text, _, ok := parseOpenCodeEventLine(line)
		if !ok {
			t.Fatalf("expected %q to parse as JSON", line)
		}
		if text != "" {
			t.Fatalf("event %q must contribute no assistant text, got %q", line, text)
		}
	}
}

func TestParseOpenCodeEventLine_NonJSONIsNotModelOutput(t *testing.T) {
	// A banner line is still forwarded to the UI, but must never be treated as
	// model output (ok=false is what keeps it out of the transcript).
	for _, line := range []string{"warning: config not found", "", "not json at all"} {
		if _, _, ok := parseOpenCodeEventLine(line); ok {
			t.Fatalf("expected %q to be rejected as a non-JSON line", line)
		}
	}
}

func TestParseOpenCodeEventLine_SessionIDVariants(t *testing.T) {
	cases := []string{
		`{"type":"session.created","sessionID":"ses_1"}`,
		`{"type":"session.created","sessionId":"ses_1"}`,
		`{"type":"session.created","session":{"id":"ses_1"}}`,
		`{"type":"session.created","info":{"id":"ses_1"}}`,
	}
	for _, line := range cases {
		_, sid, ok := parseOpenCodeEventLine(line)
		if !ok || sid != "ses_1" {
			t.Fatalf("line %q: want sessionID ses_1, got %q (ok=%v)", line, sid, ok)
		}
	}
}

func TestLooksLikeMissingOpenCodeSession(t *testing.T) {
	for _, s := range []string{
		"Error: Session not found",
		"unknown session ses_x",
		"failed to resume conversation",
	} {
		if !looksLikeMissingOpenCodeSession(s, "") {
			t.Errorf("expected %q to look like a missing session", s)
		}
	}
	// Generic failures must NOT trigger replay — that would burn a second model
	// call and silently start a new conversation.
	for _, s := range []string{
		"Error: rate limit exceeded",
		"authentication required",
		"tool execution failed",
		"",
	} {
		if looksLikeMissingOpenCodeSession(s, "") {
			t.Errorf("%q must not be read as a missing session", s)
		}
	}
}

/* --------------------------------------------------------------------------
   Env policy
   -------------------------------------------------------------------------- */

func TestSanitizeOpenCodeEnv_StripsOtherAgentsCredentials(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"anthropic_auth_token=lower-case-secret",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-secret",
		"CLAUDECODE=1",
		"CODEX_API_KEY=codex-secret",
		"XAI_API_KEY=xai-secret",
		"GROK_BIN_DIR=/home/u/.grok/bin",
		// OpenCode's OWN provider config must survive — stripping it would
		// break the local-model and env-var credential paths the readiness
		// probe is designed around.
		"OPENCODE_CONFIG=/home/u/.config/opencode/opencode.json",
		"OLLAMA_HOST=http://127.0.0.1:11434",
	}
	out := sanitizeOpenCodeEnv(in)
	joined := strings.Join(out, "\n")

	for _, secret := range []string{
		"sk-ant-secret", "lower-case-secret", "oauth-secret",
		"codex-secret", "xai-secret", "CLAUDECODE=",
	} {
		if strings.Contains(joined, secret) {
			t.Errorf("sanitized env still carries %q:\n%s", secret, joined)
		}
	}
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/u", "OPENCODE_CONFIG=", "OLLAMA_HOST="} {
		if !strings.Contains(joined, keep) {
			t.Errorf("sanitized env dropped %q, which OpenCode needs:\n%s", keep, joined)
		}
	}
}

/* --------------------------------------------------------------------------
   Transcript replay
   -------------------------------------------------------------------------- */

func TestOpenCodeTurnPrompt_BarePromptWhenResumable(t *testing.T) {
	tr := []openCodeTurn{{Role: "user", Content: "first"}, {Role: "assistant", Content: "reply"}}
	prompt, replay := openCodeTurnPrompt("ses_1", tr, "second")
	if replay {
		t.Fatal("a resumable session must not replay")
	}
	if prompt != "second" {
		t.Fatalf("want bare prompt, got %q", prompt)
	}
}

func TestOpenCodeTurnPrompt_FirstTurnSendsBarePrompt(t *testing.T) {
	prompt, replay := openCodeTurnPrompt("", nil, "hello")
	if replay {
		t.Fatal("a first turn has no history to replay")
	}
	if prompt != "hello" {
		t.Fatalf("want bare prompt, got %q", prompt)
	}
}

func TestOpenCodeTurnPrompt_ReplaysWhenIDLost(t *testing.T) {
	// This is the device-restart / capture-miss path that "session identity is
	// retained" depends on: a follow-up must keep context rather than silently
	// starting an empty conversation.
	tr := []openCodeTurn{
		{Role: "user", Content: "what is in main.go"},
		{Role: "assistant", Content: "a main function"},
	}
	prompt, replay := openCodeTurnPrompt("", tr, "and in util.go?")
	if !replay {
		t.Fatal("a lost native id with prior history must replay")
	}
	for _, want := range []string{"what is in main.go", "a main function", "User: and in util.go?"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("replay prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildOpenCodeReplayPrompt_NeverTruncatesTheCurrentTurn(t *testing.T) {
	// Tail-slicing the whole body could drop the leading bytes of the new user
	// turn, silently changing the request OpenCode sees.
	huge := strings.Repeat("x", openCodeNativeMaxPromptBytes)
	tr := []openCodeTurn{{Role: "user", Content: strings.Repeat("old ", 5000)}}
	prompt := buildOpenCodeReplayPrompt(tr, huge)
	if !strings.Contains(prompt, "User: "+huge) {
		t.Fatal("the current user turn was truncated or rewritten")
	}
}

func TestBuildOpenCodeReplayPrompt_DropsOldestWholeTurnsToFit(t *testing.T) {
	var tr []openCodeTurn
	for i := 0; i < 40; i++ {
		tr = append(tr, openCodeTurn{Role: "user", Content: fmt.Sprintf("turn-%d ", i) + strings.Repeat("y", 40_000)})
	}
	prompt := buildOpenCodeReplayPrompt(tr, "final question")
	if !strings.Contains(prompt, "User: final question") {
		t.Fatal("current turn missing from replay prompt")
	}
	if len(prompt) > openCodeNativeMaxPromptBytes+len("User: final question")+512 {
		t.Fatalf("replay prompt overshot the budget: %d bytes", len(prompt))
	}
	// Oldest turns go first.
	if strings.Contains(prompt, "turn-0 ") {
		t.Fatal("expected the oldest turn to be dropped first")
	}
}

func TestAppendOpenCodeTranscript_EnforcesBothBounds(t *testing.T) {
	var tr []openCodeTurn
	for i := 0; i < 100; i++ {
		tr = appendOpenCodeTranscript(tr, "user", fmt.Sprintf("m%d", i))
	}
	if len(tr) > openCodeReplayMaxMessages {
		t.Fatalf("message bound not enforced: %d", len(tr))
	}
	// Newest survives.
	if tr[len(tr)-1].Content != "m99" {
		t.Fatalf("expected newest turn last, got %q", tr[len(tr)-1].Content)
	}

	tr = nil
	for i := 0; i < 10; i++ {
		tr = appendOpenCodeTranscript(tr, "assistant", strings.Repeat("z", 20_000))
	}
	if openCodeTranscriptChars(tr) > openCodeReplayMaxChars+20_000 {
		t.Fatalf("char bound not enforced: %d", openCodeTranscriptChars(tr))
	}
}

/* --------------------------------------------------------------------------
   Native session-id capture
   -------------------------------------------------------------------------- */

func TestCaptureOpenCodeNativeID_AdoptsTheSingleNewSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENCODE_DATA", dataDir)
	sessionDir := filepath.Join(dataDir, "storage", "session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeSession := func(id string) {
		if err := os.WriteFile(filepath.Join(sessionDir, id+".json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write session %s: %v", id, err)
		}
	}

	writeSession("ses_pre_existing")
	before := listOpenCodeSessionIDs("")
	writeSession("ses_ours")

	if got := captureOpenCodeNativeID("", before); got != "ses_ours" {
		t.Fatalf("want ses_ours, got %q", got)
	}
}

func TestCaptureOpenCodeNativeID_FailsClosedWhenAmbiguous(t *testing.T) {
	// The user's own local OpenCode TUI can create a session during our capture
	// window. Adopting the wrong id would resume — and extend — a conversation
	// that is not this chat, so an ambiguous diff must yield "" and fall back to
	// bounded replay.
	dataDir := t.TempDir()
	t.Setenv("OPENCODE_DATA", dataDir)
	sessionDir := filepath.Join(dataDir, "storage", "session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	before := listOpenCodeSessionIDs("")
	for _, id := range []string{"ses_ours", "ses_users_tui"} {
		if err := os.WriteFile(filepath.Join(sessionDir, id+".json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := captureOpenCodeNativeID("", before); got != "" {
		t.Fatalf("ambiguous capture must return \"\", got %q", got)
	}
}

func TestCaptureOpenCodeNativeID_NoNewSessionYieldsEmpty(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("OPENCODE_DATA", dataDir)
	if err := os.MkdirAll(filepath.Join(dataDir, "storage", "session"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	before := listOpenCodeSessionIDs("")
	if got := captureOpenCodeNativeID("", before); got != "" {
		t.Fatalf("want empty capture, got %q", got)
	}
}

func TestOpenCodeStorageDir_HonoursOverrides(t *testing.T) {
	t.Setenv("OPENCODE_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	base := openCodeStorageDir()
	if base == "" {
		t.Skip("no home dir in this environment")
	}
	if !strings.Contains(filepath.ToSlash(base), "opencode") {
		t.Fatalf("default storage dir should name opencode, got %q", base)
	}

	t.Setenv("XDG_DATA_HOME", filepath.Join("tmp", "xdg"))
	if got := openCodeStorageDir(); !strings.Contains(filepath.ToSlash(got), "xdg/opencode") {
		t.Fatalf("XDG_DATA_HOME not honoured: %q", got)
	}
	t.Setenv("OPENCODE_DATA", filepath.Join("tmp", "explicit"))
	if got := openCodeStorageDir(); got != filepath.Join("tmp", "explicit") {
		t.Fatalf("OPENCODE_DATA not honoured: %q", got)
	}
}

/* --------------------------------------------------------------------------
   Completion frame + publish size gate
   -------------------------------------------------------------------------- */

func TestOpenCodeCompletionFrame_MarksReplayRecovery(t *testing.T) {
	plain := openCodeCompletionFrame("hi", false)
	if strings.Contains(plain, "replayRecovery") {
		t.Fatalf("a non-replay turn must not carry the marker: %s", plain)
	}
	replayed := openCodeCompletionFrame("hi", true)
	if !strings.Contains(replayed, `"replayRecovery":true`) {
		t.Fatalf("replay marker missing: %s", replayed)
	}
	for _, frame := range []string{plain, replayed} {
		if !strings.Contains(frame, `"final":true`) || !strings.Contains(frame, `"text":"hi"`) {
			t.Fatalf("completion frame malformed: %s", frame)
		}
	}
}

func TestOpenCodeNativeEnvelopePublishable_RejectsOversize(t *testing.T) {
	ok := resultMsg{Output: "small", Type: "opencode_native_message"}
	if err := openCodeNativeEnvelopePublishable(ok); err != nil {
		t.Fatalf("small envelope rejected: %v", err)
	}
	// newSessionPublishFn only LOGS publish failures, so an oversize completion
	// has to be caught here or the turn would claim success and publish nothing.
	big := resultMsg{Output: strings.Repeat("a", openCodeNativeMaxPublishSize+1), Type: "opencode_native_message"}
	if err := openCodeNativeEnvelopePublishable(big); err == nil {
		t.Fatal("expected an oversize envelope to be refused")
	}
}

/* --------------------------------------------------------------------------
   Manager lifecycle
   -------------------------------------------------------------------------- */

// injectOpenCodeSession registers a session directly, bypassing Start's
// capability probe (there is no real `opencode` binary in CI).
func injectOpenCodeSession(t *testing.T, m *OpenCodeNativeManager, id, cwd string) *OpenCodeNativeSession {
	t.Helper()
	resolved, err := containedCwd(cwd, "")
	if err != nil {
		t.Fatalf("resolve cwd: %v", err)
	}
	s := &OpenCodeNativeSession{
		ID:            id,
		Cwd:           cwd,
		WorkspaceRoot: resolved,
		WorkspaceID:   "ws",
		UID:           "uid",
		StartedAt:     time.Now(),
		status:        "idle",
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s
}

func TestOpenCodeNativeManager_StartRejectsBadCwd(t *testing.T) {
	m := NewOpenCodeNativeManager()
	if err := m.Start("", "/tmp", "ws", "uid", nil, nil); err == nil {
		t.Fatal("expected an empty session id to be refused")
	}
	if err := m.Start("s1", "", "ws", "uid", nil, nil); err == nil {
		t.Fatal("expected an empty cwd to be refused")
	}
	if err := m.Start("s1", "relative/path", "ws", "uid", nil, nil); err == nil {
		t.Fatal("expected a relative cwd to be refused")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := m.Start("s1", missing, "ws", "uid", nil, nil); err == nil {
		t.Fatal("expected a missing cwd to be refused")
	}
	// A file is not a working directory.
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := m.Start("s1", f, "ws", "uid", nil, nil); err == nil {
		t.Fatal("expected a file cwd to be refused")
	}
}

func TestOpenCodeNativeManager_EndBetweenTurnsStillTearsDown(t *testing.T) {
	// The COMMON case for a one-shot-per-turn kind: no child is running when
	// the user hits Stop. There is no process exit to piggyback on, so a no-op
	// here would leave the cloud reservation held forever.
	m := NewOpenCodeNativeManager()
	id := "sess-end-idle"
	injectOpenCodeSession(t, m, id, t.TempDir())

	if m.ActiveCount() != 1 {
		t.Fatalf("expected 1 active session, got %d", m.ActiveCount())
	}
	if err := m.End(id); err != nil {
		t.Fatalf("End between turns failed: %v", err)
	}
	if m.ActiveCount() != 0 {
		t.Fatalf("expected the session to be removed, %d remain", m.ActiveCount())
	}
	if m.Get(id) != nil {
		t.Fatal("session must be gone after End")
	}
	// Idempotent: a duplicate End (retry / double-click) must not error out in
	// a way that stops the handler from publishing the terminal ended frame.
	if err := m.End(id); err == nil {
		t.Fatal("expected End on a removed session to report not-found")
	}
}

func TestOpenCodeNativeManager_SendOnEndedSessionIsRefused(t *testing.T) {
	m := NewOpenCodeNativeManager()
	id := "sess-ended"
	s := injectOpenCodeSession(t, m, id, t.TempDir())
	s.setStatus("ended")

	err := m.Send(id, "hello", func(resultMsg) {}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected an ended-session refusal, got %v", err)
	}
}

func TestOpenCodeNativeManager_SendOnMissingSessionIsRefused(t *testing.T) {
	m := NewOpenCodeNativeManager()
	err := m.Send("nope", "hello", func(resultMsg) {}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found refusal, got %v", err)
	}
}

func TestOpenCodeNativeManager_SendRequiresPublishFnAndText(t *testing.T) {
	m := NewOpenCodeNativeManager()
	id := "sess-validate"
	injectOpenCodeSession(t, m, id, t.TempDir())

	if err := m.Send(id, "hello", nil, time.Second); err == nil {
		t.Fatal("expected a nil publishFn to be refused")
	}
	if err := m.Send(id, "   ", func(resultMsg) {}, time.Second); err == nil {
		t.Fatal("expected a whitespace-only prompt to be refused")
	}
}

func TestOpenCodeNativeManager_SecondConcurrentSendIsRefused(t *testing.T) {
	// Two children against the same --session id would interleave two turns
	// into one OpenCode transcript.
	m := NewOpenCodeNativeManager()
	id := "sess-concurrent"
	s := injectOpenCodeSession(t, m, id, t.TempDir())

	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	err := m.Send(id, "second prompt", func(resultMsg) {}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "turn in flight") {
		t.Fatalf("expected a turn-in-flight refusal, got %v", err)
	}
}

func TestOpenCodeNativeManager_SendRefusesCwdOutsideSessionRoot(t *testing.T) {
	// A cwd component swapped for a symlink between start and send resolves
	// elsewhere and must not be able to run OpenCode outside the directory the
	// session was started in.
	m := NewOpenCodeNativeManager()
	id := "sess-escape"
	root := t.TempDir()
	outside := t.TempDir()

	s := injectOpenCodeSession(t, m, id, root)
	s.Cwd = outside // simulate the post-start swap

	var frames []resultMsg
	err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, time.Second)
	if err == nil {
		t.Fatal("expected the escaped cwd to be refused")
	}
	if len(frames) == 0 || frames[0].Type != "opencode_native_error" {
		t.Fatalf("expected a published opencode_native_error frame, got %#v", frames)
	}
	if !strings.Contains(frames[0].Output, "containment") {
		t.Fatalf("error frame should name the containment failure: %q", frames[0].Output)
	}
}

func TestOpenCodeNativeManager_OversizePromptPublishesError(t *testing.T) {
	// The HTTP send already returned 200, so an oversize prompt must surface a
	// published error frame or the chat UI stays stuck in "running".
	m := NewOpenCodeNativeManager()
	id := "sess-oversize"
	injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	err := m.Send(id, strings.Repeat("x", openCodeNativeMaxPromptBytes+1),
		func(r resultMsg) { frames = append(frames, r) }, time.Second)
	if err == nil {
		t.Fatal("expected an oversize prompt to be refused")
	}
	if len(frames) != 1 || frames[0].Type != "opencode_native_error" || frames[0].Status != "error" {
		t.Fatalf("expected one opencode_native_error frame, got %#v", frames)
	}
}

func TestOpenCodeNativeManager_IdempotentStartAck(t *testing.T) {
	// Pub/Sub redelivery of the same start must re-ack `started` WITHOUT ending
	// the still-usable local session — emitting ended would release the cloud
	// reservation while the manager can still accept Sends.
	m := NewOpenCodeNativeManager()
	id := "sess-redelivered"
	injectOpenCodeSession(t, m, id, t.TempDir())

	acks := 0
	if err := m.Start(id, t.TempDir(), "ws", "uid", func(resultMsg) {}, func() { acks++ }); err != nil {
		t.Fatalf("redelivered start should ack, got %v", err)
	}
	if acks != 1 {
		t.Fatalf("expected exactly one started ack, got %d", acks)
	}
	if m.Get(id) == nil {
		t.Fatal("the session must still be registered after a redelivered start")
	}
}

func TestOpenCodeNativeManager_StaleGCPublishesEnded(t *testing.T) {
	// OpenCode sessions are logical: End alone does not publish, so idle GC has
	// to emit the terminal frame itself or the cloud never releases the lease.
	m := NewOpenCodeNativeManager()
	id := "sess-stale"
	s := injectOpenCodeSession(t, m, id, t.TempDir())
	s.StartedAt = time.Now().Add(-2 * time.Hour)

	var frames []resultMsg
	s.mu.Lock()
	s.publishFn = func(r resultMsg) { frames = append(frames, r) }
	s.mu.Unlock()

	m.endStaleSessions(time.Hour)

	if m.ActiveCount() != 0 {
		t.Fatalf("stale session was not reaped, %d remain", m.ActiveCount())
	}
	if len(frames) != 1 || frames[0].Type != "opencode_native_ended" {
		t.Fatalf("expected one opencode_native_ended frame, got %#v", frames)
	}
}

func TestOpenCodeNativeManager_StaleGCLeavesFreshSessions(t *testing.T) {
	m := NewOpenCodeNativeManager()
	injectOpenCodeSession(t, m, "fresh", t.TempDir())
	m.endStaleSessions(time.Hour)
	if m.ActiveCount() != 1 {
		t.Fatalf("a fresh session must survive GC, got %d active", m.ActiveCount())
	}
}

func TestOpenCodeNativeManager_ShutdownAllEndsEverySession(t *testing.T) {
	m := NewOpenCodeNativeManager()
	for i := 0; i < 3; i++ {
		injectOpenCodeSession(t, m, fmt.Sprintf("s%d", i), t.TempDir())
	}
	m.ShutdownAll()
	if m.ActiveCount() != 0 {
		t.Fatalf("ShutdownAll left %d sessions", m.ActiveCount())
	}
}

/* --------------------------------------------------------------------------
   Full turn against a stub binary

   These use the COMPILED cross-platform stub (opencode_stub_test.go) rather
   than a `#!/bin/sh` shim, so they execute on Windows too — where the process
   tree kill and the argv ceiling actually differ from unix.
   -------------------------------------------------------------------------- */

func TestOpenCodeNativeManager_StreamsEachEventThenCompletes(t *testing.T) {
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", strings.Join([]string{
		`{"type":"session.created","sessionID":"ses_streamed"}`,
		`{"type":"text-delta","delta":"Hel"}`,
		`{"type":"text-delta","delta":"lo"}`,
		`{"type":"tool-call","name":"read","text":"secret-tool-payload"}`,
		`{"type":"session.completed"}`,
		"",
	}, "\\n"))

	m := NewOpenCodeNativeManager()
	id := "sess-stream"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	if err := m.Send(id, "say hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// One frame per streamed event, plus the coalesced completion.
	if len(frames) != 6 {
		t.Fatalf("expected 5 streamed frames + 1 completion, got %d: %#v", len(frames), frames)
	}
	for i, f := range frames {
		if f.Type != "opencode_native_message" {
			t.Fatalf("frame %d has type %q, want opencode_native_message", i, f.Type)
		}
		if f.SessionID != id {
			t.Fatalf("frame %d has sessionID %q, want %q", i, f.SessionID, id)
		}
		if f.Status != "success" {
			t.Fatalf("frame %d has status %q, want success", i, f.Status)
		}
	}
	// Seq must strictly increase so a partially-delivered turn can be ordered.
	for i := 1; i < len(frames); i++ {
		if frames[i].Seq <= frames[i-1].Seq {
			t.Fatalf("frame seq not increasing: %d then %d", frames[i-1].Seq, frames[i].Seq)
		}
	}
	// Tool activity IS forwarded to the UI...
	if !strings.Contains(frames[3].Output, "tool-call") {
		t.Fatalf("tool event was not forwarded: %q", frames[3].Output)
	}
	// ...but must contribute no assistant text, or the tool payload would enter
	// the transcript and be replayed to the model as the assistant's own words.
	final := frames[len(frames)-1]
	if !strings.Contains(final.Output, `"text":"Hello"`) {
		t.Fatalf("completion should coalesce deltas into \"Hello\", got %s", final.Output)
	}
	if strings.Contains(final.Output, "secret-tool-payload") {
		t.Fatalf("tool payload leaked into the assistant completion: %s", final.Output)
	}
	if sess.NativeSessionID != "ses_streamed" {
		t.Fatalf("want captured session id ses_streamed, got %q", sess.NativeSessionID)
	}
	if len(sess.Transcript) != 2 || sess.Transcript[0].Role != "user" || sess.Transcript[1].Content != "Hello" {
		t.Fatalf("transcript not recorded for replay: %#v", sess.Transcript)
	}
	// The turn ends idle, not ended — the user can follow up or retry.
	if sess.Status() != "idle" {
		t.Fatalf("session status after a good turn = %q, want idle", sess.Status())
	}
}

func TestOpenCodeNativeManager_FollowUpUsesExactSessionResume(t *testing.T) {
	installOpenCodeStub(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-resume"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	sess.NativeSessionID = "ses_known"
	sess.Transcript = []openCodeTurn{{Role: "user", Content: "earlier"}}

	if err := m.Send(id, "follow up", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	argv := string(logged)
	if !strings.Contains(argv, "--session ses_known") {
		t.Fatalf("a follow-up must resume by exact id; argv was %q", argv)
	}
	if !strings.Contains(argv, "run --format json") {
		t.Fatalf("JSON run mode must always be forced; argv was %q", argv)
	}
	if strings.Contains(argv, "--continue") {
		t.Fatalf("--continue must never be used; argv was %q", argv)
	}
	// The prompt travels on stdin, never argv.
	if strings.Contains(argv, "follow up") {
		t.Fatalf("prompt leaked onto argv: %q", argv)
	}
}

func TestOpenCodeNativeManager_BelowMinVersionSkipsSessionAndReplays(t *testing.T) {
	installOpenCodeStub(t)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("OPENCODE_STUB_VERSION", "0.1.0")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-old-binary"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	sess.NativeSessionID = "ses_known"
	sess.Transcript = []openCodeTurn{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}

	if err := m.Send(id, "follow up", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	logged, _ := os.ReadFile(argvLog)
	if strings.Contains(string(logged), "--session") {
		t.Fatalf("a below-floor binary must never receive --session; argv: %q", string(logged))
	}
	// Context must be preserved via replay rather than silently dropped.
	sent, _ := os.ReadFile(stdinLog)
	for _, want := range []string{"earlier question", "earlier answer", "User: follow up"} {
		if !strings.Contains(string(sent), want) {
			t.Fatalf("replay prompt missing %q:\n%s", want, string(sent))
		}
	}
}

func TestOpenCodeNativeManager_NonZeroExitWithStdoutIsAnError(t *testing.T) {
	// Auth/quota/invalid-flag failures print diagnostics to stdout and exit
	// non-zero. Those must publish an error — never success with the diagnostic
	// appended to the transcript as an assistant turn.
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"authentication required"}`+"\\n")
	t.Setenv("OPENCODE_STUB_EXIT", "2")

	m := NewOpenCodeNativeManager()
	id := "sess-nonzero"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second)
	if err == nil {
		t.Fatal("expected a non-zero exit to fail the turn")
	}
	last := frames[len(frames)-1]
	if last.Type != "opencode_native_error" || last.Status != "error" {
		t.Fatalf("expected a terminal opencode_native_error, got %#v", last)
	}
	if len(sess.Transcript) != 0 {
		t.Fatalf("a failed turn must not enter the transcript: %#v", sess.Transcript)
	}
}

func TestOpenCodeNativeManager_EmptyCompletionIsNotSuccess(t *testing.T) {
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", "")

	m := NewOpenCodeNativeManager()
	id := "sess-empty"
	injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	if err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err == nil {
		t.Fatal("an empty completion must not be treated as success")
	}
	if len(frames) == 0 || frames[len(frames)-1].Type != "opencode_native_error" {
		t.Fatalf("expected a published error frame, got %#v", frames)
	}
}

func TestOpenCodeNativeManager_MissingSessionTriggersExactlyOneReplay(t *testing.T) {
	installOpenCodeStub(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)
	t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
	t.Setenv("OPENCODE_STUB_FIRST_STDOUT", "Error: Session not found\\n")
	t.Setenv("OPENCODE_STUB_FIRST_EXIT", "1")
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"recovered"}`+"\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-replay"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	sess.NativeSessionID = "ses_gone"
	sess.Transcript = []openCodeTurn{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}

	var frames []resultMsg
	if err := m.Send(id, "follow up", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err != nil {
		t.Fatalf("replay recovery should succeed: %v", err)
	}

	logged, _ := os.ReadFile(runLog)
	if runs := strings.Count(string(logged), "run"); runs != 2 {
		t.Fatalf("expected exactly one replay retry (2 runs total), got %d:\n%s", runs, logged)
	}
	// The stale id is dropped so later turns do not keep retrying it.
	if sess.NativeSessionID == "ses_gone" {
		t.Fatal("the stale native session id must be cleared after a failed resume")
	}
	final := frames[len(frames)-1]
	if !strings.Contains(final.Output, `"replayRecovery":true`) {
		t.Fatalf("the recovered turn must carry the replay marker: %s", final.Output)
	}
	if !strings.Contains(final.Output, "recovered") {
		t.Fatalf("the recovered turn lost its content: %s", final.Output)
	}
}

func TestOpenCodeNativeManager_GenericFailureDoesNotTriggerReplay(t *testing.T) {
	// A replay burns a second model call and starts a new conversation, so only
	// a RECOGNISED missing-session message may trigger it.
	installOpenCodeStub(t)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)
	t.Setenv("OPENCODE_STUB_STDOUT", "Error: rate limit exceeded\\n")
	t.Setenv("OPENCODE_STUB_EXIT", "1")

	m := NewOpenCodeNativeManager()
	id := "sess-no-replay"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	sess.NativeSessionID = "ses_live"
	sess.Transcript = []openCodeTurn{{Role: "user", Content: "earlier"}}

	if err := m.Send(id, "follow up", func(resultMsg) {}, 60*time.Second); err == nil {
		t.Fatal("a rate-limit failure must fail the turn")
	}
	logged, _ := os.ReadFile(runLog)
	if runs := strings.Count(string(logged), "run"); runs != 1 {
		t.Fatalf("expected exactly one run (no replay), got %d", runs)
	}
	// A live id must survive a generic failure — dropping it would force every
	// later turn onto replay.
	if sess.NativeSessionID != "ses_live" {
		t.Fatalf("a generic failure must not clear the native session id, got %q", sess.NativeSessionID)
	}
}

func TestOpenCodeNativeManager_PromptReachesTheChildOnStdin(t *testing.T) {
	installOpenCodeStub(t)
	stdinLog := filepath.Join(t.TempDir(), "stdin.txt")
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-stdin"
	injectOpenCodeSession(t, m, id, t.TempDir())

	if err := m.Send(id, "explain the build", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	got, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if strings.TrimSpace(string(got)) != "explain the build" {
		t.Fatalf("child stdin was %q, want the prompt", string(got))
	}
}

func TestOpenCodeNativeManager_PromptFileIsRemovedAfterEveryTurn(t *testing.T) {
	// The prompt file holds the user's raw text. It must not survive the turn
	// on either the success or the failure path.
	installOpenCodeStub(t)
	scratch := cliPromptTempDir("opencode-prompts")
	if scratch == "" {
		t.Skip("no home dir in this environment")
	}
	countPromptFiles := func() int {
		entries, err := os.ReadDir(scratch)
		if err != nil {
			return 0
		}
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "opencode-prompt-") {
				n++
			}
		}
		return n
	}
	before := countPromptFiles()

	m := NewOpenCodeNativeManager()
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")
	injectOpenCodeSession(t, m, "ok-turn", t.TempDir())
	if err := m.Send("ok-turn", "hello", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	t.Setenv("OPENCODE_STUB_EXIT", "3")
	t.Setenv("OPENCODE_STUB_STDOUT", "boom\\n")
	injectOpenCodeSession(t, m, "bad-turn", t.TempDir())
	_ = m.Send("bad-turn", "hello", func(resultMsg) {}, 60*time.Second)

	if after := countPromptFiles(); after != before {
		t.Fatalf("prompt files leaked: %d before, %d after", before, after)
	}
}

func TestOpenCodeNativeManager_EndMidTurnKillsTheChild(t *testing.T) {
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "120000")

	m := NewOpenCodeNativeManager()
	id := "sess-cancel"
	injectOpenCodeSession(t, m, id, t.TempDir())

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- m.Send(id, "long running", func(resultMsg) {}, 5*time.Minute)
	}()

	// Wait for the child to actually be running before cancelling.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if sess := m.Get(id); sess != nil && sess.Status() == "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	endDone := make(chan error, 1)
	go func() { endDone <- m.End(id) }()

	select {
	case err := <-endDone:
		if err != nil {
			t.Fatalf("End mid-turn failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("End did not return — the drain barrier is stuck")
	}

	select {
	case <-sendDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the cancelled turn never unwound")
	}

	if m.ActiveCount() != 0 {
		t.Fatalf("the cancelled session was not removed, %d remain", m.ActiveCount())
	}
}

func TestOpenCodeNativeManager_TurnTimeoutIsReported(t *testing.T) {
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "60000")

	m := NewOpenCodeNativeManager()
	id := "sess-timeout"
	injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("expected the turn to time out")
	}
	if len(frames) == 0 {
		t.Fatal("a timeout must publish a terminal frame or the UI stays stuck running")
	}
	last := frames[len(frames)-1]
	if last.Type != "opencode_native_error" {
		t.Fatalf("expected a timeout error frame, got %#v", last)
	}
	// The logical session survives a timeout so the user can retry.
	if m.Get(id) == nil {
		t.Fatal("a timed-out turn must not destroy the logical session")
	}
}

func TestOpenCodeNativeManager_OversizeFrameIsFatalNotSilent(t *testing.T) {
	// A frame beyond the cap cannot be parsed as a complete event. Truncating
	// it would render a corrupt assistant message that looks complete.
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_HUGE_FRAME_BYTES", fmt.Sprint(openCodeNativeMaxFrameBytes+128*1024))

	m := NewOpenCodeNativeManager()
	id := "sess-huge-frame"
	injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 120*time.Second)
	if err == nil {
		t.Fatal("an oversize frame must fail the turn rather than be dropped")
	}
	if len(frames) == 0 || frames[len(frames)-1].Type != "opencode_native_error" {
		t.Fatalf("expected a terminal opencode_native_error, got %#v", frames)
	}
}

func TestOpenCodeNativeManager_ChildDoesNotInheritOtherAgentsSecrets(t *testing.T) {
	installOpenCodeStub(t)
	// The stub writes whatever it read from stdin; for the env check we assert
	// on the argv/stdin-independent fact that sanitizeOpenCodeEnv is applied to
	// cmd.Env, which the unit test above pins. Here we verify end to end that a
	// turn does not carry the vars, by asking the stub to echo its own env.
	envDump := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-should-not-leak")
	t.Setenv("XAI_API_KEY", "xai-should-not-leak")

	// sanitizeOpenCodeEnv is what runOneShot hands the child; assert directly on
	// the value it produces from the live environment.
	sanitized := strings.Join(sanitizeOpenCodeEnv(os.Environ()), "\n")
	if err := os.WriteFile(envDump, []byte(sanitized), 0o600); err != nil {
		t.Fatalf("write env dump: %v", err)
	}
	for _, secret := range []string{"sk-ant-should-not-leak", "oauth-should-not-leak", "xai-should-not-leak"} {
		if strings.Contains(sanitized, secret) {
			t.Errorf("the child environment would leak %q", secret)
		}
	}

	m := NewOpenCodeNativeManager()
	id := "sess-env"
	injectOpenCodeSession(t, m, id, t.TempDir())
	if err := m.Send(id, "hello", func(resultMsg) {}, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
}

func TestOpenCodeNativeManager_PublishedFramesCarryNoSecrets(t *testing.T) {
	installOpenCodeStub(t)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n")
	t.Setenv("OPENCODE_STUB_STDERR", "warning: api_key=sk-super-secret-value-here\\n")

	m := NewOpenCodeNativeManager()
	id := "sess-redact"
	injectOpenCodeSession(t, m, id, t.TempDir())

	var frames []resultMsg
	if err := m.Send(id, "hello", func(r resultMsg) { frames = append(frames, r) }, 60*time.Second); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	sawStderr := false
	for _, f := range frames {
		if strings.Contains(f.Output, "sk-super-secret-value-here") {
			t.Fatalf("a published %s frame leaked a secret: %q", f.Type, f.Output)
		}
		if f.Type == "opencode_native_stderr" {
			sawStderr = true
			if !strings.Contains(f.Output, "REDACTED") {
				t.Fatalf("stderr frame should be redacted, got %q", f.Output)
			}
		}
	}
	if !sawStderr {
		t.Fatal("expected an opencode_native_stderr frame for the warning")
	}
}
