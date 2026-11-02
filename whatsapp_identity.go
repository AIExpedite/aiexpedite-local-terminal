// whatsapp_identity.go — one identity per person, for the life of a link.
//
// The cloud hashes (linkId, chatJid) into a conversation's document id and
// (linkId, chatJid, senderJid, waMessageId) into a message's, so the JID a
// person is written under must never change while the link lives. WhatsApp
// addresses a person by phone number (PN, `…@s.whatsapp.net`) or by LID
// (`…@lid`); which one an event carries, and whether it also carries the
// other, depends on the chat's addressing mode and on what whatsmeow has
// learned so far. Choosing "the phone number when the mapping is known" at
// normalization time split a chat whenever its history copy was normalized
// before the mapping arrived and its live copy after: the chat became two
// conversations and each message two documents. History made that the common
// case, because whatsmeow stores a history chunk's own LID mappings on a
// goroutine of its own, racing the chunk's dispatch to this agent.
//
// So the FIRST identity written for a person stays theirs. Each resolution:
//
//  1. completes the (PN, LID) pair from what the event carries, then the
//     history chunk's own mappings (Learn), then whatsmeow's LID store, both
//     directions. It does so even when the form the event carries is already
//     recorded: the chunk's mappings die with this runner and whatsmeow's
//     store with the session (a re-link wipes session.db), so a join only
//     they know has to reach the table while it is known, or a later message
//     addressed by the other form alone would choose a second identity;
//  2. returns the identity recorded for either form, recording every form it
//     has against it;
//  3. only for a person never seen at all, chooses: the phone number when it
//     is known (an unnamed chat shows the number part of its JID), else the
//     LID.
//
// A person first seen ONLY by LID and, separately, ONLY by phone number before
// anything joined the two still splits: nothing on this computer can know
// they are one person. When a later event joins them, the phone-number form's
// identity wins from then on (the LID form is re-pointed to it), so the split
// stops growing. The identity the re-pointed form had is kept as one of its
// FORMER identities, with when the re-point retired it (Resolve): an edit,
// revoke or reaction aimed at a message written under it goes under each
// former identity as well (whatsapp_normalize.go, whatsAppTargetCopies). A
// delete-for-everyone of a message from before the join therefore still finds
// it, rather than parking in the cloud until the park expires with the text
// still stored.
//
// The table lives in outbox.db (whatsapp_outbox.go), so it is kept exactly as
// long as the link's outbox: across a re-link ("Link again" keeps the outbox,
// whatsapp_supervisor.go), because the cloud keeps the link's earlier
// conversations and a re-link's history sync must land in the same ones. Rows
// are indexed by an HMAC of the JID and sealed under the outbox key, so the
// file holds no phone number in the clear. Nothing here is ever logged.
//
// Choices are STAGED, never written on their own: the in-memory cache answers
// at once, and the staged entries commit inside the transaction of the next
// Append - the one that writes the records using them - so a record is never
// durable without the identity it was written under, and a large history sync
// costs no fsync per new contact. Close flushes what is still staged.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// whatsAppIdentityStore is where chosen identities are recorded: the outbox,
// or a map in tests. Keys and values are JID strings (non-AD).
type whatsAppIdentityStore interface {
	lookupIdentity(jid string) (string, bool)
	// stageIdentities records jid → identity for each entry: readable through
	// lookupIdentity at once, durable with the next records appended.
	stageIdentities(map[string]string)
	// mentionedOutside reports whether a row other than the identity and
	// former rows of the forms in own names id: as its identity, or in its
	// former list.
	mentionedOutside(id string, own []string) bool
}

// whatsAppAlternateMissTTL is how long a store lookup that found nothing is
// trusted. whatsmeow keeps learning mappings (every LID-addressed live
// message carries one), so a miss only holds for now; the TTL keeps a large
// history sync from asking the store again for every message.
var whatsAppAlternateMissTTL = time.Minute

// A form's former identities are recorded in the same table under a key no
// JID string can be: ParseJID splits on every "@", so a JID's String() holds
// exactly one, while this key holds two (and whatsAppPersonForm admits only a
// digits-only user besides). Each entry is "<identity>#<unix ms it was
// retired>", space-separated, newest first, at most whatsAppMaxFormers.
const (
	whatsAppFormerKeyPrefix = "former@"
	whatsAppMaxFormers      = 4
)

