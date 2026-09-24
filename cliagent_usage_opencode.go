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
// credential arrived. The provider names on the card — which double as the
// snapshot's Account, the identity the capacity view keys the install by —
// come from those model ids; `opencode auth list` is asked only when the ids
// name no provider. The parser FAILS OPEN: anything inconclusive (timeout,
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
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

// Compile-time proof that the parser still satisfies the context parser
// interface: runProviderParseSafely selects ParseContext through a TYPE
// ASSERTION that fails silently, and Parse's context.Background() would let the
// two readiness probes run their full 3s each after the gather's shared 10s
// budget is already gone (Codex round 17 on #147).
var _ cliAgentUsageContextParser = openCodeUsageParser{}

func (openCodeUsageParser) Provider() string { return "opencode" }

// openCodeReadiness is one probe cycle's answer.
type openCodeReadiness struct {
	AuthState string
	// Providers are provider NAMES only (e.g. "anthropic", "ollama") — never
	// tokens. Rendered in the card detail.
	Providers []string
	// Model is the device-level default, when resolvable.
	Model string
	// Models is the bounded list of model ids `opencode models` reports. This is
	// the card's actual content for OpenCode: it has no quota to plot, so "what
	// can this install reach" is the question the card answers.
	Models []string
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
	// openCodeLastProviders is the last provider list a probe of this binary
	// could name. OpenCode has no login, so the joined list IS the snapshot's
	// Account, and the backend and frontend key an account by that string. A
	// probe that could not ask (timeout, non-zero exit) must not rename the
	// install for a cycle: every rename minted a second "account" for the same
	// computer in the capacity view.
	openCodeLastProviders = map[string][]string{}
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
	openCodeLastProviders = map[string][]string{}
	openCodeForceProbe = false
	openCodeReadinessMu.Unlock()
}

func (p openCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	return p.ParseContext(context.Background(), home, detected, now)
}

// ParseContext is the gather's entry point: both readiness probes derive their
// deadline from ctx, so under GatherCLIAgentUsageOnly's shared budget they end
// with the gather instead of overrunning it on their own clocks.
func (p openCodeUsageParser) ParseContext(ctx context.Context, home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	usage := &cliAgentUsage{
		CliAgentID:  "opencode",
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "OpenCode"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "opencode models",
		CollectedAt: now.UTC().Format(time.RFC3339),
	}

	readiness := probeOpenCodeReadiness(ctx, detected.Path, home)
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
	usage.Models = readiness.Models
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
func probeOpenCodeReadiness(ctx context.Context, executable, home string) openCodeReadiness {
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

	result := probeOpenCodeReadinessUncached(ctx, executable, home)
	openCodeReadinessMu.Lock()
	switch {
	case len(result.Providers) > 0:
		openCodeLastProviders[executable] = result.Providers
	case result.Conclusive:
		// A conclusive "no usable provider" is a real change: forget the old
		// names, or a later probe that cannot ask would bring them back.
		delete(openCodeLastProviders, executable)
	default:
		// We could not ask: keep the name the install last had.
		result.Providers = openCodeLastProviders[executable]
	}
	openCodeReadinessMu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		// The probes lost the gather's deadline rather than answering. Caching
		// that "unknown" would pin the card to it for the whole TTL (and clear
		// the user's forced re-probe) because one refresh ran out of time, so
		// return it uncached and let the next gather ask again.
		return result
	}

	openCodeReadinessMu.Lock()
	openCodeReadinessCache[executable] = openCodeReadinessEntry{At: time.Now(), Result: result}
	// One forced probe per refresh — clear the flag so the rest of the gather
	// (and the idle cycles after it) go back through the TTL.
	openCodeForceProbe = false
	openCodeReadinessMu.Unlock()
	return result
}

