package main

import (
	"os"
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

// statusLinePosixCommand must wrap the exe path in POSIX single quotes too
// (not raw double quotes), otherwise an install path containing shell
// metacharacters like `$`, backticks, or `"` — all legal in macOS/Linux/Git
// Bash directory names — gets expanded by sh before exec. In that case the
// hook either resolves to the wrong binary or runs a command substitution on
// every render and the side-channel never updates the rate-limit cache.
func TestStatusLinePosixCommand_QuotesExeForShellSafety(t *testing.T) {
	cmd := statusLinePosixCommand(
		`/Users/dan/$WORK/aiexpedite`,
		"/cache.json",
		"/prev.json",
	)
	// The literal `$WORK` must survive verbatim — sh expands `$WORK` inside
	// double quotes but treats it as literal inside single quotes.
	if !strings.Contains(cmd, `'/Users/dan/$WORK/aiexpedite'`) {
		t.Errorf("exe path should be single-quoted to suppress `$` expansion: %q", cmd)
	}
	if !isOurStatusLineCommand(cmd) {
		t.Errorf("single-quoted-exe command must still be recognized as ours: %q", cmd)
	}
}

// statusLinePowerShellCommand must escape the exe path through
// powerShellDoubleQuote — Windows permits `$`/`$(...)` in directory names, and
// a raw `"..."` PowerShell literal would expand a variable or evaluate a
// subexpression before invoking the binary, leaving the hook silently broken.
func TestStatusLinePowerShellCommand_EscapesExe(t *testing.T) {
	cmd := statusLinePowerShellCommand(
		`C:/Users/A$B/aiexpedite.exe`,
		"C:/cache.json",
		"C:/prev.json",
	)
	// The `$` must be backtick-escaped so PowerShell doesn't substitute `$B`.
	if !strings.Contains(cmd, "& \"C:/Users/A`$B/aiexpedite.exe\" "+statusLineHookArg) {
		t.Errorf("exe path should be backtick-escaped for PowerShell: %q", cmd)
	}
	if !isOurStatusLineCommand(cmd) {
		t.Errorf("PowerShell-escaped exe command must still be recognized as ours: %q", cmd)
	}
}

// Installing the hook when settings.json has no statusLine (or one we manually
// wiped) must clear any leftover prev-stash so a later opt-out doesn't
// resurrect a command the user has already removed.
func TestEnsureClaudeStatusLineHook_ClearsStaleStashOnEmptyInstall(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	prevFile := filepath.Join(t.TempDir(), "prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", prevFile)

	// Drop a stale stash from a hypothetical earlier install — the user has
	// since removed `statusLine` from settings.json on their own.
	if err := os.WriteFile(prevFile,
		[]byte(`{"statusLine":{"type":"command","command":"old-line.sh"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"),
		[]byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if changed, err := ensureClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}

	// Stale stash must be gone so opt-out doesn't resurrect `old-line.sh`.
	if _, err := os.Stat(prevFile); !os.IsNotExist(err) {
		t.Errorf("stale prev stash should be removed on empty-statusLine install; err=%v", err)
	}

	// Opt-out drops statusLine entirely — not the resurrected old command.
	if changed, err := removeClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if strings.Contains(string(raw), "old-line.sh") {
		t.Errorf("opt-out resurrected a removed third-party command: %s", raw)
	}
}

// extractInstalledPinnedPath must round-trip the pinned stash path embedded in
// an installed command — including paths whose shell-specific escapes were
// applied at install time — so a refresh that targets a NEW pinned path can
// migrate the existing stash file instead of orphaning it.
func TestExtractInstalledPinnedPath_RoundTripsPosixAndPowerShell(t *testing.T) {
	const apostrophePath = "/Users/bob's mac/.aiexpedite/claude_statusline_prev.json"
	posix := statusLinePosixCommand("/Users/bob's mac/aiexpedite", "/cache.json", apostrophePath)
	if got := extractInstalledPinnedPath(posix, "STATUSLINE_PREV"); got != apostrophePath {
		t.Errorf("POSIX extract: got %q want %q", got, apostrophePath)
	}
	const dollarPath = `C:/Users/A$B/prev.json`
	ps := statusLinePowerShellCommand(`C:/Users/A$B/aiexpedite.exe`, "C:/cache.json", dollarPath)
	if got := extractInstalledPinnedPath(ps, "STATUSLINE_PREV"); got != dollarPath {
		t.Errorf("PowerShell extract: got %q want %q", got, dollarPath)
	}
}

// When the existing installed command is one of ours but its pinned
// STATUSLINE_PREV differs from the freshly resolved path (e.g. GetConfigDir()
// moved between the previous install and this boot), the stash file must be
// migrated to the new pinned path BEFORE the command is overwritten. Otherwise
// the refreshed hook and opt-out look at the new path while the user's chained
// third-party command remains stashed only at the old one, silently dropping
// the chain target.
func TestEnsureClaudeStatusLineHook_MigratesStashOnPinnedPathChange(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	// Old install pinned the stash here…
	oldPrev := filepath.Join(t.TempDir(), "old-prev.json")
	stashBody := []byte(`{"statusLine":{"type":"command","command":"old-line.sh"}}`)
	if err := os.WriteFile(oldPrev, stashBody, 0o600); err != nil {
		t.Fatal(err)
	}
	oldCmd := statusLinePosixCommand("/old/path/aiexpedite", "/old/cache.json", oldPrev)

	// …this boot resolves a different pinned stash path.
	newPrev := filepath.Join(t.TempDir(), "new-prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", newPrev)

	settings := `{"theme":"dark","statusLine":{"type":"command","command":` +
		mustJSONString(t, oldCmd) + `,"padding":2}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	if changed, err := ensureClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}

	if _, err := os.Stat(oldPrev); !os.IsNotExist(err) {
		t.Errorf("old pinned stash should be migrated away; err=%v", err)
	}
	got, err := os.ReadFile(newPrev)
	if err != nil {
		t.Fatalf("new pinned stash should exist: %v", err)
	}
	if string(got) != string(stashBody) {
		t.Errorf("migrated stash body mismatch: got %s want %s", got, stashBody)
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	// Minimal JSON string escaper for test fixtures — only need `"` and `\`.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
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