func whatsAppFormerKey(form string) string { return whatsAppFormerKeyPrefix + form }

// whatsAppFormer is one former identity of a person (Resolve).
type whatsAppFormer struct {
	JID types.JID
	// RetiredAt is when the re-point retired it, on this computer's clock;
	// the zero time for an entry that does not say.
	RetiredAt time.Time
	// Shared: a row outside the person's own pair names it too, so it may be
	// someone else's identity now (a recycled number). Worked out only when
	// Resolve is asked to.
	Shared bool
}

func whatsAppFormerEntry(id string, retiredAt time.Time) string {
	return id + "#" + strconv.FormatInt(retiredAt.UnixMilli(), 10)
}

// parseWhatsAppFormerEntry splits a former entry into its identity and when
// it was retired. A value without "#" (an identity row's, or an entry that
// does not say) is all identity, retired at the zero time.
func parseWhatsAppFormerEntry(entry string) (string, time.Time) {
	id, ms, ok := strings.Cut(entry, "#")
	if !ok {
		return entry, time.Time{}
	}
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return id, time.Time{}
	}
	return id, time.UnixMilli(n)
}

// whatsAppIdentityResolver implements whatsAppIdentity (whatsapp_normalize.go).
type whatsAppIdentityResolver struct {
	// mu makes "look up, choose, record" one step: live messages and history
	// chunks are normalized on different whatsmeow goroutines, and two first
	// sightings of one person racing each other could otherwise each choose.
	mu    sync.Mutex
	store whatsAppIdentityStore
	// alternate is whatsmeow's LID store: the PN for a LID or the LID for a
	// PN, the empty JID when unknown. Nil: no store.
	alternate func(types.JID) types.JID
	// known holds every mapping this resolver has seen, both directions:
	// history-chunk mappings (Learn), pairs an event carried, and store hits.
	// Consulted before the store, which may not have a chunk's mappings yet.
	known map[types.JID]types.JID
	// missedAt is when a store lookup last found nothing.
	missedAt map[types.JID]time.Time
	now      func() time.Time
}

func newWhatsAppIdentityResolver(store whatsAppIdentityStore, alternate func(types.JID) types.JID) *whatsAppIdentityResolver {
	return &whatsAppIdentityResolver{
		store: store, alternate: alternate,
		known: map[types.JID]types.JID{}, missedAt: map[types.JID]time.Time{}, now: time.Now,
	}
}

// whatsAppPersonForm puts a person's address in canonical form: device part
// dropped, legacy c.us folded into s.whatsapp.net. ok is false for anything
// that is not a person (groups, broadcasts, newsletters, bots), which is
// never resolved.
func whatsAppPersonForm(j types.JID) (types.JID, bool) {
	j = j.ToNonAD()
	switch j.Server {
	case types.DefaultUserServer, types.HiddenUserServer:
	case types.LegacyUserServer:
		j.Server = types.DefaultUserServer
	default:
		return j, false
	}
	// A phone number and a LID are digits. A reaction or revoke key's
	// participant is the sender's own payload, so anything else is refused
	// before it can read (or plant) a row of the identity table.
	return j, whatsAppDigitsOnly(j.User)
}

func whatsAppDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Learn records that pn and lid are one person. The pair is remembered for
// the resolver's lifetime, and when either form already has an identity the
// other form is recorded against it NOW (the join rule applies when both do):
// the chunk that carried the pair may hold nothing else about the person, and
// a re-link before they appear again would wipe whatsmeow's store and this
// memory, leaving a later message addressed by the other form to choose anew.
// A pair for a person never written needs nothing recorded: whatever their
// first record chooses is their first identity.
func (ir *whatsAppIdentityResolver) Learn(pn, lid types.JID) {
	pn, okPN := whatsAppPersonForm(pn)
	lid, okLID := whatsAppPersonForm(lid)
	if !okPN || !okLID || pn.Server != types.DefaultUserServer || lid.Server != types.HiddenUserServer {
		return
	}
	ir.mu.Lock()
	defer ir.mu.Unlock()
	ir.rememberLocked(pn, lid)
	pnID, pnOK := ir.store.lookupIdentity(pn.String())
	lidID, lidOK := ir.store.lookupIdentity(lid.String())
	switch {
	case pnOK && (!lidOK || lidID != pnID):
		updates := map[string]string{lid.String(): pnID}
		if lidOK {
			ir.keepFormerLocked(lid.String(), lidID, pnID, updates)
		}
		ir.store.stageIdentities(updates)
	case lidOK && !pnOK:
		ir.store.stageIdentities(map[string]string{pn.String(): lidID})
	}
}

