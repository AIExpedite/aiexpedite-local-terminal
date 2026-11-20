// musecode_session_sources.go — Muse Code's sessions, for discovery and the
// adoption probe (cli_session_discovery.go), and the writer evidence of a
// session AI Expedite continues (cli_session_outside_writer.go).
//
// Muse Code keeps no store this agent reads: its sessions are known to the
// host itself, which lists them over its own protocol, MSP `session/list`
// (in the method list of Muse Code 1.4.18's schema export,
// testdata/muse-msp-1.4.18.surface.json). So a discovery does what the CLI
// Agents card's usage read does (cliagent_usage_musecode_live.go): start a
// private, short-lived `muse serve` — in an empty temp folder, no writes, no
// shell, its whole tree killed on close — and ask it, inside the discovery
// budget. The host is the reader's alone: it is not a chat host, the send
// fence (msp_fence.go, which refuses `session/list` from the cloud) never sees
// it, and nothing but `initialize`, `initialized` and `session/list` is ever
// sent to it — no session is started, resumed or read, so nothing lands in the
// person's Muse history. It runs WITHOUT `--no-session-log`, because a
// memory-only host knows no saved session (musecode_msp_driver.go: it cannot
// resume one).
//
// NOT VERIFIED on a real Muse install (none on the machine this was written
// on): the exact params and result of `session/list`. The reader expects the
// session shape MSP's own answers use (`session/start` / `session/resume`
// answer {"session":{"sessionId","workspaceRoot",…}}, which msp_fence.go
// reads): {"sessions":[{"sessionId", "workspaceRoot", "name"|"title",
// "updatedAt"|"lastActivityAt"|"createdAt"}], "nextCursor"?}. Anything else
// is `unrecognised`, never guessed at; a host without the method (JSON-RPC
// -32601) is providerUnsupported; a list that says it has more (a cursor) is
// truncated, because its paging param is not known either, and the probe of a
// session such a list does not name is an error, never "gone". Whether the
// list includes sessions started outside a host (a `muse` TUI's) is the open
// question in the per-agent matrix: if it does not, Muse Code is listed as
// having none, which is why terminal-service keeps Muse Code's adoption
// behind a confirmation.
//
// ONLY A LISTED ID IS EVER RESUMED. Muse takes the session id from the
// caller (`muse exec --session-id`), and an id it does not hold silently
// starts a NEW session under it. So a row is only ever a listed session, and
// the probe confirms a session only from a listing taken there and then: an id
// the host does not list — or a listing that could not be taken, or one cut
// short of the id — is never reported resumable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// museCodeListingServeArgs: the listing host's argv. No writes and no shell
// (the usage probe host's limits, musecode_msp.go), but a session log, so it
// can list the saved sessions.
var museCodeListingServeArgs = []string{"serve", "--disable-write", "--disable-shell"}

// museCodeListingClientName is the listing host's `clientInfo.name`.
const museCodeListingClientName = "aiexpedite_session_discovery"

// mspMethodSessionList lists the host's sessions.
const mspMethodSessionList = "session/list"

// startMuseCodeListingHostFn starts the listing host; a seam so tests drive
// the protocol over pipes without a real Muse binary.
var startMuseCodeListingHostFn = func(ctx context.Context, launcher string) (*museCodeMSPClient, error) {
	return startMuseCodeMSPWith(ctx, launcher, museCodeListingServeArgs, museCodeListingClientName, nil)
}

// museCodeInstalledLauncher is the installed `muse` launcher's absolute path,
// "" when Muse Code is not installed. A var so tests stand one in.
var museCodeInstalledLauncher = func() string {
	launcher := resolveMuseCodeExecutable()
	if !filepath.IsAbs(launcher) {
		return ""
	}
	if fi, err := os.Stat(launcher); err != nil || fi.IsDir() {
		return ""
	}
	return launcher
}

var museCodeSessionReader = &cliSessionReader{
	engine:  cliSessionEngineMuseCode,
	sources: []string{cliSessionSourceMuseListing},
	discover: func(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
		return discoverMuseCodeSessions(ctx, req)
	},
	probe: func(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
		return probeMuseCodeSession(ctx, req)
	},
}

