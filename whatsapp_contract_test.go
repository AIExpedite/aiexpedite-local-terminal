package main

import (
	"encoding/json"
	"os"
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
		{waSyncKindState, "state"},
		{waSyncKindHeartbeat, "heartbeat"},
		{waSyncKindPairing, "pairing"},
		{waSyncKindBatch, "batch"},
		{waSyncKindAck, "ack"},
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

func TestWhatsAppCommandsAreOperationalDemand(t *testing.T) {
	for _, c := range []string{whatsAppLinkStartCommand, whatsAppLinkCancelCommand, whatsAppUnlinkCommand, whatsAppResyncStateCommand} {
		if !isInternalDemandCommand(c) {
			t.Errorf("%s must skip staleness / rate limits", c)
		}
		if !isOperationalCommand(c) {
			t.Errorf("%s must pass a drain", c)
		}
	}
	if isWhatsAppCommand("__whatsapp_send__") {
		t.Error("unknown WhatsApp command accepted")
	}
}

func TestParseWhatsAppCommandParams(t *testing.T) {
	ok := commandMsg{Args: []string{`{"linkId":"lnk_1","generation":2,"method":"qr","browserPub":"x"}`}}
	if p, err := parseWhatsAppCommandParams(ok); err != nil || p.LinkID != "lnk_1" || p.Generation != 2 {
		t.Fatalf("parse: %+v %v", p, err)
	}
	for _, bad := range [][]string{nil, {"not json"}, {`{"linkId":"../x","generation":1}`}, {`{"linkId":"lnk","generation":0}`}, {"{}", "{}"}} {
		if _, err := parseWhatsAppCommandParams(commandMsg{Args: bad}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
