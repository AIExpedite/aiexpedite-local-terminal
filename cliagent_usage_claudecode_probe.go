// cliagent_usage_claudecode_probe.go — bounded Claude utilization probe.
//
// Why this exists:
//
//	Neither Claude execution path produces a NUMERIC utilization reading after a
//	normal, under-quota run. Agent-driven runs emit only usage-less
//	`rate_limit_event` heartbeats, and mergeClaudeRateLimitCache deliberately
//	refuses to let a heartbeat advance a carried reading's ObservedAtMs (see the
//	carry-forward branch there — repeated heartbeats must not make stale usage
//	look fresh). The status-line hook DOES carry numbers, but it only fires on
//	interactive TUI renders. So a device that only ever runs headless Claude
//	sessions reports the same `latestObservedAt` forever, even though every run
//	succeeds and the backend polls for it.
//
//	This probe closes that gap by reading real percentages from Anthropic's OAuth
//	usage endpoint using the credential Claude Code already stored, then merging
//	them into the SAME cache the other two writers use. The freshness guard in
//	mergeClaudeRateLimitCache is untouched: freshness is fixed by adding a real
//	reading, not by loosening the guard.
//
// Boundaries:
//
//   - Single-flight per process, with a hard minimum interval between attempts
//     (claudeUsageProbeMinInterval). A user-initiated __cli_usage_refresh__ may
//     bypass the interval via SetClaudeUsageForceProbe, never the single-flight.
//   - 3s timeout, no proxy inheritance, HTTPS-only (a loopback override is
//     accepted so tests can point at an httptest server), 32 KB body cap.
//   - Decoded into a typed, allow-listed struct — never map[string]interface{} —
//     so unknown vendor fields (tokens, raw config, prose) are discarded by
//     encoding/json rather than carried into the cache or the signed receipt.
//   - Skipped entirely when the user opted out (disable_claude_usage_probe), when
//     an env credential outranks the stored /login (claudeEnvAuthActive — that
//     account has no card here), or when no stored access token exists.
//   - On any failure the cache is left byte-identical, preserving
//     terminal-service's payload-hash delta-skip.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// claudeUsageProbeEndpoint is the OAuth usage endpoint Claude Code's own
	// /usage panel reads. AIEXPEDITE_CLAUDE_USAGE_PROBE_URL overrides it, the
	// same env-pinning convention AIEXPEDITE_CLAUDE_RL_CACHE uses for the cache
	// (tests isolate from the real endpoint; ops can point at a proxy).
	claudeUsageProbeEndpoint = "https://api.anthropic.com/api/oauth/usage"
	// claudeUsageProbeEndpointEnv / claudeUsageProbeMinIntervalEnv are the test +
	// ops overrides for the two values worth pinning.
	claudeUsageProbeEndpointEnv    = "AIEXPEDITE_CLAUDE_USAGE_PROBE_URL"
	claudeUsageProbeMinIntervalEnv = "AIEXPEDITE_CLAUDE_USAGE_PROBE_MIN_INTERVAL_MS"

	// claudeUsageProbeTimeout keeps the whole probe well inside the 10s gather
	// budget shared with every other provider (antigravity's loopback probe uses
	// 2s; this one crosses the internet, so it gets one more second).
	claudeUsageProbeTimeout = 3 * time.Second
	// claudeUsageProbeMaxBody bounds the decode. The real payload is ~1 KB.
	claudeUsageProbeMaxBody = 32 * 1024
	// claudeUsageProbeMinInterval is the floor between two attempts on this
	// device. Worst case is therefore ~60 requests/hour per device — a per-device
	// call, not a fan-out, so it scales with active devices rather than runs.
	claudeUsageProbeMinInterval = 60 * time.Second
	// claudeUsageProbeMaxInterval caps the consecutive-failure backoff, so a
	// persistently broken endpoint settles at ~2 requests/hour/device instead of
	// retrying at the floor forever.
	claudeUsageProbeMaxInterval = 30 * time.Minute
	// claudeUsageProbeMaxFailureStreak bounds the counter so it cannot grow
	// without limit; the backoff has already saturated well before this.
	claudeUsageProbeMaxFailureStreak = 16
	// claudeUsageProbeStaleAfter is how old the freshest cached reading must be
	// before a ROUTINE gather (as opposed to a run-completion or a user-initiated
	// refresh) is allowed to spend a probe on it.
	claudeUsageProbeStaleAfter = 10 * time.Minute
)

