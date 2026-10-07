package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

func TestHandleCLIUsageRefreshCommand_PersistsAndRefreshesChangedCatalog(t *testing.T) {
	oldBaseDir := baseDir
	baseDir = t.TempDir()
	t.Cleanup(func() { baseDir = oldBaseDir })

	SetCLIAgentCatalog(nil)
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	offlineMutex.Lock()
	originalOffline := isOffline
	isOffline = true
	offlineMutex.Unlock()
	t.Cleanup(func() {
		offlineMutex.Lock()
		isOffline = originalOffline
		offlineMutex.Unlock()
	})

	originalRefresh := refreshMachineInfoAfterCatalogUpdate
	var refreshes atomic.Int32
	refreshMachineInfoAfterCatalogUpdate = func() {
		refreshes.Add(1)
	}
	t.Cleanup(func() { refreshMachineInfoAfterCatalogUpdate = originalRefresh })

	cfg := &Config{AgentID: "agent-1", CommandSecret: "secret"}
	cmd := commandMsg{
		ID:        "cmd-1",
		RefreshID: "refresh-1",
		CliAgentCatalog: []cliAgentCatalogEntry{
			{
				ID:          "futureAgent",
				DisplayName: "Future Agent",
				Command:     "future-agent",
			},
		},
	}

	if err := handleCLIUsageRefreshCommand(context.Background(), nil, cmd, cfg); err != nil {
		t.Fatalf("handleCLIUsageRefreshCommand failed: %v", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshMachineInfoAfterCatalogUpdate calls=%d, want 1", got)
	}

	loaded, err := LoadConfig(ConfigPath())
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	catalog := loaded.cliAgentCatalogSnapshot()
	if len(catalog) != 1 || catalog[0].ID != "futureAgent" {
		t.Fatalf("persisted catalog=%#v, want futureAgent", catalog)
	}
}

func TestPubSubMessageSizeLimit_AllowsLargerCLIUsageRefreshCatalogs(t *testing.T) {
	if got := pubSubMessageSizeLimit(commandMsg{Command: "__cli_usage_refresh__"}); got != maxOIDCTokenResponseBytes {
		t.Fatalf("refresh message size limit=%d, want auth catalog cap %d", got, maxOIDCTokenResponseBytes)
	}
	if got := pubSubMessageSizeLimit(commandMsg{Command: "echo"}); got != maxPubSubCommandMessageBytes {
		t.Fatalf("normal command message size limit=%d, want %d", got, maxPubSubCommandMessageBytes)
	}
	if maxPubSubCatalogMessageBytes <= maxPubSubCommandMessageBytes {
		t.Fatalf("catalog cap=%d should exceed normal command cap=%d", maxPubSubCatalogMessageBytes, maxPubSubCommandMessageBytes)
	}
}

func TestMakeCLIUsageRefreshFailureResult_CarriesRefreshMetadata(t *testing.T) {
	res := makeCLIUsageRefreshFailureResult(commandMsg{
		ID:          "cmd-1",
		WorkspaceID: "workspace-1",
		UID:         "user-1",
		AgentID:     "payload-agent",
		RefreshID:   "refresh-1",
	}, &Config{AgentID: "config-agent"}, "payload too large")

	if res.Type != "__cli_usage_refresh_result__" {
		t.Fatalf("Type=%q, want __cli_usage_refresh_result__", res.Type)
	}
	if res.RefreshID != "refresh-1" {
		t.Fatalf("RefreshID=%q, want refresh-1", res.RefreshID)
	}
	if res.AgentID != "config-agent" {
		t.Fatalf("AgentID=%q, want config-agent", res.AgentID)
	}
	if res.Success == nil || *res.Success {
		t.Fatalf("Success=%v, want false pointer", res.Success)
	}
	if len(res.Errors) != 1 || res.Errors[0].Provider != "_dispatch" || res.Errors[0].ErrorCategory != cliUsageErrorInternal || res.Errors[0].Message != "" {
		t.Fatalf("Errors=%#v, want categorized dispatch error", res.Errors)
	}
}

func TestSignedCLIUsageFailureRequiresVerifiedChallenge(t *testing.T) {
	cfg := &Config{AgentID: "agent-1", CommandSecret: "secret"}
	cmd := commandMsg{ID: "cmd-1", Command: "__cli_usage_refresh__", Args: []string{}, Ts: 123, AgentID: "agent-1", RefreshID: "refresh-1"}
	if canPublishSignedCLIUsageFailure(cmd, cfg) {
		t.Fatal("unsigned challenge must not receive a signed failure receipt")
	}
	payload := signaturePayload{ID: cmd.ID, Command: cmd.Command, Args: cmd.Args, Ts: cmd.Ts, RefreshID: cmd.RefreshID}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Signature = generateHMAC(string(data), cfg.CommandSecret)
	if !canPublishSignedCLIUsageFailure(cmd, cfg) {
		t.Fatal("valid signed challenge should permit a signed failure receipt")
	}
	res := makeCLIUsageRefreshFailureResult(cmd, cfg, "usage result rejected")
	if res.ReceiptVersion != 1 || res.Receipt == "" || res.ChallengeTs != cmd.Ts {
		t.Fatalf("missing signed failure receipt metadata: %#v", res)
	}
}

func TestPrepareCLIUsageRefreshResult_CachesOnlyValidatedNormalizedSnapshot(t *testing.T) {
	machineInfoMu.Lock()
	originalCache := machineInfoCache
	machineInfoCache = &MachineInfo{CliAgents: []cliAgentUsage{{Provider: "previous"}}}
	machineInfoMu.Unlock()
	t.Cleanup(func() {
		machineInfoMu.Lock()
		machineInfoCache = originalCache
		machineInfoMu.Unlock()
	})

	rejected := []cliAgentUsage{{Provider: "invalid", Path: string(make([]byte, 2049))}}
	if _, _, _, err := prepareCLIUsageRefreshResult("secret", "refresh-1", 1, true, rejected, nil); err == nil {
		t.Fatal("oversized provider snapshot should be rejected")
	}
	if got := GetMachineInfo().CliAgents; len(got) != 1 || got[0].Provider != "previous" {
		t.Fatalf("rejected snapshot replaced cache: %#v", got)
	}

	valid := []cliAgentUsage{{Provider: "zeta"}, {Provider: "alpha"}}
	_, normalized, _, err := prepareCLIUsageRefreshResult("secret", "refresh-2", 2, true, valid, nil)
	if err != nil {
		t.Fatalf("valid provider snapshot rejected: %v", err)
	}
	got := GetMachineInfo().CliAgents
	if len(got) != 2 || got[0].Provider != "alpha" || got[1].Provider != "zeta" {
		t.Fatalf("cache was not replaced with normalized snapshot: %#v", got)
	}
	if &got[0] != &normalized[0] {
		t.Fatal("cache does not contain the exact normalized snapshot returned for publishing")
	}
}

// The force reason a __cli_usage_refresh__ carries: a click is `click`, an
// automatic refresh with a debt owed is `debt`, and one with nothing owed is
// not forced at all.
func TestClaudeUsageRefreshForceReason(t *testing.T) {
	resetClaudeUsageProbeGate()
	t.Cleanup(resetClaudeUsageProbeGate)
	automatic := commandMsg{Command: "__cli_usage_refresh__"}
	if got := claudeUsageRefreshForceReason(automatic); got != claudeForceNone {
		t.Errorf("automatic refresh, nothing owed: reason=%v, want none", got)
	}
	claudeUsageProbe.recordOwed(time.Now())
	if got := claudeUsageRefreshForceReason(automatic); got != claudeForceDebt {
		t.Errorf("automatic refresh, debt owed: reason=%v, want debt", got)
	}
	click := commandMsg{Command: "__cli_usage_refresh__", Args: []string{cliUsageLiveProbeArg}}
	if got := claudeUsageRefreshForceReason(click); got != claudeForceClick {
		t.Errorf("click: reason=%v, want click (debt owed or not)", got)
	}
}

// refreshWithDiscoveredClaudeGeneration stands in for a gather whose Claude
// read discovers an external (status-line) commit, and returns the usage the
// receipt signs.
func refreshWithDiscoveredClaudeGeneration(t *testing.T, g cliUsageGeneration) {
	t.Helper()
	prev := gatherCLIUsageForRefresh
	gatherCLIUsageForRefresh = func(ctx context.Context) ([]cliAgentUsage, []cliAgentUsageError) {
		observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
		return []cliAgentUsage{{Provider: claudeUsageProvider, CollectedAt: "now", UsageGeneration: &g}}, nil
	}
	t.Cleanup(func() { gatherCLIUsageForRefresh = prev })
}

func stubRefreshPublish(t *testing.T, fail func(resultMsg) bool) *[]resultMsg {
	t.Helper()
	var mu sync.Mutex
	var published []resultMsg
	prev := publishMsg
	publishMsg = func(_ context.Context, _ *pubsub.Publisher, res resultMsg) error {
		mu.Lock()
		defer mu.Unlock()
		if fail != nil && fail(res) {
			return errors.New("publish unavailable")
		}
		published = append(published, res)
		return nil
	}
	t.Cleanup(func() { publishMsg = prev })
	return &published
}

// A published receipt that carried a discovered generation suppresses the
// immediate hint and leaves exactly one confirmation beyond the backend
// cooldown — and the refresh result itself is unchanged.
func TestHandleCLIUsageRefreshCommand_PublishedReceiptLeavesOneConfirmation(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	refreshWithDiscoveredClaudeGeneration(t, gen(301, 4))
	published := stubRefreshPublish(t, nil)

	start := time.Now()
	if err := handleCLIUsageRefreshCommand(context.Background(), nil, commandMsg{ID: "c1", RefreshID: "r1", Ts: 1}, cfg); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 1 || (*published)[0].Receipt == "" || len((*published)[0].CliAgents) != 1 {
		t.Fatalf("published = %+v, want one signed result", *published)
	}
	hints := waitHints(t, rec, 1, 3*cliUsageHintSpacing)
	if len(hints) != 1 || hints[0].hint.Provider != claudeUsageProvider || hints[0].hint.Generation != 4 {
		t.Fatalf("hints = %+v, want exactly one confirmation", hints)
	}
	if at := time.UnixMilli(hints[0].hint.Timestamp); at.Sub(start) < cliUsageHintSpacing-5*time.Millisecond {
		t.Fatalf("confirmation went %s after the refresh, want >= %s", at.Sub(start), cliUsageHintSpacing)
	}
}

// A refresh whose result could not be published releases the generation to
// the ordinary lifecycle: the backend never saw it, so it is hinted (and
// followed up) as usual.
func TestHandleCLIUsageRefreshCommand_FailedPublishReleasesTheReservation(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	refreshWithDiscoveredClaudeGeneration(t, gen(302, 1))
	stubRefreshPublish(t, func(resultMsg) bool { return true })

	if err := handleCLIUsageRefreshCommand(context.Background(), nil, commandMsg{ID: "c1", RefreshID: "r1", Ts: 1}, cfg); err == nil {
		t.Fatal("a failed publish must be reported so the command is redelivered")
	}
	hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing)
	if len(hints) != 2 || hints[0].hint.GenerationEpoch != 302 {
		t.Fatalf("hints = %+v, want the released generation's hint and follow-up", hints)
	}
}

