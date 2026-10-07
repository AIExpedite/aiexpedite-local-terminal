package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_opencode_test.go
   --------------------------------------------------------------------------
   Every case drives the exec seam, so no test spawns a real `opencode` or
   spends an inference turn. The invariants worth stating up front:
     - launch_error (we could not START the child) NEVER shares a bucket with
       no_envelope (the child ran and produced no terminal frame) — that fused
       bucket is the reported symptom;
     - framing_rejected is never inferred from SILENCE, only from a positive
       rejection naming the output contract;
     - partial output (a malformed frame, a frame past the cap, stdout past the
       retention cap) is never compared against the marker;
     - the pre-checks short-circuit BEFORE the seam runs, so no quota is spent
       on a device with no binary or a conclusive "no usable provider";
     - nothing the CLI authored reaches the published result or the device log.
   ------------------------------------------------------------------------ */

// openCodeSmokeEnv isolates every on-disk side channel an OpenCode smoke touches
// (the prompt/cwd scratch dirs under the user home) and clears the in-memory
// caches so cases cannot leak into each other.
func openCodeSmokeEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resetCLISmokeState()
	resetVersionProbeCache()
	resetOpenCodeVersionNegatives()
	resetOpenCodeReadinessCache()
	t.Cleanup(func() {
		resetCLISmokeState()
		resetVersionProbeCache()
		resetOpenCodeVersionNegatives()
		resetOpenCodeReadinessCache()
	})
}

// stubOpenCodeBinary writes a fake binary and returns its path. It is never
// executed — the exec seam and the version probe are both stubbed — so it
// carries no .exe extension: nothing should be able to run it by accident.
func stubOpenCodeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubOpenCodeSmokePath(t *testing.T, path string) {
	t.Helper()
	original := resolveOpenCodeSmokePath
	resolveOpenCodeSmokePath = func() string { return path }
	t.Cleanup(func() { resolveOpenCodeSmokePath = original })
}

// stubOpenCodeSmokeExec replaces the exec seam with a scripted responder,
// records every launch, and returns pointers to the call count and the launches.
// The scripted child always reached Start; stubOpenCodeSmokeExecUnstarted is
// the pre-spawn variant.
func stubOpenCodeSmokeExec(t *testing.T, fn func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error)) (*int, *[]openCodeLaunch) {
	t.Helper()
	return stubOpenCodeSmokeExecStarted(t, true, fn)
}

// stubOpenCodeSmokeExecUnstarted scripts a seam that failed BEFORE Start, the
// way a missing binary or a refused prompt handle does.
func stubOpenCodeSmokeExecUnstarted(t *testing.T, fn func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error)) (*int, *[]openCodeLaunch) {
	t.Helper()
	return stubOpenCodeSmokeExecStarted(t, false, fn)
}

func stubOpenCodeSmokeExecStarted(t *testing.T, started bool, fn func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error)) (*int, *[]openCodeLaunch) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	var launches []openCodeLaunch
	original := runOpenCodeSmokeCommand
	runOpenCodeSmokeCommand = func(ctx context.Context, launch openCodeLaunch) ([]byte, []byte, bool, error) {
		mu.Lock()
		calls++
		launches = append(launches, launch)
		mu.Unlock()
		stdout, stderr, err := fn(ctx, launch)
		return stdout, stderr, started, err
	}
	t.Cleanup(func() { runOpenCodeSmokeCommand = original })
	return &calls, &launches
}

// openCodeMarkerFromLaunch recovers the nonce the probe generated for this run
// from the staged prompt file — the ONLY place it appears, which is the point.
func openCodeMarkerFromLaunch(t *testing.T, launch openCodeLaunch) string {
	t.Helper()
	body, err := os.ReadFile(launch.PromptFile)
	if err != nil {
		t.Fatalf("prompt file must exist while the child runs: %v", err)
	}
	prompt := string(body)
	if !strings.HasPrefix(prompt, openCodeMaintenanceSmokePromptPrefix) {
		t.Fatalf("prompt %q does not start with the smoke prefix", prompt)
	}
	return strings.TrimSpace(strings.TrimPrefix(prompt, openCodeMaintenanceSmokePromptPrefix))
}

