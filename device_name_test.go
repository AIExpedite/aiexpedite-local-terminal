// device_name_test.go
// -----------------------------------------------------------------------------
// The device name is a display label, but it is the ONLY thing identifying a
// machine in the website's device list — and auth.go re-sends it on every token
// exchange, so whatever resolveDeviceName returns overwrites the stored name.
// A Mac reported itself as "56:36:05:D3:C2:75" because os.Hostname() returns the
// DHCP/Bonjour hostname, not the ComputerName the user set.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestResolveDeviceNamePrefersTheFriendlyName(t *testing.T) {
	restore := stubFriendlyComputerName(t, "Daniel-Mac")
	defer restore()

	if got := resolveDeviceName(); got != "Daniel-Mac" {
		t.Fatalf("resolveDeviceName() = %q, want the friendly computer name", got)
	}
}

func TestResolveDeviceNameTrimsTheFriendlyName(t *testing.T) {
	// scutil's output ends in a newline; an untrimmed name would be sent over
	// the wire and rendered with a stray break.
	restore := stubFriendlyComputerName(t, "  Daniel-Mac\n")
	defer restore()

	if got := resolveDeviceName(); got != "Daniel-Mac" {
		t.Fatalf("resolveDeviceName() = %q, want it trimmed", got)
	}
}

func TestResolveDeviceNameFallsBackToTheHostname(t *testing.T) {
	restore := stubFriendlyComputerName(t, "")
	defer restore()

	hostname, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	if got := resolveDeviceName(); got != strings.TrimSpace(hostname) {
		t.Fatalf("resolveDeviceName() = %q, want the hostname %q", got, hostname)
	}
}

// A blank friendly name AND a blank hostname must still yield something a
// device list can render.
func TestResolveDeviceNameNeverReturnsEmpty(t *testing.T) {
	restore := stubFriendlyComputerName(t, "   ")
	defer restore()

	if got := resolveDeviceName(); strings.TrimSpace(got) == "" {
		t.Fatal("resolveDeviceName() returned an empty name")
	}
}

// getDeviceName is called on every token exchange; resolution may shell out, so
// it must resolve once and reuse the answer.
func TestGetDeviceNameIsMemoised(t *testing.T) {
	calls := 0
	restore := stubFriendlyComputerNameFunc(t, func() string {
		calls++
		return "Daniel-Mac"
	})
	defer restore()

	for i := 0; i < 5; i++ {
		if got := getDeviceName(); got != "Daniel-Mac" {
			t.Fatalf("getDeviceName() = %q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("friendly name resolved %d times, want 1 — getDeviceName is not memoised", calls)
	}
}

// The real platform hook must work on the host this runs on: a stubbed-only
// test would pass even if scutil were mis-invoked.
func TestFriendlyComputerNameOnThisHost(t *testing.T) {
	name := friendlyComputerNamePlatform()
	switch runtime.GOOS {
	case "darwin":
		if strings.TrimSpace(name) == "" {
			t.Skip("no ComputerName configured on this Mac")
		}
		if strings.ContainsAny(name, "\n\r") {
			t.Fatalf("friendlyComputerName() = %q, want a single trimmed line", name)
		}
	default:
		// Non-darwin returns COMPUTERNAME, which is empty off Windows. Either
		// way it must never be whitespace-padded junk.
		if name != strings.TrimSpace(name) {
			t.Fatalf("friendlyComputerName() = %q, want no surrounding whitespace", name)
		}
	}
}

// stubFriendlyComputerName swaps the platform hook for the duration of a test
// and clears the memoised name on both sides of the swap.
func stubFriendlyComputerName(t *testing.T, name string) func() {
	t.Helper()
	return stubFriendlyComputerNameFunc(t, func() string { return name })
}

func stubFriendlyComputerNameFunc(t *testing.T, fn func() string) func() {
	t.Helper()
	previous := friendlyComputerName
	friendlyComputerName = fn
	resetDeviceNameCache()
	return func() {
		friendlyComputerName = previous
		resetDeviceNameCache()
	}
}
