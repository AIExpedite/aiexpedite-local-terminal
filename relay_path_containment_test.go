package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// containmentTree is a root with nested folders and the neighbours an escape
// would aim at: a shared-prefix sibling and a folder outside.
type containmentTree struct {
	base, root, nested, sibling, outside string
}

func newContainmentTree(t *testing.T) containmentTree {
	t.Helper()
	base := resolvedTempDir(t)
	tree := containmentTree{
		base:    base,
		root:    filepath.Join(base, "AIExpedite"),
		nested:  filepath.Join(base, "AIExpedite", "frontend", "src", "a", "b", "c"),
		sibling: filepath.Join(base, "AIExpedite2"),
		outside: filepath.Join(base, "other"),
	}
	for _, d := range []string{tree.nested, tree.sibling, tree.outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return tree
}

// caseVariant is path with the case of its last component swapped.
func caseVariant(path string) string {
	return filepath.Join(filepath.Dir(path), swapLetterCase(filepath.Base(path)))
}

func TestDirWithin(t *testing.T) {
	tree := newContainmentTree(t)
	file := filepath.Join(tree.root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name            string
		root, candidate string
		want            bool
	}{
		{"root itself", tree.root, tree.root, true},
		{"root with a trailing separator", tree.root + string(filepath.Separator), tree.root, true},
		{"one level down", tree.root, filepath.Join(tree.root, "frontend"), true},
		{"five levels down", tree.root, tree.nested, true},
		{"sibling with a shared prefix", tree.root, tree.sibling, false},
		{"child of the shared-prefix sibling", tree.sibling, tree.nested, false},
		{"outside folder", tree.root, tree.outside, false},
		{"parent of the root", tree.root, tree.base, false},
		{"traversal out of the root", tree.root, filepath.Join(tree.root, "..", "other"), false},
		{"traversal that stays inside", tree.root, filepath.Join(tree.nested, "..", ".."), true},
		{"missing candidate", tree.root, filepath.Join(tree.root, "missing"), false},
		{"missing root", filepath.Join(tree.base, "missing"), tree.nested, false},
		{"a file is not a folder", tree.root, file, false},
		{"relative candidate", tree.root, "frontend", false},
		{"empty candidate", tree.root, "", false},
		{"empty root", "", tree.root, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dirWithin(c.root, c.candidate); got != c.want {
				t.Fatalf("dirWithin(%q, %q) = %v, want %v", c.root, c.candidate, got, c.want)
			}
		})
	}
}

func TestDirWithin_LinksResolveToWhereTheyPoint(t *testing.T) {
	tree := newContainmentTree(t)
	inside := filepath.Join(tree.root, "link-inside")
	makeAbsDirLink(t, inside, tree.nested)
	if !dirWithin(tree.root, inside) {
		t.Errorf("a link inside the root pointing inside it must be within")
	}
	escape := filepath.Join(tree.root, "link-outside")
	makeAbsDirLink(t, escape, tree.outside)
	if dirWithin(tree.root, escape) {
		t.Errorf("a link inside the root pointing outside it must not be within")
	}
	if dirWithin(tree.root, filepath.Join(escape, ".")) {
		t.Errorf("a path through the escaping link must not be within")
	}
	// A root reached through a link is the folder it points at.
	rootLink := filepath.Join(tree.base, "root-link")
	makeAbsDirLink(t, rootLink, tree.root)
	if !dirWithin(rootLink, tree.nested) {
		t.Errorf("a root spelled through a link must contain its real sub-folders")
	}
}

func TestDirWithin_WindowsJunctionEscapeRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions exist only on Windows")
	}
	tree := newContainmentTree(t)
	junction := filepath.Join(tree.root, "junction-outside")
	makeAbsDirLink(t, junction, tree.outside)
	if dirWithin(tree.root, junction) {
		t.Fatalf("a junction inside the root pointing outside must not be within")
	}
	inside := filepath.Join(tree.root, "junction-inside")
	makeAbsDirLink(t, inside, tree.nested)
	if !dirWithin(tree.root, inside) {
		t.Fatalf("a junction inside the root pointing inside it must be within")
	}
}

