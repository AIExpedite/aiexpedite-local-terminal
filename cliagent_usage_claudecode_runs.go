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
//     cliagent_usage_claudecode_pending_run.go.
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
func noteClaudeTurnSpent(completedAt time.Time, harvested bool) {
	// A run whose own envelope supplied the reading owes nothing: the cache
	// already shows every displayed row at an instant that can have seen this
	// turn. Checked BEFORE the debt is recorded rather than settled afterwards,
	// so there is no window in which the trailing goroutine reads a debt that is
	// about to be discharged and buys an OAuth request for it.
	if harvested && claudeUsageRunCoveredByOwnTelemetry(completedAt) {
		return
	}
	triggerClaudeUsageProbeForTurn(completedAt)
}

// claudeUsageRunCoveredByOwnTelemetry reports whether the shared cache already
// shows EVERY row the card displays at an observation that can have seen a run
// completing at `completedAt`.
//
// Row-aware (claudeSnapshotFreshness), not "the newest reading anywhere": an
// envelope that carried only `five_hour` leaves the weekly and Fable rows exactly
// as stale as they were, and calling that covered would reintroduce the reported
// defect on two of the three rows. The account scope comes from the cache's own
// snapshot for the same reason claudeUsagePendingRunFingerprint uses it — it is
// the identity those buckets are already filed under, and it costs no credential
// read.
func claudeUsageRunCoveredByOwnTelemetry(completedAt time.Time) bool {
	fingerprint, scoped := claudeUsagePendingRunFingerprint()
	if !scoped {
		return false
	}
	now := time.Now()
	return claudeUsageObservationCovers(
		claudeSnapshotFreshness(loadMergedClaudeRateLimitView(fingerprint), now), completedAt)
}

// claudeUsageHarvestPrintStdout scans a completed run's own stdout for rate-limit
// telemetry and merges whatever it finds into the shared cache, reporting whether
// anything was persisted.
//
// Free in the sense that matters: no OAuth request, no credential read beyond the
// one captureClaudeRateLimitLine already performs for the cache's account scope.
// Line-tolerant for the same reason parseClaudePrintResultEnvelope is — a build
// that prefixes a banner, or emits NDJSON instead of one object, still yields its
// reading.
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
		// Decode once here purely to answer "did this line hold any window?" —
		// captureClaudeRateLimitLine reports only the REJECTED bucket, and an
		// allowed reading (the normal case) is indistinguishable from "nothing
		// found" in its return value. The merge itself stays in that one function
		// so every writer of this cache goes through the same path.
		var raw map[string]interface{}
		if json.Unmarshal([]byte(trimmed), &raw) != nil {
			continue
		}
		if len(extractClaudeRateLimitBuckets(raw, nowMs)) == 0 {
			continue
		}
		captureClaudeRateLimitLine(trimmed, now)
		captured = true
	}
	return captured
}
