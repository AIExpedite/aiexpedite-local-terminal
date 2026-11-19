package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	muA = "0199b000-0000-7000-8000-00000000000a"
	muB = "0199b000-0000-7000-8000-00000000000b"
	muC = "0199b000-0000-7000-8000-00000000000c"
	muX = "0199b000-0000-7000-8000-0000000000ff"
)

// fakeMuseListingHost is a pipe-backed `muse serve` answering the listing:
// `session/list` with list (or the JSON-RPC error code errCode).
type fakeMuseListingHost struct {
	list    any
	errCode int

	mu      sync.Mutex
	methods []string
}

func (f *fakeMuseListingHost) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *fakeMuseListingHost) start(ctx context.Context, _ string) (*museCodeMSPClient, error) {
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	go f.serve(serverIn, serverOut)
	c := newMuseCodeMSPClient(clientOut, clientIn, nil)
	c.clientName = museCodeListingClientName
	if err := c.handshake(ctx); err != nil {
		_ = clientOut.Close()
		return nil, err
	}
	return c, nil
}

func (f *fakeMuseListingHost) serve(in io.Reader, out io.WriteCloser) {
	defer out.Close()
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		f.mu.Unlock()
		if req.ID == nil {
			continue
		}
		switch {
		case req.Method == "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{"serverInfo": map[string]any{"version": "1.4.20"}}})
		case req.Method == mspMethodSessionList && f.errCode != 0:
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": f.errCode, "message": "nope"}})
		case req.Method == mspMethodSessionList:
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": f.list})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "unexpected"}})
		}
	}
}

// useMuseListingHost stands host in for `muse serve` and an installed Muse
// Code for the rest of the test.
func useMuseListingHost(t *testing.T, host *fakeMuseListingHost, installed bool) {
	t.Helper()
	prevStart, prevLauncher := startMuseCodeListingHostFn, museCodeInstalledLauncher
	startMuseCodeListingHostFn = host.start
	museCodeInstalledLauncher = func() string {
		if installed {
			return "/opt/muse/bin/muse"
		}
		return ""
	}
	t.Cleanup(func() { startMuseCodeListingHostFn, museCodeInstalledLauncher = prevStart, prevLauncher })
}

func museListing(now time.Time) map[string]any {
	return map[string]any{"sessions": []any{
		map[string]any{"sessionId": muA, "workspaceRoot": `\\?\C:\work\app`, "name": "Tidy the release notes", "updatedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		map[string]any{"sessionId": muB, "workspaceRoot": "/w/b", "title": "Second", "lastActivityAt": now.Add(-2 * time.Hour).UnixMilli()},
		map[string]any{"sessionId": muC, "workspaceRoot": "/w/c"},
		map[string]any{"sessionId": "not-a-uuid", "name": "Never listed"},
		map[string]any{"sessionId": muX, "name": "Too old", "updatedAt": now.Add(-40 * 24 * time.Hour).Unix()},
	}}
}

// The listing host's argv keeps the usage probe's limits (no writes, no
// shell) but not its memory-only session log, which would list nothing saved.
func TestMuseCodeListingHostArgv(t *testing.T) {
	joined := strings.Join(museCodeListingServeArgs, " ")
	if joined != "serve --disable-write --disable-shell" {
		t.Fatalf("listing host argv = %q", joined)
	}
}

// Only what the host lists is a row; the host is asked nothing but the
// handshake and `session/list` (no session is started, resumed or read).
func TestMuseCodeDiscoveryListsOnlyListedSessions(t *testing.T) {
	now := time.Now()
	host := &fakeMuseListingHost{list: museListing(now)}
	useMuseListingHost(t, host, true)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := runCliSessionDiscover(context.Background(), museCodeSessionReader, newCliSessionDiscoverRequest(30, 500))
	ids := []string{}
	for _, r := range res.Rows {
		ids = append(ids, r.SessionID)
	}
	if strings.Join(ids, ",") != strings.Join([]string{muA, muB, muC}, ",") {
		t.Fatalf("rows = %v, want A, B, then C (no time)", ids)
	}
	a := res.Rows[0]
	if a.Title != "Tidy the release notes" || a.TitleSource != cliSessionTitleSavedTitle || a.Cwd != `C:\work\app` || !a.Resumable || a.Engine != cliSessionEngineMuseCode {
		t.Fatalf("A = %+v", a)
	}
	if res.Rows[1].Title != "Second" || res.Rows[1].LastActiveAt == 0 || res.Rows[2].TitleSource != cliSessionTitleNone {
		t.Fatalf("rows = %+v", res.Rows)
	}
	if res.Error != "" || res.Truncated || res.SourcesChecked[0] != cliSessionSourceMuseListing {
		t.Fatalf("result = %+v", res)
	}
	if got := strings.Join(host.seen(), ","); got != "initialize,initialized,session/list" {
		t.Fatalf("the host was asked %s", got)
	}
	host.list = map[string]any{"sessions": []any{map[string]any{"sessionId": muA}}, "nextCursor": "c2"}
	if res := discoverMuseCodeSessions(context.Background(), newCliSessionDiscoverRequest(30, 500)); !res.Truncated {
		t.Fatalf("a listing with more behind a cursor = %+v, want truncated", res)
	}
}

// A host without `session/list` offers no list at all: the engine is
// providerUnsupported, never "no sessions", and nothing is ever resumable.
func TestMuseCodeHostWithoutSessionListIsUnsupported(t *testing.T) {
	useMuseListingHost(t, &fakeMuseListingHost{errCode: -32601}, true)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	res := discoverMuseCodeSessions(context.Background(), newCliSessionDiscoverRequest(30, 500))
	if res.Error != cliSessionErrProviderUnsupported || skipReason(res, cliSessionSourceMuseListing) != cliSessionSkipUnrecognised || len(res.Rows) != 0 {
		t.Fatalf("result = %+v, want providerUnsupported", res)
	}
	probe := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: muA})
	if probe.Error != cliSessionErrProviderUnsupported || probe.Resumable {
		t.Fatalf("probe = %+v, want providerUnsupported, never resumable", probe)
	}
}

