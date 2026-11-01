// whatsapp_outbox.go — the encrypted, durable queue between whatsmeow and the
// cloud.
//
// Every normalized record (whatsapp_normalize.go) is appended here BEFORE it is
// uploaded, under a monotonic per-link sequence that is also the record's
// `deviceSeq`. The uploader sends contiguous ranges (≤ 200 records, ≤ the batch
// byte budget) through POST /device/:agentId/whatsapp/sync and deletes rows only
// up to the `ackedSeq` the cloud answers — so a crash, a restart or an outage
// of any length resends exactly what was not durably written, and the cloud's
// idempotent ingest collapses any resend.
//
// Rows are AES-256-GCM sealed under the link's key from the OS credential store
// (whatsapp_keystore.go); the AAD binds each row to its link and sequence, so a
// row copied between links or reordered fails to open. The same file holds the
// link's identity table (whatsapp_identity.go), indexed by HMAC and sealed
// under the same key. The database file therefore holds only ciphertext —
// message text and phone numbers never sit on disk in the clear outside
// whatsmeow's own store.
//
// Caps (WHATSAPP_INGEST_LIMITS OUTBOX_MAX_RECORDS / OUTBOX_MAX_BYTES): past
// them a HISTORY append is refused with errWhatsAppOutboxFull — the supervisor
// then pauses consuming history-sync chunks rather than dropping messages —
// while LIVE records are always accepted.
package main

import (
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	_ "modernc.org/sqlite"
)

// Caps are vars so tests can reach them without 50 000 rows.
var (
	whatsAppOutboxMaxRecords int64 = 50000
	whatsAppOutboxMaxBytes   int64 = 200 * 1024 * 1024
)

const (
	whatsAppBatchMaxRecords    = 200
	whatsAppBatchMaxBodyBytes  = 512 * 1024
	whatsAppBatchRecordsBudget = 480 * 1024 // leaves room for the envelope
)

var errWhatsAppOutboxFull = errors.New("whatsapp outbox is full")

// errWhatsAppOutboxClosed answers a write after Close.
var errWhatsAppOutboxClosed = errors.New("whatsapp outbox is closed")

// errWhatsAppOutboxChunkTooLarge is the refusal of a history chunk the EMPTY
// queue cannot take: the chunk alone passes a cap, so no drain will ever
// admit it whole. It wraps errWhatsAppOutboxFull. Append decides it under
// the outbox lock, so an ack racing the refusal cannot make a refusal at the
// cap read as this one.
var errWhatsAppOutboxChunkTooLarge = fmt.Errorf("%w: the chunk alone passes the caps", errWhatsAppOutboxFull)

// whatsAppOutbox is one link's queue, and its identity table
// (whatsapp_identity.go). Both belong to the LINK, not to a generation: a
// re-link keeps them (whatsapp_supervisor.go, linkStart).
type whatsAppOutbox struct {
	mu     sync.Mutex
	db     *sql.DB
	linkID string
	aead   cipher.AEAD
	// cached depth, so a cap check is not a COUNT(*) per message
	count int64
	bytes int64
	next  int64
	// idxKey indexes the identity table; identities caches it (index hex →
	// identity JID).
	idxKey     []byte
	identities map[string]string
	// pendingIdentities are staged identities not yet committed (index hex →
	// identity); see stageIdentities.
	pendingIdentities map[string]string
	// closed: Close ran (under mu, so it waited for any write in flight).
	closed bool
}

// whatsAppOutboxBatch is a contiguous range ready to upload.
type whatsAppOutboxBatch struct {
	SeqFrom int64
	SeqTo   int64
	Records []json.RawMessage
}

