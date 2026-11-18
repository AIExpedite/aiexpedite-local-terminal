package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Contract pins. The Go agent cannot import shared-constants, so these
// literals are the agent's copy of WHATSAPP_LINK_STATES, WHATSAPP_COMMAND_TYPES,
// WHATSAPP_SYNC_KINDS, the message vocabulary and the error codes it reports
// (shared-constants src/constants/whatsapp.js, tests/whatsapp.test.js pins the
// same strings). A rename on either side is a data migration and an agent
// flag day — change both or neither.
func TestWhatsAppContractPins(t *testing.T) {
	pins := [][2]string{
		{waStatePairingRequired, "pairingRequired"},
		{waStatePairing, "pairing"},
		{waStateConnecting, "connecting"},
		{waStateSyncingHistory, "syncingHistory"},
		{waStateConnected, "connected"},
		{waStateAuthExpired, "authExpired"},
		{waStateDisconnecting, "disconnecting"},
		{waStateError, "error"},
		{whatsAppLinkStartCommand, "__whatsapp_link_start__"},
		{whatsAppLinkCancelCommand, "__whatsapp_link_cancel__"},
		{whatsAppUnlinkCommand, "__whatsapp_unlink__"},
		{whatsAppResyncStateCommand, "__whatsapp_resync_state__"},
		{whatsAppSendTextCommand, "__whatsapp_send_text__"},
		{waSyncKindState, "state"},
		{waSyncKindHeartbeat, "heartbeat"},
		{waSyncKindPairing, "pairing"},
		{waSyncKindBatch, "batch"},
		{waSyncKindAck, "ack"},
		{waSyncKindSendResult, "sendResult"},
		{waSendOutcomeSent, "sent"},
		{waSendOutcomeFailed, "failed"},
		{waSendOutcomeUnknown, "unknown"},
		{waAckUnlink, "unlink"},
		{waPairQR, "qr"},
		{waPairPhoneCode, "phoneCode"},
		{waRecordConversation, "conversation"},
		{waRecordMessage, "message"},
		{waOpNew, "new"},
		{waOpEdit, "edit"},
		{waOpRevoke, "revoke"},
		{waOpReaction, "reaction"},
		{waSourceHistory, "history"},
		{waSourceLive, "live"},
		{waTypeText, "text"},
		{waTypeImage, "image"},
		{waTypeVideo, "video"},
		{waTypeAudio, "audio"},
		{waTypeVoiceNote, "voiceNote"},
		{waTypeDocument, "document"},
		{waTypeSticker, "sticker"},
		{waTypeLocation, "location"},
		{waTypeContact, "contact"},
		{waTypeSystem, "system"},
		{waTypeUnsupported, "unsupported"},
		{waChatDirect, "direct"},
		{waChatGroup, "group"},
		{waErrAccountRestricted, "WHATSAPP_ACCOUNT_RESTRICTED"},
		{waErrConnectFailing, "WHATSAPP_CONNECT_FAILING"},
		{waErrClientOutdated, "WHATSAPP_CLIENT_OUTDATED"},
		{waErrSecureStorageUnavailable, "WHATSAPP_SECURE_STORAGE_UNAVAILABLE"},
		{waErrLinkFenced, "WHATSAPP_LINK_FENCED"},
		// The device's definite send refusals (WHATSAPP_ERROR_CODES).
		{waErrSendStale, "WHATSAPP_SEND_STALE"},
		{waErrSendNotConnected, "WHATSAPP_SEND_NOT_CONNECTED"},
		{waErrSendRecipientUnavailable, "WHATSAPP_SEND_RECIPIENT_UNAVAILABLE"},
		{waErrSendRefused, "WHATSAPP_SEND_REFUSED"},
		{whatsAppProposalIDPattern.String(), "^[A-Za-z0-9]{20}$"}, // WHATSAPP_PROPOSAL_ID_PATTERN
	}
	for _, p := range pins {
		if got, want := p[0], p[1]; got != want {
			t.Errorf("pinned value drifted: %q != %q", got, want)
		}
	}
	if whatsAppPairingHKDFSalt != "aix-whatsapp-pairing-v1" || whatsAppPairingHKDFInfo != "aix-whatsapp-pairing-payload" {
		t.Error("pairing HKDF parameters drifted from WHATSAPP_PAIRING_CRYPTO")
	}
	if whatsAppBatchMaxRecords != 200 || whatsAppBatchMaxBodyBytes != 512*1024 {
		t.Error("batch bounds drifted from WHATSAPP_INGEST_LIMITS")
	}
	if whatsAppCommandParamsMaxBytes != 4096 {
		t.Error("the command parameter bound drifted from WHATSAPP_COMMAND_PARAMS_MAX_BYTES")
	}
	// terminal-service's WHATSAPP_MIN_AGENT_PROTOCOL refuses to pair below
	// this; raising it there before a release reports it locks every
	// computer out of linking. `sendText` is what isWhatsAppSendCapable
	// reads before terminal-service publishes a send.
	capability, _ := json.Marshal(whatsAppCapability{Supported: true, SecureStorage: true, Protocol: whatsAppAgentProtocol, SendText: true})
	if whatsAppAgentProtocol != 2 || string(capability) != `{"supported":true,"secureStorage":true,"protocol":2,"sendText":true}` {
		t.Errorf("capabilities.whatsapp drifted: %s", capability)
	}
	// The sendResult report's keys (the whatsapp-service ingest reads them).
	p := whatsAppSendTextParams{LinkID: "lnk_1", Generation: 3, ProposalID: "AbCdEfGhIjKlMnOpQrSt", Attempt: 2, ChatJid: "x", Text: "y"}
	for _, c := range []struct {
		res  whatsAppSendResult
		want string
	}{
		{whatsAppSendResult{Outcome: waSendOutcomeSent, WaMessageID: "3EB0AB", SentAt: 1_800_000_000_000},
			`{"attempt":2,"generation":3,"kind":"sendResult","linkId":"lnk_1","outcome":"sent","proposalId":"AbCdEfGhIjKlMnOpQrSt","sentAt":1800000000000,"v":1,"waMessageId":"3EB0AB"}`},
		{whatsAppSendFailure(waErrSendStale),
			`{"attempt":2,"errorCode":"WHATSAPP_SEND_STALE","generation":3,"kind":"sendResult","linkId":"lnk_1","outcome":"failed","proposalId":"AbCdEfGhIjKlMnOpQrSt","v":1}`},
		{whatsAppSendResult{Outcome: waSendOutcomeUnknown},
			`{"attempt":2,"generation":3,"kind":"sendResult","linkId":"lnk_1","outcome":"unknown","proposalId":"AbCdEfGhIjKlMnOpQrSt","v":1}`},
	} {
		if raw, _ := json.Marshal(c.res.payload(p, 3)); string(raw) != c.want {
			t.Errorf("sendResult drifted:\n got %s\nwant %s", raw, c.want)
		}
	}
}

