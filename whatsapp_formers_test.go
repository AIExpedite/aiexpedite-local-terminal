package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Former identities (whatsapp_identity.go, Resolve): a person written under
// their LID and, separately, under their number before anything joined the
// two keeps the LID side's messages there. Edits, revokes and reactions aimed
// at those messages must still reach them after the join.

var (
	bobLID = types.NewJID("424242424242", types.HiddenUserServer)
	bobPN  = types.NewJID("15550000042", types.DefaultUserServer)
	dave   = types.NewJID("15550000077", types.DefaultUserServer)
)

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func reactionMessage(key *waCommon.MessageKey, emoji string) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: key, Text: proto.String(emoji)}}
}

func revokeMessage(key *waCommon.MessageKey) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: key}}
}

func editMessage(key *waCommon.MessageKey, text string, at time.Time) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: key, EditedMessage: textMsg(text), TimestampMS: proto.Int64(at.UnixMilli()),
	}}
}

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

// repointedChat writes Bob's 1:1 chat the way history splits it - keyed by
// his LID (his M1 and my MINE0) and, separately, by his number (his M2) -
// then a live message from him carrying both forms joins them, at waTS.
func repointedChat(t *testing.T) whatsAppNormalizeContext {
	t.Helper()
	ir := testWhatsAppIdentity(nil)
	ir.now = func() time.Time { return waTS }
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
	one(t, normalizeWhatsAppMessage(liveMsg(bobLID, bobLID, false, "M1", textMsg("old")), waSourceHistory, nc))
	mine0 := one(t, normalizeWhatsAppMessage(liveMsg(bobLID, waMe, true, "MINE0", textMsg("old mine")), waSourceHistory, nc))
	if mine0.ChatJid != bobLID.String() || mine0.SenderJid != waMe.String() {
		t.Fatalf("MINE0 stored at (%s, %s), want (%s, %s)", mine0.ChatJid, mine0.SenderJid, bobLID, waMe)
	}
	one(t, normalizeWhatsAppMessage(liveMsg(bobPN, bobPN, false, "M2", textMsg("hi")), waSourceHistory, nc))
	joined := liveMsg(bobLID, bobLID, false, "M3", textMsg("joined"))
	joined.Info.SenderAlt = bobPN
	if r := one(t, normalizeWhatsAppMessage(joined, waSourceLive, nc)); r.ChatJid != bobPN.String() {
		t.Fatalf("the join did not move the chat: %s", r.ChatJid)
	}
	return nc
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
// keeps its newest formers, each with when its own re-point retired it, never
// the identity it has now, each identity once, at most whatsAppMaxFormers.
func TestWhatsAppIdentityFormersAreBoundedAndNeverCurrent(t *testing.T) {
	store := newMemoryWhatsAppIdentityStore()
	ir := newWhatsAppIdentityResolver(store, nil)
	var clock time.Time
	ir.now = func() time.Time { return clock }
	at := func(i int) time.Time { return waTS.Add(time.Duration(i) * time.Minute) }
	pn := func(i int) types.JID { return types.NewJID("1555000010"+strconv.Itoa(i), types.DefaultUserServer) }
	// entry is pn(i) as a former retired at join `retired`.
	entry := func(i, retired int) string { return whatsAppFormerEntry(pn(i).String(), at(retired)) }
	ir.Canonical(bobLID, pn(1)) // first sighting carries both: the number's identity
	for i := 2; i <= 7; i++ {
		clock = at(i)
		ir.Canonical(pn(i), types.EmptyJID) // seen alone first
		if got := ir.Canonical(bobLID, pn(i)); got != pn(i) {
			t.Fatalf("join %d resolved to %s", i, got)
		}
	}
	stored, _ := store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
	if want := strings.Join([]string{entry(6, 7), entry(5, 6), entry(4, 5), entry(3, 4)}, " "); stored != want {
		t.Fatalf("stored formers %q, want the newest %d: %q", stored, whatsAppMaxFormers, want)
	}
	clock = at(8)
	ir.Learn(pn(5), bobLID) // a stale mapping re-points back to a former
	stored, _ = store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
	if want := strings.Join([]string{entry(7, 8), entry(6, 7), entry(4, 5), entry(3, 4)}, " "); stored != want {
		t.Fatalf("after re-pointing back to %s the formers are %q, want %q", pn(5), stored, want)
	}
	for _, f := range ir.Formers(bobLID, types.EmptyJID) {
		if f == pn(5) {
			t.Fatalf("Formers returned the current identity: %v", ir.Formers(bobLID, types.EmptyJID))
		}
	}
	// Even a stored list naming the current identity is read without it, an
	// identity it repeats is read once, and an entry that does not say when
	// it was retired reads as retired at the zero time.
	store.stageIdentities(map[string]string{whatsAppFormerKey(bobLID.String()): pn(5).String() + " " + pn(2).String() + " " + entry(2, 3)})
	if _, f := ir.Resolve(bobLID, types.EmptyJID, false); len(f) != 1 || f[0].JID != pn(2) || !f[0].RetiredAt.IsZero() {
		t.Fatalf("Resolve read %+v, want only %s, retired at the zero time", f, pn(2))
	}
	// The next re-point writes each identity once, compared by identity.
	clock = at(9)
	ir.Canonical(pn(8), types.EmptyJID)
	ir.Canonical(bobLID, pn(8))
	stored, _ = store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
	if want := entry(5, 9) + " " + pn(2).String(); stored != want {
		t.Fatalf("after the next re-point the formers are %q, want %q", stored, want)
	}
}

// Formers live in the sealed identity table, so they survive a restart and
// a re-link with the outbox, and so does when each was retired.
func TestWhatsAppIdentityFormersSurviveAReopen(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_formers")
	ir := newWhatsAppIdentityResolver(ob, nil)
	ir.now = func() time.Time { return waTS }
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
	if _, f := ir2.Resolve(bobLID, types.EmptyJID, false); len(f) != 1 || !f[0].RetiredAt.Equal(waTS) {
		t.Fatalf("after reopen the former was retired at %+v, want %s", f, waTS)
	}
}

// A former list's key is something no JID string can be: a payload can name
// any address, so a key a JID could take would let a crafted address read or
// plant a person's former list.
func TestWhatsAppFormerKeyIsNoJIDString(t *testing.T) {
	for _, form := range []types.JID{dave, bobLID, waMe} {
		key := whatsAppFormerKey(form.String())
		j, err := types.ParseJID(key)
		if err == nil && j.String() == key {
			t.Fatalf("the former key %q is the JID %s", key, j)
		}
		if _, ok := whatsAppPersonForm(j); ok {
			t.Fatalf("the former key %q parses as a person: %s", key, j)
		}
	}
}

// A person's address is digits - a phone number or a LID - on a person's
// server. Anything else is no person: never resolved, so it never reaches the
// identity table.
func TestWhatsAppPersonFormNeedsADigitsOnlyUser(t *testing.T) {
	for _, c := range []struct{ jid, want string }{ // want "": not a person
		{"15550000077@s.whatsapp.net", "15550000077@s.whatsapp.net"},
		{"15550000077:3@s.whatsapp.net", "15550000077@s.whatsapp.net"},
		{"15550000077@c.us", "15550000077@s.whatsapp.net"},
		{"424242424242:7@lid", "424242424242@lid"},
		{"former|15550000077@s.whatsapp.net", ""},
		{"former@15550000077@s.whatsapp.net", ""},
		{"+15550000077@s.whatsapp.net", ""},
		{"1555 0000077@s.whatsapp.net", ""},
		{"bob@lid", ""},
		{"@s.whatsapp.net", ""},
		{"120363000000000001@g.us", ""},
	} {
		j, err := types.ParseJID(c.jid)
		if err != nil {
			t.Fatalf("%s: %v", c.jid, err)
		}
		if got, ok := whatsAppPersonForm(j); ok != (c.want != "") || (ok && got.String() != c.want) {
			t.Fatalf("whatsAppPersonForm(%s) = %s, %v; want %q", c.jid, got, ok, c.want)
		}
	}
}

// whatsAppIdentityRowCount is how many rows the identity table holds.
func whatsAppIdentityRowCount(t *testing.T, store whatsAppIdentityStore) int {
	t.Helper()
	switch s := store.(type) {
	case *memoryWhatsAppIdentityStore:
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.m)
	case *whatsAppOutbox:
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.identities)
	}
	t.Fatalf("unknown identity store %T", store)
	return 0
}

