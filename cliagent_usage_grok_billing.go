// cliagent_usage_grok_billing.go — reads Grok Build's own billing telemetry out
// of the CLI's local log.
//
// Grok exposes no quota file, but the CLI logs the credits config it fetches for
// itself to `$GROK_HOME/logs/unified.jsonl` (default `~/.grok/logs`), one JSON
// object per line:
//
//	{"ts":"2026-08-10T17:08:10.511Z","msg":"billing: fetched credits config",
//	 "ctx":{"config":{"creditUsagePercent":52.0,
//	   "currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"…","end":"…"},
//	   "onDemandCap":{"val":0},"onDemandUsed":{"val":0},"prepaidBalance":{"val":0}},
//	  "subscriptionTier":"SuperGrok"}}
//
// We only READ what the CLI already wrote — no request is made to xAI. `ts` is
// the provider observation time (the instant Grok fetched the figure), which is
// why it maps onto ObservedAt rather than CollectedAt: a later gather that finds
// no new line must not make an old percentage look current. For Grok 1.0's
// provider-confirmed unmetered records the same timestamp proves that billing
// was freshly checked even though no numeric percentage was exposed.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// grokBillingLogTailBytes bounds how much of the log we read. unified.jsonl is
// append-only and grows past 5 MB on an active machine; the newest billing line
// is near the end, so tailing keeps a 6-hour machine-info gather cheap. A line
// older than the tail window is indistinguishable from "never logged" — that is
// the correct downgrade, since such a reading would be far too old to plot.
const grokBillingLogTailBytes = 1 << 20 // 1 MiB

// grokBillingMaxClockSkew is the largest future offset trusted as a provider
// observation. A record beyond this bound is authoritative enough to block an
// older record, but none of its timestamped values are safe to publish.
const grokBillingMaxClockSkew = 5 * time.Minute

// grokBillingLogMessage is the log `msg` that carries the credits config. Grok
// writes it when the CLI fetches billing (session start / periodic refresh).
const grokBillingLogMessage = "billing: fetched credits config"

// grokBillingObservationTTL bounds how long a CONFIRMED-UNMETERED observation
// stays trustworthy. Grok 1.0 publishes no percentage, so the record timestamp
// is the only thing separating "the provider confirmed this period a moment
// ago" from "we have never read a usable record" — and a timestamp that never
// ages would present a week-old confirmation as current. Three days spans a
// long weekend of not running Grok while staying well inside a weekly window.
//
// Deliberately NOT grokLimitNoticeTTL: that one bounds the discrete limit
// banner, this one bounds capacity freshness. Sharing a value would make a
// banner tuning change silently retune what the capacity bar claims to know.
const grokBillingObservationTTL = 72 * time.Hour

// grokBillingLogMessages is the CLOSED set of `msg` values accepted as a
// credits-config record. Matching is EXACT — never a `billing:` prefix or
// substring: the reader fails closed on a newest record it cannot decode, so
// letting an unrelated high-frequency `billing:` line become the newest
// "billing record" would permanently block a good older percentage. The extra
// spellings tolerate a Grok release that renames the message without changing
// its payload; anything outside this set degrades to unobservable.
var grokBillingLogMessages = []string{
	grokBillingLogMessage,
	"billing: fetched credit config",
	"billing: fetched credits",
	"billing: credits config fetched",
}

// grokBillingMessageRecognized reports whether msg is one of the allowlisted
// credits-config messages.
func grokBillingMessageRecognized(msg string) bool {
	for _, candidate := range grokBillingLogMessages {
		if msg == candidate {
			return true
		}
	}
	return false
}

// grokLineMentionsBillingMessage is the cheap pre-filter before the JSON
// decode: the vast majority of lines in this log are chat/tool telemetry, and
// unmarshalling every one of them would dominate the gather. A hit here is only
// a candidate — grokBillingMessageRecognized still checks the decoded `msg`.
func grokLineMentionsBillingMessage(line []byte) bool {
	for _, candidate := range grokBillingLogMessages {
		if bytes.Contains(line, []byte(candidate)) {
			return true
		}
	}
	return false
}

// grokBillingRecord mirrors the fields we consume from that line. Everything is
// optional: an older CLI, a different plan shape, or a partially written line
// must degrade to "unobservable", never to a wrong number.
type grokBillingRecord struct {
	TS  string `json:"ts"`
	Ctx struct {
		Config grokBillingConfig `json:"config"`
		// Both spellings of the tier are decoded for the same reason the config
		// fields below are: the allowlist stays closed, but a Grok release that
		// switches the envelope to snake_case must not blank the plan.
		SubscriptionTier      string `json:"subscriptionTier"`
		SubscriptionTierSnake string `json:"subscription_tier"`
	} `json:"ctx"`
}

// grokBillingPeriodFields is the billing window as the record states it.
type grokBillingPeriodFields struct {
	Type string `json:"type"`
	End  string `json:"end"`
}

// grokBillingAmount is one credit pool leaf (`{"val": <number>}`).
type grokBillingAmount struct {
	Val grokBillingNumber `json:"val"`
}

