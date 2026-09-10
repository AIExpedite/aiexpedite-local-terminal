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
	"strings"
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

// `env_key` is the second credential spelling xAI's config supports: instead of
// carrying the key inline it names the environment variable the child reads it
// from. classifyGrokSystemSemanticValue already treats every non-empty env_key
// as a credential, so an attribution guard that recognised only `api_key` would
// name the cached login for a child billing the API-key account — the exact
// misattribution the contested sentinel exists to prevent.
//
// The pin counts even when the variable it names is unset in THIS process: the
// child resolves it in its own environment, per turn, and the guard is
// conservative by construction.
func TestGrokDirectRunBillingIdentity_ContestsAnEnvKeyCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		launch func(t *testing.T) grokDirectRunLaunch
		want   string
	}{
		{
			name: "argv env_key",
			launch: func(*testing.T) grokDirectRunLaunch {
				return grokDirectRunLaunch{Args: []string{"--config", "model.env_key=MY_XAI_KEY"}}
			},
			want: grokContestedBillingIdentity,
		},
		{
			name: "argv per-model envKey",
			launch: func(*testing.T) grokDirectRunLaunch {
				return grokDirectRunLaunch{Args: []string{"--config=model.grok-4-fast.envKey=MY_XAI_KEY"}}
			},
			want: grokContestedBillingIdentity,
		},
		{
			name: "argv empty env_key",
			launch: func(*testing.T) grokDirectRunLaunch {
				return grokDirectRunLaunch{Args: []string{"--config", "model.env_key="}}
			},
			want: "acct-login",
		},
		{
			name: "project config env_key",
			launch: func(t *testing.T) grokDirectRunLaunch {
				repo := t.TempDir()
				helperWriteGrokProjectConfig(t, repo, "[model]\nenv_key = \"MY_XAI_KEY\"\n")
				return grokDirectRunLaunch{Cwd: repo}
			},
			want: grokContestedBillingIdentity,
		},
		{
			name: "project config per-model env_key",
			launch: func(t *testing.T) grokDirectRunLaunch {
				repo := t.TempDir()
				helperWriteGrokProjectConfig(t, repo, "[model.grok-4-fast]\nenv_key = \"MY_XAI_KEY\"\n")
				return grokDirectRunLaunch{Cwd: repo}
			},
			want: grokContestedBillingIdentity,
		},
		{
			name: "project config empty env_key",
			launch: func(t *testing.T) grokDirectRunLaunch {
				repo := t.TempDir()
				helperWriteGrokProjectConfig(t, repo, "[model]\nenv_key = \"\"\n")
				return grokDirectRunLaunch{Cwd: repo}
			},
			want: "acct-login",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGrokBillingAttribution(t)
			helperNoGrokSystemConfigLayers(t)
			base := helperGrokHomeWithAccount(t, "acct-login")
			if got := grokDirectRunBillingIdentity(tc.launch(t), base); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

// The home config and the system layers carry the same second spelling, and
// neither is redirected away from the child: the home one is the file a direct
// run really reads (it is not the neutralised isolated copy), and a system
// layer survives GROK_HOME isolation entirely.
func TestGrokDirectRunBillingIdentity_ContestsAnEnvKeyOutsideTheRepository(t *testing.T) {
	t.Run("home config", func(t *testing.T) {
		resetGrokBillingAttribution(t)
		helperNoGrokSystemConfigLayers(t)
		base := helperGrokHomeWithAccount(t, "acct-login")
		if err := os.WriteFile(filepath.Join(base, "config.toml"),
			[]byte("[model]\nenv_key = \"MY_XAI_KEY\"\n"), 0o600); err != nil {
			t.Fatalf("write home config.toml: %v", err)
		}
		if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != grokContestedBillingIdentity {
			t.Fatalf("identity = %q, want the contested sentinel for a home-pinned env_key", got)
		}
	})

	t.Run("system layer", func(t *testing.T) {
		resetGrokBillingAttribution(t)
		base := helperGrokHomeWithAccount(t, "acct-login")
		layer := filepath.Join(t.TempDir(), "managed_config.toml")
		if err := os.WriteFile(layer, []byte("[model]\nenv_key = \"MY_XAI_KEY\"\n"), 0o600); err != nil {
			t.Fatalf("write system layer: %v", err)
		}
		prev := grokSystemConfigPathsFn
		grokSystemConfigPathsFn = func() []string { return []string{layer} }
		t.Cleanup(func() { grokSystemConfigPathsFn = prev })

		if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != grokContestedBillingIdentity {
			t.Fatalf("identity = %q, want the contested sentinel for a system-pinned env_key", got)
		}
	})
}

// helperWriteGrokProjectConfig writes a repository-scoped `.grok/config.toml`,
// the layer Grok discovers by walking upward from the child's cwd.
func helperWriteGrokProjectConfig(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".grok"), 0o700); err != nil {
		t.Fatalf("mkdir .grok: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".grok", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write project config.toml: %v", err)
	}
}

