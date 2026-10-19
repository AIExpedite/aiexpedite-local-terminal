// The load / presence report (PARALLEL_RUNS_AND_COMPUTER_MAINTENANCE_PLAN.md
// §6.1, §8).
//
// The signed canonical, the cadence and the pressure vocabulary are asserted
// against testdata/device_load_report_vectors.json — the same file
// terminal-service's load.route.test.js and deviceLoad.service.test.js assert
// against @aiexpedite/shared-constants and its own builder. A change on any
// side fails on every side.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
)

func withLoadProbes(
	t *testing.T,
	vmem *mem.VirtualMemoryStat,
	swap *mem.SwapMemoryStat,
	cpuPct float64, cpuOK bool,
	sessions int, pids []int,
	rss map[int]uint64,
) {
	t.Helper()
	prevVmem, prevSwap, prevCPU, prevCounts, prevRSS :=
		loadProbeVirtualMemory, loadProbeSwapMemory, loadProbeCPUPercent, loadProbeSessionCounts, loadProbeProcessRSS

	loadProbeVirtualMemory = func() (*mem.VirtualMemoryStat, error) { return vmem, nil }
	loadProbeSwapMemory = func() (*mem.SwapMemoryStat, error) { return swap, nil }
	loadProbeCPUPercent = func() (float64, bool) { return cpuPct, cpuOK }
	loadProbeSessionCounts = func() (int, []int) { return sessions, pids }
	loadProbeProcessRSS = func(pid int) (uint64, bool) {
		bytes, ok := rss[pid]
		return bytes, ok
	}
	t.Cleanup(func() {
		loadProbeVirtualMemory, loadProbeSwapMemory, loadProbeCPUPercent, loadProbeSessionCounts, loadProbeProcessRSS =
			prevVmem, prevSwap, prevCPU, prevCounts, prevRSS
	})
}

// withBatteryProbe pins the AC reading and clears the cache around the test.
func withBatteryProbe(t *testing.T, on bool) {
	t.Helper()
	onBatteryCache.mu.Lock()
	onBatteryCache.at, onBatteryCache.on, onBatteryCache.got = time.Time{}, false, false
	onBatteryCache.mu.Unlock()
	previous := loadProbeOnBattery
	loadProbeOnBattery = func() (bool, bool) { return on, true }
	t.Cleanup(func() {
		loadProbeOnBattery = previous
		onBatteryCache.mu.Lock()
		onBatteryCache.at, onBatteryCache.on, onBatteryCache.got = time.Time{}, false, false
		onBatteryCache.mu.Unlock()
	})
}

func TestLoadReportCadenceMatchesTheSharedContract(t *testing.T) {
	contract := readLoadReportContract(t)

	if got := loadSampleInterval.Milliseconds(); got != contract.Cadence.SampleIntervalMs {
		t.Fatalf("loadSampleInterval = %dms, contract %dms", got, contract.Cadence.SampleIntervalMs)
	}
	if got := loadReportMaxInterval.Milliseconds(); got != contract.Cadence.MaxIntervalMs {
		t.Fatalf("loadReportMaxInterval = %dms, contract %dms", got, contract.Cadence.MaxIntervalMs)
	}
	if loadReportChangePercent != contract.Cadence.ChangePercent {
		t.Fatalf("loadReportChangePercent = %d, contract %d", int64(loadReportChangePercent), contract.Cadence.ChangePercent)
	}
	if loadMetricUnreadable != contract.Unreadable {
		t.Fatalf("loadMetricUnreadable = %d, contract %d", loadMetricUnreadable, contract.Unreadable)
	}

	want := []string{memPressureNormal, memPressureWarn, memPressureCritical}
	if len(contract.MemPressureLevels) != len(want) {
		t.Fatalf("contract has %d pressure levels, this build knows %d", len(contract.MemPressureLevels), len(want))
	}
	for i, level := range want {
		if contract.MemPressureLevels[i] != level {
			t.Fatalf("pressure level %d: contract %q, build %q", i, contract.MemPressureLevels[i], level)
		}
	}
}

// The canonical string and its signature, byte for byte against the file
// terminal-service's own tests assert. This is the one place a literal IS the
// assertion: the wire format must not move without both sides failing.
func TestDeviceLoadReportSignedMessageMatchesTheGoldenVectors(t *testing.T) {
	contract := readLoadReportContract(t)
	if len(contract.Vectors) == 0 {
		t.Fatal("no vectors in the contract file")
	}
	for _, vector := range contract.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			report := vector.Report
			report.Timestamp = vector.Timestamp
			message := buildDeviceLoadReportSignedMessage(vector.AgentID, report)
			if message != vector.Message {
				t.Fatalf("canonical:\n got %q\nwant %q", message, vector.Message)
			}
			if signature := generateHMAC(message, vector.Secret); signature != vector.Signature {
				t.Fatalf("signature: got %q, want %q", signature, vector.Signature)
			}
		})
	}
}

