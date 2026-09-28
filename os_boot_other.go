//go:build !windows && !linux && !darwin

// File: os_boot_other.go
// Platforms without an OS boot identity reader: no identity, so no session is
// ever proven reaped by an OS reboot here.

package main

func readOSBootStamp() osBootStamp { return osBootStamp{} }
