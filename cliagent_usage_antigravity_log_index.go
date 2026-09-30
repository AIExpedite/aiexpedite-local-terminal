// cliagent_usage_antigravity_log_index.go — which `agy` logs are runs that
// still owe a utilization reading.
//
// Every `agy` process writes a second-stamped log under
// ~/.gemini/antigravity-cli/log (cli-YYYYMMDD_HHMMSS.log, local time; two runs
// started in the same second share one file, each block opening with
// "Starting language server process with pid N"). The gather used to treat the
// newest of those logs as a finished run. That made three mistakes:
//
//   - The agent's OWN children (`agy models` for model discovery, the Refresh
//     click's live probe) write logs too, so every model probe looked like a
//     missed run and owed a Code Assist read ("behindBy=1s; refresh owed").
//   - A direct run (the user's own shell) was judged finished after 60 s of log
//     silence, whether or not its process was still alive.
//   - Nothing noticed a direct run until a gather happened to run.
//
// This file keeps an in-memory index instead:
//
//   - Own children register before they start (beginAntigravityOwnChild) and,
//     when they end, claim the new logs whose SOLE PID block is theirs.
//   - A process-wide discovery tick lists the log names every
//     antigravityDiscoveryInterval, classifies new ones, and re-stats a bounded
//     watched set so a run appended to an already-classified file (same-second
//     names) or a PID block written after the first sighting is still found.
//   - A foreign log newer than the cached reading is a CANDIDATE: its PIDs are
//     held by start token until every one has exited, and only then is the run
//     owed a refresh (floor = max(log mtime, exit seen)). While it is live its
//     first-seen instant is a protected floor, so a restart adopts it.
//   - Managed runs (a capture handle knows their agy PID) and own children are
//     never candidates: their own paths settle them.
//
// Nothing here is persisted and nothing is logged beyond integer counters: no
// path, file name, PID, account or log text.
//
// Lock order: the index lock is a leaf with respect to the freshness and
// live-runs locks — nothing holds it while taking either. Floors are armed and
// released after the index lock is dropped.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// antigravityWatchedNewest is how many of the newest names every tick
	// re-stats, whatever their classification.
	antigravityWatchedNewest = 32
	// antigravityOwnedCap bounds the owned logs kept watched (about 16 h of
	// `agy models` at its 30 min cadence).
	antigravityOwnedCap = 32
	// antigravityCandidateCap bounds the foreign logs held as candidates, and
	// antigravityNoPIDCap the logs with no PID block yet.
	antigravityCandidateCap = 8
	antigravityNoPIDCap     = 32
	// antigravityGraceCap bounds the collision-grace list (evicted owned and
	// noPID entries still inside their name-second's collision window).
	antigravityGraceCap = 64
	// antigravityProcessOnlyCap bounds the live PIDs of evicted candidates kept
	// armed without a log.
	antigravityProcessOnlyCap = 64
	// antigravityCandidateMaxPIDs is how many foreign PIDs one log may name
	// before the candidate is held as pidOverflow (live until proven otherwise).
	antigravityCandidateMaxPIDs = 4
	// antigravityOwnChildMaxFiles bounds the new files an own child examines
	// when it ends, and antigravityOwnLogMaxBytes the size of one it reads
	// whole.
	antigravityOwnChildMaxFiles = 8
	antigravityOwnLogMaxBytes   = 256 * 1024
	// antigravityStartupScanCap bounds the names the first pass classifies,
	// and antigravityStartupDSTSlack widens its window: names are local time.
	antigravityStartupScanCap  = 256
	antigravityStartupDSTSlack = time.Hour
	// antigravityCandidateSettle is how long an exited candidate, or a noPID
	// log, must stay quiet before its run is owed.
	antigravityCandidateSettle = 60 * time.Second
	// antigravityCandidateHoldLimit bounds how long a candidate held only by
	// unreadable or overflowing PIDs may stay armed.
	antigravityCandidateHoldLimit = 6 * time.Hour
	// antigravityKnownPIDCap bounds the own and managed PID rings.
	antigravityKnownPIDCap = 64
)

// Vars rather than consts so tests can pin them.
var (
	// antigravityDiscoveryInterval is the discovery tick.
	antigravityDiscoveryInterval = 60 * time.Second
	// antigravityCollisionGrace is how long after its name-second a file can
	// still be appended to by a run started in that same second (or by its own
	// run finishing its startup lines).
	antigravityCollisionGrace = 2 * time.Minute
	// antigravityCandidateProbe decides whether one recorded PID is still the
	// process that wrote the log. An empty token means the process was already
	// gone when first seen.
	antigravityCandidateProbe = func(pid int, token string) processProbeResult {
		if token == "" {
			return processGone
		}
		return probeRecordedProcess(ledgerProcess{PID: pid, StartTime: token})
	}
	// antigravityProcessStartToken reads a live PID's start identity.
	antigravityProcessStartToken = processStartToken
	// antigravityProcessScan lists live allowlisted CLI processes; ok=false
	// means the list could not be read (never "none running").
	antigravityProcessScan = ScanCLIProcessesChecked
)

// antigravityLogNamePattern is the second-stamped run-log name.
var antigravityLogNamePattern = regexp.MustCompile(`^cli-(\d{8})_(\d{6})`)

type antigravityLogClass int

const (
	// antigravityLogSettled: nothing is owed for this file (managed, own,
	// older than the reading, or already owed).
	antigravityLogSettled antigravityLogClass = iota
	antigravityLogOwned
	antigravityLogCandidate
	antigravityLogNoPID
)

type antigravityTrackedPID struct {
	pid   int
	token string
}

type antigravityLogEntry struct {
	path   string
	base   string
	nameAt time.Time
	class  antigravityLogClass
	size   int64
	mtime  time.Time
	pids   []antigravityTrackedPID
	// pidOverflow: the log named more foreign PIDs than are tracked, so the run
	// is live until the hold limit.
	pidOverflow bool
	firstSeen   time.Time
	exitSeenAt  time.Time
	// floorMs is the protected floor armed for this candidate (0 = none).
	floorMs int64
	// grace: evicted by a cap, still watched until the collision window ends.
	grace bool
}

