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
	"fmt"
	"os"
	"reflect"
	"strings"
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

// A conclusive "no usable provider" forgets the old name, so a later probe
// that cannot ask does not bring a disconnected install's account back.
func TestOpenCodeForgetsItsNameAfterAConclusiveEmptyProbe(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)

	if named := parseOpenCodeUnder(t, "opencode"); named.Account == "" {
		t.Fatal("expected a named install to start from")
	}
	if empty := parseOpenCodeUnder(t, "opencode-no-providers"); empty.Account != "" {
		t.Fatalf("account after a conclusive empty probe = %q, want none", empty.Account)
	}
	if after := parseOpenCodeUnder(t, "opencode-unreachable"); after.Account != "" {
		t.Fatalf("a failed probe revived the forgotten name %q", after.Account)
	}
}

// The name covers every provider in the catalog, including those listed after
// the cap on the published models.
func TestOpenCodeNamesProvidersBeyondTheModelCap(t *testing.T) {
	var out strings.Builder
	for i := 0; i < cliUsageMaxModelsPerProvider+5; i++ {
		fmt.Fprintf(&out, "opencode/model-%d\n", i)
	}
	out.WriteString("ollama/qwen3-coder:30b\n")

	all := parseOpenCodeModelIDs(out.String())
	if got, want := openCodeProvidersFromModelIDs(all), []string{"ollama", "opencode"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %#v, want %#v", got, want)
	}
	if got := len(parseOpenCodeModelList(out.String())); got != cliUsageMaxModelsPerProvider {
		t.Fatalf("published models = %d, want the cap %d", got, cliUsageMaxModelsPerProvider)
	}
}

// The usage ledger keys its buckets by the same fingerprint the card publishes,
// so a provider-set change (a new `/connect`) starts a new bucket, while a probe
// that could not ask keeps the old name — and with it the day's numbers.
func TestOpenCodeUsageBucketsFollowTheAccountFingerprint(t *testing.T) {
	openCodeUsageFixture(t, 1501)
	path := "opencode-identity-bin"
	now := time.Now()
	parse := func() *cliAgentUsage {
		t.Helper()
		SetOpenCodeReadinessForceProbe(true)
		usage, _ := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Path: path}, now)
		return usage
	}
	spend := func(part string, input int64) {
		t.Helper()
		run := armOpenCodeUsageRunForExecutable(path, "")
		captureOpenCodeUsageLine(run, openCodeStepFinish("ses_i", part, input, 0, 0, "0", now.UnixMilli()))
		settleOpenCodeUsageRun(run, "")
	}

	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	first := parse()
	spend("prt_1", 50)
	if got := parse(); got.AccountFingerprint != first.AccountFingerprint || len(got.Metrics) == 0 || *got.Metrics[0].Consumed != 50 {
		t.Fatalf("the install's own runs are not on its card: %+v", got.Metrics)
	}

	// A probe that could not ask: same name, same bucket.
	stubOpenCodeReadiness(t, "", false)
	if kept := parse(); kept.AccountFingerprint != first.AccountFingerprint || len(kept.Metrics) == 0 {
		t.Fatalf("an inconclusive probe dropped the bucket: fp %s→%s metrics %+v",
			first.AccountFingerprint, kept.AccountFingerprint, kept.Metrics)
	}

	// A new provider renames the install: a fresh bucket, the old one untouched.
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\nopenai/gpt-5\n", true)
	renamed := parse()
	if renamed.AccountFingerprint == first.AccountFingerprint || len(renamed.Metrics) != 0 {
		t.Fatalf("a provider-set change kept the old bucket: %+v", renamed.Metrics)
	}
	spend("prt_2", 7)
	if got := parse(); len(got.Metrics) == 0 || *got.Metrics[0].Consumed != 7 {
		t.Fatalf("the renamed install's bucket = %+v, want 7", got.Metrics)
	}
	if old, _, _ := openCodeUsageBucketForDay(first.AccountFingerprint, now); old.tokens() != 50 {
		t.Fatalf("the old bucket moved to %d", old.tokens())
	}
}

// Spend banked before any probe named the account stays unpublished while the
// card still has no account: the gather would file it under a device fallback
// fingerprint no later probe names, and the backend would record its
// generation as applied. The first gather with a real account publishes it.
func TestOpenCodeUsageWithholdsPendingSpendUntilAnAccountResolves(t *testing.T) {
	openCodeUsageFixture(t, 1502)
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)
	path := "opencode-pending-bin"
	now := time.Now()
	parse := func() *cliAgentUsage {
		t.Helper()
		SetOpenCodeReadinessForceProbe(true)
		usage, _ := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Path: path}, now)
		return usage
	}

	stubOpenCodeReadiness(t, "", false)
	run := armOpenCodeUsageRunForExecutable(path, "")
	captureOpenCodeUsageLine(run, openCodeStepFinish("ses_p", "prt_p", 40, 0, 0, "0", now.UnixMilli()))
	settleOpenCodeUsageRun(run, "")

	if unresolved := parse(); unresolved.AccountFingerprint != "" || len(unresolved.Metrics) != 0 || unresolved.UsageGeneration != nil {
		t.Fatalf("an unresolved account published pending spend: fp %q metrics %+v generation %+v",
			unresolved.AccountFingerprint, unresolved.Metrics, unresolved.UsageGeneration)
	}

	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	resolved := parse()
	if resolved.AccountFingerprint == "" || len(resolved.Metrics) == 0 || *resolved.Metrics[0].Consumed != 40 {
		t.Fatalf("the resolved account did not adopt the pending spend: %+v", resolved.Metrics)
	}
	if resolved.UsageGeneration == nil {
		t.Fatal("the resolved account withheld the generation of the spend it published")
	}
}
