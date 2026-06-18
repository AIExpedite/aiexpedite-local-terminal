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
	// Values matching the expanded signaturePayload (id, command, args, ts, type, sessionID, input, signal, refreshId)
	expectedSignatureData := `{"id":"05989d90-0b3d-4592-bf14-928d11a82e14","command":"echo \"Testing terminal tool\" && date && pwd","args":[],"ts":1771714908831,"type":"","sessionID":"","input":"","signal":"","refreshId":""}`

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
