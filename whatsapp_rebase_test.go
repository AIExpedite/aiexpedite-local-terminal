package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
)

// batchRecords decodes a batch's records.
func batchRecords(t *testing.T, b *whatsAppOutboxBatch) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range b.Records {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// Rebase moves every queued row above the watermark, in order, re-sealed
// under its new sequence with its deviceSeq rewritten and every other field
// kept; `next` follows, and both survive a reopen.
func TestWhatsAppOutboxRebaseRenumbersAboveTheWatermark(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_rebase")
	var records []whatsAppRecord
	for i := 1; i <= 5; i++ {
		records = append(records, textRecord("chat@s.whatsapp.net", "A"+strconv.Itoa(i), "text "+strconv.Itoa(i)))
	}
	if err := ob.Append(records, false); err != nil {
		t.Fatal(err)
	}
	before, err := ob.Next()
	if err != nil || before.SeqFrom != 1 || before.SeqTo != 5 {
		t.Fatalf("before: %+v %v", before, err)
	}
	moved, err := ob.Rebase(100)
	if err != nil || moved != 5 {
		t.Fatalf("Rebase(100) = %d, %v; want 5 rows moved", moved, err)
	}
	after, err := ob.Next()
	if err != nil || after.SeqFrom != 101 || after.SeqTo != 105 {
		t.Fatalf("after: %+v %v", after, err)
	}
	was, now := batchRecords(t, before), batchRecords(t, after)
	for i := range now {
		if now[i]["deviceSeq"].(float64) != float64(101+i) {
			t.Fatalf("row %d carries deviceSeq %v", 101+i, now[i]["deviceSeq"])
		}
		delete(was[i], "deviceSeq")
		delete(now[i], "deviceSeq")
		a, _ := json.Marshal(was[i])
		b, _ := json.Marshal(now[i])
		if string(a) != string(b) {
			t.Fatalf("row %d changed beyond its deviceSeq:\n was %s\n now %s", 101+i, a, b)
		}
	}
	if d := ob.Depth(); d != 5 {
		t.Fatalf("depth %d after a rebase", d)
	}
	if moved, err := ob.Rebase(100); err != nil || moved != 0 {
		t.Fatalf("a second Rebase at the same watermark moved %d (%v)", moved, err)
	}
	ob.Close()

	ob2, _, err := openWhatsAppOutbox(dir, "lnk_rebase")
	if err != nil {
		t.Fatal(err)
	}
	defer ob2.Close()
	if b, err := ob2.Next(); err != nil || b.SeqFrom != 101 || b.SeqTo != 105 {
		t.Fatalf("after reopen: %+v %v", b, err)
	}
	more := []whatsAppRecord{textRecord("chat@s.whatsapp.net", "A6", "six")}
	if err := ob2.Append(more, false); err != nil {
		t.Fatal(err)
	}
	if more[0].DeviceSeq != 106 {
		t.Fatalf("the next record was numbered %d, want 106", more[0].DeviceSeq)
	}
}

// A shift shorter than the queue's span is where an ascending renumber would
// collide with rows not yet moved; a queue longer than one page is where a
// row could be skipped or moved twice.
func TestWhatsAppOutboxRebasePagesThroughALongQueue(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_rebase_long")
	defer ob.Close()
	const n = 2*whatsAppBatchMaxRecords + 50
	var records []whatsAppRecord
	for i := 1; i <= n; i++ {
		records = append(records, textRecord("chat@s.whatsapp.net", "L"+strconv.Itoa(i), "x"))
	}
	if err := ob.Append(records, false); err != nil {
		t.Fatal(err)
	}
	moved, err := ob.Rebase(10) // rows 1..n become 11..n+10
	if err != nil || moved != n {
		t.Fatalf("Rebase(10) = %d, %v; want %d", moved, err, n)
	}
	want := 1
	for {
		b, err := ob.Next()
		if err != nil {
			t.Fatal(err)
		}
		if b == nil {
			break
		}
		for _, rec := range batchRecords(t, b) {
			if rec["waMessageId"] != "L"+strconv.Itoa(want) || rec["deviceSeq"].(float64) != float64(want+10) {
				t.Fatalf("expected L%d at %d, got %v at %v", want, want+10, rec["waMessageId"], rec["deviceSeq"])
			}
			want++
		}
		if err := ob.Ack(b.SeqTo); err != nil {
			t.Fatal(err)
		}
	}
	if want != n+1 {
		t.Fatalf("read %d rows back, want %d", want-1, n)
	}
}

// A watermark no sequence can pass refuses, and moves nothing.
func TestWhatsAppOutboxRebaseRefusesPastTheLargestSequence(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_rebase_max")
	defer ob.Close()
	if err := ob.Append([]whatsAppRecord{textRecord("chat@s.whatsapp.net", "M1", "x")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Rebase(whatsAppMaxSeq); !errors.Is(err, errWhatsAppRebaseOutOfRange) {
		t.Fatalf("Rebase(max) = %v, want errWhatsAppRebaseOutOfRange", err)
	}
	if b, err := ob.Next(); err != nil || b.SeqFrom != 1 {
		t.Fatalf("a refused rebase moved rows: %+v %v", b, err)
	}
}

// watermarkCloud answers batches like whatsapp-service's applyBatch: a batch
// ending at or below the watermark is a replay, acked with the watermark and
// NOT written; anything else is written and acked through its end.
type watermarkCloud struct {
	mu        sync.Mutex
	watermark int64
	written   map[string]float64 // waMessageId -> deviceSeq it was written with
}

func (c *watermarkCloud) respond(_ []string, p map[string]any) whatsAppSyncResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	to := int64(p["seqTo"].(float64))
	if to <= c.watermark {
		w := c.watermark
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &w}}
	}
	for _, rec := range p["records"].([]any) {
		m := rec.(map[string]any)
		c.written[m["waMessageId"].(string)] = m["deviceSeq"].(float64)
	}
	c.watermark = max(c.watermark, to)
	return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
}

