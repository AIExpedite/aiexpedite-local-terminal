// File: env_sign_in.go
// -----------------------------------------------------------------------------
// __env_sign_in__ — open a VISIBLE terminal window on this computer running a
// tool's sign-in (`gh auth login --web`, `codex login`, …) and return at once
// with { launched: true }. The server then polls the tool's own verify command
// (an ordinary read-only step) until the person at the computer finishes
// (COMPUTER_SETUP_CHECKLIST_PLAN.md §7 step 2).
//
// The program is resolved on the refreshed PATH (a CLI installed by the
// previous batch must be found), then the installer's default bin dir for the
// CLIs whose installers write outside PATH. The window gets the agent's
// environment WITHOUT the headless overlay: this is the one interactive thing
// the agent starts, and it must be able to prompt.
//
// The argv is launched without a shell interpreting it:
//   - Windows: a new console (CREATE_NEW_CONSOLE) running
//     `cmd.exe /d /c title <title> & "<program>" <args> & echo. & pause`, built
//     as a raw command line from validated tokens (no quotes or cmd
//     metacharacters are accepted in the args), so the window stays until the
//     user has read the result;
//   - macOS: Terminal.app via osascript `do script`, every word single-quoted
//     for the login shell;
//   - Linux: x-terminal-emulator, gnome-terminal, then xterm, each given the
//     argv as separate arguments.
// -----------------------------------------------------------------------------

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxSignInArgs     = 16
	maxSignInArgBytes = 256
	maxSignInTitle    = 80
)

type envSignInRequest struct {
	Argv  []string `json:"argv"`
	Title string   `json:"title"`
}

type envSignInResult struct {
	Launched bool `json:"launched"`
}

// signInArgPattern: sign-in argv are fixed catalog tokens — flags, subcommands,
// hostnames, scopes. No quotes, whitespace or shell / cmd metacharacters.
var signInArgPattern = regexp.MustCompile(`^[A-Za-z0-9_@+=:,./\\-]+$`)

// signInTitleUnsafe strips anything outside a conservative set from the title.
var signInTitleUnsafe = regexp.MustCompile(`[^A-Za-z0-9 .,:()'_-]`)

// parseEnvSignInRequest decodes and validates args[0].
func parseEnvSignInRequest(args []string) (envSignInRequest, error) {
	var req envSignInRequest
	if err := decodeEnvSetupArgs(args, &req); err != nil {
		return req, err
	}
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return req, errors.New("argv is required")
	}
	if len(req.Argv) > maxSignInArgs {
		return req, fmt.Errorf("argv has more than %d entries", maxSignInArgs)
	}
	for i, a := range req.Argv {
		if len(a) > maxSignInArgBytes || !signInArgPattern.MatchString(a) {
			return req, fmt.Errorf("argv[%d] contains characters a sign-in command never needs", i)
		}
	}
	req.Title = sanitizeSignInTitle(req.Title, req.Argv[0])
	return req, nil
}

func sanitizeSignInTitle(title, program string) string {
	t := strings.TrimSpace(signInTitleUnsafe.ReplaceAllString(title, ""))
	if t == "" {
		t = "Sign in to " + commandBaseName(program)
		t = strings.TrimSpace(signInTitleUnsafe.ReplaceAllString(t, ""))
	}
	if len(t) > maxSignInTitle {
		t = t[:maxSignInTitle]
	}
	return t
}

// resolveSignInProgram finds argv[0] on the refreshed PATH (or, for the CLIs
// whose installers write outside PATH, in the installer's bin dir). An absolute
// argv[0] must exist.
func resolveSignInProgram(program string) (string, error) {
	refreshCommandPath()
	if filepath.IsAbs(program) {
		if info, err := os.Stat(program); err == nil && !info.IsDir() {
			return program, nil
		}
		return "", fmt.Errorf("%s not found", program)
	}
	if strings.ContainsAny(program, `/\`) {
		return "", fmt.Errorf("program must be a command name or an absolute path: %s", program)
	}
	if p, err := exec.LookPath(program); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs, nil
		}
		return p, nil
	}
	if p := resolveInstallerBinary(program, installerBinDirFor(program)); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("%s is not installed (not found on PATH)", program)
}

// signInLauncher opens the window. Seam for tests; production is the platform
// launcher in env_sign_in_<os>.go.
var signInLauncher = launchSignInWindow

// launchSignIn resolves the program and opens the sign-in window. It does not
// wait for the sign-in to finish.
func launchSignIn(req envSignInRequest) (envSignInResult, error) {
	program, err := resolveSignInProgram(req.Argv[0])
	if err != nil {
		return envSignInResult{}, err
	}
	if err := signInLauncher(program, req.Argv[1:], req.Title); err != nil {
		return envSignInResult{}, err
	}
	return envSignInResult{Launched: true}, nil
}

// windowsSignInCommandLine is the cmd.exe command line for the Windows window.
// Every piece is either a validated token or quoted program path, so cmd has
// nothing to reinterpret.
func windowsSignInCommandLine(comspec, program string, args []string, title string) string {
	var b strings.Builder
	b.WriteString(`"` + comspec + `" /d /c title ` + title + ` & "` + program + `"`)
	for _, a := range args {
		b.WriteString(" " + a)
	}
	b.WriteString(` & echo. & pause`)
	return b.String()
}

// posixShellQuote single-quotes s for sh / zsh.
func posixShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// macSignInScript is the AppleScript that opens Terminal.app on the sign-in.
func macSignInScript(program string, args []string, title string) string {
	words := []string{posixShellQuote(program)}
	for _, a := range args {
		words = append(words, posixShellQuote(a))
	}
	shell := "clear; echo " + posixShellQuote(title) + "; echo; " + strings.Join(words, " ")
	escaped := strings.ReplaceAll(strings.ReplaceAll(shell, `\`, `\\`), `"`, `\"`)
	return `tell application "Terminal"` + "\n" +
		`  activate` + "\n" +
		`  do script "` + escaped + `"` + "\n" +
		`end tell`
}

// linuxSignInCandidates lists the terminal emulators tried, in order, with the
// argv each needs to run program args.
func linuxSignInCandidates(program string, args []string, title string) [][]string {
	run := append([]string{program}, args...)
	return [][]string{
		append([]string{"x-terminal-emulator", "-T", title, "-e"}, run...),
		append([]string{"gnome-terminal", "--title=" + title, "--"}, run...),
		append([]string{"xterm", "-T", title, "-e"}, run...),
	}
}
