//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_opencode_exec_windows_test.go — the reported regression,
   executed.
   --------------------------------------------------------------------------
   CI has no real `opencode` binary, so classification is unit-tested through the
   exec seam. What CANNOT be proven that way is the thing that actually broke on
   Windows: that cmd.exe starts an npm `.cmd` shim, that the shim's re-parse
   preserves our argv, and that the staged prompt reaches the shim's CHILD on
   stdin. These cases prove all three against a temporary fake shim that
   delegates to the compiled stub — the same two-process shape npm installs
   (`opencode.cmd` → node) — and then drive the PUBLIC path,
   runCLISmoke(ctx, "opencode"), with the real exec seam in place.

   The first proof against the real CLI is still the post-update smoke on a
   Windows device.
   ------------------------------------------------------------------------ */

// writeOpenCodeFakeShim writes an `opencode.cmd` that forwards its argv and
// stdin to the compiled stub, and points the smoke's resolver at it. The
// directory name deliberately contains a space and an `&`.
func writeOpenCodeFakeShim(t *testing.T) string {
	t.Helper()
	stub := buildOpenCodeStub(t)

	dir := filepath.Join(t.TempDir(), "npm & co")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "opencode.cmd")
	// `%*` is exactly what an npm shim forwards. Quoting the stub path keeps the
	// space in the directory name intact.
	body := "@echo off\r\n\"" + stub + "\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	stubOpenCodeSmokePath(t, shim)
	t.Setenv("OPENCODE_STUB_VERSION", "opencode 0.9.1")
	return shim
}

func TestRunCLISmoke_OpenCodeCmdShimEchoesTheMarkerThroughCmdExe(t *testing.T) {
	openCodeSmokeEnv(t)
	shim := writeOpenCodeFakeShim(t)

	argvLog := filepath.Join(t.TempDir(), "argv.log")
	stdinLog := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_STDIN_LOG", stdinLog)
	t.Setenv("OPENCODE_STUB_ECHO_STDIN", "1")
	envLog := filepath.Join(t.TempDir(), "env.log")
	t.Setenv("OPENCODE_STUB_ENV_LOG", envLog)

	result, replayed := runCLISmoke(context.Background(), "opencode")
	if replayed {
		t.Fatal("the first smoke must actually run")
	}
	// The maintenance pins cross cmd.exe and reach the shim's grandchild.
	if logged, err := os.ReadFile(envLog); err != nil ||
		strings.TrimSpace(string(logged)) != "autoupdate=true title=true" {
		t.Fatalf("maintenance pins did not reach the shim's child: %q (err=%v)", logged, err)
	}

	// An ordinary (non-Maintenance) launch through the same shim carries none.
	ordinaryLog := filepath.Join(t.TempDir(), "env-ordinary.log")
	env := os.Environ()
	// Drop the pins, and the argv / stdin logs the smoke's assertions below read.
	for _, key := range []string{"OPENCODE_DISABLE_AUTOUPDATE", "OPENCODE_DISABLE_TERMINAL_TITLE",
		"OPENCODE_STUB_ARGV_LOG", "OPENCODE_STUB_STDIN_LOG"} {
		env = withoutEnvVar(env, key)
	}
	env = setEnvVar(env, "OPENCODE_STUB_ENV_LOG", ordinaryLog)
	ordinary, err := newOpenCodeCmd(context.Background(), openCodeLaunch{
		Path: shim,
		Args: buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""),
		Env:  env,
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ordinary shim launch refused: %v", err)
	}
	if out, runErr := ordinary.CombinedOutput(); runErr != nil {
		t.Fatalf("ordinary shim launch failed: %v\n%s", runErr, out)
	}
	if logged, _ := os.ReadFile(ordinaryLog); strings.TrimSpace(string(logged)) != "autoupdate=unset title=unset" {
		t.Fatalf("an ordinary launch carries maintenance pins: %q", logged)
	}
	if result.Status != cliSmokeStatusSuccess || !result.MarkerMatched {
		t.Fatalf("a shim install must classify success with markerMatched: %+v", result)
	}
	if result.Diagnostic != cliSmokeDiagnosticNone {
		t.Fatalf("diagnostic = %q, want none", result.Diagnostic)
	}
	if result.ArgvShapeID != openCodeRunShapeIDPlain {
		t.Fatalf("shape id = %q", result.ArgvShapeID)
	}
	if result.Version != "opencode 0.9.1" {
		t.Fatalf("the version probe did not survive the shim: %q", result.Version)
	}

	// cmd.exe started the shim, and the argv reached the shim's CHILD intact.
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the shim's child never ran: %v", err)
	}
	wantArgv := strings.Join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), " ")
	if !strings.Contains(string(argv), wantArgv) {
		t.Fatalf("shim child argv %q does not contain %q", string(argv), wantArgv)
	}

	// The staged prompt reached the shim's child on stdin — the transport that
	// keeps the marker nonce out of a process listing.
	delivered, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("stdin never reached the shim's child: %v", err)
	}
	prompt := strings.TrimSpace(string(delivered))
	if !strings.HasPrefix(prompt, openCodeMaintenanceSmokePromptPrefix) {
		t.Fatalf("stdin = %q, want the reserved marker sentence", prompt)
	}
	if !strings.Contains(prompt, openCodeSmokeMarkerPrefix) {
		t.Fatalf("stdin %q carries no probe marker", prompt)
	}
	// Nothing vendor-authored — and no nonce — reaches the published result.
	marker := strings.TrimPrefix(prompt, openCodeMaintenanceSmokePromptPrefix)
	if strings.Contains(result.Diagnostic+result.ArgvShapeID+result.ErrorCategory, marker) ||
		strings.Contains(result.Version, marker) {
		t.Fatalf("the published result carries the marker nonce: %+v", result)
	}
	if _, err := os.Stat(shim); err != nil {
		t.Fatalf("the shim vanished: %v", err)
	}
}

