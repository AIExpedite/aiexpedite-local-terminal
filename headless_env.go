// File: headless_env.go
// Non-interactive ("headless") execution hardening for non-agent commands.
//
// Non-agent commands (bash, sh, git, PowerShell, test runners) run with piped
// stdio, but git, credential helpers, ssh, and editors can still open the
// controlling terminal (/dev/tty) and prompt — bypassing the captured pipes and
// blocking indefinitely. This module centralises the environment overlay and
// spawn hardening that force those tools into a non-interactive, fail-fast mode.
//
// Two concerns live here:
//  1. headlessEnvOverlay()  — authoritative git/editor/credential env keys that
//     always win over the inherited environment (appended last).
//  2. hardenNonAgentCommand() — applies the overlay, closes stdin to EOF, and
//     detaches the child from any controlling terminal (platform-specific).
//
// Resident CLI-agent sessions (claude/codex/gemini/grok/agy) do NOT go through
// here — they keep their interactive-capable environment (see session.go).
package main

import (
	"os"
	"os/exec"
	"strings"
)

// headlessEnvOverlay returns the authoritative non-interactive environment keys
// for non-agent / git / test-runner commands. These are appended AFTER the
// inherited environment so they always win: a caller cannot re-enable an
// interactive prompt on the headless path. The only interactive path is the
// resident CLI-agent session, which never routes through here.
func headlessEnvOverlay() []string {
	askpass := nonInteractiveAskpassPath()
	overlay := []string{
		// Git: never prompt on the terminal; fail fast with a captured
		// "could not read Username ... terminal prompts disabled" error.
		"GIT_TERMINAL_PROMPT=0",
		// Editors: never open an interactive editor for merges, commits, or
		// interactive rebases — treat them as accepted / no-op.
		"GIT_MERGE_AUTOEDIT=no",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		// Pagers hang waiting for a key on a tty; force straight-through output.
		"GIT_PAGER=cat",
		"PAGER=cat",
		// Git Credential Manager: never pop an interactive prompt, and bound its
		// provider autodetect so it can't stall probing for a helper.
		"GCM_INTERACTIVE=never",
		"GCM_PROVIDER_AUTODETECT_TIMEOUT=2000",
	}
	if askpass != "" {
		// Point credential/passphrase prompts at a helper that exits non-zero.
		// SSH_ASKPASS_REQUIRE=force makes OpenSSH consult it even with no DISPLAY
		// (OpenSSH >= 8.4), so ssh fails fast instead of falling back to /dev/tty.
		overlay = append(overlay,
			"GIT_ASKPASS="+askpass,
			"SSH_ASKPASS="+askpass,
			"SSH_ASKPASS_REQUIRE=force",
		)
	}
	return overlay
}

