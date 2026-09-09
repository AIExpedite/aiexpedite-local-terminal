// grok_auth.go — shared Grok credential assessment for CLI status and ACP
// launch pre-flight. Never returns or logs token material.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const grokNotAuthenticatedCode = "GROK_NOT_AUTHENTICATED"

const (
	grokAuthSourceCachedLogin = "cached-login"
	grokAuthSourceAPIKey      = "api-key"
	grokAuthSourceNone        = "none"
)

const (
	grokAuthStateAuthenticated = "authenticated"
	grokAuthStateMissing       = "missing"
	grokAuthStateExpired       = "expired"
	grokAuthStateUnknown       = "unknown"
)

// grokAuthAssessment is the structured, secret-free result used by both
// periodic CLI status and the ACP spawn pre-flight.
type grokAuthAssessment struct {
	Authenticated bool
	Source        string
	AuthState     string
	ReasonCode    string
	Reason        string
	Refreshable   bool
}

// grokAuthError is a typed Start failure so the Pub/Sub publisher can keep
// errorCode: GROK_NOT_AUTHENTICATED instead of flattening to message text.
type grokAuthError struct {
	Code    string
	Message string
}

func (e *grokAuthError) Error() string { return e.Message }

func newGrokAuthError(message string) *grokAuthError {
	return &grokAuthError{Code: grokNotAuthenticatedCode, Message: message}
}

func grokAuthErrorFrom(err error) *grokAuthError {
	if err == nil {
		return nil
	}
	var typed *grokAuthError
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}

// assessGrokAuth classifies credentials in the given Grok home (the isolated
// GROK_HOME the child will use, or the host home for status). Secrets never
// appear in the result.
func assessGrokAuth(base string, now time.Time, allowAPIKeyFallback bool, runtimeModel string) grokAuthAssessment {
	out := grokAuthAssessment{
		Source:    grokAuthSourceNone,
		AuthState: grokAuthStateMissing,
	}
	if base == "" {
		out.ReasonCode = grokNotAuthenticatedCode
		out.Reason = "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate."
		return out
	}

	usableToken := grokHasUsableToken(base)
	refreshable := grokHasRefreshToken(base)
	out.Refreshable = refreshable

	if usableToken {
		out.Authenticated = true
		out.Source = grokAuthSourceCachedLogin
		out.AuthState = grokAuthStateAuthenticated
		if !refreshable {
			if expiry, ok := readGrokAuthExpiry(base); ok && !expiry.After(now) {
				out.Authenticated = false
				out.AuthState = grokAuthStateExpired
				out.ReasonCode = grokNotAuthenticatedCode
				out.Reason = "Grok login has expired — run `grok login` on the terminal computer to re-authenticate."
			}
		}
	} else if authNotice, severity := grokAuthNotice(base, now); authNotice != "" && severity == "error" {
		out.Authenticated = false
		out.AuthState = grokAuthStateMissing
		out.ReasonCode = grokNotAuthenticatedCode
		out.Reason = authNotice
	} else if grokAuthFileExists(base) && !usableToken {
		// A credential file exists but the installed CLI format cannot be
		// classified. Status reporting may keep "unknown"; launch pre-flight
		// decides separately.
		out.AuthState = grokAuthStateUnknown
	}

	if out.Authenticated {
		return out
	}

	if allowAPIKeyFallback && grokHasAPIKeyFallback(base, runtimeModel) {
		out.Authenticated = true
		out.Source = grokAuthSourceAPIKey
		out.AuthState = grokAuthStateAuthenticated
		out.ReasonCode = ""
		out.Reason = ""
		return out
	}

	if out.ReasonCode == "" && out.AuthState != grokAuthStateUnknown {
		out.ReasonCode = grokNotAuthenticatedCode
		out.Reason = "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate."
	}
	if out.AuthState != grokAuthStateUnknown && !out.Authenticated {
		out.ReasonCode = grokNotAuthenticatedCode
	}
	return out
}

