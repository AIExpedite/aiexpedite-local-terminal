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
//     reading against that floor — never a flag — and, when nothing covers it,
//     persists the debt and hands it to a bounded single-flight worker.
//   - Clear: settleAntigravityRunFreshness runs inside
//     writeAntigravityQuotaSnapshotLocked, so ANY route that lands a reading at
//     or after the floor (in-run loopback, Code Assist, a Refresh click, a
//     concurrent run's poller) retires the debt exactly once.
//   - Survive: the debt is a file, so StartAgent's payOwedAntigravityUsageRefresh
//     pays one bounded attempt for a run the previous process never settled
//     (crash, restart, self-update).
//   - Report: a debt that outlived its attempts surfaces through
//     antigravityFreshnessNotice — empty wording on a gated build, where
//     antigravityGateNotice already owns the banner.
//
// Redaction: the state file holds schemaVersion, four epoch-millisecond fields,
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
	// antigravityRefreshAfterRunMaxAttempts bounds the Code Assist reads one
	// run's debt may spend (immediate, then one retry after
	// antigravityRefreshAfterRunRetryDelay). Mirrors
	// codexRefreshAfterRunMaxAttempts.
	antigravityRefreshAfterRunMaxAttempts = 2
	// antigravityRefreshOwedMaxAge retires a debt nothing could pay, so it can
	// never pin work (or a stale notice) forever. Mirrors codexRefreshOwedMaxAge.
	antigravityRefreshOwedMaxAge = 30 * time.Minute
	// antigravityRunFloorLocalSkew is how far ahead of the local clock the
	// persisted floors may legitimately sit. They are stamped ONLY from this
	// machine's clock, so the one benign way a floor outruns a caller's `now`
	// is that `now` was read before a concurrent run armed. Anything further
	// ahead is a backwards clock step, and a floor parked in the future would
	// make every later run owe a debt nothing can cover. Mirrors
	// codexRunFloorLocalSkew.
	antigravityRunFloorLocalSkew = 30 * time.Second
	// antigravityObservedAtGrace absorbs the one-second resolution of the
	// RFC3339 observation times: a reading taken 400 ms after the floor is
	// stamped with the floor's own second and would otherwise read as older
	// than the run it was taken during. It is also what makes a reading taken
	// AT the floor cover it.
	antigravityObservedAtGrace = time.Second

	antigravityFreshnessSchema = 1
	// antigravityFreshnessEnv relocates the state file (tests isolate from the
	// real machine; mirrors AIEXPEDITE_AGY_QUOTA_GATE).
	antigravityFreshnessEnv         = "AIEXPEDITE_AGY_FRESHNESS"
	antigravityFreshnessNoticeLimit = 320
)

// Vars rather than consts so tests can pin them small.
var (
	antigravityRefreshAfterRunRetryDelay = 5 * time.Second
	// antigravityRefreshMinInterval spaces the outbound Google reads a NEW
	// debt may trigger. A debt's own retry is the same unpaid run and bypasses
	// it, exactly as a forced Codex reconcile bypasses
	// codexForcedReconcileMinInterval; the Refresh click does not go through
	// this worker at all, so a user-initiated refresh is never throttled by it.
	antigravityRefreshMinInterval = 60 * time.Second
	antigravityUsageFreshnessNow  = time.Now
)

// antigravityUsageFreshness is the persisted debt. Deliberately its own file
// rather than a field of the quota snapshot: that cache is a single sanitized
// reading written monotonically by saveAntigravityQuotaSnapshotIfNewer, and
// folding mutable counters into it would make an older-but-owed write fight the
// monotonic guard. antigravity_quota_gate.json is the precedent.
type antigravityUsageFreshness struct {
	SchemaVersion int `json:"schemaVersion,omitempty"`
	// RunFloorMs is the newest armed run's start. A restart adopts it for a run
	// nobody settled; each run's own settle uses the floor its arm returned.
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
}

