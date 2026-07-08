// cliagent_ratelimit_gemini.go — passively captures Gemini CLI's discrete
// quota/rate-limit signal off the streaming/ACP stdout we already scan in
// session.go and acp_core.go, and caches the latest state to disk.
//
// Why this exists (and why it is NOT a percentage cache like Codex's):
//
//	Gemini CLI exposes NO numeric remaining-quota counter on disk or on its
//	output — the free-tier daily request cap is only knowable as a published
//	constant (see cliagent_usage_gemini.go). What the CLI DOES surface is a
//	discrete, server-pushed signal when you hit the cap: a 429 error carrying
//	`RESOURCE_EXHAUSTED` / "Quota exceeded" / `rateLimitExceeded`, and a
//	soft pro→flash fallback notice ("You have reached your daily
//	gemini-*-pro quota limit") when only the pro-model budget is spent. None
//	of these carry a "% remaining", so there is nothing to render as a gauge
//	— only an approaching/reached WARNING. That is what this captures.
//
// One consumer reads the cache this writes:
//  1. cliagent_usage_gemini.go — turns the latest non-stale state into the
//     card-level notice (warning / error banner) shown on the CLI Agents tab,
//     while the capacity bar framing stays tier-based.
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

// Gemini usage-limit severities, mirrored by cliagent_usage_gemini.go onto
// the card-level notice severity ("approaching" → warning, "reached" → error).
const (
	geminiLimitApproaching = "approaching"
	geminiLimitReached     = "reached"
)

// geminiLimitNoticeTTL bounds how long a captured limit state is trusted.
// Gemini's quotas roll over daily and there is no "cleared" frame to observe,
// so a stale "reached" must not stick on the card forever. Same 6h window as
// the Grok analog: long enough to span a session's worth of follow-ups but
// short of a daily reset, so a genuinely active limit keeps re-arming on each
// throttled turn while yesterday's limit decays on its own.
const geminiLimitNoticeTTL = 6 * time.Hour

// geminiUsageLimitState is the on-disk cache (one state, latest-wins).
// AccountFingerprint pins it to the Google account that produced it so a
// stale limit can't bleed onto a different account after a re-login.
type geminiUsageLimitState struct {
	Severity           string `json:"severity"` // geminiLimitApproaching | geminiLimitReached
	Message            string `json:"message,omitempty"`
	ObservedAt         string `json:"observedAt"`
	ObservedAtMs       int64  `json:"observedAtMs"`
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
}

var geminiUsageLimitMu sync.Mutex

// geminiUsageLimitCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_GEMINI_LIMIT_CACHE overrides it (tests isolate from the real
// machine cache; ops can relocate it if the data dir is read-only).
func geminiUsageLimitCachePath() string {
	if p := os.Getenv("AIEXPEDITE_GEMINI_LIMIT_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "gemini_usage_limit.json")
}

// captureGeminiUsageLimitLine parses one stdout line from a Gemini session
// (stream-json or ACP JSON-RPC) and, if it carries a quota / rate-limit
// signal, records it in the on-disk cache. Best-effort; silent on every
// failure.
func captureGeminiUsageLimitLine(line string, now time.Time) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	// Cheap prefilter: only decode lines that could plausibly carry a limit
	// signal. Covers the gRPC status name, the REST error code, the JSON-API
	// reason strings, and the prose forms the CLI's fallback notices use.
	lower := strings.ToLower(trimmed)
	if !strings.Contains(lower, "resource_exhausted") &&
		!strings.Contains(lower, "quota") &&
		!strings.Contains(lower, "ratelimitexceeded") &&
		!strings.Contains(lower, "rate_limit") &&
		!strings.Contains(lower, "rate limit") &&
		!strings.Contains(lower, "429") {
		return
	}
	var raw map[string]interface{}
	if json.Unmarshal([]byte(trimmed), &raw) != nil {
		return
	}
	state, ok := geminiLimitStateFromFrame(raw, now)
	if !ok {
		return
	}
	writeGeminiUsageLimitState(geminiUsageLimitCachePath(), state, currentGeminiAccountFingerprint())
}

