package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// The device half of terminal-service #348: `executionId` is part of the
// signature canonical, and this build advertises that so the server will send
// it.
//
// Every canonical and HMAC literal below was produced by the NODE signer
// (terminal-service/src/utils/hmac.util.js `signCommand`) against the same
// fixture, not by reading this file's code. That is the only assertion that
// can actually fail usefully: the two implementations serialise independently,
// and a position or gating difference shows up as a different canonical even
// though both sides "include the field".

const signedExecIDSecret = "test-secret"

func signedExecIDFixture() commandMsg {
	return commandMsg{
		ID:      "cmd-exec-1",
		Command: "npm",
		Args:    []string{"run", "build"},
		Ts:      1790000000000,
		Type:    "execute",
	}
}

func canonicalAndHMAC(t *testing.T, cmd commandMsg) (string, string) {
	t.Helper()
	canonical, err := signatureCanonical(cmd)
	if err != nil {
		t.Fatalf("signatureCanonical: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(signedExecIDSecret))
	mac.Write(canonical)
	return string(canonical), hex.EncodeToString(mac.Sum(nil))
}

func TestSignedExecutionID_CanonicalMatchesNode(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*commandMsg)
		canonical string
		nodeHMAC  string
	}{
		{
			name:      "executionId only",
			mutate:    func(c *commandMsg) { c.ExecutionID = "exec-abc123" },
			canonical: `{"id":"cmd-exec-1","command":"npm","args":["run","build"],"ts":1790000000000,"type":"execute","sessionID":"","input":"","signal":"","executionId":"exec-abc123"}`,
			nodeHMAC:  "e2453f4ef9b5b18ce325e49cbac4d4df1507d388a70c6d251d115c6c912e4cb1",
		},
		{
			// the ordering case: riskLevel, THEN executionId, THEN cwd
			name: "riskLevel + executionId + cwd (env-setup shape)",
			mutate: func(c *commandMsg) {
				c.RiskLevel = "destructive"
				c.ExecutionID = "exec-abc123"
				c.Cwd = `C:\repo`
			},
			canonical: `{"id":"cmd-exec-1","command":"npm","args":["run","build"],"ts":1790000000000,"type":"execute","sessionID":"","input":"","signal":"","riskLevel":"destructive","executionId":"exec-abc123","cwd":"C:\\repo"}`,
			nodeHMAC:  "22530f5ae317ec4f99404ccf9641dddb9a7970874d616bd941a118c4a7a5b1c7",
		},
		{
			// THE UPGRADE-WINDOW CASE. A command carrying no executionId must
			// sign exactly what it signed before this field existed, or every
			// ordinary command breaks while the fleet updates.
			name:      "none of them (back-compat)",
			mutate:    func(c *commandMsg) {},
			canonical: `{"id":"cmd-exec-1","command":"npm","args":["run","build"],"ts":1790000000000,"type":"execute","sessionID":"","input":"","signal":""}`,
			nodeHMAC:  "38f9857295840edff0e9b62213968aa55bc3b6984073742abd59a2699762010a",
		},
		{
			name: "executionId + readOnly + cwd",
			mutate: func(c *commandMsg) {
				c.Type = "session_start"
				c.ExecutionID = "exec-ro"
				c.ReadOnly = true
				c.Cwd = `C:\repo`
			},
			canonical: `{"id":"cmd-exec-1","command":"npm","args":["run","build"],"ts":1790000000000,"type":"session_start","sessionID":"","input":"","signal":"","executionId":"exec-ro","cwd":"C:\\repo","readOnly":true}`,
			nodeHMAC:  "b0f85cf3034cafed35834e661e7d89f326714ef1b7e6c27f51cfd0d671e5e822",
		},
		{
			// executionId is UNGATED where cwd is gated: a plain execute with a
			// cwd signs the id and drops the cwd. Getting this backwards (gating
			// the id on the same condition as the cwd) is the easy mistake, and
			// it only shows up here.
			name: "executionId with an unsigned cwd",
			mutate: func(c *commandMsg) {
				c.ExecutionID = "exec-abc123"
				c.Cwd = `C:\repo`
			},
			canonical: `{"id":"cmd-exec-1","command":"npm","args":["run","build"],"ts":1790000000000,"type":"execute","sessionID":"","input":"","signal":"","executionId":"exec-abc123"}`,
			nodeHMAC:  "e2453f4ef9b5b18ce325e49cbac4d4df1507d388a70c6d251d115c6c912e4cb1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := signedExecIDFixture()
			tc.mutate(&cmd)
			gotCanonical, gotHMAC := canonicalAndHMAC(t, cmd)
			if gotCanonical != tc.canonical {
				t.Errorf("canonical diverges from the Node signer\n go:   %s\n node: %s", gotCanonical, tc.canonical)
			}
			if gotHMAC != tc.nodeHMAC {
				t.Errorf("hmac = %s, Node computed %s", gotHMAC, tc.nodeHMAC)
			}
		})
	}
}

