// File: powershell_launch.go
// How the agent launches commands inside PowerShell on Windows so a restrictive
// execution policy (the Windows default `Restricted`, or `AllSigned`) cannot
// stop them, and so a program that never started is reported as a failure.
//
// Three pieces, all process-scoped — nothing here touches the CurrentUser or
// LocalMachine execution policy:
//   - psHostArgs: every PowerShell host the agent spawns runs with
//     `-ExecutionPolicy Bypass`, which applies to that process only.
//   - preferBatchShim: a bare command whose PATH hit is a .cmd/.bat shim
//     (`npm` -> npm.cmd) is launched by that name, so PowerShell never loads the
//     sibling .ps1 (npm.ps1). This also covers a Group-Policy-pinned policy,
//     where Windows ignores the -ExecutionPolicy flag.
//   - psExitCaptureScript: a final statement that failed because PowerShell
//     could not start the program (not found, or script blocked) reports exit
//     code 1 instead of the $LASTEXITCODE = 0 left over from the reset.
//
// No build tag and no runtime.GOOS guard: the only caller of preferBatchShim is
// runLocalCommandWindows, which the Unix path never reaches, and keeping this
// file portable lets the lookPathFn-driven tests run on every CI leg.
// See local/documents/features/terminal/ENVIRONMENT_SETUP.md →
// "Windows execution policy (npm.ps1)".

package main

import (
	"os/exec"
	"strings"
)

// psHostPolicyArgs is passed to every powershell.exe / pwsh.exe host the agent
// starts to run commands. `-ExecutionPolicy Bypass` sets the policy for that
// process only (nothing is written to the registry). Without it, a .ps1 the
// command calls — npm.ps1, or `-File` for the temp-file transport — fails under
// the default `Restricted` policy. `-EncodedCommand` exempts only the encoded
// script itself, not the .ps1 files that script calls.
var psHostPolicyArgs = []string{"-ExecutionPolicy", "Bypass"}

// psHostArgs is the argv for a powershell.exe / pwsh.exe host the agent starts
// to run commands: the shared flags, then the transport (`-Command <line>`,
// `-EncodedCommand <b64>`, `-File <path>`, `-Command -`), which must come last.
// `-OutputFormat Text` stops PowerShell from serializing stderr as CLIXML (XML
// error records) when stderr is piped to a non-console parent; without it any
// PowerShell error surfaces as `#< CLIXML <Objs ...>` noise that leaks past
// filterCLIXML back to the user.
func psHostArgs(transport ...string) []string {
	args := make([]string, 0, 6+len(transport))
	args = append(args, "-NoProfile", "-NonInteractive")
	args = append(args, psHostPolicyArgs...)
	args = append(args, "-OutputFormat", "Text")
	return append(args, transport...)
}

// lookPathFn resolves a bare command name the way Go's exec does (PATHEXT on
// Windows, which does not include .PS1). Test seam.
var lookPathFn = exec.LookPath

// preferBatchShim returns the name to launch for cmd. When cmd is a bare name
// (no directory, no extension) whose PATH hit is a .cmd or .bat shim, and every
// argument survives cmd.exe re-parsing, it returns the shim's bare name
// (`npm` -> `npm.cmd`) so PowerShell resolves that same file and never loads
// npm.ps1. Anything else — a path, an explicit extension, an .exe hit (winget
// grok.exe), a failed lookup (Node missing) — is returned unchanged, so the
// real PowerShell error still surfaces.
func preferBatchShim(cmd string, args []string) string {
	if cmd == "" || strings.ContainsAny(cmd, `/\:`) || strings.Contains(cmd, ".") {
		return cmd
	}
	for _, arg := range args {
		if !isCmdSafeArg(arg) {
			return cmd
		}
	}
	resolved, err := lookPathFn(cmd)
	if err != nil {
		return cmd
	}
	// Take the extension from the base name by hand: filepath.Ext on Linux/macOS
	// does not treat `\` as a separator, and the seam-driven tests feed it
	// Windows paths.
	base := resolved[strings.LastIndexAny(resolved, `/\`)+1:]
	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return cmd
	}
	ext := base[dot:]
	if !strings.EqualFold(ext, ".cmd") && !strings.EqualFold(ext, ".bat") {
		return cmd
	}
	return cmd + ext
}

// isCmdSafeArg reports whether s can be handed to a .cmd/.bat shim unchanged.
// A shim runs through cmd.exe, which re-parses its arguments: `%` and `!`
// expand variables, `^` escapes, `&` `|` `<` `>` chain or redirect, `"` toggles
// quoting and CR/LF end the line. `@` is safe, so `@xai-org/grok` qualifies.
func isCmdSafeArg(s string) bool {
	return !strings.ContainsAny(s, "%^&|<>!\"\r\n")
}

// psResetScript runs before the user command in both the persistent host and
// the one-shot fallback. $LASTEXITCODE is only updated when a native program
// runs, and a fresh pwsh.exe can start with a non-zero value, so it is reset.
// $Error is cleared so $Error[0] in psExitCaptureScript cannot be a leftover
// from an earlier command in the long-lived host. $__aix_ok starts true so a
// command that leaves early (`return`) is never promoted.
const psResetScript = "$LASTEXITCODE = 0; $Error.Clear(); $global:__aix_ok = $true\n"

// psStatusCaptureStatement records whether the user command's final statement
// succeeded. It MUST be the very next statement after the user command — any
// statement in between overwrites $?. In the persistent host it goes INSIDE the
// `& { ...; <here> }` block: $? after `& { }` itself is always true.
const psStatusCaptureStatement = "$global:__aix_ok = $?"

// psExitCaptureScript turns the user command's outcome into $__aix_exit. It
// promotes a missing native exit code to 1 only when the final statement failed
// because PowerShell never started the program: the exact error ids
// CommandNotFoundException (not found) and UnauthorizedAccess (script blocked
// by execution policy). Handled errors (try/catch) leave $__aix_ok true; a
// cmdlet's own error carries a suffix (`CommandNotFoundException,Microsoft.
// PowerShell.Commands.GetCommandCommand` for a `Get-Command -ErrorAction
// SilentlyContinue` probe) and does not match; a real non-zero native code is
// never overridden. $__aix_promoted lets a caller echo the error when the host's
// stderr is not part of the output.
//
// One line on purpose: the persistent host reads `-Command -` from stdin, where
// a statement split across lines (`if (...) {` newline ...) is not run until a
// blank line follows — the host would hang waiting for it.
const psExitCaptureScript = "$__aix_exit = $LASTEXITCODE; $__aix_promoted = $false; " +
	"if (-not $__aix_ok -and ($null -eq $__aix_exit -or $__aix_exit -eq 0) -and $Error.Count -gt 0 -and " +
	"@('CommandNotFoundException', 'UnauthorizedAccess') -contains $Error[0].FullyQualifiedErrorId) " +
	"{ $__aix_exit = 1; $__aix_promoted = $true }"
