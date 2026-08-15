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

const updateAppliedArg = "--update-applied"

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

// SetUpdateHandoff marks an update whose platform-specific installer already
// relaunched the replacement. onTrayExit uses this marker to skip the ordinary
// offline notification and subprocess teardown without launching another
// update process. macOS uses this after replacing and opening the new bundle.
func SetUpdateHandoff() {
	updateMutex.Lock()
	updatePath = ""
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
	autoUpdateFirstCheckDelay = 5 * time.Minute
	autoUpdateCheckInterval   = 6 * time.Hour
	autoUpdateDrainDeadline   = 7 * 24 * time.Hour
	autoUpdateDeferCooldown   = 24 * time.Hour
	// Renew substantially before the five-minute heartbeat-staleness safety
	// window so polling and RPC latency cannot let a live drain lease expire.
	autoUpdateDrainConfirmEvery = 2 * time.Minute
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
	manualApply    func(info *UpdateInfo) error
	activeWork     func() int
	claimInstall   func() bool
	installable    func() (bool, string)
	registering    func() bool
	cloudConnected func() bool
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

	mu               sync.Mutex
	inProgress       bool
	manualInProgress bool
	autoDone         chan struct{}
	manualTakeover   chan struct{}
	drainAttempt     string
	installing       bool
	applying         bool
	applyDone        chan struct{}
	stopping         bool
	deferredUntil    time.Time // cooldown floor after a defer (24h)

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
		manualApply:    downloadAndApplyUpdate,
		activeWork:     ActiveWork,
		claimInstall:   sealDrainForInstall,
		installable:    installUpdatable,
		registering:    func() bool { return false },
		savePath:       ConfigPath(),
		stopCh:         make(chan struct{}),
		triggerCh:      make(chan struct{}, 1),
	}
	au.cloudConnected = func() bool {
		connected := false
		cfg.WithPersistenceLock(func() {
			connected = cfg.IsRegistered() && !cfg.OfflineMode
		})
		return connected
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
func StartAutoUpdateScheduler(cfg *Config, tray *trayUpdateHandles, registering func() bool) *autoUpdater {
	au := newAutoUpdater(cfg, tray)
	if registering != nil {
		au.registering = registering
	}
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
	au.mu.Lock()
	au.stopping = true
	select {
	case <-au.stopCh:
	default:
		close(au.stopCh)
	}
	au.mu.Unlock()
}

// stopForShutdown prevents an explicit process exit from turning into an
// update restart. It abandons local drain state without reporting ready; the
// ordinary shutdown path reports the captured attempt exit and then offline.
// If replacement already started, it waits and reports whether a successful
// handoff now owns shutdown so the old process can skip that offline path.
func (au *autoUpdater) stopForShutdown() (drainAttemptID string, updateHandoff bool) {
	if au == nil {
		return "", false
	}
	au.stop()

	au.mu.Lock()
	if au.applying {
		// Replacement has already begun and cannot be safely rolled back here.
		// Wait until it either establishes the update handoff or finishes its
		// failure cleanup; ordinary shutdown must not exit between bundle swaps.
		done := au.applyDone
		au.mu.Unlock()
		if done != nil {
			<-done
		}
		path, pending := GetUpdateReady()
		return "", pending && path == ""
	}
	attemptID := au.drainAttempt
	if attemptID != "" {
		au.drainAttempt = ""
		au.installing = false
	}
	au.mu.Unlock()
	if attemptID == "" || !isDraining() || drainingAttempt() != attemptID {
		return "", false
	}

	reopenAdmission()
	if au.tray != nil {
		au.tray.draining(false)
	}
	au.clearAttempt()
	return attemptID, false
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
	if au.inProgress || au.manualInProgress || au.now().Before(au.deferredUntil) {
		au.mu.Unlock()
		return
	}
	au.inProgress = true
	au.autoDone = make(chan struct{})
	au.manualTakeover = make(chan struct{})
	done := au.autoDone
	au.mu.Unlock()

	defer func() {
		au.mu.Lock()
		au.inProgress = false
		au.autoDone = nil
		au.manualTakeover = nil
		au.installing = false
		au.applying = false
		au.drainAttempt = ""
		close(done)
		au.mu.Unlock()
	}()

	au.runAttempt()
}

// runAttempt performs one full attempt: writability probe, bounded
// check+download+verify, then drain+install.
func (au *autoUpdater) runAttempt() {
	if au.takeoverRequested() {
		return
	}
	var reconciliationPending bool
	au.cfg.WithPersistenceLock(func() {
		reconciliationPending = au.cfg.PendingUpdateAttemptID != ""
	})
	if reconciliationPending {
		fmt.Println("[autoupdate] Skipping check while restart reconciliation is pending")
		return
	}
	// macOS check-and-offer fallback builds do not replace their bundle, so the
	// install location is irrelevant: even a copy launched from Downloads or a
	// mounted DMG can still perform the promised metadata check and offer the
	// release for user-initiated installation.
	if !silentUpdateCapable() {
		au.tray.blocked(false, "")
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
	if au.registering() {
		fmt.Println("[autoupdate] Skipping automatic install while device registration is in progress")
		return
	}

	// A read-only / not-yet-relocated install cannot update itself. Say so in
	// the tray and stop — never retry silently in a loop.
	if ok, reason := au.installable(); !ok {
		fmt.Printf("[autoupdate] Install location not updatable: %s\n", reason)
		au.tray.blocked(true, reason)
		return
	}
	au.tray.blocked(false, "")

	info, path := au.checkAndVerifyWithRetry()
	if info == nil || path == "" {
		return // no update, or abandoned after retries
	}
	if au.takeoverRequested() {
		_ = os.Remove(path)
		return
	}
	if !au.cfg.IsAutoUpdate() {
		fmt.Println("[autoupdate] Preference disabled after verification; discarding update")
		_ = os.Remove(path)
		return
	}
	if au.registering() {
		fmt.Println("[autoupdate] Deferring verified update while device registration is in progress")
		_ = os.Remove(path)
		return
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
		if au.takeoverRequested() {
			return nil
		}
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
		if au.takeoverRequested() {
			return nil, ""
		}
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
	var applyDone chan struct{}
	defer func() {
		if !installed && path != "" {
			_ = os.Remove(path)
		}
		if applyDone != nil {
			au.mu.Lock()
			au.applying = false
			if au.applyDone == applyDone {
				close(applyDone)
				au.applyDone = nil
			}
			au.mu.Unlock()
		}
	}()

	// Publish the attempt identity and close admission while manual takeover is
	// excluded by au.mu. A manual click therefore either wins before draining
	// starts, or captures the exact drain it must restore if its install fails.
	au.mu.Lock()
	if au.stopping || channelClosed(au.manualTakeover) {
		au.mu.Unlock()
		return
	}
	au.drainAttempt = attemptID
	if !closeAdmission(attemptID) {
		au.drainAttempt = ""
		au.mu.Unlock()
		fmt.Println("[autoupdate] Deferring verified update because device registration won the admission boundary")
		return
	}
	au.mu.Unlock()
	au.tray.draining(true)
	if err := au.persistAttempt(attemptID, info.LatestVersion); err != nil {
		fmt.Printf("[autoupdate] Could not persist attempt marker; abandoning update: %v\n", err)
		reopenAdmission()
		au.tray.draining(false)
		au.mu.Lock()
		if au.drainAttempt == attemptID {
			au.drainAttempt = ""
		}
		au.mu.Unlock()
		return
	}

	connected := au.cloudConnected()
	reachedService := false
	if connected {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := au.drainEnter(ctx, attemptID, info.LatestVersion); err == nil {
			reachedService = true
		}
		cancel()
	}

	startedAt := au.now()
	deadline := startedAt.Add(autoUpdateDrainDeadline)
	lastConfirm := time.Time{}
	lastEnterAttempt := startedAt
	if reachedService {
		lastConfirm = startedAt
	}

	for {
		// A user-selected Update Now supersedes the automatic attempt. Keep
		// admission closed and leave the attempt marker intact while ownership
		// passes to the manual installer; it will restore routing if it fails.
		if au.takeoverRequested() {
			return
		}
		// Preference turned off before replacement began — cancel cleanly and
		// return the agent to normal admission without terminating work.
		if !au.cfg.IsAutoUpdate() {
			au.exitDrain(attemptID, "preference_off", false)
			return
		}

		liveConnected := au.cloudConnected()
		if liveConnected && !connected {
			// A generic reconnect /online can make an offline-origin attempt
			// routable again. From this point, installation is blocked until the
			// service accepts this attempt's explicit drain (or the user
			// disconnects again), regardless of the original drain age.
			reachedService = false
			lastEnterAttempt = time.Time{}
		}
		connected = liveConnected
		// A connected agent may install only after this attempt's drain has
		// actually been accepted. Retry both a failed initial entry and an entry
		// invalidated by reconnect/renewal failure; local refusal alone does not
		// stop the healthy Pub/Sub heartbeat from keeping cloud routing active.
		if connected && !reachedService &&
			(lastEnterAttempt.IsZero() || au.now().Sub(lastEnterAttempt) >= autoUpdateDrainConfirmEvery) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := au.drainEnter(ctx, attemptID, info.LatestVersion)
			cancel()
			lastEnterAttempt = au.now()
			if err == nil {
				reachedService = true
				lastConfirm = au.now()
				completeCloudReconnectDrain()
			}
		}
		// A suspended process may wake after the service-side drain lease has
		// expired. Renew overdue permission before using it to authorize an
		// install, even when work became idle while the process was asleep.
		if connected && reachedService && au.now().Sub(lastConfirm) >= autoUpdateDrainConfirmEvery {
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
				lastConfirm = au.now()
			} else {
				// Do not extend local installation permission for a renewal the
				// service did not acknowledge. Re-enter idempotently before this
				// connected process may install.
				reachedService = false
				lastEnterAttempt = au.now()
			}
		}

		safeToInstall := au.canInstallNow(startedAt, connected, reachedService)
		if connected && !reachedService {
			safeToInstall = false
		}
		if au.activeWork() == 0 && safeToInstall {
			// Serialize the final idle-drain claim with stopForShutdown. Once an
			// explicit exit sets stopping, this process can no longer launch a
			// replacement even if teardown makes ActiveWork fall to zero.
			au.mu.Lock()
			if !au.stopping && au.claimInstall() {
				au.installing = true
				au.mu.Unlock()
				break
			}
			au.mu.Unlock()
		}

		if au.now().After(deadline) {
			// 7-day window elapsed with work still active: defer, become
			// routable again, and don't retry for 24h so a continuously-busy
			// device can't re-enter a week-long drain every 6 hours.
			au.exitDrain(attemptID, "deferred", true)
			return
		}

		if !au.sleep(autoUpdateDrainPollInterval) {
			return // updater stopped
		}
	}

	// Fully drained: install. The replacement claim above was made under the
	// same mutex used by shutdown and manual takeover, so neither can start a
	// competing transition after the zero-work observation.
	au.mu.Lock()
	if au.stopping || channelClosed(au.manualTakeover) {
		au.mu.Unlock()
		return
	}
	applyDone = make(chan struct{})
	au.applying = true
	au.applyDone = applyDone
	au.mu.Unlock()

	// The current version stays usable until the
	// replacement is complete; on failure we keep running the old version.
	fmt.Printf("[autoupdate] Drained; installing %s\n", info.LatestVersion)
	err := au.apply(path, info)
	au.mu.Lock()
	stopping := au.stopping
	au.mu.Unlock()
	if err != nil {
		fmt.Printf("[autoupdate] Install failed, staying on current version: %v\n", err)
		if stopping {
			reopenAdmission()
			au.tray.draining(false)
			au.clearAttempt()
			return
		}
		au.exitDrain(attemptID, "deferred", false)
		return
	}
	installed = true // artifact consumed by the self-replace handoff / bundle swap
	// apply() restarts the process; the new version reconciles the drain via
	// resolveInterruptedAttempt → notifyOnlineWithVersion on next boot.
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (au *autoUpdater) takeoverRequested() bool {
	au.mu.Lock()
	ch := au.manualTakeover
	au.mu.Unlock()
	return channelClosed(ch)
}

// installManually serializes a user-selected install with the automatic state
// machine. Manual Update Now intentionally wins and still installs without a
// drain, but the automatic goroutine first yields ownership so two downloads
// or replacement handoffs cannot race. If the manual path fails after taking
// over an active drain, routing is restored on the old version.
func (au *autoUpdater) installManually(info *UpdateInfo) error {
	if au == nil {
		return downloadAndApplyUpdate(info)
	}

	au.mu.Lock()
	if au.manualInProgress {
		au.mu.Unlock()
		return fmt.Errorf("an update install is already in progress")
	}
	if au.stopping {
		au.mu.Unlock()
		return fmt.Errorf("the application is shutting down")
	}
	if au.installing {
		au.mu.Unlock()
		return fmt.Errorf("the automatic update is already being installed")
	}
	au.manualInProgress = true
	done := au.autoDone
	takeover := au.manualTakeover
	attemptID := au.drainAttempt
	if takeover != nil && !channelClosed(takeover) {
		close(takeover)
	}
	au.mu.Unlock()

	if done != nil {
		<-done
	}
	defer func() {
		au.mu.Lock()
		au.manualInProgress = false
		au.drainAttempt = ""
		au.mu.Unlock()
	}()

	// A manual check can select a newer release than the one that started the
	// automatic drain. Retarget the durable marker before applying so the
	// replacement's --update-applied signal reconciles against the version that
	// was actually installed, while preserving the automatic attempt identity.
	if attemptID != "" {
		if info == nil || info.LatestVersion == "" {
			if isDraining() && drainingAttempt() == attemptID {
				au.exitDrain(attemptID, "superseded", false)
			}
			return fmt.Errorf("manual update is missing a target version")
		}
		if err := au.persistAttempt(attemptID, info.LatestVersion); err != nil {
			if isDraining() && drainingAttempt() == attemptID {
				au.exitDrain(attemptID, "superseded", false)
			}
			return fmt.Errorf("could not persist manual update target: %w", err)
		}
	}

	var (
		err       error
		applyDone chan struct{}
	)
	if !silentUpdateCapable() {
		// Check-and-offer macOS builds open the release for an ordinary manual
		// install; there is no in-process bundle replacement to coordinate.
		err = au.manualApply(info)
	} else {
		var path string
		path, err = au.downloadVerify(info)
		if err == nil {
			au.mu.Lock()
			if au.stopping {
				au.mu.Unlock()
				_ = os.Remove(path)
				err = fmt.Errorf("the application is shutting down")
			} else {
				applyDone = make(chan struct{})
				au.applying = true
				au.applyDone = applyDone
				au.mu.Unlock()

				err = applyManualVerifiedUpdate(path, info, au.apply)
			}
		}
	}
	if err != nil && attemptID != "" && isDraining() && drainingAttempt() == attemptID {
		au.exitDrain(attemptID, "superseded", false)
	}
	if applyDone != nil {
		// Keep shutdown waiting through failed-takeover routing restoration, not
		// just the platform swap itself, so an online recovery cannot race the
		// ordinary shutdown path's offline notification.
		au.mu.Lock()
		au.applying = false
		if au.applyDone == applyDone {
			close(applyDone)
			au.applyDone = nil
		}
		au.mu.Unlock()
	}
	return err
}

func installUpdateManually(info *UpdateInfo) error {
	if globalAutoUpdater == nil {
		return downloadAndApplyUpdate(info)
	}
	return globalAutoUpdater.installManually(info)
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
func (au *autoUpdater) persistAttempt(attemptID, target string) error {
	var previousAttempt, previousVersion string
	var previousApplied bool
	return au.cfg.MutateAndSaveRollback(au.savePath, func() {
		previousAttempt = au.cfg.PendingUpdateAttemptID
		previousVersion = au.cfg.PendingUpdateVersion
		previousApplied = au.cfg.PendingUpdateApplied
		au.cfg.PendingUpdateAttemptID = attemptID
		au.cfg.PendingUpdateVersion = target
		au.cfg.PendingUpdateApplied = false
	}, func() {
		au.cfg.PendingUpdateAttemptID = previousAttempt
		au.cfg.PendingUpdateVersion = previousVersion
		au.cfg.PendingUpdateApplied = previousApplied
	})
}

func (au *autoUpdater) clearAttempt() {
	if err := au.cfg.MutateAndSave(au.savePath, func() {
		au.cfg.PendingUpdateAttemptID = ""
		au.cfg.PendingUpdateVersion = ""
		au.cfg.PendingUpdateApplied = false
	}); err != nil {
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
func resolveInterruptedAttempt(cfg *Config) bool {
	return resolveInterruptedAttemptWithPath(cfg, ConfigPath())
}

// resolveInterruptedAttemptWithPath is resolveInterruptedAttempt with an
// explicit save path (for tests).
// The return value reports that service reconciliation is still pending. The
// caller must then suppress the generic boot-time /online call, which lacks the
// attempt identity and could make a later targeted reconciliation a no-op.
func resolveInterruptedAttemptWithPath(cfg *Config, savePath string) bool {
	if cfg == nil {
		return false
	}
	var attemptID, target string
	var applied, registered, offline bool
	cfg.WithPersistenceLock(func() {
		attemptID = cfg.PendingUpdateAttemptID
		target = cfg.PendingUpdateVersion
		applied = cfg.PendingUpdateApplied
		registered = cfg.IsRegistered()
		offline = cfg.OfflineMode
	})
	if attemptID == "" {
		return false
	}

	reopenAdmission()

	if !registered || offline {
		clearInterruptedAttempt(cfg, savePath, attemptID)
		return false
	}

	// Reconcile synchronously. StartAgent calls this before it starts Pub/Sub
	// and before it schedules the ordinary boot-time /online signal, so the
	// version-aware request must reach the service first; otherwise a generic
	// already-online response can discard the attempt/version outcome.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if applied && Version == target {
		// The update landed. Report ready on the new version + attempt id so
		// the service reconciles the drain it cleared.
		if err := notifyOnlineWithVersion(ctx, cfg, Version, attemptID); err != nil {
			fmt.Printf("[autoupdate] post-update online report failed: %v\n", err)
			return true
		}
	} else {
		// Interrupted mid-drain (crash / failed install): abandon the attempt,
		// tell the service to exit the drain, and become routable.
		if err := notifyDrainExit(ctx, cfg, attemptID, "deferred"); err != nil {
			fmt.Printf("[autoupdate] interrupted-attempt drain exit failed: %v\n", err)
			return true
		}
		if err := notifyOnline(ctx, cfg); err != nil {
			fmt.Printf("[autoupdate] interrupted-attempt online report failed: %v\n", err)
			return true
		}
	}

	clearInterruptedAttempt(cfg, savePath, attemptID)
	return false
}

func clearInterruptedAttempt(cfg *Config, savePath, attemptID string) {
	if err := cfg.MutateAndSave(savePath, func() {
		if cfg.PendingUpdateAttemptID != attemptID {
			return
		}
		cfg.PendingUpdateAttemptID = ""
		cfg.PendingUpdateVersion = ""
		cfg.PendingUpdateApplied = false
	}); err != nil {
		fmt.Printf("[autoupdate] Could not clear interrupted attempt marker: %v\n", err)
	}
}

// retryInterruptedAttemptReconciliation keeps a successfully installed agent
// from remaining unroutable until another process restart when the service was
// temporarily unavailable during its first version-aware online report.
func retryInterruptedAttemptReconciliation(cfg *Config) {
	if cfg == nil {
		return
	}
	for attempt := 1; ; attempt++ {
		timer := time.NewTimer(retryBackoff(attempt))
		select {
		case <-shutdownChan:
			timer.Stop()
			return
		case <-timer.C:
		}
		if !resolveInterruptedAttempt(cfg) {
			return
		}
	}
}
