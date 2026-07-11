// cliagent_usage_claudecode.go — Claude Code usage parser.
//
// Claude Code exposes no public utilization API and no on-disk usage file.
// What it DOES emit is a structured `rate_limit_event` on its stream-json
// stdout during a session, carrying the exact reset time and utilization per
// window. session.go captures those events into a small on-disk cache
// (cliagent_ratelimit.go); this parser turns the latest snapshot into the
// real five-hour / weekly capacity metrics shown on the CLI Agents tab.
//
// When the cache is absent (no Claude session has run since install, or the
// account isn't a Claude.ai subscription so no rate_limit_event is emitted),
// the metrics fall back to Unknown=true and the UI renders the dashed
// "usage unobservable" gauge — same as before.
//
// CLAUDE_CONFIG_DIR/.credentials.json (or ~/.claude/.credentials.json) is still
// read for account fingerprinting + plan display.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type claudeCodeUsageParser struct{}

func (claudeCodeUsageParser) Provider() string { return "claudeCode" }

type claudeCredentials struct {
	Account      string `json:"account"`
	Email        string `json:"email"`
	Organization string `json:"organization"`
	Plan         string `json:"plan"`
	Subscription string `json:"subscription"`
}

// claudeOAuthCredentials mirrors the nested `claudeAiOauth` object Claude Code
// writes to .credentials.json for a claude.ai subscription login. The access
// token (ExpiresAt) auto-refreshes silently in the real ~/.claude using
// RefreshToken, so its short expiry is NOT a re-login signal — the user only has
// to interactively `/login` again once the REFRESH token expires
// (RefreshTokenExpiresAt). That's the real re-authentication deadline.
type claudeOAuthCredentials struct {
	ClaudeAiOauth struct {
		ExpiresAt             int64 `json:"expiresAt"`
		RefreshTokenExpiresAt int64 `json:"refreshTokenExpiresAt"`
	} `json:"claudeAiOauth"`
}

// claudeAuthExpiryWarnWindow is how far ahead of the refresh-token deadline we
// warn. Claude native strips env credentials and requires a subscription
// /login, so an expired OAuth session stalls a chat headlessly (the same class
// of failure Grok hits).
const claudeAuthExpiryWarnWindow = 48 * time.Hour

// claudeKeychainReader returns the raw Claude credential JSON from the platform
// credential store used when Claude Code does NOT persist it to
// ~/.claude/.credentials.json — on macOS the login lives in the encrypted
// Keychain, not on disk (https://code.claude.com/docs/en/authentication).
// Overridable in tests.
var claudeKeychainReader = readClaudeKeychainCredential

// claudeKeychainReadTimeout bounds the macOS `security find-generic-password`
// call. That command can block on a Keychain unlock/access prompt (Apple docs:
// the Keychain may require a password after inactivity or ask whether an app may
// retrieve a password), and this parser runs from gatherCLIAgentUsage with no
// per-call deadline — a waiting child would otherwise hang the whole CLI-usage
// refresh. On timeout we treat it as "no credential" and fall through.
const claudeKeychainReadTimeout = 3 * time.Second

// readClaudeKeychainCredential reads the macOS Keychain generic-password item
// Claude Code stores its credential JSON under. No-op (nil,false) off Darwin or
// on any error, so callers transparently fall back to "no credential" on Linux/
// Windows (where the on-disk file IS authoritative) and on a fresh macOS box.
func readClaudeKeychainCredential() ([]byte, bool) {
	if runtime.GOOS != "darwin" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeKeychainReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(
		ctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w",
	).Output()
	if err != nil {
		return nil, false
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, false
	}
	return trimmed, true
}