// openCodeSuccessFrames is what `--format json` emits on a healthy turn: the
// assistant text split across incremental deltas, then a completion event. The
// split is deliberate — a classifier that inserted a separator at a frame
// boundary would corrupt the exact marker.
func openCodeSuccessFrames(marker string) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"session.started","sessionID":"ses_probe"}` + "\n")
	third := len(marker) / 3
	for _, delta := range []string{marker[:third], marker[third : 2*third], marker[2*third:]} {
		line, _ := json.Marshal(map[string]string{"type": "text", "text": delta})
		b.Write(line)
		b.WriteString("\n")
	}
	b.WriteString(`{"type":"session.completed"}` + "\n")
	return []byte(b.String())
}

// openCodeExitError is the shared killedChildError (cliagent_smoke_claudecode_test.go)
// under a provider-local name, exactly as codexExitError is. It spawns no shell
// of its own: a third hand-rolled copy of "manufacture an *exec.ExitError" is a
// third thing to fix when the trick stops working on a platform.
func openCodeExitError(t *testing.T) error {
	t.Helper()
	return killedChildError(t)
}

// stubOpenCodeReadiness pins the readiness pre-check so a case never spawns the
// `opencode models` child. `known=false` (inconclusive) is the default the real
// probe returns for an unreachable binary, and it PROCEEDS.
func stubOpenCodeReadiness(t *testing.T, out string, ok bool) {
	t.Helper()
	original := runOpenCodeProbe
	runOpenCodeProbe = func(context.Context, string, string, ...string) (string, bool) { return out, ok }
	t.Cleanup(func() { runOpenCodeProbe = original })
}

/* --------------------------------------------------------------------------
   Classification
   -------------------------------------------------------------------------- */

func TestClassifyOpenCodeSmokeRun_MapsEveryOutcomeOntoTheClosedSet(t *testing.T) {
	const marker = "AIEXPEDITE_OPENCODE_SMOKE_OK_0a1b2c3d"
	exitErr := openCodeExitError(t)

	cases := []struct {
		name           string
		timedOut       bool
		stdout, stderr string
		runErr         error
		wantCategory   string
		wantDiagnostic string
		wantMatched    bool
	}{
		{
			name:           "exact marker across deltas succeeds",
			stdout:         string(openCodeSuccessFrames(marker)),
			wantDiagnostic: cliSmokeDiagnosticNone,
			wantMatched:    true,
		},
		{
			name:           "a chatty model is a mismatch, not a broken contract",
			stdout:         `{"type":"text","text":"Sure! ` + marker + `"}` + "\n" + `{"type":"session.completed"}`,
			wantCategory:   cliUsageErrorParseFailed,
			wantDiagnostic: cliSmokeDiagnosticMarkerMismatch,
		},
		{
			name:           "a rejected flag is named, not fused",
			stderr:         "error: unknown option '--pure'",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:           "a rejected output contract is framing, not flag",
			stderr:         "error: unrecognized value 'json' for --format",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFramingRejected,
		},
		{
			name:           "a rejected output contract printed on stdout is still framing",
			stdout:         "error: unknown option '--format'\n",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFramingRejected,
		},
		{
			name:           "a rejected flag printed on stdout is still flag_rejected",
			stdout:         "error: unexpected argument '--pure' found\n\nUsage: opencode run [OPTIONS]\n      --format <FORMAT>\n",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:           "model text in a JSON event never reads as an option rejection",
			stdout:         `{"type":"text","text":"unknown option --format"}` + "\n",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "a generic non-zero exit with no frame is no_output",
			stderr:         "something went wrong deep inside",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoOutput,
		},
		{
			name:           "a generic non-zero exit after frames is no_envelope",
			stdout:         `{"type":"step_start"}` + "\n",
			stderr:         "something went wrong deep inside",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "an empty clean exit is no_output",
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoOutput,
		},
		{
			name:           "banner-only stdout is no_output",
			stdout:         "opencode 0.9.1 — a new version is available\n",
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoOutput,
		},
		{
			name:           "frames with no terminal frame are no_envelope",
			stdout:         `{"type":"step_start"}` + "\n" + `{"type":"text","text":"` + marker + `"}` + "\n",
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "a frameless flag rejection keeps flag_rejected over no_output",
			stderr:         "error: unknown option '--pure'",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:           "a frameless framing rejection keeps framing_rejected over no_output",
			stderr:         "error: unknown option '--format'",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticFramingRejected,
		},
		{
			name:           "a timeout still outranks silence",
			timedOut:       true,
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProviderTimeout,
			wantDiagnostic: cliSmokeDiagnosticTimeout,
		},
		{
			name:           "a title-escape-prefixed stream with no parsable frame is no_output",
			stdout:         "\x1b]0;opencode\x07" + `{"type":"step_finish","part":{"reason":"stop"}}` + "\n",
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoOutput,
		},
		{
			name:           "a clean exit with no terminal frame is no_envelope, never framing",
			stdout:         `{"type":"text","text":"` + marker + `"}`,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "malformed JSON is no_envelope and is never marker-matched",
			stdout:         `{"type":"text","text":"` + marker + "\n" + `{"type":"session.completed"}`,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "a banner line is skipped rather than treated as a failure",
			stdout:         "opencode 0.9.1 — starting\n" + string(openCodeSuccessFrames(marker)),
			wantDiagnostic: cliSmokeDiagnosticNone,
			wantMatched:    true,
		},
		{
			name:           "an auth error event is auth_error",
			stdout:         `{"type":"error","error":{"message":"authentication failed: token expired"}}`,
			wantCategory:   cliUsageErrorNotAuthenticated,
			wantDiagnostic: cliSmokeDiagnosticAuthError,
		},
		{
			name:           "a provider refusal event is provider_error",
			stdout:         `{"type":"session.error","error":"usage limit reached for this quota window"}`,
			wantCategory:   cliUsageErrorProviderUnavailable,
			wantDiagnostic: cliSmokeDiagnosticProviderError,
		},
		{
			name:           "an unclassifiable error event is no_envelope",
			stdout:         `{"type":"error","error":"the sky fell"}`,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:           "the attempt deadline outranks whatever the child reported",
			timedOut:       true,
			stdout:         string(openCodeSuccessFrames(marker)),
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProviderTimeout,
			wantDiagnostic: cliSmokeDiagnosticTimeout,
		},
		{
			name:           "a cancelled parent is a timeout",
			runErr:         context.Canceled,
			wantCategory:   cliUsageErrorProviderTimeout,
			wantDiagnostic: cliSmokeDiagnosticTimeout,
		},
		{
			name:           "a spawn failure is launch_error, NOT no_envelope",
			runErr:         errOpenCodeShimUnrenderable,
			wantCategory:   cliUsageErrorProviderUnavailable,
			wantDiagnostic: cliSmokeDiagnosticLaunchError,
		},
		{
			name:           "CreateProcess refusing a .cmd shim is launch_error",
			runErr:         &exec.Error{Name: "opencode.cmd", Err: errors.New("exec format error")},
			wantCategory:   cliUsageErrorProviderUnavailable,
			wantDiagnostic: cliSmokeDiagnosticLaunchError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			category, diagnostic, matched := classifyOpenCodeSmokeRun(
				tc.timedOut, []byte(tc.stdout), []byte(tc.stderr), tc.runErr, marker)
			if category != tc.wantCategory || diagnostic != tc.wantDiagnostic || matched != tc.wantMatched {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)",
					category, diagnostic, matched, tc.wantCategory, tc.wantDiagnostic, tc.wantMatched)
			}
		})
	}
}

