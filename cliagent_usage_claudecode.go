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
	"sort"
	"strings"
	"time"
)

type claudeCodeUsageParser struct{}

func (claudeCodeUsageParser) Provider() string { return "claudeCode" }

var claudeAuthStatusProbe = func(path string) (bool, bool) {
	if path == "" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	probeEnv, _ := prepareClaudeChildEnv(path, os.Environ())
	out, err := runClaudeAuthStatusCommand(ctx, path, probeEnv)
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	// Claude intentionally exits 1 for a logged-out status while still emitting
	// definitive JSON. Parse stdout first; only treat the command error as
	// inconclusive when no documented status payload was returned.
	if json.Unmarshal(out, &status) == nil && status.LoggedIn != nil {
		return *status.LoggedIn, true
	}
	if err != nil {
		return false, false
	}
	return false, false
}

var runClaudeAuthStatusCommand = func(ctx context.Context, path string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Env = env
	// Background probe: the agent is a tray app with no console of its own, so
	// a console child would otherwise flash a window on the user's desktop
	// every time usage/rate-limit collection runs. Same treatment as every
	// other silent probe (see probeVersionArgs in systemInfo.go).
	hideWindow(cmd)
	return cmd.Output()
}

// claudeDotJSON mirrors the (non-secret) `oauthAccount` block Claude Code writes
// to ~/.claude.json — the main config file, distinct from .credentials.json.
// This is the ONLY on-disk place the signed-in account email lives: a
// claude.ai `/login` populates emailAddress / displayName / organizationName
// here, while .credentials.json holds only the OAuth token object (no email).
// Reading it lets the CLI Agents card show `Account: …` for Claude Code, the
// same way Codex surfaces it from ~/.codex/auth.json. Contains NO tokens, so
// it's safe to read alongside the credential probe.
//
// https://code.claude.com/docs/en/authentication documents this layout; note
// that an unhydrated block (Windows Desktop / SSO Team-plan — anthropics/
// claude-code#57026) leaves emailAddress empty, in which case we degrade to no
// account line, exactly as before this fallback existed.
type claudeDotJSON struct {
	OAuthAccount struct {
		EmailAddress string `json:"emailAddress"`
		DisplayName  string `json:"displayName"`
	} `json:"oauthAccount"`
	// customApiKeyResponses records the user's one-time approve/decline decision
	// for an ANTHROPIC_API_KEY seen in the environment. Each entry is the LAST 20
	// CHARACTERS of the key (Claude Code never stores the full secret). A key that
	// is present in the env but NOT in `approved` is not the active credential —
	// Claude keeps using the stored /login — so its rate_limits still belong to
	// the oauthAccount.
	CustomApiKeyResponses struct {
		Approved []string `json:"approved"`
	} `json:"customApiKeyResponses"`
}

