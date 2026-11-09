//go:build windows

package main

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The real refusal these guards exist for: Windows refuses a rename over a
// file another process has open, because Go opens files without
// FILE_SHARE_DELETE. The retry outlasts the holder. Here the holder lets go
// during the first backoff, and the second attempt lands.
func TestCacheFileGuard_ReplaceOutlastsAFileHeldOpenElsewhere(t *testing.T) {
	var g cacheFileGuard
	path, tmp := cacheFileFixture(t, "old", "new")
	// Opened outside the guard, the way another process holds it.
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	backoffs := stubCacheFileBackoff(t, func(time.Duration) { _ = held.Close() })

	if err := g.replace(tmp, path); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := readCacheFileFixture(t, path); got != "new" {
		t.Fatalf("cache = %q, want the replacement", got)
	}
	if got := len(backoffs.taken()); got != 1 {
		t.Fatalf("backoffs = %d, want 1: refused while the file was held, then landing", got)
	}
}

// A read that lands while another process is renaming over the file is
// refused with a sharing violation: the rename holds the file without sharing
// it. The guard retries the read rather than reporting the cache absent.
func TestCacheFileGuard_ReadOutlastsARenameInProgressElsewhere(t *testing.T) {
	var g cacheFileGuard
	path, _ := cacheFileFixture(t, "cached", "")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	open := true
	release := func() {
		if open {
			open = false
			_ = windows.CloseHandle(h)
		}
	}
	t.Cleanup(release)
	backoffs := stubCacheFileBackoff(t, func(time.Duration) { release() })

	b, err := g.read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "cached" {
		t.Fatalf("read %q, want the cache", b)
	}
	if got := len(backoffs.taken()); got != 1 {
		t.Fatalf("backoffs = %d, want 1: refused while the rename held the file, then read", got)
	}
}
