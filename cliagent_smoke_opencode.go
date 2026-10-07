// cliagent_smoke_opencode.go — the OpenCode provider of the signed
// `__cli_smoke__` operational command: a single non-interactive
// `opencode run --format json` turn whose marker echo proves the CLI can still
// complete a round trip. The provider-agnostic core (closed diagnostic set,
// metric-only result, cooldown, singleflight, shape cache) lives in
// cliagent_smoke.go; the argv contract and the one launcher live in
// opencode_argv.go.
//
// Before this provider existed OpenCode had NO row in cliSmokeProviders, so a
// maintenance smoke resolved to provider_unavailable / unknown_cli without
// spawning anything, and the only OpenCode smoke that could run was the legacy
// signed `session_start` one — which ran whatever argv the publisher signed
// (`run --pure --format json <prompt>`, rejected during option parsing) through
// a bare exec.Command that cannot even start a Windows `.cmd` npm shim. The
// outcome collapsed into one `launch_or_protocol` bucket, so maintenance could
// not tell a failed launch from a rejected flag from a reply we no longer parse.
//
// This probe:
//   - runs every pre-check the direct path runs (binary resolvable, `--version`
//     answerable, a conclusive "no usable provider" readiness verdict) and maps
//     each refusal onto a closed diagnostic, spending no turn;
//   - spawns exactly the no-resume ladder rung through the shared, shim-aware
//     newOpenCodeCmd, with the marker prompt in a 0600 file handed to the child
//     as stdin — never on argv;
//   - classifies the outcome into ONE closed diagnostic, with launch_error kept
//     distinct from no_envelope so "we could not start the child" never shares
//     a bucket with "the child ran and produced no envelope", and no_output
//     ("the child ran and not one frame arrived") split from both;
//   - never retries: nothing in its argv is droppable.
//
// Retention discipline applies exactly as in cliagent_smoke.go: the child's
// stdout and stderr are read to derive a verdict and then DISCARDED. The marker
// nonce, the prompt, the resolved argv, the resolved path and any config content
// are never published or logged at any severity.

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// openCodeSmokeMarkerPrefix + 8 hex chars is the nonce the model is asked to
// echo. Generated per run: a FIXED marker could be satisfied by a cached
// transcript and would pass on a dead binary. The prefix is distinct from every
// other provider's so a test fixture can never satisfy the wrong provider by
// accident.
const openCodeSmokeMarkerPrefix = "AIEXPEDITE_OPENCODE_SMOKE_OK_"

// openCodeSmokeTimeout bounds the one inference turn. A var, not a const, so a
// test can shrink it and exercise the real deadline-kill path.
var openCodeSmokeTimeout = 60 * time.Second

// openCodeSmokeWaitDelay bounds how long Wait may stay blocked on the captured
// stdout/stderr handles once the deadline has fired and the tree has been
// killed. Short, because by then the whole tree is already force-killed; this
// only covers the reap race.
const openCodeSmokeWaitDelay = 2 * time.Second

/* --------------------------------------------------------------------------
   Exec seam
   -------------------------------------------------------------------------- */

// runOpenCodeSmokeCommand spawns the probe. A package-level var so tests drive
// the classification without ever spawning a real binary or spending a turn —
// the same seam shape as runGrokSmokeCommand / runCodexSmokeCommand.
//
// The prompt is NOT an argument: it is already on disk at launch.PromptFile
// (0600) and reaches the child only as its stdin. An expired attempt tears the
// whole process TREE down, because an OpenCode tool child otherwise holds the
// captured pipes past the deadline and Wait would never return.
// `started` reports whether the child actually reached Start. A pre-spawn
// failure spent no tokens, so the caller withdraws the usage capture instead of
// settling it as an uncovered run that would owe a reconcile.
var runOpenCodeSmokeCommand = func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, started bool, err error) {
	cmd, closePrompt, err := newOpenCodeSmokeCmd(ctx, launch)
	if err != nil {
		return nil, nil, false, err
	}
	defer closePrompt()
	outBuf := &boundedBuffer{limit: cliSmokeMaxStdout}
	errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if err = cmd.Start(); err != nil {
		return nil, nil, false, err
	}
	err = cmd.Wait()
	return outBuf.Bytes(), errBuf.Bytes(), true, err
}

