// File: processRegistry.go
// -----------------------------------------------------------------------------
// Central thread-safe registry of OS process IDs spawned by this Go agent.
// Both interactive sessions (session.go) and one-shot commands (pubsub.go)
// register their PIDs here on spawn and deregister on completion. The orphan
// scanner (orphanScanner.go) consults this registry to determine which CLI
// processes (claude/codex/gemini/agy) are legitimately backed by an active call
// vs. orphans that should be killed.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"sync"
	"time"
)

// AppStartTime records when this process started. Used by the orphan scanner
// to skip processes that predate this agent (we have no record of them and
// shouldn't assume ownership). Assigned exactly once at package init and read
// (never written) by all goroutines afterwards — safe without a mutex.
var AppStartTime = time.Now()

// registryEntryMaxAge caps how long a registered PID remains considered active
// without being explicitly deregistered. This is a safety valve: normal flow
// always calls Deregister after Wait() returns, but a goroutine panic between
// Start() and Wait() would leave a stale entry indefinitely. After this max
// age, the entry is presumed stale and excluded from GetAll(). Chosen to
// exceed sessionMaxLifetime (6h) plus a small buffer — shorter is safer
// because long-lived stale entries can mask PID reuse bugs.
const registryEntryMaxAge = 7 * time.Hour

// processRegistryEntry tracks where a registered PID came from for diagnostics.
type processRegistryEntry struct {
	PID    int
	Source string // e.g. "session:abc123" or "pubsub:powershell"
	Added  time.Time
}

// processRegistry is a thread-safe set of currently-tracked PIDs.
type processRegistry struct {
	mu      sync.RWMutex
	entries map[int]processRegistryEntry
}

// globalProcessRegistry is the singleton used by all spawn sites.
var globalProcessRegistry = &processRegistry{
	entries: make(map[int]processRegistryEntry),
}

// Register adds a PID with its source label. Safe to call multiple times for
// the same PID — the latest source wins.
func (r *processRegistry) Register(pid int, source string) {
	if pid <= 0 {
		return
	}
	r.mu.Lock()
	r.entries[pid] = processRegistryEntry{
		PID:    pid,
		Source: source,
		Added:  time.Now(),
	}
	r.mu.Unlock()
}

// Deregister removes a PID. Safe to call for unknown PIDs.
func (r *processRegistry) Deregister(pid int) {
	if pid <= 0 {
		return
	}
	r.mu.Lock()
	delete(r.entries, pid)
	r.mu.Unlock()
}

// IsRegistered returns true if the PID is currently tracked.
func (r *processRegistry) IsRegistered(pid int) bool {
	if pid <= 0 {
		return false
	}
	r.mu.RLock()
	_, ok := r.entries[pid]
	r.mu.RUnlock()
	return ok
}

// GetAll returns a snapshot of currently registered PIDs, filtering out any
// entries older than registryEntryMaxAge (stale entries from a goroutine that
// panicked or otherwise failed to deregister). Uses a fast RLock path for the
// common case (no stale entries) and only escalates to Lock when cleanup is
// needed — stale-entry deletion is non-critical self-healing, not required
// for correctness.
func (r *processRegistry) GetAll() []int {
	cutoff := time.Now().Add(-registryEntryMaxAge)

	r.mu.RLock()
	pids := make([]int, 0, len(r.entries))
	hasStale := false
	for pid, entry := range r.entries {
		if entry.Added.Before(cutoff) {
			hasStale = true
			continue
		}
		pids = append(pids, pid)
	}
	r.mu.RUnlock()

	if hasStale {
		r.pruneStale(cutoff)
	}
	return pids
}

// GetSnapshot returns a snapshot of registered PIDs mapped to their registration
// time. Used by the orphan scanner to detect PID reuse: if the OS-reported
// process start time is substantially later than our registration time, the
// registered entry is stale (original process died without Deregister) and
// this PID belongs to an unrelated later process. Same fast RLock + slow
// prune pattern as GetAll.
func (r *processRegistry) GetSnapshot() map[int]time.Time {
	cutoff := time.Now().Add(-registryEntryMaxAge)

	r.mu.RLock()
	out := make(map[int]time.Time, len(r.entries))
	hasStale := false
	for pid, entry := range r.entries {
		if entry.Added.Before(cutoff) {
			hasStale = true
			continue
		}
		out[pid] = entry.Added
	}
	r.mu.RUnlock()

	if hasStale {
		r.pruneStale(cutoff)
	}
	return out
}

// pruneStale removes entries whose Added time is before cutoff. Acquires the
// write lock briefly; callers that only want to read should call GetAll or
// GetSnapshot instead (they trigger pruneStale only when stale entries are
// observed).
func (r *processRegistry) pruneStale(cutoff time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, entry := range r.entries {
		if entry.Added.Before(cutoff) {
			delete(r.entries, pid)
		}
	}
}

// SourceOf returns the source label for a registered PID, or "" if not found.
func (r *processRegistry) SourceOf(pid int) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.entries[pid]; ok {
		return e.Source
	}
	return ""
}

// SelfPID returns the PID of the running agent process. Used by the orphan
// scanner for self-protection (never kill our own PID or descendants).
func SelfPID() int {
	return os.Getpid()
}
