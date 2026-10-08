package main

// Merge safety of the SessionEnd entry in Claude's settings.json
// (claude_run_end_hook_install.go): ours is added, re-pointed and removed
// without disturbing anything the user or another tool put there.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// armClaudeRunEndInstallTest points Claude's config dir and the agent's cache
// at temp dirs, pins the executable seam, and returns the settings path.
func armClaudeRunEndInstallTest(t *testing.T, exe string) string {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(t.TempDir(), "prev.json"))
	setClaudeHookExecutable(t, exe)
	return filepath.Join(configDir, "settings.json")
}

func setClaudeHookExecutable(t *testing.T, exe string) {
	t.Helper()
	prev := claudeHookExecutable
	claudeHookExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { claudeHookExecutable = prev })
}

func readSettingsMap(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings.json no longer parses: %v", err)
	}
	return m
}

// sessionEndCommands lists every command hook under hooks.SessionEnd.
func sessionEndCommands(t *testing.T, settings map[string]any) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[claudeRunEndHookEvent].([]any)
	var out []string
	for _, g := range groups {
		inner, _ := g.(map[string]any)["hooks"].([]any)
		for _, h := range inner {
			if c, ok := h.(map[string]any)["command"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func countOurRunEndHooks(cmds []string) int {
	n := 0
	for _, c := range cmds {
		if isOurClaudeHookCommand(c, claudeRunEndHookArg) {
			n++
		}
	}
	return n
}

func TestEnsureClaudeRunEndHook_PreservesEverythingElseAndIsIdempotent(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
	helperWriteJSON(t, settingsPath, map[string]any{
		"model":       "opus",
		"permissions": map[string]any{"allow": []any{"Bash(git status)"}},
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "~/bin/log-session.sh"}}},
			},
			"PreToolUse": []any{
				map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "guard.sh"}}},
			},
		},
	})

	changed, err := ensureClaudeRunEndHook("")
	if err != nil || !changed {
		t.Fatalf("ensure: changed=%v err=%v, want an install", changed, err)
	}
	settings := readSettingsMap(t, settingsPath)
	if settings["model"] != "opus" || settings["permissions"] == nil {
		t.Fatalf("unrelated keys were not preserved: %+v", settings)
	}
	hooks := settings["hooks"].(map[string]any)
	if pre, _ := hooks["PreToolUse"].([]any); len(pre) != 1 {
		t.Fatalf("PreToolUse = %+v, want the user's group preserved", hooks["PreToolUse"])
	}
	cmds := sessionEndCommands(t, settings)
	if len(cmds) != 2 || cmds[0] != "~/bin/log-session.sh" || countOurRunEndHooks(cmds) != 1 {
		t.Fatalf("SessionEnd commands = %q, want the user's group first and ours once", cmds)
	}
	ours, _ := ourClaudeHookCommand(claudeRunEndHookArg)
	if cmds[1] != ours || !strings.Contains(ours, "AIEXPEDITE_CLAUDE_RL_CACHE") || strings.Contains(ours, "STATUSLINE_PREV") {
		t.Fatalf("our command = %q, want the cache pin only", cmds[1])
	}

	before, _ := os.ReadFile(settingsPath)
	if changed, err := ensureClaudeRunEndHook(""); err != nil || changed {
		t.Fatalf("re-run: changed=%v err=%v, want a no-op", changed, err)
	}
	if after, _ := os.ReadFile(settingsPath); string(after) != string(before) {
		t.Fatal("an idempotent re-run rewrote settings.json")
	}
}

func TestEnsureClaudeRunEndHook_RepointsAfterTheBinaryMoves(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/old/aiexpedite-terminal")
	if _, err := ensureClaudeRunEndHook(""); err != nil {
		t.Fatal(err)
	}
	setClaudeHookExecutable(t, "/opt/new/aiexpedite-terminal")
	changed, err := ensureClaudeRunEndHook("")
	if err != nil || !changed {
		t.Fatalf("ensure after a move: changed=%v err=%v", changed, err)
	}
	cmds := sessionEndCommands(t, readSettingsMap(t, settingsPath))
	if len(cmds) != 1 || !strings.Contains(cmds[0], "/opt/new/") || strings.Contains(cmds[0], "/opt/old/") {
		t.Fatalf("SessionEnd commands = %q, want one entry re-pointed at the new binary", cmds)
	}
}