// antigravityProcessOnly is a live PID of an evicted candidate, tracked
// without its log.
type antigravityProcessOnly struct {
	antigravityTrackedPID
	firstSeen  time.Time
	exitSeenAt time.Time
	floorMs    int64
}

// antigravitySentinel stands in for live PIDs past the process-only cap: it
// keeps the oldest of their floors armed.
type antigravitySentinel struct {
	firstSeen time.Time
	floorMs   int64
}

type antigravityLogStat struct {
	size  int64
	mtime time.Time
}

var antigravityLogIndex = struct {
	mu     sync.Mutex
	primed bool
	// known is every name listed by the previous pass, per base.
	known map[string]map[string]struct{}
	// entries holds classified files by path.
	entries map[string]*antigravityLogEntry
	// newestStat is the last stat of the newest names that carry no entry.
	newestStat  map[string]antigravityLogStat
	processOnly []*antigravityProcessOnly
	sentinel    *antigravitySentinel
	ownRunning  int
	ownPIDs     []int
	managedPIDs []int
	// pendingOwed is the newest owe-ready floor no reading has covered yet.
	pendingOwed time.Time
}{
	known:      map[string]map[string]struct{}{},
	entries:    map[string]*antigravityLogEntry{},
	newestStat: map[string]antigravityLogStat{},
}

/* ─────────────────────────── lock-order check ─────────────────────────── */

// antigravityLockOrderCheck, set by tests, makes taking the live-runs or the
// freshness lock panic when the SAME goroutine holds the index lock.
var (
	antigravityLockOrderCheck atomic.Bool
	antigravityIndexHolder    atomic.Int64
)

func lockAntigravityLogIndex() {
	antigravityLogIndex.mu.Lock()
	if antigravityLockOrderCheck.Load() {
		antigravityIndexHolder.Store(antigravityGoroutineID())
	}
}

func unlockAntigravityLogIndex() {
	if antigravityLockOrderCheck.Load() {
		antigravityIndexHolder.Store(0)
	}
	antigravityLogIndex.mu.Unlock()
}

// antigravityAssertIndexUnlocked is called before the live-runs and freshness
// locks are taken.
func antigravityAssertIndexUnlocked(lock string) {
	if !antigravityLockOrderCheck.Load() {
		return
	}
	if holder := antigravityIndexHolder.Load(); holder != 0 && holder == antigravityGoroutineID() {
		panic("antigravity lock order: index lock held while taking " + lock)
	}
}

// antigravityGoroutineID is the current goroutine's id. Test-only cost: it is
// read only while antigravityLockOrderCheck is set.
func antigravityGoroutineID() int64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	if i := bytes.IndexByte(buf, ' '); i > 0 {
		id, _ := strconv.ParseInt(string(buf[:i]), 10, 64)
		return id
	}
	return 0
}

/* ─────────────────────────── log reading ─────────────────────────── */

// antigravityLogNameTime is the second a run-log name records, in local time.
// Names without the stamp (a legacy spelling) fall back to the file's mtime.
func antigravityLogNameTime(name string, mtime time.Time) (time.Time, bool) {
	m := antigravityLogNamePattern.FindStringSubmatch(name)
	if m == nil {
		return mtime.Truncate(time.Second), false
	}
	at, err := time.ParseInLocation("20060102150405", m[1]+m[2], time.Local)
	if err != nil {
		return mtime.Truncate(time.Second), false
	}
	return at, true
}

// antigravityIndexableLog reports whether a directory entry is a run log the
// index tracks. cli.log is a hard link to the newest run log, so indexing it
// would count that run twice.
func antigravityIndexableLog(name string) bool {
	return strings.HasSuffix(name, ".log") && !strings.EqualFold(name, "cli.log")
}

