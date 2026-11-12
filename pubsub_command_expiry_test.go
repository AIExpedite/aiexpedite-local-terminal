package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The signed delivery window on worktree built-ins (commandMaxAgeSec,
// signaturePayload.DeliveryExpiresAt). terminal-service sets `deliveryExpiresAt` to the time it
// waits for the answer, so a frame that reached this device late still runs
// while the server is waiting — and is refused once it is not.

const commandExpiryFixtureSecret = "test-secret"

// The same frame terminal-service's hmac.deliveryExpiresAt.test.js signs.
func commandExpiryFixtureCmd() commandMsg {
	return commandMsg{
		ID:      "cmd-expiry-1",
		Command: worktreeEnsureCommand,
		Args:    []string{`{"root":"/r"}`},
		Ts:      1760400000000,
		Type:    "execute",
	}
}

// ensureAged is a worktree ensure whose ts is `age` old and whose signed window
// is `window` (0 for none).
func ensureAged(age, window time.Duration) commandMsg {
	ts := time.Now().Add(-age).UnixMilli()
	cmd := commandMsg{ID: "c1", Command: worktreeEnsureCommand, Ts: ts, Type: "execute"}
	if window != 0 {
		cmd.DeliveryExpiresAt = ts + window.Milliseconds()
	}
	return cmd
}

func staleUnderOwnWindow(cmd commandMsg) bool {
	return isCommandStaleAfter(cmd.Ts, commandMaxAgeSec(cmd), commandMaxFutureSkewSec(cmd))
}

func TestCommandExpiry_SignedWindowAdmitsALateEnsure(t *testing.T) {
	// THE case from the evidence: an ensure delivered 90-113 s late, while the
	// server still waits up to 120 s, used to be refused at 60 s.
	if staleUnderOwnWindow(ensureAged(90*time.Second, 120*time.Second)) {
		t.Fatal("a 90 s old ensure inside its signed 120 s window was rejected as stale")
	}
	if got := commandMaxAgeSec(ensureAged(90*time.Second, 120*time.Second)); got != 120 {
		t.Fatalf("commandMaxAgeSec = %d, want 120", got)
	}
}

func TestCommandExpiry_PastItsWindowIsStale(t *testing.T) {
	if !staleUnderOwnWindow(ensureAged(130*time.Second, 120*time.Second)) {
		t.Fatal("a 130 s old ensure past its signed 120 s window was admitted")
	}
}

func TestCommandExpiry_WindowIsCapped(t *testing.T) {
	cmd := ensureAged(11*time.Minute, 30*time.Minute)
	if got := commandMaxAgeSec(cmd); got != int(maxSignedCommandWindow.Seconds()) {
		t.Fatalf("commandMaxAgeSec = %d, want the %v cap", got, maxSignedCommandWindow)
	}
	if !staleUnderOwnWindow(cmd) {
		t.Fatal("an 11 min old ensure was admitted under a 30 min window; the 10 min cap must hold")
	}
}

func TestCommandExpiry_NoUsableWindowFallsBackToTheGenericGate(t *testing.T) {
	ts := time.Now().Add(-61 * time.Second).UnixMilli()
	for name, deliveryExpiresAt := range map[string]int64{
		"absent":         0,
		"equal to ts":    ts,
		"before ts":      ts - 1000,
		"negative value": -1,
	} {
		cmd := commandMsg{ID: "c1", Command: worktreeEnsureCommand, Ts: ts, DeliveryExpiresAt: deliveryExpiresAt}
		if got := commandMaxAgeSec(cmd); got != maxCommandAgeSec {
			t.Errorf("%s: commandMaxAgeSec = %d, want %d", name, got, maxCommandAgeSec)
		}
		if !staleUnderOwnWindow(cmd) {
			t.Errorf("%s: a 61 s old ensure was admitted without a usable window", name)
		}
	}
}

func TestCommandExpiry_IgnoredOnEveryOtherCommand(t *testing.T) {
	// A future server sending the field on another command kind must widen
	// nothing: only the worktree built-ins honour it.
	ts := time.Now().Add(-61 * time.Second).UnixMilli()
	cmd := commandMsg{ID: "c1", Command: "git", Args: []string{"status"}, Type: "execute", Ts: ts, DeliveryExpiresAt: ts + 120_000}
	if got := commandMaxAgeSec(cmd); got != maxCommandAgeSec {
		t.Fatalf("commandMaxAgeSec = %d on a plain execute, want %d", got, maxCommandAgeSec)
	}
	if !staleUnderOwnWindow(cmd) {
		t.Fatal("a 61 s old plain execute was admitted because it carried deliveryExpiresAt")
	}
}

func TestCommandExpiry_EveryWorktreeBuiltInHonoursIt(t *testing.T) {
	for _, name := range []string{
		worktreeEnsureCommand, worktreeListCommand, worktreeRemoveCommand,
		worktreeEvictDepsCommand, rootRestoreCommand,
	} {
		cmd := ensureAged(90*time.Second, 120*time.Second)
		cmd.Command = name
		if staleUnderOwnWindow(cmd) {
			t.Errorf("%s: rejected inside its signed window", name)
		}
	}
}

func TestCommandExpiry_FutureSkewBoundIsIndependent(t *testing.T) {
	// Lengthening how OLD a frame may be must not lengthen how far in the
	// future its ts may sit.
	ts := time.Now().Add(90 * time.Second).UnixMilli()
	cmd := commandMsg{ID: "c1", Command: worktreeEnsureCommand, Ts: ts, DeliveryExpiresAt: ts + 120_000}
	if got := commandMaxFutureSkewSec(cmd); got != maxCommandAgeSec {
		t.Fatalf("commandMaxFutureSkewSec = %d, want %d", got, maxCommandAgeSec)
	}
	if !staleUnderOwnWindow(cmd) {
		t.Fatal("a ts 90 s in the future was admitted on the strength of a valid expiry")
	}
}

func TestCommandExpiry_CanonicalMatchesNode(t *testing.T) {
	// The same literals and HMACs as terminal-service
	// src/utils/__tests__/hmac.deliveryExpiresAt.test.js.
	cases := []struct {
		name      string
		mutate    func(*commandMsg)
		canonical string
		nodeHMAC  string
	}{
		{
			name:      "absent: byte-identical to the pre-expiry canonical",
			mutate:    func(*commandMsg) {},
			canonical: `{"id":"cmd-expiry-1","command":"__worktree_ensure__","args":["{\"root\":\"/r\"}"],"ts":1760400000000,"type":"execute","sessionID":"","input":"","signal":""}`,
			nodeHMAC:  "d77cd9cc755db8954791848fc9d52f3fbf2343fc7c4d8969665a666967ffaa0e",
		},
		{
			name:      "deliveryExpiresAt after signal",
			mutate:    func(c *commandMsg) { c.DeliveryExpiresAt = 1760400120000 },
			canonical: `{"id":"cmd-expiry-1","command":"__worktree_ensure__","args":["{\"root\":\"/r\"}"],"ts":1760400000000,"type":"execute","sessionID":"","input":"","signal":"","deliveryExpiresAt":1760400120000}`,
			nodeHMAC:  "f05c30a2a246fc9f6299bd45e949f1ca2397fd23a53641bc95f2ef07efff5c1b",
		},
		{
			name: "deliveryExpiresAt before cwdFence, readOnly last",
			mutate: func(c *commandMsg) {
				c.DeliveryExpiresAt = 1760400120000
				c.CwdFence = "/Users/daniel/aiexpedite"
				c.ReadOnly = true
			},
			canonical: `{"id":"cmd-expiry-1","command":"__worktree_ensure__","args":["{\"root\":\"/r\"}"],"ts":1760400000000,"type":"execute","sessionID":"","input":"","signal":"","deliveryExpiresAt":1760400120000,"cwdFence":"/Users/daniel/aiexpedite","readOnly":true}`,
			nodeHMAC:  "dbbebb453b231a7f9dbfa9123dd1956e461d0f43417552a1b9435cc7bb0175fd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := commandExpiryFixtureCmd()
			tc.mutate(&cmd)
			got, err := signatureCanonical(cmd)
			if err != nil {
				t.Fatalf("signatureCanonical: %v", err)
			}
			if string(got) != tc.canonical {
				t.Fatalf("canonical =\n%s\nwant\n%s", got, tc.canonical)
			}
			if sig := signFixture(t, cmd, commandExpiryFixtureSecret); sig != tc.nodeHMAC {
				t.Fatalf("HMAC = %s, want Node's %s", sig, tc.nodeHMAC)
			}
			cmd.Signature = tc.nodeHMAC
			if !verifySignature(cmd, commandExpiryFixtureSecret) {
				t.Fatal("verifySignature rejected Node's signature")
			}
		})
	}
}

