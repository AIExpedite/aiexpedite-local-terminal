//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// isTransientVersionProbeStartError reports a start failure that a binary on
// disk does not explain: ETXTBSY while an installer still has the new binary
// open for writing.
func isTransientVersionProbeStartError(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}
