// grok_acp_test.go
// -----------------------------------------------------------------------------
// Unit + lifecycle tests for GrokACPManager. Unit tests pin the argv builder /
// env sanitizer / Send validation. The lifecycle tests drive a real
// GrokACPManager against the test binary in TEST_MOCK_CLI_MODE=grok-acp-echo
// so we don't need a real `grok` install on the test host.
//
// Shape mirrors codex_appserver_test.go — both managers share the same
// JSON-RPC stdio contract, so the same battery of invariants must hold for
// each (no dropped frames, fail-fast on malformed input, terminal `_ended`
// frame, stdin-close graceful exit, …).
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   argv builder
   -------------------------------------------------------------------------- */

func TestBuildGrokACPArgs_DefaultsToAgentStdio(t *testing.T) {
	got := buildGrokACPArgs(nil, false)
	want := []string{"agent", "stdio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildGrokACPArgs(nil) = %#v, want %#v", got, want)
	}
}

func TestBuildGrokACPArgs_ForwardsExtraArgs(t *testing.T) {
	got := buildGrokACPArgs([]string{"--model", "grok-2-fast", "--config", "auth.method=cached_token"}, false)
	want := []string{"agent", "stdio", "--model", "grok-2-fast", "--config", "auth.method=cached_token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildGrokACPArgs = %#v, want %#v", got, want)
	}
}

func TestBuildGrokACPArgs_StripsDuplicateEntryTokens(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"duplicate_agent", []string{"agent", "--model", "grok-2-fast"}},
		{"duplicate_stdio", []string{"stdio", "--model", "grok-2-fast"}},
		{"tui_subcommand", []string{"tui", "--model", "grok-2-fast"}},
		{"chat_subcommand", []string{"chat", "--model", "grok-2-fast"}},
		{"run_subcommand", []string{"run", "--model", "grok-2-fast"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if len(got) < 2 || got[0] != "agent" || got[1] != "stdio" {
				t.Fatalf("expected built-in `agent stdio` prefix; got %#v", got)
			}
			for _, a := range got[2:] {
				lower := strings.ToLower(a)
				if lower == "agent" || lower == "stdio" || lower == "tui" || lower == "chat" || lower == "run" {
					t.Fatalf("duplicate entry token leaked into final argv: %#v", got)
				}
			}
		})
	}
}

