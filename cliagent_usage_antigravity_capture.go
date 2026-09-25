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
// The fix is to read the server WHILE it exists. Every path that spawns `agy`
// arms this bounded poller for the life of the child: it probes immediately and
// ramps down to a steady interval. The next signed refresh then replays a
// snapshot whose observedAt falls inside the run. The five arm sites, and the
// per-path timing that goes with them, are documented in CLI_AGENT_INTEGRATION.md
// ("Antigravity quota capture"):
//
//   - runOneShot            (antigravity_native.go) — native chat, after Start
//   - runPTYCommand         (pty_session_unix.go)   — PTY session + execute, after Start
//   - StartSession          (session.go)            — pipe session, after Start, released in waitForExit
//   - runLocalCommandUnix   (pubsub.go)             — tty=false execute, incl. a DIRECT
//     `agy` on Windows, after Start
//   - runLocalCommandWindows (pubsub.go)            — the wrapped Windows transport
//     chain, armed at function ENTRY because no single post-Start hook exists there
//
// Cost discipline matters because this runs on the user's machine while they are
// working, and the expensive part of a probe is log scanning
// (antigravityQuotaMaxLogs files × 2×antigravityLogScanBytes), not the RPC. So:
// the winning port is memoized for the life of the run and rediscovered only
// after an RPC failure, ONE poller is shared process-wide no matter how many
// concurrent `agy` runs are armed, and expensive discovery is hard-capped by
// both attempt count and wall-clock. Once that cap is reached, a known-good port
// continues to receive cheap loopback probes until the run ends; otherwise a
// long successful run could finish with a reading hours older than its tail.
//
// Redaction: a captured snapshot carries exactly the allowlist the gather path
// persists (observedAt, accountFingerprint, account, plan, numeric buckets — see
// sanitizeAntigravityQuotaSnapshot). Discovered ports, log text, argv, prompts
// and settings.json contents are never persisted, and never logged.
package main

import (
	"context"
	"fmt"
	"net/http"
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
	// Bounds on attempts that may scan Antigravity's logs. After either bound is
	// reached, the poller continues querying an already memoized port but never
	// scans again. This keeps the expensive work bounded without abandoning a
	// long-running child before its only quota source disappears.
	antigravityCaptureMaxDuration = 15 * time.Minute
	antigravityCaptureMaxAttempts = 200
	// antigravityCaptureTailGrace is how long the poller keeps reading an
	// already-memoized port AFTER the last armed run has released it. A turn's
	// quota is debited at the END of the turn, so the last in-run sample can
	// under-report the run that just finished — and on the execute path the
	// release happens the instant Wait returns, while the language server is
	// still shutting down and still answering. No discovery and no log scanning
	// happen in this window: it is bounded loopback reads of a port that already
	// answered, or nothing at all.
	antigravityCaptureTailGrace = 2 * time.Second
	// antigravityCaptureIntervalEnv shortens the tick for tests, mirroring the
	// AIEXPEDITE_AGY_QUOTA_CACHE seam. Not an operator knob: an unparseable or
	// non-positive value falls back to the constant.
	antigravityCaptureIntervalEnv = "AIEXPEDITE_AGY_CAPTURE_INTERVAL"
	// antigravityCaptureTailEnv is the same seam for the tail window, so a test
	// does not pay the shipped grace on every capture it stops.
	antigravityCaptureTailEnv = "AIEXPEDITE_AGY_CAPTURE_TAIL"
)

var (
	// antigravityCaptureMu guards the refcount and the two lifecycle channels.
	antigravityCaptureMu sync.Mutex
	// antigravityCaptureRefs counts armed runs. The poller is single-flight:
	// the first armer starts it, later armers only increment. One window can
	// briefly overlap two pollers — a run arming while the previous poller is
	// still shutting down — which is harmless because every write goes
	// through saveAntigravityQuotaSnapshotIfNewer. Making the new run WAIT for
	// the old poller instead would violate the never-block-the-caller contract.
	antigravityCaptureRefs int
	// antigravityCaptureStop is closed when the last armed run finishes; the
	// poller then exits.
	antigravityCaptureStop chan struct{}
	// antigravityCaptureDone is closed by the poller once it has fully stopped.
	// Retained after a stop (until the next arm replaces it) so a caller — in
	// practice a test — can wait for shutdown without finish() having to block
	// the run it is attached to.
	antigravityCaptureDone chan struct{}

	// Lifecycle counters. They exist so the spawn paths can be asserted to arm
	// exactly once and release exactly once per run; nothing in production reads
	// them, and they carry no user data.
	antigravityCaptureArms      atomic.Int64
	antigravityCaptureFinishes  atomic.Int64
	antigravityCaptureSnapshots atomic.Int64
	// antigravityCaptureTailProbes counts probes taken in the post-release tail
	// window, so a test can prove the tail never rediscovers.
	antigravityCaptureTailProbes atomic.Int64
	// antigravityCaptureLastPersistedMs is the observation time of the newest
	// reading any capture persisted in this process. It is the cheap HINT a
	// finishing run uses to answer "did anything land for me?" without reading
	// the cache; the authoritative answer is still the time comparison in
	// antigravityUsageRunSettled.
	antigravityCaptureLastPersistedMs atomic.Int64
)

