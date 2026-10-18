package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// worktreeBuiltIns is every command the family owns, so a new one joins these
// tests by being added here rather than by being remembered.
func worktreeBuiltIns() []string {
	return []string{
		worktreeEnsureCommand, worktreeListCommand, worktreeRemoveCommand,
		worktreeEvictDepsCommand, rootRestoreCommand,
	}
}

func TestWorktreeCommandsSkipTheDialogONLYWhenSigned(t *testing.T) {
	for _, name := range worktreeBuiltIns() {
		if !isWorktreeDeviceCommand(name) {
			t.Fatalf("%s is not recognised as a worktree built-in", name)
		}

		// SIGNED: intercepted before exec and not dialog-gated. The sweep that
		// sends the destructive ones is unattended, so a native dialog would
		// block it forever. The boundary is the signed dispatch, the path
		// policy, the device-side proofs and the link-safe walk.
		signed := executeApprovalGate(&Config{CommandSecret: "s3cret"}, nil,
			commandMsg{Command: name, RiskLevel: riskDestructive})
		if !signed.DeviceCommand {
			t.Fatalf("%s is not routed to the device-command path", name)
		}
		if signed.Needed {
			t.Fatalf("%s would show the native approval dialog in signed mode", name)
		}

		// UNSIGNED: the justification for skipping the dialog is that the
		// dispatch was signed, and with no command secret nothing verifies a
		// signature. So the exemption lapses rather than quietly becoming an
		// unauthenticated bypass.
		for _, cfg := range []*Config{nil, {CommandSecret: ""}} {
			unsigned := executeApprovalGate(cfg, nil,
				commandMsg{Command: name, RiskLevel: riskDestructive})
			if !unsigned.DeviceCommand {
				t.Fatalf("%s is not routed to the device-command path when unsigned", name)
			}
			if !unsigned.Needed {
				t.Fatalf("%s skipped the approval dialog with no command secret", name)
			}
		}
	}
	for _, name := range []string{"git", "rm", "__env_find_repos__"} {
		if isWorktreeDeviceCommand(name) {
			t.Fatalf("%s must not be treated as a worktree built-in", name)
		}
	}
}

// The gate is defence in depth; this is the authoritative refusal. A dialog
// approved by a person would still reach the handler, and the handler is what
// a signature-less dispatch must not get past.
func TestWorktreeCommandsRefuseWithoutACommandSecret(t *testing.T) {
	for _, name := range worktreeBuiltIns() {
		for _, cfg := range []*Config{nil, {CommandSecret: ""}} {
			out, err := runInterceptedDeviceCommand(context.Background(), cfg,
				commandMsg{Command: name, Args: []string{"{}"}})
			if err == nil {
				t.Fatalf("%s ran with no command secret (output %q)", name, out)
			}
			if !strings.Contains(err.Error(), "signed dispatch") {
				t.Fatalf("%s refused for the wrong reason: %v", name, err)
			}
		}
	}
}

// The env-setup family keeps its own gating, which is the native dialog, so it
// must NOT pick up the worktree refusal.
func TestEnvSetupCommandsAreNotRefusedByTheWorktreeSecretCheck(t *testing.T) {
	_, err := runInterceptedDeviceCommand(context.Background(), &Config{},
		commandMsg{Command: "__env_find_repos__", Args: []string{"{}"}})
	if err != nil && strings.Contains(err.Error(), "signed dispatch") {
		t.Fatalf("an env-setup command took the worktree secret refusal: %v", err)
	}
}

func TestWorktreeCommandResultIsOneLineOfJSON(t *testing.T) {
	fx := newWorktreeFixture(t)
	args, err := json.Marshal(worktreeListRequest{Root: fx.Root, MainRepos: []string{fx.MainRepo}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runWorktreeDeviceCommand(context.Background(), commandMsg{
		Command: worktreeListCommand,
		Args:    []string{string(args)},
	})
	if err != nil {
		t.Fatalf("runWorktreeDeviceCommand: %v", err)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("the result must be ONE line: %q", out)
	}
	var decoded worktreeListResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, out)
	}
	if decoded.Root == "" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestWorktreeCommandRejectsAMalformedArgument(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"{not json"}} {
		if _, err := runWorktreeDeviceCommand(context.Background(), commandMsg{
			Command: worktreeEnsureCommand, Args: args,
		}); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}

// TestWorktreeCommandRefusalNamesTheRule: a policy refusal travels back as the
// rule that failed, so the server records WHICH rule refused rather than
// "invalid path".
func TestWorktreeCommandRefusalNamesTheRule(t *testing.T) {
	fx := newWorktreeFixture(t)
	args, err := json.Marshal(fx.removeReq("aixwt/" + wtTestFeatureFolder + "/repo."))
	if err != nil {
		t.Fatal(err)
	}
	_, err = runWorktreeDeviceCommand(context.Background(), commandMsg{
		Command: worktreeRemoveCommand, Args: []string{string(args)},
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// A trailing dot is refused by the DISCRIMINATOR rule now: a segment that
	// must end in `-<6 hex>` cannot end in a dot, so the dedicated trailing-dot
	// rule became unreachable and went.
	if !strings.Contains(err.Error(), rulePathRepoDiscriminator) {
		t.Fatalf("the error should name the rule, got: %v", err)
	}
}

func TestWorktreeCommandBoundsAreFixedNotDerived(t *testing.T) {
	fx := newWorktreeFixture(t)
	req := fx.ensureReq("feature/too-many")
	for i := 0; i <= maxEnsureCompanions; i++ {
		req.Companions = append(req.Companions, worktreeCompanionSpec{MainRepo: fx.MainRepo})
	}
	if _, err := ensureWorktree(context.Background(), req); err == nil {
		t.Fatal("accepted more companions than the fixed bound")
	}
}
