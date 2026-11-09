//go:build windows

// File: artifact_scan_guard_windows_test.go
// The Windows spellings artifactScanRootRefusal must see through — drive
// letters, \\?\ and UNC roots, junctions, 8.3 short names — and the temp
// folder under AppData that must still be scanned.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// flipDriveLetter returns path with its drive letter in the other case.
func flipDriveLetter(t *testing.T, path string) string {
	t.Helper()
	if len(path) < 2 || path[1] != ':' {
		t.Fatalf("%q does not start with a drive letter", path)
	}
	letter := path[:1]
	if upper := strings.ToUpper(letter); upper != letter {
		return upper + path[1:]
	}
	return strings.ToLower(letter) + path[1:]
}

// shortPathName returns Windows' 8.3 spelling of path, which is path itself
// when the volume keeps no short names.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	long, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		n, err := syscall.GetShortPathName(long, &buf[0], uint32(len(buf)))
		if err != nil {
			t.Fatalf("GetShortPathName(%q): %v", path, err)
		}
		if n < uint32(len(buf)) {
			return syscall.UTF16ToString(buf[:n])
		}
		buf = make([]uint16, n)
	}
}

// Every spelling Windows has for a filesystem root is refused from its text
// alone. Home is unknown here, so a root the text check missed would come back
// "" without a stat: a share on a server that does not exist costs no lookup.
func TestArtifactScanRootRefusal_WindowsRootSpellings(t *testing.T) {
	useArtifactScanHome(t, "")

	roots := []string{
		`C:\`, `c:\`, `C:/`, `C:\Users\..`,
		`\\server\share`, `\\server\share\`, `\\server\share\..`,
		`\\?\C:\`, `\\?\c:\`, `\??\C:\`, `//?/C:/`,
		// Go reads \\?\UNC as the VOLUME of these, so only the guard's own
		// verbatim-prefix handling makes them roots.
		`\\?\UNC\server\share`, `\\?\UNC\server\share\`, `\\?\unc\server\share`, `\??\UNC\server\share`,
		`\\.\C:\`, `\\.\UNC\server\share`,
		`\\?\Volume{00000000-0000-0000-0000-000000000000}\`,
	}
	for _, root := range roots {
		if got := artifactScanRootRefusal(root); got != artifactScanRefusedFilesystemRoot {
			t.Errorf("artifactScanRootRefusal(%q) = %q, want %q", root, got, artifactScanRefusedFilesystemRoot)
		}
	}
	// A folder on a share is a folder like any other.
	for _, dir := range []string{`\\server\share\dir`, `\\?\UNC\server\share\dir`} {
		if got := artifactScanRootRefusal(dir); got != "" {
			t.Errorf("artifactScanRootRefusal(%q) = %q, want \"\"", dir, got)
		}
	}
}

// Home under the spellings only Windows has. Text alone matches the drive
// letter and the \\?\ prefix; the junction needs the identity check, because
// EvalSymlinks has not followed junctions since Go 1.23.
func TestArtifactScanRootRefusal_WindowsHomeSpellings(t *testing.T) {
	start := time.Now()
	parent, home, _ := fakeArtifactScanHome(t, start)
	junction := filepath.Join(parent, "home-junction")
	makeAbsDirLink(t, junction, home)

	cases := []artifactScanRefusalCase{
		{"a drive letter in the other case", flipDriveLetter(t, home), artifactScanRefusedHome},
		{"the folder holding home, drive letter in the other case", flipDriveLetter(t, parent), artifactScanRefusedHomeAncestor},
		{`the \\?\ prefix`, `\\?\` + home, artifactScanRefusedHome},
		{"a junction to home", junction, artifactScanRefusedHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := artifactScanRootRefusal(tc.root); got != tc.want {
				t.Fatalf("artifactScanRootRefusal(%q) = %q, want %q", tc.root, got, tc.want)
			}
			if files := detectWithDeadline(t, tc.root, start); len(files) != 0 {
				t.Fatalf("the scan of %s returned %v; a refused root must not be walked", tc.root, files)
			}
		})
	}
}

// An 8.3 short name shares no text with the long one; only the identity check
// matches it. GitHub's runners hand out exactly such a spelling for TEMP
// (C:\Users\RUNNER~1\...).
func TestArtifactScanRootRefusal_WindowsShortName(t *testing.T) {
	start := time.Now()
	_, home, _ := fakeArtifactScanHome(t, start)
	short := shortPathName(t, home)
	if strings.EqualFold(short, home) {
		t.Skipf("the volume keeps no 8.3 short name on the way to %s", home)
	}

	if got := artifactScanRootRefusal(short); got != artifactScanRefusedHome {
		t.Fatalf("artifactScanRootRefusal(%q) = %q, want %q (long form %s)", short, got, artifactScanRefusedHome, home)
	}
	if files := detectWithDeadline(t, short, start); len(files) != 0 {
		t.Fatalf("the scan of %s returned %v; a refused root must not be walked", short, files)
	}
}

// The temp folder under AppData is INSIDE the home folder, and it is where
// %TEMP%, Go's t.TempDir and the agent's own temporary folders live, so it is
// still scanned, also under the 8.3 spelling Windows hands out for it
// (C:\Users\RUNNER~1\AppData\Local\Temp on GitHub's runners).
//
// The suite points USERPROFILE at its sandbox, which it creates in the REAL
// temp folder, so the real profile is the home to test against and the test's
// own temp folder is the descendant: nothing is written outside the sandbox.
func TestArtifactScanRootRefusal_WindowsTempUnderAppDataIsScanned(t *testing.T) {
	home := realHomeAtStartup
	if home == "" {
		t.Skip("the real home folder is unknown")
	}
	useArtifactScanHome(t, home)
	dir := tempRootDir(t)
	localTemp := filepath.Join(os.Getenv("LOCALAPPDATA"), "Temp")
	if os.Getenv("LOCALAPPDATA") == "" || !isPathWithin(localTemp, dir) || !isPathWithin(home, dir) {
		t.Skipf(`%s is not under %%LOCALAPPDATA%%\Temp inside the home folder %s on this computer`, dir, home)
	}
	start := time.Now()
	writeFileAt(t, filepath.Join(dir, "test-results", "shot.png"), "x", start.Add(freshOffset))

	spellings := []string{dir}
	if short := shortPathName(t, dir); !strings.EqualFold(short, dir) {
		spellings = append(spellings, short)
	}
	for _, root := range spellings {
		if got := artifactScanRootRefusal(root); got != "" {
			t.Fatalf("artifactScanRootRefusal(%q) = %q; a folder inside the home folder must be scanned", root, got)
		}
		if got := baseNames(detectWithDeadline(t, root, start)); len(got) != 1 || got[0] != "shot.png" {
			t.Fatalf(`a fresh screenshot under %%LOCALAPPDATA%%\Temp (%s) must still be found, got %v`, root, got)
		}
	}
}
