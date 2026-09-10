// cliagent_ratelimit_grok.go — passively captures Grok Build's discrete
// usage-limit signal off the streaming-json stdout we already scan in
// session.go, and caches the latest state to disk. This path is notice-only:
// neither direct stdout nor ACP tool-result content is accepted as numeric or
// confirmed-unmetered telemetry; those states come only from Grok's bounded,
// account-bound billing log reader.
//
// Why this exists (and why it is NOT a percentage cache like Codex's):
//
//	xAI exposes NO numeric request/token quota — not in auth.json, not in the
//	headless `-p` JSON envelope, not in any response header (the only headers
//	Grok reads are `retry-after` and an unrelated `x-size-limit`). What Grok
//	DOES surface is a discrete, server-pushed signal when you near or hit the
//	cap: a `usage_limit_reached` session-update frame, a two-tier credit event
//	(`credit_limit_upsell_shown` soft → `credit_limit_hit` hard), and an
//	access-gate (`allow_access:false` + `gate_message` / `gate_url`). None of
//	these carry a "% remaining", so there is nothing to render as a gauge —
//	only an approaching/reached WARNING. That is what this captures.
//
// One consumer reads the cache this writes:
//  1. cliagent_usage_grok.go — turns the latest non-stale state into the
//     card-level notice (warning / error banner) shown on the CLI Agents tab,
//     while the capacity bars stay Unknown (no numeric quota exists).
//
// ensureGrokBillingAttribution is the session-scoped sibling of the per-line
// capture: same best-effort contract, different trigger. It records WHO is
// signed in so the billing log reader can attribute a direct run's records —
// it never infers a number from stdout either.
//
// Best-effort throughout: every failure is silent (this runs in the hot
// streaming path and must never break a session).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Grok usage-limit severities, mirrored by cliagent_usage_grok.go onto the
// card-level notice severity ("approaching" → warning, "reached" → error).
const (
	grokLimitApproaching = "approaching"
	grokLimitReached     = "reached"
)

// grokLimitNoticeTTL bounds how long a captured limit state is trusted. Grok's
// quota windows roll over (daily / periodic) and there is no "cleared" frame to
// observe, so a stale "reached" must not stick on the card forever. Picked long
// enough to span a session's worth of follow-ups but short of a daily reset, so
// a genuinely active limit keeps re-arming on each near-limit turn while a
// yesterday's limit decays on its own.
const grokLimitNoticeTTL = 6 * time.Hour

// grokUsageLimitState is the on-disk cache (one state, latest-wins).
// AccountFingerprint pins it to the Grok account that produced it so a stale
// limit can't bleed onto a different account after a `grok login`.
type grokUsageLimitState struct {
	Severity           string `json:"severity"` // grokLimitApproaching | grokLimitReached
	Message            string `json:"message,omitempty"`
	UpgradeURL         string `json:"upgradeUrl,omitempty"`
	ObservedAt         string `json:"observedAt"`
	ObservedAtMs       int64  `json:"observedAtMs"`
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
}

var grokUsageLimitMu sync.Mutex

// grokUsageLimitCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_GROK_LIMIT_CACHE overrides it (tests isolate from the real machine
// cache; ops can relocate it if the data dir is read-only).
func grokUsageLimitCachePath() string {
	if p := os.Getenv("AIEXPEDITE_GROK_LIMIT_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "grok_usage_limit.json")
}

// grokLimitNoticeScope is the account ONE running Grok session's limit notices
// may be cached under, frozen at session start.
//
// The cache is keyed by account fingerprint so a stale limit cannot bleed onto
// a different account. Re-reading that fingerprint per frame — off whatever
// login the ambient Grok home holds at that instant — answers the wrong
// question twice. A direct child spawned with a credential override bills an
// account the cached login does not name (the disagreement
// grokDirectRunBillingIdentity already contests), and a managed ACP/smoke child
// keeps billing its own isolated login even after a `grok login` re-points the
// persistent home mid-session. Either way the notice is filed under an account
// that did not produce it and shows there for grokLimitNoticeTTL.
//
// cacheable is false when nobody may be named for the producer — the contested
// case. It is deliberately NOT "cache under some other key": a write with a
// different fingerprint discards the prior state outright, so caching a
// contested notice would also drop a still-live reached state belonging to the
// account that legitimately earned it. Dropping the notice matches what the
// billing path does for the same producer (falls back to unobservable).
type grokLimitNoticeScope struct {
	fingerprint string
	cacheable   bool
}

// grokAccountLimitNoticeScope freezes the scope for a session whose child runs
// under the login in `base` — an isolated ACP/smoke home, or the persistent
// home of an uncontested direct run.
func grokAccountLimitNoticeScope(base string) grokLimitNoticeScope {
	if base == "" {
		return grokLimitNoticeScope{}
	}
	return grokLimitNoticeScope{fingerprint: grokAccountFingerprintFor(base), cacheable: true}
}

// grokManagedRunLimitNoticeScope freezes the scope for a MANAGED (ACP or
// maintenance-smoke) run whose child is spawned as `launch` against the
// isolated home `base`.
//
// The copied login is the producer only when the child carries no credential of
// its own, and isolation does not guarantee that. GROK_HOME neutralises the
// user-level config layers by omission, but it redirects neither the system
// layers nor the workspace `.grok/config.toml` that Grok walks upward from the
// child's cwd to find — and with Config.EnableGrokAPIKeyFallback the ACP path
// deliberately preserves XAI_API_KEY and copies the persisted `[model] api_key`
// line into the isolated config. Any of those bills an account the copied login
// does not name, so filing that account's approaching/reached notice under the
// copied login's fingerprint shows one account's limit on another's card for
// grokLimitNoticeTTL.
//
// grokDirectRunCredentialOverride answers exactly that question for a spawn —
// its inputs are a child's environment, cwd and argv, and the home whose cached
// login is the claim under test — so the managed arm reuses it rather than
// growing a second, drifting notion of what a credential is. Conservative the
// same way: an override that is merely AVAILABLE contests, because which
// credential the CLI resolves is its own per-turn decision inside a process we
// do not observe. Contesting costs this session's notice; naming it wrong
// publishes one account's limit as another's.
func grokManagedRunLimitNoticeScope(launch grokDirectRunLaunch, base string) grokLimitNoticeScope {
	if base == "" {
		return grokLimitNoticeScope{}
	}
	if grokDirectRunCredentialOverride(launch, base) {
		return grokLimitNoticeScope{}
	}
	return grokAccountLimitNoticeScope(base)
}

// grokDirectRunLimitNoticeScope freezes the scope for a DIRECT (PTY) run.
//
// identity is the value the attribution keeper was ARMED with — the single
// direct-arm decision — so this refuses the cache for exactly the producers the
// billing log refuses to name, and never re-resolves credentials that may have
// changed since the arm.
func grokDirectRunLimitNoticeScope(identity, base string) grokLimitNoticeScope {
	if identity == "" || strings.EqualFold(identity, grokContestedBillingIdentity) {
		return grokLimitNoticeScope{}
	}
	return grokAccountLimitNoticeScope(base)
}

// captureGrokUsageLimitLine parses one stdout line from a Grok streaming-json
// session and, if it carries a usage-limit / credit-limit / access-gate signal,
// records it in the on-disk cache under the session-frozen `scope`.
// Best-effort; silent on every failure.
func captureGrokUsageLimitLine(line string, now time.Time, scope grokLimitNoticeScope) {
	if !scope.cacheable {
		return
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	// Cheap prefilter: only decode lines that could plausibly carry a limit
	// signal. Covers the streaming-json event types, the credit/gate fields,
	// and the `retry-after` throttle marker.
	if !strings.Contains(trimmed, "usage_limit") &&
		!strings.Contains(trimmed, "limit_reached") &&
		!strings.Contains(trimmed, "credit_limit") &&
		!strings.Contains(trimmed, "gate_message") &&
		!strings.Contains(trimmed, "gate_url") &&
		!strings.Contains(trimmed, "allow_access") &&
		!strings.Contains(trimmed, "retry_after") &&
		!strings.Contains(trimmed, "retry-after") {
		return
	}
	var raw map[string]interface{}
	if json.Unmarshal([]byte(trimmed), &raw) != nil {
		return
	}
	state, ok := grokLimitStateFromFrame(raw, now)
	if !ok {
		return
	}
	writeGrokUsageLimitState(grokUsageLimitCachePath(), state, scope.fingerprint)
}

// grokBillingAttributionSerialize serializes the read-then-append below so two
// Grok sessions starting at once cannot both see a missing identity and both
// append one. It is a lock, NOT a cache: the log is re-read on every call.
//
// A time-bounded "already verified recently" memo was tried and removed. Any
// window at all is a window in which someone else's identity can become the
// newest one in the log — a managed session for another account persisting its
// marker on exit, or a `grok` run outside the agent — and
// grokRecordBelongsToCurrentAccount binds a record to the NEAREST identity
// preceding it, so every direct record written during that window would be
// attributed to the wrong account and refused. The check is one bounded tail
// read per SESSION START, next to spawning a CLI process; it is not on the
// per-line streaming path.
var grokBillingAttributionSerialize sync.Mutex

// ensureGrokBillingAttribution names the signed-in account in the provider-owned
// log so the billing records a DIRECT (PTY) Grok run writes afterwards can be
// tied to it. Without a producer identity logged before a record,
// grokRecordBelongsToCurrentAccount refuses it and a direct run can never
// produce an observable metric.
//
// Session-scoped, NOT per stdout line: it reads the log, and doing that per line
// on the streaming hot path would cost a file read per output line.
// captureGrokUsageLimitLine's per-line contract is untouched, and this path
// infers no telemetry from stdout — it only records who is signed in.
//
// Appends only when our identity is not already the NEWEST one in the log, so
// attribution self-heals after a rotation, an account change, or a displacing
// marker, while staying at most one small append per displacement.
// Best-effort and silent on failure, matching this file's hot-path contract.
// launch is the credential surface the direct child is spawned with — its
// environment, working directory and argv — and it decides whether the cached
// login can be named at all (grokDirectRunBillingIdentity). Pass the zero value
// when there is no child: the caller is then asserting the cached login with no
// credential override in play.
func ensureGrokBillingAttribution(launch grokDirectRunLaunch) {
	base := grokPersistentHome()
	if base == "" {
		return
	}
	identity := grokDirectRunBillingIdentity(launch, base)
	if identity == "" {
		return
	}
	ensureGrokBillingIdentityNamed(base, identity)
}

// grokDirectRunBillingIdentity returns the producer identity a DIRECT (PTY)
// run's billing records can honestly be attributed to, or "" when there is
// nothing to name.
//
// Normally that is the cached login in `base`. When the child carries a
// credential override — an inherited API key / provider token, a key pinned in
// its own argv (`--config model.api_key=...`), or one pinned in the repository
// it runs in, the user's own config.toml or a system layer — it is the contested
// sentinel instead: the child may bill an account we cannot resolve, and naming
// the cached login above records it did not pay for would publish one account's
// spend as another's. The sentinel matches no account, so those records are
// refused and the card falls back to "unobservable" — the state that shipped
// before direct attribution existed, and the one a two-account disagreement
// already produces.
//
// The SINGLE decision point for a direct arm: the keeper must be armed with the
// same value this names, or a re-assertion would reinstate the identity the
// override just ruled out.
func grokDirectRunBillingIdentity(launch grokDirectRunLaunch, base string) string {
	if grokDirectRunCredentialOverride(launch, base) {
		return grokContestedBillingIdentity
	}
	identity, ok := grokResolvedBillingIdentity(base)
	if !ok {
		return ""
	}
	return identity
}

// grokArmableBillingIdentity is the ONE normalization a live direct arm goes
// through before it is either armed on the keeper or written as a marker.
//
// An arm whose identity could not be RESOLVED is not an arm with no opinion —
// it is a live run whose records we cannot honestly name. The contested
// sentinel is exactly that statement ("someone is live that nobody may be
// named for"), it is already what a credential override arms with, and it
// matches no account, so those records are refused rather than bound to
// whichever account the log happens to end under.
//
// It is a helper rather than logic buried inside the keeper because the caller
// must apply it to BOTH calls: normalizing only inside the keeper left the
// pre-spawn marker holding the original empty value, and
// ensureGrokBillingIdentityNamed no-ops on empty — so an unresolved arm wrote
// no marker at all, and the child's first identity-less billing record bound to
// whatever earlier account marker the log still ended under and was published
// as that account's utilization.
func grokArmableBillingIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return grokContestedBillingIdentity
	}
	return identity
}

