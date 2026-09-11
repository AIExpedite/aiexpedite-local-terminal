package main

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// Codex round 22 on #147: Claude runs up to three bounded children one after
// another (macOS Keychain read, OAuth usage request, `claude auth status`), but
// the generic accounting reserved a single probe window for the whole parser,
// and the Keychain read timed itself off context.Background(). An earlier
// discovery probe could therefore spend time Claude still needed, and the
// Keychain call could run on past a gather deadline that had already expired.

func TestClaudeReservesEveryOneOfItsSerialProbes(t *testing.T) {
	parser := cliAgentUsageParserIndex()["claudeCode"]
	if parser == nil {
		t.Fatal("claudeCode missing from the parser index")
	}
	counter, declares := parser.(cliAgentUsageSerialProbeCounter)
	if !declares {
		t.Fatal("the Claude parser must declare its serial probe count")
	}
	wantProbes := 2
	if runtime.GOOS == "darwin" {
		wantProbes = 3 // the Keychain read joins the usage request and auth status
	}
	if got := counter.SerialProbeCount(); got != wantProbes {
		t.Fatalf("SerialProbeCount() = %d, want %d", got, wantProbes)
	}
	want := time.Duration(wantProbes) * cliAgentModelDiscoveryGatherReserve
	if got := cliAgentUsageGatherReserve(parser); got != want {
		t.Fatalf("Claude reserve = %v, want %v — one window per serial child", got, want)
	}
	// A bounded parser that declares nothing keeps the one-window default, and a
	// file-only parser still reserves nothing.
	if got := cliAgentUsageGatherReserve(cliAgentUsageParserIndex()["codex"]); got != cliAgentModelDiscoveryGatherReserve {
		t.Fatalf("codex reserve = %v, want the one-probe default %v", got, cliAgentModelDiscoveryGatherReserve)
	}
	if got := cliAgentUsageGatherReserve(cliAgentUsageParserIndex()["grok"]); got != 0 {
		t.Fatalf("a file-only parser reserves %v, want 0", got)
	}
}

func TestKeychainReadDerivesItsDeadlineFromTheGather(t *testing.T) {
	// The seam is the observable one: the reader is handed the gather's context,
	// so an expired gather can no longer be followed by a fresh 3s Keychain call.
	original := claudeKeychainReader
	t.Cleanup(func() { claudeKeychainReader = original })

	var seen context.Context
	claudeKeychainReader = func(ctx context.Context) ([]byte, bool) {
		seen = ctx
		return nil, false
	}
	parent, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	readClaudeCredentialsRaw(parent, t.TempDir())
	if seen == nil {
		t.Fatal("the Keychain reader was not consulted for the default config dir")
	}
	if _, hasDeadline := seen.Deadline(); !hasDeadline {
		t.Fatal("the Keychain read must inherit the gather's deadline, not root a new one")
	}

	// And the real reader refuses an already-expired gather instead of spending
	// a full probe window on it.
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	start := time.Now()
	if _, ok := readClaudeKeychainCredential(expired); ok {
		t.Fatal("a canceled gather must not yield a Keychain credential")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled Keychain read took %v, want an immediate return", elapsed)
	}
}