var (
	antigravityFreshnessMu sync.Mutex
	// antigravityRefreshWorkerBusy is the process-wide single flight: one debt
	// worker at a time, however many runs settle at once.
	antigravityRefreshWorkerBusy atomic.Bool
	// antigravityFreshnessInFlight counts the background writes this file owns
	// (the arm persist and the debt worker), so a test can wait them out.
	antigravityFreshnessInFlight atomic.Int64
)

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
	_ = os.WriteFile(path, body, 0o600)
}

// updateAntigravityUsageFreshness applies mutate to the persisted state under
// the freshness lock and returns the result. Callers MUST NOT hold
// antigravityQuotaCacheMu-dependent state here: the lock order in this package
// is cache → freshness (writeAntigravityQuotaSnapshotLocked settles while
// holding the cache lock), so nothing under this lock may read the cache.
func updateAntigravityUsageFreshness(mutate func(*antigravityUsageFreshness)) antigravityUsageFreshness {
	antigravityFreshnessMu.Lock()
	defer antigravityFreshnessMu.Unlock()
	state := readAntigravityUsageFreshnessLocked()
	before := state
	mutate(&state)
	if state != before {
		writeAntigravityUsageFreshnessLocked(state)
	}
	return state
}

// antigravityRebaseFutureFreshness discards floors parked further than
// antigravityRunFloorLocalSkew ahead of now — a backwards clock step, not skew.
// Left in place they would make every later run owe a debt no reading can cover.
func antigravityRebaseFutureFreshness(state *antigravityUsageFreshness, now time.Time) {
	ceiling := now.Add(antigravityRunFloorLocalSkew).UnixMilli()
	if state.RunFloorMs > ceiling {
		state.RunFloorMs = 0
	}
	if state.RefreshOwedFloorMs > ceiling || state.RefreshOwedAtMs > ceiling {
		state.RefreshOwedFloorMs, state.RefreshOwedAtMs = 0, 0
		state.Attempts, state.Outcome, state.Gated = 0, "", false
	}
}

// antigravityObservedAtCovers reports whether an RFC3339 observation time is at
// or after a floor, within the one-second resolution of those timestamps. An
// unparseable reading never covers anything.
func antigravityObservedAtCovers(observedAt string, floorMs int64) bool {
	if floorMs == 0 {
		return true
	}
	at, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return false
	}
	return at.UnixMilli()+antigravityObservedAtGrace.Milliseconds() >= floorMs
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
// capturedSinceFloor is a HINT (the last snapshot the capture path persisted):
// it makes the common ungated case cheap. The decision itself is always a time
// comparison against this run's own floor.
//
// gated says the run's build refused loopback reads, so the debt goes straight
// to the Code Assist route instead of a retry the build will refuse, and the
// notice accessor leaves the wording to antigravityGateNotice.
//
// Called from the capture's finish() on its own goroutine: it must NOT wait for
// the poller, which is ref-counted and outlives a short run whenever a longer
// one is still armed.
func antigravityUsageRunSettled(floor time.Time, capturedSinceFloor, gated bool) {
	if floor.IsZero() {
		return
	}
	now := antigravityUsageFreshnessNow()
	floorMs := floor.UnixMilli()

	if !capturedSinceFloor {
		// The authoritative check: any route may have landed a reading for this
		// run, including a concurrent run's poller.
		capturedSinceFloor = antigravityObservedAtCovers(cachedAntigravityObservedAt(), floorMs)
	}
	if capturedSinceFloor {
		// A reading covers this run, and the write that landed it already
		// cleared any debt through settleAntigravityRunFreshness. Silent on
		// purpose: the poller's own close-out line already reports `captured`
		// for this run, and a second line per run is noise in a log that is
		// uploaded with diagnostics.
		return
	}

	state := updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		antigravityRebaseFutureFreshness(state, now)
		owedFloorMs := floorMs
		if state.RefreshOwedFloorMs > owedFloorMs {
			// One pending debt at a time: a reading that covers the newest
			// floor covers every earlier one.
			owedFloorMs = state.RefreshOwedFloorMs
		}
		if state.RefreshOwedAtMs == 0 || state.RefreshOwedFloorMs != owedFloorMs {
			// A new run, or a floor that moved: this debt gets its own budget.
			state.Attempts, state.Outcome = 0, ""
		}
		state.RefreshOwedFloorMs = owedFloorMs
		state.RefreshOwedAtMs = now.UnixMilli()
		state.Gated = gated
		if state.RunFloorMs < owedFloorMs {
			state.RunFloorMs = owedFloorMs
		}
	})
	fmt.Printf("%s[antigravity-freshness] Run finished with no reading of its own (owed=true gated=%v attempts=%d)%s\n",
		colorYellow, gated, state.Attempts, colorReset)
	antigravityStartRunDebtWorker(antigravityRefreshAfterRunMaxAttempts, false)
}

