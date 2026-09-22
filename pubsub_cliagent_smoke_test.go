package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	grokCalls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		t.Fatal("unknown cliId must never spawn a grok child")
		return nil, nil, nil
	})
	published := capturePublishes(t)

	codexCalls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		t.Fatal("unknown cliId must never spawn a codex child")
		return nil, nil, nil
	})
	// grok and codex are KNOWN providers now — they must not be in this list.
	for _, known := range []string{"claudeCode", "grok", "codex"} {
		if _, ok := cliSmokeProviders[known]; !ok {
			t.Fatalf("%s is missing from cliSmokeProviders", known)
		}
	}

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
	if *calls != 0 || *grokCalls != 0 || *codexCalls != 0 {
		t.Fatalf("exec seams ran %d/%d/%d times for unknown cliIds", *calls, *grokCalls, *codexCalls)
	}
}

// grok on the same signed channel: one correlated result, a marker verdict,
// and — because the probe merges the child's billing record into the
// persistent home — a newer Grok observation for the next signed refresh.
func TestHandleCLISmokeCommand_GrokPublishesMarkerVerdictAndMergesBilling(t *testing.T) {
	persistent := grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.13")
	calls, _ := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		writeGrokSmokeBillingRecord(t, launch, time.Now())
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})
	published := capturePublishes(t)

	cfg := &Config{AgentID: "agent-9"}
	if err := handleCLISmokeCommand(context.Background(), nil, smokeCommand("grok"), cfg); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}

	if len(*published) != 1 {
		t.Fatalf("published %d messages, want exactly 1", len(*published))
	}
	res := (*published)[0]
	if res.Type != cliSmokeResultType || res.RefreshID != "refresh-7" || res.ID != "cmd-1" {
		t.Fatalf("result is not correlated to its command: %+v", res)
	}
	if res.Status != "success" || res.Smoke == nil || !res.Smoke.MarkerMatched || res.Smoke.CliID != "grok" {
		t.Fatalf("healthy grok should publish a success verdict: %+v", res.Smoke)
	}
	if res.Smoke.ArgvShapeID != grokSmokeArgvShapes[0].ID {
		t.Errorf("argvShapeId = %q, want the canonical rung", res.Smoke.ArgvShapeID)
	}
	if res.Output != "" {
		t.Errorf("smoke result must carry no CLI output, got %q", res.Output)
	}
	if *calls != 1 {
		t.Errorf("handler spent %d turns, want 1", *calls)
	}
	if snap, ok := readGrokBillingSnapshot(persistent, grokIdentityCandidates(persistent)); !ok || snap.SubscriptionTier != "SuperGrok" {
		t.Fatalf("the smoke's billing record was not merged into the persistent home: ok=%t snap=%+v", ok, snap)
	}
}

func TestMakeCLISmokeResult_PublishesMetricsOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		smoke cliSmokeResult
	}{
		{"claudeCode", cliSmokeResult{
			CliID:         "claudeCode",
			Version:       "2.1.251 (Claude Code)",
			Status:        cliSmokeStatusFailed,
			ErrorCategory: cliUsageErrorProtocol,
			DurationMs:    1234,
			ArgvShapeID:   claudeArgvShapes[0].ID,
			Diagnostic:    cliSmokeDiagnosticFramingRejected,
		}},
		{"codex", cliSmokeResult{
			CliID:         "codex",
			Version:       codexSmokeTestVersion,
			Status:        cliSmokeStatusFailed,
			ErrorCategory: cliUsageErrorProtocol,
			DurationMs:    99,
			ArgvShapeID:   codexSmokeArgvShapes[0].ID,
			Diagnostic:    cliSmokeDiagnosticNoEnvelope,
		}},
		{"grok", cliSmokeResult{
			CliID:         "grok",
			Version:       "grok 1.0.13",
			Status:        cliSmokeStatusFailed,
			ErrorCategory: cliUsageErrorProtocol,
			DurationMs:    321,
			ArgvShapeID:   grokSmokeArgvShapes[0].ID,
			Diagnostic:    cliSmokeDiagnosticFlagRejected,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := makeCLISmokeResult(smokeCommand(tc.name), &Config{AgentID: "agent-9"}, tc.smoke)

			payload, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			text := string(payload)
			for _, want := range []string{
				`"type":"__cli_smoke_result__"`, `"refreshId":"refresh-7"`, `"cliId":"` + tc.name + `"`,
				`"status":"error"`, `"errorCategory":"protocol"`, `"argvShapeId":`, `"diagnostic":"` + tc.smoke.Diagnostic + `"`,
			} {
				if !strings.Contains(text, want) {
					t.Errorf("published payload missing %s: %s", want, text)
				}
			}
			// The published shape must have no room for prompt/marker/argv/config
			// material — for either provider.
			for _, banned := range []string{
				claudeSmokeMarkerPrefix, grokSmokeMarkerPrefix, codexSmokeMarkerPrefix, "Reply with exactly",
				"--output-last-message", codexSmokeLastMessageName, "read-only",
				grokMaintenanceSmokePromptPrefix, "mcpServers", "--tools", "--print",
				"--prompt-file", "auth.json", "config.toml", "GROK_HOME",
			} {
				if strings.Contains(text, banned) {
					t.Errorf("published payload leaked %q: %s", banned, text)
				}
			}
		})
	}
}

