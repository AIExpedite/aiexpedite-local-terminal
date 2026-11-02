// whatsapp_normalize.go — whatsmeow events → ingest records.
//
// The ONLY place WhatsApp content is read. It produces whatsAppRecord values
// (the batch record shapes in local/documents/features/whatsapp/WHATSAPP_INTEGRATION.md "Wire contract") and
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
// JIDs whether it arrives through history sync or live, today or after a
// re-link. Every person (a direct chat, a sender, an operation's target)
// therefore goes through the link's identity resolver
// (whatsapp_identity.go), which hands back the identity first written for
// them; groups pass through unchanged.
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

// whatsAppIdentity is the link's identity resolver (whatsapp_identity.go).
type whatsAppIdentity interface {
	// Canonical is the identity the person addressed by primary (with its
	// alternate address alt, empty when the event carries none) is written
	// under. Anything that is not a person comes back as it is.
	Canonical(primary, alt types.JID) types.JID
	// Learn records that pn and lid are one person.
	Learn(pn, lid types.JID)
	// Formers is every identity that person was written under before a join
	// re-pointed one of their forms, newest first; never their identity now.
	Formers(primary, alt types.JID) []types.JID
}

// whatsAppNormalizeContext is what normalization needs beyond the event.
type whatsAppNormalizeContext struct {
	// OwnJID is the linked account (non-AD), to resolve "my message" targets.
	OwnJID types.JID
	// ReconnectedAt marks live messages older than the last reconnect as
	// catch-up (queued by WhatsApp while this companion was offline).
	ReconnectedAt time.Time
	// Identity resolves every person. Nil (pure mapping tests only): the
	// event's own addresses decide, phone number first.
	Identity whatsAppIdentity
}

// canonicalUser is the identity a person is written under.
func (nc whatsAppNormalizeContext) canonicalUser(primary, alt types.JID) types.JID {
	if nc.Identity != nil {
		return nc.Identity.Canonical(primary, alt)
	}
	return canonicalWhatsAppUser(primary, alt)
}

// formerUsers is what the resolver's Formers says; nothing without one.
func (nc whatsAppNormalizeContext) formerUsers(primary, alt types.JID) []types.JID {
	if nc.Identity != nil {
		return nc.Identity.Formers(primary, alt)
	}
	return nil
}

// learn passes a history chunk's mapping to the resolver.
func (nc whatsAppNormalizeContext) learn(pn, lid types.JID) {
	if nc.Identity != nil {
		nc.Identity.Learn(pn, lid)
	}
}

// canonicalWhatsAppUser is the resolver-less fallback: device part dropped,
// the phone-number address preferred over a LID when the event carries both.
func canonicalWhatsAppUser(primary, alt types.JID) types.JID {
	j := primary.ToNonAD()
	if j.Server == types.HiddenUserServer && !alt.IsEmpty() && alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	return j
}

