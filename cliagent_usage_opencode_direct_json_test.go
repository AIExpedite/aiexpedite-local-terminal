package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_direct_json_test.go — the JSON-file adapter reads
   the storage/message and storage/part shapes OpenCode writes, sums multi-step
   messages, filters by last write, and fails closed on a shape it does not know.
   ------------------------------------------------------------------------ */

func openCodeDirectTestLimits() openCodeDirectLimits {
	return openCodeDirectLimits{MaxSessions: 256, MaxRecords: 8192, MaxRecordBytes: 1 << 20}
}

// Golden records in the shape OpenCode's JSON store holds them: the message
// info under storage/message/<session>, each part under storage/part/<message>.
func TestOpenCodeDirectJSON_ReadsTheStoredShape(t *testing.T) {
	root := t.TempDir()
	store := &openCodeTestStore{t: t, root: root}
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.writeFile(filepath.Join(root, "storage", "message", "ses_g", "msg_g.json"),
		`{"id":"msg_g","sessionID":"ses_g","role":"assistant","parentID":"msg_u","modelID":"claude-sonnet-4-5","providerID":"anthropic","mode":"build","path":{"cwd":"/w","root":"/w"},"cost":0.3,"tokens":{"input":20,"output":5,"reasoning":1,"cache":{"read":100,"write":4}},"time":{"created":1791450000000,"completed":1791450004000},"finish":"stop"}`, at)
	store.writeFile(filepath.Join(root, "storage", "part", "msg_g", "prt_1.json"),
		`{"id":"prt_1","sessionID":"ses_g","messageID":"msg_g","type":"step-finish","reason":"tool-calls","cost":0.1,"tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":50,"write":4}}}`, at)
	store.writeFile(filepath.Join(root, "storage", "part", "msg_g", "prt_2.json"),
		`{"id":"prt_2","sessionID":"ses_g","messageID":"msg_g","type":"step-finish","reason":"stop","cost":0.2,"tokens":{"input":20,"output":5,"reasoning":1,"cache":{"read":100,"write":4}}}`, at)
	store.writeFile(filepath.Join(root, "storage", "part", "msg_g", "prt_3.json"),
		`{"id":"prt_3","sessionID":"ses_g","messageID":"msg_g","type":"text","text":"model output"}`, at)
	store.touchDir(filepath.Join(root, "storage", "message", "ses_g"), at)

	read, err := readOpenCodeDirectJSON(context.Background(), root, at, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	m := read.Messages[0]
	if m.ID != "msg_g" || m.SessionID != "ses_g" || m.CreatedMs != 1791450000000 || m.CompletedMs != 1791450004000 || m.WrittenMs != at || !m.Valid {
		t.Fatalf("message = %+v", m)
	}
	// Summed parts (30/7/1, cache 150/8), max'd with info; cost is info's 0.3.
	if u := m.Usage; u.Input != 30 || u.Output != 7 || u.Reasoning != 1 || u.CacheRead != 150 || u.CacheWrite != 8 || u.Cost < 0.2999 || u.Cost > 0.3001 {
		t.Fatalf("usage = %+v", u)
	}
}

// Only records last written at or after the floor are read, in ascending
// last-write order.
func TestOpenCodeDirectJSON_FiltersByLastWrite(t *testing.T) {
	root := t.TempDir()
	store := &openCodeTestStore{t: t, root: root}
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.write(openCodeTestMessage{session: "ses_old", id: "msg_old", createdMs: base - 7200_000, completedMs: base - 7100_000, steps: [][3]int64{{1, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_b", createdMs: base + 20_000, completedMs: base + 30_000, steps: [][3]int64{{2, 0, 0}}})
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: base, completedMs: base + 10_000, steps: [][3]int64{{3, 0, 0}}})

	read, err := readOpenCodeDirectJSON(context.Background(), root, base, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 2 || read.Messages[0].ID != "msg_a" || read.Messages[1].ID != "msg_b" || read.Truncated {
		t.Fatalf("read = %+v, %v", read, err)
	}
}

// The scan's deadline reaches into a message's parts: a read the budget stops
// there is truncated before that message, never counted from a partial sum.
func TestOpenCodeDirectJSON_TheDeadlineStopsAPartRead(t *testing.T) {
	root := t.TempDir()
	store := &openCodeTestStore{t: t, root: root}
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC).UnixMilli()
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: base, completedMs: base + 10_000, steps: [][3]int64{{3, 0, 0}, {4, 0, 0}}})
	dir := filepath.Join(root, "storage", "part", "msg_a")
	if parts, ok := readOpenCodeDirectJSONParts(context.Background(), dir); !ok || len(parts) != 2 {
		t.Fatalf("parts = %+v, %v", parts, ok)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if parts, ok := readOpenCodeDirectJSONParts(ctx, dir); ok || parts != nil {
		t.Fatalf("a cancelled part read = %+v, %v", parts, ok)
	}
}

// Records, but none in a shape the adapter knows: unknown, never a 0.
func TestOpenCodeDirectJSON_AnUnknownShapeIsUnknown(t *testing.T) {
	root := t.TempDir()
	store := &openCodeTestStore{t: t, root: root}
	at := time.Now().UnixMilli()
	store.writeFile(filepath.Join(root, "storage", "message", "ses_a", "msg_a.json"), `{"messageId":"msg_a","who":"assistant"}`, at)
	if _, err := readOpenCodeDirectJSON(context.Background(), root, 0, openCodeDirectTestLimits()); !errors.Is(err, errOpenCodeDirectLayoutUnknown) {
		t.Fatalf("err = %v, want layout unknown", err)
	}
	// Oversized records are skipped, not read as a layout change.
	big := t.TempDir()
	(&openCodeTestStore{t: t, root: big}).writeFile(filepath.Join(big, "storage", "message", "ses_a", "msg_a.json"), `{"id":"msg_a","pad":"`+strings.Repeat("x", 512)+`"}`, at)
	limits := openCodeDirectTestLimits()
	limits.MaxRecordBytes = 64
	if read, err := readOpenCodeDirectJSON(context.Background(), big, 0, limits); err != nil || read.Skipped != 1 {
		t.Fatalf("oversized-only store = %+v, %v", read, err)
	}
	// An empty store is known and empty.
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "storage", "message"), 0o755); err != nil {
		t.Fatal(err)
	}
	if read, err := readOpenCodeDirectJSON(context.Background(), empty, 0, openCodeDirectTestLimits()); err != nil || len(read.Messages) != 0 {
		t.Fatalf("empty store = %+v, %v", read, err)
	}
}

