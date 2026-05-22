//go:build windows
// +build windows

// File: processes_windows.go
// -----------------------------------------------------------------------------
// Windows OS process scanner. Queries the local process table for processes
// that match the orphan-scanner allowlist (claude/codex/gemini/agy). Used by
// orphanScanner.go to identify candidate orphans.
//
// Data source: Microsoft deprecated WMIC starting with Windows 11 24H2 and it
// may be missing entirely on those systems. We prefer PowerShell's Get-CimInstance
// (the modern replacement) and fall back to `wmic` only when Get-CimInstance
// is unavailable. Both return equivalent data. Selection happens once at first
// use and is cached for the process lifetime.
//
// No third-party Go dependencies — uses the stdlib plus tools shipped with
// Windows.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ProcessInfo describes a running OS process at scan time.
type ProcessInfo struct {
	PID       int
	ParentPID int
	Name      string // lowercased, e.g. "claude.exe"
	StartTime time.Time
}

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

// ScanCLIProcesses returns all currently-running processes whose image name
// matches the orphan allowlist (claude / codex / gemini). Returns an empty
// slice on error so the caller can treat it as a no-op rather than crashing.
func ScanCLIProcesses() []ProcessInfo {
	backend := selectScanBackend()
	switch backend {
	case backendPowerShell:
		return scanViaPowerShell()
	case backendWMIC:
		return scanViaWMIC()
	default:
		return nil
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

// scanViaPowerShell queries the process table using Get-CimInstance Win32_Process,
// the modern WMI-over-PS equivalent of `wmic process`. Output is CSV-formatted
// so the parser can be shared with the WMIC path. We emit CreationDate in UTC
// (ToUniversalTime + yyyyMMddHHmmss) so the Go parser can anchor unambiguously
// and doesn't need to assume time.Local matches the system timezone.
func scanViaPowerShell() []ProcessInfo {
	script := `$ErrorActionPreference = 'SilentlyContinue';` +
		` Get-CimInstance Win32_Process -Filter "Name='claude.exe' OR Name='codex.exe' OR Name='gemini.exe' OR Name='agy.exe'"` +
		` | Select-Object @{Name='Name';Expression={$_.Name}},` +
		` @{Name='ProcessId';Expression={$_.ProcessId}},` +
		` @{Name='ParentProcessId';Expression={$_.ParentProcessId}},` +
		` @{Name='CreationDate';Expression={if ($_.CreationDate) { $_.CreationDate.ToUniversalTime().ToString('yyyyMMddHHmmss') } else { '' }}}` +
		` | ConvertTo-Csv -NoTypeInformation`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// Log once per scan — helps diagnose Win11 24H2 rollouts breaking the
		// backend assumption silently.
		fmt.Printf("%s[orphan-scanner] PowerShell scan failed: %v%s\n", colorRed, err, colorReset)
		return nil
	}
	return parseCSVOutput(&out, parsePowerShellDate)
}

// scanViaWMIC queries via the legacy `wmic` tool. Kept as a fallback for Windows
// versions where PowerShell is unavailable.
func scanViaWMIC() []ProcessInfo {
	whereClause := `name="claude.exe" or name="codex.exe" or name="gemini.exe" or name="agy.exe"`
	args := []string{
		"process",
		"where", whereClause,
		"get", "ProcessId,ParentProcessId,Name,CreationDate",
		"/format:csv",
	}
	cmd := exec.Command("wmic", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	// wmic exits non-zero when the WHERE clause matches nothing — that's a
	// normal "no orphans" case, not an error. If stdout is empty AND the run
	// failed, log once so a Win11 24H2 upgrade that removes wmic is visible.
	runErr := cmd.Run()
	if runErr != nil && out.Len() == 0 {
		fmt.Printf("%s[orphan-scanner] wmic invocation failed and produced no output: %v%s\n",
			colorRed, runErr, colorReset)
	}
	return parseCSVOutput(&out, parseWMIDate)
}

// parseCSVOutput parses CSV rows (header + data) and extracts a ProcessInfo per
// valid row. dateParser converts the CreationDate field to time.Time.
// Returns an empty slice (never nil) so callers can treat it uniformly.
func parseCSVOutput(r io.Reader, dateParser func(string) time.Time) []ProcessInfo {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	var header []string
	out := make([]ProcessInfo, 0, 8)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Malformed row — continue parsing remaining rows. We deliberately
			// don't log here to avoid flooding on a consistently broken backend;
			// the per-scan empty-result diagnostic in the scanner covers that.
			continue
		}
		if len(row) < 2 {
			continue
		}
		if header == nil {
			lowered := make([]string, len(row))
			for i, v := range row {
				lowered[i] = strings.ToLower(strings.TrimSpace(v))
			}
			if contains(lowered, "name") && contains(lowered, "processid") {
				header = lowered
			}
			continue
		}

		idxName := indexOf(header, "name")
		idxPID := indexOf(header, "processid")
		idxParent := indexOf(header, "parentprocessid")
		idxCreated := indexOf(header, "creationdate")
		if idxName < 0 || idxPID < 0 {
			continue
		}

		name := strings.ToLower(strings.TrimSpace(getField(row, idxName)))
		if name == "" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(getField(row, idxPID)))
		if err != nil || pid <= 0 {
			continue
		}
		parent := 0
		if idxParent >= 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(getField(row, idxParent))); err == nil && v > 0 {
				parent = v
			}
		}
		startTime := time.Time{}
		if idxCreated >= 0 && dateParser != nil {
			startTime = dateParser(strings.TrimSpace(getField(row, idxCreated)))
		}

		out = append(out, ProcessInfo{
			PID:       pid,
			ParentPID: parent,
			Name:      name,
			StartTime: startTime,
		})
	}
	return out
}

// KillProcessTree force-kills the given PID and all its descendants using the
// same `taskkill /F /T /PID` pattern used in cleanup_windows.go.
func KillProcessTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

// ─────────────────────── date parsers ───────────────────────

// parseWMIDate parses WMI's CIM_DATETIME format "yyyymmddHHMMSS.mmmmmm±UUU"
// where ±UUU is minutes offset from UTC. We respect the embedded offset rather
// than assuming the Go process's local timezone matches the WMI source (they
// can differ when TZ env var overrides the default).
func parseWMIDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	// Start with the calendar portion in UTC; we'll apply the offset afterwards.
	t, err := time.ParseInLocation("20060102150405", s[:14], time.UTC)
	if err != nil {
		return time.Time{}
	}
	// Look for the ±UUU offset after the fractional seconds.
	if idx := strings.IndexAny(s[14:], "+-"); idx >= 0 {
		signAndOffset := s[14+idx:]
		if len(signAndOffset) >= 4 {
			sign := signAndOffset[0]
			if mins, err := strconv.Atoi(signAndOffset[1:4]); err == nil {
				offset := time.Duration(mins) * time.Minute
				if sign == '+' {
					t = t.Add(-offset)
				} else {
					t = t.Add(offset)
				}
			}
		}
	}
	return t
}

// parsePowerShellDate parses the yyyyMMddHHmmss UTC format emitted by our
// PowerShell Select-Object expression (see scanViaPowerShell — we use
// ToUniversalTime explicitly so the wire format is unambiguous).
func parsePowerShellDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102150405", s[:14], time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ─────────────────────── helpers ───────────────────────

func contains(haystack []string, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

func getField(row []string, idx int) string {
	if idx < 0 || idx >= len(row) {
		return ""
	}
	return row[idx]
}
