package main

import "testing"

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
