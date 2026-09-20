// cliagent_usage_claudecode_runs.go — what every path that spends a Claude
// inference turn owes the utilization card when that turn completes.
//
// There are three such paths — the direct/native run (claude_native.go), the
// managed terminal session (session.go), and the `__cli_smoke__` probe
// (cliagent_smoke_claudecode.go) — and until the smoke was wired up here they
// disagreed: the first two recorded a post-run refresh obligation, the third
// spent a real turn and recorded nothing, so the three Claude rows kept their
// pre-smoke observedAt.
//
// The contract, in order:
//
//  1. HARVEST first, because it is free. A `--print --output-format json`
//     envelope (and a stream-json `result` event) can already carry the account's
//     `rate_limits` map; extractClaudeRateLimitBuckets understands that shape, so
//     a run whose own output carried telemetry needs no OAuth request at all. On
//     a device where the probe cannot run (API-key auth,
//     disable_claude_usage_probe) this is the ONLY freshness source.
//  2. RECORD the obligation (triggerClaudeUsageProbeAfterRun -> recordOwed) —
//     unless step 1 already covered EVERY displayed row. Newest baseline wins, so
//     a burst of runs leaves exactly one debt.
//  3. PERSIST it, so the debt outlives an agent update — see
//     cliagent_usage_claudecode_pending_run.go. A path whose process may be
//     replaced the moment it returns (the smoke) pays that write synchronously;
//     see noteClaudeTurnSpentDurably.
//
// Retention is unchanged by any of this: the run's stdout is still discarded by
// its caller, and the only thing that reaches disk is the numeric bucket set
// mergeClaudeRateLimitCache already persists.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Bounds for the harvest scan. The smoke retains up to claudeSmokeMaxStdout
// (1 MiB) of a possibly-broken CLI's output, so the scan must be bounded in all
// three dimensions or a health check becomes the thing that stalls the agent.
// A healthy run is one short object, well inside every one of these.
const (
	// claudeUsageHarvestMaxLines caps how many lines are considered. Generous
	// enough for a banner-prefixed or NDJSON build, far short of a spew.
	claudeUsageHarvestMaxLines = 256
	// claudeUsageHarvestMaxBytes caps the total scanned prefix.
	claudeUsageHarvestMaxBytes = 256 << 10 // 256 KiB
	// claudeUsageHarvestMaxLineBytes caps one candidate line before it is handed
	// to the JSON decoder — telemetry envelopes are well under a kilobyte, and
	// a megabyte-long line is a spew, not a reading.
	claudeUsageHarvestMaxLineBytes = 64 << 10 // 64 KiB
	// claudeUsageHarvestMaxWindowBytes caps a persisted window identifier. The
	// longest one Claude Code emits is `seven_day_overage_included` (26); 40
	// leaves room for a suffixed variant and is far short of any path, config
	// fragment or token a leak would carry.
	claudeUsageHarvestMaxWindowBytes = 40
)

// noteClaudeTurnSpent is the path-agnostic "a Claude inference turn just
// completed" entry point. It delegates to triggerClaudeUsageProbeAfterRun so the
// native, managed and smoke paths cannot drift apart on what a completed turn
// means, what it costs, or when it is refused — every bound (arm state, single
// flight, minimum interval, failure backoff, 429 hold) lives there.
//
// Callers must invoke it ONLY for a turn this device actually spent: a cooldown
// replay or a shared single-flight verdict spent none, and a debt recorded for
// one would buy an OAuth request that refreshes nothing.
//
// `completedAt` is the turn's completion instant — the same one the caller
// harvested at. `harvested` says whether that harvest actually persisted
// anything, which is how a run whose own output carried telemetry avoids an
// OAuth request altogether. A zero `completedAt` falls back to now.
//
// Reports whether an obligation was recorded — false when the harvest already
// covered every row, or when this process can never probe at all.
func noteClaudeTurnSpent(completedAt time.Time, harvested bool) bool {
	// A run whose own envelope supplied the reading owes nothing: the cache
	// already shows every displayed row at an instant that can have seen this
	// turn. Checked BEFORE the debt is recorded rather than settled afterwards,
	// so there is no window in which the trailing goroutine reads a debt that is
	// about to be discharged and buys an OAuth request for it.
	if harvested && claudeUsageRunCoveredByOwnTelemetry(completedAt) {
		return false
	}
	return triggerClaudeUsageProbeForTurn(completedAt)
}

