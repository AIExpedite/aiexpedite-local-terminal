//go:build !darwin

package main

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestApplyVerifiedUpdateKeepsAgentAliveWhenUpdaterCannotStart(t *testing.T) {
	previousStart := startSelfReplaceProcess
	previousQuit := quitAfterSelfReplaceStart
	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		startSelfReplaceProcess = previousStart
		quitAfterSelfReplaceStart = previousQuit
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	startSelfReplaceProcess = func(string, []string) error {
		return errors.New("execution blocked")
	}
	var quits atomic.Int32
	quitAfterSelfReplaceStart = func() { quits.Add(1) }

	artifact, err := os.CreateTemp(t.TempDir(), "verified-update-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}

	err = applyVerifiedUpdate(artifact.Name(), nil)
	if err == nil || !strings.Contains(err.Error(), "execution blocked") {
		t.Fatalf("apply error = %v, want updater start failure", err)
	}
	if got := quits.Load(); got != 0 {
		t.Fatalf("quit calls = %d, want 0", got)
	}
	if _, pending := GetUpdateReady(); pending {
		t.Fatal("failed updater start must not commit a shutdown handoff")
	}
}

func TestApplyVerifiedUpdateCommitsHandoffAfterUpdaterStarts(t *testing.T) {
	previousStart := startSelfReplaceProcess
	previousQuit := quitAfterSelfReplaceStart
	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		startSelfReplaceProcess = previousStart
		quitAfterSelfReplaceStart = previousQuit
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	var starts, quits atomic.Int32
	startSelfReplaceProcess = func(string, []string) error {
		starts.Add(1)
		return nil
	}
	quitAfterSelfReplaceStart = func() { quits.Add(1) }

	artifact, err := os.CreateTemp(t.TempDir(), "verified-update-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}
	if err := applyVerifiedUpdate(artifact.Name(), nil); err != nil {
		t.Fatalf("applyVerifiedUpdate: %v", err)
	}

	if starts.Load() != 1 || quits.Load() != 1 {
		t.Fatalf("starts/quits = %d/%d, want 1/1", starts.Load(), quits.Load())
	}
	if path, pending := GetUpdateReady(); !pending || path != "" {
		t.Fatalf("handoff marker = (%q, %v), want empty-path pending marker", path, pending)
	}
}
