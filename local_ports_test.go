// Tests for the per-channel local-resource table (local_ports.go). The
// contract: two channels of the agent on one machine must never bind the
// same port or own the same tmux session, and a non-prod config left on the
// old shared ttyd port is moved onto its channel's port at load time.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalChannelDefaultsAreDisjoint(t *testing.T) {
	ports := map[int]string{}
	sessions := map[string]string{}
	for env, defaults := range localDefaultsByEnv {
		for _, port := range []int{defaults.ttydPort, defaults.identityPort} {
			if port <= 0 {
				t.Fatalf("%s: port %d must be positive", env, port)
			}
			if owner, taken := ports[port]; taken {
				t.Fatalf("port %d is claimed by both %s and %s", port, owner, env)
			}
			ports[port] = env
		}
		if owner, taken := sessions[defaults.tmuxSession]; taken {
			t.Fatalf("tmux session %q is claimed by both %s and %s", defaults.tmuxSession, owner, env)
		}
		sessions[defaults.tmuxSession] = env
	}
	for _, env := range []string{"prod", "dev", "stg", "beta"} {
		if _, ok := localDefaultsByEnv[env]; !ok {
			t.Fatalf("channel %s has no local defaults", env)
		}
	}
}

// prod's values are the installed base and the frontend's Open Terminal
// links; changing them is a migration, not a refactor.
func TestProdKeepsLegacyLocalDefaults(t *testing.T) {
	if got := defaultTtydPortFor("prod"); got != legacySharedTtydPort {
		t.Fatalf("prod ttyd port = %d, want %d", got, legacySharedTtydPort)
	}
	if got := tmuxSessionNameFor("prod"); got != "agent" {
		t.Fatalf("prod tmux session = %q, want agent", got)
	}
	if port, ok := browserIdentityPort("prod"); !ok || port != 7682 {
		t.Fatalf("prod identity port = %d, %v", port, ok)
	}
}

func TestNonProdChannelsAvoidProdResources(t *testing.T) {
	for _, env := range []string{"dev", "stg", "beta"} {
		if defaultTtydPortFor(env) == legacySharedTtydPort {
			t.Fatalf("%s must not default to prod's ttyd port", env)
		}
		if tmuxSessionNameFor(env) == "agent" {
			t.Fatalf("%s must not use prod's tmux session", env)
		}
	}
}

// A binary built with an unrecognised EnvName must still come up (prod's
// port) but must never touch prod's tmux session.
func TestUnknownChannelFallbacks(t *testing.T) {
	if got := defaultTtydPortFor("canary"); got != legacySharedTtydPort {
		t.Fatalf("unknown channel ttyd port = %d", got)
	}
	if got := tmuxSessionNameFor("canary"); got != "agent-canary" {
		t.Fatalf("unknown channel tmux session = %q", got)
	}
	if _, ok := browserIdentityPort("canary"); ok {
		t.Fatal("unknown channel must not get an identity port")
	}
}

func TestMigrateLegacyTtydPort(t *testing.T) {
	cases := []struct {
		env, name  string
		configured int
		wantPort   int
		wantMoved  bool
	}{
		{"dev", "legacy default moves to the channel port", legacySharedTtydPort, defaultTtydPortFor("dev"), true},
		{"beta", "legacy default moves to the channel port", legacySharedTtydPort, defaultTtydPortFor("beta"), true},
		{"prod", "prod keeps the legacy port", legacySharedTtydPort, legacySharedTtydPort, false},
		{"dev", "an explicit user override is kept", 9000, 9000, false},
		{"dev", "absent stays absent (resolved at bind time)", 0, 0, false},
	}
	for _, tc := range cases {
		port, moved := migrateLegacyTtydPort(tc.env, tc.configured)
		if port != tc.wantPort || moved != tc.wantMoved {
			t.Errorf("%s/%s: got (%d, %v), want (%d, %v)", tc.env, tc.name, port, moved, tc.wantPort, tc.wantMoved)
		}
	}
}

