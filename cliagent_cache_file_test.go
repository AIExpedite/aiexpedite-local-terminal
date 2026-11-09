package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errCacheFileHeld stands in for the refusal Windows gives a rename over a file
// another process has open ("Access is denied"). A POSIX filesystem can't be
// made to refuse that way, so tests inject it through cacheFileRename.
var errCacheFileHeld = errors.New("simulated: the file is open in another process")

// cacheFileFixture writes a cache file and, when next is not empty, the temp
// file about to replace it. It returns both paths.
func cacheFileFixture(t *testing.T, current, next string) (path, tmp string) {
	t.Helper()
	dir := t.TempDir()
	path, tmp = filepath.Join(dir, "cache.json"), filepath.Join(dir, "cache.json.tmp")
	if err := os.WriteFile(path, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	if next != "" {
		if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path, tmp
}

func readCacheFileFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cacheFileBackoffs records the backoffs a guard took instead of sleeping them.
type cacheFileBackoffs struct {
	mu    sync.Mutex
	slept []time.Duration
}

func (b *cacheFileBackoffs) taken() []time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.slept)
}

// stubCacheFileBackoff makes every guard's backoff instant, recording it and
// running during (when set) in its place.
func stubCacheFileBackoff(t *testing.T, during func(time.Duration)) *cacheFileBackoffs {
	t.Helper()
	b := &cacheFileBackoffs{}
	prev := cacheFileSleep
	cacheFileSleep = func(d time.Duration) {
		b.mu.Lock()
		b.slept = append(b.slept, d)
		b.mu.Unlock()
		if during != nil {
			during(d)
		}
	}
	t.Cleanup(func() { cacheFileSleep = prev })
	return b
}

// refuseCacheFileRename refuses renames onto path the way Windows refuses one
// over a file another process has open, up to `times` refusals in all. refuse
// picks which renames, given the file's current bytes and the bytes about to
// replace them. It returns the count of refusals so far.
func refuseCacheFileRename(t *testing.T, path string, times int, refuse func(current, next []byte) bool) *atomic.Int64 {
	t.Helper()
	var refused atomic.Int64
	prev := cacheFileRename
	cacheFileRename = func(oldpath, newpath string) error {
		if newpath == path && refused.Load() < int64(times) {
			current, _ := os.ReadFile(newpath)
			next, _ := os.ReadFile(oldpath)
			if refuse(current, next) {
				refused.Add(1)
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errCacheFileHeld}
			}
		}
		return prev(oldpath, newpath)
	}
	t.Cleanup(func() { cacheFileRename = prev })
	return &refused
}

func refuseEveryCacheFileRename([]byte, []byte) bool { return true }

// A rename another process refuses is retried until it lands. The backoff
// doubles, and the guard is released for it, so a reader in this process
// never waits on the retry.
func TestCacheFileGuard_ReplaceOutlastsARefusedRename(t *testing.T) {
	var g cacheFileGuard
	path, tmp := cacheFileFixture(t, "old", "new")
	refused := refuseCacheFileRename(t, path, 2, refuseEveryCacheFileRename)
	backoffs := stubCacheFileBackoff(t, func(time.Duration) {
		if !g.mu.TryRLock() {
			t.Error("the guard was held across the backoff")
			return
		}
		g.mu.RUnlock()
	})

	if err := g.replace(tmp, path); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := readCacheFileFixture(t, path); got != "new" {
		t.Fatalf("cache = %q, want the replacement", got)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temp file is still there: %v", err)
	}
	if got := refused.Load(); got != 2 {
		t.Fatalf("refusals = %d, want 2", got)
	}
	if got, want := backoffs.taken(), []time.Duration{cacheFileBackoff, 2 * cacheFileBackoff}; !slices.Equal(got, want) {
		t.Errorf("backoffs = %v, want %v", got, want)
	}
}

// A rename still refused on its last attempt returns the refusal, leaves the
// cache as it was, and removes the temp file.
func TestCacheFileGuard_ReplaceGivesUpAfterItsAttempts(t *testing.T) {
	var g cacheFileGuard
	path, tmp := cacheFileFixture(t, "old", "new")
	refused := refuseCacheFileRename(t, path, math.MaxInt32, refuseEveryCacheFileRename)
	backoffs := stubCacheFileBackoff(t, nil)

	if err := g.replace(tmp, path); !errors.Is(err, errCacheFileHeld) {
		t.Fatalf("replace = %v, want the refusal", err)
	}
	if got := refused.Load(); got != int64(cacheFileAttempts) {
		t.Errorf("attempts = %d, want %d", got, cacheFileAttempts)
	}
	if got := len(backoffs.taken()); got != cacheFileAttempts-1 {
		t.Errorf("backoffs = %d, want one between each pair of attempts", got)
	}
	if got := readCacheFileFixture(t, path); got != "old" {
		t.Errorf("cache = %q, want it left as it was", got)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the temp file is still there: %v", err)
	}
}

