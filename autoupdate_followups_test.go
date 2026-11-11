// File: autoupdate_followups_test.go
// Tests for the fixes that followed the review of Cancel Update, Install now
// and Update Now from Downloads (PR #281): the cancel's cooldown, a "No" given
// after a cancel, the release a cancel offers, markers left while
// disconnected, the tray during a failed install at shutdown, the status an
// immediate install shows, and macOS copies that were moved or sit in
// ~/Downloads.
package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// waitForUpdater polls cond until it holds, failing the test after five seconds.
func waitForUpdater(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func attemptMarker(r *autoTestRig) string {
	var marker string
	r.au.cfg.WithPersistenceLock(func() { marker = r.au.cfg.PendingUpdateAttemptID })
	return marker
}

// macOSTray makes every tray call behave as it does on macOS while the app
// quits: once shutdown is running on the main thread, a tray call waits for
// that thread, so it returns only after shutdown does. The returned function
// runs stopForShutdown as that main thread, and its channel closes when
// shutdown has returned.
func macOSTray(r *autoTestRig) (quit func() <-chan struct{}) {
	var quitting atomic.Bool
	shutdownDone := make(chan struct{})
	trayCall := func() {
		if quitting.Load() {
			<-shutdownDone
		}
	}
	r.au.tray = &trayUpdateHandles{
		showDraining: func() { trayCall(); r.mu.Lock(); r.showDraining++; r.mu.Unlock() },
		hideDraining: func() { trayCall(); r.mu.Lock(); r.hideDraining++; r.mu.Unlock() },
		showBlocked:  func(string) { trayCall() },
		hideBlocked:  func() { trayCall() },
		showPendingInstall: func(v string) {
			trayCall()
			r.mu.Lock()
			r.pendingShown = append(r.pendingShown, v)
			r.mu.Unlock()
		},
		showCancel: func() { trayCall(); r.mu.Lock(); r.showCancel++; r.mu.Unlock() },
		hideCancel: func() { trayCall(); r.mu.Lock(); r.hideCancel++; r.mu.Unlock() },
	}
	return func() <-chan struct{} {
		quitting.Store(true)
		go func() {
			r.au.stopForShutdown()
			close(shutdownDone)
		}()
		return shutdownDone
	}
}

// failingApplyOnSignal makes the install block until fail is closed, then
// fail. applying closes when the install has started.
func failingApplyOnSignal(r *autoTestRig) (applying, fail chan struct{}) {
	applying = make(chan struct{})
	fail = make(chan struct{})
	r.au.apply = func(_ string, _ *UpdateInfo) error {
		close(applying)
		<-fail
		return errors.New("the new version failed its signature check")
	}
	return applying, fail
}

func (r *autoTestRig) stopping() bool {
	r.au.mu.Lock()
	defer r.au.mu.Unlock()
	return r.au.stopping
}

// A cancel during the download never reached the drain's own exit, the only
// place the cooldown was set, so the next scheduled check started the update
// again minutes later.
func TestCancelUpdate_DuringADownloadHoldsOffTheNextScheduledCheck(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	downloading := blockDownloadUntilCancelled(r)

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, downloading, "the requested update to start downloading")
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled download to end")

	r.au.downloadVerify = func(context.Context, *UpdateInfo) (string, error) {
		return "", errors.New("the test allows no second download")
	}
	r.mu.Lock()
	checks := r.checkCalls
	r.mu.Unlock()
	r.au.tick() // the next scheduled check; the preference is on

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkCalls != checks {
		t.Fatal("the next scheduled check must wait out the cooldown after a cancel")
	}
}

