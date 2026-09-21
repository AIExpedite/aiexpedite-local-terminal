// cliagent_smoke_grok.go — the Grok provider of the signed `__cli_smoke__`
// operational command: the no-tools, headless `grok --output-format=streaming-json`
// probe. The provider-agnostic core (closed diagnostic set, metric-only result,
// cooldown, singleflight, shape cache) lives in cliagent_smoke.go; the argv
// shape ladder and the maintenance-only env policy live in grok_argv.go.
//
// Before this provider existed, Grok's maintenance smoke ran only through the
// signed `session_start` transport with a hand-frozen 12-token argv whose
// `--tools ""` operand a Windows `.cmd` shim re-parse could drop. The child then
// exited non-zero during option parsing — before inference — so no marker
// frame was ever produced, persistGrokManagedBillingSnapshot had nothing newer
// to merge, and the post-update signed usage refresh replayed the pre-update
// observation. Nothing distinguished "our argv was rejected" from "the model
// refused": that path has no equivalent of the cliSmokeDiagnostic set.
//
// This probe:
//   - runs every pre-inference check the session path runs (system-config
//     posture, isolated auth-only home, login assessment) and classifies each
//     refusal into the closed diagnostic set, spending no turn;
//   - spawns the canonical empty-operand-free argv and walks the ladder ONLY
//     on a pre-inference flag rejection;
//   - matches the marker nonce against the concatenated `text` deltas ending
//     in Grok's terminal `end` frame;
//   - merges the child's `billing: fetched credits config` record into the
//     persistent home through the SAME persistGrokManagedBillingSnapshot the
//     session path uses, so the next signed `__cli_usage_refresh__` sees a
//     newer observation.
//
// Retention discipline applies here exactly as in cliagent_smoke.go: the
// child's stdout and stderr are read to derive a verdict and then DISCARDED.
// The marker nonce, the prompt, the resolved argv, the isolated home's
// contents (`auth.json`, `config.toml`, the private log) are never published or
// logged at any severity.

package main

import (
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
	"runtime"
	"strings"
	"time"
)

// grokSmokeMarkerPrefix + 8 hex chars is the nonce the model is asked to echo.
// Generated per run: a FIXED marker could be satisfied by a cached transcript
// and would pass on a dead binary. Distinct from Claude's prefix so a test
// fixture can never satisfy the wrong provider by accident.
const grokSmokeMarkerPrefix = "AIEXPEDITE_GROK_SMOKE_OK_"

// grokSmokeTimeout bounds the one inference turn. A var, not a const, so a
// test can shrink it and exercise the real deadline-kill path (a killed child
// reports an *exec.ExitError, NOT a wrapped context error — see runGrokSmoke).
var grokSmokeTimeout = 60 * time.Second

// grokSmokeLaunch is everything one probe attempt spawns with. Bundled so the
// exec seam has a single, stable signature and so the Windows shim routing can
// see the whole launch (path + argv + env + cwd) at once.
type grokSmokeLaunch struct {
	Path string
	Args []string
	Env  []string
	Dir  string
	// PromptFile is the staged prompt path — the value of `--prompt-file` in
	// Args. Carried separately so the Windows shim route can hand it to the
	// child through an environment variable rather than interpolating the
	// path into a cmd.exe script line (see grokSmokeShimCommand).
	PromptFile string
}

/* --------------------------------------------------------------------------
   Exec seam
   -------------------------------------------------------------------------- */