// testRunnerEnvDefaults returns the non-safety "nicety" defaults for the
// test-runner profile: quiet, non-interactive, unbuffered, ANSI-free output.
// Unlike headlessEnvOverlay these do NOT override an explicit caller value —
// they are only applied when the key is absent from base, preserving caller
// intent (see EXECUTION_LIVENESS_REDESIGN.md → test-runner precedence).
func testRunnerEnvDefaults(base []string) []string {
	defaults := map[string]string{
		"CI":               "1",
		"FORCE_COLOR":      "0",
		"NO_COLOR":         "1",
		"PYTHONUNBUFFERED": "1",
	}
	present := map[string]bool{}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			present[kv[:i]] = true
		}
	}
	var out []string
	for k, v := range defaults {
		if !present[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// hardenNonAgentCommand applies the full headless profile to a non-agent
// command before it is started: authoritative git/editor/credential env, an
// EOF stdin so a blocking read returns immediately instead of waiting for
// input, and detachment from any controlling terminal (Setsid on unix). When
// the command is a recognised test runner, the quiet/unbuffered test-runner
// defaults are layered in underneath the safety overlay.
//
// It must be called before c.Start(). It is safe to call on a command that
// already has SysProcAttr / Env set — it merges rather than clobbers. When the
// caller has pre-sanitised c.Env (e.g. prepareClaudeChildEnv strips CLAUDE_*
// from a utility session's environment), that filtered env is used as the base
// so the overlay layers on top of it instead of reintroducing the stripped
// variables from os.Environ().
func hardenNonAgentCommand(c *exec.Cmd, effectiveCommand string) {
	base := c.Env
	if base == nil {
		base = os.Environ()
	}
	if isTestRunnerCommand(effectiveCommand) {
		base = append(base, testRunnerEnvDefaults(base)...)
	}
	// Overlay appended last so its keys are authoritative.
	c.Env = append(base, headlessEnvOverlay()...)

	// A nil Stdin already maps to /dev/null in os/exec, so a blocking read
	// returns EOF immediately instead of waiting for interactive input — the
	// non-agent paths never set Stdin, so no closed-pipe wiring is needed here.
	// The remaining prompt vector is /dev/tty, closed by detachControllingTTY.
	detachControllingTTY(c)
}

// isTestRunnerCommand reports whether the effective command line is a
// recognised test runner (Node / Python / shell). Detection is an explicit
// allowlist matched against the leading token(s); anything not on the list runs
// as a plain headless command with no test-specific env. Callers that wrap a
// command in `bash -c "..."` are unwrapped by effectiveCommandLine first.
func isTestRunnerCommand(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if c == "" {
		return false
	}
	// Prefix matches (leading token / phrase).
	prefixes := []string{
		"jest", "vitest", "mocha", "node --test", "node --test=",
		"pytest", "python -m pytest", "python3 -m pytest",
		"python -m unittest", "python3 -m unittest", "tox", "bats",
		"npm test", "npm run test", "pnpm test", "pnpm run test",
		"yarn test", "npx jest", "npx vitest", "npx mocha",
	}
	for _, p := range prefixes {
		if c == p || strings.HasPrefix(c, p+" ") {
			return true
		}
	}
	return false
}

// ptyEligibleAgents is the allowlist of recognized resident interactive/TUI
// agents that genuinely require a real terminal (Antigravity's `agy`). PTY is an
// ALLOWLIST, not a blocklist: only these commands may run under a PTY when
// tty=true. Everything else — git, test runners, bash/sh/PowerShell, ssh,
// credential-helper probes, and the pipe-protocol agents (claude/codex/gemini/
// grok, which use structured stdio) — stays on the hardened pipe path. This is
// deliberate: `tty` is unsigned metadata, so a blocklist would let an attacker
// flip a signed `bash`/`ssh` command from the hardened pipe path onto a PTY
// (no controlling-terminal hardening, free to prompt). An allowlist makes tty a
// no-op on anything but a recognized TUI agent.
var ptyEligibleAgents = map[string]bool{
	"agy":         true,
	"antigravity": true,
}

// isPTYEligibleCommand reports whether a command may run under a PTY when
// tty=true. The whole effective command line must be a SINGLE invocation of a
// recognized agent — robust to an explicit path (`/usr/local/bin/agy`) and a
// Windows suffix (`agy.exe`). A compound payload (e.g. terminal-service's
// `bash -c "agy && git push"`, which effectiveCommandLine unwraps to
// `agy && git push`) is rejected: only the first token would be the agent while
// the trailing git/test-runner command would inherit the PTY's controlling
// terminal and bypass headless hardening. Such payloads fall back to the
// hardened pipe path instead of riding the PTY on their first token alone.
func isPTYEligibleCommand(command string) bool {
	if hasShellCommandChaining(command) {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := strings.ToLower(fields[0])
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	return ptyEligibleAgents[base]
}

// hasShellCommandChaining reports whether a command line contains shell
// operators that would launch a second program: chaining (`&&`, `||`, `;`),
// piping (`|`), backgrounding (`&`), command substitution (`$(...)` or
// backticks), or a newline. Used to keep multi-command payloads off the
// single-agent PTY path so a trailing git/test-runner command cannot inherit a
// controlling terminal.
func hasShellCommandChaining(command string) bool {
	return strings.ContainsAny(command, "|&;`\n") || strings.Contains(command, "$(")
}

// effectiveCommandLine returns the command string to classify for profiling. A
// command wrapped as `bash -c "<payload>"` (how terminal-service ships shell
// built-ins / operator-joined commands) is unwrapped to its payload so
// test-runner and git detection see the real leading program rather than the
// shell.
func effectiveCommandLine(cmd string, args []string) string {
	base := commandBaseName(cmd)
	if (base == "bash" || base == "sh" || base == "zsh" || base == "dash") && len(args) >= 2 {
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				return strings.TrimSpace(args[i+1])
			}
		}
	}
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + strings.Join(args, " ")
}
