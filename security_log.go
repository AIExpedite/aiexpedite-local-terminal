// File: security_log.go
//
// Structured logging for security-relevant events. The general agent flow
// uses fmt.Println for ergonomic console output, which is fine for
// developer-facing UX. But security events (denied operations, attestation
// failures, etc.) need to be:
//
//  1. Structured — so an investigation can grep / pipe to jq
//  2. Persisted — so events that happened hours ago are still inspectable
//  3. Hard to silently drop — so a future fmt.Println refactor can't
//     accidentally erase the audit trail
//
// We deliberately keep this module small. It's NOT a general-purpose logger
// migration — that would touch every file in the repo. The goal is just to
// wrap the events that matter for security investigations.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// SecurityEvent classifies the kind of event so log-readers can filter
// (e.g. show me only attestation failures from the last 7 days).
type SecurityEvent string

const (
	// SecEvtPathTraversal: a file/directory operation attempted to walk
	// outside the allowed base directory.
	SecEvtPathTraversal SecurityEvent = "path_traversal_blocked"

	// SecEvtAttestationFailed: the auto-update flow couldn't verify the
	// downloaded binary's SLSA build provenance and refused to apply.
	SecEvtAttestationFailed SecurityEvent = "attestation_failed"

	// SecEvtAttestationVerified: positive event — useful when correlating
	// "did the user successfully update to the malicious version?"
	SecEvtAttestationVerified SecurityEvent = "attestation_verified"

	// SecEvtAttestationSkipped: env-var bypass was used. Loud signal —
	// every legitimate user should see this at most once when configuring,
	// every time after that is suspicious.
	SecEvtAttestationSkipped SecurityEvent = "attestation_skipped_by_env"

	// SecEvtAllowlistDenied: a remote command was rejected by the local
	// allowlist (the user wasn't asked because the command didn't match
	// any allow pattern). Volume can be high in normal use; keep fields
	// minimal to avoid log bloat.
	SecEvtAllowlistDenied SecurityEvent = "allowlist_denied"

	// SecEvtAllowAllEnabled / SecEvtAllowAllDisabled: the operator
	// toggled the "Allow All Commands" tray override. Enabling is the
	// loud event — it disables every allow-list and approval check
	// until toggled back. Disabling is logged too so the audit trail
	// shows when normal posture was restored.
	SecEvtAllowAllEnabled  SecurityEvent = "allow_all_commands_enabled"
	SecEvtAllowAllDisabled SecurityEvent = "allow_all_commands_disabled"
)

var (
	secLogOnce   sync.Once
	secLogger    *slog.Logger
	secLogFile   *os.File
	secLogInitOk bool
)

// initSecurityLogger sets up a JSON-emitting logger writing to
// `<config>/security.log`. Safe to call concurrently — runs at most once.
// On any setup failure (e.g. read-only home dir), falls back to stderr-
// only logging rather than dropping events. We never want a logging
// failure to block a security event from being recorded.
func initSecurityLogger() {
	secLogOnce.Do(func() {
		dir := GetConfigDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			// Couldn't even create the config dir — fall back to stderr.
			secLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
			return
		}
		path := filepath.Join(dir, "security.log")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			secLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
			return
		}
		secLogFile = f
		secLogger = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
		secLogInitOk = true
	})
}

// LogSecurityEvent records a security-classified event to the audit log
// AND prints a one-line console marker so the operator notices in real
// time. Caller passes structured fields as alternating key/value pairs
// (slog convention).
//
// We don't bubble errors — a logging failure must not block the calling
// security check. If the file write fails we'll see it on stderr the next
// time the slog handler errors out.
func LogSecurityEvent(evt SecurityEvent, msg string, fields ...any) {
	initSecurityLogger()
	// Always include the event-type field so log-readers can filter by it
	// without parsing the message string.
	allFields := append([]any{"event", string(evt)}, fields...)
	secLogger.Info(msg, allFields...)

	// Console mirror — preserves the existing operator UX (people watching
	// the terminal see the event in real time). Kept brief; the JSON log
	// has the full detail.
	fmt.Printf("[security] %s: %s\n", evt, msg)
}

// CloseSecurityLogger flushes and closes the audit log file. Call from
// shutdown paths. Safe to call even if init didn't run.
func CloseSecurityLogger() {
	if secLogFile != nil {
		_ = secLogFile.Sync()
		_ = secLogFile.Close()
	}
}
