package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCaptureClaudeRateLimitsFromStatusline_BothWindows(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // unscoped fingerprint ""

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fiveReset := now.Add(time.Hour).Unix()
	weekReset := now.Add(72 * time.Hour).Unix()
	// The status-line stdin payload: rate_limits map + unrelated fields.
	payload := `{"model":{"display_name":"Sonnet"},"workspace":{"current_dir":"/x/my-repo"},` +
		`"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":` + strconv.FormatInt(fiveReset, 10) +
		`},"seven_day":{"used_percentage":81,"resets_at":` + strconv.FormatInt(weekReset, 10) + `}}}`

	captureClaudeRateLimitsFromStatusline([]byte(payload), now)

	snap, ok := loadClaudeRateLimitSnapshot(cache)
	if !ok {
		t.Fatalf("expected cache write")
	}
	if b := snap.Buckets[claudeWindowFiveHour]; b.UsedPercentage != 23.5 || b.ResetsAtMs != fiveReset*1000 {
		t.Errorf("five_hour=%+v, want 23.5%% reset %d", b, fiveReset*1000)
	}
	if b := snap.Buckets[claudeWindowSevenDay]; b.UsedPercentage != 81 || b.ResetsAtMs != weekReset*1000 {
		t.Errorf("seven_day=%+v, want 81%% reset %d", b, weekReset*1000)
	}

	// The 5-hour window now populates from the status line even though no
	// rate_limit_event ever carried it — the whole point of the side channel.
	metrics := claudeCodeMetricsFromCache(now, "")
	if metrics[0].Unknown {
		t.Errorf("5-hour session window should be observed from the status line")
	}
}

func TestDefaultStatusLine(t *testing.T) {
	payload := `{"model":{"display_name":"Opus 4.8"},"cwd":"C:\\code\\my-repo",` +
		`"rate_limits":{"five_hour":{"used_percentage":23.5},"seven_day":{"used_percentage":81}}}`
	got := defaultStatusLine([]byte(payload))
	for _, want := range []string{"Opus 4.8", "my-repo", "5h 24%", "7d 81%"} {
		if !strings.Contains(got, want) {
			t.Errorf("status line %q missing %q", got, want)
		}
	}
}

func TestEnsureClaudeStatusLineHook_FreshInstallAndIdempotent(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	changed, err := ensureClaudeStatusLineHook(home)
	if err != nil || !changed {
		t.Fatalf("fresh install: changed=%v err=%v", changed, err)
	}
	sl := readStatusLine(t, filepath.Join(claudeDir, "settings.json"))
	if sl.Type != "command" || !strings.Contains(sl.Command, statusLineHookArg) {
		t.Fatalf("statusLine not wired to hook: %+v", sl)
	}

	// Second call is a no-op (already ours).
	changed2, err := ensureClaudeStatusLineHook(home)
	if err != nil || changed2 {
		t.Errorf("idempotent call should not change: changed=%v err=%v", changed2, err)
	}
}

func TestEnsureClaudeStatusLineHook_StashesExistingAndPreservesKeys(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	prevFile := filepath.Join(t.TempDir(), "prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", prevFile)

	// Pre-existing third-party status line + an unrelated key that must survive.
	seed := `{"theme":"dark","statusLine":{"type":"command","command":"my-custom-line.sh"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := ensureClaudeStatusLineHook(home)
	if err != nil || !changed {
		t.Fatalf("install over existing: changed=%v err=%v", changed, err)
	}

	// The user's command is stashed for chaining.
	if prev := loadPrevStatusLineCommand(); prev != "my-custom-line.sh" {
		t.Errorf("prev command=%q, want my-custom-line.sh", prev)
	}
	// Our hook is installed, and the unrelated key is preserved.
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["theme"]; !ok {
		t.Errorf("unrelated 'theme' key was dropped: %s", raw)
	}
	var sl claudeStatusLine
	_ = json.Unmarshal(settings["statusLine"], &sl)
	if !strings.Contains(sl.Command, statusLineHookArg) {
		t.Errorf("statusLine not replaced with hook: %+v", sl)
	}
}

func TestEnsureClaudeStatusLineHook_SkipsWhenNoClaude(t *testing.T) {
	home := t.TempDir() // no ~/.claude created
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	changed, err := ensureClaudeStatusLineHook(home)
	if changed || err != nil {
		t.Errorf("must skip when Claude absent: changed=%v err=%v", changed, err)
	}
}

func TestOurStatusLineCommand_NoBackslashesAndQuoted(t *testing.T) {
	cmd, ok := ourStatusLineCommand()
	if !ok {
		t.Fatal("expected a command")
	}
	// On Windows the exe path must be forward-slashed so Claude's Git Bash
	// invocation doesn't treat backslashes as escapes; elsewhere paths are
	// already forward-slash. Either way: no backslashes, quoted, hook arg.
	if strings.Contains(cmd, `\`) {
		t.Errorf("command must not contain backslashes: %q", cmd)
	}
	if !strings.HasPrefix(cmd, `"`) || !strings.Contains(cmd, statusLineHookArg) {
		t.Errorf("command should be a quoted path + hook arg: %q", cmd)
	}
}

