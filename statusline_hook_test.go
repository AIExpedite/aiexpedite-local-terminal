package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

// The status-line hook scopes each capture to the account that produced it by
// the STABLE credential (.credentials.json), never the shared, mutable
// ~/.claude.json — so a concurrent /login can't misattribute a live capture.
// On top of that, the capture is written UNSCOPED ("") whenever an env
// credential actually WINS Claude's auth precedence, so its usage never merges
// into the credential account's card:
//   - credential account, no env credential   -> scoped to that account
//   - ANTHROPIC_AUTH_TOKEN / CLAUDE_CODE_OAUTH_TOKEN / cloud provider -> unscoped
//   - ANTHROPIC_API_KEY, approved              -> unscoped
//   - ANTHROPIC_API_KEY, declined/unapproved   -> scoped (subscription still active)
//   - identity only in ~/.claude.json          -> unscoped (dotfile must not scope)
func TestCaptureClaudeRateLimitsFromStatusline_ScopesByActiveCredential(t *testing.T) {
	const apiKey = "sk-ant-api03-EXAMPLEKEYSUFFIX20xx" // > 20 chars, exercises truncation
	keySuffix := apiKey[len(apiKey)-20:]
	const credsAccount = "team@corp.example"
	accountFP := fingerprintAccount("claudeCode", credsAccount)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	payload := `{"rate_limits":{"five_hour":{"used_percentage":23.5,"resets_at":` +
		strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + `}}}`

	// writeCreds writes a .credentials.json carrying the given account (the
	// stable identity the cache is scoped by). Empty account => no account field,
	// the common claude.ai login.
	writeCreds := func(t *testing.T, dir, account string) {
		creds := map[string]any{"claudeAiOauth": map[string]any{}}
		if account != "" {
			creds["account"] = account
		}
		helperWriteJSON(t, filepath.Join(dir, ".credentials.json"), creds)
	}
	// writeApproval records API-key approve/decline decisions in .claude.json.
	writeApproval := func(t *testing.T, dir string, approved []string) {
		helperWriteJSON(t, filepath.Join(dir, ".claude.json"), map[string]any{
			"customApiKeyResponses": map[string]any{"approved": approved},
		})
	}

	capture := func(t *testing.T) string {
		cache := filepath.Join(t.TempDir(), "rl.json")
		t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
		captureClaudeRateLimitsFromStatusline([]byte(payload), now)
		snap, ok := loadClaudeRateLimitSnapshot(cache)
		if !ok {
			t.Fatalf("expected cache write")
		}
		return snap.AccountFingerprint
	}

	// clearEnvAuth zeroes every env credential the guard consults so each subtest
	// starts from a clean "subscription only" baseline before setting its one var.
	clearEnvAuth := func(t *testing.T) {
		for _, k := range []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("credential account, no env -> scoped", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		writeCreds(t, configDir, credsAccount)
		if got := capture(t); got != accountFP {
			t.Errorf("AccountFingerprint=%q, want %q (scoped to credential account)", got, accountFP)
		}
	})

	t.Run("dotfile-only identity does NOT scope the live capture", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		writeCreds(t, configDir, "") // claude.ai login: no account in the credential
		// An oauthAccount email in ~/.claude.json must NOT scope this capture — it
		// is shared and mutable, so a concurrent /login could misattribute it.
		helperWriteJSON(t, filepath.Join(configDir, ".claude.json"), map[string]any{
			"oauthAccount": map[string]any{"emailAddress": "grace@example.com"},
		})
		if got := capture(t); got != "" {
			t.Errorf("AccountFingerprint=%q, want empty (dotfile must not scope a live capture)", got)
		}
	})

	t.Run("auth-token always unscopes", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "bearer-xyz")
		writeCreds(t, configDir, credsAccount)
		if got := capture(t); got != "" {
			t.Errorf("AccountFingerprint=%q, want empty (auth token is active)", got)
		}
	})

	t.Run("oauth token unscopes", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-xyz")
		writeCreds(t, configDir, credsAccount)
		if got := capture(t); got != "" {
			t.Errorf("AccountFingerprint=%q, want empty (oauth token may be a different account)", got)
		}
	})

	t.Run("cloud provider unscopes", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
		writeCreds(t, configDir, credsAccount)
		if got := capture(t); got != "" {
			t.Errorf("AccountFingerprint=%q, want empty (cloud provider routes off the subscription)", got)
		}
	})

	t.Run("approved api key unscopes", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		t.Setenv("ANTHROPIC_API_KEY", apiKey)
		writeCreds(t, configDir, credsAccount)
		writeApproval(t, configDir, []string{keySuffix})
		if got := capture(t); got != "" {
			t.Errorf("AccountFingerprint=%q, want empty (approved api key is active)", got)
		}
	})

	t.Run("declined api key stays scoped", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", configDir)
		clearEnvAuth(t)
		t.Setenv("ANTHROPIC_API_KEY", apiKey)
		writeCreds(t, configDir, credsAccount)
		writeApproval(t, configDir, []string{}) // present but not approved
		if got := capture(t); got != accountFP {
			t.Errorf("AccountFingerprint=%q, want %q (declined key -> subscription active)", got, accountFP)
		}
	})
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
	// already forward-slash. Either way: no backslashes, a quoted path, hook arg.
	if strings.Contains(cmd, `\`) {
		t.Errorf("command must not contain backslashes: %q", cmd)
	}
	// Quoted with either single quotes (POSIX/Git Bash form, safe against `$`/
	// backtick expansion) or double quotes (PowerShell form, escaped via
	// powerShellDoubleQuote). A bare/unquoted exe path is never emitted.
	if !(strings.Contains(cmd, `"`) || strings.Contains(cmd, `'`)) ||
		!strings.Contains(cmd, statusLineHookArg) {
		t.Errorf("command should carry a quoted path + hook arg: %q", cmd)
	}
	// Windows: either the Git Bash form (`KEYS… "path" hook`) or the PowerShell
	// call-operator form (`$env:…; & "path" hook`) — never an unprefixed quoted
	// path that PowerShell would treat as a bare string. The PowerShell preamble
	// terminates each `$env:` assignment with `;`, so the `& "` call op is what
	// the inner exe invocation starts with.
	if runtime.GOOS == "windows" && findGitBash() == "" && !strings.Contains(cmd, `; & "`) {
		t.Errorf("PowerShell command must invoke exe via the & call operator: %q", cmd)
	}
}

