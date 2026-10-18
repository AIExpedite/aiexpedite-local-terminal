// File: worktree_commands.go
// -----------------------------------------------------------------------------
// The desktop app's built-in WORKTREE commands (Phase 1 of
// PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4, §9.2):
//
//	__worktree_ensure__       fetch, create or verify a worktree, add companions
//	__worktree_list__         registered worktrees with clean/dirty/ahead/behind
//	__worktree_remove__       remove ONE worktree, link-safe
//	__worktree_evict_deps__   reclaim rebuildable directories, stay resumable
//	__root_restore__          return a root checkout to its default branch
//
// They are modelled on __env_find_repos__: the app intercepts the command name
// BEFORE exec, so nothing is looked up on PATH, nothing is handed to a shell,
// and there is no approval dialog (§4.4). args[0] is a JSON string and the
// result is one line of JSON on the normal command-result path.
//
// WHY NO APPROVAL DIALOG FOR THE DESTRUCTIVE ONES. Removal and eviction are
// sent by the hourly Computer Maintenance sweep (§9.2) — unattended, on a tick,
// never credit-gated — so a native dialog would simply block them forever. The
// safety boundary is therefore not a prompt. It is, in order:
//
//  1. the HMAC-signed dispatch (the command name and args cannot be forged);
//  2. the path policy, on every destructive call (worktree_path_policy.go);
//  3. a device-side proof per action — registration, provenance, "nothing
//     uncommitted", "nothing unpushed", a baseline diff and a content digest —
//     evaluated ON THE COMPUTER, because the server's evaluation is a
//     pre-filter and never the authority;
//  4. the link-safe delete walk, which never follows a reparse point.
//
// Anything that fails a check is skipped and REPORTED, never deleted.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	worktreeEnsureCommand    = "__worktree_ensure__"
	worktreeListCommand      = "__worktree_list__"
	worktreeRemoveCommand    = "__worktree_remove__"
	worktreeEvictDepsCommand = "__worktree_evict_deps__"
	rootRestoreCommand       = "__root_restore__"
)

// Bounds. Every one is FIXED, never derived from an installed enum's length: a
// newer server may send more than this build knows about, and a bound derived
// from what is installed would turn that into a refusal (tolerant readers).
const (
	maxEnsureCompanions = 32
	maxEnsureDepsSpecs  = 32
	maxSeedFiles        = 32
	maxEvictDirs        = 32
	maxListMainRepos    = 64
	maxRecordedBranches = 64
	maxSeedFileBytes    = 1 << 20
)

// Additional policy rules for the inputs that are not worktree paths.
const (
	ruleMainRepoMissing     = "main_repo_missing"
	ruleMainRepoNotAbsolute = "main_repo_not_absolute"
	ruleMainRepoNotCheckout = "main_repo_not_a_checkout"
	ruleMainRepoInsideAixwt = "main_repo_inside_aixwt"
	ruleRescueRefUnsafe     = "rescue_ref_unsafe"
	ruleSeedFileUnsafe      = "seed_file_unsafe"
)

// isWorktreeDeviceCommand reports whether command is one of the worktree
// built-ins.
func isWorktreeDeviceCommand(command string) bool {
	switch command {
	case worktreeEnsureCommand, worktreeListCommand, worktreeRemoveCommand,
		worktreeEvictDepsCommand, rootRestoreCommand:
		return true
	}
	return false
}

// runWorktreeDeviceCommand runs one worktree built-in and returns its one-line
// JSON result, redacted value by value like every other device command.
// worktreeCommandWritesRootBookkeeping reports whether a command mutates the
// MAIN repository's git bookkeeping — the `.git/worktrees` admin records a
// root restore's branch switch runs alongside. `__worktree_list__` only reads,
// and `__worktree_evict_deps__` deletes inside the worktree and never touches
// the root's `.git`, so neither is fenced: refusing a read would leave the
// sweep with no inventory for that tick and buy nothing.
func worktreeCommandWritesRootBookkeeping(command string) bool {
	switch command {
	case worktreeEnsureCommand, worktreeRemoveCommand:
		return true
	default:
		return false
	}
}

