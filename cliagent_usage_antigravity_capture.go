// cliagent_usage_antigravity_capture.go — run-scoped Antigravity quota capture.
//
// Every other CLI agent leaks its usage numbers passively: Claude prints a rate
// limit line on stdout (cliagent_ratelimit.go), Codex and Grok write files that
// outlive the run. `agy` does neither. Its quota lives ONLY in the language
// server each run starts on a random loopback port, and that server dies with
// the process — so a probe issued at gather time (the `__cli_usage_refresh__`
// poll, which by construction runs when nothing is executing) always finds no
// port, falls back to the cached snapshot and republishes its ORIGINAL
// observedAt. That is why the CLI Agents card kept ageing a day-old pool even
// after a successful run.
//
// The fix is to read the server WHILE it exists. Every path that spawns `agy` —
// native chat (antigravity_native.go), the PTY session/execute path
// (pty_session_unix.go) and the pipe session path (session.go) — arms this
// bounded poller for the life of the child, and one last attempt runs in the
// exit window. The next signed refresh then replays a snapshot whose observedAt
// falls inside the run.
//
// Cost discipline matters because this runs on the user's machine while they are
// working, and the expensive part of a probe is log scanning
// (antigravityQuotaMaxLogs files × 2×antigravityLogScanBytes), not the RPC. So:
// the winning port is memoized for the life of the run and rediscovered only
// after an RPC failure, ONE poller is shared process-wide no matter how many
// concurrent `agy` runs are armed, and the polling loop is hard-capped by both
// attempt count and wall-clock.
//
// Redaction: a captured snapshot carries exactly the allowlist the gather path
// persists (observedAt, accountFingerprint, account, plan, numeric buckets — see
// sanitizeAntigravityQuotaSnapshot). Discovered ports, log text, argv, prompts
// and settings.json contents are never persisted, and never logged.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// One probe every 3s: fast enough that a short turn still lands a reading,
	// slow enough that a memoized-port probe stays a sub-millisecond loopback
	// RPC rather than a repeated multi-megabyte log scan.
	antigravityCapturePollInterval = 3 * time.Second
	// After the child is reaped the socket may answer for a moment longer. One
	// final attempt in that window is the freshest reading a run can produce.
	antigravityCaptureExitGrace = 1500 * time.Millisecond
	// Bounds on the POLLING LOOP for a pathologically long run. The exit-grace
	// attempt is deliberately outside them, so the consequence we accept is
	// narrow: past the caps the last mid-run reading can be up to
	// antigravityCaptureMaxDuration old until that final attempt lands.
	antigravityCaptureMaxDuration = 15 * time.Minute
	antigravityCaptureMaxAttempts = 200
	// antigravityCaptureIntervalEnv shortens the tick for tests, mirroring the
	// AIEXPEDITE_AGY_QUOTA_CACHE seam. Not an operator knob: an unparseable or
	// non-positive value falls back to the constant.
	antigravityCaptureIntervalEnv = "AIEXPEDITE_AGY_CAPTURE_INTERVAL"
)

var (
	// antigravityCaptureMu guards the refcount and the two lifecycle channels.
	antigravityCaptureMu sync.Mutex
	// antigravityCaptureRefs counts armed runs. The poller is single-flight:
	// the first armer starts it, later armers only increment. One window can
	// briefly overlap two pollers — a run arming while the previous poller is
	// still inside its exit grace — which is harmless because every write goes
	// through saveAntigravityQuotaSnapshotIfNewer. Making the new run WAIT for
	// the old poller instead would violate the never-block-the-caller contract.
	antigravityCaptureRefs int
	// antigravityCaptureStop is closed when the last armed run finishes; the
	// poller then performs its exit-grace attempt and exits.
	antigravityCaptureStop chan struct{}
	// antigravityCaptureDone is closed by the poller once it has fully stopped.
	// Retained after a stop (until the next arm replaces it) so a caller — in
	// practice a test — can wait for the exit-grace attempt without finish()
	// having to block the run it is attached to.
	antigravityCaptureDone chan struct{}

	// Lifecycle counters. They exist so the spawn paths can be asserted to arm
	// exactly once and release exactly once per run; nothing in production reads
	// them, and they carry no user data.
	antigravityCaptureArms      atomic.Int64
	antigravityCaptureFinishes  atomic.Int64
	antigravityCaptureSnapshots atomic.Int64
)

// startAntigravityQuotaCapture arms the shared quota poller for one `agy` run
// and returns the release function.
//
// Contract, relied on by every spawn path: it never blocks the caller, never
// returns an error, and the returned finish is idempotent — a failed capture
// costs freshness, never the run. label is a fixed internal string ("native
// turn", "PTY session", …) used only for logging; it must never carry a command
// line, path or prompt.
func startAntigravityQuotaCapture(label string) (finish func()) {
	antigravityCaptureArms.Add(1)

	antigravityCaptureMu.Lock()
	antigravityCaptureRefs++
	if antigravityCaptureRefs == 1 {
		antigravityCaptureStop = make(chan struct{})
		antigravityCaptureDone = make(chan struct{})
		go runAntigravityQuotaCapture(label, antigravityCaptureStop, antigravityCaptureDone)
	}
	antigravityCaptureMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			antigravityCaptureFinishes.Add(1)

			antigravityCaptureMu.Lock()
			antigravityCaptureRefs--
			var stop chan struct{}
			if antigravityCaptureRefs <= 0 {
				antigravityCaptureRefs = 0
				// Leave antigravityCaptureDone in place: it is the only handle on
				// the in-flight exit-grace attempt, and the next arm replaces it.
				stop, antigravityCaptureStop = antigravityCaptureStop, nil
			}
			antigravityCaptureMu.Unlock()

			if stop != nil {
				close(stop)
			}
		})
	}
}