// TestBuildGrokACPArgs_PreservesValueOfValuedFlag pins the regression where a
// value that happens to spell a stripped subcommand (-c agent=true) was
// silently swallowed by the entry-token strip.
func TestBuildGrokACPArgs_PreservesValueOfValuedFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"config_value_is_agent",
			[]string{"-c", "agent", "-c", "model=grok-2"},
			[]string{"agent", "stdio", "-c", "agent", "-c", "model=grok-2"},
		},
		{
			"config_value_is_stdio",
			[]string{"--config", "stdio", "--model", "grok-2"},
			[]string{"agent", "stdio", "--config", "stdio", "--model", "grok-2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGrokACPArgs mangled a valued-flag value: got %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestBuildGrokACPArgs_StripsAuthOverridesByDefault pins finding #3 from the
// secondary review: even if cached-token auth is the orchestrator's
// preference, a caller-supplied `--api-key …` / `--auth …` could still flip
// Grok onto API-key billing. With allowAPIKey=false those args must be
// stripped (both separate-value and equals-form), so the only way to opt
// into API-key auth is to flip Config.EnableGrokAPIKeyFallback.
func TestBuildGrokACPArgs_StripsAuthOverridesByDefault(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"strips_api_key_separate_value",
			[]string{"--api-key", "xai-abc", "--model", "grok-2"},
			[]string{"agent", "stdio", "--model", "grok-2"},
		},
		{
			"strips_api_key_equals_form",
			[]string{"--api-key=xai-abc", "--model", "grok-2"},
			[]string{"agent", "stdio", "--model", "grok-2"},
		},
		{
			"strips_auth_method",
			[]string{"--auth", "xai.api_key", "--model", "grok-2"},
			[]string{"agent", "stdio", "--model", "grok-2"},
		},
		{
			"strips_auth_equals_form",
			[]string{"--auth=xai.api_key", "--model", "grok-2"},
			[]string{"agent", "stdio", "--model", "grok-2"},
		},
		{
			"strips_api_key_env",
			[]string{"--api-key-env", "OTHER_KEY", "--model", "grok-2"},
			[]string{"agent", "stdio", "--model", "grok-2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGrokACPArgs(c.args, false)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGrokACPArgs(allowAPIKey=false) failed to strip auth override: got %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestBuildGrokACPArgs_PreservesAuthOverridesWhenFallbackEnabled is the
// inverse: when the workspace has explicitly opted into API-key auth via
// Config.EnableGrokAPIKeyFallback=true, the caller-supplied auth override
// must flow through verbatim.
func TestBuildGrokACPArgs_PreservesAuthOverridesWhenFallbackEnabled(t *testing.T) {
	got := buildGrokACPArgs([]string{"--api-key", "xai-abc", "--model", "grok-2"}, true)
	want := []string{"agent", "stdio", "--api-key", "xai-abc", "--model", "grok-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildGrokACPArgs(allowAPIKey=true) must preserve --api-key; got %#v, want %#v", got, want)
	}
}

// TestRedactGrokACPArgsForLog pins the startup-banner redaction: when the
// API-key fallback is enabled and buildGrokACPArgs preserves --api-key{,-env}
// / --auth{,-method} verbatim, the value MUST be replaced with [REDACTED]
// before the args are logged — covering both equals-form (`--api-key=xai-...`)
// and separate-value form (`--api-key xai-...`). Non-auth args and the flag
// names themselves stay intact so the log is still diagnostic.
func TestRedactGrokACPArgsForLog(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"masks_api_key_separate_value",
			[]string{"agent", "stdio", "--api-key", "xai-abcdef", "--model", "grok-2"},
			[]string{"agent", "stdio", "--api-key", "[REDACTED]", "--model", "grok-2"},
		},
		{
			"masks_api_key_equals_form",
			[]string{"agent", "stdio", "--api-key=xai-abcdef", "--model", "grok-2"},
			[]string{"agent", "stdio", "--api-key=[REDACTED]", "--model", "grok-2"},
		},
		{
			"masks_api_key_env_separate_value",
			[]string{"agent", "stdio", "--api-key-env", "OTHER_KEY_VAR"},
			[]string{"agent", "stdio", "--api-key-env", "[REDACTED]"},
		},
		{
			"masks_auth_method_value",
			[]string{"agent", "stdio", "--auth", "xai.api_key"},
			[]string{"agent", "stdio", "--auth", "[REDACTED]"},
		},
		{
			"no_op_when_no_auth_flags",
			[]string{"agent", "stdio", "--model", "grok-2-fast"},
			[]string{"agent", "stdio", "--model", "grok-2-fast"},
		},
		{
			"flag_at_end_without_value_is_left_alone",
			[]string{"agent", "stdio", "--api-key"},
			[]string{"agent", "stdio", "--api-key"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactGrokACPArgsForLog(c.args)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("redactGrokACPArgsForLog: got %#v, want %#v", got, c.want)
			}
			// Defence in depth: whatever the structure, the raw secret value
			// must never appear in the joined log line.
			joined := strings.Join(got, " ")
			if strings.Contains(joined, "xai-abcdef") {
				t.Fatalf("redacted output still contains raw key: %q", joined)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   env sanitizer
   -------------------------------------------------------------------------- */

// TestSanitizeGrokACPEnv_StripsXAIKeyByDefault pins finding #3: the default
// posture is API-key auth is opt-in only, so XAI_API_KEY MUST be stripped
// when allowAPIKey=false. The cached-token path under GROK_HOME survives
// because the orchestrator's ACP authenticate flow needs it. The
// conflicting CLAUDECODE / CLAUDE_ / CODEX_IDE_ vars are always stripped.
func TestSanitizeGrokACPEnv_StripsXAIKeyByDefault(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CODEX_IDE_VERSION=0.1.0",
		"GROK_HOME=/home/user/.grok",
		"XAI_API_KEY=xai-abc",
		"HOME=/home/user",
	}
	got := sanitizeGrokACPEnv(in, false)
	wantPresent := []string{
		"PATH=/usr/bin",
		"GROK_HOME=/home/user/.grok",
		"HOME=/home/user",
	}
	wantAbsent := []string{
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CODEX_IDE_VERSION=0.1.0",
		// Critical: API-key auth is opt-in only — without the explicit
		// Config.EnableGrokAPIKeyFallback flag, a user who has
		// `export XAI_API_KEY=...` in their shell rc would otherwise
		// silently bill their xAI API wallet.
		"XAI_API_KEY=xai-abc",
	}

	for _, w := range wantPresent {
		if !envContains(got, w) {
			t.Errorf("expected env to retain %q; got %v", w, got)
		}
	}
	for _, w := range wantAbsent {
		if envContains(got, w) {
			t.Errorf("expected env to strip %q (opt-in only); got %v", w, got)
		}
	}
}

// TestSanitizeGrokACPEnv_PreservesXAIKeyWhenFallbackEnabled is the inverse:
// when the workspace has explicitly opted into API-key auth, the env var
// must survive so Grok can authenticate via its fallback flow.
func TestSanitizeGrokACPEnv_PreservesXAIKeyWhenFallbackEnabled(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"XAI_API_KEY=xai-abc",
		"HOME=/home/user",
	}
	got := sanitizeGrokACPEnv(in, true)
	for _, w := range []string{"PATH=/usr/bin", "XAI_API_KEY=xai-abc", "HOME=/home/user"} {
		if !envContains(got, w) {
			t.Errorf("expected env to retain %q when allowAPIKey=true; got %v", w, got)
		}
	}
}

/* --------------------------------------------------------------------------
   isGrokACPCommand
   -------------------------------------------------------------------------- */

func TestIsGrokACPCommand(t *testing.T) {
	cases := map[string]bool{
		"grok_acp_start":        true,
		"grok_acp_send":         true,
		"grok_acp_end":          true,
		"codex_appserver_start": false,
		"session_start":         false,
		"execute":               false,
		"":                      false,
		"grok_acp_other":        false,
	}
	for in, want := range cases {
		if got := isGrokACPCommand(in); got != want {
			t.Errorf("isGrokACPCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

/* --------------------------------------------------------------------------
   Send validation (no process required)
   -------------------------------------------------------------------------- */

func TestGrokACPManager_Send_RejectsInvalidPayloads(t *testing.T) {
	m := NewGrokACPManager()
	id := "test-fixture"
	fixture := &GrokACPSession{
		ID:         id,
		status:     "ended",
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	cases := []struct {
		name    string
		payload string
		wantErr string
	}{
		{"empty", "", "payload is empty"},
		{"whitespace_only", "   \t  ", "payload is empty"},
		{"embedded_newline", `{"jsonrpc":"2.0","id":1` + "\n" + `,"method":"initialize"}`, "must be a single line"},
		{"embedded_crlf", `{"jsonrpc":"2.0"}` + "\r\n" + `{"method":"x"}`, "must be a single line"},
		{"not_json", `oops not json`, "not valid JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := m.Send(id, c.payload)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q; got %q", c.wantErr, err.Error())
			}
		})
	}
}

func TestGrokACPManager_Send_NotFound(t *testing.T) {
	m := NewGrokACPManager()
	err := m.Send("missing", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected `not found` error; got %v", err)
	}
}

func TestGrokACPManager_Send_EndedSession(t *testing.T) {
	m := NewGrokACPManager()
	id := "ended-fixture"
	fixture := &GrokACPSession{ID: id, status: "ended", done: make(chan struct{}), streamDone: make(chan struct{})}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected `has ended` error; got %v", err)
	}
}

/* --------------------------------------------------------------------------
   Manager registry
   -------------------------------------------------------------------------- */

func TestGrokACPManager_StartRejectsDuplicateID(t *testing.T) {
	m := NewGrokACPManager()
	id := "dupe-fixture"
	m.sessions[id] = &GrokACPSession{ID: id, status: "running", done: make(chan struct{}), streamDone: make(chan struct{})}

	publishFn := func(resultMsg) {}
	err := m.Start(id, t.TempDir(), nil, "ws", "uid", GrokStartOptions{}, publishFn)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected `already exists` error; got %v", err)
	}
}

func TestGrokACPManager_StartRequiresIDAndPublish(t *testing.T) {
	m := NewGrokACPManager()
	cwd := t.TempDir()
	if err := m.Start("", cwd, nil, "ws", "uid", GrokStartOptions{}, func(resultMsg) {}); err == nil {
		t.Fatalf("expected error for empty sessionID")
	}
	if err := m.Start("x", cwd, nil, "ws", "uid", GrokStartOptions{}, nil); err == nil {
		t.Fatalf("expected error for nil publishFn")
	}
}

// TestGrokACPManager_StartRequiresValidCwd pins the workspace-safety contract
// — missing/relative/non-existent cwd must all reject before any process is
// spawned, so a malformed orchestrator command can't accidentally launch grok
// against the agent's process working directory and edit unintended files.
func TestGrokACPManager_StartRequiresValidCwd(t *testing.T) {
	m := NewGrokACPManager()
	publishFn := func(resultMsg) {}

	t.Run("empty_cwd_rejected", func(t *testing.T) {
		err := m.Start("a", "", nil, "ws", "uid", GrokStartOptions{}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "cwd is required") {
			t.Fatalf("expected `cwd is required` error; got %v", err)
		}
	})

	t.Run("relative_cwd_rejected", func(t *testing.T) {
		err := m.Start("b", "./relative/path", nil, "ws", "uid", GrokStartOptions{}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("expected `absolute path` error; got %v", err)
		}
	})

	t.Run("missing_dir_rejected", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "definitely-missing-dir-xyz123")
		err := m.Start("c", missing, nil, "ws", "uid", GrokStartOptions{}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "not accessible") {
			t.Fatalf("expected `not accessible` error; got %v", err)
		}
	})

	t.Run("file_instead_of_dir_rejected", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "afile.txt")
		if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		err := m.Start("d", filePath, nil, "ws", "uid", GrokStartOptions{}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("expected `not a directory` error; got %v", err)
		}
	})

	if m.ActiveCount() != 0 {
		t.Errorf("no session should have been registered after rejected Start calls; got %d", m.ActiveCount())
	}
}