// antigravityListLogNames lists base's run-log names (names only; no stat).
func antigravityListLogNames(base string) ([]string, bool) {
	dir := antigravityLogDir(base)
	if dir == "" {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && antigravityIndexableLog(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	// Second-stamped names sort chronologically; newest first.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, true
}

func antigravityStatLog(path string) (antigravityLogStat, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return antigravityLogStat{}, false
	}
	return antigravityLogStat{size: info.Size(), mtime: info.ModTime()}, true
}

// antigravityReadLog reads a log for PID blocks: whole up to
// antigravityOwnLogMaxBytes, head and tail beyond that.
func antigravityReadLog(path string) ([]byte, bool) {
	body, err := readBoundedHeadTail(path, antigravityOwnLogMaxBytes/2)
	return body, err == nil
}

// antigravityLogPIDs lists the PIDs whose blocks a log holds, in file order,
// each once.
func antigravityLogPIDs(body []byte) []int {
	var pids []int
	seen := map[int]bool{}
	for _, m := range antigravityLanguageServerPIDPattern.FindAllSubmatch(body, -1) {
		pid, err := strconv.Atoi(string(m[1]))
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		pids = append(pids, pid)
	}
	return pids
}

// antigravityPIDBlockIn returns the block the run with pid wrote: from its
// startup line to the next run's startup line, or the end of the file.
func antigravityPIDBlockIn(body []byte, pid int) ([]byte, bool) {
	want := strconv.Itoa(pid)
	starts := antigravityLanguageServerPIDPattern.FindAllSubmatchIndex(body, -1)
	for i, loc := range starts {
		if string(body[loc[2]:loc[3]]) != want {
			continue
		}
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		return body[loc[0]:end], true
	}
	return nil, false
}

// antigravityPIDBlock returns pid's block from the newest logs under base whose
// name-second is not before sinceFloor (less the DST slack). It generalises
// antigravityHTTPPortInPIDBlock: two runs that share a second-stamped file are
// kept apart by their blocks.
func antigravityPIDBlock(base string, pid int, sinceFloor time.Time) ([]byte, bool) {
	if pid <= 0 {
		return nil, false
	}
	names, ok := antigravityListLogNames(base)
	if !ok {
		return nil, false
	}
	dir := antigravityLogDir(base)
	since := sinceFloor.Add(-antigravityStartupDSTSlack).Truncate(time.Second)
	examined := 0
	for _, name := range names {
		if examined >= antigravityWatchedNewest {
			break
		}
		if at, stamped := antigravityLogNameTime(name, time.Time{}); stamped && at.Before(since) {
			break
		}
		examined++
		body, ok := antigravityReadLog(filepath.Join(dir, name))
		if !ok {
			continue
		}
		if block, found := antigravityPIDBlockIn(body, pid); found {
			return block, true
		}
	}
	return nil, false
}

/* ─────────────────────────── own children ─────────────────────────── */

// antigravityOwnChild is one `agy` process the agent itself starts (model
// discovery, the Refresh click's live probe). Its logs are never runs to
// refresh for.
type antigravityOwnChild struct {
	bases  []string
	before map[string]struct{}
	pid    atomic.Int64
	once   sync.Once
}

// beginAntigravityOwnChild records the log names present before an own child
// starts and counts it as running. The caller registers `defer child.done()`
// immediately, so a start failure, an early return or a panic releases it.
func beginAntigravityOwnChild(home string) *antigravityOwnChild {
	child := &antigravityOwnChild{bases: antigravityQuotaBases(home), before: map[string]struct{}{}}
	for _, base := range child.bases {
		names, _ := antigravityListLogNames(base)
		for _, name := range names {
			child.before[filepath.Join(antigravityLogDir(base), name)] = struct{}{}
		}
	}
	lockAntigravityLogIndex()
	antigravityLogIndex.ownRunning++
	unlockAntigravityLogIndex()
	return child
}

// setPID records the started child's PID. Runs after Start.
func (c *antigravityOwnChild) setPID(pid int) {
	if c == nil || pid <= 0 {
		return
	}
	c.pid.Store(int64(pid))
	lockAntigravityLogIndex()
	antigravityLogIndex.ownPIDs = antigravityPushPID(antigravityLogIndex.ownPIDs, pid)
	unlockAntigravityLogIndex()
}

// done releases the running count and claims the child's logs: up to
// antigravityOwnChildMaxFiles new files, each read whole, owned only when its
// SOLE PID block is the child's and it did not change while being read.
// Idempotent.
func (c *antigravityOwnChild) done() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		type claim struct {
			path, base string
			stat       antigravityLogStat
		}
		var claims []claim
		if pid := int(c.pid.Load()); pid > 0 {
			examined := 0
			for _, base := range c.bases {
				names, _ := antigravityListLogNames(base)
				for _, name := range names {
					path := filepath.Join(antigravityLogDir(base), name)
					if _, existed := c.before[path]; existed {
						continue
					}
					if examined >= antigravityOwnChildMaxFiles {
						break
					}
					examined++
					before, ok := antigravityStatLog(path)
					if !ok || before.size > antigravityOwnLogMaxBytes {
						continue
					}
					body, ok := antigravityReadLog(path)
					if !ok {
						continue
					}
					if pids := antigravityLogPIDs(body); len(pids) != 1 || pids[0] != pid {
						continue
					}
					if after, ok := antigravityStatLog(path); !ok || after != before {
						continue
					}
					claims = append(claims, claim{path: path, base: base, stat: before})
				}
			}
		}

		var released []int64
		lockAntigravityLogIndex()
		antigravityLogIndex.ownRunning--
		if antigravityLogIndex.ownRunning < 0 {
			antigravityLogIndex.ownRunning = 0
		}
		for _, cl := range claims {
			entry := antigravityLogIndex.entries[cl.path]
			if entry == nil {
				nameAt, _ := antigravityLogNameTime(filepath.Base(cl.path), cl.stat.mtime)
				entry = &antigravityLogEntry{path: cl.path, base: cl.base, nameAt: nameAt, firstSeen: cl.stat.mtime}
				antigravityLogIndex.entries[cl.path] = entry
			}
			if entry.floorMs != 0 {
				released = append(released, entry.floorMs)
				entry.floorMs = 0
			}
			entry.class, entry.size, entry.mtime = antigravityLogOwned, cl.stat.size, cl.stat.mtime
			entry.pids, entry.pidOverflow, entry.exitSeenAt, entry.grace = nil, false, time.Time{}, false
			if names := antigravityLogIndex.known[cl.base]; names != nil {
				names[filepath.Base(cl.path)] = struct{}{}
			}
		}
		unlockAntigravityLogIndex()
		for _, floorMs := range released {
			releaseAntigravityCandidateFloor(floorMs)
		}
	})
}

// antigravityOwnChildRunning reports whether an own child is running now.
func antigravityOwnChildRunning() bool {
	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()
	return antigravityLogIndex.ownRunning > 0
}

// noteAntigravityManagedPID records the agy PID of a managed run, so its log is
// never mistaken for a direct run.
func noteAntigravityManagedPID(pid int) {
	if pid <= 0 {
		return
	}
	lockAntigravityLogIndex()
	antigravityLogIndex.managedPIDs = antigravityPushPID(antigravityLogIndex.managedPIDs, pid)
	unlockAntigravityLogIndex()
}

func antigravityPushPID(ring []int, pid int) []int {
	for _, known := range ring {
		if known == pid {
			return ring
		}
	}
	ring = append(ring, pid)
	if len(ring) > antigravityKnownPIDCap {
		ring = ring[len(ring)-antigravityKnownPIDCap:]
	}
	return ring
}

func antigravityPIDIn(ring []int, pid int) bool {
	for _, known := range ring {
		if known == pid {
			return true
		}
	}
	return false
}

/* ─────────────────────────── process start hook ─────────────────────────── */

type antigravityProcessStartHookKey struct{}

// withProcessStartHook attaches fn to ctx; a runner that supports it calls fn
// with the child's PID right after Start.
func withProcessStartHook(ctx context.Context, fn func(pid int)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, antigravityProcessStartHookKey{}, fn)
}

