package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
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

// The link-start case: rows carried over from before a rolled-back outbox.db
// sit below the cloud's watermark, linkStart's EnsureNextAbove raised `next`
// past it, and rows were queued above it. The rebase must keep every row in
// order AND leave `next` past all of them, on the same handle: a record queued
// after it must still read back last.
func TestWhatsAppUploadRebaseAfterALinkStartKeepsTheQueueInOrder(t *testing.T) {
	cloud := &watermarkCloud{watermark: 500, written: map[string]float64{}}
	r, ob, _ := newRefusalHarness(t, "lnk_link_start", cloud.respond)
	queue := func(ids ...string) []whatsAppRecord {
		var records []whatsAppRecord
		for _, id := range ids {
			records = append(records, textRecord("chat@s.whatsapp.net", id, "x"))
		}
		if err := ob.Append(records, false); err != nil {
			t.Fatal(err)
		}
		return records
	}
	queue("C1", "C2", "C3") // 1..3, carried over
	if err := ob.EnsureNextAbove(500); err != nil {
		t.Fatal(err)
	}
	queue("N1", "N2") // 501..502
	// [1..3] is acked as a replay at 500: renumbered, nothing deleted.
	if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadSent || ob.Depth() != 5 {
		t.Fatalf("outcome %d, depth %d after the replay ack", outcome, ob.Depth())
	}
	queue("L1")
	for i := 0; ob.Depth() > 0; i++ {
		if i == 10 {
			t.Fatalf("the queue never drained: depth %d", ob.Depth())
		}
		r.uploadOnce(context.Background())
	}
	cloud.mu.Lock()
	order := []string{"C1", "C2", "C3", "N1", "N2", "L1"}
	prev := float64(500)
	for _, id := range order {
		seq, ok := cloud.written[id]
		if !ok || seq <= prev {
			cloud.mu.Unlock()
			t.Fatalf("%s written at %v (written %v): want every row above 500, in the order %v", id, seq, cloud.written, order)
		}
		prev = seq
	}
	n2 := cloud.written["N2"]
	cloud.mu.Unlock()
	next := queue("X")
	if float64(next[0].DeviceSeq) <= n2 {
		t.Fatalf("a record queued after the rebase was numbered %d, at or below N2's %v", next[0].DeviceSeq, n2)
	}
}

// applyBatchCloud answers batches like whatsapp-service's applyBatch: a batch
// ending at or below the watermark is a replay, acked with the watermark and
// NOT written; otherwise every record is validated first (a record without a
// waTimestamp is refused with a 400, and nothing in the batch is written),
// then all of them are written and the batch is acked through its end.
type applyBatchCloud struct {
	mu        sync.Mutex
	watermark int64
	writes    map[string][]int64 // waMessageId -> every deviceSeq it was written at
	// before answers the next batches with these statuses before they reach
	// ingest: nothing is applied.
	before []int
	// lostAnswer, when set, replaces the answer to the next batch the cloud
	// applies: the batch is written, but the device sees this instead of
	// its 2xx.
	lostAnswer *whatsAppSyncResult
}

func newApplyBatchCloud(watermark int64) *applyBatchCloud {
	return &applyBatchCloud{watermark: watermark, writes: map[string][]int64{}}
}

func (c *applyBatchCloud) respond(_ []string, p map[string]any) whatsAppSyncResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.before) > 0 {
		status := c.before[0]
		c.before = c.before[1:]
		return whatsAppSyncResult{Status: status}
	}
	to := int64(p["seqTo"].(float64))
	if to <= c.watermark {
		w := c.watermark
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &w}}
	}
	records := p["records"].([]any)
	for _, rec := range records {
		if ts, _ := rec.(map[string]any)["waTimestamp"].(float64); ts <= 0 {
			return whatsAppSyncResult{Status: 400, Body: whatsAppSyncResponse{Code: "WHATSAPP_INGEST_INVALID"}}
		}
	}
	for _, rec := range records {
		m := rec.(map[string]any)
		id := m["waMessageId"].(string)
		c.writes[id] = append(c.writes[id], int64(m["deviceSeq"].(float64)))
	}
	c.watermark = to
	if lost := c.lostAnswer; lost != nil {
		c.lostAnswer = nil
		return *lost
	}
	return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
}

