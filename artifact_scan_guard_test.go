// File: artifact_scan_guard_test.go
// Unit tests for artifactScanRootRefusal — the guard that keeps the media scan
// out of filesystem roots, the home folder and the folders above it — for how
// the command and session paths report a refused root, and for the bound on
// the one-shot command scan.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// artifactScanTestDeadline bounds every scan these tests run, so a guard that
// stopped refusing fails in seconds instead of walking a whole disk.
const artifactScanTestDeadline = 2 * time.Second

// useArtifactScanHome points the guard at home for the rest of the test.
func useArtifactScanHome(t *testing.T, home string) {
	t.Helper()
	original := artifactScanHomeDir
	artifactScanHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { artifactScanHomeDir = original })
}

// fakeArtifactScanHome builds a home folder of the test's own under a resolved
// temp folder and points the guard at it. Home holds a fresh screenshot of its
// own and one in a project inside it, so any scan of home or a folder above it
// that is not refused finds something. It returns the folder holding home,
// home itself and the project.
func fakeArtifactScanHome(t *testing.T, start time.Time) (parent, home, project string) {
	t.Helper()
	parent = tempRootDir(t)
	home = filepath.Join(parent, "home")
	project = filepath.Join(home, "project")
	fresh := start.Add(freshOffset)
	writeFileAt(t, filepath.Join(home, "Desktop", "desktop-shot.png"), "x", fresh)
	writeFileAt(t, filepath.Join(project, "test-results", "shot.png"), "x", fresh)
	useArtifactScanHome(t, home)
	return parent, home, project
}

// detectWithDeadline runs the scan under artifactScanTestDeadline.
func detectWithDeadline(t *testing.T, root string, start time.Time) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), artifactScanTestDeadline)
	defer cancel()
	return detectOutputFilesSinceCtx(ctx, root, start)
}

// sameDirOnDisk reports whether a and b are the same existing directory.
func sameDirOnDisk(a, b string) bool {
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(ai, bi)
}

type artifactScanRefusalCase struct {
	name, root, want string
}