// The sync signature canonical, asserted against the vectors terminal-service
// verifies too (testdata/whatsapp_sync_signature_vectors.json in both repos).
func TestWhatsAppSyncSignatureVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/whatsapp_sync_signature_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name, AgentID, Secret, Body, SignedMessage, Signature string
			Timestamp                                             int64
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range file.Vectors {
		msg := buildWhatsAppSyncSignedMessage(v.AgentID, v.Timestamp, []byte(v.Body))
		if msg != v.SignedMessage {
			t.Errorf("%s: canonical %q, want %q", v.Name, msg, v.SignedMessage)
		}
		if sig := generateHMAC(msg, v.Secret); sig != v.Signature {
			t.Errorf("%s: signature %s, want %s", v.Name, sig, v.Signature)
		}
	}
}

// Every WhatsApp command skips the generic staleness and rate-limit gates and
// passes a drain (whatsapp_commands.go, isWhatsAppCommand): their refusals
// answer on the results topic, which whatsapp-service never reads. The send
// applies the staleness rule itself (whatsapp_send_test.go).
func TestWhatsAppCommandsAreOperationalDemand(t *testing.T) {
	for _, c := range []string{whatsAppLinkStartCommand, whatsAppLinkCancelCommand, whatsAppUnlinkCommand, whatsAppResyncStateCommand, whatsAppSendTextCommand} {
		if !isInternalDemandCommand(c) {
			t.Errorf("%s must skip staleness / rate limits", c)
		}
		if !isOperationalCommand(c) {
			t.Errorf("%s must pass a drain", c)
		}
	}
	if isWhatsAppCommand("__whatsapp_unknown__") {
		t.Error("unknown WhatsApp command accepted")
	}
}

