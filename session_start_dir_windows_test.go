//go:build windows

package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartDirUnavailableErr_Windows(t *testing.T) {
	for _, errno := range []syscall.Errno{15, 21, 67} {
		err := &fs.PathError{Op: "CreateFile", Path: `X:\gone`, Err: errno}
		if !startDirUnavailableErr(err) {
			t.Fatalf("Errno %d must read as unavailable", errno)
		}
	}
	// Access denied stays unknown: the spawn answers it as before.
	if startDirUnavailableErr(&fs.PathError{Op: "CreateFile", Path: `C:\locked`, Err: syscall.Errno(5)}) {
		t.Fatal("ERROR_ACCESS_DENIED must not read as unavailable")
	}
}

// A saved Default working directory on a drive letter that no longer has a
// volume (an unplugged USB disk, a remapped drive): the case Windows answered
// with "The directory name is invalid." at spawn.
func TestResolveSessionStartDir_AMissingDriveFallsBack(t *testing.T) {
	var gone string
	for letter := 'Z'; letter >= 'D'; letter-- {
		root := fmt.Sprintf(`%c:\`, letter)
		if _, err := os.Stat(root); err != nil {
			gone = root + `aiexpedite`
			break
		}
	}
	if gone == "" {
		t.Skip("no unassigned drive letter on this runner")
	}
	own := t.TempDir()
	got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: own}, utilityStart(gone, cwdFallbackDeviceHome))
	if err != nil || got != own || from != gone {
		t.Fatalf("resolve(%s + marker) = %q, %q, %v; want %q", gone, got, from, err, own)
	}
	_, _, _, err = resolveSessionStartDir(&Config{WorkingDirectory: own}, utilityStart(gone, ""))
	if sessionStartDirErrorFrom(err) == nil {
		t.Fatalf("resolve(%s, no marker) = %v; want TERMINAL_CWD_NOT_FOUND", gone, err)
	}
}

// Existing folders keep their spelling: no normalisation of drive-letter case,
// spaces or non-ASCII names.
func TestResolveSessionStartDir_KeepsAnExistingWindowsPathVerbatim(t *testing.T) {
	base := t.TempDir()
	dir := base + `\OneDrive - Contoso\Código`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(dir[:1]) + dir[1:]
	for _, spelling := range []string{dir, lower} {
		got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: t.TempDir()}, utilityStart(spelling, cwdFallbackDeviceHome))
		if err != nil || got != spelling || from != "" {
			t.Fatalf("resolve(%q) = %q, %q, %v; want it verbatim", spelling, got, from, err)
		}
	}
}

// End to end through the dispatcher with cmd.exe: the marked start runs in the
// fallback folder; an unmarked one is refused before CreateProcess.
func TestDispatchSessionStart_WindowsFallbackRunsInTheOwnFolder(t *testing.T) {
	published := captureReadOnlyPublishes(t)
	withGlobalSessionManager(t, NewSessionManager(nil))
	captureSessionFrames(t)
	own := t.TempDir()
	gone := own + `-renamed-away`
	cmd := commandMsg{
		ID: "cmd-win-fallback", Type: "session_start", SessionID: "sess-win-startdir",
		Command: "cmd", Args: []string{"/c", "cd"}, Cwd: gone, CwdFallback: cwdFallbackDeviceHome, TimeoutMs: 30000,
	}
	dispatchSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1", WorkingDirectory: own})

	deadline := time.Now().Add(20 * time.Second)
	var frames []resultMsg
	for time.Now().Before(deadline) {
		frames = published()
		done := false
		for _, f := range frames {
			if f.Type == "session_ended" {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var out strings.Builder
	var started *resultMsg
	for i := range frames {
		if frames[i].Type == "session_started" {
			started = &frames[i]
		}
		if frames[i].Type == "stream" {
			out.WriteString(frames[i].Output)
		}
	}
	if started == nil || started.Cwd != own || started.CwdFallbackFrom != gone {
		t.Fatalf("session_started = %+v; want cwd %q from %q", started, own, gone)
	}
	if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(own)) {
		t.Fatalf("cmd printed %q; want it to run in %q", out.String(), own)
	}

	refused := commandMsg{
		ID: "cmd-win-refused", Type: "session_start", SessionID: "sess-win-startdir-refused",
		Command: "cmd", Args: []string{"/c", "cd"}, Cwd: gone, TimeoutMs: 30000,
	}
	before := len(published())
	dispatchSessionCommand(context.Background(), nil, refused, &Config{AgentID: "agent-1", WorkingDirectory: own})
	after := published()[before:]
	if len(after) != 1 || after[0].ErrorCode != sessionStartDirMissingErrorCode ||
		strings.Contains(after[0].Output, "directory name is invalid") {
		t.Fatalf("unmarked start published %+v; want one TERMINAL_CWD_NOT_FOUND refusal", after)
	}
}