// newOpenCodeSmokeCmd builds the probe child with everything but its output
// sinks: the staged prompt as stdin, the wait delay, its own process group and
// the tree-kill Cancel. The caller sets Stdout / Stderr, starts it, and calls
// closePrompt once the child has been reaped. Split from the seam so the opt-in
// live Windows gate can drive the SAME child through other capture sinks.
func newOpenCodeSmokeCmd(ctx context.Context, launch openCodeLaunch) (cmd *exec.Cmd, closePrompt func(), err error) {
	prompt, err := os.Open(launch.PromptFile)
	if err != nil {
		return nil, nil, err
	}
	launch.Stdin = prompt

	cmd, err = newOpenCodeCmd(ctx, launch)
	if err != nil {
		_ = prompt.Close()
		return nil, nil, err
	}
	// When Stdout/Stderr are plain writers (the seam's boundedBuffers), os/exec
	// copies the child's pipes on its own goroutines and Wait waits for them. A
	// tool child that outlives the kill still holds the write end, and without a
	// delay Wait would block on it forever — the per-attempt deadline would bound
	// nothing.
	cmd.WaitDelay = openCodeSmokeWaitDelay
	// Setsid on unix (hides the console window on Windows) so the child leads its
	// own process group, exactly as runOneShot does. Without it the child shares
	// the terminal agent's group, killOpenCodeProcessTree's `-pid` group kill
	// targets a group that does not exist, and a tool descendant survives the
	// deadline on macOS/Linux.
	detachControllingTTY(cmd)
	// The deadline must reach the process the probe actually cares about.
	// exec.CommandContext's own Cancel kills the single child, and the shim
	// route's kills cmd.exe plus the tree — but an `opencode` TOOL child can
	// outlive either and keep the captured pipes open past the deadline.
	// killOpenCodeProcessTree is a superset of both (tree, then the process, then
	// the unix process group), so overwriting Cancel with it is correct on every
	// route and replaces a per-attempt watchdog goroutine that double-killed the
	// shim route. Set before Start, as os/exec requires.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		killOpenCodeProcessTree(cmd)
		return nil
	}
	return cmd, func() { _ = prompt.Close() }, nil
}

/* --------------------------------------------------------------------------
   Resolution + pre-checks (no turn spent)
   -------------------------------------------------------------------------- */

// resolveOpenCodeSmokePath resolves the binary the smoke will probe, re-resolved
// on every smoke: the point of the post-update smoke is to validate the binary
// that was just replaced. Same lookup order as gatherCLIAgents — PATH first,
// then the official installer's bin dir, which GUI/launchd-spawned agents do not
// have on PATH.
//
// Returns "" when nothing resolves. Deliberately NOT resolveOpenCodeExecutable,
// whose final fallback is the bare name `opencode`: a bare name defeats the
// binary_missing pre-check (it is not a path) and would stamp an unstattable
// path into the cooldown key, so every smoke would look like a different binary.
//
// A var so tests can point it at a stub without touching the real install.
var resolveOpenCodeSmokePath = func() string {
	if path, err := exec.LookPath("opencode"); err == nil {
		return path
	}
	return resolveOpenCodeInstallerBinary()
}

// openCodeProbeVersion answers `--version` for an OpenCode binary, cached per
// (path, mtime, size) exactly as the CLI detection does — so an upgrade
// re-probes and a steady-state smoke does not spawn a version child. Returns the
// raw first line ("" when the binary cannot answer); callers parse it.
//
// This is the ONLY OpenCode version probe: gatherCLIAgents and the native
// manager's capability check route here too. They must, because the cache key is
// (path, mtime, size) alone — a probe that launched `opencode.cmd` with a plain
// exec.Command would cache its own "" under that key and every later shim-aware
// probe would read the negative back and report binary_missing without ever
// spawning cmd.exe. One route, one answer.
func openCodeProbeVersion(path string) string {
	// A cached FAILURE must not outlive the fault that produced it. The shared
	// cache keys on (path, mtime, size) alone, so a launcher that was momentarily
	// unable to start the child — a spawn that lost a race with an installer, a
	// probe deadline missed under load — would otherwise pin "" for the whole
	// life of an unchanged binary, and probeOpenCodeNativeCapability's 30-second
	// negative window could never recover: every later native start would read
	// the same cached empty string. So an empty reading is kept only for
	// openCodeVersionNegativeTTL and then dropped, which still spares a reliably
	// dead binary the doomed child on every single gather.
	openCodeExpireNegativeVersion(path)
	version := openCodeCachedProbeVersion(path)
	openCodeNoteVersionReading(path, version)
	return version
}

// openCodeCachedProbeVersion is the cached probe itself: shim-aware on Windows,
// the shared probe elsewhere. A shim's cached reading is dropped first when the
// package behind it changed (openCodeBinaryIdentity): npm replaces the package
// and leaves the shim — and so the cache key — untouched.
func openCodeCachedProbeVersion(path string) string {
	if isWindowsShimPath(path) {
		openCodeForgetVersionOnIdentityChange(path)
		return cachedProbeVersionFunc(path, func() string {
			return openCodeShimProbeVersion(path)
		})
	}
	return cachedProbeVersion(path)
}

// openCodeVersionNegativeTTL is how long a failed `--version` reading is reused
// before the binary is asked again. Matches probeOpenCodeNativeCapability's own
// negative window, so the two recover together rather than one pinning the other.
const openCodeVersionNegativeTTL = 30 * time.Second

var (
	openCodeVersionNegativeMu sync.Mutex
	// Keyed by path only: the point is to forget, and forgetCachedProbeVersion
	// drops every (mtime, size) entry the path has. A binary REPLACED inside the
	// window re-keys the shared cache on its own, so a stale timestamp here can
	// at worst schedule one extra probe of a binary that already answered.
	openCodeVersionNegativeAt = map[string]time.Time{}
)