// claudeOAuthCredentials mirrors .credentials.json: the account identifiers
// older/API-key layouts write at the top level, plus the nested `claudeAiOauth`
// object Claude Code writes for a claude.ai subscription login. The access
// token (ExpiresAt) auto-refreshes silently in the real ~/.claude using
// RefreshToken, so its short expiry is NOT a re-login signal.
//
// RefreshTokenExpiresAt is NOT the re-authentication deadline either, and must
// never be read as one. Observed on a healthy, actively used Max login: a
// credential written at 17:31 carried RefreshTokenExpiresAt 20:58 and ExpiresAt
// 01:31 — the refresh stamp was SHORTER than the access stamp, and both were
// hours out. Claude Code rolls both forward on each silent refresh and only
// rewrites the file when it refreshes, so any idle stretch leaves both stamps in
// the past while `claude auth status` still reports a signed-in session. Treating
// the stamp as a deadline reported "Login expired" on a working login and (via
// the error notice) blanked every usage bar on the card.
//
// Nothing on disk distinguishes "logged out" from "idle since the last
// refresh", so the live `claude auth status --json` probe is the only
// authoritative signal — the credential file answers only "is there a login
// here, and can it renew itself". RefreshTokenExpiresAt is therefore parsed
// (documenting the field we deliberately ignore) but never consulted.
//
// SubscriptionType is the plan Claude Code itself reports in /usage → ACCOUNT
// ("max", "pro", …) and is the ONLY place the plan is written: there is no
// top-level `plan`/`subscription` key, so the card's plan chip is driven from
// here. Published verbatim (lowercase) — the card capitalizes it for display.
type claudeOAuthCredentials struct {
	Account       string `json:"account"`
	Email         string `json:"email"`
	Organization  string `json:"organization"`
	ClaudeAiOauth struct {
		AccessToken           string `json:"accessToken"`
		RefreshToken          string `json:"refreshToken"`
		ExpiresAt             int64  `json:"expiresAt"`
		RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt"`
		SubscriptionType      string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// claudeCredentialAccount is the stable, per-config-dir identity used for
// fingerprinting and cache scoping. Empty for the common claude.ai login, whose
// credential carries only the OAuth token object.
func (c claudeOAuthCredentials) claudeCredentialAccount() string {
	return firstNonEmpty(c.Email, c.Account, c.Organization)
}

// claudeKeychainReader returns the raw Claude credential JSON from the platform
// credential store used when Claude Code does NOT persist it to
// ~/.claude/.credentials.json — on macOS the login lives in the encrypted
// Keychain, not on disk (https://code.claude.com/docs/en/authentication).
// Overridable in tests.
var claudeKeychainReader = readClaudeKeychainCredential

// readClaudeKeychainCredential reads the macOS Keychain generic-password item
// Claude Code stores its credential JSON under. No-op (nil,false) off Darwin or
// on any error, so callers transparently fall back to "no credential" on Linux/
// Windows (where the on-disk file IS authoritative) and on a fresh macOS box.
//
// The `security` call runs under machineInfoProbeTimeout so a locked Keychain or
// an access prompt can't hang the 10s/20s usage/inspect gather contexts (it would
// otherwise leave stuck processes on __env_inspect__ timeouts); on timeout we
// return "no credential" and fall through to the on-disk file.
func readClaudeKeychainCredential() ([]byte, bool) {
	if runtime.GOOS != "darwin" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	keychainCmd := exec.CommandContext(
		ctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w",
	)
	hideWindow(keychainCmd) // no-op off Windows; kept uniform with the other probes
	out, err := keychainCmd.Output()
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
// default account, so when a non-default CLAUDE_CONFIG_DIR selects a side-by-side
// profile (per Claude's env-vars docs), reading the Keychain would misattribute
// that profile's quota and auth-expiry notice to the default account. For a
// non-default profile we read only its on-disk file, and report "no credential"
// when it is absent instead of guessing.
func readClaudeCredentialsRaw(base string) ([]byte, bool) {
	if usingDefaultClaudeConfigDir() {
		if raw, ok := claudeKeychainReader(); ok {
			return raw, true
		}
	}
	if raw, err := os.ReadFile(expandHome(base, ".credentials.json")); err == nil {
		return raw, true
	}
	return nil, false
}

// usingDefaultClaudeConfigDir reports whether Claude Code is resolving to its
// default ~/.claude config dir (CLAUDE_CONFIG_DIR unset/empty) — the only case
// where the shared macOS Keychain credential is guaranteed to belong to the
// account we're inspecting.
func usingDefaultClaudeConfigDir() bool {
	return os.Getenv("CLAUDE_CONFIG_DIR") == ""
}

// claudeCredentialExpiry describes what the on-disk credential can honestly say
// about the login: the access token's expiry stamp, and whether the credential
// carries a refresh token (so Claude Code renews it without a re-login).
//
// A renewable credential's stamp is informational ONLY — the card labels it
// "(renews automatically)" and no code path may turn it into an expired verdict.
// This matches the Codex parser, which reports the access-token expiry for a
// refreshable credential and never flips authState on it.
func claudeCredentialExpiry(creds claudeOAuthCredentials) (deadlineMs int64, renewable bool) {
	oauth := creds.ClaudeAiOauth
	return oauth.ExpiresAt, oauth.RefreshToken != ""
}

// claudeAuthNotice returns a card notice + severity when the credential ITSELF
// proves the login is unusable — an access-token-only credential (no refresh
// token) whose token has already expired, which nothing can renew.
//
// A renewable credential never produces a notice here: its stamps go stale
// whenever Claude Code has been idle, so the only trustworthy sign-out signal is
// the live `claude auth status` probe in Parse. There is deliberately no
// "expires soon" warning any more — with the refresh stamp discredited, no field
// on disk states a future re-login date, and an access token's few-hour expiry
// would fire the warning permanently.
func claudeAuthNotice(creds claudeOAuthCredentials, now time.Time) (string, string) {
	deadlineMs, renewable := claudeCredentialExpiry(creds)
	if renewable || deadlineMs <= 0 {
		return "", "" // renewable, API-key, or unexpected layout — don't guess
	}
	if !time.UnixMilli(deadlineMs).After(now) {
		return "Claude login has expired — run `claude` on the terminal computer and sign in (/login) again.", "error"
	}
	return "", ""
}

// applyClaudeAuthState publishes the auth state the CREDENTIAL supports, and
// reports whether a usable credential was found at all. Parse then lets the live
// probe override this in either direction.
func applyClaudeAuthState(usage *cliAgentUsage, creds claudeOAuthCredentials, now time.Time) bool {
	if usage == nil {
		return false
	}
	oauth := creds.ClaudeAiOauth
	if oauth.AccessToken == "" && oauth.RefreshToken == "" {
		return false
	}
	usage.Authenticated = authBoolPtr(true)
	usage.AuthState = "authenticated"
	usage.LoginExpirationState = loginExpirationNotReported
	deadlineMs, renewable := claudeCredentialExpiry(creds)
	if deadlineMs <= 0 {
		if renewable {
			usage.LoginExpirationState = loginExpirationRefreshable
		}
		return true
	}
	deadline := time.UnixMilli(deadlineMs).UTC()
	usage.LoginExpiresAt = deadline.Format(time.RFC3339)
	if renewable {
		// Renewable: report the stamp so the row is not permanently blank, but
		// leave the login authenticated past it — the next request renews it.
		usage.LoginExpirationState = loginExpirationRefreshable
		return true
	}
	usage.LoginExpirationState = loginExpirationKnown
	if !deadline.After(now) {
		usage.Authenticated = authBoolPtr(false)
		usage.AuthState = "expired"
	}
	return true
}

// claudeDeadlinePassed reports whether the published login deadline is in the
// past. Unparseable/absent stamps read as "not passed" so a formatting change
// can never manufacture an expiry.
func claudeDeadlinePassed(usage *cliAgentUsage, now time.Time) bool {
	if usage == nil || usage.LoginExpiresAt == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339, usage.LoginExpiresAt)
	if err != nil {
		return false
	}
	return !deadline.After(now)
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

	// Read AND decode the credential ONCE (file, or macOS Keychain), then use the
	// decoded value for the account fingerprint, the plan chip, and the
	// auth-expiry notice — one shape, so those three can never disagree.
	credentialFound := false
	credentialUsable := false
	// Whether the notice on the card was authored from the credential's own
	// expiry. Only that notice may be withdrawn by a definite signed-in probe —
	// a notice from any other source is not this block's to clear.
	authNoticeApplied := false
	if raw, ok := readClaudeCredentialsRaw(base); ok {
		credentialFound = true
		creds := claudeOAuthCredentials{}
		if json.Unmarshal(raw, &creds) == nil {
			credentialUsable = applyClaudeAuthState(usage, creds, now)
			usage.Account = creds.claudeCredentialAccount()
			usage.Plan = creds.ClaudeAiOauth.SubscriptionType

			// Auth notice for a credential that cannot renew itself. Chat-direct
			// Claude spawns `claude --output-format stream-json` with env
			// credentials stripped (claude_auth_detect.go) so it MUST use the
			// claude.ai subscription login — an unusable one stalls the session on
			// a `/login` it can't show headlessly. Surface it in this regular usage
			// scan so the dashboard prompts a re-login first. A definite signed-in
			// probe below withdraws it; only a lapsed NON-renewable credential can
			// raise it, so a renewable login going idle never does.
			if notice, severity := claudeAuthNotice(creds, now); notice != "" {
				usage.Notice = notice
				usage.NoticeSeverity = severity
				authNoticeApplied = true
			}
		}
	}

	// The account from .credentials.json is the ONLY identity used for scoping —
	// it is stable per config dir. Everything downstream (the published/dedup
	// fingerprint AND the rate-limit cache read) uses this one identity so they
	// can never disagree.
	credsAccount := usage.Account

	// .credentials.json carries only the OAuth token object, so the account
	// above is almost always empty. The signed-in email lives in ~/.claude.json's
	// oauthAccount block — surface it as the card's DISPLAY label so the account
	// line shows like Codex's, exactly the same feature the ticket asked for.
	//
	// DISPLAY ONLY — never a fingerprint, and never used to scope/gate the cached
	// metrics. Claude's status-line/stream telemetry does NOT include the session's
	// authenticated account (anthropics/claude-code#17909), and ~/.claude.json is a
	// single shared file any concurrent session rewrites on `/login`. So there is
	// no reliable way to tie a captured window to a specific account: reading the
	// dotfile at capture time can stamp one session's usage with another session's
	// login. We therefore treat the email as "the account currently signed in on
	// this device" and leave the metrics DEVICE-scoped (the credential-only
	// fingerprint is empty for claude.ai, so gatherCLIAgentUsage device-scopes it).
	// In the common single-account case the label and the device's own captures
	// agree; in the rare concurrent/just-switched multi-account case the label may
	// momentarily lead the cached numbers until the next capture — a bounded,
	// self-healing display skew, not a cross-account data leak (the metrics are
	// this device's own usage regardless of which email is shown).
	if usage.Account == "" {
		email, displayName := claudeDotJSONAccount(home)
		usage.Account = firstNonEmpty(email, displayName) // label only
	}

	// One identity for publish + dedup + cache scope: the credential account.
	// Empty for the common claude.ai login, in which case gatherCLIAgentUsage
	// device-scopes the fingerprint and the cache read is unscoped ("") — matching
	// what the status-line hook / stream-capture wrote.
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), credsAccount)
	usage.Metrics = claudeCodeMetricsFromCache(now, usage.AccountFingerprint)
	// `claude auth status --json` is the only authoritative signal, so it decides
	// in BOTH directions. It previously could not clear a credential-derived
	// "expired" (`else if usage.AuthState != "expired"`), which is exactly the
	// state a stale-but-valid credential produces — a definite loggedIn:true was
	// discarded, the card read "Login expired", and the error notice blanked
	// every usage bar below it.
	if loggedIn, known := claudeAuthStatusProbe(detected.Path); known {
		if !loggedIn {
			usage.Authenticated = authBoolPtr(false)
			usage.AuthState = "missing"
			usage.LoginExpiresAt = ""
			usage.LoginExpirationState = loginExpirationNotReported
			usage.Notice = "Claude is not signed in on this computer — run `claude` and sign in (/login) on the terminal computer."
			usage.NoticeSeverity = "error"
		} else {
			usage.Authenticated = authBoolPtr(true)
			usage.AuthState = "authenticated"
			if authNoticeApplied {
				// The credential's own expiry notice described a session the
				// probe just proved is live. Drop it — leaving it would keep the
				// card's "Login expired" chip and (below) blank the metrics.
				usage.Notice = ""
				usage.NoticeSeverity = ""
			}
			if usage.LoginExpirationState == loginExpirationKnown && claudeDeadlinePassed(usage, now) {
				// An access-token-only credential whose stamp has lapsed, on a
				// session the probe says is live: the file is behind the CLI, not
				// the CLI behind the file. Publishing the passed stamp would let
				// the card re-derive "Expired" from a date the probe just
				// contradicted, so report no deadline instead of a wrong one.
				usage.LoginExpiresAt = ""
				usage.LoginExpirationState = loginExpirationNotReported
			}
		}
	} else if !credentialUsable && (credentialFound || strings.TrimSpace(detected.Path) != "") {
		usage.Authenticated = authBoolPtr(false)
		usage.AuthState = "missing"
		usage.LoginExpiresAt = ""
		usage.LoginExpirationState = loginExpirationNotReported
		usage.Notice = "Claude credentials are unavailable on this computer — run `claude` and sign in (/login) on the terminal computer."
		usage.NoticeSeverity = "error"
	}
	if usage.NoticeSeverity == "error" {
		usage.Metrics = utilizationMetricsUnknown(usage.Metrics)
	}

	return usage, true
}

