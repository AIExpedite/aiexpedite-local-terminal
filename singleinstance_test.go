package main

import (
	"sync/atomic"
	"testing"
)

func TestReleaseAgentInstanceForHandoffRunsDeferredReleaseOnce(t *testing.T) {
	var calls atomic.Int32
	release := trackAgentInstanceRelease(func() { calls.Add(1) })

	releaseAgentInstanceForHandoff()
	release() // main's deferred cleanup must be harmless after handoff

	if got := calls.Load(); got != 1 {
		t.Fatalf("singleton release calls = %d, want 1", got)
	}
}
