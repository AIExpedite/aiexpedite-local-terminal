package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func signInArgs(t *testing.T, argv []string, title string) []string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"argv": argv, "title": title})
	if err != nil {
		t.Fatal(err)
	}
	return []string{string(b)}
}

func TestIsEnvSetupDeviceCommand(t *testing.T) {
	for _, c := range []string{envFindReposCommand, envVerifyDepsCommand, envSignInCommand} {
		if !isEnvSetupDeviceCommand(c) {
			t.Errorf("%s should be a device command", c)
		}
	}
	for _, c := range []string{"__env_inspect__", "git", "__env_find_repos", "", "__ping__"} {
		if isEnvSetupDeviceCommand(c) {
			t.Errorf("%q must not be a device command", c)
		}
	}
}

// The approval gate: find / verify skip the allow list even when it would deny
// them; a sign-in always takes the native dialog, at external_write, even when
// the dispatch under-declares it or Allow All Commands is on; and an ordinary
// command still goes through the allow list.
func TestExecuteApprovalGate(t *testing.T) {
	cfg := &Config{EnableAllowList: true}
	al := &AllowList{} // empty: nothing allowlisted

	find := commandMsg{Command: envFindReposCommand, Args: []string{`{"roots":["/x"],"depth":1}`}, RiskLevel: riskReadOnly}
	if g := executeApprovalGate(cfg, al, find); g.Needed || !g.DeviceCommand {
		t.Fatalf("find repos: %+v", g)
	}
	verify := commandMsg{Command: envVerifyDepsCommand, Args: []string{`{"path":"/x","manager":"npm"}`}, RiskLevel: riskReadOnly}
	if g := executeApprovalGate(cfg, al, verify); g.Needed {
		t.Fatalf("verify deps must not prompt: %+v", g)
	}
	// A read-only command dispatched at a higher signed risk is still honoured.
	destructiveVerify := verify
	destructiveVerify.RiskLevel = riskDestructive
	if g := executeApprovalGate(cfg, al, destructiveVerify); !g.Needed {
		t.Fatal("a signed destructive risk must force the dialog")
	}

	for _, risk := range []string{riskExternalWrite, riskReadOnly, "safe_write", ""} {
		signIn := commandMsg{Command: envSignInCommand, Args: signInArgs(t, []string{"gh", "auth", "login", "--web"}, "Sign in to GitHub"), RiskLevel: risk}
		allowAll := &Config{EnableAllowList: false}
		allowAll.SetAllowAllCommands(true)
		for _, c := range []*Config{cfg, allowAll} {
			g := executeApprovalGate(c, nil, signIn)
			if !g.Needed || g.PolicyCommand.RiskLevel != riskExternalWrite {
				t.Fatalf("sign-in (risk %q) must always prompt at external_write: %+v", risk, g)
			}
			if g.DialogCommand != "gh" || !reflect.DeepEqual(g.DialogArgs, []string{"auth", "login", "--web"}) {
				t.Fatalf("dialog should show the argv, got %q %q", g.DialogCommand, g.DialogArgs)
			}
		}
	}
	// A timeout never auto-approves a sign-in, even with allow-on-timeout.
	signIn := commandMsg{Command: envSignInCommand, Args: signInArgs(t, []string{"codex", "login"}, ""), RiskLevel: riskReadOnly}
	g := executeApprovalGate(cfg, al, signIn)
	if got := applyTimeoutPolicy(ApprovalDeny, &Config{ApprovalTimeoutAction: "allow"}, g.PolicyCommand); got != ApprovalDeny {
		t.Fatalf("allow-on-timeout approved a sign-in: %v", got)
	}

	ordinary := commandMsg{Command: "git", Args: []string{"status"}}
	if g := executeApprovalGate(cfg, al, ordinary); !g.Needed || g.DeviceCommand {
		t.Fatalf("an unlisted ordinary command must prompt: %+v", g)
	}
	cfg.SetAllowAllCommands(true)
	if g := executeApprovalGate(cfg, al, ordinary); g.Needed {
		t.Fatalf("allow-all should pass an ordinary command: %+v", g)
	}
}

