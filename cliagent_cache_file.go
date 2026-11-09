package main

import (
	"errors"
	"os"
	"sync"
	"time"
)

// cacheFileGuard lets this process replace one of its cache files while it is
// also reading that file, on Windows as on POSIX.
//
// The usage caches are replaced by writing a temp file and renaming it over the
// cache, and they are read without the writer's locks, because a writer can hold
// those across another process's lock wait and a read must not queue behind it.
// That is enough on POSIX. On Windows, Go opens files without FILE_SHARE_DELETE,
// so a rename or delete over a file that any goroutine has open fails ("Access
// is denied") until that reader closes it. A reader that opens the file while a
// rename is in progress fails too, with a sharing violation. The writers here
// are mostly best-effort and drop a failed write without a word, so a read
// landing a millisecond before a write silently undid it: a run-debt claim lease
// left standing for ten seconds, a charge or a refund lost, a debt never written.
//
// The guard rules that out for readers in this process. Readers share the lock
// across the read. A writer holds it across one rename or delete only, never
// across a wait on another process and never across the backoff below, so a
// reader waits at most for the rename in progress. Lock order: a cache's own
// write locks (its in-process mutex, then the cross-process sibling lock), then
// this one. Readers take only this one, so the order cannot cycle. The Codex
// cache (codexRateLimitFileMu) and the OpenCode ledger (openCodeUsageFileMu)
// have the same guard of their own.
//
// Another process can still have the file open: the status-line hook, the
// other release channel's agent, or a scanner looking at the file just written.
// So a refused rename or delete is retried on a short doubling backoff, and so
// is a read refused by another process's rename.
type cacheFileGuard struct {
	mu sync.RWMutex
}

// cacheFileAttempts bounds one rename, delete or read: the first try and four
// retries, waiting cacheFileBackoff and doubling (5, 10, 20, 40 ms; 75 ms in
// all). Another process holds a cache file only while it reads a few KB, so
// that outlasts it, and a write that fails for good costs no more than that.
// Vars so tests can pin them.
var (
	cacheFileAttempts = 5
	cacheFileBackoff  = 5 * time.Millisecond

	// cacheFileSleep is time.Sleep, cacheFileRename os.Rename and
	// cacheFileRemove os.Remove. Tests replace them to refuse a write the way
	// Windows does, and to take the backoff without waiting.
	cacheFileSleep  = time.Sleep
	cacheFileRename = os.Rename
	cacheFileRemove = os.Remove
)

// read is os.ReadFile under the guard. A read refused because another process
// is renaming over the file is retried. Any other error, including a missing
// file, is returned at once.
func (g *cacheFileGuard) read(path string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		g.mu.RLock()
		b, err := os.ReadFile(path)
		g.mu.RUnlock()
		if err == nil || attempt >= cacheFileAttempts || !cacheFileReplaceInProgress(err) {
			return b, err
		}
		cacheFileSleep(cacheFileRetryDelay(attempt))
	}
}

// readJSON is readJSONFile under the guard.
func (g *cacheFileGuard) readJSON(path string, into any) bool {
	b, err := g.read(path)
	if err != nil {
		return false
	}
	return unmarshalJSON(b, into)
}

// writeJSON is writeJSONFileAtomic under the guard.
func (g *cacheFileGuard) writeJSON(path string, value any) bool {
	return writeJSONFileVia(path, value, g.replace)
}

// replace renames tmp over path, retrying a refused rename. When the last
// attempt is refused too, it removes tmp and returns that error.
func (g *cacheFileGuard) replace(tmp, path string) error {
	err := g.retry(func() error { return cacheFileRename(tmp, path) })
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// remove deletes path, retrying a refused delete. A file that is already gone
// counts as removed.
func (g *cacheFileGuard) remove(path string) error {
	return g.retry(func() error {
		if err := cacheFileRemove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// retry runs op under the write lock until it succeeds or cacheFileAttempts
// have failed. The lock is released for each backoff.
func (g *cacheFileGuard) retry(op func() error) error {
	var err error
	for attempt := 1; attempt <= cacheFileAttempts; attempt++ {
		g.mu.Lock()
		err = op()
		g.mu.Unlock()
		if err == nil {
			return nil
		}
		if attempt < cacheFileAttempts {
			cacheFileSleep(cacheFileRetryDelay(attempt))
		}
	}
	return err
}

// cacheFileRetryDelay is the backoff after failed attempt n: cacheFileBackoff,
// doubled for each attempt before it.
func cacheFileRetryDelay(n int) time.Duration {
	return cacheFileBackoff << (n - 1)
}