// claudeUsageProbeWindow is the ONLY shape decoded from the response. Every
// field is a metric; anything else the server sends is dropped by
// encoding/json. Pointers so an absent value is distinguishable from a real 0 —
// decoded into a plain float64 an omitted `utilization` would silently persist
// as "0% consumed" and overwrite a good reading.
type claudeUsageProbeWindow struct {
	Utilization    *float64           `json:"utilization"`
	UsedPercentage *float64           `json:"used_percentage"`
	ResetsAt       claudeUsageProbeTs `json:"resets_at"`
	Status         string             `json:"status"`
}

// claudeUsageProbeResponse enumerates the windows we accept, by the exact ids
// the cache and claudeFableWindowIDs already know. A window id we do not model
// here is not persisted — a new upstream meter must be added deliberately
// rather than landing on the card as an unlabelled row.
type claudeUsageProbeResponse struct {
	FiveHour                *claudeUsageProbeWindow `json:"five_hour"`
	SevenDay                *claudeUsageProbeWindow `json:"seven_day"`
	SevenDaySonnet          *claudeUsageProbeWindow `json:"seven_day_sonnet"`
	SevenDayOpus            *claudeUsageProbeWindow `json:"seven_day_opus"`
	SevenDayFable           *claudeUsageProbeWindow `json:"seven_day_fable"`
	SevenDayOverageIncluded *claudeUsageProbeWindow `json:"seven_day_overage_included"`
}

// claudeUsageProbeTs accepts a reset stamp as either a number (epoch seconds or
// milliseconds, normalized by normalizeResetMs exactly as the stream capture
// does) or an RFC3339 string. Both shapes are observed across Claude surfaces,
// and a strict int64 would silently drop the reset time — leaving the card with
// a fresh percentage under no reset date.
type claudeUsageProbeTs struct {
	Ms int64
}

func (t *claudeUsageProbeTs) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			return nil
		}
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			// A stamp we cannot read is not a failure of the whole probe — the
			// percentage is still a real observation. Leave Ms zero; the merge
			// treats an unknown reset the same way a stream event without one is
			// treated.
			return nil
		}
		t.Ms = parsed.UTC().UnixMilli()
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		// Same tolerance as the unparseable-string case: a reset stamp we cannot
		// read must not discard the percentage alongside it, which is the actual
		// reading this probe exists to obtain.
		return nil
	}
	t.Ms = normalizeResetMs(f)
	return nil
}

// claudeUsageProbeGate holds the single-flight latch, the last-attempt stamp,
// the one-shot force flag, and the arm state. Package-level because the bound is
// per PROCESS: two concurrent runs finishing at once must issue one request.
//
// `armed` is false until SetClaudeUsageProbeDisabled has been called, so the
// probe is opt-IN per process rather than opt-out. StartAgent arms it from the
// config; every other entry point (the statusline-hook subcommand, uninstall,
// one-shot CLI verbs) therefore never makes an outbound call, which is both the
// behaviour we want and what keeps the test suite off the network.
type claudeUsageProbeGate struct {
	mu          sync.Mutex
	inFlight    bool
	lastAttempt time.Time
	force       bool
	armed       bool
	// failures counts CONSECUTIVE failed probes and drives the backoff in
	// interval(). Cleared by any success or early skip.
	failures int
}

var claudeUsageProbe claudeUsageProbeGate

// SetClaudeUsageForceProbe makes the next probe bypass the minimum interval.
// Called by the __cli_usage_refresh__ handler alongside
// SetOpenCodeReadinessForceProbe: a user who just pressed Refresh must not be
// served a reading held back by a throttle they cannot see. Single-flight still
// applies — a forced probe joins an in-flight one rather than duplicating it.
func SetClaudeUsageForceProbe(force bool) {
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.force = force
	claudeUsageProbe.mu.Unlock()
}

// SetClaudeUsageProbeDisabled applies the `disable_claude_usage_probe` opt-out
// AND arms the probe for this process. Called from StartAgent next to the
// status-line hook opt-out it mirrors — see claudeUsageProbeGate for why arming
// is explicit.
func SetClaudeUsageProbeDisabled(disabled bool) {
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.armed = !disabled
	claudeUsageProbe.mu.Unlock()
}