func runWorktreeDeviceCommand(ctx context.Context, cmd commandMsg) (string, error) {
	// The root-use fence, read side. A root restore holds it across its
	// re-check AND its switch (§4.8), so a command that would write the same
	// repository's bookkeeping in that window is refused.
	//
	// The main repository is peeked out of the raw args rather than read off
	// each request type: one decode cannot drift from the five structs, and a
	// request whose own validation will reject it is refused on its own terms
	// below. A restore is exempt because it is the fence's HOLDER.
	if worktreeCommandWritesRootBookkeeping(cmd.Command) {
		var peek struct {
			MainRepo string `json:"mainRepo"`
		}
		if derr := decodeEnvSetupArgs(cmd.Args, &peek); derr == nil {
			if repo := strings.TrimSpace(peek.MainRepo); repo != "" && isRootUseFenced(repo) {
				return "", fmt.Errorf("%s refused: platform work holds the root-use fence for %s", cmd.Command, repo)
			}
		}
	}

	var result any
	var err error
	switch cmd.Command {
	case worktreeEnsureCommand:
		var req worktreeEnsureRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = ensureWorktree(ctx, req)
		}
	case worktreeListCommand:
		var req worktreeListRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = listWorktrees(ctx, req)
		}
	case worktreeRemoveCommand:
		var req worktreeRemoveRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = removeWorktree(ctx, req)
		}
	case worktreeEvictDepsCommand:
		var req worktreeEvictDepsRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = evictWorktreeDeps(ctx, req)
		}
	case rootRestoreCommand:
		var req rootRestoreRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = restoreRootCheckout(ctx, req)
		}
	default:
		err = fmt.Errorf("unknown worktree command %q", cmd.Command)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %s", cmd.Command, redactSensitiveData(err.Error()))
	}
	return encodeRedactedJSON(result)
}

// ─── shared input validation ────────────────────────────────────────────────

// resolveMainRepoPath validates the root checkout a dispatch names. It must be
// absolute, exist, be a git checkout, and NOT sit inside an `aixwt` directory —
// a worktree is never a main repository, and treating one as the main repo
// would aim `.git/worktrees` bookkeeping at the wrong place.
func resolveMainRepoPath(mainRepo string) (string, error) {
	mainRepo = strings.TrimSpace(mainRepo)
	if mainRepo == "" {
		return "", policyRefusal(ruleMainRepoMissing, "the dispatch named no main repository")
	}
	if !filepath.IsAbs(mainRepo) {
		return "", policyRefusal(ruleMainRepoNotAbsolute, fmt.Sprintf("main repository %q is not absolute", mainRepo))
	}
	mainRepo = filepath.Clean(mainRepo)
	for _, seg := range strings.Split(filepath.ToSlash(mainRepo), "/") {
		if strings.EqualFold(seg, worktreeRootSegment) {
			return "", policyRefusal(ruleMainRepoInsideAixwt,
				fmt.Sprintf("%q sits inside an %s directory, so it is a worktree and not a main repository", mainRepo, worktreeRootSegment))
		}
	}
	if !isGitCheckout(mainRepo) {
		return "", policyRefusal(ruleMainRepoNotCheckout, fmt.Sprintf("%q is not a git checkout", mainRepo))
	}
	return mainRepo, nil
}

func remoteOrDefault(remote string) string {
	if r := strings.TrimSpace(remote); r != "" {
		return r
	}
	return "origin"
}

// safeRefNamePattern is what a rescue ref may look like. Deliberately narrow:
// a ref name reaches `git push` as an argument, and `..`, a trailing `.lock`, a
// leading or trailing `/` and a leading `-` are all either refused by git or
// change what the command means.
var safeRefNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,159}$`)

func validateRescueRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", policyRefusal(ruleRescueRefUnsafe, "no rescue branch named")
	}
	if !strings.HasPrefix(ref, "aix/rescue/") {
		return "", policyRefusal(ruleRescueRefUnsafe,
			fmt.Sprintf("rescue branch %q must start with aix/rescue/", ref))
	}
	if strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") || strings.Contains(ref, "//") {
		return "", policyRefusal(ruleRescueRefUnsafe, fmt.Sprintf("rescue branch %q is not a safe ref name", ref))
	}
	if !safeRefNamePattern.MatchString(ref) {
		return "", policyRefusal(ruleRescueRefUnsafe, fmt.Sprintf("rescue branch %q is not a safe ref name", ref))
	}
	return ref, nil
}

// ─── __worktree_ensure__ ────────────────────────────────────────────────────

type worktreeCompanionSpec struct {
	MainRepo      string `json:"mainRepo"`
	Path          string `json:"path"`
	DefaultBranch string `json:"defaultBranch"`
	Remote        string `json:"remote"`
}

// worktreeDepsSpec is one ecosystem's per-checkout dependency directory. For
// `go`, `maven`, `gradle` and `nuget` there is nothing per checkout at all, so
// the server sends no spec and preparation is a no-op (§4.3.1).
type worktreeDepsSpec struct {
	Ecosystem      string `json:"ecosystem"`
	Dir            string `json:"dir"`
	Lockfile       string `json:"lockfile"`
	InstallOptions string `json:"installOptions"`
	Method         string `json:"method"`
	// ScanRelocatable asks for the clone gate of §4.3.1. `relocatable` is a
	// property of the TREE ON DISK, never of the ecosystem, and it defaults to
	// false — including for node_modules.
	ScanRelocatable bool `json:"scanRelocatable"`
}

type worktreeEnsureRequest struct {
	Root          string `json:"root"`
	MainRepo      string `json:"mainRepo"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	DefaultBranch string `json:"defaultBranch"`
	Remote        string `json:"remote"`
	// Fetch defaults to true. The object store is shared, so one fetch in the
	// main checkout serves every worktree.
	Fetch *bool `json:"fetch"`
	// Detach makes this a COMPANION: detached at <remote>/<default>, never a
	// checkout of the default branch (the root holds that branch, and git
	// refuses one branch in two worktrees).
	Detach bool `json:"detach"`
	// CopyUntracked is the approved allowlist, seeded ONCE at creation.
	CopyUntracked []string `json:"copyUntracked"`
	// Seeded is what the registry entry records having seeded already. Files
	// named here are REPORTED, never re-copied.
	Seeded     []string                `json:"seeded"`
	Deps       []worktreeDepsSpec      `json:"deps"`
	MarkDeps   bool                    `json:"markDeps"`
	Companions []worktreeCompanionSpec `json:"companions"`
}

