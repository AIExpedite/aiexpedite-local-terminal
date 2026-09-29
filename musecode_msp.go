// musecode_msp.go — a minimal client for Muse Code's own session protocol
// (MSP), served by `muse serve` over stdio.
//
// Why this exists:
//
//	Muse Code keeps its subscription quota (the 5-hour window, the weekly
//	block, the tier) and its model catalog only in memory: nothing on disk
//	records either, and `muse exec --json` — what the chat manager drives —
//	never emits them. MSP is the one documented surface that does:
//
//	  - `model/list`  the models the host accepts, with their effort scales.
//	    A query: no model call, answered in about two seconds.
//	  - `usage/read` / `usage/changed`  the last-observed quota. The host only
//	    learns it from a model response (Meta streams it beside the output),
//	    so a fresh host answers `{}` until a turn has started.
//
//	The wire is newline-delimited JSON-RPC 2.0: `initialize`, then the
//	`initialized` notification, then requests. `muse schema
//	generate-json-schema --out DIR` exports the full, versioned contract.
//
// Boundaries: the host runs with no session log (nothing lands in the user's
// Muse history), no workspace writes and no shell, in a private empty temp
// directory that is removed on close, and the whole process tree is killed on
// close. Server-initiated requests (approvals, questions) are never answered.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// museCodeServeArgs: memory-only sessions, no filesystem writes, no shell.
var museCodeServeArgs = []string{"serve", "--no-session-log", "--disable-write", "--disable-shell"}

// museCodeMSPMaxLine bounds one protocol line; a turn's frames are small, and
// the model catalog is a few KB.
const museCodeMSPMaxLine = 4 << 20

var errMuseCodeMSPClosed = errors.New("muse serve closed")

// museCodeMSPError is a JSON-RPC error reply. Only the code is kept: the
// message is vendor text and never leaves the device.
type museCodeMSPError struct{ Code int }

func (e museCodeMSPError) Error() string { return fmt.Sprintf("muse serve rpc error %d", e.Code) }

type museCodeMSPReply struct {
	result json.RawMessage
	err    error
}

// museCodeMSPClient is one `muse serve` child and its request table.
type museCodeMSPClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	tmpDir string

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan museCodeMSPReply
	closed  bool
	done    chan struct{}

	// onNotify receives every server notification (method, params). Called
	// from the reader goroutine; fixed at construction.
	onNotify func(method string, params json.RawMessage)
}

// Seam so tests drive the protocol over pipes without a real Muse binary.
var startMuseCodeMSPFn = startMuseCodeMSP

// startMuseCodeMSP launches `muse serve` for the given launcher path and
// completes the initialize handshake. onNotify (may be nil) sees every server
// notification. The caller must Close the client.
func startMuseCodeMSP(ctx context.Context, launcher string, onNotify func(string, json.RawMessage)) (*museCodeMSPClient, error) {
	tmpDir, err := os.MkdirTemp("", "aix-muse-probe-")
	if err != nil {
		return nil, err
	}
	executable := museCodeServeBinary(launcher)
	env := stripEnvPrefixes(os.Environ(), museCodeUnrelatedStripped)
	cmd := newOneShotCommand(context.Background(), executable, museCodeServeArgs, env, tmpDir)
	cmd.Stderr = io.Discard
	hideWindow(cmd)
	// Unix: own process group, so Close reaches every child.
	detachControllingTTY(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	c := newMuseCodeMSPClient(stdin, stdout, onNotify)
	c.cmd = cmd
	c.tmpDir = tmpDir
	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// newMuseCodeMSPClient wires a client to an already-running peer's stdio.
func newMuseCodeMSPClient(stdin io.WriteCloser, stdout io.Reader, onNotify func(string, json.RawMessage)) *museCodeMSPClient {
	c := &museCodeMSPClient{
		stdin:    stdin,
		onNotify: onNotify,
		pending:  map[int]chan museCodeMSPReply{},
		done:     make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

func (c *museCodeMSPClient) handshake(ctx context.Context) error {
	if _, err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "aiexpedite_usage_probe", "version": Version},
	}); err != nil {
		return err
	}
	return c.Notify("initialized")
}

func (c *museCodeMSPClient) readLoop(stdout io.Reader) {
	defer c.shutdown()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), museCodeMSPMaxLine)
	for scanner.Scan() {
		var frame struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			continue
		}
		switch {
		case frame.Method != "" && frame.ID == nil:
			if c.onNotify != nil {
				c.onNotify(frame.Method, frame.Params)
			}
		case frame.Method == "" && frame.ID != nil:
			c.mu.Lock()
			ch := c.pending[*frame.ID]
			delete(c.pending, *frame.ID)
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if frame.Error != nil {
				ch <- museCodeMSPReply{err: museCodeMSPError{Code: frame.Error.Code}}
			} else {
				ch <- museCodeMSPReply{result: frame.Result}
			}
		}
		// A server-initiated request (method AND id) is never answered.
	}
}