func TestRunCLISmoke_OpenCodeCmdShimThatDoesNotEchoIsAMarkerMismatch(t *testing.T) {
	openCodeSmokeEnv(t)
	writeOpenCodeFakeShim(t)
	// A chatty model is not a broken contract.
	t.Setenv("OPENCODE_STUB_STDOUT",
		`{"type":"text","text":"Sure, here you go!"}\n{"type":"session.completed"}`)

	result, _ := runCLISmoke(context.Background(), "opencode")
	if result.Diagnostic != cliSmokeDiagnosticMarkerMismatch ||
		result.ErrorCategory != cliUsageErrorParseFailed {
		t.Fatalf("got (%q, %q), want marker_mismatch/parse_failed", result.ErrorCategory, result.Diagnostic)
	}
	if result.MarkerMatched {
		t.Error("markerMatched must be false")
	}
}

func TestRunCLISmoke_OpenCodeCmdShimPastTheDeadlineIsTimeoutWithTheTreeReaped(t *testing.T) {
	openCodeSmokeEnv(t)
	writeOpenCodeFakeShim(t)
	t.Setenv("OPENCODE_STUB_SLEEP_MS", "60000")

	original := openCodeSmokeTimeout
	openCodeSmokeTimeout = 900 * time.Millisecond
	t.Cleanup(func() { openCodeSmokeTimeout = original })

	started := time.Now()
	result, _ := runCLISmoke(context.Background(), "opencode")
	elapsed := time.Since(started)

	if result.Diagnostic != cliSmokeDiagnosticTimeout ||
		result.ErrorCategory != cliUsageErrorProviderTimeout {
		t.Fatalf("got (%q, %q), want timeout/provider_timeout", result.ErrorCategory, result.Diagnostic)
	}
	// The whole tree — cmd.exe AND the shim's child holding the captured pipes —
	// must go down with the deadline, or Wait would block past it.
	if elapsed > 30*time.Second {
		t.Fatalf("the deadline did not bound the run: %v", elapsed)
	}
}

