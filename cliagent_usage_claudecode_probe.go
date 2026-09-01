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
//   - Single-flight per process, with a minimum interval between attempts that
//     DOUBLES per consecutive failure up to claudeUsageProbeMaxInterval. A
//     user-initiated __cli_usage_refresh__ bypasses that local timer via
//     SetClaudeUsageForceProbe; nothing bypasses the single-flight, the shared
//     cache check, or a 429 hold.
//   - The endpoint is ACCOUNT-scoped, so a per-process gate cannot bound it on
//     its own: a release and a dev agent, an agent overlapping its own restart,
//     and the status-line hook all consume the same limit while sharing nothing
//     but the on-disk cache. Before spending a request the probe therefore asks
//     that shared cache whether someone already observed inside the current
//     interval (claudeUsageProbeRecentlyObserved), and it honors Retry-After on
//     a 429. Neither is exact — two processes can still race between the check
//     and the write — but together they collapse the steady-state duplication,
//     which is what matters for a limit shared with Claude own pollers.
//   - 3s timeout, no proxy inheritance, redirects refused, and the endpoint is
//     the pinned HTTPS constant — the env override is accepted ONLY for loopback
//     (see claudeUsageProbeURL). 32 KB body cap.
//   - Decoded into a typed, allow-listed struct — never map[string]interface{} —
//     so unknown vendor fields (tokens, raw config, prose) are discarded by
//     encoding/json rather than carried into the cache or the signed receipt.
//   - Skipped entirely when the process never armed it (SetClaudeUsageProbeDisabled
//     is called only by StartAgent), when the user opted out
//     (disable_claude_usage_probe), when the agent is offline, or when no stored
//     access token exists.
//   - Deliberately NOT gated on claudeEnvAuthActive(). That guard belongs to the
//     status-line hook, whose environment IS the Claude session it reports for.
//     This probe runs in the DAEMON, and both launch paths strip CLAUDE_* and
//     ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN before spawning Claude
//     (claudeAlwaysStripped / claudeBillingStripped), so a spawned run always
//     burns the stored subscription login no matter what the daemon inherited.
//     Skipping on the daemon environment would leave a tray agent started from a
//     shell that happens to export ANTHROPIC_API_KEY running Claude against the
//     stored account and never refreshing it — the exact staleness this file
//     exists to fix. The probe reads the STORED credential and asks about the
//     STORED account, so there is no env-account usage to misattribute.
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
	// /usage panel reads, and the ONLY non-loopback host this probe may reach.
	//
	// AIEXPEDITE_CLAUDE_USAGE_PROBE_URL can redirect it to a LOOPBACK address
	// only — it is a test seam, deliberately NOT an ops knob for pointing at a
	// proxy. This request carries the user's subscription bearer token, so an
	// override that accepted arbitrary hosts would be a credential-exfiltration
	// primitive for anything able to influence the agent's environment. Do not
	// relax claudeUsageProbeURL to restore proxy support without replacing that
	// protection with something equivalent.
	claudeUsageProbeEndpoint = "https://api.anthropic.com/api/oauth/usage"
	// claudeUsageProbeEndpointEnv / claudeUsageProbeMinIntervalEnv are the two
	// pinnable values. The endpoint override is loopback-only (see above); the
	// interval override is a plain operator knob.
	claudeUsageProbeEndpointEnv    = "AIEXPEDITE_CLAUDE_USAGE_PROBE_URL"
	claudeUsageProbeMinIntervalEnv = "AIEXPEDITE_CLAUDE_USAGE_PROBE_MIN_INTERVAL_MS"

	// claudeUsageProbeTimeout keeps the whole probe well inside the 10s gather
	// budget shared with every other provider (antigravity's loopback probe uses
	// 2s; this one crosses the internet, so it gets one more second).
	claudeUsageProbeTimeout = 3 * time.Second
	// claudeUsageProbeMaxBody bounds the decode. The real payload is ~1 KB.
	claudeUsageProbeMaxBody = 32 * 1024
	// claudeUsageProbeMinInterval is the floor between two attempts while the
	// probe is HEALTHY, giving ~60 requests/hour as an upper bound. That bound is
	// enforced per PROCESS by the gate and approximately per DEVICE by the shared
	// cache check in claudeUsageProbeRecentlyObserved — a second agent channel on
	// the same machine reads the same cache and stands down rather than doubling
	// the rate. It is a per-account call, not a fan-out, so it scales with active
	// accounts rather than with runs. A repeatedly failing probe settles far
	// below the bound as its interval doubles (see interval).
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
	// claudeUsageProbeMaxRetryAfter caps how long a 429 Retry-After may park the
	// probe. Long enough to be a real reprieve for the service, short enough that
	// a malformed or hostile header cannot disable utilization for days.
	claudeUsageProbeMaxRetryAfter = 2 * time.Hour
	// claudeUsageProbeMaxTrailingWait bounds how long a deferred post-run probe
	// will hold a timer. Beyond this the debt is recorded but not scheduled, and
	// the next routine gather pays it — a 30-minute backoff or a two-hour
	// Retry-After is not a state to park a goroutine through.
	claudeUsageProbeMaxTrailingWait = 5 * time.Minute
	// claudeUsageProbeTrailingSlack pads the wake-up past the eligibility instant
	// so a timer that fires a hair early is not refused and rescheduled.
	claudeUsageProbeTrailingSlack = 50 * time.Millisecond
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

