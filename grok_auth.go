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

// grokDirectRunLaunch is the whole credential surface a DIRECT (PTY) Grok child
// is launched with. Held as one value because every field is a place the child
// can pick up a credential of its own, and the attribution decision has to see
// all of them at once: a detector handed only the environment silently answers
// "cached login" for a child billing an API-key account pinned in argv or in the
// repository it runs in.
//
// Env is the environment the child is spawned with, Cwd the directory it starts
// in (Grok discovers `.grok/config.toml` by walking upward from there), and Args
// the argv it is spawned with (`--config model.api_key=...` is a documented
// per-process override that buildGrokInteractiveArgs forwards verbatim).
//
// The zero value means "no child": the caller is asserting the cached login with
// no credential override in play.
type grokDirectRunLaunch struct {
	Env  []string
	Cwd  string
	Args []string
}

// grokDirectRunCredentialOverride reports whether a DIRECT (PTY) Grok child
// launched as `launch` could bill an account OTHER than the cached login in
// `base` — an inherited API key / provider token, or a key pinned in argv, in
// the repository the child runs in, in the user's own config.toml, or in a
// system config layer.
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
func grokDirectRunCredentialOverride(launch grokDirectRunLaunch, base string) bool {
	for _, entry := range launch.Env {
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
	if grokArgsPinAPIKey(launch.Args) {
		return true
	}
	if grokProjectPinnedAPIKey(launch.Cwd) {
		return true
	}
	return grokAnyPinnedAPIKey(base)
}

// grokArgsPinAPIKey reports whether the argv a direct child is spawned with
// hands it an API key of its own. xAI documents `-c|--config <key>=value` as the
// per-process config-override surface and buildGrokInteractiveArgs forwards it
// verbatim, so `--config model.api_key=sk-...` is a credential neither the
// environment nor any config file on this machine ever sees.
//
// Every token is inspected rather than only the ones following a recognized
// flag: the same override is spelled `--config k=v`, `--config=k=v` and `-c k=v`
// across Grok releases, and over-reporting costs one session's observability
// while under-reporting publishes one account's spend as another's.
func grokArgsPinAPIKey(args []string) bool {
	for _, arg := range args {
		if grokArgPinsAPIKey(arg) {
			return true
		}
	}
	return false
}

// grokArgPinsAPIKey reports whether ONE argv token assigns a non-empty API key.
// The flag form nests the assignment inside the flag's own value
// (`--config=model.api_key=sk-...`), so a key that is not itself an API-key path
// is retried against the value once the outer `flag=` has been peeled off.
func grokArgPinsAPIKey(arg string) bool {
	key, value, ok := strings.Cut(arg, "=")
	if !ok {
		return false
	}
	if grokAPIKeyConfigKey(key) {
		return strings.TrimSpace(strings.Trim(value, `"'`)) != ""
	}
	return grokArgPinsAPIKey(value)
}

// grokAPIKeyConfigKey reports whether a config key path names an API key, in any
// of the spellings a flag can carry it (`model.api_key`, `model.grok-4.apiKey`,
// `--api-key`). Separator- and case-insensitive, so a rename across a CLI update
// does not silently reopen the misattribution this guard closes.
func grokAPIKeyConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.TrimLeft(key, "-")
	key = strings.NewReplacer("_", "", "-", "").Replace(key)
	return strings.HasSuffix(key, "apikey")
}

// grokProjectConfigMaxDepth bounds the upward `.grok/config.toml` walk. Deep
// enough for any real checkout, finite so a pathological path cannot turn a
// session start into an unbounded stat loop.
const grokProjectConfigMaxDepth = 64

// grokProjectPinnedAPIKey reports whether a repository-scoped
// `.grok/config.toml` at or above the child's working directory pins an API key.
//
// Grok discovers project config by walking UPWARD from cwd — the same discovery
// the maintenance smoke isolates itself from by running in an empty directory —
// so a workspace that pins `model.api_key` bills its own account from a session
// whose environment and home both name the cached login. StartSession honours
// the caller's requested cwd, so that directory, not the daemon's, is what the
// child actually walks.
//
// An empty cwd walks NOTHING rather than falling back to the daemon's working
// directory: the caller knows which directory its child is really started in
// (StartSession resolves the inherited case at the spawn site), and reading the
// process-wide cwd here would make this decision depend on ambient state the
// caller never named.
//
// One bounded stat walk per direct session start, never on the streaming path.
func grokProjectPinnedAPIKey(cwd string) bool {
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		return false
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for depth := 0; depth < grokProjectConfigMaxDepth; depth++ {
		if grokConfigHasModelAPIKey(filepath.Join(dir, ".grok", "config.toml")) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	return false
}
