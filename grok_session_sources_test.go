package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	gkS1 = "11111111-2222-4333-8444-000000000001"
	gkS2 = "11111111-2222-4333-8444-000000000002"
	gkS3 = "11111111-2222-4333-8444-000000000003"
	gkS4 = "11111111-2222-4333-8444-000000000004"
)

// writeGrokSession writes one session folder the way Grok Build does:
// summary.json (when summary is set) and updates.jsonl's ACP updates, every
// file modified at mod.
func writeGrokSession(t *testing.T, home, cwd, id string, mod time.Time, summary string, updates ...map[string]any) string {
	t.Helper()
	folder := filepath.Join(home, "sessions", grokSessionCwdDirName(cwd), id)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if summary != "-" {
		raw, _ := json.Marshal(map[string]any{
			"info": map[string]any{"id": id, "cwd": cwd}, "session_summary": summary,
			"created_at": mod.UTC().Format(time.RFC3339Nano), "updated_at": mod.UTC().Format(time.RFC3339Nano),
		})
		if err := os.WriteFile(filepath.Join(folder, "summary.json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	for _, u := range updates {
		line, _ := json.Marshal(map[string]any{"method": "session/update", "timestamp": mod.Unix(),
			"params": map[string]any{"sessionId": id, "update": u}})
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(folder, "updates.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(folder)
	for _, e := range entries {
		_ = os.Chtimes(filepath.Join(folder, e.Name()), mod, mod)
	}
	_ = os.Chtimes(folder, mod, mod)
	return folder
}

func grokUserChunk(text string) map[string]any {
	return map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": text}}
}

func grokAgentChunk(text string) map[string]any {
	return map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}
}

// writeGrokSearchIndex writes sessions/session_search.sqlite's session_docs.
func writeGrokSearchIndex(t *testing.T, home string, docs map[string][2]string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "sessions", "session_search.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session_docs (session_id TEXT PRIMARY KEY, cwd TEXT NOT NULL, updated_at INTEGER NOT NULL,
		title TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL, last_indexed_offset INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	for id, d := range docs {
		if _, err := db.Exec(`INSERT INTO session_docs VALUES (?, ?, ?, ?, 'CONTENT NEVER LEAVES', 'h', 0)`, id, d[0], time.Now().Unix(), d[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func grokFixture(t *testing.T) (home string, now time.Time) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("GROK_HOME", home)
	now = time.Now()
	writeGrokSession(t, home, "/w/app", gkS1, now.Add(-time.Hour), "Refactor the billing module",
		grokUserChunk("Please split billing.go into smaller files"), grokAgentChunk("AGENT TEXT NEVER LEAVES"))
	writeGrokSession(t, home, "/w/web", gkS2, now.Add(-2*time.Hour), "Add a search bar to the…",
		grokUserChunk("Add a search bar to the header"), grokAgentChunk("ok"))
	writeGrokSession(t, home, "/w/cli", gkS3, now.Add(-3*time.Hour), "-",
		grokUserChunk("split "), grokUserChunk("chunks"), grokAgentChunk("ok"))
	writeGrokSession(t, home, "/w/old", gkS4, now.Add(-40*24*time.Hour), "Too old", grokUserChunk("old"))
	if err := os.MkdirAll(filepath.Join(home, "sessions", grokSessionCwdDirName("/w/app"), "not-a-session"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGrokSearchIndex(t, home, map[string][2]string{gkS3: {"/w/cli", "Indexed title"}})
	return home, now
}

// Folders are listed newest first: the title Grok shows when it is a title,
// the first prompt when Grok's title is only that prompt's start, and the
// search index's title when the folder has no summary; nothing else of the
// conversation leaves.
func TestGrokDiscoveryReadsTheStore(t *testing.T) {
	grokFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := runCliSessionDiscover(context.Background(), grokSessionReader, newCliSessionDiscoverRequest(30, 500))
	if len(res.Rows) != 3 || res.Rows[0].SessionID != gkS1 || res.Rows[1].SessionID != gkS2 || res.Rows[2].SessionID != gkS3 {
		t.Fatalf("rows = %+v, want S1, S2, S3 newest first", res.Rows)
	}
	s1, s2, s3 := res.Rows[0], res.Rows[1], res.Rows[2]
	if s1.Title != "Refactor the billing module" || s1.TitleSource != cliSessionTitleSavedTitle || s1.Cwd != "/w/app" || !s1.Resumable {
		t.Fatalf("S1 = %+v", s1)
	}
	if s2.Title != "Add a search bar to the header" || s2.TitleSource != cliSessionTitleFirstPrompt {
		t.Fatalf("S2 = %+v, want the prompt Grok's title was cut from", s2)
	}
	if s3.Title != "Indexed title" || s3.TitleSource != cliSessionTitleSavedTitle || s3.Cwd != "/w/cli" {
		t.Fatalf("S3 = %+v, want the index title and the folder's own cwd", s3)
	}
	if res.Engine != cliSessionEngineGrok || s1.Engine != cliSessionEngineGrok || res.Truncated || len(res.SourcesSkipped) != 0 {
		t.Fatalf("result = %+v", res)
	}
	raw, _ := json.Marshal(res)
	for _, leaked := range []string{"AGENT TEXT", "CONTENT NEVER", "split billing.go", "Too old"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("%q left the computer: %s", leaked, raw)
		}
	}
}

func TestGrokDiscoveryReportsMissingAndUnrecognisedStores(t *testing.T) {
	req := newCliSessionDiscoverRequest(30, 500)
	if res := discoverGrokSessions(context.Background(), filepath.Join(t.TempDir(), "absent"), req); skipReason(res, cliSessionSourceGrokStore) != cliSessionSkipNotFound {
		t.Fatalf("no Grok home = %+v, want notFound", res)
	}
	home := t.TempDir()
	folder := filepath.Join(home, "sessions", grokSessionCwdDirName("/w"), gkS1)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "updates.jsonl"), []byte(`{"some":"future shape"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := discoverGrokSessions(context.Background(), home, req); skipReason(res, cliSessionSourceGrokStore) != cliSessionSkipUnrecognised || len(res.Rows) != 0 {
		t.Fatalf("a folder naming no session = %+v, want unrecognised", res)
	}
}

func TestGrokProbeReportsTheSessionAndItsWriters(t *testing.T) {
	home, now := grokFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{{PID: 61, Name: "grok", Args: []string{"grok", "--resume", gkS1}}}
	})
	res := probeGrokSession(context.Background(), home, cliSessionProbeRequest{sessionID: gkS1, cwd: "/w/app"})
	if !res.Exists || !res.Resumable || res.Title != "Refactor the billing module" || len(res.Writers) != 1 || res.Writers[0].PID != 61 {
		t.Fatalf("probe = %+v", res)
	}
	if got := time.UnixMilli(res.TranscriptWrittenAt); got.Sub(now.Add(-time.Hour)).Abs() > time.Second {
		t.Fatalf("transcriptWrittenAt = %v", got)
	}
	if res := probeGrokSession(context.Background(), home, cliSessionProbeRequest{sessionID: "11111111-2222-4333-8444-0000000000ff"}); res.Exists || res.Resumable {
		t.Fatalf("an unknown session = %+v", res)
	}
}

// The probe never reads a session as gone, or as one that does not load,
// from a store it could not read: a folder whose updates.jsonl is there but
// could not be read (with a summary or without), or no folder found where a
// cwd folder of the store could not be listed, answers an error, which
// terminal-service reads as a session it could not check. A readable folder
// is still confirmed, and a store read in full without the session still
// says gone; a listing skips the folder it could not read, as it skips any.
func TestGrokProbeOfAFolderItCouldNotReadNeverSaysGone(t *testing.T) {
	home, now := grokFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	const (
		bare       = "11111111-2222-4333-8444-0000000000a1"
		summarised = "11111111-2222-4333-8444-0000000000a2"
		hidden     = "11111111-2222-4333-8444-0000000000a3"
		absent     = "11111111-2222-4333-8444-0000000000ff"
	)
	probe := func(id, cwd string) cliSessionProbeResult {
		return probeGrokSession(context.Background(), home, cliSessionProbeRequest{sessionID: id, cwd: cwd})
	}
	unknown := func(what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != cliSessionErrInternal || res.Exists || res.Resumable {
			t.Fatalf("%s = %+v, want an error (not checked), never gone or not resumable", what, res)
		}
	}
	confirmed := func(what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != "" || !res.Exists || !res.Resumable || res.Title != "Refactor the billing module" {
			t.Fatalf("%s = %+v, want confirmed", what, res)
		}
	}

	putUnreadableFile(t, filepath.Join(home, "sessions", grokSessionCwdDirName("/w/bare"), bare, "updates.jsonl"))
	unknown("a session whose folder could not be read", probe(bare, "/w/bare"))
	unknown("a session whose folder could not be read, found without its cwd", probe(bare, ""))
	folder := writeGrokSession(t, home, "/w/sum", summarised, now, "Has a summary", grokUserChunk("summarised"))
	if err := os.Remove(filepath.Join(folder, "updates.jsonl")); err != nil {
		t.Fatal(err)
	}
	putUnreadableFile(t, filepath.Join(folder, "updates.jsonl"))
	unknown("a session whose summary reads but whose updates.jsonl could not be read", probe(summarised, "/w/sum"))
	confirmed("a session a readable folder holds", probe(gkS1, "/w/app"))
	if res := probe(absent, ""); res.Error != "" || res.Exists || res.Resumable {
		t.Fatalf("a session a store read in full does not hold = %+v, want gone", res)
	}
	res := discoverGrokSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 500))
	if len(res.Rows) != 3 || res.Rows[0].SessionID != gkS1 || res.Rows[1].SessionID != gkS2 || res.Rows[2].SessionID != gkS3 || len(res.SourcesSkipped) != 0 {
		t.Fatalf("discovery = %+v, want S1, S2, S3 and neither folder it could not read", res)
	}

	t.Run("a cwd folder it could not list", func(t *testing.T) {
		kept := writeGrokSession(t, home, "/w/hidden", hidden, now, "Hidden", grokUserChunk("kept where it cannot be listed"))
		makeUnlistable(t, filepath.Dir(kept))
		unknown("a session kept in a cwd folder that could not be listed", probe(hidden, ""))
		unknown("a session whose own cwd folder could not be listed", probe(hidden, "/w/hidden"))
		unknown("a session no readable folder holds, beside one that could not be listed", probe(absent, ""))
		confirmed("a session a readable folder holds", probe(gkS1, ""))
	})
}

// The cwd folder name is Grok's own percent-encoding (every byte but the
// unreserved ones), on both platforms' paths.
func TestGrokSessionCwdDirNameMatchesGrok(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/repo":       "%2FUsers%2Fme%2Frepo",
		`C:\aiexpedite`:        "C%3A%5Caiexpedite",
		"/w/my app+v2/x_y.z~1": "%2Fw%2Fmy%20app%2Bv2%2Fx_y.z~1",
	} {
		if got := grokSessionCwdDirName(in); got != want {
			t.Errorf("grokSessionCwdDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// shortGrokCwd is a working directory SHORT enough that the store path built
// from it — the encoded cwd segment triples every separator — stays inside
// MAX_PATH. t.TempDir() does not: its encoded form pushed the link past 260
// characters, where neither cmd nor PowerShell can see the path, so these
// tests only ever passed through os.Symlink on an elevated runner and never
// exercised the junction an ordinary machine uses (grok_session_store.go).
// The cwd itself need not exist; only its name is used.
func shortGrokCwd(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `C:\gk`
	}
	return "/gk"
}

func isDirLink(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// Adopting one Grok session links THAT folder into AI Expedite's store, in
// place: the load sees it, the turns land in the person's own transcript,
// nothing is copied, and no other session of theirs becomes visible.
func TestLinkAdoptedGrokSessionLinksOneFolderInPlace(t *testing.T) {
	realHome := t.TempDir()
	store := withTempGrokSessionStore(t)
	cwd := shortGrokCwd(t)
	writeGrokSession(t, realHome, cwd, gkS1, time.Now(), "Adopted", grokUserChunk("adopt me"))
	writeGrokSession(t, realHome, cwd, gkS2, time.Now(), "Not adopted", grokUserChunk("leave me"))
	writeGrokSession(t, realHome, "/elsewhere", gkS3, time.Now(), "Other folder", grokUserChunk("other"))

	linked, err := linkAdoptedGrokSessionIn(store, realHome, gkS1, cwd)
	if err != nil || !linked {
		t.Fatalf("link = %v, %v", linked, err)
	}
	link := filepath.Join(store, grokSessionCwdDirName(cwd), gkS1)
	if !isDirLink(t, link) {
		t.Fatal("the adopted folder was not linked (a copy, or nothing)")
	}
	f, err := os.OpenFile(filepath.Join(link, "updates.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("the load cannot see the adopted session: %v", err)
	}
	_, _ = f.WriteString(`{"from":"ai-expedite"}` + "\n")
	_ = f.Close()
	real, _ := os.ReadFile(filepath.Join(realHome, "sessions", grokSessionCwdDirName(cwd), gkS1, "updates.jsonl"))
	if !strings.Contains(string(real), "ai-expedite") {
		t.Fatal("a turn written through the link did not land in the person's own transcript")
	}
	if _, err := os.Lstat(filepath.Join(store, grokSessionCwdDirName(cwd), gkS2)); !os.IsNotExist(err) {
		t.Fatal("a session that was not adopted became visible in AI Expedite's store")
	}

	if again, _ := linkAdoptedGrokSessionIn(store, realHome, gkS1, cwd); again {
		t.Fatal("a second load linked again")
	}
	if l, _ := linkAdoptedGrokSessionIn(store, realHome, gkS3, cwd); l {
		t.Fatal("a session recorded for another folder was linked under this one")
	}
	if l, _ := linkAdoptedGrokSessionIn(store, realHome, gkS4, cwd); l {
		t.Fatal("a session the person's store does not hold was linked")
	}
	// AI Expedite's own session under the same name is never touched.
	own := filepath.Join(store, grokSessionCwdDirName(cwd), gkS2)
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "updates.jsonl"), []byte("own\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, _ := linkAdoptedGrokSessionIn(store, realHome, gkS2, cwd); l || isDirLink(t, own) {
		t.Fatal("an entry already in the store was replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(own, "updates.jsonl")); string(b) != "own\n" {
		t.Fatal("AI Expedite's own session was changed")
	}
}

// The link is made where it matters: right before a Grok `session/load` is
// written, for that load's session and cwd, and for no other agent's frame.
func TestPrepareACPResumeFrameLinksBeforeAGrokLoad(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("GROK_HOME", realHome)
	store := withTempGrokSessionStore(t)
	cwd := shortGrokCwd(t)
	writeGrokSession(t, realHome, cwd, gkS1, time.Now(), "Adopted", grokUserChunk("adopt me"))
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "session/load",
		"params": map[string]any{"sessionId": gkS1, "cwd": cwd, "mcpServers": []any{}}})
	frame := string(raw)
	prepareACPResumeFrame(openCodeACPProfile, frame)
	link := filepath.Join(store, grokSessionCwdDirName(cwd), gkS1)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("another agent's frame linked a Grok session")
	}
	prepareACPResumeFrame(grokACPProfile, frame)
	if !isDirLink(t, link) {
		t.Fatal("a Grok session/load of an adopted session linked nothing")
	}
}

// The store's 14-day prune removes an expired adopted link and never the
// person's folder behind it; a link to a folder still in use stays.
func TestPruneUnlinksAnExpiredAdoptedLinkAndKeepsItsFolder(t *testing.T) {
	realHome := t.TempDir()
	store := withTempGrokSessionStore(t)
	cwd := shortGrokCwd(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	folder := writeGrokSession(t, realHome, cwd, gkS1, old, "Old adopted", grokUserChunk("adopt me"))
	fresh := writeGrokSession(t, realHome, cwd, gkS2, time.Now(), "Fresh adopted", grokUserChunk("adopt me too"))
	for _, id := range []string{gkS1, gkS2} {
		if ok, err := linkAdoptedGrokSessionIn(store, realHome, id, cwd); !ok || err != nil {
			t.Fatalf("link %s = %v, %v", id, ok, err)
		}
	}
	if _, err := pruneGrokSessionStoreAt(store, grokSessionStoreMaxAge, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(store, grokSessionCwdDirName(cwd), gkS1)); !os.IsNotExist(err) {
		t.Fatal("the expired adopted link was kept")
	}
	if _, err := os.Stat(filepath.Join(folder, "updates.jsonl")); err != nil {
		t.Fatalf("the prune deleted the person's own session folder: %v", err)
	}
	if !isDirLink(t, filepath.Join(store, grokSessionCwdDirName(cwd), gkS2)) {
		t.Fatal("a link to a session still in use was pruned")
	}
	if _, err := os.Stat(filepath.Join(fresh, "updates.jsonl")); err != nil {
		t.Fatal(err)
	}
}

// A listing the budget cuts short still answers the sessions it listed
// (truncated): their folders are read under the reader's own budget.
func TestGrokListingCutShortStillAnswersWhatItListed(t *testing.T) {
	home, _ := grokFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	pinCliSessionListing(t, 1) // the listing's budget ends after the first folder (/w/app)
	res := discoverGrokSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 500))
	if len(res.Rows) != 1 || res.Rows[0].SessionID != gkS1 || res.Rows[0].Title != "Refactor the billing module" {
		t.Fatalf("cut listing rows = %+v, want /w/app's session, read", res.Rows)
	}
	if !res.Truncated || skipReason(res, cliSessionSourceGrokStore) != cliSessionSkipTimeout {
		t.Fatalf("cut listing = %+v, want truncated and timed out", res)
	}
}