func probeOpenCodeReadinessUncached(ctx context.Context, executable, home string) openCodeReadiness {
	out := openCodeReadiness{AuthState: openCodeAuthUnknown}

	models, modelsOK := runOpenCodeProbe(ctx, executable, "models")
	if !modelsOK {
		// Timeout, non-zero exit, or the binary vanished between detection and
		// probe. We could not ask, so we do not answer — fail open.
		out.Model = readOpenCodeConfiguredModel(home)
		return out
	}

	// The provider names come from the WHOLE catalog; only the published
	// Models list is capped. Derived from the capped list, a catalog longer
	// than the cap would drop the providers listed after the cut, and the
	// name would change whenever OpenCode reordered its catalog.
	allModelIDs := parseOpenCodeModelIDs(models)
	modelIDs := capOpenCodeModelIDs(allModelIDs)
	if len(modelIDs) > 0 {
		out.AuthState = openCodeAuthReady
		out.Conclusive = true
		out.Models = modelIDs
	} else if looksLikeEmptyOpenCodeModelList(models) {
		// Positive evidence of no usable provider — the ONLY path that lights
		// the red chip.
		out.AuthState = openCodeAuthUnauthenticated
		out.Conclusive = true
	}
	// Anything else (unrecognized output shape) stays "unknown".

	// Provider names for the card. The joined list is also the snapshot's
	// Account — the identity the capacity view keys this install by — so it
	// must not depend on which probe happened to answer. It used to prefer
	// `auth list` and fall back to the model ids when that optional probe was
	// skipped for time, which renamed the same install whenever the budget
	// tipped ("google" one refresh, "google, opencode" the next), each name a
	// separate account. The model ids are the stable source: every provider
	// that can run lists its models, and they carry the provider ID where
	// `auth list` prints a display name ("GitHub Copilot", not
	// "github-copilot"). `auth list` is asked only when the ids name nothing.
	//
	// It is best-effort and never affects the auth state: `auth list` failing
	// must not downgrade a working install. It is also OPTIONAL, so it is only
	// handed what the gather can spare: a stall here until the parent expired
	// would have runProviderParseSafely discard the conclusive models answer
	// above and report the providers behind OpenCode as canceled.
	out.Providers = openCodeProvidersFromModelIDs(allModelIDs)
	if len(out.Providers) == 0 {
		if authCtx, release, affordable := optionalOpenCodeProbeContext(ctx); affordable {
			if authOut, ok := runOpenCodeProbe(authCtx, executable, "auth", "list"); ok {
				out.Providers = parseOpenCodeAuthProviders(authOut)
			}
			release()
		}
	}

	out.Model = firstNonEmpty(readOpenCodeConfiguredModel(home), openCodeSingleModel(modelIDs))
	return out
}

// openCodeOptionalProbeReserve is what the best-effort `auth list` probe leaves
// a bounded gather for the providers still to be polled: one utilization
// probe's worth, the same reserve model discovery keeps.
const openCodeOptionalProbeReserve = machineInfoProbeTimeout

// optionalOpenCodeProbeContext hands the best-effort `auth list` probe what the
// gather can spare: at most openCodeProbeTimeout, and never the gather's last
// openCodeOptionalProbeReserve. ok=false means the probe is skipped; the
// provider names then derive from the model ids the conclusive probe listed.
// An unbounded caller (the periodic gather) passes through unchanged.
func optionalOpenCodeProbeContext(ctx context.Context) (probeCtx context.Context, release func(), ok bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return ctx, func() {}, true
	}
	allowance := min(openCodeProbeTimeout, time.Until(deadline)-openCodeOptionalProbeReserve)
	if allowance <= 0 {
		return ctx, func() {}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, allowance)
	return probeCtx, cancel, true
}

// runOpenCodeProbe runs `opencode <args…>` with a short timeout and returns its
// combined output. ok=false means the probe was INCONCLUSIVE (timeout, non-zero
// exit, spawn failure) — the caller must not read a verdict into that.
//
// The timeout DERIVES from the caller's context rather than starting a fresh
// one: context.WithTimeout takes the EARLIER of the two, so a probe is capped at
// openCodeProbeTimeout on the unbounded path and at whatever the gather has
// left on the demand-driven one — it can never hold the refresh past the
// deadline the handler documents.
func runOpenCodeProbe(ctx context.Context, executable string, args ...string) (string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, openCodeProbeTimeout)
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
	return capOpenCodeModelIDs(parseOpenCodeModelIDs(out))
}

// parseOpenCodeModelIDs is parseOpenCodeModelList without the receipt cap:
// every distinct model id, in the CLI's order.
func parseOpenCodeModelIDs(out string) []string {
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
		// Strips escapes and the frame/bullet glyphs together; the same output is
		// coloured whenever OpenCode believes it has a terminal.
		id := stripTerminalDecoration(line)
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

// capOpenCodeModelIDs deduplicates (first occurrence wins, order kept — the
// plain-text path already does this while scanning) BEFORE applying the
// receipt cap, so a repeat among the first entries can never push a distinct
// later model past the cut and leave a shortened list that reads as complete.
func capOpenCodeModelIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
		if len(unique) == cliUsageMaxModelsPerProvider {
			break
		}
	}
	return unique
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
// models as `provider/model`, so requiring that shape — with no spaces — rejects
// banners and notices without needing to enumerate their wording.
//
// The model half MAY contain further slashes: a vendor-namespaced id like
// `mlx/mlx-community/Qwen3.8-27B-8bit` is one model, not a malformed row.
// Rejecting those dropped every local mlx model from the list AND lost "mlx"
// from the providers derived from it.
//
// What keeps that relaxation safe is the PROVIDER half, which must look like a
// provider id. That is what separates a model row from a bare path such as
// `~/.local/share/opencode/auth.json`, which would otherwise now qualify.
func looksLikeOpenCodeModelID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	provider, model, ok := strings.Cut(s, "/")
	if !ok || model == "" {
		return false
	}
	return openCodeProviderNameRe.MatchString(strings.ToLower(provider))
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
	lowered := strings.ToLower(ansiEscapeRe.ReplaceAllString(out, ""))
	for _, n := range openCodeEmptyModelListNeedles {
		if strings.Contains(lowered, n) {
			return true
		}
	}
	// A genuinely empty stdout from a zero-exit `models` is also conclusive:
	// the CLI ran, was asked what it can reach, and named nothing. Check the
	// escape-stripped value, not the raw `out`: reset codes like "\x1b[0m"
	// survive TrimSpace, so probing them against `out` would leave an
	// otherwise-empty response stuck at "unknown".
	return strings.TrimSpace(lowered) == ""
}