func TestDeviceLoadReportSignedMessageRendersBatteryAndAbsentPresence(t *testing.T) {
	base := deviceLoadReport{
		Timestamp:       1,
		MemTotalMB:      2,
		MemAvailableMB:  3,
		MemPressure:     memPressureWarn,
		SwapUsedMB:      4,
		CPULoadPct:      5,
		CLIProcesses:    6,
		CLIRssMB:        7,
		ActiveSessions:  8,
		OnBattery:       true,
		PresenceState:   presenceStateNotReported,
		PresenceSinceMs: loadMetricUnreadable,
	}
	if got, want := buildDeviceLoadReportSignedMessage("a", base), "a:1:2:3:warn:4:5:6:7:8:1::-1"; got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	base.OnBattery = false
	if got, want := buildDeviceLoadReportSignedMessage("a", base), "a:1:2:3:warn:4:5:6:7:8:0::-1"; got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
}

func TestClassifyMemPressure(t *testing.T) {
	cases := []struct {
		name                 string
		availableMB, totalMB int64
		want                 string
	}{
		{"plenty free is normal", 8192, 16384, memPressureNormal},
		{"exactly at the warn edge is warn", 3276, 16384, memPressureWarn},
		{"below a fifth free is warn", 2048, 16384, memPressureWarn},
		{"below a tenth free is critical", 1024, 16384, memPressureCritical},
		{"nothing free is critical", 0, 16384, memPressureCritical},
		{"an unreadable total is normal, never refusing", loadMetricUnreadable, loadMetricUnreadable, memPressureNormal},
		{"an unreadable available is normal, never refusing", loadMetricUnreadable, 16384, memPressureNormal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyMemPressure(c.availableMB, c.totalMB); got != c.want {
				t.Fatalf("classifyMemPressure(%d, %d) = %q, want %q", c.availableMB, c.totalMB, got, c.want)
			}
		})
	}
}

func TestSampleDeviceLoadReadsTheMachine(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{Idle: 7 * time.Minute, IdleOK: true})
	globalPresenceTracker.reset()
	t.Cleanup(globalPresenceTracker.reset)
	withBatteryProbe(t, true)
	withLoadProbes(t,
		&mem.VirtualMemoryStat{Total: 64 * 1024 * bytesPerMB, Available: 20 * 1024 * bytesPerMB},
		&mem.SwapMemoryStat{Used: 2 * 1024 * bytesPerMB},
		37.4, true,
		2, []int{11, 12},
		map[int]uint64{11: 1024 * bytesPerMB, 12: 512 * bytesPerMB},
	)

	now := time.Unix(1790000000, 0)
	report := sampleDeviceLoad(now)

	if report.Timestamp != now.UnixMilli() {
		t.Fatalf("timestamp = %d", report.Timestamp)
	}
	if report.MemTotalMB != 65536 || report.MemAvailableMB != 20480 {
		t.Fatalf("memory = %d/%d MB", report.MemAvailableMB, report.MemTotalMB)
	}
	if report.SwapUsedMB != 2048 {
		t.Fatalf("swap = %dMB", report.SwapUsedMB)
	}
	if report.CPULoadPct != 37 {
		t.Fatalf("cpu = %d%%", report.CPULoadPct)
	}
	if report.MemPressure != memPressureNormal {
		t.Fatalf("pressure = %q", report.MemPressure)
	}
	if report.ActiveSessions != 2 || report.CLIProcesses != 2 {
		t.Fatalf("sessions = %d, processes = %d", report.ActiveSessions, report.CLIProcesses)
	}
	if report.CLIRssMB != 1536 {
		t.Fatalf("rss = %dMB", report.CLIRssMB)
	}
	if !report.OnBattery {
		t.Fatal("expected onBattery")
	}
	if report.PresenceState != presenceStateIdle {
		t.Fatalf("presence = %q", report.PresenceState)
	}
	if report.PresenceSinceMs != (2 * time.Minute).Milliseconds() {
		t.Fatalf("presence age = %dms", report.PresenceSinceMs)
	}
}