// noteClaudeTurnSpentDurably is noteClaudeTurnSpent for a caller whose PROCESS
// may not outlive the turn it just spent — the `__cli_smoke__` probe, whose one
// caller is the CLI-maintenance flow: it smokes, updates the CLI, and the agent
// self-replaces, all inside the same window.
//
// The difference is only WHEN the debt reaches disk. noteClaudeTurnSpent leaves
// that to the trailing goroutine, which is right for the native and managed
// paths: they are driven by the stream scanner, fire once per turn of a chatty
// session, and their process is not about to be killed. But the smoke returns to
// a caller that publishes its verdict and may be replaced before that goroutine
// is ever scheduled — and losing the write in that interval loses the debt
// entirely, recreating the exact stale-card regression the record exists to
// prevent. So the smoke pays the write on its own goroutine, before it returns.
//
// Costs one ~200-byte tmp+rename (plus the cache read the account scope needs)
// per spent smoke turn — trivial beside the CLI child the smoke just waited on,
// and bounded by the 15-minute verdict cooldown. The trailing goroutine still
// calls claudeUsageRecordPendingRun; the coalesce window makes that a no-op for
// the baseline already written here, so this adds no second write.
func noteClaudeTurnSpentDurably(completedAt time.Time, harvested bool) bool {
	if !noteClaudeTurnSpent(completedAt, harvested) {
		return false
	}
	// The gate's own coalesced baseline, not `completedAt`: a concurrent run may
	// already have recorded a newer one, and the record must carry whatever
	// settlement will actually be measured against.
	claudeUsageRecordPendingRun(claudeUsageProbe.owedObservation())
	return true
}

// claudeUsageRunCoveredByOwnTelemetry reports whether the shared cache already
// shows EVERY row the card displays at an observation that can have seen a run
// completing at `completedAt`.
//
// Row-aware (claudeSnapshotFreshness), not "the newest reading anywhere": an
// envelope that carried only `five_hour` leaves the weekly and Fable rows exactly
// as stale as they were, and calling that covered would reintroduce the reported
// defect on two of the three rows. The account scope comes from the caches' own
// snapshots for the same reason claudeUsagePendingRunAccounts uses them — it is
// the identity those buckets are already filed under, and it costs no credential
// read.
//
// EVERY candidate account must be covered, not just the newest cache's. Which of
// them the next gather displays is not knowable without the credential read this
// path refuses to pay, and the two mistakes are not symmetric: calling an
// uncovered run covered suppresses the debt and leaves the card stale (the
// reported defect), while the reverse costs at most one probe under the existing
// bounds. On the single-account device this is the same single check as before.
func claudeUsageRunCoveredByOwnTelemetry(completedAt time.Time) bool {
	accounts := claudeUsagePendingRunAccounts()
	if len(accounts) == 0 {
		return false
	}
	now := time.Now()
	for _, fingerprint := range accounts {
		if !claudeUsageObservationCovers(
			claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint), now), completedAt) {
			return false
		}
	}
	return true
}