// armAntigravityCaptureForCommand arms the quota poller when spawning cmd+args
// would start `agy`, and returns the release function. When it would not, the
// returned release is a no-op and nothing is armed.
//
// Every spawn site goes through this rather than pairing its own
// commandRunsAntigravity check with startAntigravityQuotaCapture: a site that
// re-implements the pair is a site that can drift out of step with the
// classifier, which is exactly how the Windows execute path ended up arming
// nothing at all. label is a fixed internal string ("windows execute", "local
// execute", …) and must never carry a command line, path or prompt.
func armAntigravityCaptureForCommand(label, cmd string, args []string) func() {
	if !commandRunsAntigravity(cmd, args) {
		return func() {}
	}
	return startAntigravityQuotaCapture(label)
}

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
	// Arm this run's freshness floor. Every spawn site reaches here — directly
	// on the native path, through armAntigravityCaptureForCommand everywhere
	// else — so all five inherit the run-completion refresh with no new call
	// sites (cliagent_usage_antigravity_freshness.go).
	floor := armAntigravityUsageRunFloor(time.Now())

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
				// the in-flight shutdown, and the next arm replaces it.
				stop, antigravityCaptureStop = antigravityCaptureStop, nil
			}
			antigravityCaptureMu.Unlock()

			if stop != nil {
				close(stop)
			}

			// Settle THIS run, off the caller's goroutine and without waiting
			// for the poller: the poller is ref-counted and exits only when the
			// last armed run releases, so settling there would park a finished
			// run's refresh behind a long interactive session sharing it.
			go func() {
				now := time.Now()
				// The build's refusal is remembered per build, so the marker is
				// the single source for "could the poller have captured this
				// run at all?".
				_, gated := antigravityQuotaGateFor("", now)
				antigravityUsageRunSettled(floor,
					antigravityCaptureLastPersistedMs.Load() >= floor.UnixMilli(), gated)
			}()
		})
	}
}

// antigravityCaptureStopped returns the current poller's completion channel, or
// nil when none has ever been armed in this process. It is closed once the
// poller has finished.
func antigravityCaptureStopped() <-chan struct{} {
	antigravityCaptureMu.Lock()
	defer antigravityCaptureMu.Unlock()
	return antigravityCaptureDone
}

// antigravityCapturePollIntervalValue resolves the tick, honoring the test seam.
func antigravityCapturePollIntervalValue() time.Duration {
	return antigravityDurationEnv(antigravityCaptureIntervalEnv, antigravityCapturePollInterval)
}

// antigravityCaptureTailGraceValue resolves the tail window, honoring its seam.
func antigravityCaptureTailGraceValue() time.Duration {
	return antigravityDurationEnv(antigravityCaptureTailEnv, antigravityCaptureTailGrace)
}

