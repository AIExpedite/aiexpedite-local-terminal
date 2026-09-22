package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_codex_test.go — the Codex `__cli_smoke__` provider.
   --------------------------------------------------------------------------
   Every row except the real deadline kill drives the probe through the
   runCodexSmokeCommand seam, so no row spawns Codex or spends a turn.
   ------------------------------------------------------------------------ */

const codexSmokeTestVersion = "codex-cli 0.150.0"

// codexSmokeEnv isolates every side channel a Codex smoke touches — CODEX_HOME
// (credentials, the rollout tree), the rate-limit cache, the smoke caches — and
// records the utilization lifecycle calls instead of running reconciles.
func codexSmokeEnv(t *testing.T) (home string, rec *codexRunHookRecorder) {
	t.Helper()
	smokeEnv(t)
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "codex_rate_limits.json"))
	stubCodexLogin(t, true, true)
	return home, recordCodexRunHooks(t)
}

func stubCodexBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, codexSmokeTestVersion)
	return path
}

func stubCodexSmokePath(t *testing.T, path string) {
	t.Helper()
	original := resolveCodexSmokePath
	resolveCodexSmokePath = func() string { return path }
	t.Cleanup(func() { resolveCodexSmokePath = original })
}

func stubCodexLogin(t *testing.T, loggedIn, known bool) *int {
	t.Helper()
	calls := 0
	original := codexAuthStatusProbe
	codexAuthStatusProbe = func(context.Context, string) (bool, bool) {
		calls++
		return loggedIn, known
	}
	t.Cleanup(func() { codexAuthStatusProbe = original })
	return &calls
}

func stubCodexSmokeExec(t *testing.T, fn func(ctx context.Context, launch codexSmokeLaunch) (stdout, stderr []byte, err error)) (*int, *[]codexSmokeLaunch) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	var launches []codexSmokeLaunch
	original := runCodexSmokeCommand
	runCodexSmokeCommand = func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		mu.Lock()
		calls++
		launches = append(launches, launch)
		mu.Unlock()
		return fn(ctx, launch)
	}
	t.Cleanup(func() { runCodexSmokeCommand = original })
	return &calls, &launches
}

// countCodexRolloutWalks counts the rollout-signal tree walks.
func countCodexRolloutWalks(t *testing.T) *int {
	t.Helper()
	walks := 0
	original := codexSmokeRolloutWalk
	codexSmokeRolloutWalk = func(ctx context.Context, root string, after time.Time) bool {
		walks++
		return original(ctx, root, after)
	}
	t.Cleanup(func() { codexSmokeRolloutWalk = original })
	return &walks
}

// codexMarkerFromLaunch recovers the per-run nonce from the prompt on stdin —
// the only place it appears.
func codexMarkerFromLaunch(t *testing.T, launch codexSmokeLaunch) string {
	t.Helper()
	idx := strings.Index(launch.Prompt, codexSmokeMarkerPrefix)
	if idx < 0 {
		t.Fatalf("prompt carries no marker")
	}
	return launch.Prompt[idx:]
}

