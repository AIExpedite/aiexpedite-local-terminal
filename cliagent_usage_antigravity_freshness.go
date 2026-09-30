// cliagent_usage_antigravity_freshness.go — run-completion freshness for
// Antigravity utilization.
//
// Why this exists:
//
//	Antigravity utilization is only ever observed while `agy` is running: the
//	numbers live in the language server each run starts on loopback, and that
//	server dies with the process (cliagent_usage_antigravity_capture.go). Since
//	`agy` 1.2.2 that server refuses every quota RPC without a per-run CSRF token
//	nothing headless can obtain (cliagent_usage_antigravity_gate.go), so on
//	every current build the in-run poller captures NOTHING and a finished run
//	leaves the CLI Agents card pinned to the observedAt it already had. The one
//	route that still returns numbers — the Code Assist read of the keyring login
//	(cliagent_usage_antigravity_codeassist.go) — was wired to the Refresh click
//	only, so freshness needed a human. Codex closes the same gap with
//	cliagent_usage_codex_freshness.go and Claude with
//	triggerClaudeUsageProbeAfterRun; this is Antigravity's equivalent, at a
//	fraction of the size (no rollout scanning, no cursor, one outbound read).
//
// Lifecycle (every spawn path, because all five reach startAntigravityQuotaCapture):
//
//   - Arm: a run start persists RunFloorMs (armAntigravityUsageRunFloor) and
//     hands that run its own floor.
//   - Settle: run completion (antigravityUsageRunSettled) compares the CACHED
//     reading against the run's COMPLETION — never a flag, and never merely the
//     run's start, since a turn's quota is debited at its end — and, when
//     nothing covers it, persists the debt and hands it to a bounded
//     single-flight worker.
//   - Clear: settleAntigravityRunFreshness runs inside
//     writeAntigravityQuotaSnapshotLocked, so ANY route that lands a reading at
//     or after the floor (in-run loopback, Code Assist, a Refresh click, a
//     concurrent run's poller) retires the debt exactly once.
//   - Retry: a kept debt is never left with nothing scheduled to return to it.
//     antigravityScheduleRunDebtRetry persists NextAttemptAtMs and arms one
//     process-wide timer on a bounded ladder
//     (cliagent_usage_antigravity_refresh_schedule.go), and a gather or a
//     discovery tick that sees an owe-ready run newer than the cached reading
//     nudges the same worker.
//   - Discover: runs no spawn path armed (a direct `agy` in the user's own
//     shell) are found by the log index's discovery tick
//     (cliagent_usage_antigravity_log_index.go) and owed only once every
//     process of the run has exited; the agent's own `agy` children never owe.
//   - Survive: the debt and its schedule are a file, so StartAgent's
//     payOwedAntigravityUsageRefresh re-arms a pending schedule, or pays one
//     bounded attempt for a run the previous process never settled (crash,
//     restart, self-update).
//   - Report: a debt that outlived its attempts surfaces through
//     antigravityFreshnessNotice; while a gated build's own banner is the newer
//     fact, antigravityGateNotice owns the wording instead.
//
// Redaction: the state file holds schemaVersion, five epoch-millisecond fields,
// an attempt count, the gated bool, the hashed account fingerprint and a
// closed-set outcome code. Never a token, a keyring payload, settings.json
// contents, an account email, a command line, a prompt, a port or log text. The
// log lines below follow the same rule: fixed labels and counters only.
//
// Every path here is silent and bounded: it runs off run teardown and must
// never delay, block or break a run.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// antigravityRefreshAfterRunMaxAttempts bounds the Code Assist reads the
	// settle-driven pass may spend (immediate, then one retry after
	// antigravityRefreshAfterRunRetryDelay). Scheduled passes spend one each.
	// Antigravity's own bound — the Codex equivalent deliberately diverges, since
	// its attempts are local file scans rather than outbound reads.
	antigravityRefreshAfterRunMaxAttempts = 2
	// antigravityRefreshDebtMaxAttempts is the debt's LIFETIME budget of
	// outbound Code Assist reads, across the settle pass and every scheduled
	// retry. Only reads that actually reached Google count against it.
	antigravityRefreshDebtMaxAttempts = 5
	// antigravityRefreshOwedMaxAge retires a debt nothing could pay, so it can
	// never pin work (or a stale notice) forever. Long enough for the whole
	// retry ladder plus hours of an offline or expired-login device: a run
	// whose usage was never observed is still worth one read hours later.
	antigravityRefreshOwedMaxAge = 6 * time.Hour
	// antigravityRunFloorLocalSkew is how far ahead of the local clock the
	// persisted floors may legitimately sit. They are stamped ONLY from this
	// machine's clock, so the one benign way a floor outruns a caller's `now`
	// is that `now` was read before a concurrent run armed. Anything further
	// ahead is a backwards clock step, and a floor parked in the future would
	// make every later run owe a debt nothing can cover. Mirrors
	// codexRunFloorLocalSkew.
	antigravityRunFloorLocalSkew = 30 * time.Second

	antigravityFreshnessSchema = 1
	// antigravityFreshnessEnv relocates the state file (tests isolate from the
	// real machine; mirrors AIEXPEDITE_AGY_QUOTA_GATE).
	antigravityFreshnessEnv         = "AIEXPEDITE_AGY_FRESHNESS"
	antigravityFreshnessNoticeLimit = 320
)

// Vars rather than consts so tests can pin them small.
var (
	antigravityRefreshAfterRunRetryDelay = 5 * time.Second
	// antigravityRefreshMinInterval spaces the outbound Google reads the debt
	// worker sends: a new debt's first read, every scheduled rung and every
	// gather nudge. Only the same-pass retry (the same unpaid run, seconds
	// later) and the startup replay bypass it, exactly as a forced Codex
	// reconcile bypasses codexForcedReconcileMinInterval. The Refresh click does
	// not go through this worker, so a user-initiated refresh is never
	// throttled by it — though its read does feed this clock
	// (antigravityRecordClickRead).
	antigravityRefreshMinInterval = 60 * time.Second
	antigravityUsageFreshnessNow  = time.Now
	// antigravityPostRunReadingGrace is how long past the poller's own tail
	// window a settling run waits for the reading that tail is taking, and
	// antigravityPostRunReadingPoll how often it looks. Only an UNGATED run
	// that actually reached a server waits at all: a gated build pays no tail,
	// so on every current build the settle decides immediately.
	antigravityPostRunReadingGrace = time.Second
	antigravityPostRunReadingPoll  = 25 * time.Millisecond
)

// antigravityLiveRuns counts, by floor, the runs this process has armed and not
// yet settled. It is what keeps a crash marker on disk for a run that is still
// accruing usage: a reading landed mid-run (a Refresh click, a concurrent run's
// poller) covers the persisted floor, but the run it belongs to is not over, so
// the floor is rolled back to the oldest run still live rather than dropped.
// Without that, a crash or self-update before the run's own settle would leave
// startup with neither a floor nor a debt, and the run would never be refreshed.
var (
	antigravityLiveRunsMu sync.Mutex
	antigravityLiveRuns   = map[int64]int{}
)

func antigravityRegisterLiveRun(floorMs int64) {
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	antigravityLiveRuns[floorMs]++
	antigravityLiveRunsMu.Unlock()
}

