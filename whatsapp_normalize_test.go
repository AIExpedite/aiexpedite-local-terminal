package main

import (
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

var (
	waMe    = types.NewJID("15550000001", types.DefaultUserServer)
	waPeer  = types.NewJID("15550000002", types.DefaultUserServer)
	waGroup = types.NewJID("120363000000000001", types.GroupServer)
	waTS    = time.UnixMilli(1_800_000_000_000)
)

func liveMsg(chat, sender types.JID, fromMe bool, id string, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            types.MessageID(id), Timestamp: waTS, PushName: "Peer",
		},
		Message: m,
	}
}

func one(t *testing.T, recs []whatsAppRecord) whatsAppRecord {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	return recs[0]
}

func TestNormalizeCoveredTypes(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe}
	cases := []struct {
		name  string
		msg   *waE2E.Message
		typ   string
		text  string
		media bool
	}{
		{"text", &waE2E.Message{Conversation: proto.String("hi")}, waTypeText, "hi", false},
		{"extended", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("reply"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("Q1")}}}, waTypeText, "reply", false},
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("cap"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(1234), URL: proto.String("https://mmg.whatsapp.net/secret"), MediaKey: []byte("key")}}, waTypeImage, "cap", true},
		{"video", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), Seconds: proto.Uint32(9)}}, waTypeVideo, "", true},
		{"audio", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg")}}, waTypeAudio, "", true},
		{"voice", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(3)}}, waTypeVoiceNote, "", true},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("a.pdf"), Mimetype: proto.String("application/pdf")}}, waTypeDocument, "", true},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp")}}, waTypeSticker, "", true},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(1.5), DegreesLongitude: proto.Float64(2.5)}}, waTypeLocation, "", true},
		{"contact", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Bob"), Vcard: proto.String("BEGIN:VCARD")}}, waTypeContact, "", true},
		{"poll", &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("q")}}, waTypeUnsupported, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "ID1", c.msg), waSourceLive, nc))
			if r.Op != waOpNew || r.Type != c.typ || r.Text != c.text || (r.Media != nil) != c.media {
				t.Fatalf("got %+v", r)
			}
			if r.ChatJid != waPeer.String() || r.SenderJid != waPeer.String() || r.WaMessageID != "ID1" || r.WaTimestamp != waTS.UnixMilli() {
				t.Fatalf("identity: %+v", r)
			}
		})
	}
}

// Media is metadata only: the CDN URL, the media key and a location's
// coordinates never reach a record.
func TestNormalizeMediaIsMetadataOnly(t *testing.T) {
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(10), URL: proto.String("https://mmg.whatsapp.net/secret"), MediaKey: []byte("k"), DirectPath: proto.String("/v/secret")}}
	r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "I", img), waSourceLive, whatsAppNormalizeContext{}))
	if *r.Media != (whatsAppMedia{Mimetype: "image/jpeg", Size: 10}) {
		t.Fatalf("media = %+v", *r.Media)
	}
	loc := &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(51.5), Name: proto.String("Home")}}
	r = one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "L", loc), waSourceLive, whatsAppNormalizeContext{}))
	if *r.Media != (whatsAppMedia{}) || r.Text != "" {
		t.Fatalf("location leaked: %+v %+v", r, *r.Media)
	}
}

func TestNormalizeQuotedReplyAndViewOnce(t *testing.T) {
	m := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("yes"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIG")}}}
	if r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "R", m), waSourceLive, whatsAppNormalizeContext{})); r.QuotedID != "ORIG" {
		t.Fatalf("quotedId = %q", r.QuotedID)
	}
	evt := liveMsg(waPeer, waPeer, false, "V", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("secret view-once")}})
	evt.IsViewOnce = true
	if r := one(t, normalizeWhatsAppMessage(evt, waSourceLive, whatsAppNormalizeContext{})); r.Type != waTypeUnsupported || r.Text != "" || r.Media != nil {
		t.Fatalf("view-once content stored: %+v", r)
	}
}

