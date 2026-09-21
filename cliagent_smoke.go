// cliagent_smoke.go — the provider-agnostic core of the signed `__cli_smoke__`
// operational command: the closed diagnostic set, the metric-only result, the
// per-CLI cooldown + singleflight, the per-binary resolved-shape cache, and the
// provider table that routes a cliId to its probe.
//
// The CLI-maintenance harness runs a smoke once before and once after a CLI
// upgrade to prove the binary can still complete a round trip. The probes live
// HERE, in this process, because it is the only component that knows the
// resolved binary path, the sanitized child env, and the flag shape that
// actually works for each CLI (claude_argv.go, grok_argv.go).
//
// Per-provider probes:
//   - claudeCode → cliagent_smoke_claudecode.go (runClaudeCodeSmoke)
//   - grok       → cliagent_smoke_grok.go       (runGrokSmoke)
//
// Cost discipline — a smoke spends ONE real inference turn against the user's
// own subscription window, the same quota the CLI Agents tab reports:
//   - Local pre-checks (binary present, version answers, logged in)
//     short-circuit to provider_unavailable / not_authenticated BEFORE a turn is
//     spent.
//   - A per-CLI cooldown serves the previous verdict to any caller that asks
//     again too soon, so a sequential retry storm cannot drain the user's quota.
//   - A per-(CLI, binary) singleflight collapses CONCURRENT callers onto one run. The
//     cooldown cannot do that on its own: Pub/Sub delivers several outstanding
//     messages at a time, so a burst would otherwise have every callback miss
//     the same empty cache and spend its own turn.
//   - The cooldown is INVALIDATED by a binary change (path/mtime/size/version),
//     because that is exactly the post-upgrade smoke the harness must not be
//     served a stale pre-upgrade answer for — and by a logout, which changes
//     the answer without changing the binary.
//   - Only verdicts that actually SPENT a turn are cached. A free pre-check
//     verdict (no binary, not logged in) buys no quota by being pinned and
//     would keep reporting a broken CLI for 15 minutes after the user fixed it.
//   - A provider's argv ladder retries ONLY on a pre-inference flag rejection
//     (option parsing precedes inference, so a rejected rung costs no quota),
//     and at most once per smoke.
//
// Retention discipline — the child's stdout and stderr are read to derive a
// verdict and are then DISCARDED. They are not published, and they are not
// written to this device's log either: the agent log rotates to disk and can be
// uploaded on request, so "local" is not a safety property. Neither a byte cap
// nor the denylist redactor can guarantee removal of a raw config fragment, a
// private path, a short password, or a credential format the patterns do not
// know — so the only sound rule is to keep no vendor-authored bytes at all.
// What leaves this package is the closed cliSmokeDiagnostic set, the fixed argv
// shape id, and counts. The marker nonce, the prompt, the resolved argv and any
// config content are never included at any severity.

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// cliSmokeCooldown is the minimum spacing between two quota-spending smokes
	// for the same CLI on this device. Within it, the previous verdict is
	// replayed. Reset on agent restart (in-memory) and bypassed when the binary
	// itself changed — see cliSmokeBinaryStamp.
	cliSmokeCooldown = 15 * time.Minute

	cliSmokeStatusSuccess = "success"
	cliSmokeStatusFailed  = "failed"
)