// runGrokSmokeCommand spawns the probe. A package-level var so tests drive the
// classification without ever spawning a real binary or spending a turn — the
// same seam shape as runClaudeSmokeCommand.
//
// The prompt is NOT an argument: it is already on disk at launch.PromptFile
// (0600) and reaches the child only through `--prompt-file`. Stdin is left
// closed — Grok's headless mode does not read it.
var runGrokSmokeCommand = func(ctx context.Context, launch grokSmokeLaunch) (stdout, stderr []byte, err error) {
	cmd := newGrokSmokeCmd(ctx, launch)
	outBuf := &boundedBuffer{limit: cliSmokeMaxStdout}
	errBuf := &boundedBuffer{limit: cliSmokeMaxStderr}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// newGrokSmokeCmd builds the child for one attempt. A native binary is spawned
// directly. On Windows a `.cmd` / `.bat` npm shim is routed through cmd.exe
// with an explicit command line (grokSmokeShimCommand), so the shim's re-parse
// cannot re-split or drop a token — the failure class this probe exists to
// remove. Background probe on a tray app with no console of its own: hidden on
// every route, or a console window flashes on the user's desktop.
func newGrokSmokeCmd(ctx context.Context, launch grokSmokeLaunch) *exec.Cmd {
	if cmd, ok := grokSmokeShimCommand(ctx, launch); ok {
		return cmd
	}
	cmd := exec.CommandContext(ctx, launch.Path, launch.Args...)
	cmd.Env = launch.Env
	cmd.Dir = launch.Dir
	hideWindow(cmd)
	return cmd
}

// isGrokWindowsShim reports whether the resolved `grok` is a cmd.exe batch shim
// (what `npm install -g` puts on PATH on Windows) rather than a native binary.
func isGrokWindowsShim(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		return true
	}
	return false
}

/* --------------------------------------------------------------------------
   Resolution + pre-checks (no turn spent)
   -------------------------------------------------------------------------- */

// resolveGrokSmokePath resolves the binary the smoke will probe. Re-resolved
// on every smoke (there is no process-wide Grok path memo to refresh): the
// point of the post-update smoke is to validate the binary that was just
// replaced. Same lookup order as gatherCLIAgents — PATH first, then the
// official installer's bin dir, which GUI/launchd-spawned agents do not have
// on PATH. Returns "" when nothing is installed.
//
// A var so tests can point it at a stub without touching the real install.
var resolveGrokSmokePath = func() string {
	if path, err := exec.LookPath("grok"); err == nil {
		return path
	}
	return resolveGrokInstallerBinary()
}

// grokProbeVersion answers `--version` for a Grok binary under the
// maintenance-only env policy, cached per (path, mtime, size) exactly as the
// CLI detection does — so an upgrade re-probes and a steady-state smoke does
// not spawn a version child.
//
// A Windows npm shim takes the SAME cmd.exe route the inference launch takes:
// CreateProcess cannot start a batch file directly, so probing `grok.cmd`
// with a plain exec.Command answers "" and the smoke reports binary_missing
// without ever exercising the shim-safe route this probe exists to provide.
//
// This is the ONLY Grok version probe: gatherCLIAgents (systemInfo.go) and the
// session_start smoke (session.go) route here too. They must, because the
// version cache is keyed on (path, mtime, size) alone — a probe that launched
// `grok.cmd` directly would cache its own "" under the same key and every
// later shim-aware probe would read that negative back and report
// binary_missing without ever spawning cmd.exe. One route, one answer.
func grokProbeVersion(path string) string {
	env := sanitizeGrokMaintenanceSmokeEnv(os.Environ())
	if isGrokWindowsShim(path) {
		return cachedProbeVersionFunc(path, func() string {
			return grokShimProbeVersion(path, env)
		})
	}
	return cachedProbeVersionWithEnv(path, env)
}

// grokShimProbeVersion runs `<shim> --version` through grokSmokeShimCommand
// — cmd.exe, explicit command line, shim path carried in the environment —
// under the same short probe deadline the machine-info probes use. Returns ""
// on any failure, exactly like probeVersionArgsWithEnv, so the caller's
// binary_missing pre-check is unchanged for a genuinely dead shim.
func grokShimProbeVersion(path string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), machineInfoProbeTimeout)
	defer cancel()
	cmd, ok := grokSmokeShimCommand(ctx, grokSmokeLaunch{
		Path: path,
		Args: []string{"--version"},
		Env:  env,
	})
	if !ok {
		return ""
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return firstNonEmptyLine(out)
}

