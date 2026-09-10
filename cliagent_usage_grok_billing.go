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

// grokBillingRaceRepairAttempts bounds how many times a managed merge re-appends
// a newer record that a concurrent DIRECT run displaced. The direct child is an
// external writer that never takes our mutex, so the repair is a retry against
// something we cannot serialize with — one quiet pass settles it, and a child
// appending faster than we can restore does not need us: its own next record is
// the newest one anyway.
const grokBillingRaceRepairAttempts = 3

// grokBillingPreWriteBarrier is the seam where an EXTERNAL writer's append can
// land: after the supersession scan has taken its file snapshot and before the
// managed payload is written. A no-op in production — the direct Grok child
// simply writes whenever it likes — it exists so the repair below can be tested
// deterministically rather than by racing a goroutine.
var grokBillingPreWriteBarrier = func() {}

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

	// The INCOMING timestamp is checked with the same bound the supersession
	// guard applies to the records already in the log. A clock correction
	// between the child's credits fetch and its exit leaves us carrying a
	// future-dated observation; appending it puts a record grokBillingMetrics
	// refuses to publish at the end of the log, and readGrokBillingSnapshot
	// picks the LAST billing line by FILE ORDER — so the merge would blank a
	// valid reading and keep it blank until wall-clock caught up. Refusing the
	// merge costs this one session's observation; making it costs the card.
	if snap.ObservedAt.After(time.Now().Add(grokBillingMaxClockSkew)) {
		return grokManagedBillingUntrusted, nil
	}

	payload, err := grokManagedBillingPayload(identity, snap)
	if err != nil {
		return grokManagedBillingFailed, err
	}

	outcome, repairAfterWrite, err := appendGrokBillingPair(persistentHome, payload, identity, identities, snap.ObservedAt)
	if err != nil {
		return grokManagedBillingFailed, err
	}
	if outcome != grokManagedBillingPersisted && outcome != grokManagedBillingRaced {
		return outcome, nil
	}

	if repairAfterWrite {
		// Outside the write lock — the re-assertion takes it itself. It names
		// the armed run's CAPTURED account, never the live credentials, so a
		// concurrent login in the shared home cannot turn the repair into a
		// misattribution.
		reassertGrokDirectAttribution()
	}
	return outcome, nil
}