// The frame and escape-line counts are what the device log line reports for a
// field failure, so they must count exactly what the classifier saw.
func TestParseOpenCodeSmokeStream_CountsFramesAndEscapeLines(t *testing.T) {
	stdout := "banner\n" +
		"\x1b]0;opencode\x07\n" +
		"\xef\xbb\xbf" + `{"type":"text","text":"hidden"}` + "\n" +
		`{"type":"step_start"}` + "\n" +
		`{"type":"text","text":"hi"}` + "\n" +
		`{"type":"text","text":` + "\n" + // malformed: not a frame
		`{"type":"step_finish","part":{"reason":"stop"}}` + "\n"
	stream := parseOpenCodeSmokeStream([]byte(stdout))
	if stream.Frames != 3 || stream.EscLines != 2 || !stream.Ended || !stream.Malformed {
		t.Fatalf("got frames=%d escLines=%d ended=%v malformed=%v, want 3/2/true/true",
			stream.Frames, stream.EscLines, stream.Ended, stream.Malformed)
	}

	counts := openCodeSmokeCountsFor([]byte(stdout), []byte("err"), openCodeExitError(t))
	if counts.Frames != 3 || counts.EscLines != 2 || !counts.Terminal ||
		counts.StdoutBytes != len(stdout) || counts.StderrBytes != 3 || counts.Exit == 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if got := openCodeSmokeExitCode(nil); got != 0 {
		t.Errorf("clean exit code = %d, want 0", got)
	}
	if got := openCodeSmokeExitCode(errOpenCodeShimUnrenderable); got != -1 {
		t.Errorf("launch failure exit code = %d, want -1", got)
	}
}

// A completion event whose `message` is a plain STRING must not read as
// malformed. openCodeEvent (the shared reader) types that field as an object,
// and OpenCode spells it both ways across releases — so treating the shared
// reader's decode failure as "broken contract" reported a HEALTHY turn as
// no_envelope.
func TestClassifyOpenCodeSmokeRun_AStringMessageFieldIsNotMalformed(t *testing.T) {
	const marker = "AIEXPEDITE_OPENCODE_SMOKE_OK_0a1b2c3d"
	const completion = `{"type":"session.completed","message":"done"}`
	stdout := `{"type":"text","text":"` + marker + `"}` + "\n" + completion + "\n"

	// Guard the premise: the shared reader really does refuse this line, so this
	// case exercises the divergence rather than a shape both readers accept.
	if _, _, parsed := parseOpenCodeEventLine(completion); parsed {
		t.Skip("parseOpenCodeEventLine now tolerates a string `message`; the divergence is gone")
	}

	category, diagnostic, matched := classifyOpenCodeSmokeRun(false, []byte(stdout), nil, nil, marker)
	if category != "" || diagnostic != cliSmokeDiagnosticNone || !matched {
		t.Fatalf("got (%q, %q, %v), want a clean marker success", category, diagnostic, matched)
	}
}

// The `run --format json` formatter closes each model step with `step_finish`
// (part.type `step-finish`, part.reason from the AI SDK), and a turn that calls
// a tool emits one per step. A final step_finish must end the stream; an
// intermediate one with a tool-call reason must not, or partial text would be
// judged as the whole reply. Frames follow the formatter's captured shape.
func TestClassifyOpenCodeSmokeRun_StepFinishClosesTheTurnUnlessAToolCallContinues(t *testing.T) {
	const marker = "AIEXPEDITE_OPENCODE_SMOKE_OK_0a1b2c3d"
	stepStart := `{"type":"step_start","timestamp":1790500000000,"sessionID":"ses_probe","part":{"id":"prt_1","sessionID":"ses_probe","messageID":"msg_1","type":"step-start"}}`
	text := `{"type":"text","timestamp":1790500000100,"sessionID":"ses_probe","part":{"id":"prt_2","sessionID":"ses_probe","messageID":"msg_1","type":"text","text":"` + marker + `","time":{"start":1790500000050,"end":1790500000100}}}`
	finish := func(reason string) string {
		return `{"type":"step_finish","timestamp":1790500000200,"sessionID":"ses_probe","part":{"id":"prt_3","sessionID":"ses_probe","messageID":"msg_1","type":"step-finish","reason":"` + reason + `","cost":0,"tokens":{"input":12,"output":9,"reasoning":0,"cache":{"read":0,"write":0}}}}`
	}

	stdout := stepStart + "\n" + text + "\n" + finish("stop") + "\n"
	category, diagnostic, matched := classifyOpenCodeSmokeRun(false, []byte(stdout), nil, nil, marker)
	if category != "" || diagnostic != cliSmokeDiagnosticNone || !matched {
		t.Fatalf("step_finish(stop): got (%q, %q, %v), want a clean marker success", category, diagnostic, matched)
	}

	// Only a tool-call step finished; the process then exited without the
	// closing step — no terminal frame was seen.
	stdout = stepStart + "\n" + text + "\n" + finish("tool-calls") + "\n"
	if _, diagnostic, matched = classifyOpenCodeSmokeRun(false, []byte(stdout), nil, nil, marker); matched || diagnostic != cliSmokeDiagnosticNoEnvelope {
		t.Fatalf("step_finish(tool-calls): got (%q, %v), want no_envelope", diagnostic, matched)
	}

	// The session stream agrees frame-for-frame.
	if !detectCLITerminalEvent("opencode", finish("stop")) {
		t.Error("session detection must close the turn on step_finish(stop)")
	}
	if detectCLITerminalEvent("opencode", finish("tool-calls")) {
		t.Error("session detection must not close the turn on a tool-call step_finish")
	}
}