// claudeUsageProbeResponse accepts BOTH shapes this endpoint is known to use,
// because reading only one of them is how this probe silently becomes a no-op.
//
//  1. `limits[]` — the current representation: a list of entries typed
//     `session`, `weekly_all` or `weekly_scoped`, the last carrying the model
//     the scope applies to. On this shape the Fable meter may exist ONLY as a
//     weekly_scoped entry, so a decoder ignoring the list would leave that row
//     permanently unobservable.
//  2. Legacy top-level window objects, kept as a fallback so a rollback (or an
//     account still served the previous shape) keeps working.
//
// Both are allow-lists: an entry we do not model is dropped rather than landing
// on the card as an unlabelled row. When both are present the list wins, since
// it is the shape the service actively maintains; the legacy fields fill only
// windows the list did not supply.
type claudeUsageProbeResponse struct {
	Limits []claudeUsageProbeLimit `json:"limits"`

	FiveHour                *claudeUsageProbeWindow `json:"five_hour"`
	SevenDay                *claudeUsageProbeWindow `json:"seven_day"`
	SevenDaySonnet          *claudeUsageProbeWindow `json:"seven_day_sonnet"`
	SevenDayOpus            *claudeUsageProbeWindow `json:"seven_day_opus"`
	SevenDayFable           *claudeUsageProbeWindow `json:"seven_day_fable"`
	SevenDayOverageIncluded *claudeUsageProbeWindow `json:"seven_day_overage_included"`
}

// claudeUsageProbeLimit is one entry of the `limits[]` representation. Same
// allow-list discipline as claudeUsageProbeWindow: metric fields only, decoded
// into a typed struct so anything else the server sends is discarded.
//
// Percent is the reading under this shape; utilization / used_percentage are
// accepted too because this service has used all three names for the same
// 0..100 number across revisions, and taking whichever is present costs nothing.
//
// `kind` is the discriminator the service actually sends; `type` is kept as an
// alias so an older or rolled-back revision still decodes. The scope is an
// OBJECT (`{"scope":{"model":{"display_name":"Fable"}}}`), which is why it uses
// the tolerant claudeUsageProbeLabel below rather than a plain string: a string
// field facing an object makes encoding/json reject the WHOLE response, so one
// mis-modelled field would take every window down with it — the probe would
// report parse_failed forever and the card would stay exactly as stale as the
// defect this file exists to fix.
type claudeUsageProbeLimit struct {
	Kind           string                `json:"kind"`
	Type           string                `json:"type"`
	Model          claudeUsageProbeLabel `json:"model"`
	Scope          claudeUsageProbeLabel `json:"scope"`
	Percent        *float64              `json:"percent"`
	Utilization    *float64              `json:"utilization"`
	UsedPercentage *float64              `json:"used_percentage"`
	ResetsAt       claudeUsageProbeTs    `json:"resets_at"`
	Status         string                `json:"status"`
}

// claudeUsageProbeLabel flattens a field that names a model to a single string,
// accepting every shape this payload has used: a bare string, an object with a
// display name, or an object nesting the model
// (`{"model":{"display_name":"Fable"}}`).
//
// It NEVER returns an error. A field we cannot interpret must degrade to "no
// label" — dropping one entry — instead of failing the enclosing Unmarshal and
// discarding every window in the response. Guessing wrong about a shape should
// cost one row, not the whole feature.
type claudeUsageProbeLabel struct {
	Label string
}

func (l *claudeUsageProbeLabel) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if json.Unmarshal(b, &str) == nil {
			l.Label = str
		}
		return nil
	}
	if trimmed[0] != '{' {
		return nil
	}
	var obj struct {
		Model       json.RawMessage `json:"model"`
		DisplayName string          `json:"display_name"`
		Name        string          `json:"name"`
		ID          string          `json:"id"`
	}
	if json.Unmarshal(b, &obj) != nil {
		return nil
	}
	nested := ""
	if len(obj.Model) > 0 {
		var inner claudeUsageProbeLabel
		_ = inner.UnmarshalJSON(obj.Model)
		nested = inner.Label
	}
	l.Label = firstNonEmpty(nested, obj.DisplayName, obj.Name, obj.ID)
	return nil
}

// window converts a limits[] entry into the window-shaped value the rest of the
// probe already understands.
func (l claudeUsageProbeLimit) window() claudeUsageProbeWindow {
	return claudeUsageProbeWindow{
		Utilization:    l.Utilization,
		UsedPercentage: firstNonNilFloat(l.Percent, l.UsedPercentage),
		ResetsAt:       l.ResetsAt,
		Status:         l.Status,
	}
}

// cacheWindow maps a limits[] entry onto a cache window id, or "" when the entry
// is not one we model.
//
//	session       -> five_hour
//	weekly_all    -> seven_day
//	weekly_scoped -> seven_day_<model>, for the models the card has rows for
//
// An unrecognized type, or a weekly_scoped entry naming a model we have no row
// for, is dropped: surfacing it would either invent a row or file one model
// usage under a different model meter.
func (l claudeUsageProbeLimit) cacheWindow() string {
	switch strings.ToLower(strings.TrimSpace(firstNonEmpty(l.Kind, l.Type))) {
	case "session":
		return claudeWindowFiveHour
	case "weekly_all":
		return claudeWindowSevenDay
	case "weekly_scoped":
		scope := strings.ToLower(strings.TrimSpace(firstNonEmpty(l.Scope.Label, l.Model.Label)))
		switch {
		case strings.Contains(scope, "opus"):
			return claudeWindowSevenDayOpus
		case strings.Contains(scope, "sonnet"):
			return claudeWindowSevenDaySonnet
		case strings.Contains(scope, "fable"):
			return claudeWindowSevenDayFable
		}
	}
	return ""
}

