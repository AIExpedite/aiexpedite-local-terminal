package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

func TestSignatureMatch(t *testing.T) {
	// Non-refresh commands omit refreshId from the canonical JSON (omitempty),
	// so this matches the pre-refreshId signed shape — backward-compatible with
	// older agents/services that don't know about refreshId.
	expectedSignatureData := `{"id":"05989d90-0b3d-4592-bf14-928d11a82e14","command":"echo \"Testing terminal tool\" && date && pwd","args":[],"ts":1771714908831,"type":"","sessionID":"","input":"","signal":""}`

	// Build the same payload Go would build from the Pub/Sub message
	cmd := commandMsg{
		ID:      "05989d90-0b3d-4592-bf14-928d11a82e14",
		Command: `echo "Testing terminal tool" && date && pwd`,
		Args:    []string{},
		Ts:      1771714908831,
	}

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
	}

	// Use the same encoding as verifySignature (SetEscapeHTML false)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		t.Fatalf("json.Encode failed: %v", err)
	}
	signatureData := bytes.TrimRight(buf.Bytes(), "\n")

	fmt.Printf("Go signatureData:   %s\n", string(signatureData))
	fmt.Printf("Node signatureData: %s\n", expectedSignatureData)

	if string(signatureData) != expectedSignatureData {
		fmt.Printf("Go hex:   %s\n", hex.EncodeToString(signatureData))
		fmt.Printf("Node hex: %s\n", hex.EncodeToString([]byte(expectedSignatureData)))
		t.Fatalf("JSON serialization mismatch")
	}

	fmt.Println("JSON bytes match: YES")
}

