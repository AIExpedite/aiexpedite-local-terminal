// File: autoupdate_test.go
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSetUpdateHandoffSuppressesArtifactLaunch(t *testing.T) {
	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updateMutex.Unlock()
	t.Cleanup(func() {
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	SetUpdateReady("downloaded-update")
	SetUpdateHandoff()
	path, pending := GetUpdateReady()
	if !pending || path != "" {
		t.Fatalf("GetUpdateReady = (%q, %v), want empty-path pending handoff", path, pending)
	}
}

// autoTestRig bundles a stubbed autoUpdater and the observations tests assert on.
type autoTestRig struct {
	au *autoUpdater

	mu             sync.Mutex
	clock          time.Time
	activeWork     int
	checkErr       error
	verifyErr      error
	applyErr       error
	installableOK  bool
	installableMsg string
	info           *UpdateInfo

	checkCalls   int
	verifyCalls  int
	applyCalls   int
	drainEnter   int
	drainConfirm int
	drainExitR   []string
	onlineCalls  int
	showBlocked  int
	showDraining int
	hideDraining int
	pendingShown []string
}

func newAutoTestRig(t *testing.T, cfg *Config) *autoTestRig {
	t.Helper()
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })

	r := &autoTestRig{
		clock:         time.Unix(1_700_000_000, 0),
		installableOK: true,
		info:          &UpdateInfo{Available: true, LatestVersion: "v9.9.9", CurrentVersion: Version},
	}
	au := &autoUpdater{
		cfg:      cfg,
		savePath: filepath.Join(t.TempDir(), "config.json"),
		now:      func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock },
		checkForUpdate: func() (*UpdateInfo, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.checkCalls++
			if r.checkErr != nil {
				return nil, r.checkErr
			}
			return r.info, nil
		},
		downloadVerify: func(_ *UpdateInfo) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.verifyCalls++
			if r.verifyErr != nil {
				return "", r.verifyErr
			}
			return "/tmp/verified.bin", nil
		},
		apply: func(_ string, _ *UpdateInfo) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.applyCalls++
			return r.applyErr
		},
		activeWork:  func() int { r.mu.Lock(); defer r.mu.Unlock(); return r.activeWork },
		installable: func() (bool, string) { r.mu.Lock(); defer r.mu.Unlock(); return r.installableOK, r.installableMsg },
		drainEnter: func(_ context.Context, _, _ string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.drainEnter++
			return nil
		},
		drainConfirm: func(_ context.Context, _, _ string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.drainConfirm++
			return nil
		},
		drainExit: func(_ context.Context, _, reason string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.drainExitR = append(r.drainExitR, reason)
			return nil
		},
		reportOnline: func(_ context.Context) { r.mu.Lock(); defer r.mu.Unlock(); r.onlineCalls++ },
		stopCh:       make(chan struct{}),
		triggerCh:    make(chan struct{}, 1),
	}
	// sleep advances the simulated clock and never blocks.
	au.sleep = func(d time.Duration) bool {
		r.mu.Lock()
		r.clock = r.clock.Add(d)
		r.mu.Unlock()
		return true
	}
	au.tray = &trayUpdateHandles{
		showDraining: func() { r.mu.Lock(); r.showDraining++; r.mu.Unlock() },
		hideDraining: func() { r.mu.Lock(); r.hideDraining++; r.mu.Unlock() },
		showBlocked:  func(_ string) { r.mu.Lock(); r.showBlocked++; r.mu.Unlock() },
		hideBlocked:  func() {},
		showPendingInstall: func(v string) {
			r.mu.Lock()
			r.pendingShown = append(r.pendingShown, v)
			r.mu.Unlock()
		},
	}
	r.au = au
	return r
}

func registeredCfg() *Config {
	c := DefaultConfig()
	c.AgentID = "agent-1"
	c.CommandSecret = "secret"
	return c
}

func TestRunAttempt_NoUpdateDoesNothing(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.info = &UpdateInfo{Available: false}
	r.au.runAttempt()
	if r.applyCalls != 0 || isDraining() {
		t.Fatalf("no-update path must not install or drain (apply=%d draining=%v)", r.applyCalls, isDraining())
	}
}

func TestRunAttempt_BlockedShowsTrayAndSkips(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.installableOK = false
	r.installableMsg = "blocked"
	r.au.runAttempt()
	if r.checkCalls != 0 {
		t.Fatalf("blocked install must not download (checkCalls=%d)", r.checkCalls)
	}
	if r.showBlocked == 0 {
		t.Fatal("blocked install must surface the tray status item")
	}
}