// whatsAppChatAddress is how an event addresses its chat: the chat JID and,
// for a one-to-one chat, the peer's alternate address when the event carries
// it (SenderAlt for the peer's message, RecipientAlt for mine).
func whatsAppChatAddress(src types.MessageSource) (chat, alt types.JID) {
	chat = src.Chat.ToNonAD()
	if src.IsGroup || chat.Server == types.GroupServer {
		return chat, types.EmptyJID
	}
	if src.IsFromMe {
		return chat, src.RecipientAlt
	}
	return chat, src.SenderAlt
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
	rawChat, chatAlt := whatsAppChatAddress(evt.Info.MessageSource)
	if rawChat.IsEmpty() || whatsAppSkippableChat(rawChat) {
		return nil
	}
	chat := nc.canonicalUser(rawChat, chatAlt)
	sender := nc.canonicalUser(evt.Info.Sender, evt.Info.SenderAlt)
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
			// Only the author edits: the target's author is this sender.
			return whatsAppTargetCopies(rec,
				nc.formerUsers(rawChat, chatAlt), nc.formerUsers(evt.Info.Sender, evt.Info.SenderAlt))
		case waE2E.ProtocolMessage_REVOKE:
			rec := base
			rec.Op = waOpRevoke
			rec.WaMessageID = clipWhatsAppID(target.GetID())
			author, authorAlt := evt.Info.Sender, evt.Info.SenderAlt
			if p := target.GetParticipant(); p != "" {
				if j, err := types.ParseJID(p); err == nil {
					author, authorAlt = j, types.EmptyJID
					rec.SenderJid = nc.canonicalUser(j, types.EmptyJID).String()
				}
			}
			return whatsAppTargetCopies(rec, nc.formerUsers(rawChat, chatAlt), nc.formerUsers(author, authorAlt))
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
		author, authorAlt := whatsAppReactionTarget(key.GetParticipant(), key.GetFromMe(), evt.Info, rawChat, chatAlt, nc)
		rec.SenderJid = nc.canonicalUser(author, authorAlt).String()
		reaction := rm.GetText()
		rec.Reaction = &reaction
		targets := whatsAppTargetCopies(rec, nc.formerUsers(rawChat, chatAlt), nc.formerUsers(author, authorAlt))
		out := append([]whatsAppRecord(nil), targets...)
		// A reactor holds one reaction per message, keyed by their identity.
		// One left under a former identity is taken off, or it would show
		// beside every change made since the join.
		removed := ""
		for _, former := range nc.formerUsers(evt.Info.Sender, evt.Info.SenderAlt) {
			for _, t := range targets {
				t.ReactorJid = former.String()
				t.Reaction = &removed
				out = append(out, t)
			}
		}
		return out
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

// whatsAppReactionTarget is the address (primary, alt, as canonicalUser
// takes them) of whoever wrote the message a reaction points at. The key's
// FromMe is from the REACTOR's point of view.
func whatsAppReactionTarget(participant string, keyFromMe bool, info types.MessageInfo, rawChat, chatAlt types.JID, nc whatsAppNormalizeContext) (types.JID, types.JID) {
	if participant != "" {
		if j, err := types.ParseJID(participant); err == nil {
			return j, types.EmptyJID
		}
	}
	if keyFromMe {
		return info.Sender, info.SenderAlt // the reactor reacted to their own message
	}
	if info.IsFromMe {
		return rawChat, chatAlt // I reacted to the peer's message in a 1:1 chat
	}
	if !nc.OwnJID.IsEmpty() {
		return nc.OwnJID, types.EmptyJID // the peer reacted to my message
	}
	return rawChat, chatAlt
}

// whatsAppTargetCopies returns rec, an edit, revoke or reaction, then a copy
// aimed at each place an earlier copy of its target may be stored: under a
// former identity (whatsapp_identity.go, Formers) of the chat - a person's
// 1:1 chat - or of the target's author. The cloud parks a copy whose target
// does not exist, and drops it when the park expires.
func whatsAppTargetCopies(rec whatsAppRecord, chatFormers, authorFormers []types.JID) []whatsAppRecord {
	out := []whatsAppRecord{rec}
	if len(chatFormers) == 0 && len(authorFormers) == 0 {
		return out
	}
	if rec.ChatJid == rec.SenderJid {
		// The peer's own message in their 1:1 chat: the chat and the author
		// are one person, re-pointed together.
		seen := map[string]bool{rec.ChatJid: true}
		for _, f := range append(append([]types.JID(nil), chatFormers...), authorFormers...) {
			if s := f.String(); !seen[s] {
				seen[s] = true
				cp := rec
				cp.ChatJid, cp.SenderJid = s, s
				out = append(out, cp)
			}
		}
		return out
	}
	chats := []string{rec.ChatJid}
	for _, f := range chatFormers {
		chats = append(chats, f.String())
	}
	authors := []string{rec.SenderJid}
	for _, f := range authorFormers {
		authors = append(authors, f.String())
	}
	for i, c := range chats {
		for j, a := range authors {
			if i == 0 && j == 0 {
				continue
			}
			cp := rec
			cp.ChatJid, cp.SenderJid = c, a
			out = append(out, cp)
		}
	}
	return out
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
// "left" against the participant as sender, resolved through the store's LID
// mapping so it matches the identity that member's messages normalize to.
func normalizeWhatsAppGroupInfo(evt *events.GroupInfo, nc whatsAppNormalizeContext) []whatsAppRecord {
	if evt == nil || evt.JID.IsEmpty() {
		return nil
	}
	chat := evt.JID.ToNonAD().String()
	ts := evt.Timestamp.UnixMilli()
	var out []whatsAppRecord
	add := func(list []types.JID, verb string) {
		for _, p := range list {
			who := nc.canonicalUser(p, types.EmptyJID).String()
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
	// The chunk's own mappings are learned before anything in it is resolved:
	// whatsmeow stores them on a goroutine of its own, which this chunk's
	// dispatch races, and history messages carry no alternate address.
	for _, m := range data.GetPhoneNumberToLidMappings() {
		pn, errPN := types.ParseJID(m.GetPnJID())
		lid, errLID := types.ParseJID(m.GetLidJID())
		if errPN == nil && errLID == nil {
			nc.learn(pn, lid)
		}
	}
	for _, conv := range data.GetConversations() {
		if pn, lid := whatsAppConversationPair(conv); !pn.IsEmpty() && !lid.IsEmpty() {
			nc.learn(pn, lid)
		}
	}
	for _, conv := range data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil || whatsAppSkippableChat(chat) {
			skipped++
			continue
		}
		// A direct chat is announced under the same identity its messages
		// normalize to, or the two would land in different conversations.
		rawChat := chat.ToNonAD()
		chat = nc.canonicalUser(rawChat, types.EmptyJID)
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
						SenderJid:   nc.canonicalUser(who, types.EmptyJID).String(),
						WaMessageID: clipWhatsAppID(web.GetKey().GetID()),
						WaTimestamp: ts, Source: waSourceHistory, Type: waTypeSystem, Text: verb,
					})
				}
				continue
			}
			// whatsmeow parses against the chat as WhatsApp keyed it.
			evt, err := parse(rawChat, web)
			if err != nil || evt == nil {
				skipped++
				continue
			}
			records = append(records, normalizeWhatsAppMessage(evt, waSourceHistory, nc)...)
		}
	}
	return records, skipped
}

