package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// mockACPDriverMode makes the test binary a minimal `<cli> acp`: it answers
// `initialize` with what it was launched with (its argv, the ask-rule overlay,
// and the LENGTH of agy's CSRF token — never the token itself), streams two
// reply chunks and an answer for a prompt, and exits when stdin closes.
const mockACPDriverMode = "acp-driver"

// mockACPStallEnv, when set, makes the mock never answer `initialize`: a CLI
// stalled in its handshake. Any value but mockACPStallSilent is first written
// to its stderr as the CLI's one line.
const (
	mockACPStallEnv    = "TEST_MOCK_ACP_STALL"
	mockACPStallSilent = "silent"
)

func runMockACPDriver() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var msg map[string]any
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		id := msg["id"]
		switch msg["method"] {
		case "initialize":
			if stall := os.Getenv(mockACPStallEnv); stall != "" {
				if stall != mockACPStallSilent {
					fmt.Fprintln(os.Stderr, stall)
				}
				continue
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": 1,
				"_meta": map[string]any{"launch": map[string]any{
					"argv":          os.Args[1:],
					"configContent": os.Getenv(openCodeConfigContentEnv),
					"csrfLength":    len(os.Getenv(antigravityCSRFTokenEnv)),
					"autoUpdateOff": os.Getenv("OPENCODE_DISABLE_AUTOUPDATE"),
				}},
			}})
		case "session/prompt":
			for _, text := range []string{"one ", "two"} {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "ses_1",
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}},
				}})
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": "end_turn"}})
		}
	}
	os.Exit(0)
}

type frameLog struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (l *frameLog) publish(r resultMsg) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames = append(l.frames, r)
}

func (l *frameLog) snapshot() []resultMsg {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]resultMsg(nil), l.frames...)
}

func (l *frameLog) ofType(typ string) []resultMsg {
	var out []resultMsg
	for _, f := range l.snapshot() {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

// startMockACPDriver starts profile's manager on the test binary, installed
// as that CLI at version.
func startMockACPDriver(t *testing.T, p *acpAgentProfile, version string) (*ACPManager, string, *frameLog) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := p.cliName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	if err := copyTestBinary(testExe, bin); err != nil {
		t.Fatal(err)
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{p.cliAgentID: installedACPBinary(bin, version)})
	t.Setenv(mockCLIEnvVar, mockACPDriverMode)
	t.Setenv(openCodeConfigContentEnv, "")
	t.Setenv(antigravityCSRFTokenEnv, "")

	m := newACPManager(nil, p)
	log := &frameLog{}
	id := fmt.Sprintf("acp-test-%d", time.Now().UnixNano())
	if err := m.Start(id, dir, nil, "ws", "uid", ACPStartOptions{}, log.publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })
	return m, id, log
}