// `--api-key-env=OTHER_VAR` normalises to `apikeyenv`, which is a suffix of
// neither `api_key` nor `env_key`, so the config-key rule could never recognise
// it and the run was attributed to the cached login while the CLI billed the
// named environment key. The flag set is the repository's own enumeration
// (isGrokAuthOverrideArg), and both the joined and the space-separated
// spellings survive buildGrokInteractiveArgs, so both must contest.
func TestGrokDirectRunBillingIdentity_ContestsAnAuthOverrideFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"api-key-env joined", []string{"--api-key-env=OTHER_XAI_KEY"}, grokContestedBillingIdentity},
		{"api-key-env separate", []string{"--api-key-env", "OTHER_XAI_KEY"}, grokContestedBillingIdentity},
		{"api-key joined", []string{"--api-key=xai-flag-sentinel"}, grokContestedBillingIdentity},
		{"api-key separate", []string{"--api-key", "xai-flag-sentinel"}, grokContestedBillingIdentity},
		{"auth-method", []string{"--auth-method", "api-key"}, grokContestedBillingIdentity},
		// A bare flag with no value still contests: whether the CLI resolves it
		// is its decision, made inside a process we do not observe.
		{"bare flag", []string{"--api-key-env"}, grokContestedBillingIdentity},
		// An unrelated flag that merely LOOKS adjacent must not cost an honest
		// run its observability.
		{"unrelated flag", []string{"--api-keys-report", "fix", "the", "bug"}, "acct-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGrokBillingAttribution(t)
			helperNoGrokSystemConfigLayers(t)
			base := helperGrokHomeWithAccount(t, "acct-login")
			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Args: tc.args}, base); got != tc.want {
				t.Fatalf("identity = %q, want %q for args %v", got, tc.want, tc.args)
			}
		})
	}
}

// `model = { api_key = "..." }` is valid TOML that grok's own parser honours,
// but the line-oriented sweep only exposes the OUTER key, so the credential
// lives entirely inside the value. Missing it named the cached login above a
// record the API-key account was billed for.
func TestGrokDirectRunBillingIdentity_ContestsAnInlineTableCredential(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{"root inline table", "model = { api_key = \"xai-inline-sentinel\" }\n", grokContestedBillingIdentity},
		{"root inline env_key", "model = { env_key = \"OTHER_XAI_KEY\" }\n", grokContestedBillingIdentity},
		{"per-model inline table", "[model]\ngrok-4-fast = { api_key = \"xai-inline-sentinel\" }\n", grokContestedBillingIdentity},
		{"nested inline table", "model = { models = { \"grok-4-fast\" = { api_key = \"xai-inline-sentinel\" } } }\n", grokContestedBillingIdentity},
		{"dotted key inside table", "model = { \"grok-4-fast\".api_key = \"xai-inline-sentinel\" }\n", grokContestedBillingIdentity},
		// A quoted value containing a brace or a comma must not desynchronise
		// the field split and hide the assignment that follows it.
		{"credential after a quoted brace", "model = { name = \"a,{b}\", api_key = \"xai-inline-sentinel\" }\n", grokContestedBillingIdentity},
		// An empty credential is not one, and an inline table with no
		// credential in it may not cost an honest run its observability.
		{"empty inline credential", "model = { api_key = \"\" }\n", "acct-login"},
		{"unrelated inline table", "model = { temperature = 0.2, name = \"grok-4-fast\" }\n", "acct-login"},
		// A credential-shaped key under an UNRELATED table is not a credential
		// grok would bill with.
		{"credential outside [model]", "logging = { api_key = \"not-a-model-credential\" }\n", "acct-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGrokBillingAttribution(t)
			helperNoGrokSystemConfigLayers(t)
			base := helperGrokHomeWithAccount(t, "acct-login")
			repo := t.TempDir()
			helperWriteGrokProjectConfig(t, repo, tc.config)
			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Cwd: repo}, base); got != tc.want {
				t.Fatalf("identity = %q, want %q for config %q", got, tc.want, tc.config)
			}
		})
	}
}

