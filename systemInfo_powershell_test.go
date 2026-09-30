package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func psHost(effective string, scopes map[string]string) powerShellHostPolicy {
	return classifyPowerShellPolicy(powerShellHostPolicy{
		Host:      powerShellHostWindows,
		Effective: effective,
		Scopes:    scopes,
	})
}

func allUndefined(overrides map[string]string) map[string]string {
	s := map[string]string{
		psScopeMachinePolicy: "Undefined",
		psScopeUserPolicy:    "Undefined",
		psScopeProcess:       "Undefined",
		psScopeCurrentUser:   "Undefined",
		psScopeLocalMachine:  "Undefined",
	}
	for k, v := range overrides {
		s[k] = v
	}
	return s
}

func TestClassifyPowerShellPolicy(t *testing.T) {
	cases := []struct {
		name      string
		effective string
		scopes    map[string]string
		blockedBy string
		blocks    bool
		fixable   bool
	}{
		{"all undefined -> Windows default", "Restricted", allUndefined(nil), psScopeDefault, true, true},
		{"effective reported Undefined", "Undefined", allUndefined(nil), psScopeDefault, true, true},
		{"LocalMachine Restricted, CurrentUser unset", "Restricted", allUndefined(map[string]string{"LocalMachine": "Restricted"}), psScopeLocalMachine, true, true},
		{"CurrentUser Restricted", "Restricted", allUndefined(map[string]string{"CurrentUser": "Restricted"}), psScopeCurrentUser, true, true},
		{"CurrentUser RemoteSigned", "RemoteSigned", allUndefined(map[string]string{"CurrentUser": "RemoteSigned", "LocalMachine": "Restricted"}), psScopeCurrentUser, false, false},
		{"LocalMachine RemoteSigned (server default)", "RemoteSigned", allUndefined(map[string]string{"LocalMachine": "RemoteSigned"}), psScopeLocalMachine, false, false},
		{"LocalMachine Unrestricted", "Unrestricted", allUndefined(map[string]string{"LocalMachine": "Unrestricted"}), psScopeLocalMachine, false, false},
		{"Process Bypass", "Bypass", allUndefined(map[string]string{"Process": "Bypass", "LocalMachine": "Restricted"}), psScopeProcess, false, false},
		{"Process Restricted cannot be overridden", "Restricted", allUndefined(map[string]string{"Process": "Restricted"}), psScopeProcess, true, false},
		{"MachinePolicy Restricted over CurrentUser RemoteSigned", "Restricted", allUndefined(map[string]string{"MachinePolicy": "Restricted", "CurrentUser": "RemoteSigned"}), psScopeMachinePolicy, true, false},
		{"UserPolicy Restricted", "Restricted", allUndefined(map[string]string{"UserPolicy": "Restricted"}), psScopeUserPolicy, true, false},
		{"MachinePolicy RemoteSigned", "RemoteSigned", allUndefined(map[string]string{"MachinePolicy": "RemoteSigned"}), psScopeMachinePolicy, false, false},
		{"AllSigned on LocalMachine is never changed", "AllSigned", allUndefined(map[string]string{"LocalMachine": "AllSigned"}), psScopeLocalMachine, true, false},
		{"AllSigned on CurrentUser is never changed", "AllSigned", allUndefined(map[string]string{"CurrentUser": "AllSigned"}), psScopeCurrentUser, true, false},
		{"missing scope keys read as undefined", "Restricted", map[string]string{}, psScopeDefault, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := psHost(c.effective, c.scopes)
			if h.BlockedBy != c.blockedBy || h.BlocksLocalScripts != c.blocks || h.FixableByCurrentUser != c.fixable {
				t.Fatalf("got blockedBy=%s blocks=%v fixable=%v, want %s %v %v",
					h.BlockedBy, h.BlocksLocalScripts, h.FixableByCurrentUser, c.blockedBy, c.blocks, c.fixable)
			}
		})
	}
}

