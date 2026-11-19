//go:build windows

package main

import (
	"fmt"
	"path/filepath"
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
	// longPathThreshold is where Go's own os package starts adding the
	// extended-length prefix (248 = MAX_PATH less room for an 8.3 file name,
	// the limit CreateDirectory applies).
	longPathThreshold = 248
)

// extendedLengthPath is path in the `\\?\` form CreateFileW needs to open a
// path longer than MAX_PATH on a computer without the long-path opt-in: the
// same rewrite Go's os package applies internally (windows.CreateFile does
// not). A local `C:\...` path becomes `\\?\C:\...` and a share
// `\\server\share\...` becomes `\\?\UNC\server\share\...`. The path is
// cleaned first because the prefix turns off Win32 normalisation (`/`, `.`,
// `..` and repeated separators are taken literally). A short, relative,
// already-prefixed or device path is returned unchanged.
func extendedLengthPath(path string) string {
	if len(path) < longPathThreshold || !filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) ||
		strings.HasPrefix(path, `//?/`) || strings.HasPrefix(path, `//./`) {
		return path
	}
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, `\\`) {
		return `\\?\UNC\` + clean[len(`\\`):]
	}
	return `\\?\` + clean
}

// realDirPath is the path the volume itself gives an existing file or folder:
// GetFinalPathNameByHandle on a handle opened through every reparse point. It
// resolves symlinks AND junctions (filepath.EvalSymlinks has not followed
// junctions since Go 1.23) and long-forms 8.3 short names, so dirWithin walks
// a path with no link left in it. The `\\?\` (and `\\?\UNC\`) prefix the API
// adds is dropped, so a local folder reads as `C:\...` and a share as
// `\\server\share\...`. A path longer than MAX_PATH is opened through its
// extended-length form (extendedLengthPath), as os.Stat would open it.
func realDirPath(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(extendedLengthPath(path))
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
