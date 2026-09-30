// File: systemInfo_powershell.go
// -----------------------------------------------------------------------------
// PowerShell execution policy probe (Windows only) for computer setup.
//
// A Restricted (or AllSigned) policy stops npm's `npm.ps1` shim and every other
// local `.ps1` from loading, so a setup install fails with "running scripts is
// disabled on this system". Inspection reports the policy of every PowerShell
// host setup steps run in, and whether a per-user change can lift the block:
//
//   - one-shot setup commands run in Windows PowerShell 5.1 (powershell.exe,
//     runEncodedPowerShellViaArg);
//   - the persistent shell and its fallback prefer PowerShell 7 (pwsh.exe).
//
// The two hosts keep SEPARATE CurrentUser stores (5.1: the registry; 7.x:
// Documents\PowerShell\powershell.config.json), so each is probed on its own.
//
// This file is the ONE place the precedence rule lives. terminal-service and
// the frontend only read the flags reported here (shared-constants
// readPowerShellPolicy); they never re-derive which scope wins.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// PowerShell hosts, as reported (shared-constants POWERSHELL_HOST).
const (
	powerShellHostWindows = "windowsPowerShell"
	powerShellHost7       = "powershell7"
)

// Execution policies and scopes, as PowerShell spells them.
const (
	psPolicyRestricted = "Restricted"
	psPolicyAllSigned  = "AllSigned"
	psPolicyDefault    = "Default"
	psPolicyUndefined  = "Undefined"

	psScopeMachinePolicy = "MachinePolicy"
	psScopeUserPolicy    = "UserPolicy"
	psScopeProcess       = "Process"
	psScopeCurrentUser   = "CurrentUser"
	psScopeLocalMachine  = "LocalMachine"
	psScopeDefault       = "Default" // no scope defines a policy
)

// powerShellPolicyManualCommand is what a person can run themselves in the
// PowerShell host that blocks.
const powerShellPolicyManualCommand = "Set-ExecutionPolicy -Scope CurrentUser -ExecutionPolicy RemoteSigned"

// powerShellHostExe is the executable whose CurrentUser store a host reads.
var powerShellHostExe = map[string]string{
	powerShellHostWindows: "powershell",
	powerShellHost7:       "pwsh",
}

// psScopePrecedence is the order PowerShell resolves the effective policy in:
// the first scope with a defined policy wins.
var psScopePrecedence = []string{
	psScopeMachinePolicy,
	psScopeUserPolicy,
	psScopeProcess,
	psScopeCurrentUser,
	psScopeLocalMachine,
}

// powerShellPolicyScript reads the effective policy and every scope. Every enum
// is cast to a string and the scope list is projected into a map: without the
// casts ConvertTo-Json emits enum integers and {Scope, ExecutionPolicy}
// objects. Run without -ExecutionPolicy, which would set the very Process scope
// this reads.
const powerShellPolicyScript = `$s = @{}; Get-ExecutionPolicy -List | ForEach-Object { $s[[string]$_.Scope] = [string]$_.ExecutionPolicy }; ConvertTo-Json -Compress -InputObject @{ effective = [string](Get-ExecutionPolicy); scopes = $s; version = $PSVersionTable.PSVersion.ToString() }`

// powerShellHostPolicy is one host's policy.
type powerShellHostPolicy struct {
	Host      string            `json:"host"`
	Version   string            `json:"version,omitempty"`
	Effective string            `json:"effective"`
	Scopes    map[string]string `json:"scopes"`
	// BlockedBy is the scope that decides the effective policy (the first
	// defined one in precedence order), or "Default" when none is defined.
	BlockedBy            string `json:"blockedBy"`
	BlocksLocalScripts   bool   `json:"blocksLocalScripts"`
	FixableByCurrentUser bool   `json:"fixableByCurrentUser"`
}