type worktreeSeedReport struct {
	File  string `json:"file"`
	State string `json:"state"`
}

type worktreeDepsReport struct {
	Ecosystem          string                 `json:"ecosystem"`
	Dir                string                 `json:"dir"`
	State              string                 `json:"state"`
	LockfileSha256     string                 `json:"lockfileSha256,omitempty"`
	MarkerSha256       string                 `json:"markerSha256,omitempty"`
	Marked             bool                   `json:"marked,omitempty"`
	BaselineEntries    int                    `json:"baselineEntries,omitempty"`
	BaselineComplete   bool                   `json:"baselineComplete,omitempty"`
	TreeDigestComplete bool                   `json:"treeDigestComplete,omitempty"`
	Relocatable        *relocatableScanResult `json:"relocatable,omitempty"`
	Error              string                 `json:"error,omitempty"`
}

type worktreeCompanionResult struct {
	Path     string `json:"path"`
	Created  bool   `json:"created,omitempty"`
	Advanced bool   `json:"advanced,omitempty"`
	Diverged bool   `json:"diverged,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	Head     string `json:"head,omitempty"`
	Target   string `json:"target,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
}

type worktreeEnsureResult struct {
	Path        string   `json:"path"`
	Rel         string   `json:"rel"`
	Branch      string   `json:"branch"`
	Head        string   `json:"head"`
	Detached    bool     `json:"detached"`
	Dirty       bool     `json:"dirty"`
	DirtyPaths  []string `json:"dirtyPaths,omitempty"`
	Ahead       int      `json:"ahead"`
	Behind      int      `json:"behind"`
	HasUpstream bool     `json:"hasUpstream"`
	Registered  bool     `json:"registered"`
	Created     bool     `json:"created"`
	Fetched     bool     `json:"fetched"`
	DepsReady   bool     `json:"depsReady"`

	Deps        []worktreeDepsReport      `json:"deps,omitempty"`
	SeededNow   []string                  `json:"seededNow,omitempty"`
	SeedReports []worktreeSeedReport      `json:"seedReports,omitempty"`
	Companions  []worktreeCompanionResult `json:"companions,omitempty"`
	Notes       []string                  `json:"notes,omitempty"`
}

