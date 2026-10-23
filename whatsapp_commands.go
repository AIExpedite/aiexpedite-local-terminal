// whatsapp_commands.go — the four signed WhatsApp demand commands.
//
//	__whatsapp_link_start__   {linkId, generation, workspaceID, method, browserPub, phoneNumber?}
//	__whatsapp_link_cancel__  {linkId, generation}
//	__whatsapp_unlink__       {linkId, generation, workspaceID}
//	__whatsapp_resync_state__ {linkId, generation}
//
// The parameters travel as ONE JSON object in the signed Args[0], so the
// command signature canonical is unchanged on both sides. They are internal
// demand commands (never stale-refused or rate-limited — an unlink queued for
// a week must still run) and operational (a drain never refuses them).
// Results go back over POST /device/:agentId/whatsapp/sync, not the results
// topic, so the handler publishes nothing. The phone number and browserPub
// are never logged.
package main

import (
	"encoding/json"
	"errors"
)

// Pinned against shared-constants WHATSAPP_COMMAND_TYPES (whatsapp_contract_test.go).
const (
	whatsAppLinkStartCommand   = "__whatsapp_link_start__"
	whatsAppLinkCancelCommand  = "__whatsapp_link_cancel__"
	whatsAppUnlinkCommand      = "__whatsapp_unlink__"
	whatsAppResyncStateCommand = "__whatsapp_resync_state__"
)

func isWhatsAppCommand(command string) bool {
	switch command {
	case whatsAppLinkStartCommand, whatsAppLinkCancelCommand, whatsAppUnlinkCommand, whatsAppResyncStateCommand:
		return true
	}
	return false
}

func parseWhatsAppCommandParams(cmd commandMsg) (whatsAppLinkStartParams, error) {
	var p whatsAppLinkStartParams
	if len(cmd.Args) != 1 {
		return p, errors.New("expected one JSON argument")
	}
	if err := json.Unmarshal([]byte(cmd.Args[0]), &p); err != nil {
		return p, errors.New("argument is not JSON")
	}
	if !validWhatsAppLinkID(p.LinkID) || p.Generation <= 0 {
		return p, errors.New("missing linkId / generation")
	}
	return p, nil
}

// handleWhatsAppCommand dispatches a verified command. A link start runs to its
// durable accepted state (runner persisted, pairing begun) before returning;
// the other commands run on the supervisor's goroutines. Either way it returns
// before the message is acked.
func handleWhatsAppCommand(cmd commandMsg) {
	p, err := parseWhatsAppCommandParams(cmd)
	if err != nil {
		logWhatsApp("-", "command "+cmd.Command+" refused: "+err.Error())
		return
	}
	sup := globalWhatsAppSupervisor
	switch cmd.Command {
	case whatsAppLinkStartCommand:
		if p.Method != waPairQR && p.Method != waPairPhoneCode {
			logWhatsApp(p.LinkID, "link start refused: unknown method")
			return
		}
		if p.Method == waPairPhoneCode && p.PhoneNumber == "" {
			logWhatsApp(p.LinkID, "link start refused: phone number missing")
			return
		}
		if _, err := parseWhatsAppBrowserPub(p.BrowserPub); err != nil {
			logWhatsApp(p.LinkID, "link start refused: bad browser key")
			return
		}
		logWhatsApp(p.LinkID, "link start ("+p.Method+")")
		// Synchronous: the Pub/Sub message is acked only once the runner
		// is persisted and pairing has begun, so an exit before that
		// leaves the command to be redelivered instead of lost.
		sup.linkStart(p)
	case whatsAppLinkCancelCommand:
		go sup.linkCancel(p.LinkID, p.Generation)
	case whatsAppUnlinkCommand:
		go sup.unlink(p.LinkID, p.Generation)
	case whatsAppResyncStateCommand:
		go sup.resyncState(p.LinkID, p.Generation)
	}
}
