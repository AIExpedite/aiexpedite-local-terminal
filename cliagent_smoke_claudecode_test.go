package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_claudecode_test.go
   --------------------------------------------------------------------------
   Every case drives the exec seam, so no test ever spawns a real `claude` or
   spends an inference turn. The two invariants worth stating up front:
     - a broken INVOCATION CONTRACT is `protocol` (the regression this feature
       exists for), while a working CLI that answered something unexpected is
       `parse_failed`;
     - the pre-checks must short-circuit BEFORE the seam runs, because that is
       what proves no quota is spent on a device with no binary or no login.
   ------------------------------------------------------------------------ */

// stubClaudeBinary writes a fake claude binary and returns its path. It is
// never executed — the exec seam and the version probe are both stubbed — so
// it deliberately carries no .exe extension: nothing should be able to run it
// by accident, on any platform.
func stubClaudeBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubSmokePath points the smoke's binary resolver at the given path, so a
// case never depends on whichever real `claude` the process-wide resolver
// cached first.
func stubSmokePath(t *testing.T, path string) {
	t.Helper()
	original := resolveClaudeSmokePath
	resolveClaudeSmokePath = func() string { return path }
	t.Cleanup(func() { resolveClaudeSmokePath = original })
}

// seedProbeVersion pins what `<path> --version` reports without executing the
// stub. cachedProbeVersion keys on (path, mtime, size), so seeding that key is
// exactly equivalent to a successful probe — and rewriting the stub file
// invalidates it, which is how the upgrade case below is simulated.
func seedProbeVersion(t *testing.T, path, version string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	key := versionProbeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
	versionProbeMu.Lock()
	versionProbeCache[key] = version
	versionProbeMu.Unlock()
}

// smokeEnv isolates every on-disk side channel a smoke run could touch and
// clears the in-memory caches so cases cannot leak into each other.
func smokeEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(dir, "rl.json"))
	t.Setenv("AIEXPEDITE_CLAUDE_STATUSLINE_PREV", filepath.Join(dir, "prev.json"))
	resetClaudeSmokeState()
	resetVersionProbeCache()
	t.Cleanup(func() {
		resetClaudeSmokeState()
		resetVersionProbeCache()
	})
}

// stubSmokeExec replaces the exec seam with a scripted responder and returns a
// pointer to the call count.
func stubSmokeExec(t *testing.T, fn func(ctx context.Context, args []string, prompt string) (stdout, stderr []byte, err error)) *int {
	t.Helper()
	calls := 0
	original := runClaudeSmokeCommand
	runClaudeSmokeCommand = func(ctx context.Context, path string, args []string, env []string, prompt string) ([]byte, []byte, error) {
		calls++
		return fn(ctx, args, prompt)
	}
	t.Cleanup(func() { runClaudeSmokeCommand = original })
	return &calls
}

// stubAuthProbe pins the auth pre-check and counts its calls.
func stubAuthProbe(t *testing.T, loggedIn, known bool) {
	t.Helper()
	original := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(path string) (bool, bool) { return loggedIn, known }
	t.Cleanup(func() { claudeAuthStatusProbe = original })
}

// successEnvelope is what `--print --output-format json` emits on a healthy
// turn, trimmed to the fields the classifier reads.
func successEnvelope(result string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"result": result, "num_turns": 1,
	})
	return payload
}

// markerFromPrompt recovers the nonce the probe generated for this run. The
// prompt is the ONLY place it appears, which is itself the point.
func markerFromPrompt(prompt string) string {
	idx := strings.Index(prompt, claudeSmokeMarkerPrefix)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(prompt[idx:])
}

func TestRunClaudeCodeSmoke_ExactMarkerSucceeds(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251 (Claude Code)")

	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
		t.Fatalf("healthy CLI must pass the smoke: %+v", result)
	}
	if result.ErrorCategory != "" {
		t.Errorf("successful smoke must carry no error category: %q", result.ErrorCategory)
	}
	if result.ArgvShapeID != claudeArgvShapes[0].ID {
		t.Errorf("argvShapeId = %q, want the preferred shape", result.ArgvShapeID)
	}
	if *calls != 1 {
		t.Errorf("healthy smoke spent %d turns, want exactly 1", *calls)
	}
}

