package main

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var waPeerLID = types.NewJID("999888777", types.HiddenUserServer)

// learningAlternates is whatsmeow's LID store as it fills up over a session.
type learningAlternates struct {
	mu sync.Mutex
	m  map[types.JID]types.JID
}

func (a *learningAlternates) learn(pn, lid types.JID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[types.JID]types.JID{}
	}
	a.m[pn], a.m[lid] = lid, pn
}

func (a *learningAlternates) lookup(j types.JID) types.JID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.m[j]
}

// historyChunk is one direct chat with the peer, keyed as WhatsApp keyed it.
func historyChunk(chat types.JID, id string) *waHistorySync.HistorySync {
	web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String(id)}, MessageTimestamp: proto.Uint64(1_800_000_000), Message: &waE2E.Message{Conversation: proto.String("old")}}
	return &waHistorySync.HistorySync{
		SyncType:      waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: web}}}},
	}
}

// parseLikeWhatsmeow is ParseWebMessage for a direct chat: the chat is the
// sender, and history messages carry NO alternate address.
func parseLikeWhatsmeow(chat types.JID, w *waWeb.WebMessageInfo) (*events.Message, error) {
	return liveMsg(chat, chat, false, w.GetKey().GetID(), w.GetMessage()), nil
}

// The must-fix gap: a LID-keyed history copy normalized before the LID →
// phone mapping is known, and its live copy after. Both must land under one
// identity, or the chat splits into two conversations and the message into
// two documents.
func TestWhatsAppIdentityHistoryBeforeTheMappingDoesNotSplitTheChat(t *testing.T) {
	alt := &learningAlternates{}
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), alt.lookup)}

	hist, _ := normalizeWhatsAppHistory(historyChunk(waPeerLID, "M1"), parseLikeWhatsmeow, nc)
	if len(hist) != 2 {
		t.Fatalf("history records = %+v", hist)
	}

	// Later the mapping is known and the live copy carries the phone number.
	alt.learn(waPeer, waPeerLID)
	evt := liveMsg(waPeerLID, waPeerLID, false, "M1", &waE2E.Message{Conversation: proto.String("old")})
	evt.Info.SenderAlt = waPeer
	live := one(t, normalizeWhatsAppMessage(evt, waSourceLive, nc))
	if live.ChatJid != hist[0].ChatJid || live.ChatJid != hist[1].ChatJid || live.SenderJid != hist[1].SenderJid {
		t.Fatalf("split: history chat=%s sender=%s, live chat=%s sender=%s", hist[1].ChatJid, hist[1].SenderJid, live.ChatJid, live.SenderJid)
	}

	// A message addressed by phone number alone joins the same chat too.
	pnOnly := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waPeer, false, "M2", &waE2E.Message{Conversation: proto.String("new")}), waSourceLive, nc))
	if pnOnly.ChatJid != hist[0].ChatJid {
		t.Fatalf("phone-addressed message landed in %s, not %s", pnOnly.ChatJid, hist[0].ChatJid)
	}

	// An edit, a revoke and a reaction naming the author by phone number
	// target the same message document.
	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("M1"), Participant: proto.String(waPeer.String())}}}
	r := one(t, normalizeWhatsAppMessage(liveMsg(waPeer, waMe, true, "X", revoke), waSourceLive, nc))
	if r.ChatJid != hist[1].ChatJid || r.SenderJid != hist[1].SenderJid {
		t.Fatalf("revoke targets chat=%s sender=%s", r.ChatJid, r.SenderJid)
	}
}

