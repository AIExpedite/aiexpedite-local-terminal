// File: keep_awake.go
// -----------------------------------------------------------------------------
// Keep the computer awake while a CLI session this agent started is running
// (ORCHESTRATOR_SPEND_GUARD_AND_COMPUTER_LOSS_PLAN.md §4.7b).
//
// A Ship run whose computer idle-sleeps goes silent, parks
// `computer-unreachable`, and on every brief macOS dark wake resumes for a few
// minutes before the computer sleeps again — ten of those and the run is
// stopped. So while any session is in the spawn ledger, the agent holds ONE
// OS keep-awake lock:
//
//	macOS    caffeinate -i -w <agent pid>       (keep_awake_darwin.go)
//	Windows  PowerSetRequest(SystemRequired)    (keep_awake_windows.go)
//	Linux    systemd-inhibit --what=idle …      (keep_awake_linux.go)
//
// Idle sleep only: the display may still sleep, a closed lid still sleeps,
// and a sleep the person asks for is never refused.
//
// Reference counted by session id: the first Hold takes the OS lock, the last
// Release lets it go. The choke points are the spawn-ledger helpers every
// manager already calls (session_ledger.go: beginSessionSpawn /
// openLedgerLogicalSession → Hold, releaseLedgerSession → Release), so every
// CLI session is covered with no per-manager edit — including a chat or voice
// session, which the agent cannot tell apart from a Ship run.
//
// Two safeguards: a session held longer than keepAwakeSessionCap is dropped
// (a forgotten release must not keep a laptop up for days), and
// AIEXPEDITE_KEEP_AWAKE=off|0|false|no on this machine never takes the lock.
// A lock that cannot be taken (no binary, an API error) is logged once; the
// session runs anyway, just without keep-awake.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// keepAwakeEnvVar lets the person at the keyboard turn keep-awake off.
const keepAwakeEnvVar = "AIEXPEDITE_KEEP_AWAKE"

// keepAwakeSessionCap is the longest one session holds the computer awake.
const keepAwakeSessionCap = 12 * time.Hour

// keepAwakeReason names the lock where the OS shows it (pmset -g assertions,
// powercfg /requests, systemd-inhibit --list).
const keepAwakeReason = "AI Expedite: a coding-agent session is running"

type keepAwakeHoldEntry struct {
	gen  uint64
	stop func() bool
}

// keepAwakeManager owns the session set and the one OS lock.
type keepAwakeManager struct {
	mu      sync.Mutex
	held    map[string]keepAwakeHoldEntry
	gen     uint64
	release func() // non-nil while the OS lock is held

	// Seams for tests.
	acquire    func() (release func(), err error)
	afterFunc  func(d time.Duration, f func()) (stop func() bool)
	disabled   func() bool
	sessionCap time.Duration
	logf       func(format string, args ...any)

	warnedAcquire bool
}

func newKeepAwakeManager() *keepAwakeManager {
	return &keepAwakeManager{
		held:    make(map[string]keepAwakeHoldEntry),
		acquire: acquirePlatformKeepAwake,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		disabled:   keepAwakeDisabledByEnv,
		sessionCap: keepAwakeSessionCap,
		logf:       func(format string, args ...any) { fmt.Printf(format, args...) },
	}
}

var globalKeepAwake = newKeepAwakeManager()

func keepAwakeHold(sessionID string)    { globalKeepAwake.Hold(sessionID) }
func keepAwakeRelease(sessionID string) { globalKeepAwake.Release(sessionID) }
func keepAwakeReleaseAll()              { globalKeepAwake.ReleaseAll() }

// keepAwakeDisabledByEnv: off / 0 / false / no (case-insensitive) turns it off.
func keepAwakeDisabledByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(keepAwakeEnvVar))) {
	case "off", "0", "false", "no":
		return true
	default:
		return false
	}
}

// Hold keeps the computer awake for sessionID. Idempotent per id.
func (m *keepAwakeManager) Hold(sessionID string) {
	if sessionID == "" || m.disabled() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.held[sessionID]; !ok {
		m.gen++
		gen := m.gen
		stop := m.afterFunc(m.sessionCap, func() { m.expire(sessionID, gen) })
		m.held[sessionID] = keepAwakeHoldEntry{gen: gen, stop: stop}
	}
	// Taken on the first hold; retried on a later one if it failed.
	if m.release == nil {
		release, err := m.acquire()
		if err != nil {
			if !m.warnedAcquire {
				m.warnedAcquire = true
				m.logf("%s[keep-awake] Could not keep the computer awake (%v); sessions run without it%s\n",
					colorYellow, err, colorReset)
			}
			return
		}
		m.release = release
		m.logf("[keep-awake] Keeping the computer awake while sessions run\n")
	}
}

// Release ends sessionID's hold. Unknown or repeated ids are no-ops.
func (m *keepAwakeManager) Release(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.held[sessionID]
	if !ok {
		return
	}
	entry.stop()
	delete(m.held, sessionID)
	m.releaseIfIdleLocked()
}

// ReleaseAll drops every hold and the OS lock (shutdown).
func (m *keepAwakeManager) ReleaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, entry := range m.held {
		entry.stop()
		delete(m.held, id)
	}
	m.releaseIfIdleLocked()
}

// expire drops a session held past the cap. The generation guards a release
// and re-hold of the same id racing the timer.
func (m *keepAwakeManager) expire(sessionID string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.held[sessionID]
	if !ok || entry.gen != gen {
		return
	}
	delete(m.held, sessionID)
	m.logf("%s[keep-awake] Session %s held the computer awake for %s; letting it go%s\n",
		colorYellow, sessionID, m.sessionCap, colorReset)
	m.releaseIfIdleLocked()
}

func (m *keepAwakeManager) releaseIfIdleLocked() {
	if len(m.held) > 0 || m.release == nil {
		return
	}
	m.release()
	m.release = nil
	m.logf("[keep-awake] No session running; the computer may sleep again\n")
}

// heldCount is for tests.
func (m *keepAwakeManager) heldCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.held)
}
