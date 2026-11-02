package main

import (
	"strconv"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Former identities (whatsapp_identity.go, Formers): a person written under
// their LID and, separately, under their number before anything joined the
// two keeps the LID side's messages there. Edits, revokes and reactions aimed
// at those messages must still reach them after the join.

var (
	bobLID = types.NewJID("424242424242", types.HiddenUserServer)
	bobPN  = types.NewJID("15550000042", types.DefaultUserServer)
	dave   = types.NewJID("15550000077", types.DefaultUserServer)
)

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

// splitInGroup writes Bob twice in the group before anything joins his two
// forms, the way a group's history does: once under his LID, once under his
// number.
func splitInGroup(t *testing.T, nc whatsAppNormalizeContext) {
	t.Helper()
	g1 := one(t, normalizeWhatsAppMessage(liveMsg(waGroup, bobLID, false, "G1", textMsg("secret")), waSourceHistory, nc))
	g2 := one(t, normalizeWhatsAppMessage(liveMsg(waGroup, bobPN, false, "G2", textMsg("hi")), waSourceHistory, nc))
	if g1.SenderJid != bobLID.String() || g2.SenderJid != bobPN.String() {
		t.Fatalf("not split: G1 under %s, G2 under %s", g1.SenderJid, g2.SenderJid)
	}
}

// wantTargets checks recs are op on id, one per (chat, author), in order.
func wantTargets(t *testing.T, recs []whatsAppRecord, op, id string, chatAuthor ...[2]string) {
	t.Helper()
	if len(recs) != len(chatAuthor) {
		t.Fatalf("got %d records, want %d: %+v", len(recs), len(chatAuthor), recs)
	}
	for i, ca := range chatAuthor {
		r := recs[i]
		if r.Op != op || r.WaMessageID != id || r.ChatJid != ca[0] || r.SenderJid != ca[1] {
			t.Fatalf("record %d is %s %s in (%s, %s), want %s %s in (%s, %s)",
				i, r.Op, r.WaMessageID, r.ChatJid, r.SenderJid, op, id, ca[0], ca[1])
		}
	}
}

func TestWhatsAppIdentityKeepsTheFormerIdentityOfARepointedForm(t *testing.T) {
	ir := newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), nil)
	ir.Canonical(bobLID, types.EmptyJID)
	ir.Canonical(bobPN, types.EmptyJID)
	if f := ir.Formers(bobLID, types.EmptyJID); len(f) != 0 {
		t.Fatalf("formers before any join: %v", f)
	}
	if got := ir.Canonical(bobLID, bobPN); got != bobPN {
		t.Fatalf("joined forms resolve to %s", got)
	}
	for _, form := range []types.JID{bobLID, bobPN} {
		if f := ir.Formers(form, types.EmptyJID); len(f) != 1 || f[0] != bobLID {
			t.Fatalf("Formers(%s) = %v, want [%s]", form, f, bobLID)
		}
	}
	ir.Learn(bobPN, bobLID)
	if f := ir.Formers(bobLID, types.EmptyJID); len(f) != 1 {
		t.Fatalf("a repeated join added formers: %v", f)
	}
	ir.Canonical(dave, types.EmptyJID)
	if f := ir.Formers(dave, types.EmptyJID); f != nil {
		t.Fatalf("a person never re-pointed has formers: %v", f)
	}
	if f := ir.Formers(waGroup, types.EmptyJID); f != nil {
		t.Fatalf("a group has formers: %v", f)
	}

	// A history chunk's mapping is a join too (Learn).
	carolLID := types.NewJID("777777777777", types.HiddenUserServer)
	carolPN := types.NewJID("15550000088", types.DefaultUserServer)
	ir.Canonical(carolLID, types.EmptyJID)
	ir.Canonical(carolPN, types.EmptyJID)
	ir.Learn(carolPN, carolLID)
	if got := ir.Canonical(carolLID, types.EmptyJID); got != carolPN {
		t.Fatalf("Learn did not join Carol: %s", got)
	}
	if f := ir.Formers(carolLID, types.EmptyJID); len(f) != 1 || f[0] != carolLID {
		t.Fatalf("Learn's re-point kept no former: %v", f)
	}
}

