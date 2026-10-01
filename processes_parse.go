// File: processes_parse.go
// -----------------------------------------------------------------------------
// Platform-neutral half of the process-table scan: the CSV parser both Windows
// backends share, the date parsers, the rules that tell an EMPTY scan from a
// FAILED one (processes_windows.go runs the commands), and the ancestry filter
// the Antigravity wrapper resolver uses. No build tag, so the rules are tested
// on every OS.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"
)

// ProcessInfo describes a running OS process at scan time.
type ProcessInfo struct {
	PID       int
	ParentPID int
	Name      string // lowercased, e.g. "claude.exe"
	StartTime time.Time
}

// processScanOKTrailer is the line the PowerShell scan prints only after the
// query succeeded. Without it an empty result is indistinguishable from a
// query that failed (the orphan scanner could afford that; "no process is
// still running" cannot).
const processScanOKTrailer = "AIX_SCAN_OK"

// wmicNoInstances is WMIC's answer to a WHERE clause that matched nothing. It
// exits nonzero with it, which is still a valid empty result.
const wmicNoInstances = "No Instance(s) Available."

// checkedPowerShellScanScript wraps a Get-CimInstance query so a failure exits
// 3 and only a completed query prints the trailer.
func checkedPowerShellScanScript(filter string) string {
	return `try { Get-CimInstance Win32_Process -Filter "` + filter + `" -ErrorAction Stop` +
		` | Select-Object @{Name='Name';Expression={$_.Name}},` +
		` @{Name='ProcessId';Expression={$_.ProcessId}},` +
		` @{Name='ParentProcessId';Expression={$_.ParentProcessId}},` +
		` @{Name='CreationDate';Expression={if ($_.CreationDate) { $_.CreationDate.ToUniversalTime().ToString('yyyyMMddHHmmss.fffffff') } else { '' }}}` +
		` | ConvertTo-Csv -NoTypeInformation; '` + processScanOKTrailer + `' } catch { exit 3 }`
}

// interpretPowerShellScan: ok requires a clean exit AND the trailer line.
func interpretPowerShellScan(stdout []byte, runErr error) ([]ProcessInfo, bool) {
	if runErr != nil || !hasProcessScanTrailer(stdout) {
		return nil, false
	}
	return parseCSVOutput(bytes.NewReader(stdout), parsePowerShellDate), true
}

func hasProcessScanTrailer(stdout []byte) bool {
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == processScanOKTrailer {
			return true
		}
	}
	return false
}

// interpretWMICScan: a CSV header, or "No Instance(s) Available." on either
// stream, is a valid (possibly empty) result whatever the exit code; anything
// else is a failed scan.
func interpretWMICScan(stdout, stderr []byte, runErr error) ([]ProcessInfo, bool) {
	if bytes.Contains(stdout, []byte(wmicNoInstances)) || bytes.Contains(stderr, []byte(wmicNoInstances)) {
		return []ProcessInfo{}, true
	}
	if csvHasProcessHeader(stdout) {
		return parseCSVOutput(bytes.NewReader(stdout), parseWMIDate), true
	}
	return nil, false
}

// csvHasProcessHeader reports whether the output carries the scan's CSV header.
func csvHasProcessHeader(out []byte) bool {
	reader := csv.NewReader(bytes.NewReader(out))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return false
		}
		if err != nil {
			continue
		}
		lowered := make([]string, len(row))
		for i, v := range row {
			lowered[i] = strings.ToLower(strings.TrimSpace(v))
		}
		if contains(lowered, "name") && contains(lowered, "processid") {
			return true
		}
	}
}

// filterProcessAncestry keeps the processes reachable from rootPID within
// maxDepth parent links, breadth-first (nearest first). The root itself is not
// returned.
func filterProcessAncestry(procs []ProcessInfo, rootPID, maxDepth int) []ProcessInfo {
	children := map[int][]ProcessInfo{}
	for _, p := range procs {
		if p.PID > 0 && p.PID != p.ParentPID {
			children[p.ParentPID] = append(children[p.ParentPID], p)
		}
	}
	var out []ProcessInfo
	seen := map[int]bool{rootPID: true}
	frontier := []int{rootPID}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, parent := range frontier {
			for _, child := range children[parent] {
				if seen[child.PID] {
					continue
				}
				seen[child.PID] = true
				out = append(out, child)
				next = append(next, child.PID)
			}
		}
		frontier = next
	}
	return out
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
	t = t.Add(parseDateFraction(s[14:]))
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

// parsePowerShellDate parses the yyyyMMddHHmmss.fffffff UTC format emitted by
// our PowerShell Select-Object expression (see checkedPowerShellScanScript — we
// use ToUniversalTime explicitly so the wire format is unambiguous). The
// fraction is optional.
func parsePowerShellDate(s string) time.Time {
	if len(s) < 14 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102150405", s[:14], time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t.Add(parseDateFraction(s[14:]))
}

// parseDateFraction reads the ".ddd…" fractional seconds that follow a date's
// whole seconds; zero when absent. The subsecond part is what tells apart two
// processes created in the same second (the Antigravity wrapper resolver).
func parseDateFraction(s string) time.Duration {
	if len(s) < 2 || s[0] != '.' {
		return 0
	}
	var frac time.Duration
	scale := time.Second
	for i := 1; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		scale /= 10
		frac += time.Duration(s[i]-'0') * scale
	}
	return frac
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
