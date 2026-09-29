package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"
)

// The relay broker's `ackedTurnIds` watermark is SIGNED: terminal-service's
// signCommand appends `canonical.ackedTurnIds` after conversationId, only for
// a non-empty array. These fixtures are the Node canonical written out
// literally (JSON.stringify output) and the HMAC Node produced for it with
// relayAckFixtureSecret, so a Go-side reorder, a missing omitempty or an
// HTML-escaped `<`/`&` fails here rather than as "invalid signature" on every
// relayed turn in production.

const relayAckFixtureSecret = "relay-fixture-secret"

func relayAckFixtureCmd() commandMsg {
	return commandMsg{
		ID:        "relayturn_t-42",
		Ts:        1790000000000,
		Type:      "claude_native_send",
		SessionID: "sess-relay-1",
		Input:     `fix the <build> & ship "it"`,
		// An ordinary (non env-setup) command's cwd is NOT signed.
		Cwd: `C:\repo`,
	}
}

func signFixture(t *testing.T, cmd commandMsg, secret string) string {
	t.Helper()
	canonical, err := signatureCanonical(cmd)
	if err != nil {
		t.Fatalf("signatureCanonical: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAckWatermark_CanonicalMatchesNode(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*commandMsg)
		canonical string
		nodeHMAC  string
	}{
		{
			name:      "watermark after signal",
			mutate:    func(c *commandMsg) { c.AckedTurnIds = []string{"t-40", "t-41"} },
			canonical: `{"id":"relayturn_t-42","command":"","args":[],"ts":1790000000000,"type":"claude_native_send","sessionID":"sess-relay-1","input":"fix the <build> & ship \"it\"","signal":"","ackedTurnIds":["t-40","t-41"]}`,
			nodeHMAC:  "5bccf529c2cf3d6a673ca8aba40e7b0d6d237e7f015ea1f9ad3363d328b36da0",
		},
		{
			name: "watermark after conversationId",
			mutate: func(c *commandMsg) {
				c.ConversationID = "conv-9"
				c.AckedTurnIds = []string{"t-40", "t-41"}
			},
			canonical: `{"id":"relayturn_t-42","command":"","args":[],"ts":1790000000000,"type":"claude_native_send","sessionID":"sess-relay-1","input":"fix the <build> & ship \"it\"","signal":"","conversationId":"conv-9","ackedTurnIds":["t-40","t-41"]}`,
			nodeHMAC:  "e13959b587bb9481da9be6cf4d87d7f6cfb3d5470d9a8e2ddc1462a860f6a0e2",
		},
		{
			// Absent → byte-identical to the pre-relay canonical.
			name:      "absent watermark",
			mutate:    func(c *commandMsg) {},
			canonical: `{"id":"relayturn_t-42","command":"","args":[],"ts":1790000000000,"type":"claude_native_send","sessionID":"sess-relay-1","input":"fix the <build> & ship \"it\"","signal":""}`,
			nodeHMAC:  "9db954d7bf7c4e128cb76fb3411c3b061dd58f48c7fdc02aa22f0bd45ed0070e",
		},
		{
			// Node omits an empty array too.
			name:      "empty watermark",
			mutate:    func(c *commandMsg) { c.AckedTurnIds = []string{} },
			canonical: `{"id":"relayturn_t-42","command":"","args":[],"ts":1790000000000,"type":"claude_native_send","sessionID":"sess-relay-1","input":"fix the <build> & ship \"it\"","signal":""}`,
			nodeHMAC:  "9db954d7bf7c4e128cb76fb3411c3b061dd58f48c7fdc02aa22f0bd45ed0070e",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := relayAckFixtureCmd()
			tc.mutate(&cmd)
			got, err := signatureCanonical(cmd)
			if err != nil {
				t.Fatalf("signatureCanonical: %v", err)
			}
			if string(got) != tc.canonical {
				t.Fatalf("canonical mismatch\n got: %s\nwant: %s", got, tc.canonical)
			}
			// The HMAC of the literal, computed here, must equal Node's.
			mac := hmac.New(sha256.New, []byte(relayAckFixtureSecret))
			mac.Write([]byte(tc.canonical))
			if h := hex.EncodeToString(mac.Sum(nil)); h != tc.nodeHMAC {
				t.Fatalf("HMAC of the literal = %s, Node produced %s", h, tc.nodeHMAC)
			}
			cmd.Signature = tc.nodeHMAC
			if !verifySignature(cmd, relayAckFixtureSecret) {
				t.Fatal("verifySignature rejected Node's signature")
			}
		})
	}
}