// written is every deviceSeq id was written at.
func (c *applyBatchCloud) written(id string) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.writes[id]...)
}

// drainOutbox uploads until the outbox is empty.
func drainOutbox(t *testing.T, r *whatsAppLinkRunner, ob *whatsAppOutbox) {
	t.Helper()
	for i := 0; ob.Depth() > 0; i++ {
		if i == 200 {
			t.Fatalf("the queue never drained: depth %d", ob.Depth())
		}
		r.uploadOnce(context.Background())
	}
}

// A batch the cloud applied but whose 2xx never reached the device is retried
// as exactly the same range, though rows queued since would grow it: the retry
// is an exact replay, acked at its own end. Grown instead, a record the cloud
// refuses further on gets the batch narrowed down to one ending below the
// watermark the lost batch set, whose replay ack passes its end - and the
// rebase that follows renumbers, and sends again, rows the cloud already
// wrote. A 401 in between (refused before ingest) settles nothing.
func TestWhatsAppUploadRetriesAnUnsettledBatchAsTheSameRange(t *testing.T) {
	for _, c := range []struct {
		name       string
		lostAnswer whatsAppSyncResult
		before     []int
	}{
		{name: "transport error", lostAnswer: whatsAppSyncResult{Status: 0}},
		{name: "502 after the cloud applied it", lostAnswer: whatsAppSyncResult{Status: 502}},
		{name: "transport error then a 401", lostAnswer: whatsAppSyncResult{Status: 0}, before: []int{401}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cloud := newApplyBatchCloud(0)
			r, ob, _ := newRefusalHarness(t, "lnk_unsettled", cloud.respond)
			var first []whatsAppRecord
			for i := 1; i <= 80; i++ {
				first = append(first, textRecord("chat@s.whatsapp.net", "A"+strconv.Itoa(i), "x"))
			}
			if err := ob.Append(first, false); err != nil {
				t.Fatal(err)
			}
			lost := c.lostAnswer
			cloud.mu.Lock()
			cloud.lostAnswer = &lost
			cloud.mu.Unlock()
			if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadRetry || ob.Depth() != 80 {
				t.Fatalf("[1..80] applied with its 2xx lost: outcome %d, depth %d", outcome, ob.Depth())
			}
			var second []whatsAppRecord
			for i := 1; i <= 120; i++ {
				rec := textRecord("chat@s.whatsapp.net", "B"+strconv.Itoa(i), "y")
				if i == 10 {
					rec.WaTimestamp = 0 // refused by the cloud's validation
				}
				second = append(second, rec)
			}
			if err := ob.Append(second, false); err != nil {
				t.Fatal(err)
			}
			if len(c.before) > 0 {
				cloud.mu.Lock()
				cloud.before = c.before
				cloud.mu.Unlock()
				if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadIdle {
					t.Fatalf("a 401 must pause uploads, outcome %d", outcome)
				}
				r.mu.Lock()
				r.uploadPausedTil = time.Time{} // credentials work again
				r.mu.Unlock()
			}
			drainOutbox(t, r, ob)
			for _, rec := range append(first, second...) {
				got := cloud.written(rec.WaMessageID)
				if rec.WaMessageID == "B10" {
					if len(got) != 0 {
						t.Fatalf("the refused record was written at %v", got)
					}
					continue
				}
				if len(got) != 1 || got[0] != rec.DeviceSeq {
					t.Fatalf("%s queued at %d was written at %v: want it once, never renumbered", rec.WaMessageID, rec.DeviceSeq, got)
				}
			}
			more := []whatsAppRecord{textRecord("chat@s.whatsapp.net", "C1", "z")}
			if err := ob.Append(more, false); err != nil {
				t.Fatal(err)
			}
			if more[0].DeviceSeq != 201 {
				t.Fatalf("the next record was numbered %d, want 201: the outbox was renumbered", more[0].DeviceSeq)
			}
		})
	}
}

