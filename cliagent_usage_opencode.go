// cliagent_usage_opencode.go — OpenCode (`opencode`) usage parser.
//
// # WHY READINESS IS PROBED, NOT INFERRED FROM A CREDENTIALS FILE
//
// OpenCode is provider-agnostic. Credentials can arrive from at least four
// places: `/connect` writing ~/.local/share/opencode/auth.json; a provider env
// var (ANTHROPIC_API_KEY, OPENAI_API_KEY, …); `{env:…}` / `{file:…}`
// substitution inside opencode.json; or NOT AT ALL, for a local model
// (Ollama, LM Studio, llama.cpp) or a bundled provider that needs no
// credential.
//
// So "no auth.json" does NOT mean "not signed in". A naive file-presence check
// would paint a red "Login required" chip on a perfectly working local-model
// install — the single worst failure mode this parser can have, because the
// user has no way to act on it.
//
// Readiness therefore comes from asking OpenCode itself: `opencode models`
// listing at least one model means the CLI can reach a provider, however the
// credential arrived. `opencode auth list` supplies per-provider NAMES for the
// card detail. The parser FAILS OPEN: anything inconclusive (timeout,
// unrecognized output, non-zero exit) reports authState "unknown", which
// matches none of getCodingAgentStatus's failure branches and falls through to
// available. The red chip requires positive evidence of *no usable provider*.
//
// # WHY THESE PROBES ARE NOT ON THE BINARY-KEYED VERSION CACHE
//
// `--version` is a pure function of the binary, so systemInfo.go caches it on
// (path, mtime, size). Readiness is not: it changes when the user runs
// `/connect`, exports a provider env var, or edits opencode.json — none of
// which touch the binary. Keying readiness on binary mtime would pin a
// just-authenticated user to a stale "Login required" chip until their next
// OpenCode upgrade, directly violating "refresh updates status without deleting
// and re-adding the computer". These probes use a short TIME-based TTL instead,
// and a user-initiated __cli_usage_refresh__ bypasses it by design.
//
// SECRETS: this parser reads provider NAMES and model IDS only. It never reads,
// logs, or forwards a token value from auth.json. There are no quota metrics —
// OpenCode delegates quota to whichever provider is underneath.
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// Per-probe ceiling. Both probes must fit inside the shared 10-second
	// GatherCLIAgentUsageOnly context alongside the other agents' parsers.
	openCodeProbeTimeout = 3 * time.Second
	// Readiness TTL — sized to the idle gather cadence, short enough that a
	// user who just authenticated sees the chip flip on their next refresh.
	openCodeReadinessTTL = 2 * time.Minute
)

// OpenCode auth states. Deliberately chosen to line up with the branches
// frontend getCodingAgentStatus already has:
//   - "ready"          → no failure branch matches → Online
//   - "unauthenticated" → matches the Login-required branch
//   - "unknown"        → matches NOTHING → falls through to available
//
// Fail-open is achieved by picking values that fit the existing branches, which
// is why frontend cliAgentAvailability.js needs no edit.
const (
	openCodeAuthReady           = "ready"
	openCodeAuthUnauthenticated = "unauthenticated"
	openCodeAuthUnknown         = "unknown"
)

type openCodeUsageParser struct{}

func (openCodeUsageParser) Provider() string { return "opencode" }

// openCodeReadiness is one probe cycle's answer.
type openCodeReadiness struct {
	AuthState string
	// Providers are provider NAMES only (e.g. "anthropic", "ollama") — never
	// tokens. Rendered in the card detail.
	Providers []string
	// Model is the device-level default, when resolvable.
	Model string
	// Conclusive is false when a probe timed out, exited non-zero, or printed
	// something unrecognized. Kept distinct from AuthState so a caller can tell
	// "we asked and the answer was no" from "we could not ask".
	Conclusive bool
}

// Readiness cache. Keyed by executable path so two installs (an upgrade that
// relocates the binary, or a test rebinding it) do not read each other's answer.
var (
	openCodeReadinessMu    sync.Mutex
	openCodeReadinessCache = map[string]openCodeReadinessEntry{}
)

