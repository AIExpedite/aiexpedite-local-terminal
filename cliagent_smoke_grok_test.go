package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_grok_test.go
   --------------------------------------------------------------------------
   Every case drives the exec seam, so no test ever spawns a real `grok` or
   spends an inference turn. The invariants worth stating up front:
     - a pre-inference exit (a rejected flag, a rejected framing contract, a
       clean exit with no `end` frame) is `protocol` with a diagnostic that
       names WHICH — the opaque non-zero exit this feature replaces;
     - a working CLI that answered the wrong text is `parse_failed`;
     - the pre-checks short-circuit BEFORE the seam runs, so no quota is spent
       on a device with no binary, no login, or a refused config posture;
     - nothing the CLI authored — and nothing from the isolated home — reaches
       the published result or the device log.
   ------------------------------------------------------------------------ */

// grokSmokeEnv isolates every on-disk side channel a Grok smoke touches (the
// persistent GROK_HOME with a seeded login, the prompt scratch dir under the
// user home, the live billing cache, the system config layers) and clears the
// in-memory caches so cases cannot leak into each other. Returns the
// persistent home.
func grokSmokeEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	persistent := filepath.Join(home, ".grok")
	if err := os.MkdirAll(persistent, 0o700); err != nil {
		t.Fatal(err)
	}
	seedGrokHomeWithLogin(t, persistent)
	t.Setenv("GROK_HOME", persistent)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	stubGrokMaintenanceSmokePreflight(t)
	resetCLISmokeState()
	resetVersionProbeCache()
	t.Cleanup(func() {
		resetCLISmokeState()
		resetVersionProbeCache()
	})
	return persistent
}

// stubGrokBinary writes a fake grok binary and returns its path. It is never
// executed — the exec seam and the version probe are both stubbed — so it
// carries no .exe extension: nothing should be able to run it by accident.
func stubGrokBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubGrokSmokePath(t *testing.T, path string) {
	t.Helper()
	original := resolveGrokSmokePath
	resolveGrokSmokePath = func() string { return path }
	t.Cleanup(func() { resolveGrokSmokePath = original })
}

// stubGrokSmokeExec replaces the exec seam with a scripted responder, records
// every launch, and returns a pointer to the call count.
func stubGrokSmokeExec(t *testing.T, fn func(ctx context.Context, launch grokSmokeLaunch) (stdout, stderr []byte, err error)) (*int, *[]grokSmokeLaunch) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	var launches []grokSmokeLaunch
	original := runGrokSmokeCommand
	runGrokSmokeCommand = func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		mu.Lock()
		calls++
		launches = append(launches, launch)
		mu.Unlock()
		return fn(ctx, launch)
	}
	t.Cleanup(func() { runGrokSmokeCommand = original })
	return &calls, &launches
}

// grokMarkerFromLaunch recovers the nonce the probe generated for this run from
// the staged prompt file — the ONLY place it appears, which is itself the point.
func grokMarkerFromLaunch(t *testing.T, launch grokSmokeLaunch) string {
	t.Helper()
	body, err := os.ReadFile(launch.PromptFile)
	if err != nil {
		t.Fatalf("prompt file must exist while the child runs: %v", err)
	}
	prompt := string(body)
	if !strings.HasPrefix(prompt, grokMaintenanceSmokePromptPrefix) {
		t.Fatalf("prompt %q does not start with the smoke prefix", prompt)
	}
	return strings.TrimSpace(strings.TrimPrefix(prompt, grokMaintenanceSmokePromptPrefix))
}

