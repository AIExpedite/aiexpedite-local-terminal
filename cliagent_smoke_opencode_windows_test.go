//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_opencode_windows_test.go — the Windows .cmd shim spawn shape.
   --------------------------------------------------------------------------
   `npm install -g opencode-ai` puts an `opencode.cmd` batch shim on PATH, and
   CreateProcess cannot start a batch file at all — which is why the deployed
   Windows smoke failed identically before and after a CLI update. Such a launch
   must therefore go through cmd.exe with an explicit command line whose every
   variable part rides in the environment.
   ------------------------------------------------------------------------ */

const openCodeTestShimPath = `C:\Users\some one\AppData\Roaming\npm & co\opencode.cmd`

// openCodeShimArgEnv reads back the environment slot a token was carried in.
func openCodeShimArgEnv(t *testing.T, env []string, i int) (string, bool) {
	t.Helper()
	prefix := fmt.Sprintf(openCodeShimArgEnvFmt, i) + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

func TestOpenCodeShimCommand_CarriesEveryTokenInTheEnvironmentAndHidesTheWindow(t *testing.T) {
	// Every token — flag NAMES included, not just values — so the renderer
	// imposes no argv vocabulary of its own and a shim install runs exactly the
	// argv a native install does.
	argvs := map[string][]string{
		"the no-resume rung":     buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""),
		"the resume rung":        buildOpenCodeRunArgs(openCodeRunShapeResume, "ses_abc123"),
		"a model and a variant":  {"run", "--format", "json", "--model", "anthropic/claude-sonnet-4-5", "--variant", "high"},
		"an unknown caller flag": {"run", "--format", "json", "--not-a-known-flag", "some value & (more)|x^y"},
		"the models diagnostic":  {"models"},
		"the two-token auth":     {"auth", "list"},
	}
	for name, args := range argvs {
		t.Run(name, func(t *testing.T) {
			cmd, ok := openCodeShimCommand(context.Background(), openCodeLaunch{
				Path: openCodeTestShimPath,
				Args: args,
				Env:  []string{`PATH=C:\Windows`},
				Dir:  `C:\work`,
			})
			if !ok {
				t.Fatalf("shim route refused a renderable launch: %q", args)
			}
			line := cmd.SysProcAttr.CmdLine

			// The script body holds NO argv text: only the two reference kinds.
			for _, arg := range args {
				if strings.Contains(line, arg) {
					t.Fatalf("command line %q interpolates the token %q", line, arg)
				}
			}
			if strings.Contains(line, openCodeTestShimPath) {
				t.Fatalf("command line %q interpolates the binary path", line)
			}
			if !strings.Contains(line, `"%`+openCodeShimPathEnv+`%"`) {
				t.Fatalf("command line %q does not reference the shim path slot", line)
			}
			// `call` would apply a SECOND percent-expansion pass over the
			// already-expanded line and mangle a path holding `%DEV%`.
			if strings.Contains(strings.ToLower(line), " call ") {
				t.Fatalf("command line %q leads with `call`", line)
			}
			if !strings.Contains(line, "/v:off") {
				t.Fatalf("command line %q does not disable delayed expansion", line)
			}

			// Every token round-trips through its own environment slot, in order.
			for i, arg := range args {
				got, found := openCodeShimArgEnv(t, cmd.Env, i)
				if !found {
					t.Fatalf("token %d (%q) has no environment slot", i, arg)
				}
				if got != arg {
					t.Fatalf("token %d round-tripped as %q, want %q", i, got, arg)
				}
				if !strings.Contains(line, `"%`+fmt.Sprintf(openCodeShimArgEnvFmt, i)+`%"`) {
					t.Fatalf("command line %q does not reference slot %d", line, i)
				}
			}
			// A binary path with spaces AND `&` survives as data.
			if got, _ := openCodeShimPathEnvValue(cmd.Env); got != openCodeTestShimPath {
				t.Fatalf("shim path round-tripped as %q", got)
			}
			// A background probe on a windowless tray app must not flash a
			// console, and the deadline must reach the shim's Node child.
			if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
				t.Error("the shim child does not hide its console window")
			}
			if cmd.Cancel == nil {
				t.Error("the shim child has no process-tree cancel bound")
			}
			if cmd.WaitDelay != grokShimWaitDelay {
				t.Errorf("WaitDelay = %v, want %v", cmd.WaitDelay, grokShimWaitDelay)
			}
			if cmd.Dir != `C:\work` {
				t.Errorf("Dir = %q", cmd.Dir)
			}
		})
	}
}