// rememberLocked keeps a mapping, both directions. Caller holds mu.
func (ir *whatsAppIdentityResolver) rememberLocked(pn, lid types.JID) {
	ir.known[pn], ir.known[lid] = lid, pn
	delete(ir.missedAt, pn)
	delete(ir.missedAt, lid)
}

// keepFormerLocked adds `was`, the identity form is re-pointed away from, to
// the form's former identities in updates, stamped with the re-point's time:
// newest first, never `to` (the identity it is re-pointed to), each identity
// once, at most whatsAppMaxFormers. Entries compare by their identity part.
// Caller holds mu.
func (ir *whatsAppIdentityResolver) keepFormerLocked(form, was, to string, updates map[string]string) {
	formers := []string{whatsAppFormerEntry(was, ir.now())}
	seen := map[string]bool{was: true, to: true}
	if prev, ok := ir.store.lookupIdentity(whatsAppFormerKey(form)); ok {
		for _, entry := range strings.Fields(prev) {
			if len(formers) == whatsAppMaxFormers {
				break
			}
			if id, _ := parseWhatsAppFormerEntry(entry); !seen[id] {
				seen[id] = true
				formers = append(formers, entry)
			}
		}
	}
	updates[whatsAppFormerKey(form)] = strings.Join(formers, " ")
}

// whatsAppPairOf sorts an event's address (already a person's form) and its
// alternate into the phone-number and LID forms; either may stay empty.
func whatsAppPairOf(first, alt types.JID) (pn, lid types.JID) {
	place := func(j types.JID) {
		if j, ok := whatsAppPersonForm(j); ok {
			if j.Server == types.DefaultUserServer && pn.IsEmpty() {
				pn = j
			} else if j.Server == types.HiddenUserServer && lid.IsEmpty() {
				lid = j
			}
		}
	}
	place(first)
	place(alt)
	return pn, lid
}

// completePairLocked fills the form the event did not carry (otherForm); a
// pair the event carried whole is remembered. Caller holds mu.
func (ir *whatsAppIdentityResolver) completePairLocked(pn, lid types.JID) (types.JID, types.JID) {
	switch {
	case !pn.IsEmpty() && !lid.IsEmpty():
		ir.rememberLocked(pn, lid) // the event's own pair is authoritative
	case pn.IsEmpty():
		pn = ir.otherForm(lid)
	default:
		lid = ir.otherForm(pn)
	}
	return pn, lid
}

// otherForm completes a pair: known mappings first, then the store (a miss
// is trusted for whatsAppAlternateMissTTL). Caller holds mu.
func (ir *whatsAppIdentityResolver) otherForm(j types.JID) types.JID {
	want := types.HiddenUserServer
	if j.Server == types.HiddenUserServer {
		want = types.DefaultUserServer
	}
	if o, ok := ir.known[j]; ok {
		return o
	}
	if ir.alternate == nil {
		return types.EmptyJID
	}
	if at, ok := ir.missedAt[j]; ok && ir.now().Sub(at) < whatsAppAlternateMissTTL {
		return types.EmptyJID
	}
	if o, ok := whatsAppPersonForm(ir.alternate(j)); ok && o.Server == want {
		if want == types.DefaultUserServer {
			ir.rememberLocked(o, j)
		} else {
			ir.rememberLocked(j, o)
		}
		return o
	}
	ir.missedAt[j] = ir.now()
	return types.EmptyJID
}

// Canonical returns the identity this link writes the person under. primary
// is the address the event used; alt is its alternate (SenderAlt /
// RecipientAlt), empty when the event carries none.
func (ir *whatsAppIdentityResolver) Canonical(primary, alt types.JID) types.JID {
	id, _ := ir.resolve(primary, alt, false, false)
	return id
}

// Resolve is Canonical together with the identities the person was written
// under before a join re-pointed one of their forms (newest first, never the
// identity it returns), read in ONE critical section. Read under two locks,
// a join landing between them (a history chunk's Learn runs on whatsmeow's
// history goroutine) reported the identity just returned as a former, and a
// reaction written under it was taken off by its own removal. With shared,
// each former says whether a row outside the person's own pair names it too.
// No formers for a person no join ever re-pointed, or for anything that is
// not a person.
func (ir *whatsAppIdentityResolver) Resolve(primary, alt types.JID, shared bool) (types.JID, []whatsAppFormer) {
	return ir.resolve(primary, alt, true, shared)
}