func TestMuseCodeUnrecognisedListingAndMissingInstall(t *testing.T) {
	host := &fakeMuseListingHost{list: map[string]any{"conversations": []any{}}}
	useMuseListingHost(t, host, true)
	pinCliSessionProcesses(t, func() []cliSessionProcess { return nil })
	if res := discoverMuseCodeSessions(context.Background(), newCliSessionDiscoverRequest(30, 500)); skipReason(res, cliSessionSourceMuseListing) != cliSessionSkipUnrecognised || res.Error != "" {
		t.Fatalf("an unknown listing shape = %+v, want unrecognised", res)
	}
	host.list = map[string]any{"sessions": []any{map[string]any{"id": muA}}}
	if res := discoverMuseCodeSessions(context.Background(), newCliSessionDiscoverRequest(30, 500)); skipReason(res, cliSessionSourceMuseListing) != cliSessionSkipUnrecognised {
		t.Fatalf("sessions of an unknown shape = %+v, want unrecognised", res)
	}
	useMuseListingHost(t, host, false)
	res := discoverMuseCodeSessions(context.Background(), newCliSessionDiscoverRequest(30, 500))
	if res.Error != cliSessionErrNotInstalled || skipReason(res, cliSessionSourceMuseListing) != cliSessionSkipNotFound {
		t.Fatalf("no Muse Code = %+v, want notInstalled", res)
	}
	if p := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: muA}); p.Error != cliSessionErrNotInstalled || p.Resumable {
		t.Fatalf("probe without Muse Code = %+v", p)
	}
}

// An id Muse does not list would start a NEW session if resumed: the probe
// confirms only a session the host lists right then, whatever the id looks
// like, and a live `muse` naming an unlisted id still never makes it
// resumable.
func TestMuseCodeProbeNeverGuessesAnID(t *testing.T) {
	now := time.Now()
	useMuseListingHost(t, &fakeMuseListingHost{list: museListing(now)}, true)
	pinCliSessionProcesses(t, func() []cliSessionProcess {
		return []cliSessionProcess{
			{PID: 51, Name: "muse-bin-1.4.20-r8619.1", Args: []string{"muse-bin-1.4.20-R8619.1", "--session-id", muA}},
			{PID: 52, Name: "muse", Args: []string{"muse", "--session-id", "0199b000-0000-7000-8000-000000000123"}},
		}
	})
	listed := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: muA})
	if !listed.Exists || !listed.Resumable || listed.Title != "Tidy the release notes" || listed.TranscriptWrittenAt == 0 || len(listed.Writers) != 1 || listed.Writers[0].PID != 51 {
		t.Fatalf("listed = %+v", listed)
	}
	unlisted := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: "0199b000-0000-7000-8000-000000000999"})
	if unlisted.Exists || unlisted.Resumable || unlisted.Error != "" {
		t.Fatalf("an unlisted id = %+v, want gone, never resumable", unlisted)
	}
	held := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: "0199b000-0000-7000-8000-000000000123"})
	if !held.Exists || held.Resumable {
		t.Fatalf("an unlisted id a live muse names = %+v, want held but not resumable", held)
	}
	if bad := probeMuseCodeSession(context.Background(), cliSessionProbeRequest{sessionID: "s-1"}); bad.Error != cliSessionErrBadRequest {
		t.Fatalf("a non-UUID id = %+v", bad)
	}
}