func antigravityReleaseLiveRun(floorMs int64) {
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	if n := antigravityLiveRuns[floorMs]; n > 1 {
		antigravityLiveRuns[floorMs] = n - 1
	} else {
		delete(antigravityLiveRuns, floorMs)
	}
	antigravityLiveRunsMu.Unlock()
}

// antigravityOldestLiveRunFloorMs is the floor of the earliest MANAGED run still
// armed, or 0 when none is — the nudge's "a run of this process settles itself"
// check. The floors a restart must keep also include live direct-run
// candidates: antigravityOldestProtectedFloorMs. The lock order is cache ->
// freshness -> live runs; nothing may take these in the other direction, and
// the log-index lock is never held while taking any of them.
func antigravityOldestLiveRunFloorMs() int64 {
	antigravityAssertIndexUnlocked("live-runs")
	antigravityLiveRunsMu.Lock()
	defer antigravityLiveRunsMu.Unlock()
	oldest := int64(0)
	for floorMs := range antigravityLiveRuns {
		if oldest == 0 || floorMs < oldest {
			oldest = floorMs
		}
	}
	return oldest
}

// antigravityUsageFreshness is the persisted debt. Deliberately its own file
// rather than a field of the quota snapshot: that cache is a single sanitized
// reading written monotonically by saveAntigravityQuotaSnapshotIfNewer, and
// folding mutable counters into it would make an older-but-owed write fight the
// monotonic guard. antigravity_quota_gate.json is the precedent.
type antigravityUsageFreshness struct {
	SchemaVersion int `json:"schemaVersion,omitempty"`
	// RunFloorMs is the newest floor anything owes a reading for: an armed run's
	// start, or a settled run's completion once that is later. A restart adopts
	// it for a run nobody settled; each run's own settle uses the floor its arm
	// returned.
	RunFloorMs int64 `json:"runFloorMs,omitempty"`
	// RefreshOwedFloorMs is the observation time an unpaid run needs covered,
	// and RefreshOwedAtMs when that run finished (0 = nothing owed).
	RefreshOwedFloorMs int64 `json:"refreshOwedFloorMs,omitempty"`
	RefreshOwedAtMs    int64 `json:"refreshOwedAtMs,omitempty"`
	// LastPaidAtMs is the last OUTBOUND read, which is what the per-account
	// minimum interval spaces.
	LastPaidAtMs int64 `json:"lastPaidAtMs,omitempty"`
	Attempts     int   `json:"attempts,omitempty"`
	// Gated records that the run's build refused loopback reads, so the notice
	// accessor suppresses its own wording (antigravityGateNotice owns that
	// banner) and the debt goes straight to the Code Assist route.
	Gated bool `json:"gated,omitempty"`
	// AccountFingerprint is written by the PAYMENT, not by the arm: at arm time
	// no server has named an account. Diagnostic only — clearing is decided by
	// time alone, so a login switched between the floor and the reading still
	// retires the debt, and the snapshot cache remains the sole identity-scoped
	// store.
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
	// Outcome is one of the closed liveProbeOutcomeCodeAssist* codes.
	Outcome string `json:"outcome,omitempty"`
	// NextAttemptAtMs is when the retry schedule pays the debt next (0 = no
	// retry booked). Persisted so a restart or self-update re-arms the same
	// schedule instead of spending its one startup attempt and going quiet.
	// Legitimately up to the longest rung in the future, so it has its own
	// rebase rule rather than antigravityRunFloorLocalSkew's.
	NextAttemptAtMs int64 `json:"nextAttemptAtMs,omitempty"`
}

// clearDebt drops the pending debt and everything that only describes it,
// leaving the run floor and the spacing clock alone.
func (state *antigravityUsageFreshness) clearDebt() {
	state.RefreshOwedFloorMs, state.RefreshOwedAtMs = 0, 0
	state.Attempts, state.Outcome, state.Gated = 0, "", false
	state.NextAttemptAtMs = 0
}

var (
	antigravityFreshnessMu sync.Mutex
	// antigravityRefreshWorkerMu guards the process-wide single flight: one
	// debt worker at a time, however many runs settle at once.
	antigravityRefreshWorkerMu sync.Mutex
	// antigravityRefreshWorkerRunning is that claim; antigravityRefreshWorkerRearm
	// records that a run settled while it was held, so the debt it created is
	// worked by the running worker instead of being dropped.
	antigravityRefreshWorkerRunning bool
	antigravityRefreshWorkerRearm   bool
	// antigravityFreshnessInFlight counts the background writes this file owns
	// (the arm persist and the debt worker), so a test can wait them out.
	antigravityFreshnessInFlight atomic.Int64
)

// antigravityDebtID names one generation of the debt: the run floor it must
// cover and the completion that created it. An attempt is booked against the
// generation it was actually spent on, so a run that settles between the read
// and the write keeps the full attempt budget its own floor is owed (the same
// reason codexDebtID exists).
type antigravityDebtID struct {
	floorMs  int64
	owedAtMs int64
}

// equal compares two states field by field. It is a plain == on purpose: a
// future field that is not comparable (a slice, a map) then fails to COMPILE
// here rather than silently turning every update into a write — or, worse,
// into none.
func (state antigravityUsageFreshness) equal(other antigravityUsageFreshness) bool {
	return state == other
}

func (state antigravityUsageFreshness) debtID() antigravityDebtID {
	return antigravityDebtID{floorMs: state.RefreshOwedFloorMs, owedAtMs: state.RefreshOwedAtMs}
}

func antigravityFreshnessPath() string {
	if p := os.Getenv(antigravityFreshnessEnv); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "antigravity_quota_freshness.json")
}

// readAntigravityUsageFreshnessLocked reads the state file. A missing, corrupt
// or truncated file is "no debt": this is a freshness optimisation, and the
// next run rewrites it.
func readAntigravityUsageFreshnessLocked() antigravityUsageFreshness {
	var state antigravityUsageFreshness
	if !readJSONFile(antigravityFreshnessPath(), &state) {
		return antigravityUsageFreshness{}
	}
	return state
}

