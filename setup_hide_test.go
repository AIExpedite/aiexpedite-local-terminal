package main

import (
	"testing"
	"time"
)

// drainConsoleHidden empties any pending signal on ConsoleHiddenChan so each
// test observes only the signal it triggers.
func drainConsoleHidden() {
	select {
	case <-ConsoleHiddenChan:
	default:
	}
}

// TestHideAfterSetupSignalsConsoleHidden asserts the setup-complete helper
// notifies the tray event loop (via ConsoleHiddenChan) that the window is now
// hidden, which is what flips the loop's consoleVisible flag to false and
// unchecks "Show Console". A nil mConsole stands in for the "systray not ready"
// case and must not panic.
func TestHideAfterSetupSignalsConsoleHidden(t *testing.T) {
	orig := setupHideDelay
	setupHideDelay = 0
	defer func() { setupHideDelay = orig }()

	drainConsoleHidden()

	// nil mConsole + no allocated console window: hideAfterSetup must be a safe
	// no-op on the window/menu side and still signal the event loop.
	hideAfterSetup(nil)

	select {
	case v := <-ConsoleHiddenChan:
		if !v {
			t.Fatalf("expected ConsoleHiddenChan to receive true, got %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hideAfterSetup did not signal ConsoleHiddenChan")
	}
}

// TestHideAfterSetupNonBlockingWhenChannelFull guards the non-blocking send: if
// monitorConsoleMinimize already left a pending signal in the buffered channel,
// hideAfterSetup must not deadlock the registration goroutine.
func TestHideAfterSetupNonBlockingWhenChannelFull(t *testing.T) {
	orig := setupHideDelay
	setupHideDelay = 0
	defer func() { setupHideDelay = orig }()

	// Pre-fill the single-slot buffered channel.
	drainConsoleHidden()
	ConsoleHiddenChan <- true

	done := make(chan struct{})
	go func() {
		hideAfterSetup(nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hideAfterSetup blocked when ConsoleHiddenChan was full")
	}

	drainConsoleHidden()
}
