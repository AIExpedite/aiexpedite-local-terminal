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
//     interval (claudeUsageProbeObservedSince), and it honors Retry-After on
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
	"sync/atomic"
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
	// cache check in claudeUsageProbeObservedSince — a second agent channel on
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
	// claudeUsageProbeAfterRunMaxAttempts bounds how many times one finished run
	// re-enters the eligibility wait after the single-flight latch turned it away.
	// Two: the attempt it was scheduled for, plus one retry behind the probe that
	// beat it to the slot. More would let a busy device queue a goroutine per run
	// on a latch they all keep losing; fewer is the bug this bounds — a run whose
	// refresh is dropped because a probe started microseconds earlier.
	claudeUsageProbeAfterRunMaxAttempts = 2
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
// and the arm state. Package-level because the bound is
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
	// admittedOver is the lastAttempt the current admission replaced, kept only
	// so refundAdmission can restore it — see there.
	admittedOver time.Time
	// NOTE: the refresh bypass is deliberately NOT a field here. It belongs to
	// one gather, and a package-level gate cannot express that — see
	// claudeUsageForceProbeTicket, which carries it on the refresh's context.
	armed bool
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
	// owedSeeded latches the ONE read of the persisted debt this process makes
	// for the account named by owedSeededFor (see seedOwedFromCache). Scoped to
	// the fingerprint rather than the process, so a first gather that could not
	// identify the account — a transient Keychain timeout hands it an empty
	// fingerprint, which no scoped snapshot matches — does not consume the only
	// chance the real account's debt and hold have of being adopted. Cleared by
	// resetClaudeUsageProbeGate alongside the fields it already zeroes: a latch
	// that survived the reset would let one test's persisted debt decide whether
	// the next test reads its own cache — the same cross-test leak class the
	// drain and cancelTrailing prevent.
	owedSeeded    bool
	owedSeededFor string
	// owedSeededStampMod / owedSeededStampSize are the claudeRateLimitCacheStamp
	// the latch above was set against, and the latch is believed only while the
	// snapshot file still carries them. Another agent channel writes the same
	// file, so a latch trusted for the whole process lifetime would hide a debt or
	// a 429 hold that channel recorded afterwards — the card left stale for the
	// whole TTL, or a probe admitted inside a window the endpoint already imposed
	// on this device. An unchanged stamp cannot hide a write, so the ordinary case
	// still reads the persisted state exactly once per account.
	owedSeededStampMod  int64
	owedSeededStampSize int64
	// owedSeededObservation is the freshest observation that seed MEASURED for
	// owedSeededFor — the shared cache as it stood after the adoption, including
	// a replay or another process's probe that landed while it read. Retained
	// because the latch above is process-wide while `latest` is per-CALLER: a
	// gather that loaded the pre-replay view and then waited on seedingCh (or one
	// that arrives later in the same process) would otherwise pass the latch,
	// receive "nothing to re-read", and publish the pre-replay utilization the
	// claimant was told to re-read. Every latched caller re-applies the same
	// supersession rules against its OWN view instead, from this one reading —
	// no second cache load.
	owedSeededObservation time.Time
	// seedingCh is non-nil while a seed is between claiming the latch and
	// finishing its adoption, and is closed when it does. A concurrent gather
	// WAITS on it rather than treating "a seed has started" as "the debt and hold
	// are adopted": the claimant does its cache read unlocked, and a peer that
	// ran on in that window could publish the pre-run reading or be admitted
	// inside a persisted Retry-After.
	seedingCh chan struct{}
	// settling counts the post-run settlement calls currently running —
	// claudeUsageProbePayRecordedRun, whether reached synchronously or on the
	// goroutine triggerClaudeUsageProbeAfterRun spawns. The slot
	// trailingScheduled guards is process-wide and NOT keyed to a gate
	// generation, so a settlement that outlives the state it was scheduled
	// against can reserve that slot and make the next caller's debt look
	// already-owned — the refusal path drops the debt outright. Draining this
	// counter in resetClaudeUsageProbeGate is what stops a settlement from one
	// test from silently disabling the trailing probe in the next.
	settling int
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
	//
	// lastRefreshFingerprint is the ACCOUNT that reading belongs to. A joiner
	// cannot use a reading persisted for a different account: the merge scopes
	// every snapshot to its fingerprint and drops the buckets on a transition, so
	// the gather that inherited it would re-read the cache, find nothing for its
	// own account, and sign a receipt with no fresh utilization — having already
	// spent the join that would have probed for it.
	//
	// lastRefreshShared marks an advance that came from the cross-process dedupe:
	// the probe issued nothing because another writer had already persisted a
	// covering reading. That reading is on disk, so a gather that loaded its view
	// earlier must still re-read (refreshLandedSince, seedOwedFromCache) — but it
	// is not this process's answer, so joinInFlight does not hand it to a forced
	// refresh, which is never deduped against the shared cache.
	refreshes              uint64
	lastRefreshAt          time.Time
	lastRefreshFingerprint string
	lastRefreshShared      bool
	// cancelTrailing is closed by resetClaudeUsageProbeGate to abandon any timer
	// that is currently sleeping. A trailing probe resolves the endpoint and the
	// cache path when it WAKES, so one that outlives its test would read whatever
	// environment is live by then — another test's server, or once the env is
	// restored, the real endpoint with a fixture token. Same escape the in-flight
	// drain closes, one timer removed.
	cancelTrailing chan struct{}
}

var claudeUsageProbe claudeUsageProbeGate

// claudeUsageForceProbeTicket is a one-shot bypass of the minimum interval,
// OWNED BY THE GATHER THAT ASKED FOR IT. It rides on the refresh's context
// rather than sitting on the gate, and that ownership is the whole point.
//
// A process-global flag cannot express it. refreshClaudeUsageIfStale sits in the
// COMMON parser path — gatherMachineInfo -> Parse -> ParseContext reaches it on
// every startup and every six-hour machine-info gather, not only from the
// refresh handler. With the bypass in a package variable, a routine gather
// overlapping the window after handleCLIUsageRefreshCommand armed it would claim
// it, and the REQUESTED gather would then see forced == false: on a cache that
// still looks fresh it returns at the TTL check without joining the probe now in
// flight, and signs the receipt from the snapshot it loaded before that reading
// landed — the stale latestObservedAt this file exists to fix, reintroduced by
// the mechanism meant to prevent it.
//
// Carried on the context, the bypass reaches exactly the gather the user asked
// for: a routine parse holds no ticket and can claim nothing, two concurrent
// refreshes hold one each, and nothing is left standing in the process for a
// later gather to inherit. claudeCodeUsageParser.Parse hands ParseContext a
// context.Background(), so the non-context entry point is unforced by
// construction.
type claudeUsageForceProbeTicket struct {
	claimed atomic.Bool
}

// claudeUsageForceProbeKey is an unexported struct{} type, so nothing outside
// this package can collide with it on a context or forge a bypass onto one.
type claudeUsageForceProbeKey struct{}

// WithClaudeUsageForceProbe marks ctx as a user-initiated refresh whose probe
// bypasses the minimum interval. Called by the __cli_usage_refresh__ handler
// alongside SetOpenCodeReadinessForceProbe: a user who just pressed Refresh must
// not be served a reading held back by a throttle they cannot see.
//
// The bypass covers the INTERVAL only. Single flight still applies — a forced
// probe joins an in-flight one rather than duplicating it — and a 429 hold
// outranks it, because the service has told us to stop and a Refresh button is
// not a reason to ignore that.
func WithClaudeUsageForceProbe(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, claudeUsageForceProbeKey{}, &claudeUsageForceProbeTicket{})
}

// claimClaudeUsageForceProbe takes ownership of ctx's bypass: it reports whether
// one was outstanding and consumes it in the same atomic step, so a given
// WithClaudeUsageForceProbe is spent at most once.
//
// Its only caller is refreshClaudeUsageIfStale, the one path a user-initiated
// refresh takes. Once claimed the bypass travels as an explicit argument down
// through probeClaudeUsage to begin(), which never reads it from anywhere else —
// so a concurrent post-run or routine probe can neither consume it nor be
// accelerated by it.
func claimClaudeUsageForceProbe(ctx context.Context) bool {
	ticket := claudeUsageForceProbeTicketFrom(ctx)
	return ticket != nil && ticket.claimed.CompareAndSwap(false, true)
}

// claudeUsageForceProbePending reports whether ctx still carries an UNCLAIMED
// bypass, without consuming it. Diagnostic/assertion use only — a caller that
// intends to SPEND the bypass must use claimClaudeUsageForceProbe.
func claudeUsageForceProbePending(ctx context.Context) bool {
	ticket := claudeUsageForceProbeTicketFrom(ctx)
	return ticket != nil && !ticket.claimed.Load()
}

