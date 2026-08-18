//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRelaunchDarwinBundleWaitsForOpenResult(t *testing.T) {
	binDir := t.TempDir()
	openPath := filepath.Join(binDir, "open")
	script := []byte("#!/bin/sh\necho launch-rejected >&2\nexit 23\n")
	if err := os.WriteFile(openPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := relaunchDarwinBundle("/Applications/AI Expedite.app")
	if err == nil {
		t.Fatal("non-zero LaunchServices result must fail the handoff")
	}
	if !strings.Contains(err.Error(), "launch-rejected") {
		t.Fatalf("relaunch error should include open output, got %v", err)
	}
}

func TestFailedDarwinRelaunchYieldsWhenSingletonWasTaken(t *testing.T) {
	previousAcquire := acquireAgentInstanceAfterDarwinHandoff
	previousQuit := quitAfterDarwinHandoff
	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		acquireAgentInstanceAfterDarwinHandoff = previousAcquire
		quitAfterDarwinHandoff = previousQuit
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	acquireAgentInstanceAfterDarwinHandoff = func() (func(), bool) {
		return func() {}, false
	}
	var quits atomic.Int32
	quitAfterDarwinHandoff = func() { quits.Add(1) }

	if err := handleFailedDarwinRelaunch("/Applications/AI Expedite.app", "", errors.New("open failed")); err != nil {
		t.Fatalf("lost-singleton handoff should yield without reopening admission: %v", err)
	}
	if got := quits.Load(); got != 1 {
		t.Fatalf("quit calls = %d, want 1", got)
	}
	if path, pending := GetUpdateReady(); !pending || path != "" {
		t.Fatalf("handoff marker = (%q, %v), want empty-path pending marker", path, pending)
	}
}

func TestFailedDarwinRelaunchStaysAliveAfterReacquiringSingleton(t *testing.T) {
	previousAcquire := acquireAgentInstanceAfterDarwinHandoff
	previousQuit := quitAfterDarwinHandoff
	t.Cleanup(func() {
		acquireAgentInstanceAfterDarwinHandoff = previousAcquire
		quitAfterDarwinHandoff = previousQuit
		releaseAgentInstanceForHandoff()
	})

	var releases atomic.Int32
	acquireAgentInstanceAfterDarwinHandoff = func() (func(), bool) {
		return func() { releases.Add(1) }, true
	}
	var quits atomic.Int32
	quitAfterDarwinHandoff = func() { quits.Add(1) }

	parent := t.TempDir()
	bundle := filepath.Join(parent, "AI Expedite.app")
	backup := filepath.Join(parent, ".aixupd_old_AI Expedite.app")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "version"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "version"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := handleFailedDarwinRelaunch(bundle, backup, errors.New("open failed"))
	if err == nil {
		t.Fatal("reacquired singleton should keep the current process alive and report apply failure")
	}
	if got := quits.Load(); got != 0 {
		t.Fatalf("quit calls = %d, want 0", got)
	}
	releaseAgentInstanceForHandoff()
	if got := releases.Load(); got != 1 {
		t.Fatalf("tracked singleton release calls = %d, want 1", got)
	}
	version, readErr := os.ReadFile(filepath.Join(bundle, "version"))
	if readErr != nil {
		t.Fatalf("restored bundle is not launchable: %v", readErr)
	}
	if string(version) != "old" {
		t.Fatalf("installed bundle version = %q, want restored old bundle", version)
	}
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Fatalf("backup should be consumed by rollback: %v", statErr)
	}
}

