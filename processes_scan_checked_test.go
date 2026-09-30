package main

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The checked scans must tell an EMPTY process table from a FAILED query: the
// Antigravity index releases a held run only on an ok scan, so a failure read
// as "nothing running" would owe refreshes for runs still going.

func TestInterpretPowerShellScan(t *testing.T) {
	header := "\"Name\",\"ProcessId\",\"ParentProcessId\",\"CreationDate\"\r\n"
	row := "\"agy.exe\",\"4242\",\"100\",\"20260929204356\"\r\n"
	for _, tc := range []struct {
		name   string
		stdout string
		err    error
		ok     bool
		rows   int
	}{
		{"trailer, no rows", processScanOKTrailer + "\r\n", nil, true, 0},
		{"rows and trailer", header + row + processScanOKTrailer + "\r\n", nil, true, 1},
		{"rows without trailer", header + row, nil, false, 0},
		{"nothing at all", "", nil, false, 0},
		{"nonzero exit", processScanOKTrailer + "\r\n", errors.New("exit status 3"), false, 0},
	} {
		procs, ok := interpretPowerShellScan([]byte(tc.stdout), tc.err)
		if ok != tc.ok || len(procs) != tc.rows {
			t.Errorf("%s: ok=%v rows=%d, want ok=%v rows=%d", tc.name, ok, len(procs), tc.ok, tc.rows)
		}
	}
}

func TestInterpretWMICScan(t *testing.T) {
	header := "Node,CreationDate,Name,ParentProcessId,ProcessId\r\n"
	row := "HOST,20260929204356.000000+120,agy.exe,100,4242\r\n"
	exitErr := errors.New("exit status 2147749911")
	for _, tc := range []struct {
		name           string
		stdout, stderr string
		err            error
		ok             bool
		rows           int
	}{
		{"no instances on stderr, nonzero exit", "", wmicNoInstances + "\r\n", exitErr, true, 0},
		{"no instances on stdout", wmicNoInstances + "\r\n", "", nil, true, 0},
		{"header, no rows", header, "", nil, true, 0},
		{"header and a row", header + row, "", nil, true, 1},
		{"missing binary", "", "", exec.ErrNotFound, false, 0},
		{"unknown message", "", "Invalid query\r\n", exitErr, false, 0},
	} {
		procs, ok := interpretWMICScan([]byte(tc.stdout), []byte(tc.stderr), tc.err)
		if ok != tc.ok || len(procs) != tc.rows {
			t.Errorf("%s: ok=%v rows=%d, want ok=%v rows=%d", tc.name, ok, len(procs), tc.ok, tc.rows)
		}
	}
}

// The checked PowerShell query carries the trailer and stops on any error.
func TestCheckedPowerShellScanScript(t *testing.T) {
	script := checkedPowerShellScanScript("Name='agy.exe'")
	for _, want := range []string{"-ErrorAction Stop", "'" + processScanOKTrailer + "'", "catch { exit 3 }", "Name='agy.exe'"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q: %s", want, script)
		}
	}
}

// ScanProcessAncestryChecked's filter: only the root's tree, at most three
// levels, intermediates included, nearest first; an unrelated agy excluded.
func TestFilterProcessAncestry(t *testing.T) {
	procs := []ProcessInfo{
		{PID: 10, ParentPID: 1, Name: "powershell.exe"}, // the wrapper (root)
		{PID: 11, ParentPID: 10, Name: "powershell.exe"},
		{PID: 12, ParentPID: 11, Name: "cmd.exe"},
		{PID: 13, ParentPID: 12, Name: "agy.exe"},
		{PID: 14, ParentPID: 13, Name: "agy.exe"}, // four levels down
		{PID: 20, ParentPID: 1, Name: "agy.exe"},  // unrelated
		{PID: 21, ParentPID: 10, Name: "agy.exe"},
	}
	got := filterProcessAncestry(procs, 10, 3)
	want := []int{11, 21, 12, 13}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want pids %v", got, want)
	}
	for i, p := range got {
		if p.PID != want[i] {
			t.Errorf("got[%d]=%d, want %d (nearest first)", i, p.PID, want[i])
		}
	}
}

// On every platform but Windows the checked scans are unavailable — never an
// empty ok result.
func TestScanChecked_UnavailableOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by the Windows backend tests")
	}
	if procs, ok := ScanCLIProcessesChecked(); ok || procs != nil {
		t.Errorf("ScanCLIProcessesChecked=(%v,%v), want (nil,false)", procs, ok)
	}
	if procs, ok := ScanProcessAncestryChecked(1); ok || procs != nil {
		t.Errorf("ScanProcessAncestryChecked=(%v,%v), want (nil,false)", procs, ok)
	}
}
