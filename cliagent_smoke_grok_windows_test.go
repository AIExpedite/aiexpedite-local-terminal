//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_smoke_grok_windows_test.go — the Windows .cmd shim spawn shape.
   --------------------------------------------------------------------------
   `npm install -g` puts a `grok.cmd` batch shim on PATH, and cmd.exe re-parses
   the shim's `%*` before the real binary sees argv. The smoke must therefore
   route such a launch through cmd.exe with an explicit command line whose
   only variable parts (the two paths) ride in the environment. These cases
   run only on a Windows runner; the pure-string script assertions live in
   cliagent_smoke_grok_test.go and run everywhere.
   ------------------------------------------------------------------------ */

func TestGrokSmokeShimCommand_RoutesCmdShimThroughCmdExeAndHidesTheWindow(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "smoke prompt & (nonce)!.txt")
	launch := grokSmokeLaunch{
		Path:       `C:\Users\someone\AppData\Roaming\npm\grok.cmd`,
		Args:       buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[0], promptFile),
		Env:        []string{`PATH=C:\Windows`, `GROK_HOME=C:\isolated`},
		Dir:        `C:\isolated\workspace`,
		PromptFile: promptFile,
	}
	cmd, ok := grokSmokeShimCommand(context.Background(), launch)
	if !ok {
		t.Fatal("a .cmd shim launch must take the cmd.exe route")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Error("shim route lost HideWindow — cmd.exe would flash a console on the user's desktop")
	}
	line := cmd.SysProcAttr.CmdLine
	if !strings.HasPrefix(line, "/d /v:off /c call \"%"+grokSmokeShimPathEnv+"%\"") {
		t.Errorf("CmdLine = %q, want the /d /v:off /c call prefix with an env-indirected shim path", line)
	}
	if strings.Contains(line, launch.Path) || strings.Contains(line, promptFile) {
		t.Errorf("a path leaked into the raw cmd.exe command line: %q", line)
	}
	// Every fixed token survives verbatim (including the equals-form empty
	// --tools=), and the prompt path is handed over by env, not script.
	for _, arg := range launch.Args[:len(launch.Args)-2] {
		if !strings.Contains(" "+line+" ", " "+arg+" ") {
			t.Errorf("CmdLine dropped token %q: %q", arg, line)
		}
	}
	env := map[string]string{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env[grokSmokeShimPathEnv] != launch.Path || env[grokSmokeShimPromptEnv] != promptFile {
		t.Errorf("shim/prompt env = %q / %q", env[grokSmokeShimPathEnv], env[grokSmokeShimPromptEnv])
	}
	if env["GROK_HOME"] != `C:\isolated` || cmd.Dir != launch.Dir {
		t.Errorf("shim route changed the child's home/cwd: home=%q dir=%q", env["GROK_HOME"], cmd.Dir)
	}

	// A native binary is spawned directly.
	launch.Path = `C:\Users\someone\.grok\bin\grok.exe`
	if _, ok := grokSmokeShimCommand(context.Background(), launch); ok {
		t.Error("a native grok.exe must not be routed through cmd.exe")
	}
	if isGrokWindowsShim(`C:\tools\grok.ps1`) {
		t.Error("a .ps1 is not a cmd.exe batch shim (CreateProcess cannot run it); it must not take this route")
	}
}

