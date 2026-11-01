package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// A history chunk can carry a person's PN↔LID pair and nothing else about
// them. When one form already has an identity, Learn records the other
// against it at once: a re-link before the person appears again would wipe
// whatsmeow's store and the resolver's memory, and a later message addressed
// by the other form alone would choose a second identity.
func TestWhatsAppIdentityLearnRecordsTheAliasOfAKnownPerson(t *testing.T) {
	store := newMemoryWhatsAppIdentityStore()
	ir := newWhatsAppIdentityResolver(store, nil)
	if got := ir.Canonical(waPeerLID, types.EmptyJID); got != waPeerLID {
		t.Fatal(got)
	}
	ir.Learn(waPeer, waPeerLID) // a later chunk's mapping, no message of theirs in it

	relinked := newWhatsAppIdentityResolver(store, nil) // fresh memory, empty store
	if got := relinked.Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("after a re-link the phone form chose %s, not the recorded %s", got, waPeerLID)
	}

	// A pair for someone never written records nothing: their first record
	// chooses.
	stranger, strangerLID := types.NewJID("15550000005", types.DefaultUserServer), types.NewJID("555444333", types.HiddenUserServer)
	ir.Learn(stranger, strangerLID)
	if _, ok := store.lookupIdentity(stranger.String()); ok {
		t.Fatal("a mapping for a person never written chose an identity")
	}

	// Joined forms (both recorded, different): the phone side wins, as in
	// Canonical.
	lid2, pn2 := types.NewJID("444555666", types.HiddenUserServer), types.NewJID("15550000008", types.DefaultUserServer)
	ir.Canonical(lid2, types.EmptyJID)
	ir.Canonical(pn2, types.EmptyJID)
	ir.Learn(pn2, lid2)
	if id, _ := store.lookupIdentity(lid2.String()); id != pn2.String() {
		t.Fatalf("a learned join left the LID form at %s", id)
	}
}

// Identities are staged and commit inside the transaction of the records that
// use them, so a record is never durable without its identity; what is still
// staged at Close is flushed.
func TestWhatsAppIdentitiesCommitWithTheirRecords(t *testing.T) {
	withWhatsAppTestEnv(t)
	ob, dir := openTestOutbox(t, "lnk_stage")
	ir := newWhatsAppIdentityResolver(ob, nil)
	ir.Canonical(waPeerLID, types.EmptyJID)
	if n, _ := countWhatsAppIdentityRows(ob.db); n != 0 {
		t.Fatalf("an identity was written before any record used it (%d rows)", n)
	}
	if err := ob.Append([]whatsAppRecord{textRecord(waPeerLID.String(), "A", "x")}, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := countWhatsAppIdentityRows(ob.db); n != 1 || len(ob.pendingIdentities) != 0 {
		t.Fatalf("rows=%d pending=%d after the append, want 1 and 0", n, len(ob.pendingIdentities))
	}

	// Staged with no append after it (a learned alias): Close flushes it.
	ir.Learn(waPeer, waPeerLID)
	ob.Close()
	ob, _, err := openWhatsAppOutbox(dir, "lnk_stage")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if got := newWhatsAppIdentityResolver(ob, nil).Canonical(waPeer, types.EmptyJID); got != waPeerLID {
		t.Fatalf("an alias staged before Close was lost: %s", got)
	}
}
