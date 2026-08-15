// File: autoupdate_test.go
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
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

	mu                  sync.Mutex
	clock               time.Time
	activeWork          int
	checkErr            error
	verifyErr           error
	applyErr            error
	installableOK       bool
	installableMsg      string
	registrationPending bool
	info                *UpdateInfo

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
		activeWork:   func() int { r.mu.Lock(); defer r.mu.Unlock(); return r.activeWork },
		claimInstall: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.activeWork == 0 },
		installable:  func() (bool, string) { r.mu.Lock(); defer r.mu.Unlock(); return r.installableOK, r.installableMsg },
		registering:  func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.registrationPending },
		cloudConnected: func() bool {
			connected := false
			cfg.WithPersistenceLock(func() {
				connected = cfg.IsRegistered() && !cfg.OfflineMode
			})
			return connected
		},
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

func TestRunAttempt_RegistrationInProgressDefersAutomaticInstall(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.registrationPending = true

	r.au.runAttempt()

	if r.checkCalls != 0 || r.verifyCalls != 0 || r.applyCalls != 0 {
		t.Fatalf("registration must defer check/download/install, got check=%d verify=%d apply=%d",
			r.checkCalls, r.verifyCalls, r.applyCalls)
	}
	if isDraining() {
		t.Fatal("registration deferral must not enter draining")
	}
}

func TestRunAttempt_RegistrationStartingDuringDownloadDefersBeforeDrain(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.au.downloadVerify = func(_ *UpdateInfo) (string, error) {
		r.mu.Lock()
		r.verifyCalls++
		r.registrationPending = true
		r.mu.Unlock()
		return artifact, nil
	}

	r.au.runAttempt()

	if r.applyCalls != 0 || r.drainEnter != 0 || isDraining() {
		t.Fatalf("registration beginning during download must defer before drain (apply=%d enter=%d draining=%v)",
			r.applyCalls, r.drainEnter, isDraining())
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("deferred verified artifact was not removed: %v", err)
	}
}

func TestRunAttempt_PreferenceOffDuringDownloadDefersBeforeDrain(t *testing.T) {
	cfg := DefaultConfig()
	r := newAutoTestRig(t, cfg)
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.au.downloadVerify = func(_ *UpdateInfo) (string, error) {
		r.mu.Lock()
		r.verifyCalls++
		r.mu.Unlock()
		cfg.SetAutoUpdate(false)
		return artifact, nil
	}

	r.au.runAttempt()

	if r.applyCalls != 0 || r.drainEnter != 0 || isDraining() {
		t.Fatalf("preference-off during download must defer before drain (apply=%d enter=%d draining=%v)",
			r.applyCalls, r.drainEnter, isDraining())
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("discarded verified artifact was not removed: %v", err)
	}
}

