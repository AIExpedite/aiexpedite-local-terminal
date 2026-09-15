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
// Copies are registered when an isolated home is created — before the auth file
// is read — and released when it is removed. Registration holds a seed
// exclusion until that snapshot is on disk, so a renewal cannot rotate the
// real home under the copy and leave the new session on the superseded
// credential.
type grokLoginGuard struct {
	mu       sync.Mutex
	copies   map[string]struct{}
	renewing bool
	// seeding is how many copies are currently reading the real home and
	// writing the snapshot. Renewals wait this out.
	seeding int
	// changed is closed (and replaced) on every state change, waking waiters.
	changed chan struct{}
}

var grokLogin = &grokLoginGuard{copies: map[string]struct{}{}, changed: make(chan struct{})}

func (g *grokLoginGuard) broadcastLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquireCopy registers an isolated home as holding a copy of the login,
// waiting out a renewal in flight (bounded by grokLoginRenewTimeout). The
// copy is assumed already seeded; setupIsolatedGrokHome uses beginCopy so
// renewals also wait out the snapshot write.
func (g *grokLoginGuard) acquireCopy(home string) {
	g.mu.Lock()
	g.acquireCopyLocked(home)
	g.mu.Unlock()
}

func (g *grokLoginGuard) acquireCopyLocked(home string) {
	key := filepath.Clean(home)
	for g.renewing {
		wait := g.changed
		g.mu.Unlock()
		<-wait
		g.mu.Lock()
	}
	g.copies[key] = struct{}{}
}

// beginCopy is acquireCopy plus a seed exclusion: the returned function
// must be called once the snapshot is on disk (or the home is abandoned).
// Renewals wait for that call, so they cannot rotate under a read that has
// not yet been written.
func (g *grokLoginGuard) beginCopy(home string) func() {
	g.mu.Lock()
	g.acquireCopyLocked(home)
	g.seeding++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.seeding > 0 {
				g.seeding--
			}
			g.broadcastLocked()
			g.mu.Unlock()
		})
	}
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
}

// beginRenewal waits until no other renewal runs and no copy is still being
// seeded, then holds the login exclusively until the returned release is
// called. It gives up when ctx ends first: two renewals of one refresh token
// minutes apart sign the loser out, so a renewal that cannot take the turn
// is skipped, never forced.
func (g *grokLoginGuard) beginRenewal(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	for {
		g.pruneRemovedLocked()
		if !g.renewing && g.seeding == 0 {
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
		}
	}
}
