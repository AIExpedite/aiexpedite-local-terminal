// File: worktree_path_policy.go
// -----------------------------------------------------------------------------
// The path policy every worktree built-in passes BEFORE it reads, writes or
// deletes anything (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §4.2, §9.2).
//
// A worktree path is only ever `aixwt/<slug>-<key>/<repoFolder>` relative to the
// root the SERVER named on the dispatch, and it must sit under that root on
// disk. Three layers, because each catches what the others cannot:
//
//  1. the PATTERN — identical, character for character, to shared-constants
//     WORKTREE_RELATIVE_PATH_PATTERN. Written without a lookahead on purpose:
//     Go's RE2 engine has none, so anything unrepresentable here would drift
//     into two different rules on the two sides;
//  2. per-rule DIAGNOSTICS, so a refusal names the rule that failed rather than
//     saying "did not match". A test pins diagnostics and pattern to the same
//     verdict over a corpus, so they cannot disagree;
//  3. the LINK check. The pattern is lexical, so it cannot see that
//     `aixwt/<folder>` is itself a junction into a sibling checkout. Every
//     component is lstat'd, and the deepest existing ancestor's real path must
//     still resolve under the root's real path.
//
// Why the repository segment is not a plain character class: `[A-Za-z0-9._-]+`
// admits `.` and `..`, and `aixwt/<folder>/..` would then satisfy the boundary
// while naming the SHARED `aixwt` directory — one registry row aimed at every
// worktree on the computer instead of one repository. A trailing dot is the same
// class, because Windows strips it, so `repo.` and `repo` are one directory
// under two distinct registry paths.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// worktreeRootSegment mirrors shared-constants WORKTREE_ROOT_SEGMENT.
const worktreeRootSegment = "aixwt"

// worktreeRepoSegmentMaxLength is the longest repository folder the pattern
// admits (1 + 62 + 1). A FIXED bound, never derived from anything installed.
const worktreeRepoSegmentMaxLength = 64

// worktreeRelativePathPattern is shared-constants WORKTREE_RELATIVE_PATH_PATTERN
// verbatim. Keep the two literally identical: the server derives the path from
// it and this agent refuses to write anywhere that does not match.
var worktreeRelativePathPattern = regexp.MustCompile(
	`^aixwt/[a-z0-9-]{1,24}-[0-9a-f]{12}/[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9_-])?$`)

// worktreeFeatureFolderPattern is the `<slug>-<key>` half, split out for
// diagnostics only. The authority is worktreeRelativePathPattern.
var worktreeFeatureFolderPattern = regexp.MustCompile(`^[a-z0-9-]{1,24}-[0-9a-f]{12}$`)

// Policy rule names. They travel to the server in a refusal, so they are
// persisted vocabulary: renaming one is a contract change.
const (
	rulePathRootMissing       = "root_missing"
	rulePathRootNotAbsolute   = "root_not_absolute"
	rulePathMissing           = "path_missing"
	rulePathNotUnderRoot      = "path_not_under_root"
	rulePathSegmentCount      = "segment_count"
	rulePathRootSegment       = "root_segment"
	rulePathFeatureFolder     = "feature_folder"
	rulePathRepoEmpty         = "repo_segment_empty"
	rulePathRepoTraversal     = "repo_segment_traversal"
	rulePathRepoTrailingDot   = "repo_segment_trailing_dot"
	rulePathRepoLeadingChar   = "repo_segment_leading_char"
	rulePathRepoCharset       = "repo_segment_charset"
	rulePathRepoTooLong       = "repo_segment_too_long"
	rulePathPatternMismatch   = "pattern_mismatch"
	rulePathComponentIsLink   = "component_is_link"
	rulePathPathspecMagic     = "pathspec_magic"
	rulePathEscapesRootByLink = "escapes_root_by_link"
	rulePathRootUnreadable    = "root_unreadable"
)

// worktreePolicyError is a refusal naming the rule that failed. Every
// destructive built-in returns one unchanged, so the server and the Computers
// tab can say WHICH rule refused rather than "invalid path".
type worktreePolicyError struct {
	Rule   string
	Detail string
}

func (e *worktreePolicyError) Error() string {
	if e.Detail == "" {
		return "path policy refused: " + e.Rule
	}
	return fmt.Sprintf("path policy refused (%s): %s", e.Rule, e.Detail)
}

func policyRefusal(rule, detail string) *worktreePolicyError {
	return &worktreePolicyError{Rule: rule, Detail: detail}
}