// cliSmokeDiagnostic is the CLOSED set of locally-derived diagnostics the
// result may carry. It exists because the alternative — publishing the child's
// stderr tail — is unsafe at any cap: stderr is arbitrary vendor text that can
// contain a settings fragment, a private path, or a credential shape no
// denylist regex anticipates. A redactor can only remove what it recognises,
// so the fix is to publish a code we authored, never text the CLI authored.
//
// Every value here is a compile-time constant chosen by our own classifiers;
// nothing derived from the child's bytes ever reaches the wire — or the log
// (see claudeSmokeFailureLogLine / grokSmokeFailureLogLine, which cannot even
// accept text).
const (
	cliSmokeDiagnosticNone = "" // success, or nothing further to say
	// The CLI rejected one of OUR flags — the one failure that is safe to
	// retry on the next argv rung, because it happens during option parsing,
	// before any inference.
	cliSmokeDiagnosticFlagRejected = "flag_rejected"
	// The CLI rejected the input/output framing contract (the Claude 2.1.x
	// `--input-format=stream-json requires output-format=stream-json` class,
	// or a Grok build refusing `--output-format=streaming-json`).
	cliSmokeDiagnosticFramingRejected = "framing_rejected"
	// Exited without emitting the documented terminal envelope (Claude's
	// `result` object, Grok's `end` frame).
	cliSmokeDiagnosticNoEnvelope = "no_envelope"
	// A well-formed envelope reporting an auth failure.
	cliSmokeDiagnosticAuthError = "auth_error"
	// A well-formed envelope reporting a provider-side refusal (API error
	// status, overloaded, usage limit).
	cliSmokeDiagnosticProviderError = "provider_error"
	// A well-formed successful envelope whose text was not the marker.
	cliSmokeDiagnosticMarkerMismatch = "marker_mismatch"
	// Our per-attempt deadline killed the child.
	cliSmokeDiagnosticTimeout = "timeout"
	// Pre-check failures — no turn was spent.
	cliSmokeDiagnosticBinaryMissing = "binary_missing"
	cliSmokeDiagnosticNotLoggedIn   = "not_logged_in"
	cliSmokeDiagnosticInternal      = "internal"
	// An unrecognised cliId reached the probe.
	cliSmokeDiagnosticUnknownCLI = "unknown_cli"
)

// cliSmokeResult is the entire published payload of `__cli_smoke_result__`.
// Every field is a metric, a closed-enum value, or a fixed identifier. By
// construction there is NO field that can carry CLI stdout, CLI stderr, prompt
// text, argv values or config content — the diagnostic is a code we chose, and
// the shape id names a ladder entry rather than the flags it produces.
type cliSmokeResult struct {
	CliID         string `json:"cliId"`
	Version       string `json:"version,omitempty"`
	Status        string `json:"status"`
	ErrorCategory string `json:"errorCategory,omitempty"`
	MarkerMatched bool   `json:"markerMatched"`
	DurationMs    int64  `json:"durationMs"`
	// ArgvShapeID names the ladder entry that ran (claudeArgvShapes /
	// grokSmokeArgvShapes), never the argv itself.
	ArgvShapeID string `json:"argvShapeId,omitempty"`
	// Diagnostic is one of the cliSmokeDiagnostic* constants — a locally
	// authored code, never vendor text.
	Diagnostic string `json:"diagnostic,omitempty"`
}

/* --------------------------------------------------------------------------
   Bounded capture
   -------------------------------------------------------------------------- */

// cliSmokeMaxStdout / cliSmokeMaxStderr bound what one probe attempt RETAINS
// in memory.
//
// A health check must not be the thing that kills the agent: a broken or
// half-upgraded CLI can spew for the full per-attempt timeout, and an unbounded
// bytes.Buffer would follow it until the tray process is OOM-killed — losing
// the very failure report the smoke exists to publish. Neither cap can truncate
// a healthy run: stdout carries one terminal envelope (a marker echo is well
// under a kilobyte), and stderr is read solely to pick between a handful of
// constants, which match on text a CLI emits in its first line, not its
// millionth.
const (
	cliSmokeMaxStdout = 1 << 20  // 1 MiB
	cliSmokeMaxStderr = 64 << 10 // 64 KiB
)

// boundedBuffer keeps at most `limit` bytes and DISCARDS the rest while still
// reporting a successful short-circuit-free write.
//
// Returning (len(p), nil) unconditionally is the load-bearing half: os/exec
// copies the child's pipes on its own goroutines and abandons the copy on a
// writer error, which would leave the child blocked on a full pipe until the
// deadline killed it — turning "noisy CLI" into "every smoke times out". So the
// excess is drained and dropped rather than refused.
type boundedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

/* --------------------------------------------------------------------------
   Binary stamp + resolved-shape cache (per binary)
   -------------------------------------------------------------------------- */

// cliSmokeBinaryStamp identifies a specific CLI binary: its path, its
// (mtime, size), and the version it reports. The same triple systemInfo.go's
// version-probe cache keys on, plus the version string so a rebuild that
// preserves mtime/size but bumps the version still reads as a change.
//
// It keys the smoke cooldown: a verdict cached before an upgrade must never be
// served for the post-upgrade binary.
func cliSmokeBinaryStamp(path, version string) string {
	if path == "" {
		return "absent"
	}
	info, err := os.Stat(path)
	if err != nil {
		return path + "|?|?|" + version
	}
	return fmt.Sprintf("%s|%d|%d|%s", path, info.ModTime().UnixNano(), info.Size(), version)
}

