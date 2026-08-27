// cliagent_usage_codex.go — Codex (OpenAI) usage parser.
//
// Reads CODEX_HOME/auth.json (or ~/.codex/auth.json) for the active account
// and any local plan/quota hints. Live utilization comes from the
// `token_count` JSON-RPC notification the Codex app-server emits on stdout
// while a session is active — codex_appserver.go forwards every line to
// captureCodexRateLimitLine, which writes the latest primary (5-hour) /
// secondary (weekly) windows to an on-disk cache keyed by account
// fingerprint. This parser turns the latest snapshot into real percentage
// metrics for the CLI Agents tab.
//
// When the live cache has no entry for a window — the common case when Codex
// is only ever driven through its own TUI rather than our app-server — the
// parser backfills from Codex's own session rollout logs
// (CODEX_HOME/sessions/.../rollout-*.jsonl), which persist the identical
// `token_count.rate_limits` telemetry. Only windows still Unknown after the
// live cache are filled in this way.
package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

type codexUsageParser struct{}

func (codexUsageParser) Provider() string { return "codex" }

// Every optional Codex sub-operation is carved out of the SHARED
// GatherCLIAgentUsageOnly deadline, so each one holds back the time the steps
// after it still need. codexGatherSiblingReserve is what the later providers
// in the catalog are owed once Codex is done; the login probe additionally
// reserves its own machineInfoProbeTimeout out of the rollout scan's share,
// because it runs after the scan and cannot be interrupted mid-`CombinedOutput`
// by anything but its own deadline.
const codexGatherSiblingReserve = 2 * time.Second

// codexRolloutScanBudget is the most the optional direct-run rollout scan may
// take when the parent gather is unconstrained.
const codexRolloutScanBudget = 5 * time.Second

// codexSubBudget is how long an optional step may run without consuming
// holdBack of the parent gather's remaining time. Zero means "skip it".
func codexSubBudget(ctx context.Context, want, holdBack time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return want
	}
	available := time.Until(deadline) - holdBack
	if available <= 0 {
		return 0
	}
	if available < want {
		return available
	}
	return want
}

// The caller passes a context already bounded to this probe's share of the
// gather deadline: an unbounded background probe is exactly what let Codex
// overrun the deadline it shares with the providers parsed after it.
var codexAuthStatusProbe = func(ctx context.Context, path string) (bool, bool) {
	if strings.TrimSpace(path) == "" {
		return false, false
	}
	cmd := exec.CommandContext(ctx, path, "login", "status")
	// Background probe: hide the console the child would otherwise pop up on
	// Windows, where the agent itself runs windowless in the tray.
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	status := strings.ToLower(string(out))
	if strings.Contains(status, "not logged in") || strings.Contains(status, "login required") {
		return false, true
	}
	if err == nil && strings.Contains(status, "logged in") {
		return true, true
	}
	return false, false
}

// codexProbeLoginStatus skips the probe outright when its budget is gone; an
// already-expired context would otherwise spawn a child process only to kill it.
func codexProbeLoginStatus(ctx context.Context, path string) (bool, bool) {
	if ctx.Err() != nil {
		return false, false
	}
	return codexAuthStatusProbe(ctx, path)
}

type codexAuth struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	OrgID    string `json:"org_id"`
	APIKey   string `json:"OPENAI_API_KEY"`
	Tokens   struct {
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
		RefreshToken string `json:"refresh_token"`
	} `json:"tokens"`
}

type codexIDTokenClaims struct {
	Account  string `json:"account"`
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Subject  string `json:"sub"`
	Plan     string `json:"plan"`
	PlanType string `json:"plan_type"`
	OrgID    string `json:"org_id"`
	// ChatGPT-login claims. A `codex login` against a ChatGPT account does NOT
	// put the plan at the top level: it namespaces every account attribute
	// under the `https://api.openai.com/auth` claim, where the subscription
	// tier is `chatgpt_plan_type` ("pro", "plus", "business", …). Reading only
	// the flat `plan` / `plan_type` above left the plan chip permanently blank
	// for the common ChatGPT login — the only auth mode that HAS a plan.
	OpenAIAuth codexOpenAIAuthClaim `json:"https://api.openai.com/auth"`
	// Standard JWT expiry (seconds since epoch). Reported even when the
	// credential refreshes itself — see the LoginExpiresAt assignment below.
	Exp int64 `json:"exp"`
}

