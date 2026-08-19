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
