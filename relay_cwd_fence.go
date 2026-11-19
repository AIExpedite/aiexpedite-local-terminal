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
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2"
)

// cwdOutsideGrantFolderRejectionCode is the rejection reason when a session's
// working directory escapes the granted folder boundary (shared-constants
// REJECTION_REASONS.CWD_OUTSIDE_GRANT_FOLDER).
const cwdOutsideGrantFolderRejectionCode = "CWD_OUTSIDE_GRANT_FOLDER"

// maxFenceLinkHops bounds how many links fenceRealPath follows, so a link
// cycle refuses instead of looping.
const maxFenceLinkHops = 64

// pathSegments splits a path below its volume into its names. Only an ASCII
// rune can be a separator: uint8(r) alone would read U+015C as a backslash and
// split a name such as "aŜlink" in two, hiding a junction named that.
func pathSegments(p string) (vol string, segments []string) {
	vol = filepath.VolumeName(p)
	segments = strings.FieldsFunc(p[len(vol):], func(r rune) bool {
		return r < utf8.RuneSelf && os.IsPathSeparator(uint8(r))
	})
	return vol, segments
}

// hasParentSegment reports whether a path names ".." anywhere. Such a path is
// never fenced: the server never admits one through a parent grant (its
// lineage is itself only), and POSIX resolves ".." after a link while Windows
// resolves it by name, so no single reading of it is the folder the CLI runs in.
func hasParentSegment(p string) bool {
	_, segments := pathSegments(p)
	for _, seg := range segments {
		if seg == ".." {
			return true
		}
	}
	return false
}

// linkTargetPath is where a link in folder `dir` (a real folder, no link in
// it) points, without cleaning: an absolute target as is; on Windows a
// root-relative target ("\Windows") on dir's own volume, never under dir; a
// plain relative target under dir. A drive-relative target ("C:foo") depends
// on that drive's current directory, so it is refused.
func linkTargetPath(dir, target string) (string, error) {
	switch {
	case filepath.IsAbs(target):
		return target, nil
	case filepath.VolumeName(target) != "":
		return "", fmt.Errorf("drive-relative target %q", target)
	case target != "" && os.IsPathSeparator(target[0]):
		return filepath.VolumeName(dir) + target, nil
	default:
		return dir + string(filepath.Separator) + target, nil
	}
}

// fenceRealPath is the real folder an absolute path names, following symlinks
// AND Windows junctions in every component. filepath.EvalSymlinks alone is not
// enough: since Go 1.23 it does not traverse junctions, so a junction inside
// the granted folder (C:\AIExpedite\link -> D:\other) would read as inside it,
// and it fails outright on a path that runs THROUGH one.
//
// It walks the path name by name from its volume; at the first name that is a
// link (os.Readlink reads symlinks and junctions alike) it restarts from the
// link's target plus the names left, never cleaning the joined path
// lexically, so a ".." inside a link's target is resolved against the real
// folder it follows. With no link left, EvalSymlinks gives the canonical
// spelling. A missing path, a dangling link or a cycle is an error, so the
// fence fails closed.
func fenceRealPath(p string) (string, error) {
	sep := string(filepath.Separator)
	for hops := 0; ; {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%q is not an absolute path", p)
		}
		vol, segments := pathSegments(p)
		cur := vol + sep
		next := ""
		for i, seg := range segments {
			switch seg {
			case ".":
				continue
			case "..":
				// cur holds no link (each was followed), so its parent is real.
				cur = filepath.Dir(cur)
				continue
			}
			candidate := filepath.Join(cur, seg)
			target, err := os.Readlink(candidate)
			if err != nil || target == "" {
				cur = candidate
				continue
			}
			if hops++; hops > maxFenceLinkHops {
				return "", errors.New("too many links")
			}
			target, err = linkTargetPath(cur, target)
			if err != nil {
				return "", fmt.Errorf("link %q: %w", candidate, err)
			}
			next = target
			if rest := segments[i+1:]; len(rest) > 0 {
				next += sep + strings.Join(rest, sep)
			}
			break
		}
		if next == "" {
			return filepath.EvalSymlinks(cur)
		}
		p = next
	}
}

// refuseCwdOutsideFence checks whether a session start admitted through an
// ancestor grant stays inside that granted boundary on disk.
// If cmd carries CwdFence, both the fence and the working directory are
// resolved through symlinks and junctions (fenceRealPath); a path naming ".."
// is refused unread. If either cannot be
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

	if cmd.Cwd == "" || !filepath.IsAbs(cmd.Cwd) || !filepath.IsAbs(cmd.CwdFence) ||
		hasParentSegment(cmd.Cwd) || hasParentSegment(cmd.CwdFence) {
		msg := fmt.Sprintf("working directory and fence must be absolute paths; got cwd %q, fence %q", cmd.Cwd, cmd.CwdFence)
		if cmd.Cwd == "" {
			msg = fmt.Sprintf("working directory is empty; must be inside granted folder %q", cmd.CwdFence)
		} else if hasParentSegment(cmd.Cwd) || hasParentSegment(cmd.CwdFence) {
			msg = fmt.Sprintf("working directory %q or granted folder %q names \"..\"; a fenced start never does", cmd.Cwd, cmd.CwdFence)
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
