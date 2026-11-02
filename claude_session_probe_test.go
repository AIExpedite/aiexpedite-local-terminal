package main

import (
	"context"
	"encoding/json"
	"strings"
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