// ensureGrokBillingIdentityNamed is the same guard for a CAPTURED account
// rather than whichever one the credentials resolve to right now.
//
// The distinction is the account boundary. A direct run keeps writing billing
// records under the credentials it was SPAWNED with, while a `grok login` in the
// shared home changes only who a LATER process would be. Re-reading the auth
// file on every re-assertion would name the new account above the old process's
// records and publish one account's utilization as another's — worse than the
// unattributable record the re-assertion exists to prevent. So the keeper and
// the managed merge both re-assert a CAPTURED identity, and reading the live
// credentials stays confined to session start, where the two are the same thing
// by construction.
func ensureGrokBillingIdentityNamed(base, identity string) {
	identity = strings.TrimSpace(identity)
	if base == "" || identity == "" {
		return
	}

	grokBillingAttributionSerialize.Lock()
	defer grokBillingAttributionSerialize.Unlock()

	// A live direct run under a DIFFERENT account makes this marker a claim we
	// cannot support: the next record in the shared log could be that run's,
	// and it would bind here. Name the contested sentinel instead, so records
	// written while the accounts overlap are refused rather than published as
	// the wrong account's utilization.
	//
	// Taken UNDER the append lock, never before it. Deciding outside left an
	// ordering in which A reads "no conflict", B arms and appends the contested
	// marker, and A then appends a plain A marker LAST — leaving the log falsely
	// naming A while both accounts are live. Holding the lock across the check
	// and the append makes the two runs' decisions serial, so whoever appends
	// second has already observed the other's arm.
	if grokArmedDirectAccountsDisagreeWith(identity) {
		identity = grokContestedBillingIdentity
	}

	if !grokBillingIdentityIsNewest(base, identity) {
		_ = appendGrokBillingIdentityValue(base, identity)
	}

	// A standing marker proves the NEXT record will be attributable; it says
	// nothing about the records already in the log. When ownership of the
	// reader moves to this account after a merge left a foreign record on top
	// — a `grok login` back to the direct run's account — the marker check
	// passes while every gather here publishes nothing. This is where that is
	// noticed, because it is the only guard that runs again after the merge.
	// A no-op read when the log is healthy.
	restoreGrokBillingRecordForArmedReaderLocked(base, identity)
}

