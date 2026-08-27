//go:build windows

package main

import (
	"os"
	"syscall"
)

// stillActiveExitCode is Windows' GetExitCodeProcess sentinel for a process
// that has not exited (STILL_ACTIVE, 259).
const stillActiveExitCode = 259

// processQueryLimitedInformation is the minimal access right needed to read
// an exit code (PROCESS_QUERY_LIMITED_INFORMATION).
const processQueryLimitedInformation = 0x1000

// processHandleGone reports whether the OS confirms this process no longer
// runs.
//
// Windows makes this check strong: the caller's os.Process holds an open
// handle to the process OBJECT, and Windows does not recycle a PID while any
// handle to its object remains open — so OpenProcess-by-PID here reaches OUR
// child, not a stranger, and a terminated child reads gone (exit code ≠
// STILL_ACTIVE) even while unreaped. FAIL CLOSED on anything unreadable: an
// open we cannot classify, or an exit code we cannot fetch, reads "alive" so
// the caller keeps the device fenced (see probeProcessGone).
func processHandleGone(p *os.Process) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(p.Pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER (87): no live process object for this PID —
		// our handle in os.Process would keep the object alive, so this means
		// the process record itself is gone. Any other open failure (access
		// denied, transient) fails closed as alive.
		return err == syscall.Errno(87)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code != stillActiveExitCode
}