// resetClaudeUsageProbeGate clears the throttle/latch. Test-only seam, mirroring
// resetOpenCodeReadinessCache.
func resetClaudeUsageProbeGate() {
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.inFlight = false
	claudeUsageProbe.lastAttempt = time.Time{}
	claudeUsageProbe.force = false
	claudeUsageProbe.armed = false
	claudeUsageProbe.failures = 0
	claudeUsageProbe.mu.Unlock()
	claudeUsageProbeLog.mu.Lock()
	claudeUsageProbeLog.category = ""
	claudeUsageProbeLog.at = time.Time{}
	claudeUsageProbeLog.mu.Unlock()
}

// claudeUsageProbeMinIntervalValue is the effective throttle floor, honoring the
// env override. A malformed or negative value falls back to the constant rather
// than disabling the throttle.
func claudeUsageProbeMinIntervalValue() time.Duration {
	if raw := os.Getenv(claudeUsageProbeMinIntervalEnv); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return claudeUsageProbeMinInterval
}

// begin reserves the single-flight slot, returning false when another probe is
// running or the minimum interval has not elapsed. A pending force consumes
// itself here so exactly one probe per refresh bypasses the interval.
func (g *claudeUsageProbeGate) begin(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.armed || g.inFlight {
		return false
	}
	// An explicitly disconnected agent must not make outbound calls; the cached
	// reading keeps its true age until the user reconnects.
	if IsOffline() {
		return false
	}
	if !g.force && !g.lastAttempt.IsZero() && now.Sub(g.lastAttempt) < g.interval() {
		return false
	}
	g.force = false
	g.inFlight = true
	g.lastAttempt = now
	return true
}

// interval is the effective floor between attempts: the minimum, doubled per
// consecutive failure, capped at claudeUsageProbeMaxInterval. Caller holds g.mu.
//
// Why a backoff rather than the flat minimum: a TRANSIENT failure (a minute
// offline, a 500) clears on the next attempt, but a PERSISTENT one does not —
// and the persistent case is reachable. A cache that never receives a reading
// never produces an observation, so the staleness check reports stale on EVERY
// gather; if the endpoint's shape ever drifts away from the allow-list decode,
// that pairing is an un-self-healing ~60 request/hour/device loop for as long as
// the drift lasts. Backing off settles it at ~2/hour while still retrying the
// first failure promptly, and a user-initiated refresh bypasses it entirely.
func (g *claudeUsageProbeGate) interval() time.Duration {
	base := claudeUsageProbeMinIntervalValue()
	backoff := base
	// Doubling starts on the SECOND consecutive failure, so a single transient
	// blip — one 500, a moment of packet loss — is retried at the flat minimum.
	// Only a failure that repeats is treated as persistent.
	for i := 1; i < g.failures && backoff < claudeUsageProbeMaxInterval; i++ {
		backoff *= 2
	}
	if backoff > claudeUsageProbeMaxInterval {
		backoff = claudeUsageProbeMaxInterval
	}
	if backoff < base {
		// A base pinned above the cap (or zeroed in tests) must never yield an
		// interval SHORTER than the one the operator asked for.
		backoff = base
	}
	return backoff
}

// finish releases the single-flight slot and records the outcome. A probe that
// refreshed the cache — or that skipped before issuing a request — clears the
// failure streak; only a real failure extends the backoff.
func (g *claudeUsageProbeGate) finish(probeErr *cliAgentUsageError) {
	g.mu.Lock()
	g.inFlight = false
	switch {
	case probeErr == nil:
		g.failures = 0
	case g.failures < claudeUsageProbeMaxFailureStreak:
		g.failures++
	}
	g.mu.Unlock()
}

// forced reports whether a refresh has asked for the next probe to bypass the
// interval, without consuming the flag.
func (g *claudeUsageProbeGate) forced() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.force
}

// armedForProbe reports whether a probe could run at all in this process. Used
// to skip the staleness read on a gather that could never probe anyway — that
// check costs a cache load per provider scan.
func (g *claudeUsageProbeGate) armedForProbe() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.armed
}

// claudeUsageProbeAccessToken reads the stored subscription access token from
// disk (or the macOS Keychain), or "" when there is none.
//
// Called only by the post-run path, which has no credential in hand. The gather
// path passes the token it already decoded instead — on macOS this read shells
// out to `security` under a 3s timeout, and a second spawn inside the shared 10s
// gather budget could starve the providers ordered after Claude.
//
// The token is returned to a LOCAL only — never written to the cache, never
// included in a log or error string.
func claudeUsageProbeAccessToken() string {
	home, _ := os.UserHomeDir()
	base := claudeConfigDir(home)
	if base == "" {
		return ""
	}
	raw, ok := readClaudeCredentialsRaw(base)
	if !ok {
		return ""
	}
	creds := claudeOAuthCredentials{}
	if json.Unmarshal(raw, &creds) != nil {
		return ""
	}
	return creds.ClaudeAiOauth.AccessToken
}