// framing_rejected must be named by the FLAG, not by a bare `json` substring:
// OpenCode prints a usage block on an option error and that block lists
// `--format json`, so a substring match reported every rejected caller flag as a
// broken framing contract.
func TestOpenCodeSmokeNoEnvelopeDiagnostic_UsageBlockDoesNotForgeFramingRejected(t *testing.T) {
	usage := "error: unexpected argument '--not-a-known-flag' found\n\n" +
		"Usage: opencode run [OPTIONS] [PROMPT]\n\nOptions:\n" +
		"      --format <FORMAT>  Output format [possible values: text, json]\n"
	if got := openCodeSmokeNoEnvelopeDiagnostic(nil, []byte(usage)); got != cliSmokeDiagnosticFlagRejected {
		t.Fatalf("a rejected caller flag whose usage block mentions json = %q, want flag_rejected", got)
	}
	// A genuine refusal of the output contract still reports framing_rejected.
	for _, stderr := range []string{
		"error: unknown option '--format'",
		"error: unrecognized value 'json' for --format",
	} {
		if got := openCodeSmokeNoEnvelopeDiagnostic(nil, []byte(stderr)); got != cliSmokeDiagnosticFramingRejected {
			t.Fatalf("%q = %q, want framing_rejected", stderr, got)
		}
	}
	// And a non-rejection stays no_envelope.
	if got := openCodeSmokeNoEnvelopeDiagnostic(nil, []byte("panic: nil map")); got != cliSmokeDiagnosticNoEnvelope {
		t.Fatalf("a non-rejection = %q, want no_envelope", got)
	}
}

// exec.ErrWaitDelay means "the child RAN and we stopped waiting for its I/O",
// not "we could not start it". A lingering OpenCode TOOL GRANDCHILD holding the
// captured pipe past WaitDelay is the realistic cause, and judging it a launch
// failure would report a healthy install as provider_unavailable, throw away
// stdout that holds the marker, and — because launch_error is never cached —
// re-spend a turn on every smoke. That is the fused bucket this probe exists to
// separate, reintroduced by the WaitDelay the deadline path needs.
func TestClassifyOpenCodeSmokeRun_WaitDelayExpiryStillClassifiesTheOutput(t *testing.T) {
	const marker = "AIEXPEDITE_OPENCODE_SMOKE_OK_0a1b2c3d"
	for _, tc := range []struct {
		name           string
		stdout, stderr string
		wantCategory   string
		wantDiagnostic string
		wantMatched    bool
	}{
		{
			name:           "a complete marker echo is a success",
			stdout:         string(openCodeSuccessFrames(marker)),
			wantDiagnostic: cliSmokeDiagnosticNone,
			wantMatched:    true,
		},
		{
			name:           "a chatty reply is still a mismatch",
			stdout:         `{"type":"text","text":"Sure!"}` + "\n" + `{"type":"session.completed"}`,
			wantCategory:   cliUsageErrorParseFailed,
			wantDiagnostic: cliSmokeDiagnosticMarkerMismatch,
		},
		{
			// No terminal frame: the child ran and produced no envelope. It must
			// NOT be read as a pre-inference flag rejection, because there was no
			// non-zero exit reporting one — the stderr text is incidental.
			name:           "no terminal frame is no_envelope, never a flag rejection",
			stdout:         `{"type":"text","text":"partial"}`,
			stderr:         "error: unknown option '--format'",
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			category, diagnostic, matched := classifyOpenCodeSmokeRun(
				false, []byte(tc.stdout), []byte(tc.stderr), exec.ErrWaitDelay, marker)
			if diagnostic == cliSmokeDiagnosticLaunchError {
				t.Fatalf("a WaitDelay expiry was judged a launch failure (%q)", category)
			}
			if category != tc.wantCategory || diagnostic != tc.wantDiagnostic || matched != tc.wantMatched {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", category, diagnostic, matched,
					tc.wantCategory, tc.wantDiagnostic, tc.wantMatched)
			}
		})
	}

	// A WRAPPED ErrWaitDelay (os/exec may return it joined with other context)
	// must be recognised too, so the check cannot be defeated by wrapping.
	category, diagnostic, matched := classifyOpenCodeSmokeRun(false,
		openCodeSuccessFrames(marker), nil, fmt.Errorf("waiting on opencode: %w", exec.ErrWaitDelay), marker)
	if category != "" || diagnostic != cliSmokeDiagnosticNone || !matched {
		t.Fatalf("a wrapped ErrWaitDelay: got (%q, %q, %v), want a clean success",
			category, diagnostic, matched)
	}

	// And the deadline still outranks it: a killed run says nothing about output.
	if _, diagnostic, _ := classifyOpenCodeSmokeRun(true,
		openCodeSuccessFrames(marker), nil, exec.ErrWaitDelay, marker); diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("a timed-out run reporting ErrWaitDelay = %q, want timeout", diagnostic)
	}
}

