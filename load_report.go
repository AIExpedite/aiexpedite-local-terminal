// File: load_report.go
// -----------------------------------------------------------------------------
// The computer's own load and presence report
// (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md §6.1 and §8, Phase 0
// "Measure"). The sampler reads this machine, decides whether anything
// material changed, and posts a signed sample to
// `POST /device/:agentId/load`.
//
// OBSERVE ONLY. Phase 0's behaviour change is "none": nothing here refuses
// work, parks a run, changes a route or caps concurrency. One run per computer
// (terminal-service #337) still decides alone.
//
// WHY A NEW CHANNEL AT ALL. Before this file the only periodic agent→server
// channel was the WIF token refresh (auth.go: the cached GCP token is renewed
// when it is within five minutes of its one-hour expiry), and the machine info
// it carries is re-gathered every SIX HOURS (systemInfo.go
// machineInfoRefreshInterval) — including the `live` cpu/mem sample. A number
// up to six hours old, refreshed at most hourly, cannot answer "is this
// computer busy now", which is what §6.1 says plainly: "Today's 6-hour gather
// and hourly token refresh are too slow."
//
// CADENCE AND WHAT IT COSTS (§6.1: sample every 30 s, report on a change of
// ≥ 5 % or a state change, and at least every 5 minutes):
//   - one HTTP POST and one Firestore document update per report;
//   - 288 reports per computer per day when nothing moves (the 5-minute
//     floor), and at most 2,880 — one per sample — on a machine whose load
//     changes continuously. That is well under a cent a day per computer, and
//     it replaces no existing traffic;
//   - one document is replaced in place. No history is appended: §6.1 asks for
//     a live card, and the per-activity RSS the Phase 0 exit check wants comes
//     out of the server's log line, which Cloud Logging already retains.
//
// The cadence numbers, the vocabulary and the signed canonical are pinned by
// testdata/device_load_report_vectors.json, which terminal-service asserts
// against @aiexpedite/shared-constants and its own route. Change the canonical
// and three tests fail until all three sides agree.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

/* --------------------------------------------------------------------------
   Cadence and vocabulary
   -------------------------------------------------------------------------- */

const (
	// loadSampleInterval is how often the machine is read (§6.1).
	loadSampleInterval = 30 * time.Second
	// loadReportMaxInterval is the floor: a report goes out at least this
	// often even when nothing changed, so the server can tell a steady
	// computer from a silent one.
	loadReportMaxInterval = 5 * time.Minute
	// loadReportChangePercent is how big a move counts as material (§6.1).
	loadReportChangePercent = 5
	// loadReportHTTPTimeout bounds one POST. A report is a snapshot: a slow
	// one is worthless, and the next sample is 30 s away.
	loadReportHTTPTimeout = 5 * time.Second
	// loadCPUSampleWindow is the window cpu.Percent averages over.
	// gopsutil's zero-interval form returns the since-boot average, which
	// masks exactly the load this report exists to measure (the same reason
	// gatherLiveLoad uses a one-second window).
	loadCPUSampleWindow = time.Second
	// loadMetricUnreadable is what a metric this OS cannot read reports.
	// A sentinel rather than an omitted field because the signed canonical
	// lists every numeric field in a fixed order; the server drops it instead
	// of storing it.
	loadMetricUnreadable = int64(-1)
	// loadRSSPidCap bounds the per-process RSS walk. A FIXED bound, not one
	// derived from anything this build knows, and far above any plausible
	// number of CLI processes on one computer.
	loadRSSPidCap = 64
)

// Memory pressure (shared-constants MEM_PRESSURE_LEVELS). Mirrored here for
// the same reason the presence states are (presence.go) and pinned by the same
// contract file.
const (
	memPressureNormal   = "normal"
	memPressureWarn     = "warn"
	memPressureCritical = "critical"
)

// Available-memory thresholds for the pressure level.
//
// §6.1 names the exact per-OS sources — `kern.memorystatus_vm_pressure_level`
// on macOS, commit charge against the limit on Windows. Neither is read here:
// both need APIs this cgo-free build cannot reach (macOS) or a WMI/PDH query
// too expensive for a 30-second sampler (Windows). What ships instead is one
// portable rule over available memory, which is exactly the kind of estimate
// Phase 0 exists to calibrate — §6.2: "the estimate behind each level is
// learned from Phase 0 measurements". It feeds the stored sample and nothing
// else in this phase.
const (
	memPressureCriticalBelowPct = 10
	memPressureWarnBelowPct     = 20
)

