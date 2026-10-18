package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWorktreeCommandsAreInterceptedAndNeverDialogGated(t *testing.T) {
	commands := []string{
		worktreeEnsureCommand, worktreeListCommand, worktreeRemoveCommand,
		worktreeEvictDepsCommand, rootRestoreCommand,
	}
	for _, name := range commands {
		if !isWorktreeDeviceCommand(name) {
			t.Fatalf("%s is not recognised as a worktree built-in", name)
		}
		if !isInterceptedDeviceCommand(name) {
			t.Fatalf("%s would be looked up on PATH instead of intercepted", name)
		}
		// Intercepted before exec, and never dialog-gated: the sweep that
		// sends the destructive ones is unattended, so a native dialog would
		// block it forever. The boundary is the signed dispatch, the path
		// policy, the device-side proofs and the link-safe walk.
		gate := executeApprovalGate(nil, nil, commandMsg{Command: name, RiskLevel: riskDestructive})
		if !gate.DeviceCommand {
			t.Fatalf("%s is not routed to the device-command path", name)
		}
		if gate.Needed {
			t.Fatalf("%s would show the native approval dialog", name)
		}
	}
	for _, name := range []string{"git", "rm", "__env_find_repos__"} {
		if isWorktreeDeviceCommand(name) {
			t.Fatalf("%s must not be treated as a worktree built-in", name)
		}
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
	if !strings.Contains(err.Error(), rulePathRepoTrailingDot) {
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