// settleAntigravityRunFreshness retires whatever a landed reading covers. It is
// called from inside writeAntigravityQuotaSnapshotLocked — while the quota
// cache lock is held — so it must never read the cache back.
func settleAntigravityRunFreshness(observedAt string) {
	at, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return
	}
	observedMs := at.UnixMilli() + antigravityObservedAtGrace.Milliseconds()
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		if state.RefreshOwedAtMs != 0 && observedMs >= state.RefreshOwedFloorMs {
			state.RefreshOwedFloorMs, state.RefreshOwedAtMs = 0, 0
			state.Attempts, state.Outcome, state.Gated = 0, "", false
		}
		if state.RunFloorMs != 0 && observedMs >= state.RunFloorMs {
			// Nothing left for a restart to adopt for that run.
			state.RunFloorMs = 0
		}
	})
}

/* ────────────────────────────────── pay ──────────────────────────────── */

// antigravityStartRunDebtWorker runs the bounded payment on its own goroutine,
// once: a second settle while one is in flight is a no-op, because the debt it
// would pay is the one already being paid.
func antigravityStartRunDebtWorker(maxAttempts int, bypassInterval bool) {
	// Counted BEFORE the claim: a waiter that sampled the counter between a
	// successful claim and the increment would read "idle" while a worker is
	// about to run.
	antigravityFreshnessInFlight.Add(1)
	if !antigravityRefreshWorkerBusy.CompareAndSwap(false, true) {
		antigravityFreshnessInFlight.Add(-1)
		return
	}
	go func() {
		defer antigravityFreshnessInFlight.Add(-1)
		defer antigravityRefreshWorkerBusy.Store(false)
		antigravityPayRunDebt(maxAttempts, bypassInterval)
	}()
}

// antigravityUsageRefreshWaitIdle blocks until no debt worker is in flight,
// bounded. Test seam only — production never waits on a refresh, which is why
// this polls the single-flight flag rather than adding a WaitGroup the settle
// goroutine would have to touch on every run.
func antigravityUsageRefreshWaitIdle() {
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if antigravityFreshnessInFlight.Load() == 0 {
			return
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
			state.RefreshOwedFloorMs, state.RefreshOwedAtMs = 0, 0
			state.Attempts, state.Outcome, state.Gated = 0, "", false
		}
	})
	return state, state.RefreshOwedAtMs != 0
}

// antigravityRetireRunDebt drops the debt without an attempt, for the cases
// where nothing on this machine could ever pay it.
func antigravityRetireRunDebt(reason string) {
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		state.RefreshOwedFloorMs, state.RefreshOwedAtMs = 0, 0
		state.RunFloorMs = 0
		state.Attempts, state.Outcome, state.Gated = 0, "", false
	})
	fmt.Printf("%s[antigravity-freshness] Run refresh debt retired without an attempt (%s)%s\n",
		colorYellow, reason, colorReset)
}

