// cliagent_usage_opencode_freshness.go — the run lifecycle that gets an
// OpenCode turn's spend into the ledger (cliagent_usage_opencode_capture.go),
// and the bounded `opencode export` fallback for a turn whose stream reported
// none.
//
//   - Arm, when the turn's child spawns: the run floor is now, and an ARMED
//     debt is written synchronously. A crash or self-update mid-turn drops the
//     in-memory accumulator; the armed debt (once a frame named the session) is
//     what the next process pays from.
//   - Capture: every streamed line goes to captureOpenCodeUsageLine — memory
//     only. The first session id is recorded on the debt, one write per run,
//     off the stream goroutine.
//   - Settle, once per run (terminal event or exit, whichever comes first):
//     captured steps commit and retire the debt in one write. A run with no
//     captured step but a session id keeps the debt, OWED, written
//     synchronously before an update hand-off can replace the process. With no
//     session id nothing can be exported: the debt is dropped
//     ("unattributable").
//   - Ladder: attempts at 15 s, 1 m and 5 m, each one `opencode export <id>`
//     with a 5 s timeout. A refusal that spent nothing (offline, binary
//     missing) books refreshFreeRetryDelay without consuming an attempt. A
//     debt is retired when it is paid, its attempts are spent, or it is 6 h old.
//   - Survive: StartAgent calls payOwedOpenCodeUsage, which re-arms the ladder
//     for the previous process's debts — including armed ones it never
//     settled.
//
// It is a module of its own rather than a fork of the Codex or Grok freshness
// paths: those are bound to their own caches and generation types. Only the
// ladder arithmetic (cliagent_usage_refresh_ladder.go) is shared.
//
// Logs are fixed labels only: never a session id, path or export output.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Vars so tests can pin them small.
var (
	openCodeUsageDebtLadder   = []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}
	openCodeUsageDebtMaxAge   = 6 * time.Hour
	openCodeExportTimeout     = 5 * time.Second
	openCodeUsageFreeFloor    = 15 * time.Second
	openCodeUsageAfterFunc    = time.AfterFunc
	openCodeUsageFreshnessNow = time.Now
)

const (
	openCodeUsageDebtMaxAttempts = 3
	// openCodeExportMaxBytes bounds the export capture: a long session's
	// export carries every tool output. One that does not fit is unreadable,
	// never a partial count.
	openCodeExportMaxBytes = 32 << 20
)

var (
	// openCodeUsageInFlight tracks the one-off background work this file starts
	// (a session-id write, the startup adoption) so tests can wait for it.
	openCodeUsageInFlight sync.WaitGroup
	// openCodeUsageTimers holds each debt's booked attempt, by run id.
	openCodeUsageTimersMu sync.Mutex
	openCodeUsageTimers   = map[string]*time.Timer{}
)

/* --------------------------------------------------------------------------
   Arm / settle
   -------------------------------------------------------------------------- */

// armOpenCodeUsageRun starts a run at the moment its child spawns. fingerprint
// may be "" (resolved again at settle); executable and dir are where the
// export fallback would run. The armed debt is written before returning.
func armOpenCodeUsageRun(executable, dir, fingerprint string) *openCodeUsageRun {
	now := openCodeUsageFreshnessNow()
	run := &openCodeUsageRun{
		id:          newOpenCodeUsageRunID(),
		floorMs:     now.UnixMilli(),
		fingerprint: fingerprint,
		executable:  executable,
		dir:         dir,
	}
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		ledger.Debts = append(ledger.Debts, openCodeUsageDebt{
			RunID:              run.id,
			RunFloorMs:         run.floorMs,
			AccountFingerprint: fingerprint,
			Dir:                dir,
		})
		capOpenCodeUsageDebts(ledger)
		return true, false
	})
	return run
}

// armOpenCodeUsageRunForExecutable arms with the account the readiness cache
// last named for executable — no probe.
func armOpenCodeUsageRunForExecutable(executable, dir string) *openCodeUsageRun {
	return armOpenCodeUsageRun(executable, dir, openCodeKnownAccountFingerprint(executable))
}

