package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// A START's model and effort reach every turn of a one-shot native session
// (Antigravity, OpenCode, Muse Code). Before this, the three managers ignored
// the START's args entirely: terminal-service composed `--model` / the effort
// flag into them, the CLI never saw either, and OpenCode with no model fell
// back to its own default — on a computer whose default provider has no key,
// every turn failed (ProviderAuthError). These tests pin what is kept, what is
// dropped, and that every turn — first, resumed and replay recovery — carries
// the kept flags, while a start without them builds exactly the old argv.

/* --------------------------------------------------------------------------
   Mock agy that records its argv (dispatched from runMockCLI)
   -------------------------------------------------------------------------- */

const (
	// mockAgyArgvMode is a stream-json `agy` turn that records its argv.
	mockAgyArgvMode = "antigravity-stream-stdin-argv"
	// mockAgyArgvLogEnv names the file each run appends its argv to.
	mockAgyArgvLogEnv = "TEST_MOCK_AGY_ARGV_LOG"
	// mockAgyMissingConversationEnv, when set, makes a resumed turn
	// (`--conversation <id>`) fail the way agy reports a conversation it no
	// longer holds, so the manager's replay recovery runs.
	mockAgyMissingConversationEnv = "TEST_MOCK_AGY_MISSING_CONVERSATION"
)

func runMockAgyArgv() {
	if p := os.Getenv(mockAgyArgvLogEnv); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
			f.Close()
		}
	}
	_, _ = io.ReadAll(os.Stdin)
	if os.Getenv(mockAgyMissingConversationEnv) != "" && slices.Contains(os.Args[1:], "--conversation") {
		fmt.Println("Error: conversation not found")
		os.Exit(1)
	}
	fmt.Println(`{"event":"result","result":{"status":"SUCCESS","response":"done"}}`)
	os.Exit(0)
}

// readArgvLines returns one argv per child run, in order.
func readArgvLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

/* --------------------------------------------------------------------------
   What a START keeps
   -------------------------------------------------------------------------- */

