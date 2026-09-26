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
	"sync"
	"sync/atomic"
)

// codexUsageCaptureVersion is the `--version` answer of the Codex binary this
// process is capturing for. Published by a gather (detected.Version), a smoke
// (its own version argument) and codexResolveCaptureVersion (startup replay,
// live fallback). Every publisher reads the same installed binary, so racing
// writes are benign; the atomic only guarantees a stamp is never a torn string.
// A long-lived child snapshots it at spawn and stamps with that pinned value
// (captureCodexRateLimitLineFromProducer), so an upgrade published while it
// runs is not credited to its pre-upgrade telemetry.
var codexUsageCaptureVersion atomic.Value // codexCaptureVersionStamp

// codexCaptureVersionStamp is a published version together with the identity
// of the binary it was validated against. The identity is what lets a LATER
// reader (a child about to be spawned) tell that the published build is no
// longer the one installed: the installer takes none of our locks, so the file
// can be replaced between a publish and the launch that pins it. A zero
// identity means the publisher could not name a binary for the reading.
type codexCaptureVersionStamp struct {
	version  string
	identity versionProbeKey
}

// publishCodexUsageCaptureVersion records the producing binary's version with
// no identity attached. An empty version (a failed `--version` probe) never
// overwrites a known one.
func publishCodexUsageCaptureVersion(version string) {
	publishCodexUsageCaptureVersionWithIdentity(version, versionProbeKey{})
}

func publishCodexUsageCaptureVersionWithIdentity(version string, identity versionProbeKey) {
	if version = codexNormalizeVersion(version); version != "" {
		codexUsageCaptureVersion.Store(codexCaptureVersionStamp{version: version, identity: identity})
	}
}

// publishCodexUsageCaptureVersionFrom publishes a version that was read from a
// SPECIFIC binary, and refuses when that reading is already stale.
//
// The periodic gather and a demand-driven refresh are independent callers, so
// one pass can detect the pre-upgrade build, be overtaken by an upgrade and a
// second pass publishing the new build, and only then reach its own publish —
// rolling the process-global stamp back to the old version. Captures made by
// the NEW binary would then be stamped old, and the rollout cursor reset
// against the wrong version, until another pass corrected it.
//
// The version-probe cache already keys on binary identity (path, mtime, size),
// so this check is a stat plus a map read and never spawns: a version publishes
// only when that cache holds it for the binary AS IT IS NOW.
//
// A cache MISS is a refusal, not a pass. Every caller here reads its version
// through codexProbeVersion, which records it under the identity it probed, so
// a hit is the ordinary case and a miss means the file moved underneath the
// reading — the binary was replaced mid-probe, or it cannot be stat'ed at all.
// Accepting a miss was the hole: a probe that raced a replacement leaves no
// entry for the installed binary, and the stale version would then publish
// unchallenged and roll the process-global stamp back. Refusing leaves the
// existing stamp in place, which is what an unnameable capture is supposed to
// do, and the next pass re-probes and publishes the build that is really there.
//
// The identity check and the store happen under one lock. Checked and stored
// separately, a caller could pass the check, stall while an upgrade lands and
// a newer pass publishes the replacement build, then store its now-stale
// version anyway. Under the lock a publisher either stores before the newer
// one (which then overwrites it) or checks after it, when the cache already
// names the replacement and the stale version is refused.
//
// The validated identity is KEPT with the published version. The mutex orders
// publishers against each other but not against the installer, so a binary
// replaced between the peek and the store is published as installed with no
// newer publisher around to correct it — and a child spawned in that window
// would pin the gone build and stamp its new-build telemetry old for its whole
// life. Carrying the identity lets a launch re-check it with a single stat and
// pin nothing (unknown producer) rather than the wrong version.
func publishCodexUsageCaptureVersionFrom(path, version string) {
	codexCaptureVersionPublishMu.Lock()
	defer codexCaptureVersionPublishMu.Unlock()
	if path == "" {
		publishCodexUsageCaptureVersion(version)
		return
	}
	installed, identity, ok := peekCachedProbeVersionIdentity(path)
	if !ok || codexNormalizeVersion(installed) != codexNormalizeVersion(version) {
		return
	}
	publishCodexUsageCaptureVersionWithIdentity(version, identity)
}

// codexCaptureVersionPublishMu makes publishCodexUsageCaptureVersionFrom's
// identity check and store one step. Taken before versionProbeMu (inside
// peekCachedProbeVersionIdentity), never the other way round.
var codexCaptureVersionPublishMu sync.Mutex

// codexVersionMaxBytes bounds a `--version` first line before it is persisted,
// compared or shown. Only a build printing something unexpectedly long is
// affected.
const codexVersionMaxBytes = 64

// codexNormalizeVersion is the one form a version takes everywhere here —
// trimmed and rune-safely bounded — so a stamp and a detected version of the
// same build always compare equal.
func codexNormalizeVersion(version string) string {
	return clampAntigravityQuotaField(version, codexVersionMaxBytes)
}

// currentCodexUsageCaptureVersion is the version captures are stamped with, or
// "" while no publisher has named the binary yet.
func currentCodexUsageCaptureVersion() string {
	return currentCodexUsageCaptureStamp().version
}

func currentCodexUsageCaptureStamp() codexCaptureVersionStamp {
	stamp, _ := codexUsageCaptureVersion.Load().(codexCaptureVersionStamp)
	return stamp
}

