package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Readers and writers of the cache share codexRateLimitFileMu, so an in-process
// load cannot hold the file open across a rename and refuse the commit. Run
// this under -race: the lock sits on the hot read path.
func TestCodexCommit_ReadersDoNotRefuseTheCommit(t *testing.T) {
	now := time.Now()
	f := newCodexFreshnessFixture(t, now.Add(-time.Hour))
	f.seedPreRunReading(t, now.Add(-time.Minute), now)

	const readers = 4
	const writers = 4
	const rounds = 15
	var writersWG sync.WaitGroup
	var readersWG sync.WaitGroup
	var commitFails atomic.Int32
	var loadFails atomic.Int32
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := loadCodexRateLimitSnapshot(f.cache); !ok {
					loadFails.Add(1)
				}
			}
		}()
	}
	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func(i int) {
			defer writersWG.Done()
			for n := 0; n < rounds; n++ {
				ok := codexRateLimitCacheTransaction(context.Background(), f.cache, now, true, func(snap *codexRateLimitSnapshot) bool {
					snap.RunFloorMs = int64(i*rounds + n + 1)
					return true
				})
				if !ok {
					commitFails.Add(1)
				}
			}
		}(i)
	}
	writersWG.Wait()
	close(stop)
	readersWG.Wait()
	if got := commitFails.Load(); got != 0 {
		t.Fatalf("commits refused=%d, want every commit to succeed", got)
	}
	if got := loadFails.Load(); got != 0 {
		t.Fatalf("loads that failed to parse=%d, want 0", got)
	}
}
