// claude_run_end_hook.go — the `<binary> claude-run-end-hook` entrypoint.
//
// Why this exists:
//
//	The agent books a post-run utilization refresh only for the Claude runs it
//	spawns itself: direct chat (claude_native.go), a terminal-managed session
//	whose command is `claude` (session.go) and `__cli_smoke__`
//	(cliagent_smoke_claudecode.go). A `claude -p` the maintenance controller
//	launches from its own shell, one a user types, or one started inside an
//	agent-managed PowerShell / bash session books nothing, and `--print` mode
//	never renders the status line, so the status-line hook cannot help either.
//	The run passes and the CLI Agents card keeps its last reading.
//
//	statusline_install.go therefore also registers this binary as a Claude Code
//	`SessionEnd` hook (claude_run_end_hook_install.go). When ANY Claude session
//	on the machine ends, this short-lived process records a refresh debt in the
//	shared cache — integers only, no network — and the resident agent's usage
//	tick adopts it (adoptObservedClaudeRunDebt) and pays it through the same
//	bounded ladder as every other run.
//
// Skip rules (evaluated here, in the ending session's own environment):
//   - an env credential is active: the run billed an account this card is not;
//   - AIEXPEDITE_CLAUDE_RUN_OWNER=agent: the agent spawned this run and books
//     its debt itself — owing again at a later instant would reset the debt's
//     request budget on every agent-run turn.
//
// A recent status-line reading is deliberately NOT a skip rule: the cache is
// shared by every Claude process on the machine, so the reading may come from
// another interactive session and predate this run's own usage (a controller's
// `claude -p` ending seconds after an open session re-rendered). The debt is
// owed regardless; the attempt's coverage pre-check still settles it without a
// request once a reading observed after the run's end lands.
//
// Redaction: the hook payload on stdin (session id, transcript path, cwd,
// reason) is drained and discarded unparsed; nothing is printed or logged, and
// the only values written are integers the snapshot schema already holds. The
// exit code is always 0 so Claude never reports the hook as failed.
package main

import (
	"io"
	"os"
	"time"
)

const (
	// claudeRunEndHookArg is the os.Args[1] subcommand that routes into the hook.
	claudeRunEndHookArg = "claude-run-end-hook"
	// claudeRunOwnerEnv marks a claude child the agent spawned
	// (prepareClaudeChildEnv); claudeRunOwnerAgent is its one value.
	claudeRunOwnerEnv   = "AIEXPEDITE_CLAUDE_RUN_OWNER"
	claudeRunOwnerAgent = "agent"
	// maxRunEndHookStdin caps the drain: the SessionEnd payload is a few hundred
	// bytes, and reading it only keeps Claude's write from hitting a closed pipe.
	maxRunEndHookStdin = 64 << 10
)

// runClaudeRunEndHook is the subcommand body. Best-effort and silent.
func runClaudeRunEndHook() {
	_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, maxRunEndHookStdin))
	claudeRunEndHookAt(time.Now())
}

// claudeRunEndHookAt applies the skip rules and owes the refresh, with the
// clock injected for tests. Reports whether a debt was written.
func claudeRunEndHookAt(now time.Time) bool {
	if os.Getenv(claudeRunOwnerEnv) == claudeRunOwnerAgent || claudeEnvAuthActive() {
		return false
	}
	return claudeOweObservedRunRefresh(now)
}
