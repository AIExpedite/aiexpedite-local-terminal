package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	cxA = "0199a1b2-0000-7000-8000-00000000000a"
	cxB = "0199a1b2-0000-7000-8000-00000000000b"
	cxC = "0199a1b2-0000-7000-8000-00000000000c"
	cxD = "0199a1b2-0000-7000-8000-00000000000d"
	cxE = "0199a1b2-0000-7000-8000-00000000000e"
)

// writeCodexSessionRollout writes sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl
// under home: the session_meta header, then records, modified at mod.
func writeCodexSessionRollout(t *testing.T, home, id string, started, mod time.Time, meta map[string]any, records ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", started.Format("2006"), started.Format("01"), started.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"id": id, "timestamp": started.UTC().Format(time.RFC3339Nano), "cwd": "/w/api",
		"originator": "codex_cli_rs", "source": "cli", "cli_version": "0.150.0",
		"base_instructions": map[string]any{"text": "BASE INSTRUCTIONS NEVER LEAVE"},
	}
	for k, v := range meta {
		payload[k] = v
	}
	var b strings.Builder
	line, _ := json.Marshal(map[string]any{"timestamp": started.UTC().Format(time.RFC3339Nano), "type": "session_meta", "payload": payload})
	b.Write(line)
	b.WriteByte('\n')
	for _, r := range records {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", started.Format("2006-01-02T15-04-05"), id))
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexUserEvent(text string) map[string]any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": text}}
}

func codexUserItem(text string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": text}},
	}}
}

func codexAssistantItem(text string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": text}},
	}}
}

func codexFixture(t *testing.T) (home string, now time.Time) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now = time.Now()
	day := now.Add(-26 * time.Hour)
	writeCodexSessionRollout(t, home, cxA, day, now.Add(-time.Hour), nil,
		codexUserItem("<environment_context>\n  <cwd>/w/api</cwd>\n</environment_context>"),
		codexUserItem("Fix the flaky login test"),
		codexUserEvent("Fix the flaky login test"),
		codexAssistantItem("ASSISTANT REPLY NEVER LEAVES"))
	writeCodexSessionRollout(t, home, cxB, day, now.Add(-2*time.Hour), map[string]any{"cwd": "/w/web", "source": "vscode"},
		codexUserItem("# AGENTS.md instructions for /w/web"),
		codexUserItem("add a dark mode toggle"))
	writeCodexSessionRollout(t, home, cxC, day, now.Add(-3*time.Hour), map[string]any{"originator": codexOriginatorAIExpedite},
		codexUserEvent("prepare the release"))
	writeCodexSessionRollout(t, home, cxD, day, now.Add(-4*time.Hour),
		map[string]any{"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": cxA}}}},
		codexUserEvent("sub-agent task"))
	writeCodexSessionRollout(t, home, cxE, now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour), nil, codexUserEvent("too old"))
	index := strings.Join([]string{
		`{"id":"` + cxC + `","thread_name":"Old name","updated_at":"2026-11-01T10:00:00.000000Z"}`,
		`{"id":"` + cxC + `","thread_name":"Release checklist","updated_at":"2026-11-02T10:00:00.000000Z"}`,
		`not json`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, now
}

// Rollouts are listed newest first with the thread's cwd, a title (the name
// a person gave it, else its first prompt — never a context block Codex
// wrote), and whether AI Expedite started it; sub-agent threads and old
// ones are not; nothing of the conversation but the masked label leaves.
func TestCodexDiscoveryReadsRolloutsNamesAndPrompts(t *testing.T) {
	codexFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := runCliSessionDiscover(context.Background(), codexSessionReader, newCliSessionDiscoverRequest(30, 500))

	if len(res.Rows) != 3 {
		t.Fatalf("rows = %+v, want A, B and C", res.Rows)
	}
	if res.Rows[0].SessionID != cxA || res.Rows[1].SessionID != cxB || res.Rows[2].SessionID != cxC {
		t.Fatalf("rows not newest first: %s %s %s", res.Rows[0].SessionID, res.Rows[1].SessionID, res.Rows[2].SessionID)
	}
	a, b, c := res.Rows[0], res.Rows[1], res.Rows[2]
	if a.Title != "Fix the flaky login test" || a.TitleSource != cliSessionTitleFirstPrompt || a.Cwd != "/w/api" || !a.Resumable || a.StartedByAiExpedite {
		t.Fatalf("A = %+v", a)
	}
	if b.Title != "add a dark mode toggle" || b.TitleSource != cliSessionTitleFirstPrompt || b.Cwd != "/w/web" {
		t.Fatalf("B = %+v, want the response item's prompt past the AGENTS.md block", b)
	}
	if c.Title != "Release checklist" || c.TitleSource != cliSessionTitleSavedTitle || c.TitleSavedAt == 0 || !c.StartedByAiExpedite {
		t.Fatalf("C = %+v, want its newest name and AI Expedite as its starter", c)
	}
	for _, r := range res.Rows {
		if r.Engine != cliSessionEngineCodex || r.Running != nil {
			t.Fatalf("row = %+v, want engine codex and not running", r)
		}
	}
	if res.Truncated || len(res.SourcesSkipped) != 0 || res.SourcesChecked[0] != cliSessionSourceCodexStore {
		t.Fatalf("result = %+v, want a complete, clean read", res)
	}
	raw, _ := json.Marshal(res)
	for _, leaked := range []string{"BASE INSTRUCTIONS", "ASSISTANT REPLY", "environment_context", "sub-agent task", "too old"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("%q left the computer: %s", leaked, raw)
		}
	}
}