func openCodeShimPathEnvValue(env []string) (string, bool) {
	for _, e := range env {
		if strings.HasPrefix(e, openCodeShimPathEnv+"=") {
			return strings.TrimPrefix(e, openCodeShimPathEnv+"="), true
		}
	}
	return "", false
}

func TestOpenCodeShimScript_RefusalsAreExactlyTheDocumentedSet(t *testing.T) {
	// ONE character policy: a token is REFUSED rather than escaped when it
	// contains something that breaks the quoting or gets re-read by cmd's single
	// expansion pass. Everything else must be SUPPORTED, not refused.
	supported := []string{
		"run", "--format", "json", "--model", "anthropic/claude-sonnet-4-5",
		"a value with spaces", "amp&ersand", "caret^x", "(paren)", "pipe|x",
		"semi;colon", "less<greater>", "back\\slash", "single'quote",
	}
	for _, token := range supported {
		if _, ok := openCodeShimScript([]string{token}); !ok {
			t.Errorf("token %q was refused; spaces and metacharacters must round-trip", token)
		}
	}

	refused := map[string]string{
		"a double quote":       `say "hi"`,
		"a percent":            "100%done",
		"a carriage return":    "a\rb",
		"a line feed":          "a\nb",
		"another control char": "a\x07b",
		"a DEL":                "a\x7fb",
		"an empty token":       "",
	}
	for name, token := range refused {
		if _, ok := openCodeShimScript([]string{token}); ok {
			t.Errorf("%s (%q) was rendered; it must be refused", name, token)
		}
	}

	// Too many tokens is the other refusal.
	tooMany := make([]string, openCodeShimMaxArgs+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	if _, ok := openCodeShimScript(tooMany); ok {
		t.Errorf("%d tokens were rendered; the cap is %d", len(tooMany), openCodeShimMaxArgs)
	}
	atCap := tooMany[:openCodeShimMaxArgs]
	if _, ok := openCodeShimScript(atCap); !ok {
		t.Errorf("%d tokens (exactly the cap) were refused", len(atCap))
	}
}

func TestNewOpenCodeCmd_RefusedShimRenderIsATypedLaunchFailure(t *testing.T) {
	// On Windows a refusal is TERMINAL: falling back to a direct `.cmd` spawn
	// would fail in CreateProcess anyway, so failing closed as launch_error is
	// the honest answer.
	cmd, err := newOpenCodeCmd(context.Background(), openCodeLaunch{
		Path: openCodeTestShimPath,
		Args: []string{"run", `a "quoted" token`},
		Env:  os.Environ(),
	})
	if err == nil {
		t.Fatalf("an unrenderable shim launch was accepted: %+v", cmd)
	}
	if err != errOpenCodeShimUnrenderable {
		t.Fatalf("error = %v, want errOpenCodeShimUnrenderable", err)
	}
	category, diagnostic, _ := classifyOpenCodeSmokeRun(false, nil, nil, err, "marker")
	if diagnostic != cliSmokeDiagnosticLaunchError || category != cliUsageErrorProviderUnavailable {
		t.Fatalf("a refused render classified as (%q, %q), want launch_error", category, diagnostic)
	}
}

func TestNewOpenCodeCmd_NativeBinaryIsSpawnedDirectly(t *testing.T) {
	cmd, err := newOpenCodeCmd(context.Background(), openCodeLaunch{
		Path: `C:\Program Files\opencode\opencode.exe`,
		Args: buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""),
		Env:  []string{`PATH=C:\Windows`},
		Dir:  `C:\work`,
	})
	if err != nil {
		t.Fatalf("native launch refused: %v", err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.CmdLine != "" {
		t.Fatalf("a native binary took the cmd.exe route: %q", cmd.SysProcAttr.CmdLine)
	}
	if !strings.HasSuffix(strings.ToLower(cmd.Path), "opencode.exe") {
		t.Fatalf("native launch resolved to %q", cmd.Path)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Error("the native child does not hide its console window")
	}
}
