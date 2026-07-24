package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// agent.log holds process stdout/stderr (including command display/debug) and
// must be owner-only — same posture as security.log (0o700 dir / 0o600 file).
func TestRotatingWriterPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced the same way on Windows")
	}
	dir := t.TempDir()
	// Nested path so MkdirAll creates the logs directory with our mode.
	logsDir := filepath.Join(dir, "logs")
	path := filepath.Join(logsDir, "agent.log")
	w, err := newRotatingWriter(path)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	w.mu.Lock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.mu.Unlock()

	if info, err := os.Stat(logsDir); err != nil {
		t.Fatalf("stat logs dir: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("logs dir mode = %04o, want 0700", mode)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat agent.log: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("agent.log mode = %04o, want 0600", mode)
	}

	// Reopen a pre-existing world-readable file and ensure we tighten it.
	worldPath := filepath.Join(dir, "world.log")
	if err := os.WriteFile(worldPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w2, err := newRotatingWriter(worldPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	w2.mu.Lock()
	if w2.f != nil {
		_ = w2.f.Close()
		w2.f = nil
	}
	w2.mu.Unlock()
	if info, err := os.Stat(worldPath); err != nil {
		t.Fatalf("stat world.log: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("reopened log mode = %04o, want 0600 (chmod on open)", mode)
	}
}

// Upgrade path: older builds left logs/ at 0755 and rotated backups at 0644.
// Startup must tighten those too — MkdirAll/OpenFile only apply modes on create.
func TestRotatingWriterTightensExistingDirAndBackups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced the same way on Windows")
	}
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logsDir, "agent.log")
	if err := os.WriteFile(path, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= logMaxBackups; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte("backup\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w, err := newRotatingWriter(path)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	w.mu.Lock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.mu.Unlock()

	if info, err := os.Stat(logsDir); err != nil {
		t.Fatalf("stat logs dir: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("existing logs dir mode = %04o, want 0700 after reopen", mode)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat agent.log: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("agent.log mode = %04o, want 0600", mode)
	}
	for i := 1; i <= logMaxBackups; i++ {
		bp := fmt.Sprintf("%s.%d", path, i)
		if info, err := os.Stat(bp); err != nil {
			t.Fatalf("stat %s: %v", bp, err)
		} else if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", bp, mode)
		}
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
