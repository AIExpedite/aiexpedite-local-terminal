package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// A group notice seen live and then again in a history sync (a re-link's, or
// the link-time sync overlapping live delivery) is ONE message: both copies
// carry the same (chat, participant, id) and hash to the same msgId.
func TestNormalizeGroupNoticeLiveAndHistoryCollapse(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe}
	at := time.Unix(1_800_000_123, 0) // whatsmeow parses the notice's "t" in whole seconds
	live := normalizeWhatsAppGroupInfo(&events.GroupInfo{JID: waGroup, Timestamp: at, Join: []types.JID{waPeer}}, nc)

	stub := &waWeb.WebMessageInfo{
		Key:                   &waCommon.MessageKey{RemoteJID: proto.String(waGroup.String()), ID: proto.String("3EB0STANZA")},
		MessageTimestamp:      proto.Uint64(uint64(at.Unix())),
		MessageStubType:       waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD.Enum(),
		MessageStubParameters: []string{waPeer.String()},
	}
	data := &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(waGroup.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: stub}}}}}
	recs, _ := normalizeWhatsAppHistory(data, parseLikeWhatsmeow, nc)
	var hist *whatsAppRecord
	for i := range recs {
		if recs[i].Type == waTypeSystem {
			hist = &recs[i]
		}
	}
	if len(live) != 1 || hist == nil {
		t.Fatalf("live=%+v history=%+v", live, recs)
	}
	if live[0].ChatJid != hist.ChatJid || live[0].SenderJid != hist.SenderJid || live[0].WaMessageID != hist.WaMessageID {
		t.Fatalf("one notice, two messages: live (%s, %s, %s) history (%s, %s, %s)",
			live[0].ChatJid, live[0].SenderJid, live[0].WaMessageID, hist.ChatJid, hist.SenderJid, hist.WaMessageID)
	}
}

// groupStub is a history-sync group notice about who, keyed id.
func groupStub(id string, at time.Time, typ waWeb.WebMessageInfo_StubType, who types.JID) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:                   &waCommon.MessageKey{RemoteJID: proto.String(waGroup.String()), ID: proto.String(id)},
		MessageTimestamp:      proto.Uint64(uint64(at.Unix())),
		MessageStubType:       typ.Enum(),
		MessageStubParameters: []string{who.String()},
	}}
}

// noticeKeys is the (chat, sender, waMessageId) key of every system record:
// the cloud keeps one message per key.
func noticeKeys(recs []whatsAppRecord) []string {
	var keys []string
	for _, r := range recs {
		if r.Type == waTypeSystem {
			keys = append(keys, r.ChatJid+"|"+r.SenderJid+"|"+r.WaMessageID)
		}
	}
	return keys
}

func distinctKeys(keys []string) map[string]bool {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// Distinct history notices sharing a key are numbered: X invited, removed and
// invited again within one second is three messages, not two ("joined" covers
// both ADD and INVITE, "left" both REMOVE and LEAVE). The first of each key
// keeps the key of the live notice of the same event, so the two still
// collapse - per verb and per person - and the chunk's order does not change
// which keys there are.
func TestNormalizeHistoryNumbersDistinctNoticesSharingAKey(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe}
	at := time.Unix(1_800_000_123, 0)
	other := types.NewJID("15550000003", types.DefaultUserServer)
	// chunk normalizes stubs named <verb><n>[@other]: JOIN is an invite, KICK
	// a removal, and the participant is waPeer unless the name says other.
	chunk := func(ids ...string) []string {
		var msgs []*waHistorySync.HistorySyncMsg
		for _, id := range ids {
			typ, who := waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE, waPeer
			if strings.HasPrefix(id, "KICK") {
				typ = waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE
			}
			if strings.HasSuffix(id, "@other") {
				who = other
			}
			msgs = append(msgs, groupStub(id, at, typ, who))
		}
		data := &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(waGroup.String()), Messages: msgs}}}
		recs, _ := normalizeWhatsAppHistory(data, parseLikeWhatsmeow, nc)
		return noticeKeys(recs)
	}
	liveKeys := func(evt *events.GroupInfo) []string {
		evt.JID, evt.Timestamp = waGroup, at
		return noticeKeys(normalizeWhatsAppGroupInfo(evt, nc))
	}
	keys := chunk("JOIN1", "KICK1", "JOIN2")
	if len(keys) != 3 || len(distinctKeys(keys)) != 3 {
		t.Fatalf("three notices gave the keys %v: the cloud keeps one message per key, so a notice is lost", keys)
	}
	liveJoin := liveKeys(&events.GroupInfo{Join: []types.JID{waPeer}})[0]
	if keys[0] != liveJoin || keys[1] != liveKeys(&events.GroupInfo{Leave: []types.JID{waPeer}})[0] {
		t.Fatalf("history keys %v: the first join and the first leave must keep their live notices' keys, or one notice is two messages", keys)
	}
	// The key is persisted (it names the cloud's message), so its form is too.
	if keys[2] != liveJoin+"-2" {
		t.Fatalf("the second join is keyed %q, want %q", keys[2], liveJoin+"-2")
	}
	reversed := chunk("JOIN2", "KICK1", "JOIN1")
	got := distinctKeys(reversed)
	for k := range distinctKeys(keys) {
		if !got[k] || len(got) != 3 {
			t.Fatalf("the reversed chunk gave the keys %v, the chunk %v", reversed, keys)
		}
	}
	if twice := chunk("SAME", "SAME"); len(distinctKeys(twice)) != 1 {
		t.Fatalf("one stub listed twice gave the keys %v, want one message", twice)
	}
	// Two people joining in one second share no key: neither is numbered.
	both := chunk("JOIN1", "JOIN2@other")
	live := liveKeys(&events.GroupInfo{Join: []types.JID{waPeer, other}})
	if len(both) != 2 || len(live) != 2 || both[0] != live[0] || both[1] != live[1] {
		t.Fatalf("two people joining: history keys %v, live keys %v", both, live)
	}
}