// cliSmokeShapeKey is keyed on (path, mtime, size) — the same key the
// `--version` probe uses in systemInfo.go. A reinstall or upgrade changes the
// key, so a shape that stopped working is re-resolved exactly when the binary
// changes and never re-probed in between. Paths are unique per CLI, so one
// map serves every provider.
type cliSmokeShapeKey struct {
	Path    string
	ModUnix int64
	Size    int64
}

var (
	cliSmokeShapeMu    sync.Mutex
	cliSmokeShapeCache = map[cliSmokeShapeKey]string{}
)

func cliSmokeShapeKeyFor(path string) (cliSmokeShapeKey, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return cliSmokeShapeKey{}, false
	}
	return cliSmokeShapeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}, true
}

// cliSmokeRememberedShape reports the shape id previously resolved for this
// exact binary, if any. Providers use it to collapse their ladder to that one
// entry so a steady-state smoke spawns one child rather than walking the
// ladder again.
func cliSmokeRememberedShape(path string) (string, bool) {
	key, ok := cliSmokeShapeKeyFor(path)
	if !ok {
		return "", false
	}
	cliSmokeShapeMu.Lock()
	defer cliSmokeShapeMu.Unlock()
	resolved, cached := cliSmokeShapeCache[key]
	return resolved, cached
}

// cliSmokeShapeBinding pins a ladder walk to the binary it started against.
// A smoke can outlive the binary it probed (an upgrade lands mid-run), and a
// shape is only a fact about the bytes that accepted it. Re-stating the path
// on the way out would file the OLD binary's winning shape under the NEW
// binary's key — and, because a post-upgrade flight writes the same key, the
// late walk would clobber the shape that flight had already resolved. Every
// later smoke (including the legacy session_start one, which reads the same
// cache) would then collapse its ladder onto a rung the installed build may
// reject. Same discipline runCLISmoke applies to the cooldown verdict.
type cliSmokeShapeBinding struct {
	path  string
	key   cliSmokeShapeKey
	known bool
}

// bindCLISmokeShape captures the binary identity BEFORE the first child is
// launched. An unstattable path yields an unknown binding, which never writes.
func bindCLISmokeShape(path string) cliSmokeShapeBinding {
	key, ok := cliSmokeShapeKeyFor(path)
	return cliSmokeShapeBinding{path: path, key: key, known: ok}
}

// remember stores the winning shape only while the binary on disk is still the
// one this walk probed.
func (b cliSmokeShapeBinding) remember(shapeID string) {
	if !b.known {
		return
	}
	current, ok := cliSmokeShapeKeyFor(b.path)
	if !ok || current != b.key {
		return
	}
	rememberCLISmokeShape(b.key, shapeID)
}

func rememberCLISmokeShape(key cliSmokeShapeKey, shapeID string) {
	path := key.Path
	cliSmokeShapeMu.Lock()
	// Drop stale entries for the same path first: an upgrade leaves the old
	// (mtime,size) key behind and nothing else prunes this map.
	for k := range cliSmokeShapeCache {
		if k.Path == path && k != key {
			delete(cliSmokeShapeCache, k)
		}
	}
	cliSmokeShapeCache[key] = shapeID
	cliSmokeShapeMu.Unlock()
}

// resetCLISmokeState clears the resolved-shape and cooldown caches.
// Test-only seam.
func resetCLISmokeState() {
	cliSmokeShapeMu.Lock()
	cliSmokeShapeCache = map[cliSmokeShapeKey]string{}
	cliSmokeShapeMu.Unlock()
	cliSmokeCooldownMu.Lock()
	cliSmokeCooldownCache = map[string]cliSmokeCooldownEntry{}
	cliSmokeCooldownMu.Unlock()
}

/* --------------------------------------------------------------------------
   Cooldown
   -------------------------------------------------------------------------- */

type cliSmokeCooldownEntry struct {
	At     time.Time
	Stamp  string
	Result cliSmokeResult
}

var (
	cliSmokeCooldownMu    sync.Mutex
	cliSmokeCooldownCache = map[string]cliSmokeCooldownEntry{}
)