// antigravityPayRunDebt spends at most maxAttempts Code Assist reads on the
// pending debt — the route that still answers on a CSRF-gated build. Each read
// is bounded by antigravityCodeAssistTimeout and the first is spaced from the
// previous outbound read by antigravityRefreshMinInterval; a retry within one
// debt bypasses that interval, because it is the same unpaid run.
//
// It never runs a model turn. The Refresh click may run the `agy models`
// warm-up to make the CLI refresh its own keyring token; doing that behind the
// user's back on run teardown is a different class of side effect, so a
// token_expired debt is KEPT (the next real run refreshes the keyring for free)
// and only a no_login debt stops attempting, since nothing here can pay it.
// The one child this can start is the bounded `<agy> --version` behind
// antigravityCodeAssistBuildVersion's cache, and only on a cold cache.
func antigravityPayRunDebt(maxAttempts int, bypassInterval bool) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(antigravityRefreshAfterRunRetryDelay)
		}
		now := antigravityUsageFreshnessNow()
		state, owed := antigravityPendingDebt(now)
		if !owed {
			return
		}
		// Any route may have landed a reading since the settle: a concurrent
		// run's poller, a Refresh click, or the poller's own 2 s tail on an
		// ungated build.
		if cached := cachedAntigravityObservedAt(); antigravityObservedAtCovers(cached, state.RefreshOwedFloorMs) {
			settleAntigravityRunFreshness(cached)
			return
		}
		if state.Attempts >= antigravityRefreshAfterRunMaxAttempts {
			return
		}
		// An uninstall between the run and now must not leave a debt retrying,
		// or a notice, on a provider the card no longer shows.
		path := antigravityExecutablePath()
		if path == "" {
			antigravityRetireRunDebt("agy is no longer installed")
			return
		}
		// An offline agent makes no outbound request. The debt stays pending
		// for the next run rather than retiring: offline is temporary.
		if IsOffline() {
			fmt.Printf("%s[antigravity-freshness] Run refresh deferred: the agent is offline (debt kept)%s\n",
				colorYellow, colorReset)
			return
		}
		if !bypassInterval && attempt == 0 && state.LastPaidAtMs > 0 {
			if since := now.Sub(time.UnixMilli(state.LastPaidAtMs)); since >= 0 && since < antigravityRefreshMinInterval {
				fmt.Printf("%s[antigravity-freshness] Run refresh deferred: last read %ds ago (minimum %ds, debt kept)%s\n",
					colorCyan, int(since.Seconds()), int(antigravityRefreshMinInterval.Seconds()), colorReset)
				return
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), antigravityCodeAssistTimeout)
		outcome := probeAntigravityQuotaCodeAssistFn(ctx, antigravityCodeAssistBuildVersion(""), antigravityUsageFreshnessNow)
		cancel()
		antigravityRecordRefreshAttempt(outcome, antigravityUsageFreshnessNow())
		fmt.Printf("%s[antigravity-freshness] Run refresh attempt %d/%d finished (%s)%s\n",
			colorCyan, attempt+1, maxAttempts, outcome, colorReset)

		switch outcome {
		case liveProbeOutcomeCodeAssistOK:
			// The persist inside the probe cleared the debt through the settle
			// hook. The one way it did not is an unattributable reading
			// (codeassist_not_attributable), which is reported as its own
			// outcome and leaves the remaining attempt.
			return
		case liveProbeOutcomeCodeAssistNoLogin, liveProbeOutcomeCodeAssistTokenExpired:
			return
		}
	}
}

// antigravityRecordRefreshAttempt books one payment attempt. An outbound read
// updates LastPaidAtMs (what the minimum interval spaces) and the fingerprint
// the reading was taken under; the two local refusals spend no request, so they
// space nothing. Attempts are only counted while a debt is actually pending: a
// successful read has already retired it through the settle hook.
func antigravityRecordRefreshAttempt(outcome string, now time.Time) {
	fingerprint := ""
	if outcome == liveProbeOutcomeCodeAssistOK {
		if snap, ok := cachedAntigravityQuotaSnapshot(); ok {
			fingerprint = snap.AccountFingerprint
		}
	}
	updateAntigravityUsageFreshness(func(state *antigravityUsageFreshness) {
		state.Outcome = outcome
		switch outcome {
		case liveProbeOutcomeCodeAssistNoLogin:
			// Nothing on this machine can pay it: stop attempting, but keep the
			// debt so the card can say why the reading is not moving.
			if state.RefreshOwedAtMs != 0 {
				state.Attempts = antigravityRefreshAfterRunMaxAttempts
			}
		case liveProbeOutcomeCodeAssistTokenExpired:
			// Keep the debt and its budget: the next real `agy` run refreshes
			// the keyring token for free, and this cost no request.
		default:
			state.LastPaidAtMs = now.UnixMilli()
			if fingerprint != "" {
				state.AccountFingerprint = fingerprint
			}
			if state.RefreshOwedAtMs != 0 {
				state.Attempts++
			}
		}
	})
}