func TestRunClaudeCodeSmoke_FramingRejectionIsProtocol(t *testing.T) {
	// The exact regression: claude exits 1 complaining about the stream-json
	// framing contract and never emits a result envelope.
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return nil,
			[]byte("Error: --input-format=stream-json requires output-format=stream-json.\n"),
			errors.New("exit status 1")
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")

	if result.ErrorCategory != cliUsageErrorProtocol {
		t.Fatalf("errorCategory = %q, want protocol: %+v", result.ErrorCategory, result)
	}
	if result.Status != cliSmokeStatusFailed || result.MarkerMatched {
		t.Errorf("framing rejection must fail without a marker match: %+v", result)
	}
	if result.Diagnostic != cliSmokeDiagnosticFramingRejected {
		t.Errorf("diagnostic = %q, want framing_rejected", result.Diagnostic)
	}
}

func TestRunClaudeCodeSmoke_ChattyAnswerIsParseFailedNotProtocol(t *testing.T) {
	// A model that prepends "Sure! " is a healthy CLI. Reporting `protocol`
	// here would make the maintenance flow chase an invocation bug that does
	// not exist.
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope("Sure! " + markerFromPrompt(prompt)), nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")

	if result.ErrorCategory != cliUsageErrorParseFailed {
		t.Fatalf("errorCategory = %q, want parse_failed: %+v", result.ErrorCategory, result)
	}
	if result.MarkerMatched {
		t.Error("markerMatched must be false when the text is not exactly the marker")
	}
}

func TestRunClaudeCodeSmoke_MissingBinarySpendsNothing(t *testing.T) {
	smokeEnv(t)
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		t.Fatal("exec seam must not run without a binary")
		return nil, nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), "", "")

	if result.ErrorCategory != cliUsageErrorProviderUnavailable {
		t.Fatalf("errorCategory = %q, want provider_unavailable", result.ErrorCategory)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times for an absent binary", *calls)
	}
}

func TestRunClaudeCodeSmoke_LoggedOutSpendsNoTurn(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, false, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		t.Fatal("exec seam must not run for a logged-out CLI")
		return nil, nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")

	if result.ErrorCategory != cliUsageErrorNotAuthenticated {
		t.Fatalf("errorCategory = %q, want not_authenticated", result.ErrorCategory)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times while logged out — that is user quota", *calls)
	}
}

func TestRunClaudeCodeSmoke_InconclusiveAuthStillProbes(t *testing.T) {
	// An env-credential login reports "unknown" from `auth status`. Refusing
	// there would report a broken CLI on a working device.
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, false, false)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")

	if result.Status != cliSmokeStatusSuccess || *calls != 1 {
		t.Fatalf("inconclusive auth must still smoke: %+v (calls=%d)", result, *calls)
	}
}

func TestRunClaudeCodeSmoke_CancelledContextIsProviderTimeout(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return nil, nil, context.DeadlineExceeded
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := runClaudeCodeSmoke(ctx, path, "2.1.251")

	if result.ErrorCategory != cliUsageErrorProviderTimeout {
		t.Fatalf("errorCategory = %q, want provider_timeout", result.ErrorCategory)
	}
}

