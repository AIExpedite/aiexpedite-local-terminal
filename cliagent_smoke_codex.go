// cliagent_smoke_codex.go — the Codex provider of the signed `__cli_smoke__`
// operational command: a read-only, headless `codex exec --json` probe. The
// provider-agnostic core (closed diagnostic set, metric-only result, cooldown,
// singleflight, shape cache) lives in cliagent_smoke.go.
//
// After a Codex update the terminal path could exit 0 while the marker the
// maintenance flow compares was empty or split: a post-update build emits its
// assistant text as a JSON-RPC `method`, nested under `params.msg`, or as
// `agent_message_delta` chunks, and the display parser only read the
// pre-update `item.completed` shape. Both this probe and the session path now
// read through codex_frames.go, so the same four shapes yield the same text on
// either.
//
// This probe:
//   - runs the free pre-checks (binary present, version answers, not a
//     definite logout) before spending a turn;
//   - asks for the marker on STDIN (never argv) from a per-run temp cwd,
//     read-only sandbox, and reads the answer from `--output-last-message`
//     OR the folded JSON stream — a trimmed exact match of either succeeds;
//   - walks its two-rung ladder only on an option-parsing rejection of the
//     one droppable flag, so the walk costs children, never turns;
//   - arms the Codex utilization run floor before the first spawn and, once
//     the ladder stops, SETTLES it exactly when the run produced evidence
//     that pays it (its own captured rate-limit windows, marker match,
//     completion frame, or a new rollout write) and DISARMS it otherwise —
//     see settleOrDisarmCodexSmokeRun;
//   - feeds the rate-limit frames its own turn printed through the ordinary
//     capture path, so the turn it spent produces its own reading instead of
//     leaving it to a rollout scan a Codex upgrade may have broken.
//
// Retention discipline applies here exactly as in cliagent_smoke.go: the
// child's stdout, stderr and last-message file are read to derive a verdict
// and then DISCARDED. The only thing kept is what the capture path keeps from
// a recognised rate-limit envelope — numeric windows. The marker nonce, the
// prompt, the resolved argv, the last-message path and `auth.json` /
// `config.toml` are never published or logged at any severity.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexSmokeMarkerPrefix + 8 hex chars is the nonce the model is asked to
// echo. Generated per run: a FIXED marker could be satisfied by a cached
// transcript and would pass on a dead binary. Distinct from the other
// providers' prefixes so a fixture can never satisfy the wrong one.
const codexSmokeMarkerPrefix = "AIEXPEDITE_CODEX_SMOKE_OK_"

// codexSmokeLastMessageName is the `--output-last-message` file, created in
// the per-run temp cwd and removed with it.
const codexSmokeLastMessageName = "last-message.txt"

// codexSmokeTimeout bounds the one inference turn. A var, not a const, so a
// test can shrink it and exercise the real deadline-kill path (a killed child
// reports an *exec.ExitError, NOT a wrapped context error).
var codexSmokeTimeout = 60 * time.Second

// codexSmokeArgvShape is one rung of the probe's ladder. The ID is what the
// published result carries — never the argv it produces.
type codexSmokeArgvShape struct {
	ID string
	// LastMessage adds `--output-last-message <file>`, the one flag a build
	// may reject and the one rung 2 drops.
	LastMessage bool
}

// codexSmokeArgvShapes is the ladder, preferred rung first. Neither carries
// the approval/sandbox bypass flag the interactive session uses.
var codexSmokeArgvShapes = []codexSmokeArgvShape{
	{ID: "codex-exec-json-last-message-v1", LastMessage: true},
	{ID: "codex-exec-json-v1"},
}

// codexSmokeRetryableFlag is the only flag a later rung drops.
const codexSmokeRetryableFlag = "output-last-message"

// buildCodexSmokeArgs renders one rung. The trailing `-` reads the prompt from
// stdin; `--skip-git-repo-check` is required because the cwd is a fresh temp
// dir, not a repository.
func buildCodexSmokeArgs(shape codexSmokeArgvShape, lastMessageFile string) []string {
	args := []string{"exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check"}
	if shape.LastMessage {
		args = append(args, "--output-last-message", lastMessageFile)
	}
	return append(args, "-")
}

