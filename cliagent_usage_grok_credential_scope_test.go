// cliagent_usage_grok_credential_scope_test.go
// -----------------------------------------------------------------------------
// A direct (PTY) Grok run can pick up a credential from places the environment
// and the cached home never show: its own argv (`--config model.api_key=...`,
// which buildGrokInteractiveArgs forwards verbatim) and the repository it runs
// in (`.grok/config.toml`, which Grok discovers by walking upward from cwd).
// Naming the cached login above a record billed to one of those accounts is the
// misattribution the contested sentinel exists to prevent.
//
// The merge-side tests here cover the other half of the same question: which
// observation a managed receipt is allowed to replace once timestamps cannot be
// trusted in either direction.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An override need not be in the environment or in any config file we own: xAI
// documents `--config <key>=value` as the per-process override surface, so a
// direct run can bill an API-key account while both the environment and the
// home name the cached login.
func TestGrokDirectRunBillingIdentity_ContestsAnArgvAPIKey(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate value", []string{"--config", "model.api_key=xai-argv-sentinel"}, grokContestedBillingIdentity},
		{"joined flag", []string{"--config=model.api_key=xai-argv-sentinel"}, grokContestedBillingIdentity},
		{"short form", []string{"-c", "model.api_key=xai-argv-sentinel"}, grokContestedBillingIdentity},
		{"per-model key", []string{"--config", "model.grok-4-fast.apiKey=xai-argv-sentinel"}, grokContestedBillingIdentity},
		// An empty value is not a credential, and an unrelated override is not
		// one either. Neither may cost an honest run its observability.
		{"empty value", []string{"--config", "model.api_key="}, "acct-login"},
		{"unrelated override", []string{"--config", "log.level=debug", "fix", "the", "bug"}, "acct-login"},
		{"no args", nil, "acct-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Args: tc.args}, base)
			if got != tc.want {
				t.Fatalf("identity = %q, want %q for args %v", got, tc.want, tc.args)
			}
		})
	}
}

// Grok discovers project `.grok/config.toml` by walking UPWARD from the working
// directory — the discovery the maintenance smoke isolates itself from by
// running in an empty directory. A direct run honours the caller's cwd, so a
// workspace that pins `model.api_key` bills its own account from a session
// whose environment and home both name the cached login.
func TestGrokDirectRunBillingIdentity_ContestsAWorkspacePinnedKey(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")

	repo := t.TempDir()
	nested := filepath.Join(repo, "packages", "api")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Cwd: nested}, base); got != "acct-login" {
		t.Fatalf("identity = %q, want the cached login before any project key is pinned", got)
	}

	// Pinned at the repository ROOT while the session runs in a subdirectory:
	// the walk has to climb, exactly as Grok discovery does.
	if err := os.MkdirAll(filepath.Join(repo, ".grok"), 0o700); err != nil {
		t.Fatalf("mkdir .grok: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".grok", "config.toml"),
		[]byte("[model]\napi_key = \"xai-workspace-sentinel\"\n"), 0o600); err != nil {
		t.Fatalf("write project config.toml: %v", err)
	}
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Cwd: nested}, base); got != grokContestedBillingIdentity {
		t.Fatalf("identity = %q, want the contested sentinel for a workspace-pinned key", got)
	}

	// A session in an unrelated directory is unaffected: the pin belongs to the
	// repository, not to the machine.
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Cwd: t.TempDir()}, base); got != "acct-login" {
		t.Fatalf("identity = %q, want the cached login outside the pinned repository", got)
	}

	// An unnamed cwd walks nothing rather than reading the daemon's own
	// directory: the detector must never decide on ambient process state.
	if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != "acct-login" {
		t.Fatalf("identity = %q, want the cached login when no cwd is named", got)
	}
}

// The supersession scan has to look PAST an untrusted future record rather than
// stop at it. Stopping reported "this account has nothing here", which hid an
// earlier record that is both trusted and newer than the managed receipt, and
// the receipt was then appended last — replacing a good observation with a
// stale one.
func TestPersistGrokManagedBillingSnapshot_KeepsATrustedRecordBehindAFutureOne(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	// A good direct-run observation, then a future-dated one on top of it.
	trusted := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(trusted, 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339), 99,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	// The managed child fetched BEFORE the trusted record and only exits now.
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingSuperseded {
		t.Fatalf("outcome = %s, want %s — a future record must not hide the newer "+
			"trusted observation behind it", outcome, grokManagedBillingSuperseded)
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a stale managed receipt rewrote the log:\nbefore=%s\nafter=%s", before, after)
	}
	got, ok := newestTrustedGrokBillingObservationFor(persistent,
		grokIdentityCandidates(persistent), time.Now().Add(grokBillingMaxClockSkew))
	if !ok || got.UTC().Format(time.RFC3339) != trusted {
		t.Fatalf("newest trusted observation = %v ok=%v, want %s", got, ok, trusted)
	}
}

// The clock can move backwards between the managed child's credits fetch and its
// exit, leaving us carrying a future-dated observation. grokBillingMetrics
// refuses to publish such a record, and readGrokBillingSnapshot takes the LAST
// billing line by file order — so merging it would blank a valid reading and
// keep it blank until wall-clock caught up.
func TestPersistGrokManagedBillingSnapshot_RefusesAFutureDatedSnapshot(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339), 52,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}

	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingUntrusted {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingUntrusted)
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("an unpublishable future receipt rewrote the log:\nbefore=%s\nafter=%s", before, after)
	}
	snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent))
	if !ok || !snap.HasUsedPercent || snap.UsedPercent != 52 {
		t.Fatalf("the valid observation must survive the refused merge: %+v ok=%v", snap, ok)
	}
}
