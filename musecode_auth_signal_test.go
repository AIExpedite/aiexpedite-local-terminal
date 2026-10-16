package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The refusal Muse Code prints at kickoff when it has no credential (prod, AIX5).
const museRefusalLine = "missing meta credentials: run `muse login` or set META_API_KEY, or save credentials at C:\\Users\\dkupi\\.config\\muse\\auth.json"

func TestMuseCodeAuthEvidence(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{"the CLI's own refusal on stderr", museRefusalLine, -1},
		{"a failed run whose reason is the refusal", `{"payload_type":"run.terminal.failed","payload":{"error_kind":"auth","reason":"missing meta credentials"}}`, -1},
		{"a completed run", `{"payload_type":"run.terminal.completed","payload":{"text":"done"}}`, 1},
		// The model's own text can quote anything (a review of this file does):
		// only the CLI's words count.
		{"model output quoting the refusal", `{"payload_type":"run.output.delta","payload":{"delta":"handles missing meta credentials"}}`, 0},
		{"a completed run whose answer quotes it", `{"payload_type":"run.terminal.completed","payload":{"text":"run ` + "`muse login`" + ` first"}}`, 1},
		{"an ordinary failure", `{"payload_type":"run.terminal.failed","payload":{"error_kind":"rate_limit","reason":"slow down"}}`, 0},
		{"an unrelated line", "loading tools…", 0},
		{"broken JSON", `{"payload_type":`, 0},
	}
	for _, tc := range cases {
		if got := museCodeAuthEvidence(tc.line); got != tc.want {
			t.Errorf("%s: museCodeAuthEvidence = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestMuseCodeSignedOut_BlocksUntilPositiveEvidence(t *testing.T) {
	home := isolateMuseCode(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	now := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	detected := detectedCLIAgent{Name: "Muse Code", Detected: true, Version: "1.4.9", Path: "/x/muse"}
	parse := func() *cliAgentUsage {
		t.Helper()
		usage, ok := museCodeUsageParser{}.Parse(home, detected, now)
		if !ok || usage == nil {
			t.Fatal("the parser must always emit an entry")
		}
		return usage
	}

	if usage := parse(); usage.AuthState != museCodeAuthUnknown || usage.NoticeSeverity != "" {
		t.Fatalf("before any refusal: authState=%q notice=%q", usage.AuthState, usage.NoticeSeverity)
	}

	// A Ship session prints the refusal: signed out, with an ERROR notice
	// (terminal-service marks that blocked, and routing skips it).
	observeMuseCodeAuthOutput(museRefusalLine, now)
	usage := parse()
	if usage.AuthState != museCodeAuthSignedOut || usage.Authenticated == nil || *usage.Authenticated {
		t.Fatalf("after the refusal: authState=%q authenticated=%v", usage.AuthState, usage.Authenticated)
	}
	if usage.NoticeSeverity != "error" || !strings.Contains(usage.Notice, "muse login") {
		t.Fatalf("notice=%q severity=%q", usage.Notice, usage.NoticeSeverity)
	}
	// Stays signed out on later scans while nothing changed.
	if again := parse(); again.AuthState != museCodeAuthSignedOut {
		t.Fatalf("a later scan lost the mark: %q", again.AuthState)
	}

	t.Run("a credential written since (muse login) clears it", func(t *testing.T) {
		observeMuseCodeAuthOutput(museRefusalLine, now)
		writeMuseConfig(t, home, "auth.json", `{"t":"x"}`)
		if usage := parse(); usage.AuthState != museCodeAuthReady || usage.NoticeSeverity != "" {
			t.Fatalf("authState=%q notice=%q", usage.AuthState, usage.NoticeSeverity)
		}
		if _, err := os.Stat(museCodeAuthStatePath()); !os.IsNotExist(err) {
			t.Fatalf("the mark must be cleared (stat err=%v)", err)
		}
	})

	t.Run("a refusal with the credential file in place still blocks (the CLI rejected it)", func(t *testing.T) {
		observeMuseCodeAuthOutput(museRefusalLine, now)
		if usage := parse(); usage.AuthState != museCodeAuthSignedOut {
			t.Fatalf("authState=%q", usage.AuthState)
		}
	})

	t.Run("a completed run clears it, even in the same Chat turn as a refusal", func(t *testing.T) {
		observeMuseCodeAuthOutput(museRefusalLine, now)
		observeMuseCodeAuthOutput(museRefusalLine+"\n"+`{"payload_type":"run.terminal.completed","payload":{"text":"ok"}}`, now)
		if usage := parse(); usage.AuthState == museCodeAuthSignedOut {
			t.Fatal("a completed run proves the sign-in")
		}
	})

	t.Run("the API key set since clears it", func(t *testing.T) {
		os.Remove(filepath.Join(home, ".config", "muse", "auth.json"))
		observeMuseCodeAuthOutput(museRefusalLine, now)
		t.Setenv("META_API_KEY", "k")
		if usage := parse(); usage.AuthState != museCodeAuthReady {
			t.Fatalf("authState=%q", usage.AuthState)
		}
	})
}

func TestMuseCodeNativeSpec_ObservesEveryTurn(t *testing.T) {
	isolateMuseCode(t)
	if museCodeNativeSpec.ObserveTurn == nil {
		t.Fatal("Muse Code Chat turns must feed the sign-in signal")
	}
	museCodeNativeSpec.ObserveTurn("", museRefusalLine)
	if _, err := os.Stat(museCodeAuthStatePath()); err != nil {
		t.Fatalf("a Chat turn's refusal must record the mark: %v", err)
	}
}

// End to end through the stub `muse`: a Chat turn the CLI refuses records the
// mark; a later completed turn clears it.
func TestMuseCodeChatTurn_RecordsAndClearsTheSignedOutMark(t *testing.T) {
	isolateMuseCode(t)
	m := installMuseCodeStub(t)
	startMuse(t, m, "s", "")

	t.Setenv("OPENCODE_STUB_STDOUT", "")
	t.Setenv("OPENCODE_STUB_STDERR", museRefusalLine)
	t.Setenv("OPENCODE_STUB_EXIT", "1")
	_ = m.Send("s", "hello", (&frameSink{}).publish, time.Minute)
	if _, err := os.Stat(museCodeAuthStatePath()); err != nil {
		t.Fatalf("a refused turn must record the mark: %v", err)
	}

	t.Setenv("OPENCODE_STUB_STDERR", "")
	t.Setenv("OPENCODE_STUB_EXIT", "0")
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameDeltaHello, museFrameCompleted))
	if err := m.Send("s", "hello again", (&frameSink{}).publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := os.Stat(museCodeAuthStatePath()); !os.IsNotExist(err) {
		t.Fatalf("a completed turn must clear the mark (stat err=%v)", err)
	}
}

// A stale-session recovery's replay is a turn of its own: its completed run
// clears the mark (a keychain re-login leaves no stamp to change).
func TestMuseCodeChatTurn_ReplayRecoveryClearsTheMark(t *testing.T) {
	isolateMuseCode(t)
	m := installMuseCodeStub(t)
	recordMuseCodeSignedOut(time.Now(), t.TempDir(), func(string) string { return "" })
	t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
	t.Setenv("OPENCODE_STUB_FIRST_STDOUT", `Error: session not found\n`)
	t.Setenv("OPENCODE_STUB_FIRST_EXIT", "1")
	t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
	startMuse(t, m, "s", "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11")
	if err := m.Send("s", "follow up", (&frameSink{}).publish, time.Minute); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := os.Stat(museCodeAuthStatePath()); !os.IsNotExist(err) {
		t.Fatalf("the replay's completed run must clear the mark (stat err=%v)", err)
	}
}

// A scan never erases a refusal recorded after the mark it compared: the
// stamp check and the removal are one step under the lock.
func TestMuseCodeSignedOutNotice_RemovesOnlyTheMarkItCompared(t *testing.T) {
	home := isolateMuseCode(t)
	getenv := func(string) string { return "" }
	recordMuseCodeSignedOut(time.Now(), home, getenv)
	// Credentials changed since: the scan clears the old mark…
	writeMuseConfig(t, home, "auth.json", `{"t":"x"}`)
	if _, out := museCodeSignedOutNotice(home, getenv); out {
		t.Fatal("a changed credential must clear the old mark")
	}
	// …and a refusal of the NEW credential is kept by the next scan.
	recordMuseCodeSignedOut(time.Now(), home, getenv)
	if _, out := museCodeSignedOutNotice(home, getenv); !out {
		t.Fatal("a refusal recorded with the current stamp must stand")
	}
}