// A reaction or revoke key's participant is the sender's own payload. One
// naming a former key - today's, or the "former|" one an earlier draft used,
// of Dave, of Bob (whose list is real) or of the linked account - is no
// person: it plants no row and reads no former list, so Dave, whom no join
// ever re-pointed, still costs one record per op. On the memory table, and on
// the sealed one across the Append that commits what was staged and a reopen.
func TestNormalizeACraftedParticipantPlantsNoFormer(t *testing.T) {
	eve := types.NewJID("15550000099", types.DefaultUserServer)
	var crafted []string
	for _, victim := range []types.JID{dave, bobLID, waMe} {
		crafted = append(crafted, whatsAppFormerKey(victim.String()), "former|"+victim.String())
	}
	for _, onOutbox := range []bool{false, true} {
		name := "memory"
		if onOutbox {
			name = "outbox"
		}
		t.Run(name, func(t *testing.T) {
			var store whatsAppIdentityStore = newMemoryWhatsAppIdentityStore()
			var ob *whatsAppOutbox
			var dir string
			if onOutbox {
				withWhatsAppTestEnv(t)
				ob, dir = openTestOutbox(t, "lnk_crafted")
				defer ob.Close() // closed below before the reopen; a second Close is a no-op
				store = ob
			}
			ir := newWhatsAppIdentityResolver(store, nil)
			nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
			splitInGroup(t, nc)
			nc.canonicalUser(bobLID, bobPN) // a live message from Bob joins his forms
			bobList, _ := store.lookupIdentity(whatsAppFormerKey(bobLID.String()))
			if bobList == "" {
				t.Fatal("Bob's join kept no former")
			}
			recs := []whatsAppRecord{
				one(t, normalizeWhatsAppMessage(liveMsg(waGroup, dave, false, "D1", textMsg("hi")), waSourceLive, nc)),
				one(t, normalizeWhatsAppMessage(liveMsg(waGroup, eve, false, "E1", textMsg("hey")), waSourceLive, nc)),
			}
			rows := whatsAppIdentityRowCount(t, store)
			for i, p := range crafted {
				key := &waCommon.MessageKey{ID: proto.String("D1"), Participant: proto.String(p)}
				for _, m := range []*waE2E.Message{reactionMessage(key, "x"), revokeMessage(key)} {
					// One record each: a crafted target is nobody, with no formers.
					recs = append(recs, one(t, normalizeWhatsAppMessage(liveMsg(waGroup, eve, false, "C"+strconv.Itoa(i), m), waSourceLive, nc)))
				}
			}
			if n := whatsAppIdentityRowCount(t, store); n != rows {
				t.Fatalf("crafted participants planted %d identity rows", n-rows)
			}
			if onOutbox {
				// The records' own Append commits whatever they staged.
				if err := ob.Append(recs, false); err != nil {
					t.Fatal(err)
				}
				ob.Close()
				reopened, _, err := openWhatsAppOutbox(dir, "lnk_crafted")
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				store, ir = reopened, newWhatsAppIdentityResolver(reopened, nil)
				nc.Identity = ir
				if n := whatsAppIdentityRowCount(t, store); n != rows {
					t.Fatalf("after reopen the table holds %d rows, want %d", n, rows)
				}
			}
			if list, _ := store.lookupIdentity(whatsAppFormerKey(bobLID.String())); list != bobList {
				t.Fatalf("Bob's former list changed from %q to %q", bobList, list)
			}
			for _, victim := range []types.JID{dave, waMe} {
				if f := ir.Formers(victim, types.EmptyJID); f != nil {
					t.Fatalf("%s, never re-pointed, has formers: %v", victim, f)
				}
			}
			one(t, normalizeWhatsAppMessage(liveMsg(waGroup, dave, false, "R9",
				revokeMessage(&waCommon.MessageKey{ID: proto.String("D1"), Participant: proto.String(dave.String())})), waSourceLive, nc))
		})
	}
}