// utilizationMetricsUnknown preserves metric identity and provenance while
// removing numeric values that cannot be trusted for an unavailable login.
func utilizationMetricsUnknown(metrics []cliAgentUsageMetric) []cliAgentUsageMetric {
	out := make([]cliAgentUsageMetric, len(metrics))
	for i, metric := range metrics {
		out[i] = cliAgentUsageMetric{
			Kind: metric.Kind, Label: metric.Label, Unit: metric.Unit,
			ResetAt: metric.ResetAt, ObservedAt: metric.ObservedAt,
			Model: metric.Model, Unknown: true,
		}
	}
	return out
}

// claudeConfigDir resolves the Claude config dir using the same precedence Parse
// uses (CLAUDE_CONFIG_DIR override, then ~/.claude). Empty when neither resolves.
func claudeConfigDir(home string) string {
	return firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), expandHome(home, ".claude"))
}

// claudeDotJSONPath resolves Claude Code's main config file, ~/.claude.json. It
// sits at the HOME root next to the ~/.claude dir, NOT inside it — but when
// CLAUDE_CONFIG_DIR selects a side-by-side profile, .claude.json moves into
// that dir alongside .credentials.json. Empty when neither resolves.
func claudeDotJSONPath(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return expandHome(dir, ".claude.json")
	}
	return expandHome(home, ".claude.json")
}