// grokManagedBillingPayload renders one identity + billing line pair for the
// provider-owned log. Only normalized allowlisted fields are rendered — prompts,
// credentials, raw config, tool results and unrelated log keys never reach it.
//
// Shared by the managed merge and by the race repair below, so a record we
// re-append to restore timestamp order is byte-identical in shape to the one the
// merge writes and passes the same reader.
func grokManagedBillingPayload(identity string, snap grokBillingSnapshot) ([]byte, error) {
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
		return nil, err
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
		return nil, err
	}
	payload := make([]byte, 0, len(identityLine)+len(billingLine)+2)
	payload = append(payload, identityLine...)
	payload = append(payload, '\n')
	payload = append(payload, billingLine...)
	payload = append(payload, '\n')
	return payload, nil
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
//
// The supersession check is a READ of a file an EXTERNAL writer also appends to:
// a same-account direct Grok child never takes this mutex, so holding it does not
// make the scan-and-append atomic with the writer that matters. A direct record
// that lands between the scan and the write leaves our older receipt last, and
// readGrokBillingSnapshot publishes by FILE ORDER — the stale reading the guard
// exists to prevent, arriving through the one door the guard cannot close. So the
// decision is re-tested AFTER the write (restoreGrokBillingRecordDisplacedByRace)
// and a displaced newer record is put back on top.
func appendGrokBillingPair(
	persistentHome string,
	payload []byte,
	identity string,
	identities []string,
	observedAt time.Time,
) (outcome grokManagedBillingOutcome, repairAfterWrite bool, err error) {
	grokBillingAttributionSerialize.Lock()
	defer grokBillingAttributionSerialize.Unlock()

	// Scanned with the MANAGED account's identities, and NOT via
	// readGrokBillingSnapshot: that reader answers "what should we publish",
	// so it stops at the globally newest record and reports nothing when that
	// record belongs to someone else. This guard asks a different question —
	// "does this account already have a fresher receipt in here" — and an
	// intervening account-B line must not hide account A's newer record behind
	// it, or we would append A's older receipt last and republish stale A
	// utilization. So the scan walks PAST foreign and undecodable records to
	// the newest one that is genuinely A's.
	//
	// A record dated beyond grokBillingMaxClockSkew is skipped rather than
	// honored. grokBillingMetrics refuses to PUBLISH such a record, so letting
	// it supersede would leave managed usage Unknown until wall-clock caught up
	// with it — after a clock correction, potentially for the rest of the
	// period. The read path's distrust and this one have to agree: a timestamp
	// too far ahead to publish is too far ahead to protect.
	//
	// The bound is passed INTO the scan, not applied to its result. Filtering
	// afterwards asked the wrong question: the helper had already stopped at the
	// newest same-account record, so one untrusted future record made the guard
	// read "this account has nothing here" and hid an EARLIER trusted record
	// that is genuinely newer than our snapshot — which we would then append
	// last, replacing a newer valid observation with a stale one. Skipping the
	// untrusted record inside the scan keeps looking for the newest record this
	// account has that we would also be willing to publish.
	trustedThrough := time.Now().Add(grokBillingMaxClockSkew)
	if existing, ok := newestTrustedGrokBillingRecordFor(persistentHome, identities, trustedThrough); ok &&
		!existing.ObservedAt.Before(observedAt) {
		return grokManagedBillingSuperseded, false, nil
	}

	payload, repairAfterWrite = grokBillingPayloadWithDirectMarker(persistentHome, payload, identity, observedAt)
	grokBillingPreWriteBarrier()
	if err := writeGrokBillingPayload(persistentHome, payload); err != nil {
		return grokManagedBillingFailed, repairAfterWrite, err
	}

	raced, err := restoreGrokBillingRecordDisplacedByRace(persistentHome, identity, identities)
	if err != nil {
		return grokManagedBillingFailed, repairAfterWrite, err
	}

	directRaced, err := restoreArmedDirectBillingRecordDisplacedByRace(persistentHome, identity, identities)
	if err != nil {
		return grokManagedBillingFailed, repairAfterWrite, err
	}

	// A managed-only merge otherwise leaves OUR identity standing as the log's
	// newest marker, and a trailing marker vouches for whatever is written
	// next — the exact hazard sealGrokDirectAttribution closes when the last
	// direct run releases. Without the seal, a later Grok invocation OUTSIDE
	// the agent (an API-key override under our still-cached login, or a
	// `grok login` elsewhere) has its identity-less record accepted as this
	// account's usage indefinitely, publishing one subscription's utilization
	// under another. The record we just wrote keeps its own attribution: it
	// binds to the identity ABOVE it, so a marker appended after it cannot
	// unbind it.
	//
	// Taken under the lock we already hold, so a direct run that arms
	// concurrently either observes this seal or replaces it. When one IS armed
	// the payload already ends with its marker or preserved pair, so this is a
	// bounded tail read that writes nothing — except in the repairAfterWrite
	// fallback, where naming it here is the repair.
	sealGrokBillingAttributionLocked(persistentHome)

	if raced || directRaced {
		return grokManagedBillingRaced, repairAfterWrite, nil
	}
	return grokManagedBillingPersisted, repairAfterWrite, nil
}

