// cliagent_usage_opencode_direct_json.go — the direct-run reader's adapter for
// OpenCode's JSON-file store (cliagent_usage_opencode_direct.go):
//
//	storage/message/<sessionID>/<messageID>.json   one message's info
//	storage/part/<messageID>/<partID>.json         one part of a message
//
// A message is read when its file was last written at or after the scan floor,
// from a session directory written since the session floor — which reaches
// further back, since a message rewritten in place does not move its directory. Its usage is summed from its
// step-finish parts and max'd with its own info, exactly as the export fallback
// does (openCodeExportMessageUsage). Files are opened and closed one record at a
// time, so the reader never holds a handle that would block OpenCode's own
// rename of a record.
package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// openCodeDirectSessionSlack widens the session-directory filter: a
	// directory's mtime moves when a record is created in it, which can precede
	// that record's own last write.
	openCodeDirectSessionSlack = time.Minute
	// openCodeDirectMaxPartBytes skips a part file without opening it: a
	// step-finish part is a few hundred bytes, and anything far larger is text
	// or tool output the reader has no reason to load.
	openCodeDirectMaxPartBytes = 64 << 10
	// openCodeDirectMaxPartsPerMessage bounds the part listing of one message.
	openCodeDirectMaxPartsPerMessage = 1024
	// openCodeDirectListBatch is how many entries one directory listing call
	// reads, between deadline checks.
	openCodeDirectListBatch = 64
)

// openCodeDirectJSONMessage is the narrow view of a stored message.
type openCodeDirectJSONMessage struct {
	openCodeExportMessageInfo
	SessionID string `json:"sessionID"`
}

type openCodeDirectJSONFile struct {
	path    string
	session string
	at      int64
	size    int64
	before  int // records listed before it at the same time
}

func readOpenCodeDirectJSON(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
	var read openCodeDirectRead
	messageRoot := filepath.Join(root, "storage", "message")
	if _, err := os.Stat(messageRoot); err != nil {
		return read, err
	}

	type sessionDir struct {
		name   string
		at     int64
		before int // sessions listed before it at the same time
	}
	var sessions []sessionDir
	sinceMs := limits.sessionsSinceMs(floorMs)
	// Sessions are read in last-write order, so the root must be listed whole
	// before any is read: a root the budget cannot list reads nothing, and the
	// scan fails without moving the cursor.
	rootListed := eachOpenCodeDirectEntry(ctx, messageRoot, 0, func(e os.DirEntry) {
		if !e.IsDir() || !isValidOpenCodeSessionID(e.Name()) {
			return
		}
		info, err := e.Info()
		if err != nil {
			return
		}
		if at := info.ModTime().UnixMilli(); at >= sinceMs {
			sessions = append(sessions, sessionDir{name: e.Name(), at: at})
		}
	})
	if !rootListed {
		return read, ctx.Err()
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].at != sessions[j].at {
			return sessions[i].at < sessions[j].at
		}
		return sessions[i].name < sessions[j].name
	})
	sessions = dropOpenCodeDirectRead(sessions, limits.skipFor(true),
		func(s sessionDir) int64 { return s.at }, func(s *sessionDir, before int) { s.before = before })
	if len(sessions) > limits.MaxSessions {
		cut := sessions[limits.MaxSessions]
		read.truncateSessionsAt(cut.at, cut.before)
		sessions = sessions[:limits.MaxSessions]
	}

	var files []openCodeDirectJSONFile
	for _, s := range sessions {
		dir := filepath.Join(messageRoot, s.name)
		listed := len(files)
		ok := eachOpenCodeDirectEntry(ctx, dir, 0, func(n os.DirEntry) {
			if n.IsDir() || !strings.HasSuffix(n.Name(), ".json") {
				return
			}
			info, err := n.Info()
			if err != nil {
				return
			}
			if at := info.ModTime().UnixMilli(); at >= floorMs {
				files = append(files, openCodeDirectJSONFile{path: filepath.Join(dir, n.Name()), session: s.name, at: at, size: info.Size()})
			}
		})
		if !ok {
			// The budget ran out before this session was listed whole: none of
			// it is read, and the next scan resumes at it.
			files = files[:listed]
			read.truncateSessionsAt(s.at, s.before)
			break
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].at != files[j].at {
			return files[i].at < files[j].at
		}
		return files[i].path < files[j].path
	})
	files = dropOpenCodeDirectRead(files, limits.skipFor(false),
		func(f openCodeDirectJSONFile) int64 { return f.at }, func(f *openCodeDirectJSONFile, before int) { f.before = before })
	if len(files) > limits.MaxRecords {
		cut := files[limits.MaxRecords]
		read.truncateAt(cut.at, cut.before)
		files = files[:limits.MaxRecords]
	}

	recognised, misshapen := 0, 0
	for _, f := range files {
		if ctx.Err() != nil {
			read.truncateAt(f.at, f.before)
			break
		}
		if f.size > limits.MaxRecordBytes {
			read.Skipped++
			continue
		}
		var msg openCodeDirectJSONMessage
		if !readJSONFileWithin(f.path, limits.MaxRecordBytes, &msg) {
			read.Skipped++
			continue
		}
		created, ok := openCodeUsageMillis(msg.Time.Created)
		if !ok || msg.Role == "" || !isValidOpenCodeSessionID(msg.ID) {
			read.Skipped++
			misshapen++
			continue
		}
		recognised++
		if msg.Role != "assistant" {
			continue
		}
		m := openCodeDirectMessage{
			ID:        msg.ID,
			SessionID: firstNonEmpty(msg.SessionID, f.session),
			CreatedMs: created,
			WrittenMs: f.at,
		}
		if completed, ok := openCodeUsageMillis(msg.Time.Completed); ok {
			m.CompletedMs = completed
			parts, ok := readOpenCodeDirectJSONParts(ctx, filepath.Join(root, "storage", "part", msg.ID))
			if !ok {
				// The budget ran out inside this message's parts: it was not
				// read whole, so the cursor stops before it.
				read.truncateAt(f.at, f.before)
				break
			}
			m.Usage, m.Valid = openCodeExportMessageUsage(openCodeExportMessage{Parts: parts}, msg.openCodeExportMessageInfo)
		}
		read.Messages = append(read.Messages, m)
	}
	if recognised == 0 && misshapen > 0 {
		// Records that decode, but none in a shape this adapter knows: a layout
		// change, not a few corrupt or oversized files. Fail closed rather than
		// publish a 0.
		return openCodeDirectRead{}, errOpenCodeDirectLayoutUnknown
	}
	return read, nil
}

