package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGetOIDCToken_PersistsAndRefreshesChangedCLIAgentCatalog(t *testing.T) {
	oldBaseDir := baseDir
	baseDir = t.TempDir()
	t.Cleanup(func() { baseDir = oldBaseDir })

	SetCLIAgentCatalog(nil)
	t.Cleanup(func() { SetCLIAgentCatalog(nil) })

	originalRefresh := refreshMachineInfoAfterCatalogUpdate
	var refreshes atomic.Int32
	refreshMachineInfoAfterCatalogUpdate = func() {
		refreshes.Add(1)
	}
	t.Cleanup(func() { refreshMachineInfoAfterCatalogUpdate = originalRefresh })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s, want POST", r.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["agentId"] != "agent-1" {
			t.Fatalf("agentId=%v, want agent-1", payload["agentId"])
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id_token":   "oidc-token",
			"expires_in": 3600,
			"token_type": "Bearer",
			"cliAgentCatalog": []map[string]any{
				{
					"id":          "futureAgent",
					"displayName": "Future Agent",
					"command":     "future-agent",
				},
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{
		AgentID:       "agent-1",
		CommandSecret: "secret",
		TokenEndpoint: srv.URL,
	}

	token, err := NewWIFTokenSource(cfg).getOIDCToken()
	if err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	if token != "oidc-token" {
		t.Fatalf("token=%q, want oidc-token", token)
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
