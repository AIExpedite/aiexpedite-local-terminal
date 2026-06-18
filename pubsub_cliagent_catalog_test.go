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
