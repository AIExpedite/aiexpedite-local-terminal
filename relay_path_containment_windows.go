//go:build windows

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	// finalPathMax is the longest path GetFinalPathNameByHandle can answer
	// (Windows' extended-length limit, in UTF-16 units).
	finalPathMax = 32768
	// finalPathVolumeNameDOS asks for a drive-letter path (VOLUME_NAME_DOS |
	// FILE_NAME_NORMALIZED, both 0); x/sys/windows does not name them.
	finalPathVolumeNameDOS = 0
)

// realDirPath is the path the volume itself gives an existing file or folder:
// GetFinalPathNameByHandle on a handle opened through every reparse point. It
// resolves symlinks AND junctions (filepath.EvalSymlinks has not followed
// junctions since Go 1.23) and long-forms 8.3 short names, so dirWithin walks
// a path with no link left in it. The `\\?\` (and `\\?\UNC\`) prefix the API
// adds is dropped, so a local folder reads as `C:\...` and a share as
// `\\server\share\...`.
func realDirPath(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(
		name,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		// Required to open a directory; without FILE_FLAG_OPEN_REPARSE_POINT
		// every link on the way is followed to its target.
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	// A buffer too small is answered with the size it needs (terminator
	// included), so a typical path costs one call and a long one two.
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), finalPathVolumeNameDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			buf = buf[:n]
			break
		}
		if int(n) > finalPathMax {
			return "", fmt.Errorf("final path of %q is longer than %d characters", path, finalPathMax)
		}
		buf = make([]uint16, n)
	}
	final := windows.UTF16ToString(buf)
	switch {
	case strings.HasPrefix(final, `\\?\UNC\`):
		final = `\\` + final[len(`\\?\UNC\`):]
	case strings.HasPrefix(final, `\\?\`):
		final = final[len(`\\?\`):]
	}
	return final, nil
}
