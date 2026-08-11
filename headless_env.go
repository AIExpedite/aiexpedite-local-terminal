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

// isPTYEligibleCommand reports whether a tty=true command may run under a PTY.
// The command must resolve to a SINGLE invocation of a recognized agent — robust
// to an explicit path (`/usr/local/bin/agy`) and a Windows suffix (`agy.exe`).
//
// Two command shapes are handled distinctly so that literal argv punctuation is
// never mistaken for shell chaining:
//   - Direct argv (`agy --print "fix A; then B"`): the executable is the first
//     token and the arguments are literal data. Punctuation such as `;`/`&`/`|`
//     inside a prompt argument is NOT shell chaining, so eligibility is decided by
//     the executable's base name alone.
//   - Shell-wrapped payload (`bash -c "agy && git push"`, how terminal-service
//     ships operator-joined commands): the payload IS a real shell string, so a
//     chaining operator means a second program (git/test-runner) would inherit the
//     PTY's controlling terminal and bypass headless hardening. Such payloads are
//     rejected and fall back to the hardened pipe path.
func isPTYEligibleCommand(cmd string, args []string) bool {
	if payload, ok := shellDashCPayload(cmd, args); ok {
		if hasShellCommandChaining(payload) {
			return false
		}
		return firstTokenIsPTYAgent(payload)
	}
	return firstTokenIsPTYAgent(cmd)
}

// firstTokenIsPTYAgent reports whether the leading token of command names a
// PTY-eligible agent. Normalization is single-sourced through commandBaseName
// so it matches the rest of the command routing: an explicit path
// (`/usr/local/bin/agy`) and every recognized shim suffix (`.exe`, and the
// Windows launcher shims `.cmd`/`.bat`/`.ps1`) resolve to the same base name.
// Stripping only `.exe` here would leave `agy.cmd`/`antigravity.cmd` classified
// ineligible, so a tty=true Antigravity session would skip the Windows PTY
// rejection (or the Unix PTY path) and silently fall back to the pipe path where
// Antigravity has no terminal and hangs.
func firstTokenIsPTYAgent(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	return ptyEligibleAgents[commandBaseName(fields[0])]
}

// hasShellCommandChaining reports whether a command line contains shell
// operators that would launch a second program or reach another process/file:
// chaining (`&&`, `||`, `;`), piping (`|`), backgrounding (`&`), command
// substitution (`$(...)` or backticks), process substitution (`<(...)` /
// `>(...)`) and any redirection or here-string/here-doc (`<`, `>`), or a
// newline. Used to keep multi-command payloads off the single-agent PTY path so
// a trailing git/test-runner command (whether joined by an operator or launched
// via process substitution such as `agy <(git credential fill)`) cannot inherit
// a controlling terminal and bypass headless hardening.
//
// The scan is quote-aware so operators that are literal shell DATA — e.g. an
// `agy 'fix A & B'` prompt — are not mistaken for chaining and forced off the
// PTY. It follows bash quoting: single quotes make every character literal;
// double quotes neutralize `|&;<>` and newline but keep command substitution
// (`$(...)` / backticks) active; a backslash escapes the next character outside
// single quotes. Only genuinely unquoted operators (plus substitution active in
// an unquoted or double-quoted context) trigger the pipe fallback. An
// unterminated quote leaves any trailing operator treated as literal, which is
// safe: bash itself rejects such a payload before launching anything.
func hasShellCommandChaining(command string) bool {
	const (
		stateNone = iota
		stateSingle
		stateDouble
	)
	state := stateNone
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch state {
		case stateSingle:
			// Single quotes preserve the literal value of every character.
			if c == '\'' {
				state = stateNone
			}
		case stateDouble:
			switch c {
			case '\\':
				i++ // backslash escapes the next character inside double quotes
			case '"':
				state = stateNone
			case '`':
				return true // command substitution stays active inside double quotes
			case '$':
				if i+1 < len(runes) && runes[i+1] == '(' {
					return true
				}
			}
		default: // stateNone
			switch c {
			case '\\':
				i++ // backslash escapes the next character
			case '\'':
				state = stateSingle
			case '"':
				state = stateDouble
			case '|', '&', ';', '<', '>', '\n', '`':
				return true
			case '$':
				if i+1 < len(runes) && runes[i+1] == '(' {
					return true
				}
			}
		}
	}
	return false
}

// shellDashCPayload returns the payload of a `bash -c "<payload>"` invocation
// (any of bash/sh/zsh/dash) and true, or ("", false) when the command is not a
// shell running a `-c` string. This is how terminal-service ships shell built-ins
// and operator-joined commands, so the payload is a genuine shell string whose
// operators (unlike literal argv) really do launch further programs.
func shellDashCPayload(cmd string, args []string) (string, bool) {
	raw, ok := shellDashCPayloadRaw(cmd, args)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

// shellDashCPayloadRaw is shellDashCPayload without the whitespace trim.
// Rewriters must use this: trimming a trailing backslash-newline leaves a
// dangling `\`, which bash then passes as an extra argument — verified on
// 5.2.21, where a payload ending in backslash-newline supplies just `review`,
// while the same text trimmed supplies `review` and a literal backslash.
func shellDashCPayloadRaw(cmd string, args []string) (string, bool) {
	base := commandBaseName(cmd)
	if (base == "bash" || base == "sh" || base == "zsh" || base == "dash") && len(args) >= 2 {
		for i, a := range args {
			if a == "-c" && i+1 < len(args) {
				return args[i+1], true
			}
		}
	}
	return "", false
}

// effectiveCommandLine returns the command string to classify for profiling. A
// command wrapped as `bash -c "<payload>"` (how terminal-service ships shell
// built-ins / operator-joined commands) is unwrapped to its payload so
// test-runner and git detection see the real leading program rather than the
// shell.
func effectiveCommandLine(cmd string, args []string) string {
	if payload, ok := shellDashCPayload(cmd, args); ok {
		return payload
	}
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + strings.Join(args, " ")
}