// resetOpenCodeVersionNegatives forgets every recorded failure window. Test-only
// seam, beside resetVersionProbeCache, so a case that drives the negative TTL
// starts from a known state.
func resetOpenCodeVersionNegatives() {
	openCodeVersionNegativeMu.Lock()
	openCodeVersionNegativeAt = map[string]time.Time{}
	openCodeVersionNegativeMu.Unlock()
}

// backdateOpenCodeVersionNegative moves a recorded failure window into the past.
// Test-only seam: a case must be able to reach the far side of
// openCodeVersionNegativeTTL without sleeping for it.
func backdateOpenCodeVersionNegative(path string, by time.Duration) {
	openCodeVersionNegativeMu.Lock()
	if at, ok := openCodeVersionNegativeAt[path]; ok {
		openCodeVersionNegativeAt[path] = at.Add(-by)
	}
	openCodeVersionNegativeMu.Unlock()
}

// openCodeExpireNegativeVersion drops a cached empty reading for path once it is
// older than openCodeVersionNegativeTTL, so the next probe actually spawns.
func openCodeExpireNegativeVersion(path string) {
	if path == "" {
		return
	}
	openCodeVersionNegativeMu.Lock()
	at, failed := openCodeVersionNegativeAt[path]
	expired := failed && time.Since(at) >= openCodeVersionNegativeTTL
	if expired {
		delete(openCodeVersionNegativeAt, path)
	}
	openCodeVersionNegativeMu.Unlock()
	if expired {
		forgetCachedProbeVersion(path)
	}
}

// openCodeNoteVersionReading records when a reading FAILED, and forgets the
// failure as soon as the binary answers.
func openCodeNoteVersionReading(path, version string) {
	if path == "" {
		return
	}
	openCodeVersionNegativeMu.Lock()
	defer openCodeVersionNegativeMu.Unlock()
	if version == "" {
		if _, already := openCodeVersionNegativeAt[path]; !already {
			// Measure the window from the FIRST failure, so repeated reads of the
			// same cached negative cannot postpone the re-probe indefinitely.
			openCodeVersionNegativeAt[path] = time.Now()
		}
		return
	}
	delete(openCodeVersionNegativeAt, path)
}

// openCodeShimProbeVersion runs `<shim> --version` through the shared
// shim-aware launcher under the same short probe deadline the machine-info
// probes use. Returns "" on any failure, exactly like probeVersionArgsWithEnv,
// so the caller's binary_missing pre-check is unchanged for a genuinely dead
// shim.
func openCodeShimProbeVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	cmd, err := newOpenCodeCmd(ctx, openCodeLaunch{
		Path: path,
		Args: []string{"--version"},
		Env:  sanitizeOpenCodeEnv(os.Environ()),
	})
	if err != nil {
		return ""
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return ""
	}
	return firstNonEmptyLine(out)
}

/* --------------------------------------------------------------------------
   Probe
   -------------------------------------------------------------------------- */

