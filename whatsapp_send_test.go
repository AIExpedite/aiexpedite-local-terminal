package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The confirmed send (whatsapp_send.go). CI never sends a real WhatsApp
// message: every test here drives the fake client (whatsapp_supervisor_test.go).

const testProposalID = "PROPOSAL000000000001"

func testSend(proposalID string, attempt int64) whatsAppSendTextParams {
	return whatsAppSendTextParams{LinkID: "lnk_1", Generation: 2, ProposalID: proposalID, Attempt: attempt,
		ChatJid: waPeer.String(), Text: "see you at 6"}
}

func sendCommandMsg(p whatsAppSendTextParams, ts int64) commandMsg {
	arg, _ := json.Marshal(p)
	return commandMsg{Command: whatsAppSendTextCommand, Args: []string{string(arg)}, Ts: ts}
}

func nowTs() int64 { return time.Now().UnixMilli() }

// sendResults are the sendResult reports posted so far, in order.
func sendResults(h *supervisorHarness) []map[string]any {
	return h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindSendResult })
}

func onlySendResult(t *testing.T, h *supervisorHarness) map[string]any {
	t.Helper()
	res := sendResults(h)
	if len(res) != 1 {
		t.Fatalf("want one sendResult, got %d: %v", len(res), res)
	}
	return res[0]
}

func runnerOutbox(r *whatsAppLinkRunner) *whatsAppOutbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outbox
}

// ledgerRow reads lnk_1's ledger row for a proposal.
func ledgerRow(t *testing.T, h *supervisorHarness, proposalID string) (whatsAppSendRecord, bool) {
	t.Helper()
	r := h.sup.runner("lnk_1")
	if r == nil {
		t.Fatal("no runner")
	}
	rec, found, err := runnerOutbox(r).GetSend(proposalID)
	if err != nil {
		t.Fatal(err)
	}
	return rec, found
}

// uploadedMessage is the uploaded message record with waMessageId id, or nil.
func uploadedMessage(h *supervisorHarness, id string) map[string]any {
	for _, p := range h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }) {
		for _, rec := range p["records"].([]any) {
			if m := rec.(map[string]any); m["kind"] == waRecordMessage && m["waMessageId"] == id {
				return m
			}
		}
	}
	return nil
}