func TestSampleDeviceLoadCarriesTheUnreadableSentinel(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{})
	withBatteryProbe(t, false)
	withLoadProbes(t, nil, nil, 0, false, 0, nil, nil)

	report := sampleDeviceLoad(time.Unix(1790000000, 0))

	for name, got := range map[string]int64{
		"memTotalMB":     report.MemTotalMB,
		"memAvailableMB": report.MemAvailableMB,
		"swapUsedMB":     report.SwapUsedMB,
		"cpuLoadPct":     report.CPULoadPct,
	} {
		if got != loadMetricUnreadable {
			t.Fatalf("%s = %d, want the unreadable sentinel", name, got)
		}
	}
	// An empty ledger is a measurement of zero, not an absence.
	if report.ActiveSessions != 0 || report.CLIProcesses != 0 || report.CLIRssMB != 0 {
		t.Fatalf("counts = %d/%d/%d", report.ActiveSessions, report.CLIProcesses, report.CLIRssMB)
	}
	if report.MemPressure != memPressureNormal {
		t.Fatalf("pressure = %q, want the non-refusing level", report.MemPressure)
	}
	if report.PresenceState != presenceStateNotReported || report.PresenceSinceMs != loadMetricUnreadable {
		t.Fatalf("presence = %q/%d", report.PresenceState, report.PresenceSinceMs)
	}
}

func TestSampleDeviceLoadReportsRSSUnreadableWhenEveryProbeFails(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{})
	withBatteryProbe(t, false)
	withLoadProbes(t,
		&mem.VirtualMemoryStat{Total: 16 * 1024 * bytesPerMB, Available: 8 * 1024 * bytesPerMB},
		&mem.SwapMemoryStat{}, 0, true,
		1, []int{404},
		map[int]uint64{},
	)

	report := sampleDeviceLoad(time.Unix(1790000000, 0))

	if report.CLIProcesses != 1 {
		t.Fatalf("processes = %d", report.CLIProcesses)
	}
	if report.CLIRssMB != loadMetricUnreadable {
		t.Fatalf("rss = %d, want the unreadable sentinel", report.CLIRssMB)
	}
	if report.CLIRssRead != 0 {
		t.Fatalf("covered = %d, want 0 — nothing was read", report.CLIRssRead)
	}
}

// THE FIGURE HAS TO SAY HOW MUCH OF THE MACHINE IT COVERS.
//
// Found in the fleet rather than in a test: the first Windows computer on
// v1.1.14 reported `cliProcesses: 1` and no `cliRssMB` at all, while a mac
// with no CLI running reported `cliRssMB: 0`. The figure went missing exactly
// when a CLI was running. The all-failed case at least disappears visibly;
// the PARTIAL case did not — one process read out of three was reported as a
// whole total, an undercount indistinguishable from the real thing, and peak
// RSS is the number Phase 0 exists to collect.
func TestSampleDeviceLoadReportsHowManyProcessesTheRSSCovers(t *testing.T) {
	for _, c := range []struct {
		name     string
		tracked  int
		pids     []int
		rss      map[int]uint64
		wantMB   int64
		wantRead int64
	}{
		{
			name:    "no processes is a measurement of zero",
			tracked: 0, pids: nil, rss: map[int]uint64{},
			wantMB: 0, wantRead: 0,
		},
		{
			name:    "every process read is a whole total",
			tracked: 2, pids: []int{11, 12},
			rss:    map[int]uint64{11: 100 * bytesPerMB, 12: 200 * bytesPerMB},
			wantMB: 300, wantRead: 2,
		},
		{
			// The one that was silent: the sum stands, and the count is what
			// stops a reader taking 100MB for this computer's whole CLI load.
			name:    "one of three read is a partial total, and says so",
			tracked: 3, pids: []int{21, 22, 23},
			rss:    map[int]uint64{22: 100 * bytesPerMB},
			wantMB: 100, wantRead: 1,
		},
		{
			name:    "none read is no measurement",
			tracked: 2, pids: []int{31, 32}, rss: map[int]uint64{},
			wantMB: loadMetricUnreadable, wantRead: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			withOwnerSwitch(t, true)
			withPresenceEnv(t, "", false)
			withProbe(t, presenceSignals{})
			withBatteryProbe(t, false)
			withLoadProbes(t,
				&mem.VirtualMemoryStat{Total: 16 * 1024 * bytesPerMB, Available: 8 * 1024 * bytesPerMB},
				&mem.SwapMemoryStat{}, 0, true,
				c.tracked, c.pids,
				c.rss,
			)

			report := sampleDeviceLoad(time.Unix(1790000000, 0))

			if report.CLIRssMB != c.wantMB {
				t.Fatalf("rss = %dMB, want %dMB", report.CLIRssMB, c.wantMB)
			}
			if report.CLIRssRead != c.wantRead {
				t.Fatalf("covered = %d of %d, want %d", report.CLIRssRead, report.CLIProcesses, c.wantRead)
			}
		})
	}
}