func TestResolvedTtydPortFollowsChannel(t *testing.T) {
	saved := EnvName
	t.Cleanup(func() { EnvName = saved })
	for _, env := range []string{"prod", "dev", "stg", "beta"} {
		EnvName = env
		if got := resolvedTtydPort(0); got != defaultTtydPortFor(env) {
			t.Fatalf("%s: resolvedTtydPort(0) = %d, want %d", env, got, defaultTtydPortFor(env))
		}
		if got := resolvedTtydPort(9000); got != 9000 {
			t.Fatalf("%s: explicit port must win, got %d", env, got)
		}
	}
}

func TestDefaultConfigUsesChannelTtydPort(t *testing.T) {
	saved := EnvName
	t.Cleanup(func() { EnvName = saved })
	EnvName = "dev"
	if got := DefaultConfig().LocalTtydPort; got != defaultTtydPortFor("dev") {
		t.Fatalf("dev DefaultConfig ttyd port = %d, want %d", got, defaultTtydPortFor("dev"))
	}
}

// The on-disk dev config from an older build (the one that collided with the
// prod agent on 2026-09-09) is moved to the dev port and rewritten so the
// next load does not repeat the migration.
func TestLoadConfigMigratesLegacyTtydPortOffProd(t *testing.T) {
	saved := EnvName
	t.Cleanup(func() { EnvName = saved })
	EnvName = "dev"

	path := filepath.Join(t.TempDir(), "config.json")
	original := DefaultConfig()
	original.LocalTtydPort = legacySharedTtydPort
	if err := original.Save(path); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalTtydPort != defaultTtydPortFor("dev") {
		t.Fatalf("loaded port = %d, want %d", cfg.LocalTtydPort, defaultTtydPortFor("dev"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := onDisk["local_ttyd_port"]; got != float64(defaultTtydPortFor("dev")) {
		t.Fatalf("persisted local_ttyd_port = %v, want %d", got, defaultTtydPortFor("dev"))
	}

	// prod never migrates: 7681 is its own default.
	EnvName = "prod"
	if err := original.Save(path); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(path); err != nil || cfg.LocalTtydPort != legacySharedTtydPort {
		t.Fatalf("prod load = (%d, %v), want %d", cfg.LocalTtydPort, err, legacySharedTtydPort)
	}
}

func TestTmuxDuplicateSessionDetection(t *testing.T) {
	if !tmuxDuplicateSession("duplicate session: agent-dev\n") {
		t.Fatal("duplicate-session stderr must be recognised")
	}
	if tmuxDuplicateSession("error connecting to /tmp/tmux-501/default (No such file or directory)\n") {
		t.Fatal("unrelated tmux errors must not read as a lost race")
	}
}

// When ttyd was disabled during startup (missing binary, busy port, failed
// spawn) nothing is listening, so the "Ready to Connect" box must not send the
// user to a loopback URL that will refuse the connection — nor to the tray's
// Open Terminal item, which opens the same dead URL.
func TestShowConnectionInstructionsHidesDeadLocalURL(t *testing.T) {
	cfg := &Config{ProjectID: "aix-test"}
	port := defaultTtydPortFor("prod")
	url := fmt.Sprintf("http://127.0.0.1:%d", port)

	up := captureStdout(t, func() { showConnectionInstructions(cfg, port, true) })
	if !strings.Contains(up, url) {
		t.Fatalf("a live local terminal must advertise %s, got:\n%s", url, up)
	}
	if !strings.Contains(up, "Open Terminal") {
		t.Fatalf("a live local terminal must keep the tray tip, got:\n%s", up)
	}

	down := captureStdout(t, func() { showConnectionInstructions(cfg, port, false) })
	if strings.Contains(down, url) {
		t.Fatalf("a disabled local terminal must not advertise %s, got:\n%s", url, down)
	}
	if strings.Contains(down, "Open Terminal") {
		t.Fatalf("a disabled local terminal must not offer the tray tip, got:\n%s", down)
	}
	if !strings.Contains(down, "Unavailable") {
		t.Fatalf("a disabled local terminal must say so, got:\n%s", down)
	}
	// Remote access is independent of the local terminal and must survive.
	if !strings.Contains(down, "Enabled (Pub/Sub)") {
		t.Fatalf("remote access must still be reported, got:\n%s", down)
	}
}
