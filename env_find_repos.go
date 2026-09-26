// File: env_find_repos.go
// -----------------------------------------------------------------------------
// __env_find_repos__ — list the git checkouts directly under a set of roots
// (the clone root and the home directory), so the server can map repositories
// already on disk before it clones anything (COMPUTER_SETUP_CHECKLIST_PLAN.md
// §7 step 4).
//
// Read-only and spawn-free: remotes, HEAD and the branch are read from the
// repository files themselves (.git/config, HEAD, refs, packed-refs), which also
// covers linked worktrees (`.git` is a file pointing at the real git dir). Only
// a reftable repository, whose refs are not plain files, asks git for HEAD.
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	maxFindReposCheckouts = 500
	maxFindReposRoots     = 16
	maxFindReposDepth     = 3
	// maxFindReposDirsVisited bounds the directories read per request, so a
	// home directory with an enormous fan-out cannot stall the command.
	maxFindReposDirsVisited = 20000
)

type envFindReposRequest struct {
	Roots []string `json:"roots"`
	Depth int      `json:"depth"`
}

type envCheckout struct {
	Path    string            `json:"path"`
	Remotes map[string]string `json:"remotes"`
	HeadSha string            `json:"headSha"`
	Branch  string            `json:"branch"`
}

type envFindReposResult struct {
	Checkouts []envCheckout `json:"checkouts"`
	// Truncated is set when the 500-checkout cap (or the directory-visit
	// bound) stopped the scan early.
	Truncated bool `json:"truncated,omitempty"`
}

// findRepositoryCheckouts scans each root for git checkouts up to req.Depth
// levels below it (default and minimum 1: direct children; a checkout is never
// descended into). Unreadable directories are skipped; roots that are not
// absolute or do not exist are ignored.
func findRepositoryCheckouts(ctx context.Context, req envFindReposRequest) (envFindReposResult, error) {
	res := envFindReposResult{Checkouts: []envCheckout{}}
	if len(req.Roots) == 0 {
		return res, errors.New("roots is required")
	}
	if len(req.Roots) > maxFindReposRoots {
		return res, errors.New("too many roots")
	}
	depth := req.Depth
	if depth < 1 {
		depth = 1
	}
	if depth > maxFindReposDepth {
		depth = maxFindReposDepth
	}

	seen := map[string]bool{}
	visited := 0
	type level struct {
		dir   string
		depth int
	}
	for _, root := range req.Roots {
		root = strings.TrimSpace(root)
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		root = filepath.Clean(root)
		queue := []level{{dir: root, depth: 0}}
		for len(queue) > 0 {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			cur := queue[0]
			queue = queue[1:]
			if visited >= maxFindReposDirsVisited {
				res.Truncated = true
				break
			}
			visited++
			entries, err := os.ReadDir(cur.dir)
			if err != nil {
				continue // unreadable: skip
			}
			for _, e := range entries {
				child := filepath.Join(cur.dir, e.Name())
				if !isDirFollowingLinks(e, child) {
					continue
				}
				if isGitCheckout(child) {
					key := pathIdentityKey(child)
					if seen[key] {
						continue
					}
					seen[key] = true
					if len(res.Checkouts) >= maxFindReposCheckouts {
						res.Truncated = true
						break
					}
					res.Checkouts = append(res.Checkouts, readCheckout(ctx, child))
					continue
				}
				if cur.depth+1 < depth && !skipFindReposDescent(e.Name()) {
					queue = append(queue, level{dir: child, depth: cur.depth + 1})
				}
			}
			if len(res.Checkouts) >= maxFindReposCheckouts && res.Truncated {
				break
			}
		}
		if len(res.Checkouts) >= maxFindReposCheckouts && res.Truncated {
			break
		}
	}
	sort.SliceStable(res.Checkouts, func(i, j int) bool { return res.Checkouts[i].Path < res.Checkouts[j].Path })
	return res, nil
}

// skipFindReposDescent: never walk into dependency / cache trees when depth > 1.
func skipFindReposDescent(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor"
}

// pathIdentityKey dedupes the same directory reached from two roots.
func pathIdentityKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

// isDirFollowingLinks reports whether a directory entry is (or links to) a
// directory.
func isDirFollowingLinks(e os.DirEntry, full string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		info, err := os.Stat(full)
		return err == nil && info.IsDir()
	}
	return false
}

// isGitCheckout reports whether dir has a `.git` directory, or a `.git` file
// (a linked worktree or a submodule checkout).
func isGitCheckout(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	if info.IsDir() {
		return true
	}
	return info.Mode().IsRegular()
}