// grokSmokeLoggedIn is the free login re-check the cooldown replay runs. Same
// classifier the ACP launch pre-flight and the usage card use, against the
// persistent home (the isolated copy the probe spawns with is seeded from it),
// so the replay cannot disagree with them about missing/expired. An unknown
// credential format is INCONCLUSIVE (known=false): the replay proceeds, the
// same way the probe itself proceeds only after assessIsolatedGrokLaunch has
// ruled on the copied login.
func grokSmokeLoggedIn(_ context.Context, _ string) (loggedIn, known bool) {
	assessment := assessGrokAuth(grokPersistentHome(), time.Now(), false, "")
	if assessment.Authenticated {
		return true, true
	}
	if assessment.AuthState == grokAuthStateUnknown {
		return false, false
	}
	return false, true
}

// grokSmokeShapeLadder returns the shapes to try, in the order the installed
// build makes most likely to pass (grokSmokeArgvShapesForVersion). When a
// shape has already been resolved for this exact binary it is the ONLY entry,
// so a steady-state smoke spawns one child rather than walking the ladder.
// The session_start smoke takes its single rung from here too: it cannot walk,
// so the version-ordered first entry IS its resolution of a compatible rung.
func grokSmokeShapeLadder(path, version string) []grokSmokeArgvShape {
	resolved, cached := cliSmokeRememberedShape(path)
	if !cached {
		return grokSmokeArgvShapesForVersion(version)
	}
	for _, shape := range grokSmokeArgvShapes {
		if shape.ID == resolved {
			return []grokSmokeArgvShape{shape}
		}
	}
	return grokSmokeArgvShapes
}

/* --------------------------------------------------------------------------
   Probe
   -------------------------------------------------------------------------- */