// cliSmokeGroup collapses concurrent smokes for the same CLI into ONE
// execution. The cooldown alone cannot do that: Pub/Sub delivers several
// outstanding messages in parallel, so a retry burst could have every callback
// observe the same cache miss and independently spawn a quota-spending probe —
// N inference turns against the user's own subscription window despite a
// cooldown that promises one. Followers block on the leader and are handed its
// verdict. Same singleflight the token refresh in auth.go uses.
//
// The key is (cliId, binary stamp), not the cliId alone: a caller arriving
// after an upgrade replaced the binary mid-flight must test the NEW binary, not
// join the pre-upgrade leader and be handed its verdict — that would bypass the
// cooldown's binary-change invalidation exactly when the smoke matters.
var cliSmokeGroup singleflight.Group

// cliSmokeFlightKey scopes a singleflight to callers probing the same binary.
func cliSmokeFlightKey(cliID, stamp string) string {
	return cliID + "\x00" + stamp
}

/* --------------------------------------------------------------------------
   Provider table
   -------------------------------------------------------------------------- */

// cliSmokeProvider is one row of the provider table: how to find the binary,
// how to read its version, a FREE login re-check the replay path uses, and the
// probe itself. Every function is looked up at call time (closures over the
// package-level seams), so a test that swaps a seam sees it honoured here.
type cliSmokeProvider struct {
	// resolvePath RE-RESOLVES the binary rather than reading any memo: the
	// point of the post-update smoke is to validate a binary that was just
	// replaced.
	resolvePath func() string
	// probeVersion answers `--version` for the resolved path ("" when the
	// binary cannot answer).
	probeVersion func(path string) string
	// loggedIn is the local, non-quota login re-check the cooldown replay
	// runs because authentication is not part of the binary stamp. `known`
	// false means inconclusive, which replays (see replayableCLISmokeVerdict).
	loggedIn func(ctx context.Context, path string) (loggedIn, known bool)
	// run performs the probe against an already-resolved binary.
	run func(ctx context.Context, path, version string) cliSmokeResult
}

// cliSmokeProviders is keyed by the catalog cliId the harness sends as the
// command's first arg. An id missing here never spawns anything.
var cliSmokeProviders = map[string]cliSmokeProvider{
	"claudeCode": {
		resolvePath:  func() string { return resolveClaudeSmokePath() },
		probeVersion: cachedProbeVersion,
		loggedIn:     func(ctx context.Context, path string) (bool, bool) { return claudeAuthStatusProbe(ctx, path) },
		run:          runClaudeCodeSmoke,
	},
	"grok": {
		resolvePath:  func() string { return resolveGrokSmokePath() },
		probeVersion: grokProbeVersion,
		loggedIn:     grokSmokeLoggedIn,
		run:          runGrokSmoke,
	},
}

// runCLISmoke is the entry point the `__cli_smoke__` handler calls. It resolves
// the CLI, applies the cooldown, and returns (result, servedWithoutSpendingATurn)
// — true both for a cooldown replay and for a caller that shared a concurrent
// leader's run.
//
// An unrecognised cliId returns provider_unavailable WITHOUT spawning anything
// — it must never fall through to a generic execute.
func runCLISmoke(ctx context.Context, cliID string) (cliSmokeResult, bool) {
	provider, known := cliSmokeProviders[cliID]
	if !known {
		return cliSmokeResult{
			CliID:         cliID,
			Status:        cliSmokeStatusFailed,
			ErrorCategory: cliUsageErrorProviderUnavailable,
			Diagnostic:    cliSmokeDiagnosticUnknownCLI,
		}, false
	}

	path := provider.resolvePath()
	version := ""
	if path != "" {
		version = provider.probeVersion(path)
	}
	stamp := cliSmokeBinaryStamp(path, version)

	// `executed` is written only by the closure below, and singleflight runs
	// that closure synchronously in the LEADER's own goroutine — a follower's
	// closure never runs, so its copy stays false. That makes "did this call
	// spend a turn?" answerable without racing, which singleflight's own
	// `shared` flag cannot do (it reports true for the leader too whenever
	// anyone waited on it).
	executed := false
	// The cooldown lookup lives INSIDE the group so a burst collapses onto one
	// lookup — including its auth re-check, which may spawn a child of its own.
	v, _, _ := cliSmokeGroup.Do(cliSmokeFlightKey(cliID, stamp), func() (any, error) {
		if replay, ok := replayableCLISmokeVerdict(cliID, stamp, path, provider.loggedIn); ok {
			return replay, nil
		}
		executed = true
		result := provider.run(ctx, path, version)
		// A verdict reached because the CALLER went away (delivery cancelled,
		// agent shutting down) says nothing about the binary: the providers
		// classify that kill as `timeout` like an attempt-deadline expiry, and
		// caching it would hand the redelivered post-upgrade smoke a stale
		// failure for 15 minutes instead of testing the CLI.
		//
		// Nor is a verdict on a binary replaced while it ran: the post-upgrade
		// flight (keyed on the new stamp) may already have cached the new
		// binary's verdict, and this late write would evict it.
		if ctx.Err() == nil && cliSmokeBinaryStamp(path, version) == stamp {
			rememberCLISmokeVerdict(cliID, stamp, result)
		}
		return result, nil
	})
	result, _ := v.(cliSmokeResult)
	return result, !executed
}