// restoreArmedDirectBillingRecordDisplacedByRace is the armed DIRECT account's
// half of the post-write repair, and it exists for the same reason its managed
// twin does: grokDirectBillingPairToPreserve is a scan taken BEFORE the write,
// against a writer that never takes our mutex.
//
// When the direct child lands its newest record in the gap between that scan and
// writeGrokBillingPayload, our payload settles on top of it carrying either a
// bare direct marker or a preserved but now-OLDER direct pair. Neither leaves the
// direct account readable: a trailing marker cannot authenticate the record above
// it, so the direct account's next gather stops at OUR managed record, refuses it
// as foreign and publishes nothing — and a preserved older pair publishes a stale
// reading instead. restoreGrokBillingRecordDisplacedByRace cannot see any of this
// because it scans only the MANAGED account's identities.
//
// The tiebreak is the same one grokBillingPayloadWithDirectMarker already
// applies: the genuinely NEWEST trusted observation is the one that gets to be
// last, whichever account produced it. A direct record no newer than the managed
// record we just wrote is left exactly where it is, because the reader publishes
// by FILE ORDER and moving it last would republish a stale reading.
//
// Writes nothing in the common case: when the merge already left the direct
// account's newest record readable, the publish reader confirms it and this pass
// is a read. Callers must hold grokBillingAttributionSerialize.
func restoreArmedDirectBillingRecordDisplacedByRace(
	persistentHome, identity string,
	identities []string,
) (bool, error) {
	directIdentity, armed := grokDirectAttributionAssertion()
	if !armed || strings.EqualFold(directIdentity, identity) {
		// No live direct run, or the pair we wrote already names it — the
		// managed repair above covers that account on its own.
		return false, nil
	}
	directIdentities := []string{directIdentity}
	repaired := false

	// Bounded and RE-TESTED after each write, exactly like its managed twin: the
	// pair this repair appends is itself chosen by a scan taken before a write,
	// against the same external writer. A direct record landing in THAT window
	// leaves the pair we just appended stale as the log's last line — and
	// nothing else corrects it, because the keeper sees a marker that already
	// names the direct account and the managed repair scans only the managed
	// identities. Each pass re-reads, so a quiet log settles on the first, and a
	// child appending faster than we can restore does not need us: its own next
	// record is the newest anyway and the following gather reads it.
	for attempt := 0; attempt < grokBillingRaceRepairAttempts; attempt++ {
		trustedThrough := time.Now().Add(grokBillingMaxClockSkew)

		newest, found := newestTrustedGrokBillingRecordFor(persistentHome, directIdentities, trustedThrough)
		if !found {
			return repaired, nil
		}
		if published, ok := readGrokBillingSnapshot(persistentHome, directIdentities); ok &&
			!published.ObservedAt.Before(newest.ObservedAt) {
			// The direct account already publishes its own newest observation,
			// so the race — if there was one — did no harm and no duplicate
			// line is written for it.
			return repaired, nil
		}

		// Compared against the managed account's newest TRUSTED record rather
		// than the snapshot we set out to merge: the repair above may have
		// re-appended a newer managed record since, and displacing that one
		// would undo it.
		var managedObservedAt time.Time
		if managedNewest, ok := newestTrustedGrokBillingRecordFor(persistentHome, identities, trustedThrough); ok {
			managedObservedAt = managedNewest.ObservedAt
		}
		pair, ok := grokDirectBillingPairToPreserve(persistentHome, directIdentity, identity, managedObservedAt)
		if !ok {
			return repaired, nil
		}
		grokBillingPreWriteBarrier()
		if err := writeGrokBillingPayload(persistentHome, pair); err != nil {
			return repaired, err
		}
		repaired = true
	}
	return repaired, nil
}

// grokBillingPayloadWithDirectMarker appends the armed direct run's identity
// after the caller's pair so a still-live direct child keeps writing records that
// bind to ITS account rather than to the managed one we just named.
//
// When that direct account ALREADY holds a record in the log newer than the one
// we are merging, its marker alone is not enough: a trailing identity line
// cannot authenticate a record that PRECEDES it, so the direct account's next
// gather would stop at OUR newest managed record, refuse it as foreign, and
// report nothing — losing an observation that is both fresher and attributable.
// So that account's own newest record is re-appended WITH its marker, as a
// complete pair, and the log ends with the genuinely newest reading. It is
// re-rendered through grokManagedBillingPayload, so it carries only the same
// normalized allowlisted fields the merge itself writes.
//
// Only when it is NEWER than ours. A direct record OLDER than the one we are
// merging must stay where it is: the reader publishes by FILE ORDER, so moving
// it last would republish a stale reading — the very failure the supersession
// guard and the race repair exist to prevent.
//
// Callers must hold grokBillingAttributionSerialize: the armed CHECK and the
// payload it produces are taken UNDER the lock, not before it (see the ordering
// argument on appendGrokBillingPair).
//
// Returns repairAfterWrite when the marker could not be rendered and the caller
// must fall back to a post-write ensureGrokBillingAttribution.
func grokBillingPayloadWithDirectMarker(
	persistentHome string,
	payload []byte,
	identity string,
	observedAt time.Time,
) ([]byte, bool) {
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
			return payload, true
		}
		if pair, ok := grokDirectBillingPairToPreserve(persistentHome, directIdentity, identity, observedAt); ok {
			return append(payload, pair...), false
		}
		payload = append(payload, directLine...)
		payload = append(payload, '\n')
	}
	return payload, false
}