// rawSendsTable is every cell of a link's send ledger as stored on disk.
func rawSendsTable(t *testing.T, linkID string) string {
	t.Helper()
	dir, err := whatsAppLinkDir(linkID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, whatsAppOutboxDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT proposal_id, IFNULL(state, ''), IFNULL(wa_message_id, ''), IFNULL(sent_at, 0), IFNULL(error_code, ''), IFNULL(updated_at, 0) FROM sends`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var pid, state, id, code string
		var sentAt, updatedAt int64
		if err := rows.Scan(&pid, &state, &id, &sentAt, &code, &updatedAt); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "%s|%s|%s|%d|%s|%d\n", pid, state, id, sentAt, code, updatedAt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The ledger's states are persisted on this computer: a rename would misread
// every row an earlier build wrote.
func TestWhatsAppSendLedgerStatesArePinned(t *testing.T) {
	for _, p := range [][2]string{
		{whatsAppSendAttempting, "attempting"},
		{whatsAppSendSent, "sent"},
		{whatsAppSendFailed, "failed"},
		{whatsAppSendUnknown, "unknown"},
	} {
		if p[0] != p[1] {
			t.Errorf("ledger state drifted: %q != %q", p[0], p[1])
		}
	}
}

// A confirmed text goes out once, under an id the ledger recorded BEFORE the
// send. WhatsApp's acceptance is recorded with its time and reported under
// the link's generation, the ledger holds ids and the outcome only, and the
// message itself is queued into the thread like a live one from this account.
func TestWhatsAppSendSendsOnceRecordsReportsAndQueuesTheMessage(t *testing.T) {
	h, c := pairedHarness(t)
	r := h.sup.runner("lnk_1")
	var claim whatsAppSendRecord
	var claimed bool
	c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) {
		claim, claimed, _ = runnerOutbox(r).GetSend(testProposalID)
		return waTS, nil
	})
	if retry := h.sup.sendText(testSend(testProposalID, 1), nowTs()); retry {
		t.Fatal("a send was deferred")
	}
	sends := c.sent()
	if len(sends) != 1 || sends[0].To != waPeer || sends[0].Text != "see you at 6" || sends[0].ID == "" {
		t.Fatalf("sends = %+v", sends)
	}
	id := string(sends[0].ID)
	if !claimed || claim.State != whatsAppSendAttempting || claim.WaMessageID != id {
		t.Fatalf("the ledger did not hold the attempt and its id before the send: %+v (found %v)", claim, claimed)
	}
	rec, found := ledgerRow(t, h, testProposalID)
	if !found || rec.State != whatsAppSendSent || rec.WaMessageID != id || rec.SentAt != waTS.UnixMilli() || rec.ErrorCode != "" || rec.UpdatedAt == 0 {
		t.Fatalf("ledger = %+v (found %v)", rec, found)
	}
	got := onlySendResult(t, h)
	want := map[string]any{
		"v": 1.0, "linkId": "lnk_1", "generation": 2.0, "kind": waSyncKindSendResult, "proposalId": testProposalID,
		"attempt": 1.0, "outcome": waSendOutcomeSent, "waMessageId": id, "sentAt": float64(waTS.UnixMilli()),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sendResult = %v\nwant %v", got, want)
	}
	if table := rawSendsTable(t, "lnk_1"); strings.Contains(table, "see you") || strings.Contains(table, waPeer.User) {
		t.Fatalf("the ledger holds the text or the chat: %s", table)
	}

	waitFor(t, "the sent message uploaded", func() bool { return uploadedMessage(h, id) != nil })
	m := uploadedMessage(h, id)
	if m["op"] != waOpNew || m["fromMe"] != true || m["source"] != waSourceLive || m["type"] != waTypeText ||
		m["chatJid"] != waPeer.String() || m["senderJid"] != waMe.String() || m["text"] != "see you at 6" ||
		m["waTimestamp"] != float64(waTS.UnixMilli()) || m["catchUp"] != nil {
		t.Fatalf("the sent message's record = %v", m)
	}
}

// A repeat for a recorded proposal (a redelivery, a Try again, a replay after
// a restart) sends nothing and reports what the ledger recorded, even when it
// is too old to send: sent with its id and time, failed with its code.
func TestWhatsAppSendRepeatReportsTheRecordedOutcomeAndNeverResends(t *testing.T) {
	h, c := pairedHarness(t)
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	first := onlySendResult(t, h)
	h.sup.sendText(testSend(testProposalID, 2), nowTs())                                // a Try again
	h.sup.sendText(testSend(testProposalID, 2), time.Now().Add(-time.Hour).UnixMilli()) // a late redelivery
	if n := len(c.sent()); n != 1 {
		t.Fatalf("a recorded proposal went out %d times", n)
	}
	results := sendResults(h)
	if len(results) != 3 {
		t.Fatalf("results = %v", results)
	}
	for _, res := range results[1:] {
		if res["outcome"] != waSendOutcomeSent || res["waMessageId"] != first["waMessageId"] || res["sentAt"] != first["sentAt"] || res["attempt"] != 2.0 {
			t.Fatalf("a repeat reported %v, want the recorded send %v under attempt 2", res, first)
		}
	}

	const refused = "PROPOSAL000000000002"
	c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) {
		return time.Time{}, fmt.Errorf("%w 463", whatsmeow.ErrServerReturnedError)
	})
	h.sup.sendText(testSend(refused, 1), nowTs())
	c.answerSends(nil)
	h.sup.sendText(testSend(refused, 2), nowTs())
	if n := len(c.sent()); n != 2 {
		t.Fatalf("a refused proposal was sent again: %d sends in all", n)
	}
	results = sendResults(h)
	for _, res := range results[3:] {
		if res["proposalId"] != refused || res["outcome"] != waSendOutcomeFailed || res["errorCode"] != waErrSendRefused {
			t.Fatalf("refused proposal reported %v", res)
		}
	}
}

// A row left `attempting` (the agent stopped between the claim and the
// outcome), recorded `unknown`, or in a state this build does not know may
// stand for a message that went out: a repeat reports unknown again and never
// sends.
func TestWhatsAppSendAttemptedWithoutAnAnswerStaysUnknown(t *testing.T) {
	h, c := pairedHarness(t)
	ob := runnerOutbox(h.sup.runner("lnk_1"))
	for i, state := range []string{whatsAppSendAttempting, whatsAppSendUnknown, "settledByANewerBuild"} {
		pid := fmt.Sprintf("PROPOSAL00000000001%d", i)
		if put, err := ob.PutSendAttempting(pid, "3EB0EARLIER"); err != nil || !put {
			t.Fatalf("claim: %v %v", put, err)
		}
		if state != whatsAppSendAttempting {
			if err := ob.RecordSendOutcome(pid, state, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
		h.sup.sendText(testSend(pid, 2), nowTs())
		res := sendResults(h)
		if len(res) != i+1 {
			t.Fatalf("%s: results = %v", state, res)
		}
		if last := res[i]; last["proposalId"] != pid || last["outcome"] != waSendOutcomeUnknown || last["waMessageId"] != nil || last["errorCode"] != nil {
			t.Fatalf("%s: reported %v, want unknown", state, last)
		}
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("an attempted proposal was sent again: %+v", sends)
	}
}

// When the outcome cannot be recorded (here the stores close under the send,
// as a pause or a retire closes them), the report still says what WhatsApp
// answered, and the row stays `attempting`: a later repeat finds it and never
// sends again.
func TestWhatsAppSendOutcomeNotRecordedLeavesTheAttempt(t *testing.T) {
	h, c := pairedHarness(t)
	r := h.sup.runner("lnk_1")
	c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) {
		_ = r.closeStores()
		return waTS, nil
	})
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	if res := onlySendResult(t, h); res["outcome"] != waSendOutcomeSent || res["sentAt"] != float64(waTS.UnixMilli()) {
		t.Fatalf("reported %v, want what WhatsApp answered", res)
	}
	h.sup.sendText(testSend(testProposalID, 2), nowTs()) // the ledger cannot be read now: no answer, no send
	if n := len(c.sent()); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	if n := len(sendResults(h)); n != 1 {
		t.Fatalf("a send whose ledger could not be read was answered: %v", sendResults(h))
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	reopened, _, err := openWhatsAppOutbox(dir, "lnk_1")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if rec, found, err := reopened.GetSend(testProposalID); err != nil || !found || rec.State != whatsAppSendAttempting || rec.result().Outcome != waSendOutcomeUnknown {
		t.Fatalf("ledger = %+v found=%v %v, want the attempt left for a repeat to report unknown", rec, found, err)
	}
}

// The ledger outlives the agent process: after a restart (a new supervisor
// reopens outbox.db) a repeat still finds the send, reports it, and the new
// client sends nothing.
func TestWhatsAppSendLedgerSurvivesARestart(t *testing.T) {
	h, c := pairedHarness(t)
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	first := onlySendResult(t, h)
	if len(c.sent()) != 1 {
		t.Fatal("not sent")
	}
	h.sup.ShutdownAll(context.Background())

	h.mu.Lock()
	h.session = true
	h.mu.Unlock()
	next := &whatsAppSupervisor{}
	next.start(h.transport)
	defer next.ShutdownAll(context.Background())
	waitFor(t, "the link resumed and connected", func() bool {
		c2 := h.client(1)
		return c2 != nil && c2.IsConnected()
	})
	next.sendText(testSend(testProposalID, 2), nowTs())
	if sent := h.client(1).sent(); len(sent) != 0 {
		t.Fatalf("sent again after a restart: %+v", sent)
	}
	res := sendResults(h)
	if last := res[len(res)-1]; last["outcome"] != waSendOutcomeSent || last["attempt"] != 2.0 ||
		last["waMessageId"] != first["waMessageId"] || last["sentAt"] != first["sentAt"] {
		t.Fatalf("after a restart reported %v, want the recorded send %v", last, first)
	}
}

// The ledger is never sealed: a lost outbox key discards the sealed rows and
// must not take the record of what was sent with them. An outcome, once
// recorded, is final, and a second claim changes nothing.
func TestWhatsAppSendLedgerIsUnsealedAndOutlivesALostKey(t *testing.T) {
	ks, _ := withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_ledger")
	if err := ob.Append([]whatsAppRecord{textRecord(waPeer.String(), "Q1", "queued")}, false); err != nil {
		t.Fatal(err)
	}
	if put, err := ob.PutSendAttempting(testProposalID, "3EB0KEPT"); err != nil || !put {
		t.Fatalf("claim: %v %v", put, err)
	}
	if put, err := ob.PutSendAttempting(testProposalID, "3EB0OTHER"); err != nil || put {
		t.Fatalf("a second claim was put: %v %v", put, err)
	}
	if err := ob.RecordSendOutcome(testProposalID, whatsAppSendSent, waTS.UnixMilli(), ""); err != nil {
		t.Fatal(err)
	}
	if err := ob.RecordSendOutcome(testProposalID, whatsAppSendFailed, 0, waErrSendRefused); !errors.Is(err, errWhatsAppSendNotAttempting) {
		t.Fatalf("a recorded outcome was overwritten: %v", err)
	}
	if err := ob.RecordSendOutcome("PROPOSAL000000000099", whatsAppSendSent, 1, ""); !errors.Is(err, errWhatsAppSendNotAttempting) {
		t.Fatalf("an outcome without a claim was recorded: %v", err)
	}
	ob.Close()

	_ = ks.Delete("lnk_ledger")
	ob2, discarded, err := openWhatsAppOutbox(dir, "lnk_ledger")
	if err != nil {
		t.Fatal(err)
	}
	defer ob2.Close()
	if !discarded || ob2.Depth() != 0 {
		t.Fatalf("the sealed rows survived their key: discarded=%v depth=%d", discarded, ob2.Depth())
	}
	rec, found, err := ob2.GetSend(testProposalID)
	if err != nil || !found || rec.State != whatsAppSendSent || rec.WaMessageID != "3EB0KEPT" || rec.SentAt != waTS.UnixMilli() {
		t.Fatalf("the ledger went with the key: %+v found=%v %v", rec, found, err)
	}
	if _, found, _ := ob2.GetSend("PROPOSAL000000000099"); found {
		t.Fatal("a refused outcome wrote a row")
	}
}

// Too old (or too far ahead of this clock), or for an older generation:
// failed WHATSAPP_SEND_STALE, reported under the link's own generation, with
// nothing sent and NO ledger row, so a Try again that arrives in time sends.
func TestWhatsAppSendStaleCommandIsRefusedWithoutARow(t *testing.T) {
	h, c := pairedHarness(t)
	older := testSend(testProposalID, 1)
	older.Generation = 1
	for i, cmd := range []struct {
		name string
		p    whatsAppSendTextParams
		ts   int64
	}{
		{"older than the staleness window", testSend(testProposalID, 1), time.Now().Add(-61 * time.Second).UnixMilli()},
		{"dated past the allowed clock skew", testSend(testProposalID, 1), time.Now().Add(2 * time.Minute).UnixMilli()},
		{"for an older generation", older, nowTs()},
	} {
		h.sup.sendText(cmd.p, cmd.ts)
		res := sendResults(h)
		if len(res) != i+1 {
			t.Fatalf("%s: results = %v", cmd.name, res)
		}
		if last := res[i]; last["outcome"] != waSendOutcomeFailed || last["errorCode"] != waErrSendStale || last["generation"] != 2.0 || last["attempt"] != 1.0 {
			t.Fatalf("%s: reported %v, want failed %s under generation 2", cmd.name, last, waErrSendStale)
		}
		if _, found := ledgerRow(t, h, testProposalID); found {
			t.Fatalf("%s: a refusal wrote a ledger row", cmd.name)
		}
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("a stale command was sent: %+v", sends)
	}
	h.sup.sendText(testSend(testProposalID, 2), nowTs())
	if n := len(c.sent()); n != 1 {
		t.Fatal("a refusal stood in the way of a later Try again")
	}
	if last := sendResults(h)[3]; last["outcome"] != waSendOutcomeSent || last["attempt"] != 2.0 {
		t.Fatalf("the Try again reported %v", last)
	}
}

// A command for a newer generation than this computer holds (the link start
// has not reached it yet), or for a link it does not hold, gets no answer at
// all: a report under the older generation would fence the device. Nothing
// is sent, nothing recorded, and the command is not redelivered.
func TestWhatsAppSendForANewerGenerationOrAnotherLinkIsNotAnswered(t *testing.T) {
	h, c := pairedHarness(t)
	newer := testSend(testProposalID, 1)
	newer.Generation = 3
	other := testSend(testProposalID, 1)
	other.LinkID = "lnk_other"
	for _, p := range []whatsAppSendTextParams{newer, other} {
		if retry := h.sup.sendText(p, nowTs()); retry {
			t.Fatalf("%+v: asked for a redelivery", p)
		}
	}
	if res := sendResults(h); len(res) != 0 {
		t.Fatalf("answered: %v", res)
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("sent: %+v", sends)
	}
	if _, found := ledgerRow(t, h, testProposalID); found {
		t.Fatal("a ledger row was written")
	}
}

// With WhatsApp not connected: failed WHATSAPP_SEND_NOT_CONNECTED and no row,
// so a Try again once it is connected goes out.
func TestWhatsAppSendWhileNotConnectedIsRefusedWithoutARow(t *testing.T) {
	h, c := pairedHarness(t)
	c.Disconnect()
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	if res := onlySendResult(t, h); res["outcome"] != waSendOutcomeFailed || res["errorCode"] != waErrSendNotConnected {
		t.Fatalf("reported %v", res)
	}
	if _, found := ledgerRow(t, h, testProposalID); found {
		t.Fatal("a refusal wrote a ledger row")
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("sent while not connected: %+v", sends)
	}
	_ = c.Connect()
	h.sup.sendText(testSend(testProposalID, 2), nowTs())
	if n := len(c.sent()); n != 1 {
		t.Fatal("the Try again did not go out once connected")
	}
}

// No definite answer (WhatsApp's acknowledgement never came within the bound,
// the connection dropped) is unknown: the message may have gone out, so it is
// recorded as such, never sent again, and not queued into the thread.
func TestWhatsAppSendWithoutADefiniteAnswerIsUnknown(t *testing.T) {
	h, c := pairedHarness(t)
	prev := whatsAppSendTimeout
	whatsAppSendTimeout = 50 * time.Millisecond
	t.Cleanup(func() { whatsAppSendTimeout = prev })
	c.answerSends(func(ctx context.Context, _ types.JID, _ string, _ types.MessageID) (time.Time, error) {
		<-ctx.Done() // WhatsApp never acknowledges
		return time.Time{}, ctx.Err()
	})
	start := time.Now()
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the send was not bounded: %v", took)
	}
	if res := onlySendResult(t, h); res["outcome"] != waSendOutcomeUnknown || res["errorCode"] != nil || res["waMessageId"] != nil {
		t.Fatalf("reported %v, want unknown", res)
	}
	if rec, found := ledgerRow(t, h, testProposalID); !found || rec.State != whatsAppSendUnknown {
		t.Fatalf("ledger = %+v (found %v)", rec, found)
	}

	c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) {
		return time.Time{}, &whatsmeow.DisconnectedError{Action: "message send"}
	})
	const dropped = "PROPOSAL000000000002"
	h.sup.sendText(testSend(dropped, 1), nowTs())
	c.answerSends(nil)
	h.sup.sendText(testSend(testProposalID, 2), nowTs()) // Try again: unknown again
	h.sup.sendText(testSend(dropped, 2), nowTs())
	sends := c.sent()
	if len(sends) != 2 {
		t.Fatalf("an unknown send went out again: %+v", sends)
	}
	for _, res := range sendResults(h) {
		if res["outcome"] != waSendOutcomeUnknown {
			t.Fatalf("reported %v, want unknown", res)
		}
	}
	waitFor(t, "the outbox drained", func() bool { return runnerOutbox(h.sup.runner("lnk_1")).Depth() == 0 })
	for _, s := range sends {
		if uploadedMessage(h, string(s.ID)) != nil {
			t.Fatal("a send without WhatsApp's acceptance was queued into the thread")
		}
	}
}

// WhatsApp's own answer is definite: the message refused with an error code,
// a recipient it will not take (not a member, no such group), a coded answer
// to the look-ups before the send. Failed with its code, recorded, never sent
// again, and not queued into the thread.
func TestWhatsAppSendDefiniteRefusalIsFailed(t *testing.T) {
	h, c := pairedHarness(t)
	group := testSend(testProposalID, 1)
	group.ChatJid = waGroup.String()
	for i, cse := range []struct {
		p    whatsAppSendTextParams
		err  error
		code string
	}{
		{testSend("PROPOSAL0000000000R1", 1), fmt.Errorf("%w 463", whatsmeow.ErrServerReturnedError), waErrSendRefused},
		{group, fmt.Errorf("failed to get group members: %w", whatsmeow.ErrNotInGroup), waErrSendRecipientUnavailable},
		{testSend("PROPOSAL0000000000R3", 1), fmt.Errorf("failed to get user info to fill LID cache: %w", &whatsmeow.IQError{Code: 404, Text: "item-not-found"}), waErrSendRecipientUnavailable},
	} {
		err := cse.err
		c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) { return time.Time{}, err })
		h.sup.sendText(cse.p, nowTs())
		res := sendResults(h)
		if len(res) != i+1 {
			t.Fatalf("results = %v", res)
		}
		if last := res[i]; last["outcome"] != waSendOutcomeFailed || last["errorCode"] != cse.code || last["waMessageId"] != nil {
			t.Fatalf("%v: reported %v, want failed %s", cse.err, last, cse.code)
		}
		if rec, found := ledgerRow(t, h, cse.p.ProposalID); !found || rec.State != whatsAppSendFailed || rec.ErrorCode != cse.code {
			t.Fatalf("%v: ledger = %+v (found %v)", cse.err, rec, found)
		}
	}
	if sends := c.sent(); len(sends) != 3 || sends[1].To != waGroup {
		t.Fatalf("sends = %+v", sends)
	}
	c.answerSends(nil)
	h.sup.sendText(testSend("PROPOSAL0000000000R1", 2), nowTs())
	if n := len(c.sent()); n != 3 {
		t.Fatal("a refused proposal was sent again")
	}
}

// A chat WhatsApp cannot address is failed WHATSAPP_SEND_RECIPIENT_UNAVAILABLE
// before anything is claimed or sent.
func TestWhatsAppSendToAChatWhatsAppCannotAddressIsRefusedWithoutARow(t *testing.T) {
	h, c := pairedHarness(t)
	for i, chat := range []string{"not a jid", "status@broadcast", "120363000000000099@newsletter", "15550000002@broadcast", "abc@s.whatsapp.net", "@g.us"} {
		p := testSend(testProposalID, int64(i+1))
		p.ChatJid = chat
		h.sup.sendText(p, nowTs())
		res := sendResults(h)
		if len(res) != i+1 {
			t.Fatalf("%q: results = %v", chat, res)
		}
		if last := res[i]; last["outcome"] != waSendOutcomeFailed || last["errorCode"] != waErrSendRecipientUnavailable {
			t.Fatalf("%q: reported %v", chat, last)
		}
	}
	if _, found := ledgerRow(t, h, testProposalID); found {
		t.Fatal("a refusal wrote a ledger row")
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("sent: %+v", sends)
	}
}

func TestParseWhatsAppSendChat(t *testing.T) {
	for in, want := range map[string]string{
		"15550000002@s.whatsapp.net":   "15550000002@s.whatsapp.net",
		"15550000002:3@s.whatsapp.net": "15550000002@s.whatsapp.net", // device part dropped
		"15550000002@c.us":             "15550000002@s.whatsapp.net",
		"999888777@lid":                "999888777@lid",
		"120363000000000001@g.us":      "120363000000000001@g.us",
		"15557654321-1600000000@g.us":  "15557654321-1600000000@g.us", // a legacy group
	} {
		if got, ok := parseWhatsAppSendChat(in); !ok || got.String() != want {
			t.Errorf("%q: %v %v, want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "x", "status@broadcast", "1@newsletter", "abc@lid", "@s.whatsapp.net", "@g.us", "1@bot"} {
		if got, ok := parseWhatsAppSendChat(bad); ok {
			t.Errorf("%q accepted as %v", bad, got)
		}
	}
}

// Only an answer WhatsApp gave is a definite refusal; a timeout or a dropped
// connection on either side of the message write reads the same, so it is
// unknown.
func TestClassifyWhatsAppSendError(t *testing.T) {
	for _, c := range []struct {
		name          string
		err           error
		outcome, code string
	}{
		{"accepted", nil, waSendOutcomeSent, ""},
		{"an error code on the message", fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), waSendOutcomeFailed, waErrSendRefused},
		{"not a member of the group", fmt.Errorf("failed to get group members: %w", whatsmeow.ErrNotInGroup), waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"no such group", fmt.Errorf("failed to get group members: %w", whatsmeow.ErrGroupNotFound), waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"a device address", whatsmeow.ErrRecipientADJID, waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"an unknown server", fmt.Errorf("%w %s", whatsmeow.ErrUnknownServer, "example.net"), waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"no paired identity", whatsmeow.ErrNotLoggedIn, waSendOutcomeFailed, waErrSendNotConnected},
		{"info query 403", fmt.Errorf("failed to get device list: %w", whatsmeow.ErrIQForbidden), waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"info query 404", fmt.Errorf("failed to get user info: %w", whatsmeow.ErrIQNotFound), waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"info query 410", whatsmeow.ErrIQGone, waSendOutcomeFailed, waErrSendRecipientUnavailable},
		{"info query 429", whatsmeow.ErrIQRateOverLimit, waSendOutcomeFailed, waErrSendRefused},
		{"info query 500", &whatsmeow.IQError{Code: 500, Text: "internal-server-error"}, waSendOutcomeFailed, waErrSendRefused},
		{"an info query answer without a code", &whatsmeow.IQError{}, waSendOutcomeUnknown, ""},
		{"the acknowledgement timed out", whatsmeow.ErrMessageTimedOut, waSendOutcomeUnknown, ""},
		{"the send bound passed", context.DeadlineExceeded, waSendOutcomeUnknown, ""},
		{"cancelled", context.Canceled, waSendOutcomeUnknown, ""},
		{"disconnected awaiting the acknowledgement", &whatsmeow.DisconnectedError{Action: "message send"}, waSendOutcomeUnknown, ""},
		{"not connected (may follow a write, on the retry)", whatsmeow.ErrNotConnected, waSendOutcomeUnknown, ""},
		{"a socket error", fmt.Errorf("failed to send message node: %w", errors.New("write: broken pipe")), waSendOutcomeUnknown, ""},
		{"an info query timed out", whatsmeow.ErrIQTimedOut, waSendOutcomeUnknown, ""},
		{"anything else", errors.New("surprise"), waSendOutcomeUnknown, ""},
	} {
		if outcome, code := classifyWhatsAppSendError(c.err); outcome != c.outcome || code != c.code {
			t.Errorf("%s: (%s, %s), want (%s, %s)", c.name, outcome, code, c.outcome, c.code)
		}
	}
}

// Two deliveries of one proposal at once: the second waits for the first's
// outcome and reports it from the ledger, and one message goes out. Another
// proposal is not held up meanwhile.
func TestWhatsAppSendSerializesAProposalsDeliveries(t *testing.T) {
	h, c := pairedHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	c.answerSends(func(_ context.Context, _ types.JID, text string, _ types.MessageID) (time.Time, error) {
		if text == "first" {
			entered <- struct{}{}
			<-release
		}
		return waTS, nil
	})
	var wg sync.WaitGroup
	deliver := func(attempt int64) {
		defer wg.Done()
		p := testSend(testProposalID, attempt)
		p.Text = "first"
		h.sup.sendText(p, nowTs())
	}
	wg.Add(1)
	go deliver(1)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first delivery never reached WhatsApp")
	}
	wg.Add(1)
	go deliver(2)
	other := testSend("PROPOSAL0000000000B1", 1)
	other.Text = "other"
	h.sup.sendText(other, nowTs())
	select {
	case <-entered:
		t.Fatal("a second delivery of the proposal reached WhatsApp while the first was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	sends := c.sent()
	if len(sends) != 2 {
		t.Fatalf("sends = %+v", sends)
	}
	byAttempt := map[float64]map[string]any{}
	for _, res := range sendResults(h) {
		if res["proposalId"] == testProposalID {
			byAttempt[res["attempt"].(float64)] = res
		}
	}
	one, two := byAttempt[1], byAttempt[2]
	if one == nil || two == nil || one["outcome"] != waSendOutcomeSent || two["outcome"] != waSendOutcomeSent || one["waMessageId"] != two["waMessageId"] {
		t.Fatalf("results = %v", sendResults(h))
	}
}

// The Pub/Sub callback runs a send to its report before it returns, so the
// message is acked only then (and a drain counts the send meanwhile). A
// malformed send is acked with no answer.
func TestWhatsAppSendCommandRunsToItsReportBeforeItIsAcked(t *testing.T) {
	h, c := pairedHarness(t)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	t.Cleanup(func() { globalWhatsAppSupervisor = prev })
	if !handleWhatsAppCommand(sendCommandMsg(testSend(testProposalID, 1), nowTs())) {
		t.Fatal("a handled send was not acked")
	}
	if res := onlySendResult(t, h); res["outcome"] != waSendOutcomeSent {
		t.Fatalf("reported %v", res)
	}
	malformed := testSend("PROPOSAL000000000002", 0) // attempt 0
	if !handleWhatsAppCommand(sendCommandMsg(malformed, nowTs())) {
		t.Fatal("a malformed send must be acked")
	}
	if n := len(sendResults(h)); n != 1 || len(c.sent()) != 1 {
		t.Fatalf("a malformed send was answered or sent: %d results, %d sends", n, len(c.sent()))
	}
}

// A send that lands while the agent is disconnected from the cloud is not
// acked: nothing may be posted then, so Pub/Sub redelivers it after the
// reconnect, when it is sent.
func TestWhatsAppSendWhilePausedIsNacked(t *testing.T) {
	h, c := pairedHarness(t)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	t.Cleanup(func() { globalWhatsAppSupervisor = prev })
	h.sup.requestCloudPause()
	cmd := sendCommandMsg(testSend(testProposalID, 1), nowTs())
	if handleWhatsAppCommand(cmd) {
		t.Fatal("a send was acked while disconnected from the cloud")
	}
	if len(c.sent()) != 0 || len(sendResults(h)) != 0 {
		t.Fatal("a send went out, or was answered, while disconnected from the cloud")
	}
	h.sup.setCloudConnected(true)
	if !handleWhatsAppCommand(cmd) {
		t.Fatal("the redelivered send was not acked after the reconnect")
	}
	if len(c.sent()) != 1 {
		t.Fatal("the redelivered send did not go out")
	}
}

// A dormant link (its session ended and its stores closed) cannot read its
// send ledger, so it cannot tell a proposal it sent from one it never saw: it
// sends nothing and answers nothing, and whatsapp-service's own timeout calls
// the proposal unknown.
func TestWhatsAppSendOnADormantLinkIsNotAnswered(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })
	h.sup.sendText(testSend(testProposalID, 1), nowTs())
	if res := sendResults(h); len(res) != 0 {
		t.Fatalf("a dormant link answered: %v", res)
	}
	if sends := c.sent(); len(sends) != 0 {
		t.Fatalf("a dormant link sent: %+v", sends)
	}
}

// No log line carries a send's text or its chat, on any path (sent, repeated,
// refused stale, unanswered, refused by WhatsApp, unknown, not connected,
// malformed), and no sendResult report carries either.
func TestWhatsAppSendLogsAndReportsNeverCarryTheText(t *testing.T) {
	var mu sync.Mutex
	var captured strings.Builder
	prevSink := whatsAppLogSink
	whatsAppLogSink = func(line string) {
		mu.Lock()
		captured.WriteString(line)
		captured.WriteByte('\n')
		mu.Unlock()
	}
	t.Cleanup(func() { whatsAppLogSink = prevSink })

	const secret = "TOP SECRET SEND BODY 4815162342"
	chat := types.NewJID("15559876543", types.DefaultUserServer)
	h, c := pairedHarness(t)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	t.Cleanup(func() { globalWhatsAppSupervisor = prev })
	send := func(proposalID string, attempt, generation, ts int64) {
		p := whatsAppSendTextParams{LinkID: "lnk_1", Generation: generation, ProposalID: proposalID, Attempt: attempt, ChatJid: chat.String(), Text: secret}
		if !handleWhatsAppCommand(sendCommandMsg(p, ts)) {
			t.Fatal("a send was not acked")
		}
	}
	refuse := func(err error) {
		c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) { return time.Time{}, err })
	}
	send("PROPOSAL0000000000A1", 1, 2, nowTs())                                // sent
	send("PROPOSAL0000000000A1", 2, 2, nowTs())                                // repeated
	send("PROPOSAL0000000000A2", 1, 2, time.Now().Add(-time.Hour).UnixMilli()) // stale
	send("PROPOSAL0000000000A3", 1, 1, nowTs())                                // an older generation
	send("PROPOSAL0000000000A4", 1, 3, nowTs())                                // a newer generation: unanswered
	refuse(fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError))
	send("PROPOSAL0000000000A5", 1, 2, nowTs()) // refused by WhatsApp
	refuse(whatsmeow.ErrMessageTimedOut)
	send("PROPOSAL0000000000A6", 1, 2, nowTs()) // unknown
	// whatsmeow's own errors quote the recipient: as a JID (send.go, filling
	// the LID cache) or as a device's Signal address, its number without a
	// server (send.go, encrypting with neither a session nor, the bound having
	// passed while its keys were fetched, a prekey bundle).
	refuse(fmt.Errorf("failed to get user info for %s to fill LID cache: %w", chat, whatsmeow.ErrIQNotFound))
	send("PROPOSAL0000000000A8", 1, 2, nowTs()) // refused: no such user
	device := types.JID{User: chat.User, Device: 3, Server: chat.Server}
	refuse(fmt.Errorf("%w with %s", whatsmeow.ErrNoSession, device.SignalAddress().String()))
	send("PROPOSAL0000000000A9", 1, 2, nowTs()) // unknown: cut short while encrypting
	c.Disconnect()
	send("PROPOSAL0000000000A7", 1, 2, nowTs()) // not connected
	malformed := whatsAppSendTextParams{LinkID: "lnk_1", Generation: 2, ProposalID: "short", Attempt: 1, ChatJid: chat.String(), Text: secret}
	handleWhatsAppCommand(sendCommandMsg(malformed, nowTs()))

	mu.Lock()
	out := captured.String()
	mu.Unlock()
	for _, leak := range []string{secret, "4815162342", chat.User} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{"send proposal=PROPOSAL0000000000A1 attempt=1: sent", "refused: the command is older", "not answered", "definitely not sent (WHATSAPP_SEND_REFUSED)", "no definite answer", "not connected", "refused: missing or malformed proposalId"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected a log line with %q, got:\n%s", want, out)
		}
	}
	results := sendResults(h)
	if len(results) != 9 {
		t.Fatalf("results = %v", results)
	}
	for _, res := range results {
		raw, _ := json.Marshal(res)
		if strings.Contains(string(raw), "4815162342") || strings.Contains(string(raw), chat.User) {
			t.Fatalf("a sendResult carried the text or the chat: %s", raw)
		}
	}
}

// whatsmeow logs a device it cannot encrypt a send for, quoting the device's
// JID and its Signal address: the number with no server ("15551234567:3", a
// LID's "123456789012345_1:3"). A business recipient's devices include hosted
// (Cloud API) ones, "…@hosted" and "…@hosted.lid". No form reaches the log.
func TestWhatsAppLogsMaskSignalAddressesAndHostedDevices(t *testing.T) {
	var mu sync.Mutex
	var captured strings.Builder
	prevSink := whatsAppLogSink
	whatsAppLogSink = func(line string) {
		mu.Lock()
		captured.WriteString(line)
		captured.WriteByte('\n')
		mu.Unlock()
	}
	t.Cleanup(func() { whatsAppLogSink = prevSink })

	devices := []types.JID{
		{User: "15559876543", Device: 3, Server: types.DefaultUserServer},
		{User: "123456789012345", Device: 2, Server: types.HiddenUserServer},
		{User: "15551112222", Device: 99, Server: types.HostedServer},
		{User: "543210987654321", Device: 99, Server: types.HostedLIDServer},
	}
	wm := newWhatsAppLogger("client")
	for _, dev := range devices {
		noSession := fmt.Errorf("%w with %s", whatsmeow.ErrNoSession, dev.SignalAddress().String())
		wm.Warnf("Failed to encrypt %s for %s: %v", "3EB0ABCDEF", dev, noSession) // send.go, per device
		logWhatsApp("lnk_1", "send proposal=PROPOSAL000000000001 attempt=1: no definite answer from WhatsApp; never sent again: "+noSession.Error())
	}

	mu.Lock()
	out := captured.String()
	mu.Unlock()
	for _, dev := range devices {
		if strings.Contains(out, dev.User) {
			t.Fatalf("log leaked %q:\n%s", dev.User, out)
		}
	}
	if n := strings.Count(out, "Failed to encrypt 3EB0ABCDEF for [jid]: "+whatsmeow.ErrNoSession.Error()+" with [jid]"); n != len(devices) {
		t.Fatalf("want every whatsmeow line kept with its devices masked, got %d:\n%s", n, out)
	}
}

// A pause that begins while a send is in flight (the tray's "Disconnect from
// cloud" pauses at once and stops the links after) keeps its result from the
// cloud: nothing is posted while paused. The command is then not acked.
// Redelivered after the reconnect it is answered from the ledger, so the
// cloud learns the message went out instead of calling it unknown, and
// nothing goes out twice.
func TestWhatsAppSendPausedMidSendIsNackedAndAnsweredFromTheLedger(t *testing.T) {
	h, c := pairedHarness(t)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	t.Cleanup(func() { globalWhatsAppSupervisor = prev })
	c.answerSends(func(context.Context, types.JID, string, types.MessageID) (time.Time, error) {
		h.sup.requestCloudPause() // clicked while WhatsApp answered
		return waTS, nil
	})
	cmd := sendCommandMsg(testSend(testProposalID, 1), nowTs())
	if handleWhatsAppCommand(cmd) {
		t.Fatal("a send whose result the pause kept from the cloud was acked")
	}
	if res := sendResults(h); len(res) != 0 {
		t.Fatalf("posted while paused: %v", res)
	}
	rec, found := ledgerRow(t, h, testProposalID)
	if !found || rec.State != whatsAppSendSent {
		t.Fatalf("ledger = %+v (found %v)", rec, found)
	}

	c.answerSends(nil)
	h.sup.setCloudConnected(true)
	if !handleWhatsAppCommand(cmd) {
		t.Fatal("the redelivery after the reconnect was not acked")
	}
	if n := len(c.sent()); n != 1 {
		t.Fatalf("sent %d times", n)
	}
	if res := onlySendResult(t, h); res["outcome"] != waSendOutcomeSent || res["waMessageId"] != rec.WaMessageID ||
		res["sentAt"] != float64(waTS.UnixMilli()) || res["attempt"] != 1.0 {
		t.Fatalf("reported %v, want the recorded send", res)
	}
}
