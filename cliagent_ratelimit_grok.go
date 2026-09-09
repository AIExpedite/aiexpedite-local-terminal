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

// captureGrokUsageLimitLine parses one stdout line from a Grok streaming-json
// session and, if it carries a usage-limit / credit-limit / access-gate signal,
// records it in the on-disk cache. Best-effort; silent on every failure.
func captureGrokUsageLimitLine(line string, now time.Time) {
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
	writeGrokUsageLimitState(grokUsageLimitCachePath(), state, currentGrokAccountFingerprint())
}

// grokBillingAttributionRecheck bounds how often ensureGrokBillingAttribution
// re-verifies that our producer identity is still present in the provider log.
// The check reads the bounded log tail, so it is session-scoped work, not
// per-line work.
//
// Deliberately a SEPARATE constant from grokBillingObservationTTL: they answer
// different questions (how often to re-verify attribution vs. how long an
// observation stays trustworthy), and sharing one value would make a freshness
// tuning change silently retune how often we write into a provider-owned file.
const grokBillingAttributionRecheck = time.Hour

// grokBillingAttributionState re-arms the append guard. A plain sync.Once is
// wrong here: Grok rotates unified.jsonl, which discards our identity line, and
// a once-per-process guard would leave every post-rotation record
// unattributable for the life of a long-running agent.
// Keyed by (home, identity): the guard must not suppress an append into a
// DIFFERENT log just because the same account was already attributed elsewhere.
var grokBillingAttribution struct {
	mu           sync.Mutex
	base         string
	identity     string
	lastVerified time.Time
}

// ensureGrokBillingAttribution names the signed-in account in the provider-owned
// log so the billing records a DIRECT (PTY) Grok run writes afterwards can be
// tied to it. Without a producer identity logged before a record,
// grokRecordBelongsToCurrentAccount refuses it and a direct run can never
// produce an observable metric.
//
// Session-scoped, NOT per stdout line: the rotation check reads the log, and
// doing that per line on the streaming hot path would cost a file read per
// output line. captureGrokUsageLimitLine's per-line contract is untouched, and
// this path infers no telemetry from stdout — it only records who is signed in.
//
// Re-arms on an account change AND when the log no longer carries our identity
// (rotation), so attribution self-heals while staying at most one small append
// per rotation. Best-effort and silent on failure, matching this file's
// hot-path contract.
func ensureGrokBillingAttribution(now time.Time) {
	base := grokPersistentHome()
	if base == "" {
		return
	}
	identity, ok := grokResolvedBillingIdentity(base)
	if !ok {
		return
	}

	grokBillingAttribution.mu.Lock()
	defer grokBillingAttribution.mu.Unlock()

	if grokBillingAttribution.base == base && grokBillingAttribution.identity == identity {
		if now.Sub(grokBillingAttribution.lastVerified) < grokBillingAttributionRecheck {
			return
		}
		if grokBillingIdentityLogged(base, identity) {
			grokBillingAttribution.lastVerified = now
			return
		}
	}
	if err := appendGrokBillingIdentity(base); err != nil {
		return
	}
	grokBillingAttribution.base = base
	grokBillingAttribution.identity = identity
	grokBillingAttribution.lastVerified = now
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
