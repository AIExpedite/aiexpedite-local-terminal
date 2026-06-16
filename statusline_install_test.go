package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// posixSingleQuote emits the close/escape/reopen form to embed a literal
// apostrophe inside a single-quoted POSIX value:
//
//	'\''
//
// The env-preamble regex used
// by isOurStatusLineCommand has to accept that sequence — otherwise a config
// dir containing an apostrophe (e.g. `/Users/bob's/...`) breaks recognition of
// our OWN installed command, and:
//   - opt-out / uninstall stops cleaning Claude's settings.json
//   - a later install refresh stashes our own command as the chained "previous"
//     status line, leaving the user with a hook -> hook chain
func TestIsOurStatusLineCommand_AcceptsApostropheEscapedPaths(t *testing.T) {
	cmd := statusLinePosixCommand(
		"/Users/bob's mac/aiexpedite",
		"/Users/bob's mac/.aiexpedite/claude_rate_limits.json",
		"/Users/bob's mac/.aiexpedite/claude_statusline_prev.json",
	)
	// Sanity-check that posixSingleQuote actually emits the `'\''` escape — if a
	// future refactor flips the quoting strategy this test should still cover
	// whatever shape `statusLinePosixCommand` produces.
	if !strings.Contains(cmd, `'\''`) {
		t.Fatalf("expected posix command to carry the `'\\''` apostrophe-escape, got %q", cmd)
	}
	if !isOurStatusLineCommand(cmd) {
		t.Errorf("posix command with apostrophe-escaped env values must be recognized as ours: %q", cmd)
	}
}

// Sanity: the legacy (no-apostrophe) shapes already covered by other tests must
// still round-trip — the new regex alternative shouldn't accidentally change
// the simple body's match behavior.
func TestIsOurStatusLineCommand_PlainPosixStillRoundTrips(t *testing.T) {
	cmd := statusLinePosixCommand(
		"/usr/local/bin/aiexpedite",
		"/home/dan/.aiexpedite/claude_rate_limits.json",
		"/home/dan/.aiexpedite/claude_statusline_prev.json",
	)
	if !isOurStatusLineCommand(cmd) {
		t.Errorf("plain posix command should still be recognized: %q", cmd)
	}
}

// mergeClaudeRateLimitCache uses an advisory file lock to serialize the
// read-modify-write across the multiple `aiexpedite statusline-hook` processes
// Claude can spawn in parallel. Verify the lock file is materialised next to
// the cache so concurrent invocations from different processes contend on the
// same lockable path.
func TestMergeClaudeRateLimitCache_CreatesLockFile(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "rl.json")

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 5, ResetsAtMs: now.Add(time.Hour).UnixMilli(), Status: "allowed", usageKnown: true},
	}, now, "")

	matches, _ := filepath.Glob(cache + ".lock")
	if len(matches) == 0 {
		t.Errorf("expected a sibling lock file at %s.lock", cache)
	}
	// The intermediate tmp file is per-process / per-call unique so two writers
	// can't clobber each other's pending bytes — verify none of the simple
	// fixed-name tmp residue from the old write path remains.
	if _, err := filepath.Glob(cache + ".tmp"); err == nil {
		if leftover, _ := filepath.Glob(cache + ".tmp"); len(leftover) > 0 {
			t.Errorf("legacy fixed `.tmp` file should not be produced: %v", leftover)
		}
	}
}
