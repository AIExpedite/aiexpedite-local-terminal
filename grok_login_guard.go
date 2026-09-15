package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// grokLoginGuard tracks every child that runs on a COPY of the real Grok
// login, and serializes renewals of that login.
//
// Grok's login carries a rotating refresh token. An isolated home (ACP
// sessions, maintenance smokes, `grok models` discovery) holds a copy of
// auth.json for as long as the home exists. A renewal rotates the token for
// EVERY holder: after it, the copies hold a refresh token xAI revokes within
// minutes. So one renewal runs at a time, no copy is taken while one runs
// (a copy taken after it holds the renewed credential), and every renewal is
// followed by reconcileGrokLogin (grok_login_keeper.go), which writes the
// renewed file into each live copy inside the grace window — and writes a
// copy's own renewal back to the real home. Renewal therefore no longer
// waits for the copies to go away: an ACP session holds its copy for hours,
// and a real home that could not renew for hours was exactly the login that
// died.
//
// Copies are registered when an isolated home is created, under the same
// lock as the credential copy (seedIsolatedGrokLogin), and released when
// the home is removed.
type grokLoginGuard struct {
	mu       sync.Mutex
	copies   map[string]struct{}
	renewing bool
	// deferredRemoval holds registered copies whose removal could not yet
	// hand their credential to the real home after the quick retries; the
	// keeper retries them every tick (retryDeferredGrokHomeRemovals).
	deferredRemoval map[string]struct{}
	// owners holds the owner lock (grokOwnerLockName) of every home this
	// process created, so another agent process's stale-home sweep can see
	// the home is alive. Dropped before the home is removed.
	owners map[string]*os.File
	// changed is closed (and replaced) on every state change, waking waiters.
	changed chan struct{}
}

var grokLogin = &grokLoginGuard{copies: map[string]struct{}{}, changed: make(chan struct{})}

func (g *grokLoginGuard) broadcastLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquireCopy registers an isolated home as holding a copy of the login,
// waiting out a renewal in flight. The copy is assumed already seeded;
// setupIsolatedGrokHome uses seedIsolatedGrokLogin so registration and the
// credential copy are one step under the login lock.
func (g *grokLoginGuard) acquireCopy(home string) {
	key := filepath.Clean(home)
	g.mu.Lock()
	for g.renewing {
		wait := g.changed
		g.mu.Unlock()
		<-wait
		g.mu.Lock()
	}
	g.copies[key] = struct{}{}
	g.mu.Unlock()
}

// registerCopyHeld registers an isolated home from a caller that already holds
// the login exclusively (beginRenewal): seeding a copy happens under that
// lock, so a renewal can neither slip between the source read and the
// destination write nor reconcile past a home that is registered but still
// empty. acquireCopy would deadlock here — it waits for the very lock the
// caller holds.
func (g *grokLoginGuard) registerCopyHeld(home string) {
	key := filepath.Clean(home)
	g.mu.Lock()
	g.copies[key] = struct{}{}
	g.mu.Unlock()
}

// holdOwnerLock takes home's owner lock for this process and keeps it until
// dropOwnerLock. Failing to take it is not fatal: the home then only has the
// age guard against another process's sweep.
func (g *grokLoginGuard) holdOwnerLock(home string) error {
	key := filepath.Clean(home)
	f, err := os.OpenFile(filepath.Join(key, grokOwnerLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if prev, ok := g.owners[key]; ok {
		_ = unlockFile(prev)
		_ = prev.Close()
	}
	if g.owners == nil {
		g.owners = map[string]*os.File{}
	}
	g.owners[key] = f
	return nil
}

// dropOwnerLock releases and closes home's owner lock, if this process holds
// it. Called before the home's files are removed — an open handle inside the
// directory would keep Windows from removing it.
func (g *grokLoginGuard) dropOwnerLock(home string) {
	key := filepath.Clean(home)
	g.mu.Lock()
	f, ok := g.owners[key]
	delete(g.owners, key)
	g.mu.Unlock()
	if ok {
		_ = unlockFile(f)
		_ = f.Close()
	}
}

// deferRemoval marks a registered home for removal by the keeper's next
// ticks, once its credential has reached the real home.
func (g *grokLoginGuard) deferRemoval(home string) {
	key := filepath.Clean(home)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.deferredRemoval == nil {
		g.deferredRemoval = map[string]struct{}{}
	}
	g.deferredRemoval[key] = struct{}{}
}

// takeDeferredRemovals returns and clears the homes marked for removal; a
// removal that still cannot complete marks itself again.
func (g *grokLoginGuard) takeDeferredRemovals() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.deferredRemoval))
	for key := range g.deferredRemoval {
		out = append(out, key)
	}
	g.deferredRemoval = nil
	sort.Strings(out)
	return out
}

// holds reports whether home is currently registered as a copy of the login.
func (g *grokLoginGuard) holds(home string) bool {
	key := filepath.Clean(home)
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.copies[key]
	return ok
}

// releaseCopy forgets an isolated home. Idempotent, and a no-op for a home that
// was never registered.
func (g *grokLoginGuard) releaseCopy(home string) {
	if home == "" {
		return
	}
	key := filepath.Clean(home)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.copies[key]; !ok {
		return
	}
	delete(g.copies, key)
	g.broadcastLocked()
	if f, ok := g.owners[key]; ok {
		delete(g.owners, key)
		_ = unlockFile(f)
		_ = f.Close()
	}
}

// beginRenewal waits until no other renewal runs, then holds the login
// exclusively until the returned release is called. It gives up when ctx ends
// first: two renewals of one refresh token minutes apart sign the loser out,
// so a renewal that cannot take the turn is skipped, never forced. Copy
// seeding (seedIsolatedGrokLogin) takes this same lock, so a renewal cannot
// rotate under a snapshot that has not yet been written.
func (g *grokLoginGuard) beginRenewal(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	for {
		g.pruneRemovedLocked()
		if !g.renewing {
			break
		}
		wait := g.changed
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false
		}
		g.mu.Lock()
	}
	g.renewing = true
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.renewing = false
		g.broadcastLocked()
		g.mu.Unlock()
	}, true
}

// liveCopies returns the isolated homes currently holding a copy of the login,
// after forgetting any whose directory is already gone. A copy's auth file is
// as readable as the real home's — an ACP session that renewed ITS token holds a
// fresher credential than a real home nothing has touched for hours — so a
// read-only consumer (the billing probe) may present the freshest of them, and
// the keeper reconciles the newest across all of them.
func (g *grokLoginGuard) liveCopies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneRemovedLocked()
	out := make([]string, 0, len(g.copies))
	for key := range g.copies {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// pruneRemovedLocked forgets copies whose home no longer exists. Every removal
// path releases its copy, so this only matters for one that was missed — whose
// login copy is gone with the directory and can no longer be redeemed — and it
// keeps such a miss from blocking renewal for the life of the process.
func (g *grokLoginGuard) pruneRemovedLocked() {
	for key := range g.copies {
		if _, err := os.Stat(key); os.IsNotExist(err) {
			delete(g.copies, key)
			if f, ok := g.owners[key]; ok {
				delete(g.owners, key)
				_ = unlockFile(f)
				_ = f.Close()
			}
		}
	}
}
