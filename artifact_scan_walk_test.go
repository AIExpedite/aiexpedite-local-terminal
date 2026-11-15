package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oneFolderAtATimeArtifactWalk is the walk scanOutputFilesSince ran before it
// read several folders at once (filepath.WalkDir, the cap as a sentinel),
// kept verbatim as the reference the parallel walk must agree with.
func oneFolderAtATimeArtifactWalk(ctx context.Context, absBase string, cutoff time.Time) []string {
	files := []string{}
	errLimit := errors.New("file upload limit reached")
	_ = filepath.WalkDir(absBase, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Type()&^os.ModeDir != 0 {
				return fs.SkipDir
			}
			if path != absBase {
				if _, skip := ignoredDirs[d.Name()]; skip {
					return fs.SkipDir
				}
				name := d.Name()
				if strings.HasPrefix(name, ".") && name != ".maestro" {
					return fs.SkipDir
				}
			}
			return nil
		}
		if len(files) >= maxUploadFiles {
			return errLimit
		}
		if d.Type() != 0 {
			return nil
		}
		if _, ok := mediaExtensions[strings.ToLower(filepath.Ext(path))]; !ok {
			return nil
		}
		baseLower := strings.ToLower(filepath.Base(path))
		for _, prefix := range sensitiveBasenamePrefixes {
			if strings.HasPrefix(baseLower, prefix) {
				return nil
			}
		}
		if !isPathSafeUnder(path, absBase) {
			return nil
		}
		if !cutoff.IsZero() {
			info, infoErr := d.Info()
			if infoErr != nil || info.ModTime().Before(cutoff) {
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	return files
}

// artifactWalkTree builds a session folder that exercises every rule of the
// walk and its order: names that sort around a subfolder of the same stem
// ("a", "a-c.png", "a.b", "a.png"), upper case before lower, deep nesting,
// pruned and kept hidden folders, ignored folders, old, non-media and
// secret-looking files, and more fresh media than the cap.
func artifactWalkTree(t *testing.T, start time.Time) string {
	t.Helper()
	root := tempRootDir(t)
	fresh := start.Add(freshOffset)
	old := start.Add(-time.Hour)
	for _, f := range []struct {
		rel string
		at  time.Time
	}{
		{"a.png", fresh},
		{"a/b.png", fresh},
		{"a/z/deep/er/still.webm", fresh},
		{"a-c.png", fresh},
		{"a.b/x.png", fresh},
		{"B.png", fresh},
		{"_under/u.jpg", fresh},
		{"report.json", fresh},
		{"old.png", old},
		{"a/old.mp4", old},
		{".env.png", fresh},
		{"shots/secret-key.png", fresh},
		{"node_modules/pkg/n.png", fresh},
		{"vendor/v.png", fresh},
		{".hidden/h.png", fresh},
		{".maestro/out/m.png", fresh},
	} {
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(f.rel)), "x", f.at)
	}
	for d := 0; d < 12; d++ {
		for i := 0; i < 6; i++ {
			rel := "runs/run-" + strconv.Itoa(d) + "/shot-" + strconv.Itoa(i) + ".png"
			writeFileAt(t, filepath.Join(root, filepath.FromSlash(rel)), "x", fresh)
		}
	}
	return root
}

// Reading several folders at once changes nothing the scan returns: the same
// files, capped at the same first maxUploadFiles, in the same order, every
// time.
func TestTheParallelArtifactWalkReturnsWhatTheOneFolderWalkReturned(t *testing.T) {
	start := time.Now()
	root := artifactWalkTree(t, start)
	for _, cutoff := range []time.Time{start.Add(-mtimeSkew), {}} {
		want := oneFolderAtATimeArtifactWalk(context.Background(), root, cutoff)
		if len(want) != maxUploadFiles {
			t.Fatalf("the tree must hold more fresh media than the cap; the reference walk found %d", len(want))
		}
		for run := 0; run < 5; run++ {
			got, err := walkArtifactCandidates(context.Background(), root, cutoff)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("cutoff %v, run %d: the parallel walk returned\n%v\nthe one-folder walk returned\n%v", cutoff, run, got, want)
			}
		}
	}

	// Under the cap, every kept file and only those, in walk order.
	small := tempRootDir(t)
	for _, rel := range []string{"a.png", "a/b.png", "a-c.png", "a.b/x.png", "B.png", ".maestro/m.png", ".git/g.png", "old.png"} {
		at := start.Add(freshOffset)
		if rel == "old.png" {
			at = start.Add(-time.Hour)
		}
		writeFileAt(t, filepath.Join(small, filepath.FromSlash(rel)), "x", at)
	}
	// A junction (a symlink off Windows) to a folder outside is never walked.
	outside := tempRootDir(t)
	writeFileAt(t, filepath.Join(outside, "leak.png"), "x", start.Add(freshOffset))
	makeAbsDirLink(t, filepath.Join(small, "linked"), outside)
	got, _ := scanOutputFilesSince(context.Background(), small, start)
	var rels []string
	for _, p := range got {
		rel, _ := filepath.Rel(small, p)
		rels = append(rels, filepath.ToSlash(rel))
	}
	if want := []string{".maestro/m.png", "B.png", "a/b.png", "a-c.png", "a.b/x.png", "a.png"}; !reflect.DeepEqual(rels, want) {
		t.Fatalf("scan = %v, want %v", rels, want)
	}
}

// The walk reads more than one folder at a time; that is what keeps a
// session's ended frame from waiting on a whole checkout tree read one
// folder after another.
func TestTheArtifactWalkReadsSeveralFoldersAtOnce(t *testing.T) {
	start := time.Now()
	root := tempRootDir(t)
	const folders = 24
	for i := 0; i < folders; i++ {
		writeFileAt(t, filepath.Join(root, "f"+strconv.Itoa(i), "shot.png"), "x", start.Add(freshOffset))
	}
	var inFlight, most atomic.Int32
	readDir := func(dir string) ([]os.DirEntry, error) {
		n := inFlight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		return os.ReadDir(dir)
	}
	files, err := walkArtifactTree(context.Background(), root, start, artifactScanWorkers, readDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != folders {
		t.Fatalf("found %d files, want %d", len(files), folders)
	}
	if got := most.Load(); got < 2 || got > artifactScanWorkers {
		t.Fatalf("at most %d folders were read at once; want 2..%d", got, artifactScanWorkers)
	}
}

// A walk whose context ends stops every reader: it returns the context's
// error, and no reader starts on another folder once one has seen the end.
func TestTheArtifactWalkStopsEveryReaderWhenItsContextEnds(t *testing.T) {
	start := time.Now()
	root := tempRootDir(t)
	const folders = 200
	for i := 0; i < folders; i++ {
		if err := os.MkdirAll(filepath.Join(root, "f"+strconv.Itoa(i), "inner"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int32
	var once sync.Once
	readDir := func(dir string) ([]os.DirEntry, error) {
		if reads.Add(1) == 10 {
			once.Do(cancel)
		}
		return os.ReadDir(dir)
	}
	done := make(chan error, 1)
	go func() {
		_, err := walkArtifactTree(ctx, root, start, artifactScanWorkers, readDir)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a stopped walk returned %v, want the context's error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the walk did not return after its context ended")
	}
	if got := reads.Load(); got > 10+artifactScanWorkers {
		t.Fatalf("%d folders were read; the walk went on after its context ended (at most %d)", got, 10+artifactScanWorkers)
	}
}
