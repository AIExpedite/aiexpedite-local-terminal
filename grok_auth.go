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
		if grokConfigLoaderEnvContests(name, value, launch.Cwd) {
			return true
		}
	}
	if grokArgsPinCredential(launch.Args) {
		return true
	}
	if grokProjectPinnedCredential(launch.Cwd) {
		return true
	}
	for _, dir := range grokArgSelectedCwds(launch.Args, launch.Cwd) {
		if dir == grokUnresolvableArgCwd || grokProjectPinnedCredential(dir) {
			return true
		}
	}
	return grokAnyPinnedCredential(base)
}

// grokArgCwdFlag is the one argv flag that changes WHICH directory a direct
// child starts in, and therefore which workspace `.grok/config.toml` chain
// Grok walks upward from. buildGrokInteractiveArgs forwards it verbatim (it is
// in that builder's valuedFlags map, so both the `--cwd <dir>` and `--cwd=<dir>`
// spellings survive to the child), which means a credential-pinning workspace
// can be selected in argv alone while launch.Cwd stays clean.
//
// Deliberately just `--cwd`: `-w`/`--worktree`/`--ref` also move the child, but
// their values are refs rather than directories on the releases we have seen,
// and guessing a directory out of a ref would scan an unrelated path rather
// than the one the child loads. The ACP path needs no equivalent — it strips
// `--cwd*` outright (sanitizeGrokACPExtraArgs).
const grokArgCwdFlag = "--cwd"

// grokUnresolvableArgCwd is the sentinel grokArgSelectedCwds returns for a
// RELATIVE `--cwd` it cannot anchor, because the launch named no starting
// directory. Reading the daemon's own cwd to anchor it would make this
// decision depend on ambient state the caller never named (the same reason
// grokProjectPinnedCredential walks nothing for an empty cwd), so the caller
// contests instead: "we could not look" is not "there is nothing there".
const grokUnresolvableArgCwd = "\x00grok-unresolvable-arg-cwd"