// firstNonNilFloat returns the first non-nil pointer, or nil.
func firstNonNilFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
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
	// heldUntil is a server-imposed floor from a 429 Retry-After. Unlike the
	// local backoff it is NOT bypassable by a user refresh: the service has told
	// us to stop, and a Refresh button is not a reason to ignore it.
	heldUntil time.Time
	// owedBaseline is the newest run completion for which we still owe an
	// observation, and trailingScheduled marks that a timer is already waiting to
	// pay it. See oweObservation: the throttle must be able to DELAY a post-run
	// probe, never to discard it.
	owedBaseline      time.Time
	trailingScheduled bool
	// doneCh is created by begin() and closed by finish(), so a caller that was
	// refused the single-flight slot can WAIT for the probe already holding it
	// instead of reporting a stale reading. Nil whenever inFlight is false.
	doneCh chan struct{}
	// refreshes counts probes that actually persisted a reading. A joiner
	// compares the value it sampled before waiting against the value after: an
	// advance means the cache it is about to re-read is newer than the one it
	// loaded.
	//
	// lastRefreshAt is the observation instant that advance carries — the reading
	// the CACHE holds for the windows the persisting probe wrote, which is its own
	// stamp only where the merge accepted it. "Something was written" is not
	// enough for a joiner holding a post-run debt: a probe that STARTED before the
	// run persists a PRE-run reading, and settling the debt with it would sign the
	// pre-run timestamp and cancel the trailing probe that would have paid it.
	// Only meaningful when paired with an advance of `refreshes`, which is why the
	// two are read together under one lock.
	refreshes     uint64
	lastRefreshAt time.Time
	// cancelTrailing is closed by resetClaudeUsageProbeGate to abandon any timer
	// that is currently sleeping. A trailing probe resolves the endpoint and the
	// cache path when it WAKES, so one that outlives its test would read whatever
	// environment is live by then — another test's server, or once the env is
	// restored, the real endpoint with a fixture token. Same escape the in-flight
	// drain closes, one timer removed.
	cancelTrailing chan struct{}
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

// claudeUsageProbeDrainTimeout bounds resetClaudeUsageProbeGate's wait for an
// in-flight probe. A var rather than a const so the test that asserts the
// give-up behaviour can pin it small: CI runs this package under `go test -race
// -timeout 5m`, where a hard-coded five-second sleep is pure wall-clock spent
// asserting one boolean.
var claudeUsageProbeDrainTimeout = 5 * time.Second

