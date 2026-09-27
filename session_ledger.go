// File: session_ledger.go
// -----------------------------------------------------------------------------
// Boot identity and the spawn ledger (failover Wave 4 Phase 2, row 2.2 —
// local/documents/features/ship/ORCHESTRATOR_SPEND_GUARD_AND_COMPUTER_LOSS_PLAN.md
// §4.7 and §5.2).
//
// The server has to tell "network blip, same agent process, CLI still
// running" from "agent restarted, CLI gone". Two things make that possible:
//
//   - bootId: a random id minted once per agent process start. It rides on the
//     /online report, every pong and every session error frame, so the server
//     can recognise the SAME boot (a transient drop) and a NEW one (a restart).
//     previousBootId, the last boot's id, is persisted here.
//
//   - the spawn ledger: for each CLI session this agent starts, the session id,
//     the boot that started it, and each process it spawned for it as
//     { pid, start time, process group }. Persisted atomically in the config
//     dir, bounded. An entry is removed when its session ends cleanly.
//
// At boot, before the boot-time /online, runBootReap reads the entries of
// EARLIER boots and looks up ONLY their recorded PIDs. A PID is ours only if
// it is alive AND its start time equals the recorded one, so a reused PID is
// never killed or counted. Any still ours is ended (its process group on
// Unix, its tree on Windows — normally the kill-on-close Job Object already
// did that when the old agent died). Each session is then:
//
//   - reaped    — none of its recorded processes is still ours;
//   - surviving — one could not be ended (kept, retried every boot);
//   - unknown   — its state could not be determined (access denied, an
//     incomplete record): never listed, kept for a retry.
//
// sessionsReaped is signed into the /online body (bootReportSignature,
// deregister.go) and a reaped entry is dropped only after an /online carrying
// it was accepted. The ledger never enumerates other processes, never reads
// any process's environment, and never signals anything but a recorded PID
// whose start time still matches.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// agentBootID is this process's boot id, minted once at start.
var agentBootID = newBootID()

