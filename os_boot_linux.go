//go:build linux

// File: os_boot_linux.go
// Linux OS boot identity (os_boot.go): the kernel's random per-boot id,
// /proc/sys/kernel/random/boot_id, the same reader the Linux start token uses
// (process_identity_linux.go). Nothing about any process is read.

package main

import "strings"

// readOSBootStamp returns the current OS boot identity, or none.
func readOSBootStamp() osBootStamp {
	id := linuxBootID()
	if id == "" || strings.ContainsAny(id, " \t\r\n") {
		return osBootStamp{}
	}
	return osBootStamp{ID: osBootKindLinux + ":" + id}
}