// A LID re-pointed between numbers (a number change, a stale chunk mapping)
// keeps its newest formers, never the identity it has now, at most
// whatsAppMaxFormers of them.
func TestWhatsAppIdentityFormersAreBoundedAndNeverCurrent(t *testing.T) {
	store := newMemoryWhatsAppIdentityStore()
	ir := newWhatsAppIdentityResolver(store, nil)
	pn := func(i int) types.JID { return types.NewJID("1555000010"+strconv.Itoa(i), types.DefaultUserServer) }
	ir.Canonical(bobLID, pn(1)) // first sighting carries both: the number's identity
	for i := 2; i <= 7; i++ {
		ir.Canonical(pn(i), types.EmptyJID) // seen alone first
		if got := ir.Canonical(bobLID, pn(i)); got != pn(i) {
			t.Fatalf("join %d resolved to %s", i, got)
		}
	}
	stored, _ := store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
	want := strings.Join([]string{pn(6).String(), pn(5).String(), pn(4).String(), pn(3).String()}, " ")
	if stored != want {
		t.Fatalf("stored formers %q, want the newest %d: %q", stored, whatsAppMaxFormers, want)
	}
	ir.Learn(pn(5), bobLID) // a stale mapping re-points back to a former
	stored, _ = store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
	if strings.Contains(stored, pn(5).String()) || !strings.HasPrefix(stored, pn(7).String()+" ") {
		t.Fatalf("after re-pointing back to %s the formers are %q", pn(5), stored)
	}
	for _, f := range ir.Formers(bobLID, types.EmptyJID) {
		if f == pn(5) {
			t.Fatalf("Formers returned the current identity: %v", ir.Formers(bobLID, types.EmptyJID))
		}
	}
	// Even a stored list naming the current identity is read without it.
	store.stageIdentities(map[string]string{whatsAppFormerKey(bobLID.String()): pn(5).String() + " " + pn(2).String()})
	if f := ir.Formers(bobLID, types.EmptyJID); len(f) != 1 || f[0] != pn(2) {
		t.Fatalf("Formers = %v, want [%s]", f, pn(2))
	}
}

// Formers live in the sealed identity table, so they survive a restart and
// a re-link with the outbox.
func TestWhatsAppIdentityFormersSurviveAReopen(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_formers")
	ir := newWhatsAppIdentityResolver(ob, nil)
	ir.Canonical(bobLID, types.EmptyJID)
	ir.Canonical(bobPN, types.EmptyJID)
	ir.Canonical(bobLID, bobPN)
	if err := ob.Append([]whatsAppRecord{textRecord(waGroup.String(), "X", "x")}, false); err != nil {
		t.Fatal(err)
	}
	ob.Close()
	ob2, _, err := openWhatsAppOutbox(dir, "lnk_formers")
	if err != nil {
		t.Fatal(err)
	}
	defer ob2.Close()
	ir2 := newWhatsAppIdentityResolver(ob2, nil)
	if got := ir2.Canonical(bobLID, types.EmptyJID); got != bobPN {
		t.Fatalf("after reopen the LID resolves to %s", got)
	}
	if f := ir2.Formers(bobLID, types.EmptyJID); len(f) != 1 || f[0] != bobLID {
		t.Fatalf("after reopen Formers = %v", f)
	}
}

// The verifiers' case: Bob deletes a message from before the join for
// everyone, and the revoke's own participant_pn is the join. The revoke must
// reach G1 where it is stored, under his LID, or its text stays stored and
// visible. Edits and reactions aimed at G1 likewise; a person never
// re-pointed still costs exactly one record.
func TestNormalizeOpsReachAMessageWrittenBeforeTheJoin(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: testWhatsAppIdentity(nil)}
	splitInGroup(t, nc)
	group := waGroup.String()
	byNumber := [2]string{group, bobPN.String()}
	byLID := [2]string{group, bobLID.String()}

	revoke := liveMsg(waGroup, bobLID, false, "R1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("G1"), Participant: proto.String(bobLID.String())},
	}})
	revoke.Info.SenderAlt = bobPN // participant_pn: the revoke itself joins Bob's forms
	wantTargets(t, normalizeWhatsAppMessage(revoke, waSourceLive, nc), waOpRevoke, "G1", byNumber, byLID)

	// A group admin deletes it: the target's author is the key's
	// participant, not the revoker.
	adminRevoke := liveMsg(waGroup, dave, false, "R3", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("G1"), Participant: proto.String(bobLID.String())},
	}})
	wantTargets(t, normalizeWhatsAppMessage(adminRevoke, waSourceLive, nc), waOpRevoke, "G1", byNumber, byLID)

	edit := liveMsg(waGroup, bobLID, false, "E1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("G1")},
		EditedMessage: textMsg("edited"),
	}})
	wantTargets(t, normalizeWhatsAppMessage(edit, waSourceLive, nc), waOpEdit, "G1", byNumber, byLID)

	daveReacts := liveMsg(waGroup, dave, false, "X1", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("G1"), Participant: proto.String(bobLID.String())},
		Text: proto.String("👍"),
	}})
	recs := normalizeWhatsAppMessage(daveReacts, waSourceLive, nc)
	wantTargets(t, recs, waOpReaction, "G1", byNumber, byLID)
	for _, r := range recs {
		if r.ReactorJid != dave.String() || r.Reaction == nil || *r.Reaction != "👍" {
			t.Fatalf("Dave's reaction changed on its way: %+v", r)
		}
	}

	daveRevokes := liveMsg(waGroup, dave, false, "R2", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("D1"), Participant: proto.String(dave.String())},
	}})
	one(t, normalizeWhatsAppMessage(daveRevokes, waSourceLive, nc))
}

