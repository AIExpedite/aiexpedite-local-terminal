package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Cross-platform stub `opencode` binary for the native-manager tests.
//
// The existing CLI-agent suites shim a fake binary with a `#!/bin/sh` script,
// which means every behavioural test SKIPS on Windows — and Windows is the
// platform where the process-tree kill, the CreateProcess argv ceiling and the
// console-window hiding actually differ. The OpenCode driver's riskiest code
// (per-event streaming, cancel-mid-turn, replay recovery) would then have zero
// executed coverage on half the supported platforms.
//
// So the stub is a tiny Go program compiled once per test binary instead. It
// runs everywhere `go test` does, needs no shell, and its behaviour is driven
// entirely by environment variables so one compiled artifact serves every case.
//
// Env contract (all optional):
//
//	OPENCODE_STUB_VERSION    version string printed for `--version`
//	OPENCODE_STUB_STDOUT     literal stdout, "\n"-escaped as \n
//	OPENCODE_STUB_STDERR     literal stderr
//	OPENCODE_STUB_EXIT       exit code (default 0)
//	OPENCODE_STUB_ARGV_LOG   append the received argv to this file
//	OPENCODE_STUB_STDIN_LOG  write everything read from stdin to this file
//	OPENCODE_STUB_SLEEP_MS   sleep this long before exiting (cancel/timeout)
//	OPENCODE_STUB_RUN_LOG    append one line per invocation (replay counting)
//	OPENCODE_STUB_FAIL_FIRST when set, invocation #1 uses …_FIRST_STDOUT/EXIT
const openCodeStubSource = `package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(env("OPENCODE_STUB_VERSION", "0.9.0"))
		return
	}

	runs := 0
	if p := os.Getenv("OPENCODE_STUB_RUN_LOG"); p != "" {
		existing, _ := os.ReadFile(p)
		runs = strings.Count(string(existing), "\n")
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, "run")
			f.Close()
		}
		runs++
	}

	if p := os.Getenv("OPENCODE_STUB_ARGV_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
			f.Close()
		}
	}

	// Always drain stdin: the parent hands us a real file, and leaving it
	// unread would not block, but the capture is what several tests assert on.
	data, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("OPENCODE_STUB_STDIN_LOG"); p != "" {
		os.WriteFile(p, data, 0o600)
	}

	if ms := os.Getenv("OPENCODE_STUB_SLEEP_MS"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil {
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
	}

	stdout := unescape(os.Getenv("OPENCODE_STUB_STDOUT"))
	exitCode, _ := strconv.Atoi(env("OPENCODE_STUB_EXIT", "0"))
	if os.Getenv("OPENCODE_STUB_FAIL_FIRST") != "" && runs == 1 {
		stdout = unescape(os.Getenv("OPENCODE_STUB_FIRST_STDOUT"))
		exitCode, _ = strconv.Atoi(env("OPENCODE_STUB_FIRST_EXIT", "1"))
	}
	if stdout != "" {
		os.Stdout.WriteString(stdout)
	}
	if s := unescape(os.Getenv("OPENCODE_STUB_STDERR")); s != "" {
		os.Stderr.WriteString(s)
	}
	// A huge single frame, used to exercise the frame-cap path without
	// embedding megabytes of literal in the test source.
	if n, err := strconv.Atoi(os.Getenv("OPENCODE_STUB_HUGE_FRAME_BYTES")); err == nil && n > 0 {
		os.Stdout.WriteString("{\"type\":\"text\",\"text\":\"")
		chunk := strings.Repeat("a", 64*1024)
		for written := 0; written < n; written += len(chunk) {
			os.Stdout.WriteString(chunk)
		}
		os.Stdout.WriteString("\"}\n")
	}
	os.Exit(exitCode)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func unescape(s string) string {
	return strings.ReplaceAll(s, "\\n", "\n")
}
`

var (
	openCodeStubOnce sync.Once
	openCodeStubPath string
	openCodeStubErr  error
)

// buildOpenCodeStub compiles the stub once per test binary and returns its
// path. Subsequent callers reuse the artifact.
func buildOpenCodeStub(t *testing.T) string {
	t.Helper()
	openCodeStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "opencode-stub-*")
		if err != nil {
			openCodeStubErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(openCodeStubSource), 0o600); err != nil {
			openCodeStubErr = err
			return
		}
		// A module file keeps `go build` from walking up into this repo's
		// module and trying to resolve its dependencies.
		if err := os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module opencodestub\n\ngo 1.21\n"), 0o600); err != nil {
			openCodeStubErr = err
			return
		}
		out := filepath.Join(dir, "opencode")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = dir
		// GOFLAGS from the parent build can carry -mod=vendor, which fails in a
		// module with no vendor dir.
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GO111MODULE=on")
		if combined, err := cmd.CombinedOutput(); err != nil {
			openCodeStubErr = err
			openCodeStubPath = strings.TrimSpace(string(combined))
			return
		}
		openCodeStubPath = out
	})
	if openCodeStubErr != nil {
		t.Skipf("could not build the opencode stub (%v): %s", openCodeStubErr, openCodeStubPath)
	}
	return openCodeStubPath
}

// installOpenCodeStub puts the compiled stub first on PATH under the name
// `opencode` and resets every probe cache so the test sees it. Returns the
// directory it was installed into.
func installOpenCodeStub(t *testing.T) string {
	t.Helper()
	stub := buildOpenCodeStub(t)

	binDir := t.TempDir()
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dest := filepath.Join(binDir, name)
	data, err := os.ReadFile(stub)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		t.Fatalf("install stub: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Every probe result is cached; a test that installs a new stub must not
	// read the previous test's answer.
	resetOpenCodeCapabilityCache()
	resetOpenCodeReadinessCache()
	resetVersionProbeCache()
	t.Cleanup(func() {
		resetOpenCodeCapabilityCache()
		resetOpenCodeReadinessCache()
		resetVersionProbeCache()
	})
	return binDir
}