// The field's POSITION is the whole contract: Go marshals in declaration order
// and Node appends in a fixed order, so a struct reshuffle that still
// "includes executionId" silently breaks every signature carrying it. Asserted
// structurally as well as by the literals above, because a reshuffle would
// otherwise only be caught as five opaque hash mismatches.
func TestSignedExecutionID_SitsBetweenRiskLevelAndCwd(t *testing.T) {
	cmd := signedExecIDFixture()
	cmd.RiskLevel = "destructive"
	cmd.ExecutionID = "exec-abc123"
	cmd.Cwd = `C:\repo`
	canonical, _ := canonicalAndHMAC(t, cmd)

	risk := strings.Index(canonical, `"riskLevel"`)
	exec := strings.Index(canonical, `"executionId"`)
	cwd := strings.Index(canonical, `"cwd"`)
	if risk < 0 || exec < 0 || cwd < 0 {
		t.Fatalf("expected all three fields in %s", canonical)
	}
	if !(risk < exec && exec < cwd) {
		t.Errorf("order must be riskLevel < executionId < cwd, got risk=%d exec=%d cwd=%d in %s", risk, exec, cwd, canonical)
	}
}

// THE SECURITY PROPERTY, stated as a test rather than left to the comment.
// While the id was unsigned, an attacker with topic-write but no key could
// take a replayed signed frame, append a victim run's execution id, and have
// the device resolve the command against that run's remembered directory —
// the signature still verified, because the field was not part of it.
func TestSignedExecutionID_AppendingAnIDBreaksTheSignature(t *testing.T) {
	clean := signedExecIDFixture()
	_, cleanHMAC := canonicalAndHMAC(t, clean)

	tampered := signedExecIDFixture()
	tampered.ExecutionID = "exec-victims-run"
	_, tamperedHMAC := canonicalAndHMAC(t, tampered)

	if cleanHMAC == tamperedHMAC {
		t.Fatal("appending an executionId left the signature unchanged — the field is not covered by the HMAC")
	}
}

// The capability must be reported, and must be reported even when false, or a
// future build that turns it off reads to the server as an older agent that
// never knew the field. `omitempty` would do exactly that.
func TestSignedExecutionID_CapabilityIsReportedAndNeverOmitted(t *testing.T) {
	caps := newMachineCapabilities()
	if !caps.SignedExecutionID {
		t.Error("newMachineCapabilities must report SignedExecutionID: this build does sign it")
	}

	encoded, err := json.Marshal(&capabilitiesInfo{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"signedExecutionId":false`) {
		t.Errorf("a false SignedExecutionID must still serialise (no omitempty); got %s", encoded)
	}

	// and the wire key is the one shared-constants compares against
	on, err := json.Marshal(newMachineCapabilities())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(on), `"signedExecutionId":true`) {
		t.Errorf("expected signedExecutionId:true on the wire; got %s", on)
	}
}