// processStartHookFrom returns the hook attached to ctx, or a no-op.
func processStartHookFrom(ctx context.Context) func(pid int) {
	if ctx != nil {
		if fn, ok := ctx.Value(antigravityProcessStartHookKey{}).(func(pid int)); ok && fn != nil {
			return fn
		}
	}
	return func(int) {}
}

/* ─────────────────────────── discovery pass ─────────────────────────── */

// antigravityDiscoveryResult is what a pass decided, applied after the index
// lock is released.
type antigravityDiscoveryResult struct {
	arm     []int64
	release []int64
	// owed is the newest owe-ready floor no reading covers yet (zero = none).
	owed time.Time
	// heldOwed counts candidates owed at the hold limit this pass.
	heldOwed int
	// needScan: the sentinel is armed and may be released by a checked scan.
	needScan bool
}

// antigravityDiscoveryPass runs one pass over bases. observedMs is the cached
// reading's instant (read by the caller BEFORE the index lock: the cache lock
// is never taken under it).
func antigravityDiscoveryPass(bases []string, now time.Time, observedMs int64) antigravityDiscoveryResult {
	idx := &antigravityLogIndex
	var res antigravityDiscoveryResult

	// Listing and stat'ing happen before the lock: names only, then the few
	// files the pass needs.
	listings := map[string][]string{}
	for _, base := range bases {
		if names, ok := antigravityListLogNames(base); ok {
			listings[base] = names
		}
	}

	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()

	observed := time.UnixMilli(observedMs)
	var fresh []*antigravityLogEntry
	type newestName struct {
		path, base string
		nameAt     time.Time
	}
	var newest []newestName
	for _, base := range bases {
		names, listed := listings[base]
		if !listed {
			continue
		}
		dir := antigravityLogDir(base)
		prev := idx.known[base]
		current := make(map[string]struct{}, len(names))
		startupTaken := 0
		for _, name := range names {
			current[name] = struct{}{}
			path := filepath.Join(dir, name)
			nameAt, _ := antigravityLogNameTime(name, now)
			newest = append(newest, newestName{path: path, base: base, nameAt: nameAt})
			isNew := false
			if !idx.primed {
				// Startup scan: names at or after the cached reading, less the
				// DST slack, capped at the newest few hundred.
				if startupTaken < antigravityStartupScanCap &&
					(observedMs == 0 || !nameAt.Before(observed.Add(-antigravityStartupDSTSlack))) {
					isNew = true
					startupTaken++
				}
			} else if _, seen := prev[name]; !seen {
				isNew = true
			}
			if !isNew {
				continue
			}
			if _, tracked := idx.entries[path]; tracked {
				continue
			}
			fresh = append(fresh, &antigravityLogEntry{path: path, base: base, nameAt: nameAt, firstSeen: now})
		}
		idx.known[base] = current
	}
	idx.primed = true

	for _, entry := range fresh {
		stat, ok := antigravityStatLog(entry.path)
		if !ok {
			continue
		}
		idx.entries[entry.path] = entry
		antigravityClassifyLogLocked(entry, stat, now, observedMs, &res)
	}

	// Watched set: the newest names, then every owned / candidate / noPID /
	// grace entry. A size or mtime change reclassifies the file.
	sort.SliceStable(newest, func(i, j int) bool { return newest[i].nameAt.After(newest[j].nameAt) })
	if len(newest) > antigravityWatchedNewest {
		newest = newest[:antigravityWatchedNewest]
	}
	watchedNewest := map[string]bool{}
	for _, n := range newest {
		watchedNewest[n.path] = true
		stat, ok := antigravityStatLog(n.path)
		if !ok {
			continue
		}
		if entry := idx.entries[n.path]; entry != nil {
			if stat != (antigravityLogStat{size: entry.size, mtime: entry.mtime}) {
				antigravityClassifyLogLocked(entry, stat, now, observedMs, &res)
			}
			continue
		}
		prevStat, seen := idx.newestStat[n.path]
		idx.newestStat[n.path] = stat
		if seen && prevStat != stat {
			entry := &antigravityLogEntry{path: n.path, base: n.base, nameAt: n.nameAt, firstSeen: now}
			idx.entries[n.path] = entry
			delete(idx.newestStat, n.path)
			antigravityClassifyLogLocked(entry, stat, now, observedMs, &res)
		}
	}
	for path := range idx.newestStat {
		if !watchedNewest[path] {
			delete(idx.newestStat, path)
		}
	}
	for path, entry := range idx.entries {
		if watchedNewest[path] || entry.class == antigravityLogSettled {
			continue
		}
		stat, ok := antigravityStatLog(path)
		if !ok {
			// Gone from disk: nothing more can be learned from it.
			antigravityDropEntryLocked(entry, now, observedMs, &res)
			continue
		}
		if stat != (antigravityLogStat{size: entry.size, mtime: entry.mtime}) {
			antigravityClassifyLogLocked(entry, stat, now, observedMs, &res)
		}
	}

	antigravityEvaluateLocked(now, observedMs, &res)
	antigravityEnforceCapsLocked(now, observedMs, &res)
	// Settled entries outside the newest names are forgotten; their names stay
	// in known, so they are never classified again.
	for path, entry := range idx.entries {
		if entry.class == antigravityLogSettled && !watchedNewest[path] {
			delete(idx.entries, path)
		}
	}

	if !idx.pendingOwed.IsZero() && observedMs >= idx.pendingOwed.UnixMilli() {
		idx.pendingOwed = time.Time{}
	}
	res.owed = idx.pendingOwed
	res.needScan = idx.sentinel != nil
	return res
}

