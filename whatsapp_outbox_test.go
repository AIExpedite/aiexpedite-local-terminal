package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// withWhatsAppTestEnv points the WhatsApp root at a temp dir and installs a
// memory keystore.
func withWhatsAppTestEnv(t *testing.T) (*memoryWhatsAppKeystore, string) {
	t.Helper()
	root := t.TempDir()
	prevRoot := whatsAppRootDirFn
	whatsAppRootDirFn = func() string { return root }
	ks := newMemoryWhatsAppKeystore()
	restore := setWhatsAppKeystoreForTest(ks)
	t.Cleanup(func() {
		whatsAppRootDirFn = prevRoot
		restore()
	})
	return ks, root
}

func textRecord(chat, id, text string) whatsAppRecord {
	return whatsAppRecord{Kind: waRecordMessage, Op: waOpNew, ChatJid: chat, SenderJid: chat, WaMessageID: id, WaTimestamp: 1, Source: waSourceLive, Type: waTypeText, Text: text}
}

func openTestOutbox(t *testing.T, linkID string) (*whatsAppOutbox, string) {
	t.Helper()
	dir, err := ensureWhatsAppLinkDir(linkID)
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := openWhatsAppOutbox(dir, linkID)
	if err != nil {
		t.Fatal(err)
	}
	return ob, dir
}