// worktreePolicyRuleOf returns the rule a refusal names, or "" for any other
// error. Callers report it without having to type-switch.
func worktreePolicyRuleOf(err error) string {
	for err != nil {
		if pe, ok := err.(*worktreePolicyError); ok {
			return pe.Rule
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		err = u.Unwrap()
	}
	return ""
}

// resolvedWorktreePath is a path that passed every layer of the policy.
type resolvedWorktreePath struct {
	// Root is the cleaned absolute root the server named.
	Root string
	// Abs is the cleaned absolute worktree path.
	Abs string
	// Rel is the slash-separated path relative to Root (what the pattern matched).
	Rel string
	// FeatureFolder and RepoFolder are Rel's second and third segments.
	FeatureFolder string
	RepoFolder    string
	// Exists is whether Abs existed when the policy ran.
	Exists bool
}

// diagnoseWorktreeRelativePath names the first rule a candidate relative path
// breaks, or returns nil. Purely lexical and purely for the MESSAGE: the
// verdict must agree with worktreeRelativePathPattern, which
// TestWorktreeRelativePathDiagnosticsMatchPattern pins over a corpus.
func diagnoseWorktreeRelativePath(rel string) *worktreePolicyError {
	if rel == "" {
		return policyRefusal(rulePathMissing, "empty relative path")
	}
	segments := strings.Split(rel, "/")
	if len(segments) != 3 {
		return policyRefusal(rulePathSegmentCount,
			fmt.Sprintf("want aixwt/<feature>/<repo> (3 segments), got %d in %q", len(segments), rel))
	}
	if segments[0] != worktreeRootSegment {
		return policyRefusal(rulePathRootSegment,
			fmt.Sprintf("first segment must be %q, got %q", worktreeRootSegment, segments[0]))
	}
	if !worktreeFeatureFolderPattern.MatchString(segments[1]) {
		return policyRefusal(rulePathFeatureFolder,
			fmt.Sprintf("feature folder must be <=24 chars of [a-z0-9-] then -<12 hex>, got %q", segments[1]))
	}
	return diagnoseWorktreeRepoSegment(segments[2])
}

// diagnoseWorktreeRepoSegment names the rule a repository folder breaks. The
// order matters for the message only; the verdict is the pattern's.
func diagnoseWorktreeRepoSegment(seg string) *worktreePolicyError {
	if seg == "" {
		return policyRefusal(rulePathRepoEmpty, "repository segment is empty")
	}
	// `.`, `..`, `...`: an all-dot segment names the feature folder or the
	// shared aixwt directory, never a repository.
	if strings.Trim(seg, ".") == "" {
		return policyRefusal(rulePathRepoTraversal,
			fmt.Sprintf("repository segment %q names a directory above the repository", seg))
	}
	if len(seg) > worktreeRepoSegmentMaxLength {
		return policyRefusal(rulePathRepoTooLong,
			fmt.Sprintf("repository segment is %d characters, max %d", len(seg), worktreeRepoSegmentMaxLength))
	}
	for i := 0; i < len(seg); i++ {
		if !isWorktreeRepoChar(seg[i]) {
			return policyRefusal(rulePathRepoCharset,
				fmt.Sprintf("repository segment %q holds a character outside [A-Za-z0-9._-]", seg))
		}
	}
	if !isWorktreeAlnum(seg[0]) {
		return policyRefusal(rulePathRepoLeadingChar,
			fmt.Sprintf("repository segment %q must start with a letter or digit", seg))
	}
	last := seg[len(seg)-1]
	if last == '.' {
		return policyRefusal(rulePathRepoTrailingDot,
			fmt.Sprintf("repository segment %q ends in a dot; Windows strips it, so two registry paths would name one directory", seg))
	}
	if !isWorktreeAlnum(last) && last != '_' && last != '-' {
		return policyRefusal(rulePathRepoCharset,
			fmt.Sprintf("repository segment %q must end with [A-Za-z0-9_-]", seg))
	}
	return nil
}

func isWorktreeAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isWorktreeRepoChar(c byte) bool {
	return isWorktreeAlnum(c) || c == '.' || c == '_' || c == '-'
}

// resolveWorktreePath is THE gate. Every built-in that reads, writes or deletes
// a worktree calls it first and uses only the paths it returns.
//
// root must be the absolute root recorded on the registry entry (never the
// current Default working directory setting — §4.5 "the recorded root wins").
// path may be absolute or relative to root; either way the CLEANED result has
// to be the three-segment pattern, which is what makes `..` unrepresentable.
func resolveWorktreePath(root, path string) (resolvedWorktreePath, error) {
	var out resolvedWorktreePath
	root = strings.TrimSpace(root)
	if root == "" {
		return out, policyRefusal(rulePathRootMissing, "the dispatch named no root")
	}
	if !filepath.IsAbs(root) {
		return out, policyRefusal(rulePathRootNotAbsolute, fmt.Sprintf("root %q is not absolute", root))
	}
	root = filepath.Clean(root)

	path = strings.TrimSpace(path)
	if path == "" {
		return out, policyRefusal(rulePathMissing, "the dispatch named no path")
	}

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)

	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return out, policyRefusal(rulePathNotUnderRoot,
			fmt.Sprintf("%q is not expressible under root %q", abs, root))
	}
	relSlash := filepath.ToSlash(rel)
	if relSlash == "." || relSlash == ".." || strings.HasPrefix(relSlash, "../") {
		return out, policyRefusal(rulePathNotUnderRoot,
			fmt.Sprintf("%q resolves to %q, outside root %q", path, relSlash, root))
	}

	if d := diagnoseWorktreeRelativePath(relSlash); d != nil {
		return out, d
	}
	// Belt and braces: the shared pattern is the authority. Reaching here with
	// a non-match would mean the diagnostics drifted from it, which is a bug in
	// this file rather than a bad dispatch — so it refuses loudly.
	if !worktreeRelativePathPattern.MatchString(relSlash) {
		return out, policyRefusal(rulePathPatternMismatch,
			fmt.Sprintf("%q passed the per-rule checks but not WORKTREE_RELATIVE_PATH_PATTERN", relSlash))
	}

	segments := strings.Split(relSlash, "/")
	out = resolvedWorktreePath{
		Root:          root,
		Abs:           abs,
		Rel:           relSlash,
		FeatureFolder: segments[1],
		RepoFolder:    segments[2],
	}
	if err := checkWorktreePathLinkFree(root, segments); err != nil {
		return resolvedWorktreePath{}, err
	}
	if _, statErr := os.Lstat(abs); statErr == nil {
		out.Exists = true
	}
	return out, nil
}