// On macOS fsync() does not flush the drive's cache: the outbox asks SQLite
// for F_FULLFSYNC (ignored elsewhere), which then covers checkpoints too. A
// large transaction must not leave the WAL at its size either.
func TestWhatsAppOutboxAsksForFullFsync(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_ffs")
	defer ob.Close()
	for pragma, want := range map[string]int64{"fullfsync": 1, "journal_size_limit": 67108864} {
		var got int64
		if err := ob.db.QueryRow(`PRAGMA ` + pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d (%v), want %d", pragma, got, err, want)
		}
	}
}

// Shutdown's store close is bounded by the caller's deadline: closing an
// outbox waits for whatever holds its lock. Held by anything but a committing
// write (no Append or Rebase reports one in flight here), it gets no grace.
func TestWhatsAppShutdownCloseHonoursTheDeadline(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_close_deadline")
	r := &whatsAppLinkRunner{linkID: "lnk_close_deadline", outbox: ob}
	ob.mu.Lock() // held, but not by a write in flight
	deadline := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(deadline) })
	start := time.Now()
	if closeStoresWithin([]*whatsAppLinkRunner{r}, deadline) {
		ob.mu.Unlock()
		t.Fatal("reported closed while a write held the outbox")
	}
	if took := time.Since(start); took > time.Second {
		ob.mu.Unlock()
		t.Fatalf("waited %v past a 100ms deadline", took)
	}
	ob.mu.Unlock()
	if !closeStoresWithin(nil, nil) {
		t.Fatal("nothing to close must report closed")
	}
}

// WriteInFlight is set while an Append or a Rebase is inside its transaction,
// and cleared by the time it returns.
func TestWhatsAppOutboxReportsAWriteInFlight(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_in_flight")
	defer ob.Close()
	if ob.WriteInFlight() {
		t.Fatal("an idle outbox reported a write in flight")
	}
	records := make([]whatsAppRecord, 20000)
	for i := range records {
		records[i] = textRecord("chat@s.whatsapp.net", "W"+strconv.Itoa(i), "x")
	}
	watch := func(name string, write func() error) {
		done := make(chan error, 1)
		go func() { done <- write() }()
		saw := false
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !saw {
					t.Fatalf("%s never reported a write in flight", name)
				}
				if ob.WriteInFlight() {
					t.Fatalf("a write still in flight after %s returned", name)
				}
				return
			default:
				saw = saw || ob.WriteInFlight()
				runtime.Gosched()
			}
		}
	}
	watch("Append", func() error { return ob.Append(records, true) })
	watch("Rebase", func() error { _, err := ob.Rebase(100000); return err })
}

// bigHistoryChunk is one RECENT history chunk (not the final one) of n
// messages in one direct chat.
func bigHistoryChunk(n int) *waHistorySync.HistorySync {
	msgs := make([]*waHistorySync.HistorySyncMsg, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(waPeer.String()), ID: proto.String("HIST" + strconv.Itoa(i))},
			MessageTimestamp: proto.Uint64(uint64(1_800_000_000 + i)),
			Message:          &waE2E.Message{Conversation: proto.String(strings.Repeat("x", 40))},
		}})
	}
	return &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
		Progress:      proto.Uint32(40),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(waPeer.String()), Messages: msgs}},
	}
}

