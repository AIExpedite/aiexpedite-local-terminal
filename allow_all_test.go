// Tests for the "Allow All Commands" tray bypass. The toggle is a hard
// operator override: when on, both the execute and session-entry paths
// MUST skip the allow-list and approval dialog entirely.
package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// restrictiveAllowList returns an AllowList that only matches the
// internal __ping__ command — so any real command we test against will
// miss the list and (without the bypass) be routed to the dialog.
func restrictiveAllowList() *AllowList {
	return &AllowList{
		patterns: []string{"__ping__"},
		compiled: []*regexp.Regexp{patternToRegex("__ping__")},
	}
}

func TestShouldGateExecuteCommand_AllowAllBypasses(t *testing.T) {
	al := restrictiveAllowList()

	cases := []struct {
		name            string
		enableAllowList bool
		allowAll        bool
		al              *AllowList
		cmd             string
		args            []string
		wantShouldGate  bool
		why             string
	}{
		{
			name:            "bypass on — unmatched cmd skips gating",
			enableAllowList: true,
			allowAll:        true,
			al:              al,
			cmd:             "curl",
			args:            []string{"http://example.com"},
			wantShouldGate:  false,
			why:             "AllowAllCommands must short-circuit even when the command misses the list",
		},
		{
			name:            "bypass off — unmatched cmd is gated",
			enableAllowList: true,
			allowAll:        false,
			al:              al,
			cmd:             "curl",
			args:            []string{"http://example.com"},
			wantShouldGate:  true,
			why:             "default posture must still route unknown commands through the dialog",
		},
		{
			name:            "bypass off — matched cmd skips gating",
			enableAllowList: true,
			allowAll:        false,
			al:              al,
			cmd:             "__ping__",
			args:            nil,
			wantShouldGate:  false,
			why:             "allowlist hit means no dialog is needed",
		},
		{
			name:            "allowlist disabled — everything skips gating",
			enableAllowList: false,
			allowAll:        false,
			al:              al,
			cmd:             "curl",
			args:            nil,
			wantShouldGate:  false,
			why:             "EnableAllowList=false is the explicit \"don't gate\" config",
		},
		{
			name:            "nil allowlist — degrade open (matches handleMessage's nil check)",
			enableAllowList: true,
			allowAll:        false,
			al:              nil,
			cmd:             "curl",
			args:            nil,
			wantShouldGate:  false,
			why:             "without an initialised list we can't gate; mirrors handleMessage's guard",
		},
		{
			name:            "bypass on AND allowlist disabled — still skip",
			enableAllowList: false,
			allowAll:        true,
			al:              al,
			cmd:             "anything",
			args:            nil,
			wantShouldGate:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{EnableAllowList: tc.enableAllowList}
			// shouldGateExecuteCommand reads AllowAllCommands via the
			// synchronised accessor (cfg.IsAllowAllCommands), not the
			// raw field — so tests must publish through SetAllowAllCommands
			// to populate the atomic mirror.
			cfg.SetAllowAllCommands(tc.allowAll)
			got := shouldGateExecuteCommand(cfg, tc.al, tc.cmd, tc.args)
			if got != tc.wantShouldGate {
				t.Errorf("shouldGateExecuteCommand = %v; want %v (%s)", got, tc.wantShouldGate, tc.why)
			}
		})
	}
}

// gateSessionEntryCommand has multiple entry kinds. When AllowAllCommands
// is on, EVERY kind must return true without touching the allow list or
// dialog. We swap defaultAllowList for a restrictive list so a missed
// bypass would (in the non-test world) hit ShowCommandApprovalDialog —
// here it would simply return false because no dialog is available, which
// the test catches.
func TestGateSessionEntryCommand_AllowAllBypassesAllKinds(t *testing.T) {
	prev := defaultAllowList
	defaultAllowList = restrictiveAllowList()
	t.Cleanup(func() { defaultAllowList = prev })

	cfg := &Config{
		EnableAllowList: true,
		// CommandSecret left empty so grok_acp_start does NOT take its
		// "signed mode → return true" short-circuit; we want to prove
		// AllowAllCommands is what's letting it through.
	}
	// gateSessionEntryCommand reads AllowAllCommands via the synchronised
	// accessor, so publish through SetAllowAllCommands to populate the
	// atomic mirror — a raw `AllowAllCommands: true` literal would leave
	// the atomic at false and the bypass would never fire.
	cfg.SetAllowAllCommands(true)

	kinds := []string{
		"session_start",
		"codex_appserver_start",
		"grok_acp_start",
		"session_input", // mid-session pass-through — included for completeness
		"",              // execute-shape — also a pass-through here
	}

	for _, kind := range kinds {
		t.Run("kind="+kind, func(t *testing.T) {
			cmd := commandMsg{
				ID:      "t",
				Type:    kind,
				Command: "curl", // intentionally not in the restrictive list
				Args:    []string{"http://example.com"},
			}
			ok := gateSessionEntryCommand(context.Background(), nil, nil, cmd, cfg)
			if !ok {
				t.Errorf("gateSessionEntryCommand kind=%q returned false with AllowAllCommands=true; expected immediate pass-through", kind)
			}
		})
	}
}

