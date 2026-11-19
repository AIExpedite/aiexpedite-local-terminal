// codex_session_sources.go — Codex's sessions, for discovery and the adoption
// probe (cli_session_discovery.go), and the writer evidence of a thread AI
// Expedite resumes (cli_session_outside_writer.go).
//
// Codex writes one rollout per thread, under the home every Codex reader here
// uses (codexHomeBase: $CODEX_HOME, else ~/.codex):
//
//	sessions/YYYY/MM/DD/rollout-<start, 2006-01-02T15-04-05>-<thread id>.jsonl
//
// Checked against the rollouts Codex 0.1xx writes and the header
// cliagent_ratelimit_codex_rollout.go already reads: the first record is
// {"timestamp", "type":"session_meta", "payload":{"id" (the thread id),
// "cwd", "timestamp", "originator", "source", "cli_version", …}}.
// `originator` is the app-server client's name — AI Expedite's own
// agent-orchestrator-service calls itself ai-expedite-agent-orchestrator
// (codexConversation.service.js) — and `source` is an object for a sub-agent
// thread another thread spawned, which is no session a person started. A
// person's prompt is an `event_msg` whose payload type is `user_message`
// (`message`); where a rollout has none it is a `response_item` user
// `message`, after the context blocks Codex writes there itself
// (<environment_context>, AGENTS.md, …), which are never a label. A name a
// person gave a thread is in session_index.jsonl ({"id", "thread_name",
// "updated_at"}, the newest line for an id winning).
//
// Only a rollout's head (the header and the records up to the first prompt,
// bounded) is read; one whose first record is no session header is
// `unrecognised`. Read-only. Resuming a thread a terminal or IDE started is
// the app-server's `thread/resume` (agent-orchestrator-service): that it
// accepts one it did not start is not verified on this device.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// codexOriginatorAIExpedite is the originator agent-orchestrator-service's
	// app-server client stamps on the threads it starts.
	codexOriginatorAIExpedite = "ai-expedite-agent-orchestrator"
	// A rollout's head is read up to this much / this many records for its
	// header and first prompt (the header carries Codex's base instructions).
	codexRolloutHeadMaxBytes = 1 << 20
	codexRolloutHeadMaxLines = 400
	// codexSessionIndexMaxBytes bounds session_index.jsonl.
	codexSessionIndexMaxBytes = 8 << 20
	// codexRolloutSearchEvery: a watcher that has not found its thread's
	// rollout yet (a thread whose first turn has not run) looks again this
	// often, never on every poll.
	codexRolloutSearchEvery = 30 * time.Second
)

var codexSessionReader = &cliSessionReader{
	engine:  cliSessionEngineCodex,
	sources: []string{cliSessionSourceCodexStore},
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverCodexSessions(ctx, codexHomeBase(), req)
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeCodexSession(codexHomeBase(), req)
	},
}

// codexWriterEvidence: a live `codex` naming the thread (`codex resume <id>`)
// holds it open; its rollout written within writerQuietMs is a writer
// mid-turn.
var codexWriterEvidence = cliSessionWriterEvidence{
	validID:        isCliSessionUUID,
	isAgentProcess: cliSessionNameIn("codex"),
	storeClock: func(threadID string) func() int64 {
		home := codexHomeBase()
		path, searched := "", time.Time{}
		return func() int64 {
			if path == "" && time.Since(searched) >= codexRolloutSearchEvery {
				searched = time.Now()
				path = findCodexRollout(home, threadID)
			}
			return newestFileMs(path)
		}
	},
}

// codexRolloutFile is one rollout the listing found.
type codexRolloutFile struct {
	threadID string
	path     string
	modified int64
}

// codexRolloutHead is what a rollout's head says.
type codexRolloutHead struct {
	threadID   string
	cwd        string
	originator string
	subagent   bool
	prompt     string
}

// listCodexRollouts lists every rollout under root (newest file per thread),
// newest day first: os.ReadDir sorts by name, so Codex's zero-padded
// YYYY/MM/DD layout read backwards is newest first (as the usage reader walks
// it, cliagent_ratelimit_codex_rollout.go). Every day is listed — a thread a
// person resumes keeps appending to the rollout of the day it started — but a
// listing ctx cuts short has the newest days, not the oldest. complete is
// false when ctx ended first.
func listCodexRollouts(ctx context.Context, root string) (files []codexRolloutFile, complete bool, err error) {
	years, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	newest := map[string]int{}
	for _, y := range codexDirNamesNewestFirst(years) {
		months, _ := os.ReadDir(filepath.Join(root, y))
		for _, m := range codexDirNamesNewestFirst(months) {
			days, _ := os.ReadDir(filepath.Join(root, y, m))
			for _, d := range codexDirNamesNewestFirst(days) {
				if ctx.Err() != nil {
					return files, false, nil
				}
				dir := filepath.Join(root, y, m, d)
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					id, ok := codexRolloutThreadID(e.Name())
					if !ok || e.IsDir() {
						continue
					}
					fi, err := e.Info()
					if err != nil {
						continue
					}
					f := codexRolloutFile{threadID: id, path: filepath.Join(dir, e.Name()), modified: fi.ModTime().UnixMilli()}
					if at, seen := newest[id]; seen {
						if files[at].modified < f.modified {
							files[at] = f
						}
						continue
					}
					newest[id] = len(files)
					files = append(files, f)
				}
			}
		}
	}
	return files, ctx.Err() == nil, nil
}