type openCodeReadinessEntry struct {
	At     time.Time
	Result openCodeReadiness
}

// openCodeForceProbe is set for the duration of a user-initiated
// __cli_usage_refresh__ so an explicit refresh always re-probes. An idle gather
// inside the TTL reuses the cached answer.
var openCodeForceProbe bool

// SetOpenCodeReadinessForceProbe makes the next readiness probe bypass the TTL.
// Called by the __cli_usage_refresh__ handler: a user who just ran `/connect`
// and hit Refresh must not be told to wait out a cache.
func SetOpenCodeReadinessForceProbe(force bool) {
	openCodeReadinessMu.Lock()
	openCodeForceProbe = force
	openCodeReadinessMu.Unlock()
}

// resetOpenCodeReadinessCache clears the cache. Test-only seam.
func resetOpenCodeReadinessCache() {
	openCodeReadinessMu.Lock()
	openCodeReadinessCache = map[string]openCodeReadinessEntry{}
	openCodeForceProbe = false
	openCodeReadinessMu.Unlock()
}

func (p openCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	usage := &cliAgentUsage{
		CliAgentID:  "opencode",
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "OpenCode"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "opencode models",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	readiness := probeOpenCodeReadiness(detected.Path, home)
	usage.AuthState = readiness.AuthState
	usage.Model = readiness.Model
	// Authenticated is deliberately left NIL for "unknown": the frontend treats
	// `authenticated === false` as a hard Login-required signal, so writing
	// false here would defeat the whole fail-open design.
	switch readiness.AuthState {
	case openCodeAuthReady:
		usage.Authenticated = authBoolPtr(true)
	case openCodeAuthUnauthenticated:
		usage.Authenticated = authBoolPtr(false)
	}

	if len(readiness.Providers) > 0 {
		usage.Account = strings.Join(readiness.Providers, ", ")
	}
	// OpenCode keeps no durable session deadline of its own — each underlying
	// provider owns expiry. Do not reinterpret a file mtime as a login deadline.
	usage.LoginExpirationState = loginExpirationNotReported
	usage.AccountFingerprint = fingerprintAccount(p.Provider(), usage.Account)

	// No metrics: OpenCode delegates quota to the underlying provider and
	// exposes none of its own. The placeholder rows the other parsers emit
	// ("limits exist, values unobservable") would be a lie here — there is no
	// OpenCode-level limit window to observe.
	return usage, true
}

// probeOpenCodeReadiness returns the cached readiness for this binary, or runs
// the probes when the cache is cold, expired, or a refresh forced it.
func probeOpenCodeReadiness(executable, home string) openCodeReadiness {
	if strings.TrimSpace(executable) == "" {
		executable = resolveOpenCodeExecutable()
	}

	openCodeReadinessMu.Lock()
	forced := openCodeForceProbe
	entry, cached := openCodeReadinessCache[executable]
	openCodeReadinessMu.Unlock()

	if cached && !forced && time.Since(entry.At) < openCodeReadinessTTL {
		return entry.Result
	}

	result := probeOpenCodeReadinessUncached(executable, home)

	openCodeReadinessMu.Lock()
	openCodeReadinessCache[executable] = openCodeReadinessEntry{At: time.Now(), Result: result}
	// One forced probe per refresh — clear the flag so the rest of the gather
	// (and the idle cycles after it) go back through the TTL.
	openCodeForceProbe = false
	openCodeReadinessMu.Unlock()
	return result
}

func probeOpenCodeReadinessUncached(executable, home string) openCodeReadiness {
	out := openCodeReadiness{AuthState: openCodeAuthUnknown}

	models, modelsOK := runOpenCodeProbe(executable, "models")
	if !modelsOK {
		// Timeout, non-zero exit, or the binary vanished between detection and
		// probe. We could not ask, so we do not answer — fail open.
		out.Model = readOpenCodeConfiguredModel(home)
		return out
	}

	modelIDs := parseOpenCodeModelList(models)
	if len(modelIDs) > 0 {
		out.AuthState = openCodeAuthReady
		out.Conclusive = true
	} else if looksLikeEmptyOpenCodeModelList(models) {
		// Positive evidence of no usable provider — the ONLY path that lights
		// the red chip.
		out.AuthState = openCodeAuthUnauthenticated
		out.Conclusive = true
	}
	// Anything else (unrecognized output shape) stays "unknown".

	// Per-provider detail for the card. Best-effort and never affects the
	// auth state: `auth list` failing on a machine whose models list is
	// non-empty must not downgrade a working install.
	if authOut, ok := runOpenCodeProbe(executable, "auth", "list"); ok {
		out.Providers = parseOpenCodeAuthProviders(authOut)
	}
	if len(out.Providers) == 0 {
		out.Providers = openCodeProvidersFromModelIDs(modelIDs)
	}

	out.Model = firstNonEmpty(readOpenCodeConfiguredModel(home), openCodeSingleModel(modelIDs))
	return out
}

// runOpenCodeProbe runs `opencode <args…>` with a short timeout and returns its
// combined output. ok=false means the probe was INCONCLUSIVE (timeout, non-zero
// exit, spawn failure) — the caller must not read a verdict into that.
func runOpenCodeProbe(executable string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	hideWindow(cmd)
	// The probes must not inherit another agent's credentials any more than a
	// real turn does.
	cmd.Env = sanitizeOpenCodeEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// parseOpenCodeModelList extracts model identifiers from `opencode models`.
//
// Two output shapes are handled: a JSON array/object (newer builds) and one
// `provider/model` per line (the plain-text default). Lines that are obviously
// prose — a banner, a "no providers configured" notice, a usage hint — are
// rejected, because counting one of those as a model would report an
// unauthenticated machine as ready.
func parseOpenCodeModelList(out string) []string {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}

	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		if ids := parseOpenCodeModelJSON(trimmed); len(ids) > 0 {
			return ids
		}
	}

	seen := map[string]bool{}
	var ids []string
	for _, line := range strings.Split(trimmed, "\n") {
		id := strings.TrimSpace(line)
		// Strip a leading list bullet / selection marker.
		id = strings.TrimLeft(id, "*-• \t")
		id = strings.TrimSpace(id)
		if !looksLikeOpenCodeModelID(id) {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func parseOpenCodeModelJSON(trimmed string) []string {
	// Array of strings, or of objects carrying an id.
	var rawList []json.RawMessage
	if json.Unmarshal([]byte(trimmed), &rawList) == nil {
		var ids []string
		for _, raw := range rawList {
			var str string
			if json.Unmarshal(raw, &str) == nil {
				if looksLikeOpenCodeModelID(str) {
					ids = append(ids, str)
				}
				continue
			}
			var obj struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Name  string `json:"name"`
			}
			if json.Unmarshal(raw, &obj) == nil {
				if id := firstNonEmpty(obj.ID, obj.Model, obj.Name); looksLikeOpenCodeModelID(id) {
					ids = append(ids, id)
				}
			}
		}
		return ids
	}

	// Object keyed by provider → list of models.
	var byProvider map[string][]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &byProvider) == nil {
		var ids []string
		for provider, raws := range byProvider {
			for _, raw := range raws {
				var str string
				if json.Unmarshal(raw, &str) == nil && str != "" {
					ids = append(ids, provider+"/"+str)
					continue
				}
				var obj struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &obj) == nil && obj.ID != "" {
					ids = append(ids, provider+"/"+obj.ID)
				}
			}
		}
		sort.Strings(ids)
		return ids
	}
	return nil
}