func TestClassifyOpenCodeSmokeRun_OversizeOutputIsNeverMarkerMatched(t *testing.T) {
	const marker = "AIEXPEDITE_OPENCODE_SMOKE_OK_0a1b2c3d"

	// A single frame past the frame cap: bufio reports ErrTooLong, so the reply
	// cannot be trusted as complete even though a completion event follows.
	huge := `{"type":"text","text":"` + strings.Repeat("a", openCodeNativeMaxFrameBytes+16) + `"}` + "\n" +
		`{"type":"session.completed"}` + "\n"
	category, diagnostic, matched := classifyOpenCodeSmokeRun(false, []byte(huge), nil, nil, marker)
	if diagnostic != cliSmokeDiagnosticNoEnvelope || matched {
		t.Fatalf("oversize frame: got (%q, %q, %v), want no_envelope", category, diagnostic, matched)
	}

	// Stdout past the retention cap. Driven through the REAL capture buffer the
	// exec seam uses, not a hand-built oversize slice: boundedBuffer stops AT its
	// limit, so a detection that summed scanned LINE lengths could never fire on
	// a genuine capture (newlines are dropped, putting that sum strictly below the
	// byte total) and the blindness was invisible to a test that fed the
	// classifier an over-limit slice directly.
	captured := &boundedBuffer{limit: cliSmokeMaxStdout}
	for len(captured.Bytes()) < cliSmokeMaxStdout {
		captured.Write([]byte(`{"type":"text","text":"` + strings.Repeat("b", 4096) + `"}` + "\n"))
	}
	// The dropped tail is what would otherwise be believed: a marker echo and a
	// completion event the caller never actually received in full.
	captured.Write([]byte(`{"type":"text","text":"` + marker + `"}` + "\n"))
	captured.Write([]byte(`{"type":"session.completed"}` + "\n"))
	if len(captured.Bytes()) != cliSmokeMaxStdout {
		t.Fatalf("the capture buffer must stop at its limit, got %d bytes", len(captured.Bytes()))
	}
	category, diagnostic, matched = classifyOpenCodeSmokeRun(false, captured.Bytes(), nil, nil, marker)
	if diagnostic != cliSmokeDiagnosticNoEnvelope || matched {
		t.Fatalf("stdout cap overflow: got (%q, %q, %v), want no_envelope", category, diagnostic, matched)
	}

	// The genuinely dangerous shape: a truncated capture whose retained bytes end
	// in an INTACT marker echo and completion event, so every content check would
	// read a clean success. The reply was still incomplete, so the marker proves
	// nothing about this turn and only the truncation signal can refuse it. The
	// padding ends in a newline so the marker frame really is its own scanned
	// line rather than being swallowed by a partial one.
	tail := `{"type":"text","text":"` + marker + `"}` + "\n" + `{"type":"session.completed"}` + "\n"
	padding := strings.Repeat("x", cliSmokeMaxStdout-len(tail)-1) + "\n"
	forged := &boundedBuffer{limit: cliSmokeMaxStdout}
	forged.Write([]byte(padding))
	forged.Write([]byte(tail))
	if len(forged.Bytes()) != cliSmokeMaxStdout {
		t.Fatalf("the forged capture must fill the cap exactly, got %d", len(forged.Bytes()))
	}
	// Prove the premise: with the truncation signal ignored, this capture reads
	// as a clean marker success — which is exactly what must not be published.
	if stream := parseOpenCodeSmokeStream(forged.Bytes()); !stream.Ended ||
		strings.TrimSpace(stream.Text) != marker {
		t.Fatalf("the forged capture must look like a success on content alone "+
			"(ended=%v text=%q)", stream.Ended, stream.Text)
	}
	category, diagnostic, matched = classifyOpenCodeSmokeRun(false, forged.Bytes(), nil, nil, marker)
	if diagnostic != cliSmokeDiagnosticNoEnvelope || matched {
		t.Fatalf("a truncated capture ending in the marker: got (%q, %q, %v), want no_envelope",
			category, diagnostic, matched)
	}
}

/* --------------------------------------------------------------------------
   Pre-checks (no turn spent)
   -------------------------------------------------------------------------- */

func TestRunOpenCodeSmoke_MissingBinaryOrVersionSpendsNothing(t *testing.T) {
	openCodeSmokeEnv(t)
	calls, _ := stubOpenCodeSmokeExec(t, func(context.Context, openCodeLaunch) ([]byte, []byte, error) {
		t.Fatal("the seam must not run when the binary is unusable")
		return nil, nil, nil
	})

	for _, tc := range []struct{ path, version string }{
		{"", "0.9.1"},                            // nothing resolved
		{filepath.Join(t.TempDir(), "gone"), ""}, // unstattable path, no version
		{stubOpenCodeBinary(t), ""},              // present but cannot answer --version
	} {
		result := runOpenCodeSmoke(context.Background(), tc.path, tc.version)
		if result.Diagnostic != cliSmokeDiagnosticBinaryMissing {
			t.Fatalf("path=%q version=%q → %q, want binary_missing", tc.path, tc.version, result.Diagnostic)
		}
		if result.ErrorCategory != cliUsageErrorProviderUnavailable {
			t.Fatalf("path=%q → category %q", tc.path, result.ErrorCategory)
		}
		if cliSmokeVerdictSpentTurn(result) {
			t.Fatalf("binary_missing must not be cached as a spent turn")
		}
	}
	if *calls != 0 {
		t.Fatalf("pre-checks spawned %d children", *calls)
	}
}