// grokLimitStateFromFrame walks a decoded Grok frame for a usage-limit signal.
// The streaming-json envelope nests the payload under varying keys across
// versions, so we search recursively rather than assume a fixed path. Only the
// gate-specific message / url keys are harvested (not generic `message`/`url`)
// to avoid false positives from unrelated frames.
//
// Severity: a hard hit (`usage_limit_reached`, `credit_limit_hit`,
// `limit_reached`, `allow_access:false`) → reached; a soft upsell
// (`credit_limit_upsell*`, `approaching`, a bare `usage_limit*`) → approaching.
// A reached signal anywhere in the frame wins over an approaching one.
func grokLimitStateFromFrame(raw map[string]interface{}, now time.Time) (grokUsageLimitState, bool) {
	st := grokUsageLimitState{
		ObservedAt:   now.UTC().Format(time.RFC3339),
		ObservedAtMs: now.UnixMilli(),
	}
	found := false
	severity := ""
	setSeverity := func(s string) {
		found = true
		if s == grokLimitReached {
			severity = grokLimitReached
		} else if severity == "" {
			severity = grokLimitApproaching
		}
	}

	var walk func(v interface{})
	walk = func(v interface{}) {
		switch t := v.(type) {
		case map[string]interface{}:
			for k, val := range t {
				lk := strings.ToLower(k)
				switch s := val.(type) {
				case string:
					ls := strings.ToLower(s)
					// `sessionupdate` / `session_update` cover Grok's ACP
					// session-update frames — xAI's docs key the limit
					// signal off `update.sessionUpdate` (e.g.
					// `{"params":{"update":{"sessionUpdate":"usage_limit_reached"}}}`),
					// so without these the walker classifies usage-limit
					// session updates as no-signal even though the prefilter
					// admitted them.
					if lk == "type" || lk == "event" || lk == "kind" || lk == "reason" || lk == "name" ||
						lk == "sessionupdate" || lk == "session_update" {
						switch {
						case strings.Contains(ls, "usage_limit_reached"),
							strings.Contains(ls, "credit_limit_hit"),
							strings.Contains(ls, "limit_reached"):
							setSeverity(grokLimitReached)
						case strings.Contains(ls, "credit_limit_upsell"),
							strings.Contains(ls, "approaching"),
							strings.Contains(ls, "usage_limit"),
							strings.Contains(ls, "credit_limit"):
							setSeverity(grokLimitApproaching)
						}
					}
					if (lk == "gate_message" || lk == "pause_message") && st.Message == "" && s != "" {
						st.Message = s
					}
					if (lk == "gate_url" || lk == "upgrade_url" || lk == "usage_billing_redirect_url") && st.UpgradeURL == "" && s != "" {
						st.UpgradeURL = s
					}
				case bool:
					if lk == "allow_access" && !s {
						setSeverity(grokLimitReached)
					}
				}
				walk(val)
			}
		case []interface{}:
			for _, x := range t {
				walk(x)
			}
		}
	}
	walk(raw)

	if !found {
		return st, false
	}
	st.Severity = severity
	return st, true
}