func TestParsePowerShellPolicyJSON(t *testing.T) {
	out := `{"effective":"Restricted","scopes":{"MachinePolicy":"Undefined","UserPolicy":"Undefined","Process":"Undefined","CurrentUser":"Undefined","LocalMachine":"Undefined"},"version":"5.1.26100.4768"}` + "\r\n"
	h := parsePowerShellPolicyJSON(powerShellHostWindows, out)
	if h == nil || h.Host != powerShellHostWindows || h.Version != "5.1.26100.4768" || h.Effective != "Restricted" ||
		h.BlockedBy != psScopeDefault || !h.BlocksLocalScripts || !h.FixableByCurrentUser {
		t.Fatalf("parsed %+v", h)
	}

	// A warning line before the JSON is tolerated.
	h = parsePowerShellPolicyJSON(powerShellHost7, "WARNING: something\n"+`{"effective":"RemoteSigned","scopes":{},"version":"7.4.6"}`)
	if h == nil || h.Host != powerShellHost7 || h.BlocksLocalScripts {
		t.Fatalf("parsed %+v", h)
	}

	for _, bad := range []string{
		"",
		"not json",
		`{"scopes":{}}`,                // no effective
		`{"effective":"","scopes":{}}`, // empty effective
		`{"effective":"Restricted"}`,   // no scopes
		`{"effective":3,"scopes":{}}`,  // enum integer (script without casts)
		`{"effective":"Restricted","scopes":[{"Scope":0}]}`, // list, not a map
		`{"effective":"Restr`,
	} {
		if got := parsePowerShellPolicyJSON(powerShellHostWindows, bad); got != nil {
			t.Fatalf("%q parsed as %+v, want nil", bad, got)
		}
	}
}

func TestSummarizePowerShellPolicy(t *testing.T) {
	fixable := psHost("Restricted", allUndefined(nil))
	locked := psHost("Restricted", allUndefined(map[string]string{"MachinePolicy": "Restricted"}))
	locked.Host = powerShellHost7
	permissive := psHost("RemoteSigned", allUndefined(map[string]string{"LocalMachine": "RemoteSigned"}))
	permissive.Host = powerShellHost7

	if summarizePowerShellPolicy(nil) != nil {
		t.Fatal("no hosts must report nothing")
	}
	s := summarizePowerShellPolicy([]powerShellHostPolicy{fixable, permissive})
	if !s.BlocksLocalScripts || !s.FixableByCurrentUser {
		t.Fatalf("5.1 fixable + 7 permissive: %+v", s)
	}
	s = summarizePowerShellPolicy([]powerShellHostPolicy{fixable, locked})
	if !s.BlocksLocalScripts || s.FixableByCurrentUser {
		t.Fatalf("mixed report must not be fixable: %+v", s)
	}
	s = summarizePowerShellPolicy([]powerShellHostPolicy{permissive})
	if s.BlocksLocalScripts || s.FixableByCurrentUser {
		t.Fatalf("permissive: %+v", s)
	}
}

// policyProbeRunner answers the policy script per command and everything else
// as absent.
type policyProbeRunner struct {
	mu      sync.Mutex
	answers map[string]string
	calls   []string
}

func (f *policyProbeRunner) run(ctx context.Context, cmd string, args, env []string, timeout time.Duration) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd+" "+strings.Join(args, " "))
	for _, a := range args {
		if a == "-ExecutionPolicy" {
			panic("the probe must never pass -ExecutionPolicy")
		}
	}
	if len(args) == 0 || args[len(args)-1] != powerShellPolicyScript {
		return "", false
	}
	out, ok := f.answers[cmd]
	return out, ok
}

func (f *policyProbeRunner) ran(cmd string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, cmd+" ") && strings.HasSuffix(c, powerShellPolicyScript) {
			return true
		}
	}
	return false
}

const restrictedJSON = `{"effective":"Restricted","scopes":{"MachinePolicy":"Undefined","UserPolicy":"Undefined","Process":"Undefined","CurrentUser":"Undefined","LocalMachine":"Undefined"},"version":"5.1.26100.4768"}`

