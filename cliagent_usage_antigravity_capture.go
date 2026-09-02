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
// bounded poller for the life of the child: it probes immediately, ramps down
// to a steady interval, and probes once more the instant the run reports
// completion. The next signed refresh then replays a snapshot whose observedAt
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
	// Steady-state probe interval, used once the opening ramp is spent: by then
	// the port is memoized, so a probe is a sub-millisecond loopback RPC rather
	// than a repeated multi-megabyte log scan.
	antigravityCapturePollInterval = 3 * time.Second
	// Opening ramp. The quota server belongs to the child, so it is unreachable
	// the moment that child is reaped — every reading MUST be taken while the
	// process is alive, and a fast successful turn can be over inside a single
	// steady tick. The ramp probes quickly while the language server is binding
	// its port and while a short turn is still running; it is bounded because an
	// unmemoized probe is the expensive kind (up to antigravityQuotaMaxLogs
	// files × 2×antigravityLogScanBytes of log scanning).
	antigravityCaptureInitialPollInterval = 250 * time.Millisecond
	antigravityCaptureInitialPolls        = 12
	// Bounds on the POLLING LOOP for a pathologically long run. The final probe
	// taken when the run ends is deliberately outside them, so the consequence
	// we accept is narrow: past the caps the last mid-run reading can be up to
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
	// still taking its final probe — which is harmless because every write goes
	// through saveAntigravityQuotaSnapshotIfNewer. Making the new run WAIT for
	// the old poller instead would violate the never-block-the-caller contract.
	antigravityCaptureRefs int
	// antigravityCaptureStop is closed when the last armed run finishes; the
	// poller then takes its final probe and exits.
	antigravityCaptureStop chan struct{}
	// antigravityCaptureDone is closed by the poller once it has fully stopped.
	// Retained after a stop (until the next arm replaces it) so a caller — in
	// practice a test — can wait for that final probe without finish() having to
	// block the run it is attached to.
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
				// the in-flight final probe, and the next arm replaces it.
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
// poller — including its final probe — has finished.
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

// runAntigravityQuotaCapture is the single shared poller goroutine. It probes
// immediately, ramps down to the steady interval, and probes once more the
// instant the last armed run reports completion.
func runAntigravityQuotaCapture(label string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	steady := antigravityCapturePollIntervalValue()
	initial := antigravityCaptureInitialPollInterval
	if initial > steady {
		initial = steady
	}
	deadline := time.Now().Add(antigravityCaptureMaxDuration)

	// memoPort is the port that last answered. Reusing it is what keeps a long
	// run off the log-scanning path; an RPC failure clears it so the next
	// attempt rediscovers — which is also how a mid-run `agy` restart that moves
	// the server to a new port is picked up.
	memoPort := 0
	attempts, captured := 0, 0
	lastObserved := ""

	probe := func() {
		attempts++
		ok, observedAt := antigravityCaptureAttempt(&memoPort)
		if !ok {
			return
		}
		captured++
		lastObserved = observedAt
		if captured == 1 {
			fmt.Printf("%s[antigravity-quota] Captured live quota during %s (observedAt=%s)%s\n",
				colorBlue, label, observedAt, colorReset)
		}
	}

	// Probe before waiting on anything. A turn that finishes inside one tick
	// would otherwise never be sampled at all: the server dies with the child,
	// so there is no second chance once the run is over.
	probe()

	timer := time.NewTimer(initial)
	defer timer.Stop()

polling:
	for {
		select {
		case <-stop:
			break polling
		case <-timer.C:
			probe()
			if attempts >= antigravityCaptureMaxAttempts || !time.Now().Before(deadline) {
				fmt.Printf("%s[antigravity-quota] Capture polling bound reached for %s after %d attempts — waiting for the run to finish%s\n",
					colorYellow, label, attempts, colorReset)
				// Park on stop rather than returning: the refcount must stay
				// sound, and the run still deserves its final reading.
				<-stop
				break polling
			}
			next := steady
			if attempts < antigravityCaptureInitialPolls {
				next = initial
			}
			timer.Reset(next)
		}
	}

	// One last probe the moment completion is signalled — immediately, with no
	// grace delay. The child owns the listening socket, so waiting would only
	// widen the gap between "still answering" and "asked"; for a run longer than
	// the polling caps this is also the only reading left to take.
	probe()
	fmt.Printf("%s[antigravity-quota] Capture finished for %s (attempts=%d captured=%d lastObservedAt=%s)%s\n",
		colorCyan, label, attempts, captured, firstNonEmpty(lastObserved, "none"), colorReset)
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