// newBootID returns a random RFC 4122 version-4 UUID.
func newBootID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to the
		// clock so a boot id is still unique per process.
		return fmt.Sprintf("boot-%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// CurrentBootID returns this agent process's boot id.
func CurrentBootID() string { return agentBootID }

// processProbeResult classifies one recorded PID.
type processProbeResult int

const (
	// processGone: not running, or running with another start time (a
	// reused PID, which is not ours).
	processGone processProbeResult = iota
	// processOurs: alive with the recorded start time.
	processOurs
	// processUnknown: the state could not be read.
	processUnknown
)

const (
	spawnLedgerFileName = "session-ledger.json"
	spawnLedgerVersion  = 1
	// spawnLedgerMaxEntries bounds the file. It equals the most session ids
	// terminal-service accepts per /online list, so a report always fits.
	// Evicting an entry is always safe: an unlisted session is never
	// certified, it only stays unproven.
	spawnLedgerMaxEntries = 200
	// spawnLedgerMaxPIDs bounds one session's recorded processes. A session
	// that spawns more is marked incomplete and is never certified reaped.
	spawnLedgerMaxPIDs = 16
	// spawnLedgerMaxAge drops an earlier boot's unresolved entry. Far beyond
	// the longest a run waits for a lost computer (72 h).
	spawnLedgerMaxAge = 7 * 24 * time.Hour
	// bootReapKillWait bounds the wait for one ended process to disappear.
	bootReapKillWait = 3 * time.Second
)

// Ledger entry states for an EARLIER boot's session.
const (
	ledgerStateLive      = ""          // current boot, session open
	ledgerStateReaped    = "reaped"    // proven gone, awaiting an accepted /online
	ledgerStateSurviving = "surviving" // a process could not be ended
	ledgerStateUnknown   = "unknown"   // could not be determined
)

type ledgerProcess struct {
	PID       int    `json:"pid"`
	StartTime string `json:"startTime"`
	// PGID is the process group the process leads (Unix), 0 otherwise.
	PGID int `json:"pgid,omitempty"`
	// Contained: the process was in a kill-on-close Job Object (Windows), so
	// the agent's death ended every descendant it started.
	Contained bool `json:"contained,omitempty"`
}

type ledgerEntry struct {
	SessionID string          `json:"sessionId"`
	BootID    string          `json:"bootId"`
	PIDs      []ledgerProcess `json:"pids"`
	// Logical marks a session that lives in the agent between processes (an
	// Antigravity / OpenCode session spawns one process per turn). Such an
	// entry stays while the session is open, with no PIDs between turns.
	Logical bool `json:"logical,omitempty"`
	// PendingSpawns counts spawns begun but not yet recorded. A crash inside
	// that window leaves a process the ledger never saw, so an entry with a
	// pending spawn is never certified.
	PendingSpawns int `json:"pendingSpawns,omitempty"`
	// Incomplete marks an entry missing a process (no start time could be
	// read, or the PID cap was hit): never certified.
	Incomplete bool   `json:"incomplete,omitempty"`
	State      string `json:"state,omitempty"`
	UpdatedAt  int64  `json:"updatedAt"`
}

type ledgerFileData struct {
	Version int            `json:"version"`
	BootID  string         `json:"bootId"`
	Entries []*ledgerEntry `json:"entries"`
}

// spawnLedger is the in-memory ledger, mirrored to disk on every change.
type spawnLedger struct {
	mu     sync.Mutex
	path   func() string
	bootID string

	loaded     bool
	prevBootID string
	entries    []*ledgerEntry
	jobs       map[int]uintptr // pid → Job Object handle (Windows)

	reapStarted bool
	reapDone    chan struct{}

	// Seams for tests.
	now        func() time.Time
	writeFile  func(path string, data []byte) error
	startToken func(pid int) (string, error)
	probe      func(ledgerProcess) processProbeResult
	end        func(ledgerProcess, time.Duration) processProbeResult
	// descendants answers, for a recorded process that is itself gone,
	// whether what it started can be proven gone too (its process group on
	// Unix, its kill-on-close job on Windows).
	descendants func(ledgerProcess) processProbeResult
	attachJob   func(proc *os.Process, suspended bool) (job uintptr, contained bool, err error)
	// treeGone: an exited process's group (Unix) / job (Windows) is empty.
	treeGone func(p ledgerProcess, job uintptr) bool
	// acquireOwnership takes the ledger's lock file for this process's life.
	// A second live agent on the same config dir (Linux has no instance
	// guard) must never reap or rewrite the first one's ledger.
	acquireOwnership func() bool
	disabled         bool
	lockFile         *os.File // held (locked) for the life of the process
	releaseJob       func(uintptr)
	groupOf          func(*os.Process) int
}

func newSpawnLedger(path func() string, bootID string) *spawnLedger {
	l := &spawnLedger{
		path:        path,
		bootID:      bootID,
		jobs:        make(map[int]uintptr),
		reapDone:    make(chan struct{}),
		now:         time.Now,
		writeFile:   writeLedgerFileAtomic,
		startToken:  processStartToken,
		probe:       probeRecordedProcess,
		end:         endRecordedProcess,
		descendants: probeRecordedDescendants,
		attachJob:   attachSessionJob,
		treeGone:    processTreeGone,
		releaseJob:  releaseSessionJob,
		groupOf:     processGroupOf,
	}
	l.acquireOwnership = func() bool {
		f := acquireLedgerLock(l.path() + ".lock")
		l.lockFile = f
		return f != nil
	}
	return l
}

// globalSpawnLedger is the agent's ledger, in the config dir.
var globalSpawnLedger = newSpawnLedger(func() string {
	return filepath.Join(GetConfigDir(), spawnLedgerFileName)
}, agentBootID)

// writeLedgerFileAtomic writes data to a temp file in the same directory,
// flushes it, and renames it over path, so a crash leaves either the old or
// the new ledger, never a torn one.
func writeLedgerFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".session-ledger-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := atomicReplaceConfigFile(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// loadLocked reads the file once. A missing or unreadable file starts empty:
// the ledger can then certify nothing from earlier boots, which is the safe
// direction (unlisted sessions stay unproven).
func (l *spawnLedger) loadLocked() {
	if l.loaded {
		return
	}
	l.loaded = true
	if !l.acquireOwnership() {
		// Another live agent owns this ledger: this process neither reads,
		// reaps nor writes it. Its own sessions stay unrecorded, which can
		// only leave them unproven (never falsely reaped).
		l.disabled = true
		fmt.Printf("%s[ledger] Another agent process owns the spawn ledger — boot reap and session recording disabled for this process%s\n",
			colorYellow, colorReset)
		return
	}
	raw, err := os.ReadFile(l.path())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Printf("%s[ledger] Could not read the spawn ledger: %v%s\n", colorYellow, err, colorReset)
		}
		return
	}
	var data ledgerFileData
	if err := json.Unmarshal(raw, &data); err != nil {
		fmt.Printf("%s[ledger] Spawn ledger unreadable, starting empty: %v%s\n", colorYellow, err, colorReset)
		return
	}
	if data.BootID != l.bootID {
		l.prevBootID = data.BootID
	}
	for _, e := range data.Entries {
		if e == nil || e.SessionID == "" || e.BootID == "" {
			continue
		}
		// An entry that claims this boot was not written by this process
		// (a boot id is minted per process); it cannot be trusted as live.
		if e.BootID == l.bootID {
			continue
		}
		l.entries = append(l.entries, e)
	}
}

