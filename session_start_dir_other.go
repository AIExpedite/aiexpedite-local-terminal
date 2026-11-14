//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// startDirUnavailableErr reports a start folder that cannot exist because a
// component of its path is a file (ENOTDIR). A missing path is already
// fs.ErrNotExist; every other error stays unknown, so the spawn answers it as
// before.
func startDirUnavailableErr(err error) bool {
	return errors.Is(err, syscall.ENOTDIR)
}