// End to end through a REAL batch shim: cmd.exe re-parses the line, the shim's
// `%*` re-parses it again, and the child must still receive exactly the
// ladder's tokens — with the prompt path (spaces and metacharacters included)
// intact and no empty element dropped.
func TestGrokSmokeShimCommand_RoundTripsEveryTokenThroughARealShim(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mockExe := filepath.Join(dir, "grok-mock.exe")
	if err := copyTestBinary(testExe, mockExe); err != nil {
		t.Fatal(err)
	}
	// The shape npm writes: a batch file that forwards %* to the real program.
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0grok-mock.exe\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(t.TempDir(), "with space & (meta)")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(promptDir, "grok-prompt-x.txt")
	if err := os.WriteFile(promptFile, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, shape := range grokSmokeArgvShapes {
		args := buildGrokNoToolsSmokeArgs(shape, promptFile)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		stdout, stderr, runErr := runGrokSmokeCommand(ctx, grokSmokeLaunch{
			Path: shim, Args: args, Dir: dir, PromptFile: promptFile,
			Env: setEnvVar(os.Environ(), mockCLIEnvVar, "grok-smoke-argv-echo"),
		})
		cancel()
		if runErr != nil {
			t.Fatalf("rung %s: shim run failed: %v (stderr %q)", shape.ID, runErr, stderr)
		}
		var received []string
		for _, line := range bytes.Split(stdout, []byte("\n")) {
			var frame struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(bytes.TrimSpace(line), &frame) == nil && frame.Type == "text" {
				if strings.HasPrefix(frame.Text, "shim=") {
					if got := strings.TrimPrefix(frame.Text, "shim="); got != shim {
						t.Fatalf("rung %s: child saw shim %q via the env route, want %q", shape.ID, got, shim)
					}
					continue
				}
				received = append(received, frame.Text)
			}
		}
		if !reflect.DeepEqual(received, args) {
			t.Fatalf("rung %s: child received %#v, want %#v", shape.ID, received, args)
		}
		stream := parseGrokSmokeStream(stdout)
		if !stream.Ended {
			t.Fatalf("rung %s: terminal end frame missing from %q", shape.ID, stdout)
		}
	}
}

// The version precheck must survive the shim too: CreateProcess cannot start a
// batch file, so probing `grok.cmd` directly answers "" and runGrokSmoke would
// report binary_missing without ever exercising the shim-safe route.
func TestGrokSmokeProbeVersion_AnswersThroughACmdShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\nif \"%1\"==\"--version\" echo grok 1.0.13\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	stamp, err := os.Stat(shim)
	if err != nil {
		t.Fatal(err)
	}

	if got := grokProbeVersion(shim); got != "grok 1.0.13" {
		t.Fatalf("grokProbeVersion(%q) = %q, want the shim's version", shim, got)
	}
	// The answer is remembered on the binary stamp like every other version
	// probe: a second call spawns no child, which a now-broken shim proves.
	broken := []byte("@echo off\r\nexit /b 1\r\n")
	for len(broken) < int(stamp.Size()) { // same (size, mtime) => same cache key
		broken = append(broken, ' ')
	}
	if err := os.WriteFile(shim, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(shim, time.Time{}, stamp.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := grokProbeVersion(shim); got != "grok 1.0.13" {
		t.Fatalf("cached shim version = %q, want the remembered answer", got)
	}
}

// The poisoning interaction the single route exists to prevent: gatherCLIAgents
// runs first, PATH resolves the npm `grok.cmd` shim, and its --version answer is
// cached under (path, mtime, size). If that probe launched the batch file
// directly it cached "", and the smoke's own precheck read the negative back and
// reported binary_missing without ever spawning cmd.exe. Both callers — and the
// session_start smoke's probe — must answer the shim's real version.
func TestGrokVersionProbes_ShimAnswerSurvivesAGatherFirstOrdering(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\nif \"%1\"==\"--version\" echo grok 1.0.13\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetVersionProbeCache()
	t.Cleanup(resetVersionProbeCache)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	agents := gatherCLIAgents()
	if got := agents["grok"].Version; got != "grok 1.0.13" {
		t.Fatalf("gatherCLIAgents reported grok version %q, want the shim's version", got)
	}
	if got := grokProbeVersion(shim); got != "grok 1.0.13" {
		t.Fatalf("smoke precheck after gather = %q, want the shim's version (a cached direct-launch negative)", got)
	}
	if got := grokMaintenanceSmokeVersionProbeFn(shim); got != "grok 1.0.13" {
		t.Fatalf("session_start smoke precheck = %q, want the shim's version", got)
	}
}

// The retained session_start smoke takes the SAME cmd.exe route as the
// first-class probe. Before this, StartSession built the shim-safe argv and
// then handed `grok.cmd` straight to CreateProcess, which cannot start a batch
// file — so an older publisher's maintenance smoke on an npm install failed
// before the marker exactly as the un-shimmed probe did. Drive the real
// StartSession through a real batch shim and check the child received every
// ladder token and the session exited clean.
func TestStartSession_GrokMaintenanceSmokeLaunchesThroughACmdShim(t *testing.T) {
	stubGrokMaintenanceSmokePreflight(t)
	enableTestGrokLogin(t)
	resetCLISmokeState()
	t.Cleanup(resetCLISmokeState)

	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := copyTestBinary(testExe, filepath.Join(dir, "grok-mock.exe")); err != nil {
		t.Fatal(err)
	}
	// Only the shim carries the `grok` name, so PATH resolution lands on it.
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0grok-mock.exe\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, "grok-smoke-argv-echo")
	// The staged prompt file lives under the user's home; give it the
	// metacharacters the shim round-trip test uses so the path must travel
	// as environment data rather than script text.
	home := filepath.Join(t.TempDir(), "home & (co)")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERPROFILE", home)
	if got := resolveExecutable("grok"); !strings.EqualFold(got, shim) {
		t.Fatalf("resolveExecutable(grok) = %q, want the shim %q", got, shim)
	}

	var mu sync.Mutex
	var captured []resultMsg
	sm := NewSessionManager(nil)
	const id = "grok-smoke-cmd-shim"
	err = sm.StartSession(id, "grok", grokMaintenanceSmokeStartArgs(), dir, "ws", "uid", 30000, false, func(res resultMsg) {
		mu.Lock()
		captured = append(captured, res)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("StartSession through the shim: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		ended := false
		for _, m := range captured {
			if m.Type == "session_ended" {
				ended = true
			}
		}
		mu.Unlock()
		if ended || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	messages := append([]resultMsg(nil), captured...)
	mu.Unlock()
	ended := false
	for _, m := range messages {
		if m.Type == "session_ended" {
			ended = true
			if m.ExitCode != 0 {
				t.Fatalf("shim-launched smoke exited %d — the batch shim was not started through cmd.exe", m.ExitCode)
			}
		}
	}
	if !ended {
		t.Fatal("shim-launched smoke never ended")
	}
	// The argv-echo child streams every token it received; the ladder's fixed
	// flags must all be there, and the prompt must have arrived by file.
	received := concatStreamOutput(messages)
	if promptDir := filepath.Join(home, ".ai-expedite", "grok-prompts"); !strings.Contains(received, promptDir) {
		t.Errorf("child did not receive the staged prompt path under %q verbatim; streamed %q", promptDir, received)
	}
	// Only the cmd.exe route hands the shim path to the child through the
	// environment; the echo mock reports it, so its presence proves the
	// session took that route rather than a direct CreateProcess launch.
	if !strings.Contains(received, "shim="+shim) {
		t.Errorf("session_start smoke did not launch the shim through grokSmokeShimCommand; streamed %q", received)
	}
	want := buildGrokNoToolsSmokeArgs(grokSmokeShapeLadder(shim, "grok 1.0.13")[0], "unused")
	for _, token := range want[:len(want)-1] {
		if !strings.Contains(received, token) {
			t.Errorf("child did not receive %q through the shim; streamed %q", token, received)
		}
	}
	if strings.Contains(received, grokMaintenanceSmokePromptPrefix) {
		t.Errorf("prompt text reached argv through the shim: %q", received)
	}
}

// The per-attempt deadline must reach the process that actually runs Grok.
// exec.CommandContext kills only the intermediate cmd.exe; the npm `grok.cmd`
// shim starts the real binary as a separate child, which survives that kill,
// keeps the smoke's stdout/stderr handles open (so Run never returns) and
// leaks a provider process on every timed out attempt.
func TestGrokSmokeShimCommand_KillsTheShimsChildOnDeadline(t *testing.T) {
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mockExe := filepath.Join(dir, "grok-mock.exe")
	if err := copyTestBinary(testExe, mockExe); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "grok.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n\"%~dp0grok-mock.exe\" %*\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "child.pid")

	cmd, ok := grokSmokeShimCommand(context.Background(), grokSmokeLaunch{Path: shim, Args: []string{"--version"}})
	if !ok {
		t.Fatal("a .cmd shim launch must take the cmd.exe route")
	}
	if cmd.Cancel == nil || cmd.WaitDelay <= 0 {
		t.Fatalf("shim route left the deadline unbounded: cancel=%v waitDelay=%v", cmd.Cancel != nil, cmd.WaitDelay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		env := setEnvVar(os.Environ(), mockCLIEnvVar, "grok-smoke-hang")
		_, _, runErr := runGrokSmokeCommand(ctx, grokSmokeLaunch{
			Path: shim,
			Args: buildGrokNoToolsSmokeArgs(grokSmokeArgvShapes[0], filepath.Join(dir, "prompt.txt")),
			Dir:  dir,
			Env:  setEnvVar(env, mockGrokSmokeHangPidEnv, pidFile),
		})
		done <- runErr
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a killed attempt must report an error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run never returned after the deadline — the shim's child still holds the captured pipes")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("the shim's child never recorded its pid (%v); nothing to assert about the tree kill", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("unreadable child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !grokTestProcessAlive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = KillProcessTree(pid)
	t.Fatalf("the shim's child (pid %d) survived the deadline kill", pid)
}

// grokTestProcessAlive reports whether pid is still running, via the same
// snapshot the agent's own process inventory uses.
func grokTestProcessAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}