// nodeJSONStringify produces JSON matching Node.js JSON.stringify behavior
// (no HTML escaping of &, <, >)
func nodeJSONStringify(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func TestVerifySignatureWithAmpersand(t *testing.T) {
	secret := "test-secret-for-unit-test"

	// Simulate Node.js signCommand output (Node doesn't escape & in JSON)
	nodePayload := signaturePayload{
		ID:        "test-123",
		Command:   "echo hello && echo world",
		Args:      []string{},
		Ts:        1234567890,
		Type:      "",
		SessionID: "",
		Input:     "",
		Signal:    "",
		RefreshID: "",
	}
	nodeJSON, err := nodeJSONStringify(nodePayload)
	if err != nil {
		t.Fatalf("nodeJSONStringify failed: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nodeJSON)
	nodeSignature := hex.EncodeToString(mac.Sum(nil))

	cmd := commandMsg{
		ID:        "test-123",
		Command:   "echo hello && echo world",
		Args:      []string{},
		Ts:        1234567890,
		Signature: nodeSignature,
	}

	if !verifySignature(cmd, secret) {
		t.Fatal("verifySignature should return true for matching signatures with & in command")
	}
	fmt.Println("verifySignature with & in command: PASS")
}

func TestVerifySignatureWithSpecialChars(t *testing.T) {
	secret := "test-secret-for-unit-test"

	// Test with <, >, and & which Go's json.Marshal normally escapes
	// but Node.js JSON.stringify does not
	testCases := []struct {
		name    string
		command string
	}{
		{"ampersand", "echo hello && echo world"},
		{"less than", "echo 1 < 2"},
		{"greater than", "echo 1 > /dev/null"},
		{"mixed special", "if [ $x > 0 ] && [ $y < 10 ]; then echo ok; fi"},
		{"no special chars", "echo hello world"},
		{"empty command", "ls -la"},
		{"quotes", `echo "hello world"`},
		{"backtick", "echo `date`"},
		{"pipe", "ls | grep foo"},
		{"dollar", "echo $HOME"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Generate signature the way Node.js would (no HTML escaping)
			payload := signaturePayload{
				ID:        "test-123",
				Command:   tc.command,
				Args:      []string{},
				Ts:        1234567890,
				Type:      "",
				SessionID: "",
				Input:     "",
				Signal:    "",
				RefreshID: "",
			}
			nodeJSON, err := nodeJSONStringify(payload)
			if err != nil {
				t.Fatalf("nodeJSONStringify failed: %v", err)
			}

			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(nodeJSON)
			sig := hex.EncodeToString(mac.Sum(nil))

			cmd := commandMsg{
				ID:        "test-123",
				Command:   tc.command,
				Args:      []string{},
				Ts:        1234567890,
				Signature: sig,
			}

			if !verifySignature(cmd, secret) {
				t.Fatalf("verifySignature failed for command: %s\nNode JSON: %s", tc.command, string(nodeJSON))
			}
		})
	}
}

func TestSignatureMatchWithRefreshId(t *testing.T) {
	// __cli_usage_refresh__ commands DO carry refreshId, so it must appear
	// last in the canonical JSON (matches Node signCommand output).
	expectedSignatureData := `{"id":"cmd-abc","command":"__cli_usage_refresh__","args":[],"ts":1771714908831,"type":"","sessionID":"","input":"","signal":"","refreshId":"rid-xyz"}`

	cmd := commandMsg{
		ID:        "cmd-abc",
		Command:   "__cli_usage_refresh__",
		Args:      []string{},
		Ts:        1771714908831,
		RefreshID: "rid-xyz",
	}

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
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		t.Fatalf("json.Encode failed: %v", err)
	}
	signatureData := bytes.TrimRight(buf.Bytes(), "\n")

	if string(signatureData) != expectedSignatureData {
		t.Fatalf("refreshId canonical mismatch\nwant: %s\n got: %s", expectedSignatureData, string(signatureData))
	}
}

func TestVerifySignaturePreservesRawCLIAgentCatalogBytes(t *testing.T) {
	secret := "test-secret-for-unit-test"
	catalogJSON := `[{"id":"grok","displayName":"Grok Build","command":"grok","backendOnly":{"beta":true,"weight":7},"detectionKeys":["grokBuild","grok"]}]`
	canonical := `{"id":"refresh-raw","command":"__cli_usage_refresh__","args":[],"ts":1234567890,"type":"","sessionID":"","input":"","signal":"","refreshId":"rid-raw","cliAgentCatalog":` + catalogJSON + `}`

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	messageJSON := `{"id":"refresh-raw","command":"__cli_usage_refresh__","args":[],"ts":1234567890,"refreshId":"rid-raw","cliAgentCatalog":` + catalogJSON + `,"signature":"` + sig + `"}`
	var cmd commandMsg
	if err := json.Unmarshal([]byte(messageJSON), &cmd); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if string(cmd.rawCliAgentCatalog) != catalogJSON {
		t.Fatalf("raw catalog mismatch\nwant: %s\n got: %s", catalogJSON, string(cmd.rawCliAgentCatalog))
	}
	if !verifySignature(cmd, secret) {
		t.Fatalf("verifySignature should use raw cliAgentCatalog JSON bytes")
	}
}

// TestVerifySignatureBackwardCompatLegacyProducer simulates the agent/service
// upgrade window: a producer that signed without refreshId at all (the
// pre-refreshId canonical shape) must still verify on the new agent for
// non-refresh commands. Regression guard for the P1 raised on commit 59bc0cc.
func TestVerifySignatureBackwardCompatLegacyProducer(t *testing.T) {
	secret := "test-secret-for-unit-test"

	// Legacy canonical JSON — no refreshId field at all.
	legacyCanonical := `{"id":"legacy-1","command":"ls","args":[],"ts":1234567890,"type":"","sessionID":"","input":"","signal":""}`

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(legacyCanonical))
	legacySig := hex.EncodeToString(mac.Sum(nil))

	cmd := commandMsg{
		ID:        "legacy-1",
		Command:   "ls",
		Args:      []string{},
		Ts:        1234567890,
		Signature: legacySig,
		// RefreshID intentionally left empty — old producer didn't set it.
	}

	if !verifySignature(cmd, secret) {
		t.Fatalf("verifySignature should accept legacy signatures (no refreshId) for non-refresh commands")
	}
}

func TestVerifySignatureWithArgs(t *testing.T) {
	secret := "test-secret-for-unit-test"

	// Test with args containing special characters
	testCases := []struct {
		name string
		args []string
	}{
		{"empty args", []string{}},
		{"nil args", nil},
		{"single arg", []string{"-la"}},
		{"args with ampersand", []string{"--filter", "name && value"}},
		{"args with angle brackets", []string{"<input>", ">output"}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = []string{}
			}

			payload := signaturePayload{
				ID:        "test-123",
				Command:   "test",
				Args:      args,
				Ts:        1234567890,
				Type:      "",
				SessionID: "",
				Input:     "",
				Signal:    "",
				RefreshID: "",
			}
			nodeJSON, err := nodeJSONStringify(payload)
			if err != nil {
				t.Fatalf("nodeJSONStringify failed: %v", err)
			}

			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(nodeJSON)
			sig := hex.EncodeToString(mac.Sum(nil))

			cmd := commandMsg{
				ID:        "test-123",
				Command:   "test",
				Args:      tc.args, // Pass original (may be nil)
				Ts:        1234567890,
				Signature: sig,
			}

			if !verifySignature(cmd, secret) {
				t.Fatalf("verifySignature failed for args: %v\nNode JSON: %s", tc.args, string(nodeJSON))
			}
		})
	}
}

