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

// TestGrokLoginGuard_RenewalRunsBesideLiveCopiesButOneAtATime: a live copy
// no longer blocks a renewal (the keeper reconciles the renewed file into it
// instead), but two renewals never overlap — the second waits for the first
// to release, and gives up when its budget ends first.
func TestGrokLoginGuard_RenewalRunsBesideLiveCopiesButOneAtATime(t *testing.T) {
	g := newTestGrokLoginGuard()
	g.acquireCopy(t.TempDir())

	release, ok := g.beginRenewal(context.Background())
	if !ok {
		t.Fatal("a renewal must run while a copy of the login is live")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.beginRenewal(ctx); ok {
		t.Fatal("a second renewal started while the first was in flight")
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
		t.Fatal("second renewal did not wait for the first")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case release := <-started:
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("second renewal never started after the first released")
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
// discovery, ACP sessions, smokes — registers its copy at setup, so the
// keeper can reconcile it, and releases it on removal.
func TestIsolatedGrokHome_HoldsTheLoginUntilRemoved(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := setupIsolatedGrokSmokeHomeFrom(src)
	if err != nil {
		t.Fatal(err)
	}
	holds := func() bool {
		for _, live := range grokLogin.liveCopies() {
			if live == filepath.Clean(home) {
				return true
			}
		}
		return false
	}
	if !holds() {
		t.Fatal("isolated home not registered as a copy of the login")
	}
	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatal(err)
	}
	if holds() {
		t.Fatal("isolated home still registered after removal")
	}
}

// TestProbeGrokBillingLive_RenewalRunsAndFansOutWhileACopyIsLive: an expired
// token triggers a renewal against the real home even while a model
// discovery's isolated home is alive — and the copy receives the renewed
// login before its superseded refresh token is revoked.
func TestProbeGrokBillingLive_RenewalRunsAndFansOutWhileACopyIsLive(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, home, "access-A", "a@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer access-B" {
			return 401, `{}`
		}
		return 200, grokFixtureBody(now.Add(72 * time.Hour))
	})
	renewals := 0
	runGrokLoginRenewal = func(_ context.Context, _, base string) {
		renewals++
		writeGrokAuthMinted(t, base, "access-B", "a@example.com", now, now.Add(6*time.Hour))
	}

	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "access-A", "a@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := probeGrokBillingLive(ctx, "grok", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok after the renewal", got)
	}
	if renewals != 1 {
		t.Errorf("renewals=%d, want one even with a copy of the login live", renewals)
	}
	if got := grokKeyIn(t, copyHome); got != "access-B" {
		t.Errorf("live copy holds %q, want the renewed credential fanned out to it", got)
	}
}
