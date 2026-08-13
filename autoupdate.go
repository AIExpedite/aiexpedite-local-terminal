// File: autoupdate.go
// -----------------------------------------------------------------------------
// Silent, job-aware automatic update: the scheduler and per-attempt state
// machine behind the "Automatically update" tray preference.
//
// When the preference is enabled the agent checks for a newer trusted release
// (first ~5 minutes after launch, then every 6 hours), and on finding one:
//
//	check → download+verify → drain → install → restart
//
// It enters DRAINING (drain.go) so it refuses new work while finishing accepted
// work, reports draining to the terminal-service so the cloud stops routing to
// it, waits for ActiveWork() to reach zero (up to a 7-day deadline), then
// installs via applyVerifiedUpdate and restarts. Nothing in this path prompts
// the user; the only visible evidence is the tray "Updating after current work"
// status item and, after restart, the new version label.
//
// This file also owns the update-state accessors (updatePath / updatePending /
// pendingUpdateInfo) that used to sit in agent.go among unrelated lifecycle
// globals — they live next to the code that reads and writes them.
//
// The core is written against injected dependencies (clock, check/verify/apply,
// ActiveWork, drain RPCs) so the attempt state machine is unit-testable without
// real network or real time.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// silentUpdateCapableFlag is "true" on builds that can replace themselves
// silently (Windows, Linux, and macOS bundle replacement). A macOS build may
// set it "false" via -ldflags "-X main.silentUpdateCapableFlag=false" to ship
// the check-and-offer fallback (the same persisted preference, relabelled
// "Automatically check for updates", installing only when the user clicks
// "Install Update"). It never changes Windows/Linux behaviour or labelling.
var silentUpdateCapableFlag = "true"

// silentUpdateCapable reports whether this build installs updates silently.
// Only macOS can be shipped non-capable; Windows and Linux are always capable.
func silentUpdateCapable() bool {
	if runtime.GOOS == "darwin" {
		return silentUpdateCapableFlag != "false"
	}
	return true
}

// autoUpdateTrayLabel returns the checkbox label + tooltip. On a non-capable
// macOS build the wording never promises more than the platform delivers.
func autoUpdateTrayLabel() (label, tooltip string) {
	if !silentUpdateCapable() {
		return "Automatically check for updates",
			"Automatically check for updates; they install when you choose Install Update"
	}
	return "Automatically update",
		"Keep AI Expedite up to date automatically, installing in the background after current work finishes"
}

/* ───────────────────────── update-state accessors ───────────────────────── */

var (
	updatePath    string
	updatePending bool
	updateMutex   sync.RWMutex

	// Pending update state (when user clicks "Later" on the manual dialog).
	pendingUpdateInfo  *UpdateInfo
	pendingUpdateMutex sync.RWMutex
)

// SetUpdateReady stores the path of the downloaded update binary and marks the
// update as pending. Called before systray.Quit() so onTrayExit performs the
// self-replace handoff.
func SetUpdateReady(path string) {
	updateMutex.Lock()
	updatePath = path
	updatePending = true
	updateMutex.Unlock()
}

// GetUpdateReady returns the pending update path and whether an update is ready.
func GetUpdateReady() (path string, pending bool) {
	updateMutex.RLock()
	defer updateMutex.RUnlock()
	return updatePath, updatePending
}

// SetPendingUpdate stores update info for later installation (manual "Later").
func SetPendingUpdate(info *UpdateInfo) {
	pendingUpdateMutex.Lock()
	pendingUpdateInfo = info
	pendingUpdateMutex.Unlock()
}

// GetPendingUpdate returns pending update info or nil if none.
func GetPendingUpdate() *UpdateInfo {
	pendingUpdateMutex.RLock()
	defer pendingUpdateMutex.RUnlock()
	return pendingUpdateInfo
}

// HasPendingUpdate returns true if an update is waiting to be installed.
func HasPendingUpdate() bool {
	return GetPendingUpdate() != nil
}