// A 2xx whose local Ack fails leaves the applied rows queued, exactly as a lost
// 2xx does: the same range goes again (unsettledTo) and comes back as a
// replay acked at its own end - not grown by rows queued since, refused for
// one of them, narrowed below the acked end and renumbered. And the failing
// Ack backs off instead of posting again at once.
func TestWhatsAppUploadRetriesARangeWhoseAckFailed(t *testing.T) {
	cloud := newApplyBatchCloud(0)
	r, ob, _ := newRefusalHarness(t, "lnk_ack_failed", cloud.respond)
	var first []whatsAppRecord
	for i := 1; i <= 80; i++ {
		first = append(first, textRecord("chat@s.whatsapp.net", "A"+strconv.Itoa(i), "x"))
	}
	if err := ob.Append(first, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.db.Exec(`CREATE TRIGGER fail_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT, 'disk said no'); END`); err != nil {
		t.Fatal(err)
	}
	if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadRetry || ob.Depth() != 80 {
		t.Fatalf("[1..80] applied, its Ack failed: outcome %d, depth %d (want a backed-off retry, all 80 queued)", outcome, ob.Depth())
	}
	if _, err := ob.db.Exec(`DROP TRIGGER fail_ack`); err != nil {
		t.Fatal(err)
	}
	var second []whatsAppRecord
	for i := 1; i <= 120; i++ {
		rec := textRecord("chat@s.whatsapp.net", "B"+strconv.Itoa(i), "y")
		if i == 10 {
			rec.WaTimestamp = 0 // refused by the cloud's validation
		}
		second = append(second, rec)
	}
	if err := ob.Append(second, false); err != nil {
		t.Fatal(err)
	}
	drainOutbox(t, r, ob)
	for _, rec := range append(first, second...) {
		got := cloud.written(rec.WaMessageID)
		if rec.WaMessageID == "B10" {
			if len(got) != 0 {
				t.Fatalf("the refused record was written at %v", got)
			}
			continue
		}
		if len(got) != 1 || got[0] != rec.DeviceSeq {
			t.Fatalf("%s queued at %d was written at %v: want it once, never renumbered", rec.WaMessageID, rec.DeviceSeq, got)
		}
	}
	more := []whatsAppRecord{textRecord("chat@s.whatsapp.net", "C1", "z")}
	if err := ob.Append(more, false); err != nil {
		t.Fatal(err)
	}
	if more[0].DeviceSeq != 201 {
		t.Fatalf("the next record was numbered %d, want 201: the outbox was renumbered", more[0].DeviceSeq)
	}
}

// An outbox restored below the cloud's watermark (rows numbered from 1, the
// cloud at 150) whose first batch fails before the cloud applies it (a 502),
// and is then refused and narrowed down: the retry as the same range must not
// stand in the renumbering's way, and every valid row is written once, above
// the watermark.
func TestWhatsAppUploadRenumbersARestoredOutboxAfterAFailedAndRefusedBatch(t *testing.T) {
	cloud := newApplyBatchCloud(150)
	cloud.before = []int{502}
	r, ob, _ := newRefusalHarness(t, "lnk_restored", cloud.respond)
	var records []whatsAppRecord
	for i := 1; i <= 250; i++ {
		rec := textRecord("chat@s.whatsapp.net", "N"+strconv.Itoa(i), "z")
		if i == 120 {
			rec.WaTimestamp = 0 // refused by the cloud's validation
		}
		records = append(records, rec)
	}
	if err := ob.Append(records, false); err != nil {
		t.Fatal(err)
	}
	drainOutbox(t, r, ob)
	prev := int64(150)
	for _, rec := range records {
		got := cloud.written(rec.WaMessageID)
		if rec.WaMessageID == "N120" {
			if len(got) != 0 {
				t.Fatalf("the refused record was written at %v", got)
			}
			continue
		}
		if len(got) != 1 || got[0] <= prev {
			t.Fatalf("%s written at %v: want it once, above the watermark and after the row before it (%d)", rec.WaMessageID, got, prev)
		}
		prev = got[0]
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