// The "Install Update Now?" question stays open while the update goes on. If
// the user cancels it from the menu meanwhile, then answers No ("keep
// waiting"), there is nothing left to wait for, and No must not start the
// update again.
func TestCancelUpdate_NoAfterTheAttemptEndedStartsNothing(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 1
	polls := steppedSleep(r)
	failed := recordManualFailures(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	r.au.confirm = func(string, string) bool {
		if err := cancelUpdateManually(); err != nil {
			t.Errorf("cancelUpdateManually: %v", err)
		}
		receiveWithin(t, attemptDone, "the cancelled attempt to end")
		return false
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	expectNoManualFailure(t, failed)

	r.au.mu.Lock()
	requested := r.au.manualRequested
	r.au.mu.Unlock()
	if requested || len(r.au.triggerCh) != 0 {
		t.Fatalf("No must not start the update the user cancelled (requested=%v, nudges=%d)", requested, len(r.au.triggerCh))
	}
	if !onMenu(r, r.info.LatestVersion) {
		t.Fatal("the release must stay on the menu")
	}
}

// The same, with No answered while the cancelled attempt is still leaving.
func TestCancelUpdate_NoWhileTheCancelledAttemptLeavesStartsNothing(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 1
	polls := steppedSleep(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	var offeredBeforeNo int
	r.au.confirm = func(string, string) bool {
		if err := cancelUpdateManually(); err != nil {
			t.Errorf("cancelUpdateManually: %v", err)
		}
		r.mu.Lock()
		offeredBeforeNo = r.showCancel
		r.mu.Unlock()
		return false
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")

	r.au.mu.Lock()
	requested := r.au.manualRequested
	r.au.mu.Unlock()
	if requested || len(r.au.triggerCh) != 0 {
		t.Fatalf("No must not queue the update the user cancelled (requested=%v, nudges=%d)", requested, len(r.au.triggerCh))
	}
	// Nor make the leaving attempt the user's request, which would offer
	// Cancel Update again for an update already called off.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.showCancel != offeredBeforeNo {
		t.Fatalf("No offered Cancel Update again for a cancelled update (shown %d, then %d)", offeredBeforeNo, r.showCancel)
	}
}

// A request names the release its own check found; the attempt's check can
// find a newer one. Cancelling must leave the newer one on the menu, not go
// back to the older.
func TestCancelUpdate_OffersTheNewestReleaseTheAttemptFound(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)
	older := &UpdateInfo{Available: true, LatestVersion: "v9.9.8", CurrentVersion: Version}

	if err := installUpdateManually(older); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the requested update to wait in its drain")
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")

	if got := GetPendingUpdate(); got == nil || got.LatestVersion != r.info.LatestVersion {
		t.Fatalf("Install Update would act on %+v, want %s", got, r.info.LatestVersion)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if last := r.pendingShown[len(r.pendingShown)-1]; last != r.info.LatestVersion {
		t.Fatalf("the menu shows %s last, want %s (shown %v)", last, r.info.LatestVersion, r.pendingShown)
	}
}

// Disconnected, an install that never told the service about its drain left
// its marker, and every later update was refused ("finish an earlier update
// attempt while connected") until the app reconnected.
func TestManualUpdate_FailedInstallNowWhileDisconnectedLeavesNoMarker(t *testing.T) {
	cfg := registeredCfg()
	cfg.OfflineMode = true
	r, _ := newManualRig(t, cfg)
	r.activeWork = 0
	r.verifyErr = errors.New("the download failed")

	if err := r.au.installManually(r.info); err == nil {
		t.Fatal("the failed install must be reported")
	}
	if marker := attemptMarker(r); marker != "" {
		t.Fatalf("an unreported drain must not leave its marker; found %q", marker)
	}
	if isDraining() {
		t.Fatal("a failed install must reopen admission")
	}

	r.mu.Lock()
	r.verifyErr = nil
	r.mu.Unlock()
	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyCalls != 1 {
		t.Fatalf("installs = %d; the next request must not be refused for the earlier attempt", r.applyCalls)
	}
}

func TestCancelUpdate_WhileDisconnectedLeavesNoMarker(t *testing.T) {
	cfg := registeredCfg()
	cfg.OfflineMode = true
	r, dialog := newManualRig(t, cfg)
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, polls, "the requested update to wait in its drain")
	if marker := attemptMarker(r); marker == "" {
		t.Fatal("the premise needs the drain's marker saved")
	}
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")

	if marker := attemptMarker(r); marker != "" {
		t.Fatalf("a drain the service never heard of must not leave its marker; found %q", marker)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drainEnter != 0 {
		t.Fatalf("the premise is a drain never reported; enters=%d", r.drainEnter)
	}
}

// A drain the service may hold keeps its marker while disconnected, as
// before: reconnecting reconciles it.
func TestCancelUpdate_AReportedDrainKeepsItsMarkerWhileDisconnected(t *testing.T) {
	cfg := registeredCfg()
	r, dialog := newManualRig(t, cfg)
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, polls, "the requested update to wait in its drain")
	cfg.WithPersistenceLock(func() { cfg.OfflineMode = true })
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")

	if marker := attemptMarker(r); marker == "" {
		t.Fatal("the service holds this drain, so its marker must wait for a reconnect")
	}
}

// macOS: Quit while an immediate install fails. Shutdown waits on the main
// thread for the failed install to clean up, and a tray call in that cleanup
// waits for the main thread, so the app never exited.
func TestInstallNow_AFailedInstallNeverHoldsShutdownOnTheTray(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	quit := macOSTray(r)
	applying, fail := failingApplyOnSignal(r)

	installed := make(chan error, 1)
	go func() { installed <- r.au.installManually(r.info) }()
	receiveWithin(t, applying, "the install to start replacing the app")

	shutdownDone := quit()
	waitForUpdater(t, "shutdown to wait for the install", r.stopping)
	close(fail)
	receiveWithin(t, shutdownDone, "shutdown to finish once the failed install let go")
	select {
	case err := <-installed:
		if err == nil {
			t.Fatal("the install failed, so it must say so")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed install did not return")
	}
}

// The same for an install at the end of a drain.
func TestDrainAndInstall_AFailedInstallNeverHoldsShutdownOnTheTray(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	quit := macOSTray(r)
	applying, fail := failingApplyOnSignal(r)

	attemptDone := make(chan struct{})
	go func() {
		r.au.tick()
		close(attemptDone)
	}()
	receiveWithin(t, applying, "the drained update to start installing")

	shutdownDone := quit()
	waitForUpdater(t, "shutdown to wait for the install", r.stopping)
	close(fail)
	receiveWithin(t, shutdownDone, "shutdown to finish once the failed install let go")
	receiveWithin(t, attemptDone, "the attempt to end")
}

// Without a shutdown, a failed install still clears the waiting status, but
// only once it has let go of the install.
func TestDrainAndInstall_AFailedInstallClearsTheStatusAfterLettingGo(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	r.applyErr = errors.New("the new version failed its signature check")
	var hiddenWhileInstalling atomic.Bool
	r.au.tray.hideDraining = func() {
		r.au.mu.Lock()
		installing := r.au.applying
		r.au.mu.Unlock()
		if installing {
			hiddenWhileInstalling.Store(true)
		}
		r.mu.Lock()
		r.hideDraining++
		r.mu.Unlock()
	}

	r.au.tick()

	if hiddenWhileInstalling.Load() {
		t.Fatal("the tray was updated while shutdown could still be waiting for the install")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hideDraining == 0 {
		t.Fatal("the waiting status must go once the install failed")
	}
	if isDraining() {
		t.Fatal("a failed install must reopen admission")
	}
}

// "Updating after current work" is wrong for an install the user chose to run
// now: it waits for nothing. Its own drain shows no status.
func TestInstallNow_ShowsNoWaitingStatusForItsOwnDrain(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1

	downloading := make(chan struct{})
	var downloads atomic.Int32
	var shownDuringInstall int
	r.au.downloadVerify = func(ctx context.Context, _ *UpdateInfo) (string, error) {
		if downloads.Add(1) == 1 {
			close(downloading)
			<-ctx.Done()
			return "", ctx.Err()
		}
		r.mu.Lock()
		shownDuringInstall = r.showDraining
		r.mu.Unlock()
		return "/tmp/verified.bin", nil
	}

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, downloading, "the scheduled attempt to start its download")
	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the downloading attempt to yield")

	if shownDuringInstall != 0 {
		t.Fatalf("the immediate install showed the waiting status (%d times)", shownDuringInstall)
	}
}

// Taking over a drain that showed the status hides it for the install.
func TestInstallNow_HidesTheWaitingStatusOfTheDrainItTakesOver(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)
	var shown, hidden int
	r.au.downloadVerify = func(context.Context, *UpdateInfo) (string, error) {
		r.mu.Lock()
		shown, hidden = r.showDraining, r.hideDraining
		r.mu.Unlock()
		return "/tmp/verified.bin", nil
	}

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	r.mu.Lock()
	shownWhileWaiting := r.showDraining
	r.mu.Unlock()
	if shownWhileWaiting == 0 {
		t.Fatal("the premise needs the waiting status shown")
	}
	r.mu.Lock()
	shown, hidden = 0, 0
	r.mu.Unlock()
	r.au.downloadVerify = func(context.Context, *UpdateInfo) (string, error) {
		r.mu.Lock()
		shown, hidden = r.showDraining, r.hideDraining
		r.mu.Unlock()
		return "/tmp/verified.bin", nil
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the waiting attempt to yield")

	if hidden < shown {
		t.Fatalf("the waiting status was still up during the install (shown %d, hidden %d)", shown, hidden)
	}
}

// An "Install now" that fails stays on the menu, as the README says of every
// update a user asked for.
func TestInstallNow_AFailedInstallStaysOnTheMenu(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	r.mu.Lock()
	r.verifyErr = errors.New("the download failed")
	r.mu.Unlock()

	if err := installUpdateManually(r.info); err == nil {
		t.Fatal("the failed install must be reported to the click")
	}
	receiveWithin(t, attemptDone, "the waiting attempt to yield")

	if !onMenu(r, r.info.LatestVersion) || GetPendingUpdate() == nil {
		t.Fatal("a failed Install now must stay on the menu")
	}
}

func TestDarwinMovedWhileRunning(t *testing.T) {
	exe := "/Users/x/Downloads/AI Expedite.app/Contents/MacOS/aix"
	gone := func(string) bool { return false }
	there := func(string) bool { return true }
	var asked string
	probe := func(p string) bool { asked = p; return true }

	if !darwinMovedWhileRunning(exe, gone) {
		t.Fatal("a bundle no longer at its launch path was moved")
	}
	if darwinMovedWhileRunning(exe, there) {
		t.Fatal("a bundle still at its launch path was not moved")
	}
	darwinMovedWhileRunning(exe, probe)
	if asked != "/Users/x/Downloads/AI Expedite.app" {
		t.Fatalf("checked %q, want the bundle itself", asked)
	}
	if darwinMovedWhileRunning("/usr/local/bin/aix", gone) {
		t.Fatal("a raw executable is not a bundle, so the location rule speaks for it")
	}
}

func TestDarwinRepointsAutoStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	inDownloads := filepath.ToSlash(filepath.Join(home, "Downloads", "AI Expedite.app"))
	inApplications := filepath.ToSlash(filepath.Join(home, "Applications", "AI Expedite.app"))

	if darwinRepointsAutoStart(inDownloads) {
		t.Fatal("updating a copy in ~/Downloads must not point login at it")
	}
	if !darwinRepointsAutoStart(inApplications) {
		t.Fatal("an installed copy keeps login pointed at itself")
	}
	if !darwinRepointsAutoStart("/Applications/AI Expedite.app") {
		t.Fatal("an installed copy keeps login pointed at itself")
	}
}