// Formers is Resolve's former identities as plain JIDs, Shared not worked
// out: the tests' view of them. Like Canonical, it records what it resolves.
func (ir *whatsAppIdentityResolver) Formers(primary, alt types.JID) []types.JID {
	_, formers := ir.resolve(primary, alt, true, false)
	var out []types.JID
	for _, f := range formers {
		out = append(out, f.JID)
	}
	return out
}

// resolve is Canonical and, with formers, Resolve: one critical section.
func (ir *whatsAppIdentityResolver) resolve(primary, alt types.JID, formers, shared bool) (types.JID, []whatsAppFormer) {
	first, ok := whatsAppPersonForm(primary)
	if !ok {
		return first, nil
	}
	pn, lid := whatsAppPairOf(first, alt)

	ir.mu.Lock()
	defer ir.mu.Unlock()
	pn, lid = ir.completePairLocked(pn, lid)
	lookup := func(j types.JID) (string, bool) {
		if j.IsEmpty() {
			return "", false
		}
		return ir.store.lookupIdentity(j.String())
	}
	pnID, pnOK := lookup(pn)
	lidID, lidOK := lookup(lid)
	var id string
	switch {
	case pnOK:
		id = pnID
	case lidOK:
		id = lidID
	case !pn.IsEmpty():
		id = pn.String()
	default:
		id = lid.String()
	}
	updates := map[string]string{}
	if !pn.IsEmpty() && (!pnOK || pnID != id) {
		updates[pn.String()] = id
		if pnOK {
			ir.keepFormerLocked(pn.String(), pnID, id, updates)
		}
	}
	if !lid.IsEmpty() && (!lidOK || lidID != id) {
		updates[lid.String()] = id
		if lidOK {
			ir.keepFormerLocked(lid.String(), lidID, id, updates)
		}
	}
	if len(updates) > 0 {
		ir.store.stageIdentities(updates)
	}
	out, err := types.ParseJID(id)
	if err != nil {
		return first, nil
	}
	if !formers {
		return out, nil
	}
	return out, ir.formersLocked(pn, lid, id, shared)
}

// formersLocked reads the former identities of a completed pair, never id
// (the identity the pair resolves to now), each once. With shared, a former
// that a row outside the pair's own (its forms' identity and former rows)
// names too is marked Shared. Caller holds mu.
func (ir *whatsAppIdentityResolver) formersLocked(pn, lid types.JID, id string, shared bool) []whatsAppFormer {
	var own []string
	for _, form := range []types.JID{lid, pn} {
		if !form.IsEmpty() {
			own = append(own, form.String())
		}
	}
	seen := map[string]bool{id: true}
	var out []whatsAppFormer
	for _, form := range own {
		list, _ := ir.store.lookupIdentity(whatsAppFormerKey(form))
		for _, entry := range strings.Fields(list) {
			former, retiredAt := parseWhatsAppFormerEntry(entry)
			j, err := types.ParseJID(former)
			if seen[former] || err != nil {
				continue
			}
			seen[former] = true
			f := whatsAppFormer{JID: j, RetiredAt: retiredAt}
			if shared {
				f.Shared = ir.store.mentionedOutside(former, own)
			}
			out = append(out, f)
		}
	}
	return out
}

/* --------------------------------------------------------------------------
   The table in outbox.db
   -------------------------------------------------------------------------- */

// whatsAppIdentityIndexLabel derives the index key from the outbox key, so
// one secret in the credential store covers both.
const whatsAppIdentityIndexLabel = "aiexpedite-whatsapp-identity-index-v1"

func deriveWhatsAppIdentityIndexKey(outboxKey []byte) []byte {
	m := hmac.New(sha256.New, outboxKey)
	m.Write([]byte(whatsAppIdentityIndexLabel))
	return m.Sum(nil)
}

func (ob *whatsAppOutbox) identityIndex(jid string) []byte {
	m := hmac.New(sha256.New, ob.idxKey)
	m.Write([]byte(jid))
	return m.Sum(nil)
}

// identityAAD binds a sealed identity to its link and its index, so a row
// copied to another link or another index does not open.
func (ob *whatsAppOutbox) identityAAD(index []byte) []byte {
	return append([]byte(ob.linkID+":identity:"), index...)
}