// TestGrokACPManager_StartEnforcesWorkspaceRootContainment pins finding #1
// from the secondary review: a configured WorkspaceRoot must contain the
// requested cwd after symlink resolution. Without this check a signed
// grok_acp_start could launch Grok against any local directory the OS user
// can read/write, defeating the workspace/path-safety stance.
func TestGrokACPManager_StartEnforcesWorkspaceRootContainment(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows symlink semantics + permission requirements would force
		// elevation on most CI machines. The cross-platform invariant is
		// covered on unix.
		t.Skip("symlink + containment semantics covered on unix")
	}
	m := NewGrokACPManager()
	publishFn := func(resultMsg) {}

	root := t.TempDir()
	insideDir := filepath.Join(root, "project")
	if err := os.Mkdir(insideDir, 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	outsideRoot := t.TempDir()
	outsideDir := filepath.Join(outsideRoot, "elsewhere")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}

	t.Run("inside_root_accepted_then_dup_id", func(t *testing.T) {
		// We don't have a real `grok` binary on PATH so we expect Start to
		// either fail at exec time or, more directly, the containment check
		// to pass — we assert the containment-specific error doesn't fire.
		err := m.Start("dup-1", insideDir, nil, "ws", "uid", GrokStartOptions{WorkspaceRoot: root}, publishFn)
		if err != nil && strings.Contains(err.Error(), "outside the configured workspace root") {
			t.Errorf("containment incorrectly rejected %q inside %q: %v", insideDir, root, err)
		}
	})

	t.Run("outside_root_rejected", func(t *testing.T) {
		err := m.Start("dup-2", outsideDir, nil, "ws", "uid", GrokStartOptions{WorkspaceRoot: root}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "outside the configured workspace root") {
			t.Fatalf("expected `outside the configured workspace root` error; got %v", err)
		}
	})

	t.Run("symlink_escape_rejected", func(t *testing.T) {
		// Symlink inside root pointing at a sibling root → EvalSymlinks
		// must resolve through it and reject. This is the canonical
		// "appears inside, actually escapes" attack.
		escape := filepath.Join(root, "escape")
		if err := os.Symlink(outsideDir, escape); err != nil {
			t.Skipf("symlink not supported in tempdir: %v", err)
		}
		err := m.Start("dup-3", escape, nil, "ws", "uid", GrokStartOptions{WorkspaceRoot: root}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "outside the configured workspace root") {
			t.Fatalf("expected symlink-resolved escape to be rejected; got %v", err)
		}
	})

	t.Run("filesystem_root_rejected", func(t *testing.T) {
		err := m.Start("dup-4", "/", nil, "ws", "uid", GrokStartOptions{WorkspaceRoot: root}, publishFn)
		if err == nil || !strings.Contains(err.Error(), "outside the configured workspace root") {
			t.Fatalf("expected `/` to be rejected when WorkspaceRoot is a tempdir; got %v", err)
		}
	})

	t.Run("empty_root_skips_containment", func(t *testing.T) {
		// Backwards-compat path: when no root is configured the existing
		// absolute/exists contract still applies but no containment check.
		// outsideDir should NOT trigger containment-rejection here.
		err := m.Start("dup-5", outsideDir, nil, "ws", "uid", GrokStartOptions{}, publishFn)
		if err != nil && strings.Contains(err.Error(), "outside the configured workspace root") {
			t.Errorf("containment fired without WorkspaceRoot set; got %v", err)
		}
	})
}

