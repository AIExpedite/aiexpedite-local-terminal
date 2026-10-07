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
// Approval: a sign-in whose argv EXACTLY matches one of the catalog's own
// sign-in commands (builtinSignInArgvs) runs without the native dialog — the
// owner consented to it on the setup card, and all it does is open a window
// in which the person at the computer signs in. Anything else still takes the
// external_write dialog every time.
//
// The argv is launched without a shell interpreting it:
//   - Windows: a new console (CREATE_NEW_CONSOLE, started by startInNewConsole
//     so its stdin/stdout/stderr ARE that console) running
//     `cmd.exe /d /c title <title> & echo <title> & echo. & "<program>" <args>
//     & echo. & pause`, built as a raw command line from validated tokens (no
//     quotes or cmd metacharacters are accepted in the args), so the command's
//     prompts and output are visible and the window stays until the user has
//     read the result;
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

// signInNotInstalledSuffix ends the error resolveSignInProgram returns for a
// CLI that is on neither the refreshed PATH nor its installer's bin dir.
// terminal-service matches this text (isSignInProgramMissing) to fail the
// sign-in instead of waiting on a "run <cli> login" the user can't run: keep
// the wording stable.
const signInNotInstalledSuffix = "is not installed (not found on PATH)"

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
	return "", fmt.Errorf("%s %s", program, signInNotInstalledSuffix)
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

// builtinSignInArgvs are the catalog's own sign-in commands (db-content
// dev/cliAgents/*.json and dev/setupTools/gh.json `signIn.argv`, 2026-09-26).
// A sign-in whose argv matches one EXACTLY — same program name, same
// arguments, same order and case, nothing extra — runs without the native
// approval dialog. A new or changed sign-in command needs an agent release
// before it skips the dialog; until then it is asked about each time.
var builtinSignInArgvs = [][]string{
	{"gh", "auth", "login", "--web", "--git-protocol", "https"}, // setupTools/gh
	{"claude", "auth", "login"},                                 // cliAgents/claudeCode
	{"codex", "login"},                                          // cliAgents/codex
	{"grok", "login", "--oauth"},                                // cliAgents/grok
	{"opencode", "auth", "login"},                               // cliAgents/opencode
	{"agy"},                                                     // cliAgents/antigravity
}

// isBuiltinSignInArgv reports whether argv is exactly one of builtinSignInArgvs.
func isBuiltinSignInArgv(argv []string) bool {
	for _, known := range builtinSignInArgvs {
		if len(known) != len(argv) {
			continue
		}
		match := true
		for i := range known {
			if known[i] != argv[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// windowsSignInCommandLine is the cmd.exe command line for the Windows window.
// Every piece is either a validated token or quoted program path, so cmd has
// nothing to reinterpret. The title is echoed first so the window never looks
// empty while the command starts.
func windowsSignInCommandLine(comspec, program string, args []string, title string) string {
	var b strings.Builder
	b.WriteString(`"` + comspec + `" /d /c title ` + title + ` & echo ` + title + ` & echo. & "` + program + `"`)
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