// grokSuccessFrames is what `--output-format=streaming-json` emits on a healthy
// turn: the assistant text split across incremental deltas, then `end`. The
// split is deliberate — a classifier that inserted a separator at a frame
// boundary would corrupt the marker.
func grokSuccessFrames(marker string) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"thought","text":"echoing"}` + "\n")
	third := len(marker) / 3
	for _, delta := range []string{marker[:third], marker[third : 2*third], marker[2*third:]} {
		line, _ := json.Marshal(map[string]string{"type": "text", "text": delta})
		b.Write(line)
		b.WriteString("\n")
	}
	b.WriteString(`{"type":"end","stopReason":"end_turn"}` + "\n")
	return []byte(b.String())
}

func grokExitError(t *testing.T) error {
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

func TestRunGrokSmoke_ExactMarkerSucceeds(t *testing.T) {
	persistent := grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")

	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
		t.Fatalf("healthy CLI must pass the smoke: %+v", result)
	}
	if result.ErrorCategory != "" || result.Diagnostic != cliSmokeDiagnosticNone {
		t.Errorf("success must carry no error category/diagnostic: %+v", result)
	}
	if result.CliID != "grok" || result.Version != "grok 1.0.13" {
		t.Errorf("result identity = %q/%q", result.CliID, result.Version)
	}
	if result.ArgvShapeID != grokSmokeArgvShapes[0].ID {
		t.Errorf("shape = %q, want the canonical rung %q", result.ArgvShapeID, grokSmokeArgvShapes[0].ID)
	}
	if *calls != 1 {
		t.Fatalf("spent %d turns, want 1", *calls)
	}

	launch := (*launches)[0]
	if err := validateGrokSmokeShape(launch.Args); err != nil {
		t.Errorf("spawned argv is not a ladder rung: %v (%#v)", err, launch.Args)
	}
	for i, arg := range launch.Args {
		if arg == "" {
			t.Errorf("spawned argv has an empty element at %d: %#v", i, launch.Args)
		}
	}
	if launch.Path != path {
		t.Errorf("spawned %q, want the resolved binary %q", launch.Path, path)
	}
	// Isolation: the child inherits a private GROK_HOME (never the real one),
	// no credential override, HOME/USERPROFILE/PWD redirected, and runs in the
	// empty workspace under it.
	env := map[string]string{}
	for _, kv := range launch.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	isolated := env["GROK_HOME"]
	if isolated == "" || isolated == persistent {
		t.Fatalf("child GROK_HOME = %q, want an isolated copy (persistent %q)", isolated, persistent)
	}
	if env["HOME"] != isolated || env["USERPROFILE"] != isolated {
		t.Errorf("HOME/USERPROFILE not redirected to the isolated home: %q / %q", env["HOME"], env["USERPROFILE"])
	}
	if launch.Dir != filepath.Join(isolated, "workspace") || env["PWD"] != launch.Dir {
		t.Errorf("child cwd = %q (PWD %q), want the isolated workspace", launch.Dir, env["PWD"])
	}
	for _, banned := range []string{"XAI_API_KEY", "GROK_LOG_FILE", "RUST_LOG"} {
		if _, present := env[banned]; present {
			t.Errorf("child env carries %s", banned)
		}
	}
	// Cleanup: the isolated home and the prompt file are gone once the smoke
	// returns — the prompt was removed exactly once, after the child was reaped.
	if _, err := os.Stat(isolated); !os.IsNotExist(err) {
		t.Errorf("isolated home %q survived the smoke (stat err = %v)", isolated, err)
	}
	if _, err := os.Stat(launch.PromptFile); !os.IsNotExist(err) {
		t.Errorf("prompt file %q survived the smoke (stat err = %v)", launch.PromptFile, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(grokPromptTempDir(), "grok-prompt-*.txt")); len(leftovers) != 0 {
		t.Errorf("prompt scratch dir holds leftovers: %v", leftovers)
	}
}

// The prompt file is created owner-only and holds ONLY the prompt; the nonce
// is never on argv or in the environment.
func TestRunGrokSmoke_PromptFileIsPrivateAndNonceStaysOffArgv(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	var marker string
	_, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		marker = grokMarkerFromLaunch(t, launch)
		if runtime.GOOS != "windows" {
			info, err := os.Stat(launch.PromptFile)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("prompt file mode = %o, want 0600", perm)
			}
		}
		return grokSuccessFrames(marker), nil, nil
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")
	if !result.MarkerMatched {
		t.Fatalf("smoke did not pass: %+v", result)
	}
	if !strings.HasPrefix(marker, grokSmokeMarkerPrefix) || len(marker) != len(grokSmokeMarkerPrefix)+8 {
		t.Fatalf("marker %q is not prefix + 8 hex chars", marker)
	}
	launch := (*launches)[0]
	for _, arg := range launch.Args {
		if strings.Contains(arg, marker) || strings.Contains(arg, grokMaintenanceSmokePromptPrefix) {
			t.Fatalf("nonce/prompt leaked into argv: %#v", launch.Args)
		}
		if arg == "-p" || arg == "--single" {
			t.Fatalf("smoke used an inline prompt flag: %#v", launch.Args)
		}
	}
	for _, kv := range launch.Env {
		if strings.Contains(kv, marker) {
			t.Fatalf("nonce leaked into the child env")
		}
	}
}

func TestClassifyGrokSmokeRun_MapsEveryOutcomeOntoTheClosedSet(t *testing.T) {
	const marker = "AIEXPEDITE_GROK_SMOKE_OK_0badf00d"
	exit := grokExitError(t)
	cases := []struct {
		name       string
		timedOut   bool
		stdout     string
		stderr     string
		err        error
		category   string
		diagnostic string
		matched    bool
	}{
		{
			name:     "marker across deltas with end frame",
			stdout:   string(grokSuccessFrames(marker)),
			category: "", diagnostic: cliSmokeDiagnosticNone, matched: true,
		},
		{
			name:     "1.0.13 data-field deltas",
			stdout:   `{"type":"text","data":"AIEXPEDITE_GROK_"}` + "\n" + `{"type":"text","data":"SMOKE_OK_0badf00d"}` + "\n" + `{"type":"end"}`,
			category: "", diagnostic: cliSmokeDiagnosticNone, matched: true,
		},
		{
			name:     "banner line before frames is tolerated",
			stdout:   "grok 1.0.13 starting\n" + string(grokSuccessFrames(marker)),
			category: "", diagnostic: cliSmokeDiagnosticNone, matched: true,
		},
		{
			name:     "chatty answer is parse_failed not protocol",
			stdout:   `{"type":"text","text":"Sure! Here it is: ` + marker + `"}` + "\n" + `{"type":"end"}`,
			category: cliUsageErrorParseFailed, diagnostic: cliSmokeDiagnosticMarkerMismatch,
		},
		{
			name:     "per-attempt deadline kill",
			timedOut: true, err: exit,
			category: cliUsageErrorProviderTimeout, diagnostic: cliSmokeDiagnosticTimeout,
		},
		{
			name:     "cancelled context",
			err:      context.Canceled,
			category: cliUsageErrorProviderTimeout, diagnostic: cliSmokeDiagnosticTimeout,
		},
		{
			name:   "clap rejects an optional flag",
			stderr: "error: unexpected argument '--no-subagents' found\n\nUsage: grok [OPTIONS]", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:   "clap rejects a shared flag",
			stderr: "error: unexpected argument '--tools=' found", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:   "empty operand dropped by a shim re-parse",
			stderr: "error: a value is required for '--tools <TOOLS>' but none was supplied", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFlagRejected,
		},
		{
			name:   "streaming contract rejected",
			stderr: "error: invalid value 'streaming-json' for '--output-format <FORMAT>'", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFramingRejected,
		},
		{
			name:   "prompt-file transport rejected",
			stderr: "error: unexpected argument '--prompt-file' found", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFramingRejected,
		},
		{
			name:   "non-zero exit with no frames and unrecognised stderr",
			stderr: "thread 'main' panicked at src/main.rs", err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:     "clean exit with no end frame",
			stdout:   "A new version of grok is available\n",
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:     "text without a terminal frame",
			stdout:   `{"type":"text","text":"` + marker + `"}`,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope,
		},
		{
			name:   "error frame reporting auth",
			stdout: `{"type":"error","message":"Not logged in. Run grok login."}` + "\n" + `{"type":"end"}`, err: exit,
			category: cliUsageErrorNotAuthenticated, diagnostic: cliSmokeDiagnosticAuthError,
		},
		{
			name:   "error frame reporting credit exhaustion",
			stdout: `{"type":"error","error":"usage limit reached for this billing period"}`, err: exit,
			category: cliUsageErrorProviderUnavailable, diagnostic: cliSmokeDiagnosticProviderError,
		},
		{
			name:   "error frame reporting an xAI outage",
			stdout: `{"type":"error","message":"API error: status 503 service unavailable"}`, err: exit,
			category: cliUsageErrorProviderUnavailable, diagnostic: cliSmokeDiagnosticProviderError,
		},
		{
			name:   "error frame with unrecognised text",
			stdout: `{"type":"error","message":"something unexpected"}`, err: exit,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			category, diagnostic, matched := classifyGrokSmokeRun(tc.timedOut, []byte(tc.stdout), []byte(tc.stderr), tc.err, marker)
			if category != tc.category || diagnostic != tc.diagnostic || matched != tc.matched {
				t.Fatalf("classify = (%q, %q, %t), want (%q, %q, %t)",
					category, diagnostic, matched, tc.category, tc.diagnostic, tc.matched)
			}
			if !isKnownCLISmokeDiagnostic(diagnostic) {
				t.Fatalf("diagnostic %q is outside the closed set", diagnostic)
			}
		})
	}
}

func TestRunGrokSmoke_MissingBinaryOrVersionSpendsNothing(t *testing.T) {
	grokSmokeEnv(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		t.Fatal("pre-check failure must not spawn a child")
		return nil, nil, nil
	})

	for _, tc := range []struct{ path, version string }{
		{"", ""},
		{stubGrokBinary(t), ""},
	} {
		result := runGrokSmoke(context.Background(), tc.path, tc.version)
		if result.Status != cliSmokeStatusFailed || result.ErrorCategory != cliUsageErrorProviderUnavailable ||
			result.Diagnostic != cliSmokeDiagnosticBinaryMissing {
			t.Fatalf("path=%q version=%q → %+v, want provider_unavailable/binary_missing", tc.path, tc.version, result)
		}
		if cliSmokeVerdictSpentTurn(result) {
			t.Fatalf("a free verdict must not be pinned by the cooldown: %+v", result)
		}
	}
	// A version the config-posture check cannot evaluate is OUR gap: internal.
	result := runGrokSmoke(context.Background(), stubGrokBinary(t), "not a version")
	if result.ErrorCategory != cliUsageErrorInternal || result.Diagnostic != cliSmokeDiagnosticInternal {
		t.Fatalf("unparseable version → %+v, want internal", result)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times", *calls)
	}
}

func TestRunGrokSmoke_LoggedOutSpendsNoTurnAndLeavesNoPromptFile(t *testing.T) {
	grokSmokeEnv(t)
	// An EMPTY persistent home: isolation succeeds but copies no login.
	empty := t.TempDir()
	t.Setenv("GROK_HOME", empty)
	path := stubGrokBinary(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		t.Fatal("logged-out CLI must not spawn a child")
		return nil, nil, nil
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")

	if result.ErrorCategory != cliUsageErrorNotAuthenticated || result.Diagnostic != cliSmokeDiagnosticNotLoggedIn {
		t.Fatalf("logged-out → %+v, want not_authenticated/not_logged_in", result)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times", *calls)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(grokPromptTempDir(), "grok-prompt-*.txt")); len(leftovers) != 0 {
		t.Errorf("a refused smoke staged a prompt file: %v", leftovers)
	}
	if cliSmokeVerdictSpentTurn(result) {
		t.Fatal("not_logged_in must not be pinned by the cooldown")
	}
}

// A system config layer that pins a credential refuses the smoke BEFORE any
// child, and the refusal publishes `internal` — never the layer's contents.
func TestRunGrokSmoke_SystemConfigRefusalIsInternalAndSpendsNothing(t *testing.T) {
	grokSmokeEnv(t)
	configureTestGrokSystemLayers(t, "[model]\napi_key = \"xai-system-credential-sentinel\"\n")
	path := stubGrokBinary(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		t.Fatal("refused config posture must not spawn a child")
		return nil, nil, nil
	})

	var result cliSmokeResult
	logged := captureStdout(t, func() {
		result = runGrokSmoke(context.Background(), path, "grok 1.0.13")
	})

	if result.ErrorCategory != cliUsageErrorInternal || result.Diagnostic != cliSmokeDiagnosticInternal {
		t.Fatalf("pinned credential → %+v, want internal", result)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times", *calls)
	}
	payload, _ := json.Marshal(result)
	if strings.Contains(string(payload)+logged, "xai-system-credential-sentinel") {
		t.Fatal("system config contents leaked into the result or the log")
	}
	var preflight *grokSmokePreflightError
	if err := detectGrokMaintenanceSmokeSystemConfig("grok 1.0.13"); !errors.As(err, &preflight) || preflight.Diagnostic != cliSmokeDiagnosticInternal {
		t.Fatalf("preflight refusal is not the typed error carrying a closed diagnostic: %v", err)
	}
}

// Only a rejection naming a flag the NEXT rung drops earns a second child —
// and exactly one. The winning rung is then cached for the binary.
func TestRunGrokSmoke_FlagRejectionRetriesOnceAndCachesTheRung(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		for _, arg := range launch.Args {
			if arg == "--no-subagents" {
				return nil, []byte("error: unexpected argument '--no-subagents' found\n"), grokExitError(t)
			}
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	// A build that documents the hardened rung yet rejects one of its
	// switches: the canonical rung goes first and the walk heals on the legacy
	// rung.
	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")

	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
		t.Fatalf("legacy rung should have passed: %+v", result)
	}
	if result.ArgvShapeID != grokSmokeArgvShapes[1].ID {
		t.Errorf("shape = %q, want the legacy rung", result.ArgvShapeID)
	}
	if *calls != 2 {
		t.Fatalf("spent %d children, want 2 (one rejected rung + one turn)", *calls)
	}
	if len(*launches) == 2 && (*launches)[0].PromptFile != (*launches)[1].PromptFile {
		t.Errorf("the retry must reuse the staged prompt file")
	}

	// Steady state: the resolved rung is the only one tried.
	*calls = 0
	result = runGrokSmoke(context.Background(), path, "grok 1.0.13")
	if *calls != 1 || result.ArgvShapeID != grokSmokeArgvShapes[1].ID {
		t.Fatalf("cached rung not honoured: calls=%d shape=%q", *calls, result.ArgvShapeID)
	}
}

// A build that predates the isolation switches starts on the legacy rung, so
// the probe spends no rejected spawn on it — and the legacy session_start
// smoke, which takes the ladder's first entry as its ONLY child, resolves the
// compatible rung without being able to walk at all.
func TestRunGrokSmoke_PreHardenedBuildStartsOnTheLegacyRung(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		for _, arg := range launch.Args {
			if arg == "--disable-web-search" || arg == "--no-subagents" {
				return nil, []byte("error: unexpected argument '" + arg + "' found\n"), grokExitError(t)
			}
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	if first := grokSmokeShapeLadder(path, "grok 1.0.5")[0]; first.ID != grokSmokeArgvShapes[1].ID {
		t.Fatalf("session-path rung for a pre-1.0.13 build = %q, want the legacy rung", first.ID)
	}

	result := runGrokSmoke(context.Background(), path, "grok 1.0.5")
	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched || result.ArgvShapeID != grokSmokeArgvShapes[1].ID {
		t.Fatalf("pre-hardened build should pass on the legacy rung first: %+v", result)
	}
	if *calls != 1 {
		t.Fatalf("spent %d children, want 1 (no rejected rung)", *calls)
	}
	if err := validateGrokSmokeShape((*launches)[0].Args); err != nil {
		t.Fatalf("legacy rung failed the shape contract: %v", err)
	}
	for _, arg := range (*launches)[0].Args {
		if arg == "" {
			t.Fatal("legacy rung emitted an empty argv element")
		}
	}

	// Once resolved, both transports collapse onto that rung for this binary.
	if ladder := grokSmokeShapeLadder(path, "grok 1.0.5"); len(ladder) != 1 || ladder[0].ID != grokSmokeArgvShapes[1].ID {
		t.Fatalf("resolved rung not cached for the binary: %+v", ladder)
	}
}

// The reverse walk: a build whose version says "legacy" but that has dropped
// `--no-auto-update` is healed by advancing to the hardened rung — the
// retryable set is symmetric, so a legacy-first walk can move on too.
func TestRunGrokSmoke_LegacyFirstWalkAdvancesOnARejectedAutoUpdateFlag(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		for _, arg := range launch.Args {
			if arg == "--no-auto-update" {
				return nil, []byte("error: unexpected argument '--no-auto-update' found\n"), grokExitError(t)
			}
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.5")
	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched || result.ArgvShapeID != grokSmokeArgvShapes[0].ID {
		t.Fatalf("walk should have advanced to the canonical rung: %+v", result)
	}
	if *calls != 2 {
		t.Fatalf("spent %d children, want 2 (one rejected rung + one turn)", *calls)
	}
}

// A walk that outlives the binary it probed must not file its winning rung
// under the replacement's key: the shape is a fact about the bytes that
// accepted it, and a post-upgrade flight may already have resolved a different
// one. Every later smoke — including the legacy session_start path, which
// reads the same cache — would otherwise collapse its ladder onto a rung the
// installed build can reject.
func TestRunGrokSmoke_ShapeIsNotBoundToABinaryReplacedMidRun(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		frames := grokSuccessFrames(grokMarkerFromLaunch(t, launch))
		// The upgrade lands while this child is still running.
		if err := os.WriteFile(path, []byte("stub-upgraded-longer"), 0o600); err != nil {
			t.Fatal(err)
		}
		return frames, nil, nil
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.5")
	if result.Status != cliSmokeStatusSuccess || *calls != 1 {
		t.Fatalf("pre-upgrade walk should have passed in one child: %+v calls=%d", result, *calls)
	}
	if shape, cached := cliSmokeRememberedShape(path); cached {
		t.Fatalf("late walk bound %q to the replacement binary", shape)
	}

	// Nor can a late walk overwrite the shape a post-upgrade flight already
	// resolved: the stale binding is taken first, the upgrade lands, the
	// post-upgrade flight stores its rung, and the stale write is refused.
	stale := bindCLISmokeShape(path)
	if err := os.WriteFile(path, []byte("stub-upgraded-longer-still"), 0o600); err != nil {
		t.Fatal(err)
	}
	bindCLISmokeShape(path).remember(grokSmokeArgvShapes[1].ID)
	stale.remember(grokSmokeArgvShapes[0].ID)
	shape, cached := cliSmokeRememberedShape(path)
	if !cached || shape != grokSmokeArgvShapes[1].ID {
		t.Fatalf("stale walk clobbered the post-upgrade rung: shape=%q cached=%t", shape, cached)
	}
}

func TestRunGrokSmoke_RejectionOfASharedFlagSpendsNoSecondChild(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		return nil, []byte("error: unexpected argument '--max-turns=1' found\n"), grokExitError(t)
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")

	if result.ErrorCategory != cliUsageErrorProtocol || result.Diagnostic != cliSmokeDiagnosticFlagRejected {
		t.Fatalf("shared-flag rejection → %+v, want protocol/flag_rejected", result)
	}
	if *calls != 1 {
		t.Fatalf("spent %d children for a flag every rung carries, want 1", *calls)
	}
	if result.ArgvShapeID != grokSmokeArgvShapes[0].ID {
		t.Errorf("shape = %q, want the rung that was actually tried", result.ArgvShapeID)
	}
}

// Any other protocol failure — a turn may already have been consumed — stops
// after the first attempt.
func TestRunGrokSmoke_OnlyFlagRejectionRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		stderr string
	}{
		{"framing rejected", "", "error: invalid value 'streaming-json' for '--output-format'"},
		{"no envelope", "", "panicked"},
		{"marker mismatch", `{"type":"text","text":"nope"}` + "\n" + `{"type":"end"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grokSmokeEnv(t)
			path := stubGrokBinary(t)
			calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
				var err error
				if tc.stderr != "" {
					err = grokExitError(t)
				}
				return []byte(tc.stdout), []byte(tc.stderr), err
			})
			result := runGrokSmoke(context.Background(), path, "grok 1.0.13")
			if result.Status != cliSmokeStatusFailed || result.MarkerMatched {
				t.Fatalf("expected a failure: %+v", result)
			}
			if *calls != 1 {
				t.Fatalf("spent %d children, want 1", *calls)
			}
		})
	}
}

