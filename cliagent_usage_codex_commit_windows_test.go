//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A handle another process holds open refuses the rename. One held for less
// than the retry budget is cleared by a later attempt.
func TestCodexCommit_ABriefForeignHandleIsClearedByRetry(t *testing.T) {
	path, _ := codexCommitFixture(t)
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	closeHandle := func() {
		if !closed {
			closed = true
			_ = handle.Close()
		}
	}
	defer closeHandle()

	prev := codexRateLimitRename
	sawRefusal := false
	codexRateLimitRename = func(old, new string) error {
		err := prev(old, new)
		if !sawRefusal {
			sawRefusal = true
			if err == nil {
				t.Errorf("the open handle did not refuse the first rename")
			}
			closeHandle()
		}
		return err
	}
	t.Cleanup(func() { codexRateLimitRename = prev })

	if !codexCommitRateLimitSnapshot(path, []byte("{\"buckets\":{}}\n"), time.Now()) {
		t.Fatal("a handle released inside the budget must not refuse the commit")
	}
	if !sawRefusal {
		t.Fatal("the rename was never attempted")
	}
}

// A handle held past the budget returns false within the bound, leaves no temp
// file, and leaves the previous cache bytes in place.
func TestCodexCommit_AHandleHeldPastTheBudgetLeavesTheCache(t *testing.T) {
	path, original := codexCommitFixture(t)
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	started := time.Now()
	ok := codexCommitRateLimitSnapshot(path, []byte("{\"replaced\":true}\n"), time.Now())
	elapsed := time.Since(started)
	if ok {
		t.Fatal("a handle held past the budget must refuse the commit")
	}
	if elapsed > time.Second {
		t.Fatalf("commit waited %s, past the rename budget", elapsed)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("the refused commit changed the cache")
	}
	matches, err := filepath.Glob(path + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("left %d temp files", len(matches))
	}
}

func codexCommitFixture(t *testing.T) (string, []byte) {
	t.Helper()
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Minute), now)
	original, err := os.ReadFile(f.cache)
	if err != nil || len(original) == 0 {
		t.Fatal("precondition: the cache is missing")
	}
	return f.cache, original
}
