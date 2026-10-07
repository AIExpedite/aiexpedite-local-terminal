//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_opencode_live_windows_test.go — the stage-one reproduction
   gate for "OpenCode noninteractive smoke exits before the marker envelope".
   --------------------------------------------------------------------------
   OPT-IN, never part of CI. It spends real inference turns against the
   provider configured behind the installed OpenCode, so it runs only when

       AIX_OPENCODE_LIVE=1
       AIX_OPENCODE_LIVE_BIN=<path to the affected opencode(.cmd)>

   are both set, pinned to the version the nightly smoke reported:

       go test -run TestOpenCodeLiveGate -count=1 -v -timeout 15m .

   It drives the REAL runOpenCodeSmoke pipeline three times — through today's
   default os/exec pipe, through a 1 MiB CreatePipe, and through a file sink —
   with launcher, shim route, argv, env, cwd, auth and prompt identical in every
   mode, so those act as controls. Then two controls on prompt delivery: a
   64-byte stdin prompt against the affected build (does it refuse with "must
   provide a message"?) and against the compiled stub through the same cmd.exe
   route (does cmd.exe pass stdin through at all?), and whether the build
   accepts `run --file`.

   Everything it logs is a closed value: counts, booleans, the closed
   diagnostic, allowlisted event-type counts, and for an unknown event type only
   the first 8 hex characters of its SHA-256. No CLI text, prompt, marker or
   type literal is logged. The file sink holds child output in t.TempDir() for
   the length of one run.

   The last log line names the design branch the outcome table selects. A green
   run here that does not reproduce the field failure does NOT close the
   feature: run it on the nightly Windows device itself.
   ------------------------------------------------------------------------ */

// openCodeLiveKnownEventTypes is the allowlist of event types logged by name.
// Anything else is counted and digested, never spelled.
var openCodeLiveKnownEventTypes = []string{
	"step_start", "step_finish", "text", "tool_use", "tool_result", "reasoning",
	"error", "session.idle", "session.started", "session.completed",
}

// openCodeLiveRow is one run's closed-value record.
type openCodeLiveRow struct {
	Mode           string
	Counts         openCodeSmokeCounts
	Diagnostic     string
	MarkerMatched  bool
	TypeCounts     map[string]int
	UnknownTypes   int
	UnknownDigests []string
}

func (r openCodeLiveRow) String() string {
	known := make([]string, 0, len(r.TypeCounts))
	for name, n := range r.TypeCounts {
		known = append(known, fmt.Sprintf("%s=%d", name, n))
	}
	sort.Strings(known)
	return fmt.Sprintf("mode=%s exit=%d stdoutBytes=%d stderrBytes=%d frames=%d escLines=%d terminal=%t diagnostic=%s markerMatched=%t types=[%s] unknownTypes=%d unknownDigests=[%s]",
		r.Mode, r.Counts.Exit, r.Counts.StdoutBytes, r.Counts.StderrBytes, r.Counts.Frames,
		r.Counts.EscLines, r.Counts.Terminal, firstNonEmpty(r.Diagnostic, "none"), r.MarkerMatched,
		strings.Join(known, " "), r.UnknownTypes, strings.Join(r.UnknownDigests, " "))
}

// openCodeLiveEventTypes counts event types in a capture: allowlisted names by
// name, anything else by an 8-hex SHA-256 prefix.
func openCodeLiveEventTypes(stdout []byte) (known map[string]int, unknown int, digests []string) {
	known = map[string]int{}
	allowed := map[string]bool{}
	for _, name := range openCodeLiveKnownEventTypes {
		allowed[name] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		frame, ok := parseOpenCodeSmokeFrame(line)
		if !ok {
			continue
		}
		if allowed[frame.Type] {
			known[frame.Type]++
			continue
		}
		unknown++
		sum := sha256.Sum256([]byte(frame.Type))
		digest := hex.EncodeToString(sum[:])[:8]
		if !seen[digest] {
			seen[digest] = true
			digests = append(digests, digest)
		}
	}
	sort.Strings(digests)
	return known, unknown, digests
}

func openCodeLiveRowFor(mode string, stdout, stderr []byte, runErr error, diagnostic string, matched bool) openCodeLiveRow {
	known, unknown, digests := openCodeLiveEventTypes(stdout)
	return openCodeLiveRow{
		Mode: mode, Counts: openCodeSmokeCountsFor(stdout, stderr, runErr),
		Diagnostic: diagnostic, MarkerMatched: matched,
		TypeCounts: known, UnknownTypes: unknown, UnknownDigests: digests,
	}
}

/* --------------------------------------------------------------------------
   Capture modes — each builds the SAME child via newOpenCodeSmokeCmd
   -------------------------------------------------------------------------- */

type openCodeLiveCapture func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error)