func TestRunOpenCodeSmoke_ConclusiveNoProviderSpendsNoTurn(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	// Positive evidence of no usable provider — the only negative the fail-open
	// readiness design admits.
	stubOpenCodeReadiness(t, "No providers configured. Run `opencode auth login`.", true)
	calls, _ := stubOpenCodeSmokeExec(t, func(context.Context, openCodeLaunch) ([]byte, []byte, error) {
		t.Fatal("a conclusively unusable install must not spend a turn")
		return nil, nil, nil
	})

	result := runOpenCodeSmoke(context.Background(), path, "0.9.1")
	if result.Diagnostic != cliSmokeDiagnosticNotLoggedIn ||
		result.ErrorCategory != cliUsageErrorNotAuthenticated {
		t.Fatalf("got (%q, %q), want not_logged_in", result.ErrorCategory, result.Diagnostic)
	}
	if cliSmokeVerdictSpentTurn(result) {
		t.Error("not_logged_in must not be cached as a spent turn")
	}
	if *calls != 0 {
		t.Fatalf("readiness refusal spawned %d children", *calls)
	}
}

// A `--version` reading can fail for a reason the bytes on disk do not explain —
// a spawn that lost a race with an installer, a probe deadline missed under load.
// The shared cache keys on (path, mtime, size) alone, so pinning that failure
// would keep every later caller (notably probeOpenCodeNativeCapability, whose own
// negative window is 30 seconds) reading the same empty string until the agent
// restarts. The negative must therefore expire — and be REUSED until it does, so
// a reliably dead binary is not re-spawned on every gather.
func TestOpenCodeProbeVersion_LetsATransientFailureExpire(t *testing.T) {
	openCodeSmokeEnv(t)
	path := buildOpenCodeStub(t)

	// Stand in for the transient fault: the shared cache holds "" for this exact
	// binary, exactly as a failed probe would have left it.
	if got := cachedProbeVersionFunc(path, func() string { return "" }); got != "" {
		t.Fatalf("seeded reading = %q, want the empty failure", got)
	}
	openCodeNoteVersionReading(path, "")

	if got := openCodeProbeVersion(path); got != "" {
		t.Fatalf("inside the negative window the cached failure must be reused, got %q", got)
	}

	backdateOpenCodeVersionNegative(path, openCodeVersionNegativeTTL+time.Second)
	got := openCodeProbeVersion(path)
	if got == "" {
		t.Fatal("an expired negative must re-probe; the binary answers --version")
	}
	// And the recovery sticks: the positive is now what the shared cache holds.
	if cached, ok := lookupCachedProbeVersion(path); !ok || cached != got {
		t.Fatalf("cached reading = (%q, %v), want the recovered version %q", cached, ok, got)
	}
}

// The readiness pre-check has to answer for the SAME configuration the turn
// runs under: OpenCode resolves a project `opencode.json` upward from cwd, so a
// pre-check run in the agent's own directory can pass on a provider the isolated
// turn cannot see (and a project override can report a premature not_logged_in).
func TestRunOpenCodeSmoke_ReadinessPrecheckRunsInTheRunDirectory(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)

	var mu sync.Mutex
	var probeDirs []string
	originalProbe := runOpenCodeProbe
	runOpenCodeProbe = func(_ context.Context, _ string, dir string, _ ...string) (string, bool) {
		mu.Lock()
		probeDirs = append(probeDirs, dir)
		mu.Unlock()
		return "anthropic/claude-sonnet-4-5\n", true
	}
	t.Cleanup(func() { runOpenCodeProbe = originalProbe })

	_, launches := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result := runOpenCodeSmoke(context.Background(), path, "0.9.1"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("stub turn must succeed: %+v", result)
	}
	mu.Lock()
	dirs := append([]string(nil), probeDirs...)
	mu.Unlock()
	if len(dirs) == 0 {
		t.Fatal("the readiness pre-check never ran")
	}
	turnDir := (*launches)[0].Dir
	if turnDir == "" {
		t.Fatal("the turn must run in an isolated directory")
	}
	for _, dir := range dirs {
		if dir != turnDir {
			t.Fatalf("readiness probe ran in %q, want the turn's directory %q", dir, turnDir)
		}
	}
}

func TestRunOpenCodeSmoke_InconclusiveReadinessProceeds(t *testing.T) {
	// A local-model install with no credential anywhere is perfectly usable, and
	// an unreachable probe says nothing. Both must fall through to the turn.
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "", false)
	calls, launches := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	result := runOpenCodeSmoke(context.Background(), path, "0.9.1")
	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
		t.Fatalf("inconclusive readiness must proceed: %+v", result)
	}
	if *calls != 1 {
		t.Fatalf("expected exactly one child, got %d", *calls)
	}
	if got := (*launches)[0].Args; strings.Join(got, " ") !=
		strings.Join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), " ") {
		t.Fatalf("the probe must spawn the no-resume rung, got %q", got)
	}
	if result.ArgvShapeID != openCodeRunShapeIDPlain {
		t.Fatalf("shape id = %q, want %q", result.ArgvShapeID, openCodeRunShapeIDPlain)
	}
}