// museCodeWriterEvidence: a live `muse` (or the build it runs,
// muse-bin-<version>) naming the session holds it open. There is no store to
// stat, so a Muse writer is never seen mid-turn by its writes
// (cli_session_outside_writer.go).
var museCodeWriterEvidence = cliSessionWriterEvidence{
	validID: isValidMuseCodeSessionID,
	isAgentProcess: func(name string) bool {
		return name == "muse" || strings.HasPrefix(name, "muse-bin-")
	},
}

// museCodeListedSession is one session a listing names.
type museCodeListedSession struct {
	id, cwd, title string
	updated        int64
}

// museCodeListing is one `session/list` answer.
type museCodeListing struct {
	sessions []museCodeListedSession
	// more: the host said it holds more than it listed (a cursor).
	more bool
}

// errMuseCodeListingUnsupported: the host has no `session/list`.
var errMuseCodeListingUnsupported = errors.New("muse serve has no session/list")

// listMuseCodeSessions starts the listing host and asks it for its sessions.
func listMuseCodeSessions(ctx context.Context, launcher string) (museCodeListing, error) {
	client, err := startMuseCodeListingHostFn(ctx, launcher)
	if err != nil {
		return museCodeListing{}, err
	}
	// Closed off the answer's path: ending the host's tree may wait on it for
	// seconds, which must not eat the discovery's budget.
	defer func() { go client.Close() }()
	raw, err := client.Call(ctx, mspMethodSessionList, map[string]any{})
	if err != nil {
		var rpcErr museCodeMSPError
		// JSON-RPC "method not found" (relay_follow_up_msp.go): a host older
		// than session/list.
		if errors.As(err, &rpcErr) && rpcErr.Code == mspErrMethodNotFound {
			return museCodeListing{}, errMuseCodeListingUnsupported
		}
		return museCodeListing{}, err
	}
	return parseMuseCodeSessionList(raw)
}

