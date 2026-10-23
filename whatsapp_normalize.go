// whatsapp_normalize.go — whatsmeow events → ingest records.
//
// The ONLY place WhatsApp content is read. It produces whatsAppRecord values
// (the batch record shapes in WHATSAPP_INTEGRATION.md "Wire contract") and
// nothing else: no log line here or downstream ever carries text, a JID, a
// name or a media detail — the supervisor logs counts and link ids only.
//
// Coverage (shared-constants WHATSAPP_MESSAGE_TYPES): text and captions,
// replies (quotedId), edits, deletes for everyone, reactions, group join /
// leave notices, and media as METADATA ONLY (mimetype, size, duration, file
// name) — never bytes, never a location's coordinates. Everything else
// (polls, view-once, payments, …) is `unsupported`, so the thread still shows
// that something arrived.
//
// Identity. The cloud hashes (linkId, chatJid, senderJid, waMessageId) into
// the message document id, so the same message must normalize to the same
// JIDs whether it arrives through history sync or live. WhatsApp addresses a
// person either by phone number or by LID; canonicalWhatsAppUser prefers the
// phone-number form whenever whatsmeow knows it, and drops the device part.
package main

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Persisted vocabulary — pinned against shared-constants by
// whatsapp_contract_test.go.
const (
	waRecordConversation = "conversation"
	waRecordMessage      = "message"

	waOpNew      = "new"
	waOpEdit     = "edit"
	waOpRevoke   = "revoke"
	waOpReaction = "reaction"

	waSourceHistory = "history"
	waSourceLive    = "live"

	waTypeText        = "text"
	waTypeImage       = "image"
	waTypeVideo       = "video"
	waTypeAudio       = "audio"
	waTypeVoiceNote   = "voiceNote"
	waTypeDocument    = "document"
	waTypeSticker     = "sticker"
	waTypeLocation    = "location"
	waTypeContact     = "contact"
	waTypeSystem      = "system"
	waTypeUnsupported = "unsupported"

	waChatDirect = "direct"
	waChatGroup  = "group"

	waMaxTextChars = 16 * 1024
	waMaxIDChars   = 256
)

// whatsAppMedia is media METADATA. Bytes are never downloaded.
type whatsAppMedia struct {
	Mimetype    string `json:"mimetype,omitempty"`
	Size        uint64 `json:"size,omitempty"`
	DurationSec uint32 `json:"durationSec,omitempty"`
	FileName    string `json:"fileName,omitempty"`
}

// whatsAppRecord is one batch record. DeviceSeq is assigned by the outbox.
type whatsAppRecord struct {
	Kind      string `json:"kind"`
	Op        string `json:"op,omitempty"`
	DeviceSeq int64  `json:"deviceSeq"`
	ChatJid   string `json:"chatJid"`
	// conversation
	ChatType string `json:"chatType,omitempty"`
	Name     string `json:"name,omitempty"`
	// message
	SenderJid   string         `json:"senderJid,omitempty"`
	FromMe      bool           `json:"fromMe,omitempty"`
	WaMessageID string         `json:"waMessageId,omitempty"`
	WaTimestamp int64          `json:"waTimestamp,omitempty"`
	Source      string         `json:"source,omitempty"`
	CatchUp     bool           `json:"catchUp,omitempty"`
	Type        string         `json:"type,omitempty"`
	Text        string         `json:"text,omitempty"`
	Media       *whatsAppMedia `json:"media,omitempty"`
	QuotedID    string         `json:"quotedId,omitempty"`
	PushName    string         `json:"pushName,omitempty"`
	EditVersion int64          `json:"editVersion,omitempty"`
	ReactorJid  string         `json:"reactorJid,omitempty"`
	// Reaction is a pointer because "" (remove my reaction) is meaningful.
	Reaction *string `json:"reaction,omitempty"`
}

// whatsAppNormalizeContext is what normalization needs beyond the event.
type whatsAppNormalizeContext struct {
	// OwnJID is the linked account (non-AD), to resolve "my message" targets.
	OwnJID types.JID
	// ReconnectedAt marks live messages older than the last reconnect as
	// catch-up (queued by WhatsApp while this companion was offline).
	ReconnectedAt time.Time
}