// looksLikeOpenCodeModelID gates what counts as a model row. OpenCode addresses
// models as `provider/model`, so requiring exactly that shape — with no spaces
// — rejects banners and notices without needing to enumerate their wording.
func looksLikeOpenCodeModelID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	provider, model, ok := strings.Cut(s, "/")
	if !ok {
		return false
	}
	return provider != "" && model != "" && !strings.Contains(model, "/")
}

// openCodeEmptyModelListNeedles are the phrasings that constitute POSITIVE
// evidence of "no usable provider". Matching is deliberately narrow: anything
// unrecognized must stay "unknown" and fail open, so a future rewording
// degrades to no chip rather than a wrong red one.
var openCodeEmptyModelListNeedles = []string{
	"no models",
	"no providers",
	"no provider configured",
	"not logged in",
	"not authenticated",
	"no credentials",
	"run `opencode auth login`",
	"run 'opencode auth login'",
	"opencode auth login",
}

func looksLikeEmptyOpenCodeModelList(out string) bool {
	lowered := strings.ToLower(out)
	for _, n := range openCodeEmptyModelListNeedles {
		if strings.Contains(lowered, n) {
			return true
		}
	}
	// A genuinely empty stdout from a zero-exit `models` is also conclusive:
	// the CLI ran, was asked what it can reach, and named nothing.
	return strings.TrimSpace(out) == ""
}