// ansiEscapeRe matches the SGR/CSI sequences OpenCode writes when it decides it
// is talking to a terminal. Removing them is not cosmetic: they survive
// TrimSpace and Fields, so an escape becomes the "first token on the row" and
// gets published as a provider name.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// openCodeProviderNameRe is what a provider identifier actually looks like:
// lowercase alphanumerics with internal separators (anthropic, openai,
// github-copilot, ollama, mlx). An allowlist rather than a junk denylist,
// because the junk is unbounded — box-drawing characters, escape sequences,
// spinner frames — and every unrecognised shape must fail to a name we simply
// do not show.
var openCodeProviderNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// stripTerminalDecoration removes ANSI escapes and the box-drawing / bullet
// glyphs OpenCode frames its output with, leaving the text a parser can reason
// about.
func stripTerminalDecoration(s string) string {
	s = ansiEscapeRe.ReplaceAllString(s, "")
	return strings.TrimFunc(s, func(r rune) bool {
		switch {
		case unicode.IsSpace(r):
			return true
		// General punctuation: the bullets, dashes and exotic spaces used as list
		// markers (U+2022 •, U+2013 –, U+00A0 …). No identifier character lives
		// in this block.
		case r >= 0x2000 && r <= 0x206f:
			return true
		// Box drawing, block elements, geometric shapes and dingbats — the frame
		// OpenCode draws around its output.
		case r >= 0x2500 && r <= 0x27bf:
			return true
		case r == '*' || r == '-' || r == '|':
			return true
		}
		return false
	})
}

// parseOpenCodeAuthProviders extracts provider NAMES from `opencode auth list`.
// Token values are never read: only the leading identifier on each row is kept,
// and anything that is not shaped like a provider id is skipped.
//
// The output is a drawn frame, not a plain list. On a real install:
//
//	\x1b[0m
//	█▌  Credentials \x1b[90m~/.local/share/opencode/auth.json
//	█▂
//	█▄  0 credentials
//
// Every one of those four lines used to yield a "provider": the bare escape
// became its own row, and the box glyph was the first whitespace-delimited token
// on the others — so the card's Account read "[0m, ␍, |, ᴸ". Decoration is now
// stripped first, header/summary rows are recognised after stripping, and a name
// must match openCodeProviderNameRe to be published.
func parseOpenCodeAuthProviders(out string) []string {
	seen := map[string]bool{}
	var providers []string
	for _, line := range strings.Split(out, "\n") {
		line = stripTerminalDecoration(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Without a Unicode-capable terminal — the tray agent has none — the
		// frame is drawn in ASCII: "T" opens a section, "|" continues it, "—"
		// closes it, "•" marks a row. The punctuation is stripped above, but "T"
		// is a letter and was published as a provider named "t".
		if len(fields) > 1 && openCodeASCIIFrameGlyphs[fields[0]] {
			fields = fields[1:]
			line = strings.Join(fields, " ")
		}
		if len(fields) == 0 {
			continue
		}
		lowered := strings.ToLower(line)
		// Frame chrome: the "Credentials <path>" and "Environment" section
		// headers, and their "N credentials" / "N environment variables" footers
		// — the rows that COUNT providers and must never be mistaken for one.
		if strings.HasPrefix(lowered, "credentials") ||
			lowered == "environment" ||
			strings.HasPrefix(lowered, "no ") ||
			openCodeCredentialCountRe.MatchString(lowered) {
			continue
		}
		// Row shapes seen across releases: "anthropic", "anthropic (oauth)",
		// "anthropic  api". Take the first whitespace-delimited token.
		name := strings.ToLower(strings.Trim(fields[0], ":()[]"))
		if name == "" || len(name) > 40 || seen[name] {
			continue
		}
		if !openCodeProviderNameRe.MatchString(name) {
			continue
		}
		seen[name] = true
		providers = append(providers, name)
	}
	return providers
}

// openCodeCredentialCountRe matches the "0 credentials" / "1 environment
// variable" section footers. Matched on the whole (stripped) line so a provider
// literally named e.g. "2credentials" could not be swallowed by it.
var openCodeCredentialCountRe = regexp.MustCompile(`^\d+\s+(credentials?|environment\s+variables?)\b`)

// openCodeASCIIFrameGlyphs are the letters and symbols OpenCode's prompt
// library draws its frame with when the terminal cannot render Unicode, and
// that stripTerminalDecoration cannot strip because they are ordinary
// characters. Dropped only as a row's FIRST field with text after it.
var openCodeASCIIFrameGlyphs = map[string]bool{
	"T": true, ">": true, "x": true, "o": true, "!": true,
}

// openCodeProvidersFromModelIDs derives provider names from `provider/model`
// ids, sorted. It is the primary source of the card's provider names (and so
// of the Account): the ids are provider IDs, present on every install that can
// run, including a local-model one with no auth entries at all.
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