// grokDirectBillingPairToPreserve renders the armed direct account's newest
// trusted record as a fresh identity/record pair, when that record is newer than
// the record the caller is about to leave as the log's last line.
//
// Scanned under the DIRECT account's identity alone: this asks "does that
// account hold something newer here", the same question the supersession guard
// asks for the managed account, so it uses the same clock-skew bound — a record
// too far ahead to publish is too far ahead to preserve.
//
// A record that cannot be re-rendered is reported as nothing to preserve, so the
// caller falls back to the bare marker: attribution for the direct run's NEXT
// record still beats writing nothing for it.
//
// The timestamp comparison is the right tiebreak only while both records could
// be published on this device. When the persistent home's credentials resolve to
// the DIRECT account and not to `mergedIdentity`, leaving the merged record last
// serves nobody: every gather here reads under the direct account's identities,
// stops at that foreign record and reports nothing, while the keeper sees a
// direct marker as newest and never repairs it — the direct account goes dark
// with a perfectly good record of its own sitting in the file. Preserving it
// then is not republishing a stale reading either: it is that account's OWN
// newest observation, and the account it displaces has no reader left here.
//
// Callers must hold grokBillingAttributionSerialize.
func grokDirectBillingPairToPreserve(
	persistentHome, directIdentity, mergedIdentity string,
	observedAt time.Time,
) ([]byte, bool) {
	// Same boundary the armed-reader repair honours, for the same reason: the
	// supersession scan walks PAST a recognized response it cannot decode to an
	// older record, while the publish path stops dead at it. Preserving that
	// older record would put it last and publish a reading the reader had
	// deliberately refused, so the run falls back to the bare marker instead.
	// Scoped to the direct account: a response another account produced never
	// supersedes this one's own observation.
	if grokNewestBillingResponseIsUnusableFor(persistentHome, directIdentity) {
		return nil, false
	}
	newest, ok := newestTrustedGrokBillingRecordFor(
		persistentHome, []string{directIdentity}, time.Now().Add(grokBillingMaxClockSkew))
	if !ok {
		return nil, false
	}
	if !newest.ObservedAt.After(observedAt) &&
		!grokDirectAccountOwnsTheReader(persistentHome, directIdentity, mergedIdentity) {
		return nil, false
	}
	pair, err := grokManagedBillingPayload(directIdentity, newest)
	if err != nil {
		return nil, false
	}
	return pair, true
}

// grokDirectAccountOwnsTheReader reports whether the armed DIRECT account is the
// one a gather on this device would read under, while the account being merged
// is not. It is the only condition under which an older record may take the
// log's last line: the record it displaces is unreadable here regardless, so the
// choice is between one account's real observation and nothing at all.
//
// Both sides are resolved from the SAME candidate list the gather uses
// (grokIdentityCandidates), and folded with grokIdentityFoldKey, so this cannot
// disagree with grokRecordBelongsToCurrentAccount about who the current login
// is. The contested sentinel is never a login.
func grokDirectAccountOwnsTheReader(persistentHome, directIdentity, mergedIdentity string) bool {
	candidates := grokIdentityCandidates(persistentHome)
	return grokIdentityAmongCandidates(candidates, directIdentity) &&
		!grokIdentityAmongCandidates(candidates, mergedIdentity)
}

