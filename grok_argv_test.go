package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

/* --------------------------------------------------------------------------
   grok_argv_test.go — the Grok smoke argv ladder invariants.
   --------------------------------------------------------------------------
   The regression this file guards: the frozen 12-token smoke argv carried an
   EMPTY element (`--tools ""`) that a Windows .cmd-shim re-parse can drop, so
   the child exited during option parsing and no marker was ever echoed. Every
   rung below must therefore be free of empty elements, keep `--tools` in the
   equals form, keep the prompt off argv, and stay inside the closed validator.
   ------------------------------------------------------------------------ */

const grokArgvTestPromptFile = "prompt-file-path"

func TestGrokSmokeArgvShapes_NoRungEmitsAnEmptyArgvElement(t *testing.T) {
	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, grokArgvTestPromptFile)
		for i, arg := range args {
			if arg == "" {
				t.Errorf("rung %s emits an empty argv element at index %d: %#v", shape.ID, i, args)
			}
			if strings.TrimSpace(arg) != arg {
				t.Errorf("rung %s emits a token with surrounding whitespace at index %d: %q", shape.ID, i, arg)
			}
		}
	}
}

func TestGrokSmokeArgvShapes_ToolsIsAlwaysTheEqualsForm(t *testing.T) {
	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, grokArgvTestPromptFile)
		sawTools := false
		for _, arg := range args {
			if arg == "--tools" {
				t.Errorf("rung %s emits the separate-value --tools form: %#v", shape.ID, args)
			}
			if arg == "--tools=" {
				sawTools = true
			}
		}
		if !sawTools {
			t.Errorf("rung %s does not disable built-in tools: %#v", shape.ID, args)
		}
	}
}

func TestGrokSmokeArgvShapes_EveryRungIsBoundedAndStreams(t *testing.T) {
	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, grokArgvTestPromptFile)
		joined := " " + strings.Join(args, " ") + " "
		for _, want := range []string{" --max-turns=1 ", " --output-format=streaming-json "} {
			if !strings.Contains(joined, want) {
				t.Errorf("rung %s lacks %q: %#v", shape.ID, strings.TrimSpace(want), args)
			}
		}
		if args[0] != "--output-format=streaming-json" {
			t.Errorf("rung %s must lead with the streaming contract: %#v", shape.ID, args)
		}
	}
}

// The marker nonce must never sit on argv (a process listing is readable by
// any local user), so no rung may carry an inline prompt flag, and the prompt
// file path is the ONLY caller-provided token — the last one.
func TestGrokSmokeArgvShapes_PromptNeverRidesOnArgv(t *testing.T) {
	const nonce = "AIEXPEDITE_GROK_SMOKE_OK_deadbeef"
	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, grokArgvTestPromptFile)
		for _, arg := range args {
			switch {
			case arg == "-p", arg == "--single", strings.HasPrefix(arg, "-p="), strings.HasPrefix(arg, "--single="),
				strings.HasPrefix(arg, "--prompt-json"):
				t.Errorf("rung %s carries an inline prompt flag %q: %#v", shape.ID, arg, args)
			case strings.Contains(arg, nonce), strings.Contains(arg, grokMaintenanceSmokePromptPrefix):
				t.Errorf("rung %s leaked prompt text into argv: %#v", shape.ID, args)
			}
		}
		n := len(args)
		if n < 2 || args[n-2] != grokSmokePromptFileFlag || args[n-1] != grokArgvTestPromptFile {
			t.Errorf("rung %s must end with `--prompt-file <path>`: %#v", shape.ID, args)
		}
	}
}

// Permission-bypass, provider, filesystem and response-shaping options are
// absent by construction, on every rung.
func TestGrokSmokeArgvShapes_CarryNoBypassOrLoaderFlags(t *testing.T) {
	banned := []string{
		"--always-approve", "--auto-approve", "--permission-mode", "--allow",
		"--model", "-m", "--config", "-c", "--plugin-dir", "--agent", "--agents",
		"--cwd", "-w", "--sandbox", "--rules", "--system-prompt-override",
		"--json-schema", "--debug-file", "--resume", "-r", "--session-id", "-s",
		"--continue", "--fork-session", "--restore-code", "--leader-socket",
	}
	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, grokArgvTestPromptFile)
		if arg, ok := grokNoToolsExternalLoaderArg(args); ok {
			t.Errorf("rung %s carries loader flag %q", shape.ID, arg)
		}
		for _, arg := range args {
			name := grokSmokeFlagName(arg)
			for _, b := range banned {
				if name == b {
					t.Errorf("rung %s carries banned flag %q: %#v", shape.ID, arg, args)
				}
			}
		}
	}
}

func TestGrokSmokeArgvShapes_CanonicalRungIsFirstAndCarriesNoAutoUpdateFlag(t *testing.T) {
	if len(grokSmokeArgvShapes) < 2 {
		t.Fatalf("the ladder needs a fallback rung, got %d", len(grokSmokeArgvShapes))
	}
	got := buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[0], grokArgvTestPromptFile)
	want := []string{
		"--output-format=streaming-json", "--tools=",
		"--disable-web-search", "--no-subagents", "--max-turns=1",
		grokSmokePromptFileFlag, grokArgvTestPromptFile,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical rung = %#v, want %#v", got, want)
	}
	for _, arg := range got {
		if arg == "--no-auto-update" || arg == "--verbatim" {
			t.Fatalf("canonical rung must not carry %s (rejected at the root command by current builds): %#v", arg, got)
		}
	}
}