func TestCommandExpiry_DecodesBesideTheFollowUpExpiry(t *testing.T) {
	// The follow-up protocol already owns the `expiresAt` tag (unsigned).
	// Both must decode, and only the delivery window reaches the canonical.
	var cmd commandMsg
	if err := json.Unmarshal([]byte(`{"id":"c1","command":"__worktree_ensure__","ts":1,"expiresAt":5,"deliveryExpiresAt":7}`), &cmd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cmd.FollowUpAttemptExpiresAt != 5 || cmd.DeliveryExpiresAt != 7 {
		t.Fatalf("decoded follow-up %d / delivery %d, want 5 / 7", cmd.FollowUpAttemptExpiresAt, cmd.DeliveryExpiresAt)
	}
	canonical, err := signatureCanonical(cmd)
	if err != nil {
		t.Fatalf("signatureCanonical: %v", err)
	}
	if strings.Contains(string(canonical), `"expiresAt"`) || !strings.Contains(string(canonical), `"deliveryExpiresAt":7`) {
		t.Fatalf("canonical = %s", canonical)
	}
}

func TestCommandExpiry_TamperedExpiryFailsTheSignature(t *testing.T) {
	cmd := commandExpiryFixtureCmd()
	cmd.DeliveryExpiresAt = 1760400120000
	cmd.Signature = signFixture(t, cmd, commandExpiryFixtureSecret)
	if !verifySignature(cmd, commandExpiryFixtureSecret) {
		t.Fatal("the untampered frame must verify")
	}

	stretched := cmd
	stretched.DeliveryExpiresAt = cmd.Ts + 10*time.Minute.Milliseconds()
	if verifySignature(stretched, commandExpiryFixtureSecret) {
		t.Fatal("a stretched deliveryExpiresAt kept a valid signature")
	}

	// Appending the field to a frame signed without it breaks it too.
	plain := commandExpiryFixtureCmd()
	plain.Signature = signFixture(t, plain, commandExpiryFixtureSecret)
	plain.DeliveryExpiresAt = plain.Ts + 120_000
	if verifySignature(plain, commandExpiryFixtureSecret) {
		t.Fatal("an deliveryExpiresAt appended to an unexpiring frame kept a valid signature")
	}
}

func TestCommandExpiry_StaleMessageNamesTheAppliedWindowAndSlots(t *testing.T) {
	msg := staleCommandRejectionMessage(113, 120, 10, 10)
	// The prefix the 7-day "too old" verification query matches on.
	if !strings.HasPrefix(msg, "Command rejected: too old (") {
		t.Fatalf("message lost its prefix: %q", msg)
	}
	for _, want := range []string{"113 seconds", "max 120 seconds", "10 of 10 receive slots busy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestCommandExpiry_ReceiveOccupancyReadsTheCounters(t *testing.T) {
	prevBusy, prevSlots := pubsubReceiveBusy.Load(), pubsubReceiveSlots.Load()
	t.Cleanup(func() {
		pubsubReceiveBusy.Store(prevBusy)
		pubsubReceiveSlots.Store(prevSlots)
	})
	pubsubReceiveSlots.Store(10)
	pubsubReceiveBusy.Store(0)
	pubsubReceiveBusy.Add(3)
	if busy, slots := pubsubReceiveOccupancy(); busy != 3 || slots != 10 {
		t.Fatalf("occupancy = %d of %d, want 3 of 10", busy, slots)
	}
}