// whatsmeow stores a history chunk's mappings on its own goroutine, racing
// the chunk's dispatch: the chunk's mappings are applied first, so its
// messages already resolve to the phone number — the same identity the live
// copies carry.
func TestWhatsAppIdentityUsesTheHistoryChunksOwnMappings(t *testing.T) {
	nc := whatsAppNormalizeContext{OwnJID: waMe, Identity: newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), nil)}
	chunk := historyChunk(waPeerLID, "M1")
	chunk.PhoneNumberToLidMappings = []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String(waPeer.String()), LidJID: proto.String(waPeerLID.String())}}
	hist, _ := normalizeWhatsAppHistory(chunk, parseLikeWhatsmeow, nc)
	if len(hist) != 2 || hist[0].ChatJid != waPeer.String() || hist[1].ChatJid != waPeer.String() || hist[1].SenderJid != waPeer.String() {
		t.Fatalf("history records = %+v", hist)
	}

	// The conversation's own pn / lid fields count as a mapping too.
	other := types.NewJID("111222333", types.HiddenUserServer)
	otherPN := types.NewJID("15550000009", types.DefaultUserServer)
	chunk = historyChunk(other, "M9")
	chunk.Conversations[0].PnJID = proto.String(otherPN.String())
	chunk.Conversations[0].LidJID = proto.String(other.String())
	hist, _ = normalizeWhatsAppHistory(chunk, parseLikeWhatsmeow, nc)
	if len(hist) != 2 || hist[0].ChatJid != otherPN.String() || hist[1].ChatJid != otherPN.String() {
		t.Fatalf("conversation pn/lid fields ignored: %+v", hist)
	}

	// The usual shape: a LID-keyed conversation carries only pnJID (its id is
	// the LID), and a phone-keyed one only lidJID.
	third := types.NewJID("777666555", types.HiddenUserServer)
	thirdPN := types.NewJID("15550000007", types.DefaultUserServer)
	chunk = historyChunk(third, "M7")
	chunk.Conversations[0].PnJID = proto.String(thirdPN.String())
	hist, _ = normalizeWhatsAppHistory(chunk, parseLikeWhatsmeow, nc)
	if len(hist) != 2 || hist[0].ChatJid != thirdPN.String() || hist[1].ChatJid != thirdPN.String() {
		t.Fatalf("a LID-keyed conversation's pnJID alone was ignored: %+v", hist)
	}
	fourthPN := types.NewJID("15550000006", types.DefaultUserServer)
	fourth := types.NewJID("333222111", types.HiddenUserServer)
	chunk = historyChunk(fourthPN, "M6")
	chunk.Conversations[0].LidJID = proto.String(fourth.String())
	normalizeWhatsAppHistory(chunk, parseLikeWhatsmeow, nc)
	if got := nc.Identity.Canonical(fourth, types.EmptyJID); got != fourthPN {
		t.Fatalf("a phone-keyed conversation's lidJID alone was not learned: %s", got)
	}
}

// A join known only to temporary sources (a chunk's mappings, whatsmeow's
// store) is written to the table while it is known, even when the form the
// event carries is already recorded: a re-link wipes the session store, and
// a later message addressed by the other form alone must still land in the
// same chat.
func TestWhatsAppIdentityPersistsAJoinWhileItIsKnown(t *testing.T) {
	store := newMemoryWhatsAppIdentityStore()
	alt := &learningAlternates{}
	ir := newWhatsAppIdentityResolver(store, alt.lookup)
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatal(got)
	}
	alt.learn(waPeer, waPeerLID) // whatsmeow learns the mapping later
	ir.missedAt = map[types.JID]time.Time{}
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatalf("the recorded identity moved: %s", got)
	}

	// "Link again": a new session, a new resolver over the SAME table, and a
	// session store that does not know the mapping (yet).
	relinked := newWhatsAppIdentityResolver(store, (&learningAlternates{}).lookup)
	if got := relinked.Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("after a re-link the phone form chose %s, not the recorded %s", got, waPeerLID)
	}
}

// A store miss is trusted for a while, so a large history sync does not ask
// the store again for every message; it is asked again once the TTL passes.
func TestWhatsAppIdentityStoreMissesAreTrustedBriefly(t *testing.T) {
	var asked int
	ir := newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), func(types.JID) types.JID { asked++; return types.EmptyJID })
	now := time.Unix(1_800_000_000, 0)
	ir.now = func() time.Time { return now }
	for i := 0; i < 50; i++ {
		ir.Canonical(waPeerLID, types.EmptyJID)
	}
	if asked != 1 {
		t.Fatalf("store asked %d times within the miss TTL", asked)
	}
	now = now.Add(whatsAppAlternateMissTTL + time.Second)
	ir.Canonical(waPeerLID, types.EmptyJID)
	if asked != 2 {
		t.Fatalf("store not asked again after the miss TTL (asked %d)", asked)
	}
}

// Close waits for a write in flight and refuses later ones: a re-link opens
// the same file right after the old session's stores close, and a commit
// landing on the old handle after the new one read `next` would hand out the
// same sequences twice.
func TestWhatsAppOutboxCloseWaitsForTheWriteInFlight(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_close")
	ob.mu.Lock() // a write in flight holds mu for its whole transaction
	closed := make(chan struct{})
	go func() { ob.Close(); close(closed) }()
	select {
	case <-closed:
		ob.mu.Unlock()
		t.Fatal("Close returned while a write held the outbox")
	case <-time.After(100 * time.Millisecond):
	}
	ob.mu.Unlock()
	<-closed
	if err := ob.Append([]whatsAppRecord{textRecord(waPeer.String(), "A", "x")}, false); !errors.Is(err, errWhatsAppOutboxClosed) {
		t.Fatalf("an append after Close = %v", err)
	}
	if err := ob.EnsureNextAbove(10); !errors.Is(err, errWhatsAppOutboxClosed) {
		t.Fatalf("EnsureNextAbove after Close = %v", err)
	}
}

