package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2"
)

/* --------------------------------------------------------------------------
   pubsub_cliagent_smoke_test.go — wiring for the signed __cli_smoke__ command.
   --------------------------------------------------------------------------
   The probe itself is covered in cliagent_smoke_claudecode_test.go. What is
   pinned here is the contract the CLI-maintenance flow depends on: exactly one
   correlated result per command, a cooldown that cannot be turned into a quota
   drain by a retry storm, and an unknown cliId that never reaches generic
   execute.
   ------------------------------------------------------------------------ */

// capturePublishes swaps the publish seam and returns the slice it fills.
func capturePublishes(t *testing.T) *[]resultMsg {
	t.Helper()
	var published []resultMsg
	original := publishMsg
	publishMsg = func(ctx context.Context, topic *pubsub.Publisher, res resultMsg) error {
		published = append(published, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = original })
	return &published
}

func smokeCommand(cliID string) commandMsg {
	return commandMsg{
		ID: "cmd-1", WorkspaceID: "ws-1", UID: "uid-1",
		Command: cliSmokeCommand, Args: []string{cliID}, RefreshID: "refresh-7",
	}
}

func TestHandleCLISmokeCommand_PublishesExactlyOneCorrelatedResult(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})
	published := capturePublishes(t)

	cfg := &Config{AgentID: "agent-9"}
	if err := handleCLISmokeCommand(context.Background(), nil, smokeCommand("claudeCode"), cfg); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}

	if len(*published) != 1 {
		t.Fatalf("published %d messages, want exactly 1", len(*published))
	}
	res := (*published)[0]
	if res.Type != cliSmokeResultType {
		t.Errorf("result type = %q, want %q", res.Type, cliSmokeResultType)
	}
	if res.RefreshID != "refresh-7" || res.ID != "cmd-1" {
		t.Errorf("result is not correlated to its command: %+v", res)
	}
	if res.AgentID != "agent-9" {
		t.Errorf("agentId = %q, want the configured agent", res.AgentID)
	}
	if res.Status != "success" || res.Smoke == nil || !res.Smoke.MarkerMatched {
		t.Fatalf("healthy smoke should publish a success verdict: %+v", res)
	}
	if res.Output != "" {
		t.Errorf("smoke result must carry no CLI output, got %q", res.Output)
	}
	if *calls != 1 {
		t.Errorf("handler spent %d turns, want 1", *calls)
	}
}

func TestHandleCLISmokeCommand_SecondSmokeInCooldownSpawnsNothing(t *testing.T) {
	smokeEnv(t)
	path := stubClaudeBinary(t)
	stubSmokePath(t, path)
	seedProbeVersion(t, path, "2.1.251 (Claude Code)")
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		return successEnvelope(markerFromPrompt(prompt)), nil, nil
	})
	published := capturePublishes(t)

	cfg := &Config{AgentID: "agent-9"}
	for i := 0; i < 3; i++ {
		if err := handleCLISmokeCommand(context.Background(), nil, smokeCommand("claudeCode"), cfg); err != nil {
			t.Fatalf("handler %d returned %v", i, err)
		}
	}

	// Every command still gets its own correlated result — the cooldown limits
	// quota spend, never the reply the backend is waiting on.
	if len(*published) != 3 {
		t.Fatalf("published %d results for 3 commands, want 3", len(*published))
	}
	if *calls != 1 {
		t.Fatalf("cooldown let %d inference turns through, want 1", *calls)
	}
	for i, res := range *published {
		if res.Smoke == nil || res.Status != "success" {
			t.Fatalf("result %d is not the replayed verdict: %+v", i, res)
		}
	}
}

func TestHandleCLISmokeCommand_UnknownCliIDNeverExecutes(t *testing.T) {
	smokeEnv(t)
	stubSmokePath(t, "")
	stubAuthProbe(t, true, true)
	calls := stubSmokeExec(t, func(ctx context.Context, args []string, prompt string) ([]byte, []byte, error) {
		t.Fatal("unknown cliId must never spawn a child")
		return nil, nil, nil
	})
	published := capturePublishes(t)

	for _, cmd := range []commandMsg{smokeCommand("notACLI"), {ID: "cmd-2", Command: cliSmokeCommand}} {
		if err := handleCLISmokeCommand(context.Background(), nil, cmd, &Config{AgentID: "a"}); err != nil {
			t.Fatalf("handler returned %v", err)
		}
	}

	if len(*published) != 2 {
		t.Fatalf("published %d results, want 2", len(*published))
	}
	for i, res := range *published {
		if res.Smoke == nil || res.Smoke.ErrorCategory != cliUsageErrorProviderUnavailable {
			t.Fatalf("result %d = %+v, want provider_unavailable", i, res.Smoke)
		}
		if res.Status != "error" {
			t.Errorf("result %d status = %q, want error", i, res.Status)
		}
	}
	if *calls != 0 {
		t.Fatalf("exec seam ran %d times for unknown cliIds", *calls)
	}
}

func TestMakeCLISmokeResult_PublishesMetricsOnly(t *testing.T) {
	res := makeCLISmokeResult(smokeCommand("claudeCode"), &Config{AgentID: "agent-9"}, cliSmokeResult{
		CliID:         "claudeCode",
		Version:       "2.1.251 (Claude Code)",
		Status:        cliSmokeStatusFailed,
		ErrorCategory: cliUsageErrorProtocol,
		DurationMs:    1234,
		ArgvShapeID:   claudeArgvShapes[0].ID,
		Diagnostic:    cliSmokeDiagnosticFramingRejected,
	})

	payload, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, want := range []string{
		`"type":"__cli_smoke_result__"`, `"refreshId":"refresh-7"`,
		`"status":"error"`, `"errorCategory":"protocol"`, `"argvShapeId":`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("published payload missing %s: %s", want, text)
		}
	}
	// The published shape must have no room for prompt/marker/argv material.
	for _, banned := range []string{"AIEXPEDITE_CLI_SMOKE_OK_", "Reply with exactly", "mcpServers"} {
		if strings.Contains(text, banned) {
			t.Errorf("published payload leaked %q: %s", banned, text)
		}
	}
}

func TestCLISmokeCommandIsAllowlistedInternalCommand(t *testing.T) {
	// Registered next to __ping__ so the operational command is never gated by
	// the approval dialog if it ever reaches the execute path.
	if !strings.Contains(defaultAllowListContent, cliSmokeCommand) {
		t.Fatalf("%s missing from the internal-commands allowlist", cliSmokeCommand)
	}
}