// readClaudeCredentialsRaw returns the raw Claude credential JSON — from the
// macOS Keychain when it holds the active login, otherwise from the on-disk
// .credentials.json. (nil,false) when neither yields a credential.
//
// On macOS the active login lives in the encrypted Keychain, and an upgraded box
// can still carry a STALE ~/.claude/.credentials.json left behind by an older
// Claude Code that wrote credentials to disk. So on Darwin we PREFER the Keychain
// credential — reading the stale file instead would fingerprint the wrong account
// or raise a false expired-login banner. claudeKeychainReader is a no-op off
// Darwin (and on a Mac with no Keychain item), so Linux/Windows and file-only
// installs transparently fall through to the on-disk file.
//
// The Keychain is only consulted for the DEFAULT config dir. The macOS Keychain
// item ("Claude Code-credentials") is a single shared entry that reflects the
// default account, so when the RESOLVED config dir is a non-default side-by-side
// profile (per Claude's env-vars docs), reading the Keychain would misattribute
// that profile's quota and auth-expiry notice to the default account. For a
// non-default profile we read only its on-disk file, and report "no credential"
// when it is absent instead of guessing.
func readClaudeCredentialsRaw(home, base string) ([]byte, bool) {
	if isDefaultClaudeConfigDir(home, base) {
		if raw, ok := claudeKeychainReader(); ok {
			return raw, true
		}
	}
	if raw, err := os.ReadFile(expandHome(base, ".credentials.json")); err == nil {
		return raw, true
	}
	return nil, false
}

// isDefaultClaudeConfigDir reports whether the RESOLVED config dir `base` is
// Claude Code's default ~/.claude — the only case where the shared macOS
// Keychain credential is guaranteed to belong to the account we're inspecting.
// It keys off the resolved path rather than "CLAUDE_CONFIG_DIR unset" so that
// explicitly pointing CLAUDE_CONFIG_DIR at the default dir (a supported way to
// move history/settings while still using the default login) still consults the
// Keychain, while a genuine side-by-side profile (e.g. ~/.claude-work) does not.
func isDefaultClaudeConfigDir(home, base string) bool {
	def := expandHome(home, ".claude")
	if def == "" || base == "" {
		return false
	}
	return filepath.Clean(base) == filepath.Clean(def)
}

// claudeSettings mirrors the Claude Code settings.json keys that change WHICH
// credential a spawned `claude` authenticates with. `apiKeyHelper` is a script
// Claude Code runs to mint an API key / auth token; when it is set the child
// authenticates through that helper INSTEAD of the claude.ai subscription
// /login, so a stale/expired `claudeAiOauth` blob is not a real re-login signal.
type claudeSettings struct {
	APIKeyHelper string `json:"apiKeyHelper"`
}

// claudeChildUsesHelperAuth reports whether a Claude Code `apiKeyHelper` is
// configured in the settings that apply to the driver's spawned `claude` child.
// The child runs under the DEFAULT ~/.claude config dir (sanitizeClaudeChildEnv
// strips CLAUDE_CONFIG_DIR), so only the enterprise managed-settings.json
// locations and the default user ~/.claude/settings.json can steer its auth. A
// per-project .claude/settings.json is deliberately NOT consulted: it depends
// on the child's working directory, which this usage scan doesn't know.
//
// When true, the child authenticates via the helper rather than the
// subscription login, so an expired/expiring `claudeAiOauth` deadline must NOT
// raise the "run /login" notice — chat-direct Claude keeps authenticating fine,
// and the warning would be a false alarm. env-based ANTHROPIC_API_KEY /
// ANTHROPIC_AUTH_TOKEN need no equivalent guard: sanitizeClaudeChildEnv strips
// them, so the child falls back to the OAuth login the notice is keyed off.
// Best-effort: missing or malformed settings files are treated as "no helper".
func claudeChildUsesHelperAuth(home string) bool {
	paths := append([]string{}, claudeManagedSettingsPathsFn()...)
	if def := expandHome(home, ".claude"); def != "" {
		paths = append(paths, filepath.Join(def, "settings.json"))
	}
	for _, p := range paths {
		settings := claudeSettings{}
		if readJSONFile(p, &settings) && strings.TrimSpace(settings.APIKeyHelper) != "" {
			return true
		}
	}
	return false
}

// claudeAuthNoticeFromRaw returns a card notice + severity when the claude.ai
// subscription login in a raw .credentials.json blob is (nearly) expired. It
// keys off the REFRESH-token expiry — the access token auto-refreshes, so its
// shorter expiry would false-alarm. Returns ("","") when the refresh token is
// still valid, or when there's no OAuth login (API-key installs have no
// claudeAiOauth object, so absence is not a reliable "signed out" signal).
func claudeAuthNoticeFromRaw(raw []byte, now time.Time) (string, string) {
	creds := claudeOAuthCredentials{}
	if json.Unmarshal(raw, &creds) != nil {
		return "", ""
	}
	deadlineMs := creds.ClaudeAiOauth.RefreshTokenExpiresAt
	if deadlineMs <= 0 {
		return "", "" // no OAuth deadline (API-key or unexpected layout) — don't guess
	}
	deadline := time.UnixMilli(deadlineMs)
	if !deadline.After(now) {
		return "Claude login has expired — run `claude` on the terminal computer and sign in (/login) again.", "error"
	}
	if deadline.Before(now.Add(claudeAuthExpiryWarnWindow)) {
		return "Claude login expires soon — run `claude` on the terminal computer and sign in (/login) to avoid an interrupted session.", "warning"
	}
	return "", ""
}