// writeAntigravityUsageFreshnessLocked persists the state, removing the file
// once nothing is left to remember. Best-effort: a read-only data dir costs a
// refresh, never a run.
//
// The replacement is temp-file + rename, exactly as
// writeAntigravityQuotaSnapshotLocked does it, because this is the SOLE
// crash-recovery record: a truncate-in-place the agent is killed or
// self-replaced in the middle of would leave invalid JSON, which
// readAntigravityUsageFreshnessLocked reads as "no debt" — losing the run floor
// this file exists to carry across exactly that kind of interruption. The
// pid+nanosecond suffix keeps two writers (or a stale temp from a crashed run)
// off the same intermediate file.
func writeAntigravityUsageFreshnessLocked(state antigravityUsageFreshness) {
	path := antigravityFreshnessPath()
	if path == "" {
		return
	}
	if state.RunFloorMs == 0 && state.RefreshOwedAtMs == 0 && state.LastPaidAtMs == 0 {
		_ = os.Remove(path)
		return
	}
	state.SchemaVersion = antigravityFreshnessSchema
	body, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// updateAntigravityUsageFreshness applies mutate to the persisted state under
// the freshness lock and returns the result. Callers MUST NOT hold
// antigravityQuotaCacheMu-dependent state here: the lock order in this package
// is cache → freshness (writeAntigravityQuotaSnapshotLocked settles while
// holding the cache lock), so nothing under this lock may read the cache.
func updateAntigravityUsageFreshness(mutate func(*antigravityUsageFreshness)) antigravityUsageFreshness {
	antigravityAssertIndexUnlocked("freshness")
	antigravityFreshnessMu.Lock()
	defer antigravityFreshnessMu.Unlock()
	state := readAntigravityUsageFreshnessLocked()
	before := state
	mutate(&state)
	if !state.equal(before) {
		writeAntigravityUsageFreshnessLocked(state)
	}
	return state
}

// antigravityRebaseFutureFreshness discards floors parked further than
// antigravityRunFloorLocalSkew ahead of now — a backwards clock step, not skew.
// Left in place they would make every later run owe a debt no reading can cover.
//
// NextAttemptAtMs is legitimately up to the longest retry rung ahead, so the
// floors' ceiling would discard every rung past the first. It gets its own:
// further ahead than any rung can put it is a backwards step, and resolves to
// "due now" — one early attempt, instead of a debt parked until the age-out.
func antigravityRebaseFutureFreshness(state *antigravityUsageFreshness, now time.Time) {
	ceiling := now.Add(antigravityRunFloorLocalSkew).UnixMilli()
	if state.RunFloorMs > ceiling {
		state.RunFloorMs = 0
	}
	if state.RefreshOwedFloorMs > ceiling || state.RefreshOwedAtMs > ceiling {
		state.clearDebt()
	}
	if state.NextAttemptAtMs > now.Add(antigravityRunDebtRetryHorizon()).UnixMilli() {
		state.NextAttemptAtMs = 0
	}
}

// antigravityObservedCovers reports whether a reading taken at observedMs
// (antigravitySnapshotObservedMs) is at or after a floor. Exact, with no grace:
// the floor a settle compares against is a run's COMPLETION, and a reading from
// even a few milliseconds before it cannot hold the usage debited at the end of
// that turn. Rounding a whole-second timestamp up to meet the floor is exactly
// how a Refresh click just before completion used to pass as the run's own
// reading; a reading only known to the second resolves to the start of it
// instead, which can cost a refresh but never loses one. No reading (0) never
// covers anything.
func antigravityObservedCovers(observedMs, floorMs int64) bool {
	if floorMs == 0 {
		return true
	}
	return observedMs > 0 && observedMs >= floorMs
}

// antigravityCachedReadingCovers reports whether the CACHED reading holds the
// usage of a run that ended at floorMs under account.
//
// The time-only form above is not enough on its own here. One capture poller is
// shared by every armed run (cliagent_usage_antigravity_capture.go) and the
// cache holds ONE snapshot, so two overlapping runs under different logins both
// see the same file: a reading saved for the other run's account, stamped after
// this run finished, satisfies the instant while describing a pool this run
// never spent from. account is this run's own identity, read from the block its
// PID owns; "" (no block, or no account in it) keeps the time-only rule.
func antigravityCachedReadingCovers(account string, floorMs int64) bool {
	_, ok := antigravityCachedReadingCovering(account, floorMs)
	return ok
}

// antigravityCachedReadingCovering is antigravityCachedReadingCovers plus the
// instant the covering reading was taken, which the debt worker retires with.
func antigravityCachedReadingCovering(account string, floorMs int64) (int64, bool) {
	if floorMs == 0 {
		return 0, true
	}
	snap, ok := cachedAntigravityQuotaSnapshot()
	if !ok {
		return 0, false
	}
	if account != "" && snap.AccountFingerprint != account {
		return 0, false
	}
	observedMs := antigravitySnapshotObservedMs(snap)
	return observedMs, antigravityObservedCovers(observedMs, floorMs)
}

/* ───────────────────────────────── arm ───────────────────────────────── */

// armAntigravityUsageRunFloor returns the floor of a starting `agy` run and
// persists it in the background. The persisted RunFloorMs keeps the NEWEST
// arm's — that is the one a restart adopts for a run nobody settled — while
// each run settles against the floor returned here.
//
// The write is off the caller's goroutine for the same reason
// armCodexUsageRunFloor's is: this runs on the spawn path (the Windows execute
// chain arms at function entry), and startAntigravityQuotaCapture's contract is
// that arming never blocks the run it is attached to. A write that loses the
// race with its own settle costs nothing — the arm only ever RAISES the floor,
// and a floor left behind for a run that did get its reading is dropped by the
// payment's own cached-reading check before any request is sent.
func armAntigravityUsageRunFloor(now time.Time) time.Time {
	floorMs := now.UnixMilli()
	// Registered on the CALLER's goroutine, so the run counts as live from the
	// instant it is armed: a reading that lands before the background persist
	// below must not be able to drop a floor for a run that is starting.
	antigravityRegisterLiveRun(floorMs)
	antigravityFreshnessInFlight.Add(1)
	go func() {
		defer antigravityFreshnessInFlight.Add(-1)
		updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
			antigravityRebaseFutureFreshness(state, now)
			if floorMs > state.RunFloorMs {
				state.RunFloorMs = floorMs
			}
		})
	}()
	return now
}

/* ──────────────────────────────── settle ─────────────────────────────── */