// openWhatsAppOutbox opens (creating) dir/outbox.db with the link's key. An
// outbox or identity table whose key is gone cannot be read, so its rows are
// discarded and a fresh key is minted — reported by discarded=true so the
// caller can log it. The sequence (`next`) survives that discard: the cloud's
// replay watermark does too.
func openWhatsAppOutbox(dir, linkID string) (ob *whatsAppOutbox, discarded bool, err error) {
	path := filepath.Join(dir, whatsAppOutboxDBName)
	// synchronous(FULL): a commit is on disk before Append, Ack or an
	// identity write returns. Under NORMAL an OS crash or power loss can roll
	// back the last commits, and with them `next` and identities the cloud
	// has already acted on: records re-numbered at or below its replay
	// watermark are acked without being written, and a person could be
	// written under a second identity.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, false, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS outbox (
		seq INTEGER PRIMARY KEY,
		history INTEGER NOT NULL,
		size INTEGER NOT NULL,
		nonce BLOB NOT NULL,
		payload BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, false, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, false, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS identity (
		k BLOB PRIMARY KEY,
		nonce BLOB NOT NULL,
		payload BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, false, err
	}
	ob = &whatsAppOutbox{db: db, linkID: linkID}
	if err := ob.loadCounters(); err != nil {
		db.Close()
		return nil, false, err
	}
	identityRows, err := countWhatsAppIdentityRows(db)
	if err != nil {
		db.Close()
		return nil, false, err
	}

	key, keyErr := loadOrCreateWhatsAppOutboxKey(linkID, ob.count == 0 && identityRows == 0)
	if errors.Is(keyErr, errWhatsAppKeyNotFound) {
		// Rows exist but their key is gone: unreadable by construction.
		for _, stmt := range []string{`DELETE FROM outbox`, `DELETE FROM identity`} {
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, false, err
			}
		}
		discarded = true
		ob.count, ob.bytes = 0, 0
		key, keyErr = loadOrCreateWhatsAppOutboxKey(linkID, true)
	}
	if keyErr != nil {
		db.Close()
		return nil, false, keyErr
	}
	ob.aead, err = newWhatsAppGCM(key)
	ob.idxKey = deriveWhatsAppIdentityIndexKey(key)
	clear(key)
	if err != nil {
		db.Close()
		return nil, false, err
	}
	if err := ob.loadIdentities(); err != nil {
		db.Close()
		return nil, false, err
	}
	return ob, discarded, nil
}

// EnsureNextAbove makes every sequence handed out from now on greater than
// acked, the cloud's replay watermark for this link (`lastAckedSeq`, which a
// re-link never resets). The cloud acks a batch at or below that watermark as
// a replay WITHOUT writing it, so a sequence restarted below it would lose
// every record up to it — which is exactly what an outbox lost with its
// directory (an earlier build, a key that vanished with its rows) would do.
func (ob *whatsAppOutbox) EnsureNextAbove(acked int64) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.closed {
		return errWhatsAppOutboxClosed
	}
	if acked < ob.next {
		return nil
	}
	next := acked + 1
	if _, err := ob.db.Exec(`INSERT INTO meta (k, v) VALUES ('next', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(next, 10)); err != nil {
		return err
	}
	ob.next = next
	return nil
}

func (ob *whatsAppOutbox) loadCounters() error {
	row := ob.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0), COALESCE(MAX(seq), 0) FROM outbox`)
	var maxSeq int64
	if err := row.Scan(&ob.count, &ob.bytes, &maxSeq); err != nil {
		return err
	}
	// `next` survives acks that emptied the table, so a sequence is never reused.
	var stored string
	err := ob.db.QueryRow(`SELECT v FROM meta WHERE k = 'next'`).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		ob.next = maxSeq + 1
	case err != nil:
		return err
	default:
		n, perr := strconv.ParseInt(stored, 10, 64)
		if perr != nil {
			return perr
		}
		ob.next = max(n, maxSeq+1)
	}
	return nil
}

func (ob *whatsAppOutbox) aad(seq int64) []byte {
	return []byte(ob.linkID + ":" + strconv.FormatInt(seq, 10))
}