func TestWhatsAppOutboxRowsAreCiphertext(t *testing.T) {
	ks, _ := withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_enc")
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "A", "very secret words")}, false); err != nil {
		t.Fatal(err)
	}
	ob.Close()

	db, err := sql.Open("sqlite", filepath.Join(dir, whatsAppOutboxDBName))
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM outbox`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if bytes.Contains(payload, []byte("very secret")) || bytes.Contains(payload, []byte("s.whatsapp.net")) {
		t.Fatal("outbox row holds plaintext")
	}
	if _, err := ks.Get("lnk_enc"); err != nil {
		t.Fatal("outbox key not in the keystore")
	}

	// Without the key the rows are unreadable: reopening discards them rather
	// than re-keying silently over unreadable data.
	_ = ks.Delete("lnk_enc")
	ob2, discarded, err := openWhatsAppOutbox(dir, "lnk_enc")
	if err != nil {
		t.Fatal(err)
	}
	defer ob2.Close()
	if !discarded || ob2.Depth() != 0 {
		t.Fatalf("discarded=%v depth=%d", discarded, ob2.Depth())
	}
}

func TestWhatsAppOutboxRowFromAnotherLinkDoesNotOpen(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_aad")
	defer ob.Close()
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "A", "x")}, false); err != nil {
		t.Fatal(err)
	}
	// Re-binding the row to another sequence breaks the AAD.
	if _, err := ob.db.Exec(`UPDATE outbox SET seq = 99`); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Next(); err == nil {
		t.Fatal("a row moved to another sequence opened")
	}
}

func TestWhatsAppOutboxResendAfterCrashAndAck(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_crash")
	recs := []whatsAppRecord{textRecord("1@s.whatsapp.net", "A", "one"), textRecord("1@s.whatsapp.net", "B", "two"), textRecord("1@s.whatsapp.net", "C", "three")}
	if err := ob.Append(recs, false); err != nil {
		t.Fatal(err)
	}
	ob.Close() // "crash" before any upload

	ob, _, err := openWhatsAppOutbox(dir, "lnk_crash")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := ob.Next()
	if err != nil || batch == nil {
		t.Fatalf("next: %v %v", batch, err)
	}
	if batch.SeqFrom != 1 || batch.SeqTo != 3 || len(batch.Records) != 3 {
		t.Fatalf("batch %d..%d (%d)", batch.SeqFrom, batch.SeqTo, len(batch.Records))
	}
	var first whatsAppRecord
	_ = json.Unmarshal(batch.Records[0], &first)
	if first.DeviceSeq != 1 || first.Text != "one" {
		t.Fatalf("record = %+v", first)
	}
	if err := ob.Ack(2); err != nil {
		t.Fatal(err)
	}
	batch, _ = ob.Next()
	if batch.SeqFrom != 3 || batch.SeqTo != 3 {
		t.Fatalf("after partial ack: %d..%d", batch.SeqFrom, batch.SeqTo)
	}
	if err := ob.Ack(3); err != nil {
		t.Fatal(err)
	}
	if b, _ := ob.Next(); b != nil || ob.Depth() != 0 {
		t.Fatal("acked rows still present")
	}
	// Sequences are never reused after the table empties — not even across a restart.
	ob.Close()
	ob, _, _ = openWhatsAppOutbox(dir, "lnk_crash")
	defer ob.Close()
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "D", "four")}, false); err != nil {
		t.Fatal(err)
	}
	b, _ := ob.Next()
	if b.SeqFrom != 4 {
		t.Fatalf("sequence reused: %d", b.SeqFrom)
	}
}

func TestWhatsAppOutboxHistoryPausesAtCapLiveStillAccepted(t *testing.T) {
	withWhatsAppTestEnv(t)
	prev := whatsAppOutboxMaxRecords
	whatsAppOutboxMaxRecords = 2
	defer func() { whatsAppOutboxMaxRecords = prev }()
	ob, _ := openTestOutbox(t, "lnk_cap")
	defer ob.Close()
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "A", "h1"), textRecord("1@s.whatsapp.net", "B", "h2")}, true); err != nil {
		t.Fatal(err)
	}
	if ob.HasRoomForHistory() {
		t.Fatal("cap not reached")
	}
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "C", "h3")}, true); err != errWhatsAppOutboxFull {
		t.Fatalf("history past cap: %v", err)
	}
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "D", "live")}, false); err != nil {
		t.Fatalf("live refused at cap: %v", err)
	}
	if ob.Depth() != 3 {
		t.Fatalf("depth %d", ob.Depth())
	}
}

// A history chunk that would push the queue past a cap is refused whole:
// nothing is stored, so the supervisor's retry replays the same chunk once
// the uploader makes room.
func TestWhatsAppOutboxHistoryChunkPastCapRefusedWhole(t *testing.T) {
	withWhatsAppTestEnv(t)
	prevRec, prevBytes := whatsAppOutboxMaxRecords, whatsAppOutboxMaxBytes
	whatsAppOutboxMaxRecords = 4
	defer func() { whatsAppOutboxMaxRecords, whatsAppOutboxMaxBytes = prevRec, prevBytes }()
	ob, _ := openTestOutbox(t, "lnk_overshoot")
	defer ob.Close()
	queued := []whatsAppRecord{textRecord("1@s.whatsapp.net", "A", "h1"), textRecord("1@s.whatsapp.net", "B", "h2"), textRecord("1@s.whatsapp.net", "C", "h3")}
	if err := ob.Append(queued, true); err != nil {
		t.Fatal(err)
	}
	// Three queued and three incoming would make six: the whole chunk is
	// refused even though three alone is under the cap of four.
	chunk := []whatsAppRecord{textRecord("1@s.whatsapp.net", "D", "h4"), textRecord("1@s.whatsapp.net", "E", "h5"), textRecord("1@s.whatsapp.net", "F", "h6")}
	if err := ob.Append(chunk, true); !errors.Is(err, errWhatsAppOutboxFull) {
		t.Fatalf("overshooting history chunk: %v", err)
	}
	if ob.Depth() != 3 {
		t.Fatalf("a refused chunk stored rows: depth %d", ob.Depth())
	}
	// The same chunk lands once the uploader makes room for all of it.
	if err := ob.Ack(2); err != nil {
		t.Fatal(err)
	}
	if err := ob.Append(chunk, true); err != nil {
		t.Fatalf("chunk fitting after an ack: %v", err)
	}
	if ob.Depth() != 4 {
		t.Fatalf("depth %d", ob.Depth())
	}
	// The byte cap counts the same way: with record room to spare, a chunk
	// past the byte cap is still refused whole.
	whatsAppOutboxMaxRecords, whatsAppOutboxMaxBytes = 100, 1
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "G", "h7")}, true); !errors.Is(err, errWhatsAppOutboxFull) {
		t.Fatalf("history past the byte cap: %v", err)
	}
	if ob.Depth() != 4 {
		t.Fatalf("a refused chunk stored rows: depth %d", ob.Depth())
	}
	if err := ob.Append([]whatsAppRecord{textRecord("1@s.whatsapp.net", "H", "live")}, false); err != nil {
		t.Fatalf("live refused at the byte cap: %v", err)
	}
}

func TestWhatsAppOutboxBatchRespectsByteBudget(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_budget")
	defer ob.Close()
	big := string(bytes.Repeat([]byte("x"), waMaxTextChars))
	var recs []whatsAppRecord
	for i := 0; i < 60; i++ {
		recs = append(recs, textRecord("1@s.whatsapp.net", fmt.Sprintf("M%d", i), big))
	}
	if err := ob.Append(recs, true); err != nil {
		t.Fatal(err)
	}
	batch, _ := ob.Next()
	total := 0
	for _, r := range batch.Records {
		total += len(r)
	}
	if total > whatsAppBatchRecordsBudget || len(batch.Records) == 60 {
		t.Fatalf("batch of %d records / %d bytes exceeds the budget", len(batch.Records), total)
	}
}