// ensureWorktree is §4.4 steps 1–5.
func ensureWorktree(ctx context.Context, req worktreeEnsureRequest) (worktreeEnsureResult, error) {
	var res worktreeEnsureResult
	if len(req.Companions) > maxEnsureCompanions {
		return res, fmt.Errorf("too many companions (%d, max %d)", len(req.Companions), maxEnsureCompanions)
	}
	if len(req.Deps) > maxEnsureDepsSpecs {
		return res, fmt.Errorf("too many dependency specs (%d, max %d)", len(req.Deps), maxEnsureDepsSpecs)
	}
	if len(req.CopyUntracked) > maxSeedFiles || len(req.Seeded) > maxSeedFiles {
		return res, fmt.Errorf("too many allowlisted untracked files (max %d)", maxSeedFiles)
	}
	wt, err := resolveWorktreePath(req.Root, req.Path)
	if err != nil {
		return res, err
	}
	mainRepo, err := resolveMainRepoPath(req.MainRepo)
	if err != nil {
		return res, err
	}
	remote := remoteOrDefault(req.Remote)
	res.Path, res.Rel = wt.Abs, wt.Rel

	// Step 1: one fetch in the main checkout serves every worktree.
	if req.Fetch == nil || *req.Fetch {
		if _, ferr := wtGitNetwork(ctx, mainRepo, "fetch", remote, "--prune"); ferr != nil {
			res.Notes = append(res.Notes, "fetch failed: "+ferr.Error())
		} else {
			res.Fetched = true
		}
	}

	entries, err := wtListWorktrees(ctx, mainRepo)
	if err != nil {
		return res, err
	}
	_, registered := wtFindWorktree(entries, wt.Abs)
	res.Registered = registered

	switch {
	case wt.Exists && !registered:
		// Never clobber a directory git does not know about: it may be a
		// person's folder that happens to sit at this path.
		return res, fmt.Errorf("%s exists but git has no worktree registered there; refusing to touch it", wt.Abs)
	case !wt.Exists:
		if registered {
			res.Notes = append(res.Notes,
				"git had a registered worktree at this path with no directory on disk; recreating")
			if adminDir, aerr := findAdminDirForPath(mainRepo, wt.Abs); aerr == nil && adminDir != "" {
				if _, derr := linkSafeRemoveAll(adminDir); derr != nil {
					return res, fmt.Errorf("clearing the stale admin record %s: %w", adminDir, derr)
				}
			}
		}
		if cerr := createWorktree(ctx, mainRepo, wt, req, remote, entries, &res); cerr != nil {
			return res, cerr
		}
		res.Created = true
		res.Registered = true
	}

	// Step 3 / 5: report state.
	state, serr := wtReadState(ctx, wt.Abs)
	if serr != nil {
		return res, serr
	}
	res.Branch, res.Head, res.Detached = state.Branch, state.Head, state.Detached
	res.Dirty, res.DirtyPaths = state.Dirty, state.DirtyPaths
	res.Ahead, res.Behind, res.HasUpstream = state.Ahead, state.Behind, state.HasUpstream
	if req.Branch != "" && !req.Detach && state.Branch != req.Branch {
		res.Notes = append(res.Notes,
			fmt.Sprintf("worktree is on %q, not the expected %q", state.Branch, req.Branch))
	}

	// Step 4: seeding — ONCE, at creation only.
	if res.Created {
		res.SeededNow, res.Notes = seedAllowlistedFiles(mainRepo, wt, req.CopyUntracked, res.Notes)
	}
	res.SeedReports = reportSeededFiles(mainRepo, wt, req.Seeded)

	// Step 4: companions.
	for _, c := range req.Companions {
		res.Companions = append(res.Companions, ensureCompanion(ctx, req.Root, c))
	}

	// Step 4: reconcile (and optionally record) dependency provenance.
	res.Deps, res.DepsReady = reconcileDeps(wt, req.Deps, req.MarkDeps)
	return res, nil
}