// persistSessionIDAsync records the run's session id on its armed debt.
func (run *openCodeUsageRun) persistSessionIDAsync(sessionID string) {
	if run == nil || sessionID == "" || !run.persistedSession.CompareAndSwap(false, true) {
		return
	}
	openCodeUsageInFlight.Add(1)
	go func() {
		defer openCodeUsageInFlight.Done()
		if run.settled.Load() {
			return
		}
		openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			debt := openCodeUsageDebtByID(ledger, run.id)
			if debt == nil || debt.SessionID != "" || debt.owed() {
				return false, false
			}
			debt.SessionID = sessionID
			return true, false
		})
	}()
}

// settleOpenCodeUsageRun settles the run exactly once. sessionID is any id the
// caller learned outside the stream (a resume id); the stream's own wins.
func settleOpenCodeUsageRun(run *openCodeUsageRun, sessionID string) {
	if run == nil || !run.settled.CompareAndSwap(false, true) {
		return
	}
	steps, streamSession := run.takeUncommitted()
	sessionID = firstNonEmpty(streamSession, sessionID)
	fingerprint := run.settleFingerprint()
	now := openCodeUsageFreshnessNow()

	if len(steps) > 0 {
		commitOpenCodeUsageSteps(run, fingerprint, steps)
		return
	}
	if sessionID == "" {
		disarmOpenCodeUsageRunID(run.id)
		logOpenCodeUsageCapture("unattributable")
		return
	}
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		debt := openCodeUsageDebtByID(ledger, run.id)
		if debt == nil {
			ledger.Debts = append(ledger.Debts, openCodeUsageDebt{RunID: run.id, RunFloorMs: run.floorMs, Dir: run.dir})
			capOpenCodeUsageDebts(ledger)
			if debt = openCodeUsageDebtByID(ledger, run.id); debt == nil {
				return false, false
			}
		}
		debt.SessionID = sessionID
		debt.AccountFingerprint = fingerprint
		debt.OwedAtMs = now.UnixMilli()
		debt.SettledAtMs = now.UnixMilli()
		debt.Attempts = 0
		return true, false
	})
	if !committed {
		return
	}
	logOpenCodeUsageCapture("owed")
	scheduleOpenCodeUsageAttempt(run.id, openCodeUsageDebtLadder[0])
}

// flushOpenCodeUsageRun commits steps a settled run captured after its settle
// — a session whose terminal event was followed by another step before it
// exited. No debt changes: the settle already decided those.
func flushOpenCodeUsageRun(run *openCodeUsageRun) {
	if run == nil || !run.settled.Load() {
		return
	}
	if steps, _ := run.takeUncommitted(); len(steps) > 0 {
		commitOpenCodeUsageSteps(run, run.settleFingerprint(), steps)
	}
}

// commitOpenCodeUsageSteps folds steps into the ledger and retires the run's
// debt in one write.
func commitOpenCodeUsageSteps(run *openCodeUsageRun, fingerprint string, steps []openCodeUsageStep) {
	committed, changed, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		removeOpenCodeUsageDebt(ledger, run.id)
		return true, mergeOpenCodeUsageSteps(ledger, fingerprint, steps)
	})
	noteOpenCodeUsageAdvanced(committed, changed, generation)
	if committed {
		logOpenCodeUsageCapture("committed")
	}
}

// settleFingerprint is the account the run counts toward: the one known at
// arm, else what the readiness cache names now.
func (run *openCodeUsageRun) settleFingerprint() string {
	if run.fingerprint != "" {
		return run.fingerprint
	}
	return openCodeKnownAccountFingerprint(run.executable)
}

// settleOrDisarmOpenCodeSmokeRun settles a smoke run. A FAILED smoke with no
// completed step owes nothing — the turn may never have reached a model, and
// an export cannot tell. Completed steps still commit: those tokens were spent.
func settleOrDisarmOpenCodeSmokeRun(run *openCodeUsageRun, succeeded bool) {
	if run == nil {
		return
	}
	if steps, _ := run.snapshot(); !succeeded && len(steps) == 0 {
		disarmOpenCodeUsageRun(run)
		return
	}
	settleOpenCodeUsageRun(run, "")
}

// disarmOpenCodeUsageRun drops a run whose child never started.
func disarmOpenCodeUsageRun(run *openCodeUsageRun) {
	if run != nil && run.settled.CompareAndSwap(false, true) {
		disarmOpenCodeUsageRunID(run.id)
	}
}