func TestRunClaudeCodeSmoke_ResolvedShapeIsCachedPerBinary(t *testing.T) {
	// First run: the preferred shape is rejected, the fallback succeeds — two
	// execs. Every later run on the SAME binary must spend exactly one.
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		for _, a := range args {
			if a == "--strict-mcp-config" {
				return nil, []byte("error: unknown option '--strict-mcp-config'\n"), errors.New("exit status 1")
			}
		}
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	first := runClaudeCodeSmoke(context.Background(), path, "2.1.251")
	if first.Status != cliSmokeStatusSuccess || first.ArgvShapeID != claudeArgvShapes[1].ID {
		t.Fatalf("ladder should settle on the fallback shape: %+v", first)
	}
	if *calls != 2 {
		t.Fatalf("first run made %d execs, want 2 (one per ladder rung)", *calls)
	}

	if second := runClaudeCodeSmoke(context.Background(), path, "2.1.251"); second.Status != cliSmokeStatusSuccess {
		t.Fatalf("second run failed: %+v", second)
	}
	third := runClaudeCodeSmoke(context.Background(), path, "2.1.251")
	if third.ArgvShapeID != claudeArgvShapes[1].ID {
		t.Errorf("cached shape not reused: %+v", third)
	}
	if *calls != 4 {
		t.Errorf("cached runs made %d execs total, want 4 (2 + 1 + 1)", *calls)
	}
}

// The published result must contain NO text the CLI authored. Capping and
// redacting a stderr tail was the previous approach and it is not sound: a
// denylist can only remove the secret shapes it anticipates, so a settings
// fragment, a private path, or an unfamiliar credential format rides along
// unchanged. The result now carries a diagnostic CODE we chose, and this test
// pins that — including secrets the regex redactor would NOT have caught.
func TestRunClaudeCodeSmoke_ResultCarriesNoCLIAuthoredText(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	dirty := strings.Join([]string{
		"Authorization: Bearer sk-ant-oat01-abcdef0123456789",
		"api_key=sk-ant-secret-value-9876543210",
		"blob " + strings.Repeat("A1b2C3d4", 12),
		`failed reading C:\Users\dkupi\.claude\.credentials.json`,
		// Deliberately NOT matched by any redaction pattern — short secrets,
		// raw config, and a private path with no known credential marker.
		`{"password":"hunter2","statusLine":{"command":"/Users/dkupi/private/bin/x"}}`,
		"loaded settings from /home/dkupi/work/secret-project/.claude/settings.json",
		"deploy_key=ABC123",
	}, "\n")
	var seenPrompt string
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		seenPrompt = prompt
		return nil, []byte(dirty), errors.New("exit status 1")
	})

	result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)

	// Every string the payload carries must be one WE authored: a closed-set
	// member, the fixed shape id, or the version line the CLI Agents card
	// already publishes. Anything else is vendor text that got through.
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"claudeCode": true, "2.1.251": true,
		cliSmokeStatusSuccess: true, cliSmokeStatusFailed: true,
		cliUsageErrorProtocol: true, cliUsageErrorProviderUnavailable: true,
		cliUsageErrorNotAuthenticated: true, cliUsageErrorProviderTimeout: true,
		cliUsageErrorParseFailed: true, cliUsageErrorInternal: true,
	}
	for _, shape := range claudeArgvShapes {
		allowed[shape.ID] = true
	}
	for key, value := range fields {
		str, isString := value.(string)
		if !isString {
			continue
		}
		if key == "diagnostic" {
			if !isKnownCLISmokeDiagnostic(str) {
				t.Errorf("diagnostic %q is not a member of the closed set", str)
			}
			continue
		}
		if !allowed[str] {
			t.Errorf("field %q carries text we did not author: %q", key, str)
		}
	}

	// And no token of the child's stderr — redactable or not — may appear.
	// Tokens that collide with our own vocabulary (e.g. the word "failed",
	// which is also our status value) are skipped: the field-level check above
	// is what proves those are ours.
	for _, line := range strings.Split(dirty, "\n") {
		for _, token := range strings.Fields(line) {
			if len(token) < 6 || allowed[token] {
				continue
			}
			if strings.Contains(text, token) {
				t.Errorf("published smoke result leaked CLI text %q: %s", token, text)
			}
		}
	}
	if marker := markerFromPrompt(seenPrompt); marker == "" || strings.Contains(text, marker) {
		t.Errorf("marker nonce must never be published (marker=%q): %s", marker, text)
	}
	for _, argvish := range []string{
		"--print", "--output-format", "--mcp-config", "mcpServers",
		"settings.json", ".claude.json", "Reply with exactly", "stderrTail",
	} {
		if strings.Contains(text, argvish) {
			t.Errorf("published smoke result leaked %q: %s", argvish, text)
		}
	}
	// What it DOES carry is a code from the closed set.
	if !isKnownCLISmokeDiagnostic(result.Diagnostic) {
		t.Fatalf("diagnostic %q is not a member of the closed set", result.Diagnostic)
	}
}