// createWorktree is §4.4 step 2. It NEVER resets a ref.
//
// `git worktree add -B` is documented as "create or reset a branch": on a local
// branch holding unpushed commits it MOVES the ref and discards the only named
// reference to those commits. That is not hypothetical during the Phase 1
// transition — today's author creates the feature branch in the root checkout,
// so an unpushed local branch of exactly this name is the EXPECTED inherited
// state. So the three cases are: attach an existing local branch unchanged,
// track a remote-only one into a new local branch, or branch from the default.
// The ahead/behind report then tells the run what to reconcile.
func createWorktree(ctx context.Context, mainRepo string, wt resolvedWorktreePath,
	req worktreeEnsureRequest, remote string, entries []gitWorktreeEntry, res *worktreeEnsureResult) error {
	defaultBranch := strings.TrimSpace(req.DefaultBranch)
	branch := strings.TrimSpace(req.Branch)

	if runtime.GOOS == "windows" {
		// Deep node_modules under a worktree path easily passes MAX_PATH 260.
		if _, err := wtGit(ctx, mainRepo, "config", "core.longpaths", "true"); err != nil {
			res.Notes = append(res.Notes, "could not set core.longpaths: "+err.Error())
		}
	}
	if err := os.MkdirAll(filepath.Dir(wt.Abs), 0o755); err != nil {
		return fmt.Errorf("creating the feature folder %s: %w", filepath.Dir(wt.Abs), err)
	}

	if req.Detach {
		if defaultBranch == "" {
			return errors.New("a companion needs defaultBranch to detach at")
		}
		target := remote + "/" + defaultBranch
		if _, err := wtGit(ctx, mainRepo, "worktree", "add", "--detach", wt.Abs, target); err != nil {
			return err
		}
		return nil
	}
	if branch == "" {
		return errors.New("branch is required unless detach is set")
	}

	// A ref held elsewhere: refuse with the REASON rather than git's raw
	// `fatal: '<branch>' is already used by worktree at …`.
	for _, e := range entries {
		if e.Branch != branch {
			continue
		}
		if pathIdentityKey(e.Path) == pathIdentityKey(mainRepo) {
			return fmt.Errorf("the root checkout %s holds %q; restore the root first (__root_restore__)", mainRepo, branch)
		}
		return fmt.Errorf("the worktree at %s already holds %q", e.Path, branch)
	}

	localExists, err := wtLocalBranchExists(ctx, mainRepo, branch)
	if err != nil {
		return err
	}
	if localExists {
		// Attach it UNCHANGED. No -b, no -B, no reset.
		_, err = wtGit(ctx, mainRepo, "worktree", "add", wt.Abs, branch)
		return err
	}
	remoteExists, err := wtRemoteBranchExists(ctx, mainRepo, remote, branch)
	if err != nil {
		return err
	}
	if remoteExists {
		_, err = wtGit(ctx, mainRepo, "worktree", "add", "--track", "-b", branch, wt.Abs, remote+"/"+branch)
		return err
	}
	if defaultBranch == "" {
		return fmt.Errorf("neither %q nor %s/%s exists and no defaultBranch was given to branch from", branch, remote, branch)
	}
	_, err = wtGit(ctx, mainRepo, "worktree", "add", "-b", branch, wt.Abs, remote+"/"+defaultBranch)
	return err
}

// ensureCompanion adds a missing companion detached at <remote>/<default>, and
// ADVANCES an existing one only when both hold: its working tree is clean, AND
// its HEAD is an ancestor of the newly fetched ref.
//
// "Clean" alone is not enough. A detached HEAD can hold commits an earlier run
// made while the working tree looks pristine, and re-detaching would leave them
// reachable only through the reflog. A companion that has diverged, or that has
// local modifications, is left EXACTLY as it is and reported — nothing in the
// platform should ever have committed in a companion, so this is a signal, not
// a case to resolve silently.
func ensureCompanion(ctx context.Context, root string, c worktreeCompanionSpec) worktreeCompanionResult {
	out := worktreeCompanionResult{Path: c.Path}
	wt, err := resolveWorktreePath(root, c.Path)
	if err != nil {
		out.Error = err.Error()
		out.Reason = worktreePolicyRuleOf(err)
		return out
	}
	out.Path = wt.Abs
	mainRepo, err := resolveMainRepoPath(c.MainRepo)
	if err != nil {
		out.Error = err.Error()
		out.Reason = worktreePolicyRuleOf(err)
		return out
	}
	remote := remoteOrDefault(c.Remote)
	defaultBranch := strings.TrimSpace(c.DefaultBranch)
	if defaultBranch == "" {
		out.Error = "companion has no defaultBranch"
		out.Reason = "no_default_branch"
		return out
	}
	target, err := wtRevParse(ctx, mainRepo, "refs/remotes/"+remote+"/"+defaultBranch)
	if err != nil || target == "" {
		out.Error = fmt.Sprintf("%s/%s does not exist locally", remote, defaultBranch)
		out.Reason = "default_branch_missing"
		return out
	}
	out.Target = target

	if !wt.Exists {
		if err := os.MkdirAll(filepath.Dir(wt.Abs), 0o755); err != nil {
			out.Error = err.Error()
			return out
		}
		if _, aerr := wtGit(ctx, mainRepo, "worktree", "add", "--detach", wt.Abs, target); aerr != nil {
			out.Error = aerr.Error()
			out.Reason = "add_failed"
			return out
		}
		out.Created = true
		out.Head = target
		return out
	}
	state, serr := wtReadState(ctx, wt.Abs)
	if serr != nil {
		out.Error = serr.Error()
		return out
	}
	out.Head, out.Dirty = state.Head, state.Dirty
	if state.Head == target {
		out.Reason = "already_current"
		return out
	}
	if state.Dirty {
		out.Reason = "left_alone_dirty"
		return out
	}
	ancestor, aerr := wtIsAncestor(ctx, wt.Abs, state.Head, target)
	if aerr != nil {
		out.Error = aerr.Error()
		out.Reason = "ancestry_unknown"
		return out
	}
	if !ancestor {
		// It carries something of its own. Leave it and report; a rescue push
		// of those commits is the removal path's job (§9.2), not this one's.
		out.Diverged = true
		out.Reason = "left_alone_diverged"
		return out
	}
	if _, cerr := wtGit(ctx, wt.Abs, "checkout", "--detach", target); cerr != nil {
		out.Error = cerr.Error()
		out.Reason = "advance_failed"
		return out
	}
	out.Advanced = true
	out.Head = target
	return out
}