func disarmOpenCodeUsageRunID(runID string) {
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		return removeOpenCodeUsageDebt(ledger, runID), false
	})
}

func openCodeUsageDebtByID(ledger *openCodeUsageLedger, runID string) *openCodeUsageDebt {
	for i := range ledger.Debts {
		if ledger.Debts[i].RunID == runID {
			return &ledger.Debts[i]
		}
	}
	return nil
}

func removeOpenCodeUsageDebt(ledger *openCodeUsageLedger, runID string) bool {
	for i := range ledger.Debts {
		if ledger.Debts[i].RunID == runID {
			ledger.Debts = append(ledger.Debts[:i], ledger.Debts[i+1:]...)
			return true
		}
	}
	return false
}

// capOpenCodeUsageDebts drops the oldest runs past the cap.
func capOpenCodeUsageDebts(ledger *openCodeUsageLedger) {
	for len(ledger.Debts) > openCodeUsageMaxDebts {
		oldest := 0
		for i, d := range ledger.Debts {
			if d.RunFloorMs < ledger.Debts[oldest].RunFloorMs {
				oldest = i
			}
		}
		ledger.Debts = append(ledger.Debts[:oldest], ledger.Debts[oldest+1:]...)
	}
}

func newOpenCodeUsageRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b)
}

// openCodeKnownAccountFingerprint is the account fingerprint the card would
// publish for executable, from the providers the readiness cache last named —
// never a probe. The capacity view keys the install by this, so a run counts
// toward the bucket the card reads. When the cache knows exactly one install
// (the usual case) its names stand in for an executable spelled differently
// (`opencode` vs the resolved path).
func openCodeKnownAccountFingerprint(executable string) string {
	openCodeReadinessMu.Lock()
	providers, ok := openCodeLastProviders[executable]
	if !ok && len(openCodeLastProviders) == 1 {
		for _, only := range openCodeLastProviders {
			providers = only
		}
	}
	openCodeReadinessMu.Unlock()
	return openCodeAccountFingerprintFor(providers)
}

// openCodeAccountFingerprintFor mirrors ParseContext: the joined provider
// names are the Account.
func openCodeAccountFingerprintFor(providers []string) string {
	account := ""
	if len(providers) > 0 {
		account = joinOpenCodeProviders(providers)
	}
	return fingerprintAccount(openCodeUsageProvider, account)
}

/* --------------------------------------------------------------------------
   Ladder
   -------------------------------------------------------------------------- */

// scheduleOpenCodeUsageAttempt books the debt's next attempt after d,
// replacing any earlier booking.
func scheduleOpenCodeUsageAttempt(runID string, d time.Duration) {
	openCodeUsageTimersMu.Lock()
	defer openCodeUsageTimersMu.Unlock()
	if t := openCodeUsageTimers[runID]; t != nil {
		t.Stop()
	}
	openCodeUsageTimers[runID] = openCodeUsageAfterFunc(d, func() { attemptOpenCodeUsageDebt(runID) })
}

func clearOpenCodeUsageTimer(runID string) {
	openCodeUsageTimersMu.Lock()
	if t := openCodeUsageTimers[runID]; t != nil {
		t.Stop()
	}
	delete(openCodeUsageTimers, runID)
	openCodeUsageTimersMu.Unlock()
}