func TestGatherPowerShellPolicyWindows_PwshOnlyWhenOnPath(t *testing.T) {
	stubSetupLookPath(t) // pwsh absent
	f := &policyProbeRunner{answers: map[string]string{"powershell": restrictedJSON}}
	p := gatherPowerShellPolicyWindows(context.Background(), f.run)
	if p == nil || len(p.Hosts) != 1 || p.Hosts[0].Host != powerShellHostWindows || !p.FixableByCurrentUser {
		t.Fatalf("got %+v", p)
	}
	if f.ran("pwsh") {
		t.Fatalf("pwsh must not be probed when it is not on PATH: %v", f.calls)
	}

	stubSetupLookPath(t, "pwsh")
	f = &policyProbeRunner{answers: map[string]string{
		"powershell": restrictedJSON,
		"pwsh":       `{"effective":"RemoteSigned","scopes":{"LocalMachine":"RemoteSigned"},"version":"7.4.6"}`,
	}}
	p = gatherPowerShellPolicyWindows(context.Background(), f.run)
	if p == nil || len(p.Hosts) != 2 || p.Hosts[1].Host != powerShellHost7 || p.Hosts[1].BlocksLocalScripts {
		t.Fatalf("got %+v", p)
	}
	if !f.ran("powershell") || !f.ran("pwsh") {
		t.Fatalf("both hosts must be probed: %v", f.calls)
	}
}

func TestGatherPowerShellPolicyWindows_NoAnswerIsNil(t *testing.T) {
	stubSetupLookPath(t)
	f := &policyProbeRunner{answers: map[string]string{}} // timed out / failed
	if p := gatherPowerShellPolicyWindows(context.Background(), f.run); p != nil {
		t.Fatalf("a failed probe must leave the field nil: %+v", p)
	}
}

func TestGatherSetupExtras_PowerShellPolicyWindowsOnly(t *testing.T) {
	stubSetupLookPath(t)
	f := &policyProbeRunner{answers: map[string]string{"powershell": restrictedJSON}}
	x := gatherSetupExtras(context.Background(), "windows", f.run)
	if x.PowerShell == nil || !x.PowerShell.BlocksLocalScripts {
		t.Fatalf("windows: %+v", x.PowerShell)
	}
	info := &MachineInfo{}
	applySetupExtras(info, x)
	if info.PowerShell != x.PowerShell {
		t.Fatal("applySetupExtras must copy the policy onto MachineInfo")
	}

	for _, goos := range []string{"darwin", "linux"} {
		f := &policyProbeRunner{answers: map[string]string{"powershell": restrictedJSON, "pwsh": restrictedJSON}}
		x := gatherSetupExtras(context.Background(), goos, f.run)
		if x.PowerShell != nil || f.ran("powershell") || f.ran("pwsh") {
			t.Fatalf("%s must not probe the execution policy: %+v %v", goos, x.PowerShell, f.calls)
		}
	}
}

func TestPowerShellPolicyFindingMessages(t *testing.T) {
	cases := []struct {
		name    string
		host    powerShellHostPolicy
		fixable bool
		want    []string
	}{
		{"fixable", psHost("Restricted", allUndefined(nil)), true, []string{"(Restricted)", "npm", "RemoteSigned"}},
		{"group policy", psHost("Restricted", allUndefined(map[string]string{"MachinePolicy": "Restricted"})), false, []string{"Group Policy", "IT administrator", powerShellPolicyManualCommand}},
		{"all signed", psHost("AllSigned", allUndefined(map[string]string{"LocalMachine": "AllSigned"})), false, []string{"AllSigned", powerShellPolicyManualCommand}},
		{"process", psHost("Restricted", allUndefined(map[string]string{"Process": "Restricted"})), false, []string{"process", powerShellPolicyManualCommand}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, fixable := powerShellPolicyFinding(summarizePowerShellPolicy([]powerShellHostPolicy{c.host}))
			if fixable != c.fixable {
				t.Fatalf("fixable = %v", fixable)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Fatalf("message %q lacks %q", msg, w)
				}
			}
			if strings.Contains(msg, "Unrestricted") || strings.Contains(msg, "Bypass") {
				t.Fatalf("message must never suggest turning signing off: %q", msg)
			}
		})
	}
}
