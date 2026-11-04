package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Mock `gh` modes (session_integration_test.go runMockCLI dispatches here).
const (
	ghAuthMockSignedIn     = "gh-auth-signed-in"
	ghAuthMockSignedOut    = "gh-auth-signed-out"
	ghAuthMockInvalidToken = "gh-auth-invalid-token"
	ghAuthMockHang         = "gh-auth-hang"
)

// What gh v2.71.2 printed for `gh auth status --hostname github.com`
// (captured 2026-11-04 on Windows, NO_COLOR=1; the account renamed). The two
// "not logged into any" messages go to stderr and exit 1; the invalid-token
// block goes to stderr and exits 1 — and is what gh prints for ANY failed
// token check that is not a timeout (pkg/cmd/auth/status buildEntry), a DNS
// failure included.
const (
	ghStatusSignedIn = "github.com\n" +
		"  ✓ Logged in to github.com account octocat (keyring)\n" +
		"  - Active account: true\n" +
		"  - Git operations protocol: https\n" +
		"  - Token: gho_************************************\n" +
		"  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'\n"
	ghStatusNoHosts      = "You are not logged into any GitHub hosts. To log in, run: gh auth login\n"
	ghStatusNotOnHost    = "You are not logged into any accounts on github.com\n"
	ghStatusInvalidToken = "github.com\n" +
		"  X Failed to log in to github.com using token (GH_TOKEN)\n" +
		"  - Active account: true\n" +
		"  - The token in GH_TOKEN is invalid.\n"
	ghStatusTimeout = "github.com\n" +
		"  X Timeout trying to log in to github.com account octocat (keyring)\n" +
		"  - Active account: true\n"
	ghUnknownFlag = "unknown flag: --active\n\nUsage:  gh auth status [flags]\n"
)

// ghAuthStatusArgv is the command this probe is specified to run, written out
// rather than read from ghAuthStatusArgs: the mock pins it, so dropping
// --active (every stored account checked, one stale account fails a working
// computer) or --hostname fails the cases below instead of passing silently.
const ghAuthStatusArgv = "auth status --active --hostname github.com"

// runGhAuthStatusMock is the mock `gh`. It answers only the exact argv and
// environment the probe sends: anything else exits 3, which classifies as
// unknown, so a drift in either fails the signed-in / signed-out cases below.
func runGhAuthStatusMock(mode string) {
	if got := strings.Join(os.Args[1:], " "); got != ghAuthStatusArgv {
		fmt.Fprintf(os.Stderr, "unexpected gh argv: %q\n", got)
		os.Exit(3)
	}
	if os.Getenv("GH_PROMPT_DISABLED") != "1" {
		fmt.Fprintln(os.Stderr, "gh would have been allowed to prompt")
		os.Exit(3)
	}
	switch mode {
	case ghAuthMockSignedIn:
		fmt.Print(ghStatusSignedIn)
		os.Exit(0)
	case ghAuthMockSignedOut:
		fmt.Fprint(os.Stderr, ghStatusNoHosts)
		os.Exit(1)
	case ghAuthMockInvalidToken:
		fmt.Fprint(os.Stderr, ghStatusInvalidToken)
		os.Exit(1)
	case ghAuthMockHang:
		select {}
	}
}

// withMockGh points the probe at this test binary running as `gh` in mode.
func withMockGh(t *testing.T, mode string) {
	t.Helper()
	saved := ghAuthLookPath
	ghAuthLookPath = func(string) (string, error) { return os.Args[0], nil }
	t.Cleanup(func() { ghAuthLookPath = saved })
	t.Setenv(mockCLIEnvVar, mode)
}

func authOf(state *toolAuthInfo) string {
	if state == nil {
		return "unknown"
	}
	if state.Authenticated == nil {
		return "authenticated=nil/" + state.AuthState
	}
	return fmt.Sprintf("authenticated=%v/%s", *state.Authenticated, state.AuthState)
}

