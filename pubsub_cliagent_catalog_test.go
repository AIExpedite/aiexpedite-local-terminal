package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
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
	if len(res.Errors) != 1 || res.Errors[0].Provider != "_dispatch" || res.Errors[0].Message != "payload too large" {
		t.Fatalf("Errors=%#v, want dispatch payload-too-large error", res.Errors)
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