// geminiLimitStateFromFrame walks a decoded Gemini frame for a quota /
// rate-limit signal. The stream-json envelope and the ACP JSON-RPC error
// shape nest the payload under varying keys, so we search recursively rather
// than assume a fixed path. Only limit-bearing message strings are harvested
// (not every generic `message`) to avoid false positives from unrelated
// frames.
//
// Severity: a hard hit (`RESOURCE_EXHAUSTED` status, HTTP/gRPC code 429,
// `rateLimitExceeded`, "quota … exceeded"/"quota limit") → reached; a soft
// signal ("approaching", the pro→flash fallback "slow response times"
// upsell) → approaching. A reached signal anywhere in the frame wins over an
// approaching one.
func geminiLimitStateFromFrame(raw map[string]interface{}, now time.Time) (geminiUsageLimitState, bool) {
	st := geminiUsageLimitState{
		ObservedAt:   now.UTC().Format(time.RFC3339),
		ObservedAtMs: now.UnixMilli(),
	}
	found := false
	severity := ""
	setSeverity := func(s string) {
		found = true
		if s == geminiLimitReached {
			severity = geminiLimitReached
		} else if severity == "" {
			severity = geminiLimitApproaching
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
					// `status` carries the gRPC status name in the REST error
					// body; `reason` carries the JSON-API `rateLimitExceeded`;
					// `type`/`event`/`kind`/`name` and the ACP
					// `sessionupdate`/`session_update` keys cover the
					// stream-json and session-update envelopes.
					if lk == "type" || lk == "event" || lk == "kind" || lk == "reason" || lk == "name" ||
						lk == "status" || lk == "sessionupdate" || lk == "session_update" {
						switch {
						case strings.Contains(ls, "resource_exhausted"),
							strings.Contains(ls, "ratelimitexceeded"),
							strings.Contains(ls, "quota_exceeded"),
							strings.Contains(ls, "rate_limit_exceeded"):
							setSeverity(geminiLimitReached)
						case strings.Contains(ls, "quota"),
							strings.Contains(ls, "rate_limit"),
							strings.Contains(ls, "approaching"):
							setSeverity(geminiLimitApproaching)
						}
					}
					// Prose forms: the CLI's error text and the pro→flash
					// fallback notice both ride in `message` / `error` /
					// `content` strings. Only classify strings that
					// unambiguously talk about a quota/rate limit.
					if lk == "message" || lk == "error" || lk == "content" || lk == "text" {
						switch {
						case strings.Contains(ls, "quota exceeded"),
							strings.Contains(ls, "quota limit"),
							strings.Contains(ls, "resource_exhausted"),
							strings.Contains(ls, "rate limit exceeded"):
							setSeverity(geminiLimitReached)
							if st.Message == "" {
								st.Message = s
							}
						case strings.Contains(ls, "quota") && strings.Contains(ls, "approaching"):
							setSeverity(geminiLimitApproaching)
							if st.Message == "" {
								st.Message = s
							}
						}
					}
				case float64:
					// REST error bodies carry `code: 429`; gRPC transcodes
					// RESOURCE_EXHAUSTED as 8. Only 429 is unambiguous enough
					// to classify on a bare number.
					if (lk == "code" || lk == "status") && s == 429 {
						setSeverity(geminiLimitReached)
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

// writeGeminiUsageLimitState read-modify-writes the cache. Latest observation
// wins, EXCEPT a fresh approaching signal does not downgrade a still-live
// reached state for the same account (a reached cap is the stricter, more
// user-relevant state until it ages out via geminiLimitNoticeTTL). A changed
// account fingerprint discards any prior state outright.
func writeGeminiUsageLimitState(path string, state geminiUsageLimitState, fingerprint string) {
	if path == "" || state.Severity == "" {
		return
	}
	state.AccountFingerprint = fingerprint

	geminiUsageLimitMu.Lock()
	defer geminiUsageLimitMu.Unlock()

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
		var prev geminiUsageLimitState
		if json.Unmarshal(b, &prev) == nil &&
			prev.AccountFingerprint == fingerprint &&
			prev.Severity == geminiLimitReached &&
			state.Severity == geminiLimitApproaching &&
			!geminiLimitStateExpired(prev, state.ObservedAtMs) {
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

// geminiLimitStateExpired reports whether a captured state is older than the
// notice TTL relative to nowMs (epoch ms). Stale states are ignored by both
// the downgrade guard above and the read path in cliagent_usage_gemini.go.
func geminiLimitStateExpired(state geminiUsageLimitState, nowMs int64) bool {
	if state.ObservedAtMs <= 0 {
		return true
	}
	return nowMs-state.ObservedAtMs > geminiLimitNoticeTTL.Milliseconds()
}

// loadGeminiUsageLimitState reads the cache and returns the live state for
// `currentFingerprint`, or (zero, false) when the file is absent, pinned to a
// different account, or older than the notice TTL.
func loadGeminiUsageLimitState(currentFingerprint string, now time.Time) (geminiUsageLimitState, bool) {
	b, err := os.ReadFile(geminiUsageLimitCachePath())
	if err != nil {
		return geminiUsageLimitState{}, false
	}
	var state geminiUsageLimitState
	if json.Unmarshal(b, &state) != nil || state.Severity == "" {
		return geminiUsageLimitState{}, false
	}
	if state.AccountFingerprint != currentFingerprint {
		return geminiUsageLimitState{}, false
	}
	if geminiLimitStateExpired(state, now.UnixMilli()) {
		return geminiUsageLimitState{}, false
	}
	return state, true
}

// currentGeminiAccountFingerprint reads the Gemini auth on disk and returns
// the same fingerprint geminiUsageParser.Parse attaches to a usage snapshot.
// Used by the usage-limit capture path to scope its cache to the active
// account. Returns "" when no auth is readable (best-effort, matches the
// grok/codex analogs).
func currentGeminiAccountFingerprint() string {
	home, _ := os.UserHomeDir()
	base := expandHome(home, ".gemini")
	if base == "" {
		return ""
	}
	account, _ := readGeminiAccountAndPlan(base)
	return fingerprintAccount("geminiCli", account)
}