// writeGrokUsageLimitState read-modify-writes the cache. Latest observation
// wins, EXCEPT a fresh approaching signal does not downgrade a still-live
// reached state for the same account (a reached cap is the stricter, more
// user-relevant state until it ages out via grokLimitNoticeTTL). A changed
// account fingerprint discards any prior state outright.
func writeGrokUsageLimitState(path string, state grokUsageLimitState, fingerprint string) {
	if path == "" || state.Severity == "" {
		return
	}
	state.AccountFingerprint = fingerprint

	grokUsageLimitMu.Lock()
	defer grokUsageLimitMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	lockFile, locked := acquireCrossProcessCacheLock(path)
	if locked {
		defer func() {
			_ = unlockFile(lockFile)
			_ = lockFile.Close()
		}()
	}

	if b, err := os.ReadFile(path); err == nil {
		var prev grokUsageLimitState
		if json.Unmarshal(b, &prev) == nil &&
			prev.AccountFingerprint == fingerprint &&
			prev.Severity == grokLimitReached &&
			state.Severity == grokLimitApproaching &&
			!grokLimitStateExpired(prev, state.ObservedAtMs) {
			// Keep the stricter, still-live reached state; only refresh its
			// observedAt so it keeps re-arming while the session is active.
			prev.ObservedAt = state.ObservedAt
			prev.ObservedAtMs = state.ObservedAtMs
			state = prev
		}
	}

	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), state.ObservedAtMs)
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// grokLimitStateExpired reports whether a captured state is older than the
// notice TTL relative to nowMs (epoch ms). Stale states are ignored by both the
// downgrade guard above and the read path in cliagent_usage_grok.go.
func grokLimitStateExpired(state grokUsageLimitState, nowMs int64) bool {
	if state.ObservedAtMs <= 0 {
		return true
	}
	return nowMs-state.ObservedAtMs > grokLimitNoticeTTL.Milliseconds()
}

// loadGrokUsageLimitState reads the cache and returns the live state for
// `currentFingerprint`, or (zero, false) when the file is absent, pinned to a
// different account, or older than the notice TTL.
func loadGrokUsageLimitState(currentFingerprint string, now time.Time) (grokUsageLimitState, bool) {
	b, err := os.ReadFile(grokUsageLimitCachePath())
	if err != nil {
		return grokUsageLimitState{}, false
	}
	var state grokUsageLimitState
	if json.Unmarshal(b, &state) != nil || state.Severity == "" {
		return grokUsageLimitState{}, false
	}
	if state.AccountFingerprint != currentFingerprint {
		return grokUsageLimitState{}, false
	}
	if grokLimitStateExpired(state, now.UnixMilli()) {
		return grokUsageLimitState{}, false
	}
	return state, true
}