// codexOpenAIAuthClaim is the namespaced `https://api.openai.com/auth` claim
// carried by a ChatGPT-login id_token. Only the fields the card needs are
// modelled; the claim also carries org membership we deliberately ignore.
type codexOpenAIAuthClaim struct {
	PlanType  string `json:"chatgpt_plan_type"`
	UserID    string `json:"chatgpt_user_id"`
	AccountID string `json:"chatgpt_account_id"`
}

// codexAccount returns the human-readable credential identity shown by the
// usage parser. Workspace account IDs remain fallbacks here because an email or
// ChatGPT user ID is a more useful label on the card.
func codexAccount(auth codexAuth, claims codexIDTokenClaims) string {
	return firstNonEmpty(
		auth.Email,
		auth.Account,
		auth.UserID,
		claims.Email,
		claims.Account,
		claims.UserID,
		claims.OpenAIAuth.UserID,
		auth.Tokens.AccountID,
		claims.OpenAIAuth.AccountID,
		claims.Subject,
	)
}

// codexAccountScope returns the credential identity used to scope cached
// telemetry. A ChatGPT user can belong to multiple workspace accounts with
// independent plans and quotas, so the active workspace ID must outrank the
// display identity whenever Codex provides one.
func codexAccountScope(auth codexAuth, claims codexIDTokenClaims) string {
	return firstNonEmpty(
		auth.Tokens.AccountID,
		claims.OpenAIAuth.AccountID,
		codexAccount(auth, claims),
	)
}

func (p codexUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	return p.ParseContext(context.Background(), home, detected, now)
}

