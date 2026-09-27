//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// launchSignInWindow asks Windows for an interactive console child: a new
// console, no STARTF_USESTDHANDLES, no inherited handles, the inherited
// (refreshed) environment, and the exact cmd.exe command line — so gh's
// "Authenticate Git …? (Y/n)" question, its one-time code and its "Press Enter"
// prompt are shown in the window and answered from its keyboard.
func TestLaunchSignInWindow_ProcessAttributes(t *testing.T) {
	type call struct {
		app, cmdLine, dir string
		inherit           bool
		flags             uint32
		env               *uint16
		si                windows.StartupInfo
		sa                bool
	}
	var got []call
	prev := createProcessFn
	createProcessFn = func(appName *uint16, commandLine *uint16, procSecurity *windows.SecurityAttributes, threadSecurity *windows.SecurityAttributes, inheritHandles bool, creationFlags uint32, env *uint16, currentDir *uint16, startupInfo *windows.StartupInfo, outProcInfo *windows.ProcessInformation) error {
		got = append(got, call{
			app:     windows.UTF16PtrToString(appName),
			cmdLine: windows.UTF16PtrToString(commandLine),
			dir:     windows.UTF16PtrToString(currentDir),
			inherit: inheritHandles,
			flags:   creationFlags,
			env:     env,
			si:      *startupInfo,
			sa:      procSecurity != nil || threadSecurity != nil,
		})
		return nil
	}
	t.Cleanup(func() { createProcessFn = prev })
	t.Setenv("ComSpec", `C:\Windows\System32\cmd.exe`)

	gh := `C:\Program Files\GitHub CLI\gh.exe`
	if err := launchSignInWindow(gh, []string{"auth", "login", "--web", "--git-protocol", "https"}, "Sign in to GitHub"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("CreateProcess calls = %d", len(got))
	}
	c := got[0]
	if c.app != `C:\Windows\System32\cmd.exe` {
		t.Errorf("app = %q", c.app)
	}
	wantLine := `"C:\Windows\System32\cmd.exe" /d /c title Sign in to GitHub & echo Sign in to GitHub & echo. & "C:\Program Files\GitHub CLI\gh.exe" auth login --web --git-protocol https & echo. & pause`
	if c.cmdLine != wantLine {
		t.Errorf("command line\n got %s\nwant %s", c.cmdLine, wantLine)
	}
	if c.flags != windows.CREATE_NEW_CONSOLE {
		t.Errorf("creation flags = %#x, want CREATE_NEW_CONSOLE only (no CREATE_NO_WINDOW / DETACHED_PROCESS)", c.flags)
	}
	if c.inherit {
		t.Error("handles must not be inherited: the child's std handles must be its new console's")
	}
	if c.si.Flags&windows.STARTF_USESTDHANDLES != 0 || c.si.StdInput != 0 || c.si.StdOutput != 0 || c.si.StdErr != 0 {
		t.Errorf("STARTUPINFO must not redirect std handles: flags=%#x in=%v out=%v err=%v", c.si.Flags, c.si.StdInput, c.si.StdOutput, c.si.StdErr)
	}
	if c.si.Flags&windows.STARTF_USESHOWWINDOW != 0 {
		t.Error("the window must not be hidden")
	}
	if c.env != nil {
		t.Error("the environment must be inherited (the refreshed PATH), not replaced")
	}
	if c.sa {
		t.Error("no security attributes expected")
	}
	if home, _ := os.UserHomeDir(); c.dir != home {
		t.Errorf("dir = %q, want the home directory", c.dir)
	}
}

// TestSignInWindowStdioIsTheConsole pins the root cause of the blank sign-in
// window. It opens real console windows, so it only runs when
// AIX_INTERACTIVE_WINDOW_TESTS=1.
//
// The child reports whether its stdin/stdout are redirected. Launched the old
// way (os/exec + CREATE_NEW_CONSOLE, nil Stdin/Stdout) both are redirected — to
// NUL — so nothing it prints is visible and every prompt reads EOF. Launched
// through startInNewConsole both are the new console.
func TestSignInWindowStdioIsTheConsole(t *testing.T) {
	if os.Getenv("AIX_INTERACTIVE_WINDOW_TESTS") != "1" {
		t.Skip("opens console windows; set AIX_INTERACTIVE_WINDOW_TESTS=1 to run")
	}
	comspec := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	probe := func(out string) string {
		return `"` + comspec + `" /d /c powershell -NoProfile -Command "'{0},{1}' -f [Console]::IsInputRedirected,[Console]::IsOutputRedirected | Set-Content -LiteralPath '` + out + `'"`
	}
	read := func(p string) string {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
				return strings.TrimSpace(string(b))
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("probe wrote nothing to %s", p)
		return ""
	}
	dir := t.TempDir()

	oldOut := filepath.Join(dir, "old.txt")
	c := exec.Command(comspec)
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: probe(oldOut), CreationFlags: CREATE_NEW_CONSOLE}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if got := read(oldOut); got != "True,True" {
		t.Fatalf("old launch: stdin,stdout redirected = %q, want True,True (NUL)", got)
	}

	newOut := filepath.Join(dir, "new.txt")
	if err := startInNewConsole(comspec, probe(newOut), dir); err != nil {
		t.Fatal(err)
	}
	if got := read(newOut); got != "False,False" {
		t.Fatalf("startInNewConsole: stdin,stdout redirected = %q, want False,False (the console)", got)
	}
}
