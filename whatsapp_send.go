// whatsapp_send.go — one confirmed WhatsApp text, sent at most once.
//
// __whatsapp_send_text__ (whatsapp_commands.go) carries a message the person
// confirmed in AI Expedite: {linkId, generation, proposalId, attempt, chatJid,
// text}. whatsapp-service publishes it once per confirm or Try again, Pub/Sub
// may deliver it more than once, and this agent can stop at any point, so "at
// most once" is decided here, on the computer, by a durable ledger: the
// `sends` table in the link's outbox.db (whatsapp_outbox.go), ids and
// outcomes only.
//
//  1. Ledger first. A proposal with a row is never sent again: `sent` is
//     reported with the recorded id and time, `failed` with its recorded
//     code, and `attempting` or `unknown` (attempted without a definite
//     answer, or cut short) as unknown again.
//  2. Then the refusals, none of which writes a row, so none stands in the
//     way of a later Try again: a command older than the staleness window
//     (isCommandStale, the generic gate's own 60 s, applied here because that
//     gate lets every WhatsApp command through) or for an OLDER generation
//     than the link's is failed WHATSAPP_SEND_STALE; a client that is not
//     connected and logged in is failed WHATSAPP_SEND_NOT_CONNECTED; a chat
//     WhatsApp cannot address is failed WHATSAPP_SEND_RECIPIENT_UNAVAILABLE.
//     A command for a NEWER generation than this computer holds, or for a
//     link it does not hold, gets no answer at all (whatsapp_supervisor.go,
//     sendText): the device is behind, and a report under its generation
//     would fence it.
//  3. The claim, then the send: a row `attempting` under a message id chosen
//     BEFORE the send (whatsmeow's GenerateMessageID), committed under
//     synchronous(FULL); then SendText, bounded by whatsAppSendTimeout.
//     WhatsApp's acceptance is `sent` (the id and WhatsApp's own timestamp);
//     a definite refusal is `failed` with its code (classifyWhatsAppSendError);
//     a timeout or any ambiguous transport error is `unknown`: the message
//     may have gone out, so it is never sent again.
//  4. On `sent`, the message itself is queued like a live one (op new,
//     fromMe, source live, through the identity resolver): WhatsApp delivers
//     a companion's own send to none of its event handlers, so this is how it
//     reaches the thread by normal ingest. It is queued in the transaction
//     that records the outcome (RecordSendOutcome): a repeat reports the row
//     and queues nothing, so a `sent` committed alone, with the agent
//     stopping before the message was, would keep it out of the thread.
//  5. The result is reported under the generation of the runner that holds
//     the link then (whatsapp_supervisor.go, reportSendResult);
//     whatsapp-service applies it by proposal id and attempt. When every
//     retry of a row's report (step 1 or 3) fails with a transport error,
//     429 or 5xx, the command is left unacked: Pub/Sub redelivers it while
//     the subscription keeps it, and step 1 reports the row again, so a short
//     outage of the sync endpoint does not leave a message WhatsApp accepted
//     reading unknown. A step-2 refusal is acked all the same: it has no row,
//     so a redelivery would be decided afresh and might send.
//
// A runner that cannot read its ledger (its stores are closed: dormant, or
// held while they cannot open; or the read fails) cannot tell a proposal it
// already sent from one it never saw. It sends nothing and answers nothing,
// and whatsapp-service's own timeout calls the proposal unknown, as for any
// command that goes unanswered. The same holds when the claim cannot be
// written: nothing is sent without its row.
//
// Sends of one proposal are serialized (whatsAppSendLocks): a redelivery
// racing the attempt in flight waits for its outcome, then reads it from the
// ledger. The text travels only in the command and, once sent, in the queued
// message record (ingest content, like any message's); the ledger never holds
// it, and no log line carries it or the chat.
package main

