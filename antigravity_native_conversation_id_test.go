package main

// The Antigravity conversation id read from agy's own stream-json events
// (coding-agent parity plan, W2.2).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockAgyConversationIDMode is a stub `agy` that reports a conversation on its
// own stream-json events, as agy 1.5.8 does: the `init` event's top-level
// `conversation_id` and the `result`'s. It can also leave behind what the
// last_conversations.json scrape reads, its own or another agy's.
const (
	mockAgyConversationIDMode = "antigravity-stream-conversation-id"
	// The id the run reports; "prompt" reports the prompt text itself, so
	// concurrent runs each report their own. Empty: no event names one.
	mockAgyReportIDEnv = "TEST_MOCK_AGY_REPORT_ID"
	// Map last_conversations.json[TEST_MOCK_AGY_SCRAPE_CWD] to this id and
	// create its conversation DB: what the newest agy run in the folder
	// leaves for the scrape.
	mockAgyScrapeIDEnv  = "TEST_MOCK_AGY_SCRAPE_ID"
	mockAgyScrapeCwdEnv = "TEST_MOCK_AGY_SCRAPE_CWD"
	// A delegated subagent's conversation, carried on a step_update.
	mockAgySubagentConversationID = "99999999-9999-4999-8999-999999999999"
)

func runMockAgyConversationID() {
	data, _ := io.ReadAll(os.Stdin)
	var input struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &input); err != nil || input.Event != "user" {
		fmt.Println(`{"event":"result","result":{"status":"ERROR","error":"invalid stdin envelope"}}`)
		os.Exit(1)
	}
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	createConversation := func(id string) {
		_ = os.MkdirAll(filepath.Join(base, "conversations"), 0o755)
		_ = os.WriteFile(filepath.Join(base, "conversations", id+".db"), nil, 0o600)
	}
	report := os.Getenv(mockAgyReportIDEnv)
	if report == "prompt" {
		report = strings.TrimSpace(input.Message.Content)
	}
	if report != "" {
		createConversation(report)
	}
	if id := os.Getenv(mockAgyScrapeIDEnv); id != "" {
		createConversation(id)
		mapping, _ := json.Marshal(map[string]string{os.Getenv(mockAgyScrapeCwdEnv): id})
		_ = os.MkdirAll(filepath.Join(base, "cache"), 0o755)
		_ = os.WriteFile(filepath.Join(base, "cache", "last_conversations.json"), mapping, 0o600)
	}
	if report != "" {
		fmt.Printf(`{"event":"init","conversation_id":%q,"init":{"cwd":"x","tools":[],"permission_mode":"default"}}`+"\n", report)
	} else {
		fmt.Println(`{"event":"init","init":{"cwd":"x","tools":[],"permission_mode":"default"}}`)
	}
	fmt.Printf(`{"event":"step_update","step_update":{"conversation_id":%q,"step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"ok"}}`+"\n", mockAgySubagentConversationID)
	if report != "" {
		fmt.Printf(`{"event":"result","result":{"conversation_id":%q,"status":"SUCCESS","response":"ok","num_turns":1}}`+"\n", report)
	} else {
		fmt.Println(`{"event":"result","result":{"status":"SUCCESS","response":"ok","num_turns":1}}`)
	}
	os.Exit(0)
}

// antigravityConversationIDHarness puts the stub on PATH with a private agy
// home, and registers idle sessions in one folder.
type antigravityConversationIDHarness struct {
	t   *testing.T
	m   *AntigravityNativeManager
	cwd string
}

func newAntigravityConversationIDHarness(t *testing.T) *antigravityConversationIDHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	helperMockAgyOnPath(t, mockAgyConversationIDMode)
	cwd := t.TempDir()
	// The scrape keys the folder by its symlink-free path (containedCwd).
	runDir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(mockAgyScrapeCwdEnv, runDir)
	return &antigravityConversationIDHarness{t: t, m: NewAntigravityNativeManager(nil), cwd: cwd}
}

func (h *antigravityConversationIDHarness) addSession(id string) {
	h.m.mu.Lock()
	h.m.sessions[id] = &AntigravityNativeSession{
		ID:          id,
		Cwd:         h.cwd,
		WorkspaceID: "ws",
		UID:         "uid",
		StartedAt:   time.Now(),
		status:      "idle",
	}
	h.m.mu.Unlock()
}

