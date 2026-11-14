package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

/* --------------------------------------------------------------------------
   Helpers
   -------------------------------------------------------------------------- */

// captureSessionFrames routes the generic session path's frames (the
// session_started ack, stream output, session_ended) into the same capture as
// publishMsg, so a test sees a whole session without a Pub/Sub topic.
func captureSessionFrames(t *testing.T) {
	t.Helper()
	prev := sessionPublishFnFor
	sessionPublishFnFor = func(_ *pubsub.Publisher, _ string) PublishFunc {
		return func(res resultMsg) { _ = publishMsg(context.Background(), nil, res) }
	}
	t.Cleanup(func() { sessionPublishFnFor = prev })
}

// withStartDirHome stands in the home folder the fallback falls through to.
func withStartDirHome(t *testing.T, home string) {
	t.Helper()
	prev := sessionStartHomeDir
	sessionStartHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { sessionStartHomeDir = prev })
}

// countStartDirStats counts the folder checks, so a test can prove a start
// was left alone without one.
func countStartDirStats(t *testing.T) *int {
	t.Helper()
	prev := sessionStartStat
	calls := 0
	sessionStartStat = func(p string) (fs.FileInfo, error) {
		calls++
		return prev(p)
	}
	t.Cleanup(func() { sessionStartStat = prev })
	return &calls
}

func missingDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "renamed-away")
}

func utilityStart(cwd, fallback string) commandMsg {
	return commandMsg{
		ID:          "cmd-1",
		Type:        "session_start",
		SessionID:   "sess-startdir",
		Command:     "bash",
		Args:        []string{"-c", "true"},
		Cwd:         cwd,
		CwdFallback: fallback,
	}
}

/* --------------------------------------------------------------------------
   resolveSessionStartDir
   -------------------------------------------------------------------------- */

func TestResolveSessionStartDir_AnExistingFolderIsUsedVerbatim(t *testing.T) {
	dir := t.TempDir()
	got, from, reason, err := resolveSessionStartDir(&Config{WorkingDirectory: t.TempDir()}, utilityStart(dir, cwdFallbackDeviceHome))
	if err != nil || got != dir || from != "" || reason != "" {
		t.Fatalf("resolve(existing) = %q, %q, %q, %v; want the folder verbatim and no fallback", got, from, reason, err)
	}
}

func TestResolveSessionStartDir_AMarkedDefaultThatIsGoneStartsInThisComputersFolder(t *testing.T) {
	own := t.TempDir()
	withStartDirHome(t, t.TempDir())
	gone := missingDir(t)
	for _, command := range []string{"bash", "sh", "powershell", "pwsh", "git", "gh", "/bin/bash", "cmd"} {
		cmd := utilityStart(gone, cwdFallbackDeviceHome)
		cmd.Command = command
		got, from, reason, err := resolveSessionStartDir(&Config{WorkingDirectory: own}, cmd)
		if err != nil || got != own || from != gone || reason != startDirNotFound {
			t.Fatalf("resolve(%s, gone + marker) = %q, %q, %q, %v; want %q from %q (not_found)", command, got, from, reason, err, own, gone)
		}
	}
}

func TestResolveSessionStartDir_FallsThroughToTheHomeFolder(t *testing.T) {
	home := t.TempDir()
	withStartDirHome(t, home)
	gone := missingDir(t)

	// This computer's own folder is gone too.
	got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: missingDir(t)}, utilityStart(gone, cwdFallbackDeviceHome))
	if err != nil || got != home || from != gone {
		t.Fatalf("resolve with the own folder gone = %q, %q, %v; want the home folder", got, from, err)
	}
	// The missing folder IS this computer's own folder: never "fall back" to it.
	got, from, _, err = resolveSessionStartDir(&Config{WorkingDirectory: gone}, utilityStart(gone, cwdFallbackDeviceHome))
	if err != nil || got != home || from != gone {
		t.Fatalf("resolve with the own folder missing = %q, %q, %v; want the home folder", got, from, err)
	}
	// No config at all.
	got, _, _, err = resolveSessionStartDir(nil, utilityStart(gone, cwdFallbackDeviceHome))
	if err != nil || got != home {
		t.Fatalf("resolve without a config = %q, %v; want the home folder", got, err)
	}
}

func TestResolveSessionStartDir_RefusesWhenNothingCanStandIn(t *testing.T) {
	withStartDirHome(t, missingDir(t))
	gone := missingDir(t)
	_, _, _, err := resolveSessionStartDir(&Config{WorkingDirectory: missingDir(t)}, utilityStart(gone, cwdFallbackDeviceHome))
	typed := sessionStartDirErrorFrom(err)
	if typed == nil || typed.dir != gone || !strings.Contains(err.Error(), "does not exist on this computer") {
		t.Fatalf("resolve with nothing to stand in = %v; want a refusal naming %q", err, gone)
	}
}