func TestClassifyGhAuthStatus(t *testing.T) {
	cases := []struct {
		name string
		in   ghAuthStatusResult
		want string
	}{
		{"exit 0 is signed in", ghAuthStatusResult{Output: ghStatusSignedIn, ExitedZero: true, Completed: true}, "authenticated=true/authenticated"},
		{"no hosts at all is signed out", ghAuthStatusResult{Output: ghStatusNoHosts, Completed: true}, "authenticated=false/unauthenticated"},
		{"no account on github.com is signed out", ghAuthStatusResult{Output: ghStatusNotOnHost, Completed: true}, "authenticated=false/unauthenticated"},
		// The token check failed: a revoked token and an unreachable GitHub
		// print the same thing, so it proves nothing.
		{"a failed token check is unknown", ghAuthStatusResult{Output: ghStatusInvalidToken, Completed: true}, "unknown"},
		{"a timed-out token check is unknown", ghAuthStatusResult{Output: ghStatusTimeout, Completed: true}, "unknown"},
		{"a gh too old for --active is unknown", ghAuthStatusResult{Output: ghUnknownFlag, Completed: true}, "unknown"},
		{"a run that never finished is unknown", ghAuthStatusResult{Output: ghStatusNoHosts, Completed: false}, "unknown"},
		{"an empty failure is unknown", ghAuthStatusResult{Completed: true}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authOf(classifyGhAuthStatus(tc.in)); got != tc.want {
				t.Fatalf("classifyGhAuthStatus = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRunGhAuthStatus_AgainstAProcess(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{ghAuthMockSignedIn, "authenticated=true/authenticated"},
		{ghAuthMockSignedOut, "authenticated=false/unauthenticated"},
		{ghAuthMockInvalidToken, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			withMockGh(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if got := authOf(probeGhAuth(ctx, runGhAuthStatus)); got != tc.want {
				t.Fatalf("probe = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRunGhAuthStatus_HungGhIsUnknownAndBounded(t *testing.T) {
	withMockGh(t, ghAuthMockHang)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := runGhAuthStatus(ctx)
	if got.Completed {
		t.Fatalf("a gh killed by the deadline produced a verdict: %+v", got)
	}
	// The deadline plus WaitDelay, with room for a loaded runner.
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the probe outlived its deadline by far: %s", elapsed)
	}
}

func TestRunGhAuthStatus_NoGhOnPathIsUnknown(t *testing.T) {
	saved := ghAuthLookPath
	ghAuthLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { ghAuthLookPath = saved })
	if got := runGhAuthStatus(context.Background()); got.Completed {
		t.Fatalf("no gh on PATH must not produce a verdict: %+v", got)
	}
}

func TestWithToolAuth_NilStateLeavesInfoUntouched(t *testing.T) {
	info := &MachineInfo{}
	withToolAuth(info, toolAuthGitHubCLI, nil)
	if info.ToolAuth != nil {
		t.Fatalf("an unknown state must add nothing, got %+v", info.ToolAuth)
	}
	withToolAuth(nil, toolAuthGitHubCLI, &toolAuthInfo{AuthState: "authenticated"}) // must not panic
}

// inspectWithGhAuth runs one fast inspection whose GitHub check answers run.
func inspectWithGhAuth(t *testing.T, run ghAuthStatusRunner, within time.Duration) (ReadinessReport, map[string]any) {
	t.Helper()
	isolateMachineInfo(t)
	fresh := stampedMachine(healthyMachine(), time.Now())
	withInspectionSeams(t, func() *MachineInfo { return fresh }, noSetupToolPass)
	inspectionGhAuthStatus = run // restored by withInspectionSeams' cleanup
	report := inspectWithin(within)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	return report, wire
}

// wireToolAuth is specs.toolAuth exactly as the server receives it.
func wireToolAuth(wire map[string]any) (map[string]any, bool) {
	specs, _ := wire["specs"].(map[string]any)
	toolAuth, ok := specs["toolAuth"].(map[string]any)
	return toolAuth, ok
}

func TestGatherReadinessOnly_ReportsGitHubSignedIn(t *testing.T) {
	_, wire := inspectWithGhAuth(t, func(context.Context) ghAuthStatusResult {
		return ghAuthStatusResult{Output: ghStatusSignedIn, ExitedZero: true, Completed: true}
	}, 5*time.Second)
	toolAuth, ok := wireToolAuth(wire)
	if !ok {
		t.Fatalf("specs.toolAuth missing from the report: %v", wire["specs"])
	}
	gh, _ := toolAuth["gh"].(map[string]any)
	// The field names the server's auth reader already reads for coding
	// agents (shared-constants cliAgentAuthState).
	if gh["authenticated"] != true || gh["authState"] != "authenticated" {
		t.Fatalf("specs.toolAuth.gh = %v, want authenticated", gh)
	}
}

func TestGatherReadinessOnly_ReportsGitHubSignedOut(t *testing.T) {
	_, wire := inspectWithGhAuth(t, func(context.Context) ghAuthStatusResult {
		return ghAuthStatusResult{Output: ghStatusNoHosts, Completed: true}
	}, 5*time.Second)
	toolAuth, _ := wireToolAuth(wire)
	gh, _ := toolAuth["gh"].(map[string]any)
	// `authenticated: false` must survive omitempty: it is the signal.
	if v, present := gh["authenticated"]; !present || v != false || gh["authState"] != "unauthenticated" {
		t.Fatalf("specs.toolAuth.gh = %v, want an explicit signed-out reading", gh)
	}
}

func TestGatherReadinessOnly_UnknownGitHubSendsNothing(t *testing.T) {
	report, wire := inspectWithGhAuth(t, func(context.Context) ghAuthStatusResult {
		return ghAuthStatusResult{Output: ghStatusInvalidToken, Completed: true}
	}, 5*time.Second)
	if _, ok := wireToolAuth(wire); ok {
		t.Fatalf("an unknown GitHub sign-in must send no toolAuth at all: %v", wire["specs"])
	}
	if report.State != ReadinessReady {
		t.Fatalf("the GitHub check never changes the agent's own verdict, got %s", report.State)
	}
}

func TestGatherReadinessOnly_GitHubCheckNeverHoldsTheInspection(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := time.Now()
	report, wire := inspectWithGhAuth(t, func(context.Context) ghAuthStatusResult {
		<-release // answers only after the inspection is over
		return ghAuthStatusResult{Output: ghStatusSignedIn, ExitedZero: true, Completed: true}
	}, 300*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a hung GitHub check held the inspection for %s", elapsed)
	}
	if report.State != ReadinessReady {
		t.Fatalf("the gather finished in time; expected its verdict, got %s (%+v)", report.State, report.Findings)
	}
	if _, ok := wireToolAuth(wire); ok {
		t.Fatalf("a check that missed the deadline must report nothing: %v", wire["specs"])
	}
}