// codexPreUpdateFrames is a healthy pre-update `exec --json` turn.
func codexPreUpdateFrames(text string) []byte {
	return []byte(strings.Join([]string{
		`{"type":"thread.started","thread_id":"t-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"` + text + `"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n") + "\n")
}

// codexExitError is a real *exec.ExitError (non-zero exit).
func codexExitError(t *testing.T) error {
	t.Helper()
	return killedChildError(t)
}

func (r *codexRunHookRecorder) lifecycle() (started, settled, disarmed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started), len(r.settled), len(r.disarmed)
}

// writeCodexSmokeRollout drops a rollout file under CODEX_HOME/sessions with
// the given mtime.
func writeCodexSmokeRollout(t *testing.T, home, name string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "09", "22")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+name+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

/* --------------------------------------------------------------------------
   Verdicts
   -------------------------------------------------------------------------- */

// The preferred rung's evidence is the last-message file alone: a build whose
// JSON stream carries nothing this reader recognises still succeeds, and the
// success settles the run — it must never fall through to disarm.
func TestRunCodexSmoke_LastMessageFileAloneSucceeds(t *testing.T) {
	_, rec := codexSmokeEnv(t)
	path := stubCodexBinary(t)
	walks := countCodexRolloutWalks(t)
	calls, launches := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		if err := os.WriteFile(launch.LastMessageFile, []byte(codexMarkerFromLaunch(t, launch)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return []byte(`{"type":"some.future.event"}` + "\n"), nil, nil
	})

	result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion)

	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched || result.ArgvShapeID != codexSmokeArgvShapes[0].ID {
		t.Fatalf("result = %+v, want a first-rung success", result)
	}
	if *calls != 1 {
		t.Fatalf("spent %d children, want 1", *calls)
	}
	launch := (*launches)[0]
	for _, arg := range launch.Args {
		if strings.Contains(arg, codexSmokeMarkerPrefix) || strings.Contains(arg, "dangerously") {
			t.Fatalf("argv carries the marker or the bypass flag: %v", launch.Args)
		}
	}
	if launch.Args[len(launch.Args)-1] != "-" || !strings.Contains(strings.Join(launch.Args, " "), "--sandbox read-only") {
		t.Fatalf("argv = %v, want a read-only sandbox reading the prompt from stdin", launch.Args)
	}
	if _, err := os.Stat(launch.Dir); !os.IsNotExist(err) {
		t.Errorf("per-run temp dir (and its last-message file) survived the probe")
	}
	if started, settled, disarmed := rec.lifecycle(); started != 1 || settled != 1 || disarmed != 0 {
		t.Fatalf("lifecycle started=%d settled=%d disarmed=%d, want 1/1/0", started, settled, disarmed)
	}
	if *walks != 0 {
		t.Fatalf("a marker match settled, yet the rollout tree was walked %d times", *walks)
	}
}

// The folded stream alone is enough, in every post-update dialect and with no
// turn-level completion frame at all: `item.completed` is not a terminal event,
// so gating on one would fail a build that returned the exact marker.
func TestRunCodexSmoke_FoldedStreamWithoutCompletionFrameSucceeds(t *testing.T) {
	for name, frames := range map[string]func(marker string) string{
		"pre-update item.completed only": func(m string) string {
			return `{"type":"item.completed","item":{"type":"agent_message","text":"` + m + `"}}`
		},
		"post-update nested": func(m string) string {
			return `{"id":"0","msg":{"type":"agent_message","message":"` + m + `"}}`
		},
		"post-update JSON-RPC deltas": func(m string) string {
			half := len(m) / 2
			return `{"method":"item/agentMessage/delta","params":{"delta":"` + m[:half] + `"}}` + "\n" +
				`{"method":"item/agentMessage/delta","params":{"delta":"` + m[half:] + `"}}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, rec := codexSmokeEnv(t)
			path := stubCodexBinary(t)
			stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
				return []byte("Codex banner line\n" + frames(codexMarkerFromLaunch(t, launch)) + "\n"), nil, nil
			})

			result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion)

			if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
				t.Fatalf("result = %+v, want success from the folded stream", result)
			}
			if _, settled, disarmed := rec.lifecycle(); settled != 1 || disarmed != 0 {
				t.Fatalf("settled=%d disarmed=%d, want the match to settle", settled, disarmed)
			}
		})
	}
}