// validateGrokSmokeShape is the single contract check both call sites use. It
// must accept exactly the rungs (with any non-empty staged path) and refuse
// injected options, an empty element, and an inline prompt.
func TestValidateGrokSmokeShape_AcceptsEveryRungAndRejectsInjectedFlags(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "smoke prompt.txt")
	for _, shape := range grokSmokeArgvShapes {
		valid := buildGrokNoToolsSmokeArgs(shape, promptFile)
		if err := validateGrokSmokeShape(valid); err != nil {
			t.Fatalf("rung %s rejected: %v", shape.ID, err)
		}
		for _, extra := range [][]string{
			{"--always-approve"},
			{"--permission-mode=bypassPermissions"},
			{"--model", "provider-sentinel"},
			{"--plugin-dir", "loader-path-sentinel"},
			{"--json-schema", `{"secret":"schema-sentinel"}`},
			{""},
			{"--tools", ""},
			{"-p", "inline-prompt-sentinel"},
		} {
			injected := append(append([]string(nil), valid[:len(valid)-2]...), extra...)
			injected = append(injected, grokSmokePromptFileFlag, promptFile)
			err := validateGrokSmokeShape(injected)
			if err == nil {
				t.Errorf("rung %s accepted injected %#v", shape.ID, extra)
				continue
			}
			for _, sentinel := range []string{"provider-sentinel", "loader-path-sentinel", "schema-sentinel", "inline-prompt-sentinel", promptFile} {
				if strings.Contains(err.Error(), sentinel) {
					t.Errorf("shape rejection leaked %q: %v", sentinel, err)
				}
			}
		}
	}
}

// The retryable set is DERIVED from the ladder: exactly the flags some rung
// carries and another drops — in BOTH directions, since the walk order
// follows the installed version — and never one every rung shares.
func TestGrokSmokeRetryableFlags_DerivedFromTheLadder(t *testing.T) {
	want := map[string]bool{"--disable-web-search": true, "--no-subagents": true, "--no-auto-update": true}
	got := map[string]bool{}
	for _, flag := range grokSmokeRetryableFlags {
		got[flag] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retryable flags = %v, want %v", got, want)
	}
	for _, shared := range []string{"--tools", "--max-turns", "--output-format", grokSmokePromptFileFlag} {
		if got[shared] {
			t.Errorf("%s is carried by every rung and must not be retryable", shared)
		}
	}
}

// The ladder is ordered for the installed build: a release that predates the
// hardened rung's isolation switches tries the legacy rung first, everything
// else (including an unparseable version) keeps the canonical order. Every
// rung is always present, so a wrong guess costs one free spawn, not the run.
func TestGrokSmokeArgvShapesForVersion_OrdersTheRungTheBuildDocumentsFirst(t *testing.T) {
	canonical := grokSmokeArgvShapes[0].ID
	legacy := grokSmokeArgvShapes[1].ID
	for _, tc := range []struct {
		version string
		first   string
	}{
		{"grok 1.0.13", canonical},
		{"grok 1.0.14 (5e9a1c2) [stable]", canonical},
		{"grok 1.1.0", canonical},
		{"grok 2.0.0", canonical},
		{"grok 1.0.12", legacy},
		{"grok 1.0.5", legacy},
		{"grok 0.9.0", legacy},
		{"", canonical},
		{"not a version", canonical},
	} {
		ladder := grokSmokeArgvShapesForVersion(tc.version)
		if len(ladder) != len(grokSmokeArgvShapes) {
			t.Fatalf("%q: ladder has %d rungs, want every rung (%d)", tc.version, len(ladder), len(grokSmokeArgvShapes))
		}
		if ladder[0].ID != tc.first {
			t.Errorf("%q: first rung = %q, want %q", tc.version, ladder[0].ID, tc.first)
		}
		seen := map[string]bool{}
		for _, shape := range ladder {
			if seen[shape.ID] {
				t.Errorf("%q: rung %q appears twice", tc.version, shape.ID)
			}
			seen[shape.ID] = true
		}
	}
	// Reordering must never mutate the canonical ladder itself.
	if grokSmokeArgvShapes[0].ID != canonical || grokSmokeArgvShapes[1].ID != legacy {
		t.Fatal("grokSmokeArgvShapesForVersion mutated grokSmokeArgvShapes")
	}
}

// The signed session_start wire request stays byte-stable so older publishers
// keep signing the same tokens; its prompt is the only variable part.
func TestGrokSmokeWireRequest_IsTheLegacyEightTokenContract(t *testing.T) {
	want := []string{"--tools", "", "--disable-web-search", "--no-subagents", "--max-turns", "1", "--verbatim"}
	if !reflect.DeepEqual(grokSmokeWireRequest, want) {
		t.Fatalf("wire request = %#v, want the legacy contract %#v", grokSmokeWireRequest, want)
	}
	prompt := grokMaintenanceSmokePromptPrefix + "MARKER"
	got, err := validateGrokSmokeRequest(append(append([]string(nil), want...), prompt))
	if err != nil || got != prompt {
		t.Fatalf("validateGrokSmokeRequest = %q, %v", got, err)
	}
	if !grokMaintenanceSmokeRequest(append(append([]string(nil), want...), prompt)) {
		t.Fatal("the legacy wire request must still be recognised for promotion")
	}
}
