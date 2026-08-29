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
//
// This file owns the CREDENTIAL and AUTH-STATE half of the parser. Its two
// siblings own the rest:
//   - cliagent_usage_claudecode_metrics.go — cache → metric rows.
//   - cliagent_usage_claudecode_probe.go   — the bounded utilization probe that
//     supplies a fresh numeric reading after a run, which neither the heartbeat
//     stream nor the interactive-only status-line hook can do on its own.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
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
	return p.ParseContext(context.Background(), home, detected, now)
}

// ParseContext is the real implementation. Claude Code joins the small set of
// providers that perform optional bounded I/O during a refresh (see
// cliAgentUsageContextParser): the utilization probe must be able to honor the
// gather deadline rather than spend its own timeout after the budget is gone.
func (p claudeCodeUsageParser) ParseContext(ctx context.Context, home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
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
	// A run that stayed under quota leaves only usage-less heartbeats behind, and
	// those deliberately cannot advance the cached observation — so a device that
	// never renders Claude's interactive status line would report the same
	// `latestObservedAt` forever. Spend one bounded probe when the freshest
	// reading has aged past the staleness TTL (or when a user-initiated refresh
	// forced it), then re-read the cache once.
	refreshClaudeUsageIfStale(ctx, now, usage.AccountFingerprint)
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