// codexSmokeLaunch is everything one probe attempt spawns with, bundled so the
// exec seam has one stable signature and the Windows shim route sees the whole
// launch at once.
type codexSmokeLaunch struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Prompt string
	// LastMessageFile is the value of `--output-last-message` in Args, carried
	// separately so the shim route can hand it to cmd.exe through the
	// environment instead of interpolating it into the script line.
	LastMessageFile string
}

/* --------------------------------------------------------------------------
   Exec seam
   -------------------------------------------------------------------------- */

// runCodexSmokeCommand spawns the probe. A package-level var so tests drive
// the classification without spawning a real binary or spending a turn.
var runCodexSmokeCommand = func(ctx context.Context, launch codexSmokeLaunch) (stdout, stderr []byte, err error) {
	cmd := newCodexSmokeCmd(ctx, launch)
	cmd.Stdin = strings.NewReader(launch.Prompt)
	outBuf := &boundedBuffer{limit: cliSmokeMaxStdout}
	errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// newCodexSmokeCmd builds the child for one attempt: a native binary directly,
// a Windows `.cmd` / `.bat` npm shim through cmd.exe (codexSmokeShimCommand).
// Hidden on every route — a background probe on a windowless tray app.
func newCodexSmokeCmd(ctx context.Context, launch codexSmokeLaunch) *exec.Cmd {
	if cmd, ok := codexSmokeShimCommand(ctx, launch); ok {
		return cmd
	}
	cmd := exec.CommandContext(ctx, launch.Path, launch.Args...)
	cmd.Env = launch.Env
	cmd.Dir = launch.Dir
	hideWindow(cmd)
	return cmd
}

/* --------------------------------------------------------------------------
   Resolution + pre-checks (no turn spent)
   -------------------------------------------------------------------------- */

// resolveCodexSmokePath re-resolves the binary on every smoke — the point of
// the post-update smoke is to validate the binary that was just replaced. Same
// lookup order as gatherCLIAgents: PATH, then the installer's bin dir.
//
// A var so tests can point it at a stub without touching the real install.
var resolveCodexSmokePath = func() string {
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	return resolveInstallerBinary("codex", installerBinDirFor("codex"))
}

// codexProbeVersion answers `--version` for a Codex binary, cached per
// (path, mtime, size). This is the ONLY Codex version probe — gatherCLIAgents
// routes here too — because the cache key is shared: a plain spawn of
// `codex.cmd` would cache "" under it and every later probe, including this
// smoke's binary_missing pre-check, would read that negative back.
func codexProbeVersion(path string) string {
	if isWindowsShimPath(path) {
		return cachedProbeVersionFunc(path, func() string {
			ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
			defer cancel()
			cmd, ok := codexSmokeShimCommand(ctx, codexSmokeLaunch{
				Path: path, Args: []string{"--version"}, Env: os.Environ(),
			})
			if !ok {
				return ""
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return ""
			}
			return firstNonEmptyLine(out)
		})
	}
	return cachedProbeVersion(path)
}

// codexSmokeShapeLadder returns the rungs to try. A shape already resolved for
// this exact binary is the ONLY entry, so a steady-state smoke spawns one child.
func codexSmokeShapeLadder(path string) []codexSmokeArgvShape {
	resolved, cached := cliSmokeRememberedShape(path)
	if !cached {
		return codexSmokeArgvShapes
	}
	for _, shape := range codexSmokeArgvShapes {
		if shape.ID == resolved {
			return []codexSmokeArgvShape{shape}
		}
	}
	return codexSmokeArgvShapes
}

/* --------------------------------------------------------------------------
   Probe
   -------------------------------------------------------------------------- */

// runCodexSmoke performs the probe against an already-resolved binary.
// Registered in cliSmokeProviders (cliagent_smoke.go).
func runCodexSmoke(ctx context.Context, path, version string) cliSmokeResult {
	result := cliSmokeResult{CliID: "codex", Version: version, Status: cliSmokeStatusFailed}
	started := time.Now()
	finish := func(category, diagnostic string) cliSmokeResult {
		result.ErrorCategory = category
		result.Diagnostic = diagnostic
		result.DurationMs = time.Since(started).Milliseconds()
		return result
	}

	// Pre-check 1: the binary must exist and answer `--version`.
	if path == "" || version == "" {
		return finish(cliUsageErrorProviderUnavailable, cliSmokeDiagnosticBinaryMissing)
	}
	// Pre-check 2: a definite logout cannot complete a turn. Inconclusive —
	// including a logout while an API key will authenticate — proceeds.
	if loggedIn, known := codexSmokeLoginCheck(ctx, path); known && !loggedIn {
		return finish(cliUsageErrorNotAuthenticated, cliSmokeDiagnosticNotLoggedIn)
	}

	marker, err := newCodexSmokeMarker()
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}
	// A per-run cwd outside any repository (hence --skip-git-repo-check), which
	// also holds the last-message file. Removed on every path below, after the
	// last child that could write it has been reaped.
	workDir, err := os.MkdirTemp("", "aiexpedite-codex-smoke-")
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	lastMessageFile := filepath.Join(workDir, codexSmokeLastMessageName)

	// The agent's own env, sanitized like every Codex child — NOT an isolated
	// home: Codex must keep its credentials and write its rollout telemetry
	// where codexReconcileFromRollout scans.
	env, _ := prepareClaudeChildEnv(path, os.Environ())

	shapeBinding := bindCLISmokeShape(path)
	// A capture made outside a gather still stamps the binary that produced
	// it — the one this smoke was sent to validate.
	publishCodexUsageCaptureVersion(version)
	// The account the turn runs under. A reading taken after the credentials
	// changed cannot be attributed to it and never reaches the cache.
	smokeFingerprint := currentCodexAccountFingerprint()
	// The run floor is armed before the first spawn and settled or disarmed
	// exactly once, after the ladder stops, from the FINAL attempt's evidence.
	// A rejected rung that continues the walk is not an outcome.
	floor := time.UnixMilli(time.Now().UnixMilli())
	codexUsageRunStarted(floor)
	var evidence codexSmokeEvidence
	// usageCaptured is sticky across rungs: a window an earlier rung merged is
	// already in the cache, so a later rung that captures nothing must not
	// disarm a run whose telemetry did land.
	usageCaptured := false
	defer func() { settleOrDisarmCodexSmokeRun(floor, evidence) }()

	var lastCategory, lastDiagnostic string
	for _, shape := range codexSmokeShapeLadder(path) {
		_ = os.Remove(lastMessageFile)
		runCtx, cancel := context.WithTimeout(ctx, codexSmokeTimeout)
		stdout, stderr, runErr := runCodexSmokeCommand(runCtx, codexSmokeLaunch{
			Path: path, Args: buildCodexSmokeArgs(shape, lastMessageFile), Env: env, Dir: workDir,
			Prompt: codexSmokePrompt(marker), LastMessageFile: lastMessageFile,
		})
		// Read the PER-ATTEMPT context before cancelling it: a deadline kill
		// reports an *exec.ExitError, not a wrapped context error.
		timedOut := runCtx.Err() != nil || ctx.Err() != nil
		cancel()
		var lastMessage []byte
		if shape.LastMessage {
			lastMessage = readCodexSmokeLastMessage(lastMessageFile)
		}

		result.ArgvShapeID = shape.ID
		stream := parseCodexSmokeStream(stdout)
		if captureCodexSmokeRateLimits(stream.RateLimitLines, smokeFingerprint) {
			usageCaptured = true
		}
		category, diagnostic, matched := classifyCodexSmokeRun(timedOut, stream, stderr, lastMessage, runErr, marker)
		// Utilization evidence is what the TURN produced, not what the verdict
		// was: a run that emitted the exact marker and THEN reported an error
		// still spent a payable turn, so it settles even though it fails.
		evidence = codexSmokeEvidence{
			markerSeen:    codexSmokeMarkerSeen(stream, lastMessage, marker),
			completed:     stream.Completed,
			usageCaptured: usageCaptured,
		}
		if category == "" {
			result.Status = cliSmokeStatusSuccess
			result.MarkerMatched = matched
			result.Diagnostic = diagnostic
			result.DurationMs = time.Since(started).Milliseconds()
			shapeBinding.remember(shape.ID)
			return result
		}
		lastCategory, lastDiagnostic = category, diagnostic
		// Device-local log line: closed values plus the stderr LENGTH.
		fmt.Print(codexSmokeFailureLogLine(shape.ID, category, diagnostic, len(stderr)))

		// Walk ONLY on an option-parsing rejection of the flag the next rung
		// drops — it precedes inference, so it cannot double-charge a turn.
		if diagnostic != cliSmokeDiagnosticFlagRejected || !codexSmokeMentionsRetryableFlag(stderr) {
			break
		}
	}

	result.MarkerMatched = false
	return finish(lastCategory, lastDiagnostic)
}

