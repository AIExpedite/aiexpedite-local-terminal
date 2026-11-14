package main

import (
	"encoding/json"
	"testing"
)

// The device half of the SIGNED `expectedTurnId` on a live Codex follow-up
// (L1/L2 spec §2.2). The device writes it into the `turn/steer` it builds, so
// Codex itself refuses the message once that turn has ended; unsigned, it
// could be swapped for a later turn's id and move the message there.
//
// The canonicals and HMACs below are terminal-service's own literal vectors
// (src/utils/__tests__/hmac.expectedTurnId.test.js, secret "test-secret"),
// byte for byte: the two signers serialise independently, so only a shared
// vector can catch a position or gating difference.

func expectedTurnIDFixture() commandMsg {
	return commandMsg{
		ID:             "relayfollowup_att1",
		Ts:             1760400000000,
		Type:           "relay_follow_up",
		SessionID:      "sess-codex-1",
		Input:          "When you reach 30, also write BANANA.",
		ConversationID: "01a1d7fe-4d4a-7000-8000-000000000001",
	}
}

const expectedTurnIDVectorTurn = "01a1d7fe-4eae-7380-9393-d31ad77472b8"

func TestSignedExpectedTurnID_CanonicalMatchesTerminalService(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*commandMsg)
		canonical string
		nodeHMAC  string
	}{
		{
			// Absent: today's canonical, byte for byte.
			name:      "without expectedTurnId",
			mutate:    func(c *commandMsg) {},
			canonical: `{"id":"relayfollowup_att1","command":"","args":[],"ts":1760400000000,"type":"relay_follow_up","sessionID":"sess-codex-1","input":"When you reach 30, also write BANANA.","signal":"","conversationId":"01a1d7fe-4d4a-7000-8000-000000000001"}`,
			nodeHMAC:  "af6beaa995d66d2fb9693e7b80aa4cc286751a457590bf6e6282531f199a3d31",
		},
		{
			name:      "with expectedTurnId",
			mutate:    func(c *commandMsg) { c.ExpectedTurnID = expectedTurnIDVectorTurn },
			canonical: `{"id":"relayfollowup_att1","command":"","args":[],"ts":1760400000000,"type":"relay_follow_up","sessionID":"sess-codex-1","input":"When you reach 30, also write BANANA.","signal":"","conversationId":"01a1d7fe-4d4a-7000-8000-000000000001","expectedTurnId":"01a1d7fe-4eae-7380-9393-d31ad77472b8"}`,
			nodeHMAC:  "7af67b182e93a8e3cdf0ca79d8d8b1be04547b514865e61f2b2647f71a1d92be",
		},
		{
			// After deliveryExpiresAt and before cwdFence, so readOnly stays last.
			name: "with deliveryExpiresAt, cwdFence and readOnly",
			mutate: func(c *commandMsg) {
				c.ReadOnly = true
				c.CwdFence = "/Users/daniel/aiexpedite"
				c.ExpectedTurnID = expectedTurnIDVectorTurn
				c.DeliveryExpiresAt = 1760400120000
			},
			canonical: `{"id":"relayfollowup_att1","command":"","args":[],"ts":1760400000000,"type":"relay_follow_up","sessionID":"sess-codex-1","input":"When you reach 30, also write BANANA.","signal":"","conversationId":"01a1d7fe-4d4a-7000-8000-000000000001","deliveryExpiresAt":1760400120000,"expectedTurnId":"01a1d7fe-4eae-7380-9393-d31ad77472b8","cwdFence":"/Users/daniel/aiexpedite","readOnly":true}`,
			nodeHMAC:  "6fbedf81c16156786739327b1c6db234db7ca91f5ef3a4a923a0d52567d2e4ee",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := expectedTurnIDFixture()
			c.mutate(&cmd)
			canonical, mac := canonicalAndHMAC(t, cmd)
			if canonical != c.canonical {
				t.Fatalf("canonical\n got %s\nwant %s", canonical, c.canonical)
			}
			if mac != c.nodeHMAC {
				t.Fatalf("hmac %s, want %s", mac, c.nodeHMAC)
			}
			cmd.Signature = c.nodeHMAC
			if !verifySignature(cmd, signedExecIDSecret) {
				t.Fatal("terminal-service's signature does not verify")
			}
		})
	}
}

// A command signed with the id no longer verifies once it is stripped or
// swapped for another turn's.
func TestSignedExpectedTurnID_TamperingFailsTheSignature(t *testing.T) {
	cmd := expectedTurnIDFixture()
	cmd.ExpectedTurnID = expectedTurnIDVectorTurn
	cmd.Signature = "7af67b182e93a8e3cdf0ca79d8d8b1be04547b514865e61f2b2647f71a1d92be"
	if !verifySignature(cmd, signedExecIDSecret) {
		t.Fatal("the signed command does not verify")
	}
	stripped := cmd
	stripped.ExpectedTurnID = ""
	if verifySignature(stripped, signedExecIDSecret) {
		t.Fatal("a command with expectedTurnId stripped still verifies")
	}
	swapped := cmd
	swapped.ExpectedTurnID = "01a1d7fe-ffff-7380-9393-d31ad77472b8"
	if verifySignature(swapped, signedExecIDSecret) {
		t.Fatal("a command with another turn's id still verifies")
	}
}

// The wire field decodes into the signed field.
func TestSignedExpectedTurnID_DecodesFromTheWire(t *testing.T) {
	var cmd commandMsg
	if err := json.Unmarshal([]byte(`{"id":"relayfollowup_att1","type":"relay_follow_up","expectedTurnId":"turn-1","conversationId":"thread-1","attemptId":"att1"}`), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.ExpectedTurnID != "turn-1" || cmd.ConversationID != "thread-1" || cmd.FollowUpAttemptID != "att1" {
		t.Fatalf("decoded %+v", cmd)
	}
}
