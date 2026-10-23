package main

import (
	"encoding/json"
	"testing"
)

// The relay protocol flags are facts about this BUILD, not about the
// hardware, so a failed CPU or memory probe must never take them down with
// the concurrency hints (capabilitiesInfo). Before the relay the struct was
// allocated only when both probes succeeded, which would have made such a
// machine refuse every relay with DEVICE_UPDATE_REQUIRED.

func TestCapabilities_ReportBothRelayFlags(t *testing.T) {
	caps := newMachineCapabilities()
	applyConcurrencyHints(caps, &cpuInfo{Cores: 8, Threads: 16}, &memoryInfo{TotalGB: 32})

	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["relayTurnInbox"] != true || got["relayAckWatermark"] != true {
		t.Fatalf("capabilities JSON = %s, want relayTurnInbox and relayAckWatermark true", raw)
	}
	if got["recommendedConcurrentTests"] != float64(16) || got["recommendedConcurrentBuilds"] != float64(8) {
		t.Fatalf("capabilities JSON = %s, want the concurrency hints merged in", raw)
	}
}

func TestCapabilities_FailedHardwareProbeKeepsRelayFlags(t *testing.T) {
	cases := []struct {
		name string
		cpu  *cpuInfo
		mem  *memoryInfo
	}{
		{"cpu probe failed", nil, &memoryInfo{TotalGB: 16}},
		{"memory probe failed", &cpuInfo{Cores: 4, Threads: 8}, nil},
		{"both failed", nil, nil},
		{"memory reported zero", &cpuInfo{Cores: 4, Threads: 8}, &memoryInfo{TotalGB: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := newMachineCapabilities()
			applyConcurrencyHints(caps, tc.cpu, tc.mem)
			if !caps.RelayTurnInbox || !caps.RelayAckWatermark {
				t.Fatalf("caps = %+v, want both relay flags true", *caps)
			}
			// The load / presence sampler is a build fact as well: a machine
			// whose CPU probe failed still takes load samples, and the server
			// must not read a missing flag as "this app cannot report load".
			if !caps.LoadReport {
				t.Fatalf("caps = %+v, want the load-report flag true", *caps)
			}
			if caps.RecommendedConcurrentTests != 0 || caps.RecommendedConcurrentBuilds != 0 {
				t.Fatalf("caps = %+v, want no concurrency hints without both probes", *caps)
			}
			if caps.RelayFollowUps == nil || !caps.RelayFollowUps.AttemptExpiry {
				t.Fatalf("caps = %+v, want the follow-up ledger reported", *caps)
			}
			caps.RelayFollowUps = nil // its ledgerId is per machine
			// WhatsApp is a build fact too, but its secureStorage half depends
			// on the host's credential store, so only its key set is pinned.
			if caps.WhatsApp == nil || !caps.WhatsApp.Supported {
				t.Fatalf("caps = %+v, want the WhatsApp capability reported", *caps)
			}
			waRaw, _ := json.Marshal(&whatsAppCapability{Supported: true, SecureStorage: caps.WhatsApp.SecureStorage})
			raw, _ := json.Marshal(caps)
			// The literal IS the assertion: this pins the exact wire shape, so
			// adding a capability has to be a deliberate edit here rather than
			// something that rides along silently. `signedExecutionId` joined it
			// with the signed-executionId device half, `whatsapp` with the
			// WhatsApp bridge.
			if string(raw) != `{"relayTurnInbox":true,"loadReport":true,"relayAckWatermark":true,"relayPermissionPrompts":true,"readOnlySessions":true,"worktreeBuiltIns":true,"signedExecutionId":true,"whatsapp":`+string(waRaw)+`}` {
				t.Fatalf("capabilities JSON = %s", raw)
			}
		})
	}
}

// Registration (POST /auth/token) lifts `capabilities` onto the agent doc;
// the relay flags must ride on it even when the gather had no CPU/memory.
func TestCapabilities_RegistrationCarriesRelayFlags(t *testing.T) {
	isolateMachineInfo(t)
	mi := baselineMachineInfo()
	mi.CPU = nil // a failed CPU probe
	mi.Capabilities = newMachineCapabilities()
	applyConcurrencyHints(mi.Capabilities, mi.CPU, mi.Memory)
	storeMachineInfo(mi)

	rec, srv := newTokenRequestRecorder(t)
	ts := NewWIFTokenSource(&Config{AgentID: "agent-1", CommandSecret: "secret", TokenEndpoint: srv.URL})
	if _, err := ts.getOIDCToken(); err != nil {
		t.Fatalf("getOIDCToken failed: %v", err)
	}
	payloads := rec.snapshot()
	if len(payloads) == 0 {
		t.Fatal("no token request recorded")
	}
	caps, ok := payloads[0]["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("registration payload lacks capabilities: %v", payloads[0])
	}
	if caps["relayTurnInbox"] != true || caps["relayAckWatermark"] != true {
		t.Fatalf("registration capabilities = %v, want both relay flags true", caps)
	}
}