// attemptOpenCodeUsageDebt makes one export attempt for a debt.
func attemptOpenCodeUsageDebt(runID string) {
	now := openCodeUsageFreshnessNow()
	openCodeUsageMu.Lock()
	ledger := readOpenCodeUsageLedger()
	openCodeUsageMu.Unlock()
	found := openCodeUsageDebtByID(&ledger, runID)
	if found == nil || !found.owed() {
		clearOpenCodeUsageTimer(runID)
		return
	}
	debt := *found
	owedFor := now.Sub(time.UnixMilli(debt.OwedAtMs))
	if owedFor > openCodeUsageDebtMaxAge {
		retireOpenCodeUsageDebt(runID, "aged_out")
		return
	}
	executable := resolveOpenCodeExecutable()
	if IsOffline() || executable == "" {
		// Spent nothing: no attempt consumed, the delay grows with the debt.
		label := "deferred_offline"
		if executable == "" {
			label = "deferred_binary_missing"
		}
		logOpenCodeUsageCapture(label)
		scheduleOpenCodeUsageAttempt(runID, refreshFreeRetryDelay(owedFor, openCodeUsageFreeFloor, openCodeUsageDebtLadder))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), openCodeExportTimeout)
	steps, ok := readOpenCodeExportUsage(ctx, executable, debt)
	cancel()
	if ok && len(steps) > 0 {
		committed, changed, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
			if openCodeUsageDebtByID(ledger, runID) == nil {
				return false, false // paid or retired meanwhile
			}
			removeOpenCodeUsageDebt(ledger, runID)
			return true, mergeOpenCodeUsageSteps(ledger, debt.AccountFingerprint, steps)
		})
		noteOpenCodeUsageAdvanced(committed, changed, generation)
		if committed {
			clearOpenCodeUsageTimer(runID)
			logOpenCodeUsageCapture("export_paid")
		}
		return
	}

	// The export ran and paid nothing (unreadable, or the session holds no
	// assistant message yet): an attempt is spent.
	attempts := debt.Attempts + 1
	committed, _, _ := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		d := openCodeUsageDebtByID(ledger, runID)
		if d == nil {
			return false, false
		}
		d.Attempts = attempts
		return true, false
	})
	if !committed {
		clearOpenCodeUsageTimer(runID)
		return
	}
	// The ladder is indexed by attempts already MADE: the head (15 s) was the
	// settle's booking, so attempt n books ladder[n]. refreshRetryDelayForAttempt
	// treats rung 0 as the settle's own attempt, hence the +1.
	if delay, more := refreshRetryDelayForAttempt(attempts+1, openCodeUsageDebtMaxAttempts+1, openCodeUsageDebtLadder); more {
		logOpenCodeUsageCapture("export_retry")
		scheduleOpenCodeUsageAttempt(runID, delay)
		return
	}
	retireOpenCodeUsageDebt(runID, "exhausted")
}

func retireOpenCodeUsageDebt(runID, label string) {
	disarmOpenCodeUsageRunID(runID)
	clearOpenCodeUsageTimer(runID)
	logOpenCodeUsageCapture(label)
}

// payOwedOpenCodeUsage runs at StartAgent: every debt left by an earlier
// process is re-armed on the ladder. An armed debt (a run cut off before it
// settled) becomes owed when it named a session and is dropped otherwise. Debts
// this process armed (a smoke racing startup) are left to their own settle.
func payOwedOpenCodeUsage() {
	startedAt := openCodeUsageFreshnessNow()
	openCodeUsageInFlight.Add(1)
	go func() {
		defer openCodeUsageInFlight.Done()
		adoptOwedOpenCodeUsage(startedAt)
	}()
}

func adoptOwedOpenCodeUsage(startedAt time.Time) {
	now := openCodeUsageFreshnessNow()
	var owed []openCodeUsageDebt
	var dropped, aged int
	openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		write := false
		kept := ledger.Debts[:0]
		for _, d := range ledger.Debts {
			switch {
			case !d.owed() && d.RunFloorMs >= startedAt.UnixMilli():
				kept = append(kept, d) // this process's own run
				continue
			case !d.owed() && d.SessionID == "":
				dropped++
				write = true
				continue
			case !d.owed():
				d.OwedAtMs = now.UnixMilli()
				write = true
			case now.Sub(time.UnixMilli(d.OwedAtMs)) > openCodeUsageDebtMaxAge:
				aged++
				write = true
				continue
			}
			kept = append(kept, d)
			owed = append(owed, d)
		}
		ledger.Debts = kept
		return write, false
	})
	for range dropped {
		logOpenCodeUsageCapture("unattributable")
	}
	for range aged {
		logOpenCodeUsageCapture("aged_out")
	}
	for _, d := range owed {
		delay := openCodeUsageDebtLadder[0]
		if next, more := refreshRetryDelayForAttempt(d.Attempts+1, openCodeUsageDebtMaxAttempts+1, openCodeUsageDebtLadder); more && d.Attempts > 0 {
			delay = next
		}
		logOpenCodeUsageCapture("resumed")
		scheduleOpenCodeUsageAttempt(d.RunID, delay)
	}
}

