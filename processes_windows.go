//go:build windows
// +build windows

// File: processes_windows.go
// -----------------------------------------------------------------------------
// Windows OS process scanner. Queries the local process table for processes
// that match the orphan-scanner allowlist (claude/codex/agy/grok). Used by
// orphanScanner.go to identify candidate orphans, and — through the CHECKED
// variants — by the Antigravity utilization index, which must tell "nothing is
// running" from "the table could not be read".
//
// Data source: Microsoft deprecated WMIC starting with Windows 11 24H2 and it
// may be missing entirely on those systems. We prefer PowerShell's Get-CimInstance
// (the modern replacement) and fall back to `wmic` only when Get-CimInstance
// is unavailable. Both return equivalent data. Selection happens once at first
// use and is cached for the process lifetime. The parsing and the empty-vs-
// failed rules live in processes_parse.go.
//
// No third-party Go dependencies — uses the stdlib plus tools shipped with
// Windows.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// scanBackend selects how we query the Windows process table. Decided lazily
// on first call to ScanCLIProcesses and cached for the remainder of the run.
type scanBackend int

const (
	backendUnknown scanBackend = iota
	backendPowerShell
	backendWMIC
	backendNone
)

var (
	scanBackendValue atomic.Int32 // stores a scanBackend; atomic for lock-free reads
	scanBackendOnce  sync.Once
)

const (
	// cliProcessFilter / cliProcessWMICWhere are the orphan allowlist.
	cliProcessFilter    = `Name='claude.exe' OR Name='codex.exe' OR Name='agy.exe' OR Name='grok.exe'`
	cliProcessWMICWhere = `name="claude.exe" or name="codex.exe" or name="agy.exe" or name="grok.exe"`
	// ancestryProcessFilter / ancestryProcessWMICWhere are what a wrapped
	// Antigravity run's tree can hold: the PowerShell / cmd intermediates and
	// agy itself.
	ancestryProcessFilter    = `Name='powershell.exe' OR Name='pwsh.exe' OR Name='cmd.exe' OR Name='agy.exe'`
	ancestryProcessWMICWhere = `name="powershell.exe" or name="pwsh.exe" or name="cmd.exe" or name="agy.exe"`
	// ancestryMaxDepth: the wrapper's child, a file-mode launcher's grandchild,
	// and one more level of shell.
	ancestryMaxDepth = 3
)

// processScanTimeout bounds one scan query. Get-CimInstance answers in about a
// second, but a wedged WMI/CIM provider can hang indefinitely, and callers —
// the Antigravity wrapper resolver most of all — wait on the result inline.
// A timed-out scan is a failed scan: ok=false, never an empty process table.
const processScanTimeout = 20 * time.Second

// runProcessScanCommand runs one scan command and returns both streams. A seam
// so tests can drive the checked-scan rules without the real process table.
var runProcessScanCommand = func(name string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), processScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Report the timeout itself: a killed query can leave partial output
		// on stdout, which the empty-vs-failed rules would otherwise read as
		// a valid (possibly empty) table.
		return nil, errOut.Bytes(), fmt.Errorf("process scan timed out after %s: %w", processScanTimeout, ctxErr)
	}
	return out.Bytes(), errOut.Bytes(), err
}

// ScanCLIProcesses returns all currently-running processes whose image name
// matches the orphan allowlist. Returns an empty slice on error so the caller
// can treat it as a no-op rather than crashing.
func ScanCLIProcesses() []ProcessInfo {
	procs, _ := ScanCLIProcessesChecked()
	return procs
}

// ScanCLIProcessesChecked is ScanCLIProcesses with ok=false for a scan that
// failed (or has no backend), so a caller can tell it from an empty table.
func ScanCLIProcessesChecked() ([]ProcessInfo, bool) {
	return scanProcessesChecked(selectScanBackend(), cliProcessFilter, cliProcessWMICWhere)
}

// ScanProcessAncestryChecked returns rootPID's descendants within
// ancestryMaxDepth levels, intermediates (powershell, pwsh, cmd) included,
// nearest first. Used only by the Antigravity wrapper resolver: the allowlisted
// scan above lists only the CLIs, so a grandchild could never be linked to its
// wrapper through it.
func ScanProcessAncestryChecked(rootPID int) ([]ProcessInfo, bool) {
	if rootPID <= 0 {
		return nil, false
	}
	procs, ok := scanProcessesChecked(selectScanBackend(), ancestryProcessFilter, ancestryProcessWMICWhere)
	if !ok {
		return nil, false
	}
	return filterProcessAncestry(procs, rootPID, ancestryMaxDepth), true
}

// scanProcessesChecked runs one query on the given backend.
func scanProcessesChecked(backend scanBackend, filter, wmicWhere string) ([]ProcessInfo, bool) {
	switch backend {
	case backendPowerShell:
		stdout, _, err := runProcessScanCommand("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			checkedPowerShellScanScript(filter))
		procs, ok := interpretPowerShellScan(stdout, err)
		if !ok {
			// Log once per scan — helps diagnose Win11 24H2 rollouts breaking
			// the backend assumption silently.
			fmt.Printf("%s[orphan-scanner] PowerShell scan failed: %v%s\n", colorRed, err, colorReset)
		}
		return procs, ok
	case backendWMIC:
		stdout, stderr, err := runProcessScanCommand("wmic", "process", "where", wmicWhere,
			"get", "ProcessId,ParentProcessId,Name,CreationDate", "/format:csv")
		procs, ok := interpretWMICScan(stdout, stderr, err)
		if !ok {
			fmt.Printf("%s[orphan-scanner] wmic scan failed: %v%s\n", colorRed, err, colorReset)
		}
		return procs, ok
	default:
		return nil, false
	}
}

// selectScanBackend probes for an available query mechanism on first call and
// caches the result. Prefers PowerShell because WMIC is deprecated. If neither
// tool works the scanner becomes a no-op (we log this once on selection).
func selectScanBackend() scanBackend {
	scanBackendOnce.Do(func() {
		// Prefer PowerShell (pwsh preferred, but any powershell works).
		if _, err := exec.LookPath("powershell.exe"); err == nil {
			scanBackendValue.Store(int32(backendPowerShell))
			fmt.Printf("%s[orphan-scanner] Using PowerShell Get-CimInstance as process scan backend%s\n",
				colorCyan, colorReset)
			return
		}
		if _, err := exec.LookPath("wmic"); err == nil {
			scanBackendValue.Store(int32(backendWMIC))
			fmt.Printf("%s[orphan-scanner] PowerShell not available — falling back to deprecated wmic%s\n",
				colorYellow, colorReset)
			return
		}
		scanBackendValue.Store(int32(backendNone))
		fmt.Printf("%s[orphan-scanner] Neither PowerShell nor wmic available — scanner disabled%s\n",
			colorRed, colorReset)
	})
	return scanBackend(scanBackendValue.Load())
}

// KillProcessTree force-kills the given PID and all its descendants using the
// same `taskkill /F /T /PID` pattern used in cleanup_windows.go.
func KillProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
	hideWindow(cmd)
	return cmd.Run()
}
