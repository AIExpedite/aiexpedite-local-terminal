//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A junction is linked from background work — the usage refresh on the
// Computers tab warms Grok model discovery, which sets up the isolated
// GROK_HOME — so cmd.exe must start hidden. configureGrokWindowsCommandLine
// used to assign a fresh SysProcAttr, dropping HideWindow and flashing a
// console window on the user's desktop mid-refresh.
func TestGrokWindowsCommands_HideConsoleWindow(t *testing.T) {
	for name, cmd := range map[string]*exec.Cmd{
		"junction": grokWindowsJunctionCommand(`C:\link`, `C:	arget`),
		"remove":   grokWindowsRemoveJunctionCommand(`C:\link`),
	} {
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
			t.Errorf("%s command does not hide its console window", name)
		}
		if cmd.SysProcAttr.CmdLine == "" {
			t.Errorf("%s command lost its explicit CmdLine", name)
		}
	}
}

func TestGrokWindowsJunctionCommands_HandleMetacharacterPaths(t *testing.T) {
	t.Setenv("AIEXPEDITE_GROK_UNSET_SENTINEL", "")
	root := t.TempDir()
	target := filepath.Join(root, `target%AIEXPEDITE_GROK_UNSET_SENTINEL%&(!)`)
	link := filepath.Join(root, `home&(!)`, `sessions%AIEXPEDITE_GROK_UNSET_SENTINEL%!`)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("create link parent: %v", err)
	}
	persisted := filepath.Join(target, "updates.jsonl")
	if err := os.WriteFile(persisted, []byte("synthetic"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	createCmd := grokWindowsJunctionCommand(link, target)
	if strings.Contains(createCmd.SysProcAttr.CmdLine, link) || strings.Contains(createCmd.SysProcAttr.CmdLine, target) {
		t.Fatalf("filesystem path leaked into raw cmd.exe command line: %q", createCmd.SysProcAttr.CmdLine)
	}
	if out, err := createCmd.CombinedOutput(); err != nil {
		t.Fatalf("create junction: %v (output: %s)", err, out)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(link); err == nil {
			_, _ = grokWindowsRemoveJunctionCommand(link).CombinedOutput()
		}
	})

	if _, err := os.Stat(filepath.Join(link, "updates.jsonl")); err != nil {
		t.Fatalf("read target through metacharacter junction: %v", err)
	}
	removeCmd := grokWindowsRemoveJunctionCommand(link)
	if strings.Contains(removeCmd.SysProcAttr.CmdLine, link) {
		t.Fatalf("filesystem path leaked into raw cmd.exe removal line: %q", removeCmd.SysProcAttr.CmdLine)
	}
	if out, err := removeCmd.CombinedOutput(); err != nil {
		t.Fatalf("remove junction: %v (output: %s)", err, out)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("junction still exists (lstat err = %v)", err)
	}
	if body, err := os.ReadFile(persisted); err != nil || string(body) != "synthetic" {
		t.Fatalf("target after junction removal = %q, err=%v", body, err)
	}
}

func TestRemoveIsolatedGrokHome_KeepsCaseVariantStoreWhenUnlinkFails(t *testing.T) {
	store := withTempGrokSessionStore(t)
	t.Setenv("GROK_HOME", t.TempDir())
	home, err := setupIsolatedGrokHome(false, grokACPDefaultModel)
	if err != nil {
		t.Fatalf("setupIsolatedGrokHome: %v", err)
	}

	lowercaseLink := filepath.Join(home, grokSessionsDirName)
	caseVariantLink := filepath.Join(home, "Sessions")
	temporaryLink := filepath.Join(home, "sessions-case-rename")
	if err := os.Rename(lowercaseLink, temporaryLink); err != nil {
		t.Fatalf("temporarily rename sessions junction: %v", err)
	}
	if err := os.Rename(temporaryLink, caseVariantLink); err != nil {
		t.Fatalf("case-rename sessions junction: %v", err)
	}
	persisted := writeTestGrokTranscript(t, store)

	syntheticErr := errors.New("synthetic unlink failure")
	err = removeIsolatedGrokHomeWithUnlink(home, func(string) error { return syntheticErr })
	if !errors.Is(err, syntheticErr) {
		t.Fatalf("removeIsolatedGrokHome error = %v, want synthetic unlink failure", err)
	}
	if _, err := os.Lstat(caseVariantLink); err != nil {
		t.Fatalf("case-variant sessions junction should remain: %v", err)
	}
	if body, err := os.ReadFile(persisted); err != nil || string(body) != testGrokTranscript {
		t.Fatalf("persistent transcript after partial cleanup = %q, err=%v", body, err)
	}
	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatalf("final isolated home cleanup: %v", err)
	}
}

/* --------------------------------------------------------------------------
   Codex: the same cmd.exe route, with Codex's own script renderer
   --------------------------------------------------------------------------
   Grok's renderer refuses every non-`--` token, so a Codex argv (`exec`,
   `login status`, `read-only`, the stdin `-`) needs codexSmokeShimScript. A
   regression here is a silent fallback to a plain spawn of `codex.cmd`, which
   CreateProcess cannot start.
   ------------------------------------------------------------------------ */

// writeCodexTestShim writes the shape npm puts on PATH — a batch file that
// forwards %* to the real program — next to a copy of this test binary.
func writeCodexTestShim(t *testing.T, dir string) string {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyTestBinary(testExe, filepath.Join(dir, "codex-mock.exe")); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0codex-mock.exe\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return shim
}