func TestRunAttempt_UnregisteredInstallsWithoutDraining(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig()) // unregistered
	r.activeWork = 0
	r.au.runAttempt()
	if r.applyCalls != 1 {
		t.Fatalf("apply should be called once, got %d", r.applyCalls)
	}
	if r.drainEnter != 0 {
		t.Fatalf("unregistered agent must not report draining, got %d enters", r.drainEnter)
	}
}

func TestRunAttempt_RegisteredReportsDrainThenInstalls(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 0
	r.au.runAttempt()
	if r.drainEnter != 1 {
		t.Fatalf("registered agent should report draining once, got %d", r.drainEnter)
	}
	if r.applyCalls != 1 {
		t.Fatalf("apply should be called once, got %d", r.applyCalls)
	}
}

func TestDrainAndInstall_ConfirmsBeforeHeartbeatStalenessWindow(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 1

	r.mu.Lock()
	startedAt := r.clock
	r.mu.Unlock()
	var confirmedAt time.Time
	r.au.drainConfirm = func(_ context.Context, _, _ string) error {
		r.mu.Lock()
		confirmedAt = r.clock
		r.drainConfirm++
		r.activeWork = 0
		r.mu.Unlock()
		return nil
	}

	r.au.runAttempt()

	if confirmedAt.IsZero() {
		t.Fatal("long-running drain was never confirmed")
	}
	if elapsed := confirmedAt.Sub(startedAt); elapsed >= heartbeatStaleWindow {
		t.Fatalf("first drain confirmation arrived after %v, want before %v", elapsed, heartbeatStaleWindow)
	}
	if r.applyCalls != 1 {
		t.Fatalf("confirmed drain should install after work completes, apply=%d", r.applyCalls)
	}
}

func TestRunAttempt_SkippedVersionIgnoredOnAutomaticPath(t *testing.T) {
	cfg := registeredCfg()
	cfg.SkippedVersion = "v9.9.9" // same as the available update
	r := newAutoTestRig(t, cfg)
	r.activeWork = 0
	r.au.runAttempt()
	if r.applyCalls != 1 {
		t.Fatalf("automatic path must ignore SkippedVersion and install, apply=%d", r.applyCalls)
	}
}

func TestRunAttempt_SevenDayDeadlineDefersWithCooldown(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 1 // never drains

	// Make each sleep jump well past the deadline so the loop defers promptly.
	r.au.sleep = func(_ time.Duration) bool {
		r.mu.Lock()
		r.clock = r.clock.Add(8 * 24 * time.Hour)
		r.mu.Unlock()
		return true
	}

	r.au.runAttempt()

	if r.applyCalls != 0 {
		t.Fatalf("deadline must defer, not install (apply=%d)", r.applyCalls)
	}
	if len(r.drainExitR) == 0 || r.drainExitR[len(r.drainExitR)-1] != "deferred" {
		t.Fatalf("expected a deferred drain exit, got %v", r.drainExitR)
	}
	if isDraining() {
		t.Fatal("admission must be reopened after a defer")
	}
	// 24h cooldown recorded.
	r.au.mu.Lock()
	cooldown := r.au.deferredUntil
	r.au.mu.Unlock()
	if cooldown.IsZero() {
		t.Fatal("defer must set a cooldown floor")
	}
}

func TestRunAttempt_PreferenceOffDuringDrainCancels(t *testing.T) {
	cfg := registeredCfg()
	r := newAutoTestRig(t, cfg)
	r.activeWork = 1 // won't drain on its own

	// On the first sleep, turn the preference off; the loop should cancel.
	first := true
	r.au.sleep = func(d time.Duration) bool {
		r.mu.Lock()
		r.clock = r.clock.Add(d)
		r.mu.Unlock()
		if first {
			first = false
			cfg.SetAutoUpdate(false)
		}
		return true
	}

	r.au.runAttempt()

	if r.applyCalls != 0 {
		t.Fatalf("preference-off must not install (apply=%d)", r.applyCalls)
	}
	if len(r.drainExitR) == 0 || r.drainExitR[len(r.drainExitR)-1] != "preference_off" {
		t.Fatalf("expected preference_off drain exit, got %v", r.drainExitR)
	}
	if isDraining() {
		t.Fatal("admission must be reopened after preference-off cancel")
	}
}

func TestDrainAndInstall_CleansUpArtifactOnDefer(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 1 // never drains → defers at the deadline

	// Point downloadVerify at a REAL temp file so we can assert it is deleted
	// when the attempt defers instead of installing.
	artifact := filepath.Join(t.TempDir(), "agent_update_x.bin")
	if err := os.WriteFile(artifact, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.au.downloadVerify = func(_ *UpdateInfo) (string, error) { return artifact, nil }
	r.au.sleep = func(_ time.Duration) bool {
		r.mu.Lock()
		r.clock = r.clock.Add(8 * 24 * time.Hour)
		r.mu.Unlock()
		return true
	}

	r.au.runAttempt()

	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("verified artifact should be removed on defer, stat err = %v", err)
	}
	if r.applyCalls != 0 {
		t.Fatal("deferred attempt must not install")
	}
}