// ClearPendingUpdate removes the pending update info.
func ClearPendingUpdate() {
	pendingUpdateMutex.Lock()
	pendingUpdateInfo = nil
	pendingUpdateMutex.Unlock()
}

/* ───────────────────────────── scheduler config ─────────────────────────── */

const (
	autoUpdateFirstCheckDelay   = 5 * time.Minute
	autoUpdateCheckInterval     = 6 * time.Hour
	autoUpdateDrainDeadline     = 7 * 24 * time.Hour
	autoUpdateDeferCooldown     = 24 * time.Hour
	autoUpdateDrainConfirmEvery = 5 * time.Minute
	autoUpdateDrainPollInterval = 10 * time.Second
	autoUpdateMaxRetries        = 3
	autoUpdateRetryMin          = 1 * time.Minute
	autoUpdateRetryMax          = 30 * time.Minute

	// heartbeatStaleWindow mirrors terminal-service ONLINE_THRESHOLD_MS: once a
	// registered agent has been refusing work locally for longer than this, the
	// service's lastSeen has gone stale and it has stopped routing to the
	// device, so a partitioned agent may safely install.
	heartbeatStaleWindow = 5 * time.Minute
)

// trayUpdateHandles lets the scheduler reflect draining / blocked status in the
// tray without importing systray specifics. All fields are optional (nil-safe).
type trayUpdateHandles struct {
	showDraining func()
	hideDraining func()
	showBlocked  func(reason string)
	hideBlocked  func()
	// showPendingInstall reveals the manual "Install Update (vX)" tray item.
	// Used by the macOS check-and-offer fallback, which does NOT drain or
	// restart itself — it surfaces the pending update for the user to click.
	showPendingInstall func(version string)
}

func (t *trayUpdateHandles) draining(on bool) {
	if t == nil {
		return
	}
	if on {
		if t.showDraining != nil {
			t.showDraining()
		}
	} else if t.hideDraining != nil {
		t.hideDraining()
	}
}

func (t *trayUpdateHandles) blocked(on bool, reason string) {
	if t == nil {
		return
	}
	if on {
		if t.showBlocked != nil {
			t.showBlocked(reason)
		}
	} else if t.hideBlocked != nil {
		t.hideBlocked()
	}
}

/* ─────────────────────────── attempt state machine ──────────────────────── */

// autoUpdater owns the scheduler and one-attempt-at-a-time state machine.
// Dependencies are injectable so the machine can be driven by tests with a
// simulated clock and stubbed network.
type autoUpdater struct {
	cfg  *Config
	tray *trayUpdateHandles

	// Injected dependencies (defaulted by newAutoUpdater to the real impls).
	now            func() time.Time
	checkForUpdate func() (*UpdateInfo, error)
	downloadVerify func(*UpdateInfo) (string, error)
	apply          func(path string, info *UpdateInfo) error
	activeWork     func() int
	installable    func() (bool, string)
	drainEnter     func(ctx context.Context, attemptID, target string) error
	drainConfirm   func(ctx context.Context, attemptID, target string) error
	drainExit      func(ctx context.Context, attemptID, reason string) error
	reportOnline   func(ctx context.Context)
	// sleep waits d (or until stopped). It advances the simulated clock in
	// tests. Returns false if the updater was stopped during the wait.
	sleep func(d time.Duration) bool

	// savePath is where the crash-recovery attempt marker is persisted.
	// Defaults to ConfigPath(); overridable in tests.
	savePath string

	mu            sync.Mutex
	inProgress    bool
	deferredUntil time.Time // cooldown floor after a defer (24h)

	attemptSeq atomic.Uint64
	stopCh     chan struct{}
	triggerCh  chan struct{}
}

// globalAutoUpdater is the running scheduler, so tray toggles can nudge it.
var globalAutoUpdater *autoUpdater

