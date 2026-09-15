//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestHideWindow_CreatesNoConsoleWindowAtAll pins the Windows Terminal case:
// HideWindow alone is a request the default terminal app ignores, so a hidden
// child must also be created with CREATE_NO_WINDOW — and hiding must merge
// with flags a caller already set rather than replace them.
func TestHideWindow_CreatesNoConsoleWindowAtAll(t *testing.T) {
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP, CmdLine: "/c echo"}
	hideWindow(cmd)
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow not set")
	}
	if cmd.SysProcAttr.CreationFlags&CREATE_NO_WINDOW == 0 {
		t.Error("CREATE_NO_WINDOW not set: Windows Terminal would open a tab for this child")
	}
	if cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("hideWindow dropped a creation flag the caller had set")
	}
	if cmd.SysProcAttr.CmdLine != "/c echo" {
		t.Error("hideWindow dropped the caller's CmdLine")
	}

	fresh := exec.Command("cmd")
	hideWindow(fresh)
	if fresh.SysProcAttr == nil || !fresh.SysProcAttr.HideWindow || fresh.SysProcAttr.CreationFlags&CREATE_NO_WINDOW == 0 {
		t.Errorf("fresh SysProcAttr=%+v, want HideWindow + CREATE_NO_WINDOW", fresh.SysProcAttr)
	}
}

// TestNoHandRolledHideWindow fails when a source file builds its own
// `SysProcAttr{HideWindow: true}` instead of calling hideWindow. Every such
// site is a console the default terminal app may still show.
func TestNoHandRolledHideWindow(t *testing.T) {
	matched, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range matched {
		if strings.HasSuffix(file, "_test.go") || file == "agent_windows.go" {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if strings.Contains(code, "HideWindow: true") || strings.Contains(code, ".HideWindow = true") {
				t.Errorf("%s:%d sets HideWindow by hand — call hideWindow() so CREATE_NO_WINDOW travels with it", file, i+1)
			}
		}
	}
}