func TestRunAttempt_MacFallbackChecksOnlyNoDownload(t *testing.T) {
	prev := silentUpdateCapableFlag
	silentUpdateCapableFlag = "false"
	t.Cleanup(func() { silentUpdateCapableFlag = prev })
	if silentUpdateCapable() {
		t.Skip("fallback only applies to non-capable macOS builds")
	}

	t.Cleanup(ClearPendingUpdate)
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 0
	r.au.runAttempt()

	if r.verifyCalls != 0 {
		t.Fatalf("fallback must NOT download/verify, got %d verify calls", r.verifyCalls)
	}
	if r.applyCalls != 0 || isDraining() {
		t.Fatal("fallback must not drain or install")
	}
	if len(r.pendingShown) == 0 {
		t.Fatal("fallback must offer the update via the pending-install tray item")
	}
	if GetPendingUpdate() == nil {
		t.Fatal("fallback must record the pending update (with its AssetURL) for a manual click")
	}
}

func TestRunAttempt_MacFallbackChecksFromUnsupportedLocation(t *testing.T) {
	prev := silentUpdateCapableFlag
	silentUpdateCapableFlag = "false"
	t.Cleanup(func() { silentUpdateCapableFlag = prev })
	if silentUpdateCapable() {
		t.Skip("fallback only applies to non-capable macOS builds")
	}

	t.Cleanup(ClearPendingUpdate)
	r := newAutoTestRig(t, registeredCfg())
	r.installableOK = false
	r.installableMsg = "unsupported location"
	r.au.runAttempt()

	if r.checkCalls != 1 {
		t.Fatalf("fallback checkCalls = %d, want 1 despite unsupported install location", r.checkCalls)
	}
	if r.verifyCalls != 0 || r.applyCalls != 0 || isDraining() {
		t.Fatal("fallback must check and offer without downloading, draining, or installing")
	}
	if GetPendingUpdate() == nil {
		t.Fatal("fallback must offer the update from an unsupported install location")
	}
}

func TestCheckAndVerifyWithRetry_ExhaustsAndAbandons(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.checkErr = errors.New("network down")
	r.au.runAttempt()
	if r.checkCalls != autoUpdateMaxRetries {
		t.Fatalf("expected %d check attempts, got %d", autoUpdateMaxRetries, r.checkCalls)
	}
	if r.applyCalls != 0 || isDraining() {
		t.Fatal("retry exhaustion must not drain or install")
	}
}

func TestRetryBackoff_Clamped(t *testing.T) {
	if got := retryBackoff(1); got != autoUpdateRetryMin {
		t.Fatalf("retryBackoff(1) = %v, want %v", got, autoUpdateRetryMin)
	}
	if got := retryBackoff(20); got != autoUpdateRetryMax {
		t.Fatalf("retryBackoff(20) = %v, want %v (clamped)", got, autoUpdateRetryMax)
	}
}

func TestResolveInterruptedAttempt_SuccessReportsVersion(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })
	closeAdmission("stale") // simulate coming back still marked draining

	cfg := DefaultConfig() // unregistered → no network side effects
	cfg.PendingUpdateAttemptID = "att-1"
	cfg.PendingUpdateVersion = Version // "landed": running version == target

	resolveInterruptedAttemptWithPath(cfg, filepath.Join(t.TempDir(), "c.json"))

	if isDraining() {
		t.Fatal("interrupted-attempt resolution must reopen admission")
	}
	if cfg.PendingUpdateAttemptID != "" || cfg.PendingUpdateVersion != "" {
		t.Fatal("attempt marker must be cleared")
	}
}

func TestResolveInterruptedAttempt_NoMarkerNoOp(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })
	cfg := DefaultConfig()
	// No marker set — must be a no-op that does not touch admission.
	resolveInterruptedAttemptWithPath(cfg, filepath.Join(t.TempDir(), "c.json"))
	if isDraining() {
		t.Fatal("no-op resolution should not change admission state")
	}
}

func TestIsDrainExpiredErr(t *testing.T) {
	if !isDrainExpiredErr(errors.New("server returned status 410")) {
		t.Fatal("410 should be treated as expired")
	}
	if !isDrainExpiredErr(errors.New("DRAIN_EXPIRED")) {
		t.Fatal("DRAIN_EXPIRED should be treated as expired")
	}
	if isDrainExpiredErr(errors.New("server returned status 503")) {
		t.Fatal("503 should NOT be treated as expired")
	}
	if isDrainExpiredErr(nil) {
		t.Fatal("nil should not be expired")
	}
}