// antigravityUsageRunSettled decides, when one `agy` run ends, whether the
// agent owes a refresh for it.
//
// The requirement is a reading taken at or after this run COMPLETED, not one
// merely newer than its start: a turn's quota is debited at the END of the turn
// (antigravityCaptureTailGrace in cliagent_usage_antigravity_capture.go), so a
// snapshot taken while the run was still going — a Refresh click mid-run, or an
// overlapping run's Code Assist read — does not contain this run's usage, and
// treating it as coverage would leave that usage unobserved until some later
// run happened to owe a debt.
//
// capturedDuringRun says the capture path persisted a reading WHILE this run
// was going. It is never coverage by itself — a snapshot taken mid-run predates
// the turn's own debit — but it does mean the poller reached a server, so the
// post-release tail grace is being paid and the reading that covers this run is
// moments away. That is the only case that waits for it, bounded by
// antigravityPostRunReadingGrace. On a gated build the poller captures nothing
// and pays no tail, so every current build decides immediately.
//
// gated says the run's build refused loopback reads, so the debt goes straight
// to the Code Assist route instead of a retry the build will refuse, and the
// notice accessor leaves the wording to antigravityGateNotice.
//
// pid is the run's own agy process when the capture handle knows it (0
// otherwise). Its log block is read for exhaustion evidence, kept in memory
// only (recordAntigravityRunEvidence) — the one input that lets an omitted
// Code Assist bucket be charted as consumed.
//
// Called from the capture's Finish on its own goroutine: it must NOT wait for
// the poller, which is ref-counted and outlives a short run whenever a longer
// one is still armed.
func antigravityUsageRunSettled(floor time.Time, pid int, capturedDuringRun, gated bool) {
	if floor.IsZero() {
		return
	}
	now := antigravityUsageFreshnessNow()
	account := recordAntigravityRunEvidence(floor, pid, now)
	// The run is over, so the reading that covers it is one taken at or after
	// this instant — never one taken while the run was still accruing usage.
	// The completion is also what the debt asks for, so the payment's own
	// cached-reading check and settleAntigravityRunFreshness agree with it.
	completionMs := now.UnixMilli()
	if completionMs < floor.UnixMilli() {
		// A clock stepped backwards between arm and settle: the run's own start
		// is the most honest floor left.
		completionMs = floor.UnixMilli()
	}

	// The authoritative check, and the ONLY one: any route may have landed a
	// reading for this run — a concurrent run's poller, a Refresh click, this
	// poller's own tail — but only one taken at or after the completion holds
	// the usage this run just spent.
	covered := antigravityCachedReadingCovers(account, completionMs)
	if !covered && capturedDuringRun && !gated {
		covered = antigravityAwaitPostRunReading(account, completionMs)
	}
	// The run is over either way: stop protecting its floor from the GC below
	// before deciding what to persist for it.
	antigravityReleaseLiveRun(floor.UnixMilli())
	if covered {
		// A reading covers this run, and the write that landed it already
		// cleared any debt through settleAntigravityRunFreshness. All that is
		// left is the run's own floor, which that write had to keep while the
		// run was live. Silent on purpose: the poller's own close-out line
		// already reports `captured` for this run, and a second line per run is
		// noise in a log that is uploaded with diagnostics.
		updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
			if state.RunFloorMs != 0 && state.RunFloorMs <= completionMs &&
				state.RefreshOwedAtMs == 0 {
				state.RunFloorMs = antigravityOldestProtectedFloorMs()
			}
		})
		return
	}

	var (
		// priorID is the generation this settle found, and ownsFloor says the
		// floor it leaves behind is THIS run's completion rather than a newer
		// run's that was already pending. Read inside the closure so both come
		// from the same locked view of the state the update rewrote.
		priorID   antigravityDebtID
		ownsFloor bool
	)
	state := updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		antigravityRebaseFutureFreshness(state, now)
		priorID = state.debtID()
		ownsFloor = state.RefreshOwedAtMs == 0 || completionMs > state.RefreshOwedFloorMs
		owedFloorMs := completionMs
		if state.RefreshOwedFloorMs > owedFloorMs {
			// One pending debt at a time: a reading that covers the newest
			// floor covers every earlier one.
			owedFloorMs = state.RefreshOwedFloorMs
		}
		if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != owedFloorMs {
			// A new run, or a floor that moved: this debt gets its own budget
			// and its own schedule.
			state.Attempts, state.Outcome, state.NextAttemptAtMs = 0, "", 0
		}
		state.RefreshOwedFloorMs = owedFloorMs
		state.RefreshOwedAtMs = now.UnixMilli()
		state.Gated = gated
		if state.RunFloorMs < owedFloorMs {
			state.RunFloorMs = owedFloorMs
		}
	})
	// The generation this run just created is the one the worker must keep
	// scoped to this run's login. In memory only: the state file keeps its
	// fields, and a generation nobody remembers falls back to time alone.
	//
	// When this settle did NOT advance the floor — two differently
	// authenticated runs settling concurrently, and the NEWER completion got
	// its debt in first — the pending debt still asks for that newer run's
	// reading, so it must keep that run's login. Naming this (older) run's
	// account instead would have the worker test the newer floor against the
	// wrong identity: it would ignore the snapshot that actually covers it, and
	// accept one that does not. An account nobody remembers for the retained
	// generation falls back to the time-only rule, as a restart's would.
	debtAccount := account
	if !ownsFloor {
		debtAccount = antigravityDebtAccountFor(priorID)
	}
	noteAntigravityDebtAccount(state.debtID(), debtAccount)
	fmt.Printf("%s[antigravity-freshness] Run finished with no reading of its own (owed=true gated=%v attempts=%d)%s\n",
		colorYellow, gated, state.Attempts, colorReset)
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, false)
}

// antigravityAwaitPostRunReading waits, bounded, for a reading taken at or
// after completionMs to reach the quota cache.
//
// It exists for exactly one case: an ungated run whose poller reached a server
// is followed by antigravityCaptureTailGrace of post-release probing, and the
// reading that holds the run's end-of-turn usage lands in that window. Owing a
// debt without waiting for it would spend an outbound Google read on every
// ungated run for a number that was already on its way.
//
// It waits on the CACHE, never on the poller: the poller is ref-counted and a
// short run sharing it with a long interactive session would otherwise park its
// settle for the length of that session. Runs on the settle goroutine, so it
// delays nothing but the debt decision it is making.
func antigravityAwaitPostRunReading(account string, completionMs int64) bool {
	// Wall clock, not antigravityUsageFreshnessNow: this is a real wait for a
	// real write, and a test that pins the logical clock must not turn it into
	// a spin. antigravityCaptureTailEnv shrinks it where a test needs it short.
	deadline := time.Now().Add(antigravityCaptureTailGraceValue() + antigravityPostRunReadingGrace)
	for {
		if antigravityCachedReadingCovers(account, completionMs) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if remaining > antigravityPostRunReadingPoll {
			remaining = antigravityPostRunReadingPoll
		}
		time.Sleep(remaining)
	}
}

// settleAntigravityRunFreshness retires whatever a landed reading covers. It is
// called from inside writeAntigravityQuotaSnapshotLocked — while the quota
// cache lock is held — so it must never read the cache back. observedMs is the
// landed reading's instant (antigravitySnapshotObservedMs); 0 retires nothing.
//
// account is the login the landed reading describes. A debt is scoped to the
// account of the run that raised it, and one capture poller plus one cache file
// are shared by every armed run, so a concurrent run or a Refresh click under
// another login can land a reading stamped after this debt's floor that says
// nothing about the pool that run spent from. Such a reading must not retire
// the debt — that is the same account-aware rule antigravityUsageRunSettled and
// antigravityPayRunDebtPass apply, enforced here so no write can slip past
// them. "" (an unidentified reading, or a generation this process does not
// remember) keeps the time-only rule.
func settleAntigravityRunFreshness(observedMs int64, account string) {
	if observedMs <= 0 {
		return
	}
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		if state.RefreshOwedAtMs != 0 && observedMs >= state.RefreshOwedFloorMs &&
			// A leaf lock taken under the freshness lock: nothing is ever taken
			// while antigravityDebtAccount is held, so this adds no order.
			antigravityDebtAccountAnswers(state.debtID(), account) {
			state.clearDebt()
		}
		if state.RunFloorMs != 0 && observedMs >= state.RunFloorMs {
			// Nothing left for a restart to adopt for the run that floor named
			// — UNLESS a run is still armed. A reading taken while a run is
			// going does not hold the usage that run is still spending, so
			// dropping the marker outright would leave a crash or self-update
			// before its settle with neither a floor nor a debt, and no
			// recovery refresh. Roll back to the oldest protected floor instead
			// (a managed run, or a live direct-run candidate).
			state.RunFloorMs = antigravityOldestProtectedFloorMs()
			if state.RefreshOwedAtMs != 0 && state.RunFloorMs < state.RefreshOwedFloorMs {
				// A debt this reading did NOT answer is still pending (another
				// account's). It keeps the invariant the settle established:
				// the marker a restart adopts is never behind the floor the
				// debt is still asking for.
				state.RunFloorMs = state.RefreshOwedFloorMs
			}
		}
	})
}

/* ────────────────────────────────── pay ──────────────────────────────── */