func TestCodexDiscoveryReportsMissingAndUnrecognisedStores(t *testing.T) {
	req := newCliSessionDiscoverRequest(30, 500)
	if res := discoverCodexSessions(context.Background(), filepath.Join(t.TempDir(), "absent"), req); skipReason(res, cliSessionSourceCodexStore) != cliSessionSkipNotFound {
		t.Fatalf("no Codex home = %+v, want notFound", res)
	}
	empty := t.TempDir()
	if res := discoverCodexSessions(context.Background(), empty, req); len(res.SourcesChecked) != 1 || len(res.SourcesSkipped) != 0 || len(res.Rows) != 0 {
		t.Fatalf("a home with no sessions yet = %+v, want checked and empty", res)
	}
	odd := t.TempDir()
	dir := filepath.Join(odd, "sessions", "2026", "11", "19")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-19T10-00-00-"+cxA+".jsonl"), []byte(`{"type":"event_msg","payload":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := discoverCodexSessions(context.Background(), odd, req); skipReason(res, cliSessionSourceCodexStore) != cliSessionSkipUnrecognised || len(res.Rows) != 0 {
		t.Fatalf("a rollout with no session header = %+v, want unrecognised", res)
	}
}

func TestCodexDiscoveryTruncatesAtMaxRowsAndAtTheDeadline(t *testing.T) {
	home, _ := codexFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 1))
	if len(res.Rows) != 1 || res.Rows[0].SessionID != cxA || !res.Truncated {
		t.Fatalf("maxRows 1 = %+v, want the newest row, truncated", res)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	res = discoverCodexSessions(ended, home, newCliSessionDiscoverRequest(30, 500))
	if !res.Truncated || skipReason(res, cliSessionSourceCodexStore) != cliSessionSkipTimeout {
		t.Fatalf("an ended budget = %+v, want truncated and timed out", res)
	}
}

// maxRows counts the threads listed, not the rollouts read: sub-agent
// threads newer than a person's own never take its place, and the answer is
// truncated only when a listable thread is left over.
func TestCodexDiscoveryCapsListedThreadsNotRollouts(t *testing.T) {
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	now := time.Now()
	day := now.Add(-26 * time.Hour)
	spawned := map[string]any{"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": cxA}}}}
	home := t.TempDir()
	writeCodexSessionRollout(t, home, cxB, day, now.Add(-time.Minute), spawned, codexUserEvent("sub-agent task"))
	writeCodexSessionRollout(t, home, cxC, day, now.Add(-2*time.Minute), map[string]any{"thread_source": "subagent"}, codexUserEvent("another sub-agent task"))
	writeCodexSessionRollout(t, home, cxA, day, now.Add(-time.Hour), nil, codexUserEvent("Fix the flaky login test"))

	one := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 1))
	if len(one.Rows) != 1 || one.Rows[0].SessionID != cxA || one.Truncated {
		t.Fatalf("maxRows 1 = %+v, want the person's thread past the newer sub-agents, and complete", one)
	}
	writeCodexSessionRollout(t, home, cxD, day, now.Add(-2*time.Hour), nil, codexUserEvent("an older thread"))
	more := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 1))
	if len(more.Rows) != 1 || more.Rows[0].SessionID != cxA || !more.Truncated {
		t.Fatalf("maxRows 1 with a second thread = %+v, want the newest thread, truncated", more)
	}
	both := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 2))
	if len(both.Rows) != 2 || both.Rows[1].SessionID != cxD || both.Truncated {
		t.Fatalf("maxRows 2 = %+v, want both threads, complete", both)
	}
}