// Every commit is durable before it returns (synchronous FULL): the cloud
// acts on what the outbox reports, so a power loss must not roll it back.
func TestWhatsAppOutboxCommitsAreDurable(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_sync")
	defer ob.Close()
	var level int
	if err := ob.db.QueryRow(`PRAGMA synchronous`).Scan(&level); err != nil || level != 2 {
		t.Fatalf("synchronous = %d (%v), want 2 (FULL)", level, err)
	}
}

// Recorded identities are sticky; when a later event proves a LID-only and a
// phone-only person are one, the phone-number side wins from then on.
func TestWhatsAppIdentityStickyAndJoinedForms(t *testing.T) {
	store := newMemoryWhatsAppIdentityStore()
	ir := newWhatsAppIdentityResolver(store, nil)
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatalf("an unknown LID is its own identity, got %s", got)
	}
	if got := ir.Canonical(waPeerLID, waPeer); got != waPeerLID {
		t.Fatalf("a recorded identity must not move when the phone number appears, got %s", got)
	}
	if got := ir.Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("the phone form was recorded against the first identity, got %s", got)
	}

	// Seen separately before anything joined them: two identities.
	lid2 := types.NewJID("444555666", types.HiddenUserServer)
	pn2 := types.NewJID("15550000008", types.DefaultUserServer)
	ir.Canonical(lid2, types.EmptyJID)
	ir.Canonical(pn2, types.EmptyJID)
	if got := ir.Canonical(lid2, pn2); got != pn2 {
		t.Fatalf("joined forms resolve to the phone side, got %s", got)
	}
	if got := ir.Canonical(lid2, types.EmptyJID); got != pn2 {
		t.Fatalf("the LID form follows the join, got %s", got)
	}

	// Device parts, legacy c.us and non-persons.
	if got := ir.Canonical(types.JID{User: waPeerLID.User, Device: 7, Server: types.HiddenUserServer}, types.EmptyJID); got != waPeerLID {
		t.Fatalf("device part kept: %s", got)
	}
	if got := ir.Canonical(types.NewJID(pn2.User, types.LegacyUserServer), types.EmptyJID); got != pn2 {
		t.Fatalf("legacy c.us not folded: %s", got)
	}
	if got := ir.Canonical(waGroup, waPeer); got != waGroup {
		t.Fatalf("a group is not a person: %s", got)
	}
}

// Concurrent first sightings (live and history goroutines) choose once.
func TestWhatsAppIdentityConcurrentFirstSightingsAgree(t *testing.T) {
	for i := 0; i < 50; i++ {
		alt := &learningAlternates{}
		alt.learn(waPeer, waPeerLID)
		ir := newWhatsAppIdentityResolver(newMemoryWhatsAppIdentityStore(), alt.lookup)
		var wg sync.WaitGroup
		got := make([]types.JID, 2)
		wg.Add(2)
		go func() { defer wg.Done(); got[0] = ir.Canonical(waPeerLID, types.EmptyJID) }()
		go func() { defer wg.Done(); got[1] = ir.Canonical(waPeer, types.EmptyJID) }()
		wg.Wait()
		if got[0] != got[1] {
			t.Fatalf("one person, two identities: %s / %s", got[0], got[1])
		}
	}
}