// antigravityClassifyLogLocked (re)reads one log and classifies it.
func antigravityClassifyLogLocked(entry *antigravityLogEntry, stat antigravityLogStat, now time.Time, observedMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	wasOwned := entry.class == antigravityLogOwned
	entry.size, entry.mtime = stat.size, stat.mtime
	body, ok := antigravityReadLog(entry.path)
	if !ok {
		return
	}
	pids := antigravityLogPIDs(body)
	var foreign []antigravityTrackedPID
	overflow := false
	allOwn := true
	for _, pid := range pids {
		if antigravityPIDIn(idx.ownPIDs, pid) {
			continue
		}
		allOwn = false
		if antigravityPIDIn(idx.managedPIDs, pid) {
			continue
		}
		if known := antigravityTrackedFind(entry.pids, pid); known != nil {
			foreign = append(foreign, *known)
			continue
		}
		if len(foreign) >= antigravityCandidateMaxPIDs {
			overflow = true
			continue
		}
		token, err := antigravityProcessStartToken(pid)
		if err != nil {
			token = ""
		}
		foreign = append(foreign, antigravityTrackedPID{pid: pid, token: token})
	}
	switch {
	case len(pids) == 0:
		if entry.class == antigravityLogOwned || entry.class == antigravityLogCandidate {
			// A file only ever gains blocks; nothing to reclassify.
			return
		}
		if !entry.mtime.After(time.UnixMilli(observedMs)) && observedMs > 0 {
			entry.class = antigravityLogSettled
			return
		}
		entry.class = antigravityLogNoPID
	case allOwn:
		// Only our own children's blocks: owned (a file done() has not claimed
		// yet, or one it claimed that another own child appended to).
		if entry.class != antigravityLogOwned {
			antigravityReleaseEntryFloorLocked(entry, res)
		}
		entry.class = antigravityLogOwned
	case len(foreign) == 0 && !overflow:
		// Managed runs (and own children): their own settles cover them.
		antigravityReleaseEntryFloorLocked(entry, res)
		entry.class = antigravityLogSettled
	default:
		if wasOwned || entry.class != antigravityLogCandidate {
			entry.exitSeenAt = time.Time{}
		}
		entry.pids, entry.pidOverflow = foreign, entry.pidOverflow || overflow
		entry.class = antigravityLogCandidate
		entry.grace = false
	}
}

func antigravityTrackedFind(pids []antigravityTrackedPID, pid int) *antigravityTrackedPID {
	for i := range pids {
		if pids[i].pid == pid {
			return &pids[i]
		}
	}
	return nil
}

// antigravityEvaluateLocked decides, for every candidate, noPID log and
// process-only PID, whether it is live, released or owed.
func antigravityEvaluateLocked(now time.Time, observedMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	for _, entry := range idx.entries {
		switch entry.class {
		case antigravityLogCandidate:
			antigravityEvaluateCandidateLocked(entry, now, observedMs, res)
		case antigravityLogNoPID:
			if observedMs > 0 && observedMs >= entry.mtime.UnixMilli() {
				entry.class = antigravityLogSettled
				continue
			}
			if now.Sub(entry.mtime) >= antigravityCandidateSettle && !entry.grace {
				antigravityOweLocked(entry.mtime)
				entry.class = antigravityLogSettled
			}
		}
	}

	kept := idx.processOnly[:0]
	for _, p := range idx.processOnly {
		switch antigravityCandidateProbe(p.pid, p.token) {
		case processGone:
			if p.exitSeenAt.IsZero() {
				p.exitSeenAt = now
			}
		default:
			p.exitSeenAt = time.Time{}
		}
		switch {
		case !p.exitSeenAt.IsZero() && observedMs >= p.exitSeenAt.UnixMilli():
			res.release = append(res.release, p.floorMs)
		case !p.exitSeenAt.IsZero() && now.Sub(p.exitSeenAt) >= antigravityCandidateSettle:
			antigravityOweLocked(p.exitSeenAt)
			res.release = append(res.release, p.floorMs)
		case p.exitSeenAt.IsZero() && now.Sub(p.firstSeen) >= antigravityCandidateHoldLimit:
			antigravityOweLocked(now)
			res.heldOwed++
			res.release = append(res.release, p.floorMs)
		default:
			kept = append(kept, p)
		}
	}
	idx.processOnly = kept

	if s := idx.sentinel; s != nil && now.Sub(s.firstSeen) >= antigravityCandidateHoldLimit {
		antigravityOweLocked(now)
		res.heldOwed++
		res.release = append(res.release, s.floorMs)
		idx.sentinel = nil
	}
}

func antigravityEvaluateCandidateLocked(entry *antigravityLogEntry, now time.Time, observedMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	// A PID resolved as managed after the log was first seen leaves the
	// candidate: that run's own settle covers it.
	kept := entry.pids[:0]
	for _, p := range entry.pids {
		if !antigravityPIDIn(idx.managedPIDs, p.pid) && !antigravityPIDIn(idx.ownPIDs, p.pid) {
			kept = append(kept, p)
		}
	}
	entry.pids = kept
	if len(entry.pids) == 0 && !entry.pidOverflow {
		antigravityReleaseEntryFloorLocked(entry, res)
		entry.class = antigravityLogSettled
		return
	}

	live, ours := entry.pidOverflow, false
	for _, p := range entry.pids {
		switch antigravityCandidateProbe(p.pid, p.token) {
		case processOurs:
			live, ours = true, true
		case processUnknown:
			live = true
		}
	}
	if live {
		entry.exitSeenAt = time.Time{}
		if entry.floorMs == 0 {
			entry.floorMs = entry.firstSeen.UnixMilli()
			res.arm = append(res.arm, entry.floorMs)
		}
		if !ours && now.Sub(entry.firstSeen) >= antigravityCandidateHoldLimit {
			// Held only by PIDs that cannot be read (or more than are
			// tracked) for the whole hold limit: owe it rather than hold forever.
			antigravityOweLocked(now)
			res.heldOwed++
			antigravityReleaseEntryFloorLocked(entry, res)
			entry.class = antigravityLogSettled
		}
		return
	}
	if entry.exitSeenAt.IsZero() {
		entry.exitSeenAt = now
	}
	if observedMs > 0 && observedMs >= entry.exitSeenAt.UnixMilli() {
		// A reading landed after the run was seen gone: it holds the usage.
		antigravityReleaseEntryFloorLocked(entry, res)
		entry.class = antigravityLogSettled
		return
	}
	if !entry.mtime.After(time.UnixMilli(observedMs)) && observedMs > 0 && entry.floorMs == 0 {
		// Exited, and never live while watched, before the reading was
		// taken: that reading already covers it.
		entry.class = antigravityLogSettled
		return
	}
	if now.Sub(entry.exitSeenAt) >= antigravityCandidateSettle || now.Sub(entry.mtime) >= antigravityCandidateSettle {
		floor := entry.mtime
		if entry.exitSeenAt.After(floor) {
			floor = entry.exitSeenAt
		}
		antigravityOweLocked(floor)
		antigravityReleaseEntryFloorLocked(entry, res)
		entry.class = antigravityLogSettled
	}
}

