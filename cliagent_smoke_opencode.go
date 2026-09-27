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
//     a bucket with "the child ran and produced no envelope";
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
	"strings"
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
var runOpenCodeSmokeCommand = func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error) {
	prompt, openErr := os.Open(launch.PromptFile)
	if openErr != nil {
		return nil, nil, openErr
	}
	defer prompt.Close()
	launch.Stdin = prompt

	cmd, launchErr := newOpenCodeCmd(ctx, launch)
	if launchErr != nil {
		return nil, nil, launchErr
	}
	outBuf := &boundedBuffer{limit: cliSmokeMaxStdout}
	errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	// Stdout/Stderr are plain writers, so os/exec copies the child's pipes on its
	// own goroutines and Wait waits for those copies. A tool child that outlives
	// the kill still holds the write end, and without a delay Wait would block on
	// it forever — the per-attempt deadline would bound nothing.
	cmd.WaitDelay = openCodeSmokeWaitDelay
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
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	err = cmd.Wait()
	return outBuf.Bytes(), errBuf.Bytes(), err
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
	if isWindowsShimPath(path) {
		return cachedProbeVersionFunc(path, func() string {
			return openCodeShimProbeVersion(path)
		})
	}
	return cachedProbeVersion(path)
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

	// Pre-check 2: a CONCLUSIVE "no usable provider" means the turn cannot
	// complete. Anything inconclusive proceeds — that is what a working
	// local-model or env-credential install looks like (see
	// cliagent_usage_opencode.go's fail-open design).
	if loggedIn, known := openCodeSmokeLoggedIn(ctx, path); known && !loggedIn {
		return finish(cliUsageErrorNotAuthenticated, cliSmokeDiagnosticNotLoggedIn)
	}

	marker, err := newOpenCodeSmokeMarker()
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}

	// A fresh empty cwd per run: OpenCode discovers a project `opencode.json`
	// (and its agents/MCP entries) by walking upward from cwd, so probing inside
	// the caller's workspace would let a repository's configuration decide what
	// the smoke measures. A directory this process cannot create is a LAUNCH
	// failure, not a missing envelope.
	runDir, err := os.MkdirTemp(cliPromptTempDir("opencode-smoke"), "cwd-*")
	if err != nil {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticLaunchError)
	}
	defer func() { _ = os.RemoveAll(runDir) }()

	promptPath, promptFile, err := writeOpenCodePromptFile(openCodeSmokePrompt(marker))
	if err != nil {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticLaunchError)
	}
	// The handle is not the one the child reads (the exec seam opens the path
	// itself, so a test double can recover what the child was asked); close it
	// immediately and remove the file once the child has been reaped.
	_ = promptFile.Close()
	defer func() { _ = os.Remove(promptPath) }()

	// Bound BEFORE the spawn: the shape this run resolves is a fact about THESE
	// bytes, not about whatever is at `path` when it finishes.
	shapeBinding := bindCLISmokeShape(path)
	shape := openCodeRunShapeNoSession
	result.ArgvShapeID = shape.ID

	runCtx, cancel := context.WithTimeout(ctx, openCodeSmokeTimeout)
	stdout, stderr, runErr := runOpenCodeSmokeCommand(runCtx, openCodeLaunch{
		Path:       path,
		Args:       buildOpenCodeRunArgs(shape, ""),
		Env:        sanitizeOpenCodeEnv(os.Environ()),
		Dir:        runDir,
		PromptFile: promptPath,
	})
	// Read the PER-ATTEMPT context before cancelling it: a deadline kill reports
	// an *exec.ExitError, not a wrapped context error, so judging by the parent
	// ctx alone would read a timeout as a launch or protocol failure.
	timedOut := runCtx.Err() != nil || ctx.Err() != nil
	cancel()

	category, diagnostic, matched := classifyOpenCodeSmokeRun(timedOut, stdout, stderr, runErr, marker)
	if category == "" {
		result.Status = cliSmokeStatusSuccess
		result.MarkerMatched = matched
		result.Diagnostic = diagnostic
		result.DurationMs = time.Since(started).Milliseconds()
		shapeBinding.remember(shape.ID)
		return result
	}
	// Device-local log line: closed values plus LENGTHS — metrics, not content.
	// The child's bytes live no longer than the classifier that read them.
	fmt.Print(openCodeSmokeFailureLogLine(shape.ID, category, diagnostic, len(stderr), len(stdout)))
	return finish(category, diagnostic)
}

// openCodeSmokeFailureLogLine renders the device-local diagnostic line. It takes
// stderr/stdout LENGTHS rather than the bytes, deliberately: a function that
// cannot receive vendor text cannot leak it, no matter how a future caller wires
// it up. Every other argument is a value this package defines.
func openCodeSmokeFailureLogLine(shapeID, category, diagnostic string, stderrBytes, stdoutBytes int) string {
	return fmt.Sprintf("%s[cli-smoke] opencode shape=%s category=%s diagnostic=%s stderrBytes=%d stdoutBytes=%d%s\n",
		colorYellow, shapeID, category, diagnostic, stderrBytes, stdoutBytes, colorReset)
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
   Classification
   -------------------------------------------------------------------------- */

// openCodeSmokeStream is what the classifier needs from the child's `--format
// json` stdout: the concatenated assistant text, whether a terminal frame
// arrived, the first error event's message (read to pick a constant, never
// retained past the classifier), and whether the stream was TRUNCATED or
// malformed — in which case the accumulated text is partial and must never be
// compared against the marker.
type openCodeSmokeStream struct {
	Text         string
	Ended        bool
	ErrorMessage string
	SawError     bool
	Malformed    bool
	Overflow     bool
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
		if msg := frame.errorMessage(); msg != "" {
			if !stream.SawError {
				stream.SawError = true
				stream.ErrorMessage = msg
			}
			continue
		}
		if isOpenCodeTerminalEventType(frame.Type) {
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
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
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
		if runErr != nil {
			// Exit non-zero with no terminal frame: the CLI rejected our
			// invocation shape before producing its documented output — the
			// pre-inference exit this probe exists to name precisely.
			return cliUsageErrorProtocol, openCodeSmokeNoEnvelopeDiagnostic(stderr), false
		}
		// A clean exit that never emitted a completion event — an updater
		// notice on stdout, a build whose event contract moved — is a broken
		// contract, not a broken model. framing_rejected is NEVER inferred from
		// silence: only a positive rejection names it.
		return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
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
// The stderr bytes are read only to pick between these constants; not one byte
// of them travels any further.
func openCodeSmokeNoEnvelopeDiagnostic(stderr []byte) string {
	lower := strings.ToLower(string(stderr))
	if !openCodeOptionRejectionText(lower) {
		return cliSmokeDiagnosticNoEnvelope
	}
	// Read the ERROR region only, and match the flag NAME rather than a bare
	// `json` substring. OpenCode prints a usage block after an option error and
	// that block lists `--format <FORMAT> … [possible values: text, json]`, so
	// searching the whole of stderr for either `json` or `--format` reported
	// every rejected CALLER flag as a broken framing contract — the precise
	// diagnostic this probe exists to provide, inverted.
	if strings.Contains(openCodeStderrErrorRegion(lower), "--format") {
		return cliSmokeDiagnosticFramingRejected
	}
	return cliSmokeDiagnosticFlagRejected
}