// Home, every folder above it and the filesystem root are refused under each
// spelling, and the scan walks none of them. Each case asks the guard first, so
// a guard that stopped refusing fails before anything is walked.
func TestArtifactScanRootRefusal_RefusesHomeAndAbove(t *testing.T) {
	start := time.Now()
	parent, home, _ := fakeArtifactScanHome(t, start)
	sep := string(filepath.Separator)

	cases := []artifactScanRefusalCase{
		{"home", home, artifactScanRefusedHome},
		{"home with a trailing separator", home + sep, artifactScanRefusedHome},
		{"home through a dot segment", filepath.Join(home, "project") + sep + "..", artifactScanRefusedHome},
		{"the folder holding home", parent, artifactScanRefusedHomeAncestor},
		{"the volume root under home", filepath.VolumeName(home) + sep, artifactScanRefusedFilesystemRoot},
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// Both ignore case by default, so this IS home.
		cases = append(cases, artifactScanRefusalCase{"home with its case changed", filepath.Join(parent, "HOME"), artifactScanRefusedHome})
	}
	if runtime.GOOS != "windows" {
		// Windows' junction is pinned in artifact_scan_guard_windows_test.go.
		link := filepath.Join(parent, "home-link")
		makeAbsDirLink(t, link, home)
		cases = append(cases, artifactScanRefusalCase{"a symlink to home", link, artifactScanRefusedHome})
	}
	if runtime.GOOS == "darwin" {
		// The data volume's own spelling of the same folder. EvalSymlinks does
		// not resolve a firmlink, so only the identity check sees it.
		if twin := "/System/Volumes/Data" + home; sameDirOnDisk(twin, home) {
			cases = append(cases, artifactScanRefusalCase{"home through the data-volume firmlink", twin, artifactScanRefusedHome})
		}
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

// Everything inside home is scanned as before — that is where projects,
// worktrees and (on Windows) the temp folder live — and so is a folder beside
// home, which does not contain it.
func TestArtifactScanRootRefusal_ScansInsideAndBesideHome(t *testing.T) {
	start := time.Now()
	parent, _, project := fakeArtifactScanHome(t, start)
	beside := filepath.Join(parent, "next-to-home")
	if err := os.MkdirAll(beside, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, root := range []string{project, filepath.Join(project, "test-results"), beside} {
		if got := artifactScanRootRefusal(root); got != "" {
			t.Fatalf("artifactScanRootRefusal(%q) = %q; only home and the folders above it are refused", root, got)
		}
	}
	got := baseNames(detectWithDeadline(t, project, start))
	if len(got) != 1 || got[0] != "shot.png" {
		t.Fatalf("a fresh screenshot in a project inside home must still be found, got %v", got)
	}
}

// The seam is the only home the guard reads: with it set, HOME and USERPROFILE
// do not matter.
func TestArtifactScanRootRefusal_HomeComesFromTheSeam(t *testing.T) {
	parent, home, _ := fakeArtifactScanHome(t, time.Now())
	elsewhere := filepath.Join(parent, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", elsewhere)
	t.Setenv("USERPROFILE", elsewhere)

	if got := artifactScanRootRefusal(elsewhere); got != "" {
		t.Fatalf("artifactScanRootRefusal(%q) = %q; the guard read the environment instead of the seam", elsewhere, got)
	}
	if got := artifactScanRootRefusal(home); got != artifactScanRefusedHome {
		t.Fatalf("artifactScanRootRefusal(%q) = %q, want %q", home, got, artifactScanRefusedHome)
	}
}

// Production leaves the seam at its default, the user's real home folder
// (USERPROFILE on Windows, HOME elsewhere).
func TestArtifactScanRootRefusal_DefaultsToTheUserHome(t *testing.T) {
	profile := filepath.Join(tempRootDir(t), "profile")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", profile)
	t.Setenv("USERPROFILE", profile)

	if got := artifactScanRootRefusal(profile); got != artifactScanRefusedHome {
		t.Fatalf("with the default home lookup, artifactScanRootRefusal(%q) = %q, want %q", profile, got, artifactScanRefusedHome)
	}
}

// Without a home folder only the filesystem-root check is left; nothing else
// is refused on a guess.
func TestArtifactScanRootRefusal_UnknownHomeLeavesTheRootCheck(t *testing.T) {
	original := artifactScanHomeDir
	artifactScanHomeDir = func() (string, error) { return "", errors.New("no home folder") }
	t.Cleanup(func() { artifactScanHomeDir = original })

	dir := tempRootDir(t)
	if got := artifactScanRootRefusal(dir); got != "" {
		t.Fatalf("artifactScanRootRefusal(%q) = %q with no home folder, want \"\"", dir, got)
	}
	// "/" and "/." are the current drive's root on Windows, where "//" is a UNC
	// path with no server rather than a root.
	roots := []string{"/", "/."}
	if runtime.GOOS != "windows" {
		roots = append(roots, "//")
	}
	for _, root := range roots {
		if got := artifactScanRootRefusal(root); got != artifactScanRefusedFilesystemRoot {
			t.Fatalf("artifactScanRootRefusal(%q) = %q, want %q", root, got, artifactScanRefusedFilesystemRoot)
		}
	}
}

// The session path says it skipped a refused root, against its own id, instead
// of printing "scanned … → 0", which would report a scan that never ran.
func TestCollectSessionArtifacts_RefusedRootIsReportedAsSkipped(t *testing.T) {
	home := filepath.Join(tempRootDir(t), "home")
	// No media here, so even a guard that stopped refusing never reaches
	// GetStorageClient: these tests have no GCS credentials, and a developer's
	// Application Default Credentials would make a real call.
	writeFileAt(t, filepath.Join(home, "notes.txt"), "not media", time.Now())
	useArtifactScanHome(t, home)
	cfg := &Config{EnableFileUpload: true, StorageBucket: "b"}
	ctx, cancel := context.WithTimeout(context.Background(), artifactScanTestDeadline)
	defer cancel()

	var files []FileInfo
	var errs []UploadError
	out := captureStdout(t, func() {
		files, errs = collectSessionArtifacts(ctx, cfg, "s-home", "ws-1", home, time.Now().Add(-time.Minute))
	})

	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors for a refused root, got %v / %v", files, errs)
	}
	if want := "Session s-home: skipping media scan of " + home + ": it is the home folder"; !strings.Contains(out, want) {
		t.Fatalf("the log does not report the skip; want %q in:\n%s", want, out)
	}
	if strings.Contains(out, "scanned") {
		t.Fatalf("the log reports a refused root as scanned:\n%s", out)
	}
}

// The one-shot command path does the same against the command's id.
func TestScanCommandArtifacts_RefusedRootIsReportedAsSkipped(t *testing.T) {
	start := time.Now()
	_, home, _ := fakeArtifactScanHome(t, start)

	var files []string
	out := captureStdout(t, func() {
		files = scanCommandArtifacts("cmd-home", home, start, artifactScanTestDeadline)
	})

	if len(files) != 0 {
		t.Fatalf("a command run in the home folder returned %v for upload", files)
	}
	if want := "Command cmd-home: skipping media scan of " + home + ": it is the home folder"; !strings.Contains(out, want) {
		t.Fatalf("the log does not report the skip; want %q in:\n%s", want, out)
	}
	if strings.Contains(out, "scanned") {
		t.Fatalf("the log reports a refused root as scanned:\n%s", out)
	}
}

// The one-shot command scan's bound reaches the walk. It used to run on a
// Background context with no limit, so nothing could stop it. Within its bound
// it finds the screenshot; with a bound that has already passed, the walk stops
// at its first entry with nothing collected, and the log says the scan did not
// finish instead of "scanned … → 0", which would report a walk the bound
// stopped as one that found nothing. A walk past an expired bound never gets as
// far as a file, so dropping files a cut-short walk had already found is pinned
// by TestScanCommandArtifacts_CutShortScanUploadsNothing.
func TestScanCommandArtifacts_ExpiredBoundStopsTheWalk(t *testing.T) {
	start := time.Now()
	_, _, project := fakeArtifactScanHome(t, start)

	var files []string
	out := captureStdout(t, func() {
		files = scanCommandArtifacts("cmd-in-time", project, start, artifactScanTestDeadline)
	})
	if got := baseNames(files); len(got) != 1 || got[0] != "shot.png" {
		t.Fatalf("within its bound the scan must find the screenshot, got %v", got)
	}
	if want := "Command cmd-in-time: scanned " + project + " since "; !strings.Contains(out, want) {
		t.Fatalf("the log does not report the finished scan; want %q in:\n%s", want, out)
	}

	// A bound of zero has passed before the walk starts.
	out = captureStdout(t, func() {
		files = scanCommandArtifacts("cmd-out-of-time", project, start, 0)
	})
	if len(files) != 0 {
		t.Fatalf("a scan past its bound returned %v for upload", files)
	}
	if want := "Command cmd-out-of-time: media scan of " + project + " did not finish within 0s (0 file(s) found so far, none uploaded)"; !strings.Contains(out, want) {
		t.Fatalf("the log does not report the unfinished scan; want %q in:\n%s", want, out)
	}
	if strings.Contains(out, "Command cmd-out-of-time: scanned") {
		t.Fatalf("the log reports a scan the bound stopped as finished:\n%s", out)
	}
}

// A walk the bound cut short may already have found files. They are a partial
// list, so none of them is uploaded, and the log says the scan did not finish
// and how many it had found. A real walk cannot be made to find files and then
// outrun its bound on cue, so the walk is stood in: it finds two files, waits
// until the bound has passed, and returns them.
func TestScanCommandArtifacts_CutShortScanUploadsNothing(t *testing.T) {
	dir := tempRootDir(t)
	start := time.Now()
	const bound = time.Millisecond

	original := scanOutputFilesSinceFn
	scanOutputFilesSinceFn = func(ctx context.Context, workDir string, _ time.Time) ([]string, string) {
		found := []string{filepath.Join(workDir, "early.png"), filepath.Join(workDir, "shot.png")}
		select {
		case <-ctx.Done():
		case <-time.After(artifactScanTestDeadline):
			// Not t.Fatal: this runs inside captureStdout, which must get to
			// put os.Stdout back.
			t.Errorf("the scan's context was still live %s after its %s bound; the bound does not reach the walk",
				artifactScanTestDeadline, bound)
		}
		return found, ""
	}
	t.Cleanup(func() { scanOutputFilesSinceFn = original })

	var files []string
	out := captureStdout(t, func() {
		files = scanCommandArtifacts("cmd-cut-short", dir, start, bound)
	})

	if files != nil {
		t.Fatalf("a scan the bound cut short returned %v for upload", files)
	}
	if want := "Command cmd-cut-short: media scan of " + dir + " did not finish within " + bound.String() + " (2 file(s) found so far, none uploaded)"; !strings.Contains(out, want) {
		t.Fatalf("the log does not report the cut-short scan; want %q in:\n%s", want, out)
	}
	if strings.Contains(out, "Command cmd-cut-short: scanned") {
		t.Fatalf("the log reports a scan the bound cut short as finished:\n%s", out)
	}
}
