package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

// When the pinned stash path moves across filesystems, the EXDEV fallback
// must keep the stash private. The binary `copyFile` helper chmods to 0o755,
// which on Unix would leave `claude_statusline_prev.json` world-readable and
// expose the previously chained status-line command (potentially including
// inline env vars / tokens) to other local users. The migration must instead
// produce the same owner-only 0o600 mode `savePrevStatusLine` uses.
func TestCopyPrevStatusLine_PreservesPrivateMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	src := filepath.Join(t.TempDir(), "old-prev.json")
	if err := os.WriteFile(src, []byte(`{"statusLine":{"type":"command","command":"third-party --token=secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "new-prev.json")
	if err := copyPrevStatusLine(src, dst); err != nil {
		t.Fatalf("copyPrevStatusLine: %v", err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("migrated stash must be owner-only (0o600), got %o", mode)
	}
}

// When opt-out runs after the data dir moved (e.g. XDG_CONFIG_HOME changed),
// the current prevStatusLinePath() resolves to a fresh empty location, but the
// stash is still sitting at the path the installed command itself pinned.
// removeClaudeStatusLineHook must fall back to that pinned path so the user's
// original third-party command is restored instead of dropped.
func TestRemoveClaudeStatusLineHook_RestoresFromPinnedPathWhenCurrentMissing(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	// Install-time pinned stash path (data dir before the move).
	pinnedPrev := filepath.Join(t.TempDir(), "pinned-prev.json")
	stashBody := []byte(`{"statusLine":{"type":"command","command":"third-party.sh","padding":2}}`)
	if err := os.WriteFile(pinnedPrev, stashBody, 0o600); err != nil {
		t.Fatal(err)
	}
	installedCmd := statusLinePosixCommand("/old/path/aiexpedite", "/old/cache.json", pinnedPrev)

	// Current boot resolves to a different, NON-EXISTENT stash path — the move.
	newPrev := filepath.Join(t.TempDir(), "new-prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", newPrev)

	settings := `{"theme":"dark","statusLine":{"type":"command","command":` +
		mustJSONString(t, installedCmd) + `}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	if changed, err := removeClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if !strings.Contains(string(raw), `"third-party.sh"`) {
		t.Errorf("opt-out should restore the pinned-path stash, got: %s", raw)
	}
	if !strings.Contains(string(raw), `"padding": 2`) {
		t.Errorf("opt-out should restore the full stashed object (padding), got: %s", raw)
	}
	if _, err := os.Stat(pinnedPrev); !os.IsNotExist(err) {
		t.Errorf("pinned-path stash should be consumed after restore; err=%v", err)
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

/* -------------------------------------------------------------------------- */
/* Post-update hook repair (ensureClaudeStatusLineHookIfStale)                 */
/* -------------------------------------------------------------------------- */

// armStatusLineReconcile isolates the run-start reconcile for one test: a
// private Claude config dir with a settings.json, a private prev-stash path, and
// an armed, unthrottled reconcile state. Returns the home dir and the
// settings.json path inside it.
func armStatusLineReconcile(t *testing.T) (home, settingsPath string) {
	t.Helper()
	home = t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))

	resetClaudeStatusLineReconcile()
	SetClaudeStatusLineHookDisabled(false)
	t.Cleanup(resetClaudeStatusLineReconcile)

	return home, filepath.Join(claudeDir, "settings.json")
}

// clearStatusLineReconcileThrottle drops only the interval gate, so a test can
// exercise two consecutive reconciles without also forgetting the mtime stamp
// the second one is supposed to compare against.
func clearStatusLineReconcileThrottle() {
	claudeStatusLineReconcile.mu.Lock()
	claudeStatusLineReconcile.lastCheck = time.Time{}
	claudeStatusLineReconcile.mu.Unlock()
}

// A Claude Code update rewrites settings.json and can replace our statusLine
// with its own. Nothing re-verified that until the agent restarted, so the
// numeric side-channel silently stopped writing and the utilization card froze.
// The run-start reconcile must restore our command and stash the foreign one.
func TestEnsureClaudeStatusLineHookIfStale_RepairsAfterClaudeUpdate(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Boot-time install, exactly as StartAgent performs it.
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("initial reconcile: changed=%v err=%v", changed, err)
	}

	// Simulate the update: Claude rewrites settings.json with a foreign
	// statusLine, dropping ours.
	if err := os.WriteFile(settingsPath,
		[]byte(`{"theme":"dark","statusLine":{"type":"command","command":"/opt/claude/vendor-line.sh"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	clearStatusLineReconcileThrottle()

	changed, err := ensureClaudeStatusLineHookIfStale(home)
	if err != nil {
		t.Fatalf("reconcile after update: %v", err)
	}
	if !changed {
		t.Fatal("a settings.json rewritten by a Claude Code update must be repaired")
	}

	var settings struct {
		Theme      string `json:"theme"`
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if !isOurStatusLineCommand(settings.StatusLine.Command) {
		t.Errorf("statusLine=%q, want our hook restored", settings.StatusLine.Command)
	}
	if settings.Theme != "dark" {
		t.Errorf("theme=%q, want the user's other settings preserved", settings.Theme)
	}
	if prev := loadPrevStatusLineCommand(); prev != "/opt/claude/vendor-line.sh" {
		t.Errorf("stashed previous command=%q, want the foreign one preserved for chaining", prev)
	}
}

// An unchanged settings.json must cost nothing: no write, and the file's mtime
// must survive so a backup/sync tool doesn't see churn on every session start.
func TestEnsureClaudeStatusLineHookIfStale_NoOpWhenSettingsUnchanged(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("initial reconcile: changed=%v err=%v", changed, err)
	}
	installed, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	clearStatusLineReconcileThrottle()
	changed, err := ensureClaudeStatusLineHookIfStale(home)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if changed {
		t.Error("an unchanged settings.json must not be rewritten")
	}
	after, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(installed.ModTime()) || after.Size() != installed.Size() {
		t.Errorf("settings.json was touched: mtime %v -> %v, size %d -> %d",
			installed.ModTime(), after.ModTime(), installed.Size(), after.Size())
	}
}

// A burst of session starts must not storm the file: inside the throttle window
// at most one reconcile happens, however many runs begin.
func TestEnsureClaudeStatusLineHookIfStale_ThrottlesRepeatedStarts(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || !changed {
		t.Fatalf("initial reconcile: changed=%v err=%v", changed, err)
	}

	// Break the hook again, then start several sessions without clearing the
	// throttle — none of them may reconcile.
	if err := os.WriteFile(settingsPath,
		[]byte(`{"statusLine":{"type":"command","command":"/opt/claude/vendor-line.sh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || changed {
			t.Fatalf("reconcile %d inside the throttle window: changed=%v err=%v", i, changed, err)
		}
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), statusLineHookArg) {
		t.Errorf("throttled reconcile still wrote the hook:\n%s", raw)
	}
}

// The opt-out must hold for the run-start reconcile too — otherwise a user who
// disabled the hook would have it reinstalled by their next Claude session.
func TestEnsureClaudeStatusLineHookIfStale_RespectsOptOut(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	SetClaudeStatusLineHookDisabled(true)
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || changed {
		t.Fatalf("opt-out reconcile: changed=%v err=%v", changed, err)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "statusLine") {
		t.Errorf("opt-out must not install the hook:\n%s", raw)
	}
}

// A process that never armed the reconcile (a one-shot CLI verb, the
// statusline-hook subcommand) must never rewrite Claude's settings.json as a
// side effect of starting a session.
func TestEnsureClaudeStatusLineHookIfStale_UnarmedProcessDoesNothing(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetClaudeStatusLineReconcile() // leaves the reconcile unarmed

	if changed, err := ensureClaudeStatusLineHookIfStale(home); err != nil || changed {
		t.Fatalf("unarmed reconcile: changed=%v err=%v", changed, err)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "statusLine") {
		t.Errorf("unarmed process must not touch settings.json:\n%s", raw)
	}
}

// Concurrent Claude starts must produce exactly ONE reconcile. This is not
// theoretical: ClaudeNativeManager.Start runs the reconcile outside its manager
// mutex (deliberately — it must not hold that lock over disk I/O) and
// SessionManager.StartSession runs it under a different lock entirely, so
// nothing upstream serializes the two. Without an atomic claim, both would
// observe an un-throttled state and both would drive ensureClaudeStatusLineHook's
// read-modify-write of the user's settings.json at once: one write is lost, and
// the loser can stash the winner's in-flight statusLine as the "previous
// third-party" command — permanently hiding it behind a hook -> hook chain.
func TestEnsureClaudeStatusLineHookIfStale_ConcurrentStartsReconcileOnce(t *testing.T) {
	home, settingsPath := armStatusLineReconcile(t)
	if err := os.WriteFile(settingsPath,
		[]byte(`{"theme":"dark","statusLine":{"type":"command","command":"/opt/claude/vendor-line.sh"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	const starts = 12
	var wg sync.WaitGroup
	changedCount := int64(0)
	errCount := int64(0)
	for i := 0; i < starts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			changed, err := ensureClaudeStatusLineHookIfStale(home)
			if err != nil {
				atomic.AddInt64(&errCount, 1)
				return
			}
			if changed {
				atomic.AddInt64(&changedCount, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&errCount); got != 0 {
		t.Errorf("%d concurrent reconciles errored", got)
	}
	if got := atomic.LoadInt64(&changedCount); got != 1 {
		t.Errorf("reconciles that wrote=%d, want exactly 1 across %d concurrent starts", got, starts)
	}

	// The single winner's write must be intact and complete: our hook installed,
	// the user's other settings preserved, and the foreign command stashed
	// exactly once so opt-out can still restore it.
	var settings struct {
		Theme      string `json:"theme"`
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON after concurrent reconciles: %v\n%s", err, raw)
	}
	if !isOurStatusLineCommand(settings.StatusLine.Command) {
		t.Errorf("statusLine=%q, want our hook", settings.StatusLine.Command)
	}
	if settings.Theme != "dark" {
		t.Errorf("theme=%q, want the user's other settings preserved", settings.Theme)
	}
	// The stash must hold the USER's command, never one of ours — a lost update
	// would have stashed our own hook and buried the vendor line for good.
	prev := loadPrevStatusLineCommand()
	if prev != "/opt/claude/vendor-line.sh" {
		t.Errorf("stashed command=%q, want the foreign one exactly once", prev)
	}
	if isOurStatusLineCommand(prev) {
		t.Errorf("our own hook was stashed as the previous command: %q", prev)
	}
}

// The run-start reconcile is best-effort, but Claude's config dir is user-pointed
// (CLAUDE_CONFIG_DIR) and can sit on a slow or unreachable filesystem where
// Stat/ReadFile/WriteFile honour no deadline of ours. A start must never inherit
// that wait — in the managed path it used to run under sm.mu, so one stalled
// repair would have blocked every unrelated session operation with it.
//
// The stall is modelled by holding the reconcile gate's own mutex, which is the
// first thing ensureClaudeStatusLineHookIfStale takes: the helper's goroutine
// parks there exactly as it would inside a hung syscall, and the caller must
// still come back within its bound.
func TestReconcileClaudeStatusLineHookBoundedReturnsWhileRepairStalls(t *testing.T) {
	home, _ := armStatusLineReconcile(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	claudeStatusLineReconcile.mu.Lock()

	returned := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		reconcileClaudeStatusLineHookBounded("test")
		returned <- time.Since(start)
	}()

	select {
	case elapsed := <-returned:
		if elapsed < claudeStatusLineReconcileWait {
			// It must have WAITED for the repair, not skipped it: the common case
			// is one Stat and the ordering (repair before the child launches) is
			// the whole point of the run-start reconcile.
			t.Fatalf("returned after %v, before the %v bound — the reconcile was not awaited",
				elapsed, claudeStatusLineReconcileWait)
		}
	case <-time.After(claudeStatusLineReconcileWait + 10*time.Second):
		claudeStatusLineReconcile.mu.Unlock()
		t.Fatal("reconcileClaudeStatusLineHookBounded blocked on a stalled repair")
	}

	// Release the parked goroutine into a no-op rather than a real reconcile, so
	// it cannot outlive the test and race this test's temp dirs.
	claudeStatusLineReconcile.optOut = true
	claudeStatusLineReconcile.mu.Unlock()
}

// A pinned-path migration that CANNOT complete must abort the refresh instead
// of installing a command pinned to the new stash path. Writing that command
// would point both the hook's chain lookup and opt-out at newPrev while the
// user's third-party status line is still stashed only at oldPrev — the chain
// target disappears even though its stash is right there on disk. Aborting
// leaves settings.json untouched, so the previously installed command (still
// pinned to oldPrev, where the stash actually is) keeps resolving.
func TestEnsureClaudeStatusLineHook_AbortsWhenPinnedStashCannotMigrate(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	oldPrev := filepath.Join(t.TempDir(), "old-prev.json")
	stashBody := []byte(`{"statusLine":{"type":"command","command":"third-party.sh"}}`)
	if err := os.WriteFile(oldPrev, stashBody, 0o600); err != nil {
		t.Fatal(err)
	}
	oldCmd := statusLinePosixCommand("/old/path/aiexpedite", "/old/cache.json", oldPrev)

	// Resolve the new pinned stash underneath a REGULAR FILE, so both the
	// MkdirAll and any rename/copy fallback are guaranteed to fail on every OS.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	newPrev := filepath.Join(blocker, "sub", "new-prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", newPrev)

	settingsPath := filepath.Join(claudeDir, "settings.json")
	settings := `{"theme":"dark","statusLine":{"type":"command","command":` +
		mustJSONString(t, oldCmd) + `}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := ensureClaudeStatusLineHook(home)
	if err == nil {
		t.Fatalf("install must surface the failed stash migration; changed=%v", changed)
	}
	if changed {
		t.Errorf("install must not report a change when the migration aborted")
	}

	// settings.json is byte-identical: the old command still pins oldPrev.
	got, readErr := os.ReadFile(settingsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != settings {
		t.Errorf("settings.json must be untouched after an aborted migration:\ngot  %s\nwant %s", got, settings)
	}

	// The stash is still where the surviving installed command points.
	stash, readErr := os.ReadFile(oldPrev)
	if readErr != nil {
		t.Fatalf("old stash must survive an aborted migration: %v", readErr)
	}
	if string(stash) != string(stashBody) {
		t.Errorf("old stash body mismatch: got %s want %s", stash, stashBody)
	}

	// Opt-out still restores the third-party command from the pinned path.
	if changed, err := removeClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	raw, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(raw), "third-party.sh") {
		t.Errorf("opt-out lost the chained third-party command: %s", raw)
	}
}

// "Nothing to migrate" is not a migration failure: an unset or unchanged
// pinned path, a stash already sitting at the new path, and an absent old
// stash must all leave the refresh free to proceed.
func TestMigratePrevStatusLine_NoOpCasesSucceed(t *testing.T) {
	dir := t.TempDir()
	newPrev := filepath.Join(dir, "new-prev.json")

	if err := migratePrevStatusLine("", newPrev); err != nil {
		t.Errorf("unset old path must be a no-op, got %v", err)
	}
	if err := migratePrevStatusLine(newPrev, newPrev); err != nil {
		t.Errorf("unchanged path must be a no-op, got %v", err)
	}
	if err := migratePrevStatusLine(filepath.Join(dir, "absent.json"), newPrev); err != nil {
		t.Errorf("absent old stash must be a no-op, got %v", err)
	}
	if _, err := os.Stat(newPrev); !os.IsNotExist(err) {
		t.Errorf("no-op migration must not create the new stash; err=%v", err)
	}

	// A stash already at newPrev wins: the old one is left alone, not copied over.
	if err := os.WriteFile(newPrev, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPrev := filepath.Join(dir, "old-prev.json")
	if err := os.WriteFile(oldPrev, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migratePrevStatusLine(oldPrev, newPrev); err != nil {
		t.Errorf("pre-existing new stash must be a no-op, got %v", err)
	}
	if got, _ := os.ReadFile(newPrev); string(got) != `{"new":true}` {
		t.Errorf("existing new stash must not be overwritten, got %s", got)
	}
}