// The attempt's own deadline killed the child: classified `timeout`, never
// retried, prompt file still removed.
func TestRunGrokSmoke_AttemptDeadlineKillIsProviderTimeout(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	original := grokSmokeTimeout
	grokSmokeTimeout = 30 * time.Millisecond
	t.Cleanup(func() { grokSmokeTimeout = original })
	calls, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		<-ctx.Done()
		return []byte(`{"type":"text","text":"partial"}`), nil, grokExitError(t)
	})

	result := runGrokSmoke(context.Background(), path, "grok 1.0.13")

	if result.ErrorCategory != cliUsageErrorProviderTimeout || result.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("deadline kill → %+v, want provider_timeout/timeout", result)
	}
	if *calls != 1 {
		t.Fatalf("spent %d children, want 1", *calls)
	}
	if _, err := os.Stat((*launches)[0].PromptFile); !os.IsNotExist(err) {
		t.Errorf("prompt file survived the deadline-kill path")
	}
}

func TestRunCLISmoke_GrokCooldownReplaysButNotAcrossAnUpgrade(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.5")
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	first, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || first.Status != cliSmokeStatusSuccess || first.Version != "grok 1.0.5" {
		t.Fatalf("first smoke = %+v replayed=%t", first, replayed)
	}
	second, replayed := runCLISmoke(context.Background(), "grok")
	if !replayed || second != first {
		t.Fatalf("second smoke inside the cooldown must replay: %+v replayed=%t", second, replayed)
	}
	if *calls != 1 {
		t.Fatalf("cooldown let %d turns through, want 1", *calls)
	}

	// The upgrade: a different binary (mtime/size) reporting a new version.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("stub-upgraded"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, "grok 1.0.13")
	third, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || third.Version != "grok 1.0.13" {
		t.Fatalf("post-upgrade smoke replayed the pre-upgrade verdict: %+v replayed=%t", third, replayed)
	}
	if *calls != 2 {
		t.Fatalf("post-upgrade smoke did not execute: calls=%d", *calls)
	}
}

