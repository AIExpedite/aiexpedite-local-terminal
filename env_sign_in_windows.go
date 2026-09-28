//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// launchSignInWindow opens a new, visible console window running the sign-in
// through cmd.exe (so npm .cmd shims work and the window can pause on the
// result). Not waited for and not registered with the process registry: the
// window belongs to the person signing in and must outlive this command.
func launchSignInWindow(program string, args []string, title string) error {
	comspec := os.Getenv("ComSpec")
	if comspec == "" || !filepath.IsAbs(comspec) {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	dir, _ := os.UserHomeDir()
	return startInNewConsole(comspec, windowsSignInCommandLine(comspec, program, args, title), dir)
}

// createProcessFn is windows.CreateProcess behind a seam so a test can inspect
// exactly what a sign-in launch asks Windows for, without opening a window.
var createProcessFn = windows.CreateProcess

// startInNewConsole starts appName with the verbatim cmdLine in a NEW console
// whose standard handles the child takes from that console.
//
// It deliberately does not use os/exec. exec.Cmd always launches with
// STARTF_USESTDHANDLES and, for a nil Stdin/Stdout/Stderr, hands the child the
// NUL device — so a CREATE_NEW_CONSOLE child opened a real window (hosted by
// Windows Terminal when it is the default terminal) whose cmd.exe ran `title`
// (a console API call, so the tab WAS titled) but read every prompt from NUL
// and wrote every line to NUL. That was the blank "Sign in to GitHub" window:
// `gh auth login --web` ran with no visible one-time code and an EOF where its
// "Press Enter" prompt expected a key. Here no STARTF_USESTDHANDLES is set and
// no handles are inherited, so the child's stdin/stdout/stderr are the new
// console's own. The environment block is inherited (nil), i.e. this process's
// PATH as refreshCommandPath just left it.
func startInNewConsole(appName, cmdLine, dir string) error {
	app, err := windows.UTF16PtrFromString(appName)
	if err != nil {
		return err
	}
	line, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	si := &windows.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// No STARTF_USESTDHANDLES and no inherited handles: the child's std
	// handles are the new console's. CREATE_NEW_CONSOLE alone — never
	// CREATE_NO_WINDOW / DETACHED_PROCESS, which would leave it no visible
	// console at all.
	pi := &windows.ProcessInformation{}
	if err := createProcessFn(app, line, nil, nil, false,
		windows.CREATE_NEW_CONSOLE, nil, dirPtr, si, pi); err != nil {
		return errors.New("could not open the sign-in window: " + err.Error())
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
	return nil
}