// canonicalWhatsAppUser drops the device part and prefers the phone-number
// address over a LID when the alternative is known.
func canonicalWhatsAppUser(primary, alt types.JID) types.JID {
	j := primary.ToNonAD()
	if j.Server == types.HiddenUserServer && !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	return j
}

// canonicalWhatsAppChat is the chat's stable address: groups as they are, a
// one-to-one chat by the peer's phone number when whatsmeow knows it.
func canonicalWhatsAppChat(src types.MessageSource) types.JID {
	chat := src.Chat.ToNonAD()
	if src.IsGroup || chat.Server != types.HiddenUserServer {
		return chat
	}
	if !src.IsFromMe && src.SenderAlt.Server == types.DefaultUserServer {
		return src.SenderAlt.ToNonAD()
	}
	if src.IsFromMe && src.RecipientAlt.Server == types.DefaultUserServer {
		return src.RecipientAlt.ToNonAD()
	}
	return chat
}

func whatsAppChatType(chat types.JID) string {
	if chat.Server == types.GroupServer {
		return waChatGroup
	}
	return waChatDirect
}

func clipWhatsAppText(s string) string {
	if len(s) <= waMaxTextChars {
		return s
	}
	cut := waMaxTextChars
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func clipWhatsAppID(s string) string {
	if len(s) > waMaxIDChars {
		return s[:waMaxIDChars]
	}
	return s
}

// whatsAppSkippableChat: status broadcasts (stories) and newsletters are out of
// scope.
func whatsAppSkippableChat(chat types.JID) bool {
	return chat == types.StatusBroadcastJID || chat.Server == types.BroadcastServer || chat.Server == types.NewsletterServer
}

// normalizeWhatsAppMessage maps one message event (live, or parsed from a
// history sync) to zero or more records.
func normalizeWhatsAppMessage(evt *events.Message, source string, nc whatsAppNormalizeContext) []whatsAppRecord {
	if evt == nil {
		return nil
	}
	chat := canonicalWhatsAppChat(evt.Info.MessageSource)
	if chat.IsEmpty() || whatsAppSkippableChat(chat) {
		return nil
	}
	sender := canonicalWhatsAppUser(evt.Info.Sender, evt.Info.SenderAlt)
	base := whatsAppRecord{
		Kind:        waRecordMessage,
		ChatJid:     chat.String(),
		SenderJid:   sender.String(),
		WaMessageID: clipWhatsAppID(string(evt.Info.ID)),
		WaTimestamp: evt.Info.Timestamp.UnixMilli(),
		Source:      source,
	}
	msg := evt.Message
	if msg == nil {
		return nil
	}

	if pm := msg.GetProtocolMessage(); pm != nil {
		target := pm.GetKey()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			rec := base
			rec.Op = waOpEdit
			rec.WaMessageID = clipWhatsAppID(target.GetID())
			rec.EditVersion = pm.GetTimestampMS()
			if rec.EditVersion == 0 {
				rec.EditVersion = base.WaTimestamp
			}
			text, _, _, _ := whatsAppContent(pm.GetEditedMessage())
			rec.Text = clipWhatsAppText(text)
			return []whatsAppRecord{rec}
		case waE2E.ProtocolMessage_REVOKE:
			rec := base
			rec.Op = waOpRevoke
			rec.WaMessageID = clipWhatsAppID(target.GetID())
			if p := target.GetParticipant(); p != "" {
				if j, err := types.ParseJID(p); err == nil {
					rec.SenderJid = canonicalWhatsAppUser(j, types.EmptyJID).String()
				}
			}
			return []whatsAppRecord{rec}
		default:
			// Key shares, history notifications, ephemeral settings: not content.
			return nil
		}
	}

	if rm := msg.GetReactionMessage(); rm != nil {
		rec := base
		rec.Op = waOpReaction
		key := rm.GetKey()
		rec.WaMessageID = clipWhatsAppID(key.GetID())
		rec.ReactorJid = sender.String()
		rec.SenderJid = whatsAppReactionTargetSender(key.GetParticipant(), key.GetFromMe(), evt.Info, chat, sender, nc).String()
		reaction := rm.GetText()
		rec.Reaction = &reaction
		return []whatsAppRecord{rec}
	}

	rec := base
	rec.Op = waOpNew
	rec.FromMe = evt.Info.IsFromMe
	rec.PushName = evt.Info.PushName
	if source == waSourceLive && !nc.ReconnectedAt.IsZero() && evt.Info.Timestamp.Before(nc.ReconnectedAt) {
		rec.CatchUp = true
	}
	text, typ, media, quoted := whatsAppContent(msg)
	if evt.IsViewOnce {
		// View-once content is never stored, not even its caption.
		text, typ, media = "", waTypeUnsupported, nil
	}
	rec.Text = clipWhatsAppText(text)
	rec.Type = typ
	rec.Media = media
	rec.QuotedID = clipWhatsAppID(quoted)
	return []whatsAppRecord{rec}
}