// runOpenCodeSmoke performs the probe against an already-resolved binary.
// Registered in cliSmokeProviders (cliagent_smoke.go); split from runCLISmoke so
// the cooldown/catalog concerns stay out of the classification logic.
//
// Order matters for the turn budget: every step up to the spawn is local and
// free, and each failure there maps onto a pre-check diagnostic the cooldown
// deliberately does NOT pin (cliSmokeVerdictSpentTurn).
//
// Cost note: OpenCode has no quota of its own — the turn is spent against
// whichever provider sits behind it. The shared 15-minute cooldown and the
// singleflight bound that to one turn per binary per window, and the free
// `opencode models` readiness pre-check short-circuits an unusable install
// before anything is spent.
func runOpenCodeSmoke(ctx context.Context, path, version string) cliSmokeResult {
	result := cliSmokeResult{CliID: "opencode", Version: version, Status: cliSmokeStatusFailed}
	started := time.Now()
	finish := func(category, diagnostic string) cliSmokeResult {
		result.ErrorCategory = category
		result.Diagnostic = diagnostic
		result.DurationMs = time.Since(started).Milliseconds()
		return result
	}

	// Pre-check 1: the binary must exist and answer `--version`. Both failures
	// mean there is nothing to smoke — and neither costs a turn.
	if path == "" || version == "" {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticBinaryMissing)
	}

	// The per-run empty cwd is created BEFORE the readiness pre-check, because
	// the pre-check has to run in it: OpenCode discovers a project
	// `opencode.json` (and its agents/MCP/provider entries) by walking upward
	// from cwd, so a pre-check answered from the agent's own directory can
	// describe a different configuration than the turn below — a provider
	// configured only in that project would pass the pre-check and then fail the
	// turn, and a project override would report a premature not_logged_in. Both
	// steps are turn-free, so the reordering cannot cost anything. A directory
	// this process cannot create is a LAUNCH failure, not a missing envelope.
	runDir, err := newOpenCodeSmokeRunDir()
	if err != nil {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticLaunchError)
	}
	defer func() { _ = os.RemoveAll(runDir) }()

	// Pre-check 2: a CONCLUSIVE "no usable provider" means the turn cannot
	// complete. Anything inconclusive proceeds — that is what a working
	// local-model or env-credential install looks like (see
	// cliagent_usage_opencode.go's fail-open design).
	if loggedIn, known := openCodeSmokeLoggedInDir(ctx, path, runDir); known && !loggedIn {
		return finish(cliUsageErrorNotAuthenticated, cliSmokeDiagnosticNotLoggedIn)
	}

	marker, err := newOpenCodeSmokeMarker()
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}

	promptPath, promptFile, err := writeOpenCodePromptFile(openCodeSmokePrompt(marker))
	if err != nil {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticLaunchError)
	}
	// The handle is not the one the child reads (the exec seam opens the path
	// itself, so a test double can recover what the child was asked); close it
	// immediately and remove the file once the child has been reaped.
	_ = promptFile.Close()
	defer func() { _ = os.Remove(promptPath) }()

	// The turn below spends tokens against whatever provider sits behind
	// OpenCode, and its `step_finish` events are the only record of how many —
	// so arm the usage capture before the spawn and settle it below. Every
	// pre-check refusal above returns before this point and arms nothing.
	// See cliagent_usage_opencode_capture.go.
	usage := armOpenCodeUsageRun("smoke")

	// Bound BEFORE the spawn: the shape this run resolves is a fact about THESE
	// bytes, not about whatever is at `path` when it finishes.
	shapeBinding := bindCLISmokeShapeWithIdentity(path, openCodeBinaryIdentity)
	shape := openCodeRunShapeNoSession
	result.ArgvShapeID = shape.ID

	runCtx, cancel := context.WithTimeout(ctx, openCodeSmokeTimeout)
	stdout, stderr, spawned, runErr := runOpenCodeSmokeCommand(runCtx, openCodeLaunch{
		Path:       path,
		Args:       buildOpenCodeRunArgs(shape, ""),
		Env:        sanitizeOpenCodeEnv(os.Environ()),
		Dir:        runDir,
		PromptFile: promptPath,
		// Pins self-update off (and the terminal title with it) for this child
		// only: a pre-update smoke must not BE the update, and a title escape on
		// stdout is noise in the frame stream. See openCodeMaintenanceEnvPins.
		Maintenance: true,
	})
	// Read the PER-ATTEMPT context before cancelling it: a deadline kill reports
	// an *exec.ExitError, not a wrapped context error, so judging by the parent
	// ctx alone would read a timeout as a launch or protocol failure.
	timedOut := runCtx.Err() != nil || ctx.Err() != nil
	cancel()

	// Fold the child's figures out of the capture before the bytes go: only
	// integers, two hashed ids and the event time survive. A clean verdict is
	// the stream's own statement that the turn ran to its end, which is what
	// decides whether this run is covered or owes a reconcile.
	category, diagnostic, matched := classifyOpenCodeSmokeRun(timedOut, stdout, stderr, runErr, marker)
	if !spawned {
		// The child never ran: no inference happened, so there is nothing for a
		// reconcile to find. Settling it as an uncovered run would open a debt
		// that retries OpenCode commands against the launch failure and could
		// end by marking today's totals a lower bound.
		usage.Disarm()
	} else {
		observeOpenCodeUsageFromStdout(usage, stdout)
		settleOpenCodeUsageRunAsync(usage, category == "" && !timedOut)
	}
	if category == "" {
		result.Status = cliSmokeStatusSuccess
		result.MarkerMatched = matched
		result.Diagnostic = diagnostic
		result.DurationMs = time.Since(started).Milliseconds()
		shapeBinding.remember(shape.ID)
		return result
	}
	// Device-local log line: closed values plus COUNTS — metrics, not content.
	// The child's bytes live no longer than the classifier that read them.
	fmt.Print(openCodeSmokeFailureLogLine(shape.ID, category, diagnostic,
		openCodeSmokeCountsFor(stdout, stderr, runErr)))
	return finish(category, diagnostic)
}

// openCodeSmokeCounts is everything the failure log line says about the child's
// output: byte and line counts, whether a terminal frame arrived, and the exit
// code. Numbers and a bool only — a struct that cannot hold text cannot leak it.
//
// Frames and EscLines are what tell the no_output / no_envelope failures apart
// in the field: zero frames with zero bytes is a transport or prompt-delivery
// fault, frames with EscLines > 0 is terminal-control noise in the stream, and
// frames with neither but no terminal frame is an event-contract change.
type openCodeSmokeCounts struct {
	StderrBytes int
	StdoutBytes int
	Frames      int
	EscLines    int
	Terminal    bool
	Exit        int
}

// openCodeSmokeCountsFor derives the log counts from one run's capture.
func openCodeSmokeCountsFor(stdout, stderr []byte, runErr error) openCodeSmokeCounts {
	stream := parseOpenCodeSmokeStream(stdout)
	return openCodeSmokeCounts{
		StderrBytes: len(stderr),
		StdoutBytes: len(stdout),
		Frames:      stream.Frames,
		EscLines:    stream.EscLines,
		Terminal:    stream.Ended,
		Exit:        openCodeSmokeExitCode(runErr),
	}
}