// readCodexSmokeLastMessage reads the `--output-last-message` file, bounded
// like the child's stdout. A missing file reads as empty.
func readCodexSmokeLastMessage(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	body, _ := io.ReadAll(io.LimitReader(f, cliSmokeMaxStdout))
	return body
}

// captureCodexSmokeRateLimits hands the rate-limit frames one attempt printed
// to the ordinary capture path, pinned to the account the smoke started
// under — and only while that account is still the signed-in one, checked
// BEFORE any write: a reading that cannot be attributed must never reach the
// cache. Reports whether any frame landed a numeric window (or an
// authoritative clear).
func captureCodexSmokeRateLimits(lines []string, fingerprint string) bool {
	if len(lines) == 0 || currentCodexAccountFingerprint() != fingerprint {
		return false
	}
	captured := false
	now := time.Now()
	for _, line := range lines {
		if captureCodexRateLimitLineForAccount(line, now, fingerprint) {
			captured = true
		}
	}
	return captured
}

// codexSmokeFailureLogLine renders the device-local diagnostic line. It takes
// the stderr LENGTH rather than the bytes: a function that cannot receive
// vendor text cannot leak it.
func codexSmokeFailureLogLine(shapeID, category, diagnostic string, stderrBytes int) string {
	return fmt.Sprintf("%s[cli-smoke] codex shape=%s category=%s diagnostic=%s stderrBytes=%d%s\n",
		colorYellow, shapeID, category, diagnostic, stderrBytes, colorReset)
}