// antigravityOweLocked records an owe-ready floor.
func antigravityOweLocked(floor time.Time) {
	if floor.After(antigravityLogIndex.pendingOwed) {
		antigravityLogIndex.pendingOwed = floor
	}
}

func antigravityReleaseEntryFloorLocked(entry *antigravityLogEntry, res *antigravityDiscoveryResult) {
	if entry.floorMs != 0 {
		res.release = append(res.release, entry.floorMs)
		entry.floorMs = 0
	}
}

// antigravityDropEntryLocked forgets an entry. A noPID log is never dropped
// silently: one newer than the reading is owed on the way out, and a live
// candidate moves to process-only tracking.
func antigravityDropEntryLocked(entry *antigravityLogEntry, now time.Time, observedMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	switch entry.class {
	case antigravityLogNoPID:
		if observedMs == 0 || entry.mtime.UnixMilli() > observedMs {
			antigravityOweLocked(entry.mtime)
		}
	case antigravityLogCandidate:
		if entry.exitSeenAt.IsZero() {
			antigravityTrackProcessOnlyLocked(entry, res)
		} else {
			floor := entry.mtime
			if entry.exitSeenAt.After(floor) {
				floor = entry.exitSeenAt
			}
			antigravityOweLocked(floor)
			antigravityReleaseEntryFloorLocked(entry, res)
		}
	}
	delete(idx.entries, entry.path)
}

// antigravityTrackProcessOnlyLocked moves an evicted live candidate's PIDs to
// process-only tracking; its floor moves with the first, and past the cap the
// sentinel keeps the oldest floor armed.
func antigravityTrackProcessOnlyLocked(entry *antigravityLogEntry, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	floorMs := entry.floorMs
	entry.floorMs = 0
	if floorMs == 0 {
		floorMs = entry.firstSeen.UnixMilli()
		res.arm = append(res.arm, floorMs)
	}
	pids := entry.pids
	if len(pids) == 0 {
		// pidOverflow with nothing trackable: the sentinel holds it.
		pids = []antigravityTrackedPID{{}}
	}
	for i, p := range pids {
		ownFloor := int64(0)
		if i == 0 {
			ownFloor = floorMs
		}
		if p.pid > 0 && len(idx.processOnly) < antigravityProcessOnlyCap {
			idx.processOnly = append(idx.processOnly, &antigravityProcessOnly{
				antigravityTrackedPID: p, firstSeen: entry.firstSeen, floorMs: ownFloor,
			})
			continue
		}
		antigravitySentinelLocked(entry.firstSeen, ownFloor, res)
	}
}

func antigravitySentinelLocked(firstSeen time.Time, floorMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	if idx.sentinel == nil {
		idx.sentinel = &antigravitySentinel{firstSeen: firstSeen, floorMs: floorMs}
		return
	}
	s := idx.sentinel
	if firstSeen.Before(s.firstSeen) {
		s.firstSeen = firstSeen
	}
	switch {
	case floorMs == 0:
	case s.floorMs == 0 || floorMs < s.floorMs:
		if s.floorMs != 0 {
			res.release = append(res.release, s.floorMs)
		}
		s.floorMs = floorMs
	default:
		res.release = append(res.release, floorMs)
	}
}

// antigravityEnforceCapsLocked applies the owned / noPID / candidate caps
// (oldest evicted) and ages the collision-grace list.
func antigravityEnforceCapsLocked(now time.Time, observedMs int64, res *antigravityDiscoveryResult) {
	idx := &antigravityLogIndex
	byClass := func(class antigravityLogClass, grace bool) []*antigravityLogEntry {
		var out []*antigravityLogEntry
		for _, entry := range idx.entries {
			if entry.class == class && entry.grace == grace {
				out = append(out, entry)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].nameAt.Equal(out[j].nameAt) {
				return out[i].nameAt.After(out[j].nameAt)
			}
			return out[i].path > out[j].path
		})
		return out
	}
	for _, class := range []antigravityLogClass{antigravityLogOwned, antigravityLogNoPID} {
		limit := antigravityOwnedCap
		if class == antigravityLogNoPID {
			limit = antigravityNoPIDCap
		}
		if list := byClass(class, false); len(list) > limit {
			for _, entry := range list[limit:] {
				entry.grace = true
			}
		}
	}
	if list := byClass(antigravityLogCandidate, false); len(list) > antigravityCandidateCap {
		for _, entry := range list[antigravityCandidateCap:] {
			antigravityDropEntryLocked(entry, now, observedMs, res)
		}
	}

	var grace []*antigravityLogEntry
	for _, entry := range idx.entries {
		if entry.grace {
			grace = append(grace, entry)
		}
	}
	sort.Slice(grace, func(i, j int) bool { return grace[i].nameAt.After(grace[j].nameAt) })
	for i, entry := range grace {
		expired := now.Sub(entry.nameAt) > antigravityCollisionGrace
		if expired || i >= antigravityGraceCap {
			antigravityDropEntryLocked(entry, now, observedMs, res)
		}
	}
}

/* ─────────────────────────── floors ─────────────────────────── */

var (
	// antigravityCandidateFloors counts, by floor, the direct-run candidates
	// armed and not yet released. Guarded by antigravityLiveRunsMu.
	antigravityCandidateFloors = map[int64]int{}
)

