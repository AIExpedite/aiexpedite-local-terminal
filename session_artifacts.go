package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// sessionArtifactCollectTimeout bounds collectSessionArtifactsBounded. Every
// resident manager's waitForExit collects artifacts BEFORE publishing its
// `*_ended` frame and BEFORE closing session.done — so a scan/upload that
// hangs (a stalled GCS upload, an unreachable network share in the workdir)
// silently suppresses the ended frame terminal-service is waiting for, and
// leaves End() callers blocked on session.done. That is one of the two wedge
// shapes behind the 2026-08-27 HOMETHEATRE device outage (see end_confirm.go
// for the other). Two minutes is generous for legitimate video uploads while
// still guaranteeing the ended frame is published the same order of magnitude
// as the server's 5-minute reaper cadence. Declared as a var so tests can
// shorten it.
var sessionArtifactCollectTimeout = 2 * time.Minute

// collectSessionArtifactsBounded runs collectSessionArtifacts with a hard
// deadline. On expiry it returns (nil, nil, true) and abandons the scan
// goroutine — the scan holds no locks and its uploads are best-effort, so an
// abandoned one can only waste its own network round-trips. The `*_ended`
// frame then ships without file metadata, which is strictly better than never
// shipping at all.
func collectSessionArtifactsBounded(
	cfg *Config,
	sessionID string,
	workspaceID string,
	workDir string,
	startedAt time.Time,
	timeout time.Duration,
) ([]FileInfo, []UploadError, bool) {
	return boundedArtifactCollect(func() ([]FileInfo, []UploadError) {
		return collectSessionArtifacts(cfg, sessionID, workspaceID, workDir, startedAt)
	}, timeout, sessionID)
}

// boundedArtifactCollect is the deadline mechanism behind
// collectSessionArtifactsBounded, split out so tests can exercise the timeout
// branch with a blocking collector.
func boundedArtifactCollect(collect func() ([]FileInfo, []UploadError), timeout time.Duration, sessionID string) ([]FileInfo, []UploadError, bool) {
	type artifactResult struct {
		files []FileInfo
		errs  []UploadError
	}
	resultCh := make(chan artifactResult, 1)
	go func() {
		files, errs := collect()
		resultCh <- artifactResult{files: files, errs: errs}
	}()
	select {
	case r := <-resultCh:
		return r.files, r.errs, false
	case <-time.After(timeout):
		fmt.Printf("%s[session-file-upload] Artifact collection timed out after %s for session %s — publishing ended without file metadata%s\n",
			colorRed, timeout, sessionID, colorReset)
		return nil, nil, true
	}
}

// collectSessionArtifacts scans a finished CLI session's working directory for
// media the session produced and uploads it to GCS, returning the metadata the
// `*_ended` frame carries back to terminal-service.
//
// # WHY THIS IS SHARED AND NOT A METHOD ON SessionManager
//
// Uploading artifacts used to live inside `SessionManager.waitForExit`, so only
// the PTY session path did it. Every bundled-CLI path added since — codex
// app-server, claude native, grok ACP, antigravity native — runs its own
// manager with its own `waitForExit`, and none of them scanned. The result was
// a silent, total loss of captured media on exactly the paths the product now
// routes capture work through: prod 2026-08-20, video project vp_a72774c5, four
// `captureProductMedia` runs whose codex sessions recorded Playwright videos on
// the device and reported `NO_MEDIA_UPLOADED`, because
// `terminalSession.files[]` was empty for every one of them. The device log
// showed no `[session-file-upload]` line at all for those sessions — the scan
// never ran.
//
// So the scan belongs next to the thing every path has in common (a workdir, a
// start time, a workspace and a session id), and each manager calls it. A new
// CLI integration that forgets to is a bug, but it is now a one-line bug in an
// obvious place rather than an invisible behaviour that only the PTY path ever
// had.
//
// Never gated on exit code: a UI test that screenshots and then crashes is the
// case where the image matters MOST.
//
// @param workDir  the session's own spawn directory. Callers pass
//
//	Process.Dir; getTrackedCwd() is a last-resort fallback only, never an
//	additional root — scanning it while another workspace's command wrote
//	fresh media there would upload those files under THIS session's id.
func collectSessionArtifacts(
	cfg *Config,
	sessionID string,
	workspaceID string,
	workDir string,
	startedAt time.Time,
) ([]FileInfo, []UploadError) {
	if cfg == nil || !cfg.EnableFileUpload || workspaceID == "" {
		return nil, nil
	}

	effectiveDir := workDir
	if effectiveDir == "" {
		effectiveDir = getTrackedCwd()
	}
	if effectiveDir == "" {
		// Without a workdir we cannot scope the scan, so we skip it entirely.
		// Surface the reason — silently dropping every screenshot from a
		// session is the kind of thing operators need to see, not have to
		// git-blame for.
		fmt.Printf("[session-file-upload] Skipping upload for session %s — no effective workdir (Process.Dir and trackedCwd both empty)\n", sessionID)
		return nil, nil
	}

	files := detectOutputFilesSince(effectiveDir, startedAt)
	// Always log the scan outcome (root + count), even on zero hits. A silent
	// zero-file result is otherwise indistinguishable from "upload disabled" —
	// this line is what turns a future `hasFiles:false` report into a one-grep
	// diagnosis (scan root vs. where the agent actually wrote).
	fmt.Printf("[session-file-upload] Session %s: scanned %s since %s → %d media file(s)\n",
		sessionID, effectiveDir, startedAt.Format(time.RFC3339), len(files))
	if len(files) == 0 {
		return nil, nil
	}

	fmt.Printf("[session-file-upload] Detected %d output files, uploading to GCS (workspace: %s)...\n",
		len(files), workspaceID)

	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer uploadCancel()

	storageClient, storageErr := GetStorageClient(uploadCtx)
	if storageErr != nil {
		fmt.Printf("[session-file-upload] Failed to get storage client: %v\n", storageErr)
		return nil, uploadErrorsForFiles(files, storageErr)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	uploadResult := UploadFiles(
		uploadCtx,
		storageClient,
		cfg.StorageBucket,
		files,
		workspaceID,
		sessionID,
		logger,
	)

	fmt.Printf("[session-file-upload] Upload complete: %d successful, %d failed\n",
		len(uploadResult.Successful), len(uploadResult.Failed))

	return uploadResult.Successful, uploadResult.Failed
}

func uploadErrorsForFiles(files []string, err error) []UploadError {
	errors := make([]UploadError, 0, len(files))
	for _, file := range files {
		errors = append(errors, UploadError{File: file, Error: err.Error()})
	}
	return errors
}