func TestNativeLaunchArgs_KeepOnlyTheModelAndEffort(t *testing.T) {
	type kind struct {
		name   string
		launch func([]string) []string
	}
	openCode := kind{"opencode", openCodeNativeLaunchArgs}
	muse := kind{"musecode", museCodeNativeLaunchArgs}
	agy := kind{"antigravity", antigravityNativeLaunchArgs}

	cases := []struct {
		kind kind
		name string
		in   []string
		want []string
	}{
		// What terminal-service composes (shared-constants launchSettingsArgs).
		{openCode, "the composed model and variant", []string{"--model", "opencode/big-pickle", "--variant", "high"},
			[]string{"--model", "opencode/big-pickle", "--variant", "high"}},
		{muse, "the composed model and effort", []string{"--model", "muse-spark-1.3", "--reasoning-effort", "high"},
			[]string{"--model", "muse-spark-1.3", "--reasoning-effort", "high"}},
		// An Antigravity session doc's args, read live 2026-11-08.
		{agy, "the live effort-only start", []string{"--effort", "medium"}, []string{"--effort", "medium"}},
		{agy, "the composed model and effort", []string{"--model", "gemini-3-pro", "--effort", "high"},
			[]string{"--model", "gemini-3-pro", "--effort", "high"}},

		// Every spelling the CLI documents, inline or not, in one canonical form
		// and a fixed order.
		{openCode, "-m and inline forms", []string{"--variant=max", "-m=anthropic/claude-sonnet-4-5@20250929"},
			[]string{"--model", "anthropic/claude-sonnet-4-5@20250929", "--variant", "max"}},
		{openCode, "-m with a separate value", []string{"-m", "openrouter/x-ai/grok-4:free"},
			[]string{"--model", "openrouter/x-ai/grok-4:free"}},
		{muse, "the --effort alias", []string{"--effort", "xhigh"}, []string{"--reasoning-effort", "xhigh"}},
		{muse, "the inline alias", []string{"--effort=max", "--model=muse-spark-1.3"},
			[]string{"--model", "muse-spark-1.3", "--reasoning-effort", "max"}},
		{agy, "Go's single-dash spellings", []string{"-effort=low", "-model", "gemini-3-flash"},
			[]string{"--model", "gemini-3-flash", "--effort", "low"}},

		// A later occurrence wins, as on the CLI's own command line.
		{openCode, "the last model wins across spellings", []string{"-m", "a/b", "--model", "c/d"}, []string{"--model", "c/d"}},
		{muse, "the last effort wins across spellings", []string{"--reasoning-effort", "low", "--effort", "high"},
			[]string{"--reasoning-effort", "high"}},
		// ...and a later one this device will not pass clears the earlier
		// value: the start overrode it, so the CLI runs its own default.
		{openCode, "an invalid later value clears the flag", []string{"--model", "a/b", "--model", "x y"}, nil},

		// Everything else is dropped — exactly as before this existed.
		{openCode, "manager-owned and other forwarded flags", []string{
			"--port", "4096", "--agent", "build", "--host", "0.0.0.0", "--session", "ses_x", "-s", "ses_y",
			"--format", "text", "-f", "text", "--fork", "ses_z", "--continue", "-c", "--print-logs", "hello world"}, nil},
		{muse, "sandbox, provider, session and step flags", []string{
			"exec", "--json", "--yolo", "--disable-sandbox", "--provider", "echo", "--base-url", "https://evil.example",
			"--session-id", "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11", "--prompt-file", "/etc/passwd",
			"--max-model-steps", "40", "--approval-mode", "never", "--workspace", "/"}, nil},
		{agy, "permission, conversation and directory flags", []string{
			"--dangerously-skip-permissions", "--conversation", "conv-x", "--continue", "-c", "--add-dir", "/",
			"--mode", "yolo", "--sandbox", "--print", "hi", "--agent", "x"}, nil},

		// A valued flag's value is never read as a flag, kept or not.
		{openCode, "a forwarded flag's value", []string{"--agent", "--model", "--variant", "high"}, []string{"--variant", "high"}},
		{openCode, "a model whose value looks like a flag", []string{"--model", "--variant", "high"}, nil},
		{muse, "a stripped flag's value", []string{"--session-id", "--model", "muse-spark-1.3"}, nil},
		{agy, "an agy flag's value", []string{"--add-dir", "--model", "gemini-3-pro"}, nil},

		// `--` ends the options; a flag with no value is dropped.
		{openCode, "operands after --", []string{"--", "--model", "a/b"}, nil},
		{muse, "a trailing flag with no value", []string{"--model"}, nil},
		{agy, "no args at all", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.kind.name+"/"+tc.name, func(t *testing.T) {
			got := tc.kind.launch(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("launch args of %q = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A spelling is kept only when the CLI's forwarding allowlist (the legacy
// session path's) lists it too, so the native path never applies a flag the
// legacy path would not pass — and every production spelling is listed.
func TestSelectNativeLaunchArgs_KeepsOnlySpellingsTheCLIForwards(t *testing.T) {
	flags := nativeLaunchFlags{
		forwarded: map[string]bool{"--model": true},
		canonical: map[string]string{"--model": "--model", "--effort": "--effort"},
		order:     []string{"--model", "--effort"},
		valued:    func(name string) bool { return name == "--model" || name == "--effort" },
	}
	got := selectNativeLaunchArgs([]string{"--model", "m", "--effort", "high", "--effort=max"}, flags)
	if want := []string{"--model", "m"}; !slices.Equal(got, want) {
		t.Fatalf("a spelling the CLI does not forward was kept: got %q, want %q", got, want)
	}

	for _, kind := range []struct {
		name      string
		canonical map[string]string
		forwarded map[string]bool
	}{
		{"opencode", openCodeNativeLaunchFlags, openCodeForwardedValuedFlags},
		{"musecode", museCodeNativeLaunchFlags, museCodeForwardedValuedFlags},
		{"antigravity", antigravityNativeLaunchFlags, antigravityNativeForwardedValuedFlags},
	} {
		for spelling := range kind.canonical {
			if !kind.forwarded[spelling] {
				t.Errorf("%s: launch spelling %q is missing from the CLI's forwarding allowlist, so it is never kept", kind.name, spelling)
			}
		}
	}
}

// The value contract mirrors shared-constants CLI_MODEL_ID_PATTERN (what the
// server validates a model against before composing it), so the device keeps
// every value the server composes and nothing that could be read as a flag,
// split into two tokens, or re-read by a Windows `.cmd` shim's cmd.exe pass.
func TestNativeLaunchArgs_ValueShape(t *testing.T) {
	for _, value := range []string{
		"opencode/big-pickle", "anthropic/claude-sonnet-4-5@20250929", "openrouter/x-ai/grok-4:free",
		"gpt-5.1-codex", "muse-spark-1.3", "a+b", "_x", "9", "high", strings.Repeat("a", 200),
	} {
		if got := openCodeNativeLaunchArgs([]string{"--model", value}); !slices.Equal(got, []string{"--model", value}) {
			t.Errorf("value %q must be kept, got %q", value, got)
		}
	}
	for _, value := range []string{
		"", "-x", "--model", ".x", "/x", ":x", "x y", `x"y`, "x'y", "x%PATH%", "x&y", "x|y", "x^y", "x<y", "x>y",
		"x\ny", "x\ty", "x;y", "x$y", "x`y", "x(y)", "x,y", "ünïcode", strings.Repeat("a", 201),
	} {
		if got := openCodeNativeLaunchArgs([]string{"--model", value}); got != nil {
			t.Errorf("value %q must be dropped, got %q", value, got)
		}
	}
}

/* --------------------------------------------------------------------------
   Each kind's turn argv, with and without launch flags
   -------------------------------------------------------------------------- */

func TestNativeTurnArgv_WithoutLaunchFlagsIsUnchanged(t *testing.T) {
	join := func(args []string) string { return strings.Join(args, " ") }
	// Byte for byte what each manager spawned before this change.
	for _, tc := range []struct{ got, want string }{
		{join(buildOpenCodeNativeTurnArgs("", nil)), join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""))},
		{join(buildOpenCodeNativeTurnArgs("ses_1", nil)), join(buildOpenCodeRunArgs(openCodeRunShapeResume, "ses_1"))},
		{join(buildAntigravityNativeTurnArgs("", nil)), join(append(antigravityDangerousFlags(), buildAntigravityNativeArgs("")...))},
		{join(buildAntigravityNativeTurnArgs("conv-1", nil)), join(append(antigravityDangerousFlags(), buildAntigravityNativeArgs("conv-1")...))},
		{join(buildMuseCodeNativeGateArgs(nil)), join(buildMuseCodeNativeArgs("", ""))},
	} {
		if tc.got != tc.want {
			t.Errorf("argv %q, want %q", tc.got, tc.want)
		}
	}
	for _, tc := range []struct{ got, want string }{
		{join(buildOpenCodeNativeTurnArgs("", nil)), "run --format json"},
		{join(buildOpenCodeNativeTurnArgs("ses_1", nil)), "run --format json --session ses_1"},
		{join(buildAntigravityNativeTurnArgs("", nil)), "--dangerously-skip-permissions --input-format stream-json --output-format stream-json"},
		{join(buildAntigravityNativeTurnArgs("conv-1", nil)), "--dangerously-skip-permissions --input-format stream-json --output-format stream-json --conversation conv-1"},
	} {
		if tc.got != tc.want {
			t.Errorf("argv %q, want %q", tc.got, tc.want)
		}
	}
}

func TestNativeTurnArgv_AppendsTheLaunchFlags(t *testing.T) {
	join := func(args []string) string { return strings.Join(args, " ") }
	for _, tc := range []struct{ got, want string }{
		{join(buildOpenCodeNativeTurnArgs("", openCodeNativeLaunchArgs([]string{"-m", "opencode/big-pickle", "--variant", "high"}))),
			"run --format json --model opencode/big-pickle --variant high"},
		{join(buildOpenCodeNativeTurnArgs("ses_1", openCodeNativeLaunchArgs([]string{"-m", "opencode/big-pickle", "--variant", "high"}))),
			"run --format json --session ses_1 --model opencode/big-pickle --variant high"},
		{join(buildAntigravityNativeTurnArgs("conv-1", antigravityNativeLaunchArgs([]string{"--effort", "medium", "--model", "gemini-3-pro"}))),
			"--dangerously-skip-permissions --input-format stream-json --output-format stream-json --conversation conv-1 --model gemini-3-pro --effort medium"},
		{join(buildMuseCodeNativeGateArgs(museCodeNativeLaunchArgs([]string{"--model", "muse-spark-1.3", "--effort", "high"}))),
			"exec --json --disable-approval --model muse-spark-1.3 --reasoning-effort high"},
	} {
		if tc.got != tc.want {
			t.Errorf("argv %q, want %q", tc.got, tc.want)
		}
	}
}

/* --------------------------------------------------------------------------
   The START reaches the manager
   -------------------------------------------------------------------------- */

func TestDispatchOneShotNativeStart_HandsTheSignedArgsToTheManager(t *testing.T) {
	for _, kind := range []nativeFrameKind{openCodeNativeKind, museCodeNativeSpec.nativeFrameKind} {
		f := &fakeOneShotTarget{}
		args := []string{"--model", "m/x", "--variant", "high"}
		r := runDispatch(commandMsg{ID: "c1", Type: kind.FramePrefix + "_start", SessionID: "s1", Cwd: "/w", Args: args}, f, kind)
		if len(r.errs) != 0 || !f.started {
			t.Fatalf("%s: start failed: %#v", kind.FramePrefix, r)
		}
		if !slices.Equal(f.startArgs, args) {
			t.Fatalf("%s: the manager got start args %q, want the command's %q", kind.FramePrefix, f.startArgs, args)
		}
	}
}

func TestAntigravityNativeStartCommand_KeepsTheStartModelAndEffort(t *testing.T) {
	primeAntigravityCapability(t)
	m := NewAntigravityNativeManager(nil)
	sink := &frameSink{}
	prevManager, prevPublisher := globalAntigravityNativeManager, antigravityNativePublisher
	globalAntigravityNativeManager = m
	antigravityNativePublisher = func(*pubsub.Publisher, string) PublishFunc { return sink.publish }
	t.Cleanup(func() {
		_ = m.End("s-agy")
		globalAntigravityNativeManager, antigravityNativePublisher = prevManager, prevPublisher
	})

	handleAntigravityNativeCommand(context.Background(), nil, commandMsg{
		ID: "c1", Type: "antigravity_native_start", SessionID: "s-agy", Cwd: t.TempDir(), WorkspaceID: "ws", UID: "uid",
		Args: []string{"--effort", "medium", "--model", "gemini-3-pro", "--conversation", "conv-injected"},
	}, &Config{})

	if started := sink.ofType("antigravity_native_started"); len(started) != 1 {
		t.Fatalf("the start must be acknowledged once, got %#v", sink.frames)
	}
	session := m.Get("s-agy")
	if session == nil {
		t.Fatal("the start must register the session")
	}
	if want := []string{"--model", "gemini-3-pro", "--effort", "medium"}; !slices.Equal(session.LaunchArgs, want) {
		t.Fatalf("session launch args = %q, want %q", session.LaunchArgs, want)
	}
	if session.NativeConversationID != "" {
		t.Fatal("a `--conversation` in the START's args must never select the conversation")
	}
}

/* --------------------------------------------------------------------------
   Every turn carries them (real child processes)
   -------------------------------------------------------------------------- */

// Each subtest runs real stub turns, which arm OpenCode usage runs: the usage
// fixture keeps their ledger and cache in the subtest's own directory, never
// the process-wide one a later test's usage propagator would read back.
func TestOpenCodeNativeTurns_CarryTheStartModelAndVariant(t *testing.T) {
	start := []string{"-m", "opencode/big-pickle", "--variant", "high",
		"--port", "4096", "--session", "ses_injected", "--format", "text", "--continue"}

	t.Run("first and resumed turns", func(t *testing.T) {
		openCodeUsageFixture(t, 1602)
		installOpenCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","sessionID":"ses_launch","part":{"type":"text","text":"ok"}}`+"\\n")

		m := NewOpenCodeNativeManager()
		if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", "", start, nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		for _, prompt := range []string{"first", "second"} {
			if err := m.Send("s", prompt, func(resultMsg) {}, time.Minute); err != nil {
				t.Fatalf("send %s: %v", prompt, err)
			}
		}
		want := []string{
			"run --format json --model opencode/big-pickle --variant high",
			"run --format json --session ses_launch --model opencode/big-pickle --variant high",
		}
		if got := readArgvLines(t, argvLog); !slices.Equal(got, want) {
			t.Fatalf("turn argv:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("the replay recovery", func(t *testing.T) {
		openCodeUsageFixture(t, 1603)
		installOpenCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_RUN_LOG", filepath.Join(t.TempDir(), "runs.log"))
		t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
		t.Setenv("OPENCODE_STUB_FIRST_STDOUT", "Error: Session not found\\n")
		t.Setenv("OPENCODE_STUB_FIRST_EXIT", "1")
		t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"recovered"}`+"\\n")

		m := NewOpenCodeNativeManager()
		if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", "ses_gone", start, nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		m.Get("s").Transcript = []openCodeTurn{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "answer"}}
		if err := m.Send("s", "follow up", func(resultMsg) {}, time.Minute); err != nil {
			t.Fatalf("send: %v", err)
		}
		want := []string{
			"run --format json --session ses_gone --model opencode/big-pickle --variant high",
			"run --format json --model opencode/big-pickle --variant high",
		}
		if got := readArgvLines(t, argvLog); !slices.Equal(got, want) {
			t.Fatalf("turn argv:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("a start without them builds the old argv", func(t *testing.T) {
		openCodeUsageFixture(t, 1604)
		installOpenCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","sessionID":"ses_plain","part":{"type":"text","text":"ok"}}`+"\\n")

		m := NewOpenCodeNativeManager()
		if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", "", []string{"--port", "1", "hello"}, nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		for _, prompt := range []string{"first", "second"} {
			if err := m.Send("s", prompt, func(resultMsg) {}, time.Minute); err != nil {
				t.Fatalf("send %s: %v", prompt, err)
			}
		}
		want := []string{"run --format json", "run --format json --session ses_plain"}
		if got := readArgvLines(t, argvLog); !slices.Equal(got, want) {
			t.Fatalf("turn argv:\n got %q\nwant %q", got, want)
		}
	})
}

func TestMuseCodeNativeTurns_CarryTheStartModelAndEffort(t *testing.T) {
	start := []string{"--model", "muse-spark-1.3", "--effort", "high", "--yolo", "--provider", "echo",
		"--session-id", "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11", "--max-model-steps", "40"}
	const launch = " --model muse-spark-1.3 --reasoning-effort high"
	// prefix + <prompt file> + suffix: the prompt file path is per turn.
	checkTurn := func(t *testing.T, line, sessionID, suffix string) {
		t.Helper()
		prefix := "exec --json --disable-approval --session-id " + sessionID + " --prompt-file "
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
			t.Fatalf("turn argv %q, want %q<prompt file>%q", line, prefix, suffix)
		}
		if promptFile := strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix); promptFile == "" || strings.Contains(promptFile, " --") {
			t.Fatalf("turn argv %q carries more than the prompt file between the fixed flags and the launch flags", line)
		}
	}

	t.Run("first turns", func(t *testing.T) {
		isolateMuseCode(t)
		m := installMuseCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
		if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", "", start, nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		minted := m.Get("s").NativeSessionID
		for _, prompt := range []string{"first", "second"} {
			if err := m.Send("s", prompt, (&frameSink{}).publish, time.Minute); err != nil {
				t.Fatalf("send %s: %v", prompt, err)
			}
		}
		lines := readArgvLines(t, argvLog)
		if len(lines) != 2 {
			t.Fatalf("want 2 turns, got %q", lines)
		}
		for _, line := range lines {
			checkTurn(t, line, minted, launch)
		}
	})

	t.Run("the replay recovery", func(t *testing.T) {
		isolateMuseCode(t)
		m := installMuseCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_RUN_LOG", filepath.Join(t.TempDir(), "runs.log"))
		t.Setenv("OPENCODE_STUB_FAIL_FIRST", "1")
		t.Setenv("OPENCODE_STUB_FIRST_STDOUT", `Error: session not found\n`)
		t.Setenv("OPENCODE_STUB_FIRST_EXIT", "1")
		t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
		seed := "0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"
		if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", seed, start, nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		sink := &frameSink{}
		if err := m.Send("s", "follow up", sink.publish, time.Minute); err != nil {
			t.Fatalf("send: %v", err)
		}
		done, ok := sink.completion()
		if !ok || !strings.Contains(done.Output, `"replayRecovery":true`) {
			t.Fatalf("the turn must have recovered by replay, got %#v", done)
		}
		lines := readArgvLines(t, argvLog)
		if len(lines) != 2 {
			t.Fatalf("want the resumed run and its replay, got %q", lines)
		}
		checkTurn(t, lines[0], seed, launch)
		checkTurn(t, lines[1], done.ConversationID, launch)
	})

	t.Run("a start without them builds the old argv", func(t *testing.T) {
		isolateMuseCode(t)
		m := installMuseCodeStub(t)
		argvLog := filepath.Join(t.TempDir(), "argv.log")
		t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
		t.Setenv("OPENCODE_STUB_STDOUT", museStubStdout(museFrameCompleted))
		if err := m.Start("s", t.TempDir(), "ws", "uid", "", nil, nil); err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := m.Send("s", "first", (&frameSink{}).publish, time.Minute); err != nil {
			t.Fatalf("send: %v", err)
		}
		lines := readArgvLines(t, argvLog)
		if len(lines) != 1 {
			t.Fatalf("want 1 turn, got %q", lines)
		}
		// Nothing after the prompt file: buildMuseCodeNativeArgs's argv exactly.
		// (The file's path may hold a space, a Windows profile name's; it
		// never holds " --".)
		prefix := "exec --json --disable-approval --session-id " + m.Get("s").NativeSessionID + " --prompt-file "
		promptFile := strings.TrimPrefix(lines[0], prefix)
		if !strings.HasPrefix(lines[0], prefix) || promptFile == "" || strings.Contains(promptFile, " --") {
			t.Fatalf("turn argv %q, want %q<prompt file> and nothing after it", lines[0], prefix)
		}
	})
}