// Without the marker the folder came from a repository mapping, a worktree or
// the caller: moving the start would run repository work somewhere else, so it
// is refused, with a message that names the folder instead of the shell.
func TestResolveSessionStartDir_RefusesAMissingFolderTheServerDidNotMark(t *testing.T) {
	withStartDirHome(t, t.TempDir())
	gone := missingDir(t)
	for _, marker := range []string{"", "home", "DEVICE_HOME", "device_home "} {
		_, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: t.TempDir()}, utilityStart(gone, marker))
		if sessionStartDirErrorFrom(err) == nil || from != "" {
			t.Fatalf("resolve(gone, marker %q) = %v (from %q); want a refusal and no fallback", marker, err, from)
		}
		msg := err.Error()
		if !strings.Contains(msg, gone) || strings.Contains(msg, "fork/exec") || strings.Contains(msg, "directory name is invalid") {
			t.Fatalf("refusal %q must name the folder and nothing about the spawn", msg)
		}
	}
}

// A coding agent's folder decides which files it edits: never moved, never
// checked here (its own start path refuses a missing folder as before).
func TestResolveSessionStartDir_NeverTouchesACodingAgentStart(t *testing.T) {
	stats := countStartDirStats(t)
	withStartDirHome(t, t.TempDir())
	gone := missingDir(t)
	for _, command := range []string{"claude", "codex", "grok", "opencode", "agy", "antigravity", "muse", "/usr/local/bin/claude", "claude.exe"} {
		cmd := utilityStart(gone, cwdFallbackDeviceHome)
		cmd.Command = command
		got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: t.TempDir()}, cmd)
		if err != nil || got != gone || from != "" {
			t.Fatalf("resolve(%s) = %q, %q, %v; want the start untouched", command, got, from, err)
		}
	}
	if *stats != 0 {
		t.Fatalf("checked the folder %d times for coding agents; want none", *stats)
	}
}

func TestResolveSessionStartDir_LeavesReadOnlySignedEmptyAndRelativeStartsAlone(t *testing.T) {
	stats := countStartDirStats(t)
	gone := missingDir(t)
	readOnly := utilityStart(gone, cwdFallbackDeviceHome)
	readOnly.ReadOnly = true
	signed := utilityStart(gone, cwdFallbackDeviceHome)
	signed.RiskLevel = "safe"
	for _, cmd := range []commandMsg{readOnly, signed, utilityStart("", cwdFallbackDeviceHome), utilityStart("relative/dir", cwdFallbackDeviceHome)} {
		got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: t.TempDir()}, cmd)
		if err != nil || got != cmd.Cwd || from != "" {
			t.Fatalf("resolve(%+v) = %q, %q, %v; want the start untouched", cmd, got, from, err)
		}
	}
	if *stats != 0 {
		t.Fatalf("checked the folder %d times; want none", *stats)
	}
}

func TestResolveSessionStartDir_AFileIsNotAFolder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := t.TempDir()
	got, from, reason, err := resolveSessionStartDir(&Config{WorkingDirectory: own}, utilityStart(file, cwdFallbackDeviceHome))
	if err != nil || got != own || from != file || reason != startDirNotADirectory {
		t.Fatalf("resolve(file + marker) = %q, %q, %q, %v; want the own folder, not_a_directory", got, from, reason, err)
	}
	_, _, _, err = resolveSessionStartDir(&Config{WorkingDirectory: own}, utilityStart(file, ""))
	if sessionStartDirErrorFrom(err) == nil || !strings.Contains(err.Error(), "is not a folder") {
		t.Fatalf("resolve(file, no marker) = %v; want a refusal saying it is not a folder", err)
	}
}

// An error the check cannot read (access denied, say) changes nothing: the
// spawn answers it exactly as it did before.
func TestResolveSessionStartDir_AnUnreadableFolderIsLeftToTheSpawn(t *testing.T) {
	prev := sessionStartStat
	sessionStartStat = func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	t.Cleanup(func() { sessionStartStat = prev })
	dir := filepath.Join(string(filepath.Separator)+"locked", "folder")
	if runtime.GOOS == "windows" {
		dir = `C:\locked\folder`
	}
	got, from, _, err := resolveSessionStartDir(&Config{WorkingDirectory: `C:\own`}, utilityStart(dir, cwdFallbackDeviceHome))
	if err != nil || got != dir || from != "" {
		t.Fatalf("resolve(unreadable) = %q, %q, %v; want the start untouched", got, from, err)
	}
}

func TestResolveSessionStartDir_AFileInThePathMeansTheFolderCannotExist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ENOTDIR is the POSIX answer; Windows answers ERROR_PATH_NOT_FOUND, which is not_found")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := t.TempDir()
	got, from, reason, err := resolveSessionStartDir(&Config{WorkingDirectory: own}, utilityStart(filepath.Join(file, "sub"), cwdFallbackDeviceHome))
	if err != nil || got != own || from == "" || reason != startDirUnavailableWhy {
		t.Fatalf("resolve(file/sub) = %q, %q, %q, %v; want a fallback for an unavailable folder", got, from, reason, err)
	}
}

