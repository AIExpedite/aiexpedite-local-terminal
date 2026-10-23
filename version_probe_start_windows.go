//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isTransientVersionProbeStartError reports a start failure that a binary on
// disk does not explain: Windows refuses to open an executable while real-time
// AV is still scanning a freshly written copy, or while an installer holds it.
func isTransientVersionProbeStartError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