// restoreGrokBillingRecordForArmedReaderLocked re-appends the armed DIRECT
// account's own newest trusted record when that account is the one a gather on
// this device reads under and a FOREIGN record sits above it.
//
// The marker guard alone is not enough once ownership of the reader can change
// AFTER a merge. A managed merge for account A, taken while the persistent
// credentials still resolved to A, correctly leaves `A identity, A record, B
// marker` for a live direct run under B: B's older record stays where it is
// because moving it last would have republished a stale reading for the account
// that was then reading. A later `grok login` back to B flips that: every gather
// now stops at A's record, refuses it as foreign and publishes nothing, while
// the keeper sees a trailing B marker, reports attribution intact, and never
// repairs it — B goes dark with a perfectly good record of its own in the file.
//
// So the re-assertion asks the same question grokDirectBillingPairToPreserve
// asks at merge time, just later: is the account we are naming the one that
// reads here, and does it already publish its own newest observation? Only when
// the answer is "yes, and no" is anything written, so a healthy log costs a
// bounded tail read and no append. The record it displaces is unreadable on this
// device regardless, which is what makes an older record taking the last line
// honest rather than a stale republish.
//
// Bounded and RE-TESTED after each write like the merge's race repair, and for
// the same reason: the pair is chosen by a read taken before a write, against a
// direct child that never takes our mutex. Callers must hold
// grokBillingAttributionSerialize.
func restoreGrokBillingRecordForArmedReaderLocked(base, identity string) bool {
	identity = strings.TrimSpace(identity)
	if base == "" || identity == "" {
		return false
	}
	// The contested sentinel is never a login, so grokIdentityAmongCandidates
	// refuses it here as it does everywhere else: while two accounts overlap
	// there is no record this device may honestly publish for either.
	if !grokIdentityAmongCandidates(grokIdentityCandidates(base), identity) {
		return false
	}

	identities := []string{identity}
	repaired := false
	for attempt := 0; attempt < grokBillingRaceRepairAttempts; attempt++ {
		newest, ok := newestTrustedGrokBillingRecordFor(
			base, identities, time.Now().Add(grokBillingMaxClockSkew))
		if !ok {
			return repaired
		}
		published, outcome := readGrokBillingSnapshotOutcome(base, identities)
		if outcome == grokBillingReadUnusable {
			// The newest recognized response is one the publish path REFUSES to
			// render, and that refusal is the point: it supersedes every older
			// record, ours included. Re-appending an older one under it would
			// hand the next gather a stale percentage the reader had already
			// failed closed on. Only a FOREIGN newest record leaves this
			// account stranded — an undecodable one THIS account produced
			// leaves it correctly blind. One produced by another account is
			// classified foreign by the reader and repaired over below.
			return repaired
		}
		if outcome == grokBillingReadOK &&
			!published.ObservedAt.Before(newest.ObservedAt) {
			// The account already publishes its own newest observation, so
			// nothing is stranded and no duplicate line is written.
			return repaired
		}
		pair, err := grokManagedBillingPayload(identity, newest)
		if err != nil {
			return repaired
		}
		grokBillingPreWriteBarrier()
		if err := writeGrokBillingPayload(base, pair); err != nil {
			return repaired
		}
		repaired = true
	}
	return repaired
}

// grokIdentityAmongCandidates reports whether `identity` is one of the account
// values the current credentials resolve to.
func grokIdentityAmongCandidates(candidates []string, identity string) bool {
	identity = strings.TrimSpace(identity)
	if identity == "" || strings.EqualFold(identity, grokContestedBillingIdentity) {
		return false
	}
	key := grokIdentityFoldKey(identity)
	for _, candidate := range candidates {
		if key == grokIdentityFoldKey(candidate) {
			return true
		}
	}
	return false
}