func TestNormalizeEditRevokeReaction(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe}
	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("TARGET")},
		EditedMessage: &waE2E.Message{Conversation: proto.String("fixed")}, TimestampMS: proto.Int64(1_800_000_000_500),
	}}
	r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "E", edit), waSourceLive, nc))
	if r.Op != waOpEdit || r.WaMessageID != "TARGET" || r.Text != "fixed" || r.EditVersion != 1_800_000_000_500 || r.SenderJid != waPeer.String() {
		t.Fatalf("edit = %+v", r)
	}

	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("GONE"), Participant: proto.String("15550000003@s.whatsapp.net")}}}
	r = one(t, normalizeWhatsAppMessage(liveMsg(waGroup, waPeer, false, "X", revoke), waSourceLive, nc))
	if r.Op != waOpRevoke || r.WaMessageID != "GONE" || r.SenderJid != "15550000003@s.whatsapp.net" {
		t.Fatalf("admin revoke targets the original author: %+v", r)
	}

	// The peer reacts to MY message in a 1:1 chat: key.FromMe=false from the
	// reactor's view, so the target was written by me.
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("MINE"), FromMe: proto.Bool(false)}, Text: proto.String("👍")}}
	r = one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "RX", react), waSourceLive, nc))
	if r.Op != waOpReaction || r.WaMessageID != "MINE" || r.SenderJid != waMe.String() || r.ReactorJid != waPeer.String() || r.Reaction == nil || *r.Reaction != "👍" {
		t.Fatalf("reaction = %+v", r)
	}
	// Removing a reaction keeps an explicit empty string.
	remove := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("MINE")}, Text: proto.String("")}}
	r = one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "RX2", remove), waSourceLive, nc))
	if r.Reaction == nil || *r.Reaction != "" {
		t.Fatal("reaction removal lost")
	}

	// Key shares and other protocol messages are not content.
	other := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE.Enum()}}
	if recs := normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "P", other), waSourceLive, nc); len(recs) != 0 {
		t.Fatalf("protocol noise became a record: %+v", recs)
	}
}

// The same message must hash the same through history and live: a LID sender
// with a known phone number normalizes to the phone-number JID, and the device
// part is dropped.
func TestNormalizeCanonicalIdentity(t *testing.T) {
	lid := types.NewJID("999888777", types.HiddenUserServer)
	evt := liveMsg(lid, types.JID{User: "999888777", Server: types.HiddenUserServer, Device: 3}, false, "ID", &waE2E.Message{Conversation: proto.String("x")})
	evt.Info.SenderAlt = types.NewJID("15550000002", types.DefaultUserServer)
	r := one(t, normalizeWhatsAppMessage(evt, waSourceLive, whatsAppNormalizeContext{}))
	if r.SenderJid != waPeer.String() || r.ChatJid != waPeer.String() {
		t.Fatalf("not canonical: chat=%s sender=%s", r.ChatJid, r.SenderJid)
	}
}

// Operation targets and history conversation ids carry no alternate address:
// the session store's LID mapping resolves them to the same phone-number
// identity the original messages normalized to.
func TestNormalizeResolvesLIDTargetsThroughTheStore(t *testing.T) {
	lid := types.NewJID("999888777", types.HiddenUserServer)
	nc := whatsAppNormalizeContext{OwnJID: waMe, PNForLID: func(j types.JID) types.JID {
		if j == lid {
			return waPeer
		}
		return types.EmptyJID
	}}

	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("GONE"), Participant: proto.String("999888777:2@lid")}}}
	if r := one(t, normalizeWhatsAppMessage(liveMsg(waGroup, waMe, true, "X", revoke), waSourceLive, nc)); r.SenderJid != waPeer.String() {
		t.Fatalf("revoke target = %s", r.SenderJid)
	}
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("T"), Participant: proto.String("999888777@lid")}, Text: proto.String("👍")}}
	if r := one(t, normalizeWhatsAppMessage(liveMsg(waGroup, waMe, true, "R", react), waSourceLive, nc)); r.SenderJid != waPeer.String() {
		t.Fatalf("reaction target = %s", r.SenderJid)
	}
	// An unknown LID stays a LID.
	other := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("G2"), Participant: proto.String("111@lid")}}}
	if r := one(t, normalizeWhatsAppMessage(liveMsg(waGroup, waMe, true, "Y", other), waSourceLive, nc)); r.SenderJid != "111@lid" {
		t.Fatalf("unknown LID = %s", r.SenderJid)
	}

	// A direct history chat keyed by LID: the conversation and its message
	// share the phone-number chat id.
	web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(lid.String()), ID: proto.String("H1")}, MessageTimestamp: proto.Uint64(1_800_000_000), Message: &waE2E.Message{Conversation: proto.String("old")}}
	data := &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String(lid.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: web}}}}}
	parse := func(chat types.JID, w *waWeb.WebMessageInfo) (*events.Message, error) {
		evt := liveMsg(lid, lid, false, w.GetKey().GetID(), w.GetMessage())
		evt.Info.SenderAlt = waPeer
		return evt, nil
	}
	recs, _ := normalizeWhatsAppHistory(data, parse, nc)
	if len(recs) != 2 || recs[0].Kind != waRecordConversation || recs[0].ChatJid != waPeer.String() || recs[1].ChatJid != waPeer.String() {
		t.Fatalf("history records = %+v", recs)
	}
}

