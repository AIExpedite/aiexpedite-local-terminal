//go:build !darwin

package main

import "os"

// friendlyComputerNamePlatform returns the platform's friendly device name, or "" when
// there is none distinct from the hostname.
//
// Windows: COMPUTERNAME is the NetBIOS name the user set — the same string
// os.Hostname() reports, but readable without a syscall and unaffected by a DNS
// suffix. Linux: there is no separate friendly name in general, so the hostname
// (via the caller's fallback) is the right answer.
func friendlyComputerNamePlatform() string {
	return os.Getenv("COMPUTERNAME")
}