// antigravityStartRunDebtWorker runs the bounded payment on its own goroutine
// under a process-wide single flight.
//
// A settle that finds the flight held does NOT simply drop its request: the
// worker may be about to return (its read succeeded, the login is gone, the
// interval blocked it, it was the startup replay's single attempt), and the
// debt this settle just created would then have no one working it — the run
// that finished would wait for the NEXT run, or the next agent start, to be
// refreshed at all. Recording a re-arm instead makes the running worker take
// another pass, which is the same shape as codexRunDebtWorker's
// claimWorker / releaseWorker.
func antigravityStartRunDebtWorker(maxAttempts int, bypassInterval bool) {
	// Counted BEFORE the claim: a waiter that sampled the counter between a
	// successful claim and the increment would read "idle" while a worker is
	// about to run.
	antigravityFreshnessInFlight.Add(1)
	if !antigravityClaimRunDebtWorker() {
		antigravityFreshnessInFlight.Add(-1)
		return
	}
	go func() {
		defer antigravityFreshnessInFlight.Add(-1)
		for {
			antigravityPayRunDebt(maxAttempts, bypassInterval)
			// Any pass after the first is an ordinary post-run debt, whatever
			// this worker was started as: the startup replay's one bypassing
			// attempt is spent on the debt it adopted, not on a run that
			// finished afterwards.
			maxAttempts, bypassInterval = antigravityRefreshAfterRunMaxAttempts, false
			if antigravityReleaseRunDebtWorker() {
				return
			}
		}
	}()
}

// antigravityClaimRunDebtWorker takes the single flight, or records a re-arm
// for the worker that holds it.
func antigravityClaimRunDebtWorker() bool {
	antigravityRefreshWorkerMu.Lock()
	defer antigravityRefreshWorkerMu.Unlock()
	if antigravityRefreshWorkerRunning {
		antigravityRefreshWorkerRearm = true
		return false
	}
	antigravityRefreshWorkerRunning, antigravityRefreshWorkerRearm = true, false
	return true
}

// antigravityReleaseRunDebtWorker retires the worker unless a run settled
// since its last pass, in which case the claim is KEPT and the worker takes
// another one. Passes are driven by real settles, and each one is bounded by
// the debt's own attempt cap, the minimum interval and the age-out, so a burst
// of runs cannot spin it.
func antigravityReleaseRunDebtWorker() bool {
	antigravityRefreshWorkerMu.Lock()
	defer antigravityRefreshWorkerMu.Unlock()
	if antigravityRefreshWorkerRearm {
		antigravityRefreshWorkerRearm = false
		return false
	}
	antigravityRefreshWorkerRunning = false
	return true
}

// antigravityUsageRefreshWaitIdle blocks until no background write of this
// feature is in flight, bounded. Tests wait on it; gracefulShutdown drains
// with the shorter antigravityUsageRefreshWaitIdleFor. It polls the counter
// rather than adding a WaitGroup the settle goroutine would have to touch on
// every run.
func antigravityUsageRefreshWaitIdle() {
	antigravityUsageRefreshWaitIdleFor(30 * time.Second)
}

// antigravityUsageRefreshWaitIdleFor waits at most d and reports whether
// nothing was left in flight.
func antigravityUsageRefreshWaitIdleFor(d time.Duration) bool {
	for deadline := time.Now().Add(d); ; {
		if antigravityFreshnessInFlight.Load() == 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// antigravityPendingDebt returns the unpaid debt, rebasing a rolled-back clock
// and retiring one older than antigravityRefreshOwedMaxAge so it can never pin
// a worker (or a notice) forever.
func antigravityPendingDebt(now time.Time) (antigravityUsageFreshness, bool) {
	state := updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		antigravityRebaseFutureFreshness(state, now)
		if state.RefreshOwedAtMs != 0 &&
			now.Sub(time.UnixMilli(state.RefreshOwedAtMs)) > antigravityRefreshOwedMaxAge {
			state.clearDebt()
		}
	})
	return state, state.RefreshOwedAtMs != 0
}

// antigravityRetireRunDebt drops the debt without an attempt, for the cases
// where nothing on this machine could ever pay it.
func antigravityRetireRunDebt(reason string) {
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		state.clearDebt()
		state.RunFloorMs = 0
	})
	fmt.Printf("%s[antigravity-freshness] Run refresh debt retired without an attempt (%s)%s\n",
		colorYellow, reason, colorReset)
}

// antigravityPayRunDebt spends at most maxAttempts Code Assist reads on the
// pending debt — the route that still answers on a CSRF-gated build. Each read
// is bounded by antigravityCodeAssistTimeout and the first is spaced from the
// previous outbound read by antigravityRefreshMinInterval; a retry within the
// same pass bypasses that interval, because it is the same unpaid run.
//
// It never runs a model turn. The Refresh click may run the `agy models`
// warm-up to make the CLI refresh its own keyring token; doing that behind the
// user's back on run teardown is a different class of side effect, so a
// token_expired debt is KEPT (the next real run refreshes the keyring for free)
// and only a no_login debt stops attempting, since nothing here can pay it.
// The one child this can start is the bounded `<agy> --version` behind
// antigravityCodeAssistBuildVersion's cache, only on a cold cache, and only
// once the probe has found a usable stored login.
//
// Whatever the pass KEEPS it hands to antigravityScheduleRunDebtRetry before
// returning, so a deferral (the minimum interval, offline, an expired login) or
// a read that failed is always followed by another attempt on the retry ladder
// rather than by nothing until the next run happens to settle.
func antigravityPayRunDebt(maxAttempts int, bypassInterval bool) {
	state, retry := antigravityPayRunDebtPass(maxAttempts, bypassInterval)
	if retry != antigravityRetryNone {
		antigravityScheduleRunDebtRetry(state, antigravityUsageFreshnessNow(), retry)
	}
}