// validateSeedFile keeps an allowlisted untracked file to a relative path with
// no traversal and no link in its way.
func validateSeedFile(wt resolvedWorktreePath, file string) (src string, dst string, err error) {
	cand, verr := resolveInsideWorktree(wt, file)
	if verr != nil {
		return "", "", policyRefusal(ruleSeedFileUnsafe, verr.Error())
	}
	abs := cand.Abs
	return filepath.Join(filepath.FromSlash(file)), abs, nil
}

// seedAllowlistedFiles copies the approved untracked files from the root
// checkout into a NEWLY CREATED worktree.
//
// Seeding happens ONCE, at creation, and never on a later refresh. §4.4 step 4
// runs on every refresh and these files are untracked, so re-copying the root's
// version would destroy work git cannot recover. "Copy when the destination is
// absent" is not enough either: an agent that deliberately DELETED an
// allowlisted file would have it silently restored, and the feature would run
// with configuration it had explicitly removed.
func seedAllowlistedFiles(mainRepo string, wt resolvedWorktreePath, files []string, notes []string) ([]string, []string) {
	var seeded []string
	for _, file := range files {
		rel, dst, err := validateSeedFile(wt, file)
		if err != nil {
			notes = append(notes, "not seeded: "+err.Error())
			continue
		}
		src := filepath.Join(mainRepo, rel)
		// The DESTINATION's components were walked for links by
		// resolveInsideWorktree. The source's were not, and it is read through:
		// a linked directory component in the root checkout (`config` ->
		// `/etc`, say) would let an allowlist entry of `config/<name>` copy a
		// file from outside the checkout into the worktree, where the feature's
		// coding agent can read it. Checking the file itself is not enough,
		// because the link is above it.
		if lerr := refuseLinkedComponents(mainRepo, rel); lerr != nil {
			notes = append(notes, "not seeded: "+lerr.Error())
			continue
		}
		info, serr := os.Lstat(src)
		if serr != nil {
			notes = append(notes, fmt.Sprintf("not seeded: %s is not in the root checkout", file))
			continue
		}
		if isWorktreeLink(info, src) || info.IsDir() {
			notes = append(notes, fmt.Sprintf("not seeded: %s is a directory or a link", file))
			continue
		}
		if info.Size() > maxSeedFileBytes {
			notes = append(notes, fmt.Sprintf("not seeded: %s is %d bytes", file, info.Size()))
			continue
		}
		if _, derr := os.Lstat(dst); derr == nil {
			notes = append(notes, fmt.Sprintf("not seeded: %s already exists in the worktree", file))
			continue
		}
		raw, rerr := os.ReadFile(src)
		if rerr != nil {
			notes = append(notes, fmt.Sprintf("not seeded: reading %s: %v", file, rerr))
			continue
		}
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			notes = append(notes, fmt.Sprintf("not seeded: %s: %v", file, merr))
			continue
		}
		if werr := os.WriteFile(dst, raw, info.Mode().Perm()); werr != nil {
			notes = append(notes, fmt.Sprintf("not seeded: %s: %v", file, werr))
			continue
		}
		seeded = append(seeded, filepath.ToSlash(file))
	}
	return seeded, notes
}

// reportSeededFiles says what became of the files the registry records having
// seeded: present, missing (an agent removed it on purpose — "absent because
// removed" must stay distinguishable from "never seeded"), differing, or
// unreadable. Reported only. Synchronising one is an explicit act.
func reportSeededFiles(mainRepo string, wt resolvedWorktreePath, seeded []string) []worktreeSeedReport {
	var reports []worktreeSeedReport
	for _, file := range seeded {
		rel, dst, err := validateSeedFile(wt, file)
		if err != nil {
			reports = append(reports, worktreeSeedReport{File: file, State: "unsafe"})
			continue
		}
		have, herr := os.ReadFile(dst)
		if herr != nil {
			if errors.Is(herr, fs.ErrNotExist) {
				reports = append(reports, worktreeSeedReport{File: filepath.ToSlash(file), State: "missing"})
			} else {
				reports = append(reports, worktreeSeedReport{File: filepath.ToSlash(file), State: "unreadable"})
			}
			continue
		}
		want, werr := os.ReadFile(filepath.Join(mainRepo, rel))
		if werr != nil {
			reports = append(reports, worktreeSeedReport{File: filepath.ToSlash(file), State: "present"})
			continue
		}
		state := "present"
		if !bytes.Equal(have, want) {
			state = "differs"
		}
		reports = append(reports, worktreeSeedReport{File: filepath.ToSlash(file), State: state})
	}
	return reports
}