// newAutoUpdater wires the real dependencies.
func newAutoUpdater(cfg *Config, tray *trayUpdateHandles) *autoUpdater {
	au := &autoUpdater{
		cfg:            cfg,
		tray:           tray,
		now:            time.Now,
		checkForUpdate: checkForNewVersion,
		downloadVerify: downloadAndVerifyUpdate,
		apply:          applyVerifiedUpdate,
		activeWork:     ActiveWork,
		installable:    installUpdatable,
		savePath:       ConfigPath(),
		stopCh:         make(chan struct{}),
		triggerCh:      make(chan struct{}, 1),
	}
	// Enter and confirm are the same wire call (the service distinguishes by
	// attempt); kept as two hooks so the state machine reads clearly and tests
	// can observe enters vs confirms independently.
	au.drainEnter = func(ctx context.Context, attemptID, target string) error {
		return notifyDrain(ctx, cfg, attemptID, target)
	}
	au.drainConfirm = func(ctx context.Context, attemptID, target string) error {
		return notifyDrain(ctx, cfg, attemptID, target)
	}
	au.drainExit = func(ctx context.Context, attemptID, reason string) error {
		return notifyDrainExit(ctx, cfg, attemptID, reason)
	}
	au.reportOnline = func(ctx context.Context) {
		if cfg.IsRegistered() && !cfg.OfflineMode {
			_ = notifyOnline(ctx, cfg)
		}
	}
	au.sleep = au.realSleep
	return au
}

// StartAutoUpdateScheduler launches the background scheduler. Replaces the old
// one-shot 3-second proactive check. Safe to call once from onTrayReady.
func StartAutoUpdateScheduler(cfg *Config, tray *trayUpdateHandles) *autoUpdater {
	au := newAutoUpdater(cfg, tray)
	globalAutoUpdater = au
	go au.run()
	return au
}

// realSleep waits d unless the updater is stopped. Returns false if stopped.
func (au *autoUpdater) realSleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-au.stopCh:
		return false
	}
}

// nudge asks the scheduler to run a check soon without an app restart. Called
// when the user turns the preference on. Non-blocking / coalescing.
func (au *autoUpdater) nudge() {
	if au == nil {
		return
	}
	select {
	case au.triggerCh <- struct{}{}:
	default:
	}
}

// stop halts the scheduler (used by tests).
func (au *autoUpdater) stop() {
	if au == nil {
		return
	}
	select {
	case <-au.stopCh:
	default:
		close(au.stopCh)
	}
}

// run is the scheduling loop: first check after the initial delay, then every
// interval, plus on an enable-nudge.
func (au *autoUpdater) run() {
	first := time.NewTimer(autoUpdateFirstCheckDelay)
	defer first.Stop()
	ticker := time.NewTicker(autoUpdateCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-au.stopCh:
			return
		case <-first.C:
			au.tick()
		case <-ticker.C:
			au.tick()
		case <-au.triggerCh:
			au.tick()
		}
	}
}

// tick runs one scheduled attempt if the preference is on, no attempt is in
// progress, and the post-defer cooldown has elapsed.
func (au *autoUpdater) tick() {
	if au.cfg == nil || !au.cfg.IsAutoUpdate() {
		return // preference off — no automatic checks
	}
	au.mu.Lock()
	if au.inProgress || au.now().Before(au.deferredUntil) {
		au.mu.Unlock()
		return
	}
	au.inProgress = true
	au.mu.Unlock()

	defer func() {
		au.mu.Lock()
		au.inProgress = false
		au.mu.Unlock()
	}()

	au.runAttempt()
}