// writableRestrictiveAllowList mirrors restrictiveAllowList but is backed by a
// real temp file so AddPattern (the "Always" persistence path) can append.
func writableRestrictiveAllowList(t *testing.T) *AllowList {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allow.txt")
	if err := os.WriteFile(path, []byte("__ping__\n"), 0600); err != nil {
		t.Fatalf("seed allow file: %v", err)
	}
	return &AllowList{
		configPath: path,
		patterns:   []string{"__ping__"},
		compiled:   []*regexp.Regexp{patternToRegex("__ping__")},
	}
}

// A grok_acp_start approved with "Always" must NEVER persist a `grok`
// allow-list entry: GeneratePatternFromCommand would emit `grok *`, permanently
// allow-listing a bare `grok` and reopening the raw `execute grok …` bypass the
// grok_acp_start short-circuit exists to close. "Always" degrades to one-time
// for that path. A session_start control proves the persist path is otherwise
// live (so the grok exception is real, not the dialog stub failing to fire).
func TestGateSessionEntryCommand_GrokAlwaysDoesNotPersistBypass(t *testing.T) {
	prevList := defaultAllowList
	al := writableRestrictiveAllowList(t)
	defaultAllowList = al
	t.Cleanup(func() { defaultAllowList = prevList })

	prevDialog := commandApprovalDialogFn
	commandApprovalDialogFn = func(string, []string, int) ApprovalResult { return ApprovalAlways }
	t.Cleanup(func() { commandApprovalDialogFn = prevDialog })

	// CommandSecret empty → unsigned mode, so grok_acp_start does NOT take the
	// signed short-circuit and instead reaches the dialog + Always branch.
	cfg := &Config{EnableAllowList: true}
	cfg.SetAllowAllCommands(false)

	grokCmd := commandMsg{
		ID:      "g",
		Type:    "grok_acp_start",
		Command: "grok",
		Args:    []string{"sessionID", "--always-approve"},
	}
	if !gateSessionEntryCommand(context.Background(), nil, nil, grokCmd, cfg) {
		t.Fatalf("grok_acp_start with Always approval should pass through (return true)")
	}
	if al.IsAllowed("grok", []string{"agent", "stdio"}) {
		t.Errorf("grok_acp_start Always persisted a grok allow-list entry; raw `execute grok …` bypass reopened")
	}
	for _, p := range al.patterns {
		if p == "grok *" {
			t.Errorf("allow list gained %q after grok_acp_start Always; must never persist", p)
		}
	}

	// Control: a session_start Always DOES persist, so the stub is reaching the
	// persistence branch — the grok result above is the deliberate exception.
	sessCmd := commandMsg{ID: "s", Type: "session_start", Command: "mytool", Args: []string{"--go"}}
	if !gateSessionEntryCommand(context.Background(), nil, nil, sessCmd, cfg) {
		t.Fatalf("session_start with Always approval should pass through")
	}
	if !al.IsAllowed("mytool", []string{"--go"}) {
		t.Errorf("session_start Always did not persist `mytool *`; persistence path is dead, so the grok assertion proves nothing")
	}
}

func TestGateSessionEntryCommand_AllowAllOffStillGatesUnknownKinds(t *testing.T) {
	// Negative-transition test: with the bypass OFF the function must
	// still treat unknown session kinds as pass-through (matches the
	// pre-feature behaviour). We only assert the cheap, non-blocking
	// branches here — the dialog-bound branches (session_start,
	// codex_appserver_start) are covered separately and can't run in CI
	// without a UI host.
	prev := defaultAllowList
	defaultAllowList = restrictiveAllowList()
	t.Cleanup(func() { defaultAllowList = prev })

	cfg := &Config{EnableAllowList: true}
	cfg.SetAllowAllCommands(false)
	for _, kind := range []string{"session_input", "session_signal", "session_end", ""} {
		cmd := commandMsg{Type: kind, Command: "curl", Args: []string{"x"}}
		if !gateSessionEntryCommand(context.Background(), nil, nil, cmd, cfg) {
			t.Errorf("kind=%q with bypass off should still pass through mid-session commands", kind)
		}
	}
}