// claudeUsageHarvestPrintStdout scans a completed run's own stdout for rate-limit
// telemetry and merges whatever it finds into the shared cache, reporting whether
// anything was persisted.
//
// Free in the sense that matters: no OAuth request, and no credential read beyond
// the one the cache's account scope needs — the same one every other writer of
// this cache performs. Line-tolerant for the same reason
// parseClaudePrintResultEnvelope is — a build that prefixes a banner, or emits
// NDJSON instead of one object, still yields its reading.
//
// Best-effort and silent throughout: this runs after a health check has already
// reached its verdict and must never change it.
func claudeUsageHarvestPrintStdout(stdout []byte, now time.Time) bool {
	captured := false
	nowMs := now.UnixMilli()
	scanned, lines := 0, 0
	rest := stdout
	for len(rest) > 0 && lines < claudeUsageHarvestMaxLines && scanned < claudeUsageHarvestMaxBytes {
		line := rest
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line, rest = rest[:idx], rest[idx+1:]
		} else {
			rest = nil
		}
		lines++
		scanned += len(line) + 1
		if len(line) > claudeUsageHarvestMaxLineBytes {
			continue
		}
		trimmed := strings.TrimSpace(string(line))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		// Same prefilter captureClaudeRateLimitLine applies, hoisted so a line
		// that cannot carry telemetry is never decoded.
		if !strings.Contains(trimmed, "rate_limit") && !strings.Contains(trimmed, "rateLimit") {
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal([]byte(trimmed), &raw) != nil {
			continue
		}
		updates := extractClaudeRateLimitBuckets(raw, nowMs)
		if len(updates) == 0 {
			continue
		}
		// Merge DIRECTLY rather than through captureClaudeRateLimitLine, for two
		// reasons that both matter on this path:
		//
		//   - Retention. bucketFromInfo copies `status` out of the envelope
		//     VERBATIM, so a vendor string lands on disk. Every other writer of
		//     this cache is a live session, where that is long-standing behaviour;
		//     the smoke is the one caller whose contract is "keep no
		//     vendor-authored bytes at all" (see cliagent_smoke_claudecode.go).
		//     claudeUsageProbeStatus is the existing normalizer that collapses the
		//     field to the closed {"", "allowed", "rejected"} set, which is all
		//     any reader of this cache has ever branched on.
		//   - The rejected bucket captureClaudeRateLimitLine returns exists to
		//     drive a SESSION's auto-defer. A completed run has nothing to defer,
		//     and the smoke reports a usage limit through its own
		//     provider_error verdict.
		//   - The window NAME is vendor-authored too, and it lands on disk as a
		//     JSON object KEY, which no normalizer downstream ever touches. A
		//     malformed or hostile CLI that answers with
		//     `{"rate_limits":{"/Users/x/.claude.json":{...}}}` would otherwise
		//     write that string into the cache verbatim. claudeUsageHarvestWindow
		//     keeps the key set to window-shaped identifiers.
		for window, bucket := range updates {
			if !claudeUsageHarvestWindow(window) {
				delete(updates, window)
				continue
			}
			bucket.Status = claudeUsageProbeStatus(bucket.Status)
			updates[window] = bucket
		}
		if len(updates) == 0 {
			continue
		}
		mergeClaudeRateLimitCacheFromSource(claudeRateLimitCachePath(), updates, now,
			currentClaudeAccountFingerprint(), claudeRateLimitSourceStream)
		captured = true
	}
	return captured
}

// claudeUsageHarvestWindow reports whether a harvested `rate_limits` key is
// window-shaped enough to persist under the smoke's "no vendor-authored bytes"
// contract. The key is the one field of an update that reaches disk UNCHANGED —
// it is the cache's JSON object key, so no per-field normalizer downstream can
// clean it up.
//
// Not a closed allowlist of the six claudeWindow* constants, because the read
// side deliberately is not one either: claudeFableWindowIDs carries a tolerant
// tail so an upstream rename or a suffixed variant still draws its row, and
// hard-coding the canonical set here would silently drop exactly the rows a
// probe-less device depends on this harvest for.
//
// Instead: the shape a window identifier has always had, plus a stem from the
// closed set of things Claude actually meters. That admits `seven_day_opus`,
// a renamed `seven_day_fable_v2` and a `fable_weekly`, and refuses everything a
// leak would look like — a path (`/`, `.`, uppercase), a config fragment
// (braces, quotes, whitespace), an OAuth token (`-`, uppercase, far over the
// length cap) and a key-shaped string generally.
func claudeUsageHarvestWindow(window string) bool {
	if len(window) == 0 || len(window) > claudeUsageHarvestMaxWindowBytes {
		return false
	}
	for i := 0; i < len(window); i++ {
		c := window[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return strings.Contains(window, claudeWindowFiveHour) ||
		strings.Contains(window, claudeWindowSevenDay) ||
		strings.Contains(window, "fable")
}
