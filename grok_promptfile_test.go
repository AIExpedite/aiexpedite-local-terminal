// File: grok_promptfile_test.go
// -----------------------------------------------------------------------------
// Tests for rewriteGrokPromptToFile, which relocates grok's headless prompt
// from the inline `-p <prompt>` argv pair to `--prompt-file <tempPath>` so a
// long review brief does not overflow the Windows command-line-length cap.
// (grok's headless mode does not read piped stdin, so a temp file is the
// file-based analog of the stdin route claude/codex use.)
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteGrokPromptToFile_HappyPath(t *testing.T) {
	prompt := "a very long review brief ... " + strings.Repeat("detail ", 5000)
	in := []string{
		"--output-format", "streaming-json", "--no-auto-update",
		"--always-approve", "-p", prompt,
	}

	out, cleanup := rewriteGrokPromptToFile(in)
	if cleanup == "" {
		t.Fatalf("expected a non-empty cleanup path, got empty")
	}
	defer os.Remove(cleanup)

	// The trailing `-p <prompt>` pair must become `--prompt-file <path>`.
	n := len(out)
	if n < 2 || out[n-2] != "--prompt-file" || out[n-1] != cleanup {
		t.Fatalf("expected trailing [--prompt-file %q], got %v", cleanup, out)
	}
	// The leading flags must be preserved unchanged.
	wantPrefix := []string{
		"--output-format", "streaming-json", "--no-auto-update", "--always-approve",
	}
	for i, w := range wantPrefix {
		if out[i] != w {
			t.Fatalf("prefix arg %d: want %q, got %q", i, w, out[i])
		}
	}
	// `-p` and the raw prompt must no longer appear on argv.
	for _, a := range out {
		if a == "-p" || a == prompt {
			t.Fatalf("argv still carries the inline prompt: %v", out)
		}
	}

	// The temp file must exist and hold the exact prompt.
	data, err := os.ReadFile(cleanup)
	if err != nil {
		t.Fatalf("reading temp prompt file: %v", err)
	}
	if string(data) != prompt {
		t.Fatalf("temp file content mismatch: got %d bytes, want %d", len(data), len(prompt))
	}

	// os.Remove must succeed (defer also covers it, but assert it here).
	if err := os.Remove(cleanup); err != nil {
		t.Fatalf("removing temp prompt file: %v", err)
	}
}

func TestRewriteGrokPromptToFile_NoPromptFlag(t *testing.T) {
	// Subcommand carve-out: buildGrokInteractiveArgs returns argv unchanged
	// with no `-p`, so the rewrite must be a no-op.
	in := []string{"models"}
	out, cleanup := rewriteGrokPromptToFile(in)
	if cleanup != "" {
		t.Fatalf("expected empty cleanup path for no -p input, got %q", cleanup)
	}
	if len(out) != len(in) {
		t.Fatalf("expected argv unchanged, got %v", out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("arg %d changed: want %q, got %q", i, in[i], out[i])
		}
	}
}

func TestRewriteGrokPromptToFile_SubcommandCarveOutWithChildDashP(t *testing.T) {
	// Reviewer-flagged regression: buildGrokInteractiveArgs returns user argv
	// verbatim for the documented `grok mcp add <name> -- <cmd> [args...]`
	// grammar (and the other subcommand carve-outs). If the trailing `-p
	// <value>` belongs to the CHILD command behind `--` (e.g. a Python server
	// taking `-p 8000` as its port), the rewrite must NOT relocate it — the
	// registered MCP server command would otherwise be corrupted.
	in := []string{"mcp", "add", "svc", "--", "python", "server.py", "-p", "8000"}
	out, cleanup := rewriteGrokPromptToFile(in)
	if cleanup != "" {
		t.Fatalf("expected empty cleanup path for carve-out argv, got %q", cleanup)
	}
	if len(out) != len(in) {
		t.Fatalf("expected argv unchanged for carve-out, got %v", out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("arg %d changed: want %q, got %q", i, in[i], out[i])
		}
	}
}

func TestRewriteGrokPromptToFile_MultilineValuePreservedByteForByte(t *testing.T) {
	// A multi-line / large value must land in the file byte-for-byte —
	// including newlines, trailing whitespace, and non-ASCII.
	prompt := "line one\nline two\r\n\ttabbed\n" +
		"unicode: café — 漢字 — 🚀\n" +
		"trailing spaces:    \n" +
		strings.Repeat("BIG ", 20000)
	in := []string{
		"--output-format", "streaming-json", "--no-auto-update", "-p", prompt,
	}

	out, cleanup := rewriteGrokPromptToFile(in)
	if cleanup == "" {
		t.Fatalf("expected a non-empty cleanup path, got empty")
	}
	defer os.Remove(cleanup)

	data, err := os.ReadFile(cleanup)
	if err != nil {
		t.Fatalf("reading temp prompt file: %v", err)
	}
	if string(data) != prompt {
		t.Fatalf("temp file content not byte-for-byte: got %d bytes, want %d", len(data), len(prompt))
	}

	n := len(out)
	if out[n-2] != "--prompt-file" || out[n-1] != cleanup {
		t.Fatalf("expected trailing [--prompt-file %q], got %v", cleanup, out)
	}
}

// TestStartSession_RemovesGrokPromptFileOnEarlyFailure proves the cleanup
// defer in StartSession removes the temp file written by rewriteGrokPromptToFile
// when proc.Start() fails — without it, every failed launch (missing grok,
// bad cwd, CreateProcess error) would leak the prompt brief on disk because
// waitForExit, which owns the eventual removal, never runs.
func TestStartSession_RemovesGrokPromptFileOnEarlyFailure(t *testing.T) {
	// An explicit absolute path that does not exist forces proc.Start() to
	// fail AFTER rewriteGrokPromptToFile has already written the temp file.
	// The basename is still "grok", so isGrokCommand() returns true and the
	// rewrite path runs.
	missing := filepath.Join(t.TempDir(), "does-not-exist", "grok")

	// Snapshot pre-existing grok-prompt-*.txt files so we can spot a leak
	// caused by THIS call regardless of what other tests have left behind.
	// Glob the SAME dir the rewrite writes into (the .ai-expedite scratch root,
	// not the OS temp dir) so the leak check is actually effective.
	pattern := filepath.Join(grokPromptTempDir(), "grok-prompt-*.txt")
	beforeList, _ := filepath.Glob(pattern)
	before := make(map[string]bool, len(beforeList))
	for _, p := range beforeList {
		before[p] = true
	}

	sm := NewSessionManager(nil)
	err := sm.StartSession(
		"leak-test",
		missing,
		[]string{"a long review brief that would have ridden on argv"},
		"", "ws", "uid", 0, false,
		func(resultMsg) {},
	)
	if err == nil {
		t.Fatalf("expected StartSession to fail when executable does not exist")
	}

	afterList, _ := filepath.Glob(pattern)
	for _, p := range afterList {
		if !before[p] {
			t.Fatalf("StartSession leaked grok prompt temp file on early failure: %s", p)
		}
	}
}
