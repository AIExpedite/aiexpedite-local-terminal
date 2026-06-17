//go:build windows
// +build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFileExclusive blocks until an exclusive byte-range lock is held on f via
// LockFileEx. Locks are released when the file handle closes, so callers can
// rely on `defer f.Close()` if unlockFile is missed.
func lockFileExclusive(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
}

// unlockFile releases the byte-range lock acquired by lockFileExclusive.
func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 0xFFFFFFFF, 0xFFFFFFFF, ol)
}
