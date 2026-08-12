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

var codexAuthStatusProbe = func(path string) (bool, bool) {
	if strings.TrimSpace(path) == "" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "login", "status").CombinedOutput()
	status := strings.ToLower(string(out))
	if strings.Contains(status, "not logged in") || strings.Contains(status, "login required") {
		return false, true
	}
	if err == nil && strings.Contains(status, "logged in") {
		return true, true
	}
	return false, false
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
	// Standard JWT expiry (seconds since epoch). Reported even when the
	// credential refreshes itself — see the LoginExpiresAt assignment below.
	Exp int64 `json:"exp"`
}

func (p codexUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
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
		usage.Account = firstNonEmpty(
			auth.Email,
			auth.Account,
			auth.UserID,
			claims.Email,
			claims.Account,
			claims.UserID,
			auth.Tokens.AccountID,
			claims.Subject,
		)
		usage.Plan = firstNonEmpty(auth.Plan, auth.PlanType, claims.Plan, claims.PlanType)
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
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	// Live app-server telemetry first; for any window never captured live,
	// fall back to Codex's own on-disk rollout logs so the card is observable
	// even when Codex is only ever driven through its TUI.
	usage.Metrics = codexMetricsFromCache(now, usage.AccountFingerprint)
	usage.Metrics = codexBackfillUnknownFromRollout(usage.Metrics, base, usage.AccountFingerprint, now)
	if loggedIn, known := codexAuthStatusProbe(detected.Path); known {
		// `codex login status` describes persisted login state, not inherited
		// OPENAI_API_KEY authentication. A definite persisted logout therefore
		// cannot invalidate a credential mode the app-server will actually use.
		if !loggedIn && hasAPIKeyAuth {
			return usage, true
		}
		usage.Authenticated = authBoolPtr(loggedIn)
		if loggedIn {
			usage.AuthState = "authenticated"
		} else {
			usage.AuthState = "missing"
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