// isKnownCLISmokeDiagnostic pins the closed diagnostic set. A new code must be
// added here deliberately — the point of the set is that a consumer can enumerate
// it, and that nothing derived from vendor bytes can enter it.
func isKnownCLISmokeDiagnostic(diagnostic string) bool {
	switch diagnostic {
	case cliSmokeDiagnosticNone, cliSmokeDiagnosticFlagRejected,
		cliSmokeDiagnosticFramingRejected, cliSmokeDiagnosticNoEnvelope,
		cliSmokeDiagnosticAuthError, cliSmokeDiagnosticProviderError,
		cliSmokeDiagnosticMarkerMismatch, cliSmokeDiagnosticTimeout,
		cliSmokeDiagnosticBinaryMissing, cliSmokeDiagnosticNotLoggedIn,
		cliSmokeDiagnosticInternal, cliSmokeDiagnosticUnknownCLI:
		return true
	}
	return false
}

func TestParseClaudePrintResultEnvelope_ToleratesBannerLine(t *testing.T) {
	stdout := append([]byte("Some npm notice\n"), successEnvelope("hello")...)
	envelope, ok := parseClaudePrintResultEnvelope(stdout)
	if !ok || envelope.Result != "hello" {
		t.Fatalf("banner-prefixed stdout should still yield the result envelope: %+v (%v)", envelope, ok)
	}
	if _, ok := parseClaudePrintResultEnvelope([]byte("not json at all")); ok {
		t.Error("non-JSON stdout must not parse as an envelope")
	}
	if _, ok := parseClaudePrintResultEnvelope(nil); ok {
		t.Error("empty stdout must not parse as an envelope")
	}
}

func TestRunCLISmoke_UnknownCliIDIsProviderUnavailable(t *testing.T) {
	smokeEnv(t)
	stubSmokePath(t, "")
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		t.Fatal("unknown cliId must never spawn anything")
		return nil, nil, nil
	})

	result, cached := runCLISmoke(context.Background(), "someFutureCLI")

	if result.ErrorCategory != cliUsageErrorProviderUnavailable || cached {
		t.Fatalf("unknown cliId = %+v (cached=%v), want provider_unavailable", result, cached)
	}
	if result.CliID != "someFutureCLI" {
		t.Errorf("result must echo the requested cliId, got %q", result.CliID)
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times for an unknown cliId", *calls)
	}
}

func TestRunCLISmoke_CooldownReplaysVerdictButNotAcrossAnUpgrade(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.247 (Claude Code)")
	if _, cached := runCLISmoke(context.Background(), "claudeCode"); cached {
		t.Fatal("first smoke must actually run")
	}
	result, cached := runCLISmoke(context.Background(), "claudeCode")
	if !cached || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("second smoke inside the cooldown must replay the verdict: %+v (cached=%v)", result, cached)
	}
	if *calls != 1 {
		t.Fatalf("cooldown spent %d turns, want 1 — this is the user's own quota", *calls)
	}

	// An upgrade lands: same path, different bytes. The post-update smoke is
	// the whole point of the maintenance flow and must NOT be answered from
	// the pre-update cache.
	if err := os.WriteFile(path, []byte("stub-upgraded-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	resetVersionProbeCache()
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")
	if _, cached := runCLISmoke(context.Background(), "claudeCode"); cached {
		t.Fatal("a changed binary must bypass the cooldown")
	}
	if *calls != 2 {
		t.Fatalf("post-upgrade smoke made %d execs, want 2 total", *calls)
	}
	if cliSmokeCooldown < 10*time.Minute {
		t.Errorf("cooldown of %v is too short to bound a retry storm", cliSmokeCooldown)
	}
}

// killedChildError returns an error shaped like the one exec.CommandContext
// reports when its deadline kills the child: an *exec.ExitError, NOT an error
// wrapping context.DeadlineExceeded. Produced by genuinely failing a child (the
// test binary with an unknown flag) so the shape is real. If this environment
// forbids spawning at all, it falls back to the bare "signal: killed" text a
// killed child produces — the assertion holds either way, because neither
// satisfies errors.Is against a context error, which is the whole point.
func killedChildError(t *testing.T) error {
	t.Helper()
	err := exec.Command(os.Args[0], "-test.unknown-flag-for-exit-error").Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr
	}
	t.Logf("no real *exec.ExitError available here (%v); using the plain killed-child error", err)
	return errors.New("signal: killed")
}