// A receipt that cannot be signed publishes the redacted failure result and
// releases the reservation.
func TestHandleCLIUsageRefreshCommand_UnsignableReceiptReleasesTheReservation(t *testing.T) {
	rec, cfg := propagatorFixture(t)
	startCLIUsagePropagator(cfg)
	g := gen(303, 1)
	prev := gatherCLIUsageForRefresh
	gatherCLIUsageForRefresh = func(ctx context.Context) ([]cliAgentUsage, []cliAgentUsageError) {
		observeCLIUsageGeneration(ctx, claudeUsageProvider, &g)
		bad := cliUsageGeneration{Epoch: -1, Counter: 1}
		return []cliAgentUsage{{Provider: claudeUsageProvider, CollectedAt: "now", UsageGeneration: &bad}}, nil
	}
	t.Cleanup(func() { gatherCLIUsageForRefresh = prev })
	published := stubRefreshPublish(t, nil)

	if err := handleCLIUsageRefreshCommand(context.Background(), nil, commandMsg{ID: "c1", RefreshID: "r1", Ts: 1}, cfg); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 1 || (*published)[0].Success == nil || *(*published)[0].Success || len((*published)[0].CliAgents) != 0 {
		t.Fatalf("published = %+v, want the failure result", *published)
	}
	if hints := waitHints(t, rec, 2, 2*cliUsageHintSpacing); len(hints) != 2 || hints[0].hint.GenerationEpoch != 303 {
		t.Fatalf("hints = %+v, want the released generation's hint and follow-up", hints)
	}
}