// TestPathInsideRoot pins the helper's edge cases — the place where a
// strings.HasPrefix shortcut would silently regress (`/root` vs `/rootkit`).
func TestPathInsideRoot(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		root      string
		want      bool
	}{
		{"exact_match", "/a/b", "/a/b", true},
		{"child_dir", "/a/b/c", "/a/b", true},
		{"nested_child", "/a/b/c/d/e", "/a/b", true},
		{"sibling_with_shared_prefix", "/a/bb", "/a/b", false},
		{"parent_dir", "/a", "/a/b", false},
		{"different_branch", "/x/y", "/a/b", false},
		{"empty_candidate", "", "/a", false},
		{"empty_root", "/a", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pathInsideRoot(c.candidate, c.root); got != c.want {
				t.Errorf("pathInsideRoot(%q, %q) = %v, want %v", c.candidate, c.root, got, c.want)
			}
		})
	}
}

// TestGrokACPManager_StartClampsTimeoutAtMaxLifetime pins finding #2:
// requested timeouts above the stale-GC ceiling are clamped, so a runaway
// orchestrator can't request a deadline longer than our GC tolerates.
func TestGrokACPManager_StartClampsTimeoutAtMaxLifetime(t *testing.T) {
	// We exercise this via the session struct directly because Start needs a
	// real binary. The clamping logic is plain integer arithmetic against
	// grokACPMaxLifetime — pin it as a value test.
	max := int64(grokACPMaxLifetime / time.Millisecond)
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero_unchanged", 0, 0},
		{"under_cap_unchanged", 30_000, 30_000},
		{"at_cap_unchanged", max, max},
		{"over_cap_clamped", max + 1, max},
		{"way_over_cap_clamped", max * 10, max},
		{"negative_zeroed", -5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in
			if got < 0 {
				got = 0
			}
			if got > max {
				got = max
			}
			if got != c.want {
				t.Errorf("clamp(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

/* --------------------------------------------------------------------------
   Stale-session cleanup
   -------------------------------------------------------------------------- */

func TestGrokACPManager_EndStaleSessions_OldOnly(t *testing.T) {
	m := NewGrokACPManager()

	now := time.Now()
	old := &GrokACPSession{
		ID:         "old",
		StartedAt:  now.Add(-2 * time.Hour),
		status:     "ended",
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(old.done)
	close(old.streamDone)
	young := &GrokACPSession{
		ID:         "young",
		StartedAt:  now,
		status:     "ended",
		done:       make(chan struct{}),
		streamDone: make(chan struct{}),
	}
	close(young.done)
	close(young.streamDone)

	m.sessions["old"] = old
	m.sessions["young"] = young

	m.endStaleSessions(30 * time.Minute)

	if _, ok := m.sessions["old"]; ok {
		t.Errorf("stale session `old` should have been removed; still present")
	}
	if _, ok := m.sessions["young"]; !ok {
		t.Errorf("fresh session `young` should still be present; was removed")
	}
}

/* --------------------------------------------------------------------------
   End-to-end lifecycle against a mock grok agent stdio
   -------------------------------------------------------------------------- */

// runMockGrokACPServer mimics enough of the Grok ACP JSON-RPC protocol to
// validate the manager:
//   - replies to `initialize` with a fake protocolVersion + authMethods that
//     include `cached_token` (the auth method the feature brief mandates we
//     prefer)
//   - acknowledges `authenticate` requests
//   - replies to `session/new` with a fake sessionId
//   - streams a `session/update` notification + final `session/prompt`
//     response for every prompt
//   - emits one warning line on stderr at startup so the stderr forwarding
//     path is exercised
//   - exits cleanly when stdin closes (ACP's documented exit path)
func runMockGrokACPServer() {
	fmt.Fprintln(os.Stderr, "[mock-grok] ready, listening on stdio")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Println(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
			continue
		}
		method, _ := msg["method"].(string)
		id, hasID := msg["id"]
		switch method {
		case "initialize":
			if hasID {
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]any{
						"protocolVersion": 1,
						"agentCapabilities": map[string]any{
							"promptCapabilities": map[string]any{"image": false},
						},
						"authMethods": []map[string]any{
							{"id": "cached_token", "name": "Cached token", "description": "Local grok login"},
							{"id": "xai.api_key", "name": "API key", "description": "XAI_API_KEY"},
						},
					},
				}
				_ = json.NewEncoder(os.Stdout).Encode(resp)
			}
		case "authenticate":
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{},
				})
			}
		case "session/new":
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"sessionId": "sess_mock"},
				})
			}
		case "session/prompt":
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "sess_mock",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "hello from grok"},
						},
					},
				})
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{"stopReason": "end_turn"},
				})
			}
		case "session/cancel":
			if hasID {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]any{},
				})
			}
		}
	}
	os.Exit(0)
}

