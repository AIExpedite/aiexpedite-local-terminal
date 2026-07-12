package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// cliAgentCatalogEntry is the local projection of the database-backed
// cliAgent catalog. Only the fields the local terminal needs are modeled; JSON
// still ignores future metadata the backend/frontend may add.
type cliAgentCatalogEntry struct {
	ID            string          `json:"id,omitempty"`
	Provider      string          `json:"provider,omitempty"`
	DisplayName   string          `json:"displayName,omitempty"`
	Name          string          `json:"name,omitempty"`
	DisplayOrder  int             `json:"displayOrder,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Command       string          `json:"command,omitempty"`
	DetectionKeys []string        `json:"detectionKeys,omitempty"`
	Capabilities  json.RawMessage `json:"capabilities,omitempty"`
}

var (
	cliAgentCatalogMu         sync.RWMutex
	cliAgentCatalogConfigured bool
	configuredCLIAgentCatalog []cliAgentCatalogEntry
)

func defaultCLIAgentCatalog() []cliAgentCatalogEntry {
	return []cliAgentCatalogEntry{
		{ID: "antigravity", DisplayName: "Antigravity", DisplayOrder: 10, Command: "agy", DetectionKeys: []string{"antigravity", "agy"}},
		{ID: "claudeCode", DisplayName: "Claude Code", DisplayOrder: 20, Command: "claude", DetectionKeys: []string{"claudeCode", "claude"}},
		{ID: "codex", DisplayName: "Codex", DisplayOrder: 30, Command: "codex", DetectionKeys: []string{"codex"}},
		{ID: "grok", DisplayName: "Grok Build", DisplayOrder: 50, Command: "grok", DetectionKeys: []string{"grok", "grokBuild"}},
	}
}

func SetCLIAgentCatalog(entries []cliAgentCatalogEntry) {
	normalized := normalizeCLIAgentCatalog(entries)
	cliAgentCatalogMu.Lock()
	cliAgentCatalogConfigured = entries != nil
	configuredCLIAgentCatalog = normalized
	cliAgentCatalogMu.Unlock()
}

func activeCLIAgentCatalog() []cliAgentCatalogEntry {
	cliAgentCatalogMu.RLock()
	if cliAgentCatalogConfigured {
		out := cloneCLIAgentCatalog(configuredCLIAgentCatalog)
		cliAgentCatalogMu.RUnlock()
		return out
	}
	cliAgentCatalogMu.RUnlock()
	return normalizeCLIAgentCatalog(defaultCLIAgentCatalog())
}

func normalizeCLIAgentCatalog(entries []cliAgentCatalogEntry) []cliAgentCatalogEntry {
	out := make([]cliAgentCatalogEntry, 0, len(entries))
	seen := map[string]bool{}
	for _, raw := range entries {
		if raw.Enabled != nil && !*raw.Enabled {
			continue
		}
		id := firstNonEmpty(raw.ID, raw.Provider)
		if id == "" {
			continue
		}
		command := firstCommandToken(raw.Command)
		if command == "" {
			continue
		}
		key := strings.ToLower(id)
		if seen[key] {
			continue
		}
		seen[key] = true

		normalized := raw
		normalized.ID = id
		normalized.Command = command
		normalized.DisplayName = firstNonEmpty(raw.DisplayName, raw.Name, id)
		normalized.DetectionKeys = uniqueStrings(append(append([]string{}, raw.DetectionKeys...), id, command))
		out = append(out, normalized)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DisplayOrder != out[j].DisplayOrder {
			return out[i].DisplayOrder < out[j].DisplayOrder
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func cloneCLIAgentCatalog(entries []cliAgentCatalogEntry) []cliAgentCatalogEntry {
	if entries == nil {
		return nil
	}
	out := make([]cliAgentCatalogEntry, len(entries))
	for i, entry := range entries {
		out[i] = entry
		if entry.DetectionKeys != nil {
			out[i].DetectionKeys = append([]string{}, entry.DetectionKeys...)
		}
		if entry.Capabilities != nil {
			out[i].Capabilities = append(json.RawMessage(nil), entry.Capabilities...)
		}
	}
	return out
}

func firstCommandToken(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func cliAgentCatalogParserKey(agent cliAgentCatalogEntry) string {
	caps := parseCLIAgentCapabilities(agent.Capabilities)
	if v, ok := caps["parserKey"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if nested, ok := caps["utilization"].(map[string]any); ok {
		if v, ok := nested["parserKey"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return agent.ID
}

func cliAgentCatalogSupportsUtilization(agent cliAgentCatalogEntry) bool {
	caps := parseCLIAgentCapabilities(agent.Capabilities)
	value, ok := caps["utilization"]
	if !ok {
		return true
	}
	if enabled, ok := value.(bool); ok {
		return enabled
	}
	if nested, ok := value.(map[string]any); ok {
		if enabled, ok := nested["enabled"].(bool); ok {
			return enabled
		}
	}
	return true
}

func parseCLIAgentCapabilities(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var caps map[string]any
	if err := json.Unmarshal(raw, &caps); err != nil {
		return nil
	}
	return caps
}