// The probe re-reads one thread: whether it exists and resumes, its title,
// when its rollout was written, and who else holds it — a live `codex`
// naming it, never this agent's own child.
func TestCodexProbeReportsTheThreadAndItsWriters(t *testing.T) {
	home, now := codexFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{
			{PID: 300, Name: "codex", Args: []string{"codex", "resume", cxA}},
			{PID: 301, Name: "codex", Args: []string{"codex", "resume", cxA}, Own: true},
		}
	})
	res := probeCodexSession(home, cliSessionProbeRequest{sessionID: cxA})
	if !res.Exists || !res.Resumable || res.Title != "Fix the flaky login test" || res.Error != "" {
		t.Fatalf("probe = %+v", res)
	}
	if len(res.Writers) != 1 || res.Writers[0].PID != 300 || res.Writers[0].Status != cliSessionRunIdle {
		t.Fatalf("writers = %+v, want only the outside codex, open (idle)", res.Writers)
	}
	if got := time.UnixMilli(res.TranscriptWrittenAt); got.Sub(now.Add(-time.Hour)).Abs() > time.Second {
		t.Fatalf("transcriptWrittenAt = %v, want the rollout's write", got)
	}
	if res := probeCodexSession(home, cliSessionProbeRequest{sessionID: "0199a1b2-0000-7000-8000-0000000000ff"}); res.Exists || res.Resumable || res.Error != "" {
		t.Fatalf("an unknown thread = %+v, want not there", res)
	}
	if res := probeCodexSession(home, cliSessionProbeRequest{sessionID: "../../etc"}); res.Error != cliSessionErrBadRequest {
		t.Fatalf("a malformed id = %+v, want badRequest", res)
	}
}

// The probe never reads a thread as gone from a store it could not read: a
// rollout that is there but could not be read, or no rollout found where a
// folder of the store could not be listed, answers an error, which
// terminal-service reads as a session it could not check. A thread a
// readable rollout holds is still confirmed, and one a store read in full
// does not hold is still gone.
func TestCodexProbeOfARolloutItCouldNotReadNeverSaysGone(t *testing.T) {
	home, now := codexFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	const (
		unread = "0199a1b2-0000-7000-8000-0000000000f1"
		hidden = "0199a1b2-0000-7000-8000-0000000000f2"
		absent = "0199a1b2-0000-7000-8000-0000000000ff"
	)
	probe := func(id string) cliSessionProbeResult {
		return probeCodexSession(home, cliSessionProbeRequest{sessionID: id})
	}
	unknown := func(what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != cliSessionErrInternal || res.Exists || res.Resumable {
			t.Fatalf("%s = %+v, want an error (not checked), never gone or resumable", what, res)
		}
	}
	confirmed := func(what string, res cliSessionProbeResult) {
		t.Helper()
		if res.Error != "" || !res.Exists || !res.Resumable || res.Title != "Fix the flaky login test" {
			t.Fatalf("%s = %+v, want confirmed", what, res)
		}
	}

	started := now.Add(-26 * time.Hour)
	day := filepath.Join(home, "sessions", started.Format("2006"), started.Format("01"), started.Format("02"))
	putUnreadableFile(t, filepath.Join(day, "rollout-"+started.Format("2006-01-02T15-04-05")+"-"+unread+".jsonl"))
	unknown("a thread whose rollout could not be read", probe(unread))
	confirmed("a thread a readable rollout holds, beside one that could not be read", probe(cxA))
	if res := probe(absent); res.Error != "" || res.Exists || res.Resumable {
		t.Fatalf("a thread a store read in full does not hold = %+v, want gone", res)
	}

	t.Run("a folder it could not list", func(t *testing.T) {
		then := now.Add(-5 * 24 * time.Hour)
		path := writeCodexSessionRollout(t, home, hidden, then, then, nil, codexUserEvent("kept where it cannot be listed"))
		makeUnlistable(t, filepath.Dir(path))
		unknown("a thread kept in a folder that could not be listed", probe(hidden))
		unknown("a thread no readable folder holds, beside one that could not be listed", probe(absent))
		confirmed("a thread a readable folder holds", probe(cxA))
	})
}