// A new message file in an existing session directory moves the store marker,
// though the mtime of storage/message itself does not change.
func TestOpenCodeDirectJSON_TheMarkerSeesAMessageInAnExistingSession(t *testing.T) {
	root := t.TempDir()
	store := &openCodeTestStore{t: t, root: root}
	base := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: base.UnixMilli(), completedMs: base.UnixMilli() + 1, steps: [][3]int64{{1, 0, 0}}})
	top := filepath.Join(root, "storage", "message")
	store.touchDir(top, base.UnixMilli())
	before, _ := openCodeStoreMarker(openCodeStoreLayoutJSON, root)
	topInfo, err := os.Stat(top)
	if err != nil {
		t.Fatal(err)
	}

	later := base.Add(time.Minute).UnixMilli()
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_b", createdMs: later, completedMs: later + 1, steps: [][3]int64{{1, 0, 0}}})
	// Linux stamps directories at a coarse kernel tick, so two writes this close
	// can share an mtime (the reader's settle window covers that in
	// production): move the session directory past the first write, and pin
	// storage/message so only the session directory moved.
	sessionDir := filepath.Join(top, "ses_a")
	moved := topInfo.ModTime().Add(time.Minute)
	if info, err := os.Stat(sessionDir); err == nil && info.ModTime().After(moved) {
		moved = info.ModTime().Add(time.Second)
	}
	if err := os.Chtimes(sessionDir, moved, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(top, topInfo.ModTime(), topInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after, _ := openCodeStoreMarker(openCodeStoreLayoutJSON, root); after == before {
		t.Fatalf("marker %q did not move", after)
	}
}