// A send command's parameters are checked for shape before anything else:
// one JSON object of at most WHATSAPP_COMMAND_PARAMS_MAX_BYTES, a link and a
// generation, a 20-character proposal id, an attempt from 1, a chat and a
// text. Unknown fields are ignored (a newer whatsapp-service may add some).
func TestParseWhatsAppSendTextParams(t *testing.T) {
	good := `{"linkId":"lnk_1","generation":2,"proposalId":"AbCdEfGhIjKlMnOpQrSt","attempt":1,"chatJid":"15550000002@s.whatsapp.net","text":"see you at 6","later":true}`
	p, err := parseWhatsAppSendTextParams(commandMsg{Args: []string{good}})
	if err != nil || p.LinkID != "lnk_1" || p.Generation != 2 || p.ProposalID != "AbCdEfGhIjKlMnOpQrSt" || p.Attempt != 1 ||
		p.ChatJid != "15550000002@s.whatsapp.net" || p.Text != "see you at 6" {
		t.Fatalf("parse: %+v %v", p, err)
	}
	// The bound is in UTF-8 bytes: 4096 of them pass, one more does not.
	base := `{"linkId":"lnk_1","generation":2,"proposalId":"AbCdEfGhIjKlMnOpQrSt","attempt":1,"chatJid":"1@s.whatsapp.net","text":""}`
	// n more bytes of text, mostly two-byte "é": measured in UTF-16 units it
	// would look half as long.
	fill := func(n int) string {
		return strings.Replace(base, `"text":""`, `"text":"`+strings.Repeat("é", n/2)+strings.Repeat("x", n%2)+`"`, 1)
	}
	atBound := fill(whatsAppCommandParamsMaxBytes - len(base))
	if len(atBound) != whatsAppCommandParamsMaxBytes {
		t.Fatalf("test setup: %d bytes", len(atBound))
	}
	if _, err := parseWhatsAppSendTextParams(commandMsg{Args: []string{atBound}}); err != nil {
		t.Fatalf("parameters at the bound refused: %v", err)
	}
	if _, err := parseWhatsAppSendTextParams(commandMsg{Args: []string{fill(whatsAppCommandParamsMaxBytes - len(base) + 1)}}); err == nil {
		t.Fatal("parameters past the bound accepted")
	}
	for name, bad := range map[string][]string{
		"no argument":       nil,
		"two arguments":     {good, good},
		"not JSON":          {"not json"},
		"bad link id":       {strings.Replace(good, `"lnk_1"`, `"../x"`, 1)},
		"generation 0":      {strings.Replace(good, `"generation":2`, `"generation":0`, 1)},
		"short proposal id": {strings.Replace(good, `"AbCdEfGhIjKlMnOpQrSt"`, `"AbCdEfGhIjKlMnOpQrS"`, 1)},
		"proposal id shape": {strings.Replace(good, `"AbCdEfGhIjKlMnOpQrSt"`, `"AbCdEfGhIjKlMnOpQr-t"`, 1)},
		"attempt 0":         {strings.Replace(good, `"attempt":1`, `"attempt":0`, 1)},
		"attempt missing":   {strings.Replace(good, `"attempt":1,`, ``, 1)},
		"empty chat":        {strings.Replace(good, `"15550000002@s.whatsapp.net"`, `""`, 1)},
		"empty text":        {strings.Replace(good, `"see you at 6"`, `""`, 1)},
		"text not a string": {strings.Replace(good, `"see you at 6"`, `7`, 1)},
	} {
		if _, err := parseWhatsAppSendTextParams(commandMsg{Args: bad}); err == nil {
			t.Errorf("%s: accepted %v", name, bad)
		}
	}
}

func TestParseWhatsAppCommandParams(t *testing.T) {
	ok := commandMsg{Args: []string{`{"linkId":"lnk_1","generation":2,"method":"qr","browserPub":"x"}`}}
	if p, err := parseWhatsAppCommandParams(ok); err != nil || p.LinkID != "lnk_1" || p.Generation != 2 || p.LastAckedSeq != 0 {
		t.Fatalf("parse: %+v %v", p, err)
	}
	// whatsapp-service sends the link's replay watermark with a link start.
	withSeq := commandMsg{Args: []string{`{"linkId":"lnk_1","generation":3,"method":"qr","browserPub":"x","lastAckedSeq":4812}`}}
	if p, err := parseWhatsAppCommandParams(withSeq); err != nil || p.LastAckedSeq != 4812 {
		t.Fatalf("parse lastAckedSeq: %+v %v", p, err)
	}
	for _, bad := range [][]string{nil, {"not json"}, {`{"linkId":"../x","generation":1}`}, {`{"linkId":"lnk","generation":0}`}, {"{}", "{}"}} {
		if _, err := parseWhatsAppCommandParams(commandMsg{Args: bad}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
