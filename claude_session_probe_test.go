package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProbeReportsWritersEditorsHistoryAndOwnPids(t *testing.T) {
	configDir, vscode := t.TempDir(), t.TempDir()
	written := time.Now().Add(-time.Minute)
	writeTranscript(t, configDir, "-w", sidA, written,
		userRecord("/w", "first question"),
		map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
			"content": []map[string]any{{"type": "text", "text": "an answer with api_key=SECRETSECRETSECRETSECRET"}}}},
		map[string]any{"type": "custom-title", "customTitle": "Ultra Plan"})
	tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{{"sessionId": sidA, "title": "Ultra Plan"}}})
	writeEditorState(t, vscode, "w1", string(tabs))
	writeRegistry(t, configDir, 10, sidA, "idle", "claude-vscode")
	writeRegistry(t, configDir, 11, sidA, "busy", "sdk-cli") // this agent's own child
	withProcessStart(t, func(int) (claudeProcessState, int64) { return claudeProcessAlive, 0 })

	env := testDiscoveryEnv(configDir, map[string]string{cliSessionSourceVSCode: vscode})
	env.isOwnPID = func(pid int) bool { return pid == 11 }

	res := probeClaudeSession(context.Background(), env, sidA, false)
	if !res.Exists || !res.Resumable {
		t.Fatalf("exists/resumable = %v/%v", res.Exists, res.Resumable)
	}
	if len(res.Writers) != 1 || res.Writers[0].PID != 10 || res.Writers[0].Editor != cliSessionSourceVSCode {
		t.Fatalf("writers = %+v, want only the editor's process", res.Writers)
	}
	if len(res.OpenInEditors) != 1 || res.OpenInEditors[0] != cliSessionSourceVSCode {
		t.Fatalf("openInEditors = %v", res.OpenInEditors)
	}
	if res.Title != "Ultra Plan" || res.TranscriptWrittenAt/1000 != written.UnixMilli()/1000 {
		t.Fatalf("title/written = %q / %d", res.Title, res.TranscriptWrittenAt)
	}
	if len(res.RecentTurns) != 0 {
		t.Fatal("recent turns returned without includeHistory")
	}

	hist := probeClaudeSession(context.Background(), env, sidA, true)
	if len(hist.RecentTurns) != 2 || hist.RecentTurns[0].Role != "user" || hist.RecentTurns[0].Text != "first question" {
		t.Fatalf("recent turns = %+v", hist.RecentTurns)
	}
	if strings.Contains(hist.RecentTurns[1].Text, "SECRETSECRET") {
		t.Fatalf("recent turn was not masked: %q", hist.RecentTurns[1].Text)
	}
}

func TestProbeMissingAndMalformedSession(t *testing.T) {
	env := testDiscoveryEnv(t.TempDir(), nil)
	if res := probeClaudeSession(context.Background(), env, sidB, true); res.Exists || res.Resumable || res.Error != "" {
		t.Fatalf("missing session = %+v, want exists=false without an error", res)
	}
	if res := probeClaudeSession(context.Background(), env, "../../etc/passwd", false); res.Error != cliSessionErrBadRequest {
		t.Fatalf("malformed id = %+v, want badRequest", res)
	}
}