// whatsAppConversationPair is the (phone number, LID) pair a history
// conversation names, empty where it names none. The conversation's own id
// is one of the two forms, and usually only the OTHER form's field is set
// (a LID-keyed chat carries pnJID, a phone-keyed one lidJID), so the id
// stands in for whichever field is empty.
func whatsAppConversationPair(conv *waHistorySync.Conversation) (pn, lid types.JID) {
	parse := func(s string) types.JID {
		if s == "" {
			return types.EmptyJID
		}
		j, err := types.ParseJID(s)
		if err != nil {
			return types.EmptyJID
		}
		return j.ToNonAD()
	}
	pn, lid = parse(conv.GetPnJID()), parse(conv.GetLidJID())
	switch id := parse(conv.GetID()); id.Server {
	case types.HiddenUserServer:
		if lid.IsEmpty() {
			lid = id
		}
	case types.DefaultUserServer, types.LegacyUserServer:
		if pn.IsEmpty() {
			pn = id
		}
	}
	return pn, lid
}

// whatsAppConversationRecord announces a chat seen live for the first time,
// under the chat identity its first message was normalized to.
func whatsAppConversationRecord(chatJid string, isGroup bool, name string) whatsAppRecord {
	chatType := waChatDirect
	if isGroup {
		chatType = waChatGroup
	}
	return whatsAppRecord{Kind: waRecordConversation, ChatJid: chatJid, ChatType: chatType, Name: strings.TrimSpace(name)}
}