// grokBillingConfig decodes the credits config under both the camelCase keys
// current Grok writes and their snake_case aliases. The allowlist is still
// CLOSED — every accepted key is named here, nothing is scraped — but a field
// rename across a CLI update degrades gracefully instead of silently zeroing
// the only source of Grok capacity we have. camelCase wins any disagreement:
// it is the shape observed on real machines.
type grokBillingConfig struct {
	CreditUsagePercent      grokBillingNumber       `json:"creditUsagePercent"`
	CreditUsagePercentSnake grokBillingNumber       `json:"credit_usage_percent"`
	CurrentPeriod           grokBillingPeriodFields `json:"currentPeriod"`
	CurrentPeriodSnake      grokBillingPeriodFields `json:"current_period"`
	OnDemandCap             grokBillingAmount       `json:"onDemandCap"`
	OnDemandCapSnake        grokBillingAmount       `json:"on_demand_cap"`
	OnDemandUsed            grokBillingAmount       `json:"onDemandUsed"`
	OnDemandUsedSnake       grokBillingAmount       `json:"on_demand_used"`
}

func (c grokBillingConfig) creditUsagePercent() grokBillingNumber {
	if c.CreditUsagePercent.Valid {
		return c.CreditUsagePercent
	}
	return c.CreditUsagePercentSnake
}

func (c grokBillingConfig) currentPeriod() grokBillingPeriodFields {
	period := c.CurrentPeriod
	if strings.TrimSpace(period.Type) == "" {
		period.Type = c.CurrentPeriodSnake.Type
	}
	if strings.TrimSpace(period.End) == "" {
		period.End = c.CurrentPeriodSnake.End
	}
	return period
}

func (c grokBillingConfig) onDemandCap() grokBillingNumber {
	if c.OnDemandCap.Val.Valid {
		return c.OnDemandCap.Val
	}
	return c.OnDemandCapSnake.Val
}

func (c grokBillingConfig) onDemandUsed() grokBillingNumber {
	if c.OnDemandUsed.Val.Valid {
		return c.OnDemandUsed.Val
	}
	return c.OnDemandUsedSnake.Val
}

// grokBillingNumber decodes one allowlisted numeric leaf without rejecting the
// entire billing record when that leaf is null, the wrong JSON type, or outside
// float64's finite range. The record remains authoritative and blocks older
// data; only the invalid field is omitted from its normalized snapshot.
type grokBillingNumber struct {
	Value float64
	Valid bool
}

// grokLogIdentityRecord is the complete allowlist for account evidence in a
// unified-log line. Known Grok versions put the identity in ctx; top-level
// fields are retained for older compatible envelopes. A typed envelope avoids
// treating credentials, prompts, or tool results that merely contain a
// user_id-looking string as proof of who fetched a billing record.
type grokLogIdentityRecord struct {
	UserID      *string `json:"user_id"`
	UserIDCamel *string `json:"userId"`
	Ctx         struct {
		UserID      *string `json:"user_id"`
		UserIDCamel *string `json:"userId"`
	} `json:"ctx"`
}

func (n *grokBillingNumber) UnmarshalJSON(data []byte) error {
	n.Value = 0
	n.Valid = false
	value, err := strconv.ParseFloat(string(bytes.TrimSpace(data)), 64)
	if err != nil || !grokFiniteNumber(value) {
		return nil
	}
	n.Value = value
	n.Valid = true
	return nil
}

// grokBillingSnapshot is the normalized view the parser plots.
type grokBillingSnapshot struct {
	UsedPercent float64
	// HasUsedPercent is false when the record named a billing period but no
	// usage figure — the shape Grok 1.0 emits. UsedPercent is meaningless then
	// and must never be plotted; the period itself is still authoritative.
	HasUsedPercent   bool
	ObservedAt       time.Time
	PeriodType       string
	PeriodEnd        time.Time
	HasPeriodEnd     bool
	OnDemandCap      float64
	OnDemandUsed     float64
	HasOnDemand      bool
	HasOnDemandUsed  bool
	SubscriptionTier string
}

// grokBillingLogPath resolves the CLI's unified log inside an already-resolved
// Grok home (the same `$GROK_HOME`/`~/.grok` precedence the parser applies).
func grokBillingLogPath(base string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(base, "logs", "unified.jsonl")
}

// grokPersistentHome resolves the provider-owned home used by direct runs. ACP
// sessions capture this path before spawning so a later `grok login` can change
// the account in that home without changing where managed evidence is merged.
func grokPersistentHome() string {
	base := os.Getenv("GROK_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".grok")
		}
	}
	if base == "" {
		return ""
	}
	if absolute, err := filepath.Abs(base); err == nil {
		return absolute
	}
	return base
}

// grokDirectChildHomeOverride returns the absolute GROK_HOME a DIRECT Grok
// child must be given, or "" when the inherited environment already resolves to
// the same place on both sides.
//
// A relative GROK_HOME is resolved against the process cwd. StartSession sets
// the child's cwd to the caller's requested directory, while grokPersistentHome
// — the path the attribution marker is written to and the billing reader reads
// back — absolutizes against the DAEMON's cwd. Left alone, the CLI would log
// into a different tree than the one carrying our identity line, and the direct
// record would be unattributable no matter how correctly it was seeded.
//
// Absolute and unset values already resolve identically for parent and child,
// so only the relative case is rewritten: this never introduces a GROK_HOME the
// operator did not set.
func grokDirectChildHomeOverride(env string) string {
	if env == "" || filepath.IsAbs(env) {
		return ""
	}
	return grokPersistentHome()
}