// send runs one turn and returns the conversation id its success frame carried.
func (h *antigravityConversationIDHarness) send(id, prompt string) string {
	h.t.Helper()
	var mu sync.Mutex
	published := ""
	err := h.m.Send(id, prompt, func(res resultMsg) {
		if res.Type == "antigravity_native_message" && res.Status == "success" {
			mu.Lock()
			published = res.ConversationID
			mu.Unlock()
		}
	}, 30*time.Second)
	if err != nil {
		h.t.Errorf("send on %s: %v", id, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return published
}

const (
	agyOwnConversationID     = "11111111-1111-4111-8111-111111111111"
	agyForeignConversationID = "22222222-2222-4222-8222-222222222222"
)

func TestAntigravityNativeSend_RecordsTheConversationIDTheStreamReports(t *testing.T) {
	h := newAntigravityConversationIDHarness(t)
	t.Setenv(mockAgyReportIDEnv, agyOwnConversationID)
	h.addSession("s1")

	if got := h.send("s1", "hello"); got != agyOwnConversationID {
		t.Fatalf("the success frame must carry the id the run reported, got %q", got)
	}
	if got := h.m.Get("s1").NativeConversationID; got != agyOwnConversationID {
		t.Fatalf("the session must resume the reported conversation, got %q", got)
	}
}

// Another agy in the same folder during the turn (the person's own TUI)
// leaves a second new conversation DB and points the folder at ITS
// conversation: the scrape fails closed, and the session could never be
// reconnected. The id the run reported is still its own.
func TestAntigravityNativeSend_ConcurrentAgyInTheFolderDoesNotDefeatTheCapture(t *testing.T) {
	h := newAntigravityConversationIDHarness(t)
	t.Setenv(mockAgyReportIDEnv, agyOwnConversationID)
	t.Setenv(mockAgyScrapeIDEnv, agyForeignConversationID)
	h.addSession("s1")

	if got := h.send("s1", "hello"); got != agyOwnConversationID {
		t.Fatalf("the reported id must win over a concurrent agy's, got %q", got)
	}
}

func TestAntigravityNativeSend_ConcurrentTurnsInOneFolderEachRecordTheirOwnID(t *testing.T) {
	h := newAntigravityConversationIDHarness(t)
	t.Setenv(mockAgyReportIDEnv, "prompt")
	ids := map[string]string{
		"s1": agyOwnConversationID,
		"s2": agyForeignConversationID,
	}
	for sid := range ids {
		h.addSession(sid)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := make(map[string]string)
	for sid, conv := range ids {
		wg.Add(1)
		go func(sid, conv string) {
			defer wg.Done()
			published := h.send(sid, conv)
			mu.Lock()
			got[sid] = published
			mu.Unlock()
		}(sid, conv)
	}
	wg.Wait()
	for sid, conv := range ids {
		if got[sid] != conv || h.m.Get(sid).NativeConversationID != conv {
			t.Fatalf("%s must record its own conversation %q; published %q, session %q",
				sid, conv, got[sid], h.m.Get(sid).NativeConversationID)
		}
	}
}

func TestAntigravityNativeSend_FallsBackToTheScrapeWhenNoEventNamesTheConversation(t *testing.T) {
	h := newAntigravityConversationIDHarness(t)
	t.Setenv(mockAgyReportIDEnv, "")
	t.Setenv(mockAgyScrapeIDEnv, agyOwnConversationID)
	h.addSession("s1")

	if got := h.send("s1", "hello"); got != agyOwnConversationID {
		t.Fatalf("an agy that reports no id must still be captured by the scrape, got %q", got)
	}
}

func TestAntigravityStreamConversationID(t *testing.T) {
	const a, b = agyOwnConversationID, agyForeignConversationID
	initOf := func(id string) string { return fmt.Sprintf(`{"event":"init","conversation_id":%q,"init":{}}`, id) }
	resultOf := func(id string) string {
		return fmt.Sprintf(`{"event":"result","result":{"conversation_id":%q,"status":"SUCCESS","response":"x"}}`, id)
	}
	stepOf := func(id string) string {
		return fmt.Sprintf(`{"event":"step_update","step_update":{"conversation_id":%q,"text_delta":"x"}}`, id)
	}
	const bareResult = `{"event":"result","result":{"status":"SUCCESS"}}`
	for name, tc := range map[string]struct {
		lines []string
		want  string
	}{
		"init and result agree":         {[]string{initOf(a), stepOf(a), resultOf(a)}, a},
		"init only":                     {[]string{initOf(a), bareResult}, a},
		"result only":                   {[]string{resultOf(a)}, a},
		"init and result disagree":      {[]string{initOf(a), resultOf(b)}, ""},
		"a subagent's step is not read": {[]string{stepOf(b), bareResult}, ""},
		"a subagent's step never wins":  {[]string{initOf(a), stepOf(b), resultOf(a)}, a},
		"not a safe argument":           {[]string{initOf("-rf"), resultOf("-rf")}, ""},
		"a top-level id on a result":    {[]string{fmt.Sprintf(`{"event":"result","conversation_id":%q,"result":{"status":"SUCCESS"}}`, a)}, ""},
		"none":                          {[]string{bareResult}, ""},
	} {
		if got := antigravityStreamConversationID(strings.Join(tc.lines, "\n")); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