// whatsAppReactionTargetSender resolves who wrote the message a reaction
// points at. The key's FromMe is from the REACTOR's point of view.
func whatsAppReactionTargetSender(participant string, keyFromMe bool, info types.MessageInfo, chat, reactor types.JID, nc whatsAppNormalizeContext) types.JID {
	if participant != "" {
		if j, err := types.ParseJID(participant); err == nil {
			return canonicalWhatsAppUser(j, types.EmptyJID)
		}
	}
	if keyFromMe {
		return reactor // the reactor reacted to their own message
	}
	if info.IsFromMe {
		return chat // I reacted to the peer's message in a 1:1 chat
	}
	if !nc.OwnJID.IsEmpty() {
		return nc.OwnJID.ToNonAD() // the peer reacted to my message
	}
	return chat
}

// whatsAppContent extracts (text/caption, type, media metadata, quoted id).
func whatsAppContent(msg *waE2E.Message) (string, string, *whatsAppMedia, string) {
	if msg == nil {
		return "", waTypeUnsupported, nil, ""
	}
	if dwc := msg.GetDocumentWithCaptionMessage(); dwc != nil && dwc.GetMessage() != nil {
		msg = dwc.GetMessage()
	}
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation(), waTypeText, nil, ""
	case msg.GetExtendedTextMessage() != nil:
		m := msg.GetExtendedTextMessage()
		return m.GetText(), waTypeText, nil, m.GetContextInfo().GetStanzaID()
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		return m.GetCaption(), waTypeImage, &whatsAppMedia{Mimetype: m.GetMimetype(), Size: m.GetFileLength()}, m.GetContextInfo().GetStanzaID()
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		return m.GetCaption(), waTypeVideo, &whatsAppMedia{Mimetype: m.GetMimetype(), Size: m.GetFileLength(), DurationSec: m.GetSeconds()}, m.GetContextInfo().GetStanzaID()
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		typ := waTypeAudio
		if m.GetPTT() {
			typ = waTypeVoiceNote
		}
		return "", typ, &whatsAppMedia{Mimetype: m.GetMimetype(), Size: m.GetFileLength(), DurationSec: m.GetSeconds()}, m.GetContextInfo().GetStanzaID()
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		return m.GetCaption(), waTypeDocument, &whatsAppMedia{Mimetype: m.GetMimetype(), Size: m.GetFileLength(), FileName: m.GetFileName()}, m.GetContextInfo().GetStanzaID()
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		return "", waTypeSticker, &whatsAppMedia{Mimetype: m.GetMimetype(), Size: m.GetFileLength()}, m.GetContextInfo().GetStanzaID()
	case msg.GetLocationMessage() != nil:
		// Coordinates are not metadata: never stored.
		return "", waTypeLocation, &whatsAppMedia{}, msg.GetLocationMessage().GetContextInfo().GetStanzaID()
	case msg.GetLiveLocationMessage() != nil:
		return "", waTypeLocation, &whatsAppMedia{}, ""
	case msg.GetContactMessage() != nil:
		return "", waTypeContact, &whatsAppMedia{}, msg.GetContactMessage().GetContextInfo().GetStanzaID()
	case msg.GetContactsArrayMessage() != nil:
		return "", waTypeContact, &whatsAppMedia{}, ""
	}
	return "", waTypeUnsupported, nil, ""
}

