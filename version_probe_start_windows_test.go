//go:build windows

package main

import (
	"context"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows refuses to open an executable while real-time AV is still scanning a
// freshly written copy, or while an installer holds it: transient.
func TestClassifyVersionProbeError_HeldFileIsTransient(t *testing.T) {
	for _, errno := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION} {
		err := &os.PathError{Op: "fork/exec", Path: `C:\cli.exe`, Err: errno}
		if got := classifyVersionProbeError(context.Background(), err); got != versionProbeTransient {
			t.Fatalf("%v = %d, want transient", errno, got)
		}
	}
	if got := classifyVersionProbeError(context.Background(), &os.PathError{Op: "fork/exec", Err: windows.ERROR_BAD_EXE_FORMAT}); got != versionProbeFailed {
		t.Fatalf("bad exe format = %d, want a stable failure", got)
	}
}
