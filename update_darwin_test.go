//go:build darwin

package main

import (
	"errors"
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

	if err := handleFailedDarwinRelaunch("/Applications/AI Expedite.app", errors.New("open failed")); err != nil {
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

	err := handleFailedDarwinRelaunch("/Applications/AI Expedite.app", errors.New("open failed"))
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
}