import (
	"context"
	"strconv"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Wire vocabulary, pinned against shared-constants WHATSAPP_SEND_RESULT_OUTCOMES
// and WHATSAPP_ERROR_CODES by whatsapp_contract_test.go.
const (
	waSendOutcomeSent    = "sent"
	waSendOutcomeFailed  = "failed"
	waSendOutcomeUnknown = "unknown"

	waErrSendStale                = "WHATSAPP_SEND_STALE"
	waErrSendNotConnected         = "WHATSAPP_SEND_NOT_CONNECTED"
	waErrSendRecipientUnavailable = "WHATSAPP_SEND_RECIPIENT_UNAVAILABLE"
	waErrSendRefused              = "WHATSAPP_SEND_REFUSED"
)

// The ledger's states, persisted on this computer and pinned by
// whatsapp_send_test.go: a rename would misread every row an earlier build
// wrote. Only `sent` and `failed` are definite. Anything else (`attempting`,
// `unknown`, or a state a newer build wrote before a downgrade) reads as
// unknown, and is never sent again.
const (
	whatsAppSendAttempting = "attempting"
	whatsAppSendSent       = "sent"
	whatsAppSendFailed     = "failed"
	whatsAppSendUnknown    = "unknown"
)

// whatsAppSendTimeout bounds one SendText. A var so tests run it fast.
var whatsAppSendTimeout = 30 * time.Second

// whatsAppSendTextParams are __whatsapp_send_text__'s parameters, in wire
// order (shared-constants buildWhatsAppSendCommandParams).
type whatsAppSendTextParams struct {
	LinkID     string `json:"linkId"`
	Generation int64  `json:"generation"`
	ProposalID string `json:"proposalId"`
	Attempt    int64  `json:"attempt"`
	ChatJid    string `json:"chatJid"`
	Text       string `json:"text"`
}

// whatsAppSendResult is what one send command is answered with.
type whatsAppSendResult struct {
	Outcome     string
	WaMessageID string // sent only
	SentAt      int64  // sent only: WhatsApp's timestamp, Unix ms
	ErrorCode   string // failed only
	// Recorded: the proposal has a ledger row, so a redelivery of the command
	// reports that row and sends nothing (step 5). Never reported.
	Recorded bool
}

func whatsAppSendFailure(code string) whatsAppSendResult {
	return whatsAppSendResult{Outcome: waSendOutcomeFailed, ErrorCode: code}
}

// payload is the `sendResult` report. It carries ids and the outcome only,
// never the text or the chat.
func (res whatsAppSendResult) payload(p whatsAppSendTextParams, generation int64) map[string]any {
	m := map[string]any{
		"v": 1, "linkId": p.LinkID, "generation": generation, "kind": waSyncKindSendResult,
		"proposalId": p.ProposalID, "attempt": p.Attempt, "outcome": res.Outcome,
	}
	switch res.Outcome {
	case waSendOutcomeSent:
		m["waMessageId"] = res.WaMessageID
		m["sentAt"] = res.SentAt
	case waSendOutcomeFailed:
		m["errorCode"] = res.ErrorCode
	}
	return m
}

// result is how a ledger row is reported.
func (rec whatsAppSendRecord) result() whatsAppSendResult {
	res := whatsAppSendResult{Outcome: waSendOutcomeUnknown}
	switch rec.State {
	case whatsAppSendSent:
		res = whatsAppSendResult{Outcome: waSendOutcomeSent, WaMessageID: rec.WaMessageID, SentAt: rec.SentAt}
	case whatsAppSendFailed:
		code := rec.ErrorCode
		if code == "" {
			code = waErrSendRefused
		}
		res = whatsAppSendFailure(code)
	}
	res.Recorded = true
	return res
}

func whatsAppSendLogTag(p whatsAppSendTextParams) string {
	return "send proposal=" + p.ProposalID + " attempt=" + strconv.FormatInt(p.Attempt, 10)
}

// sendText runs steps 1-4 for one command on this runner and returns the
// result to report, or answer=false for none. The caller (the supervisor's
// sendText) holds the proposal's send lock. ts is the command's signed
// timestamp.
func (r *whatsAppLinkRunner) sendText(p whatsAppSendTextParams, ts int64) (res whatsAppSendResult, answer bool) {
	r.mu.Lock()
	ob, client, generation := r.outbox, r.client, r.generation
	r.mu.Unlock()
	tag := whatsAppSendLogTag(p)

	// 1. Ledger first.
	if ob == nil {
		r.log(tag + " not answered: the link's stores are closed, so its send ledger cannot be read")
		return res, false
	}
	rec, found, err := ob.GetSend(p.ProposalID)
	if err != nil {
		r.log(tag + " not answered: the send ledger could not be read: " + err.Error())
		return res, false
	}
	if found {
		res = rec.result()
		r.log(tag + " already acted on; reporting the recorded outcome (" + res.Outcome + ")")
		return res, true
	}

	// 2. Refusals that write no row.
	switch {
	case isCommandStale(ts):
		r.log(tag + " refused: the command is older than the staleness window")
		return whatsAppSendFailure(waErrSendStale), true
	case p.Generation < generation:
		r.log(tag + " refused: the command is for an older generation than this link's")
		return whatsAppSendFailure(waErrSendStale), true
	case client == nil || !client.IsConnected() || !client.IsLoggedIn():
		r.log(tag + " refused: WhatsApp is not connected")
		return whatsAppSendFailure(waErrSendNotConnected), true
	}
	chat, ok := parseWhatsAppSendChat(p.ChatJid)
	if !ok {
		r.log(tag + " refused: the chat is not one WhatsApp can address")
		return whatsAppSendFailure(waErrSendRecipientUnavailable), true
	}
	if r.sup.ctx.Err() != nil {
		r.log(tag + " not sent: the agent is shutting down")
		return res, false
	}

	// 3. The claim, then the send.
	id := string(client.GenerateMessageID())
	claimed, err := ob.PutSendAttempting(p.ProposalID, id)
	if err != nil {
		r.log(tag + " not sent: the send ledger could not record the attempt: " + err.Error())
		return res, false
	}
	if !claimed {
		// Another claim landed first (the send lock makes this one process's
		// impossible): it is the attempt, and its row the answer.
		if rec, found, err := ob.GetSend(p.ProposalID); err == nil && found {
			res = rec.result()
			r.log(tag + " already acted on; reporting the recorded outcome (" + res.Outcome + ")")
			return res, true
		}
		r.log(tag + " not sent: the send ledger holds an attempt it could not read back")
		return res, false
	}
	ctx, cancel := context.WithTimeout(r.sup.ctx, whatsAppSendTimeout)
	at, sendErr := client.SendText(ctx, chat, p.Text, types.MessageID(id))
	cancel()
	state := whatsAppSendSent
	var own []whatsAppRecord
	switch outcome, code := classifyWhatsAppSendError(sendErr); outcome {
	case waSendOutcomeSent:
		if at.IsZero() {
			at = time.Now() // WhatsApp's answer carried no time
		}
		res = whatsAppSendResult{Outcome: waSendOutcomeSent, WaMessageID: id, SentAt: at.UnixMilli()}
		r.log(tag + ": sent")
		// 4. The message itself, into the thread: queued by the same commit
		// that records the outcome.
		own = r.ownSendRecords(chat, id, at, p.Text)
	case waSendOutcomeFailed:
		state, res = whatsAppSendFailed, whatsAppSendFailure(code)
		r.log(tag + ": definitely not sent (" + code + "): " + sendErr.Error())
	default:
		state, res = whatsAppSendUnknown, whatsAppSendResult{Outcome: waSendOutcomeUnknown}
		r.log(tag + ": no definite answer from WhatsApp; never sent again: " + sendErr.Error())
	}
	if err := ob.RecordSendOutcome(p.ProposalID, state, res.SentAt, res.ErrorCode, own); err != nil {
		// Neither the outcome nor the message landed. The row stays
		// `attempting`, which a repeat reports as unknown and never sends
		// again.
		r.log(tag + ": outcome not recorded; a repeat will report unknown: " + err.Error())
	} else if len(own) > 0 {
		r.liveAppended(own)
	}
	res.Recorded = true // the claim's row, whichever outcome it holds
	return res, true
}

// ownSendRecords are the records of a text this computer just sent
// (normalizeWhatsAppOwnSend), exactly as a live message's are queued: led by
// the chat's record the first time this runner sees the chat live. Nothing
// without a linked account.
func (r *whatsAppLinkRunner) ownSendRecords(chat types.JID, id string, at time.Time, text string) []whatsAppRecord {
	records := normalizeWhatsAppOwnSend(chat, id, at, text, r.normalizeContext())
	return r.withConversationRecord(records, chat.Server == types.GroupServer, "")
}

// parseWhatsAppSendChat is the JID a send goes to: a person (a phone number
// or a LID, digits only, device part dropped) or a group. Status broadcasts,
// broadcast lists, newsletters and anything unparseable are no chat a
// confirmed text can go to.
func parseWhatsAppSendChat(s string) (types.JID, bool) {
	j, err := types.ParseJID(s)
	if err != nil {
		return types.EmptyJID, false
	}
	if j = j.ToNonAD(); j.Server == types.GroupServer {
		return j, j.User != ""
	}
	return whatsAppPersonForm(j)
}

// whatsAppSendLocks serializes the sends of one proposal, so a redelivery (or
// a Try again) racing the attempt in flight waits for its outcome and then
// finds it in the ledger. The zero value is ready to use.
type whatsAppSendLocks struct {
	mu   sync.Mutex
	held map[string]*whatsAppSendLock
}

type whatsAppSendLock struct {
	mu    sync.Mutex
	users int // holders and waiters; the entry goes when it reaches 0
}

// lock takes the proposal's lock and returns what releases it.
func (l *whatsAppSendLocks) lock(proposalID string) (unlock func()) {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]*whatsAppSendLock{}
	}
	e := l.held[proposalID]
	if e == nil {
		e = &whatsAppSendLock{}
		l.held[proposalID] = e
	}
	e.users++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		if e.users--; e.users == 0 {
			delete(l.held, proposalID)
		}
		l.mu.Unlock()
	}
}