// One row per diagnostic in the closed set this provider can produce.
func TestClassifyCodexSmokeRun_MapsEveryOutcomeOntoTheClosedSet(t *testing.T) {
	const marker = codexSmokeMarkerPrefix + "0badc0de"
	exitErr := codexExitError(t)
	for _, tc := range []struct {
		name        string
		timedOut    bool
		stdout      string
		stderr      string
		lastMessage string
		runErr      error
		category    string
		diagnostic  string
	}{
		{name: "match from last-message", lastMessage: marker + "\n", stdout: `{"type":"turn.completed"}`},
		{name: "match from stream", stdout: string(codexPreUpdateFrames(marker))},
		{name: "timeout outranks a match", timedOut: true, stdout: string(codexPreUpdateFrames(marker)), runErr: exitErr,
			category: cliUsageErrorProviderTimeout, diagnostic: cliSmokeDiagnosticTimeout},
		{name: "flag rejected", runErr: exitErr, stderr: "error: unexpected argument '--output-last-message' found",
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFlagRejected},
		{name: "framing rejected", runErr: exitErr, stderr: "error: unexpected argument '--json' found",
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticFramingRejected},
		{name: "no envelope, non-zero", runErr: exitErr, stderr: "thread 'main' panicked",
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope},
		{name: "no envelope, clean exit", stdout: `{"type":"thread.started"}`,
			category: cliUsageErrorProtocol, diagnostic: cliSmokeDiagnosticNoEnvelope},
		{name: "auth error frame", stdout: `{"type":"error","message":"401 Unauthorized: token expired"}`,
			category: cliUsageErrorNotAuthenticated, diagnostic: cliSmokeDiagnosticAuthError},
		{name: "auth error on stderr", runErr: exitErr, stderr: "Error: Not logged in. Run codex login",
			category: cliUsageErrorNotAuthenticated, diagnostic: cliSmokeDiagnosticAuthError},
		{name: "provider error", stdout: `{"type":"turn.failed","error":{"message":"You've hit your usage limit"}}`,
			category: cliUsageErrorProviderUnavailable, diagnostic: cliSmokeDiagnosticProviderError},
		{name: "nested provider error", stdout: `{"method":"codex/event/error","params":{"msg":{"type":"error","message":"stream disconnected: status 503 overloaded"}}}`,
			category: cliUsageErrorProviderUnavailable, diagnostic: cliSmokeDiagnosticProviderError},
		{name: "chatty model", stdout: string(codexPreUpdateFrames("Sure! " + marker)),
			category: cliUsageErrorParseFailed, diagnostic: cliSmokeDiagnosticMarkerMismatch},
		{name: "completed with no text", stdout: `{"type":"turn.completed"}`,
			category: cliUsageErrorParseFailed, diagnostic: cliSmokeDiagnosticMarkerMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := parseCodexSmokeStream([]byte(tc.stdout))
			category, diagnostic, matched := classifyCodexSmokeRun(tc.timedOut, stream, []byte(tc.stderr), []byte(tc.lastMessage), tc.runErr, marker)
			if category != tc.category || diagnostic != tc.diagnostic || matched != (tc.category == "") {
				t.Fatalf("classify = (%q, %q, %v), want (%q, %q, %v)",
					category, diagnostic, matched, tc.category, tc.diagnostic, tc.category == "")
			}
			if !isKnownCLISmokeDiagnostic(diagnostic) {
				t.Fatalf("diagnostic %q is outside the closed set", diagnostic)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Pre-checks (no turn spent, nothing armed)
   -------------------------------------------------------------------------- */

func TestRunCodexSmoke_PrechecksSpendNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		noPath     bool
		version    string
		loggedIn   bool
		known      bool
		apiKey     string
		wantSpawn  bool
		diagnostic string
	}{
		{name: "no binary", noPath: true, version: codexSmokeTestVersion, loggedIn: true, known: true, diagnostic: cliSmokeDiagnosticBinaryMissing},
		{name: "no version", version: "", loggedIn: true, known: true, diagnostic: cliSmokeDiagnosticBinaryMissing},
		{name: "definite logout", version: codexSmokeTestVersion, known: true, diagnostic: cliSmokeDiagnosticNotLoggedIn},
		{name: "inconclusive login proceeds", version: codexSmokeTestVersion, wantSpawn: true, diagnostic: cliSmokeDiagnosticMarkerMismatch},
		{name: "logout with an API key proceeds", version: codexSmokeTestVersion, known: true, apiKey: "sk-test", wantSpawn: true, diagnostic: cliSmokeDiagnosticMarkerMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec := codexSmokeEnv(t)
			stubCodexLogin(t, tc.loggedIn, tc.known)
			t.Setenv("OPENAI_API_KEY", tc.apiKey)
			path := stubCodexBinary(t)
			if tc.noPath {
				path = ""
			}
			calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
				return codexPreUpdateFrames("not the marker"), nil, nil
			})

			result := runCodexSmoke(context.Background(), path, tc.version)

			if result.Diagnostic != tc.diagnostic {
				t.Fatalf("diagnostic = %q, want %q (%+v)", result.Diagnostic, tc.diagnostic, result)
			}
			if spawned := *calls > 0; spawned != tc.wantSpawn {
				t.Fatalf("spawned %d children, want spawn=%v", *calls, tc.wantSpawn)
			}
			if started, _, _ := rec.lifecycle(); (started > 0) != tc.wantSpawn {
				t.Fatalf("armed %d runs, want an arm only when a child is spawned", started)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Ladder + shape cache
   -------------------------------------------------------------------------- */

// A rung-1 rejection of the droppable flag walks to rung 2 — and the floor is
// touched exactly once, at the end: disarming mid-walk would race the
// succeeding rung and could roll the floor back after it owed its refresh.
func TestRunCodexSmoke_FlagRejectionWalksOnceAndTouchesTheFloorAtTheEnd(t *testing.T) {
	_, rec := codexSmokeEnv(t)
	path := stubCodexBinary(t)
	exitErr := codexExitError(t)
	calls, launches := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		if strings.Contains(strings.Join(launch.Args, " "), "--output-last-message") {
			return nil, []byte("error: unexpected argument '--output-last-message' found\n\nUsage: codex exec [OPTIONS] [PROMPT]"), exitErr
		}
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})

	result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion)

	if result.Status != cliSmokeStatusSuccess || result.ArgvShapeID != codexSmokeArgvShapes[1].ID {
		t.Fatalf("result = %+v, want a rung-2 success", result)
	}
	if *calls != 2 {
		t.Fatalf("spent %d children, want 2", *calls)
	}
	if strings.Contains(strings.Join((*launches)[1].Args, " "), "--output-last-message") {
		t.Fatalf("rung 2 still carries the rejected flag: %v", (*launches)[1].Args)
	}
	if started, settled, disarmed := rec.lifecycle(); started != 1 || settled != 1 || disarmed != 0 {
		t.Fatalf("lifecycle started=%d settled=%d disarmed=%d, want one arm and one settle", started, settled, disarmed)
	}

	// The resolved rung is remembered for THIS binary: the next smoke is one child.
	*calls = 0
	if again := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); again.Status != cliSmokeStatusSuccess || *calls != 1 {
		t.Fatalf("remembered shape: %d children, result %+v; want one child", *calls, again)
	}
	// A changed binary re-walks from the preferred rung.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n# upgraded\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	*calls = 0
	runCodexSmoke(context.Background(), path, codexSmokeTestVersion)
	if *calls != 2 {
		t.Fatalf("a replaced binary spent %d children, want the full walk (2)", *calls)
	}
}