// claudeDotJSONAccount returns (email, displayName) from ~/.claude.json's
// oauthAccount block, or ("","") when the file is absent/unparseable or the
// block is unhydrated. Used ONLY to pick a human-facing DISPLAY label for the
// card (email preferred, display name as a fallback) — never as a fingerprint.
// ~/.claude.json is shared and mutable (any session's `/login` rewrites it), so
// it must not scope the cache or drive cross-device dedup; Parse fingerprints by
// the stable credential account instead.
func claudeDotJSONAccount(home string) (email, displayName string) {
	path := claudeDotJSONPath(home)
	if path == "" {
		return "", ""
	}
	cfg := claudeDotJSON{}
	if !readJSONFile(path, &cfg) {
		return "", ""
	}
	return cfg.OAuthAccount.EmailAddress, cfg.OAuthAccount.DisplayName
}

// claudeEnvAuthActive reports whether the ACTIVE credential for this Claude
// session is an env credential rather than the stored /login — i.e. whether the
// captured rate_limits belong to a different account than the ~/.claude.json
// oauthAccount and must therefore NOT be scoped to it.
//
// This is only meaningful where the current process IS the Claude session — the
// status-line hook, which Claude Code spawns as a child of the session and which
// therefore inherits that session's environment. The daemon paths (Parse /
// gather, and the stream-capture in captureClaudeRateLimitLine) deliberately do
// NOT consult this: the daemon's env reflects the shell it was launched from,
// not the driver-launched sessions it parses, and those sessions strip these
// vars — so treating a stray daemon-shell key as "this account is env-auth"
// would wrongly blank the stored subscription account.
//
// Presence of the env var is NOT sufficient — it must actually WIN Claude's
// authentication precedence (https://code.claude.com/docs/en/authentication),
// which ranks credentials above the stored /login (#6) as:
//   - #1 cloud / alternate provider selectors (see env-vars reference) — routes
//     inference off the first-party subscription: BEDROCK, VERTEX, FOUNDRY,
//     ANTHROPIC_AWS (Claude Platform on AWS), and MANTLE (Bedrock Mantle).
//   - #2 ANTHROPIC_AUTH_TOKEN — always wins when set (bearer token, no approval).
//   - #3 ANTHROPIC_API_KEY — wins only ONCE APPROVED. In interactive mode (the
//     only mode that renders a status line) the user is prompted once to
//     approve/decline and the choice is remembered in .claude.json's
//     customApiKeyResponses. A present-but-declined (or not-yet-approved) key is
//     ignored and Claude keeps billing the subscription — so those rate_limits
//     DO belong to the oauthAccount and must stay scoped to it.
//   - #5 CLAUDE_CODE_OAUTH_TOKEN — a long-lived subscription token (claude
//     setup-token) that may belong to a DIFFERENT account than the stored
//     /login, and still emits subscription rate_limits — so it too must unscope.
//
// #4 apiKeyHelper is deliberately not handled: it lives in settings.json rather
// than the environment, and it supplies an API-key credential (Console/API
// billing) which does not emit the subscription 5h/weekly rate_limits this
// capture path records — so there is nothing to misattribute.
func claudeEnvAuthActive() bool {
	// #1 cloud / alternate provider selection. Every CLAUDE_CODE_USE_* provider
	// selector from https://code.claude.com/docs/en/env-vars must be covered so
	// provider-session rate_limits never merge into the stored /login card.
	if os.Getenv("CLAUDE_CODE_USE_BEDROCK") != "" ||
		os.Getenv("CLAUDE_CODE_USE_VERTEX") != "" ||
		os.Getenv("CLAUDE_CODE_USE_FOUNDRY") != "" ||
		os.Getenv("CLAUDE_CODE_USE_ANTHROPIC_AWS") != "" ||
		os.Getenv("CLAUDE_CODE_USE_MANTLE") != "" {
		return true
	}
	// #2 bearer token, and #5 long-lived OAuth token: both outrank the stored
	// /login unconditionally when set.
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return true
	}
	// #3 API key: active only once approved.
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	return claudeApiKeyApproved(home, key)
}