func TestParseEnvSignInRequest(t *testing.T) {
	req, err := parseEnvSignInRequest(signInArgs(t, []string{"gh", "auth", "login", "--hostname", "github.com", "--web", "--scopes=repo,workflow"}, "Sign in to GitHub & more\"; rm -rf"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Title != "Sign in to GitHub  more rm -rf" {
		t.Fatalf("title not sanitized: %q", req.Title)
	}
	if req, err := parseEnvSignInRequest(signInArgs(t, []string{"codex", "login"}, "")); err != nil || req.Title != "Sign in to codex" {
		t.Fatalf("default title: %q %v", req.Title, err)
	}
	for _, bad := range [][]string{
		{},
		{""},
		{"gh", "auth login"},
		{"gh", "a&b"},
		{"gh", `"quoted"`},
		{"gh", "x|y"},
		{"gh", "%PATH%"},
		{"gh", "$(id)"},
		{"gh", "a;b"},
		{"gh", "line\nbreak"},
	} {
		if _, err := parseEnvSignInRequest(signInArgs(t, bad, "t")); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
	if _, err := parseEnvSignInRequest(nil); err == nil {
		t.Error("expected missing args to be rejected")
	}
	if _, err := parseEnvSignInRequest([]string{"{not json"}); err == nil {
		t.Error("expected bad JSON to be rejected")
	}
}

func TestSignInWindowCommandShapes(t *testing.T) {
	cl := windowsSignInCommandLine(`C:\Windows\System32\cmd.exe`, `C:\Program Files\GitHub CLI\gh.exe`, []string{"auth", "login", "--web"}, "Sign in to GitHub")
	want := `"C:\Windows\System32\cmd.exe" /d /c title Sign in to GitHub & "C:\Program Files\GitHub CLI\gh.exe" auth login --web & echo. & pause`
	if cl != want {
		t.Fatalf("windows command line\n got %s\nwant %s", cl, want)
	}

	script := macSignInScript("/opt/homebrew/bin/gh", []string{"auth", "login"}, "Sign in to GitHub's CLI")
	if !strings.Contains(script, `do script "clear; echo 'Sign in to GitHub'\\''s CLI'; echo; '/opt/homebrew/bin/gh' 'auth' 'login'"`) {
		t.Fatalf("mac script:\n%s", script)
	}

	cands := linuxSignInCandidates("/usr/bin/gh", []string{"auth", "login"}, "T")
	if len(cands) != 3 || cands[0][0] != "x-terminal-emulator" || cands[1][0] != "gnome-terminal" || cands[2][0] != "xterm" {
		t.Fatalf("linux candidates %q", cands)
	}
	if got := cands[1]; !reflect.DeepEqual(got, []string{"gnome-terminal", "--title=T", "--", "/usr/bin/gh", "auth", "login"}) {
		t.Fatalf("gnome-terminal argv %q", got)
	}
}

// launchSignIn resolves argv[0] on the (refreshed) PATH and hands the resolved
// program to the launcher without waiting; an unknown program is an error.
func TestLaunchSignIn_ResolvesOnPath(t *testing.T) {
	dir := t.TempDir()
	name := "aix-fake-signin"
	file := name
	content := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		file = name + ".cmd"
		content = "@exit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var gotProgram, gotTitle string
	var gotArgs []string
	prev := signInLauncher
	signInLauncher = func(program string, args []string, title string) error {
		gotProgram, gotArgs, gotTitle = program, args, title
		return nil
	}
	t.Cleanup(func() { signInLauncher = prev })

	out, err := runEnvSetupDeviceCommand(context.Background(), nil, commandMsg{
		Command: envSignInCommand,
		Args:    signInArgs(t, []string{name, "login", "--device-auth"}, "Sign in to Fake"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"launched":true}` {
		t.Fatalf("output %q", out)
	}
	if !strings.EqualFold(gotProgram, filepath.Join(dir, file)) || !reflect.DeepEqual(gotArgs, []string{"login", "--device-auth"}) || gotTitle != "Sign in to Fake" {
		t.Fatalf("launcher got %q %q %q", gotProgram, gotArgs, gotTitle)
	}

	_, err = runEnvSetupDeviceCommand(context.Background(), nil, commandMsg{
		Command: envSignInCommand,
		Args:    signInArgs(t, []string{"aix-definitely-not-installed"}, ""),
	})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected a not-installed error, got %v", err)
	}
}

func TestRunEnvSetupDeviceCommand_Errors(t *testing.T) {
	for _, cmd := range []commandMsg{
		{Command: envFindReposCommand},
		{Command: envFindReposCommand, Args: []string{"not json"}},
		{Command: envVerifyDepsCommand, Args: []string{`{"path":"relative","manager":"npm"}`}},
		{Command: envFindReposCommand, Args: []string{strings.Repeat(" ", maxEnvSetupCommandArgBytes+1)}},
		{Command: "__env_unknown__", Args: []string{"{}"}},
	} {
		if out, err := runEnvSetupDeviceCommand(context.Background(), nil, cmd); err == nil {
			t.Errorf("%s %q: expected an error, got %q", cmd.Command, cmd.Args, out)
		} else if !strings.HasPrefix(err.Error(), cmd.Command+": ") {
			t.Errorf("error should name the command: %v", err)
		}
	}
	// Unknown input fields are ignored (a newer server may add some).
	out, err := runEnvSetupDeviceCommand(context.Background(), nil, commandMsg{
		Command: envVerifyDepsCommand,
		Args:    []string{`{"path":"/x","manager":"yarn","futureField":1}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"lockfilePresent": false, "lockfileSha256": "", "hiddenLockfilePresent": false,
		"match": false, "reason": "unsupported_manager", "mismatches": []any{}}
	if !reflect.DeepEqual(got, want) || strings.ContainsRune(out, '\n') {
		t.Fatalf("got %s", out)
	}
}

// Redaction runs per string, so a secret-looking value is masked without
// breaking the JSON document (a whole-output \S+ pass would eat the quotes).
func TestEncodeRedactedJSON_StaysValid(t *testing.T) {
	v := map[string]any{
		"mismatches": []string{`node_modules/@x/token: version 1 != 2`, `password=hunter2","x":"y`},
		"remote":     "https://h.example/r?token=abc",
	}
	out, err := encodeRedactedJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "token=abc") {
		t.Fatalf("secret survived: %s", out)
	}
}