// resetClaudeUsageProbeGate drains any in-flight probe, cancels any sleeping
// trailing probe, then clears the throttle/latch. Test-only seam, mirroring
// resetOpenCodeReadinessCache.
//
// The drain is load-bearing, not tidiness. triggerClaudeUsageProbeAfterRun runs
// the probe on a goroutine, and probeClaudeUsage resolves the endpoint AFTER
// claiming the gate. A goroutine descheduled between those two points outlives
// its test: cleanup disarms the gate and t.Setenv restores
// AIEXPEDITE_CLAUDE_USAGE_PROBE_URL, and the goroutine then resumes and resolves
// the REAL endpoint — sending the fixture token to api.anthropic.com from `go
// test`. Cleanup order already puts this before the env restore (t.Cleanup is
// LIFO and the t.Setenv calls register first), so waiting here closes the window.
//
// Bounded by claudeUsageProbeDrainTimeout so a wedged probe cannot hang the
// suite; the probe's own 3s timeout means a live one drains far inside it.
func resetClaudeUsageProbeGate() {
	deadline := time.Now().Add(claudeUsageProbeDrainTimeout)
	for time.Now().Before(deadline) {
		claudeUsageProbe.mu.Lock()
		inFlight := claudeUsageProbe.inFlight
		claudeUsageProbe.mu.Unlock()
		if !inFlight {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.inFlight = false
	// Release any joiner the drain gave up on, so it cannot outlive the reset
	// blocked on a channel nothing will ever close.
	if claudeUsageProbe.doneCh != nil {
		close(claudeUsageProbe.doneCh)
		claudeUsageProbe.doneCh = nil
	}
	// refreshes (and the lastRefreshAt it carries) is deliberately NOT reset: it
	// is a monotonic counter compared only against a value the joiner sampled
	// itself, so zeroing it here would make a stale sample look like an advance.
	claudeUsageProbe.lastAttempt = time.Time{}
	claudeUsageProbe.force = false
	claudeUsageProbe.armed = false
	claudeUsageProbe.failures = 0
	claudeUsageProbe.heldUntil = time.Time{}
	claudeUsageProbe.owedBaseline = time.Time{}
	claudeUsageProbe.trailingScheduled = false // release any reservation outright
	// Abandon any sleeping trailing timer before the caller restores the
	// environment it would otherwise wake into.
	if claudeUsageProbe.cancelTrailing != nil {
		close(claudeUsageProbe.cancelTrailing)
	}
	claudeUsageProbe.cancelTrailing = make(chan struct{})
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
	// A server-imposed hold outranks everything below, including force.
	if !g.heldUntil.IsZero() && now.Before(g.heldUntil) {
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
	g.doneCh = make(chan struct{})
	return true
}

// claudeUsageProbeJoinTimeout bounds how long a forced refresh waits for a probe
// that is already in flight. It must cover the WHOLE probe, not just its
// request: a probe that spends its full HTTP timeout and then contends for the
// cache lock persists a reading AFTER the request deadline, and a join that gave
// up at the request deadline would report "nothing written", fall through, be
// refused by the still-held single-flight slot, and sign the pre-probe cache
// milliseconds before the fresh one lands.
//
// Derived from the two bounds the probe itself OBSERVES — the request timeout
// and the ceiling the verified merge enforces on its own persist step — so the
// deadline cannot drift away from them. It matters that the second one is
// enforced rather than estimated: the in-process cache gate has no queue bound,
// so a budget computed as "our wait plus one other writer's" would be exceeded
// by any device running two Claude sessions at once, and the joiner would then
// give up on a probe that was still going to succeed. A var so the tests can pin
// it small.
var claudeUsageProbeJoinTimeout = claudeUsageProbeTimeout + claudeRateLimitVerifiedPersistBudget

// joinInFlight waits for a probe that already holds the single-flight slot,
// reporting whether there was one to join and, when it persisted a reading, the
// instant that reading observes. A zero `observedAt` means the joined probe
// wrote nothing — the caller must issue its own request.
//
// This is what makes a user-initiated refresh honest. The post-run probe fires
// on a goroutine, so pressing Refresh moments after a run lands squarely on an
// in-flight request; begin() refuses the slot, refreshClaudeUsageIfStale would
// report "not refreshed", and ParseContext would sign the receipt from the
// buckets it loaded BEFORE the answer arrived — the card showing the pre-run
// observation while the fresh one lands milliseconds later. Waiting costs at
// most the probe's own deadline and yields the reading the user asked for.
//
// Only the forced path joins: a routine gather that finds a probe in flight has
// nothing to gain by blocking, since its next tick reads the same cache.
func (g *claudeUsageProbeGate) joinInFlight(ctx context.Context) (joined bool, observedAt time.Time) {
	g.mu.Lock()
	done, before := g.doneCh, g.refreshes
	g.mu.Unlock()
	if done == nil {
		return false, time.Time{}
	}
	timer := time.NewTimer(claudeUsageProbeJoinTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
		// The gather is over; whatever the probe writes belongs to the next read.
		return false, time.Time{}
	case <-timer.C:
		// A probe outliving its own deadline is a wedge, not a slow answer.
		return false, time.Time{}
	}
	g.mu.Lock()
	after, at := g.refreshes, g.lastRefreshAt
	g.mu.Unlock()
	if after == before {
		return true, time.Time{}
	}
	return true, at
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
// nextEligible reports when begin() would next admit a probe, and whether it
// ever will. Caller must NOT hold g.mu.
func (g *claudeUsageProbeGate) nextEligible(now time.Time) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.armed {
		return time.Time{}, false
	}
	at := now
	if !g.heldUntil.IsZero() && g.heldUntil.After(at) {
		at = g.heldUntil
	}
	if !g.lastAttempt.IsZero() {
		if ready := g.lastAttempt.Add(g.interval()); ready.After(at) {
			at = ready
		}
	}
	return at, true
}

// recordOwed notes that a run completed at `baseline` and still needs an
// observation newer than it.
//
// Recording is deliberately separate from reserving the timer, and deliberately
// free of I/O. It must happen BEFORE anything that can fail or be refused —
// offline state, a gate race, a network error, a decode error, an unwritable
// cache — because a debt that is never recorded is a run whose refresh is lost,
// and the gather path would then trust a pre-run reading for the whole staleness
// TTL. Coalescing is inherent: the newest baseline subsumes the older ones, so a
// burst of runs leaves exactly one debt.
func (g *claudeUsageProbeGate) recordOwed(baseline time.Time) {
	g.mu.Lock()
	if baseline.After(g.owedBaseline) {
		g.owedBaseline = baseline
	}
	g.mu.Unlock()
}

// reserveTrailing claims the single trailing-timer slot, returning false when
// another caller already holds it. Every successful reservation MUST be paired
// with releaseTrailing on every exit path — a slot reserved and never released
// silently disables trailing probes for the rest of the process.
func (g *claudeUsageProbeGate) reserveTrailing() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.trailingScheduled {
		return false
	}
	g.trailingScheduled = true
	return true
}

// releaseTrailing frees the timer slot.
func (g *claudeUsageProbeGate) releaseTrailing() {
	g.mu.Lock()
	g.trailingScheduled = false
	g.mu.Unlock()
}

// trailingCancelCh returns the channel a sleeping trailing probe should abandon
// on. Lazily created so the zero-value gate is usable.
func (g *claudeUsageProbeGate) trailingCancelCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancelTrailing == nil {
		g.cancelTrailing = make(chan struct{})
	}
	return g.cancelTrailing
}

// settleOwed clears the debt once an observation at or after `baseline` exists.
// Called ONLY after a probe actually persisted a reading — never on a skip, a
// refusal, or a failure, all of which leave the debt outstanding for the next
// gather or run to retry under the existing bounds.
func (g *claudeUsageProbeGate) settleOwed(baseline time.Time) {
	g.mu.Lock()
	if !baseline.Before(g.owedBaseline) {
		g.owedBaseline = time.Time{}
	}
	g.mu.Unlock()
}

// claudeUsageObservationCovers reports whether `observed` is new enough to pay a
// debt recorded at `baseline` — i.e. whether it can have seen the run.
//
// Compared at MILLISECOND resolution because that is the cache's: ObservedAtMs
// truncates, so a reading taken microseconds after a run comes back reading a
// fraction of a millisecond BEFORE it, and a strict comparison would refuse the
// very observation that run earned. A probe fires immediately off the terminal
// `result` frame, so same-millisecond is the NORMAL case on a fast machine, not
// an edge one — refusing it would leave a debt no probe could ever settle.
//
// A zero observation is never a cover: it means the merge left no reading in any
// window this writer touched.
func claudeUsageObservationCovers(observed, baseline time.Time) bool {
	return !observed.IsZero() && !observed.Before(baseline.Truncate(time.Millisecond))
}

// owedObservation returns the outstanding post-run baseline, if any.
func (g *claudeUsageProbeGate) owedObservation() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owedBaseline
}

