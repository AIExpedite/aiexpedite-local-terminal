// cliagent_usage_opencode_direct_json.go — the direct-run reader's adapter for
// OpenCode's JSON-file store (cliagent_usage_opencode_direct.go):
//
//	storage/message/<sessionID>/<messageID>.json   one message's info
//	storage/part/<messageID>/<partID>.json         one part of a message
//
// A message is read when its file was last written at or after the scan floor,
// from a session directory written since then. Its usage is summed from its
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
}

func readOpenCodeDirectJSON(ctx context.Context, root string, floorMs int64, limits openCodeDirectLimits) (openCodeDirectRead, error) {
	var read openCodeDirectRead
	messageRoot := filepath.Join(root, "storage", "message")
	entries, err := os.ReadDir(messageRoot)
	if err != nil {
		return read, err
	}

	type sessionDir struct {
		name string
		at   int64
	}
	var sessions []sessionDir
	sinceMs := floorMs - openCodeDirectSessionSlack.Milliseconds()
	for _, e := range entries {
		if !e.IsDir() || !isValidOpenCodeSessionID(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if at := info.ModTime().UnixMilli(); at >= sinceMs {
			sessions = append(sessions, sessionDir{e.Name(), at})
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].at != sessions[j].at {
			return sessions[i].at < sessions[j].at
		}
		return sessions[i].name < sessions[j].name
	})
	if len(sessions) > limits.MaxSessions {
		read.truncateAt(sessions[limits.MaxSessions].at)
		sessions = sessions[:limits.MaxSessions]
	}

	var files []openCodeDirectJSONFile
	for _, s := range sessions {
		if ctx.Err() != nil {
			read.truncateAt(s.at)
			break
		}
		dir := filepath.Join(messageRoot, s.name)
		names, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, n := range names {
			if n.IsDir() || !strings.HasSuffix(n.Name(), ".json") {
				continue
			}
			info, err := n.Info()
			if err != nil {
				continue
			}
			if at := info.ModTime().UnixMilli(); at >= floorMs {
				files = append(files, openCodeDirectJSONFile{filepath.Join(dir, n.Name()), s.name, at, info.Size()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].at != files[j].at {
			return files[i].at < files[j].at
		}
		return files[i].path < files[j].path
	})
	if len(files) > limits.MaxRecords {
		read.truncateAt(files[limits.MaxRecords].at)
		files = files[:limits.MaxRecords]
	}

	recognised, misshapen := 0, 0
	for _, f := range files {
		if ctx.Err() != nil {
			read.truncateAt(f.at)
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
			parts := readOpenCodeDirectJSONParts(filepath.Join(root, "storage", "part", msg.ID))
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

// readOpenCodeDirectJSONParts reads a message's step-finish parts. A part that
// is too large to be one is skipped unopened.
func readOpenCodeDirectJSONParts(dir string) []openCodeExportPart {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var parts []openCodeExportPart
	for i, e := range entries {
		if i >= openCodeDirectMaxPartsPerMessage {
			break
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err != nil || info.Size() > openCodeDirectMaxPartBytes {
			continue
		}
		var part openCodeExportPart
		if readJSONFileWithin(filepath.Join(dir, e.Name()), openCodeDirectMaxPartBytes, &part) && isOpenCodeStepFinishType(part.Type) {
			parts = append(parts, part)
		}
	}
	return parts
}