// openCodeSmokeExitCode is the child's exit code: 0 for a clean exit, the code
// of an *exec.ExitError, and -1 when the error carries none (a launch failure,
// or a WaitDelay expiry whose exit status was not observed).
func openCodeSmokeExitCode(runErr error) int {
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// openCodeSmokeFailureLogLine renders the device-local diagnostic line. It takes
// COUNTS rather than the bytes, deliberately: a function that cannot receive
// vendor text cannot leak it, no matter how a future caller wires it up. Every
// other argument is a value this package defines.
func openCodeSmokeFailureLogLine(shapeID, category, diagnostic string, counts openCodeSmokeCounts) string {
	return fmt.Sprintf("%s[cli-smoke] opencode shape=%s category=%s diagnostic=%s stderrBytes=%d stdoutBytes=%d frames=%d escLines=%d terminal=%t exit=%d%s\n",
		colorYellow, shapeID, category, diagnostic, counts.StderrBytes, counts.StdoutBytes,
		counts.Frames, counts.EscLines, counts.Terminal, counts.Exit, colorReset)
}

// openCodeSmokePrompt is the one-turn instruction. It names no path, no account
// and no configuration — the marker is the only variable part, and it never
// leaves this process (the published result carries markerMatched, not the
// marker). It uses the same prefix the signed session_start contract requires,
// so both transports ask OpenCode the same thing.
func openCodeSmokePrompt(marker string) string {
	return openCodeMaintenanceSmokePromptPrefix + marker
}

func newOpenCodeSmokeMarker() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return openCodeSmokeMarkerPrefix + hex.EncodeToString(buf), nil
}

/* --------------------------------------------------------------------------
   Per-run scratch directory
   -------------------------------------------------------------------------- */

const (
	// openCodeSmokeScratchDirName is the scratch root the per-run empty cwd is
	// created under, beside the prompt scratch dir the direct path uses.
	openCodeSmokeScratchDirName = "opencode-smoke"
	// openCodeSmokeScratchMaxAge is how long an ORPHANED per-run cwd may sit
	// before the next smoke reclaims it. Comfortably longer than the per-attempt
	// deadline, so a live run's directory is never swept out from under it — even
	// one whose clock differs from ours after a suspend.
	openCodeSmokeScratchMaxAge = 6 * time.Hour
	// openCodeSmokeScratchMaxSweep bounds one sweep. A directory that somehow
	// accumulated thousands of entries must not turn a health check into a long
	// blocking scan; the remainder is reclaimed by later smokes.
	openCodeSmokeScratchMaxSweep = 64
)

// newOpenCodeSmokeRunDir creates the fresh empty working directory one smoke
// (or one cooldown replay's login re-check) runs in, under the shared scratch
// root, and reclaims orphans left by a process killed mid-smoke on the way.
// Callers own the returned directory and must remove it.
func newOpenCodeSmokeRunDir() (string, error) {
	scratch := cliPromptTempDir(openCodeSmokeScratchDirName)
	// The caller's defer removes its own directory, but only if the process lives
	// to run it: an agent killed mid-smoke leaves the entry behind forever, and
	// nothing else prunes this tree. Reclaim in the BACKGROUND so a slow
	// filesystem cannot add latency to a deadline-bounded health check.
	pruneOpenCodeSmokeScratchOnce(scratch)
	return os.MkdirTemp(scratch, "cwd-*")
}

var openCodeSmokeScratchPruneOnce sync.Once

// pruneOpenCodeSmokeScratchOnce reclaims orphaned per-run directories at most
// once per agent process, in the background. Called from the smoke path rather
// than agent startup so a device that never smokes OpenCode never touches the
// directory, and backgrounded so a slow or AV-scanned filesystem cannot spend
// the probe's deadline on housekeeping. Same shape as
// pruneGrokSessionStoreOnce.
//
// `scratch` is resolved by the CALLER, on its goroutine: cliPromptTempDir reads
// the process environment, which tests swap and restore via t.Cleanup, and a
// background read of it would race that restore under -race.
func pruneOpenCodeSmokeScratchOnce(scratch string) {
	if scratch == "" {
		return
	}
	openCodeSmokeScratchPruneOnce.Do(func() {
		go pruneOpenCodeSmokeScratch(scratch, time.Now())
	})
}