// A run killed because the CALLER's context was cancelled (delivery cancelled,
// agent shutdown) is not a verdict on the binary: it must not be pinned by the
// cooldown, so the redelivered smoke actually tests the CLI.
func TestRunCLISmoke_GrokCallerCancellationIsNotCached(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := false
	calls, _ := stubGrokSmokeExec(t, func(runCtx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		if !cancelled {
			cancelled = true
			cancel()
			<-runCtx.Done()
			return nil, nil, grokExitError(t)
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	first, replayed := runCLISmoke(ctx, "grok")
	if replayed || first.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("cancelled smoke = %+v replayed=%t, want an executed timeout", first, replayed)
	}
	second, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || second.Status != cliSmokeStatusSuccess {
		t.Fatalf("redelivered smoke replayed the cancellation: %+v replayed=%t", second, replayed)
	}
	if *calls != 2 {
		t.Fatalf("redelivered smoke did not execute: calls=%d", *calls)
	}
}

// A follower that joined the singleflight with a LIVE delivery must not be
// handed the leader's cancellation as its answer: the shared probe ran under
// the leader's ctx, so its `timeout` says nothing about the binary. The
// follower re-enters the group and runs (or joins) a fresh probe of its own.
func TestRunCLISmoke_GrokLiveFollowerRetriesAfterACancelledLeader(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	followerJoined := make(chan struct{})
	leaderTurn := make(chan struct{}, 1)
	leaderTurn <- struct{}{}
	calls, _ := stubGrokSmokeExec(t, func(runCtx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		select {
		case <-leaderTurn:
			// The first run is the leader's: park until the follower is
			// queued behind this flight, then lose the delivery mid-run.
			<-followerJoined
			cancelLeader()
			<-runCtx.Done()
			return nil, nil, grokExitError(t)
		default:
		}
		if runCtx.Err() != nil {
			t.Errorf("the follower's own run inherited a cancelled context")
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	var wg sync.WaitGroup
	var leader, follower cliSmokeResult
	var followerReplayed bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		leader, _ = runCLISmoke(leaderCtx, "grok")
	}()
	// Give the leader time to enter the exec seam before the follower joins.
	time.Sleep(50 * time.Millisecond)
	wg.Add(1)
	go func() {
		defer wg.Done()
		follower, followerReplayed = runCLISmoke(context.Background(), "grok")
	}()
	time.Sleep(50 * time.Millisecond)
	close(followerJoined)
	wg.Wait()

	if leader.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("cancelled leader = %+v, want its own timeout verdict", leader)
	}
	if follower.Status != cliSmokeStatusSuccess || followerReplayed {
		t.Fatalf("live follower = %+v replayed=%t, want a fresh executed success — it was handed the leader's cancellation", follower, followerReplayed)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want the leader's cancelled run plus the follower's own", *calls)
	}
}

// A provider-side auth rejection (the local credential still parses, xAI
// refuses it) spends no turn and is the one failure the replay's login
// re-check cannot clear — so it must never be pinned. The user who signs in
// again gets a real probe on the next wake, not the cached failure.
func TestRunCLISmoke_GrokAuthErrorIsNotCached(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")
	rejected := false
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		if !rejected {
			rejected = true
			return []byte(`{"type":"error","message":"Not logged in. Run grok login."}` + "\n" + `{"type":"end"}`), nil, grokExitError(t)
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	first, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || first.Diagnostic != cliSmokeDiagnosticAuthError {
		t.Fatalf("first smoke = %+v replayed=%t, want an executed auth_error", first, replayed)
	}
	if cliSmokeVerdictSpentTurn(first) {
		t.Fatal("a pre-inference auth rejection must not be pinned by the cooldown")
	}
	// Same binary, same cooldown window, credential healthy again.
	second, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || second.Status != cliSmokeStatusSuccess {
		t.Fatalf("post-login smoke replayed the cached auth failure: %+v replayed=%t", second, replayed)
	}
	if *calls != 2 {
		t.Fatalf("post-login smoke did not execute: calls=%d", *calls)
	}
}

func TestRunCLISmoke_GrokCachedSuccessIsNotReplayedAfterLogout(t *testing.T) {
	persistent := grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	if result, _ := runCLISmoke(context.Background(), "grok"); result.Status != cliSmokeStatusSuccess {
		t.Fatalf("first smoke = %+v", result)
	}
	// Log out without touching the binary.
	if err := os.Remove(filepath.Join(persistent, "auth.json")); err != nil {
		t.Fatal(err)
	}
	result, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || result.Diagnostic != cliSmokeDiagnosticNotLoggedIn {
		t.Fatalf("logged-out device replayed a cached success: %+v replayed=%t", result, replayed)
	}
	if *calls != 1 {
		t.Fatalf("logged-out smoke spawned a child: calls=%d", *calls)
	}
}

func TestRunCLISmoke_GrokConcurrentCallersShareOneTurn(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")
	gate := make(chan struct{})
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		<-gate
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	const callers = 4
	var wg sync.WaitGroup
	results := make([]cliSmokeResult, callers)
	replays := make([]bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], replays[i] = runCLISmoke(context.Background(), "grok")
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	executed := 0
	for i := range results {
		if results[i].Status != cliSmokeStatusSuccess {
			t.Fatalf("caller %d = %+v", i, results[i])
		}
		if !replays[i] {
			executed++
		}
	}
	if executed != 1 || *calls != 1 {
		t.Fatalf("%d callers executed / %d children spawned, want 1 / 1", executed, *calls)
	}
}

// A caller arriving after an upgrade replaced the binary while a pre-upgrade
// smoke is still in flight must test the NEW binary rather than join the old
// leader — and the old leader's late verdict must not evict the new one.
func TestRunCLISmoke_GrokUpgradeMidFlightDoesNotJoinTheOldLeader(t *testing.T) {
	grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.5")
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var mu sync.Mutex
	first := true
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		mu.Lock()
		isOld := first
		first = false
		mu.Unlock()
		if isOld {
			close(oldStarted)
			<-releaseOld
		}
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	var old cliSmokeResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		old, _ = runCLISmoke(context.Background(), "grok")
	}()
	<-oldStarted

	// The upgrade lands while the pre-upgrade smoke is still running.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("stub-upgraded"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, "grok 1.0.13")
	var upgraded cliSmokeResult
	var replayed bool
	upgradedDone := make(chan struct{})
	go func() {
		defer close(upgradedDone)
		upgraded, replayed = runCLISmoke(context.Background(), "grok")
	}()
	select {
	case <-upgradedDone:
	case <-time.After(5 * time.Second):
		// Joined the blocked pre-upgrade leader instead of running its own probe.
		close(releaseOld)
		<-done
		t.Fatal("post-upgrade caller blocked on the pre-upgrade flight")
	}
	if replayed || upgraded.Version != "grok 1.0.13" || upgraded.Status != cliSmokeStatusSuccess {
		t.Fatalf("post-upgrade caller joined the pre-upgrade flight: %+v replayed=%t", upgraded, replayed)
	}

	close(releaseOld)
	<-done
	if old.Version != "grok 1.0.5" {
		t.Fatalf("pre-upgrade leader = %+v", old)
	}
	if *calls != 2 {
		t.Fatalf("spawned %d children, want one per binary (2)", *calls)
	}
	// The stale leader finished last; the cooldown must still hold the NEW
	// binary's verdict.
	again, replayed := runCLISmoke(context.Background(), "grok")
	if !replayed || again.Version != "grok 1.0.13" {
		t.Fatalf("late pre-upgrade verdict evicted the post-upgrade one: %+v replayed=%t", again, replayed)
	}
}

// Neither the published result nor the device log carries any text the CLI
// authored — or anything from the isolated home. A stderr fixture holding a
// credential-shaped string and an auth.json path leaks neither.
func TestRunGrokSmoke_ResultAndLogCarryNoVendorText(t *testing.T) {
	persistent := grokSmokeEnv(t)
	path := stubGrokBinary(t)
	dirty := strings.Join([]string{
		"Authorization: Bearer xai-credential-sentinel-0123456789",
		`{"key":"xai-key-sentinel","refresh_token":"refresh-token-sentinel"}`,
		"loaded auth from " + filepath.Join(persistent, "auth.json"),
		"error: unexpected argument '--no-subagents' found",
	}, "\n")
	var isolated, promptFile string
	stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		for _, kv := range launch.Env {
			if v, ok := strings.CutPrefix(kv, "GROK_HOME="); ok {
				isolated = v
			}
		}
		promptFile = launch.PromptFile
		return []byte(`{"type":"text","text":"` + dirty + `"}`), []byte(dirty), grokExitError(t)
	})

	var result cliSmokeResult
	logged := captureStdout(t, func() {
		result = runGrokSmoke(context.Background(), path, "grok 1.0.13")
	})
	if result.Status != cliSmokeStatusFailed {
		t.Fatalf("expected a failure: %+v", result)
	}

	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []struct{ name, text string }{{"result", string(payload)}, {"log", logged}} {
		for _, banned := range []string{
			"xai-credential-sentinel", "xai-key-sentinel", "refresh-token-sentinel",
			"auth.json", "Bearer", "unexpected argument", "--no-subagents",
			persistent, isolated, promptFile, grokMaintenanceSmokePromptPrefix, grokSmokeMarkerPrefix,
		} {
			if banned != "" && strings.Contains(surface.text, banned) {
				t.Errorf("%s leaked %q: %s", surface.name, banned, surface.text)
			}
		}
	}
	if !strings.Contains(logged, "[cli-smoke] grok shape=") || !strings.Contains(logged, "stderrBytes=") {
		t.Errorf("failure log line missing its closed-value vocabulary: %q", logged)
	}
	// The published payload has no field that could carry output at all.
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	for _, k := range []string{"stdout", "stderr", "output", "stderrTail", "prompt", "marker", "argv"} {
		if _, present := fields[k]; present {
			t.Errorf("result payload carries a %q field", k)
		}
	}
}

func TestGrokSmokeFailureLogLine_CarriesOnlyClosedValues(t *testing.T) {
	line := grokSmokeFailureLogLine(grokSmokeArgvShapes[0].ID, cliUsageErrorProtocol, cliSmokeDiagnosticFlagRejected, 512)
	want := fmt.Sprintf("[cli-smoke] grok shape=%s category=protocol diagnostic=flag_rejected stderrBytes=512", grokSmokeArgvShapes[0].ID)
	if !strings.Contains(line, want) {
		t.Fatalf("log line = %q, want it to contain %q", line, want)
	}
}

// The version precheck takes the SAME cmd.exe route as the inference launch,
// so a `grok.cmd` npm shim answers --version instead of failing CreateProcess
// and reporting binary_missing. Pure-string half, runs everywhere.
func TestGrokSmokeShimScript_RendersTheVersionPrecheck(t *testing.T) {
	script, ok := grokSmokeShimScript([]string{"--version"})
	if !ok {
		t.Fatal("the version precheck must be renderable through the shim route")
	}
	if script != `"%`+grokSmokeShimPathEnv+`%" --version` {
		t.Fatalf("version script = %q", script)
	}
}

// The Windows shim script is rendered from the ladder's fixed tokens only; the
// two paths travel through the environment, and any token outside the flag
// charset refuses the route. Pure-string assertions that run on every OS.
func TestGrokSmokeShimScript_RendersFixedTokensAndKeepsPathsOutOfTheScript(t *testing.T) {
	for _, shape := range grokSmokeArgvShapes {
		promptFile := `C:\Users\Some One\AppData\Local\Temp\grok & (prompt)!.txt`
		args := buildGrokNoToolsSmokeArgs(shape, promptFile)
		script, ok := grokSmokeShimScript(args)
		if !ok {
			t.Fatalf("rung %s refused by the shim renderer", shape.ID)
		}
		if !strings.HasPrefix(script, `"%`+grokSmokeShimPathEnv+`%"`) {
			t.Errorf("rung %s script does not begin with the env-indirected shim path: %q", shape.ID, script)
		}
		// `call` would re-expand a percent sequence the shim or prompt path
		// legitimately contains, mangling it before the shim ever runs.
		if strings.Contains(script, "call ") {
			t.Errorf("rung %s script reintroduced CALL's second percent expansion: %q", shape.ID, script)
		}
		if strings.Contains(script, promptFile) || strings.Contains(script, "Some One") {
			t.Errorf("rung %s script interpolated the prompt path: %q", shape.ID, script)
		}
		if !strings.HasSuffix(script, grokSmokePromptFileFlag+` "%`+grokSmokeShimPromptEnv+`%"`) {
			t.Errorf("rung %s script does not pass the prompt path via env: %q", shape.ID, script)
		}
		for _, arg := range args[:len(args)-2] {
			if !strings.Contains(" "+script+" ", " "+arg+" ") {
				t.Errorf("rung %s script dropped token %q: %q", shape.ID, arg, script)
			}
		}
		if strings.Contains(script, `""`) {
			t.Errorf("rung %s script carries an empty quoted operand: %q", shape.ID, script)
		}
	}
	for _, bad := range [][]string{
		{"--output-format=streaming-json", "--tools=", "", "--max-turns=1", grokSmokePromptFileFlag, "p"},
		{"--output-format=streaming-json", "-p", "marker", grokSmokePromptFileFlag, "p"},
		{"--output-format=streaming-json", "--tools=a b", grokSmokePromptFileFlag, "p"},
		{"--output-format=streaming-json", `--tools="`, grokSmokePromptFileFlag, "p"},
		{"--output-format=streaming-json", "--tools=&calc", grokSmokePromptFileFlag, "p"},
	} {
		if script, ok := grokSmokeShimScript(bad); ok {
			t.Errorf("shim renderer accepted %#v → %q", bad, script)
		}
	}
}

// Every Grok --version probe must funnel through grokProbeVersion. The version
// cache is keyed on (path, mtime, size) alone, so a second Grok probe that
// launched the binary directly would, on a Windows `grok.cmd` npm shim, cache
// its own failed "" under the very key the shim-aware probe reads back — and
// the smoke would report binary_missing without ever spawning cmd.exe. Source
// scan because the poisoning caller (gatherCLIAgents) cannot be exercised for
// a shim on a Linux CI runner; the behaviour half is windows-tagged.
func TestGrokVersionProbes_AllRouteThroughTheShimAwareProbe(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "cliagent_smoke_grok.go" {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "sanitizeGrokMaintenanceSmokeEnv") {
				continue
			}
			for _, direct := range []string{"probeVersionArgsWithEnv(", "cachedProbeVersionWithEnv("} {
				if strings.Contains(line, direct) {
					t.Errorf("%s:%d probes a Grok version via %s; call grokProbeVersion so a Windows .cmd shim cannot cache a direct-launch failure: %s",
						file, i+1, direct, strings.TrimSpace(line))
				}
			}
		}
	}
}