// File order is APPEND order, not timestamp order. A clock correction on the
// device (or a managed snapshot merged in after the fact) can leave an older
// same-account record physically after a newer one. Stopping the supersession
// scan at the first same-account hit reported the OLDER time, which let a stale
// managed receipt append over the newer observation this guard protects.
func TestPersistGrokManagedBillingSnapshot_KeepsTheGreatestOutOfOrderObservation(t *testing.T) {
	resetGrokBillingAttribution(t)
	persistent := helperGrokHomeWithAccount(t, "acct-1")
	isolated := helperGrokHomeWithAccount(t, "acct-1")
	t.Setenv("GROK_HOME", persistent)

	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatalf("seed persistent identity: %v", err)
	}
	// The newest observation is written FIRST; a backwards clock step then
	// appends an older one on top of it. Both are this account's and both are
	// trusted, so neither is skipped for any other reason.
	newest := time.Now().Add(-57 * time.Minute).UTC().Format(time.RFC3339)
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(newest, 52, "USAGE_PERIOD_TYPE_WEEKLY",
			"2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))
	helperAppendGrokLogLine(t, persistent,
		grokBillingLine(time.Now().Add(-61*time.Minute).UTC().Format(time.RFC3339), 40,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	got, ok := newestTrustedGrokBillingObservationFor(persistent,
		grokIdentityCandidates(persistent), time.Now().Add(grokBillingMaxClockSkew))
	if !ok || got.UTC().Format(time.RFC3339) != newest {
		t.Fatalf("newest trusted observation = %v ok=%v, want %s — the scan must "+
			"take the GREATEST attributable time, not the last one appended", got, ok, newest)
	}

	// A managed receipt dated between the two must therefore be superseded
	// rather than appended over the newer observation.
	before, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if err := seedGrokManagedBillingIdentity(isolated); err != nil {
		t.Fatalf("seed isolated identity: %v", err)
	}
	helperAppendGrokLogLine(t, isolated,
		grokBillingLine(time.Now().Add(-59*time.Minute).UTC().Format(time.RFC3339), 12,
			"USAGE_PERIOD_TYPE_WEEKLY", "2026-08-17T22:28:32Z", "2126-08-24T22:28:32Z"))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent)
	if err != nil {
		t.Fatalf("persist: %v", err)
	}
	if outcome != grokManagedBillingSuperseded {
		t.Fatalf("outcome = %s, want %s", outcome, grokManagedBillingSuperseded)
	}
	after, err := os.ReadFile(grokBillingLogPath(persistent))
	if err != nil {
		t.Fatalf("read persistent log: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a stale managed receipt rewrote the log:\nbefore=%s\nafter=%s", before, after)
	}
}

// TestGrokConfigPinsCredential_QuotedTOMLKeys pins that TOML's quoted-key
// spellings are recognized as credentials. Grok's own parser applies
// `"api_key" = "xai-..."` and `"model" = { api_key = "..." }`, so a guard that
// only matched the bare spelling reported "no pinned credential" for a run the
// API-key account is billed for.
func TestGrokConfigPinsCredential_QuotedTOMLKeys(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"bare root", "[model]\napi_key = \"xai-abc\"\n", true},
		{"quoted key", "[model]\n\"api_key\" = \"xai-abc\"\n", true},
		{"single-quoted key", "[model]\n'api_key' = \"xai-abc\"\n", true},
		{"quoted section", "[\"model\"]\napi_key = \"xai-abc\"\n", true},
		{"quoted section and key", "['model']\n'env_key' = \"XAI_KEY\"\n", true},
		{"quoted inline table key", "\"model\" = { api_key = \"xai-abc\" }\n", true},
		{"quoted per-model section", "[model.\"grok-4\"]\n\"api_key\" = \"xai-abc\"\n", true},
		{"quoted key inside inline table", "model = { \"api_key\" = \"xai-abc\" }\n", true},
		{"empty quoted credential", "[model]\n\"api_key\" = \"\"\n", false},
		{"dot inside quotes is one segment", "[other]\n\"model.api_key\" = \"xai-abc\"\n", false},
		{"unrelated quoted key", "[model]\n\"base_url\" = \"https://x\"\n", false},
		// A dotted key inside a table is relative to it, so `[model]` +
		// `grok-4.api_key` is the per-model credential `model.grok-4.api_key`.
		{"dotted key under a section", "[model]\ngrok-4.api_key = \"xai-abc\"\n", true},
		{"quoted dotted key under a section", "[model]\n\"grok-4\".env_key = \"XAI_KEY\"\n", true},
		{"dotted key under an unrelated section", "[other]\ngrok-4.api_key = \"xai-abc\"\n", false},
		// Grok's parser decodes basic-string escapes in a quoted key; leaving
		// them raw yielded `api_u006bey`, which matched no credential key.
		{"unicode escape in quoted key", "[model]\n\"api_\\u006bey\" = \"xai-abc\"\n", true},
		{"long unicode escape in quoted key", "[model]\n\"api_\\U0000006bey\" = \"xai-abc\"\n", true},
		{"escaped quote keeps the key unrelated", "[model]\n\"api\\\"_key\" = \"xai-abc\"\n", false},
		{"literal keys do not decode escapes", "[model]\n'api_\\u006bey' = \"xai-abc\"\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if got := grokConfigPinsCredential(path); got != tc.want {
				t.Fatalf("grokConfigPinsCredential(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestGrokProjectPinnedCredential_ResolvesSymlinkedCwd pins that a cwd reached
// through a symlink still sees the PHYSICAL repository's `.grok/config.toml`.
// The child's process cwd resolves through the link after exec.Cmd chdirs, so a
// lexical-only walk would miss the config grok itself loads.
func TestGrokProjectPinnedCredential_ResolvesSymlinkedCwd(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical")
	work := filepath.Join(physical, "sub")
	if err := os.MkdirAll(filepath.Join(physical, ".grok"), 0o700); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	if err := os.WriteFile(filepath.Join(physical, ".grok", "config.toml"),
		[]byte("[model]\napi_key = \"xai-physical\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	alias := filepath.Join(root, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symlinks unavailable on this platform/account: %v", err)
	}

	if !grokProjectPinnedCredential(filepath.Join(alias, "sub")) {
		t.Fatal("expected the pinned credential on the symlink's physical ancestor to be detected")
	}
	if !grokProjectPinnedCredential(work) {
		t.Fatal("expected the pinned credential on the direct physical path to be detected")
	}
	if grokProjectPinnedCredential(filepath.Join(root, "unrelated")) {
		t.Fatal("did not expect a credential for a directory outside the pinned tree")
	}
}

// TestGrokDirectRunBillingIdentity_ContestsAUserLayerPinnedKey pins that EVERY
// user-level TOML layer inside GROK_HOME is scanned before a direct run is
// attributed, not just `config.toml`. `managed_config.toml` and
// `requirements.toml` are redirected by GROK_HOME — which is exactly why the
// managed path can neutralise them — but a DIRECT (PTY) run reads the user's
// REAL home, so a key pinned in either is a credential the child can bill. A
// config.toml-only scan named the cached login above that spend.
func TestGrokDirectRunBillingIdentity_ContestsAUserLayerPinnedKey(t *testing.T) {
	for _, name := range grokUserConfigFileNames {
		t.Run(name, func(t *testing.T) {
			resetGrokBillingAttribution(t)
			helperNoGrokSystemConfigLayers(t)
			base := helperGrokHomeWithAccount(t, "acct-login")

			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != "acct-login" {
				t.Fatalf("identity = %q, want the cached login before any user layer pins a key", got)
			}

			path := filepath.Join(base, name)
			if err := os.WriteFile(path, []byte("[model]\napi_key = \"xai-user-layer\"\n"), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != grokContestedBillingIdentity {
				t.Fatalf("identity = %q, want the contested sentinel once %s pins a credential", got, name)
			}

			// An empty pin is not a credential and must not cost an honest run
			// its observability.
			if err := os.WriteFile(path, []byte("[model]\napi_key = \"\"\n"), 0o600); err != nil {
				t.Fatalf("rewrite %s: %v", name, err)
			}
			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{}, base); got != "acct-login" {
				t.Fatalf("identity = %q, want the cached login for an empty pin in %s", got, name)
			}
		})
	}
}

// TestGrokConfigPinsCredential_MultilineValues pins that a credential written
// as a TOML multiline string is CONTESTED rather than read as empty. The
// line-oriented sweep only ever sees the opening `"""`, which trims to nothing,
// so the guard reported "no pinned credential" while grok's own parser resolved
// the value and billed the account it names — publishing an API-key account's
// spend under the cached login.
func TestGrokConfigPinsCredential_MultilineValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"multiline env_key", "[model]\nenv_key = \"\"\"\nOTHER_KEY_VAR\"\"\"\n", true},
		{"multiline api_key", "[model]\napi_key = \"\"\"\nxai-abc\"\"\"\n", true},
		{"literal multiline api_key", "[model]\napi_key = '''\nxai-abc'''\n", true},
		{"per-model multiline api_key", "[model.\"grok-4\"]\napi_key = \"\"\"\nxai-abc\"\"\"\n", true},
		// A multiline that OPENS AND CLOSES on one line is fully visible to the
		// sweep, so it is read normally rather than contested by shape alone.
		{"single-line multiline", "[model]\napi_key = \"\"\"xai-abc\"\"\"\n", true},
		{"empty single-line multiline", "[model]\napi_key = \"\"\"\"\"\"\n", false},
		// An unrelated multiline may not cost an honest run its observability.
		{"unrelated multiline", "[model]\nsystem_prompt = \"\"\"\nbe helpful\"\"\"\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if got := grokConfigPinsCredential(path); got != tc.want {
				t.Fatalf("grokConfigPinsCredential(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestReadGrokPersistedAPIKey_ReEncodesAQuotedModelSection pins that the
// section header handed to setupIsolatedGrokHomeWithSessionStore is a TOML key
// PATH, not a decoded string. A model whose name needs a quoted key emitted
// `[model.grok.4]` — a nested table rather than that model — so the copied
// config passed the line-based preflight and started the child with no usable
// credential.
func TestReadGrokPersistedAPIKey_ReEncodesAQuotedModelSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path,
		[]byte("[model.\"grok.4\"]\napi_key = \"xai-per-model\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	section, value := readGrokPersistedAPIKey(path, "grok.4")
	if value != "\"xai-per-model\"" {
		t.Fatalf("value = %q, want the per-model key", value)
	}
	if section != "model.\"grok.4\"" {
		t.Fatalf("section = %q, want the re-encoded quoted key path", section)
	}
	// The header the isolated config would carry must round-trip back to the
	// same segments the sweep matched on.
	if got := splitGrokTOMLKeyPath(section); len(got) != 2 || got[0] != "model" || got[1] != "grok.4" {
		t.Fatalf("round-trip of %q = %v, want [model grok.4]", section, got)
	}
	// A bare model name is still emitted bare: no gratuitous re-quoting of the
	// header every existing config already carries.
	if err := os.WriteFile(path,
		[]byte("[model.grok-4]\napi_key = \"xai-per-model\"\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if section, _ := readGrokPersistedAPIKey(path, "grok-4"); section != "model.grok-4" {
		t.Fatalf("section = %q, want the bare key path", section)
	}
}

// TestReadGrokPersistedAPIKey_SkipsAMultilineValue pins that a value this sweep
// can only see the first line of is NOT carried into the isolated config.
// Copying `api_key = """` verbatim writes a truncated string that breaks the
// whole file for the child's parser; grokConfigPinsCredential still contests
// the same config, so the run loses its carryover and never its attribution.
func TestReadGrokPersistedAPIKey_SkipsAMultilineValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path,
		[]byte("[model]\napi_key = \"\"\"\nxai-abc\"\"\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if section, value := readGrokPersistedAPIKey(path, "grok-4"); section != "" || value != "" {
		t.Fatalf("section=%q value=%q, want a skipped multiline value", section, value)
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("the same config must still contest attribution")
	}
}

// A config-loader variable carries no credential itself, which is why it is not
// in grokCredentialOverrideEnvVars — it points xAI's loader at a configuration
// none of the scanned layers contain. GROK_MANAGED_CONFIG_URL resolves over the
// network from a document this process never sees, so it contests on presence;
// GROK_CONFIG_PATH names a local file, so it gets the same credential sweep
// every other layer gets and only contests when it pins one (or cannot be read).
func TestGrokDirectRunBillingIdentity_ContestsAConfigLoaderOverride(t *testing.T) {
	resetGrokBillingAttribution(t)
	helperNoGrokSystemConfigLayers(t)
	base := helperGrokHomeWithAccount(t, "acct-login")

	dir := t.TempDir()
	pinned := filepath.Join(dir, "pinned.toml")
	if err := os.WriteFile(pinned, []byte("[model]\napi_key = \"xai-loader-sentinel\"\n"), 0o600); err != nil {
		t.Fatalf("write pinned loader config: %v", err)
	}
	clean := filepath.Join(dir, "clean.toml")
	if err := os.WriteFile(clean, []byte("[model]\ndefault = \"grok-4\"\n"), 0o600); err != nil {
		t.Fatalf("write clean loader config: %v", err)
	}
	perModel := filepath.Join(dir, "per-model.toml")
	if err := os.WriteFile(perModel, []byte("[model.grok-4-fast]\nenv_key = \"OTHER_ACCOUNT_KEY\"\n"), 0o600); err != nil {
		t.Fatalf("write per-model loader config: %v", err)
	}

	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"managed config url", []string{"GROK_MANAGED_CONFIG_URL=https://config-sentinel.invalid/grok.toml"}, grokContestedBillingIdentity},
		{"config path pinning a key", []string{"GROK_CONFIG_PATH=" + pinned}, grokContestedBillingIdentity},
		{"config path pinning a per-model env key", []string{"GROK_CONFIG_PATH=" + perModel}, grokContestedBillingIdentity},
		{"lowercase spelling is still recognised", []string{"grok_config_path=" + pinned}, grokContestedBillingIdentity},
		// "We could not look" is not "there is nothing there": the child may
		// still resolve a path we cannot, so an unreadable one fails closed.
		{"unreadable config path", []string{"GROK_CONFIG_PATH=" + filepath.Join(dir, "absent.toml")}, grokContestedBillingIdentity},
		// An honest session keeps its observability: a loader pointed at an
		// ordinary config and an empty value must still name the cached login.
		{"config path with no credential", []string{"GROK_CONFIG_PATH=" + clean}, "acct-login"},
		{"empty config path", []string{"GROK_CONFIG_PATH="}, "acct-login"},
		{"empty managed config url", []string{"GROK_MANAGED_CONFIG_URL="}, "acct-login"},
		{"unrelated variable", []string{"GROK_DEFAULT_MODEL=grok-4"}, "acct-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Env: tc.env}, base)
			if got != tc.want {
				t.Fatalf("identity = %q, want %q for env %v", got, tc.want, tc.env)
			}
		})
	}
}

// TestGrokConfigPinsCredential_ContestsATruncatedScan pins that a config the
// sweep cannot read to the end CONTESTS attribution instead of reporting "no
// pinned credential". The child's parser reads the whole file, so a
// `model.api_key` past the 1 MiB tail bound (or past the scanner's line buffer)
// would bill the API-key account while direct attribution named the cached
// login — one subscription's spend published as another's.
func TestGrokConfigPinsCredential_ContestsATruncatedScan(t *testing.T) {
	filler := strings.Repeat("# padding to push the credential past the bound\n", (1<<20)/48+64)

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "credential beyond the tail bound",
			body: "[model]\n" + filler + "api_key = \"xai-beyond-the-bound\"\n",
			want: true,
		},
		{
			name: "no credential but the file is truncated anyway",
			body: "[other]\n" + filler + "note = \"nothing here\"\n",
			want: true,
		},
		{
			name: "line longer than the scanner buffer",
			body: "[model]\nnote = \"" + strings.Repeat("x", 512*1024) + "\"\napi_key = \"xai-after-a-huge-line\"\n",
			want: true,
		},
		{
			name: "complete file with no credential still reports none",
			body: "[model]\nnote = \"nothing here\"\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			if got := grokConfigPinsCredential(path); got != tc.want {
				t.Fatalf("grokConfigPinsCredential = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGrokConfigPinsCredential_AbsentLayerIsNotContested pins the other side of
// the same boundary: an optional layer that simply does not exist is nothing to
// read, not an incomplete scan. Failing closed there would contest every run on
// every device that never wrote a project or system config.
func TestGrokConfigPinsCredential_AbsentLayerIsNotContested(t *testing.T) {
	if grokConfigPinsCredential(filepath.Join(t.TempDir(), "missing.toml")) {
		t.Fatal("an absent config layer must not contest attribution")
	}
	if grokConfigPinsCredential("") {
		t.Fatal("an empty path must not contest attribution")
	}
}
