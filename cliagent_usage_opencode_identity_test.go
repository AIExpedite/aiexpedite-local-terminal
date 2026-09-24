// cliagent_usage_opencode_identity_test.go
// -----------------------------------------------------------------------------
// OpenCode has no login, so the joined provider list IS the snapshot's
// Account, and the capacity view keys an account by that string. Observed on
// a Windows computer (AIE2, 2026-09-21..24): one install alternated between
// "t, google, 1" (from `auth list`, drawn in ASCII because the tray agent has
// no Unicode terminal) and "google, opencode" (from the model ids, when the
// optional `auth list` probe was skipped for time), with a nameless snapshot
// whenever `opencode models` failed. Each name was a separate account, so the
// one install showed up twice.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"reflect"
	"testing"
	"time"
)

// `opencode auth list` on AIE2 (OpenCode 1.18.32) under the tray agent's
// environment: the prompt library's ASCII fallback frame, and a Google key
// supplied by GEMINI_API_KEY.
const realOpenCodeAuthListASCIIEnv = "\x1b[0m\r\n" +
	"\x1b[90mT\x1b[39m  Credentials \x1b[90m~\\.local\\share\\opencode\\auth.json\n" +
	"\x1b[90m|\x1b[39m\n" +
	"\x1b[90m—\x1b[39m  0 credentials\n\n" +
	"\x1b[90mT\x1b[39m  Environment\n" +
	"\x1b[90m|\x1b[39m\n" +
	"\x1b[34m•\x1b[39m  Google \x1b[90mGEMINI_API_KEY\n" +
	"\x1b[90m|\x1b[39m\n" +
	"\x1b[90m—\x1b[39m  1 environment variable\n\n"

// The same output from a Unicode-capable terminal.
const realOpenCodeAuthListUnicodeEnv = "\x1b[0m\r\n" +
	"\x1b[90m┌\x1b[39m  Credentials \x1b[90m~\\.local\\share\\opencode\\auth.json\n" +
	"\x1b[90m│\x1b[39m\n" +
	"\x1b[90m└\x1b[39m  0 credentials\n\n" +
	"\x1b[90m┌\x1b[39m  Environment\n" +
	"\x1b[90m│\x1b[39m\n" +
	"\x1b[34m●\x1b[39m  Google \x1b[90mGEMINI_API_KEY\n" +
	"\x1b[90m│\x1b[39m\n" +
	"\x1b[90m└\x1b[39m  1 environment variable\n\n"

// "t" was the ASCII section opener, "environment" the section header and "1"
// its footer. Google is the only provider on that screen.
func TestParseOpenCodeAuthProvidersReadsBothFrameStyles(t *testing.T) {
	for name, out := range map[string]string{
		"ascii":   realOpenCodeAuthListASCIIEnv,
		"unicode": realOpenCodeAuthListUnicodeEnv,
	} {
		t.Run(name, func(t *testing.T) {
			got := parseOpenCodeAuthProviders(out)
			if want := []string{"google"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("providers = %#v, want %#v", got, want)
			}
		})
	}
}

func parseOpenCodeUnder(t *testing.T, mode string) *cliAgentUsage {
	t.Helper()
	t.Setenv(mockCLIEnvVar, mode)
	SetOpenCodeReadinessForceProbe(true)
	usage, ok := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Name: "OpenCode", Version: "1.18.32", Path: os.Args[0],
	}, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("parser refused a detected install")
	}
	return usage
}

// The model ids name the install whether or not `auth list` answered, so a
// probe that happens to answer cannot rename it.
func TestOpenCodeAccountComesFromTheModelIDs(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)

	withAuth := parseOpenCodeUnder(t, "opencode-ascii-env")
	if want := "mlx, ollama, opencode"; withAuth.Account != want {
		t.Fatalf("account = %q, want %q (the model-derived providers)", withAuth.Account, want)
	}
	plain := parseOpenCodeUnder(t, "opencode")
	if plain.Account != withAuth.Account || plain.AccountFingerprint != withAuth.AccountFingerprint {
		t.Fatalf("the same install was named %q (%s) and %q (%s)",
			withAuth.Account, withAuth.AccountFingerprint, plain.Account, plain.AccountFingerprint)
	}
}

// A probe that could not ask keeps the last name it had rather than
// publishing a nameless snapshot, which the capacity view would key as yet
// another account. It does not claim the install is ready.
func TestOpenCodeKeepsItsNameWhenAProbeFails(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)

	before := parseOpenCodeUnder(t, "opencode")
	after := parseOpenCodeUnder(t, "opencode-unreachable")
	if after.Account != before.Account || after.AccountFingerprint != before.AccountFingerprint {
		t.Fatalf("account after a failed probe = %q (%s), want %q (%s)",
			after.Account, after.AccountFingerprint, before.Account, before.AccountFingerprint)
	}
	if after.AuthState != openCodeAuthUnknown {
		t.Fatalf("authState after a failed probe = %q, want %q", after.AuthState, openCodeAuthUnknown)
	}
	if len(after.Models) != 0 {
		t.Fatalf("a failed probe must not republish the old models: %#v", after.Models)
	}
}