// runAttempt performs one full attempt: writability probe, bounded
// check+download+verify, then drain+install.
func (au *autoUpdater) runAttempt() {
	// A read-only / not-yet-relocated install cannot update itself. Say so in
	// the tray and stop — never retry silently in a loop.
	if ok, reason := au.installable(); !ok {
		fmt.Printf("[autoupdate] Install location not updatable: %s\n", reason)
		au.tray.blocked(true, reason)
		return
	}
	au.tray.blocked(false, "")

	// macOS check-and-offer fallback build: check ONLY — do not download, drain,
	// or restart. Surface the newer version as a pending "Install Update" the
	// user installs by choice (which downloads + verifies at click time). We
	// deliberately skip the download here so a fallback build does not fetch a
	// full DMG every 6h and leave the verified temp artifact unused.
	if !silentUpdateCapable() {
		info := au.checkOnlyWithRetry()
		if info == nil {
			return
		}
		fmt.Printf("[autoupdate] Check-and-offer build: offering %s for manual install\n", info.LatestVersion)
		if au.tray != nil && au.tray.showPendingInstall != nil {
			au.tray.showPendingInstall(info.LatestVersion)
		}
		SetPendingUpdate(info)
		return
	}

	info, path := au.checkAndVerifyWithRetry()
	if info == nil || path == "" {
		return // no update, or abandoned after retries
	}

	// The automatic path deliberately IGNORES cfg.SkippedVersion — Skip Version
	// is a choice made in the manual dialog and must not suppress an enabled
	// automatic update. The manual flow keeps honouring it.
	au.drainAndInstall(newAttemptID(au.now(), au.attemptSeq.Add(1)), info, path)
}

// checkOnlyWithRetry runs the metadata check (no download) with bounded
// retries, for the macOS check-and-offer fallback. Returns the newer release
// info, or nil when there is nothing newer or the check was abandoned.
func (au *autoUpdater) checkOnlyWithRetry() *UpdateInfo {
	for attempt := 1; attempt <= autoUpdateMaxRetries; attempt++ {
		info, err := au.checkForUpdate()
		if err == nil {
			if info == nil || !info.Available {
				return nil
			}
			return info
		}
		fmt.Printf("[autoupdate] check attempt %d/%d failed: %v\n",
			attempt, autoUpdateMaxRetries, err)
		if attempt < autoUpdateMaxRetries {
			if !au.sleep(retryBackoff(attempt)) {
				return nil
			}
		}
	}
	return nil
}

// checkAndVerifyWithRetry runs check+download+verify with bounded retries. A
// "no update available" result returns immediately (not a failure). Returns
// (nil, "") when there is nothing to install or the attempt was abandoned.
func (au *autoUpdater) checkAndVerifyWithRetry() (*UpdateInfo, string) {
	for attempt := 1; attempt <= autoUpdateMaxRetries; attempt++ {
		info, err := au.checkForUpdate()
		if err == nil && (info == nil || !info.Available) {
			return nil, "" // nothing newer
		}
		if err == nil {
			path, dErr := au.downloadVerify(info)
			if dErr == nil {
				return info, path
			}
			err = dErr
		}
		fmt.Printf("[autoupdate] check/verify attempt %d/%d failed: %v\n",
			attempt, autoUpdateMaxRetries, err)
		if attempt < autoUpdateMaxRetries {
			if !au.sleep(retryBackoff(attempt)) {
				return nil, ""
			}
		}
	}
	// Abandoned after the last failure; the next scheduled check starts fresh.
	return nil, ""
}