// Only a rejection naming the droppable flag walks; any other failure is
// reported from the one child it cost.
func TestRunCodexSmoke_OnlyARetryableRejectionWalks(t *testing.T) {
	exitErr := codexExitError(t)
	for name, stderr := range map[string]string{
		"shared flag rejected": "error: unexpected argument '--sandbox' found",
		"framing rejected":     "error: unexpected argument '--json' found",
		"auth failure":         "Error: Not logged in",
		"crash":                "thread 'main' panicked",
	} {
		t.Run(name, func(t *testing.T) {
			codexSmokeEnv(t)
			path := stubCodexBinary(t)
			calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
				return nil, []byte(stderr), exitErr
			})
			if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Status == cliSmokeStatusSuccess {
				t.Fatalf("result = %+v, want a failure", result)
			}
			if *calls != 1 {
				t.Fatalf("spent %d children, want 1", *calls)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Utilization: settle on evidence, disarm otherwise
   -------------------------------------------------------------------------- */

// The real deadline-kill path: no seam, a real child that sleeps past a shrunk
// deadline. The kill surfaces as an *exec.ExitError — not a context error —
// and must still classify as `timeout` AND disarm the floor rather than leave
// it armed for the next start to adopt as an interrupted run.
func TestRunCodexSmoke_RealDeadlineKillIsTimeoutAndDisarms(t *testing.T) {
	_, rec := codexSmokeEnv(t)
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, testExe, codexSmokeTestVersion)
	t.Setenv(mockCLIEnvVar, "grok-smoke-hang") // blocks for minutes, holding its pipes
	original := codexSmokeTimeout
	codexSmokeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { codexSmokeTimeout = original })

	started := time.Now()
	result := runCodexSmoke(context.Background(), testExe, codexSmokeTestVersion)

	if result.ErrorCategory != cliUsageErrorProviderTimeout || result.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Fatalf("deadline kill → %+v, want provider_timeout/timeout", result)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the probe took %s; the deadline did not bound the child", elapsed)
	}
	if _, settled, disarmed := rec.lifecycle(); settled != 0 || disarmed != 1 {
		t.Fatalf("settled=%d disarmed=%d, want the killed run disarmed", settled, disarmed)
	}
}

