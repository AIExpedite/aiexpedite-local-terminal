// File: worktree_submodules.go
// -----------------------------------------------------------------------------
// Submodules in a freshly created worktree
// (WORKTREE_CONFIG_AND_PORTABILITY.md §8 item 4).
//
// `git worktree add` checks out the superproject and NOTHING ELSE: every
// submodule directory is left empty. A repository that keeps a dependency, a
// shared schema or its own tooling in a submodule therefore cannot build in a
// worktree at all, and the failure looks like a missing file rather than a
// missing checkout. One command fixes it, and it is the same command a person
// runs after their own clone.
//
// THE SECURITY BOUNDARY IS THE SAME AS EVERYWHERE ELSE IN PREPARE (§6): this
// runs before any coding agent exists, so it must not be able to execute
// repository-controlled code. `.gitmodules` is repository-controlled DATA, and
// a URL in it reaches git's transport layer, so two transports are turned off
// explicitly:
//
//   - `ext::<command>` runs an arbitrary command AS the transport. That is
//     remote code execution with a URL as the payload, and a tracked
//     `.gitmodules` is the delivery mechanism. Denied on the command line,
//     which outranks every config file, so a repository cannot re-enable it.
//
// `submodule.<name>.update = !<command>` looks like the same hazard by another
// route, AND IT IS NOT: measured on git 2.49 and 2.56, git refuses that form
// from `.gitmodules` outright and will not even COMMIT it, so a pull request
// cannot deliver it. The mode is still pinned with `--checkout`, for the
// ordinary reason — a freshly created worktree has nothing to rebase or merge
// onto, so `checkout` is the only mode that makes sense, and prepare should not
// inherit a preference meant for a person's own `git submodule update`.
//
// WHAT IS DELIBERATELY *NOT* DENIED is the `file` transport, and not because it
// is harmless: because git already refuses it here and refuses it BETTER than
// we could. Since 2.38.1 (CVE-2022-39253) a submodule init will not clone a
// local path, and — measured, not assumed — git ignores a REPOSITORY-level
// `protocol.file.allow`, honouring only the command line, the environment and
// the GLOBAL config. So a tracked `.gitmodules` cannot re-enable it, while an
// owner with local submodules can, in their own global config. Denying it here
// would override the owner's choice and protect against nothing; this way
// prepare behaves exactly as that person's own `git clone --recursive` does.
//
// Hooks are already covered: wtNoHooksArgs passes `core.hooksPath=<empty
// absolute dir>` as a `-c` override, and git exports its `-c` options to child
// processes through GIT_CONFIG_PARAMETERS, so the submodule helper's own
// checkouts inherit the suppression.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// wtSubmoduleTimeout bounds the whole init, including the clones.
//
// Deliberately INSIDE terminal-service's per-command budget
// (WORKTREE_PREPARE_COMMAND_TIMEOUT_MS, 120 s) rather than at the network
// timeout the fetch uses: the orchestrator is waiting on this response, so a
// repository with a very large submodule must report "not finished" and let the
// coding agent complete it, not hold the whole prepare open until the server
// gives up and learns nothing.
//
// It is also what bounds recursion: a submodule that declares itself, or a
// cycle between two, would otherwise recurse as long as git let it.
const wtSubmoduleTimeout = 75 * time.Second

// wtSubmoduleSafeTransport is the denial above, as `-c` overrides. On the
// command line on purpose: `-c` outranks every config file, so neither
// `.gitmodules` nor a repository's own `.git/config` can put `ext::` back.
func wtSubmoduleSafeTransport() []string {
	return []string{"-c", "protocol.ext.allow=never"}
}

// initSubmodules checks out the submodules a new worktree declares.
//
// Returns nil when the checkout declares none, so a repository without
// submodules reports nothing at all rather than a row of zeroes.
//
// NEVER FATAL. A submodule that cannot be cloned (no network, no access to a
// private submodule, a URL on a refused transport) leaves a worktree that is
// still a valid checkout of the superproject, and the brief can tell the agent
// to finish the job. Failing the whole prepare instead would send the run back
// to the shared root checkout, which is strictly worse.
func initSubmodules(ctx context.Context, wt resolvedWorktreePath, enabled *bool) *worktreeSubmoduleResult {
	modules := filepath.Join(wt.Abs, ".gitmodules")
	info, err := os.Lstat(modules)
	if err != nil {
		// No `.gitmodules`: nothing declared, nothing to say.
		return nil
	}
	if enabled != nil && !*enabled {
		return &worktreeSubmoduleResult{Present: true, Reason: "disabled"}
	}
	if isWorktreeLink(info, modules) || !info.Mode().IsRegular() {
		// A link or a directory where git expects a tracked file. Nothing is
		// read through it and nothing is initialised from it: the same rule the
		// seed allowlist and the eviction walk apply.
		return &worktreeSubmoduleResult{Present: true, Reason: "gitmodules_not_a_file"}
	}

	out := &worktreeSubmoduleResult{Present: true}
	// `--checkout` pins the update MODE instead of taking it from configuration:
	// a new worktree has nothing to rebase or merge onto. See the file header for
	// why this is NOT the security control it first looked like.
	args := append(wtSubmoduleSafeTransport(), "submodule", "update", "--init", "--recursive", "--checkout")
	if _, uerr := wtRunGitIn(ctx, wt.Abs, wtSubmoduleTimeout, args...); uerr != nil {
		out.Reason = "failed"
		if ctx.Err() != nil || strings.Contains(uerr.Error(), "signal: killed") ||
			strings.Contains(uerr.Error(), "context deadline exceeded") {
			out.Reason = "timed_out"
		}
		out.Error = uerr.Error()
		// Fall through: a partial init is still worth COUNTING, because the
		// brief then says how much is left rather than "submodules failed".
	}

	populated, total := countSubmodules(ctx, wt)
	out.Count = populated
	if out.Reason == "" && total > 0 && populated == total {
		out.Initialised = true
	}
	if out.Reason == "" && total == 0 {
		// `.gitmodules` exists but git reports no submodule paths — an
		// unregistered or commented-out declaration. Not a failure, and not a
		// success either: reported as it is.
		out.Reason = "none_registered"
	}
	return out
}

// countSubmodules reports how many submodule paths are populated and how many
// git knows about, read from `git submodule status --recursive`.
//
// git's prefix vocabulary: `-` not initialised, `+` checked out at a different
// commit than the superproject records, `U` has merge conflicts, a space in
// sync. Only `-` means "not there", so `+` counts as populated: a submodule the
// run will reconcile is still a submodule the build can see.
func countSubmodules(ctx context.Context, wt resolvedWorktreePath) (populated, total int) {
	out, err := wtGit(ctx, wt.Abs, "submodule", "status", "--recursive")
	if err != nil {
		return 0, 0
	}
	for _, line := range nonEmptyLines(out) {
		total++
		if !strings.HasPrefix(line, "-") {
			populated++
		}
	}
	return populated, total
}