// claudeUsageProbeClient is the bounded HTTP client. Proxy inheritance is off
// (an ambient HTTP(S)_PROXY would route the bearer token through a host the
// user did not choose for this call) and the dialer carries the same deadline
// as the overall timeout.
func claudeUsageProbeClient() *http.Client {
	return &http.Client{
		Timeout: claudeUsageProbeTimeout,
		// Refuse redirects outright. This is a single fixed endpoint, so a 3xx is
		// not an expected response — and following one would re-issue a request
		// carrying the subscription bearer token to a location the pinned-URL
		// check never vetted. Go strips Authorization across domains, but
		// "mostly safe" is the wrong bar for a credential; surfacing the 3xx as a
		// non-2xx failure loses nothing real.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: claudeUsageProbeTimeout}).DialContext,
		},
	}
}

// claudeUsageProbeURL resolves the endpoint the probe may call.
//
// The override is deliberately restricted to LOOPBACK. It exists so tests can
// point at an httptest server; it is not a redirect knob. This request carries
// the user's subscription bearer token, so an env var that could aim it at an
// arbitrary host would be a credential-exfiltration primitive for anything able
// to influence the agent's environment — a much sharper edge than the path-only
// AIEXPEDITE_CLAUDE_RL_CACHE override this convention otherwise follows.
//
// A non-loopback or unparseable override returns "" (probe skipped) rather than
// silently falling back to the real endpoint: an override that was set and then
// ignored should fail visibly, not send the token somewhere the operator did not
// just ask for.
func claudeUsageProbeURL() string {
	override := strings.TrimSpace(os.Getenv(claudeUsageProbeEndpointEnv))
	if override == "" {
		return claudeUsageProbeEndpoint
	}
	parsed, err := url.Parse(override)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return override
	}
	if strings.EqualFold(host, "localhost") {
		return override
	}
	return ""
}

// runClaudeUsageProbe performs at most one bounded request and merges any
// windows it read into the shared rate-limit cache with ObservedAtMs = now.
//
// Returns whether the cache was refreshed, plus a redacted failure record. The
// record carries only a category — the message field is local-only diagnostic
// input everywhere else in this file's neighbourhood, and here it is
// deliberately never populated from the response body or the credential.
func runClaudeUsageProbe(ctx context.Context, now time.Time) (bool, *cliAgentUsageError) {
	return probeClaudeUsage(ctx, now, claudeUsageProbeAccessToken)
}