func (p codexUsageParser) ParseContext(ctx context.Context, home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	base := firstNonEmpty(os.Getenv("CODEX_HOME"), expandHome(home, ".codex"))
	if base == "" {
		return nil, false
	}

	usage := &cliAgentUsage{
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Codex"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "token_count",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}
	hasAPIKeyAuth := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != ""
	if hasAPIKeyAuth {
		usage.Authenticated = authBoolPtr(true)
		usage.AuthState = "authenticated"
		usage.LoginExpirationState = loginExpirationNotReported
	}

	auth := codexAuth{}
	if readJSONFile(expandHome(base, "auth.json"), &auth) {
		claims := codexIDTokenClaims{}
		parseJWTClaims(auth.Tokens.IDToken, &claims)
		usage.Account = codexAccount(auth, claims)
		usage.AccountFingerprint = fingerprintAccount(p.Provider(), codexAccountScope(auth, claims))
		usage.Plan = firstNonEmpty(
			auth.Plan, auth.PlanType, claims.Plan, claims.PlanType, claims.OpenAIAuth.PlanType)
		if firstNonEmpty(auth.APIKey, auth.Tokens.IDToken, auth.Tokens.RefreshToken) != "" {
			hasAPIKeyAuth = hasAPIKeyAuth || strings.TrimSpace(auth.APIKey) != ""
			usage.Authenticated = authBoolPtr(true)
			usage.AuthState = "authenticated"
			if auth.Tokens.RefreshToken != "" {
				usage.LoginExpirationState = loginExpirationRefreshable
			} else {
				usage.LoginExpirationState = loginExpirationNotReported
			}
			// Report the expiry even for a refreshing credential. It is the
			// ACCESS token's, not a logout date, so the card must label it as
			// such — but withholding it left the row permanently blank, which
			// tells the reader nothing about whether the login is healthy.
			// authState is deliberately NOT flipped on a passed expiry here: a
			// refreshable token renews on next use, so "expired" would be a lie.
			if claims.Exp > 0 {
				usage.LoginExpiresAt = time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339)
			}
		}
	}

	// Reconcile terminal-managed cache evidence with direct-run rollout logs.
	// The optional scan gets at most five seconds and always leaves the sibling
	// reserve PLUS the login probe's own budget on the parent gather, so the two
	// bounded children below cannot sum past it. Child expiry is best-effort: any
	// complete numeric objects already consumed are persisted and the valid cache
	// remains publishable.
	usage.Metrics = codexMetricsFromCache(now, usage.AccountFingerprint)
	var usageLimit codexUsageLimitEvidence
	var latestRolloutObservation time.Time
	scanBudget := codexSubBudget(ctx, codexRolloutScanBudget, codexGatherSiblingReserve+machineInfoProbeTimeout)
	if scanBudget > 0 && ctx.Err() == nil {
		scanCtx, cancel := context.WithTimeout(ctx, scanBudget)
		usage.Metrics, usageLimit, latestRolloutObservation = codexReconcileFromRollout(scanCtx, base, usage.AccountFingerprint, now)
		cancel()
	}
	// An account that is OUT of quota reports no window at all — Codex nulls both
	// `primary` and `secondary` on a refused turn — so the card fell back to
	// "Usage unobservable", which reads as "we can't see it" when the truth is
	// "it is spent". Say so instead. A later logged-out verdict overwrites this
	// with its own error notice, which is the correct precedence: a card that
	// cannot be attributed to a signed-in account has a worse problem than a
	// spent quota.
	if notice := codexUsageLimitNotice(usage.Metrics, usageLimit, latestRolloutObservation, now); notice != "" {
		usage.Notice = notice
		usage.NoticeSeverity = "error"
	}
	// No time left to answer is inconclusive — the same verdict a timed-out
	// probe already produced — so a squeezed gather degrades rather than
	// borrowing from the providers still to be parsed.
	probeCtx, cancelProbe := context.WithTimeout(
		ctx, codexSubBudget(ctx, machineInfoProbeTimeout, codexGatherSiblingReserve))
	loggedIn, known := codexProbeLoginStatus(probeCtx, detected.Path)
	cancelProbe()
	if known {
		// `codex login status` describes persisted login state, not inherited
		// OPENAI_API_KEY authentication. A definite persisted logout therefore
		// cannot invalidate a credential mode the app-server will actually use.
		if !loggedIn && hasAPIKeyAuth {
			// The probe is definite: the persisted OAuth login is gone, so any
			// expiry parsed out of a leftover auth.json id_token describes a
			// credential Codex will not use. The API key that IS in use carries
			// no expiry, so report none rather than a stale OAuth one.
			usage.LoginExpiresAt = ""
			usage.LoginExpirationState = loginExpirationNotReported
			return usage, true
		}
		usage.Authenticated = authBoolPtr(loggedIn)
		if loggedIn {
			usage.AuthState = "authenticated"
		} else {
			usage.AuthState = "missing"
			usage.LoginExpiresAt = ""
			usage.LoginExpirationState = loginExpirationNotReported
			usage.Notice = "Codex is not signed in on this computer — run `codex login` on the terminal computer to authenticate."
			usage.NoticeSeverity = "error"
			usage.Metrics = utilizationMetricsUnknown(usage.Metrics)
		}
	} else if usage.Authenticated == nil && strings.TrimSpace(detected.Path) != "" {
		// No supported credential remains to explain an inconclusive status
		// probe, so cached telemetry cannot be attributed to a usable login.
		usage.Authenticated = authBoolPtr(false)
		usage.AuthState = "missing"
		usage.Notice = "Codex is not signed in on this computer — run `codex login` on the terminal computer to authenticate."
		usage.NoticeSeverity = "error"
		usage.Metrics = utilizationMetricsUnknown(usage.Metrics)
	}
	return usage, true
}