func TestAckWatermark_UnsignedOrEditedRejected(t *testing.T) {
	// Signed WITHOUT a watermark, then one is added in transit.
	cmd := relayAckFixtureCmd()
	cmd.Signature = signFixture(t, cmd, relayAckFixtureSecret)
	cmd.AckedTurnIds = []string{"t-40"}
	if verifySignature(cmd, relayAckFixtureSecret) {
		t.Fatal("a watermark added after signing verified")
	}

	// Signed WITH a watermark, then an id is swapped / appended / dropped.
	signed := relayAckFixtureCmd()
	signed.AckedTurnIds = []string{"t-40", "t-41"}
	signed.Signature = signFixture(t, signed, relayAckFixtureSecret)
	if !verifySignature(signed, relayAckFixtureSecret) {
		t.Fatal("the untouched signed watermark did not verify")
	}
	for _, edited := range [][]string{{"t-40", "t-99"}, {"t-40", "t-41", "t-42"}, {"t-40"}, {"t-41", "t-40"}, nil} {
		c := signed
		c.AckedTurnIds = edited
		if verifySignature(c, relayAckFixtureSecret) {
			t.Fatalf("edited watermark %v verified", edited)
		}
	}

	// An unverified watermark deletes nothing.
	inbox := newTestRelayInbox(t)
	acceptTestRow(t, inbox, "sess-relay-1", "t-40")
	if n := applyRelayAckWatermark(inbox, signed, false); n != 0 {
		t.Fatalf("unsigned watermark deleted %d rows", n)
	}
	if len(inbox.snapshot()) != 1 {
		t.Fatal("unsigned watermark removed a row")
	}
}

func TestAckWatermark_OverCapRefused(t *testing.T) {
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t-%d", i)
		}
		return out
	}
	atCap := commandMsg{SessionID: "s1", AckedTurnIds: ids(relayAckWatermarkMax)}
	if err := checkRelayAckWatermark(atCap); err != nil {
		t.Fatalf("%d ids refused: %v", relayAckWatermarkMax, err)
	}
	over := commandMsg{SessionID: "s1", AckedTurnIds: ids(relayAckWatermarkMax + 1)}
	if err := checkRelayAckWatermark(over); err == nil {
		t.Fatalf("%d ids accepted", relayAckWatermarkMax+1)
	}

	// Never truncated: an over-cap watermark deletes NOTHING, not the first 32.
	inbox := newTestRelayInbox(t)
	acceptTestRow(t, inbox, "s1", "t-0")
	if n := applyRelayAckWatermark(inbox, over, true); n != 0 {
		t.Fatalf("over-cap watermark deleted %d rows", n)
	}
	if len(inbox.snapshot()) != 1 {
		t.Fatal("over-cap watermark removed a row")
	}
}

func TestAckWatermark_ConfirmedIdDeletesExactlyItsOwnRow(t *testing.T) {
	inbox := newTestRelayInbox(t)
	acceptTestRow(t, inbox, "s1", "t1")
	acceptTestRow(t, inbox, "s1", "t2")
	acceptTestRow(t, inbox, "s2", "t1") // same turn id, another session

	cmd := commandMsg{SessionID: "s1", AckedTurnIds: []string{"t1", "t-unknown"}}
	if n := applyRelayAckWatermark(inbox, cmd, true); n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	rows := inbox.snapshot()
	if _, ok := rows[relayTurnKey{"s1", "t1"}]; ok {
		t.Fatal("the confirmed row survived")
	}
	for _, k := range []relayTurnKey{{"s1", "t2"}, {"s2", "t1"}} {
		if _, ok := rows[k]; !ok {
			t.Fatalf("row %v was deleted by a watermark that did not name it", k)
		}
	}

	// And the deletion is durable.
	inbox.release()
	reloaded := newRelayTurnInbox(inbox.path)
	t.Cleanup(reloaded.release)
	if got := len(reloaded.snapshot()); got != 2 {
		t.Fatalf("reloaded inbox has %d rows, want 2", got)
	}
}

/* ---- shared helpers for the relay inbox tests ---- */

func newTestRelayInbox(t *testing.T) *relayTurnInbox {
	t.Helper()
	path := filepath.Join(t.TempDir(), relayTurnInboxFileName)
	inbox := newRelayTurnInbox(func() string { return path })
	t.Cleanup(inbox.release)
	return inbox
}

// acceptTestRow drives one delivery of (sessionID, turnID) to `accepted`.
func acceptTestRow(t *testing.T, inbox *relayTurnInbox, sessionID, turnID string) {
	t.Helper()
	cmd := commandMsg{ID: relayTurnEnvelopePrefix + turnID, SessionID: sessionID, WorkspaceID: "ws-1", UID: "u-1"}
	decision, _ := inbox.Begin(cmd, turnID, "agent-1")
	if decision != relayTurnRun {
		t.Fatalf("Begin(%s/%s) = %v, want run", sessionID, turnID, decision)
	}
	if _, ok, err := inbox.Accept(sessionID, turnID); !ok || err != nil {
		t.Fatalf("Accept(%s/%s) did not transition: %v", sessionID, turnID, err)
	}
	inbox.Finish(sessionID, turnID)
}