// antigravityDurationEnv reads a test-seam duration. Not an operator knob: an
// unparseable or non-positive value falls back to the shipped constant.
func antigravityDurationEnv(name string, fallback time.Duration) time.Duration {
	if raw := os.Getenv(name); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// runAntigravityQuotaCapture is the single shared poller goroutine. It probes
// immediately, ramps down to the steady interval, and keeps cheap probes of a
// memoized port alive until the last armed run reports completion.
func runAntigravityQuotaCapture(label string, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	client := antigravityLoopbackClient()
	defer client.CloseIdleConnections()

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
	discoveryAttempts, captured := 0, 0
	lastObserved := ""

	// gated: the run's server refused the read. Set once; every later tick
	// is skipped and the tail window is not paid, because the refusal is a
	// property of the installed build (cliagent_usage_antigravity_gate.go).
	gated := false
	probe := func(allowDiscovery bool) {
		if gated {
			return
		}
		if allowDiscovery {
			discoveryAttempts++
		}
		ok, observedAt, refused := antigravityCaptureAttempt(client, &memoPort, allowDiscovery)
		if refused {
			if !gated {
				gated = true
				noteAntigravityQuotaGate("", time.Now())
				fmt.Printf("%s[antigravity-quota] the language server refuses loopback quota reads (CSRF-gated agy build) — capture for %s stops here; the card keeps the last reading with its true age%s\n",
					colorYellow, label, colorReset)
			}
			return
		}
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

	// tailAndReport drains the post-release window and writes the single
	// close-out line. Called on every exit from the loop below, so the tail can
	// never be skipped by the path a particular run happens to take.
	tailAndReport := func() {
		tailProbes, tailCaptured := 0, 0
		// No memoized port means no cheap probe exists: discovery is the
		// expensive half, and a run that never found a server must not start
		// scanning logs at the very moment it is being torn down.
		if memoPort > 0 && !gated {
			grace := antigravityCaptureTailGraceValue()
			tailInterval := initial
			if tailInterval > grace {
				tailInterval = grace
			}
			for deadline := time.Now().Add(grace); ; {
				tailProbes++
				antigravityCaptureTailProbes.Add(1)
				before := captured
				probe(false)
				if captured > before {
					tailCaptured++
				}
				if remaining := time.Until(deadline); remaining <= 0 {
					break
				} else if remaining < tailInterval {
					time.Sleep(remaining)
				} else {
					time.Sleep(tailInterval)
				}
			}
		}
		fmt.Printf("%s[antigravity-quota] Capture finished for %s (discoveryAttempts=%d captured=%d tailProbes=%d tailCaptured=%d lastObservedAt=%s)%s\n",
			colorCyan, label, discoveryAttempts, captured, tailProbes, tailCaptured,
			firstNonEmpty(lastObserved, "none"), colorReset)
	}

	// Probe before waiting on anything. A turn that finishes inside one tick
	// would otherwise never be sampled at all: the server dies with the child,
	// so there is no second chance once the run is over.
	probe(true)

	timer := time.NewTimer(initial)
	defer timer.Stop()

	discoveryEnabled := true

	for {
		select {
		case <-stop:
			tailAndReport()
			return
		case <-timer.C:
			probe(discoveryEnabled)
			if gated {
				// Nothing this run can answer differently; park until it ends.
				<-stop
				tailAndReport()
				return
			}
			if discoveryEnabled && (discoveryAttempts >= antigravityCaptureMaxAttempts || !time.Now().Before(deadline)) {
				discoveryEnabled = false
				if memoPort == 0 {
					fmt.Printf("%s[antigravity-quota] Capture discovery bound reached for %s after %d attempts — no live port to keep polling%s\n",
						colorYellow, label, discoveryAttempts, colorReset)
					<-stop
					tailAndReport()
					return
				}
				fmt.Printf("%s[antigravity-quota] Capture discovery bound reached for %s after %d attempts — continuing bounded-cost reads on the live port%s\n",
					colorYellow, label, discoveryAttempts, colorReset)
			}
			next := steady
			if discoveryEnabled && discoveryAttempts < antigravityCaptureInitialPolls {
				next = initial
			}
			timer.Reset(next)
		}
	}
}

// antigravityCaptureAttempt performs one bounded probe and persists the reading
// when it is attributable. Returns whether a snapshot was persisted and the
// observation time that was written.
//
// The third result reports a live server that REFUSED the read (the CSRF gate,
// cliagent_usage_antigravity_gate.go). The port is still memoized — it IS the
// run's server — but the caller must stop probing: no tick of this run will
// answer differently, and each further attempt would only be another log scan.
func antigravityCaptureAttempt(client *http.Client, memoPort *int, allowDiscovery bool) (persisted bool, observedAt string, gated bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false, "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), antigravityQuotaTimeout)
	defer cancel()
	now := time.Now()

	if *memoPort > 0 {
		switch snap, outcome := fetchAntigravityQuotaOnPortOutcome(ctx, client, *memoPort, now); outcome {
		case antigravityFetchOK:
			persisted, observedAt = antigravityCapturePersist(snap)
			return persisted, observedAt, false
		case antigravityFetchGated:
			return false, "", true
		}
		if !allowDiscovery {
			// Keep retrying the known port without scanning. A transient RPC
			// failure after the discovery cap must not permanently stop tail
			// capture, while a port change remains intentionally undiscovered.
			return false, "", false
		}
		*memoPort = 0
	}
	if !allowDiscovery {
		return false, "", false
	}

	// Bases are re-resolved on every attempt (not once at arm time) so an `agy`
	// self-update that migrates ~/.agy → ~/.gemini/antigravity-cli mid-flight is
	// picked up without restarting the agent.
	for _, base := range antigravityQuotaBases(home) {
		ports, _ := discoverAntigravityHTTPPorts(base)
		for _, port := range ports {
			switch snap, outcome := fetchAntigravityQuotaOnPortOutcome(ctx, client, port, now); outcome {
			case antigravityFetchOK:
				*memoPort = port
				persisted, observedAt = antigravityCapturePersist(snap)
				return persisted, observedAt, false
			case antigravityFetchGated:
				*memoPort = port
				return false, "", true
			}
		}
	}
	return false, "", false
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
	if at, err := time.Parse(time.RFC3339, snap.ObservedAt); err == nil {
		for {
			prev := antigravityCaptureLastPersistedMs.Load()
			if at.UnixMilli() <= prev ||
				antigravityCaptureLastPersistedMs.CompareAndSwap(prev, at.UnixMilli()) {
				break
			}
		}
	}
	return true, snap.ObservedAt
}
