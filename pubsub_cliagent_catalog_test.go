package main

import (
	"context"
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