// antigravityPayRunDebtPass is antigravityPayRunDebt's body. It returns the
// debt it last looked at, so the schedule is booked against that generation.
func antigravityPayRunDebtPass(maxAttempts int, bypassInterval bool) (antigravityUsageFreshness, antigravityRunDebtRetryKind) {
	var state antigravityUsageFreshness
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(antigravityRefreshAfterRunRetryDelay)
		}
		now := antigravityUsageFreshnessNow()
		var owed bool
		state, owed = antigravityPendingDebt(now)
		if !owed {
			return state, antigravityRetryNone
		}
		// Any route may have landed a reading since the settle: a concurrent
		// run's poller, a Refresh click, or the poller's own 2 s tail on an
		// ungated build.
		// Scoped to the debt's own login where this process still remembers it:
		// the capture poller is shared, so a concurrent run under another
		// account can leave a NEWER reading that says nothing about this one.
		debtAccount := antigravityDebtAccountFor(state.debtID())
		if cached, ok := antigravityCachedReadingCovering(
			debtAccount, state.RefreshOwedFloorMs); ok {
			settleAntigravityRunFreshness(cached, debtAccount)
			return state, antigravityRetryNone
		}
		if state.Attempts >= antigravityRefreshDebtMaxAttempts {
			// Out of budget: the debt keeps its Outcome until the age-out so
			// the card can say why the figure is behind. Handed to the
			// schedule anyway, which books nothing for a spent budget and
			// clears the rung this pass was running on.
			return state, antigravityRetryAfterRead
		}
		// An uninstall between the run and now must not leave a debt retrying,
		// or a notice, on a provider the card no longer shows.
		path := antigravityExecutablePath()
		if path == "" {
			antigravityRetireRunDebt("agy is no longer installed")
			return state, antigravityRetryNone
		}
		// An offline agent makes no outbound request. The debt stays pending
		// and keeps its budget: offline is temporary, and checking costs nothing.
		if IsOffline() {
			fmt.Printf("%s[antigravity-freshness] Run refresh deferred: the agent is offline (debt kept)%s\n",
				colorYellow, colorReset)
			return state, antigravityRetryFree
		}
		if !bypassInterval && attempt == 0 && state.LastPaidAtMs > 0 {
			if since := now.Sub(time.UnixMilli(state.LastPaidAtMs)); since >= 0 && since < antigravityRefreshMinInterval {
				fmt.Printf("%s[antigravity-freshness] Run refresh deferred: last read %ds ago (minimum %ds, debt kept)%s\n",
					colorCyan, int(since.Seconds()), int(antigravityRefreshMinInterval.Seconds()), colorReset)
				return state, antigravityRetrySpacing
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), antigravityCodeAssistTimeout)
		// No version: the probe resolves it itself, and only once it knows a
		// login exists, so a no_login debt never spawns `<agy> --version`.
		takeAntigravityCodeAssistAttempt()
		outcome := probeAntigravityQuotaCodeAssistFn(ctx, "", antigravityUsageFreshnessNow)
		cancel()
		note := takeAntigravityCodeAssistAttempt()
		noteAntigravityCurrentAttempt(state.debtID(), note.fingerprint,
			outcome == liveProbeOutcomeCodeAssistBadResponse && note.unplottable)
		antigravityRecordRefreshAttempt(state.debtID(), outcome, antigravityUsageFreshnessNow())
		fmt.Printf("%s[antigravity-freshness] Run refresh attempt %d/%d finished (%s)%s\n",
			colorCyan, state.Attempts+1, antigravityRefreshDebtMaxAttempts, outcome, colorReset)

		switch outcome {
		case liveProbeOutcomeCodeAssistOK:
			// The persist inside the probe cleared the debt through the settle
			// hook, so the schedule finds nothing to book. The one way it did
			// not is an unattributable reading (codeassist_not_attributable),
			// which is reported as its own outcome and keeps retrying.
			return state, antigravityRetryAfterRead
		case liveProbeOutcomeCodeAssistNoLogin:
			// Terminal: nothing on this device can pay it.
			return state, antigravityRetryNone
		case liveProbeOutcomeCodeAssistTokenExpired:
			// The next real `agy` run refreshes the keyring token for free;
			// until then a local check is all a retry costs.
			return state, antigravityRetryFree
		}
	}
	return state, antigravityRetryAfterRead
}

// antigravityOutcomeSpentRead reports whether a Code Assist outcome reached
// Google. The two local refusals — no stored login, an expired token — are
// decided before any request is sent.
func antigravityOutcomeSpentRead(outcome string) bool {
	return outcome != liveProbeOutcomeCodeAssistTokenExpired &&
		outcome != liveProbeOutcomeCodeAssistNoLogin
}

// antigravityRecordClickRead books a Refresh click's Code Assist read on the
// spacing clock, and nothing else. The click bypasses the debt worker, so
// without this antigravityRefreshMinInterval would not see it and a nudge in the
// click's own follow-up gather could send a second outbound read seconds later.
// It must not book an attempt against a debt it does not own, nor clear one (a
// successful click's reading already retires it through
// settleAntigravityRunFreshness), nor create a floor.
func antigravityRecordClickRead(outcome string, now time.Time) {
	if !antigravityOutcomeSpentRead(outcome) {
		return
	}
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		state.LastPaidAtMs = now.UnixMilli()
	})
}

// antigravityRecordRefreshAttempt books one payment attempt. An outbound read
// updates LastPaidAtMs (what the minimum interval spaces) and the fingerprint
// the reading was taken under; the two local refusals spend no request, so they
// space nothing. Attempts are only counted while a debt is actually pending: a
// successful read has already retired it through the settle hook.
func antigravityRecordRefreshAttempt(id antigravityDebtID, outcome string, now time.Time) {
	fingerprint := ""
	if outcome == liveProbeOutcomeCodeAssistOK {
		if snap, ok := cachedAntigravityQuotaSnapshot(); ok {
			fingerprint = snap.AccountFingerprint
		}
	}
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		if antigravityOutcomeSpentRead(outcome) {
			// The machine's last OUTBOUND read, whatever it was spent on: it is
			// what the minimum interval spaces and the account it landed under.
			state.LastPaidAtMs = now.UnixMilli()
			if fingerprint != "" {
				state.AccountFingerprint = fingerprint
			}
		}
		if state.debtID() != id {
			// A run settled between the read and this write, so the debt on
			// disk is a newer generation than the one this attempt was spent
			// on. Charging it here would short-change a run that has not been
			// tried even once; the worker's next pass gives it its own budget.
			return
		}
		state.Outcome = outcome
		switch outcome {
		case liveProbeOutcomeCodeAssistNoLogin:
			// Nothing on this machine can pay it: stop attempting, but keep the
			// debt so the card can say why the reading is not moving.
			if state.RefreshOwedAtMs != 0 {
				state.Attempts = antigravityRefreshDebtMaxAttempts
			}
		case liveProbeOutcomeCodeAssistTokenExpired:
			// Keep the debt and its budget: the next real `agy` run refreshes
			// the keyring token for free, and this cost no request.
		default:
			if state.RefreshOwedAtMs != 0 {
				state.Attempts++
			}
		}
	})
}

// payOwedAntigravityUsageRefresh resumes a debt the previous agent process
// left behind: a run that finished just before a restart or self-update, or one
// the process was cut off in the middle of. A schedule the previous process
// booked is re-armed for its remainder; otherwise exactly one bounded Code
// Assist read, and any remainder goes back on the retry ladder.
//
// Asynchronous and best-effort — StartAgent must never wait on it.
func payOwedAntigravityUsageRefresh() {
	// Sampled on the CALLER's goroutine, before anything of this process can
	// have armed: every floor below it belongs to the process that is gone.
	startedAt := antigravityUsageFreshnessNow()
	antigravityFreshnessInFlight.Add(1)
	go func() {
		defer antigravityFreshnessInFlight.Add(-1)
		adoptAndPayOwedAntigravityRunDebt(startedAt)
	}()
}