// openCodeLiveLargePipe captures stdout through a CreatePipe with a SUGGESTED
// 1 MiB buffer (Windows may grant less), drained by a reader started before the
// child, bounded by cliSmokeMaxStdout, force-closed openCodeSmokeWaitDelay
// after exit so a lingering grandchild cannot hold the join.
func openCodeLiveLargePipe(ctx context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
	cmd, closePrompt, err := newOpenCodeSmokeCmd(ctx, launch)
	if err != nil {
		return nil, nil, err
	}
	defer closePrompt()
	var r, w syscall.Handle
	if err := syscall.CreatePipe(&r, &w, nil, 1<<20); err != nil {
		return nil, nil, err
	}
	readEnd, writeEnd := os.NewFile(uintptr(r), "smoke-stdout-r"), os.NewFile(uintptr(w), "smoke-stdout-w")
	out := &boundedBuffer{limit: cliSmokeMaxStdout}
	errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, readEnd)
		close(drained)
	}()
	cmd.Stdout = writeEnd
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		_ = writeEnd.Close()
		_ = readEnd.Close()
		<-drained
		return nil, nil, err
	}
	_ = writeEnd.Close()
	waitErr := cmd.Wait()
	select {
	case <-drained:
	case <-time.After(openCodeSmokeWaitDelay):
		_ = readEnd.Close()
		<-drained
	}
	_ = readEnd.Close()
	return out.Bytes(), errBuf.Bytes(), waitErr
}

// openCodeLiveFileSink lets the child write stdout straight to a file handle in
// dir, and reads it back (bounded) after exit.
func openCodeLiveFileSink(dir string) openCodeLiveCapture {
	return func(ctx context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		cmd, closePrompt, err := newOpenCodeSmokeCmd(ctx, launch)
		if err != nil {
			return nil, nil, err
		}
		defer closePrompt()
		sink, err := os.CreateTemp(dir, "stdout-*")
		if err != nil {
			return nil, nil, err
		}
		defer func() {
			_ = sink.Close()
			_ = os.Remove(sink.Name())
		}()
		errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
		cmd.Stdout = sink
		cmd.Stderr = errBuf
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		waitErr := cmd.Wait()
		if _, err := sink.Seek(0, io.SeekStart); err != nil {
			return nil, errBuf.Bytes(), err
		}
		out, _ := io.ReadAll(io.LimitReader(sink, cliSmokeMaxStdout))
		return out, errBuf.Bytes(), waitErr
	}
}

// runOpenCodeLiveSmokeMode runs the real probe with its exec seam routed
// through capture, recording what the child produced.
func runOpenCodeLiveSmokeMode(t *testing.T, mode, bin, version string, capture openCodeLiveCapture) openCodeLiveRow {
	t.Helper()
	var stdout, stderr []byte
	var runErr error
	original := runOpenCodeSmokeCommand
	runOpenCodeSmokeCommand = func(ctx context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		stdout, stderr, runErr = capture(ctx, launch)
		return stdout, stderr, runErr
	}
	defer func() { runOpenCodeSmokeCommand = original }()
	result := runOpenCodeSmoke(context.Background(), bin, version)
	return openCodeLiveRowFor(mode, stdout, stderr, runErr, result.Diagnostic, result.MarkerMatched)
}

/* --------------------------------------------------------------------------
   Prompt-delivery controls
   -------------------------------------------------------------------------- */

// openCodeLiveStdinPrompt is exactly 64 bytes, asks for nothing sensitive, and
// ends in a token the stub's echo returns.
var openCodeLiveStdinPrompt = fmt.Sprintf("%-63s", "Reply with the single word READY and nothing else. READY") + "\n"