// antigravityCaptureStopped returns the current poller's completion channel, or
// nil when none has ever been armed in this process. It is closed once the
// poller — including its exit-grace attempt — has finished.
func antigravityCaptureStopped() <-chan struct{} {
	antigravityCaptureMu.Lock()
	defer antigravityCaptureMu.Unlock()
	return antigravityCaptureDone
}

// antigravityCapturePollIntervalValue resolves the tick, honoring the test seam.
func antigravityCapturePollIntervalValue() time.Duration {
	if raw := os.Getenv(antigravityCaptureIntervalEnv); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return antigravityCapturePollInterval
}

// runAntigravityQuotaCapture is the single shared poller goroutine. It ticks
// until every armed run has finished (or the caps are reached), then takes one
// final reading in the exit window.
func runAntigravityQuotaCapture(label string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	ticker := time.NewTicker(antigravityCapturePollIntervalValue())
	defer ticker.Stop()
	deadline := time.Now().Add(antigravityCaptureMaxDuration)

	// memoPort is the port that last answered. Reusing it is what keeps a long
	// run off the log-scanning path; an RPC failure clears it so the next
	// attempt rediscovers — which is also how a mid-run `agy` restart that moves
	// the server to a new port is picked up.
	memoPort := 0
	attempts, captured := 0, 0
	lastObserved := ""

polling:
	for {
		select {
		case <-stop:
			break polling
		case <-ticker.C:
			if ok, observedAt := antigravityCaptureAttempt(&memoPort); ok {
				captured++
				lastObserved = observedAt
				if captured == 1 {
					fmt.Printf("%s[antigravity-quota] Captured live quota during %s (observedAt=%s)%s\n",
						colorBlue, label, observedAt, colorReset)
				}
			}
			attempts++
			if attempts >= antigravityCaptureMaxAttempts || !time.Now().Before(deadline) {
				fmt.Printf("%s[antigravity-quota] Capture polling bound reached for %s after %d attempts — waiting for the run to finish%s\n",
					colorYellow, label, attempts, colorReset)
				// Park on stop rather than returning: the refcount must stay
				// sound, and the run still deserves its exit-grace reading.
				<-stop
				break polling
			}
		}
	}

	// The language server dies with its process, but the socket can linger for a
	// beat after Wait() returns. This attempt is unconditional — for a run longer
	// than the polling caps it is the only fresh reading the run produces.
	time.Sleep(antigravityCaptureExitGrace)
	if ok, observedAt := antigravityCaptureAttempt(&memoPort); ok {
		captured++
		lastObserved = observedAt
	}
	fmt.Printf("%s[antigravity-quota] Capture finished for %s (attempts=%d captured=%d lastObservedAt=%s)%s\n",
		colorCyan, label, attempts+1, captured, firstNonEmpty(lastObserved, "none"), colorReset)
}

// antigravityCaptureAttempt performs one bounded probe and persists the reading
// when it is attributable. Returns whether a snapshot was persisted and the
// observation time that was written.
func antigravityCaptureAttempt(memoPort *int) (bool, string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), antigravityQuotaTimeout)
	defer cancel()
	client := antigravityLoopbackClient()
	now := time.Now()

	if *memoPort > 0 {
		if snap, ok := fetchAntigravityQuotaOnPort(ctx, client, *memoPort, now); ok {
			return antigravityCapturePersist(snap)
		}
		*memoPort = 0
	}

	// Bases are re-resolved on every attempt (not once at arm time) so an `agy`
	// self-update that migrates ~/.agy → ~/.gemini/antigravity-cli mid-flight is
	// picked up without restarting the agent.
	for _, base := range antigravityQuotaBases(home) {
		for _, port := range discoverAntigravityHTTPPorts(base) {
			snap, ok := fetchAntigravityQuotaOnPort(ctx, client, port, now)
			if !ok {
				continue
			}
			*memoPort = port
			return antigravityCapturePersist(snap)
		}
	}
	return false, ""
}

// antigravityCapturePersist scopes a live reading to the account the server
// itself named and writes it monotonically.
//
// A quota response the server could not attribute (RetrieveUserQuotaSummary
// succeeded, GetUserStatus did not) is dropped for THIS tick only, not for the
// run: the port stays memoized, so a transient identity blip costs one tick of
// freshness instead of the whole run's. Publishing it under settings.json's
// account instead would attach one account's pool to another's fingerprint —
// the same rule antigravityUsageParser.Parse enforces at gather time.
func antigravityCapturePersist(snap antigravityQuotaSnapshot) (bool, string) {
	snap.AccountFingerprint = fingerprintAccount("antigravity", snap.Account)
	if snap.AccountFingerprint == "" {
		return false, ""
	}
	if !saveAntigravityQuotaSnapshotIfNewer(snap) {
		return false, ""
	}
	antigravityCaptureSnapshots.Add(1)
	return true, snap.ObservedAt
}