// The outbox numbering restarted below the cloud's watermark off linkStart
// (an outbox.db lost or restored from a backup, then a plain restart). The
// cloud acks those batches as replays without writing them: the uploader
// must renumber and resend them, never delete them on that ack.
func TestWhatsAppUploadRenumbersWhatTheCloudAckedAsAReplay(t *testing.T) {
	cloud := &watermarkCloud{watermark: 500, written: map[string]float64{}}
	r, ob, _ := newRefusalHarness(t, "lnk_behind", cloud.respond)
	ids := []string{"R1", "R2", "R3"}
	var records []whatsAppRecord
	for _, id := range ids {
		records = append(records, textRecord("chat@s.whatsapp.net", id, "x"))
	}
	if err := ob.Append(records, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; ob.Depth() > 0; i++ {
		if i == 10 {
			t.Fatalf("the queue never drained: depth %d", ob.Depth())
		}
		r.uploadOnce(context.Background())
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	for _, id := range ids {
		seq, ok := cloud.written[id]
		if !ok {
			t.Fatalf("%s was deleted on a replay ack and never written (written: %v)", id, cloud.written)
		}
		if seq <= 500 {
			t.Fatalf("%s was written at %v, at or below the watermark", id, seq)
		}
	}
	more := []whatsAppRecord{textRecord("chat@s.whatsapp.net", "R4", "x")}
	if err := ob.Append(more, false); err != nil {
		t.Fatal(err)
	}
	if more[0].DeviceSeq <= 503 {
		t.Fatalf("a later record was numbered %d, behind the rows already sent", more[0].DeviceSeq)
	}
}

// An ack exactly at the batch's end is the ordinary replay of a batch whose
// ack was lost: the rows go, nothing is renumbered.
func TestWhatsAppUploadAckAtTheBatchEndDeletesWithoutRenumbering(t *testing.T) {
	r, ob, _ := newRefusalHarness(t, "lnk_replay", func(_ []string, p map[string]any) whatsAppSyncResult {
		return ackedThrough(p)
	})
	if err := ob.Append([]whatsAppRecord{textRecord("chat@s.whatsapp.net", "P1", "x")}, false); err != nil {
		t.Fatal(err)
	}
	if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadSent || ob.Depth() != 0 {
		t.Fatalf("outcome %d, depth %d", outcome, ob.Depth())
	}
	more := []whatsAppRecord{textRecord("chat@s.whatsapp.net", "P2", "x")}
	if err := ob.Append(more, false); err != nil {
		t.Fatal(err)
	}
	if more[0].DeviceSeq != 2 {
		t.Fatalf("an ordinary ack renumbered the outbox: next record at %d", more[0].DeviceSeq)
	}
}