// Two groups of ours (a hand edit, a merge race) collapse into one.
func TestEnsureClaudeRunEndHook_CollapsesDuplicates(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
	ours, _ := ourClaudeHookCommand(claudeRunEndHookArg)
	group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": ours, "timeout": claudeRunEndHookTimeoutSeconds}}}
	helperWriteJSON(t, settingsPath, map[string]any{"hooks": map[string]any{"SessionEnd": []any{group, group}}})
	if changed, err := ensureClaudeRunEndHook(""); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if cmds := sessionEndCommands(t, readSettingsMap(t, settingsPath)); len(cmds) != 1 {
		t.Fatalf("SessionEnd commands = %q, want exactly one", cmds)
	}
}

// A user (or another tool) adds a sibling command to the group holding ours.
// Ours is still recognised there: a move re-points it in place without adding
// a second copy, and removal takes only ours, leaving the sibling's group.
func TestClaudeRunEndHook_OwnsItsCommandInsideAMultiHookGroup(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/old/aiexpedite-terminal")
	oldOurs, _ := ourClaudeHookCommand(claudeRunEndHookArg)
	helperWriteJSON(t, settingsPath, map[string]any{"hooks": map[string]any{"SessionEnd": []any{
		map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": oldOurs, "timeout": claudeRunEndHookTimeoutSeconds},
			map[string]any{"type": "command", "command": "~/bin/log-session.sh", "timeout": 5},
		}},
	}}})

	setClaudeHookExecutable(t, "/opt/new/aiexpedite-terminal")
	if changed, err := ensureClaudeRunEndHook(""); err != nil || !changed {
		t.Fatalf("ensure after a move: changed=%v err=%v", changed, err)
	}
	settings := readSettingsMap(t, settingsPath)
	groups := settings["hooks"].(map[string]any)[claudeRunEndHookEvent].([]any)
	cmds := sessionEndCommands(t, settings)
	if len(groups) != 1 || len(cmds) != 2 || !strings.Contains(cmds[0], "/opt/new/") || cmds[1] != "~/bin/log-session.sh" {
		t.Fatalf("SessionEnd = %+v, want ours re-pointed in place beside the sibling, in one group", groups)
	}
	sibling := groups[0].(map[string]any)["hooks"].([]any)[1].(map[string]any)
	if sibling["timeout"] != float64(5) {
		t.Fatalf("sibling = %+v, want its fields preserved", sibling)
	}
	if changed, err := ensureClaudeRunEndHook(""); err != nil || changed {
		t.Fatalf("re-run: changed=%v err=%v, want a no-op", changed, err)
	}

	if changed, err := removeClaudeRunEndHook(""); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if cmds := sessionEndCommands(t, readSettingsMap(t, settingsPath)); len(cmds) != 1 || cmds[0] != "~/bin/log-session.sh" {
		t.Fatalf("SessionEnd commands = %q, want only the sibling", cmds)
	}
}

// Ours inside a group with a matcher would fire only on that end reason: it is
// moved to a group of its own, and the matcher group keeps its sibling.
func TestEnsureClaudeRunEndHook_MovesOursOutOfAMatcherGroup(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
	ours, _ := ourClaudeHookCommand(claudeRunEndHookArg)
	helperWriteJSON(t, settingsPath, map[string]any{"hooks": map[string]any{"SessionEnd": []any{
		map[string]any{"matcher": "logout", "hooks": []any{
			map[string]any{"type": "command", "command": "~/bin/on-logout.sh"},
			map[string]any{"type": "command", "command": ours, "timeout": claudeRunEndHookTimeoutSeconds},
		}},
	}}})
	if changed, err := ensureClaudeRunEndHook(""); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	groups := readSettingsMap(t, settingsPath)["hooks"].(map[string]any)[claudeRunEndHookEvent].([]any)
	if len(groups) != 2 {
		t.Fatalf("SessionEnd = %+v, want the matcher group and our own group", groups)
	}
	first := groups[0].(map[string]any)
	if first["matcher"] != "logout" || len(first["hooks"].([]any)) != 1 {
		t.Fatalf("matcher group = %+v, want only its sibling left", first)
	}
	second := groups[1].(map[string]any)
	if _, ok := second["matcher"]; ok || second["hooks"].([]any)[0].(map[string]any)["command"] != ours {
		t.Fatalf("our group = %+v, want ours without a matcher", second)
	}
}

