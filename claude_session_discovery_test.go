package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sidA = "11111111-aaaa-4bbb-8ccc-000000000001"
	sidB = "22222222-aaaa-4bbb-8ccc-000000000002"
	sidC = "33333333-aaaa-4bbb-8ccc-000000000003"
	sidD = "44444444-aaaa-4bbb-8ccc-000000000004"
)

// writeTranscript writes <configDir>/projects/<slug>/<id>.jsonl with records.
func writeTranscript(t *testing.T, configDir, slug, id string, mod time.Time, records ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range records {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func userRecord(cwd, text string) map[string]any {
	return map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{"role": "user", "content": text}}
}

// writeRegistry writes <configDir>/sessions/<pid>.json.
func writeRegistry(t *testing.T, configDir string, pid int, id, status, entrypoint string) {
	t.Helper()
	dir := filepath.Join(configDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"pid": pid, "sessionId": id, "cwd": "/work/api", "status": status,
		"entrypoint": entrypoint, "updatedAt": time.Now().UnixMilli(),
	})
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeEditorState writes one editor window's state.vscdb holding value under
// the Claude extension key (or no key at all when value is "").
func writeEditorState(t *testing.T, root, window, value string) {
	t.Helper()
	dir := filepath.Join(root, "User", "workspaceStorage", window)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	if value != "" {
		if _, err := db.Exec(`INSERT INTO ItemTable (key, value) VALUES (?, ?)`, claudeEditorStateKey, value); err != nil {
			t.Fatal(err)
		}
	}
}

func withProcessStart(t *testing.T, fn func(pid int) (claudeProcessState, int64)) {
	t.Helper()
	prev := claudeProcessStart
	claudeProcessStart = fn
	t.Cleanup(func() { claudeProcessStart = prev })
}

func testDiscoveryEnv(configDir string, editorRoots map[string]string) claudeDiscoveryEnv {
	return claudeDiscoveryEnv{
		configDir:    configDir,
		editorRoots:  editorRoots,
		now:          time.Now(),
		lookBack:     30 * 24 * time.Hour,
		maxRows:      500,
		isOwnSession: func(id string) bool { return id == sidC },
		isOwnPID:     func(int) bool { return false },
	}
}

func rowByID(rows []cliSessionRow, id string) *cliSessionRow {
	for i := range rows {
		if rows[i].SessionID == id {
			return &rows[i]
		}
	}
	return nil
}

func TestDiscoverMergesSourcesAndAppliesTitlePrecedence(t *testing.T) {
	configDir := t.TempDir()
	vscode := t.TempDir()
	now := time.Now()
	// A: open in a VS Code tab with a real title, transcript has a saved title.
	writeTranscript(t, configDir, "-work-frontend", sidA, now.Add(-time.Hour),
		userRecord("/work/frontend", "first ask"),
		map[string]any{"type": "custom-title", "customTitle": "Saved title A"})
	// B: only a transcript, no saved title: falls back to its last prompt.
	writeTranscript(t, configDir, "-work-api", sidB, now.Add(-2*time.Hour),
		userRecord("/work/api", "first prompt B"),
		map[string]any{"type": "last-prompt", "lastPrompt": "last prompt B"})
	// C: a session this agent started.
	writeTranscript(t, configDir, "-work-api", sidC, now.Add(-3*time.Hour), userRecord("/work/api", "own"))
	// Too old for the look-back window.
	writeTranscript(t, configDir, "-work-old", sidD, now.Add(-40*24*time.Hour), userRecord("/work/old", "old"))

	tabs, _ := json.Marshal(map[string]any{
		"panelTabSessions": []map[string]any{
			{"sessionId": sidA, "title": "Ultra Plan Replacing Max 20x"},
			// An orphan tab title: no transcript and no process behind it.
			{"sessionId": "55555555-aaaa-4bbb-8ccc-000000000005", "title": "Orphan"},
		},
		"storedStartupConfig": map[string]any{"cwd": "/work/frontend"},
	})
	writeEditorState(t, vscode, "w1", string(tabs))
	writeRegistry(t, configDir, 4242, sidA, "idle", "claude-vscode")
	withProcessStart(t, func(int) (claudeProcessState, int64) { return claudeProcessAlive, 0 })

	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, map[string]string{
		cliSessionSourceVSCode: vscode,
		cliSessionSourceCursor: filepath.Join(t.TempDir(), "missing"),
	}))

	if len(res.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (A, B, C; orphan and old dropped): %+v", len(res.Rows), res.Rows)
	}
	a := rowByID(res.Rows, sidA)
	if a == nil || a.Title != "Ultra Plan Replacing Max 20x" || a.TitleSource != cliSessionTitleEditorTab {
		t.Fatalf("A = %+v, want the editor tab title", a)
	}
	if a.Running == nil || a.Running.Status != "idle" || len(a.Editors) != 1 || a.Editors[0].Editor != cliSessionSourceVSCode {
		t.Fatalf("A running/editors = %+v / %+v", a.Running, a.Editors)
	}
	if b := rowByID(res.Rows, sidB); b == nil || b.Title != "last prompt B" || b.TitleSource != cliSessionTitleLastPrompt || b.Running != nil {
		t.Fatalf("B = %+v, want the last-prompt fallback, not running", b)
	}
	if c := rowByID(res.Rows, sidC); c == nil || !c.StartedByAiExpedite {
		t.Fatalf("C = %+v, want startedByAiExpedite", c)
	}
	if rowByID(res.Rows, sidD) != nil {
		t.Fatal("a session older than the look-back window was returned")
	}
	if res.Rows[0].SessionID != sidA {
		t.Fatalf("rows not newest first: %s first", res.Rows[0].SessionID)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "first ask") {
		t.Fatal("conversation content beyond the fallback label leaked into the result")
	}
	skipped := map[string]string{}
	for _, s := range res.SourcesSkipped {
		skipped[s.Source] = s.Reason
	}
	if skipped[cliSessionSourceCursor] != cliSessionSkipNotFound {
		t.Fatalf("missing Cursor should be skipped notFound, got %v", res.SourcesSkipped)
	}
}

