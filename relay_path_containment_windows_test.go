//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedLengthPath(t *testing.T) {
	long := strings.Repeat("a", longPathThreshold)
	cases := []struct {
		name, in, want string
	}{
		{"short local path unchanged", `C:\AIExpedite\frontend`, `C:\AIExpedite\frontend`},
		{"relative path unchanged", long + `\x`, long + `\x`},
		{"long local path prefixed", `C:\` + long, `\\?\C:\` + long},
		{"long local path cleaned", `C:/` + long + `/./b/../c//`, `\\?\C:\` + long + `\c`},
		{"long share uses UNC form", `\\server\share\` + long, `\\?\UNC\server\share\` + long},
		{"already prefixed unchanged", `\\?\C:\` + long, `\\?\C:\` + long},
		{"device path unchanged", `\\.\C:\` + long, `\\.\C:\` + long},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendedLengthPath(tc.in); got != tc.want {
				t.Fatalf("extendedLengthPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A folder deeper than MAX_PATH must still be contained in its root: os
// creates it through the extended-length prefix, and dirWithin must open it
// the same way rather than refuse a valid start.
func TestDirWithinLongPath(t *testing.T) {
	root := t.TempDir()
	deep := root
	for len(deep) < 300 {
		deep = filepath.Join(deep, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll(%d chars): %v", len(deep), err)
	}
	if !dirWithin(root, deep) {
		t.Fatalf("dirWithin(root, %d-char nested folder) = false, want true", len(deep))
	}
	if !dirWithin(deep, deep) {
		t.Fatalf("dirWithin(long folder, itself) = false, want true")
	}
	sibling := t.TempDir()
	if dirWithin(sibling, deep) {
		t.Fatalf("dirWithin(sibling, long folder) = true, want false")
	}
}
