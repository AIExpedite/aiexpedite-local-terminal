//go:build windows
// +build windows

// File: pty_session_manager_windows.go
// Windows PTY-session stub. ConPTY-backed tty is deferred, so a tty=true
// session_start for an eligible agent is rejected immediately with a clear
// error rather than silently downgraded to pipes. terminal-service also rejects
// tty on Windows devices at the route; this is device-side defense-in-depth.
package main

// startPTYSession always fails on Windows: PTY sessions are unsupported in this
// build. StartSession surfaces the error and handleSessionCommand publishes it
// as a captured session error.
func (sm *SessionManager) startPTYSession(id, command string, cliArgs []string,
	cwd, workspaceID, uid string, timeoutMs int64, publishFn PublishFunc) error {
	return errPTYUnsupportedWindows
}
