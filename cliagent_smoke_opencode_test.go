package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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
	resetOpenCodeReadinessCache()
	t.Cleanup(func() {
		resetCLISmokeState()
		resetVersionProbeCache()
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
func stubOpenCodeSmokeExec(t *testing.T, fn func(ctx context.Context, launch openCodeLaunch) (stdout, stderr []byte, err error)) (*int, *[]openCodeLaunch) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	var launches []openCodeLaunch
	original := runOpenCodeSmokeCommand
	runOpenCodeSmokeCommand = func(ctx context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
		mu.Lock()
		calls++
		launches = append(launches, launch)
		mu.Unlock()
		return fn(ctx, launch)
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

func openCodeExitError(t *testing.T) error {
	t.Helper()
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "exit 2").Run()
	} else {
		err = exec.Command("sh", "-c", "exit 2").Run()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("could not manufacture an *exec.ExitError: %v", err)
	}
	return err
}

// stubOpenCodeReadiness pins the readiness pre-check so a case never spawns the
// `opencode models` child. `known=false` (inconclusive) is the default the real
// probe returns for an unreachable binary, and it PROCEEDS.
func stubOpenCodeReadiness(t *testing.T, out string, ok bool) {
	t.Helper()
	original := runOpenCodeProbe
	runOpenCodeProbe = func(context.Context, string, ...string) (string, bool) { return out, ok }
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
			name:           "a generic non-zero exit is no_envelope",
			stderr:         "something went wrong deep inside",
			runErr:         exitErr,
			wantCategory:   cliUsageErrorProtocol,
			wantDiagnostic: cliSmokeDiagnosticNoEnvelope,
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

// framing_rejected must be named by the FLAG, not by a bare `json` substring:
// OpenCode prints a usage block on an option error and that block lists
// `--format json`, so a substring match reported every rejected caller flag as a
// broken framing contract.
func TestOpenCodeSmokeNoEnvelopeDiagnostic_UsageBlockDoesNotForgeFramingRejected(t *testing.T) {
	usage := "error: unexpected argument '--not-a-known-flag' found\n\n" +
		"Usage: opencode run [OPTIONS] [PROMPT]\n\nOptions:\n" +
		"      --format <FORMAT>  Output format [possible values: text, json]\n"
	if got := openCodeSmokeNoEnvelopeDiagnostic([]byte(usage)); got != cliSmokeDiagnosticFlagRejected {
		t.Fatalf("a rejected caller flag whose usage block mentions json = %q, want flag_rejected", got)
	}
	// A genuine refusal of the output contract still reports framing_rejected.
	for _, stderr := range []string{
		"error: unknown option '--format'",
		"error: unrecognized value 'json' for --format",
	} {
		if got := openCodeSmokeNoEnvelopeDiagnostic([]byte(stderr)); got != cliSmokeDiagnosticFramingRejected {
			t.Fatalf("%q = %q, want framing_rejected", stderr, got)
		}
	}
	// And a non-rejection stays no_envelope.
	if got := openCodeSmokeNoEnvelopeDiagnostic([]byte("panic: nil map")); got != cliSmokeDiagnosticNoEnvelope {
		t.Fatalf("a non-rejection = %q, want no_envelope", got)
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