func TestSampleDeviceLoadCapsTheRSSWalk(t *testing.T) {
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{})
	withBatteryProbe(t, false)

	pids := make([]int, loadRSSPidCap+10)
	rss := map[int]uint64{}
	for i := range pids {
		pids[i] = i + 1
		rss[i+1] = bytesPerMB
	}
	withLoadProbes(t, nil, nil, 0, false, 1, pids, rss)

	report := sampleDeviceLoad(time.Unix(1790000000, 0))

	// Every process is counted, but only the capped prefix is measured.
	if report.CLIProcesses != int64(loadRSSPidCap+10) {
		t.Fatalf("processes = %d", report.CLIProcesses)
	}
	if report.CLIRssMB != int64(loadRSSPidCap) {
		t.Fatalf("rss = %dMB, want %dMB", report.CLIRssMB, loadRSSPidCap)
	}
}

func TestClampPct(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want int64
	}{{-5, 0}, {0, 0}, {37.4, 37}, {37.6, 38}, {100, 100}, {140, 100}} {
		if got := clampPct(c.in); got != c.want {
			t.Fatalf("clampPct(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMaterialChange(t *testing.T) {
	base := deviceLoadReport{
		Timestamp:       1790000000000,
		MemTotalMB:      16384,
		MemAvailableMB:  8192,
		MemPressure:     memPressureNormal,
		SwapUsedMB:      0,
		CPULoadPct:      40,
		CLIProcesses:    1,
		CLIRssMB:        512,
		ActiveSessions:  1,
		PresenceState:   presenceStateActive,
		PresenceSinceMs: 1000,
	}
	change := func(mutate func(*deviceLoadReport)) deviceLoadReport {
		next := base
		mutate(&next)
		return next
	}

	cases := []struct {
		name string
		next deviceLoadReport
		want bool
	}{
		{"an identical sample is not material", base, false},
		{"the age of an unchanged presence state is not material", change(func(r *deviceLoadReport) { r.PresenceSinceMs = 999999 }), false},
		{"a smaller RSS alone is not material", change(func(r *deviceLoadReport) { r.CLIRssMB = 64 }), false},
		{"swap alone is not material", change(func(r *deviceLoadReport) { r.SwapUsedMB = 4096 }), false},
		{"a CPU move below the threshold is not material", change(func(r *deviceLoadReport) { r.CPULoadPct = 44 }), false},
		{"a CPU move at the threshold is material", change(func(r *deviceLoadReport) { r.CPULoadPct = 45 }), true},
		{"a CPU drop at the threshold is material", change(func(r *deviceLoadReport) { r.CPULoadPct = 35 }), true},
		{"a memory move below 5% of total is not material", change(func(r *deviceLoadReport) { r.MemAvailableMB = 8192 - 818 }), false},
		{"a memory move at 5% of total is material", change(func(r *deviceLoadReport) { r.MemAvailableMB = 8192 - 820 }), true},
		{"a pressure change is material", change(func(r *deviceLoadReport) { r.MemPressure = memPressureWarn }), true},
		{"a presence change is material", change(func(r *deviceLoadReport) { r.PresenceState = presenceStateAway }), true},
		{"presence going unreported is material", change(func(r *deviceLoadReport) { r.PresenceState = presenceStateNotReported }), true},
		{"unplugging is material", change(func(r *deviceLoadReport) { r.OnBattery = true }), true},
		{"a new session is material", change(func(r *deviceLoadReport) { r.ActiveSessions = 2 }), true},
		{"a new CLI process is material", change(func(r *deviceLoadReport) { r.CLIProcesses = 2 }), true},
		{"a metric becoming unreadable is material", change(func(r *deviceLoadReport) { r.CPULoadPct = loadMetricUnreadable }), true},
		{"memory becoming unreadable is material", change(func(r *deviceLoadReport) { r.MemAvailableMB = loadMetricUnreadable }), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := materialChange(base, c.next, loadReportChangePercent); got != c.want {
				t.Fatalf("materialChange = %v, want %v", got, c.want)
			}
		})
	}

	t.Run("the first report is always material", func(t *testing.T) {
		if !materialChange(deviceLoadReport{}, base, loadReportChangePercent) {
			t.Fatal("the first report must be sent")
		}
	})
}

func TestClampIntervalKeepsTheCurrentValueOutsideTheFixedBounds(t *testing.T) {
	current := loadSampleInterval
	ms := func(v int64) *int64 { return &v }

	if got := clampInterval(nil, current); got != current {
		t.Fatalf("absent field changed the interval to %v", got)
	}
	if got := clampInterval(ms(0), current); got != current {
		t.Fatalf("zero was adopted: %v", got)
	}
	if got := clampInterval(ms(-1000), current); got != current {
		t.Fatalf("a negative interval was adopted: %v", got)
	}
	if got := clampInterval(ms(time.Hour.Milliseconds()), current); got != current {
		t.Fatalf("an interval past the ceiling was adopted: %v", got)
	}
	if got := clampInterval(ms(60_000), current); got != time.Minute {
		t.Fatalf("a valid interval was not adopted: %v", got)
	}
	if got := clampInterval(ms(loadIntervalFloor.Milliseconds()), current); got != loadIntervalFloor {
		t.Fatalf("the floor was not adopted: %v", got)
	}
}

func TestApplyAnswerAdoptsOnlyWhatTheServerSaid(t *testing.T) {
	withOwnerSwitch(t, true)
	reporter := &loadReporter{
		sampleInterval: loadSampleInterval,
		maxInterval:    loadReportMaxInterval,
		changePercent:  loadReportChangePercent,
	}

	// An older server that answers none of these leaves everything alone — and
	// in particular does not read an absent `presenceEnabled` as a decision
	// either way. Seeded as ALLOWED first, so "left alone" is observable: the
	// state is tri-state now and its zero value is UNKNOWN, which withholds.
	setPresenceAllowedByOwner(true)
	reporter.applyAnswer(deviceLoadReportResponse{})
	if presenceAllowedByOwner.Load() != presenceOwnerAllowed {
		t.Fatal("an absent presenceEnabled changed the owner's switch")
	}
	if reporter.sampleInterval != loadSampleInterval || reporter.maxInterval != loadReportMaxInterval {
		t.Fatalf("an empty answer changed the cadence: %v/%v", reporter.sampleInterval, reporter.maxInterval)
	}
	if reporter.changePercent != loadReportChangePercent {
		t.Fatalf("an empty answer changed the threshold: %d", reporter.changePercent)
	}

	off, sample, max, percent := false, int64(45_000), int64(600_000), int64(9)
	reporter.applyAnswer(deviceLoadReportResponse{
		PresenceEnabled:  &off,
		SampleIntervalMs: &sample,
		MaxIntervalMs:    &max,
		ChangePercent:    &percent,
	})
	if presenceAllowedByOwner.Load() != presenceOwnerDenied {
		t.Fatal("the owner's off switch was not adopted")
	}
	if reporter.sampleInterval != 45*time.Second || reporter.maxInterval != 10*time.Minute {
		t.Fatalf("cadence = %v/%v", reporter.sampleInterval, reporter.maxInterval)
	}
	if reporter.changePercent != 9 {
		t.Fatalf("threshold = %d", reporter.changePercent)
	}

	tooBig := int64(1000)
	reporter.applyAnswer(deviceLoadReportResponse{ChangePercent: &tooBig})
	if reporter.changePercent != 9 {
		t.Fatalf("an out-of-range threshold was adopted: %d", reporter.changePercent)
	}
}

/* ----------------------------------------------------------------------- *
 * One tick, end to end, with the network stubbed.
 * ----------------------------------------------------------------------- */

type sentReport struct {
	url    string
	report deviceLoadReport
}

func withStubbedSend(t *testing.T, status int, answer deviceLoadReportResponse) *[]sentReport {
	t.Helper()
	sent := []sentReport{}
	previous := sendDeviceLoadReport
	sendDeviceLoadReport = func(_ context.Context, url string, report deviceLoadReport) (int, deviceLoadReportResponse) {
		sent = append(sent, sentReport{url: url, report: report})
		return status, answer
	}
	t.Cleanup(func() { sendDeviceLoadReport = previous })
	return &sent
}

func newTestReporter(t *testing.T) *loadReporter {
	t.Helper()
	return &loadReporter{
		cfg:            &Config{AgentID: "agent-1", CommandSecret: "secret"},
		sampleInterval: loadSampleInterval,
		maxInterval:    loadReportMaxInterval,
		changePercent:  loadReportChangePercent,
	}
}

func withQuietMachine(t *testing.T) {
	t.Helper()
	withOwnerSwitch(t, true)
	withPresenceEnv(t, "", false)
	withProbe(t, presenceSignals{Idle: time.Minute, IdleOK: true})
	globalPresenceTracker.reset()
	t.Cleanup(globalPresenceTracker.reset)
	withBatteryProbe(t, false)
	withLoadProbes(t,
		&mem.VirtualMemoryStat{Total: 16 * 1024 * bytesPerMB, Available: 8 * 1024 * bytesPerMB},
		&mem.SwapMemoryStat{}, 10, true,
		0, nil, nil,
	)
}

func withFrozenClock(t *testing.T, at time.Time) func(time.Duration) {
	t.Helper()
	now := at
	previous := loadReportNow
	loadReportNow = func() time.Time { return now }
	t.Cleanup(func() { loadReportNow = previous })
	return func(d time.Duration) { now = now.Add(d) }
}

func TestTickSendsASignedReportAndAdoptsTheAnswer(t *testing.T) {
	withQuietMachine(t)
	advance := withFrozenClock(t, time.Unix(1790000000, 0))
	_ = advance
	enabled := true
	sent := withStubbedSend(t, 202, deviceLoadReportResponse{PresenceEnabled: &enabled})
	reporter := newTestReporter(t)

	reporter.tick(context.Background())

	if len(*sent) != 1 {
		t.Fatalf("sent %d reports, want 1", len(*sent))
	}
	got := (*sent)[0]
	if got.url != getRegistrationURL()+"/device/agent-1/load" {
		t.Fatalf("url = %q", got.url)
	}
	// The signature must cover exactly the report that was sent, with the
	// signature field itself excluded from the canonical.
	unsigned := got.report
	unsigned.Signature = ""
	want := generateHMAC(buildDeviceLoadReportSignedMessage("agent-1", unsigned), "secret")
	if got.report.Signature != want {
		t.Fatalf("signature = %q, want %q", got.report.Signature, want)
	}
	if got.report.PresenceState != presenceStateActive {
		t.Fatalf("presence = %q", got.report.PresenceState)
	}
}

func TestTickHoldsAnUnchangedSampleUntilTheFloor(t *testing.T) {
	withQuietMachine(t)
	advance := withFrozenClock(t, time.Unix(1790000000, 0))
	sent := withStubbedSend(t, 202, deviceLoadReportResponse{})
	reporter := newTestReporter(t)

	reporter.tick(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("the first sample was not sent")
	}

	// Nothing changed: every sample inside the floor is held.
	for range 5 {
		advance(loadSampleInterval)
		reporter.tick(context.Background())
	}
	if len(*sent) != 1 {
		t.Fatalf("sent %d reports inside the floor, want 1", len(*sent))
	}

	// Past the floor, one report goes out even though nothing changed.
	advance(loadReportMaxInterval)
	reporter.tick(context.Background())
	if len(*sent) != 2 {
		t.Fatalf("sent %d reports, want 2", len(*sent))
	}
}

func TestTickSendsAMaterialChangeInsideTheFloor(t *testing.T) {
	withQuietMachine(t)
	advance := withFrozenClock(t, time.Unix(1790000000, 0))
	sent := withStubbedSend(t, 202, deviceLoadReportResponse{})
	reporter := newTestReporter(t)

	reporter.tick(context.Background())
	advance(loadSampleInterval)

	// The person stepped away: a state change, well inside the floor.
	probePresenceSignals = func() presenceSignals {
		return presenceSignals{Idle: 30 * time.Minute, IdleOK: true}
	}
	reporter.tick(context.Background())

	if len(*sent) != 2 {
		t.Fatalf("sent %d reports, want 2", len(*sent))
	}
	if (*sent)[1].report.PresenceState != presenceStateAway {
		t.Fatalf("presence = %q", (*sent)[1].report.PresenceState)
	}
}

func TestTickDoesNotAdvanceTheBaselineOnAFailedSend(t *testing.T) {
	withQuietMachine(t)
	advance := withFrozenClock(t, time.Unix(1790000000, 0))
	sent := withStubbedSend(t, 503, deviceLoadReportResponse{})
	reporter := newTestReporter(t)

	reporter.tick(context.Background())
	advance(loadSampleInterval)
	reporter.tick(context.Background())

	// A failure is not retried in place, but the next sample is still due:
	// the baseline never advanced, so nothing is silently skipped.
	if len(*sent) != 2 {
		t.Fatalf("sent %d reports, want 2", len(*sent))
	}
	if !reporter.sentAt.IsZero() {
		t.Fatal("a failed send advanced the report floor")
	}
}

func TestTickIsSilentWhileOfflineDrainingOrShuttingDown(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{
			name: "offline",
			setup: func(t *testing.T) {
				SetOffline(true)
				t.Cleanup(func() { SetOffline(false) })
			},
		},
		{
			name: "draining",
			setup: func(t *testing.T) {
				drain.mu.Lock()
				drain.draining = true
				drain.mu.Unlock()
				t.Cleanup(func() {
					drain.mu.Lock()
					drain.draining = false
					drain.mu.Unlock()
				})
			},
		},
		{
			name: "shutting down",
			setup: func(t *testing.T) {
				shutdownInProgress.Store(true)
				t.Cleanup(func() { shutdownInProgress.Store(false) })
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withQuietMachine(t)
			withFrozenClock(t, time.Unix(1790000000, 0))
			sent := withStubbedSend(t, 202, deviceLoadReportResponse{})
			reporter := newTestReporter(t)
			c.setup(t)

			reporter.tick(context.Background())

			if len(*sent) != 0 {
				t.Fatalf("sent %d reports while %s", len(*sent), c.name)
			}
		})
	}
}