// holdUntil records a server-imposed floor on the next attempt. Ignored when the
// deadline is zero (no usable Retry-After).
func (g *claudeUsageProbeGate) holdUntil(deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	g.mu.Lock()
	if deadline.After(g.heldUntil) {
		g.heldUntil = deadline
	}
	g.mu.Unlock()
}

// finish releases the single-flight slot. `observedAt` is the observation the
// cache holds for the windows the probe wrote, recorded only alongside a refresh
// so a joiner can tell WHEN the reading it is inheriting was taken, not merely
// that there was one.
func (g *claudeUsageProbeGate) finish(probeErr *cliAgentUsageError, refreshed bool, observedAt time.Time) {
	g.mu.Lock()
	g.inFlight = false
	if refreshed {
		g.refreshes++
		g.lastRefreshAt = observedAt
	}
	switch {
	case probeErr == nil:
		g.failures = 0
	case g.failures < claudeUsageProbeMaxFailureStreak:
		g.failures++
	}
	// Release every joiner AFTER the outcome is recorded, so a waiter that wakes
	// and re-samples refreshes cannot observe the pre-probe count.
	if g.doneCh != nil {
		close(g.doneCh)
		g.doneCh = nil
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

// armedForProbe reports whether a probe could run at all in this process.
// Its one caller is triggerClaudeUsageProbeAfterRun, which fires once per
// completed turn on every Claude session and should not pay for a goroutine
// that begin() would immediately refuse.
func (g *claudeUsageProbeGate) armedForProbe() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.armed
}

// claudeUsageProbeObservedSince reports whether the SHARED cache already holds a
// numeric observation strictly newer than `baseline` — i.e. whether some other
// writer on this machine (a second agent channel, an overlapping restart, the
// status-line hook) has already answered the question this probe is about to ask.
//
// The BASELINE is the whole design, and getting it wrong breaks the feature.
// Suppressing on "any observation younger than the interval" would mean a
// status-line render shortly BEFORE a headless run cancels the probe that run
// needs: the run consumed quota, the reading predates it, and latestObservedAt
// would not advance. So each caller supplies the instant its own answer must be
// newer than:
//
//   - post-run: the moment the run finished. Only an observation recorded AFTER
//     the run can stand in for the probe that run earned.
//   - routine gather: now minus the current interval, the steady-state dedupe
//     that keeps two agent channels from doubling the request rate.
//   - user-initiated refresh: none. Somebody is looking at the card.
//
// A zero baseline therefore means "never suppress". This cannot be exact — two
// processes can still race between the check and the write — but it collapses
// the steady-state duplication on an account-scoped endpoint without ever
// swallowing a probe that a real run made necessary.
func claudeUsageProbeObservedSince(fingerprint string, baseline time.Time) bool {
	if baseline.IsZero() {
		return false
	}
	// Row-aware for the same reason the staleness TTL is: a status-line render
	// answers only five_hour/seven_day, so accepting it as "someone already
	// answered" would let interactive renders suppress the probe the weekly-split
	// and Fable rows depend on. A probe writes every window the endpoint supplies,
	// so the cross-process dedupe this exists for still collapses cleanly.
	latest := claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint))
	return !latest.IsZero() && latest.After(baseline)
}

// claudeSnapshotFreshness is how old the DISPLAYED snapshot is — the stalest of
// the rows the card shows, excluding the rows this process's own probe has shown
// it cannot supply. Every freshness decision in this file goes through it, so
// the TTL check, the cross-process dedupe and the post-run debt cannot disagree
// about what "fresh" means.
func claudeSnapshotFreshness(view claudeRateLimitView) time.Time {
	return stalestClaudeRowObservation(view)
}

// claudeUsageProbeIdentity is everything a probe needs from the stored
// credential: the bearer token, and the fingerprint the cache is scoped by.
//
// They travel TOGETHER because they come from the same file. Deriving the
// fingerprint separately (via currentClaudeAccountFingerprint) costs an extra
// credential read, and on a default macOS config that read shells out to
// `security` under a 3s timeout — inside a 10s budget shared serially by every
// provider, two or three of those around one network call is enough to starve
// the providers ordered after Claude out of the refresh entirely.
type claudeUsageProbeIdentity struct {
	token       string
	fingerprint string
}

