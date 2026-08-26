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

// grokBillingRecord mirrors the fields we consume from that line. Everything is
// optional: an older CLI, a different plan shape, or a partially written line
// must degrade to "unobservable", never to a wrong number.
type grokBillingRecord struct {
	TS  string `json:"ts"`
	Ctx struct {
		Config struct {
			CreditUsagePercent grokBillingNumber `json:"creditUsagePercent"`
			CurrentPeriod      struct {
				Type string `json:"type"`
				End  string `json:"end"`
			} `json:"currentPeriod"`
			OnDemandCap struct {
				Val grokBillingNumber `json:"val"`
			} `json:"onDemandCap"`
			OnDemandUsed struct {
				Val grokBillingNumber `json:"val"`
			} `json:"onDemandUsed"`
		} `json:"config"`
		SubscriptionTier string `json:"subscriptionTier"`
	} `json:"ctx"`
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

const grokManagedBillingIdentityMessage = "aiexpedite: managed billing producer"

// seedGrokManagedBillingIdentity records the account copied into an isolated
// ACP home before the child starts. The isolated log is private to that one
// process, so this marker cannot be displaced by a concurrent `grok login` in
// the real home. Prefer the last candidate (normally the opaque JWT subject)
// over an email while retaining compatibility with older auth layouts.
func seedGrokManagedBillingIdentity(isolatedHome string) error {
	path := grokBillingLogPath(isolatedHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	identities := grokIdentityCandidates(isolatedHome)
	if len(identities) == 0 {
		return nil
	}
	identity := strings.TrimSpace(identities[len(identities)-1])
	if identity == "" {
		return nil
	}
	line, err := json.Marshal(map[string]any{
		"msg": grokManagedBillingIdentityMessage,
		"ctx": map[string]any{"user_id": identity},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(line, '\n'), 0o600)
}

// persistGrokManagedBillingSnapshot copies one session's newest verified
// billing observation into the provider-owned log after the managed process
// exits. The source is bound to the auth copy frozen at session start. The two
// destination lines are written in one O_APPEND call so a direct Grok process
// cannot interleave a different account identity between them.
//
// Only normalized allowlisted fields are persisted. Prompts, credentials, raw
// config, tool results, and unrelated log fields never leave the isolated home.
func persistGrokManagedBillingSnapshot(isolatedHome, persistentHome string) error {
	if isolatedHome == "" || persistentHome == "" {
		return nil
	}
	identities := grokIdentityCandidates(isolatedHome)
	if len(identities) == 0 {
		return nil
	}
	snap, ok := readGrokBillingSnapshot(isolatedHome, identities)
	if !ok {
		return nil
	}
	identity := strings.TrimSpace(identities[len(identities)-1])
	if identity == "" {
		return nil
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
	identityLine, err := json.Marshal(map[string]any{
		"msg": grokManagedBillingIdentityMessage,
		"ctx": map[string]any{"user_id": identity},
	})
	if err != nil {
		return err
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
		return err
	}
	payload := make([]byte, 0, len(identityLine)+len(billingLine)+2)
	payload = append(payload, identityLine...)
	payload = append(payload, '\n')
	payload = append(payload, billingLine...)
	payload = append(payload, '\n')

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
	path := grokBillingLogPath(base)
	if path == "" {
		return grokBillingSnapshot{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return grokBillingSnapshot{}, false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return grokBillingSnapshot{}, false
	}
	offset := int64(0)
	if info.Size() > grokBillingLogTailBytes {
		offset = info.Size() - grokBillingLogTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return grokBillingSnapshot{}, false
	}
	// Read exactly the tail length captured by Stat. An unbounded ReadAll after
	// seeking can consume more than 1 MiB when Grok appends quickly enough to
	// keep extending the file, defeating this gather's memory/work bound and
	// mixing records from different filesystem snapshots.
	tail := make([]byte, info.Size()-offset)
	if _, err := io.ReadFull(f, tail); err != nil {
		// The log was truncated or replaced between Stat and ReadFull. Fail
		// closed and let the next bounded refresh gather a coherent snapshot.
		return grokBillingSnapshot{}, false
	}

	lines := bytes.Split(tail, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		// Cheap pre-filter before the JSON decode: the vast majority of lines in
		// this log are chat/tool telemetry, and unmarshalling every one of them
		// would dominate the gather.
		if len(line) == 0 || !bytes.Contains(line, []byte(grokBillingLogMessage)) {
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
			if i == 0 && offset > 0 {
				continue
			}
			return grokBillingSnapshot{}, false
		}
		if envelope.Msg != grokBillingLogMessage {
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
	snap := grokBillingSnapshot{
		ObservedAt:       observed,
		PeriodType:       rec.Ctx.Config.CurrentPeriod.Type,
		SubscriptionTier: rec.Ctx.SubscriptionTier,
	}
	// Grok 1.0 stopped emitting creditUsagePercent. Every other field of the
	// record is unchanged, so the record is still the authoritative statement of
	// the CURRENT billing period — it simply no longer says how much of it is
	// used. Treat that as "usable record, unobserved usage" rather than "not a
	// record": rejecting it made the scanner walk further back and publish a
	// PRE-UPGRADE reading, under a period that had since ended.
	if pct := rec.Ctx.Config.CreditUsagePercent; pct.Valid && grokFiniteNumber(pct.Value) {
		snap.UsedPercent = clampPercent(pct.Value)
		snap.HasUsedPercent = true
	}
	if end, err := time.Parse(time.RFC3339, rec.Ctx.Config.CurrentPeriod.End); err == nil {
		snap.PeriodEnd = end
		snap.HasPeriodEnd = true
	}
	// On-demand is a separate, opt-in pool: only plot it when a cap exists, or
	// the row would read as a hard 0-of-0 limit on every subscription account.
	if cap := rec.Ctx.Config.OnDemandCap.Val; cap.Valid && cap.Value > 0 && grokFiniteNumber(cap.Value) {
		snap.HasOnDemand = true
		snap.OnDemandCap = cap.Value
		// A cap with no reported usage is a pool we know exists but have not
		// observed. Leaving the zero default here would report it as completely
		// unused — an assertion the record never made.
		if used := rec.Ctx.Config.OnDemandUsed.Val; used.Valid &&
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

// grokBillingMetrics turns a snapshot into the card's capacity rows.
//
// A metered period whose end has passed is reported Unknown WITH its historical
// observation time rather than as 0% used. An ended unmetered period has no
// current confirmation and drops the timestamp. In both cases, assuming an
// empty pool could hide usage from another computer on the same account.
func grokBillingMetrics(snap grokBillingSnapshot, now time.Time) []cliAgentUsageMetric {
	// A future record remains authoritative and blocks older records, but no
	// value sharing its untrusted observation time may escape this gather.
	if snap.ObservedAt.After(now.Add(grokBillingMaxClockSkew)) {
		return []cliAgentUsageMetric{grokUnknownCreditsMetric()}
	}

	kind, label, ok := grokBillingPeriodKind(snap.PeriodType)
	if !ok {
		return nil
	}
	observedAt := snap.ObservedAt.UTC().Format(time.RFC3339)
	credits := cliAgentUsageMetric{
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
		// A current period the CLI no longer meters (Grok 1.0+). The record
		// timestamp is proof that the provider freshly confirmed this unmetered
		// period; retaining it is what distinguishes this from an inferred
		// placeholder produced when no usable billing response exists.
		credits.Unknown = true
		if snap.HasPeriodEnd {
			credits.ResetAt = snap.PeriodEnd.UTC().Format(time.RFC3339)
		} else {
			// Without a parseable period end we cannot prove the unmetered period
			// is current, so keep only the inferred unknown shape.
			credits.ObservedAt = ""
		}
	default:
		credits.Total = floatPtr(100)
		credits.Consumed = floatPtr(snap.UsedPercent)
		credits.Remaining = floatPtr(100 - snap.UsedPercent)
		if snap.HasPeriodEnd {
			credits.ResetAt = snap.PeriodEnd.UTC().Format(time.RFC3339)
		}
	}
	metrics := []cliAgentUsageMetric{credits}

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

func grokUnknownCreditsMetric() cliAgentUsageMetric {
	return cliAgentUsageMetric{
		Kind:    limitKindWeekly,
		Label:   "Weekly credits",
		Unit:    "%",
		Unknown: true,
	}
}