// A re-pointed REACTOR holds one reaction per message: the one they may have
// left under their former identity is taken off, so a change since the join
// does not show beside it.
func TestNormalizeAReactorsFormerReactionIsTakenOff(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: testWhatsAppIdentity(nil)}
	splitInGroup(t, nc)
	nc.canonicalUser(bobLID, bobPN) // a live message from Bob joins his forms
	byBob := liveMsg(waGroup, bobLID, false, "X2", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("D1"), Participant: proto.String(dave.String())},
		Text: proto.String("❤️"),
	}})
	recs := normalizeWhatsAppMessage(byBob, waSourceLive, nc)
	wantTargets(t, recs, waOpReaction, "D1", [2]string{waGroup.String(), dave.String()}, [2]string{waGroup.String(), dave.String()})
	if recs[0].ReactorJid != bobPN.String() || *recs[0].Reaction != "❤️" {
		t.Fatalf("the reaction itself: %+v", recs[0])
	}
	if recs[1].ReactorJid != bobLID.String() || *recs[1].Reaction != "" {
		t.Fatalf("the former identity's reaction is not taken off: %+v", recs[1])
	}
}

// In a person's 1:1 chat the chat and its author are one person, re-pointed
// together: a revoke of their message goes to (former, former), not to every
// combination. My message in that chat is under the former chat with me as
// its author.
func TestNormalizeOneToOneTargetsMoveWithThePeer(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: testWhatsAppIdentity(nil)}
	one(t, normalizeWhatsAppMessage(liveMsg(bobLID, bobLID, false, "M1", textMsg("old")), waSourceHistory, nc))
	one(t, normalizeWhatsAppMessage(liveMsg(bobPN, bobPN, false, "M2", textMsg("hi")), waSourceHistory, nc))
	joined := liveMsg(bobLID, bobLID, false, "M3", textMsg("joined"))
	joined.Info.SenderAlt = bobPN
	if r := one(t, normalizeWhatsAppMessage(joined, waSourceLive, nc)); r.ChatJid != bobPN.String() {
		t.Fatalf("the join did not move the chat: %s", r.ChatJid)
	}

	revoke := liveMsg(bobLID, bobLID, false, "R1", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("M1")},
	}})
	wantTargets(t, normalizeWhatsAppMessage(revoke, waSourceLive, nc), waOpRevoke, "M1",
		[2]string{bobPN.String(), bobPN.String()}, [2]string{bobLID.String(), bobLID.String()})

	reacts := liveMsg(bobLID, bobLID, false, "X3", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String("MINE")},
		Text: proto.String("👍"),
	}})
	recs := normalizeWhatsAppMessage(reacts, waSourceLive, nc)
	me := waMe.String()
	wantTargets(t, recs, waOpReaction, "MINE",
		[2]string{bobPN.String(), me}, [2]string{bobLID.String(), me},
		[2]string{bobPN.String(), me}, [2]string{bobLID.String(), me})
	for i, want := range []struct{ reactor, reaction string }{
		{bobPN.String(), "👍"}, {bobPN.String(), "👍"}, {bobLID.String(), ""}, {bobLID.String(), ""},
	} {
		if recs[i].ReactorJid != want.reactor || *recs[i].Reaction != want.reaction {
			t.Fatalf("record %d: reactor %s reaction %q, want %s %q", i, recs[i].ReactorJid, *recs[i].Reaction, want.reactor, want.reaction)
		}
	}
}