/* --------------------------------------------------------------------------
   Utilization: settle on evidence, disarm otherwise
   -------------------------------------------------------------------------- */

// codexSmokeEvidence is what the final attempt proved about the turn, plus
// whether ANY attempt's own rate-limit frames reached the cache.
type codexSmokeEvidence struct {
	markerSeen bool // trimmed exact marker from the last-message file or the stream
	completed  bool // a recognised turn-completion frame (codexRunCompletionShape)
	// usageCaptured: a numeric window from the probe's own stdout merged into
	// the cache (sticky across rungs). The merge has already paid the debt the
	// settle is about to record.
	usageCaptured bool
}

// settleOrDisarmCodexSmokeRun decides the probe's armed utilization run, once.
//
// It SETTLES exactly when the run left something that pays it: its own
// rate-limit windows already captured (checked first — the settle's own write
// then finds the floor covered), a marker match, a completion frame, or —
// checked last, and only then — a rollout written after the floor. Every other outcome DISARMS: a timeout, a
// flag/framing/auth failure, a mismatch that wrote nothing. Both halves
// matter. Settling with no evidence writes RefreshOwed the bounded reconciles
// cannot pay, which IS the stale-utilization warning; leaving the floor armed
// only defers it, because payOwedCodexUsageRefresh adopts an armed, unsettled
// floor as an interrupted run at the next start — and the post-update restart
// is exactly when this smoke runs.
func settleOrDisarmCodexSmokeRun(floor time.Time, evidence codexSmokeEvidence) {
	if evidence.usageCaptured || evidence.markerSeen || evidence.completed || codexSmokeRolloutSignal(floor) {
		codexUsageRunSettled(floor)
		return
	}
	codexUsageRunDisarmed(floor, time.Time{})
}

// codexSmokeRolloutSignal reports whether a Codex rollout was written after the
// probe armed its floor. A newer file is only attributable to THIS turn while
// no other Codex run of this process is open — a terminal session or an
// app-server turn writes rollouts into the same tree — so with one open the
// signal is ignored and the outcome rests on the marker and completion frame.
//
// "Strictly after the floor" is the delta against an arm-time snapshot without
// the snapshot's extra walk: a file that predates the arm cannot pass, and the
// floor is recorded before the child is spawned, so this turn's own rollout
// (written as it ends) always lands after it. Bounded by the same budget a
// forced reconcile gets, because it is a synchronous tree walk.
func codexSmokeRolloutSignal(floor time.Time) bool {
	if codexNewestOpenRunFloor() > 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexForcedReconcileBudget)
	defer cancel()
	return codexSmokeRolloutWalk(ctx, filepath.Join(codexHomeBase(), "sessions"), floor)
}

