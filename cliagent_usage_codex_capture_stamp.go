// cliagent_usage_codex_capture_stamp.go — which Codex binary produced the
// utilization reading the cache holds.
//
// Why this exists:
//
//	After a Codex CLI upgrade the card could keep a pre-upgrade reading while
//	every published signal said the CLI was healthy: the rollout scan that
//	recovers a run's telemetry trusted a cursor written by the old binary and a
//	layout the new one may no longer write. Stamping each observation with the
//	binary that produced it lets the card say WHY it is old (capture drift)
//	instead of promising a telemetry find that is not coming, and gives the
//	rollout scan a reason to reset its cursor exactly once per binary change.
//
// Two stamps, two questions (codexRateLimitSnapshot):
//
//   - CodexVersion — the binary that produced the newest numeric observation.
//     Written only by the contributor merge, whenever it advances one.
//   - RolloutCursorVersion — the binary in use when the rollout scan cursor was
//     last reset or first established. Written only by the pre-scan reset in
//     codexReconcileFromRollout.
//
// Collapsing them would let a live-probe capture move the reading's stamp to
// the new binary and silently clear the drift signal before the scan ever
// reset a cursor written against the old layout.
//
// Every value here is a closed, locally derived version string or timestamp —
// never a path, an account, or vendor text.
package main

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// codexUsageCaptureVersion is the `--version` answer of the Codex binary this
// process is capturing for. Published by a gather (detected.Version), a smoke
// (its own version argument) and codexResolveCaptureVersion (startup replay,
// live fallback). Every publisher reads the same installed binary, so racing
// writes are benign; the atomic only guarantees a stamp is never a torn string.
var codexUsageCaptureVersion atomic.Value // string

// publishCodexUsageCaptureVersion records the producing binary's version. An
// empty version (a failed `--version` probe) never overwrites a known one.
func publishCodexUsageCaptureVersion(version string) {
	if version = strings.TrimSpace(version); version != "" {
		codexUsageCaptureVersion.Store(version)
	}
}

// currentCodexUsageCaptureVersion is the version captures are stamped with, or
// "" while no publisher has named the binary yet.
func currentCodexUsageCaptureVersion() string {
	v, _ := codexUsageCaptureVersion.Load().(string)
	return v
}

// codexInstalledVersion reads the installed Codex binary's version through the
// same (path, mtime, size)-keyed cache the gather and the smoke use, so on any
// process that has gathered since the binary changed it spawns nothing. A var
// so tests never reach a real install.
var codexInstalledVersion = func() string {
	path := resolveCodexSmokePath()
	if path == "" {
		return ""
	}
	return codexProbeVersion(path)
}

// codexResolveCaptureVersion publishes the installed binary's version for a
// capture made outside both a gather and a smoke. payOwedCodexUsageRefresh
// runs at startup before either, so without this its reconcile and live
// fallback would stamp nothing, leave the pre-upgrade stamp in place, and have
// the first gather raise capture drift against a reading just refreshed.
func codexResolveCaptureVersion() string {
	publishCodexUsageCaptureVersion(codexInstalledVersion())
	return currentCodexUsageCaptureVersion()
}

// codexCaptureDrift reports whether the reading the cache holds was produced
// by a binary other than the one installed now. Empty on either side is not
// drift: an empty stamp is a first-ever (or pre-stamp) reading, an empty
// detected version is a failed `--version` probe. A downgrade IS drift — the
// predicate is about difference, not ordering.
func codexCaptureDrift(stamped, detected string) bool {
	stamped, detected = strings.TrimSpace(stamped), strings.TrimSpace(detected)
	return stamped != "" && detected != "" && stamped != detected
}

// codexCaptureDriftFromView applies codexCaptureDrift to the account's cache.
func codexCaptureDriftFromView(view codexCacheView, detectedVersion string) bool {
	return codexCaptureDrift(view.codexVersion, detectedVersion)
}

// codexCaptureDriftNotice explains a stale card whose reading predates a Codex
// binary change. It inherits codexStaleRunNotice's gate — the bounded
// attempts are spent and the live fallback has resolved — so an upgrade alone
// never raises it. Version strings and timestamps only.
func codexCaptureDriftNotice(state codexRunFreshnessState, detectedVersion string) string {
	if !codexStaleRunNoticeDue(state) || !codexCaptureDrift(state.codexVersion, detectedVersion) {
		return ""
	}
	const layout = "2006-01-02 15:04 UTC"
	stamped, installed := codexNoticeVersion(state.codexVersion), codexNoticeVersion(detectedVersion)
	last := fmt.Sprintf("No Codex utilization reading has been observed since Codex build %q", stamped)
	if !state.latest.IsZero() {
		last = fmt.Sprintf("Codex utilization was last observed %s by Codex build %q", state.latest.UTC().Format(layout), stamped)
	}
	return fmt.Sprintf("%s; the installed build %q has not reported utilization since the most recent Codex run started (%s). It will update once that build's telemetry is captured.",
		last, installed, state.floor.UTC().Format(layout))
}

// codexNoticeVersion bounds a version string before it reaches a notice. The
// value is a `--version` first line, so this only guards against a build that
// prints something unexpectedly long.
func codexNoticeVersion(version string) string {
	const maxBytes = 64
	version = strings.TrimSpace(version)
	if len(version) > maxBytes {
		version = version[:maxBytes]
	}
	return version
}

// codexRunFreshnessNotice is the run-freshness notice the card shows: capture
// drift when the stale reading predates a binary change, the generic stale-run
// notice otherwise, or "" while the reading is current or still being chased.
func codexRunFreshnessNotice(state codexRunFreshnessState, detectedVersion string) string {
	if notice := codexCaptureDriftNotice(state, detectedVersion); notice != "" {
		return notice
	}
	return codexStaleRunNotice(state)
}

// codexStampCaptureVersion stamps the snapshot with the producing binary when
// a merge advanced a contributor observation. An unknown producer leaves the
// existing stamp untouched: a capture we cannot name must not erase the
// evidence that the previous one came from the previous binary.
func codexStampCaptureVersion(snap *codexRateLimitSnapshot, before map[string]int64) bool {
	if !codexContributorObservationAdvanced(before, snap.Contributors) {
		return false
	}
	if version := currentCodexUsageCaptureVersion(); version != "" {
		snap.CodexVersion = version
	}
	return true
}

// codexContributorObservationTimes indexes every contributor's observation
// time by (window, limit), so a merge can tell whether it moved any of them.
func codexContributorObservationTimes(contributors map[string]map[string]codexRateLimitBucket) map[string]int64 {
	out := map[string]int64{}
	for window, limits := range contributors {
		for limit, bucket := range limits {
			out[window+"\x00"+limit] = bucket.ObservedAtMs
		}
	}
	return out
}

// codexContributorObservationAdvanced reports whether any contributor is new
// or observed later than it was before the merge. Per contributor rather than
// the newest overall: a fresh reading of one limit is still a fresh reading
// when another limit happens to carry a later timestamp.
func codexContributorObservationAdvanced(before map[string]int64, after map[string]map[string]codexRateLimitBucket) bool {
	for window, limits := range after {
		for limit, bucket := range limits {
			if bucket.ObservedAtMs <= 0 {
				continue
			}
			if prev, ok := before[window+"\x00"+limit]; !ok || bucket.ObservedAtMs > prev {
				return true
			}
		}
	}
	return false
}