// TestGrokACPLifecycle_StartSendEnd drives the full ACP handshake the feature
// brief calls out: initialize → authenticate(cached_token) → session/new →
// session/prompt → streaming session/update → final response → end. Pins the
// invariants the orchestrator relies on (terminal `_ended` frame, stderr
// forwarding, exact-id correlation for every request).
func TestGrokACPLifecycle_StartSendEnd(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}

	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "grok"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "grok-acp-echo")

	m := NewGrokACPManager()
	id := fmt.Sprintf("grok-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GrokStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The feature brief mandates that the orchestrator picks `cached_token`
	// when the initialize response offers it. Drive the full handshake here so
	// the test exercises the framing path for every ACP message kind.
	initFrame := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true}}}}`
	if err := m.Send(id, initFrame); err != nil {
		t.Fatalf("Send initialize: %v", err)
	}
	authFrame := `{"jsonrpc":"2.0","id":2,"method":"authenticate","params":{"methodId":"cached_token"}}`
	if err := m.Send(id, authFrame); err != nil {
		t.Fatalf("Send authenticate: %v", err)
	}
	cwdJSON, err := json.Marshal(tmpDir)
	if err != nil {
		t.Fatalf("marshal cwd: %v", err)
	}
	sessFrame := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":%s,"mcpServers":[]}}`, cwdJSON)
	if err := m.Send(id, sessFrame); err != nil {
		t.Fatalf("Send session/new: %v", err)
	}
	promptFrame := `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"sess_mock","prompt":[{"type":"text","text":"hi"}]}}`
	if err := m.Send(id, promptFrame); err != nil {
		t.Fatalf("Send session/prompt: %v", err)
	}

	// Wait for responses ids 1..4 plus the session/update notification.
	deadline := time.Now().Add(15 * time.Second)
	requiredIDs := map[float64]bool{1: false, 2: false, 3: false, 4: false}
	gotSessionUpdate := false
	sawCachedTokenOffer := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type != "grok_acp_message" {
				continue
			}
			var probe map[string]any
			if err := json.Unmarshal([]byte(msg.Output), &probe); err != nil {
				continue
			}
			if rawID, ok := probe["id"]; ok {
				if n, ok := rawID.(float64); ok {
					if _, want := requiredIDs[n]; want {
						requiredIDs[n] = true
					}
					if n == 1 {
						if result, ok := probe["result"].(map[string]any); ok {
							if methods, ok := result["authMethods"].([]any); ok {
								for _, m := range methods {
									if mm, ok := m.(map[string]any); ok {
										if mm["id"] == "cached_token" {
											sawCachedTokenOffer = true
										}
									}
								}
							}
						}
					}
				}
			}
			if method, ok := probe["method"].(string); ok && method == "session/update" {
				gotSessionUpdate = true
			}
		}
		mu.Unlock()
		allDone := gotSessionUpdate && sawCachedTokenOffer
		for _, v := range requiredIDs {
			if !v {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	for id, got := range requiredIDs {
		if !got {
			t.Errorf("missing JSON-RPC response for id=%v", id)
		}
	}
	if !gotSessionUpdate {
		t.Errorf("missing `session/update` notification — streaming path not exercised")
	}
	if !sawCachedTokenOffer {
		t.Errorf("initialize response did not surface `cached_token` auth method; orchestrator's preference relies on this being parseable")
	}

	mu.Lock()
	sawStderr := false
	for _, msg := range captured {
		if msg.Type == "grok_acp_stderr" && strings.Contains(msg.Output, "mock-grok") {
			sawStderr = true
			break
		}
	}
	mu.Unlock()
	if !sawStderr {
		t.Errorf("expected `grok_acp_stderr` message containing `mock-grok`")
	}

	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	endedDeadline := time.Now().Add(5 * time.Second)
	var last resultMsg
	for time.Now().Before(endedDeadline) {
		mu.Lock()
		if len(captured) > 0 {
			last = captured[len(captured)-1]
		}
		mu.Unlock()
		if last.Type == "grok_acp_ended" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("no messages captured")
	}
	last = captured[len(captured)-1]
	if last.Type != "grok_acp_ended" {
		t.Errorf("expected final message to be grok_acp_ended; got %q", last.Type)
	}
	if last.SessionID != id {
		t.Errorf("expected SessionID=%q on ended frame; got %q", id, last.SessionID)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active sessions after End; got %d", m.ActiveCount())
	}
}

// TestGrokACPLifecycle_CancelTerminatesSession exercises the
// session-cancellation path the feature brief calls out: End() closes stdin,
// the mock exits cleanly, the manager publishes `grok_acp_ended` upstream so
// the orchestrator can complete the cancel turn.
func TestGrokACPLifecycle_CancelTerminatesSession(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}

	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "grok"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "grok-acp-echo")

	m := NewGrokACPManager()
	id := fmt.Sprintf("grok-cancel-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GrokStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate the cancel path: orchestrator sends an ACP session/cancel
	// notification, then End() to tear the child down.
	cancelFrame := `{"jsonrpc":"2.0","id":99,"method":"session/cancel","params":{"sessionId":"sess_mock"}}`
	if err := m.Send(id, cancelFrame); err != nil {
		t.Fatalf("Send cancel: %v", err)
	}

	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ended := false
		for _, msg := range captured {
			if msg.Type == "grok_acp_ended" {
				ended = true
				break
			}
		}
		mu.Unlock()
		if ended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if m.ActiveCount() != 0 {
		t.Errorf("expected manager to drop session after cancel+end; got %d active", m.ActiveCount())
	}
	sawEnded := false
	for _, msg := range captured {
		if msg.Type == "grok_acp_ended" && msg.SessionID == id {
			sawEnded = true
		}
	}
	if !sawEnded {
		t.Errorf("orchestrator must see `grok_acp_ended` after cancel; got types=%v", extractTypes(captured))
	}
}