// codexSmokeRolloutWalk is the tree walk behind the rollout signal. A var so a
// test can prove the walk is skipped whenever stronger evidence settled.
var codexSmokeRolloutWalk = codexRolloutWrittenAfter

// errCodexRolloutFound stops the walk at the first qualifying rollout.
var errCodexRolloutFound = errors.New("rollout found")

// codexRolloutWrittenAfter reports whether any `rollout-*.jsonl` under root was
// modified strictly after `after`. It stops at the first hit and at ctx expiry
// (which reads as "not found": no evidence, so the run disarms).
func codexRolloutWrittenAfter(ctx context.Context, root string, after time.Time) bool {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || entry.IsDir() {
			return nil
		}
		if matched, _ := filepath.Match("rollout-*.jsonl", entry.Name()); !matched {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.ModTime().After(after) {
			return errCodexRolloutFound
		}
		return nil
	})
	return errors.Is(err, errCodexRolloutFound)
}

/* --------------------------------------------------------------------------
   Classification
   -------------------------------------------------------------------------- */

// codexSmokeStream is what the classifier needs from the child's JSON stdout.
type codexSmokeStream struct {
	// Text is the folded assistant answer (codexFoldAssistantText).
	Text string
	// Completed is set by any recognised turn-completion frame.
	Completed bool
	// ErrorMessage is the first error frame's message, read only to pick a
	// constant and never retained past the classifier.
	ErrorMessage string
	SawError     bool
	// RateLimitLines are the lines that could carry rate-limit telemetry,
	// collected for runCodexSmoke to capture. Whether each is a recognised
	// envelope is the capture path's decision, not the parser's.
	RateLimitLines []string
}

// parseCodexSmokeStream reads the whole stdout once for the three facts the
// verdict rests on, and collects the candidate rate-limit lines on the same
// pass. Non-JSON lines are skipped rather than failing the parse. Pure: no
// cache I/O happens here.
func parseCodexSmokeStream(stdout []byte) codexSmokeStream {
	stream := codexSmokeStream{Text: codexFoldAssistantText(stdout)}
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if codexSmokeRateLimitCandidate(line) {
			stream.RateLimitLines = append(stream.RateLimitLines, string(line))
		}
		if codexRunCompletionShape(string(line)) != "" {
			stream.Completed = true
			continue
		}
		if stream.SawError {
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal(line, &raw) != nil {
			continue
		}
		if message, ok := codexErrorFrameMessage(raw); ok {
			stream.SawError = true
			stream.ErrorMessage = message
		}
	}
	return stream
}

// codexSmokeRateLimitCandidate is captureCodexRateLimitLineForAccount's cheap
// prefilter, applied before a line is retained at all.
func codexSmokeRateLimitCandidate(line []byte) bool {
	return bytes.Contains(line, []byte("token_count")) ||
		bytes.Contains(line, []byte("rateLimits")) ||
		bytes.Contains(line, []byte("rate_limit"))
}

// codexErrorFrameMessage recognises a Codex error frame in any envelope shape
// — `error`, `stream_error`, `turn.failed` / `turn/failed` — and returns its
// message (possibly empty). The same event is named at several nodes of a
// JSON-RPC frame (`method` over `params`, then `params.msg`), so the first
// node that carries a message wins.
func codexErrorFrameMessage(raw map[string]interface{}) (string, bool) {
	sawError := false
	for _, node := range codexAssistantEventNodes(raw) {
		name := strings.TrimPrefix(node.name, "codex/event/")
		bare := codexBareEventName(name)
		if bare != "error" && bare != "stream_error" && codexNormalizeCompletionName(name) != "turn.failed" {
			continue
		}
		message := codexFirstString(node.body, "message", "error")
		if nested, ok := node.body["error"].(map[string]interface{}); ok && message == "" {
			message = codexFirstString(nested, "message")
		}
		if message != "" {
			return message, true
		}
		sawError = true
	}
	return "", sawError
}