// powerShellPolicyInfo is report.specs.powerShell (Windows only).
type powerShellPolicyInfo struct {
	Hosts []powerShellHostPolicy `json:"hosts"`
	// BlocksLocalScripts: at least one host blocks local scripts.
	BlocksLocalScripts bool `json:"blocksLocalScripts"`
	// FixableByCurrentUser: at least one host blocks, and setting CurrentUser to
	// RemoteSigned lifts the block in EVERY blocking host. A mixed report (one
	// host fixable, another locked by policy) is not fixable.
	FixableByCurrentUser bool `json:"fixableByCurrentUser"`
	// pwshDetected: pwsh is on PATH, even when its probe failed and Hosts has
	// no powershell7 entry. The manual command must still name the host.
	pwshDetected bool
}

// primaryBlockingHost is the host a message names: the first that blocks.
func (p *powerShellPolicyInfo) primaryBlockingHost() *powerShellHostPolicy {
	if p == nil {
		return nil
	}
	for i := range p.Hosts {
		if p.Hosts[i].BlocksLocalScripts {
			return &p.Hosts[i]
		}
	}
	return nil
}

// isPolicyDefined reports whether a scope value sets a policy.
func isPolicyDefined(v string) bool {
	return v != "" && !strings.EqualFold(v, psPolicyUndefined)
}

// policyBlocksLocalScripts: Restricted blocks every script; AllSigned blocks
// unsigned ones (npm's shim is unsigned); an unset policy on Windows client is
// Restricted.
func policyBlocksLocalScripts(effective string) bool {
	switch {
	case strings.EqualFold(effective, psPolicyRestricted),
		strings.EqualFold(effective, psPolicyAllSigned),
		strings.EqualFold(effective, psPolicyDefault),
		strings.EqualFold(effective, psPolicyUndefined):
		return true
	}
	return false
}

// classifyPowerShellPolicy fills BlockedBy / BlocksLocalScripts /
// FixableByCurrentUser for one host. Pure and OS-independent.
//
// A CurrentUser value overrides LocalMachine and the OS default, so the change
// is offered only when one of those decides the policy. Group Policy
// (MachinePolicy / UserPolicy) and the Process scope cannot be overridden from
// CurrentUser, and AllSigned is a deliberate choice this never changes.
func classifyPowerShellPolicy(h powerShellHostPolicy) powerShellHostPolicy {
	h.BlockedBy = psScopeDefault
	for _, scope := range psScopePrecedence {
		if isPolicyDefined(h.Scopes[scope]) {
			h.BlockedBy = scope
			break
		}
	}
	h.BlocksLocalScripts = policyBlocksLocalScripts(h.Effective)
	switch h.BlockedBy {
	case psScopeCurrentUser, psScopeLocalMachine, psScopeDefault:
		h.FixableByCurrentUser = h.BlocksLocalScripts && !strings.EqualFold(h.Effective, psPolicyAllSigned)
	default:
		h.FixableByCurrentUser = false
	}
	return h
}

