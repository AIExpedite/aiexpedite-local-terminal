// File: worktree_dispatch.go
// -----------------------------------------------------------------------------
// The one seam that routes an intercepted built-in to its handler.
//
// Two families share the interception path in pubsub.go: the computer setup
// checklist's device commands (env_setup_commands.go) and the worktree
// built-ins (worktree_commands.go). Both arrive as ordinary `execute`
// dispatches with a signed command name, and both are handled WITHOUT ever
// looking the name up on PATH or handing anything to a shell.
//
// They differ in how they are gated, which is why the split is here and not a
// bigger switch inside one file: a setup sign-in always takes the native
// approval dialog, while a worktree command never does (see
// executeApprovalGate and the reasoning in worktree_commands.go).
// -----------------------------------------------------------------------------

package main

import "context"

// isInterceptedDeviceCommand reports whether a command is handled in-process
// rather than executed.
func isInterceptedDeviceCommand(command string) bool {
	return isEnvSetupDeviceCommand(command) || isWorktreeDeviceCommand(command)
}

// runInterceptedDeviceCommand runs whichever family owns the command.
func runInterceptedDeviceCommand(ctx context.Context, cfg *Config, cmd commandMsg) (string, error) {
	if isWorktreeDeviceCommand(cmd.Command) {
		return runWorktreeDeviceCommand(ctx, cmd)
	}
	return runEnvSetupDeviceCommand(ctx, cfg, cmd)
}