func (p claudeCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := claudeConfigDir(home)
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Claude Code"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "rate_limit_event",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	// Account/plan fingerprint is scoped to the RESOLVED config dir so a
	// side-by-side profile's quota isn't attributed to the default account (and
	// vice versa). For a non-default profile with no on-disk credential this
	// stays empty rather than borrowing the shared default Keychain identity —
	// the misattribution guard from readClaudeCredentialsRaw.
	profileRaw, profileOK := readClaudeCredentialsRaw(home, base)
	if profileOK {
		creds := claudeCredentials{}
		if json.Unmarshal(profileRaw, &creds) == nil {
			usage.Account = firstNonEmpty(creds.Email, creds.Account, creds.Organization)
			usage.Plan = firstNonEmpty(creds.Plan, creds.Subscription)
		}
	}

	// Proactive auth-expiry notice — keyed off the credential the driver's
	// spawned `claude` child ACTUALLY authenticates with, which is NOT
	// necessarily the profile fingerprinted above. sanitizeClaudeChildEnv strips
	// every CLAUDE_ variable (incl. CLAUDE_CONFIG_DIR) before launching claude
	// (session.go), so the child always falls back to the DEFAULT ~/.claude
	// login: the macOS Keychain, else the default on-disk file. An expired
	// default login stalls that headless session on a `/login` it can't show, so
	// we must warn even when the driver runs under a custom CLAUDE_CONFIG_DIR
	// whose own credential is absent or healthy. In the common case (config dir
	// IS the default) this is the very credential just read, so reuse it rather
	// than pay a second Keychain lookup.
	//
	// One exception: a configured `apiKeyHelper` (settings.json) makes the child
	// authenticate through that helper, not the OAuth login — and it isn't an env
	// var, so sanitizeClaudeChildEnv can't strip it. When it's set an expired
	// claudeAiOauth deadline is not a real re-login signal, so skip the notice
	// rather than false-alarm a correctly-authenticated user.
	childRaw, childOK := profileRaw, profileOK
	if !isDefaultClaudeConfigDir(home, base) {
		childRaw, childOK = readClaudeCredentialsRaw(home, expandHome(home, ".claude"))
	}
	if childOK && !claudeChildUsesHelperAuth(home) {
		if notice, severity := claudeAuthNoticeFromRaw(childRaw, now); notice != "" {
			usage.Notice = notice
			usage.NoticeSeverity = severity
		}
	}

	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	usage.Metrics = claudeCodeMetricsFromCache(now, usage.AccountFingerprint)

	return usage, true
}

// claudeConfigDir resolves the Claude config dir using the same precedence Parse
// uses (CLAUDE_CONFIG_DIR override, then ~/.claude). Empty when neither resolves.
func claudeConfigDir(home string) string {
	return firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), expandHome(home, ".claude"))
}

// currentClaudeAccountFingerprint reads the Claude credentials on disk and
// returns the same fingerprint Parse would attach to a usage snapshot. Used by
// the capture path to scope the rate-limit cache to the active account.
// Returns "" when no creds are readable, in which case the cache is unscoped
// (best-effort, matches pre-scoping behavior).
func currentClaudeAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	raw, ok := readClaudeCredentialsRaw(home, base)
	if !ok {
		return ""
	}
	creds := claudeCredentials{}
	if json.Unmarshal(raw, &creds) != nil {
		return ""
	}
	account := firstNonEmpty(creds.Email, creds.Account, creds.Organization)
	return fingerprintAccount("claudeCode", account)
}

// claudeCodeMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Two rows are always shown so the card layout is stable: the 5-hour session
// window and the weekly window.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one. Two empty fingerprints match (no creds + unscoped
// snapshot — the test/legacy default), but a scoped snapshot is ignored when
// the current account cannot be identified: otherwise `gatherCLIAgentUsage`
// would attribute a previous user's reset windows to the device-scoped
// fallback entry after the local credentials were removed.
func claudeCodeMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	snap, ok := loadClaudeRateLimitSnapshot(claudeRateLimitCachePath())
	buckets := map[string]claudeRateLimitBucket{}
	if ok && snap.AccountFingerprint == currentFingerprint {
		buckets = snap.Buckets
	}

	session := observedMetricOrUnknown(
		buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	// Weekly is reported under seven_day; some plans split it per-model. When
	// both per-model buckets are present we aggregate CONSERVATIVELY so an
	// exhausted Opus quota isn't hidden behind a healthier Sonnet number.
	weekly := aggregateWeeklyMetric(buckets, now)

	return []cliAgentUsageMetric{session, weekly}
}

// aggregateWeeklyMetric reports the worst observed seven-day window: the
// highest used percentage and the soonest reset across the unified `seven_day`
// bucket and any per-model split (`seven_day_sonnet`, `seven_day_opus`). This
// prevents a healthy Sonnet bucket from masking a depleted Opus bucket on
// plans that emit them separately.
func aggregateWeeklyMetric(buckets map[string]claudeRateLimitBucket, now time.Time) cliAgentUsageMetric {
	windowIDs := []string{claudeWindowSevenDay, claudeWindowSevenDaySonnet, claudeWindowSevenDayOpus}
	var (
		observed   bool
		worstUsed  float64
		worstReset int64
	)
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		used := b.UsedPercentage
		liveReset := b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs
		if b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs {
			used = 0 // this sub-window has already rolled over
		}
		used = clampPercent(used)
		// Pair the reset with the bucket that produced worstUsed so the UI
		// reports when the *constraining* quota clears, not when an unrelated
		// healthier bucket happens to reset first. On a tie (e.g. both Sonnet
		// and Opus rejected at 100%), both buckets are equally constraining
		// and the limit doesn't clear until BOTH have reset — track the later
		// reset so we don't tell operators they can retry while another tied
		// bucket is still exhausted.
		switch {
		case !observed || used > worstUsed:
			worstUsed = used
			if liveReset {
				worstReset = b.ResetsAtMs
			} else {
				worstReset = 0
			}
		case used == worstUsed:
			switch {
			case !liveReset:
				// New tied bucket has no known live reset, so we can't say
				// when the combined constraint clears — surface Unknown.
				worstReset = 0
			case worstReset > 0 && b.ResetsAtMs > worstReset:
				worstReset = b.ResetsAtMs
			}
		}
		observed = true
	}
	if !observed {
		return cliAgentUsageMetric{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true}
	}
	var resetAt string
	if worstReset > 0 {
		resetAt = time.UnixMilli(worstReset).UTC().Format(time.RFC3339)
	}
	return cliAgentUsageMetric{
		Kind:      limitKindWeekly,
		Label:     "Weekly quota",
		Unit:      "%",
		Total:     floatPtr(100),
		Consumed:  floatPtr(worstUsed),
		Remaining: floatPtr(100 - worstUsed),
		ResetAt:   resetAt,
	}
}

// observedMetricOrUnknown returns a real percentage metric for the first window
// id present in the cache, or an Unknown placeholder when none is observed. A
// window whose reset time has already passed is reported as 0% used (the window
// rolled over), which is more honest than showing a stale high-water mark.
func observedMetricOrUnknown(
	buckets map[string]claudeRateLimitBucket,
	windowIDs []string,
	kind, label string,
	now time.Time,
) cliAgentUsageMetric {
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		used := b.UsedPercentage
		var resetAt string
		if b.ResetsAtMs > 0 {
			if now.UnixMilli() >= b.ResetsAtMs {
				used = 0 // window has reset since we last observed it
			} else {
				resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
			}
		}
		used = clampPercent(used)
		return cliAgentUsageMetric{
			Kind:      kind,
			Label:     label,
			Unit:      "%",
			Total:     floatPtr(100),
			Consumed:  floatPtr(used),
			Remaining: floatPtr(100 - used),
			ResetAt:   resetAt,
		}
	}
	return cliAgentUsageMetric{Kind: kind, Label: label, Unit: "%", Unknown: true}
}