// runOpenCodeLiveOneShot spawns `bin` with extra args after the run shape and
// promptBody as stdin, through the same launcher and capture as the probe.
func runOpenCodeLiveOneShot(t *testing.T, bin string, extraArgs []string, promptBody string, env []string) (stdout, stderr []byte, runErr error, timedOut bool) {
	t.Helper()
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "stdin.txt")
	if err := os.WriteFile(promptPath, []byte(promptBody), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), openCodeSmokeTimeout)
	defer cancel()
	stdout, stderr, runErr = runOpenCodeSmokeCommand(ctx, openCodeLaunch{
		Path:        bin,
		Args:        append(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), extraArgs...),
		Env:         env,
		Dir:         runDir,
		PromptFile:  promptPath,
		Maintenance: true,
	})
	return stdout, stderr, runErr, ctx.Err() != nil
}

/* --------------------------------------------------------------------------
   The gate
   -------------------------------------------------------------------------- */

// openCodeLiveGateBinary resolves the opt-in gate's binary, skipping the row
// when the gate is off. Shared so every live row opts in the same way.
func openCodeLiveGateBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv("AIX_OPENCODE_LIVE") != "1" {
		t.Skip("opt-in live gate: set AIX_OPENCODE_LIVE=1 and AIX_OPENCODE_LIVE_BIN")
	}
	bin := os.Getenv("AIX_OPENCODE_LIVE_BIN")
	if bin == "" {
		t.Fatal("AIX_OPENCODE_LIVE_BIN must point at the affected opencode build")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("AIX_OPENCODE_LIVE_BIN is not stattable: %v", err)
	}
	return bin
}

