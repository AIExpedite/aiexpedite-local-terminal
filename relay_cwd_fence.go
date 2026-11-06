// File: relay_cwd_fence.go
// -----------------------------------------------------------------------------
// Enforces the signed folder boundary (cwdFence) on session starts admitted
// through ancestor grants.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"fmt"
	"path/filepath"

	"cloud.google.com/go/pubsub/v2"
)

// cwdOutsideGrantFolderRejectionCode is the rejection reason when a session's
// working directory escapes the granted folder boundary (shared-constants
// REJECTION_REASONS.CWD_OUTSIDE_GRANT_FOLDER).
const cwdOutsideGrantFolderRejectionCode = "CWD_OUTSIDE_GRANT_FOLDER"

// refuseCwdOutsideFence checks whether a session start admitted through an
// ancestor grant stays inside that granted boundary on disk.
// If cmd carries CwdFence, both the fence and the working directory are
// evaluated through symlinks. If either cannot be resolved or the real working
// directory leaves the real fence, the command is refused with
// CWD_OUTSIDE_GRANT_FOLDER.
// Returns true when the command was handled (refused) and settled.
func refuseCwdOutsideFence(ctx context.Context, topic *pubsub.Publisher, m *pubsub.Message, cmd commandMsg, cfg *Config) bool {
	if cmd.CwdFence == "" {
		return false
	}

	agentID := ""
	if cfg != nil {
		agentID = cfg.AgentID
	}

	if cmd.Cwd == "" {
		msg := fmt.Sprintf("working directory is empty; must be inside granted folder %q", cmd.CwdFence)
		fmt.Printf("%s[fence] Refused %s: %s%s\n", colorYellow, cmd.Type, msg, colorReset)
		res := makeRejectionResult(cmd, agentID, "denied", cwdOutsideGrantFolderRejectionCode, msg)
		err := publishMsg(ctx, topic, res)
		if m != nil {
			if err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
		}
		return true
	}

	realFence, errFence := filepath.EvalSymlinks(cmd.CwdFence)
	realCwd, errCwd := filepath.EvalSymlinks(cmd.Cwd)

	if errFence != nil || errCwd != nil || !isPathWithin(realFence, realCwd) {
		msg := fmt.Sprintf("working directory %q escapes granted folder %q", cmd.Cwd, cmd.CwdFence)
		if errCwd != nil {
			msg = fmt.Sprintf("working directory %q cannot be resolved: %v", cmd.Cwd, errCwd)
		} else if errFence != nil {
			msg = fmt.Sprintf("granted folder %q cannot be resolved: %v", cmd.CwdFence, errFence)
		}
		fmt.Printf("%s[fence] Refused %s: %s%s\n", colorYellow, cmd.Type, msg, colorReset)
		res := makeRejectionResult(cmd, agentID, "denied", cwdOutsideGrantFolderRejectionCode, msg)
		err := publishMsg(ctx, topic, res)
		if m != nil {
			if err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
		}
		return true
	}

	return false
}
