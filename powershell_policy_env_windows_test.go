//go:build windows

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestInheritedProcessBypassIsClearedForPolicyStep pins the root cause of the
// setup step "Allow PowerShell scripts for your user account" failing with
// exit code 1: every host the agent starts runs with `-ExecutionPolicy Bypass`
// (psHostArgs), PowerShell keeps that Process scope in
// PSExecutionPolicyPreference, and the step's own `powershell` child inherits
// it — so `Set-ExecutionPolicy -Scope CurrentUser` "succeeds but is overridden
// by a policy defined at a more specific scope" and throws. The step script
// (terminal-service executionPolicyScript) clears the variable first; this
// checks that clearing it really leaves the child, and the host itself, with
// no Process-scope policy. Nothing here writes a saved policy.
func TestInheritedProcessBypassIsClearedForPolicyStep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping powershell.exe integration test in -short mode")
	}
	clearInheritedPolicyPreference(t)

	// The child reads the variable rather than `Get-ExecutionPolicy -Scope
	// Process`: Windows PowerShell 5.1 cannot autoload the Security module when
	// it inherits a pwsh 7 PSModulePath (as on the CI runner). An empty value
	// is the Undefined Process scope.
	child := `& powershell -NoProfile -NonInteractive -Command '[string]` + processPolicyProbe + `'`
	script := strings.Join([]string{
		"$before = " + child,
		"Remove-Item Env:PSExecutionPolicyPreference -ErrorAction SilentlyContinue",
		"$after = " + child,
		"$self = [string]" + processPolicyProbe,
		`Write-Output ("before=" + $before + "|after=" + $after + "|self=" + $self)`,
	}, "; ")

	out, err := exec.Command("powershell", psHostArgs("-Command", script)...).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell failed: %v\noutput: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if i := strings.LastIndex(got, "\n"); i >= 0 {
		got = strings.TrimSpace(got[i+1:])
	}
	if want := "before=Bypass|after=|self="; got != want {
		t.Fatalf("inherited Process scope: got %q, want %q", got, want)
	}
}