func TestOpenCodeLiveGate_RecordsTheRootCauseOutcomeTable(t *testing.T) {
	bin := openCodeLiveGateBinary(t)
	resetCLISmokeState()
	resetVersionProbeCache()
	resetOpenCodeVersionNegatives()
	resetOpenCodeReadinessCache()
	version := openCodeProbeVersion(bin)
	if version == "" {
		t.Fatal("the affected build does not answer --version")
	}
	t.Logf("gate version=%s shim=%t", version, isWindowsShimPath(bin))

	defaultRow := runOpenCodeLiveSmokeMode(t, "default-pipe", bin, version, runOpenCodeSmokeCommand)
	pipeRow := runOpenCodeLiveSmokeMode(t, "pipe-1MiB", bin, version, openCodeLiveLargePipe)
	fileRow := runOpenCodeLiveSmokeMode(t, "file-sink", bin, version, openCodeLiveFileSink(t.TempDir()))
	for _, row := range []openCodeLiveRow{defaultRow, pipeRow, fileRow} {
		t.Logf("gate %s", row)
	}

	// Stdin control against the affected build.
	env := sanitizeOpenCodeEnv(os.Environ())
	out, errb, runErr, _ := runOpenCodeLiveOneShot(t, bin, nil, openCodeLiveStdinPrompt, env)
	stdinRefused := strings.Contains(openCodeStderrErrorRegion(strings.ToLower(string(errb))), "must provide a message") ||
		strings.Contains(openCodeStderrErrorRegion(strings.ToLower(openCodeStdoutPlainLines(out))), "must provide a message")
	stdinRow := openCodeLiveRowFor("stdin-control", out, errb, runErr, "", false)
	t.Logf("gate %s stdinRefused=%t", stdinRow, stdinRefused)

	// The same cmd.exe route against the stub: does stdin reach the child?
	stubPasses := openCodeLiveStubStdinPassesThrough(t, bin)
	t.Logf("gate stub-stdin-through-launcher=%t", stubPasses)

	// Does the pinned build accept `run --file`?
	marker, err := newOpenCodeSmokeMarker()
	if err != nil {
		t.Fatal(err)
	}
	attach := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(attach, []byte(openCodeSmokePrompt(marker)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errb, runErr, timedOut := runOpenCodeLiveOneShot(t, bin,
		[]string{"--file", attach, "Follow the instructions in the attached file."}, "", env)
	_, fileDiag, fileMatched := classifyOpenCodeSmokeRun(timedOut, out, errb, runErr, marker)
	fileFlagRow := openCodeLiveRowFor("file-flag", out, errb, runErr, fileDiag, fileMatched)
	t.Logf("gate %s", fileFlagRow)

	t.Logf("gate branch=%s", openCodeLiveGateBranch(defaultRow, pipeRow, fileRow, stdinRefused, fileFlagRow))
}

// openCodeLiveStubStdinPassesThrough runs the compiled stub with
// OPENCODE_STUB_ECHO_STDIN through the same launch route as bin (a `.cmd` shim
// when bin is one) and reports whether the stdin prompt's last token came back.
func openCodeLiveStubStdinPassesThrough(t *testing.T, bin string) bool {
	t.Helper()
	stub := buildOpenCodeStub(t)
	launch := stub
	if isWindowsShimPath(bin) {
		launch = filepath.Join(t.TempDir(), "opencode.cmd")
		if err := os.WriteFile(launch, []byte("@echo off\r\n\""+stub+"\" %*\r\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := setEnvVar(sanitizeOpenCodeEnv(os.Environ()), "OPENCODE_STUB_ECHO_STDIN", "1")
	out, _, _, _ := runOpenCodeLiveOneShot(t, launch, nil, openCodeLiveStdinPrompt, env)
	fields := strings.Fields(openCodeLiveStdinPrompt)
	return strings.TrimSpace(parseOpenCodeSmokeStream(out).Text) == fields[len(fields)-1]
}

// openCodeLiveGateBranch applies the design's closed outcome table.
func openCodeLiveGateBranch(defaultRow, pipeRow, fileRow openCodeLiveRow, stdinRefused bool, fileFlag openCodeLiveRow) string {
	lost := !defaultRow.MarkerMatched &&
		(defaultRow.Diagnostic == cliSmokeDiagnosticNoOutput || defaultRow.Diagnostic == cliSmokeDiagnosticNoEnvelope)
	switch {
	case lost && pipeRow.MarkerMatched:
		return "A1 (large-buffer pipe)"
	case lost && fileRow.MarkerMatched:
		return "A2 (nameless capture file)"
	case stdinRefused || (defaultRow.Diagnostic == cliSmokeDiagnosticNoOutput && defaultRow.Counts.Frames == 0):
		if fileFlag.Diagnostic == cliSmokeDiagnosticFlagRejected || fileFlag.Diagnostic == cliSmokeDiagnosticFramingRejected {
			return "B2 (loopback serve)"
		}
		return "B (prompt file attachment)"
	case defaultRow.Counts.Frames > 0 && !defaultRow.Counts.Terminal:
		if defaultRow.Counts.EscLines > 0 {
			return "C1 (smoke-only normalization)"
		}
		return "C2 (extend terminal predicate)"
	case defaultRow.MarkerMatched && pipeRow.MarkerMatched && fileRow.MarkerMatched:
		return "D (blocked: no mode reproduces; re-run on the nightly device)"
	default:
		return "unclassified (attach this table to the investigation)"
	}
}

// The gate's own helpers run in CI (no live binary needed), so the outcome
// table and the redaction of unknown types cannot silently rot between the
// rare live runs.
func TestOpenCodeLiveGateBranch_FollowsTheOutcomeTable(t *testing.T) {
	matched := openCodeLiveRow{MarkerMatched: true}
	silent := openCodeLiveRow{Diagnostic: cliSmokeDiagnosticNoOutput}
	framesNoEnd := openCodeLiveRow{Diagnostic: cliSmokeDiagnosticNoEnvelope, Counts: openCodeSmokeCounts{Frames: 4}}
	escFrames := openCodeLiveRow{Diagnostic: cliSmokeDiagnosticNoEnvelope, Counts: openCodeSmokeCounts{Frames: 4, EscLines: 1}}
	rejected := openCodeLiveRow{Diagnostic: cliSmokeDiagnosticFlagRejected}
	for _, tc := range []struct {
		name            string
		def, pipe, file openCodeLiveRow
		stdinRefused    bool
		fileFlag        openCodeLiveRow
		wantPrefix      string
	}{
		{"pipe fixes lost output", silent, matched, matched, false, matched, "A1"},
		{"only the file fixes it", framesNoEnd, framesNoEnd, matched, false, matched, "A2"},
		{"stdin refused, --file accepted", silent, silent, silent, true, matched, "B "},
		{"stdin refused, --file rejected", silent, silent, silent, true, rejected, "B2"},
		{"frames with escapes, no terminal", escFrames, escFrames, escFrames, false, matched, "C1"},
		{"frames without escapes, no terminal", framesNoEnd, framesNoEnd, framesNoEnd, false, matched, "C2"},
		{"every mode matches", matched, matched, matched, false, matched, "D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := openCodeLiveGateBranch(tc.def, tc.pipe, tc.file, tc.stdinRefused, tc.fileFlag)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("branch = %q, want prefix %q", got, tc.wantPrefix)
			}
		})
	}
}

func TestOpenCodeLiveEventTypes_NeverSpellsAnUnknownType(t *testing.T) {
	const secretType = "vendor.secret-event-type"
	stdout := `{"type":"text","text":"hi"}` + "\n" + `{"type":"` + secretType + `"}` + "\n" +
		`{"type":"` + secretType + `"}` + "\n" + `{"type":"step_finish"}` + "\n"
	row := openCodeLiveRowFor("unit", []byte(stdout), nil, nil, cliSmokeDiagnosticNone, true)
	if row.TypeCounts["text"] != 1 || row.TypeCounts["step_finish"] != 1 || row.UnknownTypes != 2 {
		t.Fatalf("counts = %+v unknown=%d", row.TypeCounts, row.UnknownTypes)
	}
	if len(row.UnknownDigests) != 1 || len(row.UnknownDigests[0]) != 8 {
		t.Fatalf("digests = %q, want one 8-hex digest", row.UnknownDigests)
	}
	if strings.Contains(row.String(), secretType) || strings.Contains(row.String(), "secret") {
		t.Fatalf("the row spells an unknown type: %s", row)
	}
	if len(openCodeLiveStdinPrompt) != 64 {
		t.Fatalf("the stdin control prompt is %d bytes, want 64", len(openCodeLiveStdinPrompt))
	}
}

/* --------------------------------------------------------------------------
   Usage reconciliation against the real install
   -------------------------------------------------------------------------- */

// TestOpenCodeLiveGate_UsageReconcileAnswersFromTheRealInstall proves the two
// commands the usage ledger depends on — `session list --format json` and
// `export <id>` — exist and print the shapes the reconcile decodes.
//
// CI has no real OpenCode, so no unit test can: the store tests stub the
// command seam and assert the BOUNDS, while this opt-in gate is the only place
// the SHAPES are checked against a shipped build. An `unsupported` outcome here
// means the installed version does not answer those commands and the feature
// degrades to stream-only figures.
//
// It spends no inference turn of its own: both commands only read the session
// store. Run it after the live smoke above, so there is a session to export.
func TestOpenCodeLiveGate_UsageReconcileAnswersFromTheRealInstall(t *testing.T) {
	bin := openCodeLiveGateBinary(t)

	dir := t.TempDir()
	t.Setenv(openCodeUsageLedgerEnv, filepath.Join(dir, "opencode_usage.json"))
	t.Setenv(openCodeUsageFreshnessEnv, filepath.Join(dir, "opencode_usage_freshness.json"))
	prevBinary := openCodeUsageBinary
	openCodeUsageBinary = func() string { return bin }
	t.Cleanup(func() { openCodeUsageBinary = prevBinary })

	now := time.Now()
	result := reconcileOpenCodeUsageOnce(context.Background(), now)
	// Closed values only — no session id, no export body, no CLI text.
	t.Logf("[opencode-live] reconcile outcome=%s exported=%d remaining=%d",
		result.Outcome, result.Exported, result.Remaining)
	if result.Outcome == openCodeReconcileUnsupported {
		t.Fatalf("the installed build does not answer `session list --format json` / `export` " +
			"in a shape the reconcile decodes — OpenCode usage would be stream-only here")
	}
	if !openCodeReconcileSucceeded(result.Outcome) && result.Outcome != openCodeReconcileMore {
		t.Fatalf("outcome = %s, want ok / no_change / more", result.Outcome)
	}
	metrics, generation, partial := openCodeLedgerMetrics(now)
	t.Logf("[opencode-live] rows=%d generation=%v partial=%t", len(metrics), generation != nil, partial)
	if len(metrics) == 0 {
		t.Fatal("the reconcile committed no numeric row — run the live smoke above first " +
			"so there is a session to export")
	}
	if metrics[0].Consumed == nil || *metrics[0].Consumed <= 0 {
		t.Fatalf("tokens today = %v, want a nonzero reading", metrics[0].Consumed)
	}
}
