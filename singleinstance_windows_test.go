//go:build windows

package main

import (
	"path/filepath"
	"testing"
)

func TestTryAcquireAgentInstanceLockRejectsSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.instance.lock")
	first, acquired, err := tryAcquireAgentInstanceLock(path)
	if err != nil || !acquired {
		t.Fatalf("first lock acquisition = (%v, %v), want acquired", acquired, err)
	}
	defer first.Close()

	second, acquired, err := tryAcquireAgentInstanceLock(path)
	if err != nil {
		t.Fatalf("second lock acquisition returned error: %v", err)
	}
	if acquired || second != nil {
		t.Fatal("second process-equivalent lock unexpectedly acquired")
	}

	if err := unlockFile(first); err != nil {
		t.Fatalf("unlock first owner: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first owner: %v", err)
	}

	third, acquired, err := tryAcquireAgentInstanceLock(path)
	if err != nil || !acquired {
		t.Fatalf("lock after release = (%v, %v), want acquired", acquired, err)
	}
	_ = unlockFile(third)
	_ = third.Close()
}