// A computer that stays offline for a day must not write one skip line every
// 30 seconds into a size-bounded log file; the reason is noted once and
// cleared by the next report that lands.
func TestTickNotesARepeatedSkipOnlyOnce(t *testing.T) {
	withQuietMachine(t)
	withFrozenClock(t, time.Unix(1790000000, 0))
	sent := withStubbedSend(t, 202, deviceLoadReportResponse{})
	reporter := newTestReporter(t)

	SetOffline(true)
	reporter.tick(context.Background())
	if reporter.lastNote != "skipped_offline" {
		t.Fatalf("lastNote = %q, want the offline reason", reporter.lastNote)
	}
	reporter.tick(context.Background())
	if reporter.lastNote != "skipped_offline" {
		t.Fatalf("lastNote = %q after a repeat", reporter.lastNote)
	}
	if len(*sent) != 0 {
		t.Fatalf("sent %d reports while offline", len(*sent))
	}

	SetOffline(false)
	reporter.tick(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("sent %d reports once back online, want 1", len(*sent))
	}
	if reporter.lastNote != "" {
		t.Fatalf("lastNote = %q after a report landed, want it cleared", reporter.lastNote)
	}
}

func TestTickSendsNothingWhenTheComputerIsNotRegistered(t *testing.T) {
	withQuietMachine(t)
	withFrozenClock(t, time.Unix(1790000000, 0))
	sent := withStubbedSend(t, 202, deviceLoadReportResponse{})
	reporter := newTestReporter(t)
	reporter.cfg = &Config{}

	reporter.tick(context.Background())

	if len(*sent) != 0 {
		t.Fatalf("sent %d reports unregistered", len(*sent))
	}
}