func TestCLISmokeCommandIsAllowlistedInternalCommand(t *testing.T) {
	// Registered next to __ping__ so the operational command is never gated by
	// the approval dialog if it ever reaches the execute path.
	if !strings.Contains(defaultAllowListContent, cliSmokeCommand) {
		t.Fatalf("%s missing from the internal-commands allowlist", cliSmokeCommand)
	}
}

// codex on the same signed channel: `Args: ["codex"]` reaches the Codex
// provider and publishes exactly one correlated result with Output empty.
func TestHandleCLISmokeCommand_CodexPublishesOneCorrelatedResult(t *testing.T) {
	codexSmokeEnv(t)
	path := stubCodexBinary(t)
	stubCodexSmokePath(t, path)
	calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return []byte(`{"jsonrpc":"2.0","method":"codex/event/agent_message","params":{"msg":{"type":"agent_message","message":"` +
			codexMarkerFromLaunch(t, launch) + `"}}}` + "\n"), nil, nil
	})
	published := capturePublishes(t)

	if err := handleCLISmokeCommand(context.Background(), nil, smokeCommand("codex"), &Config{AgentID: "agent-9"}); err != nil {
		t.Fatalf("handler returned %v", err)
	}

	if len(*published) != 1 {
		t.Fatalf("published %d messages, want exactly 1", len(*published))
	}
	res := (*published)[0]
	if res.Type != cliSmokeResultType || res.RefreshID != "refresh-7" || res.ID != "cmd-1" {
		t.Fatalf("result is not a correlated smoke result: %+v", res)
	}
	if res.Status != "success" || res.Smoke == nil || res.Smoke.CliID != "codex" || !res.Smoke.MarkerMatched {
		t.Fatalf("post-update codex answer should publish a success verdict: %+v", res.Smoke)
	}
	if res.Output != "" {
		t.Errorf("smoke result must carry no CLI output, got %q", res.Output)
	}
	if *calls != 1 {
		t.Errorf("handler spent %d turns, want 1", *calls)
	}
}

// Redaction: credential, prompt and raw-config sentinels planted in the
// child's stdout/stderr, the env and CODEX_HOME's config must appear in
// neither the published result, the published usage payload, nor the device
// log line.
func TestHandleCLISmokeCommand_CodexLeaksNoCredentialPromptOrConfig(t *testing.T) {
	const (
		credSentinel   = "sk-CODEX-CREDENTIAL-SENTINEL-7f3a"
		promptSentinel = "PROMPT-SENTINEL-EXACTLY-THIS"
		configSentinel = "CONFIG-SENTINEL-model_provider"
	)
	home, _ := codexSmokeEnv(t)
	t.Setenv("OPENAI_API_KEY", credSentinel)
	helperWriteJSON(t, filepath.Join(home, "auth.json"), map[string]any{
		"email": "dev@example.com", "OPENAI_API_KEY": credSentinel,
	})
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`model_provider = "`+configSentinel+`"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := stubCodexBinary(t)
	stubCodexSmokePath(t, path)
	stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		// A chatty answer echoing everything it can see, so the verdict is a
		// failure and the device log line is written.
		stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"` + promptSentinel + " " + credSentinel + " " + configSentinel + `"}}` + "\n" +
			`{"type":"turn.completed"}` + "\n"
		stderr := "warning: " + credSentinel + " " + configSentinel + " " + launch.Prompt + " " + launch.LastMessageFile
		return []byte(stdout), []byte(stderr), nil
	})
	published := capturePublishes(t)

	logged := captureStdout(t, func() {
		if err := handleCLISmokeCommand(context.Background(), nil, smokeCommand("codex"), &Config{AgentID: "agent-9"}); err != nil {
			t.Fatalf("handler returned %v", err)
		}
	})
	if len(*published) != 1 || (*published)[0].Smoke == nil || (*published)[0].Smoke.Diagnostic != cliSmokeDiagnosticMarkerMismatch {
		t.Fatalf("published = %+v, want one marker_mismatch verdict", *published)
	}
	payload, err := json.Marshal((*published)[0])
	if err != nil {
		t.Fatal(err)
	}
	usage, _ := codexUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{Detected: true, Path: path, Version: codexSmokeTestVersion}, time.Now())
	usagePayload, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for surface, text := range map[string]string{
		"published result": string(payload),
		"usage payload":    string(usagePayload),
		"device log":       logged,
	} {
		for _, banned := range []string{credSentinel, promptSentinel, configSentinel, codexSmokeMarkerPrefix, "Reply with exactly", codexSmokeLastMessageName} {
			if strings.Contains(text, banned) {
				t.Errorf("%s leaked %q: %s", surface, banned, text)
			}
		}
	}
	if !strings.Contains(logged, "diagnostic=marker_mismatch") {
		t.Errorf("the failure should still reach the device log as closed values: %q", logged)
	}
}