// runGrokSmoke performs the probe against an already-resolved binary.
// Registered in cliSmokeProviders (cliagent_smoke.go); split from runCLISmoke
// so the cooldown/catalog concerns stay out of the classification logic.
//
// Order matters for the turn budget: every step up to the spawn is local and
// free, and each failure there is mapped onto a pre-check diagnostic that the
// cooldown deliberately does NOT pin (cliSmokeVerdictSpentTurn).
func runGrokSmoke(ctx context.Context, path, version string) cliSmokeResult {
	result := cliSmokeResult{CliID: "grok", Version: version, Status: cliSmokeStatusFailed}
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
	// Version overrides are part of every managed Grok config layer, so the
	// system-config preflight below needs a version it can compare. An
	// unparseable answer fails closed as `internal` — the CLI is present but
	// this probe cannot evaluate its config posture, which is our gap, not a
	// missing install.
	if normalizeGrokConfigVersion(version) == "" {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}

	// Pre-check 2: GROK_HOME isolation cannot hide xAI's system
	// requirements/managed-config layers. A layer that pins credentials, a
	// permissive approval policy or vendor MCP re-enablement refuses the
	// no-tools smoke — published as `internal`, never as the layer's contents.
	if err := detectGrokMaintenanceSmokeSystemConfig(version); err != nil {
		diagnostic := cliSmokeDiagnosticInternal
		var preflight *grokSmokePreflightError
		if errors.As(err, &preflight) && preflight.Diagnostic != "" {
			diagnostic = preflight.Diagnostic
		}
		fmt.Print(grokSmokeFailureLogLine("", cliUsageErrorInternal, diagnostic, 0))
		return finish(cliUsageErrorInternal, diagnostic)
	}

	// Pre-check 3: the auth-only, MCP-disabled isolated home the child will
	// inherit, plus a fresh empty workspace under it so none of the caller's
	// repository-scoped extensions can participate. Same helpers as the
	// session path; removed exactly once below, through the
	// reconciliation-aware wrapper, after the billing merge.
	persistentHome := grokPersistentHome()
	isolatedHome, err := setupIsolatedGrokSmokeHomeFrom(persistentHome)
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}
	defer cleanupIsolatedGrokHome(isolatedHome, "cli-smoke")
	isolatedCwd := filepath.Join(isolatedHome, "workspace")
	if err := os.Mkdir(isolatedCwd, 0o700); err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}

	// Pre-check 4: a logged-out CLI cannot complete a turn — and Grok's
	// headless fallback is an interactive device-code sign-in the probe could
	// only time out on. Copying auth.json is best-effort, so isolation success
	// is not proof of a usable login; refuse here for free.
	if assessment := assessIsolatedGrokLaunch(isolatedHome, time.Now(), false, ""); !assessment.Authenticated {
		return finish(cliUsageErrorNotAuthenticated, cliSmokeDiagnosticNotLoggedIn)
	}

	marker, err := newGrokSmokeMarker()
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}
	promptFile, err := writeGrokPromptFile(grokSmokePrompt(marker))
	if err != nil {
		return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
	}
	// Removed exactly once, after the last child that could read it has been
	// reaped — including on the deadline-kill path (exec.CommandContext waits
	// for the killed child before Run returns).
	defer func() { _ = os.Remove(promptFile) }()

	// Subscription-only smoke: strip every Grok override and credential the
	// agent inherited, then point HOME/USERPROFILE/PWD at the isolated tree
	// too, because Grok's Claude/Cursor/Codex compatibility loaders discover
	// sources under the OS user home, outside GROK_HOME.
	env, _ := prepareClaudeChildEnv("grok", os.Environ())
	env = sanitizeGrokMaintenanceSmokeEnv(env)
	env = setEnvVar(env, "GROK_HOME", isolatedHome)
	env = setEnvVar(env, "HOME", isolatedHome)
	env = setEnvVar(env, "USERPROFILE", isolatedHome)
	env = setEnvVar(env, "PWD", isolatedCwd)

	// Frozen BEFORE the spawn, from the credential surface the child is
	// spawned with: the same verdict gates the billing merge on exit, resolved
	// once rather than re-derived after the run (see the session path).
	// Bound BEFORE the first spawn: the shape this walk resolves is a fact
	// about THESE bytes, not about whatever is at `path` when it finishes.
	shapeBinding := bindCLISmokeShape(path)
	ladder := grokSmokeShapeLadder(path, version)
	producerContested := grokManagedRunProducerContested(grokDirectRunLaunch{
		Env: env, Cwd: isolatedCwd, Args: buildGrokNoToolsSmokeArgs(ladder[0], promptFile),
	}, isolatedHome)

	spawned := false
	// The billing merge runs after the LAST child exits, whatever the verdict:
	// a rung that reached inference fetched credits into the isolated log even
	// when the model then answered the wrong text, and that observation is
	// exactly what the post-update usage refresh must see.
	defer func() {
		if !spawned {
			return
		}
		outcome, persistErr := persistGrokManagedBillingSnapshot(isolatedHome, persistentHome, producerContested)
		if persistErr != nil {
			fmt.Printf("%s[cli-smoke] grok billing snapshot not persisted (%s): %v%s\n",
				colorYellow, outcome, persistErr, colorReset)
			return
		}
		fmt.Printf("%s[cli-smoke] grok billing snapshot: %s%s\n", colorCyan, outcome, colorReset)
	}()

	var lastCategory, lastDiagnostic string
	for _, shape := range ladder {
		args := buildGrokNoToolsSmokeArgs(shape, promptFile)
		if err := validateGrokSmokeShape(args); err != nil {
			// Cannot happen for a ladder rung; kept so a future edit to the
			// builder that breaks its own contract fails here, spending nothing.
			return finish(cliUsageErrorInternal, cliSmokeDiagnosticInternal)
		}
		runCtx, cancel := context.WithTimeout(ctx, grokSmokeTimeout)
		spawned = true
		stdout, stderr, runErr := runGrokSmokeCommand(runCtx, grokSmokeLaunch{
			Path: path, Args: args, Env: env, Dir: isolatedCwd, PromptFile: promptFile,
		})
		// Read the PER-ATTEMPT context before cancelling it. exec.CommandContext
		// kills the child when its deadline fires and then reports an
		// *exec.ExitError — NOT an error wrapping context.DeadlineExceeded —
		// so judging by the parent ctx would read a timeout as `protocol`.
		timedOut := runCtx.Err() != nil || ctx.Err() != nil
		cancel()

		result.ArgvShapeID = shape.ID

		category, diagnostic, matched := classifyGrokSmokeRun(timedOut, stdout, stderr, runErr, marker)
		if category == "" {
			result.Status = cliSmokeStatusSuccess
			result.MarkerMatched = matched
			result.Diagnostic = diagnostic
			result.DurationMs = time.Since(started).Milliseconds()
			shapeBinding.remember(shape.ID)
			return result
		}
		lastCategory, lastDiagnostic = category, diagnostic
		// Device-local log line: closed values plus the stderr LENGTH — a
		// metric, not content. The child's bytes live no longer than the
		// classifier that read them.
		fmt.Print(grokSmokeFailureLogLine(shape.ID, category, diagnostic, len(stderr)))

		// Retry the next rung ONLY when the CLI positively rejected a flag that
		// rung drops. That rejection happens during option parsing, before any
		// inference, so it is the one failure a retry cannot double-charge for.
		// A rejection naming a flag EVERY rung carries is reported as-is rather
		// than spending a second child to fail identically.
		if diagnostic != cliSmokeDiagnosticFlagRejected || !grokSmokeMentionsRetryableFlag(stderr) {
			break
		}
	}

	result.MarkerMatched = false
	return finish(lastCategory, lastDiagnostic)
}