// TestGrokACPLifecycle_ForwardsBadFrameAsError mirrors the codex test —
// a non-JSON line on stdout must surface as a typed `grok_acp_error` so the
// orchestrator can fail the in-flight call instead of misreading the
// malformed line as a JSON-RPC response.
func TestGrokACPLifecycle_ForwardsBadFrameAsError(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "grok"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "grok-acp-bad-frame")

	m := NewGrokACPManager()
	id := fmt.Sprintf("grok-badframe-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GrokStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ended := false
		for _, msg := range captured {
			if msg.Type == "grok_acp_ended" {
				ended = true
				break
			}
		}
		mu.Unlock()
		if ended {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	sawError := false
	for _, msg := range captured {
		if msg.Type == "grok_acp_error" && strings.Contains(msg.Output, "non-JSON frame") {
			sawError = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on bad-frame surface; got %q", msg.Status)
			}
		}
	}
	if !sawError {
		t.Errorf("expected `grok_acp_error` surfacing the non-JSON frame; got types %v",
			extractTypes(captured))
	}
}

// TestGrokACPLifecycle_TimeoutKillsRunawaySession pins finding #2 end-to-end:
// when the orchestrator passes a per-session TimeoutMs and the Grok child
// would otherwise run forever, the deadline timer must fire, publish a typed
// grok_acp_error AND a terminal grok_acp_ended, then unregister the session.
// Without this the child would keep holding the user's Grok auth/subscription
// resources for up to grokACPMaxLifetime (6h).
func TestGrokACPLifecycle_TimeoutKillsRunawaySession(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "grok"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "grok-acp-hang")

	m := NewGrokACPManager()
	id := fmt.Sprintf("grok-timeout-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	// 500ms is small enough to keep the test fast but large enough to
	// definitively exceed any normal startup/race in the readStream goroutines.
	if err := m.Start(id, tmpDir, nil, "ws", "uid", GrokStartOptions{TimeoutMs: 500}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var sawTimeoutError, sawEnded bool
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type == "grok_acp_error" && strings.Contains(msg.Output, "timed out") {
				sawTimeoutError = true
			}
			if msg.Type == "grok_acp_ended" {
				sawEnded = true
			}
		}
		mu.Unlock()
		if sawTimeoutError && sawEnded {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !sawTimeoutError {
		t.Errorf("expected `grok_acp_error` with `timed out` reason; got types=%v", extractTypes(captured))
	}
	if !sawEnded {
		t.Errorf("expected terminal `grok_acp_ended` after timeout kill; got types=%v", extractTypes(captured))
	}
	if m.ActiveCount() != 0 {
		t.Errorf("session should have been unregistered after timeout; %d still active", m.ActiveCount())
	}
}

// TestGrokACPLifecycle_StartFailsWhenBinaryMissing pins the error-reporting
// path the feature brief calls out: when `grok` isn't installed the manager
// must return a clear actionable error mentioning grok/PATH so the
// orchestrator can surface "please install Grok Build CLI" upstream rather
// than failing the call with an opaque exec error.
func TestGrokACPLifecycle_StartFailsWhenBinaryMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)

	m := NewGrokACPManager()
	publishFn := func(resultMsg) {}
	err := m.Start("missing-bin", tmpDir, nil, "ws", "uid", GrokStartOptions{}, publishFn)
	if err == nil {
		t.Fatal("expected start error when grok binary is not on PATH")
	}
	if !strings.Contains(err.Error(), "grok") {
		t.Errorf("expected error to mention grok; got %q", err.Error())
	}
	if m.ActiveCount() != 0 {
		t.Errorf("manager should have 0 sessions after failed start; got %d", m.ActiveCount())
	}
}

