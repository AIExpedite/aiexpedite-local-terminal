// cliagent_usage_musecode.go — Meta Muse Code (`muse`) usage parser.
//
// What the card shows, and where each piece comes from:
//
//   - Readiness: binary, version, and whether the CLI has a credential.
//   - Account: the signed-in email from the credential file's identity fields
//     (`providers.meta.user_email`), which also keys the cross-device dedup.
//   - Device default model: `model` in Muse Code's settings.json.
//   - Quota: the 5-hour window and the weekly block, from the last reading a
//     live probe captured (cliagent_usage_musecode_live.go). Muse Code keeps
//     these only in memory and on no disk file, so without a reading both
//     metrics are reported Unknown — the card draws dashed bars that say the
//     limit exists but is unobserved, rather than no bars at all.
//
// # WHY AUTH FAILS OPEN
//
// Muse Code authenticates by `muse login` (browser / device code) or
// `muse auth set` / META_API_KEY, and has no auth-status command. Muse Code
// 1.4.0 stores the credential in the OS keychain and falls back to `auth.json`
// under an XDG-style config home (named in its binary; the launcher also
// honours MUSE_AUTH_PATH). That layout holds on Windows too: the same install
// keeps its data in ~/.local/share/muse, and %APPDATA%\muse does not exist. A
// keychain login leaves no file to find, so only POSITIVE evidence is
// reported: an API key in the environment or the fallback `auth.json` reads
// "ready"; anything else reads "unknown", which matches none of the frontend's
// Login-required branches. Reporting "unauthenticated" on a guessed path would
// paint a red chip on a working install the user cannot act on — the worst
// failure this parser can have.
//
// SECRETS: the credential file is decoded into structs that name ONLY the
// identity fields — or, when no email identifies the account, only the API key,
// which is immediately reduced to a one-way fingerprint. The access token is
// never read; nothing secret is logged, cached or published.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	museCodeAuthReady   = "ready"
	museCodeAuthUnknown = "unknown"
)

// museCodeConfigFileMaxBytes bounds a read of auth.json / settings.json; both
// are well under a kilobyte.
const museCodeConfigFileMaxBytes = 256 * 1024

type museCodeUsageParser struct{}

func (museCodeUsageParser) Provider() string { return "museCode" }

// museCodeCredentialFileName is the keychain-fallback credential file in the
// Muse Code config dir. A miss degrades to "unknown", never to Login-required.
const museCodeCredentialFileName = "auth.json"

// museCodeSettingsFileName holds the user's default provider/model/effort.
const museCodeSettingsFileName = "settings.json"

func (p museCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	usage := &cliAgentUsage{
		CliAgentID:  "museCode",
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Muse Code"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "muse credentials",
		CollectedAt: now.UTC().Format(time.RFC3339),
		// Muse Code reports no login deadline of its own.
		LoginExpirationState: loginExpirationNotReported,
	}
	if museCodeHasCredential(home, os.Getenv) {
		usage.AuthState = museCodeAuthReady
		usage.Authenticated = authBoolPtr(true)
	} else {
		// Authenticated stays NIL: the frontend reads `false` as a hard
		// Login-required signal.
		usage.AuthState = museCodeAuthUnknown
	}
	identity := museCodeAccountFor(home, os.Getenv)
	usage.Account = identity.Email
	usage.AccountFingerprint = identity.Fingerprint
	usage.Model = readMuseCodeDefaultModel(home, os.Getenv)

	reading, ok := loadMuseCodeUsageLive(museCodeUsageAccountKey(usage.AccountFingerprint))
	if ok {
		usage.DataSource = "muse usage/read"
	}
	usage.Metrics = museCodeUsageMetrics(reading, ok, now)
	return usage, true
}

// museCodeHasCredential reports positive evidence of a usable credential.
func museCodeHasCredential(home string, getenv func(string) string) bool {
	if strings.TrimSpace(getenv("META_API_KEY")) != "" {
		return true
	}
	for _, path := range museCodeCredentialPaths(home, getenv) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() > 0 {
			return true
		}
	}
	return false
}