// reconcileDeps compares each dependency directory's recorded lockfile digest
// with the worktree's current one, and records a marker when markDeps is set
// (which the server passes right after an install has run).
//
// Equal → ready. Different or MARKER ABSENT → install. Never trust the
// ecosystem's own record: a virtualenv, `vendor/bundle` and Mix's `deps` keep
// no portable one, so a check built on it would silently accept stale
// dependencies in exactly the ecosystems it cannot read.
func reconcileDeps(wt resolvedWorktreePath, specs []worktreeDepsSpec, markDeps bool) ([]worktreeDepsReport, bool) {
	ready := true
	var reports []worktreeDepsReport
	for _, spec := range specs {
		rep := worktreeDepsReport{Ecosystem: spec.Ecosystem, Dir: spec.Dir}
		cand, err := resolveInsideWorktree(wt, spec.Dir)
		if err != nil {
			rep.State, rep.Error = "unsafe", err.Error()
			ready = false
			reports = append(reports, rep)
			continue
		}
		abs := cand.Abs
		digest, derr := lockfileDigestFor(wt.Abs, spec.Lockfile)
		if derr != nil {
			rep.State, rep.Error = "unknown", derr.Error()
			ready = false
			reports = append(reports, rep)
			continue
		}
		rep.LockfileSha256 = digest
		info, serr := os.Lstat(abs)
		exists := serr == nil && info.IsDir() && !isWorktreeLink(info, abs)

		if markDeps && exists {
			marker, merr := recordDepsMarker(wt, spec, abs, digest)
			if merr != nil {
				rep.State, rep.Error = "unknown", merr.Error()
				ready = false
				reports = append(reports, rep)
				continue
			}
			rep.Marked = true
			rep.BaselineEntries = len(marker.Baseline)
			rep.BaselineComplete = marker.BaselineComplete
			rep.TreeDigestComplete = marker.TreeDigestComplete
			if spec.ScanRelocatable {
				if scan, scanErr := scanRelocatable(abs); scanErr == nil {
					rep.Relocatable = &scan
				}
			}
		}

		marker, merr := readDepsMarker(abs)
		switch {
		case !exists:
			rep.State = "absent"
			ready = false
		case merr != nil:
			rep.State, rep.Error = "unknown", merr.Error()
			ready = false
		case marker == nil:
			rep.State = "missing_marker"
			ready = false
		default:
			rep.MarkerSha256 = marker.LockfileSha256
			rep.BaselineEntries = len(marker.Baseline)
			rep.BaselineComplete = marker.BaselineComplete
			rep.TreeDigestComplete = marker.TreeDigestComplete
			if digest != "" && marker.LockfileSha256 == digest && marker.InstallOptions == spec.InstallOptions {
				rep.State = "ready"
			} else {
				rep.State = "stale"
				ready = false
			}
		}
		reports = append(reports, rep)
	}
	return reports, ready
}

// recordDepsMarker is the device half of `prepare`: it writes the provenance
// marker, its baseline manifest and its content digest for a directory the
// platform has just created or cloned.
func recordDepsMarker(wt resolvedWorktreePath, spec worktreeDepsSpec, abs, lockDigest string) (*depsProvenanceMarker, error) {
	baseline, complete, err := scanBaseline(abs)
	if err != nil {
		return nil, err
	}
	treeDigest, digestComplete, err := digestTree(abs)
	if err != nil {
		return nil, err
	}
	method := spec.Method
	if method == "" {
		method = "install"
	}
	marker := &depsProvenanceMarker{
		Version:            depsMarkerVersion,
		Ecosystem:          spec.Ecosystem,
		Dir:                filepath.Clean(abs),
		Worktree:           wt.Abs,
		LockfilePath:       spec.Lockfile,
		LockfileSha256:     lockDigest,
		InstallOptions:     spec.InstallOptions,
		Method:             method,
		Baseline:           baseline,
		BaselineComplete:   complete,
		TreeSha256:         treeDigest,
		TreeDigestComplete: digestComplete && complete,
	}
	marker.CreatedAt = time.Now().UTC()
	if err := writeDepsMarker(marker); err != nil {
		return nil, err
	}
	return marker, nil
}