// grokSmokeFailureLogLine renders the device-local diagnostic line. It takes
// the stderr LENGTH rather than the bytes, deliberately: a function that
// cannot receive vendor text cannot leak it, no matter how a future caller
// wires it up. Every other argument is a value this package defines.
func grokSmokeFailureLogLine(shapeID, category, diagnostic string, stderrBytes int) string {
	return fmt.Sprintf("%s[cli-smoke] grok shape=%s category=%s diagnostic=%s stderrBytes=%d%s\n",
		colorYellow, shapeID, category, diagnostic, stderrBytes, colorReset)
}

/* --------------------------------------------------------------------------
   Classification
   -------------------------------------------------------------------------- */

// grokSmokeStream is what the classifier needs from the child's streaming-json
// stdout: the concatenated assistant text, whether the terminal `end` frame
// arrived, and the first `error` frame's message (read to pick a constant,
// never retained past the classifier).
type grokSmokeStream struct {
	Text         string
	Ended        bool
	ErrorMessage string
	SawError     bool
}

// parseGrokSmokeStream folds the NDJSON frames Grok emits under
// `--output-format=streaming-json`. `text` frames are incremental deltas
// (`text` or, from 1.0.13, `data`) and are concatenated with NO separator —
// the same rule readOutputStream applies for the session smoke, because a
// newline inserted at a frame boundary corrupts the exact marker. Lines that
// are not JSON objects (a banner, an updater notice) are skipped rather than
// treated as protocol failure: the verdict rests on the frames, and the
// prefix-and-loose-decode here is the same tolerance
// parseClaudePrintResultEnvelope applies.
func parseGrokSmokeStream(stdout []byte) grokSmokeStream {
	var stream grokSmokeStream
	var text strings.Builder
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var frame struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Data    string `json:"data"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(line, &frame) != nil {
			continue
		}
		switch frame.Type {
		case "text":
			if frame.Text != "" {
				text.WriteString(frame.Text)
			} else {
				text.WriteString(frame.Data)
			}
		case "end":
			stream.Ended = true
		case "error":
			if !stream.SawError {
				stream.SawError = true
				stream.ErrorMessage = firstNonEmpty(frame.Message, frame.Error, frame.Text, frame.Data)
			}
		}
	}
	stream.Text = text.String()
	return stream
}

// classifyGrokSmokeRun maps one exec outcome onto the closed receipt enum and
// a locally authored diagnostic. Returns ("", …, true) only for an exact marker
// match. Kept pure (no exec, no clock) so every arm is unit-testable;
// `timedOut` is passed in because only the caller can see the per-attempt
// context.
func classifyGrokSmokeRun(timedOut bool, stdout, stderr []byte, runErr error, marker string) (category, diagnostic string, matched bool) {
	// An expired/cancelled attempt outranks whatever the child reported: the
	// kill IS the reason the output is incomplete.
	if timedOut || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
		return cliUsageErrorProviderTimeout, cliSmokeDiagnosticTimeout, false
	}

	stream := parseGrokSmokeStream(stdout)

	// A well-formed error frame is the CLI telling us the turn did not
	// complete; pick between auth and provider by its text, then drop it.
	if stream.SawError {
		lower := strings.ToLower(stream.ErrorMessage)
		switch {
		case grokSmokeTextMentionsAuth(lower):
			return cliUsageErrorNotAuthenticated, cliSmokeDiagnosticAuthError, false
		case strings.Contains(lower, "limit"), strings.Contains(lower, "credit"),
			strings.Contains(lower, "quota"), strings.Contains(lower, "overloaded"),
			strings.Contains(lower, "unavailable"), strings.Contains(lower, "rate"),
			strings.Contains(lower, "api error"), strings.Contains(lower, "status 5"):
			// The CLI and our invocation are both fine; the provider refused.
			return cliUsageErrorProviderUnavailable, cliSmokeDiagnosticProviderError, false
		default:
			return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
		}
	}

	if !stream.Ended {
		if runErr != nil {
			// Exit non-zero with no terminal frame: the CLI rejected our
			// invocation shape before producing its documented output. This
			// is the pre-inference exit the smoke exists to detect.
			return cliUsageErrorProtocol, grokSmokeNoEnvelopeDiagnostic(stderr), false
		}
		// A clean exit that never emitted `end` — an updater notice on stdout,
		// a build whose streaming contract moved — is a broken contract, not a
		// broken model.
		return cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, false
	}

	// A terminal frame whose text is not the marker means the model was
	// chatty ("Sure! …"), NOT that the CLI contract is broken — classifying
	// that as `protocol` would make a healthy CLI look like this regression.
	if strings.TrimSpace(stream.Text) != marker {
		return cliUsageErrorParseFailed, cliSmokeDiagnosticMarkerMismatch, false
	}
	return "", cliSmokeDiagnosticNone, true
}

func grokSmokeTextMentionsAuth(lower string) bool {
	return strings.Contains(lower, "authenticat") || strings.Contains(lower, "login") ||
		strings.Contains(lower, "logged out") || strings.Contains(lower, "credential") ||
		strings.Contains(lower, "unauthorized") || strings.Contains(lower, "sign in") ||
		strings.Contains(lower, "token expired")
}

// grokSmokeNoEnvelopeDiagnostic separates the pre-inference failures that
// must be treated differently once a child exited non-zero with no `end`:
//
//   - framing_rejected — the CLI refused the streaming-json output contract
//     or the prompt-file transport; no rung changes those, so never retried.
//   - flag_rejected — the CLI refused one of OUR options during parsing
//     (clap's `unexpected argument`, `unrecognized`, `unknown option`,
//     `a value is required for`). Retryable only when the rejection names a
//     flag a later rung drops — grokSmokeMentionsRetryableFlag decides that.
//   - no_envelope — anything else; a turn MAY already have been consumed, so
//     the ladder stops here.
//
// The stderr bytes are read only to pick between these constants; not one
// byte of them travels any further.
func grokSmokeNoEnvelopeDiagnostic(stderr []byte) string {
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
	if strings.Contains(lower, "output-format") || strings.Contains(lower, "streaming-json") ||
		strings.Contains(lower, "prompt-file") {
		return cliSmokeDiagnosticFramingRejected
	}
	return cliSmokeDiagnosticFlagRejected
}

// grokSmokeMentionsRetryableFlag reports whether the rejection names a flag
// that a LATER ladder rung actually drops. A rejection of `--tools` or
// `--max-turns` is not retryable — no rung omits those — and treating it as
// such would spawn a second child to fail identically.
func grokSmokeMentionsRetryableFlag(stderr []byte) bool {
	lower := strings.ToLower(string(stderr))
	for _, flag := range grokSmokeRetryableFlags {
		if strings.Contains(lower, flag) {
			return true
		}
	}
	return false
}

// grokSmokePrompt is the one-turn instruction. It names no path, no account
// and no configuration — the marker is the only variable part, and it never
// leaves this process (the published result carries markerMatched, not the
// marker). It uses the same prefix the signed session_start contract requires,
// so both transports ask Grok the same thing.
func grokSmokePrompt(marker string) string {
	return grokMaintenanceSmokePromptPrefix + marker
}

func newGrokSmokeMarker() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return grokSmokeMarkerPrefix + hex.EncodeToString(buf), nil
}

/* --------------------------------------------------------------------------
   Windows .cmd shim route (script rendering — platform-independent)
   -------------------------------------------------------------------------- */

// Environment variables the Windows shim route uses to hand the two PATHS in a
// launch to cmd.exe as data rather than script text. cmd.exe does not use
// CommandLineToArgvW quoting, and a path containing `&`, `(`, `)` or `%` would
// otherwise be re-parsed as script. Same technique grokWindowsJunctionCommand
// uses for its link and target paths.
const (
	grokSmokeShimPathEnv   = "AIEXPEDITE_GROK_SMOKE_SHIM"
	grokSmokeShimPromptEnv = "AIEXPEDITE_GROK_SMOKE_PROMPT_FILE"
)

// grokSmokeShimScript renders the explicit cmd.exe command line for a `.cmd` /
// `.bat` shim launch:
//
//	call "%AIEXPEDITE_GROK_SMOKE_SHIM%" <fixed flags…> --prompt-file "%AIEXPEDITE_GROK_SMOKE_PROMPT_FILE%"
//
// `call` puts a keyword — not a quote — first on the line, which keeps cmd.exe
// from applying its leading/trailing quote-stripping rule to a line that
// carries two quoted operands, and it returns the batch file's exit code. The
// only non-fixed tokens are the two paths, and both travel through the
// environment. Every other token must be one of the shape's own fixed flags —
// a token outside that charset (a space, a quote, a metacharacter) refuses the
// route (ok=false) so the caller falls back to a direct spawn rather than
// interpolating unexpected text into a script line.
func grokSmokeShimScript(args []string) (script string, ok bool) {
	var b strings.Builder
	b.WriteString(`call "%` + grokSmokeShimPathEnv + `%"`)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == grokSmokePromptFileFlag && i+1 < len(args) {
			b.WriteString(" " + grokSmokePromptFileFlag + ` "%` + grokSmokeShimPromptEnv + `%"`)
			i++
			continue
		}
		if !grokSmokeFixedFlagToken(arg) {
			return "", false
		}
		b.WriteString(" " + arg)
	}
	return b.String(), true
}

// grokSmokeFixedFlagToken accepts the flag vocabulary the ladder emits —
// `--name`, `--name=value` with alphanumerics, `-`, `=` — and nothing that
// cmd.exe could read as script.
func grokSmokeFixedFlagToken(arg string) bool {
	if !strings.HasPrefix(arg, "--") {
		return false
	}
	for _, r := range arg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '=':
		default:
			return false
		}
	}
	return true
}
