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
	"regexp"
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
		// ...\Git\cmd\git.exe -> ...\Git\bin\bash.exe. An MSYS2/Cygwin `git` on
		// PATH would derive `<msys>/bin/bash.exe` — not Git Bash, and Claude's
		// actual fallback shell here (PowerShell) would mis-execute the POSIX
		// form we emit. Filter the derived candidate the same way as PATH bash.
		gitDir := filepath.Dir(filepath.Dir(gitPath))
		derived := filepath.Join(gitDir, "bin", "bash.exe")
		if isGitForWindowsBashPath(derived) {
			candidates = append(candidates, derived)
		}
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
// We PIN the cache + stash paths via `AIEXPEDITE_CLAUDE_RL_CACHE` /
// `AIEXPEDITE_CLAUDE_STATUSLINE_PREV` in the command itself so the hook resolves
// them from the same `GetConfigDir()` the installer used — otherwise a Linux
// box where the agent starts at login without the user's shell
// `XDG_CONFIG_HOME` but Claude is launched from a shell that sets it would
// write `claude_rate_limits.json` and look for the stashed previous status line
// under a different config dir than the Agents tab and installer, so
// interactive usage would stay stale and a chained custom status line would
// silently disappear.
//
// On Windows, Claude runs the command through Git Bash when installed, else
// PowerShell — and no single literal works in both for a quoted/spaced path
// (Git Bash chokes on a leading `&`; PowerShell treats a quoted path as a mere
// string unless invoked with the `&` call operator). So we mirror Claude's own
// Git-Bash detection and emit the matching form:
//   - Git Bash:   KEY='val' KEY2='val2' '/c/path/exe' statusline-hook
//   - PowerShell: $env:KEY="val"; $env:KEY2="val2"; & "C:/path/exe" statusline-hook
//
// Re-evaluated on every startup, so adding/removing Git Bash — or relocating
// the data dir — self-corrects on the next agent boot. Both the exe path and
// the env values go through shell-safe quoting (`posixSingleQuote` /
// `powerShellDoubleQuote`) so install paths with `$`, backticks, or `$(...)`
// in any segment don't get expanded into the wrong path or a subshell.
func ourStatusLineCommand() (string, bool) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", false
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	cachePath := claudeRateLimitCachePath()
	prevPath := prevStatusLinePath()
	if runtime.GOOS == "windows" {
		// Forward-slash everything we embed: the exe path lives inside double
		// quotes where Git Bash treats `\` as an escape, and the env values —
		// even though we single-quote them — are simpler to reason about when
		// the whole command is slash-uniform.
		exe = strings.ReplaceAll(exe, `\`, "/")
		cachePath = strings.ReplaceAll(cachePath, `\`, "/")
		prevPath = strings.ReplaceAll(prevPath, `\`, "/")
		if findGitBash() != "" {
			return statusLinePosixCommand(exe, cachePath, prevPath), true
		}
		return statusLinePowerShellCommand(exe, cachePath, prevPath), true
	}
	return statusLinePosixCommand(exe, cachePath, prevPath), true
}

// statusLinePosixCommand emits the sh/bash form Claude uses on POSIX and on
// Windows when Git Bash is installed: per-invocation env assignments followed
// by the quoted exe + hook arg.
//
// The exe path is wrapped with the same posixSingleQuote helper as the env
// values — a raw `"..."` form is expanded by sh/Git Bash, so a binary under a
// directory containing `$`, backticks, or `"` (all valid in macOS/Linux/Windows
// directory names) would resolve to the wrong path or execute a command
// substitution and the hook would silently never capture metrics.
func statusLinePosixCommand(exe, cachePath, prevPath string) string {
	return "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(cachePath) +
		" AIEXPEDITE_CLAUDE_STATUSLINE_PREV=" + posixSingleQuote(prevPath) +
		" " + posixSingleQuote(exe) + " " + statusLineHookArg
}

// statusLinePowerShellCommand emits the PowerShell form Claude uses on Windows
// when Git Bash is absent: `$env:` assignments separated by `;`, then the `&`
// call operator on the quoted exe + hook arg.
//
// The exe path also goes through powerShellDoubleQuote so an install directory
// containing `$` or `$(...)` (legal Windows path characters) doesn't get
// expanded by PowerShell as a variable / subexpression before invocation.
func statusLinePowerShellCommand(exe, cachePath, prevPath string) string {
	return "$env:AIEXPEDITE_CLAUDE_RL_CACHE=" + powerShellDoubleQuote(cachePath) +
		"; $env:AIEXPEDITE_CLAUDE_STATUSLINE_PREV=" + powerShellDoubleQuote(prevPath) +
		"; & " + powerShellDoubleQuote(exe) + " " + statusLineHookArg
}

// posixSingleQuote wraps s in single quotes for sh/bash. A single quote inside
// the value is escaped via the standard close/escape/reopen trick:
//
//	'\''
//
// so paths under e.g. `/Users/dan's mac/...` survive.
func posixSingleQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// powerShellDoubleQuote wraps s in double quotes for PowerShell. The two
// expansion-time metachars are `"` (escape by doubling) and `$` (escape with a
// backtick), and the backtick itself is the escape character so it must be
// doubled first.
func powerShellDoubleQuote(s string) string {
	s = strings.ReplaceAll(s, "`", "``")
	s = strings.ReplaceAll(s, `"`, `""`)
	s = strings.ReplaceAll(s, `$`, "`$")
	return `"` + s + `"`
}

// statusLineEnvPreambleRe captures the per-invocation env preamble we install
// before the hook in either the POSIX (`KEY='val' …`) or PowerShell
// (`$env:KEY="val"; …`) form so isOurStatusLineCommand can strip it before
// matching the inner exe+hook shape.
//
// The POSIX body must also accept `posixSingleQuote`'s embedded-apostrophe
// escape — the close/escape/reopen sequence:
//
//	'\''
//
// Without it, a config dir like
// `/Users/bob's/.aiexpedite/...` stops the regex at the first apostrophe and
// our own installed command is no longer recognized — opt-out/uninstall then
// leaves Claude pointing at the binary, and a later refresh stashes our own
// command as the chained third-party one. Windows file paths can't contain
// `"`, so the PowerShell `[^"]*` body needs no escape clause.
var statusLineEnvPreambleRe = regexp.MustCompile(
	`^(?:\s*(?:AIEXPEDITE_CLAUDE_\w+='(?:[^']|'\\'')*'|\$env:AIEXPEDITE_CLAUDE_\w+="[^"]*";))+\s*`,
)

// statusLinePinnedPosixRe / statusLinePinnedPowerShellRe extract a single pinned
// env var's value from the installed command's preamble. The POSIX body mirrors
// posixSingleQuote's close/escape/reopen apostrophe shape; the PowerShell body
// mirrors powerShellDoubleQuote's escape rules.
var (
	statusLinePinnedPosixRe = regexp.MustCompile(
		`(?:^|\s)AIEXPEDITE_CLAUDE_(\w+)='((?:[^']|'\\'')*)'`,
	)
	statusLinePinnedPowerShellRe = regexp.MustCompile(
		`\$env:AIEXPEDITE_CLAUDE_(\w+)="((?:[^"` + "`" + `]|""|` + "`" + `.)*)"`,
	)
)

// extractInstalledPinnedPath returns the value of the named pinned env var
// (e.g. `STATUSLINE_PREV`) from an installed command's preamble, with the
// shell-specific escapes unwound. Returns "" when the command doesn't pin it.
//
// Used on a refresh where the existing command is already one of ours but the
// new `ourStatusLineCommand` resolves to a different `prevStatusLinePath()`
// (e.g. `GetConfigDir()` moved because XDG_CONFIG_HOME changed between the
// previous install and this boot). Without it, the stash file would remain at
// the OLD pinned path while both the hook and opt-out look at the NEW one,
// so the user's chained third-party status line would silently disappear and
// opt-out would have nothing to restore.
func extractInstalledPinnedPath(command, suffix string) string {
	for _, m := range statusLinePinnedPosixRe.FindAllStringSubmatch(command, -1) {
		if m[1] == suffix {
			return strings.ReplaceAll(m[2], `'\''`, `'`)
		}
	}
	for _, m := range statusLinePinnedPowerShellRe.FindAllStringSubmatch(command, -1) {
		if m[1] == suffix {
			v := m[2]
			v = strings.ReplaceAll(v, `""`, `"`)
			v = strings.ReplaceAll(v, "`$", "$")
			v = strings.ReplaceAll(v, "``", "`")
			return v
		}
	}
	return ""
}

// isOurStatusLineCommand reports whether a configured command is our hook (any
// binary path, with or without our pinned env preamble) — used to stay
// idempotent across binary-path / config-dir refreshes, and to avoid stashing
// our own command as the "previous" one.
//
// Matches the shape `ourStatusLineCommand` emits — an optional env-var
// preamble, then a quoted executable path followed by ` statusline-hook`, with
// an optional leading `&` call op for the PowerShell form. Both `'...'`
// (POSIX single-quoted, the current safe form) and `"..."` (legacy POSIX +
// PowerShell) are accepted so a previously installed double-quoted entry is
// still recognized as ours after we switched to single-quoting. A loose
// substring match would falsely claim a user's own command (e.g.
// `~/.claude/statusline-hook.sh`) as ours and silently overwrite it without
// stashing, leaving opt-out unable to restore it.
func isOurStatusLineCommand(command string) bool {
	s := strings.TrimSpace(command)
	s = statusLineEnvPreambleRe.ReplaceAllString(s, "")
	suffix := " " + statusLineHookArg
	if !strings.HasSuffix(s, suffix) {
		return false
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(s, suffix))
	// PowerShell form starts with the `&` call operator before the quoted path.
	prefix = strings.TrimSpace(strings.TrimPrefix(prefix, "&"))
	if len(prefix) < 2 {
		return false
	}
	return (strings.HasPrefix(prefix, `"`) && strings.HasSuffix(prefix, `"`)) ||
		(strings.HasPrefix(prefix, `'`) && strings.HasSuffix(prefix, `'`))
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

// copyPrevStatusLine copies the stash file across filesystems with the same
// owner-only mode `savePrevStatusLine` uses. The binary `copyFile` helper is
// intentionally NOT reused here: it chmods to 0o755, which would leave the
// JSON stash world-readable on Unix and expose the previously chained
// status-line command (which can include inline env vars / tokens) to other
// local users after an EXDEV migration.
func copyPrevStatusLine(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
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
	} else if existing.Command != "" {
		// Existing command is one of ours but differs from the freshly generated
		// one — usually just a refreshed binary path. If the pinned stash path
		// also moved (e.g. GetConfigDir() resolved differently on this boot),
		// migrate the stash file to the new path BEFORE we overwrite the
		// command. Otherwise the new installed hook + opt-out look at the new
		// path while the user's chained third-party command remains stashed
		// only at the old one, silently dropping the chain target.
		oldPrev := extractInstalledPinnedPath(existing.Command, "STATUSLINE_PREV")
		newPrev := prevStatusLinePath()
		if oldPrev != "" && oldPrev != newPrev {
			if _, err := os.Stat(newPrev); os.IsNotExist(err) {
				if _, err := os.Stat(oldPrev); err == nil {
					if mkErr := os.MkdirAll(filepath.Dir(newPrev), 0o755); mkErr == nil {
						// os.Rename returns EXDEV when oldPrev and newPrev sit on
						// different filesystems (e.g. XDG_CONFIG_HOME moved from
						// the home disk to a mounted drive). Ignoring it would
						// leave the stash at oldPrev while the hook + opt-out
						// look at newPrev, silently dropping the chained
						// third-party command. Fall back to a private-mode
						// copy-and-remove so the migration still happens
						// across volumes — do NOT use the binary `copyFile`
						// helper here: it chmods to 0o755, which would leave
						// the stash world-readable on Unix and expose the
						// previously chained command (potentially including
						// inline env vars / tokens) to other local users.
						if err := os.Rename(oldPrev, newPrev); err != nil {
							if copyErr := copyPrevStatusLine(oldPrev, newPrev); copyErr == nil {
								_ = os.Remove(oldPrev)
							}
						}
					}
				}
			}
		}
	} else if existing.Command == "" {
		// Installing over an empty/absent statusLine: there's no third-party
		// command to chain to, but a stale `claude_statusline_prev.json` from an
		// earlier install would later be resurrected by removeClaudeStatusLineHook
		// — restoring a command the user has already removed instead of dropping
		// the statusLine entirely. Drop the stash here so opt-out matches reality.
		_ = os.Remove(prevStatusLinePath())
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

	prev, ok := loadPrevStatusLine()
	pinnedPrev := ""
	if !ok {
		// Fall back to the path the installed command itself pinned. The data
		// dir may have moved since install (e.g. XDG_CONFIG_HOME changed), so
		// the current prevStatusLinePath() resolves to a different file than
		// the one we wrote at install time — but the stash is still sitting at
		// the old pinned path. Without this fallback we'd delete statusLine
		// and lose the user's original chained command.
		if pinned := extractInstalledPinnedPath(existing.Command, "STATUSLINE_PREV"); pinned != "" && pinned != prevStatusLinePath() {
			if b, err := os.ReadFile(pinned); err == nil {
				var p prevStatusLine
				if json.Unmarshal(b, &p) == nil && len(p.StatusLine) > 0 {
					prev, ok, pinnedPrev = p.StatusLine, true, pinned
				}
			}
		}
	}
	if ok {
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
	// doesn't resurrect a stale chain target. Also drop the pinned-path stash
	// if we restored from it.
	_ = os.Remove(prevStatusLinePath())
	if pinnedPrev != "" {
		_ = os.Remove(pinnedPrev)
	}
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
