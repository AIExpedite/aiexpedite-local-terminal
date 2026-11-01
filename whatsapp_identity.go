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
// stops growing.
//
// The table lives in outbox.db (whatsapp_outbox.go), so it is kept exactly as
// long as the link's outbox: across a re-link ("Link again" keeps the outbox,
// whatsapp_supervisor.go), because the cloud keeps the link's earlier
// conversations and a re-link's history sync must land in the same ones. Rows
// are indexed by an HMAC of the JID and sealed under the outbox key, so the
// file holds no phone number in the clear. Nothing here is ever logged.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// whatsAppIdentityStore is where chosen identities are recorded: the outbox,
// or a map in tests. Keys and values are JID strings (non-AD).
type whatsAppIdentityStore interface {
	lookupIdentity(jid string) (string, bool)
	// recordIdentities records jid → identity for each entry. The entries
	// must stay readable through lookupIdentity even when the write fails,
	// so this session keeps writing what it already chose.
	recordIdentities(map[string]string) error
}

// whatsAppAlternateMissTTL is how long a store lookup that found nothing is
// trusted. whatsmeow keeps learning mappings (every LID-addressed live
// message carries one), so a miss only holds for now; the TTL keeps a large
// history sync from asking the store again for every message.
var whatsAppAlternateMissTTL = time.Minute

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
	// onWriteError is told (without the JIDs) when a choice was not stored.
	onWriteError func(error)
}

func newWhatsAppIdentityResolver(store whatsAppIdentityStore, alternate func(types.JID) types.JID, onWriteError func(error)) *whatsAppIdentityResolver {
	return &whatsAppIdentityResolver{
		store: store, alternate: alternate, onWriteError: onWriteError,
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
		return j, j.User != ""
	case types.LegacyUserServer:
		j.Server = types.DefaultUserServer
		return j, j.User != ""
	}
	return j, false
}

// Learn records that pn and lid are one person, for this resolver's lifetime.
func (ir *whatsAppIdentityResolver) Learn(pn, lid types.JID) {
	pn, okPN := whatsAppPersonForm(pn)
	lid, okLID := whatsAppPersonForm(lid)
	if !okPN || !okLID || pn.Server != types.DefaultUserServer || lid.Server != types.HiddenUserServer {
		return
	}
	ir.mu.Lock()
	ir.rememberLocked(pn, lid)
	ir.mu.Unlock()
}

// rememberLocked keeps a mapping, both directions. Caller holds mu.
func (ir *whatsAppIdentityResolver) rememberLocked(pn, lid types.JID) {
	ir.known[pn], ir.known[lid] = lid, pn
	delete(ir.missedAt, pn)
	delete(ir.missedAt, lid)
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
	first, ok := whatsAppPersonForm(primary)
	if !ok {
		return first
	}
	var pn, lid types.JID
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

	ir.mu.Lock()
	defer ir.mu.Unlock()
	switch {
	case !pn.IsEmpty() && !lid.IsEmpty():
		ir.rememberLocked(pn, lid) // the event's own pair is authoritative
	case pn.IsEmpty():
		pn = ir.otherForm(lid)
	default:
		lid = ir.otherForm(pn)
	}
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
	}
	if !lid.IsEmpty() && (!lidOK || lidID != id) {
		updates[lid.String()] = id
	}
	if len(updates) > 0 {
		if err := ir.store.recordIdentities(updates); err != nil && ir.onWriteError != nil {
			ir.onWriteError(err)
		}
	}
	out, err := types.ParseJID(id)
	if err != nil {
		return first
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

// recordIdentities records jid → identity. The cache answers at once, so this
// session keeps writing what it chose even while the disk refuses; an entry
// whose write failed stays PENDING and is retried by the next identity write,
// and written inside the next Append's transaction at the latest, so no record
// is ever committed under an identity the table does not hold (a restart or a
// re-link would otherwise re-choose it).
func (ob *whatsAppOutbox) recordIdentities(entries map[string]string) error {
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
	if ob.closed {
		return errWhatsAppOutboxClosed
	}
	tx, err := ob.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ob.writePendingIdentitiesLocked(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	clear(ob.pendingIdentities)
	return nil
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

func (s *memoryWhatsAppIdentityStore) recordIdentities(entries map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range entries {
		s.m[k] = v
	}
	return nil
}