// checkWorktreePathLinkFree refuses when any component between the root and the
// worktree is a symlink or a Windows junction, and when the deepest existing
// component's real path leaves the root.
//
// The pattern cannot see this: `aixwt` or `aixwt/<folder>` can be a junction
// into a sibling checkout, and a lexically perfect path would then name
// somebody else's tree. A dependency directory is exactly where such a junction
// lives — following one emptied `local/node_modules` and two whole sibling
// repositories on 2026-09-17.
func checkWorktreePathLinkFree(root string, segments []string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// A root that cannot be resolved is not a root we delete under.
		return policyRefusal(rulePathRootUnreadable,
			fmt.Sprintf("root %q cannot be resolved: %v", root, err))
	}
	cur := root
	deepest := realRoot
	for _, seg := range segments {
		cur = filepath.Join(cur, seg)
		info, statErr := os.Lstat(cur)
		if statErr != nil {
			break // not created yet: nothing below it can be a link either
		}
		if isWorktreeLink(info, cur) {
			return policyRefusal(rulePathComponentIsLink,
				fmt.Sprintf("%q is a symlink or junction; the policy never writes or deletes through one", cur))
		}
		if resolved, rerr := filepath.EvalSymlinks(cur); rerr == nil {
			deepest = resolved
		}
	}
	if !isPathWithin(realRoot, deepest) {
		return policyRefusal(rulePathEscapesRootByLink,
			fmt.Sprintf("%q resolves to %q, outside root %q", cur, deepest, realRoot))
	}
	return nil
}

// isWorktreeLink reports whether an lstat'd path is a symlink or, on Windows, a
// junction. Since Go 1.23 a junction is not reported as ModeSymlink, so
// os.Readlink decides — the same rule isLinkEntry uses for npm's workspace
// links.
func isWorktreeLink(info os.FileInfo, full string) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if runtime.GOOS == "windows" && (info.IsDir() || info.Mode()&os.ModeIrregular != 0) {
		if _, err := os.Readlink(full); err == nil {
			return true
		}
	}
	return false
}

// isPathWithin reports whether candidate is base or sits under it, comparing
// case-insensitively where the file system is.
func isPathWithin(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		rel = strings.ToLower(rel)
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../"))
}