// A delete another process refuses is retried, and a file already gone counts
// as removed without a retry.
func TestCacheFileGuard_RemoveOutlastsARefusalAndToleratesAMissingFile(t *testing.T) {
	var g cacheFileGuard
	path, _ := cacheFileFixture(t, "old", "")
	var calls atomic.Int64
	prev := cacheFileRemove
	cacheFileRemove = func(name string) error {
		if calls.Add(1) == 1 {
			return &os.PathError{Op: "remove", Path: name, Err: errCacheFileHeld}
		}
		return prev(name)
	}
	t.Cleanup(func() { cacheFileRemove = prev })
	stubCacheFileBackoff(t, nil)

	if err := g.remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the cache is still there: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("deletes = %d, want the refused one and the retry", got)
	}
	if err := g.remove(path); err != nil {
		t.Fatalf("remove of a missing file: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("deletes = %d, want a missing file removed without a retry", got)
	}
}

// A reader in this process never collides with this process's own write. The
// writer waits on the guard until the reader is done, rather than renaming over
// a file the reader has open, which Windows refuses.
func TestCacheFileGuard_AWriterWaitsForAReaderInThisProcess(t *testing.T) {
	var g cacheFileGuard
	path, tmp := cacheFileFixture(t, "old", "new")
	var readerHasFile, collided atomic.Bool
	prev := cacheFileRename
	cacheFileRename = func(oldpath, newpath string) error {
		if readerHasFile.Load() {
			collided.Store(true)
		}
		return prev(oldpath, newpath)
	}
	t.Cleanup(func() { cacheFileRename = prev })

	// A reader mid-read, as read holds it: the guard shared, the file open.
	g.mu.RLock()
	f, err := os.Open(path)
	if err != nil {
		g.mu.RUnlock()
		t.Fatal(err)
	}
	readerHasFile.Store(true)

	done := make(chan error, 1)
	go func() { done <- g.replace(tmp, path) }()
	// Keep the file until the writer is queued on the guard (a queued writer
	// makes TryRLock fail). A writer that skipped the guard would rename now,
	// and the seam records the collision.
	waitUntil(time.Now().Add(10*time.Second), func() bool {
		if collided.Load() {
			return true
		}
		if g.mu.TryRLock() {
			g.mu.RUnlock()
			return false
		}
		return true
	})
	readerHasFile.Store(false)
	_ = f.Close()
	g.mu.RUnlock()

	if err := <-done; err != nil {
		t.Fatalf("replace: %v", err)
	}
	if collided.Load() {
		t.Fatal("the writer renamed over the file while a reader in this process had it open")
	}
	if got := readCacheFileFixture(t, path); got != "new" {
		t.Fatalf("cache = %q, want the replacement", got)
	}
}

// A stat is a reader too: on Windows it opens the file, so a stat taken outside
// the guard refuses this process's own rename the way a read does. It waits for
// a write in progress, and returns once the write is done.
func TestCacheFileGuard_AStatWaitsForAWriteInProgress(t *testing.T) {
	var g cacheFileGuard
	path, _ := cacheFileFixture(t, "cached", "")

	g.mu.Lock() // a rename in progress, as replace holds it
	done := make(chan error, 1)
	go func() {
		_, err := g.stat(path)
		done <- err
	}()
	select {
	case <-done:
		g.mu.Unlock()
		t.Fatal("stat ran while a write held the guard")
	case <-time.After(50 * time.Millisecond):
	}
	g.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("stat after the write: %v", err)
	}
}

// The Claude cache's stamp (taken per gather and by the propagator's tick) is a
// stat of the cache, so it goes through claudeRateLimitFile like every read.
func TestClaudeRateLimitCacheStamp_TakesTheCacheGuard(t *testing.T) {
	claudeRateLimitFile.mu.Lock()
	done := make(chan struct{})
	go func() {
		claudeRateLimitCacheStamp()
		close(done)
	}()
	select {
	case <-done:
		claudeRateLimitFile.mu.Unlock()
		t.Fatal("claudeRateLimitCacheStamp stat the cache without the guard")
	case <-time.After(50 * time.Millisecond):
	}
	claudeRateLimitFile.mu.Unlock()
	<-done
}