func TestNormalizeCatchUpAndSkippedChats(t *testing.T) {
	nc := whatsAppNormalizeContext{ReconnectedAt: waTS.Add(time.Minute)}
	r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "C", &waE2E.Message{Conversation: proto.String("queued")}), waSourceLive, nc))
	if !r.CatchUp {
		t.Fatal("a message older than the reconnect is catch-up")
	}
	status := liveMsg(types.StatusBroadcastJID, waPeer, false, "S", &waE2E.Message{Conversation: proto.String("story")})
	if recs := normalizeWhatsAppMessage(status, waSourceLive, nc); len(recs) != 0 {
		t.Fatal("status/stories are out of scope")
	}
}

func TestNormalizeHistoryChunk(t *testing.T) {
	webText := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(waPeer.String()), ID: proto.String("H1")}, MessageTimestamp: proto.Uint64(1_800_000_000), Message: &waE2E.Message{Conversation: proto.String("old")}}
	webStub := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(waGroup.String()), ID: proto.String("S1")}, MessageTimestamp: proto.Uint64(1_800_000_001),
		MessageStubType: waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD.Enum(), MessageStubParameters: []string{"15550000004@s.whatsapp.net"}}
	data := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String(waPeer.String()), Name: proto.String("Peer"), Messages: []*waHistorySync.HistorySyncMsg{{Message: webText}, {Message: nil}}},
			{ID: proto.String(waGroup.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: webStub}}},
			{ID: proto.String("status@broadcast")},
		},
	}
	parse := func(chat types.JID, web *waWeb.WebMessageInfo) (*events.Message, error) {
		return liveMsg(chat, waPeer, false, web.GetKey().GetID(), web.GetMessage()), nil
	}
	recs, skipped := normalizeWhatsAppHistory(data, parse, whatsAppNormalizeContext{})
	if skipped != 2 {
		t.Fatalf("skipped = %d", skipped)
	}
	var convs, msgs, system int
	for _, r := range recs {
		switch {
		case r.Kind == waRecordConversation:
			convs++
		case r.Type == waTypeSystem:
			system++
			if r.Text != "joined" || r.SenderJid != "15550000004@s.whatsapp.net" || r.Source != waSourceHistory {
				t.Fatalf("system = %+v", r)
			}
		default:
			msgs++
			if r.Source != waSourceHistory || r.Text != "old" {
				t.Fatalf("history message = %+v", r)
			}
		}
	}
	if convs != 2 || msgs != 1 || system != 1 {
		t.Fatalf("convs=%d msgs=%d system=%d", convs, msgs, system)
	}
}

func TestNormalizeGroupInfoJoinLeave(t *testing.T) {
	recs := normalizeWhatsAppGroupInfo(&events.GroupInfo{JID: waGroup, Timestamp: waTS, Join: []types.JID{waPeer}, Leave: []types.JID{waMe}})
	if len(recs) != 2 || recs[0].Text != "joined" || recs[1].Text != "left" || recs[0].Type != waTypeSystem {
		t.Fatalf("group info = %+v", recs)
	}
	if recs[0].WaMessageID == "" || recs[0].SenderJid == recs[1].SenderJid {
		t.Fatal("system records need distinct identities")
	}
}

func TestRedactWhatsAppLog(t *testing.T) {
	in := "pair 2@AbCdEfGhIjKl,mnopQRST,uvwx,yz12 code ABCD-EFGH from 15551234567@s.whatsapp.net and 1203630@g.us and legacy 15557654321-1600000000@g.us"
	out := redactWhatsAppLog(in)
	for _, leak := range []string{"2@AbCdEfGhIjKl", "ABCD-EFGH", "15551234567", "1203630@g.us", "15557654321", "1600000000"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q in %q", leak, out)
		}
	}
}