// restoreGrokBillingRecordDisplacedByRace re-appends this account's newest
// trusted observation when the record that would now be PUBLISHED is older than
// one the log already holds.
//
// The supersession guard reads the log; a same-account direct Grok child appends
// to it without ever taking our mutex. A record it writes between that read and
// our O_APPEND is invisible to the guard, and because readGrokBillingSnapshot
// takes the last billing line by FILE ORDER rather than by timestamp, our older
// receipt then becomes the published one — exactly the stale reading the guard
// exists to prevent. The window is small but it is a real external writer, so it
// is closed after the fact instead of pretended away.
//
// The test is deliberately the PUBLISH reader against the supersession reader,
// not a byte-offset comparison: it asks the only question that matters ("is the
// record we would show older than one we hold?"), so it is false when the racer
// landed AFTER our write and is already last — no duplicate line is written for
// a race that did no harm — and it stays correct on every platform without
// relying on the file offset an O_APPEND write leaves behind.
//
// Nothing is written when the publish reader declines (a foreign or undecodable
// newest record). That is a cross-account boundary, not a displacement: putting
// our record on top of another account's newest line would republish OUR usage
// over theirs, which is worse than the staleness this repairs.
//
// Callers must hold grokBillingAttributionSerialize.
func restoreGrokBillingRecordDisplacedByRace(persistentHome, identity string, identities []string) (bool, error) {
	repaired := false
	// Bounded: each pass re-appends the newest record it can see, so a quiet log
	// settles on the first. A child appending faster than we can restore is not
	// a loop worth spinning in — its own next record is the newest anyway, and
	// the following gather reads it.
	for attempt := 0; attempt < grokBillingRaceRepairAttempts; attempt++ {
		published, publishedOK := readGrokBillingSnapshot(persistentHome, identities)
		if !publishedOK {
			return repaired, nil
		}
		newest, newestOK := newestTrustedGrokBillingRecordFor(
			persistentHome, identities, time.Now().Add(grokBillingMaxClockSkew))
		if !newestOK || !published.ObservedAt.Before(newest.ObservedAt) {
			return repaired, nil
		}
		payload, err := grokManagedBillingPayload(identity, newest)
		if err != nil {
			return repaired, err
		}
		payload, _ = grokBillingPayloadWithDirectMarker(persistentHome, payload, identity, newest.ObservedAt)
		if err := writeGrokBillingPayload(persistentHome, payload); err != nil {
			return repaired, err
		}
		repaired = true
	}
	return repaired, nil
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
	// grokManagedBillingUntrusted — the record we carry is dated further ahead
	// than grokBillingMaxClockSkew allows, so nothing was written. The clock can
	// move backwards between the child's fetch and its exit, and merging then
	// makes an unpublishable record the globally newest line in the log —
	// grokBillingMetrics refuses it and the card reads Unknown until wall-clock
	// catches up. Leaving the log alone keeps whatever valid observation it
	// already had.
	grokManagedBillingUntrusted
	// grokManagedBillingPersisted — one normalized record was merged.
	grokManagedBillingPersisted
	// grokManagedBillingRaced — the record was merged, but a same-account direct
	// run appended a NEWER one between the supersession scan and the write, so
	// ours landed last and would have been published as the current reading.
	// The newer record was re-appended to restore timestamp order. Named
	// separately from "persisted" because a device seeing this repeatedly is
	// telling us a direct run and a managed session are competing for the same
	// log, which is worth reading in the agent output.
	grokManagedBillingRaced
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
	case grokManagedBillingUntrusted:
		return "untrusted-timestamp"
	case grokManagedBillingPersisted:
		return "persisted"
	case grokManagedBillingRaced:
		return "persisted-after-race-repair"
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

// newestTrustedGrokBillingRecordFor returns the newest billing OBSERVATION (the
// whole normalized snapshot, so the race repair can re-append it) in the log
// tail that belongs to `identities` and is not dated after `trustedThrough`,
// ignoring records produced by any other account.
//
// It is the supersession guard's reader, deliberately separate from
// readGrokBillingSnapshot. That one is the PUBLISH path: it stops at the newest
// billing line and fails closed on anything it cannot decode or attribute,
// because reviving an older percentage there would show the user a stale number.
// Here the same stop would be the bug — a single interleaved account-B record is
// not evidence about account A, and treating it as "A has nothing" is what lets
// an older A receipt be appended over a newer one.
//
// Undecodable and unrecognized lines are likewise skipped rather than
// terminating the scan: this guard only ever decides whether to SKIP a write, so
// the conservative reading of "we cannot tell whose this is" is to keep looking
// for a record we can attribute, and to merge when we find none.
//
// A record dated after `trustedThrough` is skipped for the same reason and by
// the same rule the caller applies: the publish path refuses it, so it is no
// evidence that this account already has a fresher receipt. The scan CONTINUES
// past it to the newest same-account record we would be willing to publish —
// stopping there would report "nothing" and let a stale merge land on top of a
// perfectly good older-but-trusted observation.
//
// Every attributable record is inspected and the GREATEST observation time wins,
// rather than the first one the reverse scan reaches. File order is APPEND order,
// not timestamp order: a clock correction on the device, or a managed snapshot
// merged in after the fact, can leave an older same-account record physically
// after a newer one. Stopping at the first same-account hit would then report the
// older time and let a merge append over the newer observation this guard exists
// to protect. The scan already walked the whole tail whenever nothing was
// attributable, so always finishing it adds no new worst case.
func newestTrustedGrokBillingRecordFor(base string, identities []string, trustedThrough time.Time) (grokBillingSnapshot, bool) {
	lines, _ := readGrokBillingLogTailWithOffset(base)
	if lines == nil {
		return grokBillingSnapshot{}, false
	}
	var newest grokBillingSnapshot
	found := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !grokLineMentionsBillingMessage(line) {
			continue
		}
		var envelope struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(line, &envelope) != nil || !grokBillingMessageRecognized(envelope.Msg) {
			continue
		}
		var rec grokBillingRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok {
			continue
		}
		if snap.ObservedAt.After(trustedThrough) {
			continue
		}
		if !grokRecordBelongsToCurrentAccount(lines, i, identities) {
			continue
		}
		if !found || snap.ObservedAt.After(newest.ObservedAt) {
			newest = snap
			found = true
		}
	}
	return newest, found
}

