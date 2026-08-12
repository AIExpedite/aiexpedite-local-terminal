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
	"os"
	"path/filepath"
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
	UsedPercent      float64
	ObservedAt       time.Time
	PeriodType       string
	PeriodEnd        time.Time
	HasPeriodEnd     bool
	OnDemandCap      float64
	OnDemandUsed     float64
	HasOnDemand      bool
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

// readGrokBillingSnapshot returns the NEWEST billing record in the log tail.
//
// Lines are scanned back-to-front and the first well-formed billing record wins:
// the log is append-only, so the last such line is the most recent fetch. A
// truncated first line (the tail almost always starts mid-record) simply fails
// to unmarshal and is skipped like any other non-billing line.
func readGrokBillingSnapshot(base string) (grokBillingSnapshot, bool) {
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
		return snap, true
	}
	return grokBillingSnapshot{}, false
}

// grokBillingSnapshotFromRecord validates one record. A record with no usage
// percentage or no observation time is rejected outright: the card's freshness
// rules need both, and a percentage we cannot age is worse than none at all.
func grokBillingSnapshotFromRecord(rec grokBillingRecord) (grokBillingSnapshot, bool) {
	if rec.Ctx.Config.CreditUsagePercent == nil {
		return grokBillingSnapshot{}, false
	}
	observed, err := time.Parse(time.RFC3339, rec.TS)
	if err != nil {
		return grokBillingSnapshot{}, false
	}
	snap := grokBillingSnapshot{
		UsedPercent:      clampPercent(*rec.Ctx.Config.CreditUsagePercent),
		ObservedAt:       observed,
		PeriodType:       rec.Ctx.Config.CurrentPeriod.Type,
		SubscriptionTier: rec.Ctx.SubscriptionTier,
	}
	if end, err := time.Parse(time.RFC3339, rec.Ctx.Config.CurrentPeriod.End); err == nil {
		snap.PeriodEnd = end
		snap.HasPeriodEnd = true
	}
	// On-demand is a separate, opt-in pool: only plot it when a cap exists, or
	// the row would read as a hard 0-of-0 limit on every subscription account.
	if cap := rec.Ctx.Config.OnDemandCap.Val; cap != nil && *cap > 0 {
		snap.HasOnDemand = true
		snap.OnDemandCap = *cap
		if used := rec.Ctx.Config.OnDemandUsed.Val; used != nil {
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
		credits.Unknown = true
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
		used := snap.OnDemandUsed
		if used > snap.OnDemandCap {
			used = snap.OnDemandCap
		}
		metrics = append(metrics, cliAgentUsageMetric{
			Kind:       limitKindTokens,
			Label:      "On-demand credits",
			Unit:       "credits",
			Total:      floatPtr(snap.OnDemandCap),
			Consumed:   floatPtr(used),
			Remaining:  floatPtr(snap.OnDemandCap - used),
			ObservedAt: observedAt,
		})
	}
	return metrics
}
