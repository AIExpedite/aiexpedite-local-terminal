// File: autoupdate_cancel_test.go
// Tests for the ways out of an update: Cancel Update, the admission an
// immediate install closes before it downloads, and the install-location rule
// for an update a user asked for.
package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordManualFailures captures the "Update Failed" dialog, which is shown on
// its own goroutine. expectNoManualFailure allows for that.
func recordManualFailures(r *autoTestRig) <-chan string {
	failed := make(chan string, 4)
	r.au.manualFailed = func(_ string, err error) { failed <- err.Error() }
	return failed
}

func expectNoManualFailure(t *testing.T, failed <-chan string) {
	t.Helper()
	select {
	case msg := <-failed:
		t.Fatalf("a cancelled update must not be reported as a failure; got %q", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func onMenu(r *autoTestRig, version string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.pendingShown {
		if v == version {
			return true
		}
	}
	return false
}

// startRequestedAttempt queues a click on Update Now and runs the attempt the
// scheduler would start for it.
func startRequestedAttempt(t *testing.T, r *autoTestRig) <-chan struct{} {
	t.Helper()
	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	return startScheduledAttempt(t, r)
}

// blockDownloadUntilCancelled makes every download wait for its context. The
// returned channel closes when the first one has started.
func blockDownloadUntilCancelled(r *autoTestRig) <-chan struct{} {
	downloading := make(chan struct{})
	var once sync.Once
	r.au.downloadVerify = func(ctx context.Context, _ *UpdateInfo) (string, error) {
		once.Do(func() { close(downloading) })
		<-ctx.Done()
		return "", ctx.Err()
	}
	return downloading
}

// The way out that #279 lacked: once the user chose "install when work
// finishes", only Install now or quitting the app ended the wait.
func TestCancelUpdate_WhileWaitingForWorkReopensAdmissionAndKeepsTheUpdateOnTheMenu(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)
	failed := recordManualFailures(r)

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, polls, "the requested update to wait in its drain")
	if !isDraining() {
		t.Fatal("the attempt must be holding admission closed while it waits")
	}
	r.mu.Lock()
	offered := r.showCancel
	r.mu.Unlock()
	if offered == 0 {
		t.Fatal("Cancel Update must be offered while the update waits")
	}

	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")

	if isDraining() {
		t.Fatal("cancelling must reopen admission")
	}
	r.mu.Lock()
	defer func() { r.mu.Unlock(); expectNoManualFailure(t, failed) }()
	if r.applyCalls != 0 {
		t.Fatal("a cancelled update must not install")
	}
	if r.activeWork != 1 {
		t.Fatal("cancelling must not touch the running work")
	}
	if len(r.drainExitR) != 1 || r.drainExitR[0] != "deferred" {
		t.Fatalf("drain exits = %v, want the service told once", r.drainExitR)
	}
	if r.onlineCalls == 0 {
		t.Fatal("the computer must report ready again so routing resumes")
	}
	if r.hideDraining == 0 || r.hideCancel == 0 {
		t.Fatalf("the waiting status and Cancel Update must go (hideDraining=%d hideCancel=%d)", r.hideDraining, r.hideCancel)
	}
	if len(r.pendingShown) == 0 || r.pendingShown[len(r.pendingShown)-1] != r.info.LatestVersion {
		t.Fatalf("the update must stay on the menu; shown %v", r.pendingShown)
	}
	if GetPendingUpdate() == nil {
		t.Fatal("Install Update needs the release to act on")
	}
}

// A scheduled attempt can be called off too, and the next scheduled check must
// not start waiting again straight away.
func TestCancelUpdate_ScheduledAttemptHoldsOffTheNextCheck(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 1
	polls := steppedSleep(r)
	failed := recordManualFailures(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	r.mu.Lock()
	offered := r.showCancel
	r.mu.Unlock()
	if offered == 0 {
		t.Fatal("Cancel Update must be offered while a scheduled update waits")
	}
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")
	expectNoManualFailure(t, failed)

	r.mu.Lock()
	checks := r.checkCalls
	r.mu.Unlock()
	r.au.sleep = func(time.Duration) bool { return false } // never park if this goes wrong
	r.au.tick()                                            // the next scheduled check
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkCalls != checks {
		t.Fatal("the next scheduled check must wait out the cooldown")
	}
	if len(r.pendingShown) == 0 {
		t.Fatal("the update must stay on the menu")
	}
}

func TestCancelUpdate_DuringADownloadStopsItWithoutReportingAFailure(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	downloading := blockDownloadUntilCancelled(r)
	failed := recordManualFailures(r)

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, downloading, "the requested update to start downloading")
	r.mu.Lock()
	offered := r.showCancel
	r.mu.Unlock()
	if offered == 0 {
		t.Fatal("a requested update must be cancellable while it downloads")
	}

	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled download to end")
	expectNoManualFailure(t, failed)

	if isDraining() {
		t.Fatal("admission was never closed for this attempt")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drainEnter != 0 || r.applyCalls != 0 {
		t.Fatalf("a cancelled download must go no further (enters=%d applies=%d)", r.drainEnter, r.applyCalls)
	}
	if len(r.pendingShown) == 0 {
		t.Fatal("the update must stay on the menu")
	}
}

// A cancel landing after the download finished, before admission closes.
func TestCancelUpdate_BetweenDownloadAndDrainNeverClosesAdmission(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	failed := recordManualFailures(r)
	var checks atomic.Int32
	r.au.registering = func() bool {
		// runAttempt asks before the download and again after it.
		if checks.Add(1) == 2 {
			if err := cancelUpdateManually(); err != nil {
				t.Errorf("cancelUpdateManually: %v", err)
			}
		}
		return false
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()
	expectNoManualFailure(t, failed)

	if isDraining() {
		t.Fatal("a cancelled attempt must not close admission")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drainEnter != 0 || r.applyCalls != 0 {
		t.Fatalf("a cancelled attempt must go no further (enters=%d applies=%d)", r.drainEnter, r.applyCalls)
	}
	if len(r.pendingShown) == 0 {
		t.Fatal("the update must stay on the menu")
	}
}

// The zero-work observation and the seal are one decision: a cancel that lands
// between them still wins.
func TestCancelUpdate_LandingJustBeforeTheInstallIsClaimedWins(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	failed := recordManualFailures(r)
	var claims atomic.Int32
	r.au.claimInstall = func() bool { claims.Add(1); return true }

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	// Installed after the click, whose own read of the work is not under test:
	// the drain reads it just before claiming the install.
	var once sync.Once
	r.au.activeWork = func() int {
		once.Do(func() {
			if err := cancelUpdateManually(); err != nil {
				t.Errorf("cancelUpdateManually: %v", err)
			}
		})
		return 0
	}
	r.au.tick()
	expectNoManualFailure(t, failed)

	if got := claims.Load(); got != 0 {
		t.Fatalf("the install was claimed %d time(s) after the cancel", got)
	}
	if isDraining() {
		t.Fatal("cancelling must reopen admission")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyCalls != 0 {
		t.Fatal("a cancelled update must not install")
	}
}

func TestCancelUpdate_RefusedOnceTheInstallHasBegun(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	var refusal error
	withdrawnBeforeInstall := 0
	r.au.apply = func(_ string, _ *UpdateInfo) error {
		r.mu.Lock()
		withdrawnBeforeInstall = r.hideCancel
		r.mu.Unlock()
		refusal = cancelUpdateManually()
		return nil
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()

	if !errors.Is(refusal, errUpdateSealed) {
		t.Fatalf("cancel during the install = %v, want %v", refusal, errUpdateSealed)
	}
	if withdrawnBeforeInstall == 0 {
		t.Fatal("Cancel Update must be withdrawn before the install begins")
	}
}

// However an attempt ends, the way out goes with it.
func TestCancelUpdate_WithdrawnWhenTheAttemptEnds(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	r.verifyErr = errors.New("the download failed")
	r.au.sleep = func(time.Duration) bool { return true }

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.showCancel == 0 || r.hideCancel == 0 {
		t.Fatalf("Cancel Update must go when the attempt ends (show=%d hide=%d)", r.showCancel, r.hideCancel)
	}
}

// A request that joins an attempt already under way makes that attempt the
// user's, so it must be offered the way out at that moment.
func TestCancelUpdate_OfferedToARequestThatJoinsARunningAttempt(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	downloading := blockDownloadUntilCancelled(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, downloading, "the scheduled attempt to start its download")
	r.mu.Lock()
	before := r.showCancel
	r.mu.Unlock()
	if before != 0 {
		t.Fatal("a scheduled download has no one to offer Cancel Update to yet")
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.mu.Lock()
	offered := r.showCancel
	r.mu.Unlock()
	if offered == 0 {
		t.Fatal("a request that joins a running attempt must be offered Cancel Update")
	}
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the cancelled attempt to end")
}

// The real sleep, which the rig replaces: a cancel must end the wait at once
// and report true, so the drain loop goes round and leaves properly instead of
// treating the wait as a stop.
func TestRealSleep_ACancelEndsTheWaitWithoutReadingAsAStop(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	cancelled := make(chan struct{})
	r.au.mu.Lock()
	r.au.inProgress = true
	r.au.cancelRequested = cancelled
	r.au.mu.Unlock()
	t.Cleanup(func() {
		r.au.mu.Lock()
		r.au.inProgress = false
		r.au.cancelRequested = nil
		r.au.mu.Unlock()
	})

	result := make(chan bool, 1)
	go func() { result <- r.au.realSleep(time.Hour) }()
	// realSleep reads the channel when it starts, so give it the moment to.
	time.Sleep(50 * time.Millisecond)
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	select {
	case got := <-result:
		if !got {
			t.Fatal("a cancel must not read as the updater stopping")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when the update was cancelled")
	}
}

func TestCancelUpdate_RefusedDuringAnImmediateInstall(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.au.mu.Lock()
	r.au.manualInProgress = true
	r.au.mu.Unlock()
	if err := cancelUpdateManually(); !errors.Is(err, errImmediateInstallBusy) {
		t.Fatalf("cancel during an immediate install = %v, want %v", err, errImmediateInstallBusy)
	}
}

// A menu item left showing by a race must do nothing but go away.
func TestCancelUpdate_WithNothingToCancelHidesTheItem(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hideCancel != 1 {
		t.Fatalf("hideCancel = %d, want 1", r.hideCancel)
	}
	if len(r.drainExitR) != 0 {
		t.Fatalf("there was no drain to leave, yet the service was told: %v", r.drainExitR)
	}
}

// Asking again after cancelling is the user changing their mind. It must not be
// swallowed by the attempt that is leaving.
func TestCancelUpdate_AClickWhileTheCancelledAttemptLeavesStartsAnother(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)
	seen := observeApply(r)
	failed := recordManualFailures(r)

	exiting := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.au.drainExit = func(_ context.Context, _, reason string) error {
		once.Do(func() { close(exiting) })
		<-release
		r.mu.Lock()
		defer r.mu.Unlock()
		r.drainExitR = append(r.drainExitR, reason)
		return nil
	}

	attemptDone := startRequestedAttempt(t, r)
	receiveWithin(t, polls, "the requested update to wait in its drain")
	if err := cancelUpdateManually(); err != nil {
		t.Fatalf("cancelUpdateManually: %v", err)
	}
	receiveWithin(t, exiting, "the cancelled attempt to start leaving its drain")
	<-r.au.triggerCh // the first click's nudge, already answered by the attempt above

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	wantTitle, _ := manualWaitForWorkPrompt(1, r.info.LatestVersion)
	got := dialog.questions()
	if last := got[len(got)-1]; last != wantTitle {
		t.Fatalf("a click after cancelling asked %q, want %q; it must not offer to join the attempt that is leaving", last, wantTitle)
	}

	close(release)
	receiveWithin(t, attemptDone, "the cancelled attempt to end")
	expectNoManualFailure(t, failed)
	if got := len(r.au.triggerCh); got == 0 {
		t.Fatal("the queued request must wake the scheduler")
	}

	r.mu.Lock()
	r.activeWork = 0
	r.mu.Unlock()
	nextDone := make(chan struct{})
	go func() {
		r.au.tick()
		close(nextDone)
	}()
	for finished := false; !finished; {
		select {
		case <-polls:
		case <-nextDone:
			finished = true
		case <-time.After(5 * time.Second):
			t.Fatal("the request made after cancelling did not install")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(*seen) != 1 {
		t.Fatalf("installs = %d, want the one the user asked for after cancelling", len(*seen))
	}
}

// What #279 left: "Install now" while the release was still downloading kept
// admission open, so a session started during the download was cut off by the
// restart moments later.
func TestManualUpdate_InstallNowDuringADownloadClosesAdmissionFirst(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1

	downloading := make(chan struct{})
	var downloads atomic.Int32
	var drainingDuringDownload, refusedDuringDownload bool
	var entersDuringDownload int
	r.au.downloadVerify = func(ctx context.Context, _ *UpdateInfo) (string, error) {
		if downloads.Add(1) == 1 {
			close(downloading)
			<-ctx.Done()
			return "", ctx.Err()
		}
		drainingDuringDownload = isDraining()
		admitted, release := admitWork(commandMsg{Type: "session_start"})
		refusedDuringDownload = !admitted
		if release != nil {
			release()
		}
		r.mu.Lock()
		entersDuringDownload = r.drainEnter
		r.mu.Unlock()
		return "/tmp/verified.bin", nil
	}
	seen := observeApply(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, downloading, "the scheduled attempt to start its download")
	if isDraining() {
		t.Fatal("the premise needs admission open while the attempt downloads")
	}

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the downloading attempt to yield")

	wantTitle, _ := manualInstallNowPrompt(1)
	if got := dialog.questions(); len(got) != 1 || got[0] != wantTitle {
		t.Fatalf("questions asked = %q, want exactly %q", got, wantTitle)
	}
	if !drainingDuringDownload {
		t.Fatal("admission must close before the install downloads")
	}
	if !refusedDuringDownload {
		t.Fatal("a session must be refused while the install downloads")
	}
	if entersDuringDownload != 1 {
		t.Fatalf("the service must be told before the download; enters=%d", entersDuringDownload)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(*seen) != 1 || (*seen)[0].work != 1 {
		t.Fatalf("installs = %+v, want one that does not wait for the running session", *seen)
	}
}

func TestManualUpdate_FailedInstallNowDuringADownloadReopensAdmission(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1

	downloading := make(chan struct{})
	var downloads atomic.Int32
	r.au.downloadVerify = func(ctx context.Context, _ *UpdateInfo) (string, error) {
		if downloads.Add(1) == 1 {
			close(downloading)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "", errors.New("the download failed")
	}

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, downloading, "the scheduled attempt to start its download")

	if err := installUpdateManually(r.info); err == nil {
		t.Fatal("the failed install must be reported to the click")
	}
	receiveWithin(t, attemptDone, "the downloading attempt to yield")

	if isDraining() {
		t.Fatal("a failed install must reopen admission")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.drainExitR) != 1 || r.drainExitR[0] != "superseded" {
		t.Fatalf("drain exits = %v, want the service told once", r.drainExitR)
	}
	if r.hideDraining == 0 {
		t.Fatal("the waiting status must go once admission reopens")
	}
	if r.au.cfg.PendingUpdateAttemptID != "" {
		t.Fatalf("the attempt marker must be cleared; found %q", r.au.cfg.PendingUpdateAttemptID)
	}
}

func TestManualUpdate_InstallNowIsRefusedWhileTheDeviceRegisters(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1

	downloading := make(chan struct{})
	var downloads atomic.Int32
	r.au.downloadVerify = func(ctx context.Context, _ *UpdateInfo) (string, error) {
		if downloads.Add(1) == 1 {
			close(downloading)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "/tmp/verified.bin", nil
	}

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, downloading, "the scheduled attempt to start its download")
	if !beginRegistration() {
		t.Fatal("registration should be able to begin while admission is open")
	}
	t.Cleanup(endRegistration)

	err := installUpdateManually(r.info)
	receiveWithin(t, attemptDone, "the downloading attempt to yield")
	if !errors.Is(err, errRegistrationInProgress) {
		t.Fatalf("install now during registration = %v, want %v", err, errRegistrationInProgress)
	}
	if got := downloads.Load(); got != 1 {
		t.Fatalf("downloads = %d; the install must stop before downloading", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyCalls != 0 {
		t.Fatal("nothing may install while the device registers")
	}
}

// An install that takes over a drain already in place keeps that drain: it does
// not close a second one or tell the service twice.
func TestManualUpdate_InstallNowDuringADrainKeepsThatDrain(t *testing.T) {
	r, dialog := newManualRig(t, registeredCfg())
	dialog.answer = true
	r.activeWork = 1
	polls := steppedSleep(r)
	seen := observeApply(r)

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	attempt := drainingAttempt()

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the waiting attempt to yield")

	if got := drainingAttempt(); got != attempt {
		t.Fatalf("the drain changed from %q to %q", attempt, got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drainEnter != 1 {
		t.Fatalf("drain enters = %d, want the attempt's one", r.drainEnter)
	}
	if len(*seen) != 1 {
		t.Fatalf("installs = %d, want 1", len(*seen))
	}
}

// The attempt can leave its drain in the instant before the click takes it
// over. The install must then close admission itself rather than run on the
// attempt's drain, which no longer holds anything closed.
func TestManualUpdate_InstallNowAfterTheAttemptLeftItsDrainClosesItsOwn(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 1
	polls := steppedSleep(r)
	var drainingDuringDownload bool
	var attemptDuringDownload string
	r.au.downloadVerify = func(_ context.Context, _ *UpdateInfo) (string, error) {
		drainingDuringDownload = isDraining()
		attemptDuringDownload = drainingAttempt()
		return "/tmp/verified.bin", nil
	}

	attemptDone := startScheduledAttempt(t, r)
	receiveWithin(t, polls, "the scheduled attempt to wait in its drain")
	left := drainingAttempt()
	reopenAdmission() // what exitDrain does first

	if err := r.au.installManually(r.info); err != nil {
		t.Fatalf("installManually: %v", err)
	}
	receiveWithin(t, attemptDone, "the waiting attempt to yield")

	if !drainingDuringDownload {
		t.Fatal("admission must be closed during the download")
	}
	if attemptDuringDownload == "" || attemptDuringDownload == left {
		t.Fatalf("the install must hold its own drain; had %q, the attempt's was %q", attemptDuringDownload, left)
	}
}

// A copy in ~/Downloads on a Mac: the scheduled path leaves it alone, Update
// Now updates it, as it did before #279.
func TestRunAttempt_RequestedUpdateUsesTheOnRequestLocationRule(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	r.installableOK = false
	r.installableMsg = "running from Downloads"
	r.au.manualLocation = func() (bool, string) { return true, "" }

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyCalls != 1 {
		t.Fatalf("applies = %d, want the requested update installed", r.applyCalls)
	}
	if r.showBlocked != 0 {
		t.Fatal("the update was allowed, so nothing may say it is blocked")
	}
}

func TestRunAttempt_ScheduledUpdateKeepsTheStricterLocationRule(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.installableOK = false
	r.installableMsg = "running from Downloads"
	r.au.manualLocation = func() (bool, string) {
		t.Error("the scheduled path must not use the on-request rule")
		return true, ""
	}

	r.au.tick()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkCalls != 0 || r.applyCalls != 0 {
		t.Fatalf("a blocked install must not go on (check=%d apply=%d)", r.checkCalls, r.applyCalls)
	}
	if r.showBlocked != 1 {
		t.Fatalf("showBlocked = %d, want 1", r.showBlocked)
	}
}

func TestRunAttempt_RequestedUpdateStillRefusedWhereItCannotReplace(t *testing.T) {
	r, _ := newManualRig(t, registeredCfg())
	r.activeWork = 0
	r.au.manualLocation = func() (bool, string) { return false, "running from a mounted disk image" }
	failed := recordManualFailures(r)

	if err := installUpdateManually(r.info); err != nil {
		t.Fatalf("installUpdateManually: %v", err)
	}
	r.au.tick()

	select {
	case msg := <-failed:
		if msg != "running from a mounted disk image" {
			t.Fatalf("reported %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the user must be told why the update could not install")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checkCalls != 0 || r.applyCalls != 0 {
		t.Fatalf("a refused location must not go on (check=%d apply=%d)", r.checkCalls, r.applyCalls)
	}
}

func TestDarwinDownloadsCopyReplaceable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writable := func(string) bool { return true }
	readOnly := func(string) bool { return false }
	inDownloads := filepath.Join(home, "Downloads", "AI Expedite.app", "Contents", "MacOS", "aix")
	inApplications := filepath.Join(home, "Applications", "AI Expedite.app", "Contents", "MacOS", "aix")
	lookalike := filepath.Join(home, "DownloadsOld", "AI Expedite.app", "Contents", "MacOS", "aix")
	rawBinary := filepath.Join(home, "Downloads", "aix")

	if !darwinDownloadsCopyReplaceable(inDownloads, writable) {
		t.Fatal("a writable copy in ~/Downloads is exactly what Update Now may replace")
	}
	if darwinDownloadsCopyReplaceable(inDownloads, readOnly) {
		t.Fatal("a read-only copy cannot be replaced")
	}
	if darwinDownloadsCopyReplaceable(inApplications, writable) {
		t.Fatal("only ~/Downloads gets the exception; other locations use the ordinary rule")
	}
	if darwinDownloadsCopyReplaceable(lookalike, writable) {
		t.Fatal("a folder that merely starts with Downloads is not ~/Downloads")
	}
	if darwinDownloadsCopyReplaceable(rawBinary, writable) {
		t.Fatal("a raw executable has no bundle to replace")
	}
	if !darwinBundleInDownloads(filepath.ToSlash(filepath.Join(home, "Downloads", "AI Expedite.app"))) {
		t.Fatal("the automatic rule and the exception must agree on what ~/Downloads is")
	}
}
