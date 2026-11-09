//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// cacheFileReplaceInProgress reports a read refused because another process
// was renaming over the file at that instant: the rename holds the file open
// for delete, and Go opens files without sharing delete access.
func cacheFileReplaceInProgress(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
