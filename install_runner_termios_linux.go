//go:build linux
// +build linux

// File: install_runner_termios_linux.go
//
// Per-platform ioctl request for the terminal-attribute probe used by
// isTerminalFd (see install_runner_other.go). A successful IoctlGetTermios with
// this request means the fd is backed by a real tty line discipline, which is
// what distinguishes an interactive terminal from a character device like
// /dev/null. Linux reads termios via TCGETS.
package main

import "golang.org/x/sys/unix"

const ttyIoctlReadTermios = unix.TCGETS