func TestAntigravityNativeTurns_CarryTheStartModelAndEffort(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")
	helperMockAgyOnPath(t, mockAgyArgvMode)
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv(mockAgyArgvLogEnv, argvLog)
	t.Setenv(mockAgyMissingConversationEnv, "1")
	primeAntigravityCapability(t)

	m := NewAntigravityNativeManager(nil)
	start := []string{"--effort", "medium", "--model", "gemini-3-pro",
		"--conversation", "conv-injected", "--add-dir", "/", "--dangerously-skip-permissions"}
	if err := m.StartWithArgs("s", t.TempDir(), "ws", "uid", "", start, nil, nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Start("plain", t.TempDir(), "ws", "uid", "", nil, nil); err != nil {
		t.Fatalf("start plain: %v", err)
	}
	t.Cleanup(func() { _ = m.End("s"); _ = m.End("plain") })

	// A first turn; a resumed turn whose conversation is gone, so it recovers
	// by replay; and a session whose start named nothing.
	if err := m.Send("s", "first", func(resultMsg) {}, time.Minute); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	m.Get("s").NativeConversationID = "conv-stale"
	if err := m.Send("s", "second", func(resultMsg) {}, time.Minute); err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if err := m.Send("plain", "hello", func(resultMsg) {}, time.Minute); err != nil {
		t.Fatalf("plain turn: %v", err)
	}
	helperAwaitCaptureStopped(t, "a turn leaked its quota capture", "no turn armed a quota capture")

	const base = "--dangerously-skip-permissions --input-format stream-json --output-format stream-json"
	const launch = " --model gemini-3-pro --effort medium"
	want := []string{
		base + launch,
		base + " --conversation conv-stale" + launch,
		base + launch,
		base,
	}
	if got := readArgvLines(t, argvLog); !slices.Equal(got, want) {
		t.Fatalf("turn argv:\n got %q\nwant %q", got, want)
	}
}

/* --------------------------------------------------------------------------
   The session-entry gate shows what the turns will run
   -------------------------------------------------------------------------- */

func TestNativeStartGate_ShowsTheStartLaunchFlags(t *testing.T) {
	al := &AllowList{configPath: filepath.Join(t.TempDir(), "allow.txt")}
	if err := os.WriteFile(al.configPath, []byte("git *\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := al.Load(); err != nil {
		t.Fatalf("load allowlist: %v", err)
	}
	prevList, prevDialog := defaultAllowList, commandApprovalDialogFn
	var shownCommand string
	var shownArgs []string
	defaultAllowList = al
	commandApprovalDialogFn = func(command string, args []string, _ int) ApprovalResult {
		shownCommand, shownArgs = command, args
		return ApprovalDeny
	}
	t.Cleanup(func() { defaultAllowList, commandApprovalDialogFn = prevList, prevDialog })

	// Unsigned mode: nothing authenticated the command, so every native start
	// goes through the dialog, which must show the argv the turns will run.
	unsigned := &Config{EnableAllowList: true}
	for _, tc := range []struct {
		cmdType, command string
		args             []string
		want             string
	}{
		{"antigravity_native_start", "agy", []string{"--effort", "medium", "--model", "gemini-3-pro", "--add-dir", "/"},
			"--dangerously-skip-permissions --input-format stream-json --output-format stream-json --model gemini-3-pro --effort medium"},
		{"opencode_native_start", "opencode", []string{"-m", "opencode/big-pickle", "--variant", "high", "--port", "1"},
			"run --format json --model opencode/big-pickle --variant high"},
		{"musecode_native_start", "muse", []string{"--model", "muse-spark-1.3", "--effort", "high", "--yolo"},
			"exec --json --disable-approval --model muse-spark-1.3 --reasoning-effort high"},
	} {
		shownCommand, shownArgs = "", nil
		if gateSessionEntryCommand(nil, nil, nil, commandMsg{Type: tc.cmdType, Args: tc.args}, unsigned) {
			t.Fatalf("%s: a denied dialog must refuse the start", tc.cmdType)
		}
		if shownCommand != tc.command || strings.Join(shownArgs, " ") != tc.want {
			t.Fatalf("%s: dialog showed %q %q, want %q %q", tc.cmdType, shownCommand, shownArgs, tc.command, tc.want)
		}
	}
}

/* --------------------------------------------------------------------------
   The legacy path forwards Muse's documented effort alias with its value
   -------------------------------------------------------------------------- */

func TestBuildMuseCodeInteractiveArgs_ForwardsTheEffortAlias(t *testing.T) {
	got := strings.Join(buildMuseCodeInteractiveArgs([]string{"--effort", "high", "go"}), " ")
	if want := "exec --json --disable-approval --effort high -- go"; got != want {
		t.Fatalf("got %q, want %q (the level must not be read as prompt text)", got, want)
	}
}
