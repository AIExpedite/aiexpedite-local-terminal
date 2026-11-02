package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
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

// On macOS fsync() does not flush the drive's cache: the outbox asks SQLite
// for F_FULLFSYNC (ignored elsewhere), on commits and checkpoints.
func TestWhatsAppOutboxAsksForFullFsync(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_ffs")
	defer ob.Close()
	for _, pragma := range []string{"fullfsync", "checkpoint_fullfsync"} {
		var on int
		if err := ob.db.QueryRow(`PRAGMA ` + pragma).Scan(&on); err != nil || on != 1 {
			t.Fatalf("%s = %d (%v), want 1", pragma, on, err)
		}
	}
}

// Shutdown's store close is bounded by the caller's deadline: closing an
// outbox waits for a write in flight, and a large history chunk can still be
// appending on whatsmeow's goroutine.
func TestWhatsAppShutdownCloseHonoursTheDeadline(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_close_deadline")
	r := &whatsAppLinkRunner{linkID: "lnk_close_deadline", outbox: ob}
	ob.mu.Lock() // a history append in flight
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
