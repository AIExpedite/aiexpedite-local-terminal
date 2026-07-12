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

func TestNormalizeCLIAgentCatalog_DropsGeminiByCommandBasename(t *testing.T) {
	// Entries that name Gemini through a shim or path but do NOT use the
	// geminiCli id must still be filtered — otherwise gatherCLIAgents keeps
	// detecting and advertising a removed Gemini entry.
	entries := []cliAgentCatalogEntry{
		{ID: "customGemini", DisplayName: "Gemini", Command: "gemini.cmd"},
		{ID: "winGemini", DisplayName: "Gemini", Command: "gemini.exe --model x"},
		{ID: "pathGemini", DisplayName: "Gemini", Command: "/usr/local/bin/gemini"},
		{ID: "antigravity", DisplayName: "Antigravity", Command: "agy"},
	}

	got := normalizeCLIAgentCatalog(entries)

	if len(got) != 1 || got[0].ID != "antigravity" {
		t.Fatalf("normalized catalog=%#v, want only antigravity", got)
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
