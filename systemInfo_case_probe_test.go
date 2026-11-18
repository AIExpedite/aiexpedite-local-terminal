package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSwapLetterCase(t *testing.T) {
	for in, want := range map[string]string{"Daniel": "dANIEL", "repo-2": "REPO-2", "123": "123", "": ""} {
		if got := swapLetterCase(in); got != want {
			t.Errorf("swapLetterCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeFolderCaseInsensitive(t *testing.T) {
	base := resolvedTempDir(t)
	dir := filepath.Join(base, "CaseProbe")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("matches the volume's own answer", func(t *testing.T) {
		got := probeFolderCaseInsensitive(dir)
		if got == nil {
			t.Fatal("a readable folder with letters in its name must be conclusive")
		}
		_, err := os.Stat(filepath.Join(base, "cASEpROBE"))
		if *got != (err == nil) {
			t.Fatalf("probe = %v, but the swapped spelling stat err = %v", *got, err)
		}
		if runtime.GOOS == "windows" && !*got {
			t.Fatal("NTFS folds case")
		}
	})

	t.Run("case-sensitive: a distinct case-swapped sibling is not the same folder", func(t *testing.T) {
		other := filepath.Join(base, "cASEpROBE")
		if err := os.Mkdir(other, 0o755); err != nil {
			// The volume folds case, so the swapped name IS dir.
			t.Skipf("the temp volume folds case: %v", err)
		}
		got := probeFolderCaseInsensitive(dir)
		if got == nil || *got {
			t.Fatalf("probe = %v, want false on a case-sensitive volume", got)
		}
	})

	t.Run("inconclusive probes are omitted", func(t *testing.T) {
		noLetters := filepath.Join(base, "123")
		if err := os.Mkdir(noLetters, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"", "relative", filepath.Join(base, "missing"), noLetters} {
			if got := probeFolderCaseInsensitive(p); got != nil {
				t.Errorf("probeFolderCaseInsensitive(%q) = %v, want nil", p, *got)
			}
		}
	})
}

// The fact is omitted when unknown and sent when measured, false included:
// terminal-service reads an absent value as "unknown", never as false.
func TestFolderCaseInsensitiveWireShape(t *testing.T) {
	encode := func(v *bool) string {
		caps := newMachineCapabilities()
		caps.FolderCaseInsensitive = v
		out, err := json.Marshal(caps)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if strings.Contains(encode(nil), "folderCaseInsensitive") {
		t.Error("an inconclusive probe must omit folderCaseInsensitive")
	}
	f, tr := false, true
	if !strings.Contains(encode(&f), `"folderCaseInsensitive":false`) {
		t.Error("a measured false must be sent")
	}
	if !strings.Contains(encode(&tr), `"folderCaseInsensitive":true`) {
		t.Error("a measured true must be sent")
	}
}