// pruneOpenCodeSmokeScratch removes orphaned per-run cwd directories.
//
// Best-effort and silent by design: this is housekeeping on the way to a health
// check, so every error is ignored rather than turned into a smoke failure. It
// only ever removes entries matching the name this file creates and older than
// openCodeSmokeScratchMaxAge, so it cannot touch a live run or anything a
// neighbouring feature put there.
func pruneOpenCodeSmokeScratch(scratch string, now time.Time) {
	if scratch == "" {
		return
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		return
	}
	swept := 0
	for _, entry := range entries {
		if swept >= openCodeSmokeScratchMaxSweep {
			break
		}
		// IsDir() comes from the directory entry itself (an lstat), so a SYMLINK
		// or Windows junction named like a run directory reports false here and is
		// skipped rather than descended into. That is deliberate and load-bearing:
		// this repo has lost entire sibling checkouts to a recursive delete that
		// followed a junction, so a reparse point in this tree is left strictly
		// alone rather than reclaimed.
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			!strings.HasPrefix(entry.Name(), "cwd-") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || now.Sub(info.ModTime()) < openCodeSmokeScratchMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(scratch, entry.Name()))
		swept++
	}
}

/* --------------------------------------------------------------------------
   Classification
   -------------------------------------------------------------------------- */

// openCodeSmokeStream is what the classifier needs from the child's `--format
// json` stdout: the concatenated assistant text, whether a terminal frame
// arrived, the first error event's message (read to pick a constant, never
// retained past the classifier), and whether the stream was TRUNCATED or
// malformed — in which case the accumulated text is partial and must never be
// compared against the marker.
//
// Frames counts the lines that decoded as JSON events; EscLines counts lines
// whose first byte is ESC or a UTF-8 BOM (terminal-control noise that hides a
// frame from the `{` check). Both are counts only — they feed the no_output
// arm and the failure log line, never a published value.
type openCodeSmokeStream struct {
	Text         string
	Ended        bool
	ErrorMessage string
	SawError     bool
	Malformed    bool
	Overflow     bool
	Frames       int
	EscLines     int
}

// openCodeLineStartsWithEscape reports a line whose first byte is ESC (a CSI or
// OSC sequence such as a terminal-title write) or the start of a UTF-8 BOM.
func openCodeLineStartsWithEscape(line string) bool {
	return strings.HasPrefix(line, "\x1b") || strings.HasPrefix(line, "\xef\xbb\xbf")
}

// parseOpenCodeSmokeStream folds the JSON events OpenCode emits under
// `--format json`, reusing parseOpenCodeEventLine — the SAME reader the resident
// chat path uses, so the two transports cannot disagree about what the assistant
// said. Text deltas are concatenated with NO separator: a newline inserted at a
// frame boundary would corrupt the exact marker.
//
// A line that is not a JSON object (a banner, an updater notice) is skipped
// rather than treated as a protocol failure; a line that LOOKS like JSON and
// does not decode is Malformed, because that is a broken contract rather than
// noise.
func parseOpenCodeSmokeStream(stdout []byte) openCodeSmokeStream {
	var stream openCodeSmokeStream
	var text strings.Builder
	// The RETENTION cap is what tells us bytes were dropped, and the only honest
	// signal is that the capture buffer FILLED: boundedBuffer stops writing
	// exactly AT its limit, so `len(stdout) >= cliSmokeMaxStdout` means the child
	// had more to say than we kept. Summing the scanned LINE lengths instead
	// could never fire — newlines are dropped, so that sum is strictly below the
	// byte total for any multi-line reply — which left this detection dead on
	// real captures while a test feeding the classifier directly still passed.
	if len(stdout) >= cliSmokeMaxStdout {
		stream.Overflow = true
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeNativeMaxFrameBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if openCodeLineStartsWithEscape(line) {
			stream.EscLines++
		}
		if line == "" || !strings.HasPrefix(line, "{") {
			// A line that is not a JSON object (a banner, an updater notice) is
			// skipped rather than treated as a protocol failure.
			continue
		}
		frame, ok := parseOpenCodeSmokeFrame(line)
		if !ok {
			// This decode is DELIBERATELY permissive (see openCodeSmokeFrame), so
			// failing it means the line is genuinely broken JSON — a broken
			// contract rather than noise.
			stream.Malformed = true
			continue
		}
		stream.Frames++
		if msg := frame.errorMessage(); msg != "" {
			if !stream.SawError {
				stream.SawError = true
				stream.ErrorMessage = msg
			}
			continue
		}
		if isOpenCodeTerminalEventType(frame.Type, frame.finishReason()) {
			stream.Ended = true
		}
		// A line the SHARED reader cannot decode simply carries no assistant
		// text: openCodeEvent types `message` as an object, while OpenCode spells
		// it as a plain string in some releases. Treating that as malformed would
		// report a healthy turn — one whose completion event happened to carry a
		// string `message` — as no_envelope.
		if delta, _, parsed := parseOpenCodeEventLine(line); parsed {
			text.WriteString(delta)
		}
	}
	if scanner.Err() != nil {
		// bufio.ErrTooLong (a single event past openCodeNativeMaxFrameBytes) or
		// any read error: the reply cannot be trusted as complete.
		stream.Overflow = true
	}
	stream.Text = text.String()
	return stream
}

