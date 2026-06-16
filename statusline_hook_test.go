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