// claudeUsageProbeStoredIdentity reads the stored subscription credential ONCE
// and derives both values from it.
//
// No claudeEnvAuthActive() check: see the file header. The daemon environment
// says nothing about which credential a spawned Claude used, because both launch
// paths strip the env credentials before spawning it.
//
// Called only by the post-run path, which has no credential in hand. The gather
// path passes what it already decoded instead.
//
// The token is returned to a LOCAL only — never written to the cache, never
// included in a log or error string.
func claudeUsageProbeStoredIdentity() claudeUsageProbeIdentity {
	home, _ := os.UserHomeDir()
	base := claudeConfigDir(home)
	if base == "" {
		return claudeUsageProbeIdentity{}
	}
	raw, ok := readClaudeCredentialsRaw(base)
	if !ok {
		return claudeUsageProbeIdentity{}
	}
	creds := claudeOAuthCredentials{}
	if json.Unmarshal(raw, &creds) != nil {
		return claudeUsageProbeIdentity{}
	}
	return claudeUsageProbeIdentity{
		token:       creds.ClaudeAiOauth.AccessToken,
		fingerprint: fingerprintAccount(claudeCodeUsageParser{}.Provider(), creds.claudeCredentialAccount()),
	}
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
// runClaudeUsageProbe is the POST-RUN entry point: a Claude run has just
// finished and consumed quota, so only an observation recorded after `now` can
// substitute for this probe.
func runClaudeUsageProbe(ctx context.Context, now time.Time) (bool, *cliAgentUsageError) {
	refreshed, _, probeErr := probeClaudeUsage(ctx, now, claudeUsageProbeStoredIdentity, now)
	return refreshed, probeErr
}

// probeClaudeUsage is runClaudeUsageProbe with the credential supplied by the
// caller. resolveToken is invoked ONLY once the gate has admitted the probe, so
// a throttled or opted-out call never touches the credential store at all.
//
// `observedAt` is the observation the CACHE ends up holding for the windows this
// probe wrote, which is not always the `now` it stamped: `now` is captured by
// the caller before the request, so a status-line render landing while the
// request is in flight legitimately keeps its newer reading and this probe's
// bucket is refused as older. A caller settling a post-run debt must judge that
// instant, not the bare `refreshed` — see refreshClaudeUsageIfStale.
func probeClaudeUsage(
	ctx context.Context,
	now time.Time,
	resolveIdentity func() claudeUsageProbeIdentity,
	dedupeBaseline time.Time,
) (refreshed bool, observedAt time.Time, probeErr *cliAgentUsageError) {
	// An already-cancelled gather must not burn the throttle slot on a request
	// that cannot complete: the next caller would then be refused for a minute
	// because of a probe that never left the process.
	if ctx.Err() != nil {
		return false, time.Time{}, nil
	}
	if !claudeUsageProbe.begin(now) {
		return false, time.Time{}, nil
	}
	// Release the single-flight latch on EVERY path out of here. begin() has
	// already set inFlight, so an early return that skipped this would leave the
	// latch stuck and every future probe in this process rejected — a permanent
	// wedge, not a missed sample. It must therefore be the first statement after
	// begin(), ahead of any other exit. Named returns let it record the outcome:
	// a failure extends the backoff, a success or an early skip clears it.
	defer func() { claudeUsageProbe.finish(probeErr, refreshed, observedAt) }()

	// One credential read for both the bearer token and the cache fingerprint —
	// see claudeUsageProbeIdentity for why they must not be resolved separately.
	identity := resolveIdentity()
	if identity.token == "" {
		// Not an error: a signed-out device simply has nothing for this probe.
		return false, time.Time{}, nil
	}

	// Cross-process coordination on an ACCOUNT-scoped endpoint: has another
	// writer on this machine already answered what this probe would ask?
	if claudeUsageProbeObservedSince(identity.fingerprint, dedupeBaseline) {
		return false, time.Time{}, nil
	}
	endpoint := claudeUsageProbeURL()
	if endpoint == "" {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorInternal)
	}
	req.Header.Set("Authorization", "Bearer "+identity.token)
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
		return false, time.Time{}, claudeUsageProbeFailure(category)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, claudeUsageProbeMaxBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorNotAuthenticated)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Honor the service backpressure rather than only our own timer. This
		// endpoint is account-scoped and shared with every other poller on the
		// account — Claude own /usage panel included — so ignoring Retry-After
		// would keep pressing exactly when we have been asked to stop.
		claudeUsageProbe.holdUntil(retryAfterDeadline(resp.Header.Get("Retry-After"), time.Now()))
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	// Read at most the cap + 1 byte so an oversized body is DETECTED rather than
	// silently truncated into a JSON parse error — a truncated payload must never
	// be treated as a partial observation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeUsageProbeMaxBody+1))
	if err != nil || len(body) > claudeUsageProbeMaxBody {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	var decoded claudeUsageProbeResponse
	if json.Unmarshal(body, &decoded) != nil {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}

	updates := claudeUsageProbeBuckets(decoded, now)
	if len(updates) == 0 {
		// A response we cannot plot is not an observation. Returning here leaves
		// the cache byte-identical rather than stamping a fresh ObservedAt on
		// nothing.
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}
	// Report success only if the snapshot actually reached disk. The
	// fire-and-forget merge swallows an unwritable data dir, a Windows sharing
	// violation, and a failed rename alike — returning true on any of those would
	// tell the caller to re-read a cache that never changed, clear the failure
	// backoff, and throttle the retry, while a SIGNED refresh receipt went out
	// carrying an observation that was never persisted.
	persisted, err := mergeClaudeRateLimitCacheChecked(claudeRateLimitCachePath(), updates, now,
		identity.fingerprint, claudeRateLimitSourceProbe)
	if err != nil {
		return false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorCollectionFailed)
	}
	// `persisted`, not `now`: the merge refuses a bucket whose stamp is older
	// than the reading already standing in that window, so a probe holding the
	// gather's pre-request `now` can succeed having changed nothing a debt-holder
	// cares about. Reporting what the cache HOLDS lets the caller decide whether
	// this covers the run it owes, instead of inferring it from "the write
	// succeeded".
	return true, persisted, nil
}