// claudeApiKeyApproved reports whether the given ANTHROPIC_API_KEY has been
// approved as the active credential, per ~/.claude.json's customApiKeyResponses
// (whose entries are the key's last 20 characters — the form Claude Code
// persists). Returns false when the file/field is absent or the key isn't listed
// as approved: the safe default is to keep attributing to the stored /login so a
// declined or not-yet-approved key never blanks real subscription usage.
func claudeApiKeyApproved(home, key string) bool {
	path := claudeDotJSONPath(home)
	if path == "" {
		return false
	}
	cfg := claudeDotJSON{}
	if !readJSONFile(path, &cfg) {
		return false
	}
	// Claude stores key.slice(-20): the last 20 chars, or the whole key if shorter.
	suffix := key
	if len(key) > 20 {
		suffix = key[len(key)-20:]
	}
	for _, approved := range cfg.CustomApiKeyResponses.Approved {
		if approved == suffix {
			return true
		}
	}
	return false
}

// currentClaudeAccountFingerprint returns the fingerprint used to SCOPE the
// on-disk rate-limit cache to the account that produced a capture. It reads ONLY
// the stable, per-config-dir credential (email/account/organization) — never the
// shared ~/.claude.json oauthAccount.
//
// Why not fall back to ~/.claude.json here (unlike Parse's display/dedup path):
// this feeds the LIVE writers — the status-line hook (called on every render)
// and the stream-capture — and ~/.claude.json is a single shared file that any
// interactive session rewrites on `/login`. Scoping a live capture by it would
// let a concurrent `/login` in a second session misattribute the first session's
// rate limits to the newly written account, and mergeClaudeRateLimitCache drops
// the prior account's buckets on that fingerprint flip. The credential is stable
// per config dir and can't be swapped out mid-render.
//
// Returns "" when the credential carries no account — the common claude.ai
// login. The cache is then unscoped, matching the behavior before this feature;
// Parse reads it with the same "" so the windows still display.
func currentClaudeAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	account := ""
	if raw, ok := readClaudeCredentialsRaw(base); ok {
		creds := claudeOAuthCredentials{}
		if json.Unmarshal(raw, &creds) == nil {
			account = creds.claudeCredentialAccount()
		}
	}
	return fingerprintAccount("claudeCode", account)
}