// adoptAndPayOwedAntigravityRunDebt is payOwedAntigravityUsageRefresh's body,
// off the boot goroutine: it reads the state file, the gate marker and — through
// the worker — the quota cache, and StartAgent must wait on none of them.
//
// startedAt is the instant the boot goroutine asked, and it is what makes a
// bare floor safe to convert: this replay is spawned, so a session of THIS
// process can arm and persist its own floor before it gets here. Converting
// that floor would book a completion time for a run that is still going — the
// reading it triggers would be taken at the run's START and would then satisfy
// the run's own settle, so the run would end with no refresh at all. The live
// run's settle path owes it a refresh when it finishes; leave the floor to it.
// (codexOweInterruptedRun guards the same race with codexUsageRefresh.armedLocally.)
//
// The log index's startup scan runs FIRST (antigravityStartupScan): it finds
// direct runs that started or ended while no agent was running. Its candidate
// floors are applied only AFTER the previous process's floor has been adopted,
// so a floor armed now can never mask the one a crash or update left behind.
func adoptAndPayOwedAntigravityRunDebt(startedAt time.Time) {
	scan, scanned := antigravityStartupScan(startedAt)
	if scanned {
		defer antigravityApplyDiscovery(scan, antigravityUsageFreshnessNow())
	}
	now := antigravityUsageFreshnessNow()
	state := updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		antigravityRebaseFutureFreshness(state, now)
		if state.RefreshOwedAtMs != 0 || state.RunFloorMs == 0 {
			return
		}
		if state.RunFloorMs >= startedAt.UnixMilli() {
			// A run this process armed: not interrupted, still going.
			return
		}
		// A floor with no debt beside it is a run THIS process cannot be
		// running, so its owner was cut off. Convert it into a debt completed
		// now: the age-out and the notice both need a completion time, and a
		// merely-armed floor is never refreshed.
		if now.Sub(time.UnixMilli(state.RunFloorMs)) > antigravityRefreshOwedMaxAge {
			state.RunFloorMs = 0
			return
		}
		state.RefreshOwedFloorMs, state.RefreshOwedAtMs = state.RunFloorMs, now.UnixMilli()
		state.Attempts, state.Outcome, state.NextAttemptAtMs = 0, "", 0
		// The build that refused is remembered per build, not per run, so the
		// marker is the honest source for a debt adopted across a restart.
		_, state.Gated = antigravityQuotaGateFor("", now)
	})
	if state.RefreshOwedAtMs == 0 {
		return
	}
	// A retry the previous process booked and that is not due yet is honoured
	// as booked: arm the remainder and spend nothing now. Paying at once here
	// would let a restart loop burn the whole budget in seconds.
	if next := time.UnixMilli(state.NextAttemptAtMs); state.NextAttemptAtMs != 0 && next.After(now) {
		antigravityArmRunDebtRetry(state.debtID(), next.Sub(now))
		fmt.Printf("%s[antigravity-freshness] Run refresh schedule resumed after restart (next attempt in %ds, attempts=%d/%d)%s\n",
			colorCyan, int(next.Sub(now).Seconds()), state.Attempts, antigravityRefreshDebtMaxAttempts, colorReset)
		return
	}
	// Due, past or never booked (an adopted bare floor): one attempt that
	// bypasses the interval — nothing in this fresh process has read yet.
	antigravityStartRunDebtWorker(1, true)
}

/* ──────────────────────────────── report ─────────────────────────────── */

// antigravityFreshnessNotice is the single accessor for the debt's user-facing
// state: the card banner (empty when there is nothing to say) and whether a
// debt is pending at all. No other caller reads the state file.
//
// The notice is empty until the debt's budget is spent, so a debt that is
// about to be paid never flashes a warning — except an expired login, which no
// attempt of ours can pay and which costs nothing to explain. It is worded the
// same on a gated build: ParseContext lets antigravityGateNotice own the banner
// while the gate is the newer fact, and renders this one otherwise, because a
// gated debt that exhausted its ladder would else never say anything.
// Timestamps and fixed text only — never a path, an account or log text.
func antigravityFreshnessNotice(lastObservedAt string, now time.Time) (string, bool) {
	antigravityFreshnessMu.Lock()
	state := readAntigravityUsageFreshnessLocked()
	antigravityFreshnessMu.Unlock()

	antigravityRebaseFutureFreshness(&state, now)
	if state.RefreshOwedAtMs == 0 ||
		now.Sub(time.UnixMilli(state.RefreshOwedAtMs)) > antigravityRefreshOwedMaxAge {
		return "", false
	}
	if state.Attempts < antigravityRefreshDebtMaxAttempts &&
		state.Outcome != liveProbeOutcomeCodeAssistTokenExpired {
		return "", true
	}
	// Only the card's RFC3339 string is at hand here, so it resolves to the start
	// of its second (antigravitySnapshotObservedMs): at worst a warning stays up
	// for a reading taken within a second of the floor, never the reverse. The
	// settle hook has the exact instant and retires such a debt anyway.
	if antigravityObservedCovers(antigravitySnapshotObservedMs(antigravityQuotaSnapshot{ObservedAt: lastObservedAt}), state.RefreshOwedFloorMs) {
		// The card is already showing a reading that covers the run; the debt
		// is a bookkeeping leftover, not something to warn about.
		return "", true
	}

	const layout = "2006-01-02 15:04 UTC"
	last := "No Antigravity utilization reading has been observed"
	if at, err := time.Parse(time.RFC3339, lastObservedAt); err == nil {
		last = "Antigravity utilization was last observed " + at.UTC().Format(layout)
	}
	// One sentence per case, because the remedy differs: a missing login is
	// something the user fixes, an expired one the next run renews, and a
	// failing read is something that heals itself.
	cause := "Google returned no reading for the stored login; it will update on the next run that reports one."
	switch state.Outcome {
	case liveProbeOutcomeCodeAssistUnauthorized:
		cause = "Google refused the stored login's quota read, so the reading could not be updated."
	case liveProbeOutcomeCodeAssistBadResponse:
		if antigravityAttemptWasUnplottable(state.debtID()) {
			cause = "Google answered for the stored login with no chartable quota windows, so the reading could not be updated."
		}
	case liveProbeOutcomeCodeAssistNoLogin:
		cause = "No Antigravity login is stored on this device, so no reading can be taken; sign in with the CLI to restore it."
	case liveProbeOutcomeCodeAssistTokenExpired:
		cause = "The stored Antigravity login has expired; the next Antigravity run renews it and the reading updates then."
	}
	notice := fmt.Sprintf("%s, before the most recent Antigravity run finished (%s). %s",
		last, time.UnixMilli(state.RefreshOwedFloorMs).UTC().Format(layout), cause)
	return clampASCII(notice, antigravityFreshnessNoticeLimit), true
}

// antigravityExecutablePath resolves the installed Antigravity CLI, or "" when
// it is not on this machine at all.
//
// The command comes from the ACTIVE catalog entry rather than the literal
// "agy": the catalog is server-configurable, so a deployment that points the
// provider at a different spelling or an absolute path would otherwise have the
// card detect it while this check declared it uninstalled — and a real run's
// debt would be retired instead of refreshed. Resolution then matches
// gatherCLIAgents exactly — PATH, then the installer's own bin dir, which a
// macOS GUI/launchd agent's sparse PATH misses — so the worker and the card can
// never disagree about whether the CLI exists. resolveExecutable is
// deliberately NOT used: it echoes the command back on a miss, which would read
// as "installed" forever.
func antigravityExecutablePath() string {
	command := antigravityCatalogCommand()
	if path, err := exec.LookPath(command); err == nil {
		return path
	}
	return resolveInstallerBinary(command, installerBinDirFor(command))
}

// antigravityCatalogCommand is the command the active catalog runs Antigravity
// with, falling back to the shipped default when the provider is absent from a
// configured catalog (the card shows nothing for it either, and the fallback
// keeps the uninstall check from turning a missing entry into a retired debt).
func antigravityCatalogCommand() string {
	for _, entry := range activeCLIAgentCatalog() {
		if entry.ID == "antigravity" {
			if command := firstCommandToken(entry.Command); command != "" {
				return command
			}
			break
		}
	}
	return "agy"
}

/* ──────────────────────────── evidence ──────────────────────────── */