// resolveGitDirs returns the checkout's git dir (HEAD lives here) and common
// dir (config, refs and packed-refs live here; the same dir outside worktrees).
func resolveGitDirs(checkout string) (gitDir, commonDir string, ok bool) {
	dotGit := filepath.Join(checkout, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", "", false
	}
	gitDir = dotGit
	if !info.IsDir() {
		raw, err := os.ReadFile(dotGit)
		if err != nil {
			return "", "", false
		}
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", "", false
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(checkout, target)
		}
		gitDir = filepath.Clean(target)
	}
	commonDir = gitDir
	if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(raw))
		if c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitDir, c)
			}
			commonDir = filepath.Clean(c)
		}
	}
	return gitDir, commonDir, true
}

func readCheckout(ctx context.Context, checkout string) envCheckout {
	co := envCheckout{Path: checkout, Remotes: map[string]string{}}
	gitDir, commonDir, ok := resolveGitDirs(checkout)
	if !ok {
		return co
	}
	if raw, err := os.ReadFile(filepath.Join(commonDir, "config")); err == nil {
		co.Remotes = parseGitRemotes(raw)
	}
	co.HeadSha, co.Branch = readGitHead(gitDir, commonDir)
	if co.HeadSha == "" && isDirPath(filepath.Join(commonDir, "reftable")) {
		co.HeadSha, co.Branch = gitHeadViaCLI(ctx, checkout, co.Branch)
	}
	return co
}

func isDirPath(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

var (
	gitSectionPattern = regexp.MustCompile(`(?i)^\[\s*remote\s+"((?:[^"\\]|\\.)*)"\s*\]`)
	gitShaPattern     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// parseGitRemotes reads the fetch URL of every `[remote "name"]` section of a
// git config: its first `url` value (git fetches from the first). Credentials
// embedded in an http(s) URL are stripped.
func parseGitRemotes(config []byte) map[string]string {
	remotes := map[string]string{}
	current := ""
	sc := bufio.NewScanner(bytes.NewReader(config))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			current = ""
			if m := gitSectionPattern.FindStringSubmatch(line); m != nil {
				current = strings.ReplaceAll(strings.ReplaceAll(m[1], `\"`, `"`), `\\`, `\`)
			}
			continue
		}
		if current == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		if _, have := remotes[current]; have {
			continue
		}
		value = strings.TrimSpace(value)
		if i := indexUnquotedComment(value); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		remotes[current] = sanitizeRemoteURL(value)
	}
	return remotes
}

// indexUnquotedComment finds a trailing ` #` / ` ;` comment outside quotes.
func indexUnquotedComment(v string) int {
	inQuote := false
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"':
			inQuote = !inQuote
		case '#', ';':
			if !inQuote && i > 0 && (v[i-1] == ' ' || v[i-1] == '\t') {
				return i
			}
		}
	}
	return -1
}

// sanitizeRemoteURL drops userinfo from an http(s) remote (a token in
// `https://x-access-token:TOKEN@github.com/…` must never leave the device). The
// server matches remotes by host and path, so nothing it needs is lost. SSH
// forms (`git@host:owner/repo`) keep their user, which is not a secret.
func sanitizeRemoteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		u.User = nil
	default:
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.User(u.User.Username())
		}
	}
	return u.String()
}

// readGitHead resolves HEAD: a symbolic ref reports its branch and the sha it
// points at (loose ref in the git dir, then the common dir, then packed-refs);
// a detached HEAD reports the sha and an empty branch.
func readGitHead(gitDir, commonDir string) (sha, branch string) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	head := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(head, "ref:") {
		if gitShaPattern.MatchString(head) {
			return head, ""
		}
		return "", ""
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, "ref:"))
	branch = strings.TrimPrefix(ref, "refs/heads/")
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return "", branch
	}
	for _, dir := range []string{gitDir, commonDir} {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
			if s := strings.TrimSpace(string(b)); gitShaPattern.MatchString(s) {
				return s, branch
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(commonDir, "packed-refs")); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" || line[0] == '#' || line[0] == '^' {
				continue
			}
			s, name, found := strings.Cut(line, " ")
			if found && strings.TrimSpace(name) == ref && gitShaPattern.MatchString(s) {
				return s, branch
			}
		}
	}
	return "", branch // an unborn branch (no commits yet)
}

// gitHeadViaCLI asks git for HEAD — only for reftable repositories, whose refs
// are not plain files. Bounded; failures leave the values empty.
func gitHeadViaCLI(ctx context.Context, checkout, branch string) (string, string) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", branch
	}
	run := func(args ...string) string {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c := exec.CommandContext(cctx, "git", append([]string{"-C", checkout}, args...)...)
		hideWindow(c)
		c.Env = append(append([]string(nil), c.Environ()...), headlessEnvOverlay()...)
		out, err := c.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	sha := run("rev-parse", "--verify", "-q", "HEAD")
	if !gitShaPattern.MatchString(sha) {
		sha = ""
	}
	if b := run("symbolic-ref", "--short", "-q", "HEAD"); b != "" {
		branch = b
	}
	return sha, branch
}