// installedClaudeRateLimitCachePath returns the cache path the CURRENTLY
// INSTALLED status-line hook writes to, or "" when Claude has no hook of ours.
//
// `ourStatusLineCommand` pins the path to the installing binary's
// `GetConfigDir()`, but Claude's settings.json is a single machine-wide file:
// with two channels installed (release + dev), whichever agent booted last owns
// the hook, and the other one keeps reading a cache nothing writes to any more.
// Its Claude card then freezes at whatever was observed before the handover —
// the reported observation ages by days while the device is plainly online.
func installedClaudeRateLimitCachePath(home string) string {
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	settings := map[string]json.RawMessage{}
	if !readJSONFile(filepath.Join(base, "settings.json"), &settings) {
		return ""
	}
	raw, ok := settings["statusLine"]
	if !ok {
		return ""
	}
	var sl claudeStatusLine
	if json.Unmarshal(raw, &sl) != nil || !isOurStatusLineCommand(sl.Command) {
		return ""
	}
	return extractInstalledPinnedPath(sl.Command, "RL_CACHE")
}

// loadMergedClaudeRateLimitBuckets reads every cache this machine could have
// captured into — our own config dir plus the one the installed hook pins — and
// returns the freshest observation per window.
//
// Merging rather than preferring one path: the agent's own stream capture writes
// to OUR path while the status-line hook writes to the PINNED one, so on a
// dual-channel box each file holds a genuine subset of the observations. Taking
// the higher ObservedAtMs per window is the same precedence the cache itself
// applies on merge, so this can only move a window forward in time.
//
// A snapshot whose `accountFingerprint` doesn't match the current account is
// skipped entirely — see claudeCodeMetricsFromCache for why that scoping is
// exact rather than best-effort.
func loadMergedClaudeRateLimitBuckets(currentFingerprint string) map[string]claudeRateLimitBucket {
	home, _ := os.UserHomeDir()
	paths := []string{claudeRateLimitCachePath()}
	if pinned := installedClaudeRateLimitCachePath(home); pinned != "" && pinned != paths[0] {
		paths = append(paths, pinned)
	}

	merged := map[string]claudeRateLimitBucket{}
	for _, path := range paths {
		snap, ok := loadClaudeRateLimitSnapshot(path)
		if !ok || snap.AccountFingerprint != currentFingerprint {
			continue
		}
		for window, bucket := range snap.Buckets {
			prev, seen := merged[window]
			if !seen || bucket.ObservedAtMs > prev.ObservedAtMs {
				merged[window] = bucket
			}
		}
	}
	return merged
}

// claudeCodeMetricsFromCache builds the metric rows from the rate-limit cache,
// falling back to the Unknown placeholders when a window hasn't been observed.
// Three rows are always shown so the card layout is stable: the 5-hour session
// window, the weekly window, and the weekly Fable window.
//
// The cache is trusted only when its `accountFingerprint` exactly matches the
// caller-supplied one. Two empty fingerprints match (no creds + unscoped
// snapshot — the test/legacy default), but a scoped snapshot is ignored when
// the current account cannot be identified: otherwise `gatherCLIAgentUsage`
// would attribute a previous user's reset windows to the device-scoped
// fallback entry after the local credentials were removed.
func claudeCodeMetricsFromCache(now time.Time, currentFingerprint string) []cliAgentUsageMetric {
	buckets := loadMergedClaudeRateLimitBuckets(currentFingerprint)

	session := observedMetricOrUnknown(
		buckets, []string{claudeWindowFiveHour}, limitKindSession, "5-hour session window", now)
	// Weekly is reported under seven_day; some plans split it per-model. When
	// both per-model buckets are present we aggregate CONSERVATIVELY so an
	// exhausted Opus quota isn't hidden behind a healthier Sonnet number.
	weekly := aggregateWeeklyMetric(buckets, now)
	// Fable is metered SEPARATELY from the weekly quota above — Claude Code's
	// own /usage panel shows it as its own meter — so it is deliberately NOT
	// folded into the aggregate. Always emitted: observedMetricOrUnknown returns
	// the Unknown placeholder when no Fable window has been observed, which the
	// card renders as the dashed "Usage unobservable" bar rather than a 0% bar
	// that would read as "plenty of Fable quota left".
	fable := observedMetricOrUnknown(
		buckets, claudeFableWindowIDs(buckets), limitKindWeekly, "Weekly Fable", now)

	return []cliAgentUsageMetric{session, weekly, fable}
}

