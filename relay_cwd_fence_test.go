package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cloud.google.com/go/pubsub/v2"
)

func captureCwdFencePublishes(t *testing.T) func() []resultMsg {
	t.Helper()
	var mu sync.Mutex
	var published []resultMsg
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })
	return func() []resultMsg {
		mu.Lock()
		defer mu.Unlock()
		return append([]resultMsg(nil), published...)
	}
}

func cwdFenceHmacHex(secret string, data []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestCwdFenceSignature_NodeVectors(t *testing.T) {
	const secret = "test-secret"
	const wantNoFenceCanonical = `{"id":"cmd-fence-1","command":"claude","args":["--model","opus"],"ts":1760400000000,"type":"session_start","sessionID":"sess-fence-1","input":"","signal":""}`
	const wantWithFenceCanonical = `{"id":"cmd-fence-1","command":"claude","args":["--model","opus"],"ts":1760400000000,"type":"session_start","sessionID":"sess-fence-1","input":"","signal":"","cwdFence":"/Users/daniel/aiexpedite"}`
	const wantFenceAndReadOnlyCanonical = `{"id":"cmd-fence-1","command":"claude","args":["--model","opus"],"ts":1760400000000,"type":"session_start","sessionID":"sess-fence-1","input":"","signal":"","cwdFence":"/Users/daniel/aiexpedite","readOnly":true}`

	const noFenceSig = "4f90874056fa24b0b409dd14348f3dc119ab7c7d96a15ff83d8d9fc94118c4f2"
	const withFenceSig = "311a5bec49de4180b33896e80a0cdf9492fae3f9f0a9cb6b991768f0d689a2c4"
	const fenceAndReadOnlySig = "a35a83751326363bb2cf78450ad88aeeaacd3524135070088ac1d6e6063e8f38"

	baseCmd := commandMsg{
		ID:        "cmd-fence-1",
		Command:   "claude",
		Args:      []string{"--model", "opus"},
		Ts:        1760400000000,
		Type:      "session_start",
		SessionID: "sess-fence-1",
	}

	// 1. No fence
	noFenceCmd := baseCmd
	gotNoFence, err := signatureCanonical(noFenceCmd)
	if err != nil {
		t.Fatalf("signatureCanonical(noFenceCmd): %v", err)
	}
	if string(gotNoFence) != wantNoFenceCanonical {
		t.Fatalf("noFence canonical mismatch:\nwant: %s\n got: %s", wantNoFenceCanonical, gotNoFence)
	}
	if gotSig := cwdFenceHmacHex(secret, gotNoFence); gotSig != noFenceSig {
		t.Fatalf("noFence HMAC = %s, want %s", gotSig, noFenceSig)
	}
	noFenceCmd.Signature = noFenceSig
	if !verifySignature(noFenceCmd, secret) {
		t.Fatal("noFence command must verify signature")
	}

	// 2. With fence
	withFenceCmd := baseCmd
	withFenceCmd.CwdFence = "/Users/daniel/aiexpedite"
	gotWithFence, err := signatureCanonical(withFenceCmd)
	if err != nil {
		t.Fatalf("signatureCanonical(withFenceCmd): %v", err)
	}
	if string(gotWithFence) != wantWithFenceCanonical {
		t.Fatalf("withFence canonical mismatch:\nwant: %s\n got: %s", wantWithFenceCanonical, gotWithFence)
	}
	if gotSig := cwdFenceHmacHex(secret, gotWithFence); gotSig != withFenceSig {
		t.Fatalf("withFence HMAC = %s, want %s", gotSig, withFenceSig)
	}
	withFenceCmd.Signature = withFenceSig
	if !verifySignature(withFenceCmd, secret) {
		t.Fatal("withFence command must verify signature")
	}

	// Stripping fence invalidates signature
	strippedFenceCmd := withFenceCmd
	strippedFenceCmd.CwdFence = ""
	if verifySignature(strippedFenceCmd, secret) {
		t.Fatal("stripping cwdFence must invalidate signature")
	}

	// 3. Fence + ReadOnly
	fenceAndRoCmd := withFenceCmd
	fenceAndRoCmd.ReadOnly = true
	gotFenceAndRo, err := signatureCanonical(fenceAndRoCmd)
	if err != nil {
		t.Fatalf("signatureCanonical(fenceAndRoCmd): %v", err)
	}
	if string(gotFenceAndRo) != wantFenceAndReadOnlyCanonical {
		t.Fatalf("fenceAndRo canonical mismatch:\nwant: %s\n got: %s", wantFenceAndReadOnlyCanonical, gotFenceAndRo)
	}
	if gotSig := cwdFenceHmacHex(secret, gotFenceAndRo); gotSig != fenceAndReadOnlySig {
		t.Fatalf("fenceAndRo HMAC = %s, want %s", gotSig, fenceAndReadOnlySig)
	}
	fenceAndRoCmd.Signature = fenceAndReadOnlySig
	if !verifySignature(fenceAndRoCmd, secret) {
		t.Fatal("fenceAndRo command must verify signature")
	}
}

func TestRefuseCwdOutsideFence(t *testing.T) {
	publishes := captureCwdFencePublishes(t)
	cfg := &Config{AgentID: "agent-fence-test"}

	baseDir := resolvedTempDir(t)
	fenceDir := filepath.Join(baseDir, "repo")
	subDir := filepath.Join(fenceDir, "sub", "deep")
	siblingDir := filepath.Join(baseDir, "repo-old")
	outsideDir := filepath.Join(baseDir, "other")

	for _, d := range []string{fenceDir, subDir, siblingDir, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cmdBase := commandMsg{
		ID:        "cmd-1",
		Type:      "session_start",
		SessionID: "sess-1",
	}

	// 1. No fence -> not refused
	cmd := cmdBase
	cmd.Cwd = fenceDir
	if refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("cmd without fence should not be refused")
	}
	if len(publishes()) != 0 {
		t.Errorf("expected 0 publishes, got %d", len(publishes()))
	}

	// 2. Exact folder -> allowed
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = fenceDir
	if refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("exact folder should not be refused")
	}
	if len(publishes()) != 0 {
		t.Errorf("expected 0 publishes, got %d", len(publishes()))
	}

	// 3. Sub-folder allowed
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = filepath.Join(fenceDir, "sub")
	if refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("sub-folder should not be refused")
	}
	if len(publishes()) != 0 {
		t.Errorf("expected 0 publishes, got %d", len(publishes()))
	}

	// 4. Nested sub-folder allowed
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = subDir
	if refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("nested sub-folder should not be refused")
	}
	if len(publishes()) != 0 {
		t.Errorf("expected 0 publishes, got %d", len(publishes()))
	}

	// 5. Sibling-prefix folder denied
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = siblingDir
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("sibling-prefix folder should be refused")
	}
	msgs := publishes()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 publish for sibling-prefix denial, got %d", len(msgs))
	}
	if msgs[0].Status != "denied" || msgs[0].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[0])
	}

	// 6. Outside folder denied
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = outsideDir
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("outside folder should be refused")
	}
	msgs = publishes()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 publishes total, got %d", len(msgs))
	}
	if msgs[1].Status != "denied" || msgs[1].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[1])
	}

	// 7. Path traversal denied (../segment)
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = filepath.Join(fenceDir, "..", "other")
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("path traversal should be refused")
	}
	msgs = publishes()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 publishes total, got %d", len(msgs))
	}
	if msgs[2].Status != "denied" || msgs[2].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[2])
	}

	// 8. Empty cwd denied
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = ""
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("empty cwd should be refused")
	}
	msgs = publishes()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 publishes total, got %d", len(msgs))
	}
	if msgs[3].Status != "denied" || msgs[3].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[3])
	}

	// 9. Non-existent path denied
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = filepath.Join(fenceDir, "nonexistent")
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("non-existent path should be refused")
	}
	msgs = publishes()
	if len(msgs) != 5 {
		t.Fatalf("expected 5 publishes total, got %d", len(msgs))
	}
	if msgs[4].Status != "denied" || msgs[4].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[4])
	}

	// 10. Symlinks (if supported on this environment)
	linkInside := filepath.Join(fenceDir, "linkInside")
	if err := os.Symlink(subDir, linkInside); err == nil {
		cmd = cmdBase
		cmd.CwdFence = fenceDir
		cmd.Cwd = linkInside
		if refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
			t.Errorf("symlink staying inside fence should be allowed")
		}
	} else {
		t.Logf("skipping symlink inside test: %v", err)
	}

	linkOutside := filepath.Join(fenceDir, "linkOutside")
	if err := os.Symlink(outsideDir, linkOutside); err == nil {
		cmd = cmdBase
		cmd.CwdFence = fenceDir
		cmd.Cwd = linkOutside
		if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
			t.Errorf("symlink escaping fence must be refused")
		}
	} else {
		t.Logf("skipping symlink escape test: %v", err)
	}
}