func TestEnsureClaudeStatusLineHook_PreservesStatusLineOptions(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	// A custom status line with documented options that must survive.
	seed := `{"statusLine":{"type":"command","command":"my-line.sh","padding":2,` +
		`"refreshInterval":1000,"hideVimModeIndicator":true}}`
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if changed, err := ensureClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}

	// Merged object keeps padding / refreshInterval / hideVimModeIndicator and
	// only swaps in our command.
	sl := rawStatusLineMap(t, settingsPath)
	if string(sl["padding"]) != "2" || string(sl["refreshInterval"]) != "1000" ||
		string(sl["hideVimModeIndicator"]) != "true" {
		t.Errorf("options not preserved on install: %v", sl)
	}
	var cmd string
	_ = json.Unmarshal(sl["command"], &cmd)
	if !strings.Contains(cmd, statusLineHookArg) {
		t.Errorf("command not swapped to hook: %q", cmd)
	}

	// Opt-out restores the user's FULL original object — options included.
	if changed, err := removeClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	restored := rawStatusLineMap(t, settingsPath)
	var rcmd string
	_ = json.Unmarshal(restored["command"], &rcmd)
	if rcmd != "my-line.sh" || string(restored["padding"]) != "2" ||
		string(restored["refreshInterval"]) != "1000" || string(restored["hideVimModeIndicator"]) != "true" {
		t.Errorf("original options not fully restored on opt-out: %v", restored)
	}
}

func rawStatusLineMap(t *testing.T, settingsPath string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	var sl map[string]json.RawMessage
	if err := json.Unmarshal(settings["statusLine"], &sl); err != nil {
		t.Fatalf("parse statusLine: %v", err)
	}
	return sl
}

func TestRemoveClaudeStatusLineHook_RestoresStashedCommand(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	prevFile := filepath.Join(t.TempDir(), "prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", prevFile)

	// Install once over an existing third-party command — this stashes the user's
	// command and writes our hook into settings.json.
	seed := `{"theme":"dark","statusLine":{"type":"command","command":"my-custom-line.sh"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureClaudeStatusLineHook(home); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Now opt out: the stashed command must be restored verbatim and other keys
	// preserved.
	changed, err := removeClaudeStatusLineHook(home)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	sl := readStatusLine(t, filepath.Join(claudeDir, "settings.json"))
	if sl.Type != "command" || sl.Command != "my-custom-line.sh" {
		t.Errorf("statusLine not restored to stashed command: %+v", sl)
	}
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["theme"]; !ok {
		t.Errorf("unrelated 'theme' key dropped during opt-out: %s", raw)
	}
	if _, err := os.Stat(prevFile); !os.IsNotExist(err) {
		t.Errorf("prev stash should be cleared after restore; err=%v", err)
	}

	// Second call is a no-op now that our hook is gone.
	changed2, err := removeClaudeStatusLineHook(home)
	if err != nil || changed2 {
		t.Errorf("idempotent remove should not change: changed=%v err=%v", changed2, err)
	}
}

func TestRemoveClaudeStatusLineHook_DropsStatusLineWhenNoStash(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	// Fresh install (no prior command stashed).
	if _, err := ensureClaudeStatusLineHook(home); err != nil {
		t.Fatalf("install: %v", err)
	}

	changed, err := removeClaudeStatusLineHook(home)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	raw, _ := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["statusLine"]; ok {
		t.Errorf("statusLine should be removed when nothing was stashed: %s", raw)
	}
}

func TestRemoveClaudeStatusLineHook_LeavesForeignCommandAlone(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	// settings.json carries a non-ours statusLine (user re-wired manually).
	seed := `{"statusLine":{"type":"command","command":"my-custom-line.sh"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := removeClaudeStatusLineHook(home)
	if err != nil || changed {
		t.Errorf("remove should be a no-op when statusLine isn't ours: changed=%v err=%v", changed, err)
	}
	sl := readStatusLine(t, filepath.Join(claudeDir, "settings.json"))
	if sl.Command != "my-custom-line.sh" {
		t.Errorf("foreign command got clobbered: %+v", sl)
	}
}

func readStatusLine(t *testing.T, path string) claudeStatusLine {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	var sl claudeStatusLine
	_ = json.Unmarshal(settings["statusLine"], &sl)
	return sl
}