// openCodeSmokeFrame is the subset of an event the classifier reads BEYOND what
// parseOpenCodeEventLine extracts: the event TYPE (to spot the terminal frame)
// and an error event's message (read only to pick between auth_error and
// provider_error, then dropped).
//
// `error` and `message` are RawMessage because OpenCode spells both as a string
// in some releases and an object in others; a typed field would make a
// perfectly good event look malformed.
type openCodeSmokeFrame struct {
	Type    string          `json:"type"`
	Error   json.RawMessage `json:"error"`
	Message json.RawMessage `json:"message"`
	Part    json.RawMessage `json:"part"`
	Reason  json.RawMessage `json:"reason"`
}

// finishReason is the step's finish reason (`part.reason`, else a top-level
// `reason`), read through the same map helper the session stream uses so both
// transports judge a tool-call step_finish identically.
func (f openCodeSmokeFrame) finishReason() string {
	event := map[string]interface{}{}
	var part map[string]interface{}
	if len(f.Part) > 0 && json.Unmarshal(f.Part, &part) == nil {
		event["part"] = part
	}
	var reason string
	if len(f.Reason) > 0 && json.Unmarshal(f.Reason, &reason) == nil {
		event["reason"] = reason
	}
	return openCodeEventFinishReason(event)
}

func parseOpenCodeSmokeFrame(line string) (openCodeSmokeFrame, bool) {
	var frame openCodeSmokeFrame
	if json.Unmarshal([]byte(line), &frame) != nil {
		return openCodeSmokeFrame{}, false
	}
	return frame, true
}

// errorMessage returns the text of an error event, or "" when this frame is not
// one. Only a frame whose TYPE names an error counts: an ordinary event may
// legitimately carry an empty `error` key, and treating that as a failure would
// report a healthy turn as broken.
func (f openCodeSmokeFrame) errorMessage() string {
	if !strings.Contains(strings.ToLower(f.Type), "error") {
		return ""
	}
	if msg := openCodeSmokeRawText(f.Error); msg != "" {
		return msg
	}
	if msg := openCodeSmokeRawText(f.Message); msg != "" {
		return msg
	}
	// A typed error frame with no readable message is still an error frame; the
	// type alone is what the classifier needs.
	return f.Type
}