func grokAuthFileExists(base string) bool {
	if base == "" {
		return false
	}
	for _, name := range []string{"auth.json", "cached_token.json"} {
		if _, err := os.Stat(filepath.Join(base, name)); err == nil {
			return true
		}
	}
	return false
}

func grokHasAPIKeyFallback(base, runtimeModel string) bool {
	if envKey := strings.TrimSpace(os.Getenv("XAI_API_KEY")); envKey != "" {
		return true
	}
	// A system-level `[model] api_key` (/etc/grok/*.toml) is not redirected by
	// GROK_HOME, so the child reads it even under the per-session isolated
	// home. With the API-key gate open that pinned key IS the credential —
	// ignoring it here would refuse every session on a managed host whose
	// posture detectPinnedSystemGrokRequirements explicitly permits.
	if grokSystemPinnedAPIKey(runtimeModel) {
		return true
	}
	if base == "" {
		return false
	}
	_, apiKey := readGrokPersistedAPIKey(filepath.Join(base, "config.toml"), runtimeModel)
	return strings.TrimSpace(strings.Trim(apiKey, `"'`)) != ""
}

// assessIsolatedGrokLaunch is the authoritative pre-flight: unknown formats
// fail closed so we never spawn an interactive login.
func assessIsolatedGrokLaunch(isolatedHome string, now time.Time, allowAPIKeyFallback bool, runtimeModel string) grokAuthAssessment {
	assessment := assessGrokAuth(isolatedHome, now, allowAPIKeyFallback, runtimeModel)
	if assessment.Authenticated {
		return assessment
	}
	if assessment.AuthState == grokAuthStateUnknown {
		assessment.ReasonCode = grokNotAuthenticatedCode
		assessment.Reason = "Grok credentials on this computer could not be classified — run `grok login` on the terminal computer to authenticate."
	}
	if assessment.ReasonCode == "" {
		assessment.ReasonCode = grokNotAuthenticatedCode
		assessment.Reason = "Grok is not signed in on this computer — run `grok login` on the terminal computer to authenticate."
	}
	return assessment
}

// grokCredentialOverrideEnvVars are the environment variables that hand a Grok
// child a credential of its OWN, independent of the cached login in $GROK_HOME.
// Same set the ACP/maintenance sanitizers strip; kept here so the
// billing-attribution guard and the strip lists cannot drift apart about what
// counts as a credential.
var grokCredentialOverrideEnvVars = []string{
	"XAI_API_KEY",
	"GROK_CODE_XAI_API_KEY",
	"GROK_AUTH_PROVIDER_ACCESS_TOKEN",
}

// grokDirectRunCredentialOverride reports whether a DIRECT (PTY) Grok child
// spawned with `env` could bill an account OTHER than the cached login in
// `base` — an inherited API key / provider token, or a key pinned in the user's
// own config.toml or a system config layer.
//
// A direct session deliberately inherits the user's shell environment (unlike
// the ACP and maintenance-smoke paths, which strip XAI_API_KEY unless the
// workspace opted in), and it reads the user's REAL config.toml rather than the
// neutralised isolated one. So the credential the child actually bills need not
// be the cached login that grokResolvedBillingIdentity names — and publishing
// one account's spend as another's is a billing lie, not a stale reading. The
// direct attribution arm uses this to name the contested sentinel instead,
// which costs observability (the card falls back to "unobservable", which is
// what shipped before direct attribution existed) rather than correctness.
//
// Conservative by construction: it reports true whenever a credential override
// is merely AVAILABLE, because which one the CLI resolves is its decision, made
// per turn inside a process we do not observe.
func grokDirectRunCredentialOverride(env []string, base string) bool {
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if strings.TrimSpace(value) == "" {
			continue
		}
		name = strings.ToUpper(strings.TrimSpace(name))
		for _, candidate := range grokCredentialOverrideEnvVars {
			if name == candidate {
				return true
			}
		}
	}
	return grokAnyPinnedAPIKey(base)
}