// armAntigravityCandidateFloor protects a live direct run's first-seen floor
// the way armAntigravityUsageRunFloor protects a managed run's: registered
// under the live-runs lock, persisted as RunFloorMs on a counted goroutine.
// Never called with the index lock held.
func armAntigravityCandidateFloor(floorMs int64) {
	if floorMs <= 0 {
		return
	}
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	antigravityCandidateFloors[floorMs]++
	antigravityLiveRunsMu.Unlock()
	antigravityFreshnessInFlight.Add(1)
	go func() {
		defer antigravityFreshnessInFlight.Add(-1)
		updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
			antigravityRebaseFutureFreshness(state, antigravityUsageFreshnessNow())
			if floorMs > state.RunFloorMs {
				state.RunFloorMs = floorMs
			}
		})
	}()
}

// releaseAntigravityCandidateFloor drops one candidate floor. A persisted
// RunFloorMs that only this candidate still named is rolled back to the oldest
// protected floor once a reading covers it, so a restart does not adopt a run
// that was already refreshed.
func releaseAntigravityCandidateFloor(floorMs int64) {
	if floorMs <= 0 {
		return
	}
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	if n := antigravityCandidateFloors[floorMs]; n > 1 {
		antigravityCandidateFloors[floorMs] = n - 1
	} else {
		delete(antigravityCandidateFloors, floorMs)
	}
	antigravityLiveRunsMu.Unlock()
	observedMs := cachedAntigravityObservedMs()
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		if state.RunFloorMs != 0 && state.RunFloorMs <= floorMs && observedMs >= state.RunFloorMs &&
			state.RefreshOwedAtMs == 0 {
			state.RunFloorMs = antigravityOldestProtectedFloorMs()
		}
	})
}

// antigravityOldestProtectedFloorMs is the oldest floor still protected: a
// managed run this process armed, or a live direct-run candidate. 0 when none.
// Takes the live-runs lock only; called under the freshness lock exactly where
// antigravityOldestLiveRunFloorMs was.
func antigravityOldestProtectedFloorMs() int64 {
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	defer antigravityLiveRunsMu.Unlock()
	oldest := int64(0)
	for _, floors := range []map[int64]int{antigravityLiveRuns, antigravityCandidateFloors} {
		for floorMs := range floors {
			if oldest == 0 || floorMs < oldest {
				oldest = floorMs
			}
		}
	}
	return oldest
}

/* ─────────────────────────── tick ─────────────────────────── */

var antigravityDiscovery struct {
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// startAntigravityDiscovery starts the process-wide discovery tick. StartAgent
// calls it; a second call is a no-op.
func startAntigravityDiscovery() {
	d := &antigravityDiscovery
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	d.stop, d.done = stop, done
	interval := antigravityDiscoveryInterval
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				antigravityDiscoveryTick()
			}
		}
	}()
}

// stopAntigravityDiscovery stops the tick and waits for a pass in flight.
func stopAntigravityDiscovery() {
	d := &antigravityDiscovery
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.stop, d.done = nil, nil
	d.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// antigravityDiscoveryRunning reports whether the tick is running.
func antigravityDiscoveryRunning() bool {
	d := &antigravityDiscovery
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stop != nil
}

// antigravityDiscoveryTick is one tick: a pass while `agy` is installed, then
// the nudge for anything owed.
func antigravityDiscoveryTick() {
	if IsShutdownInProgress() || antigravityExecutablePath() == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	now := antigravityUsageFreshnessNow()
	owed := antigravityDiscover(antigravityQuotaBases(home), now)
	if owed.IsZero() || antigravityOwnChildRunning() {
		return
	}
	observedAt := ""
	if snap, ok := cachedAntigravityQuotaSnapshot(); ok {
		observedAt = snap.ObservedAt
	}
	nudgeAntigravityUsageRefresh(now, observedAt, owed)
}

// antigravityDiscover runs one pass and applies its floors; returns the newest
// owe-ready floor no reading covers yet.
func antigravityDiscover(bases []string, now time.Time) time.Time {
	res := antigravityDiscoveryPass(bases, now, cachedAntigravityObservedMs())
	antigravityApplyDiscovery(res, now)
	return res.owed
}

func antigravityApplyDiscovery(res antigravityDiscoveryResult, now time.Time) {
	for _, floorMs := range res.arm {
		armAntigravityCandidateFloor(floorMs)
	}
	for _, floorMs := range res.release {
		releaseAntigravityCandidateFloor(floorMs)
	}
	if res.heldOwed > 0 {
		fmt.Printf("%s[antigravity-discovery] candidates owed at the hold limit: count=%d%s\n",
			colorYellow, res.heldOwed, colorReset)
	}
	if res.needScan {
		antigravityCheckSentinel(now)
	}
}

// antigravityCheckSentinel releases the sentinel on a checked scan that finds
// no unmanaged `agy` the index is not already tracking.
func antigravityCheckSentinel(now time.Time) {
	procs, ok := antigravityProcessScan()
	if !ok {
		return
	}
	var release int64
	lockAntigravityLogIndex()
	idx := &antigravityLogIndex
	if idx.sentinel != nil {
		untracked := false
		for _, p := range procs {
			if !strings.HasPrefix(strings.ToLower(p.Name), "agy") ||
				antigravityPIDIn(idx.ownPIDs, p.PID) || antigravityPIDIn(idx.managedPIDs, p.PID) ||
				globalProcessRegistry.IsRegistered(p.PID) || globalProcessRegistry.IsRegistered(p.ParentPID) {
				continue
			}
			tracked := false
			for _, po := range idx.processOnly {
				if po.pid == p.PID {
					tracked = true
					break
				}
			}
			if !tracked {
				untracked = true
				break
			}
		}
		if !untracked {
			antigravityOweLocked(now)
			release = idx.sentinel.floorMs
			idx.sentinel = nil
		}
	}
	unlockAntigravityLogIndex()
	releaseAntigravityCandidateFloor(release)
}

// antigravityStartupScan runs the first pass (the startup scan) and adds live
// unmanaged `agy` processes the listing cannot tie to a log. Its floors are
// NOT applied: the caller adopts the previous process's floor first, then
// applies them, so a candidate armed now cannot mask the floor a crashed or
// updated process left behind.
func antigravityStartupScan(now time.Time) (antigravityDiscoveryResult, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return antigravityDiscoveryResult{}, false
	}
	lockAntigravityLogIndex()
	primed := antigravityLogIndex.primed
	unlockAntigravityLogIndex()
	if primed {
		return antigravityDiscoveryResult{}, false
	}
	res := antigravityDiscoveryPass(antigravityQuotaBases(home), now, cachedAntigravityObservedMs())
	if procs, ok := antigravityProcessScan(); ok {
		lockAntigravityLogIndex()
		idx := &antigravityLogIndex
		for _, p := range procs {
			if !strings.HasPrefix(strings.ToLower(p.Name), "agy") ||
				antigravityPIDIn(idx.ownPIDs, p.PID) || antigravityPIDIn(idx.managedPIDs, p.PID) ||
				globalProcessRegistry.IsRegistered(p.PID) || globalProcessRegistry.IsRegistered(p.ParentPID) {
				continue
			}
			if antigravityIndexTracksPIDLocked(p.PID) {
				continue
			}
			token, err := antigravityProcessStartToken(p.PID)
			if err != nil {
				continue
			}
			entry := &antigravityLogEntry{firstSeen: now, pids: []antigravityTrackedPID{{pid: p.PID, token: token}}}
			antigravityTrackProcessOnlyLocked(entry, &res)
		}
		unlockAntigravityLogIndex()
	}
	return res, true
}