// grokBillingAttributionKeeperInterval bounds how long a DISPLACED marker can
// go unrepaired while a direct Grok session is still running. Session start is
// not enough on its own: the CLI keeps fetching credits for the whole life of
// the session, and any identity appended after ours — a managed session for
// another account merging its paired lines on exit, or a `grok login` plus
// activity outside the agent — becomes the nearest preceding identity for every
// record the live session writes from that moment on. Those records are then
// refused, so a long session silently stops producing the observations this
// whole path exists to capture.
//
// 30s trades a small bounded loss window against writes into a provider-owned
// file. Each tick is one bounded tail read and appends only when our identity
// is no longer the newest, so a session that is never displaced writes nothing
// after its first line.
const grokBillingAttributionKeeperInterval = 30 * time.Second

// grokBillingAttributionKeeperIntervalEnv is the test seam for the tick.
const grokBillingAttributionKeeperIntervalEnv = "AIX_GROK_ATTRIBUTION_KEEPER_INTERVAL"

// grokBillingAttributionKeeperIntervalValue resolves the tick, honoring the
// test seam.
func grokBillingAttributionKeeperIntervalValue() time.Duration {
	if raw := os.Getenv(grokBillingAttributionKeeperIntervalEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return grokBillingAttributionKeeperInterval
}

var (
	grokAttributionKeeperMu sync.Mutex
	// grokAttributionKeeperRefs counts live direct runs; the shared keeper
	// goroutine runs while it is above zero.
	grokAttributionKeeperRefs int
	// grokAttributionKeeperAccounts counts those runs PER captured account, so
	// a re-assertion names the account its runs were started under rather than
	// whatever the shared home resolves to now.
	//
	// KEYED CASE-INSENSITIVELY, because every attribution check around it
	// compares identities with EqualFold. A cached identity that is rewritten
	// in casing only between two same-account runs (`User@example.com` ->
	// `user@example.com`) is ONE account to the reader, so keying it as two
	// would make grokDirectAttributionAssertion see a disagreement that does
	// not exist and name the contested sentinel — leaving both runs''' records
	// unattributable until one exits. The first arm'''s spelling is retained as
	// the display form so a re-assertion still writes an identity the reader
	// recognises verbatim.
	grokAttributionKeeperAccounts = map[string]grokAttributionKeeperAccount{}
	grokAttributionKeeperStop     chan struct{}
)

// grokAttributionKeeperAccount is one case-folded account bucket: how many live
// direct runs were armed under it, and the spelling to assert for them.
type grokAttributionKeeperAccount struct {
	Identity string
	Refs     int
}

// grokIdentityFoldKey folds one identity to the canonical key every
// EqualFold-equal spelling of it shares. Must stay the same fold the
// surrounding EqualFold comparisons use, or a map keyed on it splits an account
// those comparisons treat as one.
//
// Used by the keeper's account buckets AND by the publish gate in
// grokRecordBelongsToCurrentAccount: the gate compares a logged marker against
// the credentials' candidate identities, and the keeper decides whether a
// marker already in the log may stand. Two different folds there let the keeper
// accept an existing marker while the gate refuses the record beneath it, which
// is an account whose billing is permanently unpublishable.
//
// strings.ToLower is NOT that fold. strings.EqualFold compares rune by rune
// under Unicode simple folding, whose orbits can hold several lowercase runes:
// capital sigma, small sigma and FINAL small sigma are one orbit, so `Σ` and
// `ς` are EqualFold-equal while lowercasing them yields `σ` and `ς` — two keys
// for one account, which makes the keeper see a disagreement that does not
// exist and install the contested sentinel, discarding both runs' billing
// observations. Mapping each rune to the SMALLEST rune in its simple-fold
// orbit reproduces EqualFold's equivalence exactly, because that orbit is the
// relation EqualFold itself walks.
func grokIdentityFoldKey(identity string) string {
	return strings.Map(grokFoldRune, strings.TrimSpace(identity))
}

// grokFoldRune returns the canonical representative of r's Unicode simple-fold
// orbit: the smallest rune reachable by walking unicode.SimpleFold from r back
// around to itself. Two runes share a representative exactly when
// strings.EqualFold considers them equal.
func grokFoldRune(r rune) rune {
	min := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < min {
			min = f
		}
	}
	return min
}