// The per-attempt deadline is the ONLY thing that knows a slow turn was cut
// short: the killed child reports an *exec.ExitError, and the parent context is
// still healthy. Reading the parent classified a 60-second timeout as
// `protocol` AND sent the ladder off to burn another 60 seconds.
func TestRunClaudeCodeSmoke_AttemptDeadlineKillIsProviderTimeout(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)

	originalTimeout := claudeSmokeTimeout
	claudeSmokeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { claudeSmokeTimeout = originalTimeout })

	killed := killedChildError(t)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		<-ctx.Done() // the attempt deadline fires and would kill the child
		return nil, []byte("signal: killed\n"), killed
	})

	parent := context.Background() // deliberately healthy
	result := runClaudeCodeSmoke(parent, path, "2.1.251")

	if parent.Err() != nil {
		t.Fatal("the parent context must stay healthy for this test to mean anything")
	}
	if errors.Is(killed, context.DeadlineExceeded) || errors.Is(killed, context.Canceled) {
		t.Fatal("the simulated child error must not be a context error")
	}
	if result.ErrorCategory != cliUsageErrorProviderTimeout {
		t.Fatalf("errorCategory = %q, want provider_timeout: %+v", result.ErrorCategory, result)
	}
	if result.Diagnostic != cliSmokeDiagnosticTimeout {
		t.Errorf("diagnostic = %q, want timeout", result.Diagnostic)
	}
	if *calls != 1 {
		t.Fatalf("a timed-out attempt made %d execs; the ladder must not retry it", *calls)
	}
}