// claudeUsageProbeBuckets converts the allow-listed response into cache buckets.
// A window with no readable percentage is DROPPED, not persisted as 0% — the
// same rule bucketFromInfo applies to a stream event that carries no usage.
func claudeUsageProbeBuckets(decoded claudeUsageProbeResponse, now time.Time) map[string]claudeRateLimitBucket {
	nowMs := now.UnixMilli()
	out := map[string]claudeRateLimitBucket{}
	add := func(window string, w claudeUsageProbeWindow) {
		bucket, ok := claudeUsageProbeBucket(w, nowMs)
		if !ok {
			return
		}
		out[window] = bucket
	}
	// limits[] first — it is the representation the service actively maintains,
	// so where both shapes describe a window the list is the one to trust.
	for _, limit := range decoded.Limits {
		window := limit.cacheWindow()
		if window == "" {
			continue
		}
		add(window, limit.window())
	}
	// Legacy top-level windows fill only what the list did not supply.
	addLegacy := func(window string, w *claudeUsageProbeWindow) {
		if _, taken := out[window]; taken || w == nil {
			return
		}
		add(window, *w)
	}
	addLegacy(claudeWindowFiveHour, decoded.FiveHour)
	addLegacy(claudeWindowSevenDay, decoded.SevenDay)
	addLegacy(claudeWindowSevenDaySonnet, decoded.SevenDaySonnet)
	addLegacy(claudeWindowSevenDayOpus, decoded.SevenDayOpus)
	addLegacy(claudeWindowSevenDayFable, decoded.SevenDayFable)
	addLegacy(claudeWindowSevenDayOverageIncluded, decoded.SevenDayOverageIncluded)
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
		// 0..100, NOT the SDK stream 0..1 fraction.
		//
		// This deliberately does NOT mirror bucketFromInfo. That reader parses
		// Claude stream-json RateLimitInfo, where `utilization` is a fraction;
		// this reads the OAuth usage endpoint, which reports percentages — the
		// same payload used_percentage / percent fields are plainly 0..100, and a
		// response mixing both conventions in one object would be perverse.
		//
		// Guessing a fraction here is actively harmful rather than merely
		// conservative: a genuine 0.5% reading just after a window reset would be
		// stored as 50%, and 1% as 100%, so the card would report a fresh,
		// nearly-empty quota as half or fully consumed. Matching each source own
		// convention is what keeps both readers correct, not matching each other.
		bucket.UsedPercentage = clampPercent(*w.Utilization)
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
// Takes the observation, and the accessToken AND fingerprint the caller already
// decoded from ONE credential read, so neither the cache nor the credential
// store is touched twice per gather; every remaining gate (armed, opt-out,
// offline, single-flight, minimum interval, 429 hold) belongs to the probe
// itself and is not restated here.
//
// Returns whether the cache was refreshed, so the caller knows whether it must
// re-read before shaping the metrics.
func refreshClaudeUsageIfStale(ctx context.Context, now, latest time.Time, accessToken, fingerprint string) bool {
	forced := claudeUsageProbe.forced()
	// An outstanding post-run debt overrides the staleness TTL: a reading taken
	// BEFORE the run is not "fresh enough" just because it is recent, and this is
	// the backstop for a trailing probe that was too far out to schedule or that
	// failed.
	owed := claudeUsageProbe.owedObservation()
	owing := !owed.IsZero() && (latest.IsZero() || !latest.After(owed))
	if !forced && !owing && !latest.IsZero() && now.Sub(latest) < claudeUsageProbeStaleAfter {
		return false
	}
	// A user-initiated refresh is never deduped against the shared cache —
	// somebody is looking at the card and asked for a reading now. A routine
	// gather is, against the current interval.
	baseline := time.Time{}
	switch {
	case forced:
		// Never deduped — somebody is looking at the card.
	case owing:
		// Must beat the run, not merely the interval.
		baseline = owed
	default:
		claudeUsageProbe.mu.Lock()
		window := claudeUsageProbe.interval()
		claudeUsageProbe.mu.Unlock()
		if window > 0 {
			baseline = now.Add(-window)
		}
	}
	// A forced refresh joins a probe that already holds the single-flight slot
	// rather than being turned away by it — see joinInFlight. Done BEFORE our own
	// attempt so the answer already on the wire is the one the user gets; if it
	// wrote nothing we fall through and issue the request ourselves, the slot now
	// being free and `force` still pending.
	if forced {
		// A joined reading answers the refresh only when it is at least as new as
		// what we owe. A probe that started BEFORE the run persists a PRE-run
		// observation: accepting it would settle the run's debt with a timestamp
		// that predates the run, sign that timestamp into the receipt, and leave
		// the already-scheduled trailing probe to exit finding nothing owed. When
		// it does not cover the debt we fall through and ask ourselves, the slot
		// now free and `force` still pending.
		if joined, joinedAt := claudeUsageProbe.joinInFlight(ctx); joined &&
			!joinedAt.IsZero() && (!owing || claudeUsageObservationCovers(joinedAt, owed)) {
			if owing {
				claudeUsageProbe.settleOwed(owed)
			}
			// Consume the force the joined probe effectively served, so the next
			// routine gather is throttled normally instead of inheriting a bypass
			// nobody asked for.
			SetClaudeUsageForceProbe(false)
			return true
		}
	}
	identity := claudeUsageProbeIdentity{token: accessToken, fingerprint: fingerprint}
	refreshed, observedAt, probeErr := probeClaudeUsage(ctx, now,
		func() claudeUsageProbeIdentity { return identity }, baseline)
	logClaudeUsageProbeFailure(probeErr)
	// Settle only on an observation that actually covers the run — the same test
	// the join above applies, for the same reason. `now` is ParseContext's gather
	// instant, so it can PREDATE the debt (a turn that finished while the gather
	// was assembling), and the merge can also keep a newer incumbent and leave
	// our stamp unwritten; in both cases the write succeeded while the reading
	// the receipt would carry still predates the run. Leaving the debt standing
	// costs one throttled trailing probe, whereas clearing it here would sign the
	// pre-run timestamp AND cancel the probe that would have corrected it.
	if refreshed && owing && claudeUsageObservationCovers(observedAt, owed) {
		claudeUsageProbe.settleOwed(owed)
	}
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
	completedAt := time.Now()
	go func() {
		defer func() { _ = recover() }()
		claudeUsageProbeAfterRun(completedAt)
	}()
}

// claudeUsageProbeAfterRun refreshes utilization for a run that finished at
// `completedAt`, DEFERRING rather than dropping the probe when the gate is not
// ready yet.
//
// Why deferring matters: the minimum interval is checked in begin() before any
// baseline is consulted, so a routine gather (or another run) that probed thirty
// seconds ago would otherwise make this run's refresh vanish — and the gather
// path then treats that pre-run reading as fresh for the whole staleness TTL, so
// latestObservedAt stays behind the run for minutes. The throttle exists to bound
// the request RATE, not to decide which runs get reported; delaying to the end of
// the window satisfies both.
//
// Bounded: the debt is a single coalesced baseline with at most one timer, so a
// burst of runs inside one window costs one trailing probe, not one per run.
func claudeUsageProbeAfterRun(completedAt time.Time) {
	// Record the debt FIRST, before anything that can fail or be refused. Every
	// later step — the eligibility check, the sleep, the request, the cache write
	// — may not happen, and a run whose debt was never recorded is a run whose
	// refresh is silently lost.
	claudeUsageProbe.recordOwed(completedAt)

	// No credential or cache read in this preflight. Resolving the identity here
	// would cost a `security` spawn PER RUN on macOS even when the burst
	// coalesces to one request; the single probe below resolves it once and uses
	// it for both the shared-cache check and the HTTP call.
	eligibleAt, ever := claudeUsageProbe.nextEligible(time.Now())
	if !ever {
		return
	}
	if wait := time.Until(eligibleAt); wait > 0 {
		if wait > claudeUsageProbeMaxTrailingWait {
			// Too far out to hold a timer for — a deep failure backoff or a long
			// Retry-After, both states where we should not be pressing anyway. The
			// debt stays recorded and refreshClaudeUsageIfStale pays it on the next
			// gather. Deliberately does NOT reserve the timer slot: reserving one
			// without creating a timer would leave it held forever.
			return
		}
		if !claudeUsageProbe.reserveTrailing() {
			return // another run already owns the trailing timer
		}
		// Release on EVERY path out, including the cancel and shutdown branches.
		defer claudeUsageProbe.releaseTrailing()

		select {
		case <-time.After(wait + claudeUsageProbeTrailingSlack):
		case <-shutdownChan:
			return
		case <-claudeUsageProbe.trailingCancelCh():
			// The gate was reset under us; the environment this probe would read on
			// waking is no longer the one it was scheduled against.
			return
		}
	}

	// Satisfy the NEWEST outstanding debt, which may have advanced past this
	// run while we slept.
	baseline := claudeUsageProbe.owedObservation()
	if baseline.IsZero() {
		return // already settled by another writer or a gather
	}

	ctx, cancel := context.WithTimeout(context.Background(), claudeUsageProbeTimeout)
	defer cancel()
	refreshed, observedAt, probeErr := probeClaudeUsage(ctx, time.Now(), claudeUsageProbeStoredIdentity, baseline)
	logClaudeUsageProbeFailure(probeErr)
	// Same rule as the gather path: a write that left the run's window owned by
	// an older reading has not paid this debt, even though it succeeded.
	if refreshed && claudeUsageObservationCovers(observedAt, baseline) {
		claudeUsageProbe.settleOwed(baseline)
	}
	// A refusal or failure deliberately leaves the debt standing: the next
	// gather, reconnect, or run retries it under the same bounds.
}

// retryAfterDeadline turns a Retry-After header into an absolute instant, or the
// zero time when the header is absent or unusable.
//
// Both documented forms are accepted (RFC 9110): delta-seconds, and an HTTP
// date. The result is clamped to claudeUsageProbeMaxRetryAfter so a malformed or
// hostile value cannot park the probe for days — a bound, not a rejection, since
// the honest response to "slow down" is to slow down.
func retryAfterDeadline(header string, now time.Time) time.Time {
	value := strings.TrimSpace(header)
	if value == "" {
		return time.Time{}
	}
	var deadline time.Time
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 {
			return time.Time{}
		}
		deadline = now.Add(time.Duration(secs) * time.Second)
	} else if at, err := http.ParseTime(value); err == nil {
		deadline = at
	} else {
		return time.Time{}
	}
	if !deadline.After(now) {
		return time.Time{}
	}
	if max := now.Add(claudeUsageProbeMaxRetryAfter); deadline.After(max) {
		deadline = max
	}
	return deadline
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
