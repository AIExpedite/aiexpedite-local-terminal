//go:build codexlive || groklive

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared by the Codex and Grok live follow-up certification runs
// (relay_follow_up_codex_certification_test.go,
// relay_follow_up_grok_certification_test.go). Never part of `go test ./...`.

const liveCertSecret = "test-secret"

// liveCertClient drives a real CLI through the device's own manager: frames
// go in through the manager's Send, and every stdout frame the manager
// publishes is kept in publish order.
type liveCertClient struct {
	t    *testing.T
	send func(string) error

	mu      sync.Mutex
	frames  []string
	onFrame func(f jsonRPCFrame, line string)
}

func (c *liveCertClient) publish(res resultMsg) {
	if res.Type != "codex_appserver_message" && res.Type != "grok_acp_message" {
		return
	}
	c.mu.Lock()
	c.frames = append(c.frames, res.Output)
	hook := c.onFrame
	c.mu.Unlock()
	if hook == nil {
		return
	}
	if f, ok := parseJSONRPCFrame(res.Output); ok {
		hook(f, res.Output)
	}
}

func (c *liveCertClient) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.frames...)
}

// find is the index of the first frame at or after `from` matching pred, or -1.
func (c *liveCertClient) find(from int, pred func(f jsonRPCFrame, line string) bool) int {
	frames := c.snapshot()
	for i := from; i < len(frames); i++ {
		if f, ok := parseJSONRPCFrame(frames[i]); ok && pred(f, frames[i]) {
			return i
		}
	}
	return -1
}

func (c *liveCertClient) count(from int, pred func(f jsonRPCFrame, line string) bool) int {
	n := 0
	frames := c.snapshot()
	for i := from; i < len(frames); i++ {
		if f, ok := parseJSONRPCFrame(frames[i]); ok && pred(f, frames[i]) {
			n++
		}
	}
	return n
}

// waitFor waits for a frame at or after `from` matching pred and returns its
// index; fatal on timeout.
func (c *liveCertClient) waitFor(what string, from int, timeout time.Duration, pred func(f jsonRPCFrame, line string) bool) int {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if i := c.find(from, pred); i >= 0 {
			return i
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// request sends one request (id is raw JSON, e.g. `3` or `"relay-turn-x"`)
// and waits for its answer.
func (c *liveCertClient) request(id, method string, params any, timeout time.Duration) jsonRPCFrame {
	c.t.Helper()
	mark := len(c.snapshot())
	c.sendRequest(id, method, params)
	key := jsonRPCIDKey(json.RawMessage(id))
	i := c.waitFor(method+" answer", mark, timeout, func(f jsonRPCFrame, _ string) bool { return f.isResponse() && f.idKey() == key })
	f, _ := parseJSONRPCFrame(c.snapshot()[i])
	if len(f.Error) > 0 {
		c.t.Fatalf("%s refused: %s", method, f.Error)
	}
	return f
}

func (c *liveCertClient) sendRequest(id, method string, params any) {
	c.t.Helper()
	p, _ := json.Marshal(params)
	if err := c.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + string(p) + `}`); err != nil {
		c.t.Fatalf("send %s: %v", method, err)
	}
}

func (c *liveCertClient) notify(method string) {
	c.t.Helper()
	if err := c.send(`{"jsonrpc":"2.0","method":"` + method + `"}`); err != nil {
		c.t.Fatalf("send %s: %v", method, err)
	}
}

func isResponseTo(id string) func(f jsonRPCFrame, _ string) bool {
	key := jsonRPCIDKey(json.RawMessage(id))
	return func(f jsonRPCFrame, _ string) bool { return f.isResponse() && f.idKey() == key }
}

// liveCertCommand is a `relay_follow_up` built and signed the way
// terminal-service builds it (publish.service.js, hmac.util.js).
func liveCertCommand(t *testing.T, ledger *relayFollowUpLedger, sessionID, attemptID, text, conversationID, expectedTurnID string) commandMsg {
	t.Helper()
	now := time.Now()
	cmd := commandMsg{
		ID: relayFollowUpEnvelopePrefix + attemptID, Type: relayFollowUpCommandType, SessionID: sessionID,
		Input: text, Ts: now.UnixMilli(), ConversationID: conversationID, ExpectedTurnID: expectedTurnID,
		FollowUpID: "fu-" + attemptID, FollowUpAttemptID: attemptID,
		FollowUpAttemptExpiresAt: now.Add(followUpAttemptWindow).UnixMilli(), FollowUpLedgerID: ledger.LedgerID(),
		WorkspaceID: "ws-cert", UID: "uid-cert", AgentID: "agent-cert",
	}
	canonical, err := signatureCanonical(cmd)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(liveCertSecret))
	mac.Write(canonical)
	cmd.Signature = hex.EncodeToString(mac.Sum(nil))
	if !verifySignature(cmd, liveCertSecret) {
		t.Fatal("the certification command does not verify")
	}
	return cmd
}

// installLiveCertLedger is a fresh test ledger (whose identity predates the
// run) and the ack capture.
func installLiveCertLedger(t *testing.T) (*relayFollowUpLedger, string, *ackLog) {
	t.Helper()
	resetFencedSessions(t)
	l, path := newEstablishedTestFollowUpLedger(t)
	prev := globalRelayFollowUpLedger
	globalRelayFollowUpLedger = l
	t.Cleanup(func() { globalRelayFollowUpLedger = prev })
	return l, path, captureFollowUpAcks(t)
}

// lastAck is the newest ack, fatal when there is none past `seen`.
func lastAck(t *testing.T, acks *ackLog, seen int) resultMsg {
	t.Helper()
	all := acks.snapshot()
	if len(all) <= seen {
		t.Fatalf("no ack was published (have %d)", len(all))
	}
	return all[len(all)-1]
}

// recordCertInterjection records an interjection the certification writes
// directly to stdin (bypassing the TurnRunning fence) on the prompt with
// rpcID, as the device records one it admitted. False when that prompt's
// answer was already read: the device could never have written then.
func recordCertInterjection(tr *liveFollowUpTracker, promptRPCID, attemptID, text string) bool {
	key := jsonRPCIDKey(json.RawMessage(promptRPCID))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, p := range tr.prompts {
		if p.rpcKey == key && !p.answered {
			p.interjections = append(p.interjections, grokInterjection{attemptID: attemptID, text: text})
			return true
		}
	}
	return false
}

func paramString(f jsonRPCFrame, path ...string) string {
	var v any
	if json.Unmarshal(f.Params, &v) != nil {
		return ""
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[p]
	}
	s, _ := v.(string)
	return s
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