func TestRemoveClaudeRunEndHook_DeletesOnlyOursAndPrunes(t *testing.T) {
	settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
	helperWriteJSON(t, settingsPath, map[string]any{
		"hooks": map[string]any{"SessionEnd": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "~/bin/log-session.sh"}}},
		}},
	})
	if _, err := ensureClaudeRunEndHook(""); err != nil {
		t.Fatal(err)
	}
	if changed, err := removeClaudeRunEndHook(""); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	if cmds := sessionEndCommands(t, readSettingsMap(t, settingsPath)); len(cmds) != 1 || cmds[0] != "~/bin/log-session.sh" {
		t.Fatalf("SessionEnd commands = %q, want only the user's", cmds)
	}

	// With nothing else there, the emptied array and hooks object go too.
	helperWriteJSON(t, settingsPath, map[string]any{"model": "opus"})
	if _, err := ensureClaudeRunEndHook(""); err != nil {
		t.Fatal(err)
	}
	if _, err := removeClaudeRunEndHook(""); err != nil {
		t.Fatal(err)
	}
	settings := readSettingsMap(t, settingsPath)
	if _, ok := settings["hooks"]; ok || settings["model"] != "opus" {
		t.Fatalf("settings = %+v, want the empty hooks object pruned and model kept", settings)
	}
	if changed, err := removeClaudeRunEndHook(""); err != nil || changed {
		t.Fatalf("second remove: changed=%v err=%v, want a no-op", changed, err)
	}
}

func TestClaudeRunEndHookInstall_LeavesMalformedSettingsUntouched(t *testing.T) {
	for _, body := range []string{`{"model": "opus",`, `{"hooks": []}`, `{"hooks": {"SessionEnd": {}}}`} {
		settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
		if err := os.WriteFile(settingsPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if changed, err := ensureClaudeRunEndHook(""); err == nil || changed {
			t.Errorf("%s: ensure changed=%v err=%v, want an error and no write", body, changed, err)
		}
		if changed, err := removeClaudeRunEndHook(""); err == nil || changed {
			t.Errorf("%s: remove changed=%v err=%v, want an error and no write", body, changed, err)
		}
		if after, _ := os.ReadFile(settingsPath); string(after) != body {
			t.Errorf("%s: settings.json was rewritten to %s", body, after)
		}
	}
}

// Claude not installed: nothing is created.
func TestEnsureClaudeRunEndHook_SkipsWithoutAClaudeConfigDir(t *testing.T) {
	armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
	missing := filepath.Join(t.TempDir(), "absent")
	t.Setenv("CLAUDE_CONFIG_DIR", missing)
	if changed, err := ensureClaudeRunEndHook(""); err != nil || changed {
		t.Fatalf("changed=%v err=%v, want a skip", changed, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("the installer materialised a Claude config dir")
	}
}

// The startup opt-outs: the status-line opt-out (removeClaudeStatusLineHook)
// takes the run-end hook with it, and the probe opt-out removes it on its own
// while the status line stays.
func TestClaudeRunEndHook_EitherOptOutRemovesIt(t *testing.T) {
	t.Run("status-line opt-out", func(t *testing.T) {
		settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
		if _, err := ensureClaudeStatusLineHook(""); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureClaudeRunEndHook(""); err != nil {
			t.Fatal(err)
		}
		if changed, err := removeClaudeStatusLineHook(""); err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		settings := readSettingsMap(t, settingsPath)
		if _, ok := settings["statusLine"]; ok {
			t.Fatalf("statusLine survived the opt-out: %+v", settings)
		}
		if _, ok := settings["hooks"]; ok {
			t.Fatalf("the run-end hook survived the status-line opt-out: %+v", settings)
		}
	})
	t.Run("probe opt-out", func(t *testing.T) {
		settingsPath := armClaudeRunEndInstallTest(t, "/opt/aiexpedite/aiexpedite-terminal")
		if _, err := ensureClaudeStatusLineHook(""); err != nil {
			t.Fatal(err)
		}
		if _, err := applyClaudeRunEndHook("", true); err != nil {
			t.Fatal(err)
		}
		if changed, err := applyClaudeRunEndHook("", false); err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		settings := readSettingsMap(t, settingsPath)
		if _, ok := settings["statusLine"]; !ok {
			t.Fatal("the probe opt-out removed the status line")
		}
		if cmds := sessionEndCommands(t, settings); countOurRunEndHooks(cmds) != 0 {
			t.Fatalf("SessionEnd commands = %q, want ours removed", cmds)
		}
	})
}
