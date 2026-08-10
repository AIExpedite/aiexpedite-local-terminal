//go:build darwin
// +build darwin

// File: install_runner_termios_darwin.go
//
// Per-platform ioctl request for the terminal-attribute probe used by
// isTerminalFd (see install_runner_other.go). macOS/BSD read termios via
// TIOCGETA. The elevation selection this feeds is Linux-only, but the probe
// lives in the shared !windows file, so darwin needs the constant to compile.
package main

import "golang.org/x/sys/unix"

const ttyIoctlReadTermios = unix.TIOCGETA