const grokManagedBillingIdentityMessage = "aiexpedite: managed billing producer"

// grokContestedBillingIdentity is the producer identity written when the log
// cannot honestly name ONE account: two live direct (PTY) runs were started
// under different accounts, so a record appended next could have been written
// by either of them.
//
// unified.jsonl is a single shared file and a record binds to the NEAREST
// identity above it, so there is no marker that is correct for both runs.
// Naming either one publishes the other run's utilization as that account's —
// a billing lie. Leaving whichever marker happens to be newest standing is the
// same failure with an arbitrary victim. This value can never equal a resolved
// account (grokRecordBelongsToCurrentAccount also refuses it explicitly), so
// every record written while the runs overlap is UNATTRIBUTABLE: the card falls
// back to "unobservable", which is recoverable, and the next assertion after
// the conflicting run releases repairs attribution for the survivor.
const grokContestedBillingIdentity = "aiexpedite:contested"

// grokResolvedBillingIdentity returns the single account value written as the
// producer identity for a home. Prefer the last candidate (normally the opaque
// JWT subject) over an email while retaining compatibility with older auth
// layouts. Shared by every writer so the seed, the direct-run append, and the
// managed persist can never disagree about who produced a record.
func grokResolvedBillingIdentity(base string) (string, bool) {
	identities := grokIdentityCandidates(base)
	if len(identities) == 0 {
		return "", false
	}
	identity := strings.TrimSpace(identities[len(identities)-1])
	if identity == "" {
		return "", false
	}
	return identity, true
}

// grokBillingIdentityLine renders the one allowlisted producer-identity line.
// Only the account value crosses into a log — never a token, config, or prompt.
func grokBillingIdentityLine(identity string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"msg": grokManagedBillingIdentityMessage,
		"ctx": map[string]any{"user_id": identity},
	})
}

// seedGrokManagedBillingIdentity records the account copied into an isolated
// ACP home before the child starts. The isolated log is private to that one
// process, so this marker cannot be displaced by a concurrent `grok login` in
// the real home. The write TRUNCATES on purpose: the isolated log must start
// clean so the only records in it are this session's.
func seedGrokManagedBillingIdentity(isolatedHome string) error {
	path := grokBillingLogPath(isolatedHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	identity, ok := grokResolvedBillingIdentity(isolatedHome)
	if !ok {
		return nil
	}
	line, err := grokBillingIdentityLine(identity)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(line, '\n'), 0o600)
}

// appendGrokBillingIdentity names the signed-in account in the PROVIDER-owned
// log so the records a direct (PTY) Grok run writes afterwards are attributable.
//
// grokRecordBelongsToCurrentAccount refuses any record with no producer
// identity logged before it, and the Grok CLI itself logs none — so without
// this line a direct run can never yield an observable metric, however many
// billing records it writes.
//
// One O_APPEND write of one complete line, matching
// persistGrokManagedBillingSnapshot's atomicity contract: it cannot interleave
// with a concurrent managed persist or with Grok's own writer and split a
// record. Callers append only at session start, under that session's
// credentials, so the line is never a claim about an account we are not
// currently signed in as.
func appendGrokBillingIdentity(base string) error {
	identity, ok := grokResolvedBillingIdentity(base)
	if !ok {
		return nil
	}
	return appendGrokBillingIdentityValue(base, identity)
}