// normalizeWhatsAppGroupInfo turns join / leave notices into system records.
// The participants are not written into the text — the thread says "joined" /
// "left" against the participant as sender.
func normalizeWhatsAppGroupInfo(evt *events.GroupInfo) []whatsAppRecord {
	if evt == nil || evt.JID.IsEmpty() {
		return nil
	}
	chat := evt.JID.ToNonAD().String()
	ts := evt.Timestamp.UnixMilli()
	var out []whatsAppRecord
	add := func(list []types.JID, verb string) {
		for _, p := range list {
			who := canonicalWhatsAppUser(p, types.EmptyJID).String()
			out = append(out, whatsAppRecord{
				Kind: waRecordMessage, Op: waOpNew, ChatJid: chat, SenderJid: who,
				// No WhatsApp id exists for a notification: one is derived from
				// what makes it unique, so a redelivery collapses.
				WaMessageID: "sys-" + verb + "-" + strconv.FormatInt(ts, 10),
				WaTimestamp: ts, Source: waSourceLive, Type: waTypeSystem, Text: verb,
			})
		}
	}
	add(evt.Join, "joined")
	add(evt.Leave, "left")
	return out
}

// whatsAppHistoryStubVerb maps the history-sync stub types we show.
func whatsAppHistoryStubVerb(stub waWeb.WebMessageInfo_StubType) string {
	switch stub {
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE:
		return "joined"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE, waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE:
		return "left"
	}
	return ""
}

// whatsAppWebMessageParser is whatsmeow's Client.ParseWebMessage.
type whatsAppWebMessageParser func(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)

// normalizeWhatsAppHistory maps one history-sync chunk to conversation and
// message records (source: history). Unparseable entries are skipped and
// counted, never logged.
func normalizeWhatsAppHistory(data *waHistorySync.HistorySync, parse whatsAppWebMessageParser, nc whatsAppNormalizeContext) (records []whatsAppRecord, skipped int) {
	if data == nil {
		return nil, 0
	}
	for _, conv := range data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil || whatsAppSkippableChat(chat) {
			skipped++
			continue
		}
		chat = chat.ToNonAD()
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		records = append(records, whatsAppRecord{
			Kind: waRecordConversation, ChatJid: chat.String(), ChatType: whatsAppChatType(chat), Name: name,
		})
		for _, hm := range conv.GetMessages() {
			web := hm.GetMessage()
			if web == nil {
				skipped++
				continue
			}
			if verb := whatsAppHistoryStubVerb(web.GetMessageStubType()); verb != "" {
				ts := int64(web.GetMessageTimestamp()) * 1000
				for _, p := range web.GetMessageStubParameters() {
					who, err := types.ParseJID(p)
					if err != nil {
						continue
					}
					records = append(records, whatsAppRecord{
						Kind: waRecordMessage, Op: waOpNew, ChatJid: chat.String(),
						SenderJid:   canonicalWhatsAppUser(who, types.EmptyJID).String(),
						WaMessageID: clipWhatsAppID(web.GetKey().GetID()),
						WaTimestamp: ts, Source: waSourceHistory, Type: waTypeSystem, Text: verb,
					})
				}
				continue
			}
			evt, err := parse(chat, web)
			if err != nil || evt == nil {
				skipped++
				continue
			}
			records = append(records, normalizeWhatsAppMessage(evt, waSourceHistory, nc)...)
		}
	}
	return records, skipped
}

// whatsAppConversationRecord announces a chat seen live for the first time.
func whatsAppConversationRecord(src types.MessageSource, name string) whatsAppRecord {
	chat := canonicalWhatsAppChat(src)
	return whatsAppRecord{Kind: waRecordConversation, ChatJid: chat.String(), ChatType: whatsAppChatType(chat), Name: strings.TrimSpace(name)}
}
