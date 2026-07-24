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
	// nil console + nil file: still must not error.
	s := teeSink{}
	if n, err := s.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("teeSink.Write = (%d, %v), want (5, nil)", n, err)
	}
}