// claudeFableWindowIDs returns the cache keys that may hold the weekly Fable
// window, in the order observedMetricOrUnknown should try them: the canonical
// `seven_day_fable` first, then any other observed key naming Fable, sorted.
//
// The tolerant tail exists because the canonical key is an extrapolation from
// the keys Claude Code is known to emit — a rename or a suffixed variant
// upstream would otherwise silently drop the row from every shipped agent.
// Order is load-bearing twice over: it is observedMetricOrUnknown's TIE-BREAK
// among equally-fresh live buckets (canonical-first, so a real `seven_day_fable`
// wins over a same-timestamp variant — plain sorting would not, `fable_weekly`
// sorts ahead of it), and sorting the tail makes that tie-break deterministic
// where Go map iteration is not. Precedence is only a tie-break: observedMetric-
// OrUnknown skips rolled-over candidates first and then prefers the freshest live
// observation, so neither a stale nor a rolled-over canonical bucket left behind
// by a rename can mask a live variant. Non-determinism would not just be untidy:
// terminal-service delta-skips writes by hashing the marshalled payload, so a row
// flipping between two fable-ish buckets across polls would churn a Firestore
// write plus a 7-day history document each time.
//
// five_hour* keys are excluded: Claude already splits the weekly window
// per-model, so a symmetric `five_hour_fable` is plausible, and surfacing a
// session-window bucket under a row labelled "Weekly Fable" would misreport it.
func claudeFableWindowIDs(buckets map[string]claudeRateLimitBucket) []string {
	variants := make([]string, 0, len(buckets))
	for id := range buckets {
		lower := strings.ToLower(id)
		if id == claudeWindowSevenDayFable ||
			!strings.Contains(lower, "fable") ||
			strings.HasPrefix(lower, claudeWindowFiveHour) {
			continue
		}
		variants = append(variants, id)
	}
	sort.Strings(variants)
	return append([]string{claudeWindowSevenDayFable}, variants...)
}