func TestDiscoverDefaultEditorTitleDefersToSavedTitle(t *testing.T) {
	configDir, vscode := t.TempDir(), t.TempDir()
	writeTranscript(t, configDir, "-w", sidA, time.Now(),
		userRecord("/w", "hi"), map[string]any{"type": "ai-title", "aiTitle": "Fix the login flow"})
	tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{{"sessionId": sidA, "title": "Claude Code"}}})
	writeEditorState(t, vscode, "w1", string(tabs))
	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, map[string]string{cliSessionSourceVSCode: vscode}))
	a := rowByID(res.Rows, sidA)
	if a == nil || a.Title != "Fix the login flow" || a.TitleSource != cliSessionTitleSavedTitle {
		t.Fatalf("A = %+v, want the saved title over the default tab title", a)
	}
}

func TestEditorSourceMissingKeyAndMalformedValue(t *testing.T) {
	root := t.TempDir()
	writeEditorState(t, root, "nokey", "")
	if out := readEditorClaudeTabs(cliSessionSourceVSCode, root); out.skip != "" || len(out.tabs) != 0 || !out.checked {
		t.Fatalf("missing key: %+v, want checked with no tabs", out)
	}
	writeEditorState(t, root, "bad", "{not json")
	if out := readEditorClaudeTabs(cliSessionSourceVSCode, root); out.skip != cliSessionSkipUnrecognised {
		t.Fatalf("malformed value: skip = %q, want unrecognised", out.skip)
	}
}

func TestRegistryDropsDeadAndReusedPids(t *testing.T) {
	configDir := t.TempDir()
	writeRegistry(t, configDir, 1, sidA, "busy", "cli")
	writeRegistry(t, configDir, 2, sidB, "idle", "cli")
	withProcessStart(t, func(pid int) (claudeProcessState, int64) {
		if pid == 1 {
			return claudeProcessGone, 0
		}
		return claudeProcessAlive, 0
	})
	entries, _ := readClaudeRegistryEntries(configDir)
	if len(entries) != 1 || entries[0].SessionID != sidB {
		t.Fatalf("entries = %+v, want only the live pid", entries)
	}
	// A reused pid: the process started long after the registry entry says.
	if claudeRegistryEntryLive(claudeRegistryFile{PID: 2, ProcStart: "Thu Oct 29 12:27:18 2026"}) {
		withProcessStart(t, func(int) (claudeProcessState, int64) {
			return claudeProcessAlive, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
		})
		if claudeRegistryEntryLive(claudeRegistryFile{PID: 2, ProcStart: "Thu Oct 29 12:27:18 2026"}) {
			t.Fatal("a reused pid with a mismatched procStart was accepted")
		}
	}
}

func TestStoreHonoursClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if got := claudeConfigDir("/home/nobody"); got != dir {
		t.Fatalf("claudeConfigDir = %q, want CLAUDE_CONFIG_DIR %q", got, dir)
	}
}

func TestDiscoverAndProbeAreInternalDemandCommands(t *testing.T) {
	for _, c := range []string{cliSessionsDiscoverCommand, cliSessionProbeCommand} {
		if !isInternalDemandCommand(c) {
			t.Fatalf("%s must be an internal demand command", c)
		}
	}
}

func TestCapabilityAdvertisesClaudeDiscovery(t *testing.T) {
	caps := newMachineCapabilities()
	if caps.CliSessionDiscovery == nil || caps.CliSessionDiscovery.Protocol != 1 ||
		len(caps.CliSessionDiscovery.Engines) != 1 || caps.CliSessionDiscovery.Engines[0] != "claudeCode" {
		t.Fatalf("capability = %+v", caps.CliSessionDiscovery)
	}
	b, _ := json.Marshal(caps)
	if !strings.Contains(string(b), `"cliSessionDiscovery":{"engines":["claudeCode"],"protocol":1}`) {
		t.Fatalf("wire shape: %s", b)
	}
}

func TestParseDiscoverArgsBounds(t *testing.T) {
	engine, days, rows := parseDiscoverArgs([]string{"claudeCode", "30", "500"})
	if engine != "claudeCode" || days != 30 || rows != 500 {
		t.Fatalf("got %s %d %d", engine, days, rows)
	}
	env := defaultClaudeDiscoveryEnv(9999, 99999)
	if env.lookBack != cliSessionMaxLookBackDays*24*time.Hour || env.maxRows != cliSessionMaxRows {
		t.Fatalf("bounds not applied: %v %d", env.lookBack, env.maxRows)
	}
}

func TestForkSessionPassesThroughInteractiveArgs(t *testing.T) {
	args, _ := buildClaudeInteractiveArgs([]string{"--resume=" + sidA, "--fork-session"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume="+sidA) || !strings.Contains(joined, "--fork-session") {
		t.Fatalf("server-signed resume / fork-session were not passed through: %v", args)
	}
}
