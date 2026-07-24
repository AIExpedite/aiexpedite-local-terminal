package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// The agent runs as a background tray app with no persisted console, so its
// diagnostic stdout/stderr (the `[codex-appserver]` session-teardown messages,
// stale-cleanup notices, publish-stall/oversize-frame fatals, etc.) vanish the
// moment they're printed. That made a whole class of "codex session died and the
// run re-paused" incidents undiagnosable after the fact. setupLogTee mirrors
// stdout+stderr to a size-BOUNDED rotating file so those logs are recoverable.
//
// Bounded on purpose: total on-disk log usage can never exceed
// logMaxSizeBytes * (logMaxBackups + 1) — we must never let logging eat a user's
// disk. Fail-open throughout: any setup or write error silently disables file
// logging rather than blocking the agent or losing console output.

const (
	// logMaxSizeBytes is the size at which the active log file rotates.
	logMaxSizeBytes = 5 * 1024 * 1024 // 5 MB
	// logMaxBackups is how many rotated files are kept (agent.log + .1 .. .N).
	// Total on-disk bound = logMaxSizeBytes * (logMaxBackups + 1) = 20 MB here.
	logMaxBackups = 3
	// logFileName is the active log file under <configDir>/logs/.
	logFileName = "agent.log"
)

// rotatingWriter is a minimal, dependency-free, size-bounded log writer. When a
// write would push the active file past logMaxSizeBytes it rotates
// (agent.log → agent.log.1, shifting older backups up and deleting the oldest),
// so total disk use stays bounded. All methods are fail-open: a write/rotate
// error disables further file writes instead of surfacing an error to callers
// (callers are the stdout/stderr tee — an error there would stop draining the
// pipe and could block the whole agent).
type rotatingWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func newRotatingWriter(path string) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if info, statErr := f.Stat(); statErr == nil {
		size = info.Size()
	}
	return &rotatingWriter{path: path, f: f, size: size}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return len(p), nil // logging disabled after a prior error — swallow
	}
	if w.size+int64(len(p)) > logMaxSizeBytes {
		w.rotate()
	}
	if w.f == nil {
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		// Fail-open: stop writing to disk but never propagate — the tee must
		// keep draining the pipe so the agent's stdout writes don't block.
		_ = w.f.Close()
		w.f = nil
	}
	return len(p), nil
}

// rotate closes the active file, deletes the oldest backup, shifts the rest up
// (.i → .i+1), renames the active file to .1, and opens a fresh active file.
// Caller holds the mutex. Best-effort: on reopen failure it leaves w.f nil
// (logging disabled) rather than erroring.
func (w *rotatingWriter) rotate() {
	_ = w.f.Close()
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, logMaxBackups))
	for i := logMaxBackups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Rename(w.path, w.path+".1")
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		w.f = nil
		w.size = 0
		return
	}
	w.f = f
	w.size = 0
}

// The log tee mirrors os.Stdout/os.Stderr to agent.log AND to a console target.
// The console target is SWAPPABLE at runtime because allocateConsole()
// (tray_windows.go) allocates a Windows console on demand and would otherwise
// overwrite os.Stdout/os.Stderr with CONOUT$, bypassing the tee. Instead the tee
// keeps os.Stdout pointed at its pipe for the whole process lifetime and just
// re-points these console sinks — so diagnostics keep reaching both the console
// and the file whether the console is allocated at startup (non-prod) or later
// via "Show Console" (prod). Loaded lock-free on the hot Write path.
var (
	teeOutConsole atomic.Pointer[os.File]
	teeErrConsole atomic.Pointer[os.File]
	teeActive     atomic.Bool
)

// teeSink writes to the log file and (best-effort) the current console target.
// It NEVER returns an error, so the io.Copy pump feeding it can never stop
// draining the pipe — otherwise a broken/absent console (a tray app often has
// none) would fill the pipe buffer and block every fmt.Print in the agent.
type teeSink struct {
	consoleRef *atomic.Pointer[os.File]
	file       io.Writer
}

func (t teeSink) Write(p []byte) (int, error) {
	if t.file != nil {
		_, _ = t.file.Write(p)
	}
	if t.consoleRef != nil {
		if c := t.consoleRef.Load(); c != nil {
			_, _ = c.Write(p) // best-effort; a dead console must not stop us
		}
	}
	return len(p), nil
}

// setLogTeeConsole re-points the tee's console mirror at out/err — called by
// allocateConsole() after it opens a fresh Windows console (CONOUT$) so console
// output keeps flowing to BOTH the console and agent.log. Returns false when the
// tee isn't active (setup failed / logging disabled) so the caller can fall back
// to reassigning os.Stdout/os.Stderr directly.
func setLogTeeConsole(out, err *os.File) bool {
	if !teeActive.Load() {
		return false
	}
	teeOutConsole.Store(out)
	teeErrConsole.Store(err)
	return true
}

// agentLogPath returns <configDir>/logs/agent.log, alongside security.log.
func agentLogPath() string {
	return filepath.Join(GetConfigDir(), "logs", logFileName)
}

// setupLogTee redirects os.Stdout/os.Stderr through pipes that fan out to the
// original console AND the rotating log file. Returns a cleanup func that
// restores the originals and flushes. Fail-open: on any setup error it leaves
// stdout/stderr untouched and returns a no-op cleanup — file logging must never
// be the reason the agent fails to start.
//
// Call this AFTER the fast early-return arg handlers (statusline-hook,
// --version, ...) so those paths keep a clean, untouched stdout.
func setupLogTee() func() {
	path := agentLogPath()
	rw, err := newRotatingWriter(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[log] file logging disabled (%v)\n", err)
		return func() {}
	}

	origOut, origErr := os.Stdout, os.Stderr
	// Seed the swappable console sinks with the current console (the real
	// console, or the GUI's NUL-ish handle). allocateConsole() re-points these
	// later via setLogTeeConsole without touching os.Stdout.
	teeOutConsole.Store(origOut)
	teeErrConsole.Store(origErr)
	var wg sync.WaitGroup

	// pump replaces *dst (os.Stdout or os.Stderr) with the write end of a pipe
	// and copies the read end into (current console)+file. Returns a closer for
	// the write end. On os.Pipe failure it leaves dst as-is (that stream isn't
	// teed). os.Stdout stays this pipe for the whole process lifetime — the
	// console target is swapped via the atomic ref, never by reassigning it.
	pump := func(dst **os.File, consoleRef *atomic.Pointer[os.File]) func() {
		r, w, perr := os.Pipe()
		if perr != nil {
			return func() {}
		}
		*dst = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(teeSink{consoleRef: consoleRef, file: rw}, r)
		}()
		return func() { _ = w.Close() }
	}

	closeOut := pump(&os.Stdout, &teeOutConsole)
	closeErr := pump(&os.Stderr, &teeErrConsole)
	teeActive.Store(true)

	fmt.Printf("[log] mirroring stdout/stderr → %s (rotating, max %d MB × %d files)\n",
		path, logMaxSizeBytes/(1024*1024), logMaxBackups+1)

	return func() {
		teeActive.Store(false)
		os.Stdout, os.Stderr = origOut, origErr
		closeOut()
		closeErr()
		wg.Wait()
		rw.mu.Lock()
		if rw.f != nil {
			_ = rw.f.Close()
			rw.f = nil
		}
		rw.mu.Unlock()
	}
}
