//go:build claudelive

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Certification run for live follow-ups with a real Claude Code CLI. A CLI is
// added to liveFollowUpCertified only after this passes on the CLI version the
// device ships with. Run by hand on a machine with a signed-in `claude`:
//
//	CLAUDE_LIVE_CERT=1 go test -tags claudelive -run TestClaudeLiveFollowUpCertification .
//
// It is never part of `go test ./...`.
func TestClaudeLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("CLAUDE_LIVE_CERT") != "1" {
		t.Skip("set CLAUDE_LIVE_CERT=1 to run the live certification")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("no claude CLI on PATH")
	}
	// The ledger half of the contract holds for any CLI: a written attempt is
	// never written again and survives a restart as written.
	l, path := newTestFollowUpLedger(t)
	row, write := l.Begin("cert-1", "m-cert", "s-cert", okCheck(time.Now()))
	if !write {
		t.Fatalf("certification attempt was fenced: %+v", row)
	}
	l.Finish(row.AttemptID, nil, followUpProcessedInCurrent)
	// `reopenFollowUpLedger` rather than a bare `newRelayFollowUpLedger`: it
	// releases the old handle AND registers the new one for release. Holding
	// the reloaded ledger open makes `t.TempDir()`'s cleanup fail on Windows
	// ("The process cannot access the file because it is being used by another
	// process", on `relay-follow-up-ledger.json.lock"), which FAILS the test
	// after its own contract has already held — so this certification could
	// never pass on the platform the device ships on, and no CLI could be
	// certified from a Windows machine.
	reloaded := reopenFollowUpLedger(t, l, path)
	if got := reloaded.Query("cert-1", reloaded.LedgerID(), time.Now().UnixMilli()); got.State != followUpRowWritten {
		t.Fatalf("written attempt not durable: %+v", got)
	}
	// The CLI half — a stdin user message written mid-turn is handled in the
	// running turn or the next one, exactly once, and never beside an open
	// approval — is checked by the operator against the session transcript.
	t.Log("ledger contract holds; verify the transcript before certifying this CLI version")
}