// classifyCodexSmokeRun maps one exec outcome onto the closed receipt enum and
// a locally authored diagnostic. Returns ("", …, true) only for an exact
// marker match. Pure, so every arm is unit-testable.
//
// A trimmed exact match is sufficient on its own: a build that answers with
// `item.completed` and no turn-level completion frame has still returned the
// marker. The completion frame only EXPLAINS a mismatch — framing (no text,
// no completion: no_envelope) versus a chatty model (marker_mismatch).
//
// An EXPLICIT error frame is the one thing that outranks the match: a turn
// that printed the marker and then reported `turn.failed` / `error` did not
// end well, and caching it as a success for the cooldown would hide the auth
// or quota failure the card exists to show. A non-zero exit on its own does
// NOT outrank it — the marker is random per run, so an exact match proves
// this turn answered, and failing it would turn a shim's or a teardown's exit
// code into a red card for a Codex that works.
func classifyCodexSmokeRun(timedOut bool, stream codexSmokeStream, stderr, lastMessage []byte, runErr error, marker string) (category, diagnostic string, matched bool) {
	// The kill IS the reason the output is incomplete.
	if timedOut || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		return cliUsageErrorProviderTimeout, cliSmokeDiagnosticTimeout, false
	}
	markerSeen := codexSmokeMarkerSeen(stream, lastMessage, marker)

	if stream.SawError {
		category, diagnostic := classifyCodexSmokeErrorFrame(stream.ErrorMessage)
		return category, diagnostic, false
	}
	if markerSeen {
		return "", cliSmokeDiagnosticNone, true
	}

	answered := stream.Text != "" || len(bytes.TrimSpace(lastMessage)) > 0
	if !answered && !stream.Completed {
		if runErr != nil {
			if cliSmokeTextMentionsAuth(strings.ToLower(string(stderr))) {
				return cliUsageErrorNotAuthenticated, cliSmokeDiagnosticAuthError, false
			}
			// Exit non-zero with nothing on stdout: the CLI rejected our
			// invocation before producing its documented output.
			return cliUsageErrorProtocol, codexSmokeNoEnvelopeDiagnostic(stderr), false
		}
		return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
	}
	// The turn answered (or completed) with something other than the marker:
	// the model was chatty, NOT a broken CLI contract.
	return cliUsageErrorParseFailed, cliSmokeDiagnosticMarkerMismatch, false
}

// codexSmokeMarkerSeen reports whether the turn produced the exact marker on
// either channel. It is the verdict's success test AND the utilization run's
// payable-turn evidence, which is why it is one predicate: those two answers
// diverge (an errored turn fails but still settles) and must not drift.
func codexSmokeMarkerSeen(stream codexSmokeStream, lastMessage []byte, marker string) bool {
	return strings.TrimSpace(string(lastMessage)) == marker || strings.TrimSpace(stream.Text) == marker
}

// classifyCodexSmokeErrorFrame maps an explicit error frame onto the closed
// enum. Reaching here means a recognised `error` / `stream_error` /
// `turn.failed` envelope arrived, so the CLI contract plainly held: the
// message only picks between auth and the provider, and wording we do not
// recognise stays a provider failure. no_envelope would be a false report —
// it is reserved for a stream that produced no terminal envelope at all.
func classifyCodexSmokeErrorFrame(message string) (category, diagnostic string) {
	lower := strings.ToLower(message)
	if cliSmokeTextMentionsAuth(lower) || strings.Contains(lower, "401") {
		return cliUsageErrorNotAuthenticated, cliSmokeDiagnosticAuthError
	}
	// Everything else — a quota refusal, a 5xx, or wording we have never
	// seen: the CLI and our invocation are both fine, the turn is not.
	return cliUsageErrorProviderUnavailable, cliSmokeDiagnosticProviderError
}

// codexSmokeNoEnvelopeDiagnostic separates the pre-inference rejections once a
// child exited non-zero with nothing on stdout:
//
//   - flag_rejected — clap refused one of OUR options. Retryable only when it
//     names the flag rung 2 drops (codexSmokeMentionsRetryableFlag).
//   - framing_rejected — clap refused the `--json` output contract; no rung
//     changes that, so never retried.
//   - no_envelope — anything else; a turn MAY have been consumed.
//
// The stderr bytes are read only to pick between these constants.
func codexSmokeNoEnvelopeDiagnostic(stderr []byte) string {
	lower := strings.ToLower(string(stderr))
	rejected := strings.Contains(lower, "unexpected argument") ||
		strings.Contains(lower, "unrecognized") ||
		strings.Contains(lower, "unrecognised") ||
		strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "unknown argument") ||
		strings.Contains(lower, "wasn't expected") ||
		strings.Contains(lower, "a value is required") ||
		strings.Contains(lower, "invalid value")
	if !rejected {
		return cliSmokeDiagnosticNoEnvelope
	}
	if !codexSmokeMentionsRetryableFlag(stderr) && strings.Contains(lower, "--json") {
		return cliSmokeDiagnosticFramingRejected
	}
	return cliSmokeDiagnosticFlagRejected
}

