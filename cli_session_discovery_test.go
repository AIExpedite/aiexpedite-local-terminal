package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2"
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
	if out := readEditorClaudeTabs(context.Background(), cliSessionSourceVSCode, root, time.Time{}); out.skip != "" || len(out.tabs) != 0 || !out.checked {
		t.Fatalf("missing key: %+v, want checked with no tabs", out)
	}
	writeEditorState(t, root, "bad", "{not json")
	if out := readEditorClaudeTabs(context.Background(), cliSessionSourceVSCode, root, time.Time{}); out.skip != cliSessionSkipUnrecognised {
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

// Protocol 2 (startedVia / lastVia, the partial scan, the outside-writer
// frame's agentId) and 3 (every engine with a reader, `engine` on every row)
// are additive: terminal-service accepts any protocol at or above its own
// CLI_SESSION_DISCOVERY_PROTOCOL (1), so older servers keep discovering
// Claude Code through a newer agent: Claude Code stays the first engine
// listed, and one that reads protocol 1-2 only takes that one
// (cliSessionDiscoveryEnginesOf).
func TestCapabilityAdvertisesClaudeDiscovery(t *testing.T) {
	caps := newMachineCapabilities()
	if caps.CliSessionDiscovery == nil || caps.CliSessionDiscovery.Protocol != 3 ||
		len(caps.CliSessionDiscovery.Engines) == 0 || caps.CliSessionDiscovery.Engines[0] != "claudeCode" {
		t.Fatalf("capability = %+v", caps.CliSessionDiscovery)
	}
	b, _ := json.Marshal(caps)
	if !strings.Contains(string(b), `"cliSessionDiscovery":{"engines":["claudeCode",`) || !strings.Contains(string(b), `],"protocol":3}`) {
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

func TestEditorSourceSkipsWindowsOlderThanTheLookBack(t *testing.T) {
	root := t.TempDir()
	tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{{"sessionId": sidA, "title": "Old"}}})
	writeEditorState(t, root, "old", string(tabs))
	old := time.Now().Add(-60 * 24 * time.Hour)
	dbPath := filepath.Join(root, "User", "workspaceStorage", "old", "state.vscdb")
	if err := os.Chtimes(dbPath, old, old); err != nil {
		t.Fatal(err)
	}
	if out := readEditorClaudeTabs(context.Background(), cliSessionSourceVSCode, root, time.Now().Add(-30*24*time.Hour)); len(out.tabs) != 0 {
		t.Fatalf("a window untouched for 60 days was read: %+v", out.tabs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := readEditorClaudeTabs(ctx, cliSessionSourceVSCode, root, time.Time{}); out.skip != cliSessionSkipTimeout {
		t.Fatalf("an ended context should report timeout, got %q", out.skip)
	}
}

func TestDiscoverFindsAnOpenTabsOldTranscriptThroughTheStoreIndex(t *testing.T) {
	configDir, vscode := t.TempDir(), t.TempDir()
	// Outside the 30-day window, so the windowed scan does not parse it, but
	// an open tab names it.
	writeTranscript(t, configDir, "-w-old", sidB, time.Now().Add(-90*24*time.Hour),
		userRecord("/w/old", "old work"), map[string]any{"type": "custom-title", "customTitle": "Old but open"})
	tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{{"sessionId": sidB, "title": "Old but open"}}})
	writeEditorState(t, vscode, "w1", string(tabs))

	store := readClaudeStore(context.Background(), configDir, time.Now().Add(-30*24*time.Hour), 500)
	if !store.pathsComplete || store.paths[sidB] == "" || len(store.store) != 0 {
		t.Fatalf("index = %v complete=%v parsed=%d", store.paths, store.pathsComplete, len(store.store))
	}
	if info := lookupClaudeTranscript(configDir, store, sidB); info == nil || info.SavedTitle != "Old but open" {
		t.Fatalf("lookup through the index = %+v", info)
	}
	if lookupClaudeTranscript(configDir, store, sidC) != nil {
		t.Fatal("an id the complete index does not hold has no transcript")
	}
	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, map[string]string{cliSessionSourceVSCode: vscode}))
	if r := rowByID(res.Rows, sidB); r == nil || !r.Resumable {
		t.Fatalf("the open tab's old session was not offered: %+v", res.Rows)
	}
	// An incomplete listing falls back to searching the project folders.
	partial := claudeSourceOutcome{checked: true, paths: map[string]string{}}
	if info := lookupClaudeTranscript(configDir, partial, sidB); info == nil {
		t.Fatal("an incomplete index must fall back to the folder search")
	}
}

func TestDiscoverDropsOrphanTabOverAnUnrecognisedTranscript(t *testing.T) {
	configDir, vscode := t.TempDir(), t.TempDir()
	writeTranscript(t, configDir, "-w", sidA, time.Now()) // empty file
	writeTranscript(t, configDir, "-w", sidB, time.Now(), map[string]any{"type": "some-future-record"})
	tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{
		{"sessionId": sidA, "title": "Empty"}, {"sessionId": sidB, "title": "Unknown"},
	}})
	writeEditorState(t, vscode, "w1", string(tabs))

	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, map[string]string{cliSessionSourceVSCode: vscode}))
	if len(res.Rows) != 0 {
		t.Fatalf("rows = %+v, want orphan tabs over unrecognised transcripts dropped", res.Rows)
	}
	skipped := false
	for _, s := range res.SourcesSkipped {
		if s.Source == cliSessionSourceClaudeStore && s.Reason == cliSessionSkipUnrecognised {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("sourcesSkipped = %+v, want claudeStore unrecognised", res.SourcesSkipped)
	}
}