// replayableCLISmokeVerdict reports the cached verdict when it may still stand
// in for a fresh probe.
//
// Beyond the binary stamp and the cooldown window it re-runs the provider's
// login pre-check, because AUTHENTICATION IS NOT PART OF THE STAMP and changes
// without the binary changing: replaying a cached success for a CLI the user
// has since logged out of would report a healthy CLI that cannot complete a
// turn. The check is local and spends no quota, and an INCONCLUSIVE answer
// replays — mirroring the "inconclusive proceeds" rule in the probes
// themselves, since that is what a working env-credential login looks like.
//
// The opposite direction — a user who logs IN after a failure — needs no check
// here: neither a not_logged_in nor a provider-side auth_error verdict is
// cached at all (see cliSmokeVerdictSpentTurn).
func replayableCLISmokeVerdict(cliID, stamp, path string, loggedInCheck func(context.Context, string) (bool, bool)) (cliSmokeResult, bool) {
	cliSmokeCooldownMu.Lock()
	entry, seen := cliSmokeCooldownCache[cliID]
	cliSmokeCooldownMu.Unlock()
	if !seen || entry.Stamp != stamp || time.Since(entry.At) >= cliSmokeCooldown {
		return cliSmokeResult{}, false
	}
	if loggedIn, known := loggedInCheck(context.Background(), path); known && !loggedIn {
		// Drop it rather than keep re-probing: the caller falls through to a
		// real run, whose own pre-check returns not_authenticated for free.
		cliSmokeCooldownMu.Lock()
		delete(cliSmokeCooldownCache, cliID)
		cliSmokeCooldownMu.Unlock()
		return cliSmokeResult{}, false
	}
	return entry.Result, true
}

// rememberCLISmokeVerdict stores a verdict for replay — but only one that
// actually cost the user an inference turn. See cliSmokeVerdictSpentTurn.
func rememberCLISmokeVerdict(cliID, stamp string, result cliSmokeResult) {
	if !cliSmokeVerdictSpentTurn(result) {
		return
	}
	cliSmokeCooldownMu.Lock()
	cliSmokeCooldownCache[cliID] = cliSmokeCooldownEntry{At: time.Now(), Stamp: stamp, Result: result}
	cliSmokeCooldownMu.Unlock()
}

// cliSmokeVerdictSpentTurn reports whether a verdict was reached by actually
// running the CLI, as opposed to being decided by a pre-check that returned
// before the ladder spawned anything.
//
// The cooldown exists to protect the user's inference quota, so it should hold
// exactly the verdicts that consumed some. Caching a free pre-check verdict
// buys no quota and costs recovery time: a `not_logged_in` result pinned for 15
// minutes keeps reporting a broken CLI for a user who signed in ten seconds
// after the probe, and a transient `internal` (marker RNG, isolation setup)
// failure would stick just as long.
//
// `auth_error` belongs with them even though the child did launch: the provider
// rejected the credential BEFORE any assistant turn, so nothing was spent. It
// is also the one failure the replay's login re-check cannot clear — that check
// only drops a cached verdict when the local login is known-bad, and an expired
// or revoked credential that the local precheck still reads as valid looks
// healthy again the moment the user signs in. Pinning it would keep reporting
// a broken CLI across exactly the recovery it is meant to notice.
func cliSmokeVerdictSpentTurn(result cliSmokeResult) bool {
	switch result.Diagnostic {
	case cliSmokeDiagnosticBinaryMissing,
		cliSmokeDiagnosticNotLoggedIn,
		cliSmokeDiagnosticAuthError,
		cliSmokeDiagnosticInternal:
		return false
	}
	return true
}