func TestParseDarwinTeamID(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name: "valid team identifier",
			output: "Executable=/Applications/AI Expedite.app/Contents/MacOS/aiexpedite\n" +
				"Identifier=com.aiexpedite.terminal\n" +
				"Format=app bundle with Mach-O universal (x86_64 arm64)\n" +
				"TeamIdentifier=ABCDE12345\n" +
				"Sealed Resources version=2 rules=13 files=0\n",
			want:    "ABCDE12345",
			wantErr: false,
		},
		{
			name: "windows line endings",
			output: "Executable=/Applications/AI Expedite.app\r\n" +
				"TeamIdentifier=ABCDE12345\r\n",
			want:    "ABCDE12345",
			wantErr: false,
		},
		{
			name:    "unset team identifier",
			output:  "TeamIdentifier=not set\n",
			wantErr: true,
		},
		{
			name:    "empty team identifier",
			output:  "TeamIdentifier=\n",
			wantErr: true,
		},
		{
			name:    "missing team identifier",
			output:  "Executable=/Applications/AI Expedite.app\nIdentifier=com.aiexpedite.terminal\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDarwinTeamID(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDarwinTeamID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseDarwinTeamID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyDarwinSigningTeam(t *testing.T) {
	prevExtract := extractDarwinTeamID
	t.Cleanup(func() {
		extractDarwinTeamID = prevExtract
	})

	tests := []struct {
		name          string
		installedTeam string
		installedErr  error
		updateTeam    string
		updateErr     error
		wantErrSubstr string
	}{
		{
			name:          "matching teams pass",
			installedTeam: "TEAM_123",
			updateTeam:    "TEAM_123",
		},
		{
			name:          "mismatched teams rejected",
			installedTeam: "TEAM_123",
			updateTeam:    "OTHER_456",
			wantErrSubstr: "signing team mismatch",
		},
		{
			name:          "installed extraction failure rejected",
			installedErr:  errors.New("corrupt signature"),
			updateTeam:    "TEAM_123",
			wantErrSubstr: "installed bundle team verification failed",
		},
		{
			name:          "installed empty team rejected",
			installedTeam: "",
			updateTeam:    "TEAM_123",
			wantErrSubstr: "installed bundle has empty signing team identifier",
		},
		{
			name:          "update extraction failure rejected",
			installedTeam: "TEAM_123",
			updateErr:     errors.New("no signature"),
			wantErrSubstr: "update bundle team verification failed",
		},
		{
			name:          "update empty team rejected",
			installedTeam: "TEAM_123",
			updateTeam:    "",
			wantErrSubstr: "update bundle has empty signing team identifier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extractDarwinTeamID = func(bundle string) (string, error) {
				if bundle == "installed.app" {
					return tt.installedTeam, tt.installedErr
				}
				if bundle == "update.app" {
					return tt.updateTeam, tt.updateErr
				}
				return "", errors.New("unexpected bundle")
			}

			err := verifyDarwinSigningTeam("installed.app", "update.app")
			if tt.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSubstr)
				}
			}
		})
	}
}

func TestSwapDarwinBundles_AtomicExchangeArchivesBackup(t *testing.T) {
	prevSwap := atomicSwapDarwinFn
	t.Cleanup(func() {
		atomicSwapDarwinFn = prevSwap
	})

	dir := t.TempDir()
	current := filepath.Join(dir, "AI Expedite.app")
	staged := filepath.Join(dir, ".aixupd_staged_AI Expedite.app")
	backup := filepath.Join(dir, ".aixupd_old_AI Expedite.app")

	if err := os.WriteFile(current, []byte("old-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new-version"), 0o600); err != nil {
		t.Fatal(err)
	}

	atomicSwapDarwinFn = func(path1, path2 string) error {
		tmp := filepath.Join(dir, "swap_tmp")
		if err := os.Rename(path1, tmp); err != nil {
			return err
		}
		if err := os.Rename(path2, path1); err != nil {
			_ = os.Rename(tmp, path1)
			return err
		}
		if err := os.Rename(tmp, path2); err != nil {
			return err
		}
		return nil
	}

	if err := swapDarwinBundles(current, staged, backup); err != nil {
		t.Fatalf("swapDarwinBundles failed: %v", err)
	}

	curData, _ := os.ReadFile(current)
	if string(curData) != "new-version" {
		t.Fatalf("current bundle data = %q, want new-version", curData)
	}
	backupData, _ := os.ReadFile(backup)
	if string(backupData) != "old-version" {
		t.Fatalf("backup bundle data = %q, want old-version", backupData)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged bundle should not exist after archive, got stat err: %v", err)
	}
}

