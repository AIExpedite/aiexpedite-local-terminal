// statusline_install.go — wires Claude Code's `statusLine` setting to our
// status-line hook so we receive the rate-limit telemetry on every render.
//
// We edit the user's Claude `settings.json` (CLAUDE_CONFIG_DIR or ~/.claude),
// preserving every other key. If a different status-line command was already
// configured we stash it and CHAIN to it from the hook, so a power user's
// custom status line keeps working — we only add the side-channel capture.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// claudeStatusLine mirrors Claude Code's settings.json `statusLine` object.
type claudeStatusLine struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command,omitempty"`
	Padding *int   `json:"padding,omitempty"`
}

// prevStatusLine is the small record we persist so the hook can chain to the
// user's original command (the one we replaced).
type prevStatusLine struct {
	Command string `json:"command"`
}

// prevStatusLinePath is where the replaced command is stashed, inside the
// agent's own data dir (never under Claude's config).
// AIEXPEDITE_CLAUDE_STATUSLINE_PREV overrides it (test isolation).
func prevStatusLinePath() string {
	if p := os.Getenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "claude_statusline_prev.json")
}

// ourStatusLineCommand is the command string we install into Claude's settings:
// the agent binary re-invoked with the hook subcommand. Quoted so a path with
// spaces (e.g. Program Files) runs correctly through Claude's shell.
func ourStatusLineCommand() (string, bool) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", false
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	return strconv.Quote(exe) + " " + statusLineHookArg, true
}

// isOurStatusLineCommand reports whether a configured command is our hook (any
// binary path) — used to stay idempotent and to avoid stashing our own command
// as the "previous" one after the binary path changes across updates.
func isOurStatusLineCommand(command string) bool {
	return strings.Contains(command, statusLineHookArg)
}

// loadPrevStatusLineCommand returns the user's original status-line command we
// stashed, or "" when none was chained.
func loadPrevStatusLineCommand() string {
	b, err := os.ReadFile(prevStatusLinePath())
	if err != nil {
		return ""
	}
	var p prevStatusLine
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return p.Command
}

func savePrevStatusLineCommand(command string) {
	b, err := json.MarshalIndent(prevStatusLine{Command: command}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(GetConfigDir(), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(prevStatusLinePath(), b, 0o600)
}

// ensureClaudeStatusLineHook installs (or refreshes) the status-line hook in
// Claude's settings.json. It is best-effort and idempotent:
//   - Skips entirely when Claude isn't present (no config dir).
//   - Leaves settings untouched when our hook is already configured.
//   - Stashes a pre-existing third-party command and chains to it.
//   - Preserves every other key in settings.json.
//
// Returns true when it wrote a change, false when it was already in place or
// skipped. Errors are returned for logging but are non-fatal to startup.
func ensureClaudeStatusLineHook(home string) (bool, error) {
	base := claudeConfigDir(home)
	if base == "" {
		return false, nil
	}
	// Only act for users who actually have Claude Code — don't materialise a
	// ~/.claude dir for someone who doesn't use it.
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return false, nil
	}

	ours, ok := ourStatusLineCommand()
	if !ok {
		return false, nil
	}

	settingsPath := filepath.Join(base, "settings.json")
	settings := map[string]json.RawMessage{}
	if b, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			// Refuse to clobber an unparseable settings.json — the user may have
			// a comment-laden or hand-edited file; surface the error instead.
			return false, err
		}
	}

	var existing claudeStatusLine
	if rawSL, present := settings["statusLine"]; present {
		_ = json.Unmarshal(rawSL, &existing)
	}

	if existing.Command == ours {
		return false, nil // already installed, exact match
	}

	// A different command that isn't one of ours is a real user/third-party
	// status line — stash it so the hook can chain to it. (A command that IS
	// ours but with a stale binary path just gets its path refreshed; don't
	// stash it as "previous".)
	if existing.Command != "" && !isOurStatusLineCommand(existing.Command) {
		savePrevStatusLineCommand(existing.Command)
	}

	updated, err := json.Marshal(claudeStatusLine{Type: "command", Command: ours})
	if err != nil {
		return false, err
	}
	settings["statusLine"] = updated

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeSettingsAtomic(settingsPath, out); err != nil {
		return false, err
	}
	return true, nil
}

// removeClaudeStatusLineHook reverses ensureClaudeStatusLineHook so the opt-out
// (`disable_claude_status_line_hook`) actually takes effect even after the hook
// was installed on a prior run. If we previously stashed a third-party command
// it is restored; otherwise the `statusLine` key is removed entirely. Other
// keys in settings.json are preserved. Returns true when it wrote a change.
func removeClaudeStatusLineHook(home string) (bool, error) {
	base := claudeConfigDir(home)
	if base == "" {
		return false, nil
	}
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return false, nil
	}

	settingsPath := filepath.Join(base, "settings.json")
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		// No settings file means nothing to revert.
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &settings); err != nil {
		return false, err
	}

	rawSL, present := settings["statusLine"]
	if !present {
		return false, nil
	}
	var existing claudeStatusLine
	_ = json.Unmarshal(rawSL, &existing)
	if !isOurStatusLineCommand(existing.Command) {
		// User has switched to a different command on their own — leave it be.
		return false, nil
	}

	if prev := loadPrevStatusLineCommand(); prev != "" {
		restored, err := json.Marshal(claudeStatusLine{Type: "command", Command: prev})
		if err != nil {
			return false, err
		}
		settings["statusLine"] = restored
	} else {
		delete(settings, "statusLine")
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeSettingsAtomic(settingsPath, out); err != nil {
		return false, err
	}
	// Stash is consumed (or never existed) — drop it so a later re-enable
	// doesn't resurrect a stale chain target.
	_ = os.Remove(prevStatusLinePath())
	return true, nil
}

// writeSettingsAtomic writes settings.json via a temp file + rename so a crash
// mid-write can't leave Claude with a half-written config.
func writeSettingsAtomic(settingsPath string, out []byte) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	tmp := settingsPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, settingsPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
