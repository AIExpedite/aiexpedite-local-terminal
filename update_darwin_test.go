//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRelaunchDarwinBundleWaitsForOpenResult(t *testing.T) {
	binDir := t.TempDir()
	openPath := filepath.Join(binDir, "open")
	script := []byte("#!/bin/sh\necho launch-rejected >&2\nexit 23\n")
	if err := os.WriteFile(openPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := relaunchDarwinBundle("/Applications/AI Expedite.app")
	if err == nil {
		t.Fatal("non-zero LaunchServices result must fail the handoff")
	}
	if !strings.Contains(err.Error(), "launch-rejected") {
		t.Fatalf("relaunch error should include open output, got %v", err)
	}
}

func TestFailedDarwinRelaunchYieldsWhenSingletonWasTaken(t *testing.T) {
	previousAcquire := acquireAgentInstanceAfterDarwinHandoff
	previousQuit := quitAfterDarwinHandoff
	updateMutex.Lock()
	previousPath, previousPending := updatePath, updatePending
	updatePath, updatePending = "", false
	updateMutex.Unlock()
	t.Cleanup(func() {
		acquireAgentInstanceAfterDarwinHandoff = previousAcquire
		quitAfterDarwinHandoff = previousQuit
		updateMutex.Lock()
		updatePath, updatePending = previousPath, previousPending
		updateMutex.Unlock()
	})

	acquireAgentInstanceAfterDarwinHandoff = func() (func(), bool) {
		return func() {}, false
	}
	var quits atomic.Int32
	quitAfterDarwinHandoff = func() { quits.Add(1) }

	if err := handleFailedDarwinRelaunch("/Applications/AI Expedite.app", "", errors.New("open failed")); err != nil {
		t.Fatalf("lost-singleton handoff should yield without reopening admission: %v", err)
	}
	if got := quits.Load(); got != 1 {
		t.Fatalf("quit calls = %d, want 1", got)
	}
	if path, pending := GetUpdateReady(); !pending || path != "" {
		t.Fatalf("handoff marker = (%q, %v), want empty-path pending marker", path, pending)
	}
}

func TestFailedDarwinRelaunchStaysAliveAfterReacquiringSingleton(t *testing.T) {
	previousAcquire := acquireAgentInstanceAfterDarwinHandoff
	previousQuit := quitAfterDarwinHandoff
	t.Cleanup(func() {
		acquireAgentInstanceAfterDarwinHandoff = previousAcquire
		quitAfterDarwinHandoff = previousQuit
		releaseAgentInstanceForHandoff()
	})

	var releases atomic.Int32
	acquireAgentInstanceAfterDarwinHandoff = func() (func(), bool) {
		return func() { releases.Add(1) }, true
	}
	var quits atomic.Int32
	quitAfterDarwinHandoff = func() { quits.Add(1) }

	parent := t.TempDir()
	bundle := filepath.Join(parent, "AI Expedite.app")
	backup := filepath.Join(parent, ".aixupd_old_AI Expedite.app")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "version"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "version"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := handleFailedDarwinRelaunch(bundle, backup, errors.New("open failed"))
	if err == nil {
		t.Fatal("reacquired singleton should keep the current process alive and report apply failure")
	}
	if got := quits.Load(); got != 0 {
		t.Fatalf("quit calls = %d, want 0", got)
	}
	releaseAgentInstanceForHandoff()
	if got := releases.Load(); got != 1 {
		t.Fatalf("tracked singleton release calls = %d, want 1", got)
	}
	version, readErr := os.ReadFile(filepath.Join(bundle, "version"))
	if readErr != nil {
		t.Fatalf("restored bundle is not launchable: %v", readErr)
	}
	if string(version) != "old" {
		t.Fatalf("installed bundle version = %q, want restored old bundle", version)
	}
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Fatalf("backup should be consumed by rollback: %v", statErr)
	}
}

func TestParseDarwinTeamID(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name: "valid team identifier",
			output: "Executable=/Applications/AI Expedite.app/Contents/MacOS/aiexpedite\n" +
				"Identifier=com.aiexpedite.terminal\n" +
				"Format=app bundle with Mach-O universal (x86_64 arm64)\n" +
				"TeamIdentifier=ABCDE12345\n" +
				"Sealed Resources version=2 rules=13 files=0\n",
			want:    "ABCDE12345",
			wantErr: false,
		},
		{
			name: "windows line endings",
			output: "Executable=/Applications/AI Expedite.app\r\n" +
				"TeamIdentifier=ABCDE12345\r\n",
			want:    "ABCDE12345",
			wantErr: false,
		},
		{
			name:    "unset team identifier",
			output:  "TeamIdentifier=not set\n",
			wantErr: true,
		},
		{
			name:    "empty team identifier",
			output:  "TeamIdentifier=\n",
			wantErr: true,
		},
		{
			name:    "missing team identifier",
			output:  "Executable=/Applications/AI Expedite.app\nIdentifier=com.aiexpedite.terminal\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDarwinTeamID(tt.output)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseDarwinTeamID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseDarwinTeamID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVerifyDarwinSigningTeam(t *testing.T) {
	prevExtract := extractDarwinTeamID
	t.Cleanup(func() {
		extractDarwinTeamID = prevExtract
	})

	tests := []struct {
		name          string
		installedTeam string
		installedErr  error
		updateTeam    string
		updateErr     error
		wantErrSubstr string
	}{
		{
			name:          "matching teams pass",
			installedTeam: "TEAM_123",
			updateTeam:    "TEAM_123",
		},
		{
			name:          "mismatched teams rejected",
			installedTeam: "TEAM_123",
			updateTeam:    "OTHER_456",
			wantErrSubstr: "signing team mismatch",
		},
		{
			name:          "installed extraction failure rejected",
			installedErr:  errors.New("corrupt signature"),
			updateTeam:    "TEAM_123",
			wantErrSubstr: "installed bundle team verification failed",
		},
		{
			name:          "installed empty team rejected",
			installedTeam: "",
			updateTeam:    "TEAM_123",
			wantErrSubstr: "installed bundle has empty signing team identifier",
		},
		{
			name:          "update extraction failure rejected",
			installedTeam: "TEAM_123",
			updateErr:     errors.New("no signature"),
			wantErrSubstr: "update bundle team verification failed",
		},
		{
			name:          "update empty team rejected",
			installedTeam: "TEAM_123",
			updateTeam:    "",
			wantErrSubstr: "update bundle has empty signing team identifier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extractDarwinTeamID = func(bundle string) (string, error) {
				if bundle == "installed.app" {
					return tt.installedTeam, tt.installedErr
				}
				if bundle == "update.app" {
					return tt.updateTeam, tt.updateErr
				}
				return "", errors.New("unexpected bundle")
			}

			err := verifyDarwinSigningTeam("installed.app", "update.app")
			if tt.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSubstr)
				}
			}
		})
	}
}
