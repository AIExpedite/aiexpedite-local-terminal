//go:build !windows && !linux && !darwin

// File: process_identity_other.go
// Platforms without a start-time reader: no process can be identified, so a
// session is never certified reaped here (every recorded PID reads unknown).

package main

import "errors"

func processStartToken(pid int) (string, error) {
	return "", errors.New("process start time unavailable on this platform")
}

func probeRecordedProcess(rec ledgerProcess) processProbeResult {
	return processUnknown
}

func probeRecordedDescendants(rec ledgerProcess) processProbeResult {
	return processUnknown
}