func TestRunOpenCodeSmoke_NeverRetriesAndKeepsTheMarkerOffArgv(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "", false)
	var seenPrompt, seenDir string
	calls, launches := stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		body, err := os.ReadFile(launch.PromptFile)
		if err != nil {
			t.Fatalf("prompt file missing: %v", err)
		}
		seenPrompt = string(body)
		seenDir = launch.Dir
		// A flag rejection: the probe has nothing droppable, so it must NOT walk.
		return nil, []byte("error: unknown option '--format'"), openCodeExitError(t)
	})

	result := runOpenCodeSmoke(context.Background(), path, "0.9.1")
	if result.Diagnostic != cliSmokeDiagnosticFramingRejected {
		t.Fatalf("diagnostic = %q, want framing_rejected", result.Diagnostic)
	}
	if *calls != 1 {
		t.Fatalf("the probe retried: %d children", *calls)
	}
	// The probe is a maintenance launch, so newOpenCodeCmd pins self-update and
	// the terminal title off for its child (TestNewOpenCodeCmd_MaintenancePins…).
	if !(*launches)[0].Maintenance {
		t.Fatal("the probe's launch must be marked Maintenance")
	}

	marker := strings.TrimPrefix(seenPrompt, openCodeMaintenanceSmokePromptPrefix)
	if marker == seenPrompt || !strings.HasPrefix(marker, openCodeSmokeMarkerPrefix) {
		t.Fatalf("prompt %q is not the reserved marker sentence", seenPrompt)
	}
	for _, a := range (*launches)[0].Args {
		if strings.Contains(a, marker) || strings.Contains(a, openCodeMaintenanceSmokePromptPrefix) {
			t.Fatalf("argv %q carries the prompt or its nonce", (*launches)[0].Args)
		}
	}
	// The per-run cwd and prompt file are removed on every path — a project
	// `opencode.json` in the caller's workspace must never decide what the probe
	// measures, and the nonce must not outlive the run.
	if seenDir == "" {
		t.Fatal("the probe must run in its own empty cwd")
	}
	if _, err := os.Stat(seenDir); !os.IsNotExist(err) {
		t.Errorf("per-run cwd %q survived the probe (err=%v)", seenDir, err)
	}
	if _, err := os.Stat((*launches)[0].PromptFile); !os.IsNotExist(err) {
		t.Errorf("prompt file survived the probe (err=%v)", err)
	}
}

func TestRunOpenCodeSmoke_MarkerIsFreshPerRun(t *testing.T) {
	// A FIXED marker could be satisfied by a cached transcript and would pass on
	// a dead binary.
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "", false)
	var markers []string
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		marker := openCodeMarkerFromLaunch(t, launch)
		markers = append(markers, marker)
		return openCodeSuccessFrames(marker), nil, nil
	})
	for i := 0; i < 2; i++ {
		runOpenCodeSmoke(context.Background(), path, "0.9.1")
	}
	if len(markers) != 2 || markers[0] == markers[1] {
		t.Fatalf("markers must differ per run, got %q", markers)
	}
}

/* --------------------------------------------------------------------------
   Shape cache
   -------------------------------------------------------------------------- */

func TestRunOpenCodeSmoke_ShapeIsRememberedOnlyForTheProbedBinary(t *testing.T) {
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "", false)
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		// Replace the binary WHILE the child runs: the shape this walk resolved
		// is a fact about the bytes it probed, not about whatever is at `path`
		// when it finishes.
		if err := os.WriteFile(path, []byte("a different build entirely"), 0o600); err != nil {
			t.Fatal(err)
		}
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})

	if result := runOpenCodeSmoke(context.Background(), path, "0.9.1"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("expected success, got %+v", result)
	}
	if _, cached := cliSmokeRememberedShape(path); cached {
		t.Error("a shape was filed under the REPLACED binary's key")
	}
}

func TestOpenCodeStderrErrorRegion_IsLineAnchored(t *testing.T) {
	// A `Usage:` LINE opens the help block and everything from it is dropped.
	got := openCodeStderrErrorRegion("error: unexpected argument '--x'\nusage: opencode run --format json\n")
	if strings.Contains(got, "--format") {
		t.Errorf("the usage block survived into the error region: %q", got)
	}
	if !strings.Contains(got, "--x") {
		t.Errorf("the error line was dropped: %q", got)
	}
	// Prose mentioning "usage:" mid-line is part of the error, not the help
	// block, so a genuine framing rejection behind it is still visible.
	got = openCodeStderrErrorRegion("error: invalid usage: --format needs a value\n")
	if !strings.Contains(got, "--format") {
		t.Errorf("a mid-line 'usage:' truncated a real framing rejection: %q", got)
	}
	// No usage block at all: the whole text is the error region.
	if got := openCodeStderrErrorRegion("error: unknown option '--format'"); !strings.Contains(got, "--format") {
		t.Errorf("stderr without a usage block was truncated: %q", got)
	}
}

/* --------------------------------------------------------------------------
   Per-run scratch directory
   -------------------------------------------------------------------------- */