// The verifiers' case: Bob deletes a message from before the join for
// everyone, and the revoke's own participant_pn is the join. The revoke must
// reach G1 where it is stored, under his LID, or its text stays stored and
// visible. Edits and reactions aimed at G1 likewise; a person never
// re-pointed still costs exactly one record.
func TestNormalizeOpsReachAMessageWrittenBeforeTheJoin(t *testing.T) {
	ir := testWhatsAppIdentity(nil)
	ir.now = func() time.Time { return waTS } // the revoke below joins Bob's forms at waTS
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
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
		TimestampMS:   proto.Int64(waTS.Add(5 * time.Minute).UnixMilli()), // within the edit window of the join
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

// An edit can target only a message sent within whatsmeow.EditWindow of it,
// and a message stored under a former identity arrived before the re-point
// retired it. An edit later than the window (plus an hour of clock skew)
// after the re-point therefore goes to the current identity alone: a copy
// under the former would only park, with its text.
func TestNormalizeAnEditLongAfterTheJoinGoesOnlyToTheCurrentIdentity(t *testing.T) {
	ir := testWhatsAppIdentity(nil)
	ir.now = func() time.Time { return waTS } // Bob's join below happens at waTS
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
	splitInGroup(t, nc)
	joined := liveMsg(waGroup, bobLID, false, "J1", textMsg("joined"))
	joined.Info.SenderAlt = bobPN
	one(t, normalizeWhatsAppMessage(joined, waSourceLive, nc))
	group, me := waGroup.String(), waMe.String()

	// Bob edits X1, a message of his from after the join: the author's former.
	byBob := func(after time.Duration) []whatsAppRecord {
		return normalizeWhatsAppMessage(liveMsg(waGroup, bobLID, false, "E1",
			editMessage(&waCommon.MessageKey{ID: proto.String("X1")}, "edited", waTS.Add(after))), waSourceLive, nc)
	}
	wantTargets(t, byBob(whatsAppEditFormerWindow-time.Millisecond), waOpEdit, "X1", [2]string{group, bobPN.String()}, [2]string{group, bobLID.String()})
	wantTargets(t, byBob(whatsAppEditFormerWindow), waOpEdit, "X1", [2]string{group, bobPN.String()})
	wantTargets(t, byBob(whatsAppEditFormerWindow+time.Hour), waOpEdit, "X1", [2]string{group, bobPN.String()})

	// I edit MINE1, mine from after the join, in Bob's 1:1 chat: the chat's
	// former goes the same way.
	mine := func(after time.Duration) []whatsAppRecord {
		m := liveMsg(bobLID, waMe, true, "E2",
			editMessage(&waCommon.MessageKey{ID: proto.String("MINE1"), FromMe: proto.Bool(true)}, "fixed", waTS.Add(after)))
		m.Info.RecipientAlt = bobPN
		return normalizeWhatsAppMessage(m, waSourceLive, nc)
	}
	wantTargets(t, mine(5*time.Minute), waOpEdit, "MINE1", [2]string{bobPN.String(), me}, [2]string{bobLID.String(), me})
	wantTargets(t, mine(whatsAppEditFormerWindow+time.Hour), waOpEdit, "MINE1", [2]string{bobPN.String(), me})
}

// My own message from before the join is stored at (bobLID, me), in the
// LID-keyed copy of Bob's chat. After the join my delete-for-everyone and my
// edit of it must still reach it there - under the chat's former identity -
// however the event addresses the chat.
func TestNormalizeMyOpsReachMyMessageInARepointedChat(t *testing.T) {
	me := waMe.String()
	want := [][2]string{{bobPN.String(), me}, {bobLID.String(), me}}
	for _, addr := range []struct {
		name      string
		chat, alt types.JID
	}{
		{"by LID", bobLID, types.EmptyJID},
		{"by LID with the number", bobLID, bobPN},
		{"by number with the LID", bobPN, bobLID},
		{"by number alone", bobPN, types.EmptyJID},
	} {
		t.Run(addr.name, func(t *testing.T) {
			nc := repointedChat(t)
			mine0 := &waCommon.MessageKey{ID: proto.String("MINE0"), FromMe: proto.Bool(true)}
			revoke := liveMsg(addr.chat, waMe, true, "R9", revokeMessage(mine0))
			revoke.Info.RecipientAlt = addr.alt
			wantTargets(t, normalizeWhatsAppMessage(revoke, waSourceLive, nc), waOpRevoke, "MINE0", want...)

			edit := liveMsg(addr.chat, waMe, true, "E9", editMessage(mine0, "fixed", waTS.Add(5*time.Minute)))
			edit.Info.RecipientAlt = addr.alt
			recs := normalizeWhatsAppMessage(edit, waSourceLive, nc)
			wantTargets(t, recs, waOpEdit, "MINE0", want...)
			for _, r := range recs {
				if r.Text != "fixed" {
					t.Fatalf("the edit's text changed on its way: %+v", r)
				}
			}
		})
	}
}

// whatsAppReactionTarget's branches feed the copies too: after Bob's chat is
// re-pointed, a reaction to a message from before the join also goes where
// that message is stored.
func TestNormalizeReactionTargetsInARepointedChat(t *testing.T) {
	nc := repointedChat(t)
	me, pn, lid := waMe.String(), bobPN.String(), bobLID.String()

	// The event is mine and the key is not: I react to Bob's M1, stored at
	// (bobLID, bobLID).
	toM1 := normalizeWhatsAppMessage(liveMsg(bobLID, waMe, true, "X1",
		reactionMessage(&waCommon.MessageKey{ID: proto.String("M1"), FromMe: proto.Bool(false)}, "👍")), waSourceLive, nc)
	wantTargets(t, toM1, waOpReaction, "M1", [2]string{pn, pn}, [2]string{lid, lid})
	// The key is mine: I react to my own MINE0, stored at (bobLID, me).
	toMine0 := normalizeWhatsAppMessage(liveMsg(bobLID, waMe, true, "X2",
		reactionMessage(&waCommon.MessageKey{ID: proto.String("MINE0"), FromMe: proto.Bool(true)}, "👍")), waSourceLive, nc)
	wantTargets(t, toMine0, waOpReaction, "MINE0", [2]string{pn, me}, [2]string{lid, me})
	for _, r := range append(toM1, toMine0...) {
		if r.ReactorJid != me || *r.Reaction != "👍" {
			t.Fatalf("my reaction changed on its way: %+v", r)
		}
	}

	// No linked account to name (the fallback): Bob's reaction to a message
	// that is not his is aimed at the chat, and the reaction he may have left
	// under his former identity is taken off.
	blind := whatsAppNormalizeContext{Identity: nc.Identity}
	recs := normalizeWhatsAppMessage(liveMsg(bobLID, bobLID, false, "X3",
		reactionMessage(&waCommon.MessageKey{ID: proto.String("T3"), FromMe: proto.Bool(false)}, "❤️")), waSourceLive, blind)
	wantTargets(t, recs, waOpReaction, "T3", [2]string{pn, pn}, [2]string{lid, lid}, [2]string{pn, pn}, [2]string{lid, lid})
	for i, want := range []struct{ reactor, reaction string }{{pn, "❤️"}, {pn, "❤️"}, {lid, ""}, {lid, ""}} {
		if recs[i].ReactorJid != want.reactor || *recs[i].Reaction != want.reaction {
			t.Fatalf("record %d: reactor %s reaction %q, want %s %q", i, recs[i].ReactorJid, *recs[i].Reaction, want.reactor, want.reaction)
		}
	}
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

// A former NUMBER may be someone else's by now (a number change, then the old
// number recycled), and the table still names it as their identity. A
// re-pointed reactor's removal under it would take the new owner's reaction
// off, so removals go out only under formers no other row names.
func TestNormalizeAReactorLeavesAFormerNumberThatIsSomeoneElsesNow(t *testing.T) {
	xLID := types.NewJID("424242000001", types.HiddenUserServer)
	yLID := types.NewJID("424242000002", types.HiddenUserServer)
	p1 := types.NewJID("15550000101", types.DefaultUserServer)
	p2 := types.NewJID("15550000102", types.DefaultUserServer)
	reacts := func(lid, pn types.JID, id, emoji string) *events.Message {
		m := liveMsg(waGroup, lid, false, id, reactionMessage(&waCommon.MessageKey{ID: proto.String("D1"), Participant: proto.String(dave.String())}, emoji))
		m.Info.SenderAlt = pn
		return m
	}
	for _, c := range []struct {
		name  string
		first [][2]types.JID // X's sightings before the number change: (address, alternate)
	}{
		{"X first seen with P1", [][2]types.JID{{xLID, p1}}},
		{"X first seen by LID alone", [][2]types.JID{{xLID, types.EmptyJID}, {xLID, p1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ir := testWhatsAppIdentity(nil)
			nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
			for i, s := range c.first {
				m := liveMsg(waGroup, s[0], false, "B"+strconv.Itoa(i), textMsg("P1 days"))
				m.Info.SenderAlt = s[1]
				one(t, normalizeWhatsAppMessage(m, waSourceLive, nc))
			}
			// X's number changes to P2: seen alone first, then joined.
			one(t, normalizeWhatsAppMessage(liveMsg(waGroup, p2, false, "B8", textMsg("new number")), waSourceHistory, nc))
			joined := liveMsg(waGroup, xLID, false, "B9", textMsg("joined"))
			joined.Info.SenderAlt = p2
			if r := one(t, normalizeWhatsAppMessage(joined, waSourceLive, nc)); r.SenderJid != p2.String() {
				t.Fatalf("X's join resolved to %s", r.SenderJid)
			}
			formers := ir.Formers(xLID, p2)
			if len(formers) != 1 {
				t.Fatalf("X's formers after the number change: %v", formers)
			}

			// P1 goes to Y, who reacts to Dave's D1; X reacts to it after.
			one(t, normalizeWhatsAppMessage(liveMsg(waGroup, dave, false, "D1", textMsg("dave says")), waSourceLive, nc))
			y := one(t, normalizeWhatsAppMessage(reacts(yLID, p1, "XY", "❤️"), waSourceLive, nc))
			if y.ReactorJid != formers[0].String() {
				t.Fatalf("Y reacts as %s, not as X's former %s: the case does not arise", y.ReactorJid, formers[0])
			}
			recs := normalizeWhatsAppMessage(reacts(xLID, p2, "XX", "👍"), waSourceLive, nc)
			if recs[0].ReactorJid != p2.String() || *recs[0].Reaction != "👍" {
				t.Fatalf("X's own reaction: %+v", recs[0])
			}
			for _, r := range recs {
				if r.ReactorJid == y.ReactorJid && *r.Reaction == "" {
					t.Fatalf("X's reaction takes Y's (%s) off: %+v", y.ReactorJid, r)
				}
			}
		})
	}
}

// Shared marks a former that a row outside the person's own pair names too,
// as its identity or in a former list: such an identity may be someone
// else's now. Worked out only when asked, the same on the memory table and
// on the sealed one.
func TestWhatsAppIdentitySharedFormers(t *testing.T) {
	xLID := types.NewJID("424242000001", types.HiddenUserServer)
	p1 := types.NewJID("15550000101", types.DefaultUserServer)
	p2 := types.NewJID("15550000102", types.DefaultUserServer)
	carolLID := types.NewJID("777777777777", types.HiddenUserServer)
	for _, onOutbox := range []bool{false, true} {
		name := "memory"
		if onOutbox {
			name = "outbox"
		}
		t.Run(name, func(t *testing.T) {
			var store whatsAppIdentityStore = newMemoryWhatsAppIdentityStore()
			if onOutbox {
				withWhatsAppTestEnv(t)
				ob, _ := openTestOutbox(t, "lnk_shared")
				defer ob.Close()
				store = ob
			}
			ir := newWhatsAppIdentityResolver(store, nil)
			// X moved from P1 to P2: P1 is X's former and still P1's own identity.
			ir.Canonical(xLID, p1)
			ir.Canonical(p2, types.EmptyJID)
			ir.Canonical(xLID, p2)
			// Bob split, then joined: bobLID is a former only his own rows name.
			ir.Canonical(bobLID, types.EmptyJID)
			ir.Canonical(bobPN, types.EmptyJID)
			ir.Canonical(bobLID, bobPN)
			check := func(primary types.JID, shared bool, want types.JID, wantShared bool) {
				t.Helper()
				if _, f := ir.Resolve(primary, types.EmptyJID, shared); len(f) != 1 || f[0].JID != want || f[0].Shared != wantShared {
					t.Fatalf("Resolve(%s, shared=%v) formers = %+v, want %s shared=%v", primary, shared, f, want, wantShared)
				}
			}
			check(xLID, true, p1, true)
			check(xLID, false, p1, false) // not asked, not worked out
			check(bobLID, true, bobLID, false)
			// Another person's former list naming bobLID makes it shared too.
			store.stageIdentities(map[string]string{whatsAppFormerKey(carolLID.String()): whatsAppFormerEntry(bobLID.String(), waTS)})
			check(bobLID, true, bobLID, true)
		})
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

// whatsAppTargetCopies never repeats the record itself: a former equal to
// the record's own chat or author (formers read apart from the identity,
// across a join, could list it) adds no copy.
func TestWhatsAppTargetCopiesNeverRepeatTheRecord(t *testing.T) {
	me := waMe.String()
	rec := whatsAppRecord{Kind: waRecordMessage, Op: waOpRevoke, ChatJid: bobPN.String(), SenderJid: me, WaMessageID: "MINE0"}
	wantTargets(t, whatsAppTargetCopies(rec, []types.JID{bobPN, bobLID}, []types.JID{waMe}), waOpRevoke, "MINE0",
		[2]string{bobPN.String(), me}, [2]string{bobLID.String(), me})
}

/* --------------------------------------------------------------------------
   A join racing a normalization
   -------------------------------------------------------------------------- */

// joinAtCall is the link's resolver with a join - a history chunk's Learn,
// which runs on whatsmeow's history goroutine - let in before the k-th
// identity call of one normalization. Torn, it is let in INSIDE the k-th
// Resolve instead, between its identity and its formers: what a resolver
// reading the two under separate locks would allow.
type joinAtCall struct {
	*whatsAppIdentityResolver
	k, calls int
	torn     bool
	join     func()
}

// runJoin runs the join on another goroutine, as in production, and waits.
func (j *joinAtCall) runJoin() {
	done := make(chan struct{})
	go func() { defer close(done); j.join() }()
	<-done
}

func (j *joinAtCall) Canonical(primary, alt types.JID) types.JID {
	j.calls++
	if j.calls == j.k {
		j.runJoin()
	}
	return j.whatsAppIdentityResolver.Canonical(primary, alt)
}

func (j *joinAtCall) Resolve(primary, alt types.JID, shared bool) (types.JID, []whatsAppFormer) {
	j.calls++
	switch {
	case j.calls != j.k:
	case !j.torn:
		j.runJoin()
	default:
		id := j.whatsAppIdentityResolver.Canonical(primary, alt)
		j.runJoin()
		_, formers := j.whatsAppIdentityResolver.Resolve(primary, alt, shared)
		return id, formers
	}
	return j.whatsAppIdentityResolver.Resolve(primary, alt, shared)
}

// historyMapping is a history chunk carrying one person's PN-LID mapping and
// nothing else: normalizing it is the history path's join (Learn).
func historyMapping(pn, lid types.JID) *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{
			{PnJID: proto.String(pn.String()), LidJID: proto.String(lid.String())},
		},
	}
}

// visibleReactions folds the reaction records on id the way the cloud
// applies them (whatsapp-service applyMessageOp): one message per (chat,
// author), one reaction per reactor on it, a record below the deviceSeq it
// holds skipped, "" taking the reaction off. It returns what still shows.
func visibleReactions(recs []whatsAppRecord, id string) map[string]string {
	shown, seqs := map[string]string{}, map[string]int64{}
	for _, r := range recs {
		if r.Op != waOpReaction || r.WaMessageID != id || r.Reaction == nil {
			continue
		}
		k := r.ChatJid + " " + r.SenderJid + " " + r.ReactorJid
		if r.DeviceSeq < seqs[k] {
			continue
		}
		seqs[k] = r.DeviceSeq
		if *r.Reaction == "" {
			delete(shown, k)
		} else {
			shown[k] = *r.Reaction
		}
	}
	return shown
}

// noRecordTwice fails when a record goes out twice.
func noRecordTwice(t *testing.T, recs []whatsAppRecord) {
	t.Helper()
	seen := map[string]bool{}
	for _, r := range recs {
		reaction := "-"
		if r.Reaction != nil {
			reaction = "=" + *r.Reaction
		}
		k := strings.Join([]string{r.Op, r.ChatJid, r.SenderJid, r.WaMessageID, r.ReactorJid, reaction}, " ")
		if seen[k] {
			t.Fatalf("a record goes out twice: %+v", recs)
		}
		seen[k] = true
	}
}

// A join can land between any two identity calls of one normalization. Bob's
// reaction must never go out with its own removal, and the cloud's fold must
// still show it - in a group and in his own 1:1 chat, with the join before
// each call in turn, and after them all. Torn, under a resolver that reads
// the identity and the formers apart, the normalizer's own checks keep the
// reaction and send no copy twice.
func TestNormalizeAReactionSurvivesAJoinRacingIt(t *testing.T) {
	withWhatsAppTestEnv(t)
	for _, torn := range []bool{false, true} {
		for _, where := range []string{"group", "direct"} {
			for k := 1; k <= 5; k++ {
				name := fmt.Sprintf("%s/join-at-call-%d", where, k)
				if torn {
					name = "torn/" + name
				}
				t.Run(name, func(t *testing.T) {
					ob, _ := openTestOutbox(t, fmt.Sprintf("lnk_race_%v_%s_%d", torn, where, k))
					defer ob.Close()
					// whatsmeow's own store does not know Bob: only the chunk joins him.
					ir := newWhatsAppIdentityResolver(ob, func(types.JID) types.JID { return types.EmptyJID })
					plain := whatsAppNormalizeContext{OwnJID: waMe, Identity: ir}
					chat, key := waGroup, &waCommon.MessageKey{ID: proto.String("T1"), Participant: proto.String(dave.String())}
					if where == "group" {
						splitInGroup(t, plain)
					} else {
						// His chat split the same way; he reacts to MY message in it.
						one(t, normalizeWhatsAppMessage(liveMsg(bobLID, bobLID, false, "M1", textMsg("old")), waSourceHistory, plain))
						one(t, normalizeWhatsAppMessage(liveMsg(bobPN, bobPN, false, "M2", textMsg("hi")), waSourceHistory, plain))
						chat, key = bobLID, &waCommon.MessageKey{ID: proto.String("T1")}
					}
					racing := &joinAtCall{whatsAppIdentityResolver: ir, k: k, torn: torn, join: func() {
						normalizeWhatsAppHistory(historyMapping(bobPN, bobLID), nil, plain)
					}}
					recs := normalizeWhatsAppMessage(liveMsg(chat, bobLID, false, "X1", reactionMessage(key, "❤️")), waSourceLive,
						whatsAppNormalizeContext{OwnJID: waMe, Identity: racing})
					if racing.calls < k {
						racing.runJoin() // the join after the whole normalization
					}
					if err := ob.Append(recs, false); err != nil {
						t.Fatal(err)
					}
					for _, r := range recs[1:] {
						if *r.Reaction == "" && r.ReactorJid == recs[0].ReactorJid {
							t.Fatalf("the reaction goes out with its own removal: %+v", recs)
						}
					}
					if len(visibleReactions(recs, "T1")) == 0 {
						t.Fatalf("Bob's reaction is gone after the cloud's fold: %+v", recs)
					}
					noRecordTwice(t, recs)
				})
			}
		}
	}
}

// Resolve reads the identity and the formers in one step, so a join racing
// it on another goroutine shows in both or in neither: never as a former
// equal to the identity it returns. Several split persons race at once, all
// on the resolver's one lock, so that a Resolve letting go of it between the
// two reads would let a join in there.
func TestWhatsAppIdentityResolveIsOneStepAgainstARacingJoin(t *testing.T) {
	const persons = 4
	ir := newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), nil)
	for i := 0; i < 200; i++ {
		var lids, pns, ids [persons]types.JID
		var formers [persons][]whatsAppFormer
		for p := range persons {
			lids[p] = types.NewJID(fmt.Sprintf("5%06d%05d", i, p), types.HiddenUserServer)
			pns[p] = types.NewJID(fmt.Sprintf("17%06d%03d", i, p), types.DefaultUserServer)
			ir.Canonical(lids[p], types.EmptyJID) // seen by LID alone
			ir.Canonical(pns[p], types.EmptyJID)  // and by number alone: split
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for p := range persons {
			wg.Add(2)
			go func() { defer wg.Done(); <-start; ir.Learn(pns[p], lids[p]) }()
			go func() { defer wg.Done(); <-start; ids[p], formers[p] = ir.Resolve(lids[p], types.EmptyJID, true) }()
		}
		close(start)
		wg.Wait()
		for p := range persons {
			before := ids[p] == lids[p] && len(formers[p]) == 0
			after := ids[p] == pns[p] && len(formers[p]) == 1 && formers[p][0].JID == lids[p] && !formers[p][0].Shared
			if !before && !after {
				t.Fatalf("trial %d: Resolve = %s %+v, neither before the join nor after it", i, ids[p], formers[p])
			}
		}
	}
}
