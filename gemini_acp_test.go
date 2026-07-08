// gemini_acp_test.go
// -----------------------------------------------------------------------------
// Unit + lifecycle tests for GeminiACPManager. Unit tests pin the argv builder
// / env sanitizer / command classifier. The lifecycle tests drive a real
// GeminiACPManager against the test binary in TEST_MOCK_CLI_MODE=gemini-acp-*
// modes so we don't need a real `gemini` install on the test host.
//
// Shape mirrors grok_acp_test.go — both managers ride the shared ACP core, so
// the same battery of invariants must hold for each (no dropped frames,
// fail-fast on malformed input, terminal `_ended` frame, stdin-close graceful
// exit, typed startup errors, …). The core-level invariants (Seq ordering,
// oversize frames, publish-queue stalls, waitForExit races) are pinned once in
// grok_acp_test.go against the same code; this file focuses on the
// Gemini-specific seams and the gemini_acp_* result-type mapping.
// -----------------------------------------------------------------------------

package main

import (
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

// runMockGeminiACPServer is the gemini-branded twin of runMockGrokACPServer:
// same JSON-RPC echo loop (the ACP wire protocol is identical), different
// stderr banner so the stderr-forwarding assertion can prove which mock ran.
func runMockGeminiACPServer() {
	fmt.Fprintln(os.Stderr, "[mock-gemini] ready, listening on stdio")
	runMockACPEchoLoop()
}

/* --------------------------------------------------------------------------
   argv builder + classifier
   -------------------------------------------------------------------------- */

func TestIsGeminiACPCommand(t *testing.T) {
	cases := map[string]bool{
		"gemini_acp_start": true,
		"gemini_acp_send":  true,
		"gemini_acp_end":   true,
		"grok_acp_start":   false,
		"session_start":    false,
		"execute":          false,
		"":                 false,
		"gemini_acp_other": false,
	}
	for in, want := range cases {
		if got := isGeminiACPCommand(in); got != want {
			t.Errorf("isGeminiACPCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestBuildGeminiACPArgs pins the argv contract: `--experimental-acp` always
// leads, mode-switching prompt flags are stripped (any surviving -p/-i would
// flip gemini OUT of ACP stdio mode and break the orchestrator's JSON-RPC
// handshake), and benign flags pass through for gemini itself to validate.
func TestBuildGeminiACPArgs(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  []string
	}{
		{
			"no_extras",
			nil,
			[]string{"--experimental-acp"},
		},
		{
			"model_passthrough",
			[]string{"--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"duplicate_transport_flag_dropped",
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"prompt_flag_and_value_stripped",
			[]string{"-p", "hello there", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"prompt_equals_form_stripped",
			[]string{"--prompt=hello", "--model=gemini-3-pro"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			"prompt_interactive_stripped",
			[]string{"--prompt-interactive", "hi", "-i", "hi2", "-i=hi3"},
			[]string{"--experimental-acp"},
		},
		{
			// Positional tokens after `--` are a prompt, which flips gemini
			// out of ACP mode exactly like `-p` — the delimiter AND its tail
			// must both be dropped or the handshake never starts.
			"double_dash_delimiter_and_tail_dropped",
			[]string{"--", "positional prompt words"},
			[]string{"--experimental-acp"},
		},
		{
			"flags_before_double_dash_survive",
			[]string{"--model=gemini-3-pro", "--", "trailing prompt"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			// `-y`/`--yolo` and `--approval-mode` auto-approve tool calls,
			// bypassing the orchestrator-driven session/request_permission
			// flow — a signed gemini_acp_start must not be able to smuggle
			// them in through extras.
			"yolo_and_approval_mode_stripped",
			[]string{"-y", "--yolo", "--approval-mode", "yolo", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// `--include-directories` would widen the workspace beyond the
			// WorkspaceRoot containment Start enforces; the separate-token
			// value must be consumed with the flag.
			"include_directories_and_value_stripped",
			[]string{"--include-directories", "/outside", "--model=gemini-3-pro"},
			[]string{"--experimental-acp", "--model=gemini-3-pro"},
		},
		{
			// `--policy`/`--admin-policy` load extra policy-engine files
			// whose `allow` rules auto-approve tools without confirmation —
			// the same bypass as `--allowed-tools`, which gemini's own docs
			// deprecate in favor of the policy engine. Separate-token values
			// must be consumed with the flag.
			"policy_files_and_values_stripped",
			[]string{"--policy", "/tmp/allow.toml", "--admin-policy", "/tmp/admin", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// `--skip-trust` trusts the current workspace for the session
			// without prompting, re-enabling the project `.gemini/settings.json`
			// (tool auto-acceptance / project policies) this sanitizer blocks.
			// It is a boolean flag, so nothing follows it.
			"skip_trust_stripped",
			[]string{"--skip-trust", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			"privileged_equals_forms_stripped",
			[]string{"--yolo=true", "--skip-trust=true", "--approval-mode=auto_edit", "--include-directories=/outside", "--allowed-tools=run_shell_command", "--policy=/tmp/allow.toml", "--admin-policy=/tmp/admin"},
			[]string{"--experimental-acp"},
		},
		{
			// gemini's yargs parser accepts camelCase spellings of every
			// kebab-case flag — the deny list has to catch those too.
			"privileged_camelcase_spellings_stripped",
			[]string{"--approvalMode", "yolo", "--skipTrust", "--includeDirectories", "/outside", "--allowedTools", "run_shell_command", "--adminPolicy", "/tmp/admin"},
			[]string{"--experimental-acp"},
		},
		{
			// gemini's yargs parser expands a grouped short cluster like `-yd`
			// into `-y -d`, so an exact-match deny on `-y` alone would let the
			// yolo bit through. The whole cluster must be dropped.
			"grouped_yolo_cluster_stripped",
			[]string{"-yd", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// Clusters carrying the prompt short flags (`-p`/`-i`) flip gemini
			// out of ACP mode just like their standalone forms.
			"grouped_prompt_cluster_stripped",
			[]string{"-dp", "-iv", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// gemini's yargs-parser accepts a short-option equals value
			// (`-y=true`), which would re-enable yolo if it fell through the
			// exact-match deny list. Both the bare `-y=…` and a clustered
			// `-yd=…` carrying the yolo/prompt letters must be dropped.
			"short_yolo_equals_form_stripped",
			[]string{"-y=true", "-yd=1", "-ip=x", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "--model", "gemini-3-pro"},
		},
		{
			// A benign short cluster with no dangerous letters must survive so
			// legitimate gemini short options are not silently dropped.
			"benign_short_cluster_survives",
			[]string{"-vd", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "-vd", "--model", "gemini-3-pro"},
		},
		{
			// A benign short-option equals value must still pass through.
			"benign_short_equals_survives",
			[]string{"-v=1", "--model", "gemini-3-pro"},
			[]string{"--experimental-acp", "-v=1", "--model", "gemini-3-pro"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildGeminiACPArgs(c.extra)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("buildGeminiACPArgs(%v) = %#v, want %#v", c.extra, got, c.want)
			}
		})
	}
}

// TestSanitizeGeminiACPEnv pins the nested-agent strip: CLAUDECODE / CLAUDE_*
// / CODEX_IDE_* must not leak into the gemini child, while the rest of the
// shell environment (including GEMINI_*/GOOGLE_*) survives by omission.
func TestSanitizeGeminiACPEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CODEX_IDE_VERSION=0.1.0",
		"GEMINI_API_KEY=g-key",
		"GOOGLE_CLOUD_PROJECT=proj",
		"HOME=/home/user",
		// An inherited "trust the workspace" value that Gemini would not honor
		// anyway (only the exact value `true` counts) must be normalized to the
		// inert pinned-off value so a committed workspace `.env` cannot set it
		// (the env twin of the stripped `--skip-trust` flag).
		"GEMINI_CLI_TRUST_WORKSPACE=1",
		// IDE-companion variables must be dropped: the workspace path feeds
		// includeDirectories directly (the env twin of the stripped
		// `--include-directories` flag).
		"GEMINI_CLI_IDE_WORKSPACE_PATH=/outside/workspace",
		"GEMINI_CLI_IDE_SERVER_PORT=12345",
	}
	got := sanitizeGeminiACPEnv(in)
	for _, w := range []string{"PATH=/usr/bin", "GEMINI_API_KEY=g-key", "GOOGLE_CLOUD_PROJECT=proj", "HOME=/home/user"} {
		if !envContains(got, w) {
			t.Errorf("expected env to retain %q; got %v", w, got)
		}
	}
	for _, w := range []string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CODEX_IDE_VERSION=0.1.0", "GEMINI_CLI_TRUST_WORKSPACE=1", "GEMINI_CLI_IDE_WORKSPACE_PATH=/outside/workspace", "GEMINI_CLI_IDE_SERVER_PORT=12345"} {
		if envContains(got, w) {
			t.Errorf("expected env to strip %q; got %v", w, got)
		}
	}
	// Workspace trust must be pinned OFF exactly once when the inherited value
	// is anything other than the exact `true` Gemini honors.
	if !envContains(got, "GEMINI_CLI_TRUST_WORKSPACE=false") {
		t.Errorf("expected env to pin GEMINI_CLI_TRUST_WORKSPACE=false; got %v", got)
	}
	trustCount := 0
	for _, e := range got {
		if strings.HasPrefix(strings.ToUpper(e), "GEMINI_CLI_TRUST_WORKSPACE=") {
			trustCount++
		}
	}
	if trustCount != 1 {
		t.Errorf("expected exactly one GEMINI_CLI_TRUST_WORKSPACE entry, got %d in %v", trustCount, got)
	}
	// The IDE workspace path must be pinned EMPTY exactly once so a committed
	// workspace `.env` cannot set it (dotenv does not override a present var).
	idePathCount := 0
	for _, e := range got {
		if strings.HasPrefix(strings.ToUpper(e), "GEMINI_CLI_IDE_WORKSPACE_PATH=") {
			idePathCount++
			if e != "GEMINI_CLI_IDE_WORKSPACE_PATH=" {
				t.Errorf("expected IDE workspace path pinned empty; got %q", e)
			}
		}
	}
	if idePathCount != 1 {
		t.Errorf("expected exactly one GEMINI_CLI_IDE_WORKSPACE_PATH entry, got %d in %v", idePathCount, got)
	}

	// Even with no inherited trust/IDE variables, the pinned values are appended.
	gotClean := sanitizeGeminiACPEnv([]string{"PATH=/usr/bin"})
	if !envContains(gotClean, "GEMINI_CLI_TRUST_WORKSPACE=false") {
		t.Errorf("expected trust var pinned off when absent from input; got %v", gotClean)
	}
	if !envContains(gotClean, "GEMINI_CLI_IDE_WORKSPACE_PATH=") {
		t.Errorf("expected IDE workspace path pinned empty when absent from input; got %v", gotClean)
	}

	// The operator's own headless trust opt-in — the exact value `true`, the
	// only spelling Gemini's checkPathTrust honors — set in the agent's launch
	// environment (not reachable from any repo) must survive as a single pinned
	// entry, so Folder-Trust-enabled users keep Gemini's documented
	// non-interactive bypass instead of hitting FatalUntrustedWorkspaceError.
	gotTrusted := sanitizeGeminiACPEnv([]string{"PATH=/usr/bin", "GEMINI_CLI_TRUST_WORKSPACE=true"})
	trustedCount := 0
	for _, e := range gotTrusted {
		if strings.HasPrefix(strings.ToUpper(e), "GEMINI_CLI_TRUST_WORKSPACE=") {
			trustedCount++
			if e != "GEMINI_CLI_TRUST_WORKSPACE=true" {
				t.Errorf("expected operator trust opt-in preserved as exact `true`; got %q", e)
			}
		}
	}
	if trustedCount != 1 {
		t.Errorf("expected exactly one GEMINI_CLI_TRUST_WORKSPACE entry, got %d in %v", trustedCount, gotTrusted)
	}

	// Values Gemini would ignore (`TRUE`, `1`, empty) are normalized to the
	// inert `false`, never passed through and never promoted to `true`.
	for _, v := range []string{"GEMINI_CLI_TRUST_WORKSPACE=TRUE", "GEMINI_CLI_TRUST_WORKSPACE=1", "GEMINI_CLI_TRUST_WORKSPACE="} {
		gotOther := sanitizeGeminiACPEnv([]string{"PATH=/usr/bin", v})
		if !envContains(gotOther, "GEMINI_CLI_TRUST_WORKSPACE=false") {
			t.Errorf("expected inherited %q normalized to pinned false; got %v", v, gotOther)
		}
		if envContains(gotOther, v) || envContains(gotOther, "GEMINI_CLI_TRUST_WORKSPACE=true") {
			t.Errorf("expected inherited %q neither passed through nor promoted; got %v", v, gotOther)
		}
	}
}

/* --------------------------------------------------------------------------
   workspace settings screen
   -------------------------------------------------------------------------- */

// writeGeminiSettings writes cwd/.gemini/settings.json with the given body.
func writeGeminiSettings(t *testing.T, cwd, body string) {
	t.Helper()
	dir := filepath.Join(cwd, ".gemini")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .gemini: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

func TestScreenGeminiWorkspaceSettings_Allows(t *testing.T) {
	// No file, and benign / non-privileged files, must all pass.
	cases := map[string]string{
		"no file":             "",
		"empty object":        `{}`,
		"malformed json":      `{ not valid`,
		"benign default mode": `{"general":{"defaultApprovalMode":"default"}}`,
		"manual mode":         `{"general":{"defaultApprovalMode":"manual"}}`,
		"read-only plan mode": `{"general":{"defaultApprovalMode":"plan"}}`,
		"autoAccept false":    `{"tools":{"autoAccept":false}}`,
		"empty includeDirs":   `{"context":{"includeDirectories":[]}}`,
		"empty allowed tools": `{"tools":{"allowed":[]}}`,
		// `allowed` is only a pre-approved-tools grant under `tools`. A
		// non-empty `mcp.allowed` (permitted MCP server names) is a benign
		// restriction, and other blocks spell similar allowlists — none of
		// them auto-approve tools, so they must NOT block the start.
		"mcp allowed servers": `{"mcp":{"allowed":["trusted-server","other"]}}`,
		"nested allowlist":    `{"security":{"folderTrust":{"allowed":["/repo"]}}}`,
		"empty policyPaths":   `{"security":{"policyPaths":[]}}`,
		"empty mcpServers":    `{"mcpServers":{}}`,
		"empty discoveryCmd":  `{"tools":{"discoveryCommand":""}}`,
		"blank callCommand":   `{"tools":{"callCommand":"   "}}`,
		"empty hooks":         `{"hooks":{}}`,
		"unrelated settings":  `{"ui":{"theme":"dark"},"context":{"fileName":"AGENTS.md"}}`,
		// JSONC that Gemini's loader tolerates and that carries no privileged
		// value must still be treated as parseable-and-benign, not malformed.
		"benign line comment":   "{\n  // theme choice\n  \"ui\": {\"theme\": \"dark\"}\n}",
		"benign block comment":  "{\n  /* preferences */\n  \"ui\": {\"theme\": \"dark\"}\n}",
		"benign trailing comma": `{"ui":{"theme":"dark"},}`,
		// A `//` inside a string value is NOT a comment and must be preserved.
		"url in string": `{"context":{"fileName":"https://example.com/AGENTS.md"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if body != "" {
				writeGeminiSettings(t, cwd, body)
			}
			if err := screenGeminiWorkspaceSettings(cwd, ""); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestScreenGeminiWorkspaceSettings_Blocks(t *testing.T) {
	// Each of these positively grants a privilege the argv sanitizer strips,
	// via nested, camelCase, snake_case and legacy-flat spellings.
	cases := map[string]string{
		"nested yolo mode":    `{"general":{"defaultApprovalMode":"yolo"}}`,
		"auto_edit mode":      `{"general":{"defaultApprovalMode":"auto_edit"}}`,
		"camelCase approval":  `{"approvalMode":"yolo"}`,
		"autoAccept true":     `{"tools":{"autoAccept":true}}`,
		"legacy flat yolo":    `{"yolo":true}`,
		"includeDirectories":  `{"context":{"includeDirectories":["/etc","/root"]}}`,
		"nested policyPaths":  `{"security":{"policyPaths":["/tmp/allow.toml"]}}`,
		"allowed tools":       `{"tools":{"allowed":["run_shell_command"]}}`,
		"legacy allowedTools": `{"allowedTools":"run_shell_command"}`,
		"adminPolicy string":  `{"admin_policy_paths":"/tmp/admin"}`,
		"trusted mcp server":  `{"mcpServers":{"local":{"command":"npx","trust":true}}}`,
		// Any workspace-declared MCP server spawns local code during discovery
		// before any approval — blocked even when trust is false/absent.
		"untrusted mcp server": `{"mcpServers":{"local":{"command":"npx","trust":false}}}`,
		"bare mcp server":      `{"mcpServers":{"local":{"command":"npx"}}}`,
		// tools.discoveryCommand / tools.callCommand run a workspace-named
		// executable during tool discovery / on every custom-tool call —
		// pre-approval code execution, same vector as mcpServers.
		"tool discovery command":      `{"tools":{"discoveryCommand":"./scripts/discover.sh"}}`,
		"tool call command":           `{"tools":{"callCommand":"node tools/call.js"}}`,
		"legacy toolDiscoveryCommand": `{"toolDiscoveryCommand":"bin/discover"}`,
		"legacy toolCallCommand":      `{"toolCallCommand":"bin/call"}`,
		// Hook settings attach workspace commands to lifecycle events.
		"workspace hooks": `{"hooks":{"PreToolUse":[{"command":"./steal-creds.sh"}]}}`,
		// A privileged value hiding next to JSONC comments / trailing commas
		// must not slip past the screen just because strict JSON would reject it.
		"yolo behind line comment":  "{\n  // harmless\n  \"yolo\": true\n}",
		"yolo behind block comment": "{\n  /* harmless */ \"tools\": {\"autoAccept\": true}\n}",
		"privileged trailing comma": `{"yolo":true,}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			writeGeminiSettings(t, cwd, body)
			err := screenGeminiWorkspaceSettings(cwd, "")
			if err == nil {
				t.Fatalf("expected privilege-escalation error for %s, got nil", body)
			}
			if !strings.Contains(err.Error(), "settings.json") {
				t.Errorf("error should name the offending file; got %v", err)
			}
		})
	}
}

// TestScreenGeminiWorkspaceSettings_ProjectRootWalk covers the case where the
// session runs from a subdirectory but the privileged `.gemini/settings.json`
// lives at the workspace/project root above cwd — the file Gemini itself would
// load and apply. The screen must walk up to WorkspaceRoot and catch it, while
// still leaving anything above WorkspaceRoot (e.g. the user's global settings)
// alone.
func TestScreenGeminiWorkspaceSettings_ProjectRootWalk(t *testing.T) {
	t.Run("privileged settings at workspace root block a subdir start", func(t *testing.T) {
		root := t.TempDir()
		writeGeminiSettings(t, root, `{"general":{"defaultApprovalMode":"yolo"}}`)
		sub := filepath.Join(root, "packages", "app")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		err := screenGeminiWorkspaceSettings(sub, root)
		if err == nil {
			t.Fatal("expected the root-level yolo settings to block the subdir start")
		}
		if !strings.Contains(err.Error(), "settings.json") {
			t.Errorf("error should name the offending file; got %v", err)
		}
	})

	t.Run("benign subdir with clean chain up to root passes", func(t *testing.T) {
		root := t.TempDir()
		writeGeminiSettings(t, root, `{"ui":{"theme":"dark"}}`)
		sub := filepath.Join(root, "packages", "app")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := screenGeminiWorkspaceSettings(sub, root); err != nil {
			t.Fatalf("expected no error for benign chain, got %v", err)
		}
	})

	t.Run("privileged file above WorkspaceRoot with no Git root is not screened", func(t *testing.T) {
		// A settings file above the containment root that is NOT under an
		// enclosing Git project root must be ignored: without a `.git` marker
		// Gemini would not resolve it as project settings, and the climb must not
		// reach the user's global config.
		above := t.TempDir()
		writeGeminiSettings(t, above, `{"yolo":true}`)
		root := filepath.Join(above, "workspace")
		sub := filepath.Join(root, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := screenGeminiWorkspaceSettings(sub, root); err != nil {
			t.Fatalf("expected settings above WorkspaceRoot to be ignored, got %v", err)
		}
	})

	t.Run("privileged settings at the Git project root above WorkspaceRoot block the start", func(t *testing.T) {
		// The workspace is a subdirectory of a larger Git repo whose root carries
		// a privileged `.gemini/settings.json`. Gemini still loads that repo-root
		// file, so the screen must climb above WorkspaceRoot to the Git root and
		// catch it.
		repoRoot := t.TempDir()
		mkGitMarker(t, repoRoot)
		writeGeminiSettings(t, repoRoot, `{"mcpServers":{"local":{"command":"npx"}}}`)
		workspace := filepath.Join(repoRoot, "packages", "app")
		sub := filepath.Join(workspace, "src")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		err := screenGeminiWorkspaceSettings(sub, workspace)
		if err == nil {
			t.Fatal("expected repo-root settings above WorkspaceRoot to block the start")
		}
		if !strings.Contains(err.Error(), "settings.json") {
			t.Errorf("error should name the offending file; got %v", err)
		}
	})

	t.Run("benign settings at the Git project root above WorkspaceRoot pass", func(t *testing.T) {
		repoRoot := t.TempDir()
		mkGitMarker(t, repoRoot)
		writeGeminiSettings(t, repoRoot, `{"ui":{"theme":"dark"}}`)
		workspace := filepath.Join(repoRoot, "packages", "app")
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := screenGeminiWorkspaceSettings(workspace, workspace); err != nil {
			t.Fatalf("expected benign repo-root settings to pass, got %v", err)
		}
	})

	t.Run("workspace root reached through a symlink still screens the root settings", func(t *testing.T) {
		// Regression: when WorkspaceRoot and cwd spell the same contained tree
		// through different symlinks, a purely lexical Rel would treat cwd as
		// outside the root and screen cwd alone, skipping the privileged
		// root-level settings.json that Gemini still loads. The screen must
		// resolve symlinks (as acpManager.start's containment check does) so the
		// root file is caught.
		real := t.TempDir()
		writeGeminiSettings(t, real, `{"general":{"defaultApprovalMode":"yolo"}}`)
		sub := filepath.Join(real, "packages", "app")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		// WorkspaceRoot given via the symlink; cwd via the real path.
		err := screenGeminiWorkspaceSettings(sub, link)
		if err == nil {
			t.Fatal("expected the symlinked root's yolo settings to block the start")
		}
		if !strings.Contains(err.Error(), "settings.json") {
			t.Errorf("error should name the offending file; got %v", err)
		}
	})

	t.Run("climb stops at the Git root and never screens above it", func(t *testing.T) {
		// A privileged settings file ABOVE the Git project root must be ignored:
		// Gemini resolves project settings no higher than the Git root, so the
		// screen must not climb past it into unrelated ancestors.
		above := t.TempDir()
		writeGeminiSettings(t, above, `{"yolo":true}`)
		repoRoot := filepath.Join(above, "repo")
		mkGitMarker(t, repoRoot)
		workspace := filepath.Join(repoRoot, "packages", "app")
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := screenGeminiWorkspaceSettings(workspace, workspace); err != nil {
			t.Fatalf("expected settings above the Git root to be ignored, got %v", err)
		}
	})

	t.Run("home directory global settings are never screened", func(t *testing.T) {
		// Regression: when WorkingDirectory is left at its default home directory
		// and a session runs from a project under ~, the upward walk reaches home
		// and would screen the user's own global ~/.gemini/settings.json, wrongly
		// rejecting every ACP chat for users with global mcpServers/allowedTools/
		// approval-mode set. The global config is out of scope and must be skipped.
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir source
		writeGeminiSettings(t, home, `{"mcpServers":{"local":{"command":"npx"}},"general":{"defaultApprovalMode":"yolo"}}`)
		workspace := filepath.Join(home, "proj")
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		// workspaceRoot == home: the walk from proj up to home must drop home.
		if err := screenGeminiWorkspaceSettings(workspace, home); err != nil {
			t.Fatalf("expected the user's global ~/.gemini/settings.json to be left unscreened, got %v", err)
		}
	})
}

// mkGitMarker creates a `.git` directory in dir so hasGitEntry treats it as a
// Git project root.
func mkGitMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
}

/* --------------------------------------------------------------------------
   Send / Start validation (gemini_acp result-type + noun mapping)
   -------------------------------------------------------------------------- */

func TestGeminiACPManager_Send_NotFound(t *testing.T) {
	m := NewGeminiACPManager()
	err := m.Send("missing", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "gemini acp session missing not found") {
		t.Fatalf("expected gemini-branded `not found` error; got %v", err)
	}
}

func TestGeminiACPManager_Send_EndedSession(t *testing.T) {
	m := NewGeminiACPManager()
	id := "ended-fixture"
	fixture := &GeminiACPSession{ID: id, status: "ended", done: make(chan struct{}), streamDone: make(chan struct{})}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture

	err := m.Send(id, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected `has ended` error; got %v", err)
	}
}

// TestGeminiACPLifecycle_StartFailsWhenBinaryMissing pins the startup error
// mapping: a gemini binary that isn't on PATH must surface as a synchronous
// Start error (which the dispatcher publishes as `gemini_acp_error`) naming
// the transport, with no session left registered.
func TestGeminiACPLifecycle_StartFailsWhenBinaryMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PATH", tmpDir)

	m := NewGeminiACPManager()
	publishFn := func(resultMsg) {}
	err := m.Start("missing-bin", tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn)
	if err == nil {
		t.Fatal("expected start error when gemini binary is not on PATH")
	}
	if !strings.Contains(err.Error(), "failed to start gemini --experimental-acp") {
		t.Errorf("expected error to name the gemini transport; got %q", err.Error())
	}
	if m.ActiveCount() != 0 {
		t.Errorf("manager should have 0 sessions after failed start; got %d", m.ActiveCount())
	}
}

/* --------------------------------------------------------------------------
   Lifecycle against the mock gemini ACP server
   -------------------------------------------------------------------------- */

// startMockGeminiACP copies the test binary into a tempdir as `gemini`,
// points PATH at it and selects the given TEST_MOCK_CLI_MODE. Returns the
// tempdir used as the session cwd.
func startMockGeminiACP(t *testing.T, mode string) string {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := t.TempDir()
	mockName := "gemini"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	mockPath := filepath.Join(tmpDir, mockName)
	if err := copyTestBinary(testExe, mockPath); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mode)
	return tmpDir
}

// waitForResultType polls captured until a message of the given type shows up
// or the deadline passes.
func waitForResultType(mu *sync.Mutex, captured *[]resultMsg, msgType string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range *captured {
			if msg.Type == msgType {
				mu.Unlock()
				return true
			}
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestGeminiACPLifecycle_StartSendEnd drives the full ACP handshake through
// the Gemini driver: initialize → session/new → session/prompt → streaming
// session/update → final response → end. Pins the gemini_acp_* result-type
// mapping the orchestrator relies on (verbatim `gemini_acp_message` frames,
// `gemini_acp_stderr` forwarding, terminal `gemini_acp_ended`).
func TestGeminiACPLifecycle_StartSendEnd(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-echo")

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initFrame := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true}}}}`
	if err := m.Send(id, initFrame); err != nil {
		t.Fatalf("Send initialize: %v", err)
	}
	cwdJSON, err := json.Marshal(tmpDir)
	if err != nil {
		t.Fatalf("marshal cwd: %v", err)
	}
	sessFrame := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":%s,"mcpServers":[]}}`, cwdJSON)
	if err := m.Send(id, sessFrame); err != nil {
		t.Fatalf("Send session/new: %v", err)
	}
	promptFrame := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"sess_mock","prompt":[{"type":"text","text":"hi"}]}}`
	if err := m.Send(id, promptFrame); err != nil {
		t.Fatalf("Send session/prompt: %v", err)
	}

	// Wait for responses ids 1..3 plus the session/update notification.
	deadline := time.Now().Add(15 * time.Second)
	requiredIDs := map[float64]bool{1: false, 2: false, 3: false}
	gotSessionUpdate := false
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type != "gemini_acp_message" {
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
				}
			}
			if method, ok := probe["method"].(string); ok && method == "session/update" {
				gotSessionUpdate = true
			}
		}
		mu.Unlock()
		allDone := gotSessionUpdate
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

	for rid, got := range requiredIDs {
		if !got {
			t.Errorf("missing JSON-RPC response for id=%v", rid)
		}
	}
	if !gotSessionUpdate {
		t.Errorf("missing `session/update` notification — streaming path not exercised")
	}

	mu.Lock()
	sawStderr := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_stderr" && strings.Contains(msg.Output, "mock-gemini") {
			sawStderr = true
			break
		}
	}
	mu.Unlock()
	if !sawStderr {
		t.Errorf("expected `gemini_acp_stderr` message containing `mock-gemini`")
	}

	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 5*time.Second) {
		t.Fatalf("no gemini_acp_ended frame after End; got types %v", extractTypes(captured))
	}

	mu.Lock()
	defer mu.Unlock()
	last := captured[len(captured)-1]
	if last.Type != "gemini_acp_ended" {
		t.Errorf("expected final message to be gemini_acp_ended; got %q", last.Type)
	}
	if last.SessionID != id {
		t.Errorf("expected SessionID=%q on ended frame; got %q", id, last.SessionID)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active sessions after End; got %d", m.ActiveCount())
	}
}

// TestGeminiACPLifecycle_ForwardsBadFrameAsError pins the error mapping for a
// child that emits non-JSON on stdout — the classic failure shape of an old
// gemini build rejecting `--experimental-acp` with a plain-text usage error.
// The manager must surface a `gemini_acp_error` (not silently forward the
// garbage) and still deliver the terminal `gemini_acp_ended`.
func TestGeminiACPLifecycle_ForwardsBadFrameAsError(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-bad-frame")

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-badframe-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 10*time.Second) {
		t.Fatalf("no gemini_acp_ended frame; got types %v", extractTypes(captured))
	}

	mu.Lock()
	defer mu.Unlock()
	sawError := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_error" && strings.Contains(msg.Output, "non-JSON frame on gemini acp stdout") {
			sawError = true
			if msg.Status != "error" {
				t.Errorf("expected Status=error on bad-frame surface; got %q", msg.Status)
			}
		}
	}
	if !sawError {
		t.Errorf("expected `gemini_acp_error` surfacing the non-JSON frame; got types %v",
			extractTypes(captured))
	}
}

// TestGeminiACPLifecycle_CapturesQuotaFromStream pins the ACP-path bridge to
// captureGeminiUsageLimitLine: multi-turn Gemini chat runs through
// `gemini_acp_start` / the core readStream rather than the raw
// `session_start` path in session.go that already wires the hook, so without
// the spec.captureLine hook a 429/RESOURCE_EXHAUSTED frame never populates
// `gemini_usage_limit.json` and the CLI Agents card stays silent for the
// primary Gemini flow.
func TestGeminiACPLifecycle_CapturesQuotaFromStream(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	tmpDir := startMockGeminiACP(t, "gemini-acp-quota")

	cachePath := filepath.Join(tmpDir, "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	m := NewGeminiACPManager()
	id := fmt.Sprintf("gemini-quota-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "ws", "uid", GeminiStartOptions{}, publishFn); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !waitForResultType(&mu, &captured, "gemini_acp_ended", 10*time.Second) {
		t.Fatalf("no gemini_acp_ended frame; got types %v", extractTypes(captured))
	}

	mu.Lock()
	sawQuotaFrame := false
	for _, msg := range captured {
		if msg.Type == "gemini_acp_message" && strings.Contains(msg.Output, "RESOURCE_EXHAUSTED") {
			sawQuotaFrame = true
			break
		}
	}
	mu.Unlock()
	if !sawQuotaFrame {
		t.Fatalf("expected the RESOURCE_EXHAUSTED frame to be forwarded as gemini_acp_message; got types %v",
			extractTypes(captured))
	}

	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("usage-limit cache was never written at %s: %v", cachePath, err)
	}
	var state geminiUsageLimitState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("usage-limit cache is not valid JSON: %v\n%s", err, raw)
	}
	if state.Severity != geminiLimitReached {
		t.Errorf("expected severity=%q in cache, got %q", geminiLimitReached, state.Severity)
	}
	if !strings.Contains(state.Message, "Quota exceeded") {
		t.Errorf("expected the quota message to be captured, got %q", state.Message)
	}
}

/* --------------------------------------------------------------------------
   First-frame watchdog error mapping
   -------------------------------------------------------------------------- */

// TestGeminiACPManager_WatchFirstFrame_FiresAuthErrorOnSilence pins the
// gemini branding of the shared watchdog fail-fast: a session that never
// emits a stdout frame within the budget must produce exactly one
// `gemini_acp_error` whose message points the user at re-authenticating with
// `gemini`, instead of hanging at "Gemini ACP started" forever.
func TestGeminiACPManager_WatchFirstFrame_FiresAuthErrorOnSilence(t *testing.T) {
	m := NewGeminiACPManager()
	session := &GeminiACPSession{
		ID:          "gemini-silent-1",
		WorkspaceID: "ws",
		UID:         "uid",
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
		firstFrame:  make(chan struct{}),
	}

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	m.watchFirstFrame(session, publishFn, 30*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly 1 published frame, got %d: %+v", len(captured), captured)
	}
	got := captured[0]
	if got.Type != "gemini_acp_error" {
		t.Fatalf("expected gemini_acp_error, got %q", got.Type)
	}
	if got.Status != "error" {
		t.Fatalf("expected status error, got %q", got.Status)
	}
	if !strings.Contains(got.Output, "no output") || !strings.Contains(got.Output, "gemini") {
		t.Fatalf("expected actionable auth message, got %q", got.Output)
	}
}
