//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// processHandleGone reports whether the OS confirms this process no longer
// runs. Unix: signal 0 probes existence without delivering anything.
//
// FAIL CLOSED (see probeProcessGone): a delivered signal means SOMETHING
// with this PID exists — possibly our zombie child awaiting a wedged Wait,
// possibly a recycled PID — and both read "alive" so the caller keeps the
// device fenced. Only ESRCH ("no such process") and Go's own
// ErrProcessDone are positive absence. EPERM (a recycled PID owned by
// another user) is alive; any unexpected error is alive.
func processHandleGone(p *os.Process) bool {
	err := p.Signal(syscall.Signal(0))
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrProcessDone) {
		return true
	}
	return errors.Is(err, syscall.ESRCH)
}
