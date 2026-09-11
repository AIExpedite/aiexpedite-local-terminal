package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// grokLoginGuard serializes renewing the real Grok login against every child
// that runs on a COPY of it.
//
// Grok's login carries a rotating refresh token. An isolated home (ACP
// sessions, maintenance smokes, `grok models` discovery) holds a copy of
// auth.json for as long as the home exists, and its child may redeem that
// copy's refresh token at any time. Renewing the real home while a copy is live
// lets the two redeem the SAME token: whichever rotates second either fails or
// invalidates the other, and when the loser is the real home the user's CLI is
// signed out. So a renewal only runs when no copy exists, and no copy is taken
// while a renewal runs.
//
// Copies are registered when an isolated home is created — before the auth file
// is read, so a copy taken after a renewal holds the renewed credential — and
// released when it is removed.
type grokLoginGuard struct {
	mu       sync.Mutex
	copies   map[string]struct{}
	renewing bool
	// changed is closed (and replaced) on every state change, waking waiters.
	changed chan struct{}
}

var grokLogin = &grokLoginGuard{copies: map[string]struct{}{}, changed: make(chan struct{})}

func (g *grokLoginGuard) broadcastLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquireCopy registers an isolated home as holding a copy of the login,
// waiting out a renewal in flight (bounded by grokLoginRenewTimeout).
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

// beginRenewal waits until no copy of the login is live and no other renewal
// runs, then holds the login exclusively until the returned release is called.
// It gives up when ctx ends first: a renewal that cannot run safely is skipped,
// never forced — a failed usage refresh is recoverable, a signed-out CLI is not.
func (g *grokLoginGuard) beginRenewal(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	for {
		g.pruneRemovedLocked()
		if !g.renewing && len(g.copies) == 0 {
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