// parseOpenCodeAuthProviders extracts provider NAMES from `opencode auth list`.
// Token values are never read: only the leading identifier on each row is kept,
// and anything containing whitespace or looking like a secret is skipped.
func parseOpenCodeAuthProviders(out string) []string {
	seen := map[string]bool{}
	var providers []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "*-• \t")
		if line == "" {
			continue
		}
		lowered := strings.ToLower(line)
		if strings.HasPrefix(lowered, "credentials") || strings.HasPrefix(lowered, "no ") {
			continue
		}
		// Row shapes seen across releases: "anthropic", "anthropic (oauth)",
		// "anthropic  api". Take the first whitespace-delimited token.
		name := strings.Fields(line)[0]
		name = strings.Trim(name, ":()[]")
		if name == "" || len(name) > 40 || seen[name] {
			continue
		}
		// A token-looking blob is never a provider name.
		if strings.ContainsAny(name, "=.") && len(name) > 24 {
			continue
		}
		seen[name] = true
		providers = append(providers, name)
	}
	return providers
}

// openCodeProvidersFromModelIDs derives provider names from `provider/model`
// ids, used when `auth list` is unavailable. A local-model install typically
// reports no auth entries at all, so this keeps the card informative there.
func openCodeProvidersFromModelIDs(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		provider, _, ok := strings.Cut(id, "/")
		if !ok || provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// openCodeSingleModel returns the model id when exactly one is available — the
// only case where the reachable list itself identifies the default.
func openCodeSingleModel(ids []string) string {
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// readOpenCodeConfiguredModel reads the DEVICE-LEVEL default model from
// OpenCode's global config.
//
// A project-level opencode.json inside the session cwd can override this, so a
// running session may use a different model than the card shows — the card
// labels the value as the device default for exactly that reason. Resolving
// per-repo would need a cwd this discovery pass does not have.
func readOpenCodeConfiguredModel(home string) string {
	for _, path := range openCodeGlobalConfigPaths(home) {
		if path == "" {
			continue
		}
		var cfg struct {
			Model string `json:"model"`
		}
		if readJSONFile(path, &cfg) {
			if m := strings.TrimSpace(cfg.Model); m != "" {
				return m
			}
		}
	}
	return ""
}

// openCodeGlobalConfigPaths lists the global config locations OpenCode has used
// across releases, most-current first.
func openCodeGlobalConfigPaths(home string) []string {
	var paths []string
	if cfgHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); cfgHome != "" {
		paths = append(paths, filepath.Join(cfgHome, "opencode", "opencode.json"))
	}
	if home != "" {
		paths = append(paths,
			expandHome(home, filepath.Join(".config", "opencode", "opencode.json")),
			expandHome(home, filepath.Join(".opencode", "opencode.json")),
			expandHome(home, "opencode.json"),
		)
	}
	return paths
}
