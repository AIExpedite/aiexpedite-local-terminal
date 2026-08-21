// File: session_artifacts_test.go
// Unit tests for collectSessionArtifacts — the shared post-session media scan
// that every CLI manager (PTY, codex app-server, claude native, grok ACP) calls
// on exit. Before it was shared, only the PTY path scanned, so a capture driven
// through a bundled CLI reported NO_MEDIA_UPLOADED while its recording sat on
// the device (prod 2026-08-20, video project vp_a72774c5).
package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The guard cases below all assert the same thing: we return empty WITHOUT
// reaching GetStorageClient. That matters because these tests run with no GCS
// credentials — if a guard regresses, the test fails on a storage error rather
// than passing quietly.

func TestCollectSessionArtifacts_NilConfigIsANoOp(t *testing.T) {
	files, errs := collectSessionArtifacts(nil, "s1", "ws-1", t.TempDir(), time.Now())
	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors with a nil config, got %v / %v", files, errs)
	}
}

func TestCollectSessionArtifacts_UploadDisabledIsANoOp(t *testing.T) {
	cfg := &Config{EnableFileUpload: false, StorageBucket: "b"}
	files, errs := collectSessionArtifacts(cfg, "s1", "ws-1", t.TempDir(), time.Now())
	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors when uploads are disabled, got %v / %v", files, errs)
	}
}

// A session with no workspace has nowhere to file the upload — uploading it
// under an empty workspace id would put media outside every tenant's namespace.
func TestCollectSessionArtifacts_NoWorkspaceIsANoOp(t *testing.T) {
	cfg := &Config{EnableFileUpload: true, StorageBucket: "b"}
	files, errs := collectSessionArtifacts(cfg, "s1", "", t.TempDir(), time.Now())
	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors without a workspace, got %v / %v", files, errs)
	}
}

// An empty workdir falls back to the process-wide tracked cwd, and when that is
// empty too there is nothing to scope a scan to. Scanning "everything" here
// would upload another workspace's fresh media under this session's id.
func TestCollectSessionArtifacts_NoWorkdirSkipsScan(t *testing.T) {
	setTrackedCwd("")
	cfg := &Config{EnableFileUpload: true, StorageBucket: "b"}
	files, errs := collectSessionArtifacts(cfg, "s1", "ws-1", "", time.Now())
	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors without a workdir, got %v / %v", files, errs)
	}
}

// The scan itself must find nothing when the session wrote nothing — and must
// reach that conclusion without a storage client, so a quiet session never
// pays for GCS auth.
func TestCollectSessionArtifacts_NoMediaProducedSkipsUpload(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "notes.txt"), "not media", time.Now())
	cfg := &Config{EnableFileUpload: true, StorageBucket: "b"}

	files, errs := collectSessionArtifacts(cfg, "s1", "ws-1", dir, time.Now().Add(-time.Minute))
	if files != nil || errs != nil {
		t.Fatalf("expected no files/errors when no media was written, got %v / %v", files, errs)
	}
}

// The case the bundled-CLI paths were missing: a Playwright recording written
// under the session's own cwd must be DETECTED. Upload needs GCS, so this stops
// at detection — the contract being pinned is "the scan sees the capture", which
// is exactly what returned zero for every codex capture session.
func TestCollectSessionArtifacts_FindsAPlaywrightRecording(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	capture := filepath.Join(dir, "test-results", "captures", "capture-1-workflow-1920x1080.webm")
	writeFileAt(t, capture, "webm-bytes", start.Add(freshOffset))

	found := detectOutputFilesSince(dir, start)
	if len(found) != 1 {
		t.Fatalf("expected the recording to be detected, got %v", found)
	}
	if filepath.Base(found[0]) != "capture-1-workflow-1920x1080.webm" {
		t.Fatalf("detected the wrong file: %v", found)
	}
}