// museCodeConfigDirs returns candidate config dirs: $XDG_CONFIG_HOME/muse,
// then ~/.config/muse, on every OS (see the file comment).
func museCodeConfigDirs(home string, getenv func(string) string) []string {
	var dirs []string
	if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "muse"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", "muse"))
	}
	return dirs
}

// museCodeCredentialPaths lists the credential files Muse Code would read, in
// its own order: MUSE_AUTH_PATH (the launcher's override), then auth.json in
// each config dir.
func museCodeCredentialPaths(home string, getenv func(string) string) []string {
	var paths []string
	if p := strings.TrimSpace(getenv("MUSE_AUTH_PATH")); p != "" {
		paths = append(paths, p)
	}
	for _, dir := range museCodeConfigDirs(home, getenv) {
		paths = append(paths, filepath.Join(dir, museCodeCredentialFileName))
	}
	return paths
}

// museCodeAuthIdentityFile names ONLY the identity fields of auth.json. The
// token and key beside them are skipped by the decoder, never stored.
type museCodeAuthIdentityFile struct {
	Providers struct {
		Meta struct {
			UserEmail string `json:"user_email"`
		} `json:"meta"`
	} `json:"providers"`
}

// museCodeAccountFingerprint keys the account for cross-device dedup. Emails
// compare case-insensitively.
func museCodeAccountFingerprint(email string) string {
	return fingerprintAccount("museCode", strings.ToLower(strings.TrimSpace(email)))
}

// museCodeAPIKeyFingerprint keys an API-key account: one-way (sha256,
// provider-salted, truncated), so the key itself never leaves this function.
func museCodeAPIKeyFingerprint(key string) string {
	return fingerprintAccount("museCode:api-key", strings.TrimSpace(key))
}

// museCodeAccount is who Muse Code runs as: Email when an OAuth login names
// it, and Fingerprint, the opaque key the usage cache, the model cache and
// cross-device dedup use. Fingerprint is "" only for a keychain login, which
// leaves nothing on disk to tell accounts apart.
type museCodeAccount struct {
	Email       string
	Fingerprint string
}

// museCodeAuthAPIKeyFile names ONLY the stored API key (`muse auth set`),
// read solely to fingerprint an account that has no email on disk.
type museCodeAuthAPIKeyFile struct {
	Providers struct {
		Meta struct {
			APIKey string `json:"api_key"`
		} `json:"meta"`
	} `json:"providers"`
}

// museCodeAccountFor resolves the account in Muse's own precedence:
// META_API_KEY in the environment overrides any stored login (labelling it
// with the stored email could name the wrong account); then the FIRST
// credential file that exists, in the order Muse reads them. That file is the
// active credential whatever it holds: its OAuth email, else its stored API
// key, names the account, and when it holds neither (e.g. only an access
// token) the account is unknown. A lower-priority file is never consulted
// once a higher one exists, so a MUSE_AUTH_PATH file is never attributed to a
// stale login left in ~/.config/muse.
func museCodeAccountFor(home string, getenv func(string) string) museCodeAccount {
	if key := strings.TrimSpace(getenv("META_API_KEY")); key != "" {
		return museCodeAccount{Fingerprint: museCodeAPIKeyFingerprint(key)}
	}
	for _, path := range museCodeCredentialPaths(home, getenv) {
		if info, err := os.Stat(path); err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		// This is the active credential file; the search ends here.
		var identity museCodeAuthIdentityFile
		if !readBoundedJSONFile(path, &identity) {
			return museCodeAccount{}
		}
		if email := strings.TrimSpace(identity.Providers.Meta.UserEmail); email != "" && strings.Contains(email, "@") && len(email) <= 320 {
			return museCodeAccount{Email: email, Fingerprint: museCodeAccountFingerprint(email)}
		}
		var stored museCodeAuthAPIKeyFile
		if readBoundedJSONFile(path, &stored) && strings.TrimSpace(stored.Providers.Meta.APIKey) != "" {
			return museCodeAccount{Fingerprint: museCodeAPIKeyFingerprint(stored.Providers.Meta.APIKey)}
		}
		return museCodeAccount{}
	}
	return museCodeAccount{}
}