// The rollout signal is a DELTA against the arm: a rollout that predates the
// arm settles nothing, one the run wrote does, and even that one is ignored
// while another Codex run of this process could have written it.
func TestRunCodexSmoke_RolloutSignalSettlesOnlyAWriteThisRunMade(t *testing.T) {
	exitClean := func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return []byte(`{"type":"thread.started"}` + "\n"), nil, nil // no_envelope: no text, no completion
	}
	t.Run("pre-existing rollout does not settle", func(t *testing.T) {
		home, rec := codexSmokeEnv(t)
		writeCodexSmokeRollout(t, home, "old", time.Now().Add(-time.Minute))
		path := stubCodexBinary(t)
		stubCodexSmokeExec(t, exitClean)
		if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Diagnostic != cliSmokeDiagnosticNoEnvelope {
			t.Fatalf("result = %+v, want no_envelope", result)
		}
		if _, settled, disarmed := rec.lifecycle(); settled != 0 || disarmed != 1 {
			t.Fatalf("settled=%d disarmed=%d, want a disarm", settled, disarmed)
		}
	})
	t.Run("a rollout written by the run settles", func(t *testing.T) {
		home, rec := codexSmokeEnv(t)
		writeCodexSmokeRollout(t, home, "old", time.Now().Add(-time.Minute))
		path := stubCodexBinary(t)
		stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
			writeCodexSmokeRollout(t, home, "this-run", time.Now().Add(time.Second))
			return exitClean(ctx, launch)
		})
		runCodexSmoke(context.Background(), path, codexSmokeTestVersion)
		if _, settled, disarmed := rec.lifecycle(); settled != 1 || disarmed != 0 {
			t.Fatalf("settled=%d disarmed=%d, want the new rollout to settle", settled, disarmed)
		}
	})
	t.Run("ignored while another Codex run is open", func(t *testing.T) {
		home, rec := codexSmokeEnv(t)
		path := stubCodexBinary(t)
		sm := NewSessionManager(nil)
		sibling := &CLISession{ID: "sibling-codex", Command: "codex"}
		sibling.codexUsageFloorMs.Store(time.Now().Add(-time.Minute).UnixMilli())
		sm.sessions[sibling.ID] = sibling
		previous := globalSessionManager
		globalSessionManager = sm
		t.Cleanup(func() { globalSessionManager = previous })
		stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
			writeCodexSmokeRollout(t, home, "sibling-or-this-run", time.Now().Add(time.Second))
			return exitClean(ctx, launch)
		})
		runCodexSmoke(context.Background(), path, codexSmokeTestVersion)
		if _, settled, disarmed := rec.lifecycle(); settled != 0 || disarmed != 1 {
			t.Fatalf("settled=%d disarmed=%d, want a disarm while a sibling run is open", settled, disarmed)
		}
	})
	t.Run("auth error disarms", func(t *testing.T) {
		_, rec := codexSmokeEnv(t)
		path := stubCodexBinary(t)
		exitErr := codexExitError(t)
		stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
			return nil, []byte("Error: 401 Unauthorized"), exitErr
		})
		if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Diagnostic != cliSmokeDiagnosticAuthError {
			t.Fatalf("result = %+v, want auth_error", result)
		}
		if _, settled, disarmed := rec.lifecycle(); settled != 0 || disarmed != 1 {
			t.Fatalf("settled=%d disarmed=%d, want a disarm", settled, disarmed)
		}
	})
	t.Run("a completion frame settles a mismatch without a walk", func(t *testing.T) {
		_, rec := codexSmokeEnv(t)
		walks := countCodexRolloutWalks(t)
		path := stubCodexBinary(t)
		stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
			return codexPreUpdateFrames("Sure thing!"), nil, nil
		})
		if result := runCodexSmoke(context.Background(), path, codexSmokeTestVersion); result.Diagnostic != cliSmokeDiagnosticMarkerMismatch {
			t.Fatalf("result = %+v, want marker_mismatch", result)
		}
		if _, settled, disarmed := rec.lifecycle(); settled != 1 || disarmed != 0 || *walks != 0 {
			t.Fatalf("settled=%d disarmed=%d walks=%d, want a walk-free settle", settled, disarmed, *walks)
		}
	})
}

