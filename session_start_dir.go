package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// A session start names its working folder in cmd.Cwd. When terminal-service
// chose that folder itself because nothing more specific applied (the
// computer's saved Default working directory, or the device's own working
// directory), and the folder has since been deleted, renamed or unmounted, the
// spawn failed with a message that named the shell rather than the folder:
// Windows CreateProcess answers "The directory name is invalid." and macOS
// "fork/exec /bin/bash: no such file or directory". Every start that inherits
// that folder failed the same way, repository discovery included, until a
// person noticed and saved another folder.
//
// The folder is checked once, here, before the spawn:
//   - It exists: started exactly as before, with the path verbatim.
//   - It is missing, the frame carries cwdFallback "device_home" (only a start
//     terminal-service gave the saved default or the device home to, for a
//     command that is not a coding agent, on the owner's own computer), and the
//     command is not a coding agent: started in this computer's own working
//     folder instead (Config.WorkingDirectory, else the home folder), and the
//     session_started frame says which folder it used and which was missing.
//   - Otherwise: refused with TERMINAL_CWD_NOT_FOUND and a message that names
//     the folder.
//
// A coding agent's start folder decides which files it edits, so a coding
// agent is never moved, and its start is left exactly as before (its own
// checks already refuse a missing folder). A repository mapping or a folder a
// caller named is never moved either: terminal-service marks only the starts
// whose folder was a default, and the device cannot tell those apart itself.
//
// The marker is UNSIGNED (it is not in signaturePayload), like `tty`: it names
// no path, and at most it turns a start that would fail into a start in this
// computer's own configured folder. A writer who could add it could already
// rewrite the unsigned cwd. Read-only and signed-cwd starts are never checked
// or moved.

// cwdFallbackDeviceHome is the only value of commandMsg.CwdFallback.
const cwdFallbackDeviceHome = "device_home"

// sessionStartDirMissingErrorCode is the typed refusal for a start whose
// working folder does not exist.
const sessionStartDirMissingErrorCode = "TERMINAL_CWD_NOT_FOUND"

// Why a start folder cannot be used. Sent as cwdFallbackReason.
const (
	startDirNotFound       = "not_found"
	startDirNotADirectory  = "not_a_directory"
	startDirUnavailableWhy = "unavailable" // a drive or share that is not there
)

type sessionStartDirError struct {
	dir    string
	reason string
}

// The folder is printed as it is, in plain quotes: %q would double every
// backslash of a Windows path in the message a person reads.
func (e *sessionStartDirError) Error() string {
	if e.reason == startDirNotADirectory {
		return fmt.Sprintf(`working folder "%s" is not a folder on this computer`, e.dir)
	}
	return fmt.Sprintf(`working folder "%s" does not exist on this computer`, e.dir)
}

func sessionStartDirErrorFrom(err error) *sessionStartDirError {
	var typed *sessionStartDirError
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}

// Seams, so a test can stand in a home folder and count the checks.
var (
	sessionStartHomeDir = os.UserHomeDir
	sessionStartStat    = os.Stat
)

// startDirProblem says why dir cannot be a start folder, or "" when it can, or
// when its state is unknown (access denied, say): then the spawn is left to
// answer exactly as it did before this check existed.
func startDirProblem(dir string) string {
	info, err := sessionStartStat(dir)
	if err == nil {
		if info.IsDir() {
			return ""
		}
		return startDirNotADirectory
	}
	if errors.Is(err, fs.ErrNotExist) {
		return startDirNotFound
	}
	if startDirUnavailableErr(err) {
		return startDirUnavailableWhy
	}
	return ""
}

func startDirExists(dir string) bool {
	info, err := sessionStartStat(dir)
	return err == nil && info.IsDir()
}

// resolveSessionStartDir returns the folder to start cmd in. fallbackFrom and
// reason are set only when it is not cmd.Cwd. A non-nil error is a
// *sessionStartDirError: refuse the start with it.
func resolveSessionStartDir(cfg *Config, cmd commandMsg) (dir, fallbackFrom, reason string, err error) {
	dir = cmd.Cwd
	if dir == "" || cmd.ReadOnly || cwdIsSigned(cmd) || isResidentAgentSessionCommand(cmd.Command) || !filepath.IsAbs(dir) {
		return dir, "", "", nil
	}
	problem := startDirProblem(dir)
	if problem == "" {
		return dir, "", "", nil
	}
	if cmd.CwdFallback == cwdFallbackDeviceHome {
		var candidates []string
		if cfg != nil {
			candidates = append(candidates, cfg.WorkingDirectory)
		}
		if home, homeErr := sessionStartHomeDir(); homeErr == nil {
			candidates = append(candidates, home)
		}
		for _, candidate := range candidates {
			if candidate == "" || !filepath.IsAbs(candidate) || pathIdentityKey(candidate) == pathIdentityKey(dir) {
				continue
			}
			if startDirExists(candidate) {
				return candidate, dir, problem, nil
			}
		}
	}
	return "", "", "", &sessionStartDirError{dir: dir, reason: problem}
}