func antigravityIndexTracksPIDLocked(pid int) bool {
	idx := &antigravityLogIndex
	for _, entry := range idx.entries {
		if antigravityTrackedFind(entry.pids, pid) != nil {
			return true
		}
	}
	for _, po := range idx.processOnly {
		if po.pid == pid {
			return true
		}
	}
	return false
}

// antigravityNewestOwedLog is the gather's view: the newest owe-ready floor
// under bases (zero when none). It runs a pass, so a gather sees what the tick
// would, but never nudges — the gather does that itself.
func antigravityNewestOwedLog(bases []string, now time.Time) time.Time {
	return antigravityDiscover(bases, now)
}

// resetAntigravityLogIndex clears the index. Tests only.
func resetAntigravityLogIndex() {
	lockAntigravityLogIndex()
	idx := &antigravityLogIndex
	idx.primed = false
	idx.known = map[string]map[string]struct{}{}
	idx.entries = map[string]*antigravityLogEntry{}
	idx.newestStat = map[string]antigravityLogStat{}
	idx.processOnly, idx.sentinel = nil, nil
	idx.ownRunning, idx.ownPIDs, idx.managedPIDs = 0, nil, nil
	idx.pendingOwed = time.Time{}
	unlockAntigravityLogIndex()
	antigravityLiveRunsMu.Lock()
	antigravityCandidateFloors = map[int64]int{}
	antigravityLiveRunsMu.Unlock()
}

// antigravityCandidatePaths lists the paths of base's current candidates, so a
// bounded log listing still sees a long direct session whose log has fallen
// out of the newest names.
func antigravityCandidatePaths(base string) []string {
	lockAntigravityLogIndex()
	defer unlockAntigravityLogIndex()
	var paths []string
	for path, entry := range antigravityLogIndex.entries {
		if entry.base == base && entry.class == antigravityLogCandidate {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

/* ─────────────────────────── block evidence ─────────────────────────── */

var (
	// antigravityAuthenticatedPattern is the login line each run writes.
	antigravityAuthenticatedPattern = regexp.MustCompile(`authenticated successfully as ([^\s"'<>]+@[^\s"'<>]+)`)
	// antigravityExhaustedPattern is a quota refusal with its reset, e.g.
	// "RESOURCE_EXHAUSTED (code 429): ... Resets in 1h39m21s."
	antigravityExhaustedPattern = regexp.MustCompile(`RESOURCE_EXHAUSTED[^\n]*?Resets in ((?:\d+h)?(?:\d+m)?(?:\d+(?:\.\d+)?s)?)`)
	// antigravityGlogPrefix is the glog header a line may carry
	// ("I0929 20:43:56.123456 ..."), local time, no year.
	antigravityGlogPrefix = regexp.MustCompile(`^[IWEF](\d{2})(\d{2}) (\d{2}):(\d{2}):(\d{2})`)
)

// antigravityBlockEvidence extracts, from ONE run's block, the account it
// authenticated as and the instants its quota refusals said the quota resets.
// A reset is anchored to its line's own glog time when the line carries one,
// else to at. Nothing here is logged or kept beyond the caller.
func antigravityBlockEvidence(block []byte, at time.Time) (email string, resets []time.Time) {
	if m := antigravityAuthenticatedPattern.FindSubmatch(block); m != nil {
		email = strings.TrimRight(string(m[1]), ".,;:)]")
	}
	for _, line := range bytes.Split(block, []byte("\n")) {
		m := antigravityExhaustedPattern.FindSubmatch(line)
		if m == nil || len(m[1]) == 0 {
			continue
		}
		d, err := time.ParseDuration(string(m[1]))
		if err != nil || d <= 0 {
			continue
		}
		resets = append(resets, antigravityLineTime(line, at).Add(d))
	}
	return email, resets
}

// antigravityLineTime is a glog line's local time, or at when it has none.
func antigravityLineTime(line []byte, at time.Time) time.Time {
	m := antigravityGlogPrefix.FindSubmatch(bytes.TrimSpace(line))
	if m == nil {
		return at
	}
	parts := make([]int, 5)
	for i := range parts {
		parts[i], _ = strconv.Atoi(string(m[i+1]))
	}
	local := at.In(time.Local)
	t := time.Date(local.Year(), time.Month(parts[0]), parts[1], parts[2], parts[3], parts[4], 0, time.Local)
	if t.After(at.Add(24 * time.Hour)) {
		// A December line read in January.
		t = t.AddDate(-1, 0, 0)
	}
	return t
}
