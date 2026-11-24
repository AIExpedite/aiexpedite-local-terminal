package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// logLocalOnly is the one writer for diagnostics that must never leave the
// computer: it lands in agent.log and NOT on the console mirror, which in a
// terminal-managed run is published back to the server as command output.
func TestLogLocalOnlyWritesTheFileAndNeverTheConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", logFileName)
	rw, err := newRotatingWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rw.f.Close() })

	consoleR, consoleW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer consoleR.Close()

	prevOut, prevErr := teeOutConsole.Load(), teeErrConsole.Load()
	teeOutConsole.Store(consoleW)
	teeErrConsole.Store(consoleW)
	teeFile.Store(rw)
	teeActive.Store(true)
	t.Cleanup(func() {
		teeActive.Store(false)
		teeFile.Store(nil)
		teeOutConsole.Store(prevOut)
		teeErrConsole.Store(prevErr)
	})

	const secret = `C:\Users\someone\checkouts\private-repo`
	logLocalOnly("root %q is inside %q", secret, secret)
	_ = consoleW.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, "private-repo") || !strings.HasSuffix(got, "\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("agent.log = %q, want exactly one line naming the path", got)
	}
	console, _ := io.ReadAll(consoleR)
	if len(console) != 0 {
		t.Fatalf("logLocalOnly reached the console: %q", console)
	}
}

// With no tee (go test, or file logging disabled) logLocalOnly is a silent no-op.
func TestLogLocalOnlyIsANoOpWithoutATee(t *testing.T) {
	prev := teeFile.Load()
	teeFile.Store(nil)
	t.Cleanup(func() { teeFile.Store(prev) })

	out := captureStdoutStderr(t, func() {
		logLocalOnly("nothing should be written: %s", "x")
	})
	if out != "" {
		t.Fatalf("logLocalOnly wrote %q with the tee inactive", out)
	}
}

// captureStdoutStderr redirects BOTH os.Stdout and os.Stderr for the duration
// of fn and returns everything written to either: what a terminal-managed run
// would publish as command output.
func captureStdoutStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(reader)
		done <- string(b)
	}()

	fn()

	os.Stdout, os.Stderr = origOut, origErr
	_ = writer.Close()
	captured := <-done
	_ = reader.Close()
	return captured
}