func TestDirWithin_WindowsOtherDriveRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters exist only on Windows")
	}
	tree := newContainmentTree(t)
	other := `Z:\` + filepath.Base(tree.root)
	if strings.EqualFold(filepath.VolumeName(tree.root), "Z:") {
		other = `Y:\` + filepath.Base(tree.root)
	}
	if dirWithin(tree.root, other) || dirWithin(other, tree.root) {
		t.Fatalf("a folder on another drive is never within")
	}
}

// A case variant of the root is the root exactly where the volume folds case:
// Windows and a default (case-insensitive) APFS volume admit it, a
// case-sensitive volume (most Linux runners) refuses it.
func TestDirWithin_CaseVariantFollowsTheVolume(t *testing.T) {
	tree := newContainmentTree(t)
	insensitive := probeFolderCaseInsensitive(tree.root)
	if insensitive == nil {
		t.Skip("the temp volume's case behaviour could not be probed")
	}
	variantRoot := caseVariant(tree.root)
	variantNested := filepath.Join(variantRoot, "frontend")
	if got := dirWithin(tree.root, variantNested); got != *insensitive {
		t.Fatalf("dirWithin(%q, %q) = %v on a volume with caseInsensitive=%v", tree.root, variantNested, got, *insensitive)
	}
	if got := dirWithin(variantRoot, tree.nested); got != *insensitive {
		t.Fatalf("dirWithin(%q, %q) = %v on a volume with caseInsensitive=%v", variantRoot, tree.nested, got, *insensitive)
	}
	// A case variant of a sibling is still a sibling.
	if dirWithin(tree.root, caseVariant(tree.sibling)) {
		t.Fatalf("a case variant of the shared-prefix sibling must never be within")
	}
}

func TestDirWithin_CaseVariantOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS volumes only: run on a Mac build")
	}
	tree := newContainmentTree(t)
	insensitive := probeFolderCaseInsensitive(tree.root)
	if insensitive == nil || !*insensitive {
		t.Skip("this Mac's temp volume is case-sensitive")
	}
	if !dirWithin(tree.root, filepath.Join(caseVariant(tree.root), "frontend")) {
		t.Fatalf("a case variant on a case-insensitive APFS volume must be within")
	}
}

func TestContainedCwd_UsesDirWithin(t *testing.T) {
	tree := newContainmentTree(t)
	if got, err := containedCwd(tree.nested, tree.root); err != nil || got != tree.nested {
		t.Fatalf("containedCwd(nested, root) = %q, %v; want %q", got, err, tree.nested)
	}
	if _, err := containedCwd(tree.sibling, tree.root); err == nil {
		t.Fatalf("containedCwd must refuse a shared-prefix sibling")
	}
	escape := filepath.Join(tree.root, "link-outside")
	makeAbsDirLink(t, escape, tree.outside)
	if _, err := containedCwd(escape, tree.root); err == nil {
		t.Fatalf("containedCwd must refuse a link that escapes the root")
	}
}

// Every start a voice grant admits carries the same fence, whatever kind of
// session it starts, so each is refused or admitted alike.
func TestRefuseCwdOutsideFence_EveryStartType(t *testing.T) {
	publishes := captureCwdFencePublishes(t)
	cfg := &Config{AgentID: "agent-fence-matrix"}
	tree := newContainmentTree(t)
	escape := filepath.Join(tree.root, "link-outside")
	makeAbsDirLink(t, escape, tree.outside)

	startTypes := []string{
		"session_start",
		"claude_native_start",
		"codex_appserver_start",
		"antigravity_native_start",
		"opencode_native_start",
		"musecode_native_start",
		"grok_acp_start",
		"antigravity_acp_start",
		"opencode_acp_start",
		"musecode_msp_start",
	}
	cases := []struct {
		name    string
		cwd     string
		refused bool
	}{
		{"root", tree.root, false},
		{"nested", tree.nested, false},
		{"sibling", tree.sibling, true},
		{"traversal", filepath.Join(tree.root, "..", "AIExpedite2"), true},
		{"link escape", escape, true},
		{"missing", filepath.Join(tree.root, "missing"), true},
	}
	want := 0
	for _, typ := range startTypes {
		for _, c := range cases {
			cmd := commandMsg{ID: "cmd-" + typ, Type: typ, SessionID: "sess", Cwd: c.cwd, CwdFence: tree.root}
			got := refuseCwdOutsideFence(context.Background(), nil, nil, cmd, cfg)
			if got != c.refused {
				t.Errorf("%s / %s: refused = %v, want %v", typ, c.name, got, c.refused)
			}
			if c.refused {
				want++
			}
		}
	}
	msgs := publishes()
	if len(msgs) != want {
		t.Fatalf("published %d refusals, want %d", len(msgs), want)
	}
	for _, m := range msgs {
		if m.Status != "denied" || m.RejectionReason != cwdOutsideGrantFolderRejectionCode {
			t.Errorf("unexpected refusal payload: %+v", m)
		}
	}
}

func TestResolvedPathWithin_NotYetCreatedFolders(t *testing.T) {
	tree := newContainmentTree(t)
	cases := []struct {
		name     string
		resolved string
		want     bool
	}{
		{"existing nested", tree.nested, true},
		{"not yet created inside", filepath.Join(tree.root, "new", "deeper"), true},
		{"not yet created in the shared-prefix sibling", filepath.Join(tree.sibling, "new"), false},
		{"not yet created outside", filepath.Join(tree.outside, "new"), false},
		{"relative", "new", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvedPathWithin(tree.root, c.resolved); got != c.want {
				t.Fatalf("resolvedPathWithin(%q, %q) = %v, want %v", tree.root, c.resolved, got, c.want)
			}
		})
	}
	escape := filepath.Join(tree.root, "link-outside")
	makeAbsDirLink(t, escape, tree.outside)
	resolved, err := resolveCwdForContainment(filepath.Join(escape, "new"))
	if err != nil {
		t.Fatal(err)
	}
	if resolvedPathWithin(tree.root, resolved) {
		t.Fatalf("a not-yet-created folder through an escaping link must not be within")
	}
}