// aggregateWeeklyMetric reports the worst observed seven-day window: the
// highest used percentage and the soonest reset across the unified `seven_day`
// bucket and any per-model split (`seven_day_sonnet`, `seven_day_opus`). This
// prevents a healthy Sonnet bucket from masking a depleted Opus bucket on
// plans that emit them separately.
func aggregateWeeklyMetric(buckets map[string]claudeRateLimitBucket, now time.Time) cliAgentUsageMetric {
	windowIDs := []string{claudeWindowSevenDay, claudeWindowSevenDaySonnet, claudeWindowSevenDayOpus}
	var (
		observed      bool
		worstUsed     float64
		worstReset    int64
		worstObserved int64
		worstCurrent  bool
	)
	// Live windows whose usage was never observed: they cannot contribute a
	// percentage, but their reset is current and is the right thing to show when
	// nothing else was observed.
	liveUnobservedReset := int64(0)
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		if !b.hasObservedUsage() {
			if b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs && b.ResetsAtMs > liveUnobservedReset {
				liveUnobservedReset = b.ResetsAtMs
			}
			continue
		}
		used := b.UsedPercentage
		liveReset := b.ResetsAtMs > 0 && now.UnixMilli() < b.ResetsAtMs
		current := b.ResetsAtMs <= 0 || liveReset
		if !current {
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
			worstObserved = b.ObservedAtMs
			worstCurrent = current
			if liveReset {
				worstReset = b.ResetsAtMs
			} else {
				worstReset = 0
			}
		case used == worstUsed:
			wasCurrent := worstCurrent
			if current && (!worstCurrent || b.ObservedAtMs > worstObserved) {
				worstObserved = b.ObservedAtMs
			}
			worstCurrent = worstCurrent || current
			switch {
			case !current:
				// An expired tied bucket no longer constrains the live aggregate.
			case !wasCurrent && liveReset:
				worstReset = b.ResetsAtMs
			case !wasCurrent:
				worstReset = 0
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
		metric := cliAgentUsageMetric{Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%", Unknown: true}
		if liveUnobservedReset > 0 {
			metric.ResetAt = time.UnixMilli(liveUnobservedReset).UTC().Format(time.RFC3339)
		}
		return metric
	}
	observedAt := observedAtRFC3339(worstObserved)
	if !worstCurrent {
		return cliAgentUsageMetric{
			Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%",
			ObservedAt: observedAt, Unknown: true,
		}
	}
	var resetAt string
	if worstReset > 0 {
		resetAt = time.UnixMilli(worstReset).UTC().Format(time.RFC3339)
	}
	return cliAgentUsageMetric{
		Kind: limitKindWeekly, Label: "Weekly quota", Unit: "%",
		Total: floatPtr(100), Consumed: floatPtr(worstUsed), Remaining: floatPtr(100 - worstUsed),
		ResetAt: resetAt, ObservedAt: observedAt,
	}
}

// observedMetricOrUnknown returns a real percentage metric for the first window
// id present in the cache that still describes a LIVE window, or an Unknown
// placeholder when none is observed. A window whose reset time has passed is
// unobservable: assuming 0% ignores usage that may already have happened on
// another computer.
//
// Liveness is checked BEFORE candidate precedence, which matters only for a
// multi-id list (the Fable row): the cache never prunes a window id, so an
// upstream rename leaves the old canonical bucket behind forever. Returning its
// rolled-over Unknown on sight would make the tolerant variant fallback inert in
// exactly the rename case it exists for — the row would stay unobservable while
// fresh variant telemetry kept arriving. A rolled-over bucket is still the
// answer when NO candidate is live, and the first such bucket in candidate order
// supplies the ObservedAt so the result stays deterministic.
//
// Among LIVE candidates the freshest observation wins, not merely the first in
// candidate order: a rename can leave the old canonical bucket with a still-future
// reset (live) while newer telemetry flows to the variant, and canonical-first
// would then pin the row to the stale percentage until the retained bucket finally
// rolls over. Ranking by ObservedAtMs lets the actively-updated bucket win; equal
// timestamps fall back to candidate order (canonical-first), so the result stays
// deterministic for terminal-service's payload-hash delta-skip.
func observedMetricOrUnknown(
	buckets map[string]claudeRateLimitBucket,
	windowIDs []string,
	kind, label string,
	now time.Time,
) cliAgentUsageMetric {
	rolledOver := ""
	freshestLive := ""
	// A live window we know the reset for but not the usage. Recorded from a
	// heartbeat, so it carries no percentage — but its reset is current, which
	// is strictly more useful than falling back to a rolled-over bucket's stale
	// "last observed".
	liveUnobserved := ""
	for _, id := range windowIDs {
		b, ok := buckets[id]
		if !ok {
			continue
		}
		if b.ResetsAtMs > 0 && now.UnixMilli() >= b.ResetsAtMs {
			if rolledOver == "" {
				rolledOver = id
			}
			continue
		}
		if !b.hasObservedUsage() {
			if liveUnobserved == "" || b.ResetsAtMs > buckets[liveUnobserved].ResetsAtMs {
				liveUnobserved = id
			}
			continue
		}
		// Strictly-greater keeps the earlier (canonical-first) candidate on a tie.
		if freshestLive == "" || b.ObservedAtMs > buckets[freshestLive].ObservedAtMs {
			freshestLive = id
		}
	}
	if freshestLive != "" {
		b := buckets[freshestLive]
		var resetAt string
		if b.ResetsAtMs > 0 {
			resetAt = time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339)
		}
		used := clampPercent(b.UsedPercentage)
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			Total: floatPtr(100), Consumed: floatPtr(used), Remaining: floatPtr(100 - used),
			ResetAt: resetAt, ObservedAt: observedAtRFC3339(b.ObservedAtMs),
		}
	}
	// A current window with no reading beats a rolled-over one with a stale
	// reading: "unobservable, resets Friday" is true, where "last observed six
	// days ago" describes a window that no longer exists. No ObservedAt — the
	// usage was never observed, and claiming otherwise is what made a stuck card
	// look merely stale.
	if liveUnobserved != "" {
		b := buckets[liveUnobserved]
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			ResetAt: time.UnixMilli(b.ResetsAtMs).UTC().Format(time.RFC3339), Unknown: true,
		}
	}
	if rolledOver != "" {
		return cliAgentUsageMetric{
			Kind: kind, Label: label, Unit: "%",
			ObservedAt: observedAtRFC3339(buckets[rolledOver].ObservedAtMs), Unknown: true,
		}
	}
	return cliAgentUsageMetric{Kind: kind, Label: label, Unit: "%", Unknown: true}
}

func observedAtRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
