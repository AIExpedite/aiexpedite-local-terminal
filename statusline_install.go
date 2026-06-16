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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// findGitBash mirrors how Claude Code decides the Windows status-line shell:
// Git Bash when installed, PowerShell otherwise (per the Claude status-line
// "Windows configuration" docs). Returns the path to Git Bash's bash.exe, or ""
// when not found. Checks the standard Git-for-Windows install locations, then
// derives it from `git` on PATH. A PATH-resolved `bash.exe` is only accepted
// when its path looks like Git for Windows (contains a `git` segment) — a bare
// MSYS2/Cygwin/WSL `bash` is NOT Git Bash, and accepting it would emit the
// no-`&` form that PowerShell (Claude's actual fallback shell here) treats as
// a string literal, so the hook would silently never run.
func findGitBash() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	var candidates []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if root := os.Getenv(env); root != "" {
			candidates = append(candidates, filepath.Join(root, "Git", "bin", "bash.exe"))
		}
	}
	if la := os.Getenv("LocalAppData"); la != "" {
		candidates = append(candidates, filepath.Join(la, "Programs", "Git", "bin", "bash.exe"))
	}
	if gitPath, err := exec.LookPath("git"); err == nil {
		// ...\Git\cmd\git.exe -> ...\Git\bin\bash.exe
		gitDir := filepath.Dir(filepath.Dir(gitPath))
		candidates = append(candidates, filepath.Join(gitDir, "bin", "bash.exe"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("bash"); err == nil && isGitForWindowsBashPath(p) {
		return p
	}
	return ""
}

// isGitForWindowsBashPath returns true for a bash.exe that lives under a Git
// for Windows install layout (a `git` path segment, not under System32).
// Filters out WSL (System32), MSYS2, Cygwin, and other non-Git-Bash shells
// that Claude does NOT route the status-line command through.
func isGitForWindowsBashPath(p string) bool {
	lp := strings.ToLower(filepath.ToSlash(p))
	if strings.Contains(lp, "/windows/system32/") {
		return false
	}
	for _, seg := range strings.Split(lp, "/") {
		if seg == "git" {
			return true
		}
	}
	return false
}

// claudeStatusLine mirrors Claude Code's settings.json `statusLine` object.
type claudeStatusLine struct {
	Type    string `json:"type,omitempty"`
	Command string `json:"command,omitempty"`
	Padding *int   `json:"padding,omitempty"`
}

// prevStatusLine is the record we persist so the hook can chain to the user's
// original command AND opt-out can restore their full original statusLine
// object (padding / refreshInterval / hideVimModeIndicator and any other
// options) verbatim — not just type + command.
type prevStatusLine struct {
	StatusLine json.RawMessage `json:"statusLine,omitempty"`
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
// the agent binary re-invoked with the hook subcommand.
//
// On Windows, Claude runs the command through Git Bash when installed, else
// PowerShell — and no single literal works in both for a quoted/spaced path
// (Git Bash chokes on a leading `&`; PowerShell treats a quoted path as a mere
// string unless invoked with the `&` call operator). So we mirror Claude's own
// Git-Bash detection and emit the matching form:
//   - Git Bash:   "C:/path/exe" statusline-hook        (forward-slashed, quoted)
//   - PowerShell: & "C:/path/exe" statusline-hook       (call operator)
//
// Re-evaluated on every startup, so adding/removing Git Bash self-corrects.
func ourStatusLineCommand() (string, bool) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", false
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	if runtime.GOOS != "windows" {
		return `"` + exe + `" ` + statusLineHookArg, true
	}
	exe = strings.ReplaceAll(exe, `\`, "/")
	if findGitBash() != "" {
		return `"` + exe + `" ` + statusLineHookArg, true
	}
	return `& "` + exe + `" ` + statusLineHookArg, true
}

// isOurStatusLineCommand reports whether a configured command is our hook (any
// binary path) — used to stay idempotent and to avoid stashing our own command
// as the "previous" one after the binary path changes across updates.
//
// Matches the EXACT shape `ourStatusLineCommand` emits — a quoted executable
// path followed by ` statusline-hook`, with an optional leading `&` call op for
// the PowerShell form. A loose substring match would falsely claim a user's own
// command (e.g. `~/.claude/statusline-hook.sh`) as ours and silently overwrite
// it without stashing, leaving opt-out unable to restore it.
func isOurStatusLineCommand(command string) bool {
	s := strings.TrimSpace(command)
	suffix := " " + statusLineHookArg
	if !strings.HasSuffix(s, suffix) {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(s, suffix))
	// PowerShell form starts with the `&` call operator before the quoted path.
	prefix = strings.TrimSpace(strings.TrimPrefix(prefix, "&"))
	return len(prefix) >= 2 && strings.HasPrefix(prefix, `"`) && strings.HasSuffix(prefix, `"`)
}

// loadPrevStatusLine returns the full original statusLine object we stashed.
func loadPrevStatusLine() (json.RawMessage, bool) {
	b, err := os.ReadFile(prevStatusLinePath())
	if err != nil {
		return nil, false
	}
	var p prevStatusLine
	if json.Unmarshal(b, &p) != nil || len(p.StatusLine) == 0 {
		return nil, false
	}
	return p.StatusLine, true
}

// loadPrevStatusLineCommand extracts just the command from the stashed object
// (the hook chains to it), or "" when none was stashed.
func loadPrevStatusLineCommand() string {
	raw, ok := loadPrevStatusLine()
	if !ok {
		return ""
	}
	var sl claudeStatusLine
	_ = json.Unmarshal(raw, &sl)
	return sl.Command
}

// savePrevStatusLine persists the user's original statusLine object. Returns an
// error on failure so the install can abort before overwriting Claude's
// settings — otherwise an unwritable AIExpedite config dir would leave the user
// with our hook installed but no stash to chain to or restore from on opt-out.
func savePrevStatusLine(raw json.RawMessage) error {
	b, err := json.MarshalIndent(prevStatusLine{StatusLine: raw}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(prevStatusLinePath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(prevStatusLinePath(), b, 0o600)
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
	b, err := os.ReadFile(settingsPath)
	if err != nil && !os.IsNotExist(err) {
		// A non-`IsNotExist` read error (permission, transient share/lock, …)
		// is NOT the same as "file is absent": treating it as empty would let
		// the later atomic write replace settings.json with only our statusLine
		// key, silently dropping every other Claude setting the user has.
		return false, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &settings); err != nil {
			// Refuse to clobber an unparseable settings.json — the user may have
			// a comment-laden or hand-edited file; surface the error instead.
			return false, err
		}
	}

	var existing claudeStatusLine
	var existingRaw json.RawMessage
	if rawSL, present := settings["statusLine"]; present {
		existingRaw = rawSL
		_ = json.Unmarshal(rawSL, &existing)
	}

	if existing.Command == ours {
		return false, nil // already installed, exact match
	}

	// A different command that isn't one of ours is a real user/third-party
	// status line — stash the FULL object so the hook can chain to its command
	// AND opt-out can restore its other options. (A command that IS ours but
	// with a stale binary path just gets its path refreshed; don't re-stash.)
	if existing.Command != "" && !isOurStatusLineCommand(existing.Command) {
		if err := savePrevStatusLine(existingRaw); err != nil {
			// Abort: if we can't persist the original command, installing the
			// hook would silently lose the user's third-party status line and
			// leave opt-out unable to restore it.
			return false, err
		}
	}

	// Preserve the existing statusLine's other documented options (padding,
	// refreshInterval, hideVimModeIndicator, …) by merging into it; override
	// only type + command so the user's spacing / refresh / vim behavior is kept.
	slMap := map[string]json.RawMessage{}
	if len(existingRaw) > 0 {
		_ = json.Unmarshal(existingRaw, &slMap)
	}
	cmdJSON, err := json.Marshal(ours)
	if err != nil {
		return false, err
	}
	slMap["type"] = json.RawMessage(`"command"`)
	slMap["command"] = cmdJSON
	merged, err := json.Marshal(slMap)
	if err != nil {
		return false, err
	}
	settings["statusLine"] = merged

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

	if prev, ok := loadPrevStatusLine(); ok {
		// Restore the user's full original statusLine object verbatim —
		// command and every preserved option (padding / refreshInterval / …).
		settings["statusLine"] = prev
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