// persistLocked mirrors the ledger to disk. Failures are logged, never fatal:
// the agent must keep running sessions, and a stale ledger file can only
// under-certify (see the PendingSpawns / Incomplete guards).
func (l *spawnLedger) persistLocked() {
	if l.disabled {
		return
	}
	data := ledgerFileData{Version: spawnLedgerVersion, BootID: l.bootID, Entries: l.entries}
	if data.Entries == nil {
		data.Entries = []*ledgerEntry{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		fmt.Printf("%s[ledger] Could not encode the spawn ledger: %v%s\n", colorRed, err, colorReset)
		return
	}
	if err := l.writeFile(l.path(), raw); err != nil {
		fmt.Printf("%s[ledger] Could not write the spawn ledger: %v%s\n", colorRed, err, colorReset)
	}
}

func (l *spawnLedger) findLocked(sessionID, bootID string) (int, *ledgerEntry) {
	for i, e := range l.entries {
		if e.SessionID == sessionID && e.BootID == bootID {
			return i, e
		}
	}
	return -1, nil
}

func (l *spawnLedger) removeAtLocked(i int) {
	l.entries = append(l.entries[:i], l.entries[i+1:]...)
}

// currentLocked returns this boot's entry for sessionID, creating it (and
// evicting the oldest entry when the ledger is full).
func (l *spawnLedger) currentLocked(sessionID string) *ledgerEntry {
	if _, e := l.findLocked(sessionID, l.bootID); e != nil {
		return e
	}
	for len(l.entries) >= spawnLedgerMaxEntries {
		oldest := 0
		for i, e := range l.entries {
			if e.UpdatedAt < l.entries[oldest].UpdatedAt {
				oldest = i
			}
		}
		l.removeAtLocked(oldest)
	}
	e := &ledgerEntry{SessionID: sessionID, BootID: l.bootID, PIDs: []ledgerProcess{}}
	l.entries = append(l.entries, e)
	return e
}

func (l *spawnLedger) touch(e *ledgerEntry) { e.UpdatedAt = l.now().UnixMilli() }

// OpenLogicalSession records a session that spawns one process per turn.
func (l *spawnLedger) OpenLogicalSession(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	e := l.currentLocked(sessionID)
	e.Logical = true
	l.touch(e)
	l.persistLocked()
}

// BeginSpawn is called immediately before a session's process is started.
func (l *spawnLedger) BeginSpawn(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	e := l.currentLocked(sessionID)
	e.PendingSpawns++
	l.touch(e)
	l.persistLocked()
}

// AbortSpawn undoes BeginSpawn when Start failed.
func (l *spawnLedger) AbortSpawn(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	i, e := l.findLocked(sessionID, l.bootID)
	if e == nil {
		return
	}
	if e.PendingSpawns > 0 {
		e.PendingSpawns--
	}
	if !e.Logical && e.PendingSpawns == 0 && len(e.PIDs) == 0 {
		l.removeAtLocked(i)
	} else {
		l.touch(e)
	}
	l.persistLocked()
}

// TrackProcess records a started process for sessionID (after BeginSpawn)
// and, on Windows, puts it in its kill-on-close Job Object. suspended says the
// process was created suspended (beginSessionSpawn): it is then assigned to
// the job BEFORE it runs, and resumed here whatever else fails.
func (l *spawnLedger) TrackProcess(sessionID string, proc *os.Process, suspended bool) {
	if proc == nil || proc.Pid <= 0 {
		return
	}
	// First, whatever the session id: a suspended process must be resumed.
	// Only a process assigned before it ran is "contained" (no child of it
	// can have been created outside the job).
	job, contained, jobErr := l.attachJob(proc, suspended)
	if jobErr != nil {
		fmt.Printf("%s[ledger] Could not put session %s (PID %d) in a kill-on-close job: %v%s\n",
			colorYellow, sessionID, proc.Pid, jobErr, colorReset)
	}
	if sessionID == "" {
		if job != 0 {
			l.releaseJob(job)
		}
		return
	}
	token, tokenErr := l.startToken(proc.Pid)
	pgid := l.groupOf(proc)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	if job != 0 {
		if old, ok := l.jobs[proc.Pid]; ok {
			l.releaseJob(old)
		}
		l.jobs[proc.Pid] = job
	}
	e := l.currentLocked(sessionID)
	if e.PendingSpawns > 0 {
		e.PendingSpawns--
	}
	switch {
	case tokenErr != nil || token == "":
		e.Incomplete = true
		fmt.Printf("%s[ledger] No start time for session %s PID %d (%v): it will never be reported reaped%s\n",
			colorYellow, sessionID, proc.Pid, tokenErr, colorReset)
	case len(e.PIDs) >= spawnLedgerMaxPIDs:
		e.Incomplete = true
	default:
		e.PIDs = append(e.PIDs, ledgerProcess{PID: proc.Pid, StartTime: token, PGID: pgid, Contained: contained})
	}
	l.touch(e)
	l.persistLocked()
}

// UntrackProcess forgets one process of an open session after it exited (a
// per-turn process). The session's entry stays.
func (l *spawnLedger) UntrackProcess(sessionID string, pid int) {
	if sessionID == "" || pid <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	i, e := l.findLocked(sessionID, l.bootID)
	if e == nil {
		l.releaseJobLocked(pid)
		return
	}
	kept := e.PIDs[:0]
	for _, p := range e.PIDs {
		if p.PID != pid {
			kept = append(kept, p)
			continue
		}
		// The turn's leader exited, but a tool it started may still run in
		// its group / job. Keep the record (and the kill-on-close job) until
		// that is proven empty: the next boot then sees a live group (never
		// "reaped"), or the job ends the tool if the agent dies.
		if !l.treeGone(p, l.jobs[pid]) {
			fmt.Printf("%s[ledger] Session %s: PID %d exited but left processes in its group/job; kept for the boot reap%s\n",
				colorYellow, sessionID, pid, colorReset)
			kept = append(kept, p)
			continue
		}
		l.releaseJobLocked(pid)
	}
	e.PIDs = kept
	// A turn that outlived its session's release re-created a bare entry;
	// nothing is left to own once its process is gone.
	if !e.Logical && e.PendingSpawns == 0 && len(e.PIDs) == 0 {
		l.removeAtLocked(i)
	} else {
		l.touch(e)
	}
	l.persistLocked()
}

// ReleaseSession removes this boot's entry for a session that ended cleanly
// (the removeSession / removeSessionIfSame points of every manager).
func (l *spawnLedger) ReleaseSession(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loadLocked()
	i, e := l.findLocked(sessionID, l.bootID)
	if e == nil {
		return
	}
	// A session ending does not prove its tree ended: a CLI may have left a
	// daemonized child, or a turn a tool. Keep every record whose group
	// (Unix) / job (Windows) is not proven empty — with its kill-on-close job
	// — so the next boot never certifies the session on the record's absence
	// and an agent death still ends what the job holds.
	kept := e.PIDs[:0]
	for _, p := range e.PIDs {
		if l.treeGone(p, l.jobs[p.PID]) {
			l.releaseJobLocked(p.PID)
			continue
		}
		fmt.Printf("%s[ledger] Session %s ended but PID %d's group/job still holds processes; kept for the boot reap%s\n",
			colorYellow, sessionID, p.PID, colorReset)
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		l.removeAtLocked(i)
	} else {
		e.PIDs = kept
		e.Logical = false
		e.PendingSpawns = 0
		l.touch(e)
	}
	l.persistLocked()
}

func (l *spawnLedger) releaseJobLocked(pid int) {
	if job, ok := l.jobs[pid]; ok {
		l.releaseJob(job)
		delete(l.jobs, pid)
	}
}

// bootReapClass is the classification of one earlier-boot entry.
type bootReapClass int

const (
	reapClassReaped bootReapClass = iota
	reapClassSurviving
	reapClassUnknown
)

// classifyEarlierEntry looks up ONLY the entry's recorded PIDs, ends any
// still ours, and classifies the session.
func (l *spawnLedger) classifyEarlierEntry(e ledgerEntry) bootReapClass {
	if e.State == ledgerStateReaped {
		return reapClassReaped
	}
	// A pending spawn or an incomplete record may hide a process the ledger
	// never saw: never certifiable. Kept as an unknown tombstone (not
	// dropped) so it also blocks a reaped entry of the same session id in
	// another generation; the age limit removes it eventually.
	if e.PendingSpawns > 0 || e.Incomplete {
		return reapClassUnknown
	}
	surviving, unknown := false, false
	for _, p := range e.PIDs {
		switch l.probe(p) {
		case processGone:
			// The recorded process is gone, but a tool it started may not be:
			// only a group / job that is provably empty counts. Anything else
			// is not ours to kill (it is not a recorded PID) and not proof.
			if l.descendants(p) != processGone {
				unknown = true
			}
			continue
		case processUnknown:
			unknown = true
			continue
		}
		// Still ours: end it (group / tree), then re-check.
		switch l.end(p, bootReapKillWait) {
		case processGone:
			fmt.Printf("%s[boot-reap] Ended PID %d left by session %s (boot %s)%s\n",
				colorYellow, p.PID, e.SessionID, e.BootID, colorReset)
			// Ended, but a descendant that escaped the kill is still not
			// proven gone: the same rule as a process found already gone.
			if l.descendants(p) != processGone {
				unknown = true
			}
		case processUnknown:
			unknown = true
		default:
			surviving = true
			fmt.Printf("%s[boot-reap] PID %d of session %s would not end; will retry next boot%s\n",
				colorRed, p.PID, e.SessionID, colorReset)
		}
	}
	switch {
	case surviving:
		return reapClassSurviving
	case unknown:
		return reapClassUnknown
	default:
		return reapClassReaped
	}
}

// RunBootReap classifies every earlier boot's session. Call once at boot,
// before the boot-time /online (and before any session can start).
func (l *spawnLedger) RunBootReap() {
	l.mu.Lock()
	if l.reapStarted {
		l.mu.Unlock()
		<-l.reapDone
		return
	}
	l.reapStarted = true
	l.loadLocked()
	if l.disabled {
		l.mu.Unlock()
		close(l.reapDone)
		return
	}
	cutoff := l.now().Add(-spawnLedgerMaxAge).UnixMilli()
	type work struct {
		sessionID, bootID string
		snapshot          ledgerEntry
	}
	var todo []work
	for _, e := range l.entries {
		if e.BootID == l.bootID {
			continue
		}
		snap := *e
		snap.PIDs = append([]ledgerProcess(nil), e.PIDs...)
		todo = append(todo, work{e.SessionID, e.BootID, snap})
	}
	l.mu.Unlock()
	defer close(l.reapDone)

	results := make(map[[2]string]bootReapClass, len(todo))
	for _, w := range todo {
		results[[2]string{w.sessionID, w.bootID}] = l.classifyEarlierEntry(w.snapshot)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	var reaped, surviving int
	for _, e := range l.entries {
		class, ok := results[[2]string{e.SessionID, e.BootID}]
		if !ok {
			kept = append(kept, e)
			continue
		}
		switch class {
		case reapClassReaped:
			e.State = ledgerStateReaped
			e.PIDs = []ledgerProcess{}
			reaped++
		case reapClassSurviving:
			e.State = ledgerStateSurviving
			surviving++
		case reapClassUnknown:
			e.State = ledgerStateUnknown
		}
		// An unresolved entry is retried every boot, but not forever.
		if class != reapClassReaped && e.UpdatedAt > 0 && e.UpdatedAt < cutoff {
			continue
		}
		kept = append(kept, e)
	}
	l.entries = kept
	l.persistLocked()
	fmt.Printf("%s[boot-reap] boot %s (previous %q): %d session(s) reaped, %d surviving%s\n",
		colorCyan, l.bootID, l.prevBootID, reaped, surviving, colorReset)
}

// bootReport is the boot identity and restart report sent on /online.
type bootReport struct {
	BootID            string
	PreviousBootID    string
	SessionsReaped    []string
	SessionsSurviving []string
}

// Report returns the current report. While the boot reap is still running it
// waits for it until ctx ends; a report taken before the reap finished (or
// when it never ran) carries the boot id only, and the next /online carries
// the rest.
func (l *spawnLedger) Report(ctx context.Context) bootReport {
	l.mu.Lock()
	started := l.reapStarted
	l.mu.Unlock()
	finished := false
	if started {
		select {
		case <-l.reapDone:
			finished = true
		case <-ctx.Done():
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := bootReport{
		BootID:            l.bootID,
		SessionsReaped:    []string{},
		SessionsSurviving: []string{},
	}
	if !finished {
		return r
	}
	r.PreviousBootID = l.prevBootID
	seenR, seenS := map[string]bool{}, map[string]bool{}
	// unresolved: a session id with ANY entry that is not proven reaped
	// (surviving, unknown, or open in this boot). A reaped entry of the same
	// id then certifies nothing: another generation may still act.
	unresolved := map[string]bool{}
	for _, e := range l.entries {
		if e.BootID == l.bootID || e.State != ledgerStateReaped {
			unresolved[e.SessionID] = true
		}
		if e.BootID == l.bootID {
			continue
		}
		switch e.State {
		case ledgerStateReaped:
			if !seenR[e.SessionID] {
				seenR[e.SessionID] = true
				r.SessionsReaped = append(r.SessionsReaped, e.SessionID)
			}
		case ledgerStateSurviving:
			if !seenS[e.SessionID] {
				seenS[e.SessionID] = true
				r.SessionsSurviving = append(r.SessionsSurviving, e.SessionID)
			}
		}
	}
	filtered := r.SessionsReaped[:0]
	for _, id := range r.SessionsReaped {
		if !unresolved[id] {
			filtered = append(filtered, id)
		}
	}
	r.SessionsReaped = filtered
	sort.Strings(r.SessionsReaped)
	sort.Strings(r.SessionsSurviving)
	return r
}

// AckReaped drops the reaped entries an accepted /online carried.
func (l *spawnLedger) AckReaped(sessionIDs []string) {
	if len(sessionIDs) == 0 {
		return
	}
	acked := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		acked[id] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.entries[:0]
	changed := false
	for _, e := range l.entries {
		if e.BootID != l.bootID && e.State == ledgerStateReaped && acked[e.SessionID] {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	l.entries = kept
	if changed {
		l.persistLocked()
	}
}

// ── Spawn-site helpers ────────────────────────────────────────────────────
//
// Every CLI session spawn path calls, in order:
//
//	beginSessionSpawn(id, cmd)     // immediately before cmd.Start()
//	abortSessionSpawn(id)          // when Start failed
//	trackSessionProcess(id, cmd)   // IMMEDIATELY after Start succeeded
//
// and releaseLedgerSession(id) where the manager removes the session.
// beginSessionSpawn prepares cmd for ownership (prepareOwnedStart): on Unix
// its own process group (Setpgid, unless Setsid already makes it a leader);
// on Windows it is created SUSPENDED, so trackSessionProcess can put it in its
// kill-on-close job before it runs and then resume it. A nil cmd (PTY, whose
// pty.Start sets Setsid itself) only records the spawn.

func beginSessionSpawn(sessionID string, cmd *exec.Cmd) {
	if cmd != nil {
		prepareOwnedStart(cmd)
	}
	globalSpawnLedger.BeginSpawn(sessionID)
}

func abortSessionSpawn(sessionID string) { globalSpawnLedger.AbortSpawn(sessionID) }

func trackSessionProcess(sessionID string, cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	globalSpawnLedger.TrackProcess(sessionID, cmd.Process, startedSuspended(cmd))
}

func untrackSessionProcess(sessionID string, pid int) {
	globalSpawnLedger.UntrackProcess(sessionID, pid)
}

func openLedgerLogicalSession(sessionID string) {
	globalSpawnLedger.OpenLogicalSession(sessionID)
}

func releaseLedgerSession(sessionID string) { globalSpawnLedger.ReleaseSession(sessionID) }

// ledgerLockWait bounds how long a starting agent waits for the ledger lock:
// an updating agent's predecessor may still be exiting.
var ledgerLockWait = 15 * time.Second

// acquireLedgerLock takes an exclusive lock on path, retrying up to
// ledgerLockWait, and returns the locked file (nil when not acquired). The
// caller keeps it open for the life of the process; exit releases it.
func acquireLedgerLock(path string) *os.File {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Printf("%s[ledger] Cannot create the ledger lock dir: %v%s\n", colorYellow, err, colorReset)
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Printf("%s[ledger] Cannot open the ledger lock: %v%s\n", colorYellow, err, colorReset)
		return nil
	}
	deadline := time.Now().Add(ledgerLockWait)
	for {
		ok, err := tryLockFileExclusive(f)
		if ok {
			return f
		}
		if err != nil || time.Now().After(deadline) {
			_ = f.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}