func TestSampleOnBatteryCachesForOneReportInterval(t *testing.T) {
	withBatteryProbe(t, true)
	calls := 0
	previous := loadProbeOnBattery
	loadProbeOnBattery = func() (bool, bool) {
		calls++
		return true, true
	}
	t.Cleanup(func() { loadProbeOnBattery = previous })

	now := time.Unix(1790000000, 0)
	if !sampleOnBattery(now) {
		t.Fatal("expected on battery")
	}
	if !sampleOnBattery(now.Add(loadReportMaxInterval - time.Second)) {
		t.Fatal("expected the cached reading")
	}
	if calls != 1 {
		t.Fatalf("probed %d times inside the interval, want 1", calls)
	}
	sampleOnBattery(now.Add(loadReportMaxInterval))
	if calls != 2 {
		t.Fatalf("probed %d times after the interval, want 2", calls)
	}
}

func TestSampleOnBatteryReportsFalseWhenTheACStateIsUnreadable(t *testing.T) {
	withBatteryProbe(t, false)
	previous := loadProbeOnBattery
	loadProbeOnBattery = func() (bool, bool) { return true, false }
	t.Cleanup(func() { loadProbeOnBattery = previous })

	if sampleOnBattery(time.Unix(1790000000, 0)) {
		t.Fatal("an unreadable AC state must not read as on battery")
	}
}