// refuseLinkedComponents walks rel's directory components under base and
// refuses if any of them is a symlink or junction. Used where a path is READ
// through rather than resolved by resolveInsideWorktree, which walks only the
// worktree side.
//
// The final component is included: a linked file is as unwelcome as a linked
// directory above it, and a caller that wants to allow one can lstat it itself.
func refuseLinkedComponents(base, rel string) error {
	segments := strings.FieldsFunc(filepath.ToSlash(rel), func(r rune) bool { return r == '/' })
	cur := filepath.Clean(base)
	for _, seg := range segments {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			// Absent is not unsafe. The caller's own lstat reports it, with a
			// message about the file rather than about links.
			return nil
		}
		if isWorktreeLink(info, cur) {
			return policyRefusal(rulePathComponentIsLink,
				fmt.Sprintf("%s is a symlink or junction, so %s is not read through it", cur, rel))
		}
	}
	return nil
}

// resolvedCandidate is one path inside a worktree, resolved ONCE into the two
// forms its checks need: Abs for everything that touches the filesystem
// (lstat, baseline scan, digest, delete) and GitPath for everything asked of
// git (ls-files, status, check-ignore).
//
// They are returned together, and built from the same validated segments,
// because a proof is only a proof of the thing being deleted if both halves
// denote the same directory. Taking the git half straight from the server's
// string instead let the two drift: git reads its argument as a PATHSPEC, so
// `:(glob)x` or `node_modules/*` asks about a different set of paths than the
// one the delete targets, and `git ls-files -- <that>` coming back empty would
// read as "holds no tracked file".
type resolvedCandidate struct {
	Abs     string
	GitPath string
}

// resolveInsideWorktree validates a path the server named INSIDE a worktree —
// a dependency or build directory for eviction. Relative, no `..`, no
// component that is a link, and it must stay under the worktree.
//
// Separate from resolveWorktreePath because the shapes differ: a dependency
// directory may be nested (`packages/web/node_modules`), so it has no fixed
// segment count. What it shares is that nothing may traverse upward and nothing
// may be reached through a link.
func resolveInsideWorktree(wt resolvedWorktreePath, rel string) (resolvedCandidate, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return resolvedCandidate{}, policyRefusal(rulePathMissing, "no directory named inside the worktree")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return resolvedCandidate{}, policyRefusal(rulePathNotUnderRoot,
			fmt.Sprintf("%q must be relative to the worktree", rel))
	}
	segments := strings.FieldsFunc(filepath.ToSlash(rel), func(r rune) bool { return r == '/' })
	if len(segments) == 0 {
		return resolvedCandidate{}, policyRefusal(rulePathMissing, "no directory named inside the worktree")
	}
	for _, seg := range segments {
		if strings.Trim(seg, ".") == "" {
			return resolvedCandidate{}, policyRefusal(rulePathRepoTraversal,
				fmt.Sprintf("%q holds the segment %q, which names a directory above it", rel, seg))
		}
		// A segment has to be a plain NAME, because the same segments become a
		// git pathspec below. A leading `:` opens pathspec magic (`:(glob)`,
		// `:!`, `:(icase)`), `*?[]` are wildcards, and `\` escapes inside git's
		// glob matching — each one would make git answer about something other
		// than the directory being deleted.
		//
		// Refusing costs nothing real: Windows forbids `*?:\` in a filename
		// outright, and no dependency or build directory on any platform is
		// named this way. A dispatch carrying one is malformed, so it is
		// refused loudly rather than normalised into something plausible.
		if strings.HasPrefix(seg, ":") || strings.ContainsAny(seg, "*?[]\\") {
			return resolvedCandidate{}, policyRefusal(rulePathPathspecMagic,
				fmt.Sprintf("%q holds the segment %q, which is a git pattern rather than a directory name", rel, seg))
		}
	}
	abs := filepath.Clean(filepath.Join(wt.Abs, filepath.Join(segments...)))
	if !isPathWithin(wt.Abs, abs) || abs == wt.Abs {
		return resolvedCandidate{}, policyRefusal(rulePathNotUnderRoot,
			fmt.Sprintf("%q resolves to %q, which is not inside the worktree", rel, abs))
	}
	cur := wt.Abs
	for _, seg := range segments {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			break
		}
		if isWorktreeLink(info, cur) {
			return resolvedCandidate{}, policyRefusal(rulePathComponentIsLink,
				fmt.Sprintf("%q is a symlink or junction; eviction never deletes through one", cur))
		}
	}
	// One list of segments, two forms. GitPath is slash-joined regardless of
	// platform because a pathspec always uses `/`, while Abs went through
	// filepath.Join and carries the OS separator.
	return resolvedCandidate{Abs: abs, GitPath: strings.Join(segments, "/")}, nil
}