// codexSmokeMentionsRetryableFlag reports whether the rejection names the flag
// a later rung drops. Rejecting any other flag is not retryable — every rung
// carries it — and walking would spawn a second child to fail identically.
func codexSmokeMentionsRetryableFlag(stderr []byte) bool {
	return strings.Contains(strings.ToLower(string(stderr)), codexSmokeRetryableFlag)
}

// codexSmokePrompt is the one-turn instruction. It names no path, account or
// configuration and asks for no tool use, so an answer-only turn renders
// nothing but the marker on either path.
func codexSmokePrompt(marker string) string {
	return "Reply with exactly this text and nothing else, no punctuation, no explanation and no tool use: " + marker
}

func newCodexSmokeMarker() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return codexSmokeMarkerPrefix + hex.EncodeToString(buf), nil
}

/* --------------------------------------------------------------------------
   Windows .cmd shim route (script rendering — platform-independent)
   -------------------------------------------------------------------------- */

// Environment variables the shim route uses to hand the launch's PATHS to
// cmd.exe as data rather than script text (see grokSmokeShimPathEnv).
const (
	codexSmokeShimPathEnv        = "AIEXPEDITE_CODEX_SMOKE_SHIM"
	codexSmokeShimLastMessageEnv = "AIEXPEDITE_CODEX_SMOKE_LAST_MESSAGE"
)

// codexSmokeShimOperands is the fixed allowlist of non-flag tokens the Codex
// argvs contain — subcommands, the sandbox mode, and the stdin placeholder.
// grokSmokeFixedFlagToken refuses anything without a `--` prefix, so reusing
// Grok's renderer would refuse every Codex launch and fall back to the plain
// spawn a `.cmd` shim cannot survive.
var codexSmokeShimOperands = map[string]bool{
	"exec": true, "login": true, "status": true, "read-only": true, "-": true,
}

// codexSmokeShimScript renders the explicit cmd.exe line for a shim launch:
//
//	"%AIEXPEDITE_CODEX_SMOKE_SHIM%" exec --json … --output-last-message "%AIEXPEDITE_CODEX_SMOKE_LAST_MESSAGE%" -
//
// Invoked directly (no `call`, which would expand percent sequences twice).
// Every token must be an allowlisted operand or a fixed `--flag` /
// `--flag=value`; anything else refuses the route (ok=false).
func codexSmokeShimScript(args []string) (script string, ok bool) {
	var b strings.Builder
	b.WriteString(`"%` + codexSmokeShimPathEnv + `%"`)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--output-last-message" && i+1 < len(args) {
			b.WriteString(` --output-last-message "%` + codexSmokeShimLastMessageEnv + `%"`)
			i++
			continue
		}
		if !codexSmokeShimOperands[arg] && !grokSmokeFixedFlagToken(arg) {
			return "", false
		}
		b.WriteString(" " + arg)
	}
	return b.String(), true
}

// codexSmokeShimCommand routes a `.cmd` / `.bat` Codex launch through cmd.exe
// (cliSmokeShimCommand). Returns ok=false for a native binary or an argv the
// renderer refuses; the caller then spawns directly.
func codexSmokeShimCommand(ctx context.Context, launch codexSmokeLaunch) (*exec.Cmd, bool) {
	if !isWindowsShimPath(launch.Path) {
		return nil, false
	}
	script, ok := codexSmokeShimScript(launch.Args)
	if !ok {
		return nil, false
	}
	env := setEnvVar(launch.Env, codexSmokeShimPathEnv, launch.Path)
	if launch.LastMessageFile != "" {
		env = setEnvVar(env, codexSmokeShimLastMessageEnv, launch.LastMessageFile)
	}
	return cliSmokeShimCommand(ctx, script, env, launch.Dir), true
}