func TestLoadCountsOnlyCountsThisBoot(t *testing.T) {
	ledger := &spawnLedger{bootID: "boot-now"}
	ledger.entries = []*ledgerEntry{
		{SessionID: "s1", BootID: "boot-now", PIDs: []ledgerProcess{{PID: 11}, {PID: 12}}},
		{SessionID: "s2", BootID: "boot-now", Logical: true},
		{SessionID: "s3", BootID: "boot-before", PIDs: []ledgerProcess{{PID: 99}}},
		{SessionID: "s4", BootID: "boot-now", PIDs: []ledgerProcess{{PID: 0}}},
	}

	sessions, pids := ledger.loadCounts()

	if sessions != 3 {
		t.Fatalf("sessions = %d, want 3", sessions)
	}
	if len(pids) != 2 {
		t.Fatalf("pids = %v, want the two of this boot", pids)
	}
	for _, pid := range pids {
		if pid == 99 || pid == 0 {
			t.Fatalf("pids = %v, must exclude an earlier boot and a zero pid", pids)
		}
	}
}

// A session that ended while its group / job still held processes keeps its
// current-boot entry, with `State` still `live`, because the boot reap needs
// it. Counting it told the server a finished session was running for the rest
// of this process's life — and `activeSessions` is the Phase 0 number
// parallelism will be sized on. Its surviving PIDs are real memory on this
// computer, so they stay counted. (Codex, PR #231.)
func TestLoadCountsDropsEndedSessionsButKeepsTheirProcesses(t *testing.T) {
	ledger := &spawnLedger{bootID: "boot-now"}
	ledger.entries = []*ledgerEntry{
		{SessionID: "open", BootID: "boot-now", PIDs: []ledgerProcess{{PID: 11}}},
		{SessionID: "ended-leftovers", BootID: "boot-now", Released: true, PIDs: []ledgerProcess{{PID: 12}}},
	}

	sessions, pids := ledger.loadCounts()

	if sessions != 1 {
		t.Fatalf("sessions = %d, want only the open one", sessions)
	}
	if len(pids) != 2 {
		t.Fatalf("pids = %v, want both — a leftover process still occupies memory", pids)
	}
}

