// File: gh_auth_probe.go
// -----------------------------------------------------------------------------
// Whether the GitHub CLI is signed in to github.com, reported on every
// inspection as specs.toolAuth.gh.
//
// Why. Work readiness (terminal-service workReadiness.service.js) requires
// "GitHub CLI present AND signed in", and the FRESH inspection decides
// whenever it says anything. Until this probe the agent reported only
// `gh --version`, so GitHub's sign-in was always unknown and the server fell
// back to the one sign-in a setup session had verified. A computer judged by
// automatic onboarding, or one whose session never ran the GitHub sign-in,
// read "GitHub CLI isn't signed in" on every later inspection with gh signed
// in, and stayed Setup required: routing never resumed on its own (AIE2,
// 2026-11-04).
//
// What is reported. Three answers, and only two of them are sent:
//   - `gh auth status --active --hostname github.com` exits 0 -> authenticated;
//   - it exits non-zero saying no account is logged in to the host ("You are
//     not logged into any GitHub hosts", "You are not logged into any
//     accounts on github.com": gh v2.71.2 pkg/cmd/auth/status) ->
//     unauthenticated;
//   - anything else (gh not on PATH, timeout, a failed token check, an output
//     this build does not recognise) -> nothing. `gh auth status` validates
//     the token against the API and prints "The token in <source> is invalid"
//     for EVERY failure that is not a timeout: a DNS failure or a proxy error
//     reads exactly like a revoked token. Reporting that as signed out would
//     take a working computer out of rotation whenever GitHub is unreachable.
//     Silence leaves the server on its stored evidence, exactly as before
//     this probe.
//
// Cost and privacy. Inspection only (GatherReadinessOnly), never the
// periodic gather: one authenticated API call per inspection. The output is
// classified in memory and dropped; neither the account name nor the masked
// token leaves the computer. `gh auth status` is the same read-only check the
// setup session's GitHub sign-in step verifies with.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// toolAuthInfo is a non-agent tool's sign-in state, in the field names the
// server's auth reader already understands for coding agents
// (shared-constants cliAgentAuthState: `authenticated` + `authState`).
type toolAuthInfo struct {
	Authenticated *bool  `json:"authenticated,omitempty"`
	AuthState     string `json:"authState,omitempty"`
}

// toolAuthGitHubCLI is the specs.toolAuth key for the GitHub CLI: the setup
// catalog's tool id (setupTools/gh), which the server reads it under.
const toolAuthGitHubCLI = "gh"

// ghAuthProbeTimeout bounds the status check. It makes one API call, so it
// gets more than the 5 s version probes; the inspection's own deadline still
// cuts it short.
const ghAuthProbeTimeout = 8 * time.Second

// ghAuthStatusArgs checks the ACTIVE account on github.com only. An Enterprise
// host says nothing about the repositories we push to, and without --active gh
// also validates every other stored account and exits 1 when any of them is
// stale, though the active one (what git and `gh pr create` use) works. A gh
// too old to know --active exits 1 with a usage error, which classifies as
// unknown: no worse than before this probe.
var ghAuthStatusArgs = []string{"auth", "status", "--active", "--hostname", "github.com"}

// ghAuthProbeEnv keeps the check non-interactive and quiet.
var ghAuthProbeEnv = []string{
	"GH_PROMPT_DISABLED=1",
	"GH_NO_UPDATE_NOTIFIER=1",
	"GH_SPINNER_DISABLED=1",
	"NO_COLOR=1",
}

// ghAuthStatusResult is one run of the status check. Completed is false when
// it never produced a verdict of its own (not found, failed to start, timed
// out); ExitedZero is meaningful only when Completed.
type ghAuthStatusResult struct {
	Output     string
	ExitedZero bool
	Completed  bool
}

// ghAuthStatusRunner runs the status check. Seam for tests.
type ghAuthStatusRunner func(ctx context.Context) ghAuthStatusResult

// ghAuthLookPath resolves gh. Seam for tests.
var ghAuthLookPath = exec.LookPath

// runGhAuthStatus is the production ghAuthStatusRunner.
func runGhAuthStatus(ctx context.Context) ghAuthStatusResult {
	path, err := ghAuthLookPath("gh")
	if err != nil {
		return ghAuthStatusResult{}
	}
	pctx, cancel := context.WithTimeout(ctx, ghAuthProbeTimeout)
	defer cancel()
	c := exec.CommandContext(pctx, path, ghAuthStatusArgs...)
	hideWindow(c)
	c.Env = append(os.Environ(), ghAuthProbeEnv...)
	c.WaitDelay = time.Second
	out, err := c.CombinedOutput()
	if pctx.Err() != nil {
		// Killed by the timeout (or the inspection ended): no verdict, whatever
		// the exit status says.
		return ghAuthStatusResult{}
	}
	if err == nil {
		return ghAuthStatusResult{Output: string(out), ExitedZero: true, Completed: true}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ghAuthStatusResult{Output: string(out), Completed: true}
	}
	return ghAuthStatusResult{}
}

// ghNotLoggedInMarkers are gh's own words for "no account on this host", in
// lower case. Both forms gh prints contain "not logged into any"; the second
// spelling is kept for a future wording.
var ghNotLoggedInMarkers = []string{"not logged into any", "not logged in to any"}

// classifyGhAuthStatus turns a status run into the reported state, or nil when
// the run proves nothing either way.
func classifyGhAuthStatus(r ghAuthStatusResult) *toolAuthInfo {
	if !r.Completed {
		return nil
	}
	if r.ExitedZero {
		yes := true
		return &toolAuthInfo{Authenticated: &yes, AuthState: "authenticated"}
	}
	lower := strings.ToLower(r.Output)
	for _, marker := range ghNotLoggedInMarkers {
		if strings.Contains(lower, marker) {
			no := false
			return &toolAuthInfo{Authenticated: &no, AuthState: "unauthenticated"}
		}
	}
	return nil
}

// probeGhAuth is the inspection's GitHub sign-in probe: nil when unknown.
func probeGhAuth(ctx context.Context, run ghAuthStatusRunner) *toolAuthInfo {
	if run == nil {
		return nil
	}
	return classifyGhAuthStatus(run(ctx))
}

// withToolAuth records one tool's sign-in state on info. A nil state leaves
// whatever is there (a gather never sets it, so that is nothing) untouched.
func withToolAuth(info *MachineInfo, tool string, state *toolAuthInfo) {
	if info == nil || state == nil {
		return
	}
	if info.ToolAuth == nil {
		info.ToolAuth = map[string]*toolAuthInfo{}
	}
	info.ToolAuth[tool] = state
}