func TestTranscriptChunksKeepRecordsCrossingTheChunkBoundary(t *testing.T) {
	const chunk = 64
	dir := t.TempDir()
	for _, size := range []int{chunk + 10, 2*chunk - 1, 2 * chunk, 2*chunk + 30, 5 * chunk} {
		var b strings.Builder
		i := 0
		for b.Len() < size {
			fmt.Fprintf(&b, "{\"n\":%d}\n", i)
			i++
		}
		path := filepath.Join(dir, fmt.Sprintf("t%d.jsonl", size))
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		head, tail, _, err := claudeTranscriptChunks(path, chunk)
		if err != nil {
			t.Fatal(err)
		}
		lines := append(head, tail...)
		for _, l := range lines {
			if json.Unmarshal([]byte(l), &map[string]any{}) != nil {
				t.Fatalf("size %d: fragment %q returned as a line", b.Len(), l)
			}
		}
		if b.Len() <= 2*chunk && (len(head) != i || len(tail) != 0) {
			t.Fatalf("size %d: head/tail = %d/%d lines, want all %d in the head", b.Len(), len(head), len(tail), i)
		}
		if last := fmt.Sprintf("{\"n\":%d}", i-1); lines[len(lines)-1] != last {
			t.Fatalf("size %d: last line %q, want %q", b.Len(), lines[len(lines)-1], last)
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Protocol 2: a partial scan is kept, entrypoints, editor window order       */
/* -------------------------------------------------------------------------- */

func skipReason(res cliSessionsDiscoverResult, source string) string {
	for _, s := range res.SourcesSkipped {
		if s.Source == source {
			return s.Reason
		}
	}
	return ""
}

// A transcript scan still running when the budget ends must not lose what it
// read. Before protocol 2 the store reader and the collector shared one
// context, so the collector stopped at the very moment the reader could
// answer: the scan's rows were dropped, and "truncated" was copied from the
// missing answer as false, so "no match" read as "no such session".
func TestDiscoverKeepsPartialStoreAtDeadline(t *testing.T) {
	configDir := t.TempDir()
	now := time.Now()
	pathA := writeTranscript(t, configDir, "-w", sidA, now.Add(-time.Minute),
		userRecord("/w", "fix the login"), map[string]any{"type": "custom-title", "customTitle": "Login fix"})
	writeTranscript(t, configDir, "-w", sidB, now.Add(-2*time.Minute), userRecord("/w", "not reached in time"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	budgetEnds, _ := ctx.Deadline()
	released := make(chan time.Time, 1)
	env := testDiscoveryEnv(configDir, nil)
	// A slow scan: it has parsed the newest transcript and is still working
	// when its context ends, and then answers with what it has, marked the way
	// readClaudeStore marks a scan its context cut short.
	env.readStore = func(storeCtx context.Context, dir string, since time.Time, maxRows int) claudeSourceOutcome {
		info, err := parseClaudeTranscript(sidA, pathA)
		if err != nil {
			t.Errorf("parse: %v", err)
		}
		<-storeCtx.Done()
		released <- time.Now()
		return claudeSourceOutcome{
			source: cliSessionSourceClaudeStore, checked: true, skip: cliSessionSkipTimeout, trunc: true,
			store: map[string]*claudeTranscriptInfo{sidA: info},
			paths: map[string]string{sidA: pathA},
		}
	}

	res := discoverClaudeSessions(ctx, env)

	select {
	case at := <-released:
		if !at.Before(budgetEnds) {
			t.Fatalf("the store's context ended at %v, not before the budget (%v): no time is left to collect it", at, budgetEnds)
		}
	default:
		t.Fatal("the store reader never answered")
	}
	a := rowByID(res.Rows, sidA)
	if a == nil || a.Title != "Login fix" || !a.Resumable {
		t.Fatalf("rows = %+v, want the partial scan's session kept, resumable", res.Rows)
	}
	if !res.Truncated {
		t.Fatal("a scan the budget cut short must answer truncated")
	}
	if got := skipReason(res, cliSessionSourceClaudeStore); got != cliSessionSkipTimeout {
		t.Fatalf("claudeStore skip = %q, want timeout (sourcesSkipped %+v)", got, res.SourcesSkipped)
	}
}

// A store reader that does not answer at all by the end of the budget (a hung
// disk) still yields an answer, and that answer says it may be missing
// sessions.
func TestDiscoverIsTruncatedWhenTheStoreNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	env := testDiscoveryEnv(t.TempDir(), nil)
	env.readStore = func(context.Context, string, time.Time, int) claudeSourceOutcome {
		<-release // ignores its context
		return claudeSourceOutcome{source: cliSessionSourceClaudeStore, checked: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := discoverClaudeSessions(ctx, env)
	if !res.Truncated {
		t.Fatal("no store answer must read as truncated, not as a complete empty scan")
	}
	if got := skipReason(res, cliSessionSourceClaudeStore); got != cliSessionSkipTimeout {
		t.Fatalf("claudeStore skip = %q, want timeout", got)
	}
}

// Truncation follows the store's own answer: a scan that timed out, or that
// stopped at maxRows, may be missing sessions; a complete one is not truncated.
func TestDiscoverTruncationFollowsTheStoreOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome claudeSourceOutcome
		want    bool
	}{
		{"complete", claudeSourceOutcome{checked: true}, false},
		{"stopped at maxRows", claudeSourceOutcome{checked: true, trunc: true}, true},
		{"timed out", claudeSourceOutcome{checked: true, skip: cliSessionSkipTimeout}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testDiscoveryEnv(t.TempDir(), nil)
			env.readStore = func(context.Context, string, time.Time, int) claudeSourceOutcome {
				o := tc.outcome
				o.source, o.pathsComplete = cliSessionSourceClaudeStore, true
				return o
			}
			if res := discoverClaudeSessions(context.Background(), env); res.Truncated != tc.want {
				t.Fatalf("truncated = %v, want %v", res.Truncated, tc.want)
			}
		})
	}
}

// A session an editor tab names but the store did not list is looked up on
// its own, at most cliSessionMaxExtraLookups of them. One left unread may be a
// missing row, so the answer then says truncated; at the cap nothing is left.
func TestDiscoverIsTruncatedWhenTabLookupsAreCapped(t *testing.T) {
	for _, tc := range []struct {
		tabs int
		want bool
	}{
		{cliSessionMaxExtraLookups, false},
		{cliSessionMaxExtraLookups + 1, true},
	} {
		t.Run(fmt.Sprintf("%d tabs", tc.tabs), func(t *testing.T) {
			vscode := t.TempDir()
			tabs := []map[string]any{}
			for i := 0; i < tc.tabs; i++ {
				tabs = append(tabs, map[string]any{"sessionId": fmt.Sprintf("5%07x-aaaa-4bbb-8ccc-%012x", i, i), "title": "tab"})
			}
			raw, _ := json.Marshal(map[string]any{"panelTabSessions": tabs})
			writeEditorState(t, vscode, "w1", string(raw))
			env := testDiscoveryEnv(t.TempDir(), map[string]string{cliSessionSourceVSCode: vscode})

			res := discoverClaudeSessions(context.Background(), env)
			if res.Truncated != tc.want {
				t.Fatalf("%d tab sessions to look up with a cap of %d: truncated = %v, want %v",
					tc.tabs, cliSessionMaxExtraLookups, res.Truncated, tc.want)
			}
			if !containsString(res.SourcesChecked, cliSessionSourceVSCode) {
				t.Fatalf("sourcesChecked = %v, want the editor read", res.SourcesChecked)
			}
		})
	}
}

func TestClaudeSourceContextEndsBeforeTheBudget(t *testing.T) {
	for _, tc := range []struct {
		budget, wantGrace time.Duration
	}{
		{cliSessionDiscoverBudget, cliSessionSourceDeadlineGrace},
		{400 * time.Millisecond, 100 * time.Millisecond}, // at most a quarter of what is left
	} {
		ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
		src, cancelSrc := claudeSourceContext(ctx)
		outer, _ := ctx.Deadline()
		inner, ok := src.Deadline()
		grace := outer.Sub(inner)
		if !ok || grace <= 0 || grace > tc.wantGrace || grace < tc.wantGrace-50*time.Millisecond {
			t.Fatalf("budget %v: readers stop %v before it, want about %v", tc.budget, grace, tc.wantGrace)
		}
		cancelSrc()
		cancel()
	}
	src, cancelSrc := claudeSourceContext(context.Background())
	defer cancelSrc()
	if _, ok := src.Deadline(); ok {
		t.Fatal("a context without a deadline gained one")
	}
}

// errAfterCtx answers nil to its first n Err() calls and DeadlineExceeded
// after them: a context that ends part-way through a reader.
type errAfterCtx struct {
	context.Context
	left atomic.Int64
}

func (c *errAfterCtx) Err() error {
	if c.left.Add(-1) < 0 {
		return context.DeadlineExceeded
	}
	return nil
}

// readClaudeStore stopped by its context reports what it parsed, marked as a
// truncated, timed-out scan, wherever the context ends.
func TestStoreReportsATimeoutWhenItsContextEndsMidScan(t *testing.T) {
	configDir := t.TempDir()
	now := time.Now()
	ids := []string{sidA, sidB, sidC}
	for i, id := range ids {
		writeTranscript(t, configDir, "-w", id, now.Add(-time.Duration(i+1)*time.Minute), userRecord("/w", "prompt"))
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	out := readClaudeStore(ended, configDir, now.Add(-time.Hour), 500)
	if out.skip != cliSessionSkipTimeout || !out.trunc || !out.checked || out.pathsComplete || len(out.store) != 0 {
		t.Fatalf("ended before the scan: %+v, want an empty, truncated, timed-out outcome", out)
	}
	partial := false
	for n := int64(0); n <= 12; n++ {
		ctx := &errAfterCtx{Context: context.Background()}
		ctx.left.Store(n)
		out := readClaudeStore(ctx, configDir, now.Add(-time.Hour), 500)
		if len(out.store) < len(ids) {
			if !out.trunc || out.skip != cliSessionSkipTimeout {
				t.Fatalf("n=%d: %d of %d parsed with trunc=%v skip=%q, want trunc + timeout", n, len(out.store), len(ids), out.trunc, out.skip)
			}
			partial = partial || len(out.store) > 0
		} else if out.trunc || out.skip != "" {
			t.Fatalf("n=%d: a complete scan reported trunc=%v skip=%q", n, out.trunc, out.skip)
		}
	}
	if !partial {
		t.Fatal("no run ended between two transcripts: the parse loop's own stop was never exercised")
	}
}

// writeLargeTranscript writes head records, then filler without an entrypoint
// past two chunks, then tail records: only the head and tail are ever read.
func writeLargeTranscript(t *testing.T, configDir, id string, head, tail []map[string]any) string {
	t.Helper()
	records := append([]map[string]any{}, head...)
	pad := strings.Repeat("x", 1000)
	for i := 0; i < 3*claudeTranscriptChunkBytes/1000; i++ {
		records = append(records, map[string]any{"type": "queue-operation", "pad": pad})
	}
	records = append(records, tail...)
	return writeTranscript(t, configDir, "-w", id, time.Now(), records...)
}

func viaRecord(kind, entrypoint, text string) map[string]any {
	return map[string]any{"type": kind, "entrypoint": entrypoint, "cwd": "/w",
		"message": map[string]any{"role": kind, "content": text}}
}

// The session started in VS Code and was later driven by an SDK app: where it
// started comes from the head, where it was last driven from the tail.
func TestTranscriptEntrypointFirstFromHeadLastFromTail(t *testing.T) {
	configDir := t.TempDir()
	path := writeLargeTranscript(t, configDir, sidA,
		[]map[string]any{
			viaRecord("user", "claude-vscode", "start here"),
			viaRecord("assistant", "claude-vscode", "ok"),
		},
		[]map[string]any{
			viaRecord("user", "sdk-cli", "continue"),
			{"type": "attachment", "entrypoint": "sdk-cli"},
			viaRecord("assistant", "sdk-cli", "done"),
		})
	info, err := parseClaudeTranscript(sidA, path)
	if err != nil {
		t.Fatal(err)
	}
	if info.FirstEntrypoint != "claude-vscode" || info.LastEntrypoint != "sdk-cli" {
		t.Fatalf("first/last = %q/%q, want claude-vscode/sdk-cli", info.FirstEntrypoint, info.LastEntrypoint)
	}

	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, nil))
	a := rowByID(res.Rows, sidA)
	if a == nil || a.StartedVia != "claude-vscode" || a.LastVia != "sdk-cli" {
		t.Fatalf("row = %+v, want startedVia claude-vscode, lastVia sdk-cli", a)
	}
	b, _ := json.Marshal(a)
	if !strings.Contains(string(b), `"startedVia":"claude-vscode","lastVia":"sdk-cli"`) {
		t.Fatalf("wire shape: %s", b)
	}
	probe := probeClaudeSession(context.Background(), testDiscoveryEnv(configDir, nil), sidA, false)
	if probe.StartedVia != "claude-vscode" || probe.LastVia != "sdk-cli" {
		t.Fatalf("probe = %q/%q, want the same entrypoints as the row", probe.StartedVia, probe.LastVia)
	}
}

// A first prompt too large for the head chunk leaves no complete record there:
// where the session started then falls back to the earliest record the tail
// holds.
func TestTranscriptEntrypointFallsBackToTheTailWhenTheHeadHasNoRecord(t *testing.T) {
	configDir := t.TempDir()
	huge := viaRecord("user", "claude-vscode", strings.Repeat("pasted file ", 7000)) // > one chunk
	path := writeLargeTranscript(t, configDir, sidB,
		[]map[string]any{huge},
		[]map[string]any{viaRecord("user", "cli", "next"), viaRecord("assistant", "sdk-cli", "done")})
	info, err := parseClaudeTranscript(sidB, path)
	if err != nil {
		t.Fatal(err)
	}
	if info.FirstEntrypoint != "cli" || info.LastEntrypoint != "sdk-cli" {
		t.Fatalf("first/last = %q/%q, want the tail's first (cli) and last (sdk-cli)", info.FirstEntrypoint, info.LastEntrypoint)
	}
}

// Unknown entrypoints pass through raw (capped); a value that is not a string
// never costs the record it sits on; a transcript naming none reports none.
func TestTranscriptEntrypointPassesUnknownValuesThrough(t *testing.T) {
	configDir := t.TempDir()
	long := "claude-jetbrains-" + strings.Repeat("z", 60)
	writeTranscript(t, configDir, "-w", sidA, time.Now(),
		map[string]any{"type": "custom-title", "customTitle": "Kept", "entrypoint": 7},
		viaRecord("user", "claude-jetbrains", "a"),
		viaRecord("assistant", long, "b"))
	writeTranscript(t, configDir, "-w", sidB, time.Now(), userRecord("/w", "no entrypoint anywhere"))

	res := discoverClaudeSessions(context.Background(), testDiscoveryEnv(configDir, nil))
	a := rowByID(res.Rows, sidA)
	if a == nil || a.Title != "Kept" {
		t.Fatalf("row = %+v, want the record with a numeric entrypoint still read", a)
	}
	if a.StartedVia != "claude-jetbrains" || a.LastVia != long[:cliSessionViaMaxChars] {
		t.Fatalf("startedVia/lastVia = %q/%q, want the raw unknown value and the long one capped", a.StartedVia, a.LastVia)
	}
	b := rowByID(res.Rows, sidB)
	if b == nil || b.StartedVia != "" || b.LastVia != "" {
		t.Fatalf("row = %+v, want no entrypoint reported", b)
	}
	if raw, _ := json.Marshal(b); strings.Contains(string(raw), "Via") {
		t.Fatalf("a missing entrypoint must be absent from the wire, got %s", raw)
	}
}

func TestCliSessionViaValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"sdk-cli", "sdk-cli"},
		{"  claude-vscode\n", "claude-vscode"},
		{"Claude Desktop", "Claude Desktop"},
		{"", ""},
		{"cli\u0000x", ""},
		{"a\tb", ""},
		{strings.Repeat("é", 50), strings.Repeat("é", cliSessionViaMaxChars)},
	} {
		if got := cliSessionViaValue(tc.in); got != tc.want {
			t.Errorf("cliSessionViaValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The window folders are hashes, so name order is no order: the cap must keep
// the windows written most recently.
func TestEditorWindowsAreReadNewestFirstBeforeTheCap(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for _, w := range []struct {
		name, session string
		age           time.Duration
	}{
		{"a-oldest", sidA, 20 * 24 * time.Hour},
		{"b-middle", sidB, 2 * time.Hour},
		{"c-newest", sidC, time.Minute},
	} {
		tabs, _ := json.Marshal(map[string]any{"panelTabSessions": []map[string]any{{"sessionId": w.session, "title": w.name}}})
		writeEditorState(t, root, w.name, string(tabs))
		db := filepath.Join(root, "User", "workspaceStorage", w.name, "state.vscdb")
		if err := os.Chtimes(db, now.Add(-w.age), now.Add(-w.age)); err != nil {
			t.Fatal(err)
		}
	}
	out := readEditorClaudeTabsUpTo(context.Background(), cliSessionSourceVSCode, root, now.Add(-30*24*time.Hour), 2)
	got := map[string]bool{}
	for _, tab := range out.tabs {
		got[tab.SessionID] = true
	}
	if len(out.tabs) != 2 || !got[sidC] || !got[sidB] || got[sidA] {
		t.Fatalf("tabs = %+v, want the two most recently written windows (c, b), not the first two by name", out.tabs)
	}
	if out.skip != "" || !out.checked {
		t.Fatalf("outcome = %+v, want a clean read", out)
	}
	for _, tab := range out.tabs {
		if tab.SessionID == sidC && now.Sub(time.UnixMilli(tab.SeenAt)) > 2*time.Minute {
			t.Fatalf("seenAt = %v, want the window's own last write", time.UnixMilli(tab.SeenAt))
		}
	}
}

/* -------------------------------------------------------------------------- */
/* Protocol 3: the engine registry, one budget, per-engine truncation         */
/* -------------------------------------------------------------------------- */

// standInReader replaces engine's reader (or adds one) for the rest of the test.
func standInReader(t *testing.T, r *cliSessionReader) {
	t.Helper()
	prev := cliSessionReaders
	next := []*cliSessionReader{}
	replaced := false
	for _, existing := range prev {
		if existing.engine == r.engine {
			next, replaced = append(next, r), true
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		next = append(next, r)
	}
	cliSessionReaders = next
	t.Cleanup(func() { cliSessionReaders = prev })
}

// capturePublished records every result a handler publishes.
func capturePublished(t *testing.T) func() []resultMsg {
	t.Helper()
	var mu sync.Mutex
	var got []resultMsg
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })
	return func() []resultMsg {
		mu.Lock()
		defer mu.Unlock()
		return append([]resultMsg(nil), got...)
	}
}

// The wire values are shared-constants CLI_SESSION_ENGINES / CLI_SESSION_SOURCES
// (cliSessionDiscovery.js): a contract pin, so a rename fails here.
func TestCliSessionEnginesAndSourcesMatchSharedConstants(t *testing.T) {
	want := []string{"claudeCode", "codex", "grok", "opencode", "antigravity", "museCode"}
	got := []string{}
	for _, r := range cliSessionReaders {
		got = append(got, r.engine)
		if r.discover == nil || r.probe == nil || len(r.sources) == 0 {
			t.Errorf("%s: reader incomplete: %+v", r.engine, r)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry engines = %v, want %v (Claude Code first)", got, want)
	}
	sources := map[string]string{
		cliSessionSourceCodexStore:       "codexStore",
		cliSessionSourceGrokStore:        "grokStore",
		cliSessionSourceAntigravityStore: "antigravityStore",
		cliSessionSourceOpenCodeStore:    "opencodeStore",
		cliSessionSourceMuseListing:      "museListing",
	}
	for got, want := range sources {
		if got != want {
			t.Errorf("source %q, want %q", got, want)
		}
	}
	if cliSessionDiscoveryProtocol != 3 {
		t.Fatalf("protocol = %d, want shared-constants CLI_SESSION_MULTI_ENGINE_PROTOCOL (3)", cliSessionDiscoveryProtocol)
	}
	if cliSessionErrProviderUnsupported != "providerUnsupported" || cliSessionErrNotInstalled != "notInstalled" {
		t.Fatal("error codes must spell shared-constants CLI_SESSION_DISCOVERY_STATUS_REASONS")
	}
}

// Every engine this build has a reader for, Claude Code first, whether or not
// its CLI is installed here (CONTRACT: `engines: [...six]`): terminal-service
// answers "not installed" only for a listed engine, and reads one missing at
// protocol 3 as an agent whose sessions this app cannot read at all. The PATH
// here holds no agent's CLI, which must not shorten the list.
func TestCapabilityListsEveryEngineClaudeFirst(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := newCliSessionDiscoveryCapability()
	want := `{"engines":["claudeCode","codex","grok","opencode","antigravity","museCode"],"protocol":3}`
	if raw, _ := json.Marshal(got); string(raw) != want {
		t.Fatalf("capability = %s, want %s", raw, want)
	}
	standInReader(t, &cliSessionReader{engine: "someNewAgent", sources: []string{"someStore"}})
	if got := newCliSessionDiscoveryCapability(); got.Engines[len(got.Engines)-1] != "someNewAgent" || got.Engines[0] != cliSessionEngineClaudeCode {
		t.Fatalf("capability = %+v, want every reader listed, Claude Code first", got)
	}
}

func discoverCmd(args ...string) commandMsg {
	return commandMsg{ID: "c1", WorkspaceID: "ws", UID: "u1", AgentID: "agent-1", RefreshID: "req-123456", Command: cliSessionsDiscoverCommand, Args: args}
}

// One command asks for one engine, answered by that engine's reader; every
// row leaves stamped with the engine, and an engine without a reader is
// refused as before.
func TestDiscoverCommandDispatchesByEngine(t *testing.T) {
	published := capturePublished(t)
	var gotReq cliSessionDiscoverRequest
	standInReader(t, &cliSessionReader{
		engine: cliSessionEngineCodex, sources: []string{cliSessionSourceCodexStore},
		discover: func(_ context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			gotReq = req
			res := newCliSessionsResult("", req.now)
			res.Rows = []cliSessionRow{{SessionID: "t-old", LastActiveAt: 1}, {SessionID: "t-new", LastActiveAt: 2}}
			res.SourcesChecked = []string{cliSessionSourceCodexStore}
			return res
		},
		probe: func(context.Context, cliSessionProbeRequest) cliSessionProbeResult { return cliSessionProbeResult{} },
	})
	if err := handleCliSessionsDiscoverCommand(context.Background(), nil, discoverCmd("codex", "7", "999"), &Config{AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if gotReq.lookBack != 7*24*time.Hour || gotReq.maxRows != cliSessionDefaultMaxRows {
		t.Fatalf("request = %+v, want 7 days and the row ceiling clamped", gotReq)
	}
	frames := published()
	if len(frames) != 1 || frames[0].CliSessions == nil || frames[0].Type != cliSessionsDiscoverResultType || frames[0].RefreshID != "req-123456" {
		t.Fatalf("published %+v, want one correlated discover result", frames)
	}
	res := frames[0].CliSessions
	if res.Engine != "codex" || len(res.Rows) != 2 || res.Rows[0].SessionID != "t-new" || res.Error != "" {
		t.Fatalf("result = %+v, want codex's rows newest first", res)
	}
	for _, r := range res.Rows {
		if r.Engine != "codex" || r.Editors == nil {
			t.Fatalf("row = %+v, want engine codex and an editors list", r)
		}
	}
	raw, _ := json.Marshal(res.Rows[0])
	if !strings.HasPrefix(string(raw), `{"engine":"codex","sessionId":"t-new"`) {
		t.Fatalf("row wire shape: %s", raw)
	}

	if err := handleCliSessionsDiscoverCommand(context.Background(), nil, discoverCmd("someNewAgent"), nil); err != nil {
		t.Fatal(err)
	}
	frames = published()
	last := frames[len(frames)-1]
	if last.Status != "error" || last.CliSessions.Error != cliSessionErrEngineNotSupported || last.CliSessions.Engine != "someNewAgent" {
		t.Fatalf("unknown engine = %+v / %+v, want engineNotSupported", last, last.CliSessions)
	}
}

// Claude Code's rows carry `engine` too; nothing else about them changes.
func TestClaudeRowsCarryTheirEngine(t *testing.T) {
	configDir := t.TempDir()
	writeTranscript(t, configDir, "-w", sidA, time.Now(), userRecord("/w", "fix the build"))
	reader := &cliSessionReader{
		engine: cliSessionEngineClaudeCode, selfBudgeted: true, sources: claudeSessionReader.sources,
		discover: func(ctx context.Context, _ cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			return discoverClaudeSessions(ctx, testDiscoveryEnv(configDir, nil))
		},
	}
	res := runCliSessionDiscover(context.Background(), reader, newCliSessionDiscoverRequest(30, 500))
	a := rowByID(res.Rows, sidA)
	if res.Engine != "claudeCode" || a == nil || a.Engine != "claudeCode" || a.Title != "fix the build" {
		t.Fatalf("result = %+v, want the Claude row stamped claudeCode", res)
	}
}

// A reader that never answers (a hung disk) does not hold the answer past the
// budget: the engine is reported truncated, its sources timed out.
func TestRunDiscoverReportsAReaderThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reader := &cliSessionReader{
		engine: cliSessionEngineGrok, sources: []string{cliSessionSourceGrokStore},
		discover: func(context.Context, cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			<-release // ignores its context
			return cliSessionsDiscoverResult{}
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := runCliSessionDiscover(ctx, reader, newCliSessionDiscoverRequest(30, 500))
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("the answer waited %v for a reader that never answers", waited)
	}
	if !res.Truncated || skipReason(res, cliSessionSourceGrokStore) != cliSessionSkipTimeout || res.Engine != "grok" {
		t.Fatalf("result = %+v, want grok truncated with its store timed out", res)
	}
}

// A reader the budget cuts short keeps what it read: it stops on its own
// context, which ends before the collector stops waiting.
func TestRunDiscoverKeepsAPartialAnswerAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	budgetEnds, _ := ctx.Deadline()
	reader := &cliSessionReader{
		engine: cliSessionEngineAntigravity, sources: []string{cliSessionSourceAntigravityStore},
		discover: func(rctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			<-rctx.Done()
			if !time.Now().Before(budgetEnds) {
				t.Error("the reader's context did not end before the budget")
			}
			res := newCliSessionsResult("", req.now)
			res.Rows = []cliSessionRow{{SessionID: "11111111-2222-4333-8444-555555555555", LastActiveAt: 5}}
			res.Truncated = true
			res.skip(cliSessionSourceAntigravityStore, cliSessionSkipTimeout)
			return res
		},
	}
	res := runCliSessionDiscover(ctx, reader, newCliSessionDiscoverRequest(30, 500))
	if len(res.Rows) != 1 || !res.Truncated || res.Rows[0].Engine != "antigravity" {
		t.Fatalf("result = %+v, want the partial row kept and truncated", res)
	}
}

// The row budget is shared: past maxRows the oldest rows go and the answer
// says so; a reader's panic is a skipped source, not a crashed agent.
func TestRunDiscoverCapsRowsAndRecoversAPanic(t *testing.T) {
	reader := &cliSessionReader{
		engine: cliSessionEngineOpenCode, sources: []string{cliSessionSourceOpenCodeStore},
		discover: func(_ context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			res := newCliSessionsResult("", req.now)
			for i := 0; i < 5; i++ {
				res.Rows = append(res.Rows, cliSessionRow{SessionID: fmt.Sprintf("ses_%08d", i), LastActiveAt: int64(i)})
			}
			return res
		},
	}
	res := runCliSessionDiscover(context.Background(), reader, newCliSessionDiscoverRequest(30, 3))
	if len(res.Rows) != 3 || !res.Truncated || res.Rows[0].SessionID != "ses_00000004" {
		t.Fatalf("result = %+v, want the 3 newest rows, truncated", res.Rows)
	}
	reader.discover = func(context.Context, cliSessionDiscoverRequest) cliSessionsDiscoverResult { panic("boom") }
	res = runCliSessionDiscover(context.Background(), reader, newCliSessionDiscoverRequest(30, 3))
	if skipReason(res, cliSessionSourceOpenCodeStore) != cliSessionSkipUnreadable || !res.Truncated {
		t.Fatalf("panicking reader = %+v, want its source unreadable", res)
	}
}

func probeCmd(args ...string) commandMsg {
	return commandMsg{ID: "p1", WorkspaceID: "ws", UID: "u1", AgentID: "agent-1", RefreshID: "req-654321", Command: cliSessionProbeCommand, Args: args}
}

// The probe is dispatched by engine too; one that cannot answer in time says
// so instead of "no such session".
func TestProbeCommandDispatchesByEngine(t *testing.T) {
	published := capturePublished(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	standInReader(t, &cliSessionReader{
		engine: cliSessionEngineMuseCode, sources: []string{cliSessionSourceMuseListing},
		discover: func(context.Context, cliSessionDiscoverRequest) cliSessionsDiscoverResult {
			return cliSessionsDiscoverResult{}
		},
		probe: func(_ context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
			if req.sessionID == "slow" {
				<-release
			}
			return cliSessionProbeResult{SessionID: req.sessionID, Exists: true, Resumable: true, Title: "Listed", TitleSource: cliSessionTitleSavedTitle}
		},
	})
	if err := handleCliSessionProbeCommand(context.Background(), nil, probeCmd("museCode", "s-1", "/w", "0"), nil); err != nil {
		t.Fatal(err)
	}
	frames := published()
	if len(frames) != 1 || frames[0].Probe == nil || !frames[0].Probe.Resumable || frames[0].Probe.Writers == nil || frames[0].Probe.OpenInEditors == nil {
		t.Fatalf("probe = %+v, want the reader's answer with empty lists", frames)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := runCliSessionProbe(ctx, cliSessionReaderFor(cliSessionEngineMuseCode), cliSessionProbeRequest{sessionID: "slow"})
	if res.Error != cliSessionErrTimeout || res.Exists || res.Resumable {
		t.Fatalf("slow probe = %+v, want a timeout, never gone or resumable", res)
	}
	if err := handleCliSessionProbeCommand(context.Background(), nil, probeCmd("someNewAgent", "x"), nil); err != nil {
		t.Fatal(err)
	}
	frames = published()
	if p := frames[len(frames)-1].Probe; p == nil || p.Error != cliSessionErrEngineNotSupported {
		t.Fatalf("unknown engine probe = %+v", p)
	}
}

func TestCliSessionTitleOfTellsATitleFromAPrompt(t *testing.T) {
	for _, tc := range []struct {
		saved, last, first, wantTitle, wantSource string
	}{
		{"Fix the login flow", "", "please fix login", "Fix the login flow", cliSessionTitleSavedTitle},
		// A title that is only the first prompt's start reads as that prompt.
		{"please fix the lo…", "", "please fix the login flow", "please fix the login flow", cliSessionTitleFirstPrompt},
		{"", "the last ask", "the first ask", "the last ask", cliSessionTitleLastPrompt},
		{"", "", "", "", cliSessionTitleNone},
		{"Kept", "", "", "Kept", cliSessionTitleSavedTitle},
	} {
		title, source, _ := cliSessionTitleOf(tc.saved, 1, tc.last, tc.first)
		if title != tc.wantTitle || source != tc.wantSource {
			t.Errorf("cliSessionTitleOf(%q, %q, %q) = %q/%q, want %q/%q", tc.saved, tc.last, tc.first, title, source, tc.wantTitle, tc.wantSource)
		}
	}
	long := strings.Repeat("word ", 40) + "api_key=SECRETSECRETSECRETSECRET"
	if title, _, _ := cliSessionTitleOf("", 0, "", long); utf8.RuneCountInString(title) > cliSessionFallbackLabelMaxChars {
		t.Fatalf("prompt label %q is longer than %d", title, cliSessionFallbackLabelMaxChars)
	}
	if title, _, _ := cliSessionTitleOf("", 0, "", "use api_key=SECRETSECRETSECRETSECRET now"); strings.Contains(title, "SECRETSECRET") {
		t.Fatalf("prompt label %q was not masked", title)
	}
}

func TestParseCliSessionTimeSpellings(t *testing.T) {
	want := time.Date(2026, 11, 19, 10, 0, 0, 123000000, time.UTC).UnixMilli()
	for _, s := range []string{"2026-11-19T10:00:00.123Z", "2026-11-19T11:00:00.123+01:00", "2026-11-19 11:00:00.123+01:00", "2026-11-19 10:00:00.123000+00:00"} {
		if got := parseCliSessionTime(s); got != want {
			t.Errorf("parseCliSessionTime(%q) = %d, want %d", s, got, want)
		}
	}
	if parseCliSessionTime("yesterday") != 0 || cliSessionEpochMs(1795103283) != 1795103283000 || cliSessionEpochMs(1795103283242) != 1795103283242 {
		t.Fatal("unknown spellings read as 0, seconds and milliseconds both as ms")
	}
}

// sqliteSideFiles lists what lies beside a store in its folder.
func sqliteSideFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Another program's SQLite store is read without leaving anything behind in
// its folder: a WAL store whose owner is not running is read `immutable` (a
// plain read-only open creates its -wal and -shm and leaves them there), and
// one whose owner runs is read through the owner's live log, so the writes it
// has not checkpointed yet are seen and its next write still goes through.
func TestCliSessionSQLiteReadCreatesNothingBesideTheStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	owner, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetMaxOpenConns(1)
	for _, stmt := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE t (v TEXT)`, `INSERT INTO t VALUES ('checkpointed')`} {
		if _, err := owner.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// The owner's last connection closes: SQLite checkpoints and removes the log.
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		db, err := openCliSessionSQLite(context.Background(), path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		return n
	}
	if got := sqliteSideFiles(t, dir); len(got) != 1 {
		t.Fatalf("before the read the folder holds %v, want the store alone", got)
	}
	if n := count(); n != 1 {
		t.Fatalf("read %d rows, want the checkpointed one", n)
	}
	if got := sqliteSideFiles(t, dir); len(got) != 1 || got[0] != "store.db" {
		t.Fatalf("the read left %v in the owner's folder, want the store alone", got)
	}

	// The owner runs again and writes past its last checkpoint.
	owner, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetMaxOpenConns(1)
	defer owner.Close()
	for _, stmt := range []string{`PRAGMA wal_autocheckpoint=0`, `INSERT INTO t VALUES ('in the log')`} {
		if _, err := owner.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if !cliSessionSQLiteWALLive(path) {
		t.Fatalf("the running owner's log is not live: %v", sqliteSideFiles(t, dir))
	}
	if n := count(); n != 2 {
		t.Fatalf("read %d rows, want the row still only in the owner's log too", n)
	}
	if _, err := owner.Exec(`INSERT INTO t VALUES ('after the read')`); err != nil {
		t.Fatalf("the owner's write after the read failed: %v", err)
	}
}

// cutAfterContext is a listing budget that runs out after its first `left`
// checks, whatever the clock says: a listing the budget cuts short, at a
// place the test chooses.
type cutAfterContext struct {
	context.Context
	left atomic.Int32
}

func (c *cutAfterContext) Err() error {
	if c.left.Add(-1) < 0 {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// pinCliSessionListing makes every store listing in the test run out after
// `checks` budget checks; the reader's own budget is untouched.
func pinCliSessionListing(t *testing.T, checks int32) {
	t.Helper()
	cliSessionListingContextMu.Lock()
	prev := cliSessionListingContextOf
	cliSessionListingContextOf = func(ctx context.Context) (context.Context, context.CancelFunc) {
		c := &cutAfterContext{Context: ctx}
		c.left.Store(checks)
		return c, func() {}
	}
	cliSessionListingContextMu.Unlock()
	t.Cleanup(func() {
		cliSessionListingContextMu.Lock()
		cliSessionListingContextOf = prev
		cliSessionListingContextMu.Unlock()
	})
}

// A store's listing may use half of what is left of the reader's budget, so
// a listing cut short still leaves time to read what it found; a reader with
// no deadline lists under its own cancellation.
func TestCliSessionListingContextEndsHalfwayThroughTheBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	listCtx, cancelList := cliSessionListingContext(ctx)
	defer cancelList()
	outer, _ := ctx.Deadline()
	inner, ok := listCtx.Deadline()
	if !ok {
		t.Fatal("the listing has no deadline")
	}
	if left := time.Until(inner); left > 2100*time.Millisecond || left < 1500*time.Millisecond {
		t.Fatalf("the listing ends in %v, want about half of the 4s budget", left)
	}
	if !inner.Before(outer) {
		t.Fatalf("the listing ends at %v, not before the budget's %v", inner, outer)
	}
	bare, cancelBare := context.WithCancel(context.Background())
	listBare, cancelListBare := cliSessionListingContext(bare)
	defer cancelListBare()
	if _, ok := listBare.Deadline(); ok {
		t.Fatal("a listing under no deadline got one")
	}
	cancelBare()
	if listBare.Err() == nil {
		t.Fatal("the listing outlived its reader's cancellation")
	}
}
