//go:build windows

package main

import (
	"errors"
	"testing"
)

// The Windows backends through the command seam: the trailer / WMIC rules
// decide ok, a missing backend is never ok, and the orphan scanner's wrapper
// keeps returning the same rows.
func TestScanProcessesChecked_Backends(t *testing.T) {
	orig := runProcessScanCommand
	defer func() { runProcessScanCommand = orig }()

	header := "\"Name\",\"ProcessId\",\"ParentProcessId\",\"CreationDate\"\r\n"
	rows := header +
		"\"powershell.exe\",\"11\",\"10\",\"20260929204356\"\r\n" +
		"\"agy.exe\",\"12\",\"11\",\"20260929204357\"\r\n" +
		"\"agy.exe\",\"99\",\"1\",\"20260929204357\"\r\n"
	runProcessScanCommand = func(string, ...string) ([]byte, []byte, error) {
		return []byte(rows + processScanOKTrailer + "\r\n"), nil, nil
	}
	if procs, ok := scanProcessesChecked(backendPowerShell, cliProcessFilter, cliProcessWMICWhere); !ok || len(procs) != 3 {
		t.Errorf("powershell scan=(%d rows, %v), want 3 rows ok", len(procs), ok)
	}

	runProcessScanCommand = func(string, ...string) ([]byte, []byte, error) {
		return []byte(rows), nil, errors.New("exit status 3")
	}
	if _, ok := scanProcessesChecked(backendPowerShell, cliProcessFilter, cliProcessWMICWhere); ok {
		t.Error("a failed PowerShell scan reported ok")
	}

	runProcessScanCommand = func(string, ...string) ([]byte, []byte, error) {
		return nil, []byte(wmicNoInstances), errors.New("exit status 1")
	}
	if procs, ok := scanProcessesChecked(backendWMIC, cliProcessFilter, cliProcessWMICWhere); !ok || len(procs) != 0 {
		t.Errorf("WMIC no-instances=(%v,%v), want an ok empty result", procs, ok)
	}

	if procs, ok := scanProcessesChecked(backendNone, cliProcessFilter, cliProcessWMICWhere); ok || procs != nil {
		t.Errorf("backendNone=(%v,%v), want (nil,false)", procs, ok)
	}
}