// ─── __worktree_list__ ──────────────────────────────────────────────────────

type worktreeListRequest struct {
	Root      string   `json:"root"`
	MainRepos []string `json:"mainRepos"`
}

type worktreeListEntry struct {
	Path          string   `json:"path"`
	Rel           string   `json:"rel"`
	FeatureFolder string   `json:"featureFolder"`
	RepoFolder    string   `json:"repoFolder"`
	MainRepo      string   `json:"mainRepo"`
	Branch        string   `json:"branch,omitempty"`
	Head          string   `json:"head,omitempty"`
	Detached      bool     `json:"detached,omitempty"`
	Dirty         bool     `json:"dirty"`
	DirtyPaths    []string `json:"dirtyPaths,omitempty"`
	Ahead         int      `json:"ahead"`
	Behind        int      `json:"behind"`
	HasUpstream   bool     `json:"hasUpstream"`
	Locked        bool     `json:"locked,omitempty"`
	Prunable      bool     `json:"prunable,omitempty"`
	// Missing is a registered worktree whose directory is gone. Reported, never
	// pruned: `git worktree prune` would drop EVERY such record, including a
	// person's own worktree on an unmounted drive.
	Missing bool   `json:"missing,omitempty"`
	Error   string `json:"error,omitempty"`
}

type worktreeListResult struct {
	Root      string              `json:"root"`
	Worktrees []worktreeListEntry `json:"worktrees"`
	// OtherWorktreeCount counts registered worktrees that are NOT ours (they
	// do not match the path policy under this root). A count, never a path:
	// unknown worktrees are never touched at any level, and listing their
	// paths would invite a registry row that targets one.
	OtherWorktreeCount int      `json:"otherWorktreeCount"`
	Errors             []string `json:"errors,omitempty"`
}

func listWorktrees(ctx context.Context, req worktreeListRequest) (worktreeListResult, error) {
	res := worktreeListResult{Worktrees: []worktreeListEntry{}}
	if len(req.MainRepos) > maxListMainRepos {
		return res, fmt.Errorf("too many main repositories (%d, max %d)", len(req.MainRepos), maxListMainRepos)
	}
	root := strings.TrimSpace(req.Root)
	if root == "" || !filepath.IsAbs(root) {
		return res, policyRefusal(rulePathRootNotAbsolute, fmt.Sprintf("root %q must be absolute", req.Root))
	}
	res.Root = filepath.Clean(root)

	seen := map[string]bool{}
	for _, raw := range req.MainRepos {
		mainRepo, err := resolveMainRepoPath(raw)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		entries, lerr := wtListWorktrees(ctx, mainRepo)
		if lerr != nil {
			res.Errors = append(res.Errors, lerr.Error())
			continue
		}
		for _, e := range entries {
			if pathIdentityKey(e.Path) == pathIdentityKey(mainRepo) {
				continue // the root checkout itself
			}
			wt, perr := resolveWorktreePath(res.Root, e.Path)
			if perr != nil {
				res.OtherWorktreeCount++
				continue
			}
			if seen[pathIdentityKey(wt.Abs)] {
				continue
			}
			seen[pathIdentityKey(wt.Abs)] = true
			row := worktreeListEntry{
				Path:          wt.Abs,
				Rel:           wt.Rel,
				FeatureFolder: wt.FeatureFolder,
				RepoFolder:    wt.RepoFolder,
				MainRepo:      mainRepo,
				Branch:        e.Branch,
				Head:          e.Head,
				Detached:      e.Detached,
				Locked:        e.Locked,
				Prunable:      e.Prunable,
			}
			if !wt.Exists {
				row.Missing = true
				res.Worktrees = append(res.Worktrees, row)
				continue
			}
			state, serr := wtReadState(ctx, wt.Abs)
			if serr != nil {
				row.Error = serr.Error()
				res.Worktrees = append(res.Worktrees, row)
				continue
			}
			row.Branch, row.Head, row.Detached = state.Branch, state.Head, state.Detached
			row.Dirty, row.DirtyPaths = state.Dirty, state.DirtyPaths
			row.Ahead, row.Behind, row.HasUpstream = state.Ahead, state.Behind, state.HasUpstream
			res.Worktrees = append(res.Worktrees, row)
		}
	}
	return res, nil
}
