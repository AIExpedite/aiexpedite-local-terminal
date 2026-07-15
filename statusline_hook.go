// statusline_hook.go — the `<binary> statusline-hook` entrypoint.
//
// Why this exists:
//
//	The stream-json `rate_limit_event` capture (cliagent_ratelimit.go) only sees
//	the sessions the AGENT runs, and only carries `utilization` on warning /
//	rejected events — so the 5-hour window often stays "unobservable" and the
//	user's own INTERACTIVE Claude usage between automation runs is invisible.
//
//	Claude Code's status line is the fix: it is invoked on every render with the
//	full session JSON on stdin, which — for Pro/Max after the first API response
//	— carries `rate_limits.{five_hour,seven_day}.{used_percentage,resets_at}` for
//	BOTH windows, fresh, regardless of usage level. statusline_install.go wires
//	Claude's `statusLine` setting to call this hook; here we capture that map
//	into the same on-disk cache the CLI Agents tab reads, then render a status
//	line so the user's terminal still shows something useful (chaining to their
//	previous status-line command when we replaced one).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// statusLineHookArg is the os.Args[1] subcommand that routes into the hook.
const statusLineHookArg = "statusline-hook"

// maxStatusLineStdin caps how much of Claude's stdin JSON we read — the payload
// is a few KB; the limit is a guard against a pathological producer.
const maxStatusLineStdin = 1 << 20 // 1 MiB

// runStatusLineHook reads Claude's status-line JSON from stdin, captures the
// rate-limit snapshot, and renders a status line on stdout. Best-effort: any
// failure still prints a line so the user's status bar is never left blank.
func runStatusLineHook() {
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, maxStatusLineStdin))
	captureClaudeRateLimitsFromStatusline(raw, time.Now())
	renderStatusLine(raw)
}

// captureClaudeRateLimitsFromStatusline parses the status-line payload's
// `rate_limits` map (the shape-3 form extractClaudeRateLimitBuckets already
// understands) and merges it into the cache, scoped to the current account.
func captureClaudeRateLimitsFromStatusline(raw []byte, now time.Time) {
	if len(raw) == 0 {
		return
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	updates := extractClaudeRateLimitBuckets(m, now.UnixMilli())
	if len(updates) == 0 {
		return
	}
	// This hook runs as a child of the Claude session that rendered the status
	// line, so our environment IS that session's. When it is authenticated with
	// an env credential that outranks the stored /login (ANTHROPIC_AUTH_TOKEN, an
	// approved ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, or a cloud provider —
	// see claudeEnvAuthActive), these rate_limits belong to that env account, not
	// the stored login. DON'T record them at all: the cache is scoped by the
	// stored credential — which is "" for the common claude.ai login, the very
	// same scope a subscription capture uses — so writing env usage here (even
	// "unscoped") would let the stored-login card read it back as its own. There
	// is no env-account card to show it on, so simply skip the capture, which also
	// leaves the stored login's existing buckets untouched.
	if claudeEnvAuthActive() {
		return
	}
	// Stamp the capturing dotfile account so a later render under a different
	// /login (which keeps the same "" fingerprint for a claude.ai session) can't
	// have this account's windows shown on the new email's card.
	mergeClaudeRateLimitCache(
		claudeRateLimitCachePath(), updates, now,
		currentClaudeAccountFingerprint(), currentClaudeDotfileAccount())
}

// renderStatusLine forwards to the user's previous status-line command when we
// chained one at install time (feeding it the same stdin), otherwise prints our
// own compact line built from the payload.
//
// Cancellation: Claude's status-line docs state that a new render cancels the
// in-flight one. Without help the shell child we spawn here would survive that
// kill — Claude only terminates the wrapper it invoked — leaking expensive or
// stuck third-party status-line processes across renders. We put the child in
// its own process group / job and forward catchable termination signals to it
// so a hanging chained command is torn down with us, restoring the cancellation
// guarantee users had before we wrapped their command.
func renderStatusLine(raw []byte) {
	if prev := loadPrevStatusLineCommand(); prev != "" {
		cmd := shellCommand(prev)
		cmd.Stdin = bytes.NewReader(raw)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		setStatusLineChildProcessGroup(cmd)
		if err := cmd.Start(); err == nil {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			select {
			case err := <-done:
				if err == nil {
					return
				}
			case sig := <-sigCh:
				killStatusLineChild(cmd.Process, sig)
				<-done
				// We're being torn down by Claude; do not fall back to our
				// own line — the next render will print one.
				return
			}
			// Fall through to our own line if the chained command failed.
		}
	}
	fmt.Print(defaultStatusLine(raw))
}

// shellCommand wraps a status-line command string the way Claude Code itself
// runs it, so a chained command behaves identically to when Claude invoked it
// directly. On Windows that means Git Bash when installed (so `~` expands and
// `.sh` scripts run) and PowerShell otherwise — NOT cmd, which would silently
// break a Bash-style stashed command like `~/.claude/statusline.sh`. The
// PowerShell branch mirrors Claude's own `-ExecutionPolicy Bypass` so a stashed
// `.ps1` command keeps working under the default Restricted policy.
func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		if bash := findGitBash(); bash != "" {
			return exec.Command(bash, "-c", command)
		}
		return exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", command)
	}
	return exec.Command("sh", "-c", command)
}

// defaultStatusLine builds a minimal, useful one-liner from the status-line
// payload: model, current dir, and the rate-limit summary we just captured.
// Every field is optional — a missing one is simply omitted.
func defaultStatusLine(raw []byte) string {
	var p struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
		Workspace struct {
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
		Cwd        string `json:"cwd"`
		RateLimits map[string]struct {
			UsedPercentage float64 `json:"used_percentage"`
		} `json:"rate_limits"`
	}
	_ = json.Unmarshal(raw, &p)

	var parts []string
	if p.Model.DisplayName != "" {
		parts = append(parts, p.Model.DisplayName)
	}
	if dir := firstNonEmpty(p.Workspace.CurrentDir, p.Cwd); dir != "" {
		parts = append(parts, baseName(dir))
	}
	if rl := p.RateLimits; rl != nil {
		if b, ok := rl["five_hour"]; ok {
			parts = append(parts, fmt.Sprintf("5h %d%%", int(b.UsedPercentage+0.5)))
		}
		if b, ok := rl["seven_day"]; ok {
			parts = append(parts, fmt.Sprintf("7d %d%%", int(b.UsedPercentage+0.5)))
		}
	}
	return strings.Join(parts, " · ")
}

// baseName returns the last path segment, handling both separators so the
// status line shows "my-repo" rather than the full path on either OS.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