// parsePowerShellPolicyJSON reads powerShellPolicyScript's output for host and
// classifies it. nil for empty, malformed or partial output (never panics).
func parsePowerShellPolicyJSON(host, out string) *powerShellHostPolicy {
	trimmed := strings.TrimSpace(out)
	// PowerShell may print a banner or warning before the JSON line.
	if i := strings.LastIndex(trimmed, "\n"); i >= 0 && !strings.HasPrefix(trimmed, "{") {
		trimmed = strings.TrimSpace(trimmed[i+1:])
	}
	var raw struct {
		Effective *string           `json:"effective"`
		Scopes    map[string]string `json:"scopes"`
		Version   string            `json:"version"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil
	}
	if raw.Effective == nil || strings.TrimSpace(*raw.Effective) == "" || raw.Scopes == nil {
		return nil
	}
	h := classifyPowerShellPolicy(powerShellHostPolicy{
		Host:      host,
		Version:   strings.TrimSpace(raw.Version),
		Effective: strings.TrimSpace(*raw.Effective),
		Scopes:    raw.Scopes,
	})
	return &h
}

// summarizePowerShellPolicy folds the per-host results into the report block;
// nil when no host could be probed.
func summarizePowerShellPolicy(hosts []powerShellHostPolicy) *powerShellPolicyInfo {
	if len(hosts) == 0 {
		return nil
	}
	info := &powerShellPolicyInfo{Hosts: hosts}
	allBlockingFixable := true
	for _, h := range hosts {
		if !h.BlocksLocalScripts {
			continue
		}
		info.BlocksLocalScripts = true
		if !h.FixableByCurrentUser {
			allBlockingFixable = false
		}
	}
	info.FixableByCurrentUser = info.BlocksLocalScripts && allBlockingFixable
	return info
}

// gatherPowerShellPolicyWindows probes Windows PowerShell always, and
// PowerShell 7 only when pwsh is on PATH. nil when neither answered.
func gatherPowerShellPolicyWindows(ctx context.Context, run setupProbeRunner) *powerShellPolicyInfo {
	type probe struct{ host, cmd string }
	probes := []probe{{powerShellHostWindows, "powershell"}}
	_, lookErr := setupProbeLookPath("pwsh")
	pwshDetected := lookErr == nil
	if pwshDetected {
		probes = append(probes, probe{powerShellHost7, "pwsh"})
	}
	results := make([]*powerShellHostPolicy, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p probe) {
			defer wg.Done()
			out, ok := run(ctx, p.cmd, []string{"-NoProfile", "-NonInteractive", "-Command", powerShellPolicyScript}, nil, setupProbeTimeout)
			if ok {
				results[i] = parsePowerShellPolicyJSON(p.host, out)
			}
		}(i, p)
	}
	wg.Wait()
	var hosts []powerShellHostPolicy
	for _, r := range results {
		if r != nil {
			hosts = append(hosts, *r)
		}
	}
	info := summarizePowerShellPolicy(hosts)
	if info != nil {
		info.pwshDetected = pwshDetected
	}
	return info
}

// manualCommand is what a person runs to allow local scripts in EVERY blocking
// host. The hosts keep separate CurrentUser stores, so setting the policy in
// one leaves the other restricted. The bare setter is only unambiguous when
// Windows PowerShell is the only host on the computer; once PowerShell 7 is
// installed (even when its probe failed) a person may paste into either window, so the setter is invoked
// once per blocking host through that host's own executable.
func (p *powerShellPolicyInfo) manualCommand() string {
	return p.manualCommandWhere(func(*powerShellHostPolicy) bool { return true })
}

// manualCommandWhere is manualCommand limited to the blocking hosts keep
// accepts, or "" when it accepts none.
func (p *powerShellPolicyInfo) manualCommandWhere(keep func(*powerShellHostPolicy) bool) string {
	var hosts []string
	onlyWindowsHost := p == nil || !p.pwshDetected
	anyBlocking := false
	if p != nil {
		for i := range p.Hosts {
			h := &p.Hosts[i]
			if h.Host != powerShellHostWindows {
				onlyWindowsHost = false
			}
			if !h.BlocksLocalScripts {
				continue
			}
			anyBlocking = true
			if keep(h) {
				hosts = append(hosts, h.Host)
			}
		}
	}
	if len(hosts) == 0 && anyBlocking {
		return ""
	}
	if len(hosts) == 0 || (onlyWindowsHost && len(hosts) == 1) {
		return powerShellPolicyManualCommand
	}
	cmds := make([]string, 0, len(hosts))
	for _, host := range hosts {
		exe, ok := powerShellHostExe[host]
		if !ok {
			exe = powerShellHostExe[powerShellHostWindows]
		}
		cmds = append(cmds, exe+` -NoProfile -Command "`+powerShellPolicyManualCommand+`"`)
	}
	return strings.Join(cmds, "; ")
}

// powerShellProcessOverrideHint is how a person lifts a Process-scoped policy,
// which no CurrentUser setting can override.
const powerShellProcessOverrideHint = "remove that override (for example, unset PSExecutionPolicyPreference and restart the AIExpedite agent)"

// powerShellPolicyFinding is the readiness message for a blocking policy, and
// whether setup can offer the per-user fix.
func powerShellPolicyFinding(p *powerShellPolicyInfo) (message string, fixable bool) {
	h := p.primaryBlockingHost()
	if h == nil {
		return "", false
	}
	manual := p.manualCommand()
	policy := h.Effective
	if strings.EqualFold(policy, psPolicyDefault) || strings.EqualFold(policy, psPolicyUndefined) {
		policy = psPolicyRestricted
	}
	if p.FixableByCurrentUser {
		return "PowerShell's execution policy (" + policy + ") blocks local scripts, so npm and other script-based tools won't run. " +
			"AIExpedite can allow them for your user account only (RemoteSigned, no administrator rights needed) with your permission.", true
	}
	// Name what controls the policy (the first host that blocks and cannot be
	// fixed, so a mixed report explains the part setup can't change). Group
	// Policy wins, so no other branch offers the setter for a host it owns.
	h = nil
	for i := range p.Hosts {
		o := &p.Hosts[i]
		if !o.BlocksLocalScripts || o.FixableByCurrentUser {
			continue
		}
		if o.BlockedBy == psScopeMachinePolicy || o.BlockedBy == psScopeUserPolicy {
			h = o
			break
		}
		if h == nil {
			h = o
		}
	}
	switch {
	case h.BlockedBy == psScopeMachinePolicy || h.BlockedBy == psScopeUserPolicy:
		// The CurrentUser setter cannot beat Group Policy, so it is offered
		// only for the other blocking hosts that Group Policy does not own.
		msg := "PowerShell's execution policy (" + h.Effective + ") is set by Group Policy and blocks local scripts, so npm and other script-based tools won't run. " +
			"Your IT administrator controls this setting; ask them to allow local scripts (RemoteSigned). " +
			"A per-user setting can't override Group Policy."
		// A Process scope beats CurrentUser too, so a Process-scoped host gets
		// the remove-the-override guidance instead of the setter.
		if rest := p.manualCommandWhere(func(o *powerShellHostPolicy) bool {
			return o.BlockedBy != psScopeMachinePolicy && o.BlockedBy != psScopeUserPolicy && o.BlockedBy != psScopeProcess
		}); rest != "" {
			msg += " For the other PowerShell host, run: " + rest
		}
		for i := range p.Hosts {
			if p.Hosts[i].BlocksLocalScripts && p.Hosts[i].BlockedBy == psScopeProcess {
				msg += " The other PowerShell host's policy is set for the process setup runs in; " + powerShellProcessOverrideHint + "."
				break
			}
		}
		return msg, false
	// Process outranks CurrentUser whatever the policy, so it is checked before
	// AllSigned: a Process-scoped AllSigned needs the override removed first.
	case h.BlockedBy == psScopeProcess:
		return "PowerShell's execution policy (" + h.Effective + ") is set for the PowerShell process setup runs in (for example by the PSExecutionPolicyPreference environment variable), so npm and other script-based tools won't run. " +
			"A per-user setting can't override it; " + powerShellProcessOverrideHint + " first, then, if scripts are still blocked, run: " + manual, false
	case strings.EqualFold(h.Effective, psPolicyAllSigned):
		return "PowerShell's execution policy is AllSigned, which blocks unsigned scripts such as npm's, so npm won't run. " +
			"AIExpedite won't change a policy you chose; to allow local scripts for your user, run: " + manual, false
	default:
		return "PowerShell's execution policy (" + policy + ") blocks local scripts, so npm and other script-based tools won't run. " +
			"To allow them for your user, run: " + manual, false
	}
}