// The watcher reads the rollout's own write time: within writerQuietMs it is
// a writer mid-turn, unless AI Expedite's own app-server just wrote it.
func TestCodexWriterEvidenceReadsTheRolloutsWrites(t *testing.T) {
	home, _ := codexFixture(t)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	path := findCodexRollout(home, cxB)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	clock := codexWriterEvidence.storeClock(cxB)
	if got := clock(); got/1000 != now.UnixMilli()/1000 {
		t.Fatalf("store clock = %d, want the rollout's mtime %d", got, now.UnixMilli())
	}
	writers := cliSessionOutsideWriters(codexWriterEvidence, cxB, clock, nil, now.Add(time.Second))
	if state, _, _ := claudeOutsideWriterState(writers); state != claudeOutsideWriterBusy {
		t.Fatalf("a rollout written a second ago reads %s, want busy", state)
	}
	own := func(time.Time) time.Time { return now }
	if writers := cliSessionOutsideWriters(codexWriterEvidence, cxB, clock, own, now.Add(time.Second)); len(writers) != 0 {
		t.Fatalf("AI Expedite's own write read as another writer: %+v", writers)
	}
	if codexWriterEvidence.storeClock(cxA+"0")() != 0 {
		t.Fatal("a thread with no rollout has no write time")
	}
}

// Rollouts are listed newest day first — every day, since a resumed thread
// keeps appending to the rollout of the day it started — and a listing the
// budget cuts short still answers the newest threads it listed (truncated):
// the heads are read under the reader's own budget, not the listing's.
func TestCodexListingIsNewestFirstAndACutListingStillAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 10, 0, 0, 0, time.Local) }
	writeCodexSessionRollout(t, home, cxA, day(2026, 11, 19), now.Add(-time.Hour), nil, codexUserEvent("the newest day's thread"))
	writeCodexSessionRollout(t, home, cxB, day(2026, 11, 2), now.Add(-2*time.Hour), nil, codexUserEvent("earlier this month"))
	writeCodexSessionRollout(t, home, cxC, day(2025, 12, 31), now.Add(-30*time.Minute), nil, codexUserEvent("started last year, resumed today"))
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })

	files, complete, err := listCodexRollouts(context.Background(), filepath.Join(home, "sessions"))
	if err != nil || !complete || len(files) != 3 {
		t.Fatalf("listing = %+v complete=%v err=%v, want all three", files, complete, err)
	}
	if files[0].threadID != cxA || files[1].threadID != cxB || files[2].threadID != cxC {
		t.Fatalf("listing order = %s %s %s, want the newest day first", files[0].threadID, files[1].threadID, files[2].threadID)
	}
	full := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 500))
	if len(full.Rows) != 3 || full.Rows[0].SessionID != cxC || full.Truncated {
		t.Fatalf("full read = %+v, want all three, the one resumed today newest", full)
	}

	pinCliSessionListing(t, 1) // the listing's budget ends after the first day
	cut := discoverCodexSessions(context.Background(), home, newCliSessionDiscoverRequest(30, 500))
	if len(cut.Rows) != 1 || cut.Rows[0].SessionID != cxA || cut.Rows[0].Title != "the newest day's thread" {
		t.Fatalf("cut listing rows = %+v, want the newest day's thread, read", cut.Rows)
	}
	if !cut.Truncated || skipReason(cut, cliSessionSourceCodexStore) != cliSessionSkipTimeout {
		t.Fatalf("cut listing = %+v, want truncated and timed out", cut)
	}
}
