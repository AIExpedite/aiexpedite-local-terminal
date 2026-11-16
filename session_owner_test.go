package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStampSessionOwner_ReplacesAnInheritedMarker(t *testing.T) {
	cmd := exec.Command("x")
	cmd.Env = []string{"A=1", sessionOwnerEnvVar + "=boot-old:outer", "B=2"}
	stampSessionOwner(cmd, sessionOwnerValue("boot", "inner"))
	var markers []string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, sessionOwnerEnvVar+"=") {
			markers = append(markers, kv)
		}
	}
	if len(markers) != 1 || markers[0] != sessionOwnerEnvVar+"=boot:inner" {
		t.Fatalf("markers = %v, want only this session's", markers)
	}
	if !envHasSessionOwner(cmd.Env, "boot:inner") || envHasSessionOwner(cmd.Env, "boot-old:outer") {
		t.Fatalf("envHasSessionOwner disagrees with %v", cmd.Env)
	}
	if !envHasSessionOwner(cmd.Env, "boot:inner") || len(cmd.Env) != 3 {
		t.Fatalf("other variables were not kept: %v", cmd.Env)
	}
}

func TestStampSessionOwner_NilEnvInheritsTheAgents(t *testing.T) {
	t.Setenv("AIX_STAMP_TEST", "kept")
	cmd := exec.Command("x")
	stampSessionOwner(cmd, "boot:s")
	if !envHasSessionOwner(cmd.Env, "boot:s") {
		t.Fatalf("no marker in %v", cmd.Env)
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "AIX_STAMP_TEST=kept" {
			found = true
		}
	}
	if !found || len(cmd.Env) < len(os.Environ()) {
		t.Fatalf("a nil Env must start from the agent's environment")
	}
}

// The one point every spawn passes stamps the marker.
func TestBeginSessionSpawn_StampsTheSessionOwner(t *testing.T) {
	cmd := exec.Command("x")
	cmd.Env = []string{"A=1"}
	beginSessionSpawn("stamp-test", cmd)
	t.Cleanup(func() { abortSessionSpawn("stamp-test") })
	if !envHasSessionOwner(cmd.Env, sessionOwnerValue(globalSpawnLedger.bootID, "stamp-test")) {
		t.Fatalf("beginSessionSpawn did not stamp the owner: %v", cmd.Env)
	}
}