// startGrokBillingAttributionKeeper holds attribution for the LIFETIME of a
// direct Grok run rather than only at its start, and returns the release the
// caller must invoke when the child is reaped.
//
// One shared goroutine serves every concurrently live direct session (they all
// share one provider-owned log, so per-session pollers would only duplicate the
// same read). Mirrors startAntigravityQuotaCapture's run-scoped, ref-counted
// arm/release shape.
func startGrokBillingAttributionKeeper(identity string) (finish func()) {
	// Idempotent: StartSession normalizes BEFORE arming so its pre-spawn marker
	// carries the same value, and re-applying here keeps every other caller —
	// and the invariant that an unresolved arm is never invisible to
	// disagreement detection — independent of that.
	identity = grokArmableBillingIdentity(identity)
	grokAttributionKeeperMu.Lock()
	grokAttributionKeeperRefs++
	if identity != "" {
		key := grokIdentityFoldKey(identity)
		entry := grokAttributionKeeperAccounts[key]
		if entry.Refs == 0 {
			entry.Identity = identity
		}
		entry.Refs++
		grokAttributionKeeperAccounts[key] = entry
	}
	if grokAttributionKeeperRefs == 1 {
		grokAttributionKeeperStop = make(chan struct{})
		go runGrokBillingAttributionKeeper(grokAttributionKeeperStop)
	}
	grokAttributionKeeperMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			grokAttributionKeeperMu.Lock()
			var stop chan struct{}
			grokAttributionKeeperRefs--
			if identity != "" {
				key := grokIdentityFoldKey(identity)
				if entry, ok := grokAttributionKeeperAccounts[key]; !ok || entry.Refs <= 1 {
					delete(grokAttributionKeeperAccounts, key)
				} else {
					entry.Refs--
					grokAttributionKeeperAccounts[key] = entry
				}
			}
			if grokAttributionKeeperRefs <= 0 {
				grokAttributionKeeperRefs = 0
				stop, grokAttributionKeeperStop = grokAttributionKeeperStop, nil
			}
			grokAttributionKeeperMu.Unlock()
			if stop != nil {
				close(stop)
				// The LAST instrumented run is gone, so our marker must stop
				// vouching for whatever is written next. See
				// sealGrokDirectAttribution.
				sealGrokDirectAttribution()
				return
			}
			// Runs remain, and this release may have RESOLVED a disagreement:
			// the log still carries the contested sentinel that the departing
			// account forced, so every record the survivor writes from here on
			// binds to a name no account matches and is refused. Waiting for the
			// next keeper tick makes that a known 30s hole — and a failed second
			// spawn opens it immediately after its pre-spawn contested marker.
			// Re-assert now; the guard appends nothing when the newest marker
			// already names the surviving account, so the common
			// same-account/one-run release stays one bounded tail read.
			//
			// Deliberately AFTER the unlock above: the re-assertion re-takes
			// this mutex (under the append lock), so calling it while held
			// would deadlock.
			reassertGrokDirectAttribution()
		})
	}
}

// grokArmedDirectAccountsDisagreeWith reports whether any live direct run was
// started under an account other than `identity`. It is what turns an honest
// marker into a contested one: the writer knows who it wants to name, and this
// answers whether anyone else's records could bind to that name.
func grokArmedDirectAccountsDisagreeWith(identity string) bool {
	identity = strings.TrimSpace(identity)
	if identity == "" || strings.EqualFold(identity, grokContestedBillingIdentity) {
		return false
	}
	grokAttributionKeeperMu.Lock()
	defer grokAttributionKeeperMu.Unlock()
	key := grokIdentityFoldKey(identity)
	for armed := range grokAttributionKeeperAccounts {
		if armed != key {
			return true
		}
	}
	return false
}

