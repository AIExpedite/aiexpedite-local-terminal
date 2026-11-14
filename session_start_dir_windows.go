//go:build windows

package main

import (
	"errors"
	"syscall"
)

// startDirUnavailableErr reports a start folder whose drive or share is not
// there. Go already maps ERROR_FILE_NOT_FOUND (2), ERROR_PATH_NOT_FOUND (3) and
// ERROR_BAD_NETPATH (53) to fs.ErrNotExist; these three it does not:
// ERROR_INVALID_DRIVE (15, a drive letter with no volume), ERROR_NOT_READY (21,
// removable media gone) and ERROR_BAD_NET_NAME (67, a share that is not
// there). Access denied (5) and every other error stay unknown, so the spawn
// answers them as before.
func startDirUnavailableErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case 15, 21, 67:
		return true
	}
	return false
}