// appendGrokBillingIdentityValue writes a CAPTURED account rather than the one
// the credentials resolve to now. Re-assertions during a live run must name the
// account the running CLI was spawned under — see ensureGrokBillingIdentityNamed.
func appendGrokBillingIdentityValue(base, identity string) error {
	path := grokBillingLogPath(base)
	if path == "" {
		return nil
	}
	if strings.TrimSpace(identity) == "" {
		return nil
	}
	line, err := grokBillingIdentityLine(identity)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// grokBillingIdentityIsNewest reports whether `identity` is the NEWEST valid
// producer identity in the bounded log tail. Grok rotates unified.jsonl, which
// discards our line; this is how the direct-run guard notices the rotation and
// re-appends instead of leaving every later record unattributable.
//
// It stops at the first identity-shaped line it finds rather than searching the
// whole tail for a match, because grokRecordBelongsToCurrentAccount binds a record to
// the NEAREST identity preceding it. A newer conflicting marker — e.g. a
// managed session for account B persisting its identity after the user switched
// back to account A — would otherwise leave the older A marker "still logged",
// suppress the re-append, and bind every later direct-A record to B until the
// stale marker aged out of the tail.
func grokBillingIdentityIsNewest(base, identity string) bool {
	wanted := strings.TrimSpace(identity)
	if wanted == "" {
		return false
	}
	lines, ok := readGrokBillingLogTail(base)
	if !ok {
		return false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		logged, found, valid := grokLogIdentity(lines[i])
		if !found {
			continue
		}
		if !valid {
			// A newer identity-shaped line we cannot decode (malformed, or a
			// partially written one). grokRecordBelongsToCurrentAccount stops
			// and REFUSES on exactly this line rather than falling back to
			// older evidence, so treating it as "not ours" and re-appending is
			// what keeps the following direct records observable. Skipping it
			// would let an older matching marker read as newest and suppress
			// the corrective append.
			return false
		}
		return strings.EqualFold(logged, wanted)
	}
	return false
}

// persistGrokManagedBillingSnapshot copies one session's newest verified
// billing observation into the provider-owned log after the managed process
// exits. The source is bound to the auth copy frozen at session start. The
// destination lines — our identity, our record, and (when a direct run is armed)
// the direct account's identity again — are written in one O_APPEND call so a
// direct Grok process cannot interleave a record between them.
//
// Only normalized allowlisted fields are persisted. Prompts, credentials, raw
// config, tool results, and unrelated log fields never leave the isolated home.
func persistGrokManagedBillingSnapshot(isolatedHome, persistentHome string) (grokManagedBillingOutcome, error) {
	if isolatedHome == "" || persistentHome == "" {
		return grokManagedBillingNotApplicable, nil
	}
	identities := grokIdentityCandidates(isolatedHome)
	if len(identities) == 0 {
		return grokManagedBillingNoIdentity, nil
	}
	snap, ok := readGrokBillingSnapshot(isolatedHome, identities)
	if !ok {
		return grokManagedBillingNoRecord, nil
	}
	identity := strings.TrimSpace(identities[len(identities)-1])
	if identity == "" {
		return grokManagedBillingNoIdentity, nil
	}

	period := map[string]any{"type": snap.PeriodType}
	if snap.HasPeriodEnd {
		period["end"] = snap.PeriodEnd.UTC().Format(time.RFC3339Nano)
	}
	config := map[string]any{"currentPeriod": period}
	if snap.HasUsedPercent {
		config["creditUsagePercent"] = snap.UsedPercent
	}
	if snap.HasOnDemand {
		config["onDemandCap"] = map[string]any{"val": snap.OnDemandCap}
		if snap.HasOnDemandUsed {
			config["onDemandUsed"] = map[string]any{"val": snap.OnDemandUsed}
		}
	}
	identityLine, err := grokBillingIdentityLine(identity)
	if err != nil {
		return grokManagedBillingFailed, err
	}
	billingLine, err := json.Marshal(map[string]any{
		"ts":  snap.ObservedAt.UTC().Format(time.RFC3339Nano),
		"msg": grokBillingLogMessage,
		"ctx": map[string]any{
			"config":           config,
			"subscriptionTier": snap.SubscriptionTier,
		},
	})
	if err != nil {
		return grokManagedBillingFailed, err
	}
	payload := make([]byte, 0, len(identityLine)+len(billingLine)+2)
	payload = append(payload, identityLine...)
	payload = append(payload, '\n')
	payload = append(payload, billingLine...)
	payload = append(payload, '\n')

	superseded, repairAfterWrite, err := appendGrokBillingPair(persistentHome, payload, identity, identities, snap.ObservedAt)
	if err != nil {
		return grokManagedBillingFailed, err
	}
	if superseded {
		return grokManagedBillingSuperseded, nil
	}

	if repairAfterWrite {
		// Outside the write lock — the re-assertion takes it itself. It names
		// the armed run's CAPTURED account, never the live credentials, so a
		// concurrent login in the shared home cannot turn the repair into a
		// misattribution.
		reassertGrokDirectAttribution()
	}
	return grokManagedBillingPersisted, nil
}

// appendGrokBillingPair appends the managed identity/record payload — plus, when
// a direct run is armed, that run's identity again — in one O_APPEND call.
//
// Our identity line becomes the newest one in the log, and a record binds to the
// NEAREST identity preceding it, so from this write on every record a still-live
// DIRECT run writes would bind to the managed account and be refused. Repairing
// that with a SECOND append is too late: the lock serializes this process's
// helpers, not the direct child, which can land its only billing record in the
// gap between the two writes. So the repair marker rides in the SAME atomic
// payload, after our record — our record still binds to the identity immediately
// above it, and the very next byte of the log already names the direct account.
//
// The armed CHECK and the payload it produces are taken UNDER the lock, not by
// the caller before it. A direct session arms before it calls
// ensureGrokBillingAttribution, and that call takes this same lock, so deciding
// here makes the two orderings the only ones possible: either we observe the arm
// and carry its marker, or the direct session has not yet reached the lock and
// its own append lands after ours. Checking outside left a third ordering — arm
// after our check, ensure before our append — in which the direct child ran under
// the managed identity until the next keeper tick.
//
// Skipped when the two accounts are the same (the pair already names the direct
// account) and when no direct run is armed, so a managed-only device writes
// nothing extra into a provider-owned file. Returns repairAfterWrite when the
// caller must fall back to a post-write ensureGrokBillingAttribution, which takes
// this lock itself and so cannot run while it is held.
//
// The whole merge is ABANDONED when the persistent log already holds a record
// for this same account that is at least as new as the one we carry. A managed
// session can sit open long after it fetched credits, and readGrokBillingSnapshot
// picks the LAST billing line by FILE ORDER, not by timestamp — so appending an
// older receipt last would replace a newer direct-run observation with a stale
// percentage until the next fetch. The comparison is taken under the same lock,
// immediately before the write, so no merge of ours can pass it and then be
// overtaken by another. An equal timestamp is treated as superseded too, which
// makes re-persisting one session's snapshot idempotent.
func appendGrokBillingPair(
	persistentHome string,
	payload []byte,
	identity string,
	identities []string,
	observedAt time.Time,
) (superseded bool, repairAfterWrite bool, err error) {
	grokBillingAttributionSerialize.Lock()
	defer grokBillingAttributionSerialize.Unlock()

	// Read with the MANAGED account's identities: a newest persistent record
	// belonging to anyone else (or one we cannot decode) is not evidence about
	// this account, and readGrokBillingSnapshot reports it as no snapshot, so
	// the merge proceeds.
	//
	// A record dated beyond grokBillingMaxClockSkew is skipped rather than
	// honored. grokBillingMetrics refuses to PUBLISH such a record, so letting
	// it supersede would leave managed usage Unknown until wall-clock caught up
	// with it — after a clock correction, potentially for the rest of the
	// period. The read path's distrust and this one have to agree: a timestamp
	// too far ahead to publish is too far ahead to protect.
	if existing, ok := readGrokBillingSnapshot(persistentHome, identities); ok &&
		!existing.ObservedAt.After(time.Now().Add(grokBillingMaxClockSkew)) &&
		!existing.ObservedAt.Before(observedAt) {
		return true, false, nil
	}

	directIdentity, armed := grokDirectAttributionAssertion()
	switch {
	case !armed:
		// No live direct run, so nothing extra is written into a
		// provider-owned file. When two runs disagree the assertion is the
		// contested sentinel, not silence: leaving OUR managed identity
		// standing as the newest marker would bind both of their records to
		// the managed account.
	case strings.EqualFold(directIdentity, identity):
		// Already named by the pair above.
	default:
		directLine, lineErr := grokBillingIdentityLine(directIdentity)
		if lineErr != nil {
			// Fall back to the post-write repair rather than dropping the
			// merge: a narrowed window beats an unattributed direct run.
			repairAfterWrite = true
			break
		}
		payload = append(payload, directLine...)
		payload = append(payload, '\n')
	}

	return false, repairAfterWrite, writeGrokBillingPayload(persistentHome, payload)
}

// writeGrokBillingPayload writes the caller's whole payload in one O_APPEND call.
// Callers must hold grokBillingAttributionSerialize.
func writeGrokBillingPayload(persistentHome string, payload []byte) error {
	path := grokBillingLogPath(persistentHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// grokManagedBillingOutcome names what a managed run left behind. A bare error
// made "the child never fetched credits" indistinguishable from "we merged a
// fresh observation", so a silent regression in the managed path looked exactly
// like success in the logs of both callers.
type grokManagedBillingOutcome int

const (
	// grokManagedBillingNotApplicable — the session ran no isolated home.
	grokManagedBillingNotApplicable grokManagedBillingOutcome = iota
	// grokManagedBillingNoIdentity — no account could be resolved from the
	// copied auth, so nothing may be attributed.
	grokManagedBillingNoIdentity
	// grokManagedBillingNoRecord — the child logged no usable billing record.
	// The persistent log keeps whatever it already had; a managed run that
	// never fetched credits must not downgrade a good existing reading.
	grokManagedBillingNoRecord
	// grokManagedBillingSuperseded — the persistent log already held a record
	// for this account at least as new as ours, so merging would have replaced
	// a newer observation with an older one. Nothing was written.
	grokManagedBillingSuperseded
	// grokManagedBillingPersisted — one normalized record was merged.
	grokManagedBillingPersisted
	// grokManagedBillingFailed — the merge was attempted and errored.
	grokManagedBillingFailed
)

func (o grokManagedBillingOutcome) String() string {
	switch o {
	case grokManagedBillingNotApplicable:
		return "not-applicable"
	case grokManagedBillingNoIdentity:
		return "no-identity"
	case grokManagedBillingNoRecord:
		return "no-record"
	case grokManagedBillingSuperseded:
		return "superseded"
	case grokManagedBillingPersisted:
		return "persisted"
	case grokManagedBillingFailed:
		return "failed"
	}
	return "unknown"
}

// readGrokBillingLogTail returns the bounded tail of the unified log split into
// lines, newest last. Shared by the billing scan and the direct-run attribution
// guard so both observe the same 1 MiB bound and the same failure modes.
func readGrokBillingLogTail(base string) ([][]byte, bool) {
	lines, _ := readGrokBillingLogTailWithOffset(base)
	return lines, lines != nil
}

// readGrokBillingLogTailWithOffset also reports whether the tail started at a
// non-zero offset, i.e. whether the FIRST line may legitimately be a truncated
// fragment of an older record rather than a partially written new one.
func readGrokBillingLogTailWithOffset(base string) (lines [][]byte, truncatedFirstLine bool) {
	path := grokBillingLogPath(base)
	if path == "" {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	offset := int64(0)
	if info.Size() > grokBillingLogTailBytes {
		offset = info.Size() - grokBillingLogTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false
	}
	// Read exactly the tail length captured by Stat. An unbounded ReadAll after
	// seeking can consume more than 1 MiB when Grok appends quickly enough to
	// keep extending the file, defeating this gather's memory/work bound and
	// mixing records from different filesystem snapshots.
	tail := make([]byte, info.Size()-offset)
	if _, err := io.ReadFull(f, tail); err != nil {
		// The log was truncated or replaced between Stat and ReadFull. Fail
		// closed and let the next bounded refresh gather a coherent snapshot.
		return nil, false
	}
	return bytes.Split(tail, []byte("\n")), offset > 0
}

// readGrokBillingSnapshot returns the NEWEST billing record in the log tail,
// provided it can be tied to the CURRENT credentials.
//
// Lines are scanned back-to-front and the first decoded billing record wins:
// the log is append-only, so the last such line is the most recent fetch. A
// truncated first line (the tail almost always starts mid-record) simply fails
// to unmarshal and is skipped like any other non-billing line.
//
// `identities` are the account values the current auth file resolves to (see
// grokIdentityCandidates). The log outlives a logout, so a record left by a
// previous account must not be attributed to the one signed in now — see
// grokRecordBelongsToCurrentAccount for how a record is tied to its producer.
func readGrokBillingSnapshot(base string, identities []string) (grokBillingSnapshot, bool) {
	lines, truncatedFirstLine := readGrokBillingLogTailWithOffset(base)
	if lines == nil {
		return grokBillingSnapshot{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !grokLineMentionsBillingMessage(line) {
			continue
		}
		// Decode the envelope first so an exact-message record with a malformed
		// allowlisted field remains authoritative. If full typed decoding then
		// fails, stop instead of reviving an older percentage.
		var envelope struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &envelope) != nil {
			// Only the first line of a seeked tail is expected to be truncated: the
			// 1 MiB offset normally lands in the middle of an older JSON object.
			// Any later candidate can be the newest billing append observed while it
			// was still being written. Fail closed so that transient partial record
			// cannot resurrect an older percentage.
			if i == 0 && truncatedFirstLine {
				continue
			}
			return grokBillingSnapshot{}, false
		}
		if !grokBillingMessageRecognized(envelope.Msg) {
			continue
		}
		var rec grokBillingRecord
		if json.Unmarshal(line, &rec) != nil {
			return grokBillingSnapshot{}, false
		}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok {
			// The newest exact-message record supersedes every older response,
			// even when one of its required fields is unusable. Continuing here
			// would resurrect a stale pre-upgrade percentage after a malformed or
			// timestamp-less current response.
			return grokBillingSnapshot{}, false
		}
		// Stop at the newest usable record either way: an older one is even less
		// likely to belong to the account signed in now.
		if !grokRecordBelongsToCurrentAccount(lines, i, identities) {
			return grokBillingSnapshot{}, false
		}
		return snap, true
	}
	return grokBillingSnapshot{}, false
}

// grokRecordBelongsToCurrentAccount reports whether the billing record at
// lineIdx was PRODUCED by the account the current credentials resolve to.
//
// Binding is per-record, not per-log: the identity that counts is the newest one
// logged before the record, because that is who the CLI was acting as when
// it fetched those credits. Comparing the newest identity in the whole tail
// instead would accept account B's older record as soon as account A logged in
// and had not fetched billing yet.
func grokRecordBelongsToCurrentAccount(lines [][]byte, lineIdx int, identities []string) bool {
	wanted := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if trimmed := strings.ToLower(strings.TrimSpace(identity)); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	// Start before the billing line. The observed billing envelope carries no
	// identity, so allowing a lookalike user_id on the record to authenticate
	// itself would defeat the cross-account boundary this scan enforces.
	for i := lineIdx - 1; i >= 0; i-- {
		identity, found, valid := grokLogIdentity(lines[i])
		if !found {
			continue
		}
		if !valid || len(wanted) == 0 {
			// The log names a producer but the credentials resolve to nothing we
			// can compare, or its newest identity envelope is malformed. Refuse
			// rather than falling back to older account evidence.
			return false
		}
		if strings.EqualFold(identity, grokContestedBillingIdentity) {
			// Overlapping direct runs on different accounts were live when this
			// marker was written, so nothing below it can be attributed to
			// either. Refuse explicitly rather than relying on the sentinel
			// merely failing to match a real account id.
			return false
		}
		return wanted[strings.ToLower(identity)]
	}
	// Nothing identifies the producer anywhere before the record. That is
	// not evidence it belongs to the current login: `unified.jsonl` is shared
	// across logins, so a CLI old enough never to log an identity leaves the
	// previous account's credits sitting in the same file for the next one to
	// publish as its own. Refuse — the card falls back to "unobservable", which
	// is what it showed before this source existed.
	return false
}

// grokLogIdentity returns the account in an allowlisted identity envelope.
// found distinguishes an invalid identity-shaped line from unrelated telemetry:
// the former must block older identity evidence, while the latter is skipped.
func grokLogIdentity(line []byte) (identity string, found bool, valid bool) {
	if !bytes.Contains(line, []byte(`"user_id"`)) &&
		!bytes.Contains(line, []byte(`"userId"`)) {
		return "", false, false
	}

	var record grokLogIdentityRecord
	if json.Unmarshal(line, &record) != nil {
		return "", true, false
	}

	for _, candidate := range []*string{
		record.Ctx.UserID,
		record.Ctx.UserIDCamel,
		record.UserID,
		record.UserIDCamel,
	} {
		if candidate == nil {
			continue
		}
		found = true
		value := strings.TrimSpace(*candidate)
		if value == "" || len(value) > 128 {
			return "", true, false
		}
		if identity != "" && !strings.EqualFold(identity, value) {
			return "", true, false
		}
		identity = value
	}
	if !found {
		// The key appeared only below a non-allowlisted object such as a prompt
		// or tool result; it is not account evidence.
		return "", false, false
	}
	return identity, true, true
}

// grokBillingSnapshotFromRecord validates and allowlists one record. A missing
// percentage is valid because Grok 1.0 uses that shape for an unmetered period;
// an observation time is still mandatory so the result can be distinguished
// from the parser's inferred placeholder.
func grokBillingSnapshotFromRecord(rec grokBillingRecord) (grokBillingSnapshot, bool) {
	observed, err := time.Parse(time.RFC3339, rec.TS)
	if err != nil {
		return grokBillingSnapshot{}, false
	}
	config := rec.Ctx.Config
	period := config.currentPeriod()
	snap := grokBillingSnapshot{
		ObservedAt:       observed,
		PeriodType:       period.Type,
		SubscriptionTier: firstNonEmpty(rec.Ctx.SubscriptionTier, rec.Ctx.SubscriptionTierSnake),
	}
	// Grok 1.0 stopped emitting creditUsagePercent. Every other field of the
	// record is unchanged, so the record is still the authoritative statement of
	// the CURRENT billing period — it simply no longer says how much of it is
	// used. Treat that as "usable record, unobserved usage" rather than "not a
	// record": rejecting it made the scanner walk further back and publish a
	// PRE-UPGRADE reading, under a period that had since ended.
	if pct := config.creditUsagePercent(); pct.Valid && grokFiniteNumber(pct.Value) {
		snap.UsedPercent = clampPercent(pct.Value)
		snap.HasUsedPercent = true
	}
	if end, err := time.Parse(time.RFC3339, period.End); err == nil {
		snap.PeriodEnd = end
		snap.HasPeriodEnd = true
	}
	// On-demand is a separate, opt-in pool: only plot it when a cap exists, or
	// the row would read as a hard 0-of-0 limit on every subscription account.
	if cap := config.onDemandCap(); cap.Valid && cap.Value > 0 && grokFiniteNumber(cap.Value) {
		snap.HasOnDemand = true
		snap.OnDemandCap = cap.Value
		// A cap with no reported usage is a pool we know exists but have not
		// observed. Leaving the zero default here would report it as completely
		// unused — an assertion the record never made.
		if used := config.onDemandUsed(); used.Valid &&
			grokFiniteNumber(used.Value) && used.Value >= 0 && used.Value <= cap.Value {
			snap.HasOnDemandUsed = true
			snap.OnDemandUsed = used.Value
		}
	}
	return snap, true
}

func grokFiniteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// grokBillingPeriodKind maps Grok's period enum onto our limit kinds. An
// unrecognized (or absent) period is deliberately NOT guessed: the frontend
// derives a metric's window length from the kind, so mislabeling a daily pool as
// weekly would keep a week-old reading on screen.
func grokBillingPeriodKind(periodType string) (kind string, label string, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(periodType)) {
	case "USAGE_PERIOD_TYPE_DAILY":
		return limitKindDaily, "Daily credits", true
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return limitKindWeekly, "Weekly credits", true
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return limitKindMonthly, "Monthly credits", true
	}
	return "", "", false
}

// grokBillingPeriodLength is the nominal length of a recognized billing window,
// used ONLY to bound how long an unmetered observation whose period end we could
// not parse may keep presenting itself as current. It is never used to derive a
// ResetAt: a nominal length is not an observed boundary. An unrecognized kind
// returns 0, which leaves grokBillingObservationTTL as the only bound.
func grokBillingPeriodLength(kind string) time.Duration {
	switch kind {
	case limitKindDaily:
		return 24 * time.Hour
	case limitKindWeekly:
		return 7 * 24 * time.Hour
	case limitKindMonthly:
		return 31 * 24 * time.Hour
	}
	return 0
}

// grokBillingMetrics turns a snapshot into the card's capacity rows.
//
// Three distinct states leave this function, and downstream freshness checks
// tell them apart with no wire-shape change:
//
//   - NUMERIC — total/consumed/remaining populated, ObservedAt = the record ts.
//   - CONFIRMED UNMETERED — Unknown WITH ObservedAt: proof that xAI's billing
//     endpoint was checked at that instant and reported a period carrying no
//     usage figure (the Grok 1.0 shape, and any period enum we do not
//     recognize). Renders as today's dashed Unknown bar plus "Last observed".
//   - INFERRED PLACEHOLDER — Unknown with NO ObservedAt: no usable record
//     exists, or the observation aged past grokBillingObservationTTL.
//
// A metered period whose end has passed is reported Unknown WITH its historical
// observation time rather than as 0% used. An ended unmetered period has no
// current confirmation and drops the timestamp. In both cases, assuming an
// empty pool could hide usage from another computer on the same account.
func grokBillingMetrics(snap grokBillingSnapshot, now time.Time) []cliAgentUsageMetric {
	// A future record remains authoritative and blocks older records, but no
	// value sharing its untrusted observation time may escape this gather. This
	// runs FIRST so no retention path below can publish a future ObservedAt.
	if snap.ObservedAt.After(now.Add(grokBillingMaxClockSkew)) {
		return []cliAgentUsageMetric{grokUnknownCreditsMetric()}
	}

	observedAt := snap.ObservedAt.UTC().Format(time.RFC3339)
	// Retention bound for an UNKNOWN row only. A numeric reading keeps its
	// existing lifecycle (bounded by the period end), so no shipped percentage
	// behaviour changes here.
	//
	// `window` narrows the global TTL when the record names a period we DO
	// recognize but gives us no parseable end: a daily window has certainly
	// rolled over long before 72h, and publishing its ObservedAt would let a
	// freshness check read a rolled-over period as current billing evidence.
	// A window of 0 (or one longer than the TTL) means the TTL is the only
	// bound we can justify.
	retainedWithin := func(window time.Duration) string {
		if window <= 0 || window > grokBillingObservationTTL {
			window = grokBillingObservationTTL
		}
		if snap.ObservedAt.Before(now.Add(-window)) {
			return ""
		}
		return observedAt
	}

	var credits cliAgentUsageMetric
	kind, label, ok := grokBillingPeriodKind(snap.PeriodType)
	if !ok {
		// An unrecognized (or absent) period is deliberately NOT guessed — the
		// frontend derives a window length from the kind. Discarding the whole
		// observation was wrong too: it threw away the proof that billing was
		// freshly read. Keep the placeholder's unguessed shape, plus freshness.
		// ResetAt is never set here: we do not know when this window ends.
		//
		// A record that NAMES its end and whose end has passed is a definitive
		// rollover, and it outranks the TTL exactly as it does in the recognized
		// branch below: the record describes a window that is over, so it is no
		// longer a confirmation of the live one. Falling back to the timestamp-
		// less placeholder is the same downgrade an ended recognized unmetered
		// period takes.
		switch {
		case snap.HasPeriodEnd && !now.Before(snap.PeriodEnd):
			credits = grokUnknownCreditsMetric()
		default:
			credits = grokUnknownCreditsMetricAt(retainedWithin(0))
		}
	} else {
		credits = cliAgentUsageMetric{
			Kind:       kind,
			Label:      label,
			Unit:       "%",
			ObservedAt: observedAt,
		}
		switch {
		case snap.HasPeriodEnd && !now.Before(snap.PeriodEnd):
			// The period we observed has ended; whatever it read no longer describes
			// the live one.
			credits.Unknown = true
			if !snap.HasUsedPercent {
				// Grok 1.0+ never metered this period, so there is no observation to
				// date-stamp. Keeping ObservedAt would show a stale "Last observed"
				// for a value we never read — the same misleading state the unmetered
				// case below removes for a still-live window.
				credits.ObservedAt = ""
			}
		case !snap.HasUsedPercent:
			// A CURRENT period the CLI no longer meters (Grok 1.0+). The record
			// timestamp is proof that the provider freshly confirmed this unmetered
			// period; retaining it is what distinguishes this from an inferred
			// placeholder produced when no usable billing response exists. An
			// unparseable period end costs us only ResetAt — freshness does not
			// depend on knowing when the window closes, but WITHOUT an end we
			// cannot see the rollover either, so retention falls back to the
			// period's own nominal length.
			credits.Unknown = true
			if snap.HasPeriodEnd {
				credits.ObservedAt = retainedWithin(0)
				credits.ResetAt = snap.PeriodEnd.UTC().Format(time.RFC3339)
			} else {
				credits.ObservedAt = retainedWithin(grokBillingPeriodLength(kind))
			}
		default:
			credits.Total = floatPtr(100)
			credits.Consumed = floatPtr(snap.UsedPercent)
			credits.Remaining = floatPtr(100 - snap.UsedPercent)
			if snap.HasPeriodEnd {
				credits.ResetAt = snap.PeriodEnd.UTC().Format(time.RFC3339)
			}
		}
	}
	metrics := []cliAgentUsageMetric{credits}

	// On-demand is a separate pool with its own numbers, so an unmetered — or
	// unrecognized — credits window must not suppress it.
	if snap.HasOnDemand {
		onDemand := cliAgentUsageMetric{
			Kind:       limitKindTokens,
			Label:      "On-demand credits",
			Unit:       "credits",
			Total:      floatPtr(snap.OnDemandCap),
			ObservedAt: observedAt,
		}
		if snap.HasOnDemandUsed {
			onDemand.Consumed = floatPtr(snap.OnDemandUsed)
			onDemand.Remaining = floatPtr(snap.OnDemandCap - snap.OnDemandUsed)
		} else {
			// The cap is configured but the record reported no usage against it.
			// Show the pool as existing-but-unobserved rather than as untouched:
			// "0 of 50 used" is an assertion the record never made.
			onDemand.Unknown = true
		}
		metrics = append(metrics, onDemand)
	}
	return metrics
}

// grokUnknownCreditsMetric is the inferred placeholder: a limit exists, but no
// usable record has ever been read for it.
func grokUnknownCreditsMetric() cliAgentUsageMetric {
	return grokUnknownCreditsMetricAt("")
}

// grokUnknownCreditsMetricAt is the same row with an optional provider
// observation time. A populated observedAt makes it a CONFIRMED-UNMETERED row;
// an empty one leaves it the inferred placeholder. One construction point so
// the two states can never drift apart in kind, label or unit.
//
// The label stays "Weekly credits" even when the period is unrecognized: it is
// the shipped placeholder copy, and the row carries no values and no ResetAt,
// so nothing is plotted under the guess.
func grokUnknownCreditsMetricAt(observedAt string) cliAgentUsageMetric {
	return cliAgentUsageMetric{
		Kind:       limitKindWeekly,
		Label:      "Weekly credits",
		Unit:       "%",
		Unknown:    true,
		ObservedAt: observedAt,
	}
}