// Append assigns each record the next sequence (its deviceSeq), seals it and
// stores the lot in one transaction. A history append is refused whole with
// errWhatsAppOutboxFull when the queued rows PLUS this chunk would pass the
// caps — admitting a chunk on its starting depth alone could overshoot them
// by the whole chunk. The staged rows roll back, so a refused chunk leaves
// the queue exactly as it was and the caller retries it later; a chunk the
// empty queue refuses is errWhatsAppOutboxChunkTooLarge instead, which no
// wait can cure. Live appends always land.
func (ob *whatsAppOutbox) Append(records []whatsAppRecord, history bool) error {
	if len(records) == 0 {
		return nil
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.closed {
		return errWhatsAppOutboxClosed
	}
	tx, err := ob.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seq := ob.next
	var addedBytes int64
	for i := range records {
		records[i].DeviceSeq = seq
		plain, err := json.Marshal(records[i])
		if err != nil {
			return err
		}
		nonce := make([]byte, ob.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		sealed := ob.aead.Seal(nil, nonce, plain, ob.aad(seq))
		clear(plain)
		if _, err := tx.Exec(`INSERT INTO outbox (seq, history, size, nonce, payload) VALUES (?, ?, ?, ?, ?)`,
			seq, boolToInt(history), len(sealed), nonce, sealed); err != nil {
			return err
		}
		addedBytes += int64(len(sealed))
		seq++
	}
	if history && (ob.count+int64(len(records)) > whatsAppOutboxMaxRecords || ob.bytes+addedBytes > whatsAppOutboxMaxBytes) {
		if ob.count == 0 {
			return errWhatsAppOutboxChunkTooLarge
		}
		return errWhatsAppOutboxFull
	}
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('next', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(seq, 10)); err != nil {
		return err
	}
	// Staged identities commit with the records that use them
	// (stageIdentities). Inside a savepoint: if the identity table refuses,
	// only that part rolls back and stays staged for the next Append - the
	// records themselves must never be lost to it.
	wroteIdentities := false
	if len(ob.pendingIdentities) > 0 {
		if _, err := tx.Exec(`SAVEPOINT identities`); err == nil {
			if ob.writePendingIdentitiesLocked(tx) == nil {
				_, err = tx.Exec(`RELEASE identities`)
				wroteIdentities = err == nil
			}
			if !wroteIdentities {
				if _, err := tx.Exec(`ROLLBACK TO identities`); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if wroteIdentities {
		clear(ob.pendingIdentities)
	}
	ob.next = seq
	ob.count += int64(len(records))
	ob.bytes += addedBytes
	return nil
}

// Next returns the oldest contiguous range that fits one batch, or a nil
// batch when the outbox is empty.
func (ob *whatsAppOutbox) Next() (*whatsAppOutboxBatch, error) {
	return ob.NextUpTo(whatsAppBatchMaxRecords)
}

// NextUpTo is Next with at most maxRecords records (never more than one
// batch allows): the uploader narrows a refused batch down with it.
func (ob *whatsAppOutbox) NextUpTo(maxRecords int) (*whatsAppOutboxBatch, error) {
	if maxRecords <= 0 || maxRecords > whatsAppBatchMaxRecords {
		maxRecords = whatsAppBatchMaxRecords
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.closed {
		return nil, errWhatsAppOutboxClosed
	}
	rows, err := ob.db.Query(`SELECT seq, nonce, payload FROM outbox ORDER BY seq LIMIT ?`, maxRecords)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch *whatsAppOutboxBatch
	budget := whatsAppBatchRecordsBudget
	for rows.Next() {
		var seq int64
		var nonce, payload []byte
		if err := rows.Scan(&seq, &nonce, &payload); err != nil {
			return nil, err
		}
		plain, err := ob.aead.Open(nil, nonce, payload, ob.aad(seq))
		if err != nil {
			return nil, fmt.Errorf("outbox row %d does not open: %w", seq, err)
		}
		if batch != nil && (seq != batch.SeqTo+1 || len(plain) > budget) {
			break // keep ranges contiguous and inside the byte budget
		}
		if batch == nil {
			batch = &whatsAppOutboxBatch{SeqFrom: seq}
		}
		batch.SeqTo = seq
		batch.Records = append(batch.Records, plain)
		budget -= len(plain)
	}
	return batch, rows.Err()
}

// Ack deletes every row up to and including seq.
func (ob *whatsAppOutbox) Ack(seq int64) error {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.closed {
		return errWhatsAppOutboxClosed
	}
	if _, err := ob.db.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq); err != nil {
		return err
	}
	return ob.loadCountersLocked()
}

func (ob *whatsAppOutbox) loadCountersLocked() error {
	row := ob.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM outbox`)
	return row.Scan(&ob.count, &ob.bytes)
}

// Depth is the number of not-yet-acked records (diagnostics.outboxDepth).
func (ob *whatsAppOutbox) Depth() int64 {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return ob.count
}

// HasRoomForHistory reports whether a history append would be accepted.
func (ob *whatsAppOutbox) HasRoomForHistory() bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	return ob.count < whatsAppOutboxMaxRecords && ob.bytes < whatsAppOutboxMaxBytes
}

// Close waits for a write in flight (every write holds mu for its whole
// transaction) and refuses later ones. A re-link opens the same file under a
// new handle right after the old session's stores close: an Append still
// committing on the old handle would otherwise land after the new handle read
// `next`, and the two would hand out the same sequences.
func (ob *whatsAppOutbox) Close() error {
	if ob == nil || ob.db == nil {
		return nil
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if ob.closed {
		return nil
	}
	ob.closed = true
	// Best effort: identities staged since the last Append (a history chunk's
	// learned aliases, say) must not die with this handle.
	if len(ob.pendingIdentities) > 0 {
		if tx, err := ob.db.Begin(); err == nil {
			if ob.writePendingIdentitiesLocked(tx) == nil && tx.Commit() == nil {
				clear(ob.pendingIdentities)
			} else {
				_ = tx.Rollback()
			}
		}
	}
	return ob.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