// probeClaudeUsage is runClaudeUsageProbe with the credential supplied by the
// caller. resolveToken is invoked ONLY once the gate has admitted the probe, so
// a throttled or opted-out call never touches the credential store at all.
func probeClaudeUsage(ctx context.Context, now time.Time, resolveToken func() string) (refreshed bool, probeErr *cliAgentUsageError) {
	// An already-cancelled gather must not burn the throttle slot on a request
	// that cannot complete: the next caller would then be refused for a minute
	// because of a probe that never left the process.
	if ctx.Err() != nil {
		return false, nil
	}
	if !claudeUsageProbe.begin(now) {
		return false, nil
	}
	// Named returns so the deferred finish records whatever this call returned:
	// a failure extends the backoff, a success or an early skip clears it.
	defer func() { claudeUsageProbe.finish(probeErr) }()

	// An env credential outranks the stored /login, and its account has no card
	// here — merging its usage into the stored-login card would misattribute it.
	// Checked in ONE place so both entry points share the rule.
	if claudeEnvAuthActive() {
		return false, nil
	}
	token := resolveToken()
	if token == "" {
		// Not an error: a signed-out device simply has nothing for this probe.
		return false, nil
	}
	endpoint := claudeUsageProbeURL()
	if endpoint == "" {
		return false, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, claudeUsageProbeFailure(cliUsageErrorInternal)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	// The OAuth surface is beta-gated; Claude Code sends the same opt-in.
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := claudeUsageProbeClient()
	// The transport is built per call (matching the antigravity quota probe), so
	// its keep-alive connections have no later owner. Release them explicitly
	// instead of leaving one idle TLS connection per probe until the runtime
	// finalizes the transport.
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		category := cliUsageErrorProviderUnavailable
		if reqCtx.Err() != nil {
			category = cliUsageErrorProviderTimeout
		}
		return false, claudeUsageProbeFailure(category)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, claudeUsageProbeMaxBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, claudeUsageProbeFailure(cliUsageErrorNotAuthenticated)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	// Read at most the cap + 1 byte so an oversized body is DETECTED rather than
	// silently truncated into a JSON parse error — a truncated payload must never
	// be treated as a partial observation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeUsageProbeMaxBody+1))
	if err != nil || len(body) > claudeUsageProbeMaxBody {
		return false, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	var decoded claudeUsageProbeResponse
	if json.Unmarshal(body, &decoded) != nil {
		return false, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}

	updates := claudeUsageProbeBuckets(decoded, now)
	if len(updates) == 0 {
		// A response we cannot plot is not an observation. Returning here leaves
		// the cache byte-identical rather than stamping a fresh ObservedAt on
		// nothing.
		return false, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}
	mergeClaudeRateLimitCacheFromSource(claudeRateLimitCachePath(), updates, now,
		currentClaudeAccountFingerprint(), claudeRateLimitSourceProbe)
	return true, nil
}

// claudeUsageProbeBuckets converts the allow-listed response into cache buckets.
// A window with no readable percentage is DROPPED, not persisted as 0% — the
// same rule bucketFromInfo applies to a stream event that carries no usage.
func claudeUsageProbeBuckets(decoded claudeUsageProbeResponse, now time.Time) map[string]claudeRateLimitBucket {
	nowMs := now.UnixMilli()
	out := map[string]claudeRateLimitBucket{}
	add := func(window string, w *claudeUsageProbeWindow) {
		if w == nil {
			return
		}
		bucket, ok := claudeUsageProbeBucket(*w, nowMs)
		if !ok {
			return
		}
		out[window] = bucket
	}
	add(claudeWindowFiveHour, decoded.FiveHour)
	add(claudeWindowSevenDay, decoded.SevenDay)
	add(claudeWindowSevenDaySonnet, decoded.SevenDaySonnet)
	add(claudeWindowSevenDayOpus, decoded.SevenDayOpus)
	add(claudeWindowSevenDayFable, decoded.SevenDayFable)
	add(claudeWindowSevenDayOverageIncluded, decoded.SevenDayOverageIncluded)
	return out
}

// claudeUsageProbeBucket normalizes one window. Percentage precedence and the
// utilization scale mirror bucketFromInfo so a probe reading and a stream
// reading of the same window can never disagree about what "42" means.
func claudeUsageProbeBucket(w claudeUsageProbeWindow, nowMs int64) (claudeRateLimitBucket, bool) {
	bucket := claudeRateLimitBucket{
		ObservedAtMs: nowMs,
		ResetsAtMs:   w.ResetsAt.Ms,
		Status:       claudeUsageProbeStatus(w.Status),
	}
	switch {
	case w.UsedPercentage != nil:
		bucket.UsedPercentage = clampPercent(*w.UsedPercentage)
		bucket.usageKnown = true
	case w.Utilization != nil:
		// Both scales are in the wild: the SDK's RateLimitInfo reports 0..1 while
		// the /usage surface reports 0..100. They only disagree below 1, and
		// bucketFromInfo already resolves that the same way (utilization 1.0 is
		// "fully consumed"), so keep the two readers identical.
		util := *w.Utilization
		if util > 1 {
			bucket.UsedPercentage = clampPercent(util)
		} else {
			bucket.UsedPercentage = clampPercent(util * 100)
		}
		bucket.usageKnown = true
	case bucket.Status == claudeRateLimitStatusRejected:
		// A rejected window may omit the percentage; it is exhausted by
		// definition. Same synthesis bucketFromInfo performs.
		bucket.UsedPercentage = 100
		bucket.usageKnown = true
	}
	if !bucket.usageKnown {
		return bucket, false
	}
	return bucket, true
}

// claudeUsageProbeStatus normalizes the one free-form string we read into a
// CLOSED set: "", "rejected", or "allowed". The input is never returned, so
// server prose — however long — cannot reach the cache or the signed receipt no
// matter what the endpoint sends. That closed output set is the whole guarantee;
// an input length cap would add nothing (an earlier one here was inert, and
// invited the reader to believe a truncated, possibly rune-split value could be
// persisted).
//
// An ABSENT status stays empty rather than becoming "allowed": bucketFromInfo
// leaves it empty for a stream event that omits it, and inventing a value the
// server never sent would make the same window read differently depending on
// which writer happened to observe it last.
func claudeUsageProbeStatus(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.EqualFold(trimmed, claudeRateLimitStatusRejected) {
		return claudeRateLimitStatusRejected
	}
	return "allowed"
}

// refreshClaudeUsageIfStale is the ROUTINE-gather trigger: it probes only when a
// refresh forced it, or when `latest` — the freshest observation the caller
// already read out of the cache — has aged past claudeUsageProbeStaleAfter.
// A zero `latest` means nothing has ever been observed, which is stale by
// definition.
//
// Takes the observation rather than the fingerprint, and the accessToken the
// caller already decoded rather than re-reading it, so neither the cache nor the
// credential store is touched twice per gather; every remaining gate (armed,
// opt-out, offline, env-auth, single-flight, minimum interval) belongs to the
// probe itself and is not restated here.
//
// Returns whether the cache was refreshed, so the caller knows whether it must
// re-read before shaping the metrics.
func refreshClaudeUsageIfStale(ctx context.Context, now, latest time.Time, accessToken string) bool {
	if !claudeUsageProbe.forced() && !latest.IsZero() && now.Sub(latest) < claudeUsageProbeStaleAfter {
		return false
	}
	refreshed, probeErr := probeClaudeUsage(ctx, now, func() string { return accessToken })
	logClaudeUsageProbeFailure(probeErr)
	return refreshed
}

// triggerClaudeUsageProbeAfterRun fires the probe off the hot path once a Claude
// run has finished. Asynchronous so it can never delay frame ordering,
// waitForExit, or the session_ended publish; bounded by the probe's own timeout
// and collapsed by its single-flight, so a burst of finishing sessions issues
// one request.
func triggerClaudeUsageProbeAfterRun() {
	// Cheap synchronous gate before spawning anything. This fires once per
	// completed turn on every Claude session, and a process that can never probe
	// (unarmed, or the user opted out) should not pay a goroutine for it.
	if !claudeUsageProbe.armedForProbe() {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(context.Background(), claudeUsageProbeTimeout)
		defer cancel()
		_, probeErr := runClaudeUsageProbe(ctx, time.Now())
		logClaudeUsageProbeFailure(probeErr)
	}()
}

// claudeUsageProbeFailure builds the redacted failure record. The category is
// the ONLY thing that escapes: never the response body, the endpoint, or the
// credential. Every failure path in this file goes through here so no future
// branch can start attaching a message.
func claudeUsageProbeFailure(category string) *cliAgentUsageError {
	return &cliAgentUsageError{
		Provider:      claudeCodeUsageParser{}.Provider(),
		ErrorCategory: category,
	}
}

// claudeUsageProbeLog throttles the failure notice. A persistently unreachable
// endpoint (laptop off the network, endpoint 500ing) would otherwise print once
// per minute forever in the tray app's console, drowning the messages an
// operator is actually reading. One line per category per interval keeps the
// signal — the failure stays visible — without the flood.
var claudeUsageProbeLog struct {
	mu       sync.Mutex
	category string
	at       time.Time
}

const claudeUsageProbeLogInterval = 15 * time.Minute

// logClaudeUsageProbeFailure prints the failure CATEGORY only. The response
// body, the endpoint's query, and the credential are never logged — the whole
// point of returning a typed record instead of an error string.
func logClaudeUsageProbeFailure(probeErr *cliAgentUsageError) {
	if probeErr == nil {
		return
	}
	claudeUsageProbeLog.mu.Lock()
	repeat := claudeUsageProbeLog.category == probeErr.ErrorCategory &&
		time.Since(claudeUsageProbeLog.at) < claudeUsageProbeLogInterval
	if !repeat {
		claudeUsageProbeLog.category = probeErr.ErrorCategory
		claudeUsageProbeLog.at = time.Now()
	}
	claudeUsageProbeLog.mu.Unlock()
	if repeat {
		return
	}
	fmt.Printf("%s[claude-usage] utilization probe unavailable (%s) — falling back to stream/status-line capture%s\n",
		colorYellow, probeErr.ErrorCategory, colorReset)
}