// grokBillingReadOutcome says WHY the publish reader produced no snapshot.
//
// The bare boolean was ambiguous at exactly the place it mattered. The repairs
// that may re-append an OLDER record read the log first, and two of its refusals
// call for opposite answers: a FOREIGN newest record is the case they exist to
// fix (the record it displaces is unreadable on this device regardless), while a
// recognized response the reader cannot render is the fail-closed behaviour
// itself — appending an older record under it publishes exactly the stale
// utilization that refusal suppresses.
type grokBillingReadOutcome int

const (
	// grokBillingReadOK — a usable, attributable snapshot was returned.
	grokBillingReadOK grokBillingReadOutcome = iota
	// grokBillingReadNone — the tail holds no billing record for anyone.
	grokBillingReadNone
	// grokBillingReadForeign — the newest usable record belongs to another
	// account. Nothing about THIS account's records is implied.
	grokBillingReadForeign
	// grokBillingReadUnusable — the newest recognized billing response cannot be
	// decoded or rendered. It is authoritative all the same: it is the provider's
	// latest answer, so every older record is superseded whoever produced it.
	grokBillingReadUnusable
)

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
	snap, outcome := readGrokBillingSnapshotOutcome(base, identities)
	return snap, outcome == grokBillingReadOK
}

// grokNewestBillingResponseIsUnusableFor reports whether the log's newest
// recognized billing response is one readGrokBillingSnapshot refuses because it
// cannot be decoded or rendered — as opposed to one it refuses as foreign.
//
// Scoped to the account being restored, exactly as the armed-reader repair is.
// An undecodable response supersedes the older records of the account that
// PRODUCED it, because it is that account's latest answer from the provider; it
// says nothing about a different account's credits, which is why a response
// provably produced by someone else is classified foreign instead (see
// grokUnusableResponseIsForeign). Passing no identity keeps the strict reading:
// with nobody to compare the producer against, the response is treated as
// possibly ours and still supersedes.
func grokNewestBillingResponseIsUnusableFor(base, identity string) bool {
	var identities []string
	if identity = strings.TrimSpace(identity); identity != "" {
		identities = []string{identity}
	}
	_, outcome := readGrokBillingSnapshotOutcome(base, identities)
	return outcome == grokBillingReadUnusable
}

// readGrokBillingSnapshotOutcome is readGrokBillingSnapshot with its refusal
// reason preserved. Every decision below is the reader's own, unchanged.
func readGrokBillingSnapshotOutcome(base string, identities []string) (grokBillingSnapshot, grokBillingReadOutcome) {
	lines, truncatedFirstLine := readGrokBillingLogTailWithOffset(base)
	if lines == nil {
		return grokBillingSnapshot{}, grokBillingReadNone
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
			return grokBillingSnapshot{}, grokUnusableOutcome(lines, i, identities)
		}
		if !grokBillingMessageRecognized(envelope.Msg) {
			continue
		}
		var rec grokBillingRecord
		if json.Unmarshal(line, &rec) != nil {
			return grokBillingSnapshot{}, grokUnusableOutcome(lines, i, identities)
		}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok {
			// The newest exact-message record supersedes every older response of
			// the account that produced it, even when one of its required fields
			// is unusable. Continuing here would resurrect a stale pre-upgrade
			// percentage after a malformed or timestamp-less current response.
			return grokBillingSnapshot{}, grokUnusableOutcome(lines, i, identities)
		}
		// Stop at the newest usable record either way: an older one is even less
		// likely to belong to the account signed in now.
		if !grokRecordBelongsToCurrentAccount(lines, i, identities) {
			return grokBillingSnapshot{}, grokBillingReadForeign
		}
		return snap, grokBillingReadOK
	}
	return grokBillingSnapshot{}, grokBillingReadNone
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
	producer, resolved := grokRecordProducerIdentity(lines, lineIdx)
	if !resolved {
		return false
	}
	// Keyed with grokIdentityFoldKey, NOT strings.ToLower: every other identity
	// comparison in this package is strings.EqualFold, and lowercasing is not
	// that relation. Unicode simple folding puts several lowercase runes in one
	// orbit (`Σ`, `σ` and final `ς`), so a marker the attribution keeper accepts
	// as already-present — it folds with EqualFold — could lose this lookup and
	// leave the record beneath it permanently unattributable. Both sides fold
	// the same way or the gate and the keeper disagree about one account.
	return grokIdentityFoldedAmong(identities, producer)
}