// parseMuseCodeSessionList reads a `session/list` result (see the file
// comment for the shape expected); errCliSessionUnrecognised for any other.
func parseMuseCodeSessionList(raw json.RawMessage) (museCodeListing, error) {
	var result struct {
		Sessions   *[]json.RawMessage `json:"sessions"`
		NextCursor *string            `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Sessions == nil {
		return museCodeListing{}, errCliSessionUnrecognised
	}
	out := museCodeListing{more: result.NextCursor != nil && *result.NextCursor != ""}
	for _, item := range *result.Sessions {
		var s struct {
			SessionID      string          `json:"sessionId"`
			WorkspaceRoot  string          `json:"workspaceRoot"`
			Name           string          `json:"name"`
			Title          string          `json:"title"`
			UpdatedAt      json.RawMessage `json:"updatedAt"`
			LastActivityAt json.RawMessage `json:"lastActivityAt"`
			CreatedAt      json.RawMessage `json:"createdAt"`
		}
		if json.Unmarshal(item, &s) != nil || !isValidMuseCodeSessionID(s.SessionID) {
			continue
		}
		updated := parseCliSessionTimeRaw(s.UpdatedAt)
		if updated == 0 {
			updated = parseCliSessionTimeRaw(s.LastActivityAt)
		}
		if updated == 0 {
			updated = parseCliSessionTimeRaw(s.CreatedAt)
		}
		out.sessions = append(out.sessions, museCodeListedSession{
			id: s.SessionID, cwd: localExtendedLengthPath(s.WorkspaceRoot),
			title: firstNonEmpty(strings.TrimSpace(s.Name), strings.TrimSpace(s.Title)), updated: updated,
		})
	}
	if len(out.sessions) == 0 && len(*result.Sessions) > 0 {
		return museCodeListing{}, errCliSessionUnrecognised
	}
	return out, nil
}

// museCodeListingFailure is how a listing that could not be taken is
// reported: the skip reason and the result's error code.
func museCodeListingFailure(ctx context.Context, err error) (skip, code string) {
	switch {
	case errors.Is(err, errMuseCodeListingUnsupported):
		return cliSessionSkipUnrecognised, cliSessionErrProviderUnsupported
	case errors.Is(err, errCliSessionUnrecognised):
		return cliSessionSkipUnrecognised, ""
	case ctx.Err() != nil:
		return cliSessionSkipTimeout, ""
	}
	return cliSessionSkipUnreadable, ""
}

// discoverMuseCodeSessions lists the sessions the host lists, updated within
// the request's look-back (one with no time is kept: nothing says it is old),
// newest first.
func discoverMuseCodeSessions(ctx context.Context, req cliSessionDiscoverRequest) cliSessionsDiscoverResult {
	res := newCliSessionsResult(cliSessionEngineMuseCode, req.now)
	src := cliSessionSourceMuseListing
	launcher := museCodeInstalledLauncher()
	if launcher == "" {
		res.skip(src, cliSessionSkipNotFound)
		res.Error = cliSessionErrNotInstalled
		return res
	}
	listing, err := listMuseCodeSessions(ctx, launcher)
	if err != nil {
		skip, code := museCodeListingFailure(ctx, err)
		res.skip(src, skip)
		res.Error = code
		res.Truncated = skip == cliSessionSkipTimeout
		return res
	}
	res.SourcesChecked = append(res.SourcesChecked, src)
	res.Truncated = listing.more
	since := req.since().UnixMilli()
	sessions := listing.sessions[:0]
	for _, s := range listing.sessions {
		if s.updated == 0 || s.updated >= since {
			sessions = append(sessions, s)
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].updated > sessions[j].updated })
	running := cliSessionRunningByID(museCodeWriterEvidence)
	for _, s := range sessions {
		row := cliSessionRow{
			SessionID:    s.id,
			Cwd:          s.cwd,
			LastActiveAt: s.updated,
			Editors:      []cliSessionEditorRef{},
			Resumable:    true,
			Running:      running[s.id],
		}
		row.Title, row.TitleSource, row.TitleSavedAt = cliSessionTitleOf(s.title, s.updated, "", "")
		res.Rows = append(res.Rows, row)
	}
	return res
}

// probeMuseCodeSession answers the probe of one session from a listing taken
// now: listed is resumable, unlisted is gone, and a listing that could not be
// taken says so (its error) instead of either — as does one that says it
// holds more than it listed, when the session is not on the part it listed.
func probeMuseCodeSession(ctx context.Context, req cliSessionProbeRequest) cliSessionProbeResult {
	res := cliSessionProbeResult{SessionID: req.sessionID, Writers: []cliSessionWriter{}, OpenInEditors: []string{}, TitleSource: cliSessionTitleNone}
	if !isValidMuseCodeSessionID(req.sessionID) {
		res.Error = cliSessionErrBadRequest
		return res
	}
	res.Writers = cliSessionProcessWriters(museCodeWriterEvidence, req.sessionID)
	launcher := museCodeInstalledLauncher()
	if launcher == "" {
		res.Error = cliSessionErrNotInstalled
		return res
	}
	listing, err := listMuseCodeSessions(ctx, launcher)
	if err != nil {
		skip, code := museCodeListingFailure(ctx, err)
		switch {
		case code != "":
			res.Error = code
		case skip == cliSessionSkipTimeout:
			res.Error = cliSessionErrTimeout
		default:
			res.Error = cliSessionErrInternal
		}
		return res
	}
	for _, s := range listing.sessions {
		if s.id != req.sessionID {
			continue
		}
		res.Exists, res.Resumable = true, true
		res.TranscriptWrittenAt = s.updated
		res.Title, res.TitleSource, _ = cliSessionTitleOf(s.title, s.updated, "", "")
		return res
	}
	if listing.more {
		// Not on the part listed, while the host says it holds more (a cursor
		// whose paging param is not known, see the file comment): the session
		// may be on a later page, so whether it exists is unknown. It is
		// answered the way a listing that could not be taken is: an error,
		// which terminal-service reads as a session it could not check, never
		// as one that is gone.
		res.Error = cliSessionErrInternal
		return res
	}
	// Not listed: a resume under this id would start a new session. A live
	// `muse` naming it still holds the id, so it exists, but never resumable.
	res.Exists = len(res.Writers) > 0
	return res
}