func TestPruneOpenCodeSmokeScratch_ReclaimsOnlyOrphanedRunDirs(t *testing.T) {
	// Nothing else prunes this tree, so an agent killed mid-smoke would otherwise
	// leave one directory entry behind forever.
	scratch := t.TempDir()
	now := time.Now()

	mk := func(name string, age time.Duration) string {
		path := filepath.Join(scratch, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}

	orphan := mk("cwd-orphaned", openCodeSmokeScratchMaxAge+time.Hour)
	live := mk("cwd-live", time.Minute)
	// A neighbour's directory and a file must be untouched whatever their age:
	// the sweep only ever reclaims the names THIS file creates.
	foreign := mk("someone-elses-dir", openCodeSmokeScratchMaxAge+time.Hour)
	stray := filepath.Join(scratch, "cwd-not-a-dir")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-openCodeSmokeScratchMaxAge - time.Hour)
	if err := os.Chtimes(stray, old, old); err != nil {
		t.Fatal(err)
	}

	pruneOpenCodeSmokeScratch(scratch, now)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the orphaned run dir survived (err=%v)", err)
	}
	for name, path := range map[string]string{
		"a live run's dir":     live,
		"a neighbour's dir":    foreign,
		"a same-prefixed file": stray,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was reclaimed: %v", name, err)
		}
	}

	// A missing or unreadable scratch root is not an error — the sweep is
	// housekeeping on the way to a health check, never a reason to fail one.
	pruneOpenCodeSmokeScratch("", now)
	pruneOpenCodeSmokeScratch(filepath.Join(scratch, "does-not-exist"), now)
}

func TestPruneOpenCodeSmokeScratch_IsBounded(t *testing.T) {
	// One sweep must not turn a health check into a long blocking scan; the
	// remainder is reclaimed by later smokes.
	scratch := t.TempDir()
	now := time.Now()
	stamp := now.Add(-openCodeSmokeScratchMaxAge - time.Hour)
	total := openCodeSmokeScratchMaxSweep + 10
	for i := 0; i < total; i++ {
		path := filepath.Join(scratch, fmt.Sprintf("cwd-%03d", i))
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	pruneOpenCodeSmokeScratch(scratch, now)

	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if left := len(entries); left != total-openCodeSmokeScratchMaxSweep {
		t.Fatalf("one sweep left %d entries, want %d (cap %d)",
			left, total-openCodeSmokeScratchMaxSweep, openCodeSmokeScratchMaxSweep)
	}
	// And a second sweep makes progress rather than stalling.
	pruneOpenCodeSmokeScratch(scratch, now)
	if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
		t.Errorf("a second sweep left %d entries", len(entries))
	}
}

func TestRunOpenCodeSmoke_LeavesNoScratchBehind(t *testing.T) {
	// Every run removes its own per-run cwd and staged prompt on every path, so
	// the scratch tree does not grow with use. (Reclaiming an ORPHAN left by a
	// KILLED agent is the background sweep's job and is covered directly against
	// pruneOpenCodeSmokeScratch — asserting it through here would race the
	// goroutine and the once-per-process gate.)
	openCodeSmokeEnv(t)
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "", false)
	stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		return openCodeSuccessFrames(openCodeMarkerFromLaunch(t, launch)), nil, nil
	})
	if result := runOpenCodeSmoke(context.Background(), path, "0.9.1"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("expected success, got %+v", result)
	}
	assertNoOpenCodeSmokeScratchLeaks(t)
}

// A symlink (or Windows junction) named like a run directory must be LEFT ALONE,
// never descended into: this repo has lost entire sibling checkouts to a
// recursive delete that followed a junction.
func TestPruneOpenCodeSmokeScratch_NeverFollowsASymlink(t *testing.T) {
	scratch := t.TempDir()
	// The target holds a sentinel whose survival is the actual assertion.
	target := t.TempDir()
	sentinel := filepath.Join(target, "precious.txt")
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(scratch, "cwd-looks-like-a-run-dir")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink on this platform/filesystem: %v", err)
	}
	old := time.Now().Add(-openCodeSmokeScratchMaxAge - time.Hour)
	// Backdate the LINK itself (lchown-style times); best-effort, since the point
	// is that age never even gets consulted for a reparse point.
	_ = os.Chtimes(link, old, old)

	pruneOpenCodeSmokeScratch(scratch, time.Now())

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the sweep followed the symlink and touched its target: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link itself was removed; a reparse point must be left alone: %v", err)
	}
}

// A pre-spawn failure is not a run: nothing inferred, so there is nothing for a
// reconcile to find. Settling it as an UNCOVERED run would open a debt that
// retries OpenCode commands against the launch failure and can end by marking
// today's totals a lower bound.
func TestRunOpenCodeSmoke_APreSpawnFailureWithdrawsTheUsageCapture(t *testing.T) {
	openCodeSmokeEnv(t)
	openCodeUsageFixture(t, time.Now())
	path := stubOpenCodeBinary(t)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4", false)

	for _, tc := range []struct {
		name    string
		spawned bool
		wantOwe bool
	}{
		{name: "the child never started", spawned: false, wantOwe: false},
		{name: "the child started and was killed", spawned: true, wantOwe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCLISmokeState()
			resetOpenCodeUsageLedgerForTests()
			stub := stubOpenCodeSmokeExecStarted
			calls, _ := stub(t, tc.spawned, func(context.Context, openCodeLaunch) ([]byte, []byte, error) {
				return nil, nil, errOpenCodeShimUnrenderable
			})
			result := runOpenCodeSmoke(context.Background(), path, "0.9.1")
			if result.Diagnostic != cliSmokeDiagnosticLaunchError {
				t.Fatalf("diagnostic = %q, want launch_error", result.Diagnostic)
			}
			if *calls != 1 {
				t.Fatalf("seam calls = %d, want 1", *calls)
			}
			openCodeUsageRefreshWaitFor(5 * time.Second)
			state := readOpenCodeUsageFreshness()
			if got := state.owed(); got != tc.wantOwe {
				t.Fatalf("owed = %t, want %t (state %+v)", got, tc.wantOwe, state)
			}
			if !tc.spawned && state.RunFloorMs != 0 {
				t.Fatalf("run floor = %d, want the armed floor withdrawn", state.RunFloorMs)
			}
		})
	}
}