/* --------------------------------------------------------------------------
   Wire type
   -------------------------------------------------------------------------- */

// deviceLoadReport is the signed body of POST /device/:agentId/load.
//
// No `omitempty` anywhere: every field is part of the signed canonical in a
// fixed order, so an omitted field would make the signature ambiguous.
// "Unreadable" is carried by loadMetricUnreadable, not by absence.
type deviceLoadReport struct {
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`

	MemTotalMB     int64  `json:"memTotalMB"`
	MemAvailableMB int64  `json:"memAvailableMB"`
	MemPressure    string `json:"memPressure"`
	SwapUsedMB     int64  `json:"swapUsedMB"`
	CPULoadPct     int64  `json:"cpuLoadPct"`
	CLIProcesses   int64  `json:"cliProcesses"`
	CLIRssMB       int64  `json:"cliRssMB"`
	// CLIRssRead is how many of the CLIProcesses actually contributed to
	// CLIRssMB. Equal to CLIProcesses means the figure is a whole total;
	// anything less means a process could not be read or the walk hit its
	// cap, and the figure is an UNDERCOUNT that must not be taken for a
	// peak. Outside the signed canonical on purpose — see
	// buildDeviceLoadReportSignedMessage.
	CLIRssRead     int64 `json:"cliRssRead"`
	ActiveSessions int64 `json:"activeSessions"`
	OnBattery      bool  `json:"onBattery"`

	// PresenceState is one of the three PRESENCE_STATE values, or "" when this
	// computer reports no presence (the owner's switch, the local override, or
	// an OS that cannot be read). PresenceSinceMs is the state's age, or
	// loadMetricUnreadable.
	PresenceState   string `json:"presenceState"`
	PresenceSinceMs int64  `json:"presenceSinceMs"`
}

// buildDeviceLoadReportSignedMessage mirrors terminal-service's
// buildDeviceLoadReportSignedMessage (src/utils/hmac.util.js) byte for byte.
//
// IT IS CLOSED TO ADDITIONS. Both repositories pin this string in
// testdata/device_load_report_vectors.json, so a field added here is a flag
// day: agents update on their own cadence and routinely run ahead of the
// service, and a new agent's signature would not verify on a server that had
// not shipped the matching canonical yet — every report from every updated
// computer refused until the two meet. Measurement metadata such as
// cliRssRead therefore travels as an additive unsigned field, which is what
// the tolerant-reader rule asks for and costs nothing: a report cannot be
// submitted without a valid signature over the fields below, so forging the
// metadata needs the device secret, and that already forges the measurement.
//
// The canonical:
//
//	agentId:timestamp:memTotalMB:memAvailableMB:memPressure:swapUsedMB:
//	cpuLoadPct:cliProcesses:cliRssMB:activeSessions:onBattery:presenceState:
//	presenceSinceMs
//
// Integers only, base 10, and `onBattery` as 1/0 — no float ever enters the
// canonical, because Go's and JavaScript's float formatting do not agree and a
// disagreement would reject every report from a machine. Change this and the
// Node builder and the shared vectors together, or none of them.
func buildDeviceLoadReportSignedMessage(agentID string, r deviceLoadReport) string {
	battery := "0"
	if r.OnBattery {
		battery = "1"
	}
	return fmt.Sprintf("%s:%d:%d:%d:%s:%d:%d:%d:%d:%d:%s:%s:%d",
		agentID,
		r.Timestamp,
		r.MemTotalMB,
		r.MemAvailableMB,
		r.MemPressure,
		r.SwapUsedMB,
		r.CPULoadPct,
		r.CLIProcesses,
		r.CLIRssMB,
		r.ActiveSessions,
		battery,
		r.PresenceState,
		r.PresenceSinceMs,
	)
}

// deviceLoadReportResponse is what terminal-service answers.
//
// TOLERANT READER on this side too: every field is a pointer, so a server
// older than this build (it deploys separately, and §11 deploys it FIRST but
// an older one may still be serving) leaves each setting alone instead of
// reading an absent field as `false` or as a zero interval that would turn the
// sampler into a hot loop.
type deviceLoadReportResponse struct {
	Accepted         *bool  `json:"accepted"`
	PresenceEnabled  *bool  `json:"presenceEnabled"`
	SampleIntervalMs *int64 `json:"sampleIntervalMs"`
	MaxIntervalMs    *int64 `json:"maxIntervalMs"`
	ChangePercent    *int64 `json:"changePercent"`
}

/* --------------------------------------------------------------------------
   Sampling
   -------------------------------------------------------------------------- */

// Probe seams. Vars so tests never read the real machine.
var (
	loadProbeVirtualMemory = mem.VirtualMemory
	loadProbeSwapMemory    = mem.SwapMemory
	loadProbeCPUPercent    = func() (float64, bool) {
		pcts, err := cpu.Percent(loadCPUSampleWindow, false)
		if err != nil || len(pcts) == 0 {
			return 0, false
		}
		return pcts[0], true
	}
	loadProbeSessionCounts = func() (int, []int) { return globalSpawnLedger.loadCounts() }
	// loadProbeOnBattery is the per-OS AC probe (presence_windows.go's
	// GetSystemPowerStatus, presence_battery_other.go's gatherBattery).
	loadProbeOnBattery  = probeOnBattery
	loadProbeProcessRSS = func(pid int) (uint64, bool) {
		p, err := process.NewProcess(int32(pid))
		if err != nil {
			return 0, false
		}
		info, err := p.MemoryInfo()
		if err != nil || info == nil {
			return 0, false
		}
		return info.RSS, true
	}
)

const bytesPerMB = 1024 * 1024

// classifyMemPressure maps available memory to a pressure level.
//
// An unreadable total or available reports `normal`, i.e. "not refusing": the
// server's allowlist of refusing levels makes an unknown value fail safe, and
// this keeps the device's own answer on the same side. Guessing `critical`
// from a failed probe would, once §6.3 Phase 2d enforces, refuse work on a
// computer that is merely hard to measure.
func classifyMemPressure(availableMB, totalMB int64) string {
	if totalMB <= 0 || availableMB < 0 {
		return memPressureNormal
	}
	pct := float64(availableMB) / float64(totalMB) * 100
	switch {
	case pct < memPressureCriticalBelowPct:
		return memPressureCritical
	case pct < memPressureWarnBelowPct:
		return memPressureWarn
	default:
		return memPressureNormal
	}
}

// onBatteryCache holds one AC-power reading for a report interval, so the
// probe that shells out off Windows runs at most once every five minutes
// rather than on every sample.
var onBatteryCache = struct {
	mu      sync.Mutex
	at      time.Time
	on, got bool
}{}

// sampleOnBattery answers whether the machine is on battery, cached.
func sampleOnBattery(now time.Time) bool {
	onBatteryCache.mu.Lock()
	defer onBatteryCache.mu.Unlock()
	if !onBatteryCache.at.IsZero() && now.Sub(onBatteryCache.at) < loadReportMaxInterval {
		return onBatteryCache.on && onBatteryCache.got
	}
	on, got := loadProbeOnBattery()
	onBatteryCache.at, onBatteryCache.on, onBatteryCache.got = now, on, got
	// An unreadable AC state reports `false`: "not known to be on battery".
	// The field is a hint for later phases, and claiming battery power on a
	// desktop whose probe failed would throttle a machine that has none.
	return on && got
}

// sampleDeviceLoad reads the machine once.
func sampleDeviceLoad(now time.Time) deviceLoadReport {
	report := deviceLoadReport{
		Timestamp:       now.UnixMilli(),
		MemTotalMB:      loadMetricUnreadable,
		MemAvailableMB:  loadMetricUnreadable,
		MemPressure:     memPressureNormal,
		SwapUsedMB:      loadMetricUnreadable,
		CPULoadPct:      loadMetricUnreadable,
		CLIProcesses:    loadMetricUnreadable,
		CLIRssMB:        loadMetricUnreadable,
		CLIRssRead:      0,
		ActiveSessions:  loadMetricUnreadable,
		PresenceState:   presenceStateNotReported,
		PresenceSinceMs: loadMetricUnreadable,
	}

	if vmem, err := loadProbeVirtualMemory(); err == nil && vmem != nil {
		report.MemTotalMB = int64(vmem.Total / bytesPerMB)
		// Available, not free: free excludes the cache the OS would evict
		// under pressure, and on macOS and Windows alike it understates what
		// a new process can actually get.
		report.MemAvailableMB = int64(vmem.Available / bytesPerMB)
		report.MemPressure = classifyMemPressure(report.MemAvailableMB, report.MemTotalMB)
	}
	if swap, err := loadProbeSwapMemory(); err == nil && swap != nil {
		report.SwapUsedMB = int64(swap.Used / bytesPerMB)
	}
	if pct, ok := loadProbeCPUPercent(); ok {
		report.CPULoadPct = clampPct(pct)
	}

	sessions, pids := loadProbeSessionCounts()
	report.ActiveSessions = int64(sessions)
	report.CLIProcesses = int64(len(pids))
	if len(pids) > loadRSSPidCap {
		pids = pids[:loadRSSPidCap]
	}
	var rss uint64
	read := 0
	for _, pid := range pids {
		if bytes, ok := loadProbeProcessRSS(pid); ok {
			rss += bytes
			read++
		}
	}
	// How many processes the figure covers, counted against the TRUE number
	// tracked (report.CLIProcesses, set before the cap) rather than against
	// the capped walk — so a fleet with more than loadRSSPidCap CLIs reports
	// a partial total honestly instead of a whole-looking one.
	report.CLIRssRead = int64(read)
	if read > 0 || len(pids) == 0 {
		// Zero processes is a measurement of zero; a walk where every probe
		// failed is not a measurement at all. A walk where SOME probes failed
		// is a partial one: the figure stands, and CLIRssRead says how much
		// of the machine it accounts for.
		report.CLIRssMB = int64(rss / bytesPerMB)
	}

	report.OnBattery = sampleOnBattery(now)

	presence := samplePresence(now)
	report.PresenceState = presence.State
	report.PresenceSinceMs = presence.AgeMs

	return report
}

// clampPct rounds a percentage into 0..100.
func clampPct(v float64) int64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return int64(v + 0.5)
}

/* --------------------------------------------------------------------------
   Change detection
   -------------------------------------------------------------------------- */

// materialChange reports whether `next` is worth a report given `prev`
// (§6.1: "on change (≥ 5 % or a state change)").
//
// A STATE change is always material: pressure, presence and battery are the
// fields a later phase reacts to, and they move in steps, not percentages.
// The two continuous fields — CPU and available memory — need a move of
// changePercent percentage points, memory measured against the machine's own
// total so the threshold means the same on a 16 GB laptop and a 128 GB
// workstation. Session and process counts are small integers: any change is
// material.
func materialChange(prev, next deviceLoadReport, changePercent int64) bool {
	if prev.Timestamp == 0 {
		return true
	}
	if prev.MemPressure != next.MemPressure ||
		prev.PresenceState != next.PresenceState ||
		prev.OnBattery != next.OnBattery ||
		prev.ActiveSessions != next.ActiveSessions ||
		prev.CLIProcesses != next.CLIProcesses {
		return true
	}
	// A metric that became readable (or stopped being readable) is a change.
	if (prev.CPULoadPct < 0) != (next.CPULoadPct < 0) ||
		(prev.MemAvailableMB < 0) != (next.MemAvailableMB < 0) {
		return true
	}
	if prev.CPULoadPct >= 0 && absInt64(next.CPULoadPct-prev.CPULoadPct) >= changePercent {
		return true
	}
	total := next.MemTotalMB
	if total <= 0 {
		total = prev.MemTotalMB
	}
	if total > 0 && prev.MemAvailableMB >= 0 && next.MemAvailableMB >= 0 {
		movedMB := absInt64(next.MemAvailableMB - prev.MemAvailableMB)
		if movedMB*100 >= total*changePercent {
			return true
		}
	}
	return false
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

/* --------------------------------------------------------------------------
   The reporter
   -------------------------------------------------------------------------- */

// sendDeviceLoadReport makes ONE attempt and returns the HTTP status (0 on a
// transport error) with the decoded body. A var so tests never reach the
// network.
var sendDeviceLoadReport = func(ctx context.Context, url string, report deviceLoadReport) (int, deviceLoadReportResponse) {
	var decoded deviceLoadReportResponse
	body, err := json.Marshal(report)
	if err != nil {
		return 0, decoded
	}
	ctx, cancel := context.WithTimeout(ctx, loadReportHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, decoded
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: loadReportHTTPTimeout}).Do(req)
	if err != nil {
		return 0, decoded
	}
	defer resp.Body.Close()
	// A bounded read: the answer is a handful of fields.
	_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8*1024)).Decode(&decoded)
	return resp.StatusCode, decoded
}

// loadReporter owns the sampler.
type loadReporter struct {
	mu      sync.Mutex
	cfg     *Config
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	done    chan struct{}

	// last is the report last SENT, the baseline for change detection.
	last   deviceLoadReport
	sentAt time.Time

	// lastNote is the last reason a sample was NOT sent, so an offline or
	// draining computer writes one log line instead of one every 30 seconds
	// for as long as it stays that way (the agent's log file is size-bounded —
	// TERMINAL_AGENT_LOGGING.md — and a repeated skip would push out the lines
	// that explain something).
	lastNote string

	// The cadence in force, adopted from the server's answer within fixed
	// bounds (applyServerCadence).
	sampleInterval time.Duration
	maxInterval    time.Duration
	changePercent  int64
}

var globalLoadReporter = &loadReporter{}

// loadReportNow is time.Now, replaced in tests.
var loadReportNow = time.Now

// loadReportBlocked answers why no report may go out, or "".
//
// The same gates the usage propagator uses: a draining or shutting-down agent
// must not look busy or idle to a server that is about to lose it, and an
// offline computer is disconnected by the person's choice — reporting its load
// would be traffic they did not ask for.
func loadReportBlocked() string {
	switch {
	case IsShutdownInProgress(), isDraining():
		return "deferred"
	case IsOffline():
		return "skipped_offline"
	}
	return ""
}

// noteSkip logs a skip the first time it happens and stays quiet while the
// reason has not changed.
func (r *loadReporter) noteSkip(label string) {
	r.mu.Lock()
	repeat := r.lastNote == label
	r.lastNote = label
	r.mu.Unlock()
	if !repeat {
		logLoadReport("skipped: " + label)
	}
}

// noteSent clears the skip note, so the next skip is logged again.
func (r *loadReporter) noteSent(label string) {
	r.mu.Lock()
	r.lastNote = ""
	r.mu.Unlock()
	logLoadReport(label)
}

func logLoadReport(label string) {
	fmt.Printf("%s[load] %s%s\n", colorCyan, label, colorReset)
}

// startLoadReporter begins sampling. Safe to call once; later calls are no-ops.
func startLoadReporter(cfg *Config) {
	// Restore the owner's presence switch BEFORE the first sample. Unknown
	// withholds presence (see presenceAllowedByOwner), so without this an
	// opted-out computer would withhold for one interval after every restart
	// instead of none — and an opted-IN one would too.
	loadPresenceOwnerState()

	r := globalLoadReporter
	r.mu.Lock()
	if r.cfg != nil || r.stopped {
		r.mu.Unlock()
		return
	}
	r.cfg = cfg
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.sampleInterval, r.maxInterval, r.changePercent =
		loadSampleInterval, loadReportMaxInterval, loadReportChangePercent
	r.done = make(chan struct{})
	ctx, done := r.ctx, r.done
	r.mu.Unlock()

	go func() {
		defer close(done)
		r.loop(ctx)
	}()
}

// stopLoadReporter ends sampling.
//
// It waits briefly for the sampler to leave its loop. A POST already in flight
// may still complete — it carries a sample that was true when it was taken, so
// letting it land is harmless; what stops immediately is TAKING new ones. On
// the shutdown path the gate below has already closed anyway, because
// gracefulShutdown sets shutdownInProgress before calling this.
func stopLoadReporter() {
	r := globalLoadReporter
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// resetLoadReporter restores the zero reporter in place (tests).
func resetLoadReporter() {
	stopLoadReporter()
	r := globalLoadReporter
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg, r.ctx, r.cancel, r.done = nil, nil, nil, nil
	r.stopped = false
	r.last, r.sentAt, r.lastNote = deviceLoadReport{}, time.Time{}, ""
	r.sampleInterval, r.maxInterval, r.changePercent = 0, 0, 0
}

// loop samples until ctx ends.
func (r *loadReporter) loop(ctx context.Context) {
	for {
		r.tick(ctx)
		r.mu.Lock()
		wait := reportWait(r.sampleInterval, r.maxInterval)
		r.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// reportWait is how long the loop sleeps between samples: the SMALLER of the
// sample interval and the maximum report interval.
//
// The server sets the two in independent, optional fields, so a response can
// raise the sample interval while leaving the maximum alone. Sleeping for the
// sample interval then meant the loop only NOTICED the report was overdue on
// the following tick, turning "at least every 5 minutes" into every 10.
// `maxInterval` is the promise to the server; `sampleInterval` only decides how
// often a material change can be noticed, so ticking more often than it asks
// is always safe — `tick` itself still sends nothing that is not due.
// (Codex, PR #231.)
func reportWait(sample, promised time.Duration) time.Duration {
	if sample <= 0 {
		sample = loadSampleInterval
	}
	if promised > 0 && promised < sample {
		return promised
	}
	return sample
}

// tick takes one sample and sends it when it is due.
func (r *loadReporter) tick(ctx context.Context) {
	if blocked := loadReportBlocked(); blocked != "" {
		r.noteSkip(blocked)
		return
	}
	r.mu.Lock()
	cfg, last, sentAt, maxInterval, changePercent :=
		r.cfg, r.last, r.sentAt, r.maxInterval, r.changePercent
	r.mu.Unlock()
	if cfg == nil {
		return
	}

	// Read outside the lock: the probes take a second (the CPU window) and
	// the config read takes its own lock.
	agentID, secret := cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		r.noteSkip("unregistered")
		return
	}

	now := loadReportNow()
	report := sampleDeviceLoad(now)

	due := sentAt.IsZero() || now.Sub(sentAt) >= maxInterval
	if !due && !materialChange(last, report, changePercent) {
		return
	}

	// A stop that landed while the probes ran (the CPU window alone is a
	// second) must not be followed by a report.
	if ctx.Err() != nil {
		return
	}

	report.Signature = generateHMAC(buildDeviceLoadReportSignedMessage(agentID, report), secret)
	status, answer := sendDeviceLoadReport(ctx, fmt.Sprintf("%s/device/%s/load", baseURL, agentID), report)
	if status/100 != 2 {
		// No retry, and the baseline does not advance: the next sample is one
		// interval away and carries fresher numbers, so an unreachable server
		// is answered with new samples at the sampler's own pace rather than
		// with a backlog of stale ones. Retrying a snapshot that is already
		// out of date would spend the device's budget on nothing.
		r.noteSkip(fmt.Sprintf("failed_%d", status))
		return
	}

	r.mu.Lock()
	r.last, r.sentAt = report, now
	r.mu.Unlock()
	r.applyAnswer(answer)
	r.noteSent(fmt.Sprintf("sent: mem_available=%dMB pressure=%s cpu=%d%% sessions=%d presence=%q",
		report.MemAvailableMB, report.MemPressure, report.CPULoadPct, report.ActiveSessions, report.PresenceState))
}

// applyAnswer adopts what the server said, and only what it said.
func (r *loadReporter) applyAnswer(answer deviceLoadReportResponse) {
	if answer.PresenceEnabled != nil {
		setPresenceAllowedByOwner(*answer.PresenceEnabled)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sampleInterval = clampInterval(answer.SampleIntervalMs, r.sampleInterval)
	r.maxInterval = clampInterval(answer.MaxIntervalMs, r.maxInterval)
	if answer.ChangePercent != nil && *answer.ChangePercent >= 0 && *answer.ChangePercent <= 100 {
		r.changePercent = *answer.ChangePercent
	}
}

// Fixed bounds on a server-sent interval, so a bad or hostile answer can
// neither turn the sampler into a hot loop nor silence it for a day.
const (
	loadIntervalFloor   = 10 * time.Second
	loadIntervalCeiling = 30 * time.Minute
)

// clampInterval adopts a server-sent interval within the fixed bounds, or
// keeps the current one. An absent field keeps the current value — never zero.
func clampInterval(ms *int64, current time.Duration) time.Duration {
	if ms == nil {
		return current
	}
	d := time.Duration(*ms) * time.Millisecond
	if d < loadIntervalFloor || d > loadIntervalCeiling {
		return current
	}
	return d
}