// drainAndInstall enters draining, waits for work to finish (bounded by the
// 7-day deadline), then installs. Handles preference-off cancel, deferral, and
// confirmation-rejection abandonment.
func (au *autoUpdater) drainAndInstall(attemptID string, info *UpdateInfo, path string) {
	// The verified artifact is owned by this call. Delete it on any exit that
	// does NOT hand it to a successful install (preference-off, deferral,
	// expiry, install failure, updater stop) so a long-running process that
	// repeatedly defers doesn't accumulate temp artifacts until the next
	// startup sweep. A successful apply() sets installed=true because the
	// artifact is then consumed by the self-replace handoff / bundle swap.
	installed := false
	defer func() {
		if !installed && path != "" {
			_ = os.Remove(path)
		}
	}()

	// Local refusal is authoritative from this instant — before the service
	// even learns of the drain.
	closeAdmission(attemptID)
	au.tray.draining(true)
	au.persistAttempt(attemptID, info.LatestVersion)

	registered := au.cfg.IsRegistered() && !au.cfg.OfflineMode
	reachedService := false
	if registered {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := au.drainEnter(ctx, attemptID, info.LatestVersion); err == nil {
			reachedService = true
		}
		cancel()
	}

	startedAt := au.now()
	deadline := startedAt.Add(autoUpdateDrainDeadline)
	lastConfirm := startedAt

	for {
		// Preference turned off before replacement began — cancel cleanly and
		// return the agent to normal admission without terminating work.
		if !au.cfg.IsAutoUpdate() {
			au.exitDrain(attemptID, "preference_off", false)
			return
		}

		if au.activeWork() == 0 && au.canInstallNow(startedAt, registered, reachedService) {
			break
		}

		if au.now().After(deadline) {
			// 7-day window elapsed with work still active: defer, become
			// routable again, and don't retry for 24h so a continuously-busy
			// device can't re-enter a week-long drain every 6 hours.
			au.exitDrain(attemptID, "deferred", true)
			return
		}

		if registered && au.now().Sub(lastConfirm) >= autoUpdateDrainConfirmEvery {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := au.drainConfirm(ctx, attemptID, info.LatestVersion)
			cancel()
			if err != nil && isDrainExpiredErr(err) {
				// The service expired our drain: stop treating ourselves as
				// draining and become routable again via a ready signal. A new
				// attempt may start after the cooldown.
				fmt.Printf("[autoupdate] drain confirm rejected (expired) for %s; abandoning\n", attemptID)
				au.exitDrain(attemptID, "deferred", true)
				return
			}
			if err == nil {
				reachedService = true
			}
			lastConfirm = au.now()
		}

		if !au.sleep(autoUpdateDrainPollInterval) {
			return // updater stopped
		}
	}

	// Fully drained: install. The current version stays usable until the
	// replacement is complete; on failure we keep running the old version.
	fmt.Printf("[autoupdate] Drained; installing %s\n", info.LatestVersion)
	if err := au.apply(path, info); err != nil {
		fmt.Printf("[autoupdate] Install failed, staying on current version: %v\n", err)
		au.exitDrain(attemptID, "deferred", false)
		return
	}
	installed = true // artifact consumed by the self-replace handoff / bundle swap
	// apply() restarts the process; the new version reconciles the drain via
	// resolveInterruptedAttempt → notifyOnlineWithVersion on next boot.
}

// canInstallNow decides whether a drained agent may install:
//   - unregistered or cloud-disconnected: no cloud work to protect → yes.
//   - registered and it reached the service (reported/confirmed draining): the
//     cloud has stopped routing to it → yes.
//   - registered but partitioned (never reached the service): only once it has
//     been refusing work locally for longer than the heartbeat-staleness
//     window, so the service can no longer be routing to it.
func (au *autoUpdater) canInstallNow(startedAt time.Time, registered, reachedService bool) bool {
	if !registered || reachedService {
		return true
	}
	return au.now().Sub(startedAt) >= heartbeatStaleWindow
}

// exitDrain leaves the draining state: reopen admission, clear the crash-
// recovery marker, tell the service (best-effort) and report ready so routing
// resumes. When cooldown is true (deferral / expiry) it also sets the 24h
// post-defer floor so a busy device can't re-drain every check.
func (au *autoUpdater) exitDrain(attemptID, reason string, cooldown bool) {
	reopenAdmission()
	au.tray.draining(false)
	au.clearAttempt()

	if au.cfg.IsRegistered() && !au.cfg.OfflineMode {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = au.drainExit(ctx, attemptID, reason)
		cancel()
	}
	// Report ready so the service restores routing immediately (the agent is
	// present and says so).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	au.reportOnline(ctx)
	cancel()

	if cooldown {
		au.mu.Lock()
		au.deferredUntil = au.now().Add(autoUpdateDeferCooldown)
		au.mu.Unlock()
	}
}

