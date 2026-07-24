package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Test that the rotating writer bounds total on-disk size — the whole point of
// the feature is that logging can never eat a user's disk.
func TestRotatingWriterBoundsDiskUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	w, err := newRotatingWriter(path)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}

	// Write ~10x the total allowed budget to force many rotations.
	budget := int64(logMaxSizeBytes) * int64(logMaxBackups+1)
	line := make([]byte, 64*1024) // 64 KB per write
	for i := range line {
		line[i] = 'x'
	}
	var written int64
	for written < budget*10 {
		n, _ := w.Write(line)
		written += int64(n)
	}
	w.mu.Lock()
	if w.f != nil {
		_ = w.f.Close()
	}
	w.mu.Unlock()

	// Sum the active file + all rotated backups; must stay within the bound
	// (allow one extra write's slack since a file rotates AFTER crossing the cap).
	var total int64
	names := []string{path}
	for i := 1; i <= logMaxBackups; i++ {
		names = append(names, fmt.Sprintf("%s.%d", path, i))
	}
	fileCount := 0
	for _, n := range names {
		info, statErr := os.Stat(n)
		if statErr != nil {
			continue
		}
		fileCount++
		total += info.Size()
	}
	maxAllowed := budget + int64(len(line)) // +1 write of slack
	if total > maxAllowed {
		t.Fatalf("total log size %d exceeded bound %d (files=%d)", total, maxAllowed, fileCount)
	}
	if fileCount > logMaxBackups+1 {
		t.Fatalf("kept %d log files, expected at most %d", fileCount, logMaxBackups+1)
	}
	// A file beyond the last backup must NOT exist (oldest is deleted on rotate).
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, logMaxBackups+1)); err == nil {
		t.Fatalf("backup beyond logMaxBackups was not deleted")
	}
}

// teeSink must never return an error, or the io.Copy pump feeding it could stop
// draining the pipe and block every fmt.Print in the agent.
func TestTeeSinkNeverErrors(t *testing.T) {
	// nil consoleRef + nil file: still must not error.
	s := teeSink{}
	if n, err := s.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("teeSink.Write = (%d, %v), want (5, nil)", n, err)
	}
}

// Regression for the P2: allocateConsole() allocates a Windows console AFTER the
// tee is installed and used to overwrite os.Stdout, bypassing it. The tee now
// keeps a swappable console sink; setLogTeeConsole re-points it so output keeps
// reaching the (new) console while os.Stdout stays the tee pipe.
func TestLogTeeConsoleSwap(t *testing.T) {
	c1, err := os.CreateTemp(t.TempDir(), "console1")
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := os.CreateTemp(t.TempDir(), "console2")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	teeActive.Store(true)
	teeOutConsole.Store(c1)
	defer teeActive.Store(false)

	sink := teeSink{consoleRef: &teeOutConsole}
	_, _ = sink.Write([]byte("before\n"))

	// This is what allocateConsole() does on "Show Console" / non-prod startup.
	if !setLogTeeConsole(c2, c2) {
		t.Fatal("setLogTeeConsole returned false while the tee is active")
	}
	_, _ = sink.Write([]byte("after\n"))

	if b, _ := os.ReadFile(c1.Name()); string(b) != "before\n" {
		t.Fatalf("old console = %q, want %q", b, "before\n")
	}
	if b, _ := os.ReadFile(c2.Name()); string(b) != "after\n" {
		t.Fatalf("new console = %q, want %q (output did not follow the swap)", b, "after\n")
	}

	// Inactive tee → caller must fall back to reassigning os.Stdout directly.
	teeActive.Store(false)
	if setLogTeeConsole(c1, c1) {
		t.Fatal("setLogTeeConsole returned true while the tee is inactive")
	}
}