// grokArgSelectedCwds returns the absolute directories a direct child's argv
// selects with `--cwd`, anchored on `cwd` when the value is relative (that is
// the directory the child starts in before it applies the flag). Every
// occurrence is returned, not just the last: which one a given Grok release
// wins with is its decision, and this guard is conservative by construction.
func grokArgSelectedCwds(args []string, cwd string) []string {
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"'`))
		if raw == "" {
			return
		}
		if filepath.IsAbs(raw) {
			out = append(out, raw)
			return
		}
		base := strings.TrimSpace(cwd)
		if base == "" {
			out = append(out, grokUnresolvableArgCwd)
			return
		}
		out = append(out, filepath.Join(base, raw))
	}
	for i := 0; i < len(args); i++ {
		lower := strings.ToLower(strings.TrimSpace(args[i]))
		if lower == grokArgCwdFlag {
			if i+1 < len(args) {
				add(args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(lower, grokArgCwdFlag+"=") {
			add(args[i][len(grokArgCwdFlag)+1:])
		}
	}
	return out
}

// grokConfigPathEnvVar / grokManagedConfigURLEnvVar are the two environment
// variables that point xAI's config LOADER somewhere other than the layers
// grokAnyPinnedCredential scans. They carry no credential themselves, which is
// why they are not in grokCredentialOverrideEnvVars — they name a configuration
// that can pin `model.api_key`/`env_key` for the child while every file on the
// scanned paths stays clean. sanitizeGrokMaintenanceSmokeEnv already strips
// both (it removes every inherited GROK_*) for exactly that reason; a DIRECT
// (PTY) run deliberately inherits the user's shell environment, so the
// attribution guard has to account for them itself.
const (
	grokConfigPathEnvVar       = "GROK_CONFIG_PATH"
	grokManagedConfigURLEnvVar = "GROK_MANAGED_CONFIG_URL"
)

// grokConfigLoaderEnvContests reports whether one inherited config-loader
// variable puts the launch's billing account in doubt.
//
// A managed-config URL is resolved by the child over the network, from a
// document this process never sees and cannot re-read at the instant the child
// loads it, so a non-empty value CONTESTS unconditionally.
//
// A config PATH names a local file, so it is inspected with the same
// credential sweep every other layer gets rather than contesting on presence
// alone — a developer who points GROK_CONFIG_PATH at an ordinary config keeps
// their session observable. An unreadable path contests: "we could not look"
// is not "there is nothing there", and this guard is conservative by
// construction (over-reporting costs one session's observability,
// under-reporting publishes one account's spend as another's).
//
// cwd is the directory the child is spawned in, and it is load-bearing: a
// RELATIVE GROK_CONFIG_PATH is resolved by the child against ITS cwd, not
// ours, so inspecting the raw value would stat a daemon-relative path that can
// be an unrelated clean file while the child's own path pins a credential.
func grokConfigLoaderEnvContests(name, value, cwd string) bool {
	switch name {
	case grokManagedConfigURLEnvVar:
		return true
	case grokConfigPathEnvVar:
		return grokLoaderConfigContests(value, cwd)
	}
	return false
}

// grokLoaderConfigContests reports whether the config file GROK_CONFIG_PATH
// names either pins a credential or cannot be read. The stat is what separates
// the two dismissible cases from each other: a file that is absent or
// unreadable to US may still be loaded by the child (a race, a permission
// difference, a path only the child's namespace resolves), so it fails closed.
//
// A relative path is anchored on the child's cwd. When the launch named no cwd
// there is nothing to anchor it to, and reading the daemon's own working
// directory would make the decision depend on ambient state the caller never
// named — the same reason grokArgSelectedCwds returns grokUnresolvableArgCwd —
// so it contests instead of inspecting a path the child never loads.
func grokLoaderConfigContests(path, cwd string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		base := strings.TrimSpace(cwd)
		if base == "" {
			return true
		}
		path = filepath.Join(base, path)
	}
	if _, err := os.Stat(path); err != nil {
		return true
	}
	return grokConfigPinsCredential(path)
}

// grokArgsPinCredential reports whether the argv a direct child is spawned with
// hands it a credential of its own. xAI documents `-c|--config <key>=value` as the
// per-process config-override surface and buildGrokInteractiveArgs forwards it
// verbatim, so `--config model.api_key=sk-...` is a credential neither the
// environment nor any config file on this machine ever sees.
//
// Every token is inspected rather than only the ones following a recognized
// flag: the same override is spelled `--config k=v`, `--config=k=v` and `-c k=v`
// across Grok releases, and over-reporting costs one session's observability
// while under-reporting publishes one account's spend as another's.
//
// A token that IS one of grok's own auth-override flags contests the launch on
// its own, in either the `--api-key-env=OTHER_VAR` or the space-separated
// `--api-key-env OTHER_VAR` spelling, and whether or not a value follows it in
// this argv. isGrokAuthOverrideArg is the repository's existing enumeration of
// that flag set (it is what the ACP side-door classifier strips), so reusing it
// keeps the two from disagreeing about what an auth override is — `--api-key-env`
// normalises to `apikeyenv`, which is a suffix of neither `api_key` nor
// `env_key`, so the config-key rule below can never recognise it.
func grokArgsPinCredential(args []string) bool {
	for _, arg := range args {
		if isGrokAuthOverrideArg(strings.ToLower(strings.TrimSpace(arg))) {
			return true
		}
		if grokArgPinsCredential(arg) {
			return true
		}
	}
	return false
}

// grokArgPinsCredential reports whether ONE argv token assigns a non-empty credential.
// The flag form nests the assignment inside the flag's own value
// (`--config=model.api_key=sk-...`), so a key that is not itself an API-key path
// is retried against the value once the outer `flag=` has been peeled off.
func grokArgPinsCredential(arg string) bool {
	key, value, ok := strings.Cut(arg, "=")
	if !ok {
		return false
	}
	if grokCredentialConfigKey(key) {
		return strings.TrimSpace(strings.Trim(value, `"'`)) != ""
	}
	return grokArgPinsCredential(value)
}

// grokCredentialConfigKey reports whether a config key path names a credential,
// in any of the spellings a flag can carry it (`model.api_key`,
// `model.grok-4.apiKey`, `--api-key`, and the `env_key` form that names the
// variable the key is read from rather than carrying it inline). Separator- and
// case-insensitive, so a rename across a CLI update does not silently reopen the
// misattribution this guard closes.
func grokCredentialConfigKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.TrimLeft(key, "-")
	key = strings.NewReplacer("_", "", "-", "").Replace(key)
	for _, suffix := range grokModelCredentialKeySuffixes {
		if strings.HasSuffix(key, strings.ReplaceAll(suffix, "_", "")) {
			return true
		}
	}
	return false
}

// grokProjectConfigMaxDepth bounds the upward `.grok/config.toml` walk. Deep
// enough for any real checkout, finite so a pathological path cannot turn a
// session start into an unbounded stat loop.
const grokProjectConfigMaxDepth = 64

// grokProjectPinnedCredential reports whether a repository-scoped
// `.grok/config.toml` at or above the child's working directory pins a
// credential (an inline `api_key` or an `env_key` naming one).
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
// BOTH the lexical chain and the symlink-RESOLVED chain are walked when they
// differ. A cwd that is a symlink into a checkout keeps its alias through
// filepath.Abs, but once exec.Cmd chdirs the child, the kernel's view of its
// working directory is the physical target — so grok's upward discovery reads
// the PHYSICAL repository's `.grok/config.toml`, which a lexical-only walk
// never stats. Walking the lexical chain too keeps platforms that preserve the
// alias in the process cwd covered; the guard is conservative by construction,
// so scanning a superset of what the child could load is the safe direction.
//
// One bounded stat walk per direct session start, never on the streaming path.
func grokProjectPinnedCredential(cwd string) bool {
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		return false
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	starts := []string{dir}
	if resolved, err := resolveCwdForContainment(dir); err == nil && resolved != dir {
		starts = append(starts, resolved)
	}
	for _, start := range starts {
		if grokProjectPinnedCredentialFrom(start) {
			return true
		}
	}
	return false
}

// grokProjectPinnedCredentialFrom walks one absolute chain upward, stopping at
// the volume root or grokProjectConfigMaxDepth.
func grokProjectPinnedCredentialFrom(dir string) bool {
	for depth := 0; depth < grokProjectConfigMaxDepth; depth++ {
		if grokConfigPinsCredential(filepath.Join(dir, ".grok", "config.toml")) {
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