func claudeUsageForceProbeTicketFrom(ctx context.Context) *claudeUsageForceProbeTicket {
	if ctx == nil {
		return nil
	}
	ticket, _ := ctx.Value(claudeUsageForceProbeKey{}).(*claudeUsageForceProbeTicket)
	return ticket
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
	// Unarm and cancel FIRST, before waiting for anything. A settlement sleeping
	// out a trailing wait — up to claudeUsageProbeMaxTrailingWait — would
	// otherwise sit through the entire drain budget; unarmed eligibility and a
	// closed cancel channel turn that sleep into a prompt return, so the drain
	// below measures work that is genuinely still running.
	claudeUsageProbe.mu.Lock()
	claudeUsageProbe.armed = false
	if claudeUsageProbe.cancelTrailing != nil {
		close(claudeUsageProbe.cancelTrailing)
	}
	claudeUsageProbe.cancelTrailing = make(chan struct{})
	claudeUsageProbe.mu.Unlock()

	deadline := time.Now().Add(claudeUsageProbeDrainTimeout)
	for time.Now().Before(deadline) {
		claudeUsageProbe.mu.Lock()
		inFlight, settling := claudeUsageProbe.inFlight, claudeUsageProbe.settling
		claudeUsageProbe.mu.Unlock()
		if !inFlight && settling == 0 {
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
	// refreshes (and the lastRefreshAt / lastRefreshFingerprint it carries) is
	// deliberately NOT reset: it
	// is a monotonic counter compared only against a value the joiner sampled
	// itself, so zeroing it here would make a stale sample look like an advance.
	claudeUsageProbe.lastAttempt = time.Time{}
	claudeUsageProbe.admittedOver = time.Time{}
	claudeUsageProbe.armed = false
	claudeUsageProbe.failures = 0
	claudeUsageProbe.heldUntil = time.Time{}
	claudeUsageProbe.owedBaseline = time.Time{}
	claudeUsageProbe.owedSeeded = false
	claudeUsageProbe.owedSeededFor = ""
	claudeUsageProbe.owedSeededStampMod = 0
	claudeUsageProbe.owedSeededStampSize = 0
	claudeUsageProbe.owedSeededObservation = time.Time{}
	// A seed still in flight closes its own channel when it returns; dropping the
	// reference here only stops it from marking the NEXT generation as seeded.
	claudeUsageProbe.seedingCh = nil
	// Release any reservation outright. The sleeping trailing timer was already
	// abandoned by the cancel at the top of this function, and the drain has
	// since waited for the settlement holding it to return — so this clears a
	// slot nobody is still sitting in rather than yanking one out from under a
	// live sleeper.
	claudeUsageProbe.trailingScheduled = false
	claudeUsageProbe.settling = 0
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
// running or the minimum interval has not elapsed.
//
// `forced` is the bypass the CALLER already claimed from its own context, never
// a flag begin reads for itself: begin must not consume a bypass on behalf of a
// caller that did not ask for one, or a background probe silently spends the
// refresh's (see claudeUsageForceProbeTicket). Everything above the interval
// check — arm state, single flight, a 429 hold, offline — outranks it.
func (g *claudeUsageProbeGate) begin(now time.Time, forced bool) bool {
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
	if !forced && !g.lastAttempt.IsZero() && now.Sub(g.lastAttempt) < g.interval() {
		return false
	}
	g.inFlight = true
	g.admittedOver = g.lastAttempt
	g.lastAttempt = now
	g.doneCh = make(chan struct{})
	return true
}

// refundAdmission gives back the throttle slot an admission charged, for an
// attempt that returned before asking anything (no credential to ask with).
// Called by the admitted holder while it still holds the single-flight slot, so
// no other begin() can have moved lastAttempt in between.
//
// Without it the startup replay, whose credential read can fail transiently (a
// macOS Keychain timeout), refunds its DURABLE attempt charge yet leaves this
// in-memory one standing: the immediate startup gather then adopts the
// still-outstanding debt, is throttled for a full interval without issuing a
// request, and publishes the pre-update reading.
// probeInFlight reports whether this process already has a probe on the wire.
// Such a probe was charged by whoever admitted it and holds the single-flight
// slot, so a gather arriving behind it issues nothing of its own: seedOwedFromCache
// uses this to decline an adoption rather than bill a second attempt for it.
func (g *claudeUsageProbeGate) probeInFlight() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}

func (g *claudeUsageProbeGate) refundAdmission() {
	g.mu.Lock()
	if g.inFlight {
		g.lastAttempt = g.admittedOver
	}
	g.mu.Unlock()
}

// claudeUsageProbeWholeTimeout bounds ONE complete post-run probe: credential
// lookup, the request, AND the verified persist that follows it.
// claudeUsageProbeTimeout covers the request alone, while
// machineInfoProbeTimeout bounds the macOS Keychain lookup. Anything that must
// outlast the whole sequence — a caller's root deadline, a joiner's wait — is
// derived from here rather than from the request bound, which would expire
// while the reading was still being resolved or written.
//
// Derived from the three bounds the probe itself OBSERVES — credential lookup,
// request, and the ceiling the verified merge enforces on its own persist step
// — so it cannot drift away from them. It matters that the last one is enforced
// rather than estimated: the in-process cache gate has no queue bound, so a
// budget computed as "our wait plus one other writer's" would be exceeded by
// any device running two Claude sessions at once. A var so the tests can pin it
// small.
var claudeUsageProbeWholeTimeout = machineInfoProbeTimeout + claudeUsageProbeTimeout + claudeRateLimitVerifiedPersistBudget

// claudeUsageProbeJoinTimeout bounds how long a forced refresh waits for a probe
// that is already in flight. It must cover the WHOLE probe, not just its
// request: a probe that spends its full HTTP timeout and then contends for the
// cache lock persists a reading AFTER the request deadline, and a join that gave
// up at the request deadline would report "nothing written", fall through, be
// refused by the still-held single-flight slot, and sign the pre-probe cache
// milliseconds before the fresh one lands.
var claudeUsageProbeJoinTimeout = claudeUsageProbeWholeTimeout

// claudeUsageProbeBeforeForcedAttempt is an observation point for deterministic
// gate-race tests. Production leaves it as a no-op.
var claudeUsageProbeBeforeForcedAttempt = func(int) {}

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
//
// `fingerprint` is the account the CALLER is gathering for. A reading persisted
// under a different one is reported as "joined, nothing usable" (a zero
// observedAt) rather than as an answer: the caller's own cache read is scoped to
// its fingerprint and would drop those buckets, so accepting it would end the
// refresh with no fresh utilization for the account actually being signed. The
// caller then falls through and issues the request itself, the slot now free.
func (g *claudeUsageProbeGate) joinInFlight(ctx context.Context, fingerprint string) (joined bool, observedAt time.Time) {
	g.mu.Lock()
	done, before, inFlight := g.doneCh, g.refreshes, g.inFlight
	g.mu.Unlock()
	if !inFlight || done == nil {
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
	after, at, forAccount, shared := g.refreshes, g.lastRefreshAt, g.lastRefreshFingerprint, g.lastRefreshShared
	g.mu.Unlock()
	if after == before || forAccount != fingerprint || shared {
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

// awaitInFlight blocks until the probe currently holding the single-flight slot
// releases it, reporting whether waiting is over on the gate's terms — false
// only when the process is shutting down or the holder outlived the join
// deadline, both states where a retry is pointless. Nothing in flight is
// reported as true: there is simply nothing to wait for.
//
// This is what makes a retry after a latch refusal terminate. Eligibility does
// not model the slot (begin() checks inFlight before the interval), so a caller
// that recomputed eligibility and immediately retried would spin against the
// holder for the whole of its request rather than take its turn behind it.
func (g *claudeUsageProbeGate) awaitInFlight() bool {
	g.mu.Lock()
	done, inFlight := g.doneCh, g.inFlight
	g.mu.Unlock()
	if !inFlight || done == nil {
		return true
	}
	timer := time.NewTimer(claudeUsageProbeJoinTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-shutdownChan:
		return false
	case <-timer.C:
		// A probe outliving its own deadline is a wedge, not a slow answer.
		return false
	}
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

// beginSettling / endSettling bracket one post-run settlement so a reset can
// wait for it. Every successful beginSettling MUST be paired with endSettling on
// every exit path, or resetClaudeUsageProbeGate burns its whole drain budget.
func (g *claudeUsageProbeGate) beginSettling() {
	g.mu.Lock()
	g.settling++
	g.mu.Unlock()
}

func (g *claudeUsageProbeGate) endSettling() {
	g.mu.Lock()
	if g.settling > 0 {
		g.settling--
	}
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

// settleOwedIfCovered clears an outstanding post-run debt when `observed` — a
// reading another writer already put in the SHARED cache — is new enough to
// have seen the run that debt was recorded for.
//
// Separate from settleOwed because the caller here has an OBSERVATION, not the
// baseline it is paying: the cross-process dedupe suppresses on "someone
// answered since `dedupeBaseline`", and on a routine gather that baseline is the
// throttle window rather than the run. Re-reading the debt under the lock and
// testing it with the same claudeUsageObservationCovers rule the probe paths use
// is what keeps a reading that predates the run from clearing it.
func (g *claudeUsageProbeGate) settleOwedIfCovered(observed time.Time) {
	g.mu.Lock()
	if !g.owedBaseline.IsZero() && claudeUsageObservationCovers(observed, g.owedBaseline) {
		g.owedBaseline = time.Time{}
	}
	g.mu.Unlock()
}

// owedObservation returns the outstanding post-run baseline, if any. A pure
// in-memory read: its callers include claudeUsageProbePayRecordedRun's
// preflight loop, which must stay free of cache and credential reads (see the
// note there). The DURABLE debt reaches the gate through seedOwedFromCache.
func (g *claudeUsageProbeGate) owedObservation() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owedBaseline
}

// refreshGeneration samples the count of probes that have persisted a reading in
// this process. Its only purpose is to be handed back to seedOwedFromCache, so
// the "did the shared cache move under this gather?" question is asked as of the
// moment the caller READ the cache rather than the moment the seed runs — see the
// note there.
func (g *claudeUsageProbeGate) refreshGeneration() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshes
}

// refreshLandedSince reports whether a probe of THIS process persisted — or
// deduped against another writer that persisted — for
// `fingerprint`, a reading newer than `latest` since `generation` was sampled.
//
// seedOwedFromCache asks the same question, but only as of the moment it runs.
// The startup replay is asynchronous and can land AFTER the seed has returned —
// settling the very debt the seed adopted — so a gather that then finds nothing
// owed and a fresh-looking `latest` would publish the pre-replay view. Asking
// again at each "not refreshed" exit, against the generation the caller sampled
// before its load, closes that window without a second cache read. Sampled
// relatively, never as an absolute claim, for the reason
// resetClaudeUsageProbeGate documents.
func (g *claudeUsageProbeGate) refreshLandedSince(generation uint64, fingerprint string, latest time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshes != generation && g.lastRefreshFingerprint == fingerprint && g.lastRefreshAt.After(latest)
}

// seedOwedFromCache adopts the state another process persisted — the post-run
// debt and any 429 hold — so the `owing` branch below sees a run this process
// never saw, and begin() honours backpressure this process never took.
//
// Adopted once per account per SNAPSHOT, not once per account per process: the
// latch is keyed to claudeRateLimitCacheStamp, so the persisted state is read
// once while the file is unchanged and re-read when it is not. A second agent
// channel writes the same file, and a latch believed for the process lifetime
// would hide a debt or a hold it recorded afterwards — see owedSeededStampMod.
//
// payOwedClaudeUsageRefresh restores both, but it is SPAWNED: the first gather of
// a fresh agent can reach the staleness check before that goroutine has read the
// credential store and the cache, find a reading younger than
// claudeUsageProbeStaleAfter, and publish the PRE-update utilization — the stale
// card this whole path exists to prevent — or, inside a persisted Retry-After
// window, be ADMITTED and put another request to an endpoint that just refused
// this device.
//
// It takes the fingerprint the gather already decoded from its one credential
// read rather than resolving its own. refreshClaudeUsageIfStale makes no
// credential read by contract (a `security` spawn per gather on macOS), and the
// seed must not be the thing that breaks it.
//
// The cache read happens OFF g.mu: that mutex is taken on the session stdout
// path, so nothing touching the filesystem may be held under it.
//
// The hold is adopted under the same claudeUsageProbeMaxRetryAfter ceiling
// payOwedClaudeUsageRefreshAt applies, so a value left by a stepped clock cannot
// park utilization for days. A skewed one is only IGNORED here, never cleared:
// the replay owns the durable drop, and this path writes nothing.
//
// `latest` is the freshest observation the caller already read out of the cache,
// and it is the first coverage test: a debt a reading THIS gather is holding
// already answers is paid, and adopting it would make the gather probe for an
// observation it has in hand.
//
// Adoption is then SERIALIZED against settlement, because the startup replay
// (payOwedClaudeUsageRefresh) races this read: it can persist a covering
// reading, clear the debt on disk AND on the gate, and set lastAttempt, all
// while the unlocked read above is in flight. Recording the value we loaded
// before that would resurrect a PAID debt, and the gather's own probe would then
// be refused by the throttle the replay just spent — leaving it to publish the
// pre-replay view, which is the stale first report this whole path exists to
// prevent. The generation counter is what detects it: refreshes advancing since
// `generation` was sampled means a probe of this process persisted a reading, and
// lastRefreshAt / lastRefreshFingerprint say what it observed and for whom —
// sampled relatively, never as an absolute claim, for the reason
// resetClaudeUsageProbeGate documents.
//
// `generation` is sampled by the CALLER, before it loaded `latest`, and is
// deliberately not re-sampled here: a replay that landed in the gap between the
// caller's cache load and this call is ALREADY counted in a sample taken at entry,
// so the comparison would report "nothing happened" for a reading that superseded
// `latest` — and, settlement being the same write, the debt would be gone from
// disk too, leaving nothing else to catch it. Anchoring the sample to the load it
// is compared against is what closes that window.
//
// Returns a covering observation whenever a refresh that landed while we read
// has moved the shared cache PAST `latest`, so the caller re-reads the cache
// instead of shaping metrics from the view it loaded before the replay landed.
// Zero means "nothing to re-read" — the ordinary case.
//
// That answer is given even when NO persisted debt remains, which is the usual
// ordering rather than an edge: the replay's settlement is the same locked write
// that carries the reading, so a replay landing during the unlocked read above
// has already cleared the debt from disk by the time we load it. Keying the
// answer off the debt would report "nothing happened" for precisely the case the
// generation counter exists to catch, and leave the gather to publish the
// pre-replay view for the whole TTL. A needless re-read costs one cache load, and
// the latch makes it at most one per account per process.
func (g *claudeUsageProbeGate) seedOwedFromCache(ctx context.Context, fingerprint string, generation uint64, now, latest time.Time) time.Time {
	var stampMod, stampSize int64
	var seeding chan struct{}
	// One-shot, for the post-latch stamp recheck below.
	revalidated := false
	for seeding == nil {
		// Sampled BEFORE any read and off g.mu — a write landing between this stat
		// and the reads below leaves the recorded stamp OLDER than the content
		// adopted, so the next gather re-opens the adoption; sampling it afterwards
		// could record a stamp newer than what was read and hide that write for the
		// process lifetime.
		//
		// Re-sampled on EVERY pass rather than once at entry: a pass that follows a
		// wait on seedingCh would otherwise test the latch against a stamp taken
		// before the wait, and a snapshot another process renamed in during it would
		// still match the claimant's older stamp — returning the latched verdict
		// without ever reading the debt or hold that write carried. Sampled
		// immediately before the latch test, a write can only land after it, which
		// is the same as landing after this gather returned: the next gather's stamp
		// differs and re-opens the adoption.
		stampMod, stampSize = claudeRateLimitCacheStamp()
		claudeUsageProbeAfterSeedStamp()
		g.mu.Lock()
		if g.owedSeeded && g.owedSeededFor == fingerprint &&
			g.owedSeededStampMod == stampMod && g.owedSeededStampSize == stampSize {
			// The adoption already happened — for this account, in this process,
			// against the snapshot still on disk unchanged.
			// Its debt and hold are on the gate, but its re-read verdict was
			// about the CLAIMANT's view. Re-derive one for this caller's `latest`
			// from the reading that seed measured, so a peer that waited here (or
			// arrived later) cannot publish a view the shared cache has moved
			// past.
			superseded := g.supersedingObservationLocked(g.owedSeededObservation, latest, now)
			g.mu.Unlock()
			// The stamp above was sampled off g.mu, so a snapshot another process
			// renamed in between that stat and the latch test still matches the
			// recorded one: the latch would hand back a verdict about the PREVIOUS
			// snapshot and never read the debt or Retry-After that write carried.
			// Re-stat after the decision and, when the file moved, fall back
			// through the loop — the next pass samples the new stamp, misses the
			// latch and performs a full adoption.
			//
			// ONE recheck, not a retry loop: the window cannot be closed by
			// stat-based detection (a write can always land after the last stat),
			// and spinning against a hot writer would block the gather. What makes
			// a still-missed write harmless is that a latch HIT records nothing —
			// the stamp on the gate stays the claimant's, older than what is now on
			// disk — so the next gather's sample differs and re-opens the adoption
			// regardless. This only shortens the exposure from "until the next
			// gather" to the width of the recheck.
			if !revalidated {
				if mod, size := claudeRateLimitCacheStamp(); mod != stampMod || size != stampSize {
					revalidated = true
					continue
				}
			}
			return superseded
		}
		if pending := g.seedingCh; pending != nil {
			g.mu.Unlock()
			// Another gather is mid-adoption. Wait for it, then look again: it may
			// have been seeding a DIFFERENT account, in which case this one still
			// has its own to adopt. Bounded by the gather's context — a gather that
			// gives up here degrades to the unseeded path, never to a hang.
			claudeUsageProbeSeedWaiting()
			select {
			case <-pending:
			case <-ctx.Done():
				return time.Time{}
			}
			continue
		}
		seeding = make(chan struct{})
		g.seedingCh = seeding
		g.mu.Unlock()
	}
	// Marked seeded only once the adoption below is done, so no peer can pass the
	// latch before the debt and hold are on the gate. A reset in the meantime
	// swaps seedingCh out, and this seed then leaves the latch for the next
	// generation to claim.
	// measured is the reading the adoption below settled on, handed to the latch
	// so every later caller derives its own verdict from it. Registered BEFORE
	// the tail's `defer g.mu.Unlock()`, so it runs after it — this relock is not
	// a recursive one.
	measured := time.Time{}
	// Claimed unless the adoption below declines a debt for a reason that can
	// change without the snapshot changing — see the blockedFromIssuing check.
	latch := true
	defer func() {
		g.mu.Lock()
		if g.seedingCh == seeding {
			g.seedingCh = nil
			if !latch {
				g.mu.Unlock()
				close(seeding)
				return
			}
			g.owedSeeded = true
			g.owedSeededFor = fingerprint
			g.owedSeededObservation = measured
			g.owedSeededStampMod, g.owedSeededStampSize = stampMod, stampSize
		}
		g.mu.Unlock()
		close(seeding)
	}()
	persisted, attempts, held := claudePersistedProbeStateFor(fingerprint)
	// A debt the replay would retire without a request is not adopted either:
	// this read races the replay, and adopting a capped, aged-out or skewed debt
	// would let the gather spend the uncharged request the cap exists to refuse.
	// Only the replay CLEARS it on disk — this is a read on the gather path.
	retired := time.Time{}
	if !persisted.IsZero() && claudeRefreshDebtRetired(persisted, attempts, now) {
		retired, persisted = persisted, time.Time{}
	}
	// Before anything can be admitted: holdUntil is monotonic, so a live hold this
	// process already took outranks a shorter persisted one.
	if !held.IsZero() && !held.After(now.Add(claudeUsageProbeMaxRetryAfter)) {
		g.holdUntil(held)
	}
	claudeUsageProbeAfterSeedRead()
	// The shared cache as it stands NOW, measured by the same row-aware rule as
	// `latest`. The generation counter below only sees probes of THIS process; an
	// overlapping old agent or a second agent channel writes the same file and
	// settles the same durable debt in that write, so without this a reading it
	// persisted after the caller's load would leave the seed finding no debt and
	// nothing to report, and the gather would publish the pre-refresh view. Loaded
	// AFTER the debt read, so any write that cleared the debt is visible here, and
	// before g.mu is taken, since nothing touching the filesystem may be held
	// under it.
	onDisk := claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint), now)
	// CHARGE before the gate can be handed a debt to probe for. Adoption is what
	// turns a persisted debt into this gather's `owing` branch, and that branch
	// issues its request off the in-memory baseline without consulting the durable
	// counter again — so an uncharged adoption spends a request the two-attempt cap
	// never sees. Left uncharged, a gather that beats the startup replay to the
	// seed issues one request, and the replay's own charge is then refunded when
	// the in-memory throttle refuses it: the counter comes back to where it
	// started, and every restart repeats it. The cap is the only thing bounding a
	// crash-looping agent against an account-scoped endpoint, so the counter moves
	// on the same side of the request here as it does in payOwedClaudeUsageRefreshAt.
	//
	// Keyed to the instant read above and bounded at the cap inside
	// adjustClaudeRefreshAttemptsAt, so a refused charge — contended cache, a debt
	// another writer settled or replaced while we read, or a count two overlapping
	// processes both passed the unlocked retirement test on — means "do not adopt".
	// That degrades to the pre-seed behaviour: the gather still probes on its own
	// staleness TTL, and the debt is left exactly as found for the next start.
	//
	// Charged off g.mu (it takes the cache gate and flock) and only for a debt the
	// readings already in hand do not answer, so the ordinary no-debt gather still
	// writes nothing. `landed` is not yet known here; an adoption the locked read
	// below then declines because a probe of this process settled the debt while we
	// read keeps the charge. Same direction as the replay's crash-between-charge-
	// and-refund window: it only ever spends the budget faster, and the reading
	// that declined it is the one the debt wanted.
	uncovered := !persisted.IsZero() && !claudeUsageObservationCovers(latest, persisted) &&
		!claudeUsageObservationCovers(onDisk, persisted)
	// A debt no probe of this gather could pay is not adopted, and — crucially —
	// not CHARGED. begin() refuses on an unarmed gate, a live 429 hold (possibly
	// the one this seed adopted two statements up), offline mode, an unelapsed
	// throttle interval, a probe of this process already on the wire (whoever
	// put it there charged for it; this gather would only join it), a rejected
	// endpoint override, or a context that
	// has already ended, so a charge here would buy a request that is never
	// issued: two such starts reach the attempt cap and the startup replay then
	// retires a debt nothing ever asked the endpoint about, leaving the pre-run
	// utilization stale for the whole TTL — the exact failure this path exists to
	// prevent. Same rule as payOwedClaudeUsageRefreshAt, which returns ahead of
	// its charge for the same two refusals and leaves the debt and its counter
	// exactly as found.
	//
	// Checked rather than refunded because the gather never reports back whether
	// it issued anything: the adoption hands the debt to a later `owing` branch,
	// and a refund keyed to this call would have to outlive it.
	//
	// The latch is left UNCLAIMED, so the next gather re-runs the adoption once
	// the hold expires or the agent reconnects — neither of which touches the
	// snapshot the latch is keyed to. Latching a refusal would strand the debt
	// for the rest of this process's life.
	if uncovered && (ctx.Err() != nil || g.blockedFromIssuing(now)) {
		claudeUsageProbeSeedBlocked()
		persisted, uncovered, latch = time.Time{}, false, false
	}
	claudeUsageProbeBeforeSeedCharge()
	if uncovered {
		charge := adjustClaudeRefreshAttemptsAt(persisted, +1)
		lockedHeldMs := int64(0)
		lockedObserved := time.Time{}
		lockedHeld := false
		lockedInFlight := false
		// Both values are sampled on EVERY path, under the SAME lock the charge
		// takes and BEFORE the charge decides anything, because the unlocked read
		// above is necessarily older than this write:
		//
		//   - the hold, so a 429 another channel recorded in that gap is not
		//     latched away unseen for this account;
		//   - the observation, so a covering reading another writer persisted in
		//     that same gap reaches `landed` below. That reading is exactly what
		//     makes the charge refuse (the debt it settled is gone), and a refusal
		//     that dropped it would leave a gather holding a pre-run `latest` with
		//     no debt to probe for and no re-read to report — publishing the stale
		//     view this whole path exists to prevent.
		charged, wroteMod, wroteSize := mutateClaudeRateLimitSnapshotStamped(claudeRateLimitCachePath(), fingerprint,
			func(snap *claudeRateLimitSnapshot) bool {
				lockedHeldMs = snap.HeldUntilMs
				lockedObserved = claudeSnapshotFreshness(claudeRateLimitView{
					buckets:    snap.Buckets,
					probedAtMs: snap.LastProbeObservedAtMs,
				}, now)
				// The hold the unlocked blockedFromIssuing pre-check could not see.
				// Another channel may have recorded a 429 for this account between
				// that check and this lock, and a charge on top of it buys a request
				// begin() is bound to refuse once the hold is adopted five lines
				// below — the same uncharged-refusal rule the pre-check applies,
				// decided on the freshest evidence rather than the stale read.
				//
				// A hold stamped implausibly far ahead is NOT a reason to refuse:
				// the skew ceiling discards it, so it never reaches the gate and
				// never blocks anything. Same bound as the adoption below.
				if snap.HeldUntilMs > 0 {
					if locked := time.UnixMilli(snap.HeldUntilMs); locked.After(now) &&
						!locked.After(now.Add(claudeUsageProbeMaxRetryAfter)) {
						lockedHeld = true
						return false
					}
				}
				// The probe the unlocked blockedFromIssuing pre-check could not
				// see. A probe of this process admitted between that check and
				// this lock already carries a charge of its own, and begin()
				// will refuse this gather the slot — so charging on top bills a
				// second attempt for the one request on the wire. Read under
				// g.mu INSIDE the cache lock: the ladder only ever runs this
				// way round, since nothing in this file touches the filesystem
				// while holding g.mu.
				if g.probeInFlight() {
					lockedInFlight = true
					return false
				}
				return charge(snap)
			})
		claudeUsageProbeAfterSeedCharge()
		if lockedHeld || lockedInFlight {
			// Declined for a reason that can change without the snapshot changing,
			// so the latch is left unclaimed exactly as the pre-check leaves it:
			// the gather after the hold expires — or after the in-flight probe
			// lands without settling this debt — must reach the adoption again.
			// `charged` is false, so the debt is un-adopted by the branch below and
			// its counter stays where the next start can still spend it.
			claudeUsageProbeSeedBlocked()
			latch = false
		}
		if lockedHeldMs > 0 {
			if locked := time.UnixMilli(lockedHeldMs); !locked.After(now.Add(claudeUsageProbeMaxRetryAfter)) {
				// Monotonic, like the adoption above: this only ever raises the hold.
				g.holdUntil(locked)
			}
		}
		// Measured by the same rule as `onDisk` and only ever raises it: this is
		// the primary cache alone, whereas onDisk also folds the pinned hook cache.
		if lockedObserved.After(onDisk) {
			onDisk = lockedObserved
		}
		if charged {
			// The charge rewrote the snapshot, so the stamp sampled before the
			// reads no longer describes the file. Take the one the mutation stat'd
			// under its own lock, or the latch this seed is about to record is
			// stale on arrival and EVERY later gather re-adopts — resurrecting a
			// debt this process has since settled in memory, since only the
			// covering merge clears it on disk. Stamping under that lock is also
			// what keeps the stamp describing the snapshot whose hold we just
			// adopted: an off-lock stat could describe another process's newer
			// file and latch its Retry-After away unread. A failed stat reports
			// zeroes, and the older stamp we already hold is kept — it re-opens
			// the adoption rather than latching one away.
			if wroteMod != 0 || wroteSize != 0 {
				stampMod, stampSize = wroteMod, wroteSize
			}
		} else {
			// Refused: contended cache, a debt another writer settled or replaced
			// while we read, or a count two overlapping processes both passed the
			// unlocked retirement test on. Nothing was written, so the stamp still
			// describes the file — and an uncharged debt is not adopted. The
			// reading sampled above still travels, so a settlement that landed in
			// the gap is reported to the caller as a re-read.
			persisted = time.Time{}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// A retired debt an EARLIER seed already adopted is dropped from the gate
	// too — refusing to re-adopt it is not enough, since the gate still carries
	// it and the next gather's `owing` branch would spend the uncharged request
	// the cap just refused (e.g. the replay charged the final attempt and
	// failed). Compared at the cache's millisecond resolution, so the same debt
	// recorded locally at sub-millisecond precision is recognised as the one
	// retired, while a strictly newer run this process owes survives.
	if !retired.IsZero() && !g.owedBaseline.IsZero() &&
		g.owedBaseline.UnixMilli() <= retired.UnixMilli() {
		g.owedBaseline = time.Time{}
	}
	// What a probe of THIS process persisted while we were reading, for THIS
	// account. Read only when the generation advanced: lastRefreshAt is sampled
	// RELATIVELY, never as an absolute claim, for the reason
	// resetClaudeUsageProbeGate documents.
	landed := time.Time{}
	if g.refreshes != generation && g.lastRefreshFingerprint == fingerprint {
		landed = g.lastRefreshAt
	}
	if onDisk.After(landed) {
		landed = onDisk
	}
	measured = landed
	// A debt neither the caller's view nor the landed reading covers is adopted —
	// and only one whose durable attempt was charged above ever reaches here, so
	// the request this adoption hands the caller is inside the same cap the
	// startup replay spends from.
	// (Covered by `landed` means it was settled while we were reading.)
	if !persisted.IsZero() && !claudeUsageObservationCovers(latest, persisted) &&
		!claudeUsageObservationCovers(landed, persisted) {
		// Inlined rather than calling recordOwed so the decision and the record
		// are one critical section: releasing the lock between them would reopen
		// exactly the window above. Same rule as recordOwed — only ever RAISE the
		// baseline, so a persisted debt can never walk back one this process
		// recorded in the meantime.
		if persisted.After(g.owedBaseline) {
			g.owedBaseline = persisted
		}
		// A debt was adopted, so the caller has a probe to issue: report no
		// re-read even if the cache moved, or it would return before reaching it.
		return time.Time{}
	}
	return g.supersedingObservationLocked(landed, latest, now)
}

// supersedingObservationLocked decides whether `observation` — the freshest
// reading the shared cache holds — supersedes the view a gather already loaded,
// and must therefore send it back to re-read the cache instead of shaping
// metrics from `latest`.
//
// Shared by seedOwedFromCache's two exits (the claimant that measured the
// reading, and every caller the latch turns away afterwards) so the two cannot
// drift: a peer answered by a different rule than the claimant is precisely the
// split that let a waiter publish a pre-replay view.
//
// Called with g.mu held, and it may PAY an in-memory debt, so it is not a pure
// predicate.
func (g *claudeUsageProbeGate) supersedingObservationLocked(observation, latest, now time.Time) time.Time {
	// Nothing newer than what the caller is already holding.
	if observation.IsZero() || !observation.After(latest) {
		return time.Time{}
	}
	// A superseding reading that has itself aged past the staleness TTL answers
	// nothing: the caller still has a probe to issue, and the ordinary path
	// already asks for a re-read when the shared cache moved under it.
	if now.Sub(observation) >= claudeUsageProbeStaleAfter {
		return time.Time{}
	}
	// Nor does one that predates a debt this process recorded itself — the
	// caller must reach the `owing` branch. One that covers it pays it, exactly
	// as settleOwedIfCovered would on the dedupe path this early return skips;
	// leaving it standing would bring a trailing probe back for a reading
	// already on disk.
	if !g.owedBaseline.IsZero() {
		if !claudeUsageObservationCovers(observation, g.owedBaseline) {
			return time.Time{}
		}
		g.owedBaseline = time.Time{}
	}
	return observation
}

// claudeUsageProbeAfterSeedRead is an observation point for the deterministic
// seed/replay race test, sitting between seedOwedFromCache's unlocked cache read
// and the locked adoption it guards. Production leaves it as a no-op, like
// claudeUsageProbeBeforeForcedAttempt.
var claudeUsageProbeAfterSeedRead = func() {}

// claudeUsageProbeSeedWaiting is an observation point for the latch re-sample
// test: it runs when a gather is about to block on another gather's adoption.
// Production leaves it as a no-op.
var claudeUsageProbeSeedWaiting = func() {}

// claudeUsageProbeAfterSeedStamp is an observation point for the post-latch
// stamp recheck test: it sits in seedOwedFromCache between the off-lock stamp
// sample and the locked latch test, the exact window a cross-process rename has
// to slip through. Production leaves it as a no-op.
var claudeUsageProbeAfterSeedStamp = func() {}

// claudeUsageProbeSeedBlocked is an observation point for the "adoption is not
// charged when nothing could issue a request" test. Production leaves it as a
// no-op.
var claudeUsageProbeSeedBlocked = func() {}

// claudeUsageProbeBeforeSeedCharge is an observation point for the "a refused
// charge still carries the reading that refused it" test: it sits between
// seedOwedFromCache's unlocked cache load and the locked charge, the window in
// which another process's covering write both settles the debt and invalidates
// the view this gather measured. Production leaves it as a no-op.
var claudeUsageProbeBeforeSeedCharge = func() {}

// claudeUsageProbeAfterSeedCharge is an observation point for the "the latch
// carries the stamp of the snapshot the charge wrote" test: it runs after the
// charge has released the cache lock and before its stamp is used, which is
// exactly where an off-lock stat would pick up another process's newer file.
// Production leaves it as a no-op.
var claudeUsageProbeAfterSeedCharge = func() {}

// blockedFromIssuing reports whether the gather that is adopting a debt would
// put no NEW request on the wire for it: an unarmed (opted-out) gate, a live
// server-imposed hold, offline mode, an in-memory throttle interval that has
// not elapsed, a probe of this process already in flight, or a rejected
// endpoint override. Used by seedOwedFromCache to decline — uncharged — a debt
// it cannot hand a request of its own to.
//
// The interval IS consulted, because begin() checks it too: the `owing` branch
// beats the staleness TTL and the cross-process dedupe baseline, not the
// throttle. Without it, a gather arriving right behind a failed startup replay
// (which has already charged one attempt and moved lastAttempt) charges the
// second and last attempt, is then refused by begin() without a request going
// out, and the next start retires a debt the endpoint was asked about exactly
// once. A forced refresh does bypass the interval, but it probes unconditionally
// anyway and the merge that carries its reading settles the durable debt, so
// declining the adoption costs it nothing.
//
// inFlight IS consulted, and is a refusal rather than an inheritance: the probe
// already on the wire was charged by whoever put it there, and begin() refuses
// the slot, so this gather only JOINS it. Charging here would bill two attempts
// for one request — and the seed, unlike payOwedClaudeUsageRefreshAt, has no
// refund path, because the gather never reports `issued` back to it. If that
// in-flight probe settles the debt the adoption is moot; if it fails, the latch
// is unclaimed, so the gather after the throttle interval adopts and charges
// once, keeping the cap counting requests rather than gathers.
func (g *claudeUsageProbeGate) blockedFromIssuing(now time.Time) bool {
	g.mu.Lock()
	armed, held, inFlight := g.armed, g.heldUntil, g.inFlight
	throttled := !g.lastAttempt.IsZero() && now.Sub(g.lastAttempt) < g.interval()
	g.mu.Unlock()
	if !armed {
		return true
	}
	if !held.IsZero() && now.Before(held) {
		return true
	}
	if inFlight || throttled {
		return true
	}
	// A rejected endpoint override (malformed or non-loopback) is the one
	// refusal that lives PAST admission: begin() lets the probe through, it
	// resolves the credential, and probeClaudeUsage then returns issued=false
	// without putting anything on the wire. The gather never reports that flag
	// back here — the adoption hands the debt to a later `owing` branch — so
	// unlike the startup replay, which refunds its charge on that same exit,
	// the seed has no way to give the attempt back. Two such starts would reach
	// the cap and retire a debt no request was ever made for. Checked ahead of
	// the charge instead, which is the same uncharged-refusal rule the rest of
	// this function applies; it is an env read and a url.Parse, no I/O.
	if claudeUsageProbeURL() == "" {
		return true
	}
	return IsOffline()
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

// finish releases the single-flight slot and records the outcome. A probe that
// refreshed the cache — or that skipped before issuing a request — clears the
// failure streak; only a real failure extends the backoff.
//
// `observedAt` is the observation the cache holds for the windows the probe
// wrote and `fingerprint` the account it wrote them under, both recorded only
// alongside a refresh so a joiner can tell WHEN the reading it is inheriting
// was taken and WHOSE it is, not merely that there was one.
//
// An unrefreshed probe that still reports an observation took the dedupe path:
// another writer's covering reading is on disk and the debt it covers is
// already settled. It advances the generation too — flagged shared — so a gather
// that loaded its view before it cannot publish the pre-write metrics.
func (g *claudeUsageProbeGate) finish(probeErr *cliAgentUsageError, refreshed bool, observedAt time.Time, fingerprint string) {
	g.mu.Lock()
	g.inFlight = false
	if refreshed || (probeErr == nil && !observedAt.IsZero()) {
		g.refreshes++
		g.lastRefreshAt = observedAt
		g.lastRefreshFingerprint = fingerprint
		g.lastRefreshShared = !refreshed
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
//
// Returns the shared observation alongside the verdict so a suppressed caller
// can settle a post-run debt that observation already covers, instead of leaving
// a debt recorded that no probe will ever be issued to pay.
func claudeUsageProbeObservedSince(fingerprint string, baseline, now time.Time) (time.Time, bool) {
	if baseline.IsZero() {
		return time.Time{}, false
	}
	// Row-aware for the same reason the staleness TTL is: a status-line render
	// answers only five_hour/seven_day, so accepting it as "someone already
	// answered" would let interactive renders suppress the probe the weekly-split
	// and Fable rows depend on. A probe writes every window the endpoint supplies,
	// so the cross-process dedupe this exists for still collapses cleanly.
	latest := claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint), now)
	return latest, !latest.IsZero() && latest.After(baseline)
}

// claudeSnapshotFreshness is how old the DISPLAYED snapshot is — the stalest of
// the rows the card shows, excluding the rows this process's own probe has shown
// it cannot supply. Every freshness decision in this file goes through it, so
// the TTL check, the cross-process dedupe and the post-run debt cannot disagree
// about what "fresh" means.
func claudeSnapshotFreshness(view claudeRateLimitView, now time.Time) time.Time {
	return stalestClaudeRowObservation(view, now)
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
	raw, ok := readClaudeCredentialsRaw(context.Background(), base)
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
//
// `forced` must be a bypass the caller CLAIMED (claimForce); it is never read
// from the gate here, so a post-run probe cannot spend a refresh's bypass.
func runClaudeUsageProbe(ctx context.Context, now time.Time, forced bool) (bool, *cliAgentUsageError) {
	refreshed, _, probeErr := probeClaudeUsage(ctx, now, claudeUsageProbeStoredIdentity, now, forced)
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
//
// It is ALSO populated, with `refreshed` false, when the cross-process dedupe
// suppressed the request because another writer had already recorded a reading:
// that reading is in the shared cache now, so a caller that loaded its view
// earlier must still re-read. Every other non-refreshing path — opted out, no
// credential, throttled, failed — returns a zero instant, so "non-zero"
// unambiguously means the shared cache holds this observation.
func probeClaudeUsage(
	ctx context.Context,
	now time.Time,
	resolveIdentity func() claudeUsageProbeIdentity,
	dedupeBaseline time.Time,
	forced bool,
) (refreshed bool, observedAt time.Time, probeErr *cliAgentUsageError) {
	_, _, refreshed, observedAt, probeErr = probeClaudeUsageAdmitted(ctx, now, resolveIdentity, dedupeBaseline, forced)
	return refreshed, observedAt, probeErr
}

// probeClaudeUsageAdmitted is probeClaudeUsage reporting, in addition, whether
// THIS call was the one begin() admitted to the single-flight slot. A caller
// settling a post-run debt needs that fact directly: "admitted and it
// skipped/failed" leaves the debt for the next gather, while "refused by the
// latch" means the debt has not had its turn and must retry behind the holder.
//
// It is reported here, from begin()'s own answer, rather than reconstructed
// afterwards by comparing the caller's instant with the gate's lastAttempt
// (what admittedAt did until v1.0.27). That comparison assumed two goroutines
// never read the same wall instant, which is false on Windows: Go's clock there
// advances on the interrupt tick (0.5–15.6 ms), so a post-run probe refused a
// few milliseconds behind a routine gather read the gather's instant, judged
// itself admitted, and dropped the retry — the run's utilization never moved
// (TestClaudeUsageProbeAfterRun_RetriesAfterSingleFlightRefusal and the
// native-lifecycle freshness test failing only on the windows-latest runner).
func probeClaudeUsageAdmitted(
	ctx context.Context,
	now time.Time,
	resolveIdentity func() claudeUsageProbeIdentity,
	dedupeBaseline time.Time,
	forced bool,
) (admitted, issued, refreshed bool, observedAt time.Time, probeErr *cliAgentUsageError) {
	// An already-cancelled gather must not burn the throttle slot on a request
	// that cannot complete: the next caller would then be refused for a minute
	// because of a probe that never left the process.
	if ctx.Err() != nil {
		return false, false, false, time.Time{}, nil
	}
	if !claudeUsageProbe.begin(now, forced) {
		return false, false, false, time.Time{}, nil
	}
	admitted = true
	// Release the single-flight latch on EVERY path out of here. begin() has
	// already set inFlight, so an early return that skipped this would leave the
	// latch stuck and every future probe in this process rejected — a permanent
	// wedge, not a missed sample. It must therefore be the first statement after
	// begin(), ahead of any other exit. Named returns let it record the outcome:
	// a failure extends the backoff, a success or an early skip clears it.
	//
	// persistedFor is the account any reading is written under, resolved below
	// and read by the deferred call so a joiner can reject a reading that belongs
	// to a different one. It stays empty on every path that exits before the
	// credential is read, which is also every path that persists nothing.
	persistedFor := ""
	defer func() { claudeUsageProbe.finish(probeErr, refreshed, observedAt, persistedFor) }()

	// One credential read for both the bearer token and the cache fingerprint —
	// see claudeUsageProbeIdentity for why they must not be resolved separately.
	identity := resolveIdentity()
	persistedFor = identity.fingerprint
	if identity.token == "" {
		// Not an error: a signed-out device simply has nothing for this probe.
		// `issued` stays false — this is the one path that is ADMITTED yet learns
		// nothing at all, so a caller that paid for the turn (the startup replay's
		// attempt charge) must be able to tell it apart from a probe that actually
		// asked. The credential store can fail TRANSIENTLY here — a Keychain timeout
		// on macOS — and charging those would retire a debt no request was ever made
		// for, leaving exactly the stale card the replay exists to clear. The same
		// goes for the in-memory throttle slot begin() just charged.
		claudeUsageProbe.refundAdmission()
		return admitted, false, false, time.Time{}, nil
	}
	// Past the credential: from here every exit either asked the endpoint or
	// inherited another writer's covering reading, so the turn was spent — bar
	// a rejected endpoint override, which un-issues itself below.
	issued = true

	// Cross-process coordination on an ACCOUNT-scoped endpoint: has another
	// writer on this machine already answered what this probe would ask?
	if sharedAt, answered := claudeUsageProbeObservedSince(identity.fingerprint, dedupeBaseline, now); answered {
		// Somebody else already paid for this reading. If it is also new enough to
		// have seen the run we owe an observation for, that debt is SETTLED — by
		// another writer rather than by us. Leaving it recorded would keep a debt
		// standing that no probe is ever going to be issued to pay, since every
		// later attempt re-takes this same branch against the same reading.
		// settleOwedIfCovered re-checks coverage against the CURRENT debt under the
		// gate lock, so a run that finished after this reading keeps its debt.
		claudeUsageProbe.settleOwedIfCovered(sharedAt)
		// Report the shared reading even though this process issued no request.
		// `refreshed` stays false — nothing here wrote, so finish() records it as
		// SHARED: visible to refreshLandedSince, never inherited by a forced
		// joiner as ours — but the caller still
		// has to know the SHARED cache moved: it loaded its view before this
		// check, so without the instant it would shape metrics from the pre-write
		// buckets while the debt (and the trailing retry that would have
		// corrected it) is already settled. See refreshClaudeUsageIfStale.
		return admitted, issued, false, sharedAt, nil
	}
	endpoint := claudeUsageProbeURL()
	if endpoint == "" {
		// A rejected endpoint override (malformed or non-loopback) sends nothing,
		// so this attempt is NOT issued: reporting it as one would let the startup
		// replay keep its durable charge, and two such starts would retire a debt
		// no request was ever made for. The in-memory throttle and failure backoff
		// are kept — the misconfiguration is persistent, and they are what stop
		// every gather from re-walking this path.
		return admitted, false, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	reqCtx, cancel := context.WithTimeout(ctx, claudeUsageProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorInternal)
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
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(category)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, claudeUsageProbeMaxBody))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorNotAuthenticated)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Honor the service backpressure rather than only our own timer. This
		// endpoint is account-scoped and shared with every other poller on the
		// account — Claude own /usage panel included — so ignoring Retry-After
		// would keep pressing exactly when we have been asked to stop.
		hold := retryAfterDeadline(resp.Header.Get("Retry-After"), time.Now())
		claudeUsageProbe.holdUntil(hold)
		// Durable too: an in-memory hold is forgotten by a restart, and the
		// startup replay would then fire straight at an endpoint that just told
		// us to stop — on a limit scoped to an account every device shares.
		claudeHoldUsageProbe(identity.fingerprint, hold)
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	// Read at most the cap + 1 byte so an oversized body is DETECTED rather than
	// silently truncated into a JSON parse error — a truncated payload must never
	// be treated as a partial observation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeUsageProbeMaxBody+1))
	if err != nil || len(body) > claudeUsageProbeMaxBody {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorProviderUnavailable)
	}

	var decoded claudeUsageProbeResponse
	if json.Unmarshal(body, &decoded) != nil {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}

	updates := claudeUsageProbeBuckets(decoded, now)
	if len(updates) == 0 {
		// A response we cannot plot is not an observation. Returning here leaves
		// the cache byte-identical rather than stamping a fresh ObservedAt on
		// nothing.
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorParseFailed)
	}
	// Report success only if the snapshot actually reached disk. The
	// fire-and-forget merge swallows an unwritable data dir, a Windows sharing
	// violation, and a failed rename alike — returning true on any of those would
	// tell the caller to re-read a cache that never changed, clear the failure
	// backoff, and throttle the retry, while a SIGNED refresh receipt went out
	// carrying an observation that was never persisted.
	//
	// ctx, not context.Background(): the merge has its own persist budget, but
	// this call is reached at the very end of the gather's shared deadline, so
	// that budget must be clamped by whatever is left of it rather than added to
	// it. An abandoned merge still finishes on its own goroutine — the reading
	// lands for the next gather; this probe just does not claim it.
	persisted, err := mergeClaudeRateLimitCacheChecked(ctx, claudeRateLimitCachePath(), updates, now,
		identity.fingerprint, claudeRateLimitSourceProbe)
	if err != nil {
		return admitted, issued, false, time.Time{}, claudeUsageProbeFailure(cliUsageErrorCollectionFailed)
	}
	// `persisted`, not `now`: the merge refuses a bucket whose stamp is older
	// than the reading already standing in that window, so a probe holding the
	// gather's pre-request `now` can succeed having changed nothing a debt-holder
	// cares about. Reporting what the cache HOLDS lets the caller decide whether
	// this covers the run it owes, instead of inferring it from "the write
	// succeeded".
	return admitted, issued, true, persisted, nil
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
// Returns whether the caller must re-read the cache before shaping the metrics.
// That is USUALLY "this probe refreshed it", but it also covers the case where
// no request went out because another writer on this machine had already
// recorded a reading newer than the `latest` the caller passed in.
func refreshClaudeUsageIfStale(ctx context.Context, now, latest time.Time, accessToken, fingerprint string) bool {
	// The generation is sampled HERE, which is only sound for a caller that read
	// the cache immediately before calling. A caller that does any work between its
	// cache load and this call must sample its own and use
	// refreshClaudeUsageIfStaleFrom, or a refresh landing in that gap is invisible
	// to the seed.
	return refreshClaudeUsageIfStaleFrom(ctx, claudeUsageProbe.refreshGeneration(), now, latest, accessToken, fingerprint)
}

// refreshClaudeUsageIfStaleFrom is refreshClaudeUsageIfStale taking the refresh
// generation the caller sampled BEFORE it loaded `latest`, so the seed can tell
// that the startup replay moved the shared cache in the gap between that load and
// this call. Identical in every other respect.
func refreshClaudeUsageIfStaleFrom(ctx context.Context, generation uint64, now, latest time.Time, accessToken, fingerprint string) bool {
	// CLAIM the bypass off THIS gather's context rather than a process-global
	// flag: this function is in the common parser path, so a routine machine-info
	// gather overlapping a refresh would otherwise spend the refresh's bypass and
	// leave it to sign the cache it loaded before the probe landed. The ticket is
	// on the refresh's own context, so only the requested gather can take it.
	forced := claimClaudeUsageForceProbe(ctx)
	// Adopt the previous process's debt — and any 429 hold it left — before
	// reading ours, so a run that finished just before a restart or self-update is
	// not mistaken for "no debt" on this process's very first gather, and so this
	// gather cannot be admitted inside a hold the startup replay has not yet got
	// round to restoring. Latched, and it reuses the fingerprint the caller
	// already decoded — no extra credential read.
	//
	// A non-zero answer means the shared cache moved past the `latest` this gather
	// loaded (the startup replay landing mid-gather): there is nothing left to
	// probe for, but the caller must re-read the cache rather than publish the
	// view it holds — the same "the shared cache moved" contract as the final
	// return.
	//
	// A FORCED refresh is not short-circuited by it: somebody is looking at the
	// card and asked for a reading now, and a cache that moved while we adopted is
	// not that reading. The verdict is carried instead, and answers the forced
	// gather only if its own attempts produce nothing — re-reading a cache that
	// advanced still beats publishing the view it loaded before.
	seeded := claudeUsageProbe.seedOwedFromCache(ctx, fingerprint, generation, now, latest)
	if !seeded.IsZero() && !forced {
		return true
	}
	// An outstanding post-run debt overrides the staleness TTL: a reading taken
	// BEFORE the run is not "fresh enough" just because it is recent, and this is
	// the backstop for a trailing probe that was too far out to schedule or that
	// failed.
	owed := claudeUsageProbe.owedObservation()
	owing := !owed.IsZero() && (latest.IsZero() || !latest.After(owed))
	if !forced && !owing && !latest.IsZero() && now.Sub(latest) < claudeUsageProbeStaleAfter {
		// Fresh by the caller's view — unless the startup replay landed after
		// the seed returned and settled the debt this gather would otherwise
		// have probed for. See refreshLandedSince.
		return claudeUsageProbe.refreshLandedSince(generation, fingerprint, latest)
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
	identity := claudeUsageProbeIdentity{token: accessToken, fingerprint: fingerprint}
	resolveIdentity := func() claudeUsageProbeIdentity { return identity }
	// A forced refresh joins a probe that already holds the single-flight slot
	// rather than being turned away by it — see joinInFlight. Done BEFORE our own
	// attempt so the answer already on the wire is the one the user gets; if it
	// wrote nothing we fall through and issue the request ourselves, the slot now
	// being free and the bypass ours to spend.
	//
	// Two passes, because "in flight" is not a state we sampled once. The first
	// join covers a probe already on the wire when the refresh arrived; the
	// second covers one that claimed the slot in the gap between that join and
	// our own begin(), which would otherwise refuse us and leave the refresh
	// reporting the pre-probe cache. When that second joined probe produces no
	// usable reading, the refresh gets one final attempt of its own after the slot
	// is released. Bounded at two joins and two attempts — a third join would be an
	// unbounded wait on a queue this process does not control — and each join is
	// itself bounded by the gather context and claudeUsageProbeJoinTimeout.
	if forced {
		for pass := 0; pass < 2; pass++ {
			// A joined reading answers the refresh only when it is for THIS account
			// (enforced inside joinInFlight) and at least as new as what we owe. A
			// probe that started BEFORE the run persists a PRE-run observation:
			// accepting it would settle the run's debt with a timestamp that
			// predates the run, sign that timestamp into the receipt, and leave the
			// already-scheduled trailing probe to exit finding nothing owed.
			joined, joinedAt := claudeUsageProbe.joinInFlight(ctx, fingerprint)
			if joined && !joinedAt.IsZero() && (!owing || claudeUsageObservationCovers(joinedAt, owed)) {
				if owing {
					claudeUsageProbe.settleOwed(owed)
				}
				// The bypass was claimed above, so nothing is left pending for a
				// later routine gather to inherit.
				return true
			}
			if pass > 0 && !joined {
				// Our first attempt was admitted and failed rather than losing the
				// slot to another probe. Do not turn one provider failure into an
				// immediate duplicate request merely because this is forced.
				break
			}
			claudeUsageProbeBeforeForcedAttempt(pass)
			refreshed, observedAt, probeErr := probeClaudeUsage(ctx, now, resolveIdentity, baseline, true)
			logClaudeUsageProbeFailure(probeErr)
			if refreshed {
				// Same settle rule as the routine path below.
				if owing && claudeUsageObservationCovers(observedAt, owed) {
					claudeUsageProbe.settleOwed(owed)
				}
				return true
			}
			// Refused or failed. If the slot was taken while we were asking, the
			// next pass joins that probe instead of reporting a stale reading. If
			// that second join is unusable, pass two makes one final request after
			// the joined holder has released the slot.
		}
		// Nothing of ours landed. The seed's verdict is the last thing left: when
		// the shared cache moved past `latest` while we adopted, the caller must
		// still re-read rather than publish the view it holds.
		return !seeded.IsZero()
	}
	admitted, _, refreshed, observedAt, probeErr := probeClaudeUsageAdmitted(ctx, now, resolveIdentity, baseline, false)
	logClaudeUsageProbeFailure(probeErr)
	// Refused while OWING: the slot is most likely held by the probe paying that
	// very debt — the startup replay seeded it, or this process's trailing probe
	// after a run. A routine gather ordinarily gains nothing by blocking, but
	// here it would otherwise publish the pre-run view for the whole TTL while
	// the covering answer lands a moment later. Join it, bounded by the gather's
	// context and claudeUsageProbeJoinTimeout, and accept only a reading for
	// this account that covers the debt — the forced path's rule.
	if !admitted && !refreshed && owing {
		if joined, joinedAt := claudeUsageProbe.joinInFlight(ctx, fingerprint); joined && claudeUsageObservationCovers(joinedAt, owed) {
			claudeUsageProbe.settleOwed(owed)
			return true
		}
	}
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
	if refreshed {
		return true
	}
	// No request went out, but the cross-process dedupe may have suppressed it
	// precisely BECAUSE another writer — a second agent channel, the status-line
	// hook — recorded a reading after the caller loaded `latest`. That reading is
	// on disk now and the debt it covers has just been settled, so the trailing
	// retry will not come back for it: reporting "not refreshed" here would
	// publish the pre-write metrics and leave the backend stale until some later
	// poll. Both instants come from claudeSnapshotFreshness, so a strictly newer
	// one means the DISPLAYED snapshot advanced and re-reading is worth a load;
	// an equal one is the same reading the caller already holds.
	//
	// And a refusal or skip may have raced the startup replay landing after the
	// seed returned; that reading is on disk too — see refreshLandedSince.
	return (!observedAt.IsZero() && observedAt.After(latest)) ||
		claudeUsageProbe.refreshLandedSince(generation, fingerprint, latest)
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
	// The debt is recorded HERE, synchronously, before the goroutine exists:
	// the caller's exit path (and the test harness that waits for it to
	// finish) must be able to rely on "the trigger has fired" meaning "the
	// debt is on the gate" — a debt recorded a scheduler tick later inside
	// the goroutine landed in the NEXT test's settled-turn hold window on the
	// CI runners. claudeUsageProbeAfterRun records it again for its own
	// callers; recordOwed keeps the newest baseline, so the repeat is a no-op.
	claudeUsageProbe.recordOwed(completedAt)
	// Counted on the CALLER's goroutine, before the spawn — not left to
	// claudeUsageProbePayRecordedRun's own beginSettling below. The durable
	// mirror runs FIRST inside that goroutine, so a reset that sampled
	// `settling` in the gap between the spawn and that call would see zero,
	// declare the drain complete, and return while a cache write is still on
	// its way to disk: in a test that is the next case's cache being written by
	// the previous case's run, which is exactly the cross-test escape the drain
	// exists to close. settling is a counter, so the nested beginSettling
	// inside the settlement is harmless.
	claudeUsageProbe.beginSettling()
	go func() {
		defer claudeUsageProbe.endSettling()
		defer func() { _ = recover() }()
		// Mirror the debt to disk BEFORE settling it, from this goroutine: the
		// write takes the cache gate and flock, which must never be waited on
		// from the stdout path or under the gate mutex. Persisting it is what
		// lets a self-update, crash or restart between here and the trailing
		// probe still be paid — see cliagent_usage_claudecode_freshness.go.
		claudeOweRunRefresh(completedAt)
		// Settle only: the debt above is the one record for this run.
		claudeUsageProbePayRecordedRun(completedAt)
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
	claudeUsageProbePayRecordedRun(completedAt)
}

// claudeUsageProbePayRecordedRun is the settlement half of
// claudeUsageProbeAfterRun for a debt the caller ALREADY recorded — the
// session trigger records synchronously before spawning this on a goroutine.
// It must not record again: a gather or another cache writer may have observed
// and settled that very baseline in the gap before the goroutine ran, and
// recordOwed has no settled watermark, so a repeat would resurrect a paid debt
// and schedule a trailing OAuth request for nothing.
func claudeUsageProbePayRecordedRun(completedAt time.Time) {
	// Counted for the whole settlement, not just the sleep: an attempt already on
	// the wire holds no trailing reservation but still writes the cache, and a
	// reset that returned ahead of it would hand the next caller a gate a
	// departing settlement is about to mutate.
	claudeUsageProbe.beginSettling()
	defer claudeUsageProbe.endSettling()

	for attempt := 0; attempt < claudeUsageProbeAfterRunMaxAttempts; attempt++ {
		// No credential or cache read in this preflight. Resolving the identity
		// here would cost a `security` spawn PER RUN on macOS even when the burst
		// coalesces to one request; the single probe below resolves it once and
		// uses it for both the shared-cache check and the HTTP call.
		if !claudeUsageProbeAwaitEligible() {
			return
		}

		// Satisfy the NEWEST outstanding debt, which may have advanced past this
		// run while we slept.
		baseline := claudeUsageProbe.owedObservation()
		if baseline.IsZero() {
			return // already settled by another writer, a gather, or the probe that beat us
		}

		if claudeUsageProbeAttempt(baseline) {
			return
		}
		// Refused by the single-flight latch: eligibility said "now", but another
		// probe took the slot between the check and the call. That probe answers
		// for ITS baseline, which can predate this run, so returning here is how a
		// finished run's refresh goes missing until some later gather happens to
		// retry it. Take our turn behind the holder instead, then re-derive
		// eligibility — the debt is re-read next round, so a holder whose reading
		// did cover this run ends the loop without a second request.
		if !claudeUsageProbe.awaitInFlight() {
			return
		}
	}
}

// claudeUsageProbeAwaitEligible blocks until the gate would admit a probe,
// reporting false when it never will, when the wait is too long to hold a timer
// for, when another run already owns the trailing timer, or when the process is
// shutting down.
//
// Waiting rather than dropping is the point: the minimum interval is checked in
// begin() before any baseline is consulted, so a routine gather (or another run)
// that probed thirty seconds ago would otherwise make this run's refresh vanish
// — and the gather path then treats that pre-run reading as fresh for the whole
// staleness TTL, so latestObservedAt stays behind the run for minutes. The
// throttle exists to bound the request RATE, not to decide which runs get
// reported; delaying to the end of the window satisfies both.
func claudeUsageProbeAwaitEligible() bool {
	eligibleAt, ever := claudeUsageProbe.nextEligible(time.Now())
	if !ever {
		return false
	}
	wait := time.Until(eligibleAt)
	if wait <= 0 {
		return true
	}
	if wait > claudeUsageProbeMaxTrailingWait {
		// Too far out to hold a timer for — a deep failure backoff or a long
		// Retry-After, both states where we should not be pressing anyway. The
		// debt stays recorded and refreshClaudeUsageIfStale pays it on the next
		// gather. Deliberately does NOT reserve the timer slot: reserving one
		// without creating a timer would leave it held forever.
		return false
	}
	if !claudeUsageProbe.reserveTrailing() {
		return false // another run already owns the trailing timer
	}
	// Release on EVERY path out, including the cancel and shutdown branches.
	defer claudeUsageProbe.releaseTrailing()

	select {
	case <-time.After(wait + claudeUsageProbeTrailingSlack):
		return true
	case <-shutdownChan:
		return false
	case <-claudeUsageProbe.trailingCancelCh():
		// The gate was reset under us; the environment this probe would read on
		// waking is no longer the one it was scheduled against.
		return false
	}
}

// claudeUsageProbeAttempt issues one trailing probe for an outstanding debt and
// reports whether the caller is DONE — either the debt is settled, or the gate
// admitted the attempt and it skipped/failed, which leaves the debt standing for
// the next gather or run under the existing bounds. False means the attempt was
// never admitted, so the debt has not had its turn yet.
func claudeUsageProbeAttempt(baseline time.Time) bool {
	done, _ := claudeUsageProbeAttemptIssued(baseline)
	return done
}

// claudeUsageProbeAttemptIssued is claudeUsageProbeAttempt reporting, in
// addition, whether the admitted attempt actually spent its turn — asked the
// endpoint, or inherited another writer's covering reading. False means it was
// admitted but returned before doing either (an unreadable credential store), so
// a caller that charged for the turn bought nothing and must refund it.
func claudeUsageProbeAttemptIssued(baseline time.Time) (done, issued bool) {
	// The WHOLE-probe bound, not the request bound. This context is a root the
	// trailing probe fabricates for itself — there is no gather deadline above it
	// to respect — and probeClaudeUsage derives BOTH its request timeout and its
	// persist deadline from it. Sized at claudeUsageProbeTimeout, a response that
	// used a real part of the request budget would leave the verified merge only
	// the remainder of it, so ordinary cache-lock contention would abandon a
	// persist that was going to succeed: the attempt reports refreshed == false,
	// the run's debt and the failure backoff both stay outstanding, and the
	// reading we already paid a request for goes unclaimed. Request timeout plus
	// the merge's enforced persist budget gives each phase its own room.
	ctx, cancel := context.WithTimeout(context.Background(), claudeUsageProbeWholeTimeout)
	defer cancel()

	// forced=false: this is the background trailing probe. It must never spend a
	// bypass a concurrent refresh is holding — that theft is what leaves the
	// refresh signing a pre-probe cache.
	admitted, issued, refreshed, observedAt, probeErr := probeClaudeUsageAdmitted(ctx, time.Now(), claudeUsageProbeStoredIdentity, baseline, false)
	logClaudeUsageProbeFailure(probeErr)
	// Same rule as the gather path: a write that left the run's window owned by
	// an older reading has not paid this debt, even though it succeeded.
	if refreshed && claudeUsageObservationCovers(observedAt, baseline) {
		claudeUsageProbe.settleOwed(baseline)
		return true, issued
	}
	// A refusal or failure deliberately leaves the debt standing: the next
	// gather, reconnect, or run retries it under the same bounds. Whether this
	// was OUR attempt comes from begin() itself — never from comparing instants,
	// which collide on Windows' coarse clock (see probeClaudeUsageAdmitted).
	return admitted, issued
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
