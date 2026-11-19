// File: relay_cwd_fence.go
// -----------------------------------------------------------------------------
// Enforces the signed folder boundary (cwdFence) on session starts admitted
// through ancestor grants.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/pubsub/v2"
)

// cwdOutsideGrantFolderRejectionCode is the rejection reason when a session's
// working directory escapes the granted folder boundary (shared-constants
// REJECTION_REASONS.CWD_OUTSIDE_GRANT_FOLDER).
const cwdOutsideGrantFolderRejectionCode = "CWD_OUTSIDE_GRANT_FOLDER"

// maxFenceLinkHops bounds how many links fenceRealPath follows, so a link
// cycle refuses instead of looping.
const maxFenceLinkHops = 64

// fenceRealPath is the real folder a path names, following symlinks AND
// Windows junctions in every component. filepath.EvalSymlinks alone is not
// enough: since Go 1.23 it does not traverse junctions, so a junction inside
// the granted folder (C:\AIExpedite\link -> D:\other) would read as inside it.
// Each pass resolves what EvalSymlinks can, then looks for a component that is
// still a link (os.Readlink reads junctions) and restarts from its target. An
// unreadable path, a dangling link or a cycle is an error, so the fence fails
// closed.
func fenceRealPath(p string) (string, error) {
	p = filepath.Clean(p)
	for hop := 0; hop <= maxFenceLinkHops; hop++ {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		vol := filepath.VolumeName(resolved)
		segments := strings.FieldsFunc(resolved[len(vol):], func(r rune) bool {
			return os.IsPathSeparator(uint8(r))
		})
		cur := vol + string(filepath.Separator)
		next := ""
		for i, seg := range segments {
			candidate := filepath.Join(cur, seg)
			if target, lerr := os.Readlink(candidate); lerr == nil && target != "" {
				if !filepath.IsAbs(target) {
					target = filepath.Join(cur, target)
				}
				next = filepath.Join(append([]string{target}, segments[i+1:]...)...)
				break
			}
			cur = candidate
		}
		if next == "" {
			return resolved, nil
		}
		p = next
	}
	return "", errors.New("too many links")
}

// refuseCwdOutsideFence checks whether a session start admitted through an
// ancestor grant stays inside that granted boundary on disk.
// If cmd carries CwdFence, both the fence and the working directory are
// resolved through symlinks and junctions (fenceRealPath). If either cannot be
// resolved or the real working directory leaves the real fence, the command is
// refused with CWD_OUTSIDE_GRANT_FOLDER. A grant never widens through a link.
// Returns true when the command was handled (refused) and settled.
func refuseCwdOutsideFence(ctx context.Context, topic *pubsub.Publisher, m *pubsub.Message, cmd commandMsg, cfg *Config) bool {
	if cmd.CwdFence == "" {
		return false
	}

	agentID := ""
	if cfg != nil {
		agentID = cfg.AgentID
	}

	if cmd.Cwd == "" || !filepath.IsAbs(cmd.Cwd) || !filepath.IsAbs(cmd.CwdFence) {
		msg := fmt.Sprintf("working directory and fence must be absolute paths; got cwd %q, fence %q", cmd.Cwd, cmd.CwdFence)
		if cmd.Cwd == "" {
			msg = fmt.Sprintf("working directory is empty; must be inside granted folder %q", cmd.CwdFence)
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

	realFence, errFence := fenceRealPath(cmd.CwdFence)
	realCwd, errCwd := fenceRealPath(cmd.Cwd)

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