/* --------------------------------------------------------------------------
   Cooldown
   -------------------------------------------------------------------------- */

// A pre-update verdict replays while the binary is unchanged and is NOT
// replayed once its version or (mtime, size) moves — the post-update smoke the
// harness runs must test the new binary.
func TestRunCLISmoke_CodexCooldownReplaysButNotAcrossAnUpgrade(t *testing.T) {
	codexSmokeEnv(t)
	path := stubCodexBinary(t)
	stubCodexSmokePath(t, path)
	calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})

	if result, replayed := runCLISmoke(context.Background(), "codex"); replayed || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("first smoke = %+v (replayed=%v), want a fresh success", result, replayed)
	}
	if _, replayed := runCLISmoke(context.Background(), "codex"); !replayed || *calls != 1 {
		t.Fatalf("second smoke replayed=%v with %d children, want a free replay", replayed, *calls)
	}

	// Version bump on the same bytes.
	seedProbeVersion(t, path, "codex-cli 0.151.0")
	if _, replayed := runCLISmoke(context.Background(), "codex"); replayed || *calls != 2 {
		t.Fatalf("after a version change replayed=%v children=%d, want a fresh run", replayed, *calls)
	}
	// Replaced bytes.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n# upgraded binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, "codex-cli 0.151.0")
	if _, replayed := runCLISmoke(context.Background(), "codex"); replayed || *calls != 3 {
		t.Fatalf("after a binary change replayed=%v children=%d, want a fresh run", replayed, *calls)
	}
}

/* --------------------------------------------------------------------------
   Retention
   -------------------------------------------------------------------------- */

func TestCodexSmokeFailureLogLine_CarriesOnlyClosedValues(t *testing.T) {
	line := codexSmokeFailureLogLine(codexSmokeArgvShapes[0].ID, cliUsageErrorProtocol, cliSmokeDiagnosticNoEnvelope, 4096)
	for _, want := range []string{"codex", codexSmokeArgvShapes[0].ID, "category=protocol", "diagnostic=no_envelope", "stderrBytes=4096"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q missing %q", line, want)
		}
	}
}

func TestCodexSmokeShimScript_RendersBothCodexArgvsAndRefusesTheRest(t *testing.T) {
	lastMessage := filepath.Join(`C:\Users\dev\%DEV%\tmp`, codexSmokeLastMessageName)
	script, ok := codexSmokeShimScript(buildCodexSmokeArgs(codexSmokeArgvShapes[0], lastMessage))
	if !ok {
		t.Fatal("the exec rung must render — a refusal silently falls back to a plain spawn of the shim")
	}
	want := `"%` + codexSmokeShimPathEnv + `%" exec --json --sandbox read-only --skip-git-repo-check --output-last-message "%` +
		codexSmokeShimLastMessageEnv + `%" -`
	if script != want {
		t.Fatalf("script = %s\nwant     %s", script, want)
	}
	if strings.Contains(script, lastMessage) || strings.Contains(script, "%DEV%") {
		t.Fatalf("the last-message path leaked into the script: %s", script)
	}
	if script, ok := codexSmokeShimScript([]string{"login", "status"}); !ok || script != `"%`+codexSmokeShimPathEnv+`%" login status` {
		t.Fatalf("login status script = (%q, %v)", script, ok)
	}
	for _, bad := range [][]string{
		{"exec", "hello world"},
		{"exec", "--json", "&", "calc"},
		{"review"},
		{"exec", `"quoted"`},
	} {
		if _, ok := codexSmokeShimScript(bad); ok {
			t.Errorf("script renderer accepted %q", bad)
		}
	}
}