// dropOpenCodeDirectRead drops from a sorted listing the entries skip says a
// capped scan read, and stamps each kept entry with how many entries came
// before it at its time — dropped ones included — for the cut it may become.
func dropOpenCodeDirectRead[T any](list []T, skip openCodeDirectSkip, at func(T) int64, stamp func(*T, int)) []T {
	var ties openCodeDirectTies
	kept := list[:0]
	for _, e := range list {
		before := ties.see(at(e))
		if skip.take(at(e)) {
			continue
		}
		stamp(&e, before)
		kept = append(kept, e)
	}
	return kept
}

// readOpenCodeDirectJSONParts reads a message's step-finish parts. A part that
// is too large to be one is skipped unopened. The directory is listed through
// eachOpenCodeDirectEntry, so a large part directory cannot run the scan past
// its budget; ok is false when the deadline stopped the read.
func readOpenCodeDirectJSONParts(ctx context.Context, dir string) (parts []openCodeExportPart, ok bool) {
	ok = eachOpenCodeDirectEntry(ctx, dir, openCodeDirectMaxPartsPerMessage, func(e os.DirEntry) {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			return
		}
		if info, err := e.Info(); err != nil || info.Size() > openCodeDirectMaxPartBytes {
			return
		}
		var part openCodeExportPart
		if readJSONFileWithin(filepath.Join(dir, e.Name()), openCodeDirectMaxPartBytes, &part) && isOpenCodeStepFinishType(part.Type) {
			parts = append(parts, part)
		}
	})
	if !ok {
		return nil, false
	}
	return parts, true
}

// eachOpenCodeDirectEntry lists dir in batches and calls fn for each entry, up
// to limit entries (0 for no limit). The scan's deadline is checked before each
// batch and each entry, so a directory of any size cannot run the scan past its
// budget — os.ReadDir would materialise it whole first. ok is false when the
// deadline stopped the listing; a directory that cannot be read lists nothing.
func eachOpenCodeDirectEntry(ctx context.Context, dir string, limit int, fn func(os.DirEntry)) (ok bool) {
	d, err := os.Open(dir)
	if err != nil {
		return true
	}
	defer d.Close()
	listed := 0
	for limit == 0 || listed < limit {
		if ctx.Err() != nil {
			return false
		}
		n := openCodeDirectListBatch
		if limit > 0 {
			n = min(n, limit-listed)
		}
		entries, err := d.ReadDir(n)
		for _, e := range entries {
			if ctx.Err() != nil {
				return false
			}
			fn(e)
		}
		listed += len(entries)
		if err != nil || len(entries) == 0 {
			break
		}
	}
	return true
}