func TestOutsideWriterStateAndWatcherEmitsOnlyChanges(t *testing.T) {
	if s, _, _ := claudeOutsideWriterState(nil); s != claudeOutsideWriterGone {
		t.Fatalf("no writers = %s", s)
	}
	if s, _, _ := claudeOutsideWriterState([]cliSessionWriter{{Status: "waiting"}}); s != claudeOutsideWriterBusy {
		t.Fatalf("waiting = %s, want busy", s)
	}
	seq := [][]cliSessionWriter{
		nil, // alone at start: nothing to report
		{{PID: 1, Status: "idle", Entrypoint: "claude-vscode", Editor: "vscode"}},
		{{PID: 1, Status: "idle", Entrypoint: "claude-vscode", Editor: "vscode"}},
		{{PID: 1, Status: "busy", Entrypoint: "claude-vscode", Editor: "vscode"}},
		nil,
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var got []string
	i := 0
	go func() {
		claudeOutsideWriterWatch(stop, time.Millisecond, func() []cliSessionWriter {
			if i >= len(seq) {
				return nil
			}
			w := seq[i]
			i++
			if i == len(seq) {
				defer close(stop)
			}
			return w
		}, func(w cliSessionOutsideWriter) { got = append(got, w.State) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
	want := []string{claudeOutsideWriterIdle, claudeOutsideWriterBusy, claudeOutsideWriterGone}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("emitted %v, want %v", got, want)
	}
}

func TestProbeUnrecognisedTranscriptExistsButIsNotResumable(t *testing.T) {
	configDir := t.TempDir()
	writeTranscript(t, configDir, "-w", sidB, time.Now(), map[string]any{"type": "some-future-record"})
	res := probeClaudeSession(context.Background(), testDiscoveryEnv(configDir, nil), sidB, true)
	if !res.Exists || res.Resumable || len(res.RecentTurns) != 0 {
		t.Fatalf("unrecognised transcript = %+v, want exists without resumable", res)
	}
}

/* -------------------------------------------------------------------------- */
/* Protocol 2: apps behind an entrypoint, the outside-writer frame, copies     */
/* -------------------------------------------------------------------------- */

// The Claude desktop app is named as the program holding a session; every
// writer keeps the raw entrypoint, and an entrypoint this build does not know
// names no app.
func TestProbeMapsClaudeDesktopAndKeepsTheRawEntrypoint(t *testing.T) {
	configDir := t.TempDir()
	writeTranscript(t, configDir, "-w", sidA, time.Now().Add(-time.Minute), userRecord("/w", "hi"))
	writeRegistry(t, configDir, 21, sidA, "idle", "claude-desktop")
	writeRegistry(t, configDir, 22, sidA, "idle", "claude-jetbrains")
	writeRegistry(t, configDir, 23, sidA, "idle", "claude-vscode")
	withProcessStart(t, func(int) (claudeProcessState, int64) { return claudeProcessAlive, 0 })

	res := probeClaudeSession(context.Background(), testDiscoveryEnv(configDir, nil), sidA, false)
	byPID := map[int]cliSessionWriter{}
	for _, w := range res.Writers {
		byPID[w.PID] = w
	}
	for pid, want := range map[int]cliSessionWriter{
		21: {PID: 21, Status: "idle", Entrypoint: "claude-desktop", Editor: cliSessionEditorClaudeDesktop},
		22: {PID: 22, Status: "idle", Entrypoint: "claude-jetbrains"},
		23: {PID: 23, Status: "idle", Entrypoint: "claude-vscode", Editor: cliSessionSourceVSCode},
	} {
		if byPID[pid] != want {
			t.Errorf("writer %d = %+v, want %+v", pid, byPID[pid], want)
		}
	}
	if cliSessionEditorClaudeDesktop != "claudeDesktop" {
		t.Fatalf("the desktop app's code is %q; shared-constants CODING_SESSION_STARTED_FROM.CLAUDE_DESKTOP is claudeDesktop", cliSessionEditorClaudeDesktop)
	}
}

// outsideWriterFixture is testdata/claude_native_outside_writer_frame.json.
type outsideWriterFixture struct {
	Description string         `json:"description"`
	Frame       map[string]any `json:"frame"`
}

func readOutsideWriterFixture(t *testing.T) outsideWriterFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_native_outside_writer_frame.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f outsideWriterFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The frame terminal-service's recordOutsideWriter reads, byte for byte the
// shape this agent publishes. Without agentId the server drops every frame
// (the guard never fired before protocol 2); the fixture is the copy the
// server's tests should read instead of a frame built by hand.
func TestOutsideWriterFrameMatchesTheGoldenFixture(t *testing.T) {
	fixture := readOutsideWriterFixture(t)
	session := &ClaudeNativeSession{ID: "ts1", WorkspaceID: "ws1", UID: "u1"}
	frame := claudeOutsideWriterFrame(session, "agent1", cliSessionOutsideWriter{
		State: claudeOutsideWriterBusy, Entrypoint: claudeEntrypointVSCode, Editor: cliSessionSourceVSCode, At: 1790000000000,
	}, 1790000000123)
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// This build's own version and this process's boot id; the fixture holds
	// examples of each.
	if got["version"] != Version || got["bootId"] != agentBootID || agentBootID == "" {
		t.Fatalf("version/bootId = %v/%v, want %s/%s", got["version"], got["bootId"], Version, agentBootID)
	}
	got["version"], got["bootId"] = fixture.Frame["version"], fixture.Frame["bootId"]
	if !reflect.DeepEqual(got, fixture.Frame) {
		want, _ := json.Marshal(fixture.Frame)
		t.Fatalf("frame drifted from the fixture:\n got  %s\n want %s", raw, want)
	}
	if fixture.Frame["type"] != claudeOutsideWriterResultType || fixture.Frame["agentId"] != "agent1" {
		t.Fatalf("fixture = %v", fixture.Frame)
	}
}

// waitForOutsideWriterFrame returns the first claude_native_outside_writer
// frame published within d, or nil.
func waitForOutsideWriterFrame(mu *sync.Mutex, frames *[]resultMsg, d time.Duration) *resultMsg {
	deadline := time.Now().Add(d)
	for {
		mu.Lock()
		for i := range *frames {
			if (*frames)[i].Type == claudeOutsideWriterResultType {
				f := (*frames)[i]
				mu.Unlock()
				return &f
			}
		}
		mu.Unlock()
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The watcher publishes through claudeOutsideWriterFrame, naming the agent it
// was started for.
func TestOutsideWriterWatcherFrameNamesTheAgent(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	writeRegistry(t, configDir, 987001, sidA, "busy", "claude-vscode")
	withProcessStart(t, func(int) (claudeProcessState, int64) { return claudeProcessAlive, 0 })

	session := &ClaudeNativeSession{ID: "ts-watch", WorkspaceID: "ws1", UID: "u1", processExited: make(chan struct{})}
	defer close(session.processExited)
	var mu sync.Mutex
	var frames []resultMsg
	startClaudeOutsideWriterWatcher(session, sidA, "agent-7", func(r resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, r)
	})
	f := waitForOutsideWriterFrame(&mu, &frames, 5*time.Second)
	if f == nil {
		t.Fatal("no outside-writer frame for a session another process is busy in")
	}
	if f.AgentID != "agent-7" || f.BootID != agentBootID || f.SessionID != "ts-watch" || f.UID != "u1" || f.WorkspaceID != "ws1" {
		t.Fatalf("frame = %+v, want it to name agent-7, this boot, and the session's own ids", f)
	}
	if f.OutsideWriter == nil || f.OutsideWriter.State != claudeOutsideWriterBusy || f.OutsideWriter.Editor != cliSessionSourceVSCode {
		t.Fatalf("outsideWriter = %+v", f.OutsideWriter)
	}
}

func TestClaudeResumedInPlaceIDIgnoresForkCopies(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--resume=" + sidA}, sidA},
		{[]string{"--resume", sidA}, sidA},
		{[]string{"--resume=" + sidA, "--fork-session"}, ""},
		{[]string{"--fork-session", "--resume", sidA}, ""},
		{[]string{"--resume=" + sidA, "--fork-session=true"}, ""},
		{[]string{"--model", "opus"}, ""},
		{nil, ""},
	} {
		if got := claudeResumedInPlaceID(tc.args); got != tc.want {
			t.Errorf("claudeResumedInPlaceID(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestClaudeNativeFrameAgentIDPrefersTheRegisteredID(t *testing.T) {
	if got := NewClaudeNativeManager(&Config{AgentID: "registered"}).frameAgentID("addressed"); got != "registered" {
		t.Fatalf("got %q, want the config's id", got)
	}
	if got := NewClaudeNativeManager(&Config{}).frameAgentID("addressed"); got != "addressed" {
		t.Fatalf("got %q, want the command's id when the config has none", got)
	}
	if got := NewClaudeNativeManager(nil).frameAgentID("addressed"); got != "addressed" {
		t.Fatalf("got %q, want the command's id without a config", got)
	}
}

// A session resumed in place is AI Expedite's and is watched for another
// writer; a --fork-session copy runs under a new id, so the original is
// neither claimed nor watched (its editor busy in it must never hold a send
// into the copy). The in-place run proves the fixture produces a frame at
// once, which is what makes the copy's silence meaningful.
func TestForkSessionStartsNoOutsideWriterWatcher(t *testing.T) {
	skipIfUnsupportedOS(t)
	tmpDir := installMockClaude(t, "claude-native-echo")
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	withProcessStart(t, func(int) (claudeProcessState, int64) { return claudeProcessAlive, 0 })

	for i, tc := range []struct {
		name string
		fork bool
	}{{"resumed in place", false}, {"forked copy", true}} {
		t.Run(tc.name, func(t *testing.T) {
			original := fmt.Sprintf("%08x-f0f0-4bbb-8ccc-%012x", i+1, time.Now().UnixNano()&0xffffffffffff)
			writeRegistry(t, configDir, 987100+i, original, "busy", "claude-vscode")
			args := []string{"--resume=" + original}
			if tc.fork {
				args = append(args, "--fork-session")
			}
			m := NewClaudeNativeManager(&Config{AgentID: "agent-fork-test"})
			id := fmt.Sprintf("claude-fork-%d-%d", i, time.Now().UnixNano())
			var mu sync.Mutex
			var frames []resultMsg
			if err := m.Start(id, tmpDir, args, "", "ws", "uid", func(r resultMsg) {
				mu.Lock()
				defer mu.Unlock()
				frames = append(frames, r)
			}, nil); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = m.End(id) }()

			if !tc.fork {
				f := waitForOutsideWriterFrame(&mu, &frames, 10*time.Second)
				if f == nil {
					t.Fatal("a session resumed in place while its editor is busy in it published no outside-writer frame")
				}
				if f.AgentID != "agent-fork-test" || f.BootID != agentBootID || f.SessionID != id {
					t.Fatalf("frame = %+v, want it to name this agent and session", f)
				}
				if !isClaudeOwnSessionID(original) {
					t.Fatal("a session resumed in place was not recorded as AI Expedite's")
				}
				return
			}
			if isClaudeOwnSessionID(original) {
				t.Fatal("a --fork-session copy recorded the ORIGINAL session as AI Expedite's")
			}
			if f := waitForOutsideWriterFrame(&mu, &frames, 1500*time.Millisecond); f != nil {
				t.Fatalf("a --fork-session copy watched the original and published %+v", f)
			}
		})
	}
}