// Managed-run exhaustion evidence. Google's quota reply OMITS remainingFraction
// for a bucket with nothing left, so an account whose every bucket is spent
// answers with nothing chartable. The one trustworthy statement that a
// specific bucket is spent is a managed run's OWN log block: it names the
// account it authenticated as and the RESOURCE_EXHAUSTED reset it hit. That is
// kept here, in memory only — never persisted, never logged (the email is
// reduced to its fingerprint at once) — and applied by
// antigravitySnapshotFromGroups to an omitted bucket of the same account whose
// reset matches. Direct (user-shell) runs never contribute: a login switched
// between two of them could attribute one account's exhaustion to another.
const (
	antigravityExhaustionMaxEvents = 4
	antigravityExhaustionTTL       = 10 * time.Minute
	// antigravityExhaustionMinReset: a shorter reset is a throttle, not a
	// spent quota window.
	antigravityExhaustionMinReset = 2 * time.Minute
	// antigravityExhaustionMatch is how close an event's reset must be to a
	// bucket's own reset to name that bucket.
	antigravityExhaustionMatch = 2 * time.Minute
)

type antigravityExhaustionEvent struct {
	resetAtMs   int64
	fingerprint string
	atMs        int64
}

var antigravityExhaustion struct {
	mu     sync.Mutex
	events []antigravityExhaustionEvent
}

// antigravityExhaustionEvidenceFn is the quota routes' seam.
var antigravityExhaustionEvidenceFn = antigravityExhaustionEvidence

// antigravityExhaustionEvidence returns the unexpired events recorded for the
// account with this fingerprint.
func antigravityExhaustionEvidence(now time.Time, fingerprint string) []antigravityExhaustionEvent {
	if fingerprint == "" {
		return nil
	}
	e := &antigravityExhaustion
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []antigravityExhaustionEvent
	for _, event := range e.events {
		if event.fingerprint == fingerprint && now.Sub(time.UnixMilli(event.atMs)) <= antigravityExhaustionTTL {
			out = append(out, event)
		}
	}
	return out
}

// recordAntigravityRunEvidence reads a finished managed run's own PID block
// and records its exhaustion events. Needs both the authenticated account and
// a quota reset in THAT block; anything else records nothing.
//
// It returns the account the run authenticated as ("" when its block named
// none), which is the run's own identity — read from the block the run's PID
// owns, never from settings.json or the cache. The settle scopes its coverage
// check to it: the capture poller is shared process-wide, so without the
// account a reading saved for a CONCURRENT run under another login would pass
// the time-only check and retire a run whose quota the cache cannot report.
func recordAntigravityRunEvidence(floor time.Time, pid int, now time.Time) string {
	if pid <= 0 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, base := range antigravityQuotaBases(home) {
		block, found := antigravityPIDBlock(base, pid, floor)
		if !found {
			continue
		}
		email, resets := antigravityBlockEvidence(block, now)
		fingerprint := fingerprintAccount("antigravity", email)
		if fingerprint == "" {
			return ""
		}
		for _, reset := range resets {
			if reset.Sub(now) < antigravityExhaustionMinReset {
				continue
			}
			addAntigravityExhaustionEvent(antigravityExhaustionEvent{
				resetAtMs: reset.UnixMilli(), fingerprint: fingerprint, atMs: now.UnixMilli(),
			}, now)
		}
		return fingerprint
	}
	return ""
}

func addAntigravityExhaustionEvent(event antigravityExhaustionEvent, now time.Time) {
	e := &antigravityExhaustion
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.events[:0]
	for _, existing := range e.events {
		if now.Sub(time.UnixMilli(existing.atMs)) <= antigravityExhaustionTTL {
			kept = append(kept, existing)
		}
	}
	e.events = append(kept, event)
	if len(e.events) > antigravityExhaustionMaxEvents {
		e.events = e.events[len(e.events)-antigravityExhaustionMaxEvents:]
	}
}

// resetAntigravityExhaustionEvidence forgets every event. Tests, and the model
// of a restart: evidence never survives one.
func resetAntigravityExhaustionEvidence() {
	e := &antigravityExhaustion
	e.mu.Lock()
	e.events = nil
	e.mu.Unlock()
}

/* ─────────────────────────── attempt marks ─────────────────────────── */

// antigravityCurrentAttempt is the latest payment attempt on a debt: the debt
// generation it was spent on, the account it resolved, and whether Google's
// 200 had nothing chartable. In memory only; the persisted outcome stays
// codeassist_bad_response. The notice uses the finer wording only while the
// debt it words is the one this attempt was for, so a new generation, another
// outcome or a retirement clears it without touching the state file.
type antigravityCurrentAttempt struct {
	debtID             antigravityDebtID
	attemptFingerprint string
	unplottable        bool
}

var antigravityAttempt struct {
	mu      sync.Mutex
	current antigravityCurrentAttempt
}

func noteAntigravityCurrentAttempt(id antigravityDebtID, fingerprint string, unplottable bool) {
	a := &antigravityAttempt
	a.mu.Lock()
	a.current = antigravityCurrentAttempt{debtID: id, attemptFingerprint: fingerprint, unplottable: unplottable}
	a.mu.Unlock()
}

// antigravityDebtAccount is the login the run that created a debt generation
// authenticated as, so the worker can tell a reading that answers THAT run from
// one a concurrent run under another account left in the shared cache. In
// memory only, exactly like the attempt mark: the state file gains no field,
// and a generation this process does not remember (a restart, a debt raised by
// a discovered direct run) simply falls back to the time-only rule.
var antigravityDebtAccount struct {
	mu          sync.Mutex
	debtID      antigravityDebtID
	fingerprint string
}

func noteAntigravityDebtAccount(id antigravityDebtID, fingerprint string) {
	d := &antigravityDebtAccount
	d.mu.Lock()
	d.debtID, d.fingerprint = id, fingerprint
	d.mu.Unlock()
}

// antigravityDebtAccountFor is the remembered login of debt generation id, or
// "" when this process never recorded one for it.
func antigravityDebtAccountFor(id antigravityDebtID) string {
	d := &antigravityDebtAccount
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.debtID != id {
		return ""
	}
	return d.fingerprint
}

// antigravityDebtAccountAnswers reports whether a reading belonging to account
// may retire debt generation id. A generation whose login this process does not
// remember, and a reading whose own account is unknown, both fall back to the
// time-only rule; otherwise the two identities must be the same login.
func antigravityDebtAccountAnswers(id antigravityDebtID, account string) bool {
	owner := antigravityDebtAccountFor(id)
	return owner == "" || account == "" || owner == account
}

// antigravityAttemptWasUnplottable reports whether the latest attempt on the
// debt generation id got a 200 with nothing chartable.
func antigravityAttemptWasUnplottable(id antigravityDebtID) bool {
	a := &antigravityAttempt
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current.unplottable && a.current.debtID == id
}

// antigravityShutdownDrain bounds gracefulShutdown's wait for in-flight writes.
var antigravityShutdownDrain = 5 * time.Second

// drainAntigravityUsageWrites stops the discovery tick and waits, at most
// antigravityShutdownDrain, for this feature's background writes. A write
// still stuck after that is abandoned: the floor its arm persisted at spawn
// is what the next process adopts.
func drainAntigravityUsageWrites() {
	stopAntigravityDiscovery()
	if !antigravityUsageRefreshWaitIdleFor(antigravityShutdownDrain) {
		fmt.Printf("%s[antigravity-freshness] shutdown drain timed out: inFlight=%d%s\n",
			colorYellow, antigravityFreshnessInFlight.Load(), colorReset)
	}
}
