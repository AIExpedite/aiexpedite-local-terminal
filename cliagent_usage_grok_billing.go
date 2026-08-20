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
// no new line must not make an old percentage look current.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// grokBillingLogTailBytes bounds how much of the log we read. unified.jsonl is
// append-only and grows past 5 MB on an active machine; the newest billing line
// is near the end, so tailing keeps a 6-hour machine-info gather cheap. A line
// older than the tail window is indistinguishable from "never logged" — that is
// the correct downgrade, since such a reading would be far too old to plot.
const grokBillingLogTailBytes = 1 << 20 // 1 MiB

// grokBillingLogMessage is the log `msg` that carries the credits config. Grok
// writes it when the CLI fetches billing (session start / periodic refresh).
const grokBillingLogMessage = "billing: fetched credits config"

// grokLogUserIDRe extracts the account the CLI was acting as. The billing record
// itself carries no identity, but the surrounding log lines do, and the log
// survives `grok login` as a different user — so without this check a retained
// record from a previous account would be published as the current one's.
var grokLogUserIDRe = regexp.MustCompile(`"user_?[Ii]d"\s*:\s*"([^"]{1,128})"`)

// grokBillingRecord mirrors the fields we consume from that line. Everything is
// optional: an older CLI, a different plan shape, or a partially written line
// must degrade to "unobservable", never to a wrong number.
type grokBillingRecord struct {
	TS  string `json:"ts"`
	Msg string `json:"msg"`
	Ctx struct {
		Config struct {
			CreditUsagePercent *float64 `json:"creditUsagePercent"`
			CurrentPeriod      struct {
				Type  string `json:"type"`
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"currentPeriod"`
			OnDemandCap struct {
				Val *float64 `json:"val"`
			} `json:"onDemandCap"`
			OnDemandUsed struct {
				Val *float64 `json:"val"`
			} `json:"onDemandUsed"`
			PrepaidBalance struct {
				Val *float64 `json:"val"`
			} `json:"prepaidBalance"`
		} `json:"config"`
		SubscriptionTier string `json:"subscriptionTier"`
	} `json:"ctx"`
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

// readGrokBillingSnapshot returns the NEWEST billing record in the log tail,
// provided it can be tied to the CURRENT credentials.
//
// Lines are scanned back-to-front and the first well-formed billing record wins:
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
	tail, err := io.ReadAll(f)
	if err != nil {
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
		var rec grokBillingRecord
		if json.Unmarshal(line, &rec) != nil || rec.Msg != grokBillingLogMessage {
			continue
		}
		snap, ok := grokBillingSnapshotFromRecord(rec)
		if !ok {
			continue
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
// logged at or before the record, because that is who the CLI was acting as when
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
	for i := lineIdx; i >= 0; i-- {
		match := grokLogUserIDRe.FindSubmatch(lines[i])
		if match == nil {
			continue
		}
		if len(wanted) == 0 {
			// The log names a producer but the credentials resolve to nothing we
			// can compare. Refuse rather than guess: an unidentifiable local login
			// is exactly the logged-out case this check exists for.
			return false
		}
		return wanted[strings.ToLower(string(match[1]))]
	}
	// Nothing identifies the producer, anywhere at or before the record. That is
	// not evidence it belongs to the current login: `unified.jsonl` is shared
	// across logins, so a CLI old enough never to log an identity leaves the
	// previous account's credits sitting in the same file for the next one to
	// publish as its own. Refuse — the card falls back to "unobservable", which
	// is what it showed before this source existed.
	return false
}

// grokBillingSnapshotFromRecord validates one record. A record with no usage
// percentage or no observation time is rejected outright: the card's freshness
// rules need both, and a percentage we cannot age is worse than none at all.
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
	if pct := rec.Ctx.Config.CreditUsagePercent; pct != nil && !math.IsNaN(*pct) {
		snap.UsedPercent = clampPercent(*pct)
		snap.HasUsedPercent = true
	}
	if end, err := time.Parse(time.RFC3339, rec.Ctx.Config.CurrentPeriod.End); err == nil {
		snap.PeriodEnd = end
		snap.HasPeriodEnd = true
	}
	// On-demand is a separate, opt-in pool: only plot it when a cap exists, or
	// the row would read as a hard 0-of-0 limit on every subscription account.
	if cap := rec.Ctx.Config.OnDemandCap.Val; cap != nil && *cap > 0 && !math.IsNaN(*cap) {
		snap.HasOnDemand = true
		snap.OnDemandCap = *cap
		// A cap with no reported usage is a pool we know exists but have not
		// observed. Leaving the zero default here would report it as completely
		// unused — an assertion the record never made.
		if used := rec.Ctx.Config.OnDemandUsed.Val; used != nil &&
			!math.IsNaN(*used) && *used >= 0 {
			snap.HasOnDemandUsed = true
			snap.OnDemandUsed = *used
		}
	}
	return snap, true
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
// A period whose end has already passed is reported Unknown WITH its observation
// time rather than as 0% used: the pool has rolled over since Grok last looked,
// and assuming an empty pool would hide usage that may have happened on another
// computer under the same account.
func grokBillingMetrics(snap grokBillingSnapshot, now time.Time) []cliAgentUsageMetric {
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
		// A current period the CLI no longer meters (Grok 1.0+). Report the
		// window — its reset is real and useful — but never a percentage, and no
		// ObservedAt: usage was not observed, and saying otherwise is how a card
		// ends up claiming a six-day-old reading for a live window.
		credits.Unknown = true
		credits.ObservedAt = ""
		if snap.HasPeriodEnd {
			credits.ResetAt = snap.PeriodEnd.UTC().Format(time.RFC3339)
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
			used := snap.OnDemandUsed
			if used > snap.OnDemandCap {
				used = snap.OnDemandCap
			}
			onDemand.Consumed = floatPtr(used)
			onDemand.Remaining = floatPtr(snap.OnDemandCap - used)
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