func TestVerifySignatureAuthenticatesConversationID(t *testing.T) {
	secret := "test-secret-for-unit-test"
	payload := signaturePayload{
		ID:             "antigravity-start-1",
		Command:        "agy",
		Args:           []string{},
		Ts:             1234567890,
		Type:           "antigravity_native_start",
		SessionID:      "session-1",
		ConversationID: "conversation-1",
	}
	canonical, err := nodeJSONStringify(payload)
	if err != nil {
		t.Fatalf("nodeJSONStringify failed: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	cmd := commandMsg{
		ID:             payload.ID,
		Command:        payload.Command,
		Args:           payload.Args,
		Ts:             payload.Ts,
		Type:           payload.Type,
		SessionID:      payload.SessionID,
		ConversationID: payload.ConversationID,
		Signature:      hex.EncodeToString(mac.Sum(nil)),
	}
	if !verifySignature(cmd, secret) {
		t.Fatal("verifySignature should accept the signed conversation id")
	}

	cmd.ConversationID = "conversation-2"
	if verifySignature(cmd, secret) {
		t.Fatal("verifySignature should reject a tampered conversation id")
	}
}

// Node's signCommand has signed {riskLevel, cwd} together since env-setup
// shipped (terminal-service #87); this struct signed only riskLevel, so every
// env-setup step carrying BOTH was rejected as a bad signature. These tests pin
// the repaired contract from the Node side of the wire: the canonical is built
// exactly as hmac.util.js builds it, then verified here.
func TestVerifySignatureAcceptsEnvSetupCwd(t *testing.T) {
	secret := "test-secret-for-unit-test"
	payload := signaturePayload{
		ID:        "env-setup-step-1",
		Command:   "git",
		Args:      []string{"clone", "https://example.com/repo.git"},
		Ts:        1234567890,
		Type:      "execute",
		RiskLevel: "external_write",
		Cwd:       "C:/Users/dev/projects",
	}
	canonical, err := nodeJSONStringify(payload)
	if err != nil {
		t.Fatalf("nodeJSONStringify failed: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	cmd := commandMsg{
		ID:        payload.ID,
		Command:   payload.Command,
		Args:      payload.Args,
		Ts:        payload.Ts,
		Type:      payload.Type,
		RiskLevel: payload.RiskLevel,
		Cwd:       payload.Cwd,
		Signature: hex.EncodeToString(mac.Sum(nil)),
	}
	if !verifySignature(cmd, secret) {
		t.Fatal("verifySignature should accept an env-setup command whose cwd was signed")
	}

	// A redirected cwd is the attack the signature exists to stop: an approved,
	// disk-gated step re-pointed at a different directory or volume.
	cmd.Cwd = "D:/somewhere/else"
	if verifySignature(cmd, secret) {
		t.Fatal("verifySignature should reject a tampered env-setup cwd")
	}
}

// The gate must stay riskLevel-scoped: ordinary execute/session commands carry
// a cwd the Node signer does NOT sign, so including it unconditionally would
// break every signed command that has a working directory. This is the test
// that fails if riskGatedSignatureCwd is ever inlined away.
func TestVerifySignatureIgnoresCwdWithoutRiskLevel(t *testing.T) {
	secret := "test-secret-for-unit-test"
	// Node canonical for an ordinary command: no riskLevel, no cwd.
	payload := signaturePayload{
		ID:      "ordinary-cmd-1",
		Command: "git",
		Args:    []string{"status"},
		Ts:      1234567890,
		Type:    "execute",
	}
	canonical, err := nodeJSONStringify(payload)
	if err != nil {
		t.Fatalf("nodeJSONStringify failed: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	cmd := commandMsg{
		ID:        payload.ID,
		Command:   payload.Command,
		Args:      payload.Args,
		Ts:        payload.Ts,
		Type:      payload.Type,
		Cwd:       "C:/Users/dev/projects", // present on the wire, absent from the canonical
		Signature: hex.EncodeToString(mac.Sum(nil)),
	}
	if !verifySignature(cmd, secret) {
		t.Fatal("a cwd on an ordinary (riskLevel-less) command must not enter the canonical")
	}
}
