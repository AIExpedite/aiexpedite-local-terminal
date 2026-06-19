package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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

func TestUpdateCLIAgentCatalog_CanMarshalConcurrently(t *testing.T) {
	cfg := &Config{}
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	firstCatalog := []cliAgentCatalogEntry{
		{ID: "futureAgent", DisplayName: "Future Agent", Command: "future-agent"},
	}
	secondCatalog := []cliAgentCatalogEntry{
		{ID: "grok", DisplayName: "Grok Build", Command: "grok"},
		{ID: "codex", DisplayName: "Codex", Command: "codex"},
	}

	start := make(chan struct{})
	errCh := make(chan error, 16)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				if (i+j)%2 == 0 {
					cfg.UpdateCLIAgentCatalog(firstCatalog)
				} else {
					cfg.UpdateCLIAgentCatalog(secondCatalog)
				}
			}
		}(i)
	}

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				if _, err := json.Marshal(cfg); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("Marshal failed during concurrent catalog update: %v", err)
		}
	}
}
