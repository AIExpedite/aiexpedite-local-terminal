package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMutateAndSaveSerializesConfigWriters(t *testing.T) {
	cfg := DefaultConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	secondEntered := make(chan struct{})
	errs := make(chan error, 2)

	go func() {
		errs <- cfg.MutateAndSave(path, func() {
			cfg.AgentID = "registered-agent"
			close(firstEntered)
			<-releaseFirst
		})
	}()
	<-firstEntered
	go func() {
		close(secondStarted)
		errs <- cfg.MutateAndSave(path, func() {
			close(secondEntered)
			cfg.PendingUpdateAttemptID = "attempt-1"
		})
	}()
	<-secondStarted

	select {
	case <-secondEntered:
		t.Fatal("second config mutation entered before the first mutation and save completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AgentID != "registered-agent" || loaded.PendingUpdateAttemptID != "attempt-1" {
		t.Fatalf("serialized saves lost a field: agent=%q attempt=%q", loaded.AgentID, loaded.PendingUpdateAttemptID)
	}
}

func TestMutateAndSaveRollbackLeavesOriginalFileOnReplaceFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProjectID = "original"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	previousReplace := configAtomicReplace
	configAtomicReplace = func(_, _ string) error { return errors.New("replace failed") }
	t.Cleanup(func() { configAtomicReplace = previousReplace })

	err = cfg.MutateAndSaveRollback(path, func() {
		cfg.ProjectID = "new"
	}, func() {
		cfg.ProjectID = "original"
	})
	if err == nil {
		t.Fatal("expected replacement failure")
	}
	if cfg.ProjectID != "original" {
		t.Fatalf("in-memory config was not rolled back: %q", cfg.ProjectID)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("failed atomic replacement modified the existing config file")
	}
}

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

// disable_claude_usage_probe must default to false (the probe is what keeps the
// Claude utilization card advancing after a headless run) and round-trip through
// a config save/load so an operator's opt-out survives a restart.
func TestConfig_DisableClaudeUsageProbeRoundTrips(t *testing.T) {
	if DefaultConfig().DisableClaudeUsageProbe {
		t.Error("disable_claude_usage_probe must default to false")
	}

	path := filepath.Join(t.TempDir(), "config.json")
	cfg := DefaultConfig()
	if err := cfg.MutateAndSave(path, func() {
		cfg.DisableClaudeUsageProbe = true
	}); err != nil {
		t.Fatalf("MutateAndSave: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"disable_claude_usage_probe": true`)) {
		t.Errorf("saved config should carry the flag under its documented key:\n%s", raw)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.DisableClaudeUsageProbe {
		t.Error("disable_claude_usage_probe did not survive a save/load round-trip")
	}

	// Absent from an older config file: the default (probe enabled) applies.
	legacy := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"agentId":"a1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	older, err := LoadConfig(legacy)
	if err != nil {
		t.Fatalf("LoadConfig(legacy): %v", err)
	}
	if older.DisableClaudeUsageProbe {
		t.Error("a config written before the flag existed must not read as opted out")
	}
}
