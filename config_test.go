package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateCLIAgentCatalog_IgnoresMissingCatalog(t *testing.T) {
	cfg := &Config{}
	cfg.UpdateCLIAgentCatalog([]cliAgentCatalogEntry{
		{
			ID:          "futureAgent",
			DisplayName: "Future Agent",
			Command:     "future-agent",
		},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	cfg.UpdateCLIAgentCatalog(nil)

	agents := activeCLIAgentCatalog()
	if len(agents) != 1 {
		t.Fatalf("expected nil update to preserve existing catalog, got %#v", agents)
	}
	if agents[0].ID != "futureAgent" {
		t.Fatalf("active catalog ID=%q, want futureAgent", agents[0].ID)
	}
}

func TestUpdateCLIAgentCatalog_AppliesExplicitEmptyCatalog(t *testing.T) {
	cfg := &Config{}
	cfg.UpdateCLIAgentCatalog([]cliAgentCatalogEntry{
		{
			ID:          "futureAgent",
			DisplayName: "Future Agent",
			Command:     "future-agent",
		},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	cfg.UpdateCLIAgentCatalog([]cliAgentCatalogEntry{})

	if cfg.CliAgentCatalog == nil {
		t.Fatalf("expected explicit empty catalog to remain configured")
	}
	if agents := activeCLIAgentCatalog(); len(agents) != 0 {
		t.Fatalf("expected explicit empty catalog to suppress fallback agents, got %#v", agents)
	}
}

func TestSaveConfig_PersistsExplicitEmptyCLIAgentCatalog(t *testing.T) {
	cfg := &Config{CliAgentCatalog: []cliAgentCatalogEntry{}}
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Contains(data, []byte(`"cliAgentCatalog": []`)) {
		t.Fatalf("expected cliAgentCatalog empty array in saved config, got:\n%s", string(data))
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.CliAgentCatalog == nil {
		t.Fatalf("expected loaded explicit empty catalog to remain non-nil")
	}
	if agents := activeCLIAgentCatalog(); len(agents) != 0 {
		t.Fatalf("expected loaded explicit empty catalog to suppress fallback agents, got %#v", agents)
	}
}