/* --------------------------------------------------------------------------
   Export fallback
   -------------------------------------------------------------------------- */

// runOpenCodeExport runs `opencode export <sessionID>` and returns its stdout.
// A var so tests drive the fallback without a binary.
var runOpenCodeExport = func(ctx context.Context, executable, dir, sessionID string) ([]byte, bool) {
	cmd, err := newOpenCodeCmd(ctx, openCodeLaunch{
		Path: executable,
		Args: []string{"export", sessionID},
		Env:  sanitizeOpenCodeEnv(os.Environ()),
		Dir:  dir,
		// A background read must not become the CLI's self-update.
		Maintenance: true,
	})
	if err != nil {
		return nil, false
	}
	out := &boundedBuffer{limit: openCodeExportMaxBytes}
	cmd.Stdout = out
	if cmd.Run() != nil || out.buf.Len() >= openCodeExportMaxBytes {
		return nil, false
	}
	return out.buf.Bytes(), true
}

// readOpenCodeExportUsage pays a debt from the session's export: the assistant
// messages created at or after the run floor (and, for a settled run, no later
// than its settle), one step each, keyed by message id so a second debt on the
// same session never counts a message twice. ok=false means the export could
// not be read; an unknown shape yields no steps, never a guessed number. The
// export bytes go no further than this function.
func readOpenCodeExportUsage(ctx context.Context, executable string, debt openCodeUsageDebt) ([]openCodeUsageStep, bool) {
	dir := debt.Dir
	if dir != "" {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			// The smoke's per-run directory is gone; any non-repository
			// directory resolves to the same global project.
			dir = os.TempDir()
		}
	}
	out, ok := runOpenCodeExport(ctx, executable, dir, debt.SessionID)
	if !ok {
		return nil, false
	}
	return parseOpenCodeExportUsage(out, debt), true
}

// openCodeExportMessage is one exported message, read permissively: the
// fields sit under `info` in current releases and at the top level in some.
type openCodeExportMessage struct {
	Info *openCodeExportMessageInfo `json:"info"`
	openCodeExportMessageInfo
}

type openCodeExportMessageInfo struct {
	ID     string          `json:"id"`
	Role   string          `json:"role"`
	Tokens json.RawMessage `json:"tokens"`
	Cost   json.RawMessage `json:"cost"`
	Time   struct {
		Created   json.RawMessage `json:"created"`
		Completed json.RawMessage `json:"completed"`
	} `json:"time"`
}

func parseOpenCodeExportUsage(out []byte, debt openCodeUsageDebt) []openCodeUsageStep {
	var export struct {
		Messages []openCodeExportMessage `json:"messages"`
	}
	if json.Unmarshal(out, &export) != nil {
		return nil
	}
	var steps []openCodeUsageStep
	for _, m := range export.Messages {
		info := m.openCodeExportMessageInfo
		if m.Info != nil {
			info = *m.Info
		}
		if info.Role != "assistant" || info.ID == "" {
			continue
		}
		created, ok := openCodeUsageMillis(info.Time.Created)
		if !ok || created < debt.RunFloorMs {
			continue
		}
		if debt.SettledAtMs > 0 && created > debt.SettledAtMs {
			continue // a later turn on the same session pays its own debt
		}
		step, valid := openCodeUsageFromTokens(info.Tokens, info.Cost)
		if !valid {
			continue
		}
		step.AtMs = created
		if completed, ok := openCodeUsageMillis(info.Time.Completed); ok {
			step.AtMs = completed
		}
		step.Key = openCodeUsageStepKey(debt.SessionID, "message:"+info.ID)
		steps = append(steps, step)
	}
	return steps
}

// resetOpenCodeUsageFreshness stops every booked attempt (tests).
func resetOpenCodeUsageFreshness() {
	openCodeUsageTimersMu.Lock()
	for id, t := range openCodeUsageTimers {
		t.Stop()
		delete(openCodeUsageTimers, id)
	}
	openCodeUsageTimersMu.Unlock()
	openCodeUsageInFlight.Wait()
}