// The two intervals are adopted from independent, optional response fields, so
// a response can raise the sample interval above the maximum. The loop has to
// tick at the smaller of the two or the maximum stops being a maximum.
// (Codex, PR #231.)
func TestReportWaitNeverExceedsTheMaximumInterval(t *testing.T) {
	for _, c := range []struct {
		name             string
		sample, promised time.Duration
		want             time.Duration
	}{
		{"the sample interval when it is the smaller", time.Minute, 5 * time.Minute, time.Minute},
		{"the maximum when the sample interval overruns it", 10 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		{"the sample interval when no maximum is set", 10 * time.Minute, 0, 10 * time.Minute},
		{"the default when the sample interval is unset", 0, 30 * time.Minute, loadSampleInterval},
		{"the maximum when both are unset but it is smaller", 0, time.Second, time.Second},
	} {
		if got := reportWait(c.sample, c.promised); got != c.want {
			t.Fatalf("%s: reportWait(%v, %v) = %v, want %v", c.name, c.sample, c.promised, got, c.want)
		}
	}
}

func TestLoadReporterStartIsIdempotentAndStopIsFinal(t *testing.T) {
	withQuietMachine(t)
	withFrozenClock(t, time.Unix(1790000000, 0))
	t.Cleanup(resetLoadReporter)
	resetLoadReporter()

	// A channel rather than a counter: the sampler is a goroutine, so the test
	// waits for its first report instead of racing it.
	reports := make(chan deviceLoadReport, 8)
	previous := sendDeviceLoadReport
	sendDeviceLoadReport = func(_ context.Context, _ string, report deviceLoadReport) (int, deviceLoadReportResponse) {
		reports <- report
		return 202, deviceLoadReportResponse{}
	}
	t.Cleanup(func() { sendDeviceLoadReport = previous })

	cfg := &Config{AgentID: "agent-1", CommandSecret: "secret"}
	startLoadReporter(cfg)
	startLoadReporter(cfg)

	select {
	case <-reports:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler sent no report")
	}

	// A second sampler would report at once (the clock is frozen, so its
	// baseline is empty and the sample is material). One sampler waits a full
	// interval instead.
	select {
	case <-reports:
		t.Fatal("a second call to startLoadReporter started a second sampler")
	case <-time.After(250 * time.Millisecond):
	}

	stopLoadReporter()
	startLoadReporter(cfg)
	select {
	case <-reports:
		t.Fatal("a report went out after stopLoadReporter")
	case <-time.After(250 * time.Millisecond):
	}
}
