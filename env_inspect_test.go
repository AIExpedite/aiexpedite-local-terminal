package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestIsInternalDemandCommand(t *testing.T) {
	cases := map[string]bool{
		"__cli_usage_refresh__": true,
		"__env_inspect__":       true,
		"echo hi":               false,
		"":                      false,
		"__ping__":              false,
	}
	for cmd, want := range cases {
		if got := isInternalDemandCommand(cmd); got != want {
			t.Errorf("isInternalDemandCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestRequiresNativeApprovalForStep(t *testing.T) {
	if !requiresNativeApprovalForStep(commandMsg{RiskLevel: "destructive"}) {
		t.Errorf("destructive step should require native approval")
	}
	for _, risk := range []string{"", "read_only", "safe_write", "external_write"} {
		if requiresNativeApprovalForStep(commandMsg{RiskLevel: risk}) {
			t.Errorf("risk %q should NOT force native approval", risk)
		}
	}
}

// helper: recompute the canonical signature bytes the agent verifies against.
func canonicalSig(t *testing.T, cmd commandMsg, secret string) (string, string) {
	t.Helper()
	payload := signaturePayload{
		ID:        cmd.ID,
		Command:   cmd.Command,
		Args:      cmd.Args,
		Ts:        cmd.Ts,
		Type:      cmd.Type,
		SessionID: cmd.SessionID,
		Input:     cmd.Input,
		Signal:    cmd.Signal,
		RefreshID: cmd.RefreshID,
		RiskLevel: cmd.RiskLevel,
	}
	data, err := nodeJSONStringify(payload)
	if err != nil {
		t.Fatalf("stringify failed: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return string(data), hex.EncodeToString(mac.Sum(nil))
}

func TestSignaturePayload_RiskLevelBackwardCompatible(t *testing.T) {
	// A command with no riskLevel must produce the identical canonical JSON as
	// the pre-riskLevel format (field dropped via omitempty), so older
	// producers/agents still verify.
	cmd := commandMsg{ID: "x", Command: "echo hi", Args: []string{}, Ts: 1}
	data, _ := canonicalSig(t, cmd, "secret")
	if strings.Contains(data, "riskLevel") {
		t.Fatalf("non-env command must omit riskLevel from canonical JSON, got %s", data)
	}
}

func TestSignaturePayload_DestructiveRiskIsSignedAndVerifies(t *testing.T) {
	secret := "supersecret"
	cmd := commandMsg{
		ID:        "step-1",
		Command:   "rm",
		Args:      []string{"-rf", "/tmp/build-cache"},
		Ts:        1771714908831,
		Type:      "execute",
		RiskLevel: "destructive",
	}
	data, sig := canonicalSig(t, cmd, secret)
	if !strings.Contains(data, `"riskLevel":"destructive"`) {
		t.Fatalf("destructive step must include signed riskLevel, got %s", data)
	}
	cmd.Signature = sig
	if !verifySignature(cmd, secret) {
		t.Fatalf("signature with riskLevel should verify")
	}
	// Downgrade attempt: stripping the riskLevel must invalidate the signature.
	tampered := cmd
	tampered.RiskLevel = ""
	if verifySignature(tampered, secret) {
		t.Fatalf("stripping riskLevel must break the signature (downgrade protection)")
	}
}

func TestMakeEnvInspectResult(t *testing.T) {
	cmd := commandMsg{ID: "cmd-1", WorkspaceID: "__device__", UID: "u1", RefreshID: "r1", AgentID: "a1"}
	report := ReadinessReport{State: ReadinessNeedsSetup}
	res := makeEnvInspectResult(cmd, &Config{AgentID: "cfg-agent"}, report)

	if res.Type != "__env_inspect_result__" {
		t.Errorf("expected __env_inspect_result__ type, got %q", res.Type)
	}
	if res.RefreshID != "r1" {
		t.Errorf("expected refreshId echoed, got %q", res.RefreshID)
	}
	if res.AgentID != "cfg-agent" {
		t.Errorf("expected config AgentID to win, got %q", res.AgentID)
	}
	if res.Readiness == nil || res.Readiness.State != ReadinessNeedsSetup {
		t.Errorf("expected readiness report attached, got %+v", res.Readiness)
	}
	if res.Status != "success" {
		t.Errorf("expected success status, got %q", res.Status)
	}
}

func TestEnvInspectSizeLimit_UsesNormalCommandLimit(t *testing.T) {
	// __env_inspect__ carries no large inbound catalog, so it uses the normal
	// command size limit (not the larger catalog limit).
	if got := pubSubMessageSizeLimit(commandMsg{Command: "__env_inspect__"}); got != maxPubSubCommandMessageBytes {
		t.Fatalf("expected normal command size limit for __env_inspect__, got %d", got)
	}
}