// grokRecordProducerIdentity resolves WHO logged the line at lineIdx, from the
// newest identity marker above it. resolved is false when the producer cannot be
// established at all: no marker, a malformed one, or the contested sentinel.
//
// Identity-only, so both the attribution gate and the unusable-response
// classifier below ask the same question of the same evidence and cannot drift.
func grokRecordProducerIdentity(lines [][]byte, lineIdx int) (string, bool) {
	// Start before the billing line. The observed billing envelope carries no
	// identity, so allowing a lookalike user_id on the record to authenticate
	// itself would defeat the cross-account boundary this scan enforces.
	for i := lineIdx - 1; i >= 0; i-- {
		identity, found, valid := grokLogIdentity(lines[i])
		if !found {
			continue
		}
		if !valid {
			// The log names a producer but its newest identity envelope is
			// malformed. Refuse rather than falling back to older account
			// evidence.
			return "", false
		}
		if strings.EqualFold(identity, grokContestedBillingIdentity) {
			// Overlapping direct runs on different accounts were live when this
			// marker was written, so nothing below it can be attributed to
			// either. Refuse explicitly rather than relying on the sentinel
			// merely failing to match a real account id.
			return "", false
		}
		return identity, true
	}
	// Nothing identifies the producer anywhere before the record. That is
	// not evidence it belongs to the current login: `unified.jsonl` is shared
	// across logins, so a CLI old enough never to log an identity leaves the
	// previous account's credits sitting in the same file for the next one to
	// publish as its own. Refuse — the card falls back to "unobservable", which
	// is what it showed before this source existed.
	return "", false
}

// grokIdentityFoldedAmong reports whether identity is one of the candidates,
// under the same fold key grokRecordBelongsToCurrentAccount uses. An empty
// candidate list matches nothing: credentials that resolve to nothing we can
// compare are not evidence of ownership.
func grokIdentityFoldedAmong(identities []string, identity string) bool {
	key := grokIdentityFoldKey(identity)
	if key == "" {
		return false
	}
	for _, candidate := range identities {
		if grokIdentityFoldKey(candidate) == key {
			return true
		}
	}
	return false
}

// grokUnusableOutcome classifies a recognized billing response the reader cannot
// decode or render.
//
// Such a response is the provider's latest answer to whoever fetched it, so it
// supersedes that account's older records — reviving one under it would publish
// a reading the reader had deliberately failed closed on. It says nothing about
// a DIFFERENT account's credits, though, and the producer is available from the
// preceding marker even when the response payload itself is undecodable. When
// the marker proves another account produced it, report it as foreign: identical
// to a usable foreign record, which already leaves this account's own newest
// observation free to be restored. Anything less certain — no marker, a
// malformed one, the contested sentinel, or no identity to compare against —
// stays unusable, so the strict reading is the default.
func grokUnusableOutcome(lines [][]byte, lineIdx int, identities []string) grokBillingReadOutcome {
	if len(identities) == 0 {
		return grokBillingReadUnusable
	}
	producer, resolved := grokRecordProducerIdentity(lines, lineIdx)
	if !resolved || grokIdentityFoldedAmong(identities, producer) {
		return grokBillingReadUnusable
	}
	return grokBillingReadForeign
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