// shutdown fails every waiting request once the peer's stdout ends.
func (c *museCodeMSPClient) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		ch <- museCodeMSPReply{err: errMuseCodeMSPClosed}
		delete(c.pending, id)
	}
	close(c.done)
}

func (c *museCodeMSPClient) write(frame map[string]any) error {
	frame["jsonrpc"] = "2.0"
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// Call sends one request and waits for its reply or ctx.
func (c *museCodeMSPClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan museCodeMSPReply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errMuseCodeMSPClosed
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	frame := map[string]any{"id": id, "method": method}
	if params != nil {
		frame["params"] = params
	}
	if err := c.write(frame); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Notify sends a notification (no reply).
func (c *museCodeMSPClient) Notify(method string) error {
	return c.write(map[string]any{"method": method})
}

// Done is closed when the peer's stdout ends.
func (c *museCodeMSPClient) Done() <-chan struct{} { return c.done }

// Close ends the child and everything it started, then removes its temp dir.
func (c *museCodeMSPClient) Close() {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil {
		killAntigravityProcessTree(c.cmd)
		waited := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
	}
	if c.tmpDir != "" {
		_ = os.RemoveAll(c.tmpDir)
	}
}

// museCodeServeBinary returns the binary to run for `serve`. The launcher
// install keeps the real build beside the `muse` shim as
// muse-bin-<.muse-version>[.exe]; running it directly skips the Windows
// cmd.exe → PowerShell hop (~4.5s cold) and the launcher's self-update check,
// which a background probe must never trigger (a new build is ~440 MB).
// Anything else — another install layout, a missing build — runs the
// launcher itself.
func museCodeServeBinary(launcher string) string {
	version := readMuseCodeVersionFile(launcher)
	if version == "" {
		return launcher
	}
	name := "muse-bin-" + version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(filepath.Dir(launcher), name)
	if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
		return candidate
	}
	return launcher
}

// newMuseCodeCommandID mints the UUIDv7 MSP requires for a command's
// idempotency handle (`session/start`, `turn/start`).
func newMuseCodeCommandID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// museCodeListModels asks a running host for its model catalog.
func museCodeListModels(ctx context.Context, c *museCodeMSPClient) (cliAgentModelDiscovery, bool) {
	raw, err := c.Call(ctx, "model/list", map[string]any{})
	if err != nil {
		return cliAgentModelDiscovery{}, false
	}
	return parseMuseCodeModelList(raw)
}

// parseMuseCodeModelList maps a `model/list` result onto the discovery shape.
// Only rows from the live provider catalog are trusted as exhaustive; a
// bundled or fake catalog is reported, but as a floor.
func parseMuseCodeModelList(raw json.RawMessage) (cliAgentModelDiscovery, bool) {
	var result struct {
		Source string `json:"source"`
		Models []struct {
			ModelID      string   `json:"modelId"`
			DisplayLabel string   `json:"displayLabel"`
			IsDefault    bool     `json:"isDefault"`
			Variants     []string `json:"variants"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Models) == 0 {
		return cliAgentModelDiscovery{}, false
	}
	out := cliAgentModelDiscovery{Exhaustive: result.Source == "providerCatalog"}
	for _, m := range result.Models {
		id := strings.TrimSpace(m.ModelID)
		if id == "" || containsModelID(out.Models, id) {
			continue
		}
		detail := cliAgentModelDetail{ID: id}
		if label := strings.TrimSpace(m.DisplayLabel); label != "" && label != id {
			detail.Label = label
		}
		for _, effort := range m.Variants {
			if token, ok := normalizeEffortToken(effort); ok {
				detail.Efforts = appendEffort(detail.Efforts, token)
			}
		}
		out.Models = append(out.Models, detail)
		if m.IsDefault && out.DefaultModel == "" {
			out.DefaultModel = id
		}
	}
	if len(out.Models) == 0 {
		return cliAgentModelDiscovery{}, false
	}
	return out, true
}
