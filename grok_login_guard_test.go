package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestGrokLoginGuard() *grokLoginGuard {
	return &grokLoginGuard{copies: map[string]struct{}{}, changed: make(chan struct{})}
}

// TestGrokLoginGuard_RenewalWaitsForEveryCopy: a renewal must not start while
// any isolated home holds a copy of the login, and gives up — rather than
// forcing its way in — when no gap opens within its budget.
func TestGrokLoginGuard_RenewalWaitsForEveryCopy(t *testing.T) {
	g := newTestGrokLoginGuard()
	home := t.TempDir()
	g.acquireCopy(home)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.beginRenewal(ctx); ok {
		t.Fatal("a renewal started while a copy of the login was live")
	}

	started := make(chan func(), 1)
	go func() {
		release, ok := g.beginRenewal(context.Background())
		if ok {
			started <- release
		}
	}()
	select {
	case <-started:
		t.Fatal("renewal did not wait for the copy")
	case <-time.After(50 * time.Millisecond):
	}
	g.releaseCopy(home)
	select {
	case release := <-started:
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("renewal never started after the last copy was released")
	}
}

// TestGrokLoginGuard_NoCopyTakenMidRenewal: an isolated home created while a
// renewal runs waits for it, so it never copies a half-rotated login.
func TestGrokLoginGuard_NoCopyTakenMidRenewal(t *testing.T) {
	g := newTestGrokLoginGuard()
	release, ok := g.beginRenewal(context.Background())
	if !ok {
		t.Fatal("renewal must start with no copies")
	}
	acquired := make(chan struct{})
	go func() {
		g.acquireCopy(t.TempDir())
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("a copy was taken while the login was being renewed")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the copy never proceeded after the renewal finished")
	}
}

// TestGrokLoginGuard_RemovedHomeDoesNotBlockRenewalForever: a copy whose home
// is gone can no longer be redeemed, so a missed release is pruned.
func TestGrokLoginGuard_RemovedHomeDoesNotBlockRenewalForever(t *testing.T) {
	g := newTestGrokLoginGuard()
	home := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	g.acquireCopy(home)
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, ok := g.beginRenewal(ctx)
	if !ok {
		t.Fatal("a removed home still blocked renewal")
	}
	release()
}

// TestIsolatedGrokHome_HoldsTheLoginUntilRemoved: every isolated home — model
// discovery, ACP sessions, smokes — registers its copy at setup and releases
// it on removal.
func TestIsolatedGrokHome_HoldsTheLoginUntilRemoved(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := setupIsolatedGrokSmokeHomeFrom(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := grokLogin.beginRenewal(ctx); ok {
		t.Fatal("renewal started while an isolated home held a copy of the login")
	}
	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatal(err)
	}
	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("renewal still blocked after the isolated home was removed")
	}
	release()
}

// TestProbeGrokBillingLive_RenewalSkippedWhileACopyIsLive: an expired token
// would normally trigger a renewal against the real home; with a model
// discovery's isolated home alive, it must be skipped rather than race it.
func TestProbeGrokBillingLive_RenewalSkippedWhileACopyIsLive(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "a@example.com", now.Add(-time.Minute))
	grokBillingServer(t, func(string) (int, string) { return 401, `{}` })
	renewals := 0
	prev := runGrokLoginRenewal
	runGrokLoginRenewal = func(context.Context, string, string) { renewals++ }
	t.Cleanup(func() { runGrokLoginRenewal = prev })

	copyHome := t.TempDir()
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = probeGrokBillingLive(ctx, "grok", time.Now)
	if renewals != 0 {
		t.Errorf("renewals=%d, want none while a copy of the login is live", renewals)
	}
}