// persistAttempt / clearAttempt maintain the crash-recovery marker.
func (au *autoUpdater) persistAttempt(attemptID, target string) {
	au.cfg.PendingUpdateAttemptID = attemptID
	au.cfg.PendingUpdateVersion = target
	if err := au.cfg.Save(au.savePath); err != nil {
		fmt.Printf("[autoupdate] Could not persist attempt marker: %v\n", err)
	}
}

func (au *autoUpdater) clearAttempt() {
	if au.cfg.PendingUpdateAttemptID == "" && au.cfg.PendingUpdateVersion == "" {
		return
	}
	au.cfg.PendingUpdateAttemptID = ""
	au.cfg.PendingUpdateVersion = ""
	if err := au.cfg.Save(au.savePath); err != nil {
		fmt.Printf("[autoupdate] Could not clear attempt marker: %v\n", err)
	}
}

// retryBackoff returns the backoff before the given (1-based) retry attempt,
// clamped to [autoUpdateRetryMin, autoUpdateRetryMax].
func retryBackoff(attempt int) time.Duration {
	d := autoUpdateRetryMin << (attempt - 1)
	if d < autoUpdateRetryMin {
		d = autoUpdateRetryMin
	}
	if d > autoUpdateRetryMax {
		d = autoUpdateRetryMax
	}
	return d
}

// newAttemptID builds a stable, unique attempt id. Deterministic given the
// injected clock + sequence so tests can assert identity.
func newAttemptID(now time.Time, seq uint64) string {
	return fmt.Sprintf("upd-%d-%d", now.UnixNano(), seq)
}

// isDrainExpiredErr reports whether a drain confirm/enter error signals the
// service has expired this attempt (so the agent must stop draining rather than
// keep retrying).
func isDrainExpiredErr(err error) bool {
	if err == nil {
		return false
	}
	// notifyConnectivity surfaces the HTTP status in its error text; the drain
	// route returns 410 for an expired attempt.
	msg := err.Error()
	return strings.Contains(msg, "410") || strings.Contains(msg, "DRAIN_EXPIRED")
}

// resolveInterruptedAttempt reconciles an attempt marker left by a crash or an
// install-restart, BEFORE the Pub/Sub loop can accept work. Called from
// StartAgent. It never leaves the agent believing it is mid-drain without
// telling the service.
func resolveInterruptedAttempt(cfg *Config) {
	resolveInterruptedAttemptWithPath(cfg, ConfigPath())
}

// resolveInterruptedAttemptWithPath is resolveInterruptedAttempt with an
// explicit save path (for tests).
func resolveInterruptedAttemptWithPath(cfg *Config, savePath string) {
	if cfg == nil || cfg.PendingUpdateAttemptID == "" {
		return
	}
	attemptID := cfg.PendingUpdateAttemptID
	target := cfg.PendingUpdateVersion

	// Clear the marker first so a re-crash cannot loop on it, and make sure we
	// are not draining.
	cfg.PendingUpdateAttemptID = ""
	cfg.PendingUpdateVersion = ""
	if err := cfg.Save(savePath); err != nil {
		fmt.Printf("[autoupdate] Could not clear interrupted attempt marker: %v\n", err)
	}
	reopenAdmission()

	if !cfg.IsRegistered() || cfg.OfflineMode {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if Version == target {
			// The update landed. Report ready on the new version + attempt id so
			// the service reconciles the drain it cleared.
			if err := notifyOnlineWithVersion(ctx, cfg, Version, attemptID); err != nil {
				fmt.Printf("[autoupdate] post-update online report failed: %v\n", err)
			}
		} else {
			// Interrupted mid-drain (crash / failed install): abandon the
			// attempt, tell the service to exit the drain, and become routable.
			_ = notifyDrainExit(ctx, cfg, attemptID, "deferred")
			_ = notifyOnline(ctx, cfg)
		}
	}()
}