// The wire values terminal-service sends and reads: renaming one is a protocol
// change, so it fails here first.
func TestSessionStartDir_WireValues(t *testing.T) {
	if cwdFallbackDeviceHome != "device_home" {
		t.Fatalf("cwdFallbackDeviceHome = %q", cwdFallbackDeviceHome)
	}
	if sessionStartDirMissingErrorCode != "TERMINAL_CWD_NOT_FOUND" {
		t.Fatalf("sessionStartDirMissingErrorCode = %q", sessionStartDirMissingErrorCode)
	}
	if startDirNotFound != "not_found" || startDirNotADirectory != "not_a_directory" || startDirUnavailableWhy != "unavailable" {
		t.Fatal("cwdFallbackReason values changed")
	}
}

/* --------------------------------------------------------------------------
   dispatchSessionCommand
   -------------------------------------------------------------------------- */

func TestDispatchSessionStart_RefusesAMissingFolderWithATypedError(t *testing.T) {
	published := captureReadOnlyPublishes(t)
	withGlobalSessionManager(t, NewSessionManager(nil))
	captureSessionFrames(t)
	gone := missingDir(t)
	cmd := utilityStart(gone, "")
	cmd.SessionID = "sess-startdir-refused"
	dispatchSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1", WorkingDirectory: t.TempDir()})

	frames := published()
	if len(frames) != 1 || frames[0].ErrorCode != sessionStartDirMissingErrorCode {
		t.Fatalf("published %+v; want one TERMINAL_CWD_NOT_FOUND refusal", frames)
	}
	if !strings.Contains(frames[0].Output, gone) {
		t.Fatalf("refusal %+v does not name the folder %q", frames[0], gone)
	}
	if globalSessionManager.GetSession(cmd.SessionID) != nil {
		t.Fatal("a refused start registered a session")
	}
}

// End to end on a POSIX shell: the marked start really runs in the folder the
// session_started frame names.
func TestDispatchSessionStart_AMarkedDefaultThatIsGoneRunsInTheFallbackFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh; the Windows resolution is covered by session_start_dir_windows_test.go")
	}
	published := captureReadOnlyPublishes(t)
	withGlobalSessionManager(t, NewSessionManager(nil))
	captureSessionFrames(t)
	own := t.TempDir()
	gone := missingDir(t)
	cmd := commandMsg{
		ID:          "cmd-fallback",
		Type:        "session_start",
		SessionID:   "sess-startdir-fallback",
		Command:     "sh",
		Args:        []string{"-c", "pwd -P"},
		Cwd:         gone,
		CwdFallback: cwdFallbackDeviceHome,
		TimeoutMs:   30000,
	}
	dispatchSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1", WorkingDirectory: own})

	deadline := time.Now().Add(15 * time.Second)
	var frames []resultMsg
	for time.Now().Before(deadline) {
		frames = published()
		ended := false
		for _, f := range frames {
			if f.Type == "session_ended" {
				ended = true
			}
		}
		if ended {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	var started *resultMsg
	var out strings.Builder
	for i := range frames {
		switch frames[i].Type {
		case "session_started":
			started = &frames[i]
		case "stream":
			out.WriteString(frames[i].Output)
		}
	}
	if started == nil {
		t.Fatalf("no session_started frame in %+v", frames)
	}
	if started.Cwd != own || started.CwdFallbackFrom != gone || started.CwdFallbackReason != startDirNotFound {
		t.Fatalf("session_started = cwd %q from %q (%q); want %q from %q (not_found)", started.Cwd, started.CwdFallbackFrom, started.CwdFallbackReason, own, gone)
	}
	realOwn, _ := filepath.EvalSymlinks(own)
	if !strings.Contains(out.String(), realOwn) {
		t.Fatalf("the session printed %q; want it to run in %q", out.String(), realOwn)
	}
}

func TestDispatchSessionStart_AnExistingFolderAddsNothingToTheAck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	published := captureReadOnlyPublishes(t)
	withGlobalSessionManager(t, NewSessionManager(nil))
	captureSessionFrames(t)
	dir := t.TempDir()
	cmd := commandMsg{
		ID: "cmd-plain", Type: "session_start", SessionID: "sess-startdir-plain",
		Command: "sh", Args: []string{"-c", "true"}, Cwd: dir, CwdFallback: cwdFallbackDeviceHome, TimeoutMs: 30000,
	}
	dispatchSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-1", WorkingDirectory: t.TempDir()})
	for _, f := range published() {
		if f.Type == "session_started" && (f.Cwd != "" || f.CwdFallbackFrom != "" || f.CwdFallbackReason != "") {
			t.Fatalf("session_started carried fallback fields for an existing folder: %+v", f)
		}
	}
}
