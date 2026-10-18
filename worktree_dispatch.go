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

import (
	"context"
	"fmt"
)

// runInterceptedDeviceCommand runs whichever family owns the command.
//
// There is deliberately no `isInterceptedDeviceCommand` beside it. Whether a
// command is intercepted is decided ONCE, by the approval gate's
// `DeviceCommand` flag (pubsub.go reads it as `setupDeviceCmd`), and a second
// predicate answering the same question is the kind of copy that drifts: the
// two would disagree the first time a family gained a command, and the gate's
// answer is the one that routes.
func runInterceptedDeviceCommand(ctx context.Context, cfg *Config, cmd commandMsg) (string, error) {
	if isWorktreeDeviceCommand(cmd.Command) {
		// SIGNED MODE ONLY. These built-ins are exempt from the approval dialog
		// because the maintenance sweep runs unattended, and the exemption is
		// justified by the dispatch being HMAC-signed. With no command secret
		// nothing verifies a signature (see pubsub.go's `if cfg.CommandSecret
		// != ""`), so that justification evaporates and a publisher with topic
		// access could delete a worktree with neither a signature nor a prompt.
		//
		// Not a hypothetical state: invalidateRegistration zeroes AgentID and
		// CommandSecret, and an agent mid-registration has none yet.
		//
		// Refused rather than dialog-gated: the caller is a tick, so a prompt
		// would hang it forever, and a clear refusal is a better failure than
		// an unattended destructive command nobody authenticated.
		if cfg == nil || cfg.CommandSecret == "" {
			return "", fmt.Errorf("%s refused: worktree commands require a signed dispatch (no command secret on this agent)", cmd.Command)
		}
		return runWorktreeDeviceCommand(ctx, cmd)
	}
	return runEnvSetupDeviceCommand(ctx, cfg, cmd)
}
