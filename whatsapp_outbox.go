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
// row copied between links or reordered fails to open. The database file
// therefore holds only ciphertext — message text never sits on disk in the
// clear outside whatsmeow's own store.
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

const (
	whatsAppOutboxMaxRecords   = 50000
	whatsAppOutboxMaxBytes     = 200 * 1024 * 1024
	whatsAppBatchMaxRecords    = 200
	whatsAppBatchMaxBodyBytes  = 512 * 1024
	whatsAppBatchRecordsBudget = 480 * 1024 // leaves room for the envelope
)

var errWhatsAppOutboxFull = errors.New("whatsapp outbox is full")

// whatsAppOutbox is one link's queue.
type whatsAppOutbox struct {
	mu     sync.Mutex
	db     *sql.DB
	linkID string
	aead   cipher.AEAD
	// cached depth, so a cap check is not a COUNT(*) per message
	count int64
	bytes int64
	next  int64
}

// whatsAppOutboxBatch is a contiguous range ready to upload.
type whatsAppOutboxBatch struct {
	SeqFrom int64
	SeqTo   int64
	Records []json.RawMessage
}

// openWhatsAppOutbox opens (creating) dir/outbox.db with the link's key.
// create=false refuses to mint a key: an outbox whose key is gone cannot be
// read, so its rows are discarded and a fresh key is minted — reported by
// discarded=true so the caller can log it.
func openWhatsAppOutbox(dir, linkID string) (ob *whatsAppOutbox, discarded bool, err error) {
	path := filepath.Join(dir, whatsAppOutboxDBName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
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
	ob = &whatsAppOutbox{db: db, linkID: linkID}
	if err := ob.loadCounters(); err != nil {
		db.Close()
		return nil, false, err
	}

	key, keyErr := loadOrCreateWhatsAppOutboxKey(linkID, ob.count == 0)
	if errors.Is(keyErr, errWhatsAppKeyNotFound) {
		// Rows exist but their key is gone: unreadable by construction.
		if _, err := db.Exec(`DELETE FROM outbox`); err != nil {
			db.Close()
			return nil, false, err
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
	clear(key)
	if err != nil {
		db.Close()
		return nil, false, err
	}
	return ob, discarded, nil
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
// stores the lot in one transaction. History appends past the caps are
// refused whole with errWhatsAppOutboxFull; live appends always land.
func (ob *whatsAppOutbox) Append(records []whatsAppRecord, history bool) error {
	if len(records) == 0 {
		return nil
	}
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if history && (ob.count >= whatsAppOutboxMaxRecords || ob.bytes >= whatsAppOutboxMaxBytes) {
		return errWhatsAppOutboxFull
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
	if _, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('next', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		strconv.FormatInt(seq, 10)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ob.next = seq
	ob.count += int64(len(records))
	ob.bytes += addedBytes
	return nil
}

// Next returns the oldest contiguous range that fits one batch, or a nil
// batch when the outbox is empty.
func (ob *whatsAppOutbox) Next() (*whatsAppOutboxBatch, error) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	rows, err := ob.db.Query(`SELECT seq, nonce, payload FROM outbox ORDER BY seq LIMIT ?`, whatsAppBatchMaxRecords)
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
	if _, err := ob.db.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq); err != nil {
		return err
	}
	return ob.loadCountersLocked()
}

// DropThrough deletes a batch the cloud refused as malformed (400), so one bad
// record cannot wedge the queue. Same as Ack; named for the log.
func (ob *whatsAppOutbox) DropThrough(seq int64) error { return ob.Ack(seq) }

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

func (ob *whatsAppOutbox) Close() error {
	if ob == nil || ob.db == nil {
		return nil
	}
	return ob.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