func TestOurStatusLineCommand_PinsCacheAndStashPaths(t *testing.T) {
	// Pin the cache + stash paths so the installed command is independent of
	// the caller environment (e.g. Claude launching with a different
	// XDG_CONFIG_HOME than the agent had at install time).
	cache := filepath.Join(t.TempDir(), "rl.json")
	prev := filepath.Join(t.TempDir(), "prev.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", prev)

	cmd, ok := ourStatusLineCommand()
	if !ok {
		t.Fatal("expected a command")
	}
	// Both keys must appear, and the resolved paths must be embedded so the
	// hook resolves them identically regardless of how Claude is launched.
	if !strings.Contains(cmd, "AIEXPEDITE_CLAUDE_RL_CACHE") ||
		!strings.Contains(cmd, "AIEXPEDITE_CLAUDE_STATUSLINE_PREV") {
		t.Fatalf("command should embed both env keys: %q", cmd)
	}
	// On Windows we forward-slash the embedded paths so they survive Git Bash
	// quoting and stay backslash-free in the test assertion above.
	wantCache := strings.ReplaceAll(cache, `\`, "/")
	wantPrev := strings.ReplaceAll(prev, `\`, "/")
	if !strings.Contains(cmd, wantCache) || !strings.Contains(cmd, wantPrev) {
		t.Errorf("command should embed resolved paths %q + %q: %q", wantCache, wantPrev, cmd)
	}
	// Round-trip: the command must still be recognized as ours so the next
	// install doesn't stash it as a third-party command.
	if !isOurStatusLineCommand(cmd) {
		t.Errorf("env-pinned command not recognized as ours: %q", cmd)
	}
}

func TestShellCommand_UsesClaudeShellNotCmd(t *testing.T) {
	cmd := shellCommand("echo hi")
	base := strings.ToLower(filepath.Base(cmd.Path))
	if runtime.GOOS == "windows" {
		// cmd silently breaks Bash-style stashed commands (~ / .sh); we must use
		// Git Bash or PowerShell, matching how Claude ran the original.
		if strings.HasPrefix(base, "cmd") {
			t.Errorf("must not chain via cmd: %s", cmd.Path)
		}
		if !strings.Contains(base, "bash") && !strings.Contains(base, "powershell") {
			t.Errorf("expected git bash or powershell, got %s", cmd.Path)
		}
		// When falling back to PowerShell, mirror Claude's own
		// `-ExecutionPolicy Bypass` so a stashed `.ps1` survives the default
		// Restricted policy instead of silently failing to the default line.
		if strings.Contains(base, "powershell") {
			joined := strings.Join(cmd.Args, " ")
			if !strings.Contains(joined, "-ExecutionPolicy") || !strings.Contains(joined, "Bypass") {
				t.Errorf("powershell branch must pass -ExecutionPolicy Bypass: %v", cmd.Args)
			}
		}
	} else if !strings.Contains(base, "sh") {
		t.Errorf("expected sh, got %s", cmd.Path)
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

func TestIsOurStatusLineCommand_OnlyMatchesInstalledShape(t *testing.T) {
	ours := []string{
		`"/usr/local/bin/aiexpedite" statusline-hook`,
		`"C:/Users/x/aiexpedite.exe" statusline-hook`,
		`& "C:/Users/x/aiexpedite.exe" statusline-hook`,
		`  "/x/aiexpedite" statusline-hook  `, // surrounding whitespace tolerated
		// Legacy double-quoted-exe shapes (previously installed entries) must
		// continue to be claimed as ours after we switched to POSIX single
		// quotes — otherwise the next install would treat the user's existing
		// (older-shape) hook entry as third-party and stash it.
		// Current single-quoted POSIX shape, both with and without env preamble.
		`'/usr/local/bin/aiexpedite' statusline-hook`,
		`AIEXPEDITE_CLAUDE_RL_CACHE='/x/cache.json' AIEXPEDITE_CLAUDE_STATUSLINE_PREV='/x/prev.json' '/x/aiexpedite' statusline-hook`,
		// Env-pinned shapes must also be recognized as ours so a refreshed
		// data-dir path (or an upgrade across the old shape) doesn't make the
		// installer stash our own command as the "previous" one.
		`AIEXPEDITE_CLAUDE_RL_CACHE='/x/cache.json' AIEXPEDITE_CLAUDE_STATUSLINE_PREV='/x/prev.json' "/x/aiexpedite" statusline-hook`,
		`$env:AIEXPEDITE_CLAUDE_RL_CACHE="C:/x/cache.json"; $env:AIEXPEDITE_CLAUDE_STATUSLINE_PREV="C:/x/prev.json"; & "C:/x/aiexpedite.exe" statusline-hook`,
	}
	for _, c := range ours {
		if !isOurStatusLineCommand(c) {
			t.Errorf("expected to be recognized as ours: %q", c)
		}
	}
	// Substring `statusline-hook` in a user-authored command must NOT be claimed
	// as ours — otherwise install would overwrite without stashing, and opt-out
	// would later delete the statusLine instead of restoring the user's command.
	notOurs := []string{
		`~/.claude/statusline-hook.sh`,
		`bash ~/.claude/statusline-hook.sh`,
		`my-custom-line.sh`,
		`statusline-hook`,                         // bare arg, no quoted path
		`"/x/aiexpedite" statusline-hook --extra`, // trailing extras aren't ours
		`"/x/statusline-hook.sh"`,                 // hook arg only as substring
		``,
	}
	for _, c := range notOurs {
		if isOurStatusLineCommand(c) {
			t.Errorf("must NOT be recognized as ours: %q", c)
		}
	}
}

func TestEnsureClaudeStatusLineHook_StashesUserHookSubstringCommand(t *testing.T) {
	// A user-authored command that merely CONTAINS the substring `statusline-hook`
	// (e.g. their own `~/.claude/statusline-hook.sh`) must be stashed and chained
	// to, not silently overwritten. Regression test for the old substring match.
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	seed := `{"statusLine":{"type":"command","command":"~/.claude/statusline-hook.sh"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := ensureClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	if prev := loadPrevStatusLineCommand(); prev != "~/.claude/statusline-hook.sh" {
		t.Errorf("user's hook-substring command not stashed: %q", prev)
	}
	// Opt-out restores it verbatim.
	if changed, err := removeClaudeStatusLineHook(home); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	sl := readStatusLine(t, filepath.Join(claudeDir, "settings.json"))
	if sl.Command != "~/.claude/statusline-hook.sh" {
		t.Errorf("user's command not restored on opt-out: %+v", sl)
	}
}

func TestEnsureClaudeStatusLineHook_AbortsOnUnreadableSettings(t *testing.T) {
	// A non-`IsNotExist` read error must abort the install — otherwise the
	// atomic write would replace settings.json with only `statusLine`, dropping
	// every other Claude setting.
	home := t.TempDir()
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))

	// settings.json is a DIRECTORY — os.ReadFile returns a non-IsNotExist error
	// (EISDIR), simulating an unreadable settings file in a portable way.
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.MkdirAll(settingsPath, 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := ensureClaudeStatusLineHook(home)
	if err == nil {
		t.Fatalf("expected error on unreadable settings.json, got nil (changed=%v)", changed)
	}
	if changed {
		t.Errorf("install must not report changed=true on read failure")
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