// openCodeSmokeRawText reads a field spelled either as a string or as an object
// carrying a message/name. Anything else yields "".
func openCodeSmokeRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return strings.TrimSpace(str)
	}
	var obj struct {
		Message string `json:"message"`
		Name    string `json:"name"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(firstNonEmpty(obj.Message, obj.Data.Message, obj.Name))
	}
	return ""
}

// classifyOpenCodeSmokeRun maps one exec outcome onto the closed receipt enum
// and a locally authored diagnostic. Returns ("", …, true) only for an exact
// marker match. Kept pure (no exec, no clock) so every arm is unit-testable;
// `timedOut` is passed in because only the caller can see the per-attempt
// context.
func classifyOpenCodeSmokeRun(timedOut bool, stdout, stderr []byte, runErr error, marker string) (category, diagnostic string, matched bool) {
	// An expired/cancelled attempt outranks whatever the child reported: the
	// kill IS the reason the output is incomplete.
	if timedOut || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		return cliUsageErrorProviderTimeout, cliSmokeDiagnosticTimeout, false
	}

	// cmd.Run/Wait reports an *exec.ExitError only for a child that RAN and
	// exited non-zero. Anything else — CreateProcess refusing a `.cmd` shim, a
	// refused shim render, a missing binary, an I/O failure on the pipes — means
	// we never got a turn out of the CLI at all. That is launch_error, and
	// keeping it out of no_envelope is the whole point: the deployed symptom was
	// those two sharing one bucket.
	//
	// exec.ErrWaitDelay is the one non-ExitError that still means "the child
	// RAN": the process exited (often 0, having said everything it had to say)
	// and a lingering tool GRANDCHILD held the captured pipe past our WaitDelay,
	// so os/exec stopped waiting on the copy. Judging that as a launch failure
	// would report a healthy install as provider_unavailable and throw away
	// stdout that may hold the marker — re-fusing the very buckets this probe
	// exists to separate. Fall through and classify the output we did capture.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) && !errors.Is(runErr, exec.ErrWaitDelay) {
		return cliUsageErrorProviderUnavailable, cliSmokeDiagnosticLaunchError, false
	}

	stream := parseOpenCodeSmokeStream(stdout)

	// A well-formed error event is the CLI telling us the turn did not complete;
	// pick between auth and provider by its text, then drop it.
	if stream.SawError {
		lower := strings.ToLower(stream.ErrorMessage)
		switch {
		case cliSmokeTextMentionsAuth(lower):
			return cliUsageErrorNotAuthenticated, cliSmokeDiagnosticAuthError, false
		case strings.Contains(lower, "limit"), strings.Contains(lower, "quota"),
			strings.Contains(lower, "credit"), strings.Contains(lower, "overloaded"),
			strings.Contains(lower, "unavailable"), strings.Contains(lower, "api error"),
			strings.Contains(lower, "status 5"):
			// The CLI and our invocation are both fine; the provider refused.
			return cliUsageErrorProviderUnavailable, cliSmokeDiagnosticProviderError, false
		default:
			return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
		}
	}

	if !stream.Ended {
		diagnostic := cliSmokeDiagnosticNoEnvelope
		if runErr != nil && !errors.Is(runErr, exec.ErrWaitDelay) {
			// Exit non-zero with no terminal frame: the CLI rejected our
			// invocation shape before producing its documented output — the
			// pre-inference exit this probe exists to name precisely.
			diagnostic = openCodeSmokeNoEnvelopeDiagnostic(stdout, stderr)
		}
		// A clean exit that never emitted a completion event — an updater
		// notice on stdout, a build whose event contract moved — is a broken
		// contract, not a broken model. framing_rejected is NEVER inferred from
		// silence: only a positive rejection names it.
		//
		// NOT ONE frame (and nothing malformed or truncated) is narrower still:
		// the child ran and exited having said nothing we could read, which is
		// the Windows field symptom — a transport or prompt-delivery fault
		// rather than an event-contract one. A named option rejection keeps its
		// own diagnostic.
		if diagnostic == cliSmokeDiagnosticNoEnvelope &&
			stream.Frames == 0 && !stream.Malformed && !stream.Overflow {
			diagnostic = cliSmokeDiagnosticNoOutput
		}
		return cliUsageErrorProtocol, diagnostic, false
	}

	// A truncated or malformed stream reached a terminal frame but the text is
	// partial. Never match partial text against the marker — that would turn a
	// dropped delta into a "chatty model" verdict, or worse, a false success.
	if stream.Malformed || stream.Overflow {
		return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
	}

	// A terminal frame whose text is not the marker means the model was chatty
	// ("Sure! …"), NOT that the CLI contract is broken — classifying that as
	// `protocol` would make a healthy CLI look like this regression.
	if strings.TrimSpace(stream.Text) != marker {
		return cliUsageErrorParseFailed, cliSmokeDiagnosticMarkerMismatch, false
	}
	return "", cliSmokeDiagnosticNone, true
}

// openCodeStderrErrorRegion returns the part of already-lowercased stderr that
// describes WHAT was refused, dropping the usage/help block a CLI appends after
// it. Without this split every flag name the usage block happens to document
// reads as the flag that was rejected.
//
// Bounded by construction: it only ever shortens its input, and the caller's
// stderr is already capped at cliSmokeMaxStderr.
func openCodeStderrErrorRegion(lower string) string {
	// Line-anchored, not a substring search: prose such as "error: invalid usage:
	// --format" is part of the ERROR, while a `Usage: opencode run …` line opens
	// the help block. Matching "usage:" anywhere would cut the region short and
	// hide a genuine framing rejection behind it.
	offset := 0
	for _, line := range strings.SplitAfter(lower, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "usage") {
			return lower[:offset]
		}
		offset += len(line)
	}
	return lower
}

// openCodeSmokeNoEnvelopeDiagnostic separates the pre-inference failures once a
// child exited non-zero with no terminal frame:
//
//   - framing_rejected — the CLI refused the `--format json` output contract
//     itself. Nothing in the probe's argv can fix that, so it is never retried.
//   - flag_rejected — the CLI refused one of OUR options during parsing.
//   - no_envelope — anything else; a turn MAY already have been consumed.
//
// BOTH streams are read, as openCodeRejectedSessionFlag does and for the same
// reason: an OpenCode build can print its option-parse failure as a plain line
// on STDOUT, and reading stderr alone misreported that rejection as
// no_envelope. On stdout only the non-JSON lines count — a JSON event is the
// CLI's documented output (possibly model text quoting "--format"), never a
// parser error. Each stream is cut to its own error region, so a usage block
// on one stream cannot name the flag refused on the other.
//
// The bytes are read only to pick between these constants; not one byte of
// them travels any further.
func openCodeSmokeNoEnvelopeDiagnostic(stdout, stderr []byte) string {
	rejected, framing := false, false
	for _, lower := range []string{
		strings.ToLower(openCodeStdoutPlainLines(stdout)),
		strings.ToLower(string(stderr)),
	} {
		if !openCodeOptionRejectionText(lower) {
			continue
		}
		rejected = true
		// Read the ERROR region only, and match the flag NAME rather than a bare
		// `json` substring. OpenCode prints a usage block after an option error
		// and that block lists `--format <FORMAT> … [possible values: text,
		// json]`, so searching the whole stream for either `json` or `--format`
		// reported every rejected CALLER flag as a broken framing contract — the
		// precise diagnostic this probe exists to provide, inverted.
		if strings.Contains(openCodeStderrErrorRegion(lower), "--format") {
			framing = true
		}
	}
	switch {
	case framing:
		return cliSmokeDiagnosticFramingRejected
	case rejected:
		return cliSmokeDiagnosticFlagRejected
	default:
		return cliSmokeDiagnosticNoEnvelope
	}
}

// openCodeStdoutPlainLines keeps only the stdout lines that are not JSON
// objects: the place a build that writes parser errors to stdout puts them.
// Bounded by the caller's stdout retention cap.
func openCodeStdoutPlainLines(stdout []byte) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(stdout), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		b.WriteString(line)
	}
	return b.String()
}