func TestRunCLISmoke_OpenCodeCmdShimWithNoUsableProviderSpendsNoTurn(t *testing.T) {
	// The readiness pre-check must itself survive the shim: without a shim-aware
	// route `models` answered nothing on exactly these installs, the pre-check
	// stayed inconclusive, and an unusable install spent a turn before failing.
	openCodeSmokeEnv(t)
	writeOpenCodeFakeShim(t)
	t.Setenv("OPENCODE_STUB_MODELS", "No providers configured. Run `opencode auth login`.")
	runLog := filepath.Join(t.TempDir(), "run.log")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)

	logged := captureStdout(t, func() {
		result, _ := runCLISmoke(context.Background(), "opencode")
		if result.Diagnostic != cliSmokeDiagnosticNotLoggedIn ||
			result.ErrorCategory != cliUsageErrorNotAuthenticated {
			t.Fatalf("got (%q, %q), want not_logged_in", result.ErrorCategory, result.Diagnostic)
		}
	})

	if _, err := os.Stat(runLog); err == nil {
		t.Error("a conclusively unusable install spent a turn")
	}
	// Nothing vendor-authored reaches the log, even on the refusal path.
	if strings.Contains(logged, "opencode auth login") {
		t.Errorf("the device log carries vendor text: %q", logged)
	}
}

// TestOpenCodeShimCommand_MetacharacterTokensSurviveCmdExe is the security
// property the character policy rests on, proven through a REAL cmd.exe and a
// real `.cmd` shim rather than through the renderer alone.
//
// The policy ACCEPTS `&`, `|`, `^`, `(`, `)`, `;`, `<`, `>` and spaces on the
// argument that they are rendered inside double quotes with delayed expansion
// off, so cmd.exe never re-parses the substituted text as redirection, a pipe or
// a separator. That argument is exactly the kind that is right until it is not:
// a value silently swallowed by a `>` would write a file instead of reaching the
// CLI, and a `&` would run a second command. Asserting only that the renderer
// returns ok=true would prove none of it.
func TestOpenCodeShimCommand_MetacharacterTokensSurviveCmdExe(t *testing.T) {
	openCodeSmokeEnv(t)
	shim := writeOpenCodeFakeShim(t)

	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)

	// Every token the policy accepts and a reader would worry about. The
	// redirection operators come first: if cmd.exe re-parsed them, the shim's
	// child would receive fewer tokens and a stray file would appear.
	hostile := []string{
		"run",
		"--not-a-known-flag", "redirect>out", "redirect<in",
		"amp&ersand", "pipe|x", "caret^x", "(paren)", "semi;colon",
		"a value with spaces", "all&of|it^(at);once",
	}
	cmd, ok := openCodeShimCommand(context.Background(), openCodeLaunch{
		Path: shim,
		Args: hostile,
		Env:  os.Environ(),
		Dir:  t.TempDir(),
	})
	if !ok {
		t.Fatal("the renderer refused a launch its own policy accepts")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("the shim launch failed: %v", err)
	}

	logged, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the shim's child never ran — a metacharacter broke the line: %v", err)
	}
	// The stub logs its argv space-joined, which is lossy for tokens that
	// themselves contain spaces; assert every token is PRESENT and that the count
	// is right, which together rule out both swallowing and re-splitting.
	got := strings.TrimSpace(string(logged))
	for _, token := range hostile {
		if !strings.Contains(got, token) {
			t.Errorf("token %q did not survive cmd.exe; child argv was %q", token, got)
		}
	}
	// Nothing was interpreted as redirection: no stray file in the working dir.
	if entries, err := os.ReadDir(cmd.Dir); err == nil && len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cmd.exe treated a token as redirection and created %v", names)
	}
}