func TestCodexSmokeShimCommand_KeepsPathsInTheEnvironment(t *testing.T) {
	lastMessage := filepath.Join(t.TempDir(), `x%DEV%y`, codexSmokeLastMessageName)
	launch := codexSmokeLaunch{
		Path:            `C:\Users\someone\AppData\Roaming\npm\codex.cmd`,
		Args:            buildCodexSmokeArgs(codexSmokeArgvShapes[0], lastMessage),
		Env:             []string{`PATH=C:\Windows`},
		Dir:             `C:\tmp\smoke`,
		LastMessageFile: lastMessage,
	}
	cmd, ok := codexSmokeShimCommand(context.Background(), launch)
	if !ok {
		t.Fatal("a codex.cmd launch must take the cmd.exe route")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.Cancel == nil || cmd.WaitDelay <= 0 {
		t.Fatal("shim route must start hidden and bind the deadline to the whole process tree")
	}
	line := cmd.SysProcAttr.CmdLine
	if strings.Contains(line, launch.Path) || strings.Contains(line, lastMessage) {
		t.Fatalf("a path leaked into the raw cmd.exe command line: %q", line)
	}
	env := map[string]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env[codexSmokeShimPathEnv] != launch.Path || env[codexSmokeShimLastMessageEnv] != lastMessage || cmd.Dir != launch.Dir {
		t.Fatalf("shim env/dir = %q / %q / %q", env[codexSmokeShimPathEnv], env[codexSmokeShimLastMessageEnv], cmd.Dir)
	}
	if _, ok := codexSmokeShimCommand(context.Background(), codexSmokeLaunch{Path: `C:\tools\codex.exe`, Args: launch.Args}); ok {
		t.Fatal("a native codex.exe must be spawned directly")
	}
}

// End to end through a REAL batch shim in a directory holding a paired
// percent sequence: the child receives exactly both Codex argvs, with the
// last-message path literal.
func TestCodexSmokeShimCommand_RoundTripsBothArgvsThroughARealShim(t *testing.T) {
	t.Setenv("DEV", "EXPANDED")
	dir := filepath.Join(t.TempDir(), `npm%DEV%bin`)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := writeCodexTestShim(t, dir)
	lastMessage := filepath.Join(dir, codexSmokeLastMessageName)

	for _, args := range [][]string{
		buildCodexSmokeArgs(codexSmokeArgvShapes[0], lastMessage),
		buildCodexSmokeArgs(codexSmokeArgvShapes[1], lastMessage),
		{"login", "status"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stdout, stderr, runErr := runCodexSmokeCommand(ctx, codexSmokeLaunch{
			Path: shim, Args: args, Dir: dir, LastMessageFile: lastMessage, Prompt: "ignored",
			Env: setEnvVar(os.Environ(), mockCLIEnvVar, "grok-smoke-argv-echo"),
		})
		cancel()
		if runErr != nil {
			t.Fatalf("%v: shim run failed: %v (stderr %q)", args, runErr, stderr)
		}
		var received []string
		for _, line := range bytes.Split(stdout, []byte("\n")) {
			var frame struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(bytes.TrimSpace(line), &frame) == nil && frame.Type == "text" && !strings.HasPrefix(frame.Text, "shim=") {
				received = append(received, frame.Text)
			}
		}
		if !reflect.DeepEqual(received, args) {
			t.Fatalf("child received %q, want exactly %q", received, args)
		}
	}
}

// The login-status child takes the shim route too, so a logged-out npm install
// is DETECTED (a free not_logged_in) instead of the plain spawn failing to
// start, reading as inconclusive, and spending a turn on every smoke.
func TestCodexLoginStatus_RoutesACmdShimThroughCmdExe(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho Not logged in\r\nexit /b 1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCodexLoginStatusCmd(context.Background(), shim)
	if cmd.SysProcAttr == nil || !strings.Contains(cmd.SysProcAttr.CmdLine, "login status") {
		t.Fatalf("login status did not take the cmd.exe route: %+v", cmd.SysProcAttr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if loggedIn, known := codexAuthStatusProbe(ctx, shim); loggedIn || !known {
		t.Fatalf("codexAuthStatusProbe(shim) = (%v, %v), want a definite logout", loggedIn, known)
	}
}

// The version probe for a shim goes through cmd.exe, and gatherCLIAgents asks
// the SAME probe, so whichever asks first caches the real answer — never a ""
// from a failed plain spawn that would pin binary_missing for the smoke.
func TestCodexProbeVersion_ShimAnswersThroughOneCachedRoute(t *testing.T) {
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	dir := t.TempDir()
	shim := filepath.Join(dir, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho codex-cli 9.9.9\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexProbeVersion(shim); got != "codex-cli 9.9.9" {
		t.Fatalf("codexProbeVersion(shim) = %q, want the shim's answer", got)
	}
	// Remove cmd.exe from reach: a cached answer must not spawn again.
	t.Setenv("ComSpec", filepath.Join(dir, "no-such-cmd.exe"))
	t.Setenv("SystemRoot", dir)
	if got := codexProbeVersion(shim); got != "codex-cli 9.9.9" {
		t.Fatalf("second probe = %q, want the cached answer", got)
	}
}