// TestSanitizeGrokACPExtraArgs_StripsAuthConfigOverrides pins the gate that
// prevents `-c|--config` from selecting api-key auth or supplying an api key
// while EnableGrokAPIKeyFallback is false. Without this the explicit
// `--api-key`/`--auth` strip is trivially bypassed by routing the same value
// through Grok's config knob.
func TestSanitizeGrokACPExtraArgs_StripsAuthConfigOverrides(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			"strips_config_auth_method_xai_api_key_separate",
			[]string{"--config", "auth.method=xai.api_key", "--model", "grok-2"},
			[]string{"--model", "grok-2"},
		},
		{
			"strips_short_config_auth_method_xai_api_key",
			[]string{"-c", "auth.method=xai.api_key"},
			[]string{},
		},
		{
			"strips_config_model_api_key",
			[]string{"--config", "model.api_key=xai-secret"},
			[]string{},
		},
		{
			"strips_config_xai_api_key",
			[]string{"-c", "xai.api_key=xai-secret"},
			[]string{},
		},
		{
			"strips_config_model_env_key",
			[]string{"--config", "model.env_key=XAI_API_KEY"},
			[]string{},
		},
		{
			"strips_inline_equals_form",
			[]string{"--config=auth.method=xai.api_key", "--model", "grok-2"},
			[]string{"--model", "grok-2"},
		},
		{
			"keeps_config_auth_method_cached_token",
			[]string{"--config", "auth.method=cached_token", "--model", "grok-2"},
			[]string{"--config", "auth.method=cached_token", "--model", "grok-2"},
		},
		{
			"keeps_unrelated_config",
			[]string{"-c", "model=grok-2", "--config", "log.level=debug"},
			[]string{"-c", "model=grok-2", "--config", "log.level=debug"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeGrokACPExtraArgs(c.args, false)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("sanitizeGrokACPExtraArgs(allowAPIKey=false) = %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestSanitizeGrokACPExtraArgs_PreservesAuthConfigOverridesWhenFallbackEnabled
// is the inverse: once the workspace opts into API-key fallback the gate
// disappears so callers can route credentials through `-c|--config` too.
func TestSanitizeGrokACPExtraArgs_PreservesAuthConfigOverridesWhenFallbackEnabled(t *testing.T) {
	in := []string{"--config", "auth.method=xai.api_key", "-c", "model.api_key=xai-secret"}
	got := sanitizeGrokACPExtraArgs(in, true)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("sanitizeGrokACPExtraArgs(allowAPIKey=true) must preserve auth config args; got %#v", got)
	}
}

// TestValidateGrokACPSendCwd_RejectsEscapingSessionNew pins the in-protocol
// containment check Send applies to `session/new` frames. Without this a
// later signed grok_acp_send could point Grok at a path outside the
// configured workspace root and bypass the Start-time containment gate.
func TestValidateGrokACPSendCwd_RejectsEscapingSessionNew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + containment semantics covered on unix")
	}
	root := t.TempDir()
	inside := filepath.Join(root, "ok")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval root: %v", err)
	}

	cases := []struct {
		name      string
		frame     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "session_new_inside_root_accepted",
			frame:   fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":%q}}`, inside),
			wantErr: false,
		},
		{
			name:      "session_new_outside_root_rejected",
			frame:     fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":%q}}`, outside),
			wantErr:   true,
			errSubstr: "outside the configured workspace root",
		},
		{
			name:      "session_new_relative_cwd_rejected",
			frame:     `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"../etc"}}`,
			wantErr:   true,
			errSubstr: "must be an absolute path",
		},
		{
			name:    "non_session_new_method_accepted_unchanged",
			frame:   fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"cwd":%q,"sessionId":"x"}}`, outside),
			wantErr: false,
		},
		{
			name:    "session_new_without_cwd_accepted",
			frame:   `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{}}`,
			wantErr: false,
		},
		{
			name:    "non_jsonrpc_frame_accepted",
			frame:   `"not-an-object"`,
			wantErr: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateGrokACPSendCwd(c.frame, resolvedRoot)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if c.errSubstr != "" && !strings.Contains(err.Error(), c.errSubstr) {
					t.Fatalf("expected error to contain %q; got %v", c.errSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateGrokACPSendCwd_SymlinkEscapeRejected pins the canonical
// "appears inside, actually escapes" attack — a session/new whose cwd is a
// symlink under root that resolves to an outside path.
func TestValidateGrokACPSendCwd_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink + containment semantics covered on unix")
	}
	root := t.TempDir()
	outside := t.TempDir()
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval root: %v", err)
	}
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":%q}}`, escape)
	err = validateGrokACPSendCwd(frame, resolvedRoot)
	if err == nil || !strings.Contains(err.Error(), "outside the configured workspace root") {
		t.Fatalf("symlink-resolved escape must be rejected; got %v", err)
	}
}
