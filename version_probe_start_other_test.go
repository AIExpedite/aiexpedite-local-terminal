//go:build !windows

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
)

// ETXTBSY is what starting a binary an installer still has open for writing
// returns: the bytes on disk do not explain it, so it is transient.
func TestClassifyVersionProbeError_TextFileBusyIsTransient(t *testing.T) {
	err := &os.PathError{Op: "fork/exec", Path: "/usr/local/bin/cli", Err: syscall.ETXTBSY}
	if got := classifyVersionProbeError(context.Background(), err); got != versionProbeTransient {
		t.Fatalf("ETXTBSY = %d, want transient", got)
	}
	if got := classifyVersionProbeError(context.Background(), &os.PathError{Op: "fork/exec", Err: syscall.ENOEXEC}); got != versionProbeFailed {
		t.Fatalf("ENOEXEC = %d, want a stable failure", got)
	}
}
