//go:build linux

package main

import (
	"os"
	"testing"
)

// TestLinuxStartTokenNeedsTheMachineBootID: without the kernel boot id a PID
// and start tick cannot be told apart across reboots, so no token is minted
// (the record becomes incomplete) and a record without one reads unknown.
func TestLinuxStartTokenNeedsTheMachineBootID(t *testing.T) {
	prev := linuxBootID
	t.Cleanup(func() { linuxBootID = prev })

	if tok, err := processStartToken(os.Getpid()); err != nil || tok == "" {
		t.Fatalf("with a boot id: %q, %v", tok, err)
	}
	linuxBootID = func() string { return "" }
	if tok, err := processStartToken(os.Getpid()); err == nil || tok != "" {
		t.Fatalf("a token was minted without a boot id: %q", tok)
	}
	if got := probeRecordedProcess(ledgerProcess{PID: os.Getpid(), StartTime: "linux::1"}); got != processUnknown {
		t.Fatalf("probe without a boot id = %v, want unknown", got)
	}
	linuxBootID = prev
	if got := probeRecordedProcess(ledgerProcess{PID: os.Getpid(), StartTime: "linux::1"}); got != processUnknown {
		t.Fatalf("a legacy token without a boot id = %v, want unknown", got)
	}

	// Through the ledger: the entry is incomplete, never certified.
	dir := t.TempDir()
	l := newTestLedger(t, dir, "boot-x")
	l.startToken = func(pid int) (string, error) {
		linuxBootID = func() string { return "" }
		defer func() { linuxBootID = prev }()
		return processStartToken(pid)
	}
	l.BeginSpawn("s")
	l.TrackProcess("s", proc(os.Getpid()), false)
	if e := readLedgerFile(t, dir).Entries; len(e) != 1 || !e[0].Incomplete {
		t.Fatalf("entry = %+v, want incomplete", e)
	}
}