// loadIdentities reads the whole table into memory (it holds a few entries
// per contact). A row that does not open is skipped: it cannot name anyone.
func (ob *whatsAppOutbox) loadIdentities() error {
	rows, err := ob.db.Query(`SELECT k, nonce, payload FROM identity`)
	if err != nil {
		return err
	}
	defer rows.Close()
	ob.identities = map[string]string{}
	for rows.Next() {
		var k, nonce, payload []byte
		if err := rows.Scan(&k, &nonce, &payload); err != nil {
			return err
		}
		plain, err := ob.aead.Open(nil, nonce, payload, ob.identityAAD(k))
		if err != nil {
			continue
		}
		ob.identities[hex.EncodeToString(k)] = string(plain)
	}
	return rows.Err()
}

func (ob *whatsAppOutbox) lookupIdentity(jid string) (string, bool) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	id, ok := ob.identities[hex.EncodeToString(ob.identityIndex(jid))]
	return id, ok
}

// stageIdentities records jid → identity in the cache at once and stages the
// write: the next Append commits it with its records (whatsapp_outbox.go), and
// Close flushes what is left.
func (ob *whatsAppOutbox) stageIdentities(entries map[string]string) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.pendingIdentities == nil {
		ob.pendingIdentities = map[string]string{}
	}
	for jid, id := range entries {
		k := hex.EncodeToString(ob.identityIndex(jid))
		ob.identities[k] = id
		ob.pendingIdentities[k] = id
	}
}

// mentionedOutside walks the cached table (pending rows included): a former
// is only Shared-checked for a reaction by a re-pointed reactor, so the walk
// is rare.
func (ob *whatsAppOutbox) mentionedOutside(id string, own []string) bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	skip := map[string]bool{}
	for _, form := range own {
		skip[hex.EncodeToString(ob.identityIndex(form))] = true
		skip[hex.EncodeToString(ob.identityIndex(whatsAppFormerKey(form)))] = true
	}
	return whatsAppRowsName(ob.identities, skip, id)
}

// whatsAppRowsName reports whether a row of rows other than those in skip
// names id: an identity row's value is one identity, a former row's a list of
// entries, and either compares by the identity part.
func whatsAppRowsName(rows map[string]string, skip map[string]bool, id string) bool {
	for k, v := range rows {
		if skip[k] {
			continue
		}
		for _, entry := range strings.Fields(v) {
			if named, _ := parseWhatsAppFormerEntry(entry); named == id {
				return true
			}
		}
	}
	return false
}

// writePendingIdentitiesLocked seals every pending identity into tx. The
// caller holds mu, commits, and clears the pending set only on a commit.
func (ob *whatsAppOutbox) writePendingIdentitiesLocked(tx *sql.Tx) error {
	for kHex, id := range ob.pendingIdentities {
		k, err := hex.DecodeString(kHex)
		if err != nil {
			return err
		}
		nonce := make([]byte, ob.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		sealed := ob.aead.Seal(nil, nonce, []byte(id), ob.identityAAD(k))
		if _, err := tx.Exec(`INSERT INTO identity (k, nonce, payload) VALUES (?, ?, ?)
			ON CONFLICT(k) DO UPDATE SET nonce = excluded.nonce, payload = excluded.payload`, k, nonce, sealed); err != nil {
			return err
		}
	}
	return nil
}

// countIdentityRows is used at open, before the key is known.
func countWhatsAppIdentityRows(db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRow(`SELECT COUNT(*) FROM identity`).Scan(&n)
	return n, err
}

// memoryWhatsAppIdentityStore is the test store.
type memoryWhatsAppIdentityStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemoryWhatsAppIdentityStore() *memoryWhatsAppIdentityStore {
	return &memoryWhatsAppIdentityStore{m: map[string]string{}}
}

func (s *memoryWhatsAppIdentityStore) lookupIdentity(jid string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.m[jid]
	return id, ok
}

func (s *memoryWhatsAppIdentityStore) stageIdentities(entries map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range entries {
		s.m[k] = v
	}
}

func (s *memoryWhatsAppIdentityStore) mentionedOutside(id string, own []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	skip := map[string]bool{}
	for _, form := range own {
		skip[form], skip[whatsAppFormerKey(form)] = true, true
	}
	return whatsAppRowsName(s.m, skip, id)
}