// grokDirectAttributionAssertion returns the identity that should stand as the
// newest marker for the live direct runs: the single account they were all
// started under, or grokContestedBillingIdentity when they disagree.
//
// It exists so a displacement WE cause — persistGrokManagedBillingSnapshot
// merging another session's paired identity/record lines — can be repaired the
// instant it happens instead of waiting up to one keeper tick. That is the only
// displacer this process can observe synchronously, and it is the one that
// otherwise loses every record of a short direct run that starts, is displaced,
// writes its billing line and exits inside a single interval. The periodic tick
// still covers the out-of-process case (`grok login` outside the agent).
//
// Asserting NOTHING on a disagreement is not neutral. Whatever marker is newest
// stays newest — a managed merge's identity, or the first run's account — and
// the reader binds every following record to it under credentials that may now
// match, so one run's usage is published as the other's. The sentinel is the
// only assertion that is true for both.
func grokDirectAttributionAssertion() (string, bool) {
	grokAttributionKeeperMu.Lock()
	defer grokAttributionKeeperMu.Unlock()
	switch len(grokAttributionKeeperAccounts) {
	case 0:
		return "", false
	case 1:
		for _, entry := range grokAttributionKeeperAccounts {
			return entry.Identity, true
		}
	}
	return grokContestedBillingIdentity, true
}

// reassertGrokDirectAttribution re-names the account the live direct runs were
// SPAWNED under — never the currently signed-in one, see
// ensureGrokBillingIdentityNamed.
func reassertGrokDirectAttribution() {
	identity, ok := grokDirectAttributionAssertion()
	if !ok {
		return
	}
	ensureGrokBillingIdentityNamed(grokPersistentHome(), identity)
}

// sealGrokDirectAttribution closes the attribution window the last direct run
// leaves behind: it names the contested sentinel once no instrumented run is
// live, so records written after that point bind to a name no account matches.
//
// Without it our marker keeps vouching for the log indefinitely. A later Grok
// invocation OUTSIDE the agent — a `grok login` to another account, or an
// API-key override while the cached login is still ours — writes an
// identity-less billing record beneath our stale marker, and
// grokRecordBelongsToCurrentAccount then accepts that account's record as ours
// and publishes one subscription's utilization under another. Refusing it costs
// an observation the agent never produced anyway; accepting it is a billing lie.
//
// The completed run keeps its own attribution: a record binds to the nearest
// identity ABOVE it, so a marker appended after that record cannot unbind it.
//
// A run that re-armed between the release and this append is asserted normally
// instead — the whole decision is taken under grokBillingAttributionSerialize,
// the same lock ensureGrokBillingIdentityNamed serializes its own read-then-
// append with, so a concurrent arm either observes this seal or replaces it.
func sealGrokDirectAttribution() {
	base := grokPersistentHome()
	if base == "" {
		return
	}

	grokBillingAttributionSerialize.Lock()
	defer grokBillingAttributionSerialize.Unlock()

	sealGrokBillingAttributionLocked(base)
}

// sealGrokBillingAttributionLocked names the account the live direct runs were
// spawned under — or grokContestedBillingIdentity when none is live — as the
// log's newest marker, so our marker stops vouching for records we did not
// produce. Appends nothing when that name is already newest, which keeps the
// common case one bounded tail read and writes nothing extra into a
// provider-owned file.
//
// Shared with the managed merge (appendGrokBillingPair), which leaves its own
// identity as the newest marker and needs the same seal for the same reason.
// Callers must hold grokBillingAttributionSerialize.
func sealGrokBillingAttributionLocked(base string) {
	if base == "" {
		return
	}
	identity, armed := grokDirectAttributionAssertion()
	if !armed {
		identity = grokContestedBillingIdentity
	}
	if grokBillingIdentityIsNewest(base, identity) {
		return
	}
	_ = appendGrokBillingIdentityValue(base, identity)
}

// runGrokBillingAttributionKeeper re-asserts attribution until the last armed
// session releases. Best-effort and silent, like every other path in this file.
func runGrokBillingAttributionKeeper(stop <-chan struct{}) {
	ticker := time.NewTicker(grokBillingAttributionKeeperIntervalValue())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			reassertGrokDirectAttribution()
		}
	}
}
