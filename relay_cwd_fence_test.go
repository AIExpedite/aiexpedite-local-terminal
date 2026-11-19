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

	// 10. Relative cwd denied
	cmd = cmdBase
	cmd.CwdFence = fenceDir
	cmd.Cwd = "relative/sub"
	if !refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg) {
		t.Errorf("relative cwd should be refused")
	}
	msgs = publishes()
	if len(msgs) != 6 {
		t.Fatalf("expected 6 publishes total, got %d", len(msgs))
	}
	if msgs[5].Status != "denied" || msgs[5].RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Errorf("unexpected denial payload: %+v", msgs[5])
	}
	// Links (symlinks, and junctions on Windows): TestRefuseCwdOutsideFence_SymlinkEscapes
	// and relay_cwd_fence_windows_test.go.
}

// fenceRefusal runs the fence for one start and returns the rejection it
// published, or nil when the start was admitted.
func fenceRefusal(t *testing.T, fence, cwd string) *resultMsg {
	t.Helper()
	publishes := captureCwdFencePublishes(t)
	cmd := commandMsg{ID: "cmd-link", Type: "session_start", SessionID: "sess-link", CwdFence: fence, Cwd: cwd}
	refused := refuseCwdOutsideFence(context.Background(), nil, nil, cmd, &Config{AgentID: "agent-link"})
	msgs := publishes()
	if !refused {
		if len(msgs) != 0 {
			t.Fatalf("an admitted start published %d results", len(msgs))
		}
		return nil
	}
	if len(msgs) != 1 {
		t.Fatalf("a refused start published %d results, want 1", len(msgs))
	}
	return &msgs[0]
}

// A grant never widens through a link: a symlink inside the granted folder that
// leads out of it is refused CWD_OUTSIDE_GRANT_FOLDER, and so is a folder
// reached THROUGH such a link. A link that stays inside is admitted.
func TestRefuseCwdOutsideFence_SymlinkEscapes(t *testing.T) {
	base := resolvedTempDir(t)
	fence := filepath.Join(base, "AIExpedite")
	inside := filepath.Join(fence, "frontend")
	outside := filepath.Join(base, "other")
	for _, d := range []string{inside, filepath.Join(outside, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	escape := filepath.Join(fence, "link")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	stay := filepath.Join(fence, "stay")
	if err := os.Symlink(inside, stay); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}

	for name, cwd := range map[string]string{
		"the link itself":          escape,
		"a folder under the link":  filepath.Join(escape, "sub"),
		"a traversal out of fence": filepath.Join(inside, "..", "..", "other"),
	} {
		res := fenceRefusal(t, fence, cwd)
		if res == nil {
			t.Errorf("%s (%s) leaves the granted folder and must be refused", name, cwd)
			continue
		}
		if res.Status != "denied" || res.RejectionReason != cwdOutsideGrantFolderRejectionCode {
			t.Errorf("%s: unexpected refusal %+v", name, *res)
		}
	}
	if res := fenceRefusal(t, fence, stay); res != nil {
		t.Errorf("a link that stays inside the granted folder must be admitted: %+v", *res)
	}
	// The fence itself reached through a link is still the same real folder.
	fenceLink := filepath.Join(base, "fence-link")
	if err := os.Symlink(fence, fenceLink); err == nil {
		if res := fenceRefusal(t, fenceLink, inside); res != nil {
			t.Errorf("a fence spelled through a link must admit its own sub-folder: %+v", *res)
		}
	}
}

// A link cycle refuses rather than looping.
func TestFenceRealPath_CycleFailsClosed(t *testing.T) {
	base := resolvedTempDir(t)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	if _, err := fenceRealPath(a); err == nil {
		t.Fatalf("a link cycle must not resolve")
	}
}

// A fenced start that names ".." is refused unread, even one that reads as
// inside the granted folder: POSIX resolves ".." after a link, Windows by name,
// and the server never fences such a path.
func TestRefuseCwdOutsideFence_ParentSegmentRefused(t *testing.T) {
	base := resolvedTempDir(t)
	fence := filepath.Join(base, "AIExpedite")
	inside := filepath.Join(fence, "frontend")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stillInside := fence + string(filepath.Separator) + "frontend" + string(filepath.Separator) + ".." + string(filepath.Separator) + "frontend"
	if res := fenceRefusal(t, fence, stillInside); res == nil {
		t.Fatalf("a fenced cwd naming .. must be refused: %s", stillInside)
	} else if res.RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Fatalf("unexpected refusal %+v", *res)
	}
	if res := fenceRefusal(t, fence+string(filepath.Separator)+"..", inside); res == nil {
		t.Fatalf("a fence naming .. must be refused")
	}
	if res := fenceRefusal(t, fence, inside); res != nil {
		t.Fatalf("a plain sub-folder must be admitted: %+v", *res)
	}
}

// Where a link points is joined, never cleaned: an absolute target as is, a
// relative one under the link's own (real) folder, ".." kept for the walk.
func TestLinkTargetPath(t *testing.T) {
	dir := resolvedTempDir(t)
	sep := string(filepath.Separator)
	abs := filepath.Join(dir, "elsewhere")
	for _, tc := range []struct{ target, want string }{
		{abs, abs},
		{"other", dir + sep + "other"},
		{".." + sep + "other", dir + sep + ".." + sep + "other"},
	} {
		got, err := linkTargetPath(dir, tc.target)
		if err != nil || got != tc.want {
			t.Errorf("linkTargetPath(%q, %q) = %q, %v; want %q", dir, tc.target, got, err, tc.want)
		}
	}
}

// A granted folder that no longer exists on disk refuses: the fence fails
// closed rather than admitting a working directory it cannot compare.
func TestCwdFenceRefusalReason_MissingFenceFailsClosed(t *testing.T) {
	base := resolvedTempDir(t)
	cwd := filepath.Join(base, "frontend")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if reason := cwdFenceRefusalReason(cwd, filepath.Join(base, "gone")); reason == "" {
		t.Fatalf("a missing granted folder must refuse")
	}
	if res := fenceRefusal(t, filepath.Join(base, "gone"), cwd); res == nil ||
		res.RejectionReason != cwdOutsideGrantFolderRejectionCode {
		t.Fatalf("a missing granted folder must refuse CWD_OUTSIDE_GRANT_FOLDER, got %+v", res)
	}
	if reason := cwdFenceRefusalReason(cwd, base); reason != "" {
		t.Fatalf("a sub-folder of an existing fence must be admitted: %s", reason)
	}
}
