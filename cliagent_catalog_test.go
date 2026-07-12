package main

import "testing"

func TestNormalizeCLIAgentCatalog_DropsRemovedGeminiEntries(t *testing.T) {
	entries := []cliAgentCatalogEntry{
		{ID: "geminiCli", DisplayName: "Gemini", Command: "gemini"},
		{ID: "legacyGemini", DisplayName: "Legacy Gemini", Command: "gemini --model x"},
		{ID: "antigravity", DisplayName: "Antigravity", Command: "agy"},
	}

	got := normalizeCLIAgentCatalog(entries)

	if len(got) != 1 {
		t.Fatalf("normalized catalog=%#v, want only antigravity", got)
	}
	if got[0].ID != "antigravity" {
		t.Fatalf("normalized entry ID=%q, want antigravity", got[0].ID)
	}
}

func TestSetCLIAgentCatalog_FiltersPersistedGemini(t *testing.T) {
	SetCLIAgentCatalog([]cliAgentCatalogEntry{
		{ID: "geminiCli", DisplayName: "Gemini", Command: "gemini"},
		{ID: "claudeCode", DisplayName: "Claude Code", Command: "claude"},
	})
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	for _, entry := range activeCLIAgentCatalog() {
		if isRemovedCLIAgent(entry.ID, entry.Command) {
			t.Fatalf("active catalog still exposes removed agent: %#v", entry)
		}
	}
}
