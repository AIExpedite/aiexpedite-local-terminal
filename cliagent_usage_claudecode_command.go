// cliagent_usage_claudecode_command.go — "will spawning this start `claude`?"
//
// isClaudeCommand (session.go) answers whether the PROGRAM is the Claude CLI,
// and drives argv shaping, stream-json parsing, routing and the billing-var
// strip. It checks the base command only, so a terminal-managed run whose base
// is a shell wrapper — `cmd /c claude …`, `bash -c "claude …"`, and on Windows
// the `powershell -EncodedCommand <base64>` (or file-mode launcher) shape
// terminal-service ships every command in — is not recognised. Widening it would
// change those paths, so it is left as it is.
//
// commandRunsClaude answers the narrower question the post-run utilization
// refresh needs: will SPAWNING this start a `claude` process, and therefore
// spend a turn the card must catch up with? It drives only the session-exit
// owe in session.go, and sees through the same wrappers commandRunsAntigravity
// does, through the same scan (scriptSpawnsProgram).
//
// REDACTION: the decoded script is a classification input and nothing else. It
// is never logged, persisted or returned — the only thing that leaves this file
// is a bool.
package main

// commandRunsClaude reports whether a shell-WRAPPED command+args starts the
// Claude CLI. A direct launch is isClaudeCommand's case, which the caller ORs
// in; this returns false for anything wrapperScriptPayload does not unwrap.
func commandRunsClaude(command string, args []string) bool {
	if script, ok := wrapperScriptPayload(command, args); ok {
		return scriptSpawnsProgram(script, isClaudeCommand, true)
	}
	return false
}