// The one-turn bound: only a rejection of a flag a later rung DROPS may be
// retried, because that rejection happens during option parsing, before any
// inference. Every other protocol failure may already have consumed the user's
// turn.
func TestRunClaudeCodeSmoke_OnlyFlagRejectionSpendsASecondTurn(t *testing.T) {
	cases := []struct {
		name       string
		stderr     string
		stdout     []byte
		wantCalls  int
		wantDiag   string
		wantResult string
	}{
		{
			name:       "unknown mcp flag retries the rung that drops it",
			stderr:     "error: unknown option '--strict-mcp-config'\n",
			wantCalls:  2,
			wantDiag:   cliSmokeDiagnosticFlagRejected,
			wantResult: cliUsageErrorProtocol,
		},
		{
			name:       "framing rejection stops after one turn",
			stderr:     "Error: --input-format=stream-json requires output-format=stream-json.\n",
			wantCalls:  1,
			wantDiag:   cliSmokeDiagnosticFramingRejected,
			wantResult: cliUsageErrorProtocol,
		},
		{
			name:       "malformed output stops after one turn",
			stdout:     []byte("not json at all"),
			stderr:     "",
			wantCalls:  1,
			wantDiag:   cliSmokeDiagnosticNoEnvelope,
			wantResult: cliUsageErrorProtocol,
		},
		{
			name:       "undocumented error envelope stops after one turn",
			stdout:     []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"something went wrong"}`),
			wantCalls:  1,
			wantDiag:   cliSmokeDiagnosticNoEnvelope,
			wantResult: cliUsageErrorProtocol,
		},
		{
			name:       "rejection of a flag EVERY rung carries is not retryable",
			stderr:     "error: unknown option '--tools'\n",
			wantCalls:  1,
			wantDiag:   cliSmokeDiagnosticNoEnvelope,
			wantResult: cliUsageErrorProtocol,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			smokeEnv(t)
			path := stubClaudeBinary(t)
			stubAuthProbe(t, true, true)
			calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
				return tc.stdout, []byte(tc.stderr), errors.New("exit status 1")
			})

			result := runClaudeCodeSmoke(context.Background(), path, "2.1.251")

			if *calls != tc.wantCalls {
				t.Errorf("made %d execs, want %d — each one is a turn off the user's quota", *calls, tc.wantCalls)
			}
			if result.Diagnostic != tc.wantDiag {
				t.Errorf("diagnostic = %q, want %q", result.Diagnostic, tc.wantDiag)
			}
			if result.ErrorCategory != tc.wantResult {
				t.Errorf("errorCategory = %q, want %q", result.ErrorCategory, tc.wantResult)
			}
		})
	}
}

// The retry gate reads the flags the ladder actually drops, derived from the
// shapes themselves. A rung added (or the fallback changed) without updating a
// hand-written list is the drift this guards against.
func TestClaudeRetryableArgvFlags_DerivedFromTheLadder(t *testing.T) {
	if len(claudeRetryableArgvFlags) == 0 {
		t.Fatal("the ladder drops flags but none were derived as retryable")
	}
	fallback := buildClaudeNonInteractivePrintArgs(claudeArgvShapes[len(claudeArgvShapes)-1])
	for _, flag := range claudeRetryableArgvFlags {
		if flag != strings.ToLower(flag) {
			t.Errorf("retryable flag %q must be lowercased for case-insensitive matching", flag)
		}
		if argvContains(fallback, flag) {
			t.Errorf("%q is carried by the fallback rung, so retrying cannot fix it", flag)
		}
	}
	// Flags every rung carries must never be treated as retryable.
	for _, always := range []string{"--print", "--output-format", "--tools", "--max-turns"} {
		for _, flag := range claudeRetryableArgvFlags {
			if flag == always {
				t.Errorf("%q is on every rung and must not be retryable", always)
			}
		}
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns whatever
// was written. Used to prove what actually reaches this device's log, rather
// than trusting the call site to be well-behaved.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, reader)
		done <- buf.String()
	}()

	fn()

	os.Stdout = original
	_ = writer.Close()
	captured := <-done
	_ = reader.Close()
	return captured
}

// The device's own log is NOT a safe place for vendor text either: it rotates
// to disk and is uploadable on request, and the denylist redactor cannot see a
// raw config fragment, a private path, a short password, or an unfamiliar
// credential format. A failing probe must therefore leave the child's stderr
// nowhere at all — not on the wire, not in the log.
func TestRunClaudeCodeSmoke_FailureLogsNoVendorText(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	dirty := strings.Join([]string{
		"Authorization: Bearer sk-ant-oat01-abcdef0123456789",
		`{"password":"hunter2","statusLine":{"command":"/Users/dkupi/private/bin/x"}}`,
		"loaded settings from /home/dkupi/work/secret-project/.claude/settings.json",
		"deploy_key=ABC123",
		"unknown option '--strict-mcp-config'", // also drives the retry rung
	}, "\n")
	stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return nil, []byte(dirty), errors.New("exit status 1")
	})

	var result cliSmokeResult
	logged := captureStdout(t, func() {
		result = runClaudeCodeSmoke(context.Background(), path, "2.1.251")
	})

	if result.Status != cliSmokeStatusFailed {
		t.Fatalf("expected a failed probe, got %+v", result)
	}
	if logged == "" {
		t.Fatal("expected a device-local diagnostic line for a failed probe")
	}
	for _, line := range strings.Split(dirty, "\n") {
		for _, token := range strings.Fields(line) {
			if len(token) < 6 {
				continue
			}
			if strings.Contains(logged, token) {
				t.Errorf("device log leaked CLI stderr %q:\n%s", token, logged)
			}
		}
	}
	// Not even a redaction placeholder: nothing derived from the bytes is kept.
	for _, banned := range []string{"REDACTED", "stderr=", "hunter2", "settings.json", "/Users/", "/home/"} {
		if strings.Contains(logged, banned) {
			t.Errorf("device log contains %q, so vendor text is still being retained:\n%s", banned, logged)
		}
	}
	// What it DOES contain: closed values and a length.
	for _, want := range []string{"[cli-smoke]", "category=" + cliUsageErrorProtocol, "diagnostic=", "stderrBytes="} {
		if !strings.Contains(logged, want) {
			t.Errorf("device log missing %q:\n%s", want, logged)
		}
	}
}

// The log renderer takes a LENGTH, not bytes. A function that cannot receive
// vendor text cannot leak it however a future caller wires it up — this pins
// that signature along with the exact vocabulary it emits.
func TestClaudeSmokeFailureLogLine_CarriesOnlyClosedValues(t *testing.T) {
	line := claudeSmokeFailureLogLine(claudeArgvShapes[0].ID, cliUsageErrorProtocol, cliSmokeDiagnosticFramingRejected, 4096)

	for _, want := range []string{
		"shape=" + claudeArgvShapes[0].ID,
		"category=" + cliUsageErrorProtocol,
		"diagnostic=" + cliSmokeDiagnosticFramingRejected,
		"stderrBytes=4096",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q: %q", want, line)
		}
	}
	// Every remaining token must be one we authored: the tag, the colour codes,
	// and the key=value pairs above. Nothing else may appear.
	fields := strings.Fields(strings.NewReplacer(colorYellow, "", colorReset, "").Replace(line))
	for _, field := range fields {
		switch {
		case field == "[cli-smoke]", field == "claudeCode",
			strings.HasPrefix(field, "shape="), strings.HasPrefix(field, "category="),
			strings.HasPrefix(field, "diagnostic="), strings.HasPrefix(field, "stderrBytes="):
		default:
			t.Errorf("unexpected token %q in the device log line: %q", field, line)
		}
	}
}

/* --------------------------------------------------------------------------
   Quota guards: concurrency and cache admission
   --------------------------------------------------------------------------
   The cooldown alone only bounds SEQUENTIAL callers. Pub/Sub hands the agent
   several outstanding messages at once, so these cases pin the two properties
   that make "one inference turn per smoke" true under a real retry burst.
   ------------------------------------------------------------------------ */

// stubConcurrentSmokeExec is the race-safe sibling of stubSmokeExec: it counts
// under a mutex and blocks the first caller on a gate so a burst is genuinely
// in flight at the same moment rather than accidentally serialising.
func stubConcurrentSmokeExec(t *testing.T, gate <-chan struct{}) func() int {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	original := runClaudeSmokeCommand
	runClaudeSmokeCommand = func(ctx context.Context, path string, args []string, env []string, prompt string) ([]byte, []byte, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first && gate != nil {
			<-gate
		}
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	}
	t.Cleanup(func() { runClaudeSmokeCommand = original })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

func TestRunCLISmoke_ConcurrentCallersShareOneInferenceTurn(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubAuthProbe(t, true, true)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")

	// The leader parks inside the exec seam until every follower has had time
	// to reach runCLISmoke, which is the exact interleaving the cooldown alone
	// cannot survive: without the singleflight each of these observes the same
	// cache miss and spends its own turn on the user's subscription window.
	gate := make(chan struct{})
	callCount := stubConcurrentSmokeExec(t, gate)

	const callers = 8
	results := make([]cliSmokeResult, callers)
	replayed := make([]bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], replayed[i] = runCLISmoke(context.Background(), "claudeCode")
		}(i)
	}
	// Let the burst pile up behind the leader before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := callCount(); got != 1 {
		t.Fatalf("%d concurrent smokes spent %d inference turns, want 1 — this is the user's own quota", callers, got)
	}
	spent := 0
	for i, r := range results {
		if r.Status != cliSmokeStatusSuccess {
			t.Fatalf("caller %d got %+v, want the leader's success verdict", i, r)
		}
		if !replayed[i] {
			spent++
		}
	}
	if spent != 1 {
		t.Fatalf("%d callers reported spending a turn, want exactly 1", spent)
	}
}

func TestRunCLISmoke_FreePrecheckVerdictsAreNotPinnedByTheCooldown(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")

	authLoggedIn := false
	original := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(string) (bool, bool) { return authLoggedIn, true }
	t.Cleanup(func() { claudeAuthStatusProbe = original })

	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	// A logged-out device costs no turn, so pinning that verdict for the whole
	// cooldown would buy no quota and only keep reporting a broken CLI.
	result, _ := runCLISmoke(context.Background(), "claudeCode")
	if result.ErrorCategory != cliUsageErrorNotAuthenticated || result.Diagnostic != cliSmokeDiagnosticNotLoggedIn {
		t.Fatalf("logged-out smoke = %+v, want not_authenticated/not_logged_in", result)
	}
	if *calls != 0 {
		t.Fatalf("logged-out pre-check spent %d turns, want 0", *calls)
	}

	// The user signs in seconds later. The very next smoke must see it.
	authLoggedIn = true
	result, replayed := runCLISmoke(context.Background(), "claudeCode")
	if replayed || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("after signing in, smoke = %+v (replayed=%v), want a fresh success", result, replayed)
	}
	if *calls != 1 {
		t.Fatalf("post-login smoke made %d execs, want 1", *calls)
	}
}

func TestRunCLISmoke_CachedSuccessIsNotReplayedAfterLogout(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")

	authLoggedIn := true
	original := claudeAuthStatusProbe
	claudeAuthStatusProbe = func(string) (bool, bool) { return authLoggedIn, true }
	t.Cleanup(func() { claudeAuthStatusProbe = original })

	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})

	if result, replayed := runCLISmoke(context.Background(), "claudeCode"); replayed || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("first smoke = %+v (replayed=%v), want a fresh success", result, replayed)
	}

	// Auth is NOT part of the binary stamp, so a logout inside the cooldown
	// window would otherwise keep replaying "healthy" for a CLI that can no
	// longer complete a turn.
	authLoggedIn = false
	result, replayed := runCLISmoke(context.Background(), "claudeCode")
	if replayed {
		t.Fatal("a logged-out CLI must not be answered from a cached success")
	}
	if result.ErrorCategory != cliUsageErrorNotAuthenticated {
		t.Fatalf("post-logout smoke = %+v, want not_authenticated", result)
	}
	// Re-deciding it cost a pre-check, not a turn.
	if *calls != 1 {
		t.Fatalf("post-logout smoke spent %d turns total, want 1 (the pre-logout one)", *calls)
	}

	// An inconclusive probe (a working env-credential login looks exactly like
	// this) must still replay, mirroring the "inconclusive proceeds" rule in
	// the probe itself: refusing there would re-spend a turn on every poll.
	claudeAuthStatusProbe = func(string) (bool, bool) { return false, false }
	if result, replayed := runCLISmoke(context.Background(), "claudeCode"); replayed || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("inconclusive auth = %+v (replayed=%v), want a fresh success to seed the cooldown", result, replayed)
	}
	if result, replayed := runCLISmoke(context.Background(), "claudeCode"); !replayed || result.Status != cliSmokeStatusSuccess {
		t.Fatalf("inconclusive auth = %+v (replayed=%v), want the cached verdict replayed", result, replayed)
	}
	if *calls != 2 {
		t.Fatalf("inconclusive-auth replay spent %d turns total, want 2", *calls)
	}
}