// codexInstalledCaptureVersion is the published version, but only while the
// binary it was read from is still the file on disk — the check a launch needs
// and a stamp of already-received telemetry does not. A replacement that
// landed after the publish yields "" (unknown producer), which leaves the
// cache's existing stamp untouched instead of crediting the removed build with
// the new one's telemetry. A version published without an identity (a test, or
// a caller that had no path) is returned as-is: there is nothing to invalidate.
func codexInstalledCaptureVersion() string {
	stamp := currentCodexUsageCaptureStamp()
	if stamp.version == "" || stamp.identity.Path == "" {
		return stamp.version
	}
	if !versionProbeIdentityCurrent(stamp.identity) {
		return ""
	}
	return stamp.version
}

// codexInstalledVersion reads the installed Codex binary's version through the
// same (path, mtime, size)-keyed cache the gather and the smoke use, so on any
// process that has gathered since the binary changed it spawns nothing. It
// returns the path it probed so the caller can validate the reading against
// that binary's identity. A var so tests never reach a real install.
var codexInstalledVersion = func() (path, version string) {
	path = resolveCodexSmokePath()
	if path == "" {
		return "", ""
	}
	return path, codexProbeVersion(path)
}

// codexResolveCaptureVersion publishes the installed binary's version for a
// capture made outside both a gather and a smoke. payOwedCodexUsageRefresh
// runs at startup before either, so without this its reconcile and live
// fallback would stamp nothing, leave the pre-upgrade stamp in place, and have
// the first gather raise capture drift against a reading just refreshed.
//
// It publishes through the same identity check as a gather: a probe that
// raced a binary replacement hands its stale reading back uncached, and
// publishing it unchecked would roll the stamp back to a build that is gone.
func codexResolveCaptureVersion() {
	publishCodexUsageCaptureVersionFrom(codexInstalledVersion())
}

// codexCaptureVersionForLaunch is the version a managed child launched from
// command (resolved to executable) stamps its telemetry with. A bare `codex`
// resolves through PATH exactly as detection does, so it is the published
// installed build. An explicit side-by-side path may be a different build:
// it stamps only a version already probed for that exact binary, and
// otherwise stays unknown — which leaves the existing stamp untouched rather
// than crediting the installed build with another binary's telemetry.
func codexCaptureVersionForLaunch(command, executable string) string {
	if !isExplicitPath(command) {
		return codexInstalledCaptureVersion()
	}
	if v, ok := peekCachedProbeVersion(executable); ok {
		return codexNormalizeVersion(v)
	}
	return ""
}

// codexRolloutProducerVersion maps a rollout header's `cli_version` onto the
// installed build's version string, which is what the stamp and the drift
// predicate compare. It returns the installed version only when the header
// names that same build; a rollout written by any other build — including a
// pre-upgrade process still appending after the cursor reset — or one with no
// recorded version returns "", so its evidence never claims the installed
// build produced it.
func codexRolloutProducerVersion(headerVersion, installed string) string {
	header := strings.TrimPrefix(strings.TrimSpace(headerVersion), "v")
	installed = codexNormalizeVersion(installed)
	if header == "" || installed == "" {
		return ""
	}
	for _, field := range strings.Fields(installed) {
		if strings.TrimPrefix(field, "v") == header {
			return installed
		}
	}
	return ""
}

// codexCaptureDrift reports whether the reading the cache holds was produced
// by a binary other than the one installed now. Empty on either side is not
// drift: an empty stamp is a first-ever (or pre-stamp) reading, an empty
// detected version is a failed `--version` probe. A downgrade IS drift — the
// predicate is about difference, not ordering.
func codexCaptureDrift(stamped, detected string) bool {
	stamped, detected = codexNormalizeVersion(stamped), codexNormalizeVersion(detected)
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
	stamped, installed := codexNormalizeVersion(state.codexVersion), codexNormalizeVersion(detectedVersion)
	last := fmt.Sprintf("No Codex utilization reading has been observed since Codex build %q", stamped)
	if !state.latest.IsZero() {
		last = fmt.Sprintf("Codex utilization was last observed %s by Codex build %q", state.latest.UTC().Format(layout), stamped)
	}
	return fmt.Sprintf("%s; the installed build %q has not reported utilization since the most recent Codex run started (%s). It will update once that build's telemetry is captured.",
		last, installed, state.floor.UTC().Format(layout))
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
// a merge advanced a contributor observation, or applied an authoritative
// clear — a full snapshot saying a window no longer applies is still a
// reading this binary produced, and leaving the old stamp would report drift
// against a build that did answer. An unknown producer leaves the existing
// stamp untouched: a capture we cannot name must not erase the evidence that
// the previous one came from the previous binary. Reports only the advance.
// producerVersion is the binary that produced THIS evidence — pinned by the
// caller, since a child spawned before an upgrade still runs the old build.
func codexStampCaptureVersion(snap *codexRateLimitSnapshot, before map[string]int64, authoritativeClear bool, producerVersion string) bool {
	advanced := codexContributorObservationAdvanced(before, snap.Contributors)
	if !advanced && !authoritativeClear {
		return false
	}
	if version := codexNormalizeVersion(producerVersion); version != "" {
		snap.CodexVersion = version
	}
	return advanced
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

// codexResetUsageCaptureVersion clears the published stamp. Test seam: a
// publish never clears (an empty version must not overwrite a known one), and
// a case that asserts the unknown-producer behaviour needs to start there.
func codexResetUsageCaptureVersion() {
	codexUsageCaptureVersion.Store(codexCaptureVersionStamp{})
}