// committedOutboxRows counts the rows committed to dir's outbox through a
// connection of its own: a WAL reader sees committed transactions only, which
// is what the next start would find if the process exited now.
func committedOutboxRows(t *testing.T, dir string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, whatsAppOutboxDBName))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A history chunk still committing when shutdown's deadline passes is waited
// for: the process exits right after ShutdownAll returns, and WhatsApp does
// not send the chunk again. The wait lasts as long as the write and no
// longer: the session store's close is held here too, so ShutdownAll returns
// before the grace ends only if it notices that the write committed.
func TestWhatsAppShutdownWaitsForAHistoryChunkStillCommitting(t *testing.T) {
	h, c := pairedHarness(t)
	r := h.sup.runner("lnk_1")
	dir, _ := whatsAppLinkDir("lnk_1")
	waitFor(t, "the outbox drained", func() bool { depth, _ := r.outboxDepth(); return depth == 0 })
	// A 401 pauses uploads, so shutdown's flush returns without waiting on
	// the outbox and shutdown reaches the store close with the write going.
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		return whatsAppSyncResult{Status: 401}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "L401", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "uploads paused by the 401", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return time.Now().Before(r.uploadPausedTil)
	})
	prevGrace := whatsAppShutdownWriteGrace
	whatsAppShutdownWriteGrace = 10 * time.Second
	t.Cleanup(func() { whatsAppShutdownWriteGrace = prevGrace })
	gate := make(chan struct{})
	c.mu.Lock()
	c.closeGate = gate
	c.mu.Unlock()
	t.Cleanup(func() {
		close(gate)
		h.sup.ShutdownAll(context.Background()) // a no-op once the test shut down
		// The outbox closes before the session store: once the client is
		// closed, no store holds a file the temp dir needs gone.
		waitFor(t, "the stores closed", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed })
	})
	before := committedOutboxRows(t, dir)
	r.mu.Lock()
	ob := r.outbox
	r.mu.Unlock()

	const chunk = 30000
	histDone := make(chan struct{})
	go func() { // whatsmeow's event goroutine
		defer close(histDone)
		c.emit(&events.HistorySync{Data: bigHistoryChunk(chunk)})
	}()
	appendBy := time.Now().Add(10 * time.Second)
	for !ob.WriteInFlight() {
		if time.Now().After(appendBy) {
			t.Fatal("the history chunk's Append never started")
		}
		runtime.Gosched()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.sup.ShutdownAll(ctx)
	took := time.Since(start)
	atExit := committedOutboxRows(t, dir)
	<-histDone
	if atExit < before+chunk {
		t.Fatalf("ShutdownAll returned (the process exits) with %d of the chunk's %d records committed", atExit-before, chunk)
	}
	if took >= whatsAppShutdownWriteGrace/2 {
		t.Fatalf("ShutdownAll took %v: it waited out the grace, not the write", took)
	}
}

// ShutdownAll's own store close honours the caller's deadline. The test holds
// the outbox lock itself, as no Append or Rebase does, so nothing is
// committing and no grace applies: shutdown returns at its deadline, and the
// link is still recorded.
func TestWhatsAppShutdownAllCloseHonoursTheDeadline(t *testing.T) {
	withWhatsAppTestEnv(t)
	s := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	r := s.newRunner("lnk_sd_close", "ws", 2)
	ob, _ := openTestOutbox(t, "lnk_sd_close")
	r.outbox, r.state = ob, waStateConnected
	// A 401/403 paused uploads: the flush returns without reading the outbox,
	// so shutdown reaches the store close.
	r.uploadPausedTil = time.Now().Add(time.Hour)
	s.links[r.linkID] = r
	ob.mu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			ob.mu.Unlock()
			_ = ob.Close() // waits for the abandoned close before the temp dir goes
		}
	}
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	start := time.Now()
	go func() { s.ShutdownAll(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		release()
		<-done
		t.Fatalf("ShutdownAll was still closing the stores %v into a 250ms deadline", time.Since(start).Round(time.Millisecond))
	}
	took := time.Since(start)
	release()
	if took > time.Second {
		t.Fatalf("ShutdownAll took %v with a 250ms deadline", took)
	}
	if local, err := loadWhatsAppLocalLinks(); err != nil || len(local) != 1 {
		t.Fatalf("shutdown must still record the link: %+v %v", local, err)
	}
}

// A heartbeat's diagnostics read the outbox depth without the runner lock: the
// depth waits for the outbox lock, which a long Append or Rebase holds, and
// every event (handleEvent takes the runner lock first) would queue behind it.
func TestWhatsAppDiagnosticsWaitForTheOutboxWithoutTheRunnerLock(t *testing.T) {
	r, ob, _ := newRefusalHarness(t, "lnk_diag", func(_ []string, p map[string]any) whatsAppSyncResult { return ackedThrough(p) })
	if err := ob.Append([]whatsAppRecord{textRecord("chat@s.whatsapp.net", "D1", "x"), textRecord("chat@s.whatsapp.net", "D2", "x")}, false); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.liveSynced = 2
	// Pruned by diagnostics under the runner lock: seeing it gone shows the
	// call has read its fields.
	r.reconnects = []time.Time{time.Now().Add(-48 * time.Hour)}
	r.mu.Unlock()
	ob.mu.Lock() // a long Append or Rebase
	held := true
	defer func() {
		if held {
			ob.mu.Unlock()
		}
	}()
	got := make(chan map[string]any, 1)
	go func() { got <- r.diagnostics() }()
	for giveUp := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		if r.mu.TryLock() {
			read := len(r.reconnects) == 0
			r.mu.Unlock()
			if read {
				break // read its fields, and let the runner lock go
			}
		}
		if time.Now().After(giveUp) {
			t.Fatal("diagnostics held the runner lock while it waited for the outbox")
		}
	}
	held = false
	ob.mu.Unlock()
	d := <-got
	if d["outboxDepth"] != int64(2) || d["liveMessagesSynced"] != int64(2) || d["reconnectCount24h"] != 0 {
		t.Fatalf("diagnostics = %v", d)
	}
}