func TestSwapDarwinBundles_AtomicExchangeRollsBackWhenBackupFails(t *testing.T) {
	prevSwap := atomicSwapDarwinFn
	t.Cleanup(func() {
		atomicSwapDarwinFn = prevSwap
	})

	dir := t.TempDir()
	current := filepath.Join(dir, "AI Expedite.app")
	staged := filepath.Join(dir, ".aixupd_staged_AI Expedite.app")
	backup := filepath.Join(dir, "nonexistent_dir", "sub", "backup.app")

	if err := os.WriteFile(current, []byte("old-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new-version"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapCount := 0
	atomicSwapDarwinFn = func(path1, path2 string) error {
		swapCount++
		tmp := filepath.Join(dir, fmt.Sprintf("swap_tmp_%d", swapCount))
		if err := os.Rename(path1, tmp); err != nil {
			return err
		}
		if err := os.Rename(path2, path1); err != nil {
			_ = os.Rename(tmp, path1)
			return err
		}
		if err := os.Rename(tmp, path2); err != nil {
			return err
		}
		return nil
	}

	err := swapDarwinBundles(current, staged, backup)
	if err == nil {
		t.Fatal("expected error when backup rename fails, got nil")
	}
	if !strings.Contains(err.Error(), "cannot move replaced bundle to backup") {
		t.Fatalf("error = %q, want 'cannot move replaced bundle to backup'", err.Error())
	}
	if swapCount != 2 {
		t.Fatalf("swapCount = %d, want 2 (1 swap + 1 rollback)", swapCount)
	}

	curData, _ := os.ReadFile(current)
	if string(curData) != "old-version" {
		t.Fatalf("current bundle data = %q, want rolled back old-version", curData)
	}
	stagedData, _ := os.ReadFile(staged)
	if string(stagedData) != "new-version" {
		t.Fatalf("staged bundle data = %q, want new-version", stagedData)
	}
}

func TestSwapDarwinBundles_FallbackRenameRollsBack(t *testing.T) {
	prevSwap := atomicSwapDarwinFn
	t.Cleanup(func() {
		atomicSwapDarwinFn = prevSwap
	})

	atomicSwapDarwinFn = func(path1, path2 string) error {
		return errors.New("atomic swap not supported")
	}

	dir := t.TempDir()
	current := filepath.Join(dir, "AI Expedite.app")
	staged := filepath.Join(dir, "nonexistent_staged.app")
	backup := filepath.Join(dir, ".aixupd_old_AI Expedite.app")

	if err := os.WriteFile(current, []byte("old-version"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := swapDarwinBundles(current, staged, backup)
	if err == nil {
		t.Fatal("expected error when staged rename fails, got nil")
	}
	if !strings.Contains(err.Error(), "cannot move new bundle into place") {
		t.Fatalf("error = %q, want 'cannot move new bundle into place'", err.Error())
	}

	curData, _ := os.ReadFile(current)
	if string(curData) != "old-version" {
		t.Fatalf("current bundle data = %q, want rolled back old-version", curData)
	}
}

func TestSwapDarwinBundles_PreservesOldBundleWhenRollbackFails(t *testing.T) {
	prevSwap := atomicSwapDarwinFn
	t.Cleanup(func() {
		atomicSwapDarwinFn = prevSwap
	})

	dir := t.TempDir()
	current := filepath.Join(dir, "AI Expedite.app")
	staged := filepath.Join(dir, ".aixupd_staged_AI Expedite.app")
	backup := filepath.Join(dir, "nonexistent_dir", "sub", "backup.app")

	if err := os.WriteFile(current, []byte("old-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new-version"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapCount := 0
	atomicSwapDarwinFn = func(path1, path2 string) error {
		swapCount++
		if swapCount > 1 {
			return errors.New("rollback swap failed")
		}
		tmp := filepath.Join(dir, "swap_tmp")
		if err := os.Rename(path1, tmp); err != nil {
			return err
		}
		if err := os.Rename(path2, path1); err != nil {
			_ = os.Rename(tmp, path1)
			return err
		}
		if err := os.Rename(tmp, path2); err != nil {
			return err
		}
		return nil
	}

	err := swapDarwinBundles(current, staged, backup)
	if err == nil {
		t.Fatal("expected error when archive and rollback both fail, got nil")
	}
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("error = %q, want rollback-failed report", err.Error())
	}

	// Mimic applyVerifiedUpdate's defer os.RemoveAll(staged).
	_ = os.RemoveAll(staged)

	curData, readErr := os.ReadFile(current)
	if readErr != nil {
		t.Fatalf("installed bundle missing: %v", readErr)
	}
	if string(curData) != "new-version" {
		t.Fatalf("current bundle data = %q, want new-version left in place", curData)
	}
	backupData, readErr := os.ReadFile(backup)
	if readErr != nil {
		t.Fatalf("previous bundle was not preserved at backup: %v", readErr)
	}
	if string(backupData) != "old-version" {
		t.Fatalf("preserved backup data = %q, want old-version", backupData)
	}
}

func TestSwapDarwinBundles_ParksOldBundleWhenBackupUnusable(t *testing.T) {
	prevSwap := atomicSwapDarwinFn
	t.Cleanup(func() {
		atomicSwapDarwinFn = prevSwap
	})

	dir := t.TempDir()
	current := filepath.Join(dir, "AI Expedite.app")
	staged := filepath.Join(dir, ".aixupd_staged_AI Expedite.app")
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(blocker, "backup.app")

	if err := os.WriteFile(current, []byte("old-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new-version"), 0o600); err != nil {
		t.Fatal(err)
	}

	swapCount := 0
	atomicSwapDarwinFn = func(path1, path2 string) error {
		swapCount++
		if swapCount > 1 {
			return errors.New("rollback swap failed")
		}
		tmp := filepath.Join(dir, "swap_tmp")
		if err := os.Rename(path1, tmp); err != nil {
			return err
		}
		if err := os.Rename(path2, path1); err != nil {
			_ = os.Rename(tmp, path1)
			return err
		}
		if err := os.Rename(tmp, path2); err != nil {
			return err
		}
		return nil
	}

	err := swapDarwinBundles(current, staged, backup)
	if err == nil {
		t.Fatal("expected error when archive, rollback, and backup path all fail")
	}

	_ = os.RemoveAll(staged)

	parked := staged + ".previous"
	parkedData, readErr := os.ReadFile(parked)
	if readErr != nil {
		t.Fatalf("previous bundle was not parked next to staged: %v", readErr)
	}
	if string(parkedData) != "old-version" {
		t.Fatalf("parked bundle data = %q, want old-version", parkedData)
	}
}

// TestFailedDarwinRelaunchRollsBackUnderTheCallersInstallLock guards the
// re-entrancy trap in the fix for "keep updater rollback protected through
// relaunch": applyVerifiedUpdate now holds the destination lock across the
// relaunch, and flock is per-descriptor, so a rollback that tried to take that
// same lock again would block on this process's own lock and silently skip the
// restore.
func TestFailedDarwinRelaunchRollsBackUnderTheCallersInstallLock(t *testing.T) {
	previousAcquire := acquireAgentInstanceAfterDarwinHandoff
	t.Cleanup(func() {
		acquireAgentInstanceAfterDarwinHandoff = previousAcquire
		releaseAgentInstanceForHandoff()
	})
	acquireAgentInstanceAfterDarwinHandoff = func() (func(), bool) { return func() {}, true }

	parent := t.TempDir()
	bundle := filepath.Join(parent, "AI Expedite.app")
	backup := filepath.Join(parent, ".aixupd_old_AI Expedite.app")
	for path, version := range map[string]string{bundle: "new", backup: "old"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "version"), []byte(version), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Stand in for applyVerifiedUpdate holding the destination lock.
	unlock, locked := waitForDarwinInstallDirLock(parent)
	if !locked {
		t.Fatal("could not take the destination lock")
	}
	defer unlock()

	if err := handleFailedDarwinRelaunch(bundle, backup, errors.New("open failed")); err == nil {
		t.Fatal("a failed relaunch with the singleton reacquired must report the failure")
	}
	version, err := os.ReadFile(filepath.Join(bundle, "version"))
	if err != nil {
		t.Fatalf("restored bundle is not launchable: %v", err)
	}
	if string(version) != "old" {
		t.Fatalf("rollback did not run under the caller's lock: version = %q", version)
	}
}