// codexDirNamesNewestFirst is one date level's directory names, the newest
// (the last by name) first.
func codexDirNamesNewestFirst(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].IsDir() {
			names = append(names, entries[i].Name())
		}
	}
	return names
}

// codexRolloutThreadID is the thread id a rollout's file name ends with.
func codexRolloutThreadID(name string) (string, bool) {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	stem := strings.TrimSuffix(name, ".jsonl")
	if len(stem) < 36 {
		return "", false
	}
	id := stem[len(stem)-36:]
	return id, isCliSessionUUID(id)
}

// findCodexRollout is the newest rollout of threadID under home, "" for none.
func findCodexRollout(home, threadID string) string {
	if home == "" || !isCliSessionUUID(threadID) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	best, bestAt := "", int64(0)
	for _, m := range matches {
		if at := newestFileMs(m); at > bestAt {
			best, bestAt = m, at
		}
	}
	return best
}

// codexRolloutRecord is the envelope of one rollout record, and the few
// payload fields the head is read for.
type codexRolloutRecord struct {
	Type    *string         `json:"type"`
	ID      json.RawMessage `json:"id"`
	Cwd     string          `json:"cwd"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Payload struct {
		Type         string          `json:"type"`
		ID           string          `json:"id"`
		Cwd          string          `json:"cwd"`
		Originator   string          `json:"originator"`
		Source       json.RawMessage `json:"source"`
		ThreadSource string          `json:"thread_source"`
		Role         string          `json:"role"`
		Message      string          `json:"message"`
		Content      json.RawMessage `json:"content"`
	} `json:"payload"`
}

// readCodexRolloutHead reads a rollout's header and first prompt.
func readCodexRolloutHead(path string) (codexRolloutHead, error) {
	var head codexRolloutHead
	first, sawHeader := true, false
	fallbackPrompt := ""
	err := readJSONLHead(path, codexRolloutHeadMaxBytes, codexRolloutHeadMaxLines, func(line []byte) bool {
		var rec codexRolloutRecord
		if json.Unmarshal(line, &rec) != nil {
			if first {
				first = false
				return false // the header is not JSON: nothing below is trusted
			}
			return true
		}
		if first {
			first = false
			switch {
			case rec.Type != nil && *rec.Type == codexRolloutSessionMetaType:
				head.threadID, head.cwd, head.originator = rec.Payload.ID, rec.Payload.Cwd, rec.Payload.Originator
				head.subagent = codexSourceIsSubagent(rec.Payload.Source) || rec.Payload.ThreadSource == "subagent"
				sawHeader = true
			case rec.Type == nil && len(rec.ID) > 0:
				// Rollouts before the typed envelope open with a bare header.
				_ = json.Unmarshal(rec.ID, &head.threadID)
				head.cwd = rec.Cwd
				sawHeader = true
			default:
				return false
			}
			return true
		}
		switch {
		case rec.Type != nil && *rec.Type == "event_msg" && rec.Payload.Type == "user_message":
			if t := strings.TrimSpace(rec.Payload.Message); t != "" && !cliSessionSyntheticPrompt(t) {
				head.prompt = t
				return false
			}
		case rec.Type != nil && *rec.Type == "response_item" && rec.Payload.Type == "message" && rec.Payload.Role == "user":
			if fallbackPrompt == "" {
				fallbackPrompt = codexInputText(rec.Payload.Content)
			}
		case rec.Type != nil && *rec.Type == "message" && rec.Role == "user":
			if fallbackPrompt == "" {
				fallbackPrompt = codexInputText(rec.Content)
			}
		}
		return true
	})
	if err != nil && !sawHeader {
		return codexRolloutHead{}, err
	}
	if !sawHeader || !isCliSessionUUID(head.threadID) {
		return codexRolloutHead{}, errCliSessionUnrecognised
	}
	if head.prompt == "" {
		head.prompt = fallbackPrompt
	}
	return head, nil
}

// codexInputText is a user message's first input text that a person wrote
// ("" when every part is a context block Codex added).
func codexInputText(raw json.RawMessage) string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	for _, p := range parts {
		if (p.Type == "input_text" || p.Type == "text") && !cliSessionSyntheticPrompt(p.Text) {
			return strings.TrimSpace(p.Text)
		}
	}
	return ""
}

// codexSourceIsSubagent reports a session_meta `source` naming a sub-agent
// (`{"subagent": …}`); a person's sources are plain strings ("cli", "vscode",
// "exec").
func codexSourceIsSubagent(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	_, sub := obj["subagent"]
	return sub
}

// codexThreadName is a name a person gave a thread.
type codexThreadName struct {
	name      string
	updatedAt int64
}

// readCodexThreadNames reads session_index.jsonl: thread id → its newest name.
func readCodexThreadNames(home string) map[string]codexThreadName {
	out := map[string]codexThreadName{}
	_ = readJSONLHead(filepath.Join(home, "session_index.jsonl"), codexSessionIndexMaxBytes, 1<<20, func(line []byte) bool {
		var rec struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if json.Unmarshal(line, &rec) != nil || !isCliSessionUUID(rec.ID) {
			return true
		}
		at := parseCliSessionTime(rec.UpdatedAt)
		if prev, seen := out[rec.ID]; !seen || at >= prev.updatedAt {
			out[rec.ID] = codexThreadName{name: strings.TrimSpace(rec.ThreadName), updatedAt: at}
		}
		return true
	})
	return out
}

// codexRowTitle is a thread's title: the name a person gave it, else its
// first prompt.
func codexRowTitle(head codexRolloutHead, names map[string]codexThreadName) (string, string, int64) {
	n := names[head.threadID]
	return cliSessionTitleOf(n.name, n.updatedAt, "", head.prompt)
}

// discoverCodexSessions lists the threads whose rollout was written within
// the request's look-back, newest first.
func discoverCodexSessions(ctx context.Context, home string, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	res := newCliSessionsResult(cliSessionEngineCodex, req.now)
	src := cliSessionSourceCodexStore
	if home == "" {
		res.skip(src, cliSessionSkipNotFound)
		return res
	}
	if _, err := os.Stat(home); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.skip(src, cliSessionSkipNotFound)
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	// The heads are read under ctx itself: a listing cut short still answers
	// the newest threads it found.
	listCtx, cancelList := cliSessionListingContext(ctx)
	files, complete, err := listCodexRollouts(listCtx, filepath.Join(home, "sessions"))
	cancelList()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.SourcesChecked = append(res.SourcesChecked, src) // no thread yet
		} else {
			res.skip(src, cliSessionSkipUnreadable)
		}
		return res
	}
	res.SourcesChecked = append(res.SourcesChecked, src)
	since := req.since().UnixMilli()
	kept := files[:0]
	for _, f := range files {
		if f.modified >= since {
			kept = append(kept, f)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modified > kept[j].modified })
	if len(kept) > req.maxRows {
		kept = kept[:req.maxRows]
		res.Truncated = true
	}
	names := readCodexThreadNames(home)
	running := cliSessionRunningByID(codexWriterEvidence)
	timedOut := !complete
	parsed, unrecognised, failed := 0, 0, 0
	for _, f := range kept {
		if ctx.Err() != nil {
			timedOut = true
			break
		}
		head, err := readCodexRolloutHead(f.path)
		switch {
		case errors.Is(err, errCliSessionUnrecognised):
			unrecognised++
			continue
		case err != nil:
			failed++
			continue
		}
		parsed++
		if head.subagent || head.threadID != f.threadID {
			continue
		}
		row := cliSessionRow{
			SessionID:           head.threadID,
			Cwd:                 head.cwd,
			LastActiveAt:        f.modified,
			Editors:             []cliSessionEditorRef{},
			Resumable:           true,
			StartedByAiExpedite: head.originator == codexOriginatorAIExpedite,
			Running:             cliSessionRowRunning(running[head.threadID], f.modified, req.now),
		}
		row.Title, row.TitleSource, row.TitleSavedAt = codexRowTitle(head, names)
		res.Rows = append(res.Rows, row)
	}
	switch {
	case timedOut:
		res.Truncated = true
		res.skip(src, cliSessionSkipTimeout)
	case parsed > 0:
	case unrecognised > 0:
		res.skip(src, cliSessionSkipUnrecognised)
	case failed > 0:
		res.skip(src, cliSessionSkipUnreadable)
	}
	return res
}

// cliSessionRowRunning is a row's `running` for an engine without a status
// registry: a live process naming the session holds it (idle), and a store
// written within writerQuietMs is mid-turn (busy, pid 0 when no process names
// it). nil when neither.
func cliSessionRowRunning(proc *cliSessionRunning, writtenMs int64, now time.Time) *cliSessionRunning {
	fresh := cliSessionStoreWrittenByOther(writtenMs, now, nil)
	switch {
	case proc != nil && fresh:
		return &cliSessionRunning{Status: cliSessionRunBusy, PID: proc.PID}
	case proc != nil:
		return proc
	case fresh:
		return &cliSessionRunning{Status: cliSessionRunBusy}
	}
	return nil
}

// probeCodexSession answers the probe of one thread.
func probeCodexSession(home string, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isCliSessionUUID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(codexWriterEvidence, req.sessionID)
	if path := findCodexRollout(home, req.sessionID); path != "" {
		res.TranscriptWrittenAt = newestFileMs(path)
		head, err := readCodexRolloutHead(path)
		switch {
		case err == nil && head.threadID == req.sessionID && !head.subagent:
			res.Exists, res.Resumable = true, true
			res.Title, res.TitleSource, _ = codexRowTitle(head, readCodexThreadNames(home))
		case err == nil || errors.Is(err, errCliSessionUnrecognised):
			// A rollout is there, but not one this build can resume.
			res.Exists = true
		}
	}
	res.Exists = res.Exists || len(res.Writers) > 0
	return res
}