func waitForFrame(t *testing.T, log *frameLog, typ, contains string) resultMsg {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range log.ofType(typ) {
			if strings.Contains(f.Output, contains) {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s frame containing %q; got %+v", typ, contains, log.snapshot())
	return resultMsg{}
}

type mockLaunch struct {
	Argv          []string `json:"argv"`
	ConfigContent string   `json:"configContent"`
	CSRFLength    int      `json:"csrfLength"`
	AutoUpdateOff string   `json:"autoUpdateOff"`
}

func readMockLaunch(t *testing.T, frame resultMsg) mockLaunch {
	t.Helper()
	var answer struct {
		Result struct {
			Meta struct {
				Launch mockLaunch `json:"launch"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(frame.Output), &answer); err != nil {
		t.Fatalf("initialize answer %q: %v", frame.Output, err)
	}
	return answer.Result.Meta.Launch
}

// The whole OpenCode driver end to end: `opencode acp` with the ask rules,
// every frame forwarded one line per frame under OpenCode's own types, and
// the ended frame on END.
func TestOpenCodeACPDriverLifecycle(t *testing.T) {
	m, id, log := startMockACPDriver(t, openCodeACPProfile, "2.2.7")
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{"protocolVersion":1}}`); err != nil {
		t.Fatal(err)
	}
	launch := readMockLaunch(t, waitForFrame(t, log, "opencode_acp_message", `"relay-acp-init"`))
	if strings.Join(launch.Argv, " ") != "acp" {
		t.Fatalf("argv = %v, want exactly acp", launch.Argv)
	}
	if !strings.Contains(launch.ConfigContent, `"action":"*_*"`) || launch.AutoUpdateOff != "true" {
		t.Fatalf("launch env = %+v", launch)
	}
	if launch.CSRFLength != 0 {
		t.Fatal("OpenCode was given agy's token")
	}
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"acp-turn-t1","method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"hi"}]}}`); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "opencode_acp_message", `"acp-turn-t1"`)
	// One line, one frame: no reply-chunk merge on this driver.
	var chunks []string
	for _, f := range log.ofType("opencode_acp_message") {
		if strings.Contains(f.Output, "agent_message_chunk") {
			chunks = append(chunks, f.Output)
		}
	}
	if len(chunks) != 2 {
		t.Fatalf("reply chunks published as %d frames, want 2 verbatim: %v", len(chunks), chunks)
	}
	// A refused frame never reaches the CLI and is a typed error.
	if err := m.Send(id, `{"jsonrpc":"2.0","id":5,"method":"session/set_mode","params":{"sessionId":"ses_1","modeId":"auto"}}`); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("set_mode: %v", err)
	}
	if err := m.End(id); err != nil {
		t.Fatal(err)
	}
	ended := waitForFrame(t, log, "opencode_acp_ended", "opencode acp ended")
	if ended.ExitCode != 0 {
		t.Fatalf("ended %+v", ended)
	}
	for _, f := range log.snapshot() {
		if strings.HasPrefix(f.Type, "grok_acp") {
			t.Fatalf("an OpenCode frame went out as %s", f.Type)
		}
	}
}

// The required mitigation, end to end: the agy child itself is launched with
// a 32-byte (64 hex) token in its environment and `acp` as its whole argv.
func TestAntigravityACPDriverLaunchesWithItsToken(t *testing.T) {
	m, id, log := startMockACPDriver(t, antigravityACPProfile, "1.5.9")
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{"protocolVersion":1}}`); err != nil {
		t.Fatal(err)
	}
	launch := readMockLaunch(t, waitForFrame(t, log, "antigravity_acp_message", `"relay-acp-init"`))
	if launch.CSRFLength != 2*antigravityCSRFTokenBytes {
		t.Fatalf("agy acp launched with a %d-character token, want %d", launch.CSRFLength, 2*antigravityCSRFTokenBytes)
	}
	if strings.Join(launch.Argv, " ") != "acp" || launch.ConfigContent != "" {
		t.Fatalf("launch = %+v", launch)
	}
}

// Grok's profile is what GrokACPManager always was: these strings are what
// terminal-service matches on.
func TestGrokACPProfileIsUnchanged(t *testing.T) {
	p := grokACPProfile
	got := []string{p.cliAgentID, p.noun, p.startNoun, p.cliName, p.startedOutput, p.logTag, p.processLabel,
		p.commandTypes.Start, p.commandTypes.Send, p.commandTypes.End,
		p.resultTypes.Started, p.resultTypes.Message, p.resultTypes.Stderr, p.resultTypes.Error, p.resultTypes.Ended,
		p.endedOutput(3)}
	want := []string{"grok", "grok acp", "grok agent stdio", "grok", "Grok ACP started", "[grok-acp]", "grok-acp",
		"grok_acp_start", "grok_acp_send", "grok_acp_end",
		"grok_acp_started", "grok_acp_message", "grok_acp_stderr", "grok_acp_error", "grok_acp_ended",
		"grok agent stdio ended (exit code: 3)"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("profile[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !p.mergeReplyChunks || p.fenceFrames {
		t.Errorf("Grok merges its reply chunks and keeps its own setup-cwd gate; got merge=%v fence=%v", p.mergeReplyChunks, p.fenceFrames)
	}
	if NewGrokACPManager(nil).profile != grokACPProfile {
		t.Error("NewGrokACPManager left Grok's profile")
	}
	if grokACPMaxLifetime != 6*time.Hour || grokACPFirstFrameTimeout != 45*time.Second {
		t.Error("Grok's lifetime and first-frame budget moved")
	}
}

// A refused START keeps terminal-service's settle prefix and its typed code.
func TestACPStartRefusalsKeepTheSettlePrefixAndTheirCode(t *testing.T) {
	acks := captureFollowUpAcks(t)
	prevOC, prevAG := globalOpenCodeACPManager, globalAntigravityACPManager
	globalOpenCodeACPManager, globalAntigravityACPManager = newACPManager(nil, openCodeACPProfile), nil
	t.Cleanup(func() { globalOpenCodeACPManager, globalAntigravityACPManager = prevOC, prevAG })
	dir := t.TempDir()

	pinACPDriverBinaries(t, map[string]acpDriverBinary{opencodeCliAgentID: installedACPBinary(filepath.Join(dir, "opencode"), "1.19.5")})
	handleACPCommand(context.Background(), (*pubsub.Publisher)(nil), commandMsg{ID: "c1", Type: "opencode_acp_start", SessionID: "s1", Cwd: dir}, nil)
	handleACPCommand(context.Background(), nil, commandMsg{ID: "c2", Type: "opencode_acp_start", SessionID: "s2", Cwd: dir, Args: []string{"--model", "x"}}, nil)
	handleACPCommand(context.Background(), nil, commandMsg{ID: "c3", Type: "antigravity_acp_send", SessionID: "s3", Input: "{}"}, nil)

	got := acks.snapshot()
	if len(got) != 3 {
		t.Fatalf("frames %+v", got)
	}
	if got[0].Type != "opencode_acp_error" || !strings.HasPrefix(got[0].Output, "failed to start opencode acp: ") || got[0].ErrorCode != acpErrorCodeCLITooOld {
		t.Errorf("too old: %+v", got[0])
	}
	if !strings.HasPrefix(got[1].Output, "failed to start opencode acp: ") || got[1].ErrorCode != "" {
		t.Errorf("start args: %+v", got[1])
	}
	if got[2].Type != "antigravity_acp_error" || got[2].Output != "antigravity acp manager not initialized" {
		t.Errorf("no manager: %+v", got[2])
	}
}
