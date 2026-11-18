// whatsapp_commands.go — the five signed WhatsApp demand commands.
//
//	__whatsapp_link_start__   {linkId, generation, workspaceID, method, browserPub, phoneNumber?, lastAckedSeq?}
//	__whatsapp_link_cancel__  {linkId, generation}
//	__whatsapp_unlink__       {linkId, generation, workspaceID}
//	__whatsapp_resync_state__ {linkId, generation}
//	__whatsapp_send_text__    {linkId, generation, proposalId, attempt, chatJid, text}
//
// The parameters travel as ONE JSON object in the signed Args[0], so the
// command signature canonical is unchanged on both sides. They are internal
// demand commands (never stale-refused or rate-limited by the generic gate —
// an unlink queued for a week must still run, and a send applies the
// staleness rule itself, whatsapp_send.go) and operational (a drain never
// refuses them; see isWhatsAppCommand). Results go back over POST
// /device/:agentId/whatsapp/sync, not the results topic, so the handler
// publishes nothing. The phone number, browserPub, and a send's text and chat
// are never logged.
package main

import (
	"encoding/json"
	"errors"
	"regexp"
)

// Pinned against shared-constants WHATSAPP_COMMAND_TYPES (whatsapp_contract_test.go).
const (
	whatsAppLinkStartCommand   = "__whatsapp_link_start__"
	whatsAppLinkCancelCommand  = "__whatsapp_link_cancel__"
	whatsAppUnlinkCommand      = "__whatsapp_unlink__"
	whatsAppResyncStateCommand = "__whatsapp_resync_state__"
	whatsAppSendTextCommand    = "__whatsapp_send_text__"
)

// whatsAppCommandParamsMaxBytes is shared-constants
// WHATSAPP_COMMAND_PARAMS_MAX_BYTES: JSON.stringify(params) in UTF-8 bytes,
// which is exactly the signed Args[0]. terminal-service publishes nothing past
// it, and the agent reads nothing past it.
const whatsAppCommandParamsMaxBytes = 4 * 1024

// whatsAppProposalIDPattern is shared-constants WHATSAPP_PROPOSAL_ID_PATTERN.
var whatsAppProposalIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)

// isWhatsAppCommand admits the five commands, and through it
// isInternalDemandCommand and isOperationalCommand admit them too. For the
// send that is a decision, not a side effect: every generic refusal it skips
// would answer on the results topic, which whatsapp-service never reads, and
// leave a confirmed message `sending` until the cloud's own 60 s timeout
// called it unknown ("check your phone") with nothing sent.
//
//   - Staleness: the handler applies the very same rule itself, once the
//     ledger has been read (isCommandStale, the same 60 s window), and
//     answers failed WHATSAPP_SEND_STALE.
//   - Rate limit: whatsapp-service limits sends per link when they are
//     confirmed.
//   - Drain: passed through, not refused. The send runs to its report inside
//     its Pub/Sub callback, so admitWork counts it as operational work until
//     the callback returns: an update waits for one send (at most
//     whatsAppSendTimeout) and its report. Once the drain is sealed for the
//     install, admitWork turns it away unacked and the callback nacks it, so
//     the replacement process answers the redelivery, which the ledger makes
//     safe: a recorded send is reported, never repeated, and an unrecorded one
//     that is now too old is refused stale.
func isWhatsAppCommand(command string) bool {
	switch command {
	case whatsAppLinkStartCommand, whatsAppLinkCancelCommand, whatsAppUnlinkCommand, whatsAppResyncStateCommand,
		whatsAppSendTextCommand:
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

// parseWhatsAppSendTextParams reads a send's parameters. A command it refuses
// is malformed: the handler logs that and answers nothing, since nothing in it
// can be trusted to address a report. A chatJid WhatsApp cannot address is
// not malformed: the send refuses it once the ledger has been read
// (whatsapp_send.go). The errors name the field, never its value.
func parseWhatsAppSendTextParams(cmd commandMsg) (whatsAppSendTextParams, error) {
	var p whatsAppSendTextParams
	if len(cmd.Args) != 1 {
		return p, errors.New("expected one JSON argument")
	}
	if len(cmd.Args[0]) > whatsAppCommandParamsMaxBytes {
		return p, errors.New("parameters exceed the command bound")
	}
	if err := json.Unmarshal([]byte(cmd.Args[0]), &p); err != nil {
		return p, errors.New("argument is not JSON")
	}
	switch {
	case !validWhatsAppLinkID(p.LinkID) || p.Generation <= 0:
		return p, errors.New("missing linkId / generation")
	case !whatsAppProposalIDPattern.MatchString(p.ProposalID):
		return p, errors.New("missing or malformed proposalId")
	case p.Attempt < 1:
		return p, errors.New("missing attempt")
	case p.ChatJid == "":
		return p, errors.New("missing chatJid")
	case p.Text == "":
		return p, errors.New("missing text")
	}
	return p, nil
}

// handleWhatsAppCommand dispatches a verified command. A link start runs to its
// durable accepted state (runner persisted, pairing begun) and a send to its
// recorded outcome and report before returning; the other commands run on the
// supervisor's goroutines. Either way it returns before the message is acked.
// ack is false only for a link start or a send that arrived while the
// supervisor is paused or shutting down: the caller nacks it so Pub/Sub
// redelivers it (after the resume, or to the next start) instead of dropping
// it.
func handleWhatsAppCommand(cmd commandMsg) (ack bool) {
	sup := globalWhatsAppSupervisor
	if cmd.Command == whatsAppSendTextCommand {
		p, err := parseWhatsAppSendTextParams(cmd)
		if err != nil {
			logWhatsApp("-", "command "+cmd.Command+" refused: "+err.Error())
			return true
		}
		// Synchronous, for the drain (isWhatsAppCommand) and for a crash: a
		// send cut short by one is redelivered, and its ledger row answers.
		return !sup.sendText(p, cmd.Ts)
	}
	p, err := parseWhatsAppCommandParams(cmd)
	if err != nil {
		logWhatsApp("-", "command "+cmd.Command+" refused: "+err.Error())
		return true
	}
	switch cmd.Command {
	case whatsAppLinkStartCommand:
		if p.Method != waPairQR && p.Method != waPairPhoneCode {
			logWhatsApp(p.LinkID, "link start refused: unknown method")
			return true
		}
		if p.Method == waPairPhoneCode && p.PhoneNumber == "" {
			logWhatsApp(p.LinkID, "link start refused: phone number missing")
			return true
		}
		if _, err := parseWhatsAppBrowserPub(p.BrowserPub); err != nil {
			logWhatsApp(p.LinkID, "link start refused: bad browser key")
			return true
		}
		logWhatsApp(p.LinkID, "link start ("+p.Method+")")
		// Synchronous: the Pub/Sub message is acked only once the runner
		// is persisted and pairing has begun, so an exit before that
		// leaves the command to be redelivered instead of lost.
		return !sup.linkStart(p)
	case whatsAppLinkCancelCommand:
		go sup.linkCancel(p.LinkID, p.Generation)
	case whatsAppUnlinkCommand:
		go sup.unlink(p.LinkID, p.Generation)
	case whatsAppResyncStateCommand:
		go sup.resyncState(p.LinkID, p.Generation)
	}
	return true
}