// payOwedAntigravityUsageRefresh pays, once, a debt the previous agent process
// left behind: a run that finished just before a restart or self-update, or one
// the process was cut off in the middle of. Exactly one bounded Code Assist
// read; any remainder is paid by the next run under the ordinary bounds.
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
func adoptAndPayOwedAntigravityRunDebt(startedAt time.Time) {
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
		state.Attempts, state.Outcome = 0, ""
		// The build that refused is remembered per build, not per run, so the
		// marker is the honest source for a debt adopted across a restart.
		_, state.Gated = antigravityQuotaGateFor("", now)
	})
	if state.RefreshOwedAtMs == 0 {
		return
	}
	// Bypass the interval: nothing in this fresh process has read yet.
	antigravityStartRunDebtWorker(1, true)
}

/* ──────────────────────────────── report ─────────────────────────────── */

// antigravityFreshnessNotice is the single accessor for the debt's user-facing
// state: the card banner (empty when there is nothing to say) and whether a
// debt is pending at all. No other caller reads the state file.
//
// The notice is empty on a gated build: antigravityGateNotice already names the
// build and the reading's age, and two sources for one banner would drift. It
// is also empty until the bounded attempts are spent, so a debt that is about
// to be paid never flashes a warning. Timestamps and fixed text only — never a
// path, an account or log text.
func antigravityFreshnessNotice(lastObservedAt string, now time.Time) (string, bool) {
	antigravityFreshnessMu.Lock()
	state := readAntigravityUsageFreshnessLocked()
	antigravityFreshnessMu.Unlock()

	antigravityRebaseFutureFreshness(&state, now)
	if state.RefreshOwedAtMs == 0 ||
		now.Sub(time.UnixMilli(state.RefreshOwedAtMs)) > antigravityRefreshOwedMaxAge {
		return "", false
	}
	if state.Gated || state.Attempts < antigravityRefreshAfterRunMaxAttempts {
		return "", true
	}
	if antigravityObservedAtCovers(lastObservedAt, state.RefreshOwedFloorMs) {
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
	// something the user fixes, a failing read is something that heals itself.
	cause := "Google returned no reading for the stored login; it will update on the next run that reports one."
	if state.Outcome == liveProbeOutcomeCodeAssistNoLogin {
		cause = "No Antigravity login is stored on this device, so no reading can be taken; sign in with the CLI to restore it."
	}
	notice := fmt.Sprintf("%s, before the most recent Antigravity run started (%s). %s",
		last, time.UnixMilli(state.RefreshOwedFloorMs).UTC().Format(layout), cause)
	return clampASCII(notice, antigravityFreshnessNoticeLimit), true
}

// antigravityExecutablePath resolves the installed `agy`, or "" when it is not
// on this machine at all. Same resolution as gatherCLIAgents — PATH, then the
// installer's own bin dir, which a macOS GUI/launchd agent's sparse PATH misses
// — so the worker and the card can never disagree about whether the CLI exists.
// resolveExecutable is deliberately NOT used: it echoes the command back on a
// miss, which would read as "installed" forever.
func antigravityExecutablePath() string {
	const command = "agy"
	if path, err := exec.LookPath(command); err == nil {
		return path
	}
	return resolveInstallerBinary(command, installerBinDirFor(command))
}
