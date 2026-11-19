package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	agA = "aaaaaaaa-0000-4000-8000-00000000000a"
	agB = "aaaaaaaa-0000-4000-8000-00000000000b"
	agC = "aaaaaaaa-0000-4000-8000-00000000000c"
	agD = "aaaaaaaa-0000-4000-8000-00000000000d"
	agE = "aaaaaaaa-0000-4000-8000-00000000000e"
	agF = "aaaaaaaa-0000-4000-8000-00000000000f"
	agG = "aaaaaaaa-0000-4000-8000-000000000010"
)

func writeAntigravityConversation(t *testing.T, base, id string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(base, "conversations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".db")
	if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeAntigravityJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func antigravityFixture(t *testing.T) (base string, now time.Time) {
	t.Helper()
	base = t.TempDir()
	now = time.Now()
	for i, id := range []string{agA, agB, agC, agE, agF, agG} {
		writeAntigravityConversation(t, base, id, now.Add(-time.Duration(i+1)*time.Hour))
	}
	writeAntigravityConversation(t, base, agD, now.Add(-40*24*time.Hour))

	db, err := sql.Open("sqlite", filepath.Join(base, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `conversation_summaries` (`conversation_id` text,`title` text NOT NULL DEFAULT \"\",`preview` text NOT NULL DEFAULT \"\"," +
		"`step_count` integer NOT NULL DEFAULT 0,`last_modified_time` datetime NOT NULL,`workspace_uris` text NOT NULL,`status` text NOT NULL DEFAULT \"\"," +
		"`parent_conversation_id` text NOT NULL DEFAULT \"\",`nesting_depth` integer NOT NULL DEFAULT 0)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, title, uris, parent string }{
		{agA, "Fix the crash on startup", `["file:///w/app"]`, ""},
		{agB, "", "", ""},
		{agF, "A sub-conversation", `["file:///w/app"]`, agA},
	} {
		if _, err := db.Exec(`INSERT INTO conversation_summaries (conversation_id, title, preview, last_modified_time, workspace_uris, parent_conversation_id)
			VALUES (?, ?, 'PREVIEW NEVER LEAVES', '2026-11-19 10:12:13.123456+01:00', ?, ?)`, r.id, r.title, r.uris, r.parent); err != nil {
			t.Fatal(err)
		}
	}
	writeAntigravityJSON(t, filepath.Join(base, "cache", "conversation_metadata.json"), map[string]any{"conversations": map[string]any{
		agC: map[string]any{"summary": map[string]any{"Title": "From the cache", "WorkspaceURIs": []string{"file:///w/c"}, "UpdatedAt": "2026-11-19T09:00:00.000000Z"}, "is_internal": false},
		agE: map[string]any{"summary": map[string]any{"Title": "Internal"}, "is_internal": true},
	}})
	writeAntigravityJSON(t, filepath.Join(base, "cache", "last_conversations.json"), map[string]string{"/w/b": agB})
	history := strings.Join([]string{
		`{"display":"first prompt for b","timestamp":1795000000000,"workspace":"/w/b","conversationId":"` + agB + `"}`,
		`{"display":"/model","timestamp":1795000000001,"workspace":"/w/b","conversationId":"` + agB + `","type":"slash_command"}`,
		`{"display":"latest prompt for b","timestamp":1795000000002,"workspace":"/w/b","conversationId":"` + agB + `"}`,
		`{"display":"a prompt of no conversation","timestamp":1795000000003,"workspace":"/w/x"}`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(base, "history.jsonl"), []byte(history+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base, now
}

// Conversations are listed newest first with the title Antigravity shows
// (its summaries, else its metadata cache), else the newest prompt history
// names for them, else none; sub-conversations, internal and old ones are
// not listed, and nothing of the conversation leaves.
func TestAntigravityDiscoveryTitlesFallBack(t *testing.T) {
	base, _ := antigravityFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := discoverAntigravitySessions(context.Background(), base, newCliSessionDiscoverRequest(30, 500))
	byID := map[string]cliSessionRow{}
	order := []string{}
	for _, r := range res.Rows {
		byID[r.SessionID] = r
		order = append(order, r.SessionID)
	}
	if strings.Join(order, ",") != strings.Join([]string{agA, agB, agC, agG}, ",") {
		t.Fatalf("rows = %v, want A, B, C, G newest first (no old, internal or nested one)", order)
	}
	if a := byID[agA]; a.Title != "Fix the crash on startup" || a.TitleSource != cliSessionTitleSavedTitle || a.Cwd != filepath.FromSlash("/w/app") || !a.Resumable || a.TitleSavedAt == 0 {
		t.Fatalf("A = %+v", a)
	}
	if b := byID[agB]; b.Title != "latest prompt for b" || b.TitleSource != cliSessionTitleLastPrompt || b.Cwd != "/w/b" {
		t.Fatalf("B = %+v, want the newest prompt history names for it", b)
	}
	if c := byID[agC]; c.Title != "From the cache" || c.TitleSource != cliSessionTitleSavedTitle || c.Cwd != filepath.FromSlash("/w/c") {
		t.Fatalf("C = %+v", c)
	}
	if g := byID[agG]; g.Title != "" || g.TitleSource != cliSessionTitleNone || !g.Resumable {
		t.Fatalf("G = %+v, want listed with no title", g)
	}
	if res.Truncated || len(res.SourcesSkipped) != 0 {
		t.Fatalf("result = %+v", res)
	}
	raw, _ := json.Marshal(res)
	for _, leaked := range []string{"PREVIEW", "first prompt for b", "/model", "Internal", "sub-conversation"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("%q left the computer: %s", leaked, raw)
		}
	}
	short := discoverAntigravitySessions(context.Background(), base, newCliSessionDiscoverRequest(30, 2))
	if len(short.Rows) != 2 || !short.Truncated {
		t.Fatalf("maxRows 2 = %+v, want 2 rows, truncated", short)
	}
}

func TestAntigravityDiscoveryReportsMissingAndUnrecognisedStores(t *testing.T) {
	req := newCliSessionDiscoverRequest(30, 500)
	if res := discoverAntigravitySessions(context.Background(), filepath.Join(t.TempDir(), "absent"), req); skipReason(res, cliSessionSourceAntigravityStore) != cliSessionSkipNotFound {
		t.Fatalf("no Antigravity home = %+v", res)
	}
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "conversations", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := discoverAntigravitySessions(context.Background(), base, req); skipReason(res, cliSessionSourceAntigravityStore) != cliSessionSkipUnrecognised {
		t.Fatalf("a store of no known conversation = %+v, want unrecognised", res)
	}
	// Known conversations, all older than the look-back: none to list, and
	// nothing unrecognised about the store.
	writeAntigravityConversation(t, base, agA, time.Now().Add(-90*24*time.Hour))
	if res := discoverAntigravitySessions(context.Background(), base, req); len(res.SourcesSkipped) != 0 || len(res.Rows) != 0 {
		t.Fatalf("a store of old conversations = %+v, want a clean, empty read", res)
	}
}

func TestAntigravityProbeAndWriterEvidence(t *testing.T) {
	base, now := antigravityFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{{PID: 71, Name: "agy", Args: []string{"agy", "--conversation", agA, "--output-format", "stream-json"}}}
	})
	res := probeAntigravitySession(context.Background(), base, cliSessionProbeRequest{sessionID: agA})
	if !res.Exists || !res.Resumable || res.Title != "Fix the crash on startup" || len(res.Writers) != 1 || res.Writers[0].PID != 71 {
		t.Fatalf("probe = %+v", res)
	}
	if got := time.UnixMilli(res.TranscriptWrittenAt); got.Sub(now.Add(-time.Hour)).Abs() > time.Second {
		t.Fatalf("transcriptWrittenAt = %v", got)
	}
	if res := probeAntigravitySession(context.Background(), base, cliSessionProbeRequest{sessionID: agD + "0"}); res.Error != cliSessionErrBadRequest {
		t.Fatalf("a malformed id = %+v", res)
	}
	// The write-ahead log counts as a write to the conversation.
	db := antigravityConversationDB(base, agA)
	if err := os.WriteFile(db+"-wal", []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := antigravityConversationWrittenAt(db); time.Since(time.UnixMilli(got)) > 5*time.Second {
		t.Fatalf("written at %v, want the fresh -wal", time.UnixMilli(got))
	}
}