func TestStopForShutdownPreventsDrainFromInstalling(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.activeWork = 1
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Keep the drain loop parked until stopForShutdown closes stopCh. This
	// models an explicit quit while work is active without waiting for the
	// production poll interval.
	r.au.sleep = func(time.Duration) bool {
		<-r.au.stopCh
		return false
	}
	done := make(chan struct{})
	go func() {
		r.au.drainAndInstall("shutdown-attempt", r.info, artifact)
		close(done)
	}()

	deadline := time.After(time.Second)
	for !isDraining() {
		select {
		case <-deadline:
			t.Fatal("updater did not enter draining")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	if got, handoff := r.au.stopForShutdown(); got != "shutdown-attempt" || handoff {
		t.Fatalf("stopForShutdown = (%q, %v), want (shutdown-attempt, false)", got, handoff)
	}
	<-done
	if r.applyCalls != 0 {
		t.Fatalf("explicit shutdown must not apply an update, got %d calls", r.applyCalls)
	}
	if isDraining() {
		t.Fatal("explicit shutdown must abandon local drain state")
	}
}

func TestStopForShutdownWaitsForActiveApplyHandoff(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.activeWork = 0
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}

	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	r.au.apply = func(_ string, _ *UpdateInfo) error {
		close(applyStarted)
		<-releaseApply
		SetUpdateHandoff()
		return nil
	}

	installDone := make(chan struct{})
	go func() {
		r.au.drainAndInstall("applying-attempt", r.info, artifact)
		close(installDone)
	}()
	<-applyStarted

	type shutdownResult struct {
		attempt string
		handoff bool
	}
	shutdownDone := make(chan shutdownResult, 1)
	go func() {
		attempt, handoff := r.au.stopForShutdown()
		shutdownDone <- shutdownResult{attempt: attempt, handoff: handoff}
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while replacement was still active")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseApply)
	<-installDone
	if got := <-shutdownDone; got.attempt != "" || !got.handoff {
		t.Fatalf("stopForShutdown = (%q, %v), want (empty, true)", got.attempt, got.handoff)
	}
	if path, pending := GetUpdateReady(); !pending || path != "" {
		t.Fatalf("GetUpdateReady = (%q, %v), want completed handoff", path, pending)
	}
}

func TestStopForShutdownWaitsForActiveApplyFailureCleanup(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	r.activeWork = 0
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}

	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	r.au.apply = func(_ string, _ *UpdateInfo) error {
		close(applyStarted)
		<-releaseApply
		return errors.New("replacement failed")
	}

	installDone := make(chan struct{})
	go func() {
		r.au.drainAndInstall("failing-attempt", r.info, artifact)
		close(installDone)
	}()
	<-applyStarted

	type shutdownResult struct {
		attempt string
		handoff bool
	}
	shutdownDone := make(chan shutdownResult, 1)
	go func() {
		attempt, handoff := r.au.stopForShutdown()
		shutdownDone <- shutdownResult{attempt: attempt, handoff: handoff}
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while replacement was still active")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseApply)
	if got := <-shutdownDone; got.attempt != "" || got.handoff {
		t.Fatalf("stopForShutdown = (%q, %v), want (empty, false)", got.attempt, got.handoff)
	}
	if isDraining() {
		t.Fatal("failed replacement must reopen admission before shutdown continues")
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("shutdown continued before failed artifact cleanup: %v", err)
	}
	<-installDone
}

func TestStopForShutdownWaitsForManualApplyHandoff(t *testing.T) {
	r := newAutoTestRig(t, DefaultConfig())
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.au.downloadVerify = func(*UpdateInfo) (string, error) { return artifact, nil }

	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	r.au.apply = func(_ string, _ *UpdateInfo) error {
		close(applyStarted)
		<-releaseApply
		SetUpdateHandoff()
		return nil
	}
	installDone := make(chan error, 1)
	go func() { installDone <- r.au.installManually(r.info) }()
	<-applyStarted

	type shutdownResult struct {
		attempt string
		handoff bool
	}
	shutdownDone := make(chan shutdownResult, 1)
	go func() {
		attempt, handoff := r.au.stopForShutdown()
		shutdownDone <- shutdownResult{attempt: attempt, handoff: handoff}
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while manual replacement was still active")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseApply)
	if err := <-installDone; err != nil {
		t.Fatalf("manual install failed: %v", err)
	}
	if got := <-shutdownDone; got.attempt != "" || !got.handoff {
		t.Fatalf("stopForShutdown = (%q, %v), want (empty, true)", got.attempt, got.handoff)
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

func TestDrainAndInstall_OfflineReconnectReportsDrainBeforeInstall(t *testing.T) {
	cfg := registeredCfg()
	cfg.OfflineMode = true
	r := newAutoTestRig(t, cfg)
	r.activeWork = 1
	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}

	enterStarted := make(chan struct{})
	allowEnter := make(chan struct{})
	r.au.drainEnter = func(_ context.Context, _, _ string) error {
		r.mu.Lock()
		r.drainEnter++
		r.mu.Unlock()
		close(enterStarted)
		<-allowEnter
		return nil
	}
	firstSleep := true
	r.au.sleep = func(d time.Duration) bool {
		r.mu.Lock()
		r.clock = r.clock.Add(d)
		if firstSleep {
			firstSleep = false
			r.activeWork = 0
		}
		r.mu.Unlock()
		cfg.WithPersistenceLock(func() { cfg.OfflineMode = false })
		return true
	}

	done := make(chan struct{})
	go func() {
		r.au.drainAndInstall("offline-reconnect", r.info, artifact)
		close(done)
	}()
	<-enterStarted
	r.mu.Lock()
	applyCalls := r.applyCalls
	r.mu.Unlock()
	if applyCalls != 0 {
		t.Fatalf("reconnected drain installed before service drain entry, apply=%d", applyCalls)
	}
	close(allowEnter)
	<-done
	if r.applyCalls != 1 {
		t.Fatalf("service-drained reconnect should install once, apply=%d", r.applyCalls)
	}
}

func TestDrainAndInstall_AbortsWhenAttemptMarkerCannotBePersisted(t *testing.T) {
	cfg := registeredCfg()
	r := newAutoTestRig(t, cfg)
	r.activeWork = 0
	// Replacing a directory with a config file fails on every supported OS.
	r.au.savePath = t.TempDir()

	r.au.runAttempt()

	if r.drainEnter != 0 || r.applyCalls != 0 {
		t.Fatalf("persistence failure must abort before service drain or apply (enter=%d apply=%d)", r.drainEnter, r.applyCalls)
	}
	if isDraining() {
		t.Fatal("persistence failure must reopen admission")
	}
	if cfg.PendingUpdateAttemptID != "" || cfg.PendingUpdateVersion != "" {
		t.Fatal("failed persistence must restore the previous in-memory marker")
	}
	if r.hideDraining == 0 {
		t.Fatal("persistence failure must clear the draining tray state")
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

func TestDrainAndInstall_RetriesFailedInitialDrainBeforeInstall(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 0
	r.au.drainEnter = func(_ context.Context, _, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.drainEnter++
		if r.drainEnter == 1 {
			return errors.New("temporary drain failure")
		}
		if r.applyCalls != 0 {
			t.Fatal("update installed before service accepted the drain")
		}
		return nil
	}

	r.au.runAttempt()

	if r.drainEnter != 2 {
		t.Fatalf("drain enters = %d, want initial attempt plus retry", r.drainEnter)
	}
	if r.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1 after accepted retry", r.applyCalls)
	}
}

func TestDrainAndInstall_RenewalFailureRequiresReentryBeforeInstall(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 1
	r.au.drainConfirm = func(_ context.Context, _, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.drainConfirm++
		r.activeWork = 0
		return errors.New("temporary confirmation failure")
	}
	r.au.drainEnter = func(_ context.Context, _, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.drainEnter++
		if r.drainEnter > 1 && r.applyCalls != 0 {
			t.Fatal("update installed before drain permission was reacquired")
		}
		return nil
	}

	r.au.runAttempt()

	if r.drainConfirm != 1 {
		t.Fatalf("drain confirms = %d, want 1 failed renewal", r.drainConfirm)
	}
	if r.drainEnter != 2 {
		t.Fatalf("drain enters = %d, want reentry after failed renewal", r.drainEnter)
	}
	if r.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1 after reentry", r.applyCalls)
	}
}

func TestDrainAndInstall_RenewsOverdueDrainBeforeInstall(t *testing.T) {
	r := newAutoTestRig(t, registeredCfg())
	r.activeWork = 1
	woke := false
	r.au.sleep = func(time.Duration) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !woke {
			woke = true
			r.clock = r.clock.Add(autoUpdateDrainConfirmEvery + time.Second)
			r.activeWork = 0
		}
		return true
	}

	r.au.runAttempt()

	if r.drainConfirm != 1 {
		t.Fatalf("drain confirms = %d, want overdue lease renewed before install", r.drainConfirm)
	}
	if r.applyCalls != 1 {
		t.Fatalf("apply calls = %d, want 1 after successful renewal", r.applyCalls)
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

func TestResolveInterruptedAttempt_ReportsVersionBeforeReturning(t *testing.T) {
	resetDrainState(t)
	resetConnectivityState(t)
	t.Cleanup(func() { resetDrainState(t) })

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/device/agent-reconcile/online" {
			t.Errorf("request path = %q, want version-aware online path", r.URL.Path)
		}
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := DefaultConfig()
	cfg.AgentID = "agent-reconcile"
	cfg.CommandSecret = "secret"
	cfg.PendingUpdateAttemptID = "att-reconcile"
	cfg.PendingUpdateVersion = Version
	cfg.PendingUpdateApplied = true

	if pending := resolveInterruptedAttemptWithPath(cfg, filepath.Join(t.TempDir(), "c.json")); pending {
		t.Fatal("successful version-aware reconciliation must not remain pending")
	}

	if got := requests.Load(); got != 1 {
		t.Fatalf("version-aware online requests before return = %d, want 1", got)
	}
}

func TestResolveInterruptedAttempt_RetainsMarkerWhenVersionReportFails(t *testing.T) {
	resetDrainState(t)
	resetConnectivityState(t)
	t.Cleanup(func() { resetDrainState(t) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := DefaultConfig()
	cfg.AgentID = "agent-reconcile-fail"
	cfg.CommandSecret = "secret"
	cfg.PendingUpdateAttemptID = "att-reconcile-fail"
	cfg.PendingUpdateVersion = Version
	cfg.PendingUpdateApplied = true

	if pending := resolveInterruptedAttemptWithPath(cfg, filepath.Join(t.TempDir(), "c.json")); !pending {
		t.Fatal("failed version-aware reconciliation must remain pending")
	}
	if cfg.PendingUpdateAttemptID == "" || !cfg.PendingUpdateApplied {
		t.Fatal("failed reconciliation must retain the durable attempt marker")
	}
}

func TestResolveInterruptedAttempt_TemporaryFallbackIsNotSuccessfulInstall(t *testing.T) {
	resetDrainState(t)
	resetConnectivityState(t)
	t.Cleanup(func() { resetDrainState(t) })

	var pathsMu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TERMINAL_SERVICE_URL", srv.URL)

	cfg := DefaultConfig()
	cfg.AgentID = "agent-temp-fallback"
	cfg.CommandSecret = "secret"
	cfg.PendingUpdateAttemptID = "att-temp-fallback"
	cfg.PendingUpdateVersion = Version // same binary version, but no applied bit

	if pending := resolveInterruptedAttemptWithPath(cfg, filepath.Join(t.TempDir(), "c.json")); pending {
		t.Fatal("successful fallback reconciliation must not remain pending")
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	want := []string{
		"/device/agent-temp-fallback/drain/exit",
		"/device/agent-temp-fallback/online",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("reconciliation paths = %v, want %v", paths, want)
	}
	if cfg.PendingUpdateAttemptID != "" || cfg.PendingUpdateApplied {
		t.Fatal("successfully abandoned fallback attempt must be cleared")
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

func TestManualInstallTakesOverAutomaticDrainAndRestoresOnFailure(t *testing.T) {
	resetDrainState(t)
	t.Cleanup(func() { resetDrainState(t) })
	closeAdmission("auto-attempt")

	cfg := DefaultConfig()
	cfg.PendingUpdateAttemptID = "auto-attempt"
	cfg.PendingUpdateVersion = "v1.0.0"
	au := newAutoUpdater(cfg, nil)
	au.savePath = filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(au.savePath); err != nil {
		t.Fatal(err)
	}
	autoDone := make(chan struct{})
	takeover := make(chan struct{})
	au.mu.Lock()
	au.inProgress = true
	au.autoDone = autoDone
	au.manualTakeover = takeover
	au.drainAttempt = "auto-attempt"
	au.mu.Unlock()

	artifact := filepath.Join(t.TempDir(), "verified-update")
	if err := os.WriteFile(artifact, []byte("update"), 0o600); err != nil {
		t.Fatal(err)
	}
	au.downloadVerify = func(*UpdateInfo) (string, error) { return artifact, nil }
	manualCalled := make(chan string, 1)
	au.apply = func(_ string, _ *UpdateInfo) error {
		cfg.WithPersistenceLock(func() {
			manualCalled <- cfg.PendingUpdateVersion
		})
		return errors.New("manual apply failed")
	}
	result := make(chan error, 1)
	go func() {
		result <- au.installManually(&UpdateInfo{Available: true, LatestVersion: "v9.9.9"})
	}()

	select {
	case <-takeover:
	case <-time.After(time.Second):
		t.Fatal("manual install did not request automatic takeover")
	}
	select {
	case <-manualCalled:
		t.Fatal("manual apply raced the automatic attempt")
	default:
	}

	close(autoDone)
	if err := <-result; err == nil {
		t.Fatal("manual apply failure was not returned")
	}
	if got := <-manualCalled; got != "v9.9.9" {
		t.Fatalf("manual apply observed persisted target %q, want v9.9.9", got)
	}
	if isDraining() {
		t.Fatal("failed manual takeover must restore routability")
	}
}