// The table is sealed in outbox.db: it survives a reopen (a restart, a
// re-link) and holds no phone number in the clear.
func TestWhatsAppIdentityTableIsSealedAndSurvivesReopen(t *testing.T) {
	ks, _ := withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_id")
	ir := newWhatsAppIdentityResolver(ob, nil)
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatal(got)
	}
	ir.Canonical(waPeerLID, waPeer)
	ob.Close()

	raw, err := os.ReadFile(filepath.Join(dir, whatsAppOutboxDBName))
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(filepath.Join(dir, whatsAppOutboxDBName+"-wal"))
	for _, leak := range []string{waPeer.User, waPeerLID.User} {
		if bytes.Contains(raw, []byte(leak)) || bytes.Contains(wal, []byte(leak)) {
			t.Fatalf("outbox.db holds %q in the clear", leak)
		}
	}

	ob, _, err = openWhatsAppOutbox(dir, "lnk_id")
	if err != nil {
		t.Fatal(err)
	}
	ir = newWhatsAppIdentityResolver(ob, nil)
	if got := ir.Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("after reopen the phone form resolves to %s, want %s", got, waPeerLID)
	}
	ob.Close()

	// A row copied to another index does not open.
	db, err := sql.Open("sqlite", filepath.Join(dir, whatsAppOutboxDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE identity SET k = X'00' || k`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ob, _, err = openWhatsAppOutbox(dir, "lnk_id")
	if err != nil {
		t.Fatal(err)
	}
	if len(ob.identities) != 0 {
		t.Fatalf("re-indexed rows opened: %d", len(ob.identities))
	}
	ob.Close()

	// Without the key the table is unreadable: discarded with the rows, and
	// the sequence survives.
	ob, _, _ = openWhatsAppOutbox(dir, "lnk_id")
	if err := ob.Append([]whatsAppRecord{textRecord(waPeer.String(), "A", "x")}, false); err != nil {
		t.Fatal(err)
	}
	ob.stageIdentities(map[string]string{waPeer.String(): waPeer.String()})
	next := ob.next
	ob.Close()
	_ = ks.Delete("lnk_id")
	ob, discarded, err := openWhatsAppOutbox(dir, "lnk_id")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if n, _ := countWhatsAppIdentityRows(ob.db); !discarded || n != 0 || ob.Depth() != 0 {
		t.Fatalf("discarded=%v identity rows=%d depth=%d", discarded, n, ob.Depth())
	}
	if ob.next != next {
		t.Fatalf("the sequence restarted at %d after a lost key (was %d)", ob.next, next)
	}
}

// The cloud acks anything at or below its watermark as a replay without
// writing it, so a link start raises the sequence above it — and never
// lowers it.
func TestWhatsAppOutboxEnsureNextAbove(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_seq")
	if err := ob.EnsureNextAbove(500); err != nil {
		t.Fatal(err)
	}
	recs := []whatsAppRecord{textRecord(waPeer.String(), "A", "x")}
	if err := ob.Append(recs, false); err != nil || recs[0].DeviceSeq != 501 {
		t.Fatalf("seq = %d, %v", recs[0].DeviceSeq, err)
	}
	if err := ob.EnsureNextAbove(10); err != nil || ob.next != 502 {
		t.Fatalf("a lower watermark moved the sequence to %d (%v)", ob.next, err)
	}
	ob.Close()
	ob, _, err := openWhatsAppOutbox(dir, "lnk_seq")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if ob.next != 502 {
		t.Fatalf("the raised sequence did not persist: %d", ob.next)
	}
}

// An identity whose table write failed is not treated as recorded: it stays
// pending, and commits with the next identity write or, at the latest, with
// the next records appended - so a restart or re-link never re-chooses an
// identity records were already written under.
func TestWhatsAppIdentityFailedWritesAreRetried(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_retry")
	// The disk refuses identity writes for a while.
	if _, err := ob.db.Exec(`ALTER TABLE identity RENAME TO identity_away`); err != nil {
		t.Fatal(err)
	}
	ir := newWhatsAppIdentityResolver(ob, nil)
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatal(got)
	}
	if len(ob.pendingIdentities) == 0 {
		t.Fatal("a failed identity write was not kept pending")
	}
	if _, err := ob.db.Exec(`ALTER TABLE identity_away RENAME TO identity`); err != nil {
		t.Fatal(err)
	}
	// The record that uses the identity carries it into the table.
	if err := ob.Append([]whatsAppRecord{textRecord(waPeerLID.String(), "A", "x")}, false); err != nil {
		t.Fatal(err)
	}
	if len(ob.pendingIdentities) != 0 {
		t.Fatal("pending identities survived a committed append")
	}
	ob.Close()

	// After a restart / re-link, the phone form (now known) must not
	// re-choose: the LID identity is in the table.
	ob, _, err := openWhatsAppOutbox(dir, "lnk_retry")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	alt := &learningAlternates{}
	alt.learn(waPeer, waPeerLID)
	if got := newWhatsAppIdentityResolver(ob, alt.lookup).Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("after a reopen the phone form chose %s, not the recorded %s", got, waPeerLID)
	}
}

// A table that keeps refusing never costs the records: the append commits
// them and the identities stay pending for a later write.
func TestWhatsAppIdentityRefusalNeverCostsTheRecords(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, _ := openTestOutbox(t, "lnk_refuse")
	defer ob.Close()
	if _, err := ob.db.Exec(`ALTER TABLE identity RENAME TO identity_away`); err != nil {
		t.Fatal(err)
	}
	newWhatsAppIdentityResolver(ob, nil).Canonical(waPeerLID, types.EmptyJID)
	if err := ob.Append([]whatsAppRecord{textRecord(waPeerLID.String(), "A", "x")}, false); err != nil {
		t.Fatalf("an identity refusal cost the record: %v", err)
	}
	if ob.Depth() != 1 || len(ob.pendingIdentities) == 0 {
		t.Fatalf("depth=%d pending=%d, want the record stored and the identity still pending", ob.Depth(), len(ob.pendingIdentities))
	}
}