// museCodeAccountUnchanged reports whether the account signed in now still
// maps to accountKey. Every cache write a probe makes is gated on it: a login
// switch while a host ran makes whatever it returned unattributable.
func museCodeAccountUnchanged(accountKey string) bool {
	return museCodeUsageAccountKey(currentMuseCodeAccountFingerprint()) == accountKey
}

// currentMuseCodeAccountFingerprint is the fingerprint of whoever Muse runs as
// right now, or "" for a keychain login.
func currentMuseCodeAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	return museCodeAccountFor(home, os.Getenv).Fingerprint
}

// readMuseCodeDefaultModel returns settings.json's `model`, or "".
func readMuseCodeDefaultModel(home string, getenv func(string) string) string {
	for _, dir := range museCodeConfigDirs(home, getenv) {
		var settings struct {
			Model string `json:"model"`
		}
		if !readBoundedJSONFile(filepath.Join(dir, museCodeSettingsFileName), &settings) {
			continue
		}
		if model := strings.TrimSpace(settings.Model); model != "" && len(model) <= 256 {
			return model
		}
	}
	return ""
}

// readBoundedJSONFile decodes a small JSON file, refusing anything larger than
// museCodeConfigFileMaxBytes. Any failure reads as "absent".
func readBoundedJSONFile(path string, into any) bool {
	return readJSONFileWithin(path, museCodeConfigFileMaxBytes, into)
}

// museCodeUsageMetrics turns the last live reading into the card's two rows.
// A window whose reset has passed describes a window that no longer exists, so
// it reads Unknown (with when it was last observed), as Claude Code's do.
func museCodeUsageMetrics(reading museCodeUsageLiveCache, ok bool, now time.Time) []cliAgentUsageMetric {
	sessionLabel := museCodeWindowLabel(reading.Window.WindowDurationMins)
	if !ok {
		return []cliAgentUsageMetric{
			{Kind: limitKindSession, Label: sessionLabel, Unit: "%", Unknown: true},
			{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true},
		}
	}
	return []cliAgentUsageMetric{
		museCodePercentMetric(limitKindSession, sessionLabel, reading.Window.UsedPercent, reading.Window.ResetsAtMs, reading.ObservedAtMs, now),
		museCodePercentMetric(limitKindWeekly, "Weekly quota", reading.Weekly.UsedPercent, reading.Weekly.ResetsAtMs, reading.ObservedAtMs, now),
	}
}

func museCodeWindowLabel(durationMins int) string {
	if durationMins <= 0 || durationMins == 300 {
		return "5-hour session window"
	}
	if durationMins%60 == 0 {
		return fmt.Sprintf("%d-hour session window", durationMins/60)
	}
	return fmt.Sprintf("%d-minute session window", durationMins)
}

func museCodePercentMetric(kind, label string, usedPercent int, resetsAtMs, observedAtMs int64, now time.Time) cliAgentUsageMetric {
	observedAt := observedAtRFC3339(observedAtMs)
	if resetsAtMs <= 0 || !time.UnixMilli(resetsAtMs).After(now) {
		return cliAgentUsageMetric{Kind: kind, Label: label, Unit: "%", ObservedAt: observedAt, Unknown: true}
	}
	used := clampPercent(float64(usedPercent))
	return cliAgentUsageMetric{
		Kind: kind, Label: label, Unit: "%",
		Total: floatPtr(100), Consumed: floatPtr(used), Remaining: floatPtr(100 - used),
		ResetAt:    time.UnixMilli(resetsAtMs).UTC().Format(time.RFC3339),
		ObservedAt: observedAt,
	}
}
