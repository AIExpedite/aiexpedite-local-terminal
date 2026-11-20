// File: systemInfo_case_probe.go
// -----------------------------------------------------------------------------
// The folderCaseInsensitive fact (capabilitiesInfo.FolderCaseInsensitive): does
// the volume holding a folder fold case? terminal-service compares a POSIX
// computer's voice-grant folders case-insensitively only on a computer that
// reports true. The filesystem answers it — a case-swapped spelling is the
// same directory, or it is not — never the OS name: macOS volumes can be
// either, and so can Linux ones.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// probeFolderCaseInsensitive stats dir under a case-swapped spelling of its
// last component (the lookup the volume holding that entry answers):
//
//   - the swapped spelling exists and os.SameFile says it IS dir → true;
//   - it does not exist, or it is a different entry → false;
//   - anything else (dir unreadable, no letter to swap, any other error) →
//     nil: inconclusive, so the fact is omitted rather than guessed.
func probeFolderCaseInsensitive(dir string) *bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	base := filepath.Base(dir)
	swappedBase := swapLetterCase(base)
	if swappedBase == base {
		return nil
	}
	swapped, err := os.Stat(filepath.Join(filepath.Dir(dir), swappedBase))
	switch {
	case err == nil:
		same := os.SameFile(info, swapped)
		return &same
	case errors.Is(err, fs.ErrNotExist):
		insensitive := false
		return &insensitive
	default:
		return nil
	}
}

// swapLetterCase is s with every letter's case swapped (`Daniel` → `dANIEL`);
// a name with no cased letter comes back unchanged.
func swapLetterCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsUpper(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsLower(r):
			b.WriteRune(unicode.ToUpper(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
