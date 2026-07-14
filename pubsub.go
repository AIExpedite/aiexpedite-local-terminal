// File: pubsub.go
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/pubsub/v2"
	"github.com/getlantern/systray"
	"golang.org/x/mod/semver"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
	"google.golang.org/api/option"
)

/* --------------------------------------------------------------------------
   Console Color Output
   -------------------------------------------------------------------------- */

// ANSI color codes for console output
const (
	colorReset   = "\033[0m"
	colorCyan    = "\033[36m" // Received commands
	colorGreen   = "\033[32m" // Success output
	colorYellow  = "\033[33m" // Warnings
	colorRed     = "\033[31m" // Errors
	colorMagenta = "\033[35m" // System messages
	colorBlue    = "\033[34m" // Info/metadata
)

const (
	maxPubSubCommandMessageBytes = 1024 * 1024
	maxPubSubCatalogMessageBytes = maxOIDCTokenResponseBytes
)

// colorPrefix wraps a prefix with color codes for console output
func colorPrefix(prefix, color string) string {
	return color + prefix + colorReset
}

/* --------------------------------------------------------------------------
   Sensitive Data Redaction
   -------------------------------------------------------------------------- */

var (
	// Compiled regex patterns for sensitive data detection
	sensitivePatterns     []*regexp.Regexp
	sensitivePatternsOnce sync.Once
)

// initSensitivePatterns compiles regex patterns once
func initSensitivePatterns() {
	sensitivePatternsOnce.Do(func() {
		patterns := []string{
			// Passwords in various formats
			`(?i)(password|passwd|pwd)\s*[=:]\s*\S+`,
			`(?i)(-p|--password)\s+\S+`,
			// API keys, tokens, secrets
			`(?i)(token|api_key|apikey|api-key|secret|secret_key|secretkey|auth_key|authkey)\s*[=:]\s*\S+`,
			// Bearer tokens
			`(?i)Bearer\s+[A-Za-z0-9\-_.~+/]+=*`,
			// AWS credentials
			`(?i)(aws_access_key_id|aws_secret_access_key|aws_session_token)\s*[=:]\s*\S+`,
			`AKIA[0-9A-Z]{16}`, // AWS Access Key ID pattern
			// Connection strings with credentials
			`(?i)(mongodb|postgres|mysql|redis|amqp)://[^:]+:[^@]+@`,
			// Private keys
			`(?i)-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----`,
			// GitHub tokens
			`gh[pousr]_[A-Za-z0-9_]{36,}`,
			// Generic secrets in JSON
			`(?i)"(password|secret|token|api_key|apikey)":\s*"[^"]+`,
		}

		sensitivePatterns = make([]*regexp.Regexp, 0, len(patterns))
		for _, p := range patterns {
			if re, err := regexp.Compile(p); err == nil {
				sensitivePatterns = append(sensitivePatterns, re)
			}
		}
	})
}

// redactSensitiveData replaces sensitive patterns in a string with [REDACTED]
func redactSensitiveData(s string) string {
	initSensitivePatterns()

	result := s
	for _, re := range sensitivePatterns {
		result = re.ReplaceAllStringFunc(result, func(match string) string {
			// For patterns like "password=xxx", keep the key visible
			if idx := strings.IndexAny(match, "=:"); idx != -1 {
				return match[:idx+1] + "[REDACTED]"
			}
			// For patterns like "Bearer xxx", keep the prefix
			if strings.HasPrefix(strings.ToLower(match), "bearer ") {
				return "Bearer [REDACTED]"
			}
			// For connection strings, redact credentials but keep protocol
			if strings.Contains(match, "://") {
				parts := strings.SplitN(match, "://", 2)
				if len(parts) == 2 {
					return parts[0] + "://[REDACTED]@"
				}
			}
			return "[REDACTED]"
		})
	}
	return result
}

// redactArgs redacts sensitive data from command arguments
func redactArgs(args []string) []string {
	result := make([]string, len(args))
	for i, arg := range args {
		result[i] = redactSensitiveData(arg)
	}
	return result
}

// redactCommandForLog creates a safe string for logging a command
func redactCommandForLog(cmd string, args []string) string {
	return redactSensitiveData(cmd) + " " + strings.Join(redactArgs(args), " ")
}

// makeRejectionResult builds the resultMsg sent back to the backend when a
// command is refused (allowlist deny, stale, rate-limit, unauthorized). The
// backend's persistRejection() reads command/args/cwd off this message and
// writes to workspace/{id}/rejected_terminal_commands. Without echoing
// command/args here every captured rejection is "we denied something for
// some reason" — diagnostically useless, exactly what 108 prod records
// looked like before this fix.
//
// Command and args are passed through redactSensitiveData/redactArgs so
// secrets the LLM put on the command line (Bearer tokens, passwords in
// connection strings, *_TOKEN env vars) are scrubbed before they hit
// Firestore.
//
// Cwd carries the requested cwd from the inbound command — for rejections
// the command never ran, so the post-execution `getTrackedCwd()` value
// would either be empty or describe an unrelated previous session.
func makeRejectionResult(cmd commandMsg, agentID, status, reason, output string) resultMsg {
	res := resultMsg{
		ID:              cmd.ID,
		WorkspaceID:     cmd.WorkspaceID,
		UID:             cmd.UID,
		AgentID:         agentID,
		Output:          output,
		Status:          status,
		Ts:              time.Now().UnixMilli(),
		Version:         Version,
		Cwd:             cmd.Cwd,
		Command:         redactSensitiveData(cmd.Command),
		Args:            redactRejectionArgs(cmd),
		RejectionReason: reason,
	}
	// Session-routed commands need Type/SessionID set so the backend can
	// correlate the rejection with the session document. The rejection Type
	// is derived from cmd.Type so codex_appserver_* / grok_acp_* rejections
	// (allowlist deny / stale / rate-limited / signature failure) come back
	// labeled with their family-specific error type rather than the generic
	// session_error — each orchestrator-side protocol handler can then route
	// them without having to special-case SessionID prefixes.
	if cmd.Type != "" && cmd.SessionID != "" {
		res.Type = rejectionResultType(cmd.Type)
		res.SessionID = cmd.SessionID
	}
	return res
}

// redactRejectionArgs scrubs cmd.Args before they land in the rejected-command
// record. The generic per-arg redactSensitiveData regex catches inline
// secrets (`--api-key=xai-…`, bearer tokens, etc.) but cannot see across two
// adjacent argv tokens — for `--api-key xai-…` the secret sits in the next
// arg as a bare value that matches no pattern, so the key would otherwise be
// persisted to the rejected-command record even though the approval-dialog
// path already redacts it via redactGrokACPArgsForLog. Route grok_acp_start
// rejections through the same family-specific masker so the dialog and
// rejection paths agree; everything else falls back to redactArgs.
func redactRejectionArgs(cmd commandMsg) []string {
	if cmd.Type == "grok_acp_start" {
		return redactGrokACPArgsForLog(cmd.Args)
	}
	return redactArgs(cmd.Args)
}

// rejectionResultType maps an inbound command Type to the result Type used
// when the command is rejected. Centralised so adding a new family of
// interactive commands only requires updating one switch.
func rejectionResultType(cmdType string) string {
	if isCodexAppServerCommand(cmdType) {
		return "codex_appserver_error"
	}
	if isGrokACPCommand(cmdType) {
		return "grok_acp_error"
	}
	if isClaudeNativeCommand(cmdType) {
		return "claude_native_error"
	}
	if isAntigravityNativeCommand(cmdType) {
		return "antigravity_native_error"
	}
	return "session_error"
}

/* --------------------------------------------------------------------------
   Command Staleness Check
   -------------------------------------------------------------------------- */

// maxCommandAgeSec is the maximum age (in seconds) a command can have before
// being rejected as stale. This prevents processing old queued commands when
// the terminal reconnects after being offline.
const maxCommandAgeSec = 60 // 1 minute

// isCommandStale checks if a command's timestamp is older than maxCommandAgeSec.
// Also rejects commands with future-dated timestamps (clock skew > 60 s) to
// prevent an attacker from bypassing the staleness check with a far-future ts.
func isCommandStale(cmdTs int64) bool {
	if cmdTs == 0 {
		return false // No timestamp - allow for backwards compatibility
	}
	now := time.Now().UnixMilli()
	ageMs := now - cmdTs
	// Reject if too old OR if more than 60 s in the future (abnormal clock skew)
	return ageMs > int64(maxCommandAgeSec*1000) || ageMs < -int64(maxCommandAgeSec*1000)
}

/* --------------------------------------------------------------------------
   Per-UID Rate Limiting
   -------------------------------------------------------------------------- */

type rateLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

var (
	// Per-UID rate limiters: 10 commands/second with burst of 20
	uidRateLimiters      = make(map[string]*rateLimiterEntry)
	uidRateMutex         sync.RWMutex
	rateLimiterCleanupOn sync.Once
)

/* --------------------------------------------------------------------------
   Tracked Working Directory
   Persists the last known cwd across commands so that cd changes
   are remembered even when the server sends the same default cwd.
   -------------------------------------------------------------------------- */

var (
	trackedCwd     string
	trackedCwdLock sync.RWMutex
)

func getTrackedCwd() string {
	trackedCwdLock.RLock()
	defer trackedCwdLock.RUnlock()
	return trackedCwd
}

func setTrackedCwd(cwd string) {
	if cwd == "" {
		return
	}
	trackedCwdLock.Lock()
	trackedCwd = cwd
	trackedCwdLock.Unlock()
}

// startRateLimiterCleanup launches a background goroutine that removes stale entries every minute.
// The goroutine exits when shutdownChan is closed.
func startRateLimiterCleanup() {
	rateLimiterCleanupOn.Do(func() {
		go func() {
			ticker := time.NewTicker(1 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					cutoff := time.Now().Add(-1 * time.Hour)
					uidRateMutex.Lock()
					for uid, entry := range uidRateLimiters {
						if entry.lastAccess.Before(cutoff) {
							delete(uidRateLimiters, uid)
						}
					}
					uidRateMutex.Unlock()
				case <-shutdownChan:
					return
				}
			}
		}()
	})
}

// getUIDRateLimiter returns or creates a rate limiter for the given UID
func getUIDRateLimiter(uid string, cfg *Config) *rate.Limiter {
	startRateLimiterCleanup()

	now := time.Now()

	// Fast path: check under read lock first.
	uidRateMutex.RLock()
	entry, exists := uidRateLimiters[uid]
	uidRateMutex.RUnlock()

	if exists {
		// lastAccess is updated under the write lock below to avoid a separate
		// lock acquisition that could race with eviction or concurrent creates.
		uidRateMutex.Lock()
		// Re-check: entry may have been evicted between the RUnlock and Lock.
		if e, ok := uidRateLimiters[uid]; ok {
			e.lastAccess = now
			uidRateMutex.Unlock()
			return e.limiter
		}
		// Entry was evicted — fall through to create a new one below
		// (mutex is still held; do not unlock before the creation block)
		defer uidRateMutex.Unlock()
	} else {
		uidRateMutex.Lock()
		defer uidRateMutex.Unlock()

		// Double-check after acquiring write lock
		if entry, exists = uidRateLimiters[uid]; exists {
			entry.lastAccess = now
			return entry.limiter
		}
	}

	// Use config values with defaults
	rateLimit := cfg.RateLimitPerSecond
	if rateLimit <= 0 {
		rateLimit = 10
	}
	burst := cfg.RateLimitBurst
	if burst <= 0 {
		burst = 20
	}

	// Safety cap: evict one entry if the map exceeds the limit.
	// Prevents unbounded memory growth when many distinct UIDs are seen.
	// Use a random eviction (first key in Go's randomised map iteration) rather
	// than a full O(N) scan for the oldest entry: at 10,000 entries a linear scan
	// under the write lock would block all concurrent message handlers for an
	// observable period.  Random eviction is good enough — the background cleanup
	// goroutine (startRateLimiterCleanup) removes genuinely stale entries every
	// 10 minutes, so in practice the cap is only hit by unusual traffic patterns.
	const maxRateLimiterEntries = 10000
	if len(uidRateLimiters) >= maxRateLimiterEntries {
		for evictUID := range uidRateLimiters {
			delete(uidRateLimiters, evictUID)
			break
		}
	}

	limiter := rate.NewLimiter(rate.Limit(rateLimit), burst)
	uidRateLimiters[uid] = &rateLimiterEntry{limiter: limiter, lastAccess: now}
	return limiter
}

// checkRateLimit checks if a command should be rate-limited
// Returns true if the command is allowed, false if rate-limited
func checkRateLimit(uid string, agentID string, cfg *Config) bool {
	key := uid
	if key == "" {
		key = agentID
	}
	if key == "" {
		return true // No identity at all — allow (edge case)
	}
	limiter := getUIDRateLimiter(key, cfg)
	return limiter.Allow()
}

/*
--------------------------------------------------------------------------

	Signature failure rate limiting
	--------------------------------------------------------------------------
*/
var (
	sigFailCount   int64
	sigFailResetAt time.Time
	sigFailMu      sync.Mutex
)

const maxSigFailsPerMinute = 10

// isSigFailRateLimited returns true if too many signature failures have occurred recently.
// When true, the caller should silently ACK without publishing a response.
func isSigFailRateLimited() bool {
	sigFailMu.Lock()
	defer sigFailMu.Unlock()
	now := time.Now()
	if now.After(sigFailResetAt) {
		sigFailCount = 0
		sigFailResetAt = now.Add(time.Minute)
	}
	sigFailCount++
	return sigFailCount > maxSigFailsPerMinute
}

/* --------------------------------------------------------------------------
   Command Signature Verification (HMAC-SHA256)
   -------------------------------------------------------------------------- */

// signaturePayload matches the exact JSON structure used by Node.js signCommand()
// Field order must match: id, command, args, ts, type, sessionID, input, signal, refreshId, cliAgentCatalog
//
// refreshId is signed so an adversary that can alter a signed
// __cli_usage_refresh__ command cannot swap the correlation id without
// invalidating the HMAC — protecting the backend's stale-result guard
// (cliUsageInFlightRefreshId mismatch drop) from a replay/tamper bypass.
//
// refreshId uses `omitempty` so non-refresh commands (which don't carry one)
// produce the same canonical JSON as the pre-refreshId signing format. That
// preserves signature verification across the agent/service upgrade window:
// a new agent receiving a normal command from an older Node producer (no
// refreshId in the signed shape) still matches, and a new Node producer
// sending a non-refresh command to an older agent (also without refreshId)
// still matches. Only the new __cli_usage_refresh__ command carries the
// field, and both ends include it.
type signaturePayload struct {
	ID        string   `json:"id"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Ts        int64    `json:"ts"`
	Type      string   `json:"type"`
	SessionID string   `json:"sessionID"`
	Input     string   `json:"input"`
	Signal    string   `json:"signal"`
	RefreshID string   `json:"refreshId,omitempty"`
	// riskLevel is signed and omitempty so non-env-setup commands (which never
	// carry one) produce the identical canonical JSON as the pre-riskLevel
	// format — preserving signature compatibility across the agent/service
	// upgrade window, exactly like refreshId. Only env-setup steps set it, and
	// both ends include it. Signing it prevents a "destructive"→"" downgrade
	// that would skip native approval.
	RiskLevel       string          `json:"riskLevel,omitempty"`
	CliAgentCatalog json.RawMessage `json:"cliAgentCatalog,omitempty"`
}

// verifySignature verifies the HMAC-SHA256 signature of a command
// Returns true if signature is valid, false otherwise
func verifySignature(cmd commandMsg, secret string) bool {
	// Create canonical representation matching backend signCommand()
	// Use struct to ensure consistent JSON key ordering (id, command, args, ts)
	args := cmd.Args
	if args == nil {
		args = []string{}
	}

	payload := signaturePayload{
		ID:              cmd.ID,
		Command:         cmd.Command,
		Args:            args,
		Ts:              cmd.Ts,
		Type:            cmd.Type,
		SessionID:       cmd.SessionID,
		Input:           cmd.Input,
		Signal:          cmd.Signal,
		RefreshID:       cmd.RefreshID,
		RiskLevel:       cmd.RiskLevel,
		CliAgentCatalog: cliAgentCatalogSignatureJSON(cmd),
	}

	// Use json.NewEncoder with SetEscapeHTML(false) to match Node.js JSON.stringify behavior.
	// Go's json.Marshal escapes &, <, > as \u0026, \u003c, \u003e by default,
	// but Node.js does not. This caused signature mismatches for commands containing &.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return false
	}
	// Encode appends a trailing newline — strip it to match JSON.stringify output
	signatureData := bytes.TrimRight(buf.Bytes(), "\n")

	// Compute expected signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signatureData)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	// Constant-time comparison to prevent timing attacks
	return hmac.Equal([]byte(expectedSig), []byte(cmd.Signature))
}

func cliAgentCatalogSignatureJSON(cmd commandMsg) json.RawMessage {
	if len(cmd.rawCliAgentCatalog) > 0 {
		return append(json.RawMessage(nil), cmd.rawCliAgentCatalog...)
	}
	if cmd.CliAgentCatalog == nil {
		return nil
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cmd.CliAgentCatalog); err != nil {
		return nil
	}
	return append(json.RawMessage(nil), bytes.TrimRight(buf.Bytes(), "\n")...)
}

// argsToJSON converts args slice to JSON array string
func argsToJSON(args []string) string {
	if args == nil || len(args) == 0 {
		return "[]"
	}
	bytes, err := json.Marshal(args)
	if err != nil {
		return "[]"
	}
	return string(bytes)
}

func pubSubMessageSizeLimit(cmd commandMsg) int {
	if cmd.Command == "__cli_usage_refresh__" {
		return maxPubSubCatalogMessageBytes
	}
	return maxPubSubCommandMessageBytes
}

// isInternalDemandCommand reports whether cmd is a server-internal, read-only
// demand command (CLI-usage refresh or environment inspection). These are
// exempt from staleness and rate-limiting because they are dispatched by the
// backend's own state machines (not the LLM) and MUST always run so the
// correlated result clears the backend's in-flight / pending marker.
func isInternalDemandCommand(command string) bool {
	return command == "__cli_usage_refresh__" || command == "__env_inspect__"
}

func commandPayloadTooLargeMessage(sizeBytes, limitBytes int) string {
	return fmt.Sprintf("Command rejected: payload size %d bytes exceeds %d byte limit", sizeBytes, limitBytes)
}

/* Incoming command payload (matches backend publishCommand struct) */
type commandMsg struct {
	ID          string   `json:"id"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Cwd         string   `json:"cwd,omitempty"` // Working directory for command execution
	WorkspaceID string   `json:"workspaceID"`   // Workspace scope for file uploads
	UID         string   `json:"uid"`
	Ts          int64    `json:"ts"`
	AgentID     string   `json:"agentId,omitempty"`   // Target agent for signature verification
	Signature   string   `json:"signature,omitempty"` // HMAC-SHA256 signature of command
	TimeoutMs   int64    `json:"timeoutMs,omitempty"` // Execution timeout in milliseconds (default: 120000)
	// CLI usage refresh: carries the backend's refreshId so __cli_usage_refresh_result__
	// can echo it back. Stored as `cliUsageInFlightRefreshId` on the agent doc;
	// stale results (mismatched refreshId) are dropped by the results subscriber.
	RefreshID string `json:"refreshId,omitempty"`
	// Optional database-backed CLI-agent catalog. When included, it is signed
	// with the command payload and applied before usage probing.
	CliAgentCatalog []cliAgentCatalogEntry `json:"cliAgentCatalog,omitempty"`

	// Session fields (for interactive CLI agent sessions)
	Type      string `json:"type,omitempty"`      // "execute"|"session_start"|"session_input"|"session_signal"|"session_end"|"codex_appserver_start"|"codex_appserver_send"|"codex_appserver_end"|"grok_acp_start"|"grok_acp_send"|"grok_acp_end"
	SessionID string `json:"sessionID,omitempty"` // Unique session identifier
	Input     string `json:"input,omitempty"`     // stdin text for session_input; raw JSON-RPC frame for codex_appserver_send / grok_acp_send
	Signal    string `json:"signal,omitempty"`    // "interrupt"|"kill" for session_signal

	// Environment Setup plan attribution + risk. RiskLevel is SIGNED (see
	// signaturePayload) so an attacker who can alter a signed command cannot
	// downgrade a "destructive" step to bypass the on-device native approval
	// dialog. PlanID/StepID are unsigned correlation metadata for the audit
	// trail (like WorkspaceID/UID, which are also unsigned).
	RiskLevel string `json:"riskLevel,omitempty"` // env-setup step risk (mirrors shared-constants RISK_LEVELS)
	PlanID    string `json:"planId,omitempty"`    // env-setup plan id (audit correlation)
	StepID    string `json:"stepId,omitempty"`    // env-setup step id (audit correlation)

	// Tty opts an execute/session command into the PTY path (macOS/Linux only)
	// for interactive/TUI CLIs (e.g. agy). Default false = the hardened pipe
	// path. Unsigned metadata (like WorkspaceID/UID): flipping it cannot bypass
	// approval or run a different command — git and test runners are forced back
	// onto pipes regardless, and Windows rejects tty outright. See
	// EXECUTION_LIVENESS_REDESIGN.md → PTY mode.
	Tty bool `json:"tty,omitempty"`

	rawCliAgentCatalog json.RawMessage
}

func (cmd *commandMsg) UnmarshalJSON(data []byte) error {
	type commandMsgJSON commandMsg
	var decoded commandMsgJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*cmd = commandMsg(decoded)

	var raw struct {
		CliAgentCatalog json.RawMessage `json:"cliAgentCatalog"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.CliAgentCatalog) > 0 {
		cmd.rawCliAgentCatalog = append(json.RawMessage(nil), raw.CliAgentCatalog...)
	}
	return nil
}

/* Outgoing result payload (matches backend publishResult struct) */
type resultMsg struct {
	ID              string        `json:"id"`
	WorkspaceID     string        `json:"workspaceID,omitempty"` // Workspace scope for audit trail
	UID             string        `json:"uid"`
	AgentID         string        `json:"agentId,omitempty"` // Agent ID for version updates on ping
	Output          string        `json:"output"`
	Status          string        `json:"status"` // "success" | "partial" | "error" | "denied" | "rate_limited" | "unauthorized"
	Ts              int64         `json:"ts"`
	Version         string        `json:"version,omitempty"`      // Terminal app version
	Cwd             string        `json:"cwd,omitempty"`          // Working directory: post-execution for success results, requested cwd for rejections
	Files           []FileInfo    `json:"files,omitempty"`        // Uploaded file metadata
	UploadErrors    []UploadError `json:"uploadErrors,omitempty"` // File upload failures
	RejectionReason string        `json:"rejectionReason,omitempty"`
	// Echoed back on rejections so terminal-service's persistRejection() can
	// store what the AI agent actually tried. Empty otherwise — successful
	// results don't need to repeat the command. Both are passed through
	// redactSensitiveData() before send so credentials/tokens that the LLM
	// embedded in args (e.g. `git push https://x:TOKEN@host/...`) don't end
	// up in the rejected_terminal_commands collection.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Session fields (for interactive CLI agent sessions)
	Type       string `json:"type,omitempty"`       // "result"|"stream"|"prompt"|"session_ended"|"codex_appserver_started"|"codex_appserver_message"|"codex_appserver_stderr"|"codex_appserver_error"|"codex_appserver_ended"|"grok_acp_started"|"grok_acp_message"|"grok_acp_stderr"|"grok_acp_error"|"grok_acp_ended"|"__cli_usage_refresh_result__"
	SessionID  string `json:"sessionID,omitempty"`  // Session identifier
	ExitCode   int    `json:"exitCode,omitempty"`   // Process exit code (for session_ended / codex_appserver_ended / grok_acp_ended)
	PromptText string `json:"promptText,omitempty"` // The question/approval text from CLI
	PromptType string `json:"promptType,omitempty"` // "permission"|"question"|"unknown"
	Seq        int    `json:"seq,omitempty"`        // Ordering sequence number for streaming

	// __cli_usage_refresh_result__ payload — populated only when
	// Type == "__cli_usage_refresh_result__". The backend's results
	// subscriber routes on Type and reads these directly.
	RefreshID string `json:"refreshId,omitempty"`
	// Pointer (not bool) because the zero value `false` is meaningful —
	// it's how the handled-failure path (provider timeout/panic) tells
	// the backend to set cliUsageLastFailedAt. A non-pointer bool with
	// `omitempty` would drop `success: false` from the marshaled JSON,
	// so the backend would see an absent field and fall through to its
	// legacy "no explicit signal" handling instead of the failure
	// branch. nil = field absent (non-refresh result), &true / &false =
	// explicit success/failure on a refresh result.
	Success *bool `json:"success,omitempty"`
	// No omitempty — an empty cliAgents slice ("agent has zero CLI
	// providers installed") is a legitimate successful poll, not the
	// same as "field absent". The backend's result handler treats the
	// presence of the array as the signal to advance
	// cliUsageLastCheckedAt; omitempty would force every empty-success
	// poll down the handled-failure path.
	CliAgents []cliAgentUsage      `json:"cliAgents"`
	Errors    []cliAgentUsageError `json:"errors,omitempty"`

	// __env_inspect_result__ payload — populated only when
	// Type == "__env_inspect_result__". Carries the read-only workstation
	// readiness report (state + friendly findings + full specs) for the
	// Environment Setup capability. Echoes RefreshID so terminal-service can
	// correlate the response with its pending inspection request.
	Readiness *ReadinessReport `json:"readiness,omitempty"`
}

/*
--------------------------------------------------------------------------

	StartPubSubLoop – reconnection wrapper with exponential backoff
	--------------------------------------------------------------------------
*/
// pubsubLoopRunning prevents duplicate StartPubSubLoop goroutines from stacking
// up when the tray "Reconnect to cloud" toggle fires while a previous loop is
// still alive (e.g., waiting on offlineChan). Without this guard, repeated
// toggles would spin up N goroutines all racing to Subscribe on the same
// subscription and fighting for the same offlineChan token.
var pubsubLoopRunning sync.Mutex

func StartPubSubLoop(cfg *Config) {
	fmt.Println("[pubsub] StartPubSubLoop called")
	fmt.Printf("[pubsub] Config: ProjectID=%s, Subscription=%s, Topic=%s\n", cfg.ProjectID, cfg.CommandsSubscription, cfg.ResultsTopic)

	if cfg.ProjectID == "" {
		fmt.Println("[pubsub] disabled – project_id empty")
		return
	}

	// Only one reconnection loop at a time. TryLock so a second call becomes
	// a no-op instead of blocking — the live loop is already responsible for
	// picking up the online signal we just sent on offlineChan.
	if !pubsubLoopRunning.TryLock() {
		fmt.Println("[pubsub] loop already running, skipping duplicate start")
		return
	}
	defer pubsubLoopRunning.Unlock()

	// Reconnection loop with exponential backoff
	backoff := time.Second        // Initial: 1 second
	maxBackoff := 5 * time.Minute // Max: 5 minutes

	for {
		// Check for shutdown
		select {
		case <-shutdownChan:
			fmt.Println("[pubsub] shutdown signal received")
			return
		default:
		}

		// Check if offline mode is enabled
		if IsOffline() {
			fmt.Println("[pubsub] offline mode - waiting to come online...")
			select {
			case <-shutdownChan:
				return
			case online := <-offlineChan:
				if !online {
					continue // Still offline
				}
				fmt.Println("[pubsub] coming online...")
				backoff = time.Second // Reset backoff
			}
		}

		// Attempt connection
		connStart := time.Now()
		err := runPubSubConnection(cfg)

		// Check if we went offline intentionally
		if IsOffline() {
			fmt.Println("[pubsub] disconnected (offline mode)")
			continue
		}

		if err == nil {
			// Clean shutdown
			return
		}

		// Reset backoff if the connection was healthy for a while (transient blip)
		if time.Since(connStart) > 30*time.Second {
			backoff = time.Second
		}

		fmt.Printf("[pubsub] connection lost: %v\n", err)

		// Check if this is an "Unknown agent" error - means registration is invalid
		errStr := err.Error()
		if strings.Contains(errStr, "Unknown agent") || strings.Contains(errStr, "invalid_client") {
			fmt.Printf("%s[pubsub] Terminal connection was removed via the website%s\n", colorRed, colorReset)
			fmt.Printf("%s[pubsub] Clearing local registration. Please re-register the device.%s\n", colorYellow, colorReset)

			// Clear registration credentials from config
			cfg.AgentID = ""
			cfg.CommandSecret = ""
			cfg.UserID = ""
			cfg.RegisteredAt = ""
			cfg.TokenEndpoint = ""
			cfg.WIFAudience = ""
			cfg.WIFServiceAccount = ""
			if err := cfg.Save(ConfigPath()); err != nil {
				fmt.Printf("[pubsub] Failed to save config: %v\n", err)
			}

			// Notify main.go to update the Register Device menu item
			select {
			case RegistrationInvalidChan <- true:
			default:
				// Channel full, skip (non-blocking)
			}

			// Show error dialog to user
			if IsSystrayReady() {
				ShowErrorDialog("Terminal Disconnected",
					"This terminal's connection was removed via the website.\n\n"+
						"Please click 'Register Device' in the tray menu to reconnect.")
				systray.SetTooltip(EnvDisplayName + " – Not Registered")
			}

			// Stop the Pub/Sub loop - user needs to re-register
			return
		}

		fmt.Printf("[pubsub] reconnecting in %v...\n", backoff)
		if IsSystrayReady() {
			systray.SetTooltip(EnvDisplayName + " – Reconnecting...")
		}

		// Wait for backoff or shutdown/offline signal
		select {
		case <-shutdownChan:
			return
		case offline := <-offlineChan:
			if offline {
				fmt.Println("[pubsub] going offline...")
				continue
			}
		case <-time.After(backoff):
		}

		// Exponential backoff with jitter (1s → 1.5s → 2.25s → ... → 5min max)
		backoff = time.Duration(float64(backoff) * 1.5)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		// Add ±25% jitter to prevent thundering herd when multiple terminals reconnect
		jitter := time.Duration(rand.Int63n(int64(backoff)/2)) - backoff/4
		backoff += jitter
	}
}

// handleCLIUsageRefreshCommand handles the demand-driven
// __cli_usage_refresh__ command from the backend's Active/Idle state
// machine. It always publishes a __cli_usage_refresh_result__ message
// (success or handled-failure) so the backend's in-flight marker is
// never orphaned.
//
// Wrapped in defer-recover so a panic in the dispatch path itself (not
// just per-provider) still surfaces a failure result and clears the
// server-side in-flight state. Without this, a panicking handler would
// leave the device's cliUsageInFlightSince stuck until the 60s timeout.
//
// Returns nil on a clean completion (result published OR offline-skip),
// non-nil when the result publish itself failed. The caller uses this
// to decide ack vs nack: a failed publish must nack so Pub/Sub
// redelivers the command and the backend's in-flight marker isn't left
// stuck waiting for a result that will never arrive.
func handleCLIUsageRefreshCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) (publishErr error) {
	persistCLIAgentCatalogUpdate(cfg, cmd.CliAgentCatalog, "pubsub", "refresh command")
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%s[pubsub] panic in CLI usage refresh handler: %v%s\n", colorRed, r, colorReset)
			res := makeCLIUsageRefreshFailureResult(cmd, cfg, fmt.Sprintf("panic: %v", r))
			if err := publishMsg(ctx, topic, res); err != nil {
				fmt.Printf("%s[pubsub] Failed to publish panic refresh result: %v%s\n", colorRed, err, colorReset)
				publishErr = err
			}
		}
	}()

	if IsOffline() {
		// Don't publish a result in offline mode — the backend's
		// guards already short-circuit dispatch in this case, and any
		// in-flight marker on the server is the result of a race that
		// the next scheduler tick will resolve. Logging only.
		fmt.Println("[pubsub] CLI usage refresh ignored — offline mode")
		return nil
	}

	usage, errs := GatherCLIAgentUsageOnly(ctx)
	// Refresh the cached CliAgents on the shared MachineInfo so the next
	// /auth/token doesn't POST the stale 6h-gather snapshot and revert
	// the backend's quota state. Only update on a successful poll —
	// preserving the prior cache on handled failure is intentional and
	// matches the backend's "don't overwrite snapshot on failure" rule.
	if len(errs) == 0 {
		SetCachedCLIAgents(usage)
	}
	// success is "we polled successfully", NOT "we found something". An
	// agent with zero providers installed (or zero providers that
	// matched our parsers) is a legitimate empty poll — the backend
	// should still advance cliUsageLastCheckedAt and clear any prior
	// failure marker. Only treat as failure when at least one provider
	// threw (panic / context cancel / parse error).
	success := len(errs) == 0

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		AgentID:     cfg.AgentID,
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "__cli_usage_refresh_result__",
		RefreshID:   cmd.RefreshID,
		Success:     &success,
		CliAgents:   usage,
		Errors:      errs,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[pubsub] Failed to publish refresh result: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}

// handleEnvInspectCommand fulfils a read-only __env_inspect__ demand command
// from terminal-service's Environment Setup flow. It gathers full machine info,
// derives a friendly readiness report, and publishes an __env_inspect_result__
// carrying the report + echoing RefreshID for correlation. Read-only — never
// mutates the workstation, so no allowlist/approval gating applies.
//
// Modeled on handleCLIUsageRefreshCommand: always publishes a result (even on
// panic) so the backend's pending-inspection marker is never orphaned, and
// returns non-nil only when the publish itself failed (caller nacks so Pub/Sub
// redelivers).
func handleEnvInspectCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) (publishErr error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("%s[pubsub] panic in env inspect handler: %v%s\n", colorRed, r, colorReset)
			res := makeEnvInspectResult(cmd, cfg, evaluateReadiness(nil))
			if err := publishMsg(ctx, topic, res); err != nil {
				publishErr = err
			}
		}
	}()

	// Bound the gather so a hung probe can't stall the inspection round trip.
	gatherCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	report := GatherReadinessOnly(gatherCtx)
	res := makeEnvInspectResult(cmd, cfg, report)
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[pubsub] Failed to publish env inspect result: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}

func makeEnvInspectResult(cmd commandMsg, cfg *Config, report ReadinessReport) resultMsg {
	agentID := cmd.AgentID
	if cfg != nil && cfg.AgentID != "" {
		agentID = cfg.AgentID
	}
	return resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		AgentID:     agentID,
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "__env_inspect_result__",
		RefreshID:   cmd.RefreshID,
		Status:      "success",
		Readiness:   &report,
	}
}

func makeCLIUsageRefreshFailureResult(cmd commandMsg, cfg *Config, message string) resultMsg {
	failure := false
	agentID := cmd.AgentID
	if cfg != nil && cfg.AgentID != "" {
		agentID = cfg.AgentID
	}
	return resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		AgentID:     agentID,
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "__cli_usage_refresh_result__",
		RefreshID:   cmd.RefreshID,
		Success:     &failure,
		Errors: []cliAgentUsageError{
			{Provider: "_dispatch", Message: message},
		},
	}
}

func publishCLIUsageRefreshFailure(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config, message string) error {
	fmt.Printf("%s[pubsub] CLI usage refresh rejected: %s%s\n", colorRed, message, colorReset)
	return publishMsg(ctx, topic, makeCLIUsageRefreshFailureResult(cmd, cfg, message))
}

// publishMsg marshals res and publishes it on topic using ctx.
// Logs and returns any error so callers can decide whether to ack or nack.
func publishMsg(ctx context.Context, topic *pubsub.Publisher, res resultMsg) error {
	bytes, err := json.Marshal(res)
	if err != nil {
		fmt.Printf("%s[aiexpedite] Failed to marshal result: %v%s\n", colorRed, err, colorReset)
		return err
	}
	if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
		fmt.Printf("%s[aiexpedite] Publish error: %v%s\n", colorRed, err, colorReset)
		return err
	}
	return nil
}

/*
--------------------------------------------------------------------------

	runPubSubConnection – handles a single connection attempt
	Returns nil on clean shutdown, error on connection failure
	--------------------------------------------------------------------------
*/
func runPubSubConnection(cfg *Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for shutdown or offline signal.
	// ctx.Done() arm ensures this goroutine exits when the connection ends
	// normally (e.g. sub.Receive returns), preventing a goroutine leak.
	//
	// When offline is signaled we cancel the receive context immediately so
	// sub.Receive returns and the loop stops processing inbound messages —
	// this is what guarantees no late pong escapes after disconnect.
	go func() {
		select {
		case <-shutdownChan:
			fmt.Println("[pubsub] shutdown received — cancelling subscription context")
			cancel()
		case offline := <-offlineChan:
			if offline {
				fmt.Println("[pubsub] offline signal received — cancelling subscription context")
				// State may already be set by the caller; calling SetOffline
				// here is idempotent and ensures any concurrent ping handler
				// sees IsOffline() == true on its next check.
				SetOffline(true)
				cancel()
			} else {
				// Re-queue the online signal for the main loop
				select {
				case offlineChan <- false:
				default:
				}
			}
		case <-ctx.Done():
			// Connection ended cleanly; nothing to do, just exit.
		}
	}()

	fmt.Println("[pubsub] creating Pub/Sub client...")

	// Build client options - use WIF if configured, otherwise fall back to ADC
	var clientOpts []option.ClientOption
	if IsWIFConfigured(cfg) {
		fmt.Println("[pubsub] using Workload Identity Federation for authentication")
		tokenSource := NewWIFTokenSource(cfg)
		// Wrap with ReuseTokenSource to cache and auto-refresh tokens
		clientOpts = append(clientOpts, option.WithTokenSource(
			oauth2.ReuseTokenSource(nil, tokenSource),
		))
	} else {
		fmt.Println("[pubsub] using Application Default Credentials (ADC)")
	}

	client, err := pubsub.NewClient(ctx, cfg.ProjectID, clientOpts...)
	if err != nil {
		return fmt.Errorf("client creation failed: %w", err)
	}
	defer client.Close()

	fmt.Println("[pubsub] client created successfully")

	// v2 requires fully-qualified resource names.
	subName := fmt.Sprintf("projects/%s/subscriptions/%s", cfg.ProjectID, cfg.CommandsSubscription)
	topicName := fmt.Sprintf("projects/%s/topics/%s", cfg.ProjectID, cfg.ResultsTopic)

	sub := client.Subscriber(subName)
	// Use configurable MaxOutstandingMessages for parallel processing.
	// (v2 is always asynchronous — the v1 Synchronous flag no longer exists.)
	sub.ReceiveSettings.MaxOutstandingMessages = cfg.MaxOutstandingMessages
	if sub.ReceiveSettings.MaxOutstandingMessages <= 0 {
		sub.ReceiveSettings.MaxOutstandingMessages = 10 // Default to 10 for faster stale-command drain
	}
	fmt.Printf("[pubsub] MaxOutstandingMessages set to %d\n", sub.ReceiveSettings.MaxOutstandingMessages)

	topic := client.Publisher(topicName)

	fmt.Printf("[pubsub] connected to subscription: %s\n", cfg.CommandsSubscription)
	if IsSystrayReady() {
		systray.SetTooltip(EnvDisplayName + " – Connected")
	}

	fmt.Printf("[pubsub] listening for commands on: %s\n", cfg.CommandsSubscription)
	err = sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
		// Panic recovery to prevent app crash on unhandled errors
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[pubsub] PANIC in message handler: %v\n", r)
				m.Nack() // Let Pub/Sub redeliver
			}
		}()

		// Reject oversized messages as a defense against memory exhaustion from
		// malformed or unexpectedly large Pub/Sub messages.
		//
		// Sized to accommodate large positional args — specifically the
		// kickoff-brief pattern in ai-service's TerminalWithFeatureDetailsTool,
		// which passes a verbatim feature spec (tens to hundreds of KB) as
		// args[0] so claude can be seeded with the signed-off doc without
		// AI paraphrasing. Briefs over ~78 KB were previously silently dropped
		// here and stranded the session at status=starting forever.
		//
		// 1 MB matches the next natural ceiling for normal commands:
		// Firestore's per-document cap, which terminal-service hits when it
		// writes args onto the terminalSession doc. If something ever exceeds
		// 1 MB the Firestore write will throw a clear error instead of the
		// silent pubsub drop.
		//
		// CLI usage refresh commands can carry the database-backed catalog and
		// do not write args to terminalSession, so they share the larger bounded
		// catalog cap used by /auth/token. Anything beyond that cap is rejected
		// before normal processing, with a refresh failure result when possible
		// so the backend's in-flight marker is not orphaned.
		if len(m.Data) > maxPubSubCatalogMessageBytes {
			fmt.Printf("%s[aiexpedite] Oversized message rejected (%d bytes)%s\n",
				colorRed, len(m.Data), colorReset)

			// Surface the rejection to terminal-service so the session
			// fails fast with a clear error instead of stranding at
			// status=starting forever — that silent-drop mode is exactly
			// what raising the 64 KB cap was meant to fix, so we don't
			// want to re-introduce it for payloads above 1 MB.
			//
			// Best-effort parse: pubsub already caps payloads at 10 MB,
			// so the memory cost of unmarshaling a rejected message is
			// bounded. If the JSON is malformed, or if the command is neither
			// a refresh nor session-routed command, fall through to the bare
			// ack because we have nothing to fail.
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err == nil {
				message := commandPayloadTooLargeMessage(len(m.Data), maxPubSubCatalogMessageBytes)
				if cmd.Command == "__cli_usage_refresh__" {
					if err := publishCLIUsageRefreshFailure(ctx, topic, cmd, cfg, message); err != nil {
						m.Nack()
					} else {
						m.Ack()
					}
					return
				}
				if cmd.SessionID != "" {
					publishSessionError(ctx, topic, cmd, message)
				}
			}

			m.Ack() // Ack so it isn't redelivered forever
			return
		}

		// Parse command silently (verbose logging removed in v0.4.12)
		var cmd commandMsg
		if err := json.Unmarshal(m.Data, &cmd); err != nil {
			fmt.Printf("%s[aiexpedite] Bad command payload: %v%s\n", colorRed, err, colorReset)
			if len(m.Data) > maxPubSubCommandMessageBytes {
				m.Ack()
				return
			}
			m.Nack()
			return
		}
		if messageSizeLimit := pubSubMessageSizeLimit(cmd); len(m.Data) > messageSizeLimit {
			fmt.Printf("%s[aiexpedite] Oversized message rejected (%d bytes)%s\n",
				colorRed, len(m.Data), colorReset)

			message := commandPayloadTooLargeMessage(len(m.Data), messageSizeLimit)
			if cmd.Command == "__cli_usage_refresh__" {
				if err := publishCLIUsageRefreshFailure(ctx, topic, cmd, cfg, message); err != nil {
					m.Nack()
				} else {
					m.Ack()
				}
				return
			}
			if cmd.SessionID != "" {
				publishSessionError(ctx, topic, cmd, message)
			}
			m.Ack()
			return
		}

		// ─── Offline Guard ───────────────────────────────────────────────
		// Once the agent is in offline mode (user clicked "Disconnect from
		// cloud" or graceful shutdown is in progress), suppress ALL command
		// processing — including pings. A late pong arriving after the
		// backend has set offlineSince would otherwise resurrect this device
		// in the eyes of /device/has-online and reservation.service. We Ack
		// to drain the message from the subscription instead of Nacking,
		// since Nacking would just trigger redelivery and a hot loop.
		if IsOffline() {
			fmt.Printf("%s[pubsub] Offline mode — suppressing %s (id=%s) and Acking%s\n",
				colorYellow, cmd.Command, cmd.ID, colorReset)
			m.Ack()
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Priority Ping Handler ───────────────────────────────────────
		// Process pings BEFORE staleness/rate-limit/signature checks so that
		// online-status pings are never delayed by a backlog of stale commands
		// in the Pub/Sub queue.
		if cmd.Command == "__ping__" {
			// Defense-in-depth: re-check IsOffline immediately before
			// publishing the pong. SetOffline can flip while we're in this
			// handler, and a stale pong is the exact failure mode that
			// resurrects a disconnected device.
			if IsOffline() {
				fmt.Printf("%s[pubsub] Offline mode — dropping ping pong (id=%s)%s\n",
					colorYellow, cmd.ID, colorReset)
				m.Ack()
				return
			}
			res := resultMsg{
				ID:          cmd.ID,
				WorkspaceID: cmd.WorkspaceID,
				UID:         cmd.UID,
				AgentID:     cmd.AgentID,
				Output:      "pong",
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
			}
			if err := publishMsg(ctx, topic, res); err != nil {
				fmt.Printf("%s[aiexpedite] Ping publish error: %v%s\n", colorRed, err, colorReset)
			}
			m.Ack()
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Command Staleness Check ──────────────────────────────────────
		// Reject commands older than maxCommandAgeSec to prevent processing
		// stale queued commands when terminal reconnects after being offline.
		//
		// Exempt server-internal demand commands (__cli_usage_refresh__,
		// __env_inspect__): they MUST always publish a correlated result
		// carrying refreshId so the backend can clear its in-flight / pending
		// marker. A generic "stale" rejection here would orphan that marker and
		// block the loop until its timeout. See isInternalDemandCommand.
		if !isInternalDemandCommand(cmd.Command) && isCommandStale(cmd.Ts) {
			ageSec := (time.Now().UnixMilli() - cmd.Ts) / 1000
			fmt.Printf("%s[aiexpedite] Stale command rejected (age: %ds, max: %ds)%s\n",
				colorYellow, ageSec, maxCommandAgeSec, colorReset)

			// Send stale response back to user
			res := makeRejectionResult(
				cmd,
				cfg.AgentID,
				"stale",
				"STALE",
				fmt.Sprintf("Command rejected: too old (%d seconds, max %d seconds). Terminal may have been offline.", ageSec, maxCommandAgeSec),
			)
			if err := publishMsg(ctx, topic, res); err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Per-UID Rate Limiting ─────────────────────────────────────────
		// Exempt server-internal demand commands for the same reason as the
		// staleness check above — a "rate_limited" rejection would not carry
		// refreshId and would orphan the backend's in-flight / pending marker.
		if !isInternalDemandCommand(cmd.Command) && !checkRateLimit(cmd.UID, cmd.AgentID, cfg) {
			fmt.Printf("%s[aiexpedite] Rate limit exceeded%s\n", colorYellow, colorReset)

			// Send rate_limited response back to user for immediate feedback
			res := makeRejectionResult(
				cmd,
				cfg.AgentID,
				"rate_limited",
				"RATE_LIMITED",
				"Command rate limit exceeded. Please wait before retrying.",
			)
			if err := publishMsg(ctx, topic, res); err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Command Signature Verification ──────────────────────────────
		// If agent has a secret configured, ALL commands MUST be signed (strict mode)
		if cfg.CommandSecret != "" {
			// Check if command is targeted at this agent.
			// With per-agent subscriptions this should rarely happen — silently ack
			// instead of publishing a competing "unauthorized" result that races
			// with the correct agent's response.
			if cmd.AgentID != "" && cmd.AgentID != cfg.AgentID {
				fmt.Printf("%s[aiexpedite] Ignoring command for different agent (got %s, I am %s)%s\n",
					colorYellow, cmd.AgentID, cfg.AgentID, colorReset)
				m.Ack()
				return
			}

			// Verify signature (strict mode - no signature = reject)
			if cmd.Signature == "" {
				fmt.Printf("%s[aiexpedite] Command missing signature%s\n", colorRed, colorReset)
				if isSigFailRateLimited() {
					fmt.Printf("%s[aiexpedite] Rate-limiting unauthorized responses%s\n", colorYellow, colorReset)
					m.Ack()
					return
				}
				res := makeRejectionResult(
					cmd,
					cfg.AgentID,
					"unauthorized",
					"UNAUTHORIZED",
					"Command rejected: signature required but not provided",
				)
				if err := publishMsg(ctx, topic, res); err != nil {
					m.Nack()
				} else {
					m.Ack()
				}
				return
			}

			if !verifySignature(cmd, cfg.CommandSecret) {
				fmt.Printf("%s[aiexpedite] Invalid command signature%s\n", colorRed, colorReset)
				if isSigFailRateLimited() {
					fmt.Printf("%s[aiexpedite] Rate-limiting unauthorized responses%s\n", colorYellow, colorReset)
					m.Ack()
					return
				}
				res := makeRejectionResult(
					cmd,
					cfg.AgentID,
					"unauthorized",
					"UNAUTHORIZED",
					"Command rejected: invalid signature",
				)
				if err := publishMsg(ctx, topic, res); err != nil {
					m.Nack()
				} else {
					m.Ack()
				}
				return
			}
			// Signature verified - proceed silently
		}
		// ─────────────────────────────────────────────────────────────────

		if cmd.Command == "__cli_usage_refresh__" {
			// Demand-driven CLI usage refresh from the backend's
			// Active/Idle state machine (see terminal-service's
			// cliUsageActiveLoop.service.js). We gather CLI-usage ONLY
			// — no CPU/memory/runtime probing — and publish a
			// __cli_usage_refresh_result__ message carrying the
			// refreshId so the backend can drop stale results and
			// clear the in-flight marker.
			//
			// IMPORTANT: always publish a result, even on failure. The
			// backend's in-flight marker would otherwise stick until
			// the 60s timeout and the next scheduled tick.
			//
			// Nack on publish failure so Pub/Sub redelivers the
			// command — otherwise a transient publish error would
			// leave the backend's cliUsageInFlightSince stuck and the
			// next scheduler tick blocked behind the in-flight guard.
			if err := handleCLIUsageRefreshCommand(ctx, topic, cmd, cfg); err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
			return
		}

		if cmd.Command == "__env_inspect__" {
			// Read-only workstation inspection for the Environment Setup
			// capability. Gathers machine info + readiness verdict and
			// publishes an __env_inspect_result__ carrying the report. Like
			// __cli_usage_refresh__ it always publishes a result so the
			// backend's pending marker never sticks; nack on publish failure
			// so Pub/Sub redelivers.
			if err := handleEnvInspectCommand(ctx, topic, cmd, cfg); err != nil {
				m.Nack()
			} else {
				m.Ack()
			}
			return
		}

		// ─── Interactive Session Routing ─────────────────────────────────
		// Route session_* and codex_appserver_* commands to their respective
		// managers instead of shell execution. Long-running entry-point
		// commands (session_start and codex_appserver_start) are gated
		// through the allowlist + user approval dialog before dispatch; all
		// other interactive commands (session_input / session_signal /
		// session_end / codex_appserver_send / codex_appserver_end) flow
		// through directly because they target an already-allowed session.
		if cmd.Type != "" && cmd.Type != "execute" {
			if proceed := gateSessionEntryCommand(ctx, topic, m, cmd, cfg); !proceed {
				return
			}
			handleSessionCommand(ctx, topic, cmd, cfg)
			m.Ack()
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Command Allow List Validation ───────────────────────────────
		// A high-risk (destructive) Environment Setup step ALWAYS shows the
		// native approval dialog — in addition to the chat-side scoped
		// confirmation the backend already enforced — even if the raw command
		// would otherwise be allowlisted. The riskLevel is HMAC-signed so it
		// can't be stripped to skip this gate. See requiresNativeApprovalForStep.
		if shouldGateExecuteCommand(cfg, defaultAllowList, cmd.Command, cmd.Args) ||
			requiresNativeApprovalForStep(cmd) {
			// Command not in allow list (or high-risk step) - show approval dialog

			// Get timeout settings from config
			timeoutSec := cfg.ApprovalTimeoutSec
			if timeoutSec <= 0 {
				timeoutSec = 60
			}

			// Show approval dialog
			result := commandApprovalDialogFn(cmd.Command, cmd.Args, timeoutSec)

			// Resolve the allow-on-timeout policy — deliberately NOT honored for
			// destructive Environment Setup steps (see applyTimeoutPolicy).
			result = applyTimeoutPolicy(result, cfg, cmd)

			switch result {
			case ApprovalDeny:
				fmt.Printf("%s[aiexpedite] Command denied by user%s\n", colorYellow, colorReset)

				// Send denial result back to backend
				res := makeRejectionResult(
					cmd,
					cfg.AgentID,
					"denied",
					"ALLOWLIST_DENIED",
					"Command denied by user: not in allow list",
				)
				if err := publishMsg(ctx, topic, res); err != nil {
					m.Nack()
				} else {
					m.Ack()
				}
				return

			case ApprovalAlways:
				// defaultAllowList can be nil here when the allowlist is
				// disabled and this dialog was reached solely via
				// requiresNativeApprovalForStep (forced-native high-risk step) —
				// InitAllowList is skipped in that config. Guard persistence
				// like the session path does rather than panic (which would
				// nack/redeliver the setup step forever).
				if defaultAllowList != nil {
					pattern := GeneratePatternFromCommand(cmd.Command, cmd.Args)
					if err := defaultAllowList.AddPattern(pattern); err != nil {
						fmt.Printf("%s[aiexpedite] Failed to add pattern to allow list: %v%s\n", colorYellow, err, colorReset)
					}
				}
				// Fall through to execute

			case ApprovalOnce:
				// User approved - proceed silently
			}
		}
		// ─────────────────────────────────────────────────────────────────

		// Display decoded command in green with ">" prefix
		cmdDisplay := cmd.Command
		if len(cmd.Args) > 0 {
			cmdDisplay += " " + strings.Join(cmd.Args, " ")
		}
		// Decode Base64-encoded PowerShell commands for readability
		if strings.ToLower(cmd.Command) == "powershell" && len(cmd.Args) >= 2 {
			if strings.ToLower(cmd.Args[0]) == "-encodedcommand" {
				cmdDisplay = decodeBase64PowerShell(cmd.Args[1])
			}
		}
		// Always redact sensitive data before printing — debug mode shows more
		// detail but still must not leak credentials to the console.
		fmt.Printf("%s> %s%s\n", colorGreen, redactSensitiveData(cmdDisplay), colorReset)

		// Debug mode: show raw command details (redacted)
		if cfg.DebugMode {
			redactedArgs := make([]string, len(cmd.Args))
			for i, a := range cmd.Args {
				redactedArgs[i] = redactSensitiveData(a)
			}
			fmt.Printf("%s[DEBUG] Raw command: %s%s\n", colorMagenta, redactSensitiveData(cmd.Command), colorReset)
			fmt.Printf("%s[DEBUG] Args: %v%s\n", colorMagenta, redactedArgs, colorReset)
			fmt.Printf("%s[DEBUG] Cwd: %s%s\n", colorMagenta, cmd.Cwd, colorReset)
		}

		// Capture command start time for the file-upload mtime filter — any
		// media file written under workDir during this command's lifetime
		// gets picked up, regardless of which subdirectory the framework or
		// the user's custom config chose.
		cmdStartedAt := time.Now()

		// Execute command (silently - no internal logs). Routes tty=true to the
		// PTY path; all other commands take the hardened pipe path.
		out, execErr := executeTerminalCommand(cfg, cmd)

		// Debug mode: show raw output details (redacted)
		if cfg.DebugMode {
			fmt.Printf("%s[DEBUG] Output length: %d bytes%s\n", colorMagenta, len(out), colorReset)
			fmt.Printf("%s[DEBUG] Error: %v%s\n", colorMagenta, execErr, colorReset)
			// Show raw output with visible control characters (redacted)
			fmt.Printf("%s[DEBUG] Raw output (quoted): %q%s\n", colorMagenta, redactSensitiveData(out), colorReset)
		}

		// Show output or terminal error — always redact before printing to
		// the local console so that credentials in error messages (e.g. a
		// failed psql connection string) are never written to the screen.
		if execErr != nil {
			fmt.Printf("%s%s%s\n", colorRed, redactSensitiveData(execErr.Error()), colorReset)
			if out != "" {
				fmt.Println(redactSensitiveData(out))
			}
		} else if out != "" {
			fmt.Println(redactSensitiveData(out))
		}

		// Redact sensitive data from output before publishing to Pub/Sub
		redactedOut := redactSensitiveData(out)

		res := resultMsg{
			ID:          cmd.ID,
			WorkspaceID: cmd.WorkspaceID,
			UID:         cmd.UID,
			Output:      redactedOut,
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Cwd:         getTrackedCwd(),
		}
		if execErr != nil {
			res.Status = "error"
			res.Output = redactSensitiveData(execErr.Error()) + "\n" + redactedOut
		}

		// File upload integration. Note: this block intentionally runs even
		// when res.Status == "error". A failing UI test that captured a
		// screenshot right before crashing is exactly the case where we
		// MOST want the image to reach the orchestrator — gating on a
		// successful exit code was the previous behavior, and it dropped
		// screenshots from the crashes we most needed to debug.
		if cfg.EnableFileUpload {
			effectiveDir := getTrackedCwd()
			if effectiveDir == "" {
				effectiveDir = cmd.Cwd
			}
			if effectiveDir == "" && cfg != nil {
				effectiveDir = cfg.WorkingDirectory
			}
			files := detectOutputFilesSince(effectiveDir, cmdStartedAt)
			if len(files) > 0 {
				// Security: Block file upload if workspaceID is missing
				workspaceID := extractWorkspaceID(cmd)
				if workspaceID == "" {
					fmt.Println("[file-upload] BLOCKED - no workspaceID provided (security: refusing to upload to default bucket)")
				} else {
					fmt.Printf("[file-upload] Detected %d output files, uploading to GCS (workspace: %s)...\n", len(files), workspaceID)

					// Get reusable GCS client (much faster than creating per command)
					// Use a background context for uploads: the message handler ctx may be
					// cancelled mid-upload by the Pub/Sub library if acknowledgement takes
					// too long, which would silently abort the transfer.
					uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 5*time.Minute)
					// Explicit call rather than defer: defer inside the sub.Receive
					// callback goroutine would not fire until the receive loop exits,
					// leaking the context for the entire Pub/Sub connection lifetime.
					storageClient, storageErr := GetStorageClient(uploadCtx)
					if storageErr != nil {
						uploadCancel()
						fmt.Printf("[file-upload] Failed to get storage client: %v\n", storageErr)
					} else {
						// Don't close - client is reused globally

						// Create logger
						logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

						// Upload files
						uploadResult := UploadFiles(
							uploadCtx,
							storageClient,
							cfg.StorageBucket,
							files,
							workspaceID,
							cmd.ID,
							logger,
						)
						uploadCancel()

						res.Files = uploadResult.Successful
						res.UploadErrors = uploadResult.Failed

						if len(uploadResult.Failed) > 0 && len(uploadResult.Successful) > 0 {
							res.Status = "partial"
						} else if len(uploadResult.Failed) > 0 && len(uploadResult.Successful) == 0 {
							res.Status = "error"
							res.Output += fmt.Sprintf("\n[file-upload] All file uploads failed: %d errors", len(uploadResult.Failed))
						}

						fmt.Printf("[file-upload] Upload complete: %d successful, %d failed\n",
							len(uploadResult.Successful), len(uploadResult.Failed))
						for _, ue := range uploadResult.Failed {
							fmt.Printf("[file-upload] FAILED: %s - %s\n", ue.File, ue.Error)
						}
					}
				}
			}
		}

		// Publish result using a background context to ensure delivery even during
		// shutdown — the message handler's ctx may be cancelled before we finish.
		// Explicit cancel (not defer): defer inside sub.Receive callback goroutines
		// does not fire until the receive loop exits, leaking the context.
		publishCtx, publishCancel := context.WithTimeout(context.Background(), 30*time.Second)
		pubErr := publishMsg(publishCtx, topic, res)
		publishCancel()
		if pubErr != nil {
			// Publish failed — Nack so Pub/Sub redelivers and the agent retries.
			m.Nack()
			return
		}
		m.Ack()
	})

	// Return nil on clean shutdown, error otherwise
	if ctx.Err() != nil && !IsOffline() {
		return nil // Clean shutdown
	}
	return err
}

/* --------------------------------------------------------------------------
   PowerShell CLIXML Handling
   -------------------------------------------------------------------------- */

// filterCLIXML removes CLIXML blocks from output, keeping only meaningful content.
// CLIXML is PowerShell's XML format for progress/verbose messages on stderr.
func filterCLIXML(content string) string {
	lines := strings.Split(content, "\n")
	var filtered []string
	inCLIXML := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Start of CLIXML block
		if strings.HasPrefix(trimmed, "#< CLIXML") {
			inCLIXML = true
			continue
		}

		// End of CLIXML block (closing tag)
		if inCLIXML && strings.HasPrefix(trimmed, "</Objs>") {
			inCLIXML = false
			continue
		}

		// Skip lines inside CLIXML
		if inCLIXML {
			continue
		}

		// Keep non-CLIXML content
		filtered = append(filtered, line)
	}

	return strings.Join(filtered, "\n")
}

/* --------------------------------------------------------------------------
   PowerShell Command Encoding
   -------------------------------------------------------------------------- */

// encodeForPowerShell converts a script to Base64 UTF-16LE format for use with
// PowerShell's -EncodedCommand parameter. This prevents all shell escaping issues
// with special characters like $, `, ", ', etc.
func encodeForPowerShell(script string) string {
	// Convert to UTF-16LE (PowerShell's expected encoding)
	utf16leRunes := utf16.Encode([]rune(script))
	bytes := make([]byte, len(utf16leRunes)*2)
	for i, r := range utf16leRunes {
		bytes[i*2] = byte(r)
		bytes[i*2+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

// decodeBase64PowerShell decodes a Base64 UTF-16LE encoded PowerShell script.
// Returns the decoded script or an error indicator if decoding fails.
func decodeBase64PowerShell(encoded string) string {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "[decode error]"
	}
	// Convert UTF-16LE to string
	if len(decoded)%2 != 0 {
		return "[invalid encoding]"
	}
	u16s := make([]uint16, len(decoded)/2)
	for i := 0; i < len(u16s); i++ {
		u16s[i] = uint16(decoded[i*2]) | uint16(decoded[i*2+1])<<8
	}
	return string(utf16.Decode(u16s))
}

// decodeBase64PowerShellStrict is the same as decodeBase64PowerShell but returns
// a real error instead of a magic sentinel string. Used by the stdin-pipe
// fallback path where a decode failure must abort the call rather than be
// silently forwarded to PowerShell as a literal "[decode error]" script.
func decodeBase64PowerShellStrict(encoded string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if len(decoded)%2 != 0 {
		return "", errors.New("invalid UTF-16LE encoding: odd byte length")
	}
	u16s := make([]uint16, len(decoded)/2)
	for i := 0; i < len(u16s); i++ {
		u16s[i] = uint16(decoded[i*2]) | uint16(decoded[i*2+1])<<8
	}
	return string(utf16.Decode(u16s)), nil
}

// encodedCommandFallbackThreshold is the maximum -EncodedCommand argument
// length we'll pass to powershell.exe before falling back to the temp-file
// path. Windows CreateProcess caps lpCommandLine at 32767 chars; 30000 leaves
// ~2KB headroom for the executable path, the leading flags, and a safety
// margin.
const encodedCommandFallbackThreshold = 30000

// Indirection points for tests: the dispatcher in runEncodedPowerShellCommand
// calls these vars rather than the implementations directly so unit tests can
// swap in spies to assert which transport was selected.
var (
	runEncodedPowerShellViaArgFn      = runEncodedPowerShellViaArg
	runPowerShellCommandViaTempFileFn = runPowerShellCommandViaTempFile
)

// runEncodedPowerShellCommand executes a Base64-encoded PowerShell script.
// This is the most reliable way to execute PowerShell commands as it completely
// bypasses all shell escaping issues.
//
// When the encoded script is small enough to fit in a Windows command line
// (<=encodedCommandFallbackThreshold chars) it goes through the standard
// -EncodedCommand argument path. Above that, Windows' CreateProcess cmdline
// cap (~32767 chars) starts rejecting the spawn with "The filename or
// extension is too long", so we decode the script to a temp .ps1 file and
// invoke `powershell.exe -File <path>` instead. The transport differs but
// the semantics (one-shot fresh process, default/empty stdin for any child
// tools spawned by the script, CLIXML filtering, error shape) are identical.
func runEncodedPowerShellCommand(encodedScript string, workDir string, timeout time.Duration) (string, error) {
	if len(encodedScript) > encodedCommandFallbackThreshold {
		script, decodeErr := decodeBase64PowerShellStrict(encodedScript)
		if decodeErr != nil {
			return "", fmt.Errorf("powershell temp-file fallback: decode failed: %w", decodeErr)
		}
		fmt.Printf("%s[aiexpedite] PowerShell script %d chars encoded — routing via temp .ps1 file (over -EncodedCommand cmdline limit)%s\n", colorCyan, len(encodedScript), colorReset)
		return runPowerShellCommandViaTempFileFn(script, workDir, timeout)
	}
	return runEncodedPowerShellViaArgFn(encodedScript, workDir, timeout)
}

// runEncodedPowerShellViaArg invokes powershell.exe with the script passed as
// the -EncodedCommand argument. This is the fast path used for any script
// whose encoded form fits comfortably under the Windows cmdline limit.
// It captures stdout and stderr separately and filters CLIXML progress
// messages to avoid false "exit status 1" errors when commands produce valid
// output.
func runEncodedPowerShellViaArg(encodedScript string, workDir string, timeout time.Duration) (string, error) {
	// `-OutputFormat Text` prevents PowerShell from serializing stderr as CLIXML
	// (XML error records) when stderr is piped to a non-console parent process.
	// Without it, any PowerShell error surfaces as `#< CLIXML <Objs ...>` noise
	// that leaks past filterCLIXML and back to the user.
	psArgs := []string{
		"-NoProfile",
		"-NonInteractive",
		"-OutputFormat", "Text",
		"-EncodedCommand",
		encodedScript,
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, "powershell.exe", psArgs...)
	hideWindow(c)
	// Headless hardening: authoritative non-interactive git/editor/credential env.
	// Command text is base64-encoded here, so no test-runner detection is
	// possible; the git safety overlay still applies.
	hardenNonAgentCommand(c, "")
	if workDir != "" {
		c.Dir = workDir
	}

	// Capture stdout and stderr separately to handle CLIXML filtering
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	// Register/deregister the PID so the orphan scanner doesn't flag this
	// short-lived spawn as an orphan if it overlaps with a scan cycle.
	if startErr := c.Start(); startErr != nil {
		return "", startErr
	}
	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:powershell-encoded")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}
	err := c.Wait()

	return assemblePowerShellOutput(stdout.String(), stderr.String(), err)
}

// runPowerShellCommandViaTempFile writes the (already decoded) script to a
// temp .ps1 file and invokes `powershell.exe -File <path>`. Used as a
// fallback when the Base64-encoded script is too large to fit on the Windows
// command line.
//
// We deliberately do NOT pipe the script through stdin to `-Command -`:
// PowerShell reading source from stdin shares that pipe with any child
// process started by the script (python, node, ssh, credential prompts,
// etc.), and a child reading stdin can consume the remaining PowerShell
// source instead of seeing the default/empty stdin it would have had on the
// -EncodedCommand path. The temp-file transport preserves the original
// stdin semantics. Error/output semantics match runEncodedPowerShellViaArg
// exactly.
func runPowerShellCommandViaTempFile(script string, workDir string, timeout time.Duration) (string, error) {
	tmp, err := os.CreateTemp("", "aiexpedite-ps-*.ps1")
	if err != nil {
		return "", fmt.Errorf("powershell temp-file fallback: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// UTF-8 BOM tells Windows PowerShell 5.x to read the file as UTF-8
	// rather than the legacy ANSI codepage, so non-ASCII characters in the
	// script survive intact.
	if _, err := tmp.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("powershell temp-file fallback: write BOM: %w", err)
	}
	if _, err := tmp.WriteString(script); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("powershell temp-file fallback: write script: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("powershell temp-file fallback: close temp: %w", err)
	}

	// `-ExecutionPolicy Bypass` is required because `-File` (unlike
	// `-EncodedCommand`) is subject to the machine's ExecutionPolicy. On
	// clients with the default `Restricted` policy, .ps1 scripts simply
	// won't run, so a large encoded command would silently fail where the
	// small-script `-EncodedCommand` path succeeded. Bypass is scoped to
	// this single process invocation and does not touch user/machine policy.
	psArgs := []string{
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-OutputFormat", "Text",
		"-File", tmpPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, "powershell.exe", psArgs...)
	hideWindow(c)
	// Headless hardening: authoritative non-interactive git/editor/credential env.
	hardenNonAgentCommand(c, "")
	if workDir != "" {
		c.Dir = workDir
	}

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	if startErr := c.Start(); startErr != nil {
		return "", startErr
	}
	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:powershell-tempfile")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}

	waitErr := c.Wait()
	return assemblePowerShellOutput(stdout.String(), stderr.String(), waitErr)
}

// assemblePowerShellOutput combines stdout/stderr from a one-shot PowerShell
// spawn, filters CLIXML noise out of stderr, and applies the shared
// error-suppression rule: a non-zero exit whose stderr was entirely CLIXML
// progress chatter is cosmetic, not a real failure.
func assemblePowerShellOutput(stdoutStr, stderrStr string, runErr error) (string, error) {
	filteredStderr := filterCLIXML(stderrStr)

	output := stdoutStr
	if strings.TrimSpace(filteredStderr) != "" {
		output += "\n" + filteredStderr
	}

	var finalErr error
	if runErr != nil {
		hasOnlyCLIXML := stderrStr != "" && strings.TrimSpace(filteredStderr) == ""
		if hasOnlyCLIXML {
			finalErr = nil
		} else {
			finalErr = runErr
		}
	}
	return output, finalErr
}

/* --------------------------------------------------------------------------
   cmd.exe Routing for Bash-Style Commands
   On Windows, commands containing && or || are routed through cmd.exe which
   natively supports these operators, avoiding PowerShell < 7 compatibility issues.
   -------------------------------------------------------------------------- */

// isBashStyleCommand detects commands that use bash/cmd syntax incompatible
// with PowerShell < 7, such as && chaining or || chaining.
func isBashStyleCommand(cmdLine string) bool {
	return strings.Contains(cmdLine, " && ") || strings.Contains(cmdLine, " || ")
}

// isPowerShellSpecificCommand checks if a command requires PowerShell.
// These are PowerShell cmdlets and syntax that do not work in cmd.exe.
func isPowerShellSpecificCommand(cmd string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(cmd))

	// Already explicitly prefixed with powershell
	if strings.HasPrefix(cmdLower, "powershell ") || strings.HasPrefix(cmdLower, "pwsh ") {
		return true
	}

	// PowerShell cmdlets follow Verb-Noun pattern
	psVerbs := []string{
		"get-", "set-", "new-", "remove-", "invoke-",
		"select-", "where-", "foreach-", "format-",
		"write-", "read-", "test-", "start-", "stop-",
		"import-", "export-", "add-", "clear-", "out-",
		"measure-", "sort-", "group-", "compare-",
		"convertto-", "convertfrom-",
	}

	// Check the first token of the command
	firstToken := cmdLower
	if idx := strings.IndexAny(cmdLower, " ;|"); idx != -1 {
		firstToken = cmdLower[:idx]
	}

	for _, verb := range psVerbs {
		if strings.HasPrefix(firstToken, verb) {
			return true
		}
	}

	// PowerShell-specific syntax
	if strings.Contains(cmdLower, "$env:") || strings.Contains(cmdLower, "$_") {
		return true
	}

	return false
}

// runViaShell executes a bash-style (&&/||) command on Windows by spawning a
// one-shot PowerShell process. Prefers pwsh.exe (PS 7+, supports && natively)
// and falls back to powershell.exe only when pwsh.exe is not on PATH.
// powershell.exe is used in preference to cmd.exe so that a failed `cd` (e.g.
// to a non-existent path) sets a non-zero exit code and the error propagates —
// cmd.exe's `cd` exits 0 even on failure, masking the problem.
func runViaShell(cmdLine string, workDir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	psExe := getFallbackPSExe() // prefers pwsh.exe when available
	// `-OutputFormat Text` prevents CLIXML error serialization — see
	// runEncodedPowerShellCommand for the full explanation.
	c := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-Command", cmdLine)
	hideWindow(c)
	// Headless hardening: authoritative non-interactive git/editor/credential env
	// so git/ssh/credential prompts fail fast rather than blocking.
	hardenNonAgentCommand(c, cmdLine)
	if workDir != "" {
		c.Dir = workDir
	}

	// Use Start/Wait instead of CombinedOutput so we can register the PID
	// with the orphan scanner. CombinedOutput would block before we can
	// access c.Process.
	var combined bytes.Buffer
	c.Stdout = &combined
	c.Stderr = &combined
	if err := c.Start(); err != nil {
		return "", err
	}
	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:shell")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}
	err := c.Wait()
	return combined.String(), err
}

// runLocalCommandUnix executes a command directly on macOS/Linux, with no
// PowerShell/cmd.exe wrapping. Stdout and stderr are combined. The process PID
// is registered with the orphan scanner for the lifetime of the command.
//
// Commands arrive already-shaped by the terminal-service normalizer:
//   - direct executables (`git status`, `npm run build`) run as-is
//   - shell built-ins and operator-wrapped commands arrive as `bash -c "..."`
//
// Working-directory tracking across calls (Windows persistent-PS behavior) is
// intentionally not implemented here; each Unix invocation starts fresh from
// `workDir`, which matches typical Unix shell semantics.
func runLocalCommandUnix(cmd string, args []string, workDir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, cmd, args...)
	if workDir != "" {
		c.Dir = workDir
	}

	// Headless hardening: authoritative non-interactive git/editor/credential
	// env, EOF stdin, and detachment from any controlling terminal so git/ssh/
	// credential helpers fail fast instead of prompting on /dev/tty and hanging.
	hardenNonAgentCommand(c, effectiveCommandLine(cmd, args))
	// On timeout, reap the whole detached process group (git spawns ssh /
	// credential-helper descendants that must not survive the parent). Swallow
	// the kill error (e.g. ESRCH if the group already exited) and return nil so
	// os/exec still reports the natural context-deadline result from Wait.
	c.Cancel = func() error {
		_ = killProcessGroup(c.Process.Pid)
		return nil
	}

	var combined bytes.Buffer
	c.Stdout = &combined
	c.Stderr = &combined

	if startErr := c.Start(); startErr != nil {
		return "", startErr
	}
	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:unix")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}
	err := c.Wait()
	return combined.String(), err
}

/*
runLocalCommand executes the command using persistent PowerShell for low latency.

	timeoutMs controls the maximum execution time. If 0, defaults to 120 seconds.
*/
// resolveExecTimeout normalizes a caller-supplied timeout (ms) to a bounded
// duration: default 120s when unset, capped at 4h so an unbounded caller cannot
// pin a Pub/Sub receive goroutine (exhausting MaxOutstandingMessages) forever.
func resolveExecTimeout(timeoutMs int64) time.Duration {
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	const maxTimeoutMs = 4 * 60 * 60 * 1000 // 4 hours (accommodates long codex runs)
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

// resolveWorkDir picks the working directory for a command with tracked-cwd
// support:
//  1. explicit cwd that differs from the config default → use it (user changed settings)
//  2. cwd equals the config default → prefer tracked cwd (user may have cd'd)
//  3. no cwd → tracked cwd or config default
func resolveWorkDir(cfg *Config, cwd string) string {
	workDir := cwd
	if workDir != "" && cfg != nil && strings.EqualFold(workDir, cfg.WorkingDirectory) {
		if tc := getTrackedCwd(); tc != "" {
			workDir = tc
		}
	}
	if workDir == "" {
		if tc := getTrackedCwd(); tc != "" {
			workDir = tc
		} else if cfg != nil {
			workDir = cfg.WorkingDirectory
		}
	}
	return workDir
}

// executeTerminalCommand is the single dispatch point for an `execute` command.
// It routes an opt-in tty=true request to the PTY path (macOS/Linux, eligible
// agent commands only) and everything else to the hardened pipe path. Git and
// test runners are forced onto pipes even when tty=true (design guardrail).
func executeTerminalCommand(cfg *Config, cmd commandMsg) (string, error) {
	// PTY is an allowlist: only a recognized resident TUI agent may run under a
	// PTY. Everything else (git, test runners, bash/sh/PowerShell, ssh, …) stays
	// on the hardened pipe path even with tty=true, so unsigned tty can't flip a
	// utility off the headless hardening.
	if cmd.Tty && isPTYEligibleCommand(cmd.Command, cmd.Args) {
		return runTTYCommand(cfg, cmd)
	}
	return runLocalCommand(cfg, cmd.Command, cmd.Args, cmd.Cwd, cmd.TimeoutMs)
}

// runTTYCommand runs an eligible command under a pseudo-terminal and returns its
// normalized, model-safe output. On Windows this fails fast with a clear
// captured error (ConPTY deferred).
func runTTYCommand(cfg *Config, cmd commandMsg) (string, error) {
	timeout := resolveExecTimeout(cmd.TimeoutMs)
	workDir := resolveWorkDir(cfg, cmd.Cwd)
	// Apply the same one-shot argv shaping StartSession uses for an eligible PTY
	// agent so a tty=true `execute` request returns a result instead of dropping
	// into the interactive TUI and hanging until the prompt timeout. agy/
	// antigravity need `--print --dangerously-skip-permissions <prompt>`.
	ptyCommand, ptyArgs := shapePTYExecArgs(cmd.Command, cmd.Args)
	out, aborted, abortMsg, err := runPTYCommand(
		ptyCommand, ptyArgs, workDir, nil, timeout, DefaultPTYPromptTimeout, nil)
	if err != nil {
		return out, err
	}
	if aborted {
		return out, errors.New(abortMsg)
	}
	return out, nil
}

// shapePTYExecArgs applies the same one-shot CLI argv shaping StartSession uses
// (buildInteractiveCLIArgs) for a PTY-eligible agent, so a tty=true `execute`
// request reaches the agent's non-interactive `--print` path rather than
// launching the raw interactive TUI. Only antigravity (agy/antigravity) needs
// shaping — `--print --dangerously-skip-permissions <prompt>`; other eligible
// commands pass through unchanged.
//
// Two eligible shapes are handled: a direct agy invocation (`agy fix this`),
// and a shell-wrapped single-agent payload (`bash -c "agy fix this"`, how
// terminal-service ships operator-joined commands). The shell-wrapped form must
// ALSO be shaped: its base command is the shell so isAntigravityCommand misses
// it, but isPTYEligibleCommand routes it here on the payload's first token, and
// without shaping the inner agy drops into the interactive TUI and hangs until
// the prompt timeout instead of returning a one-shot result.
func shapePTYExecArgs(command string, args []string) (string, []string) {
	if isAntigravityCommand(command) {
		return command, buildAntigravityInteractiveArgs(args)
	}
	return command, shapeShellWrappedPTYArgs(command, args)
}

// shapeShellWrappedPTYArgs applies antigravity one-shot shaping to a
// shell-wrapped single-agent PTY payload (`bash -c "agy …"`), returning args
// with the inner agy given `--print --dangerously-skip-permissions`. A DIRECT
// agy/antigravity invocation is already shaped by its caller
// (buildInteractiveCLIArgs on the session_start path, shapePTYExecArgs'
// antigravity branch on the execute path), so this only rewrites the `-c`
// payload of a shell wrapper and leaves everything else (including
// already-shaped direct argv) untouched. isPTYEligibleCommand has already
// cleared the payload of shell chaining, so its first token is the whole
// program and the remainder is preserved verbatim as the literal prompt.
func shapeShellWrappedPTYArgs(command string, args []string) []string {
	if payload, ok := shellDashCPayload(command, args); ok {
		if shaped, ok := shapeAntigravityShellPayload(payload); ok {
			return replaceDashCPayload(args, shaped)
		}
	}
	return args
}

// shapeAntigravityShellPayload rewrites a `bash -c` payload whose leading token
// is agy/antigravity so the inner invocation gains the one-shot
// `--print --dangerously-skip-permissions` shaping, returning the rewritten
// payload and true. It returns ("", false) when the first token is not an
// antigravity command. The flag list is sourced from buildAntigravityInteractiveArgs
// so it cannot drift from the direct path. Only reached for payloads
// isPTYEligibleCommand has cleared of shell chaining, so fields[0] is the whole
// program and the remainder is preserved verbatim as the literal prompt.
func shapeAntigravityShellPayload(payload string) (string, bool) {
	fields := strings.Fields(payload)
	if len(fields) == 0 || !isAntigravityCommand(fields[0]) {
		return "", false
	}
	flags := strings.Join(buildAntigravityInteractiveArgs(nil), " ")
	shaped := fields[0] + " " + flags
	if rest := strings.TrimSpace(strings.TrimPrefix(payload, fields[0])); rest != "" {
		shaped += " " + rest
	}
	return shaped, true
}

// replaceDashCPayload returns a copy of args with the string following the first
// `-c` flag replaced by payload, mirroring how shellDashCPayload locates it.
func replaceDashCPayload(args []string, payload string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "-c" && i+1 < len(out) {
			out[i+1] = payload
			return out
		}
	}
	return out
}

func runLocalCommand(cfg *Config, cmd string, args []string, cwd string, timeoutMs int64) (string, error) {
	timeout := resolveExecTimeout(timeoutMs)
	workDir := resolveWorkDir(cfg, cwd)

	// Unix (macOS/Linux): exec the command directly. The terminal-service
	// already wraps shell-bound commands (built-ins, &&/||, pipes) in
	// `bash -c "..."` before dispatch, so here we just run cmd + args with
	// no PowerShell/cmd.exe involvement. Without this branch, Unix agents
	// fall through to the Windows fallback path and try to exec
	// powershell.exe, which doesn't exist on macOS/Linux.
	if runtime.GOOS != "windows" {
		return runLocalCommandUnix(cmd, args, workDir, timeout)
	}

	// Check if this is an encoded PowerShell command (already Base64 encoded by terminal-service)
	isEncodedPowerShell := strings.ToLower(cmd) == "powershell" &&
		len(args) >= 2 &&
		strings.ToLower(args[0]) == "-encodedcommand"

	if isEncodedPowerShell {
		return runEncodedPowerShellCommand(args[1], workDir, timeout)
	}

	// Check if this is a regular PowerShell command with -Command that needs encoding
	// This handles legacy commands that weren't encoded by terminal-service
	isPowerShellCommand := strings.ToLower(cmd) == "powershell" &&
		len(args) >= 2 &&
		strings.ToLower(args[0]) == "-command"

	if isPowerShellCommand {
		// Encode the script locally to prevent escaping issues
		script := strings.Join(args[1:], " ")
		encoded := encodeForPowerShell(script)
		return runEncodedPowerShellCommand(encoded, workDir, timeout)
	}

	// Original behavior for non-PowerShell commands
	// Construct the full command line, quoting args that contain spaces or
	// special shell characters that PowerShell would misinterpret.
	cmdLine := cmd
	if len(args) > 0 {
		for _, arg := range args {
			if needsQuoting(arg) {
				// Escape embedded double-quotes by doubling them, then wrap in quotes.
				cmdLine += " \"" + strings.ReplaceAll(arg, "\"", "\"\"") + "\""
			} else {
				cmdLine += " " + arg
			}
		}
	}

	// CLI coding agents (claude, codex) use interactive/streaming output
	// that doesn't work with persistent PowerShell stdin pipes.
	// Always spawn a new powershell.exe process for these commands.
	cmdLower := strings.ToLower(cmd)
	isCLIAgent := cmdLower == "claude" || cmdLower == "codex"
	if isCLIAgent {
		fmt.Printf("%s[aiexpedite] Using dedicated process for CLI agent: %s%s\n", colorCyan, cmd, colorReset)
		// Resolve full path for claude since it may not be in fallback PowerShell's PATH.
		// Result is cached after the first resolution to avoid repeated PATH lookups and
		// filesystem scans on every Claude command.
		if strings.HasPrefix(cmdLower, "claude") {
			claudePath := cachedResolveClaudePath()
			if claudePath != "" && claudePath != "claude" {
				cmdLine = claudePath + cmdLine[6:] // 6 = len("claude")
			}
		}
		return runLocalCommandFallback(cmdLine, workDir, timeout)
	}

	// Test runners need per-command non-interactive defaults (CI=1,
	// FORCE_COLOR=0, NO_COLOR=1, PYTHONUNBUFFERED=1). The long-lived persistent
	// PowerShell fixes its env at startup and cannot inject them per command, so
	// route a detected test runner through a one-shot hardened process instead —
	// runLocalCommandFallback calls hardenNonAgentCommand, which layers
	// testRunnerEnvDefaults UNDER the authoritative git/editor safety overlay.
	// See EXECUTION_LIVENESS_REDESIGN.md → test-runner profile.
	if isTestRunnerCommand(cmdLine) {
		fmt.Printf("%s[aiexpedite] Using one-shot hardened process for test runner: %s%s\n",
			colorCyan, cmd, colorReset)
		return runLocalCommandFallback(cmdLine, workDir, timeout)
	}

	// Route bash-style commands (containing && or ||) through a one-shot
	// PowerShell process on Windows when the persistent PS instance is not
	// available. pwsh.exe is preferred (supports && natively); powershell.exe
	// is the fallback when pwsh.exe is not on PATH.
	if runtime.GOOS == "windows" && isBashStyleCommand(cmdLine) && !isPowerShellSpecificCommand(cmd) && !IsPersistentPSPwsh() {
		fmt.Printf("%s[aiexpedite] Routing bash-style command via one-shot powershell.exe (persistent PS unavailable)%s\n", colorCyan, colorReset)
		return runViaShell(cmdLine, workDir, timeout)
	}

	// Try persistent PowerShell first (much faster - avoids 300-800ms startup)
	ps, err := GetPowerShell()
	if err != nil {
		fmt.Printf("%s[aiexpedite] Persistent PowerShell unavailable, using fallback%s\n", colorYellow, colorReset)
		return runLocalCommandFallback(cmdLine, workDir, timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	output, err := ps.Execute(ctx, cmdLine, workDir)
	if err != nil {
		// ExitCodeError means the command ran but exited non-zero — the PS
		// process itself is fine, so don't restart or retry.
		if _, isExitErr := err.(*ExitCodeError); isExitErr {
			return output, err
		}

		// Process-level failure: try restarting PS once then retry.
		if restartErr := RestartPowerShell(); restartErr != nil {
			fmt.Printf("%s[aiexpedite] PowerShell restart failed, using fallback%s\n", colorYellow, colorReset)
			return runLocalCommandFallback(cmdLine, workDir, timeout)
		}

		ps, err = GetPowerShell()
		if err != nil {
			return runLocalCommandFallback(cmdLine, workDir, timeout)
		}

		// Reuse the original context so the retry does not exceed the
		// caller-requested timeout.  Creating a fresh full-timeout context here
		// could allow the command to run for up to 2× the requested duration.
		output, err = ps.Execute(ctx, cmdLine, workDir)
		if err != nil {
			return runLocalCommandFallback(cmdLine, workDir, timeout)
		}
	}

	// Sync tracked cwd from persistent PowerShell's last known location
	if newCwd := GetTrackedCwd(); newCwd != "" {
		setTrackedCwd(newCwd)
	}

	return output, nil
}

// fallbackPSExe caches the resolved PowerShell executable path so that
// runLocalCommandFallback does not re-run exec.LookPath on every invocation.
var (
	fallbackPSExe     string
	fallbackPSExeOnce sync.Once
)

func getFallbackPSExe() string {
	fallbackPSExeOnce.Do(func() {
		fallbackPSExe = "powershell.exe"
		if _, err := exec.LookPath("pwsh.exe"); err == nil {
			fallbackPSExe = "pwsh.exe"
		}
	})
	return fallbackPSExe
}

// claudePathCache caches the resolved Claude executable path so that the
// PATH lookup and filesystem scan in resolveClaudePath are only done once
// rather than on every Claude command invocation.
var (
	claudePathCached string
	claudePathOnce   sync.Once
)

// cachedResolveClaudePath returns the Claude executable path, resolving it
// once on first call and reusing the result for all subsequent calls.
func cachedResolveClaudePath() string {
	claudePathOnce.Do(func() {
		claudePathCached = resolveClaudePath()
	})
	return claudePathCached
}

// buildFallbackProbeCommand wraps a user command line for the one-shot fallback
// PowerShell so it (a) PRESERVES the user command's native exit code and (b)
// still reports the final working directory for cd tracking.
//
// The exit code must be captured into $__aix_exit IMMEDIATELY after the user
// command — before the Write-Host/Get-Location cwd probe runs — because those
// trailing cmdlets succeed and would otherwise reset the effective exit status,
// making `powershell -Command` exit 0 and mask a failing native command such as
// `npm test` / `pytest` as success. $LASTEXITCODE is $null when the command ran
// no native process (a pure cmdlet that did not fail terminally); treat that as
// success. A terminating error aborts before the probe, so PS already exits
// non-zero (cwd tracking is best-effort and skipped in that case).
//
// $LASTEXITCODE is reset to 0 BEFORE the user command: PowerShell only updates
// it when a native executable runs, and a fresh pwsh.exe can start with a
// non-zero value from internal startup steps, so a cmdlet-only fallback command
// would otherwise capture that stale non-zero code and be reported as a failure.
// This mirrors the persistent-shell reset in powershell_windows.go.
func buildFallbackProbeCommand(cmdLine, sentinel string) string {
	return "$LASTEXITCODE = 0\n" +
		cmdLine +
		"\n$__aix_exit = $LASTEXITCODE" +
		"\nWrite-Host '" + sentinel + "'" +
		"\n(Get-Location).Path" +
		"\nif ($null -eq $__aix_exit) { exit 0 } else { exit $__aix_exit }"
}

// runLocalCommandFallback uses traditional process spawning (slow but reliable).
// Prefers pwsh.exe (PowerShell 7+) when available for better compatibility.
// After the command runs, it queries the final working directory so that cd
// commands are tracked even through the fallback path.
func runLocalCommandFallback(cmdLine string, workDir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	psExe := getFallbackPSExe()

	// Append a pwd probe so we can track directory changes from this process.
	// The sentinel line lets us split user output from the directory result.
	const cwdSentinel = "<<<AIX_CWD_PROBE>>>"
	probeCmd := buildFallbackProbeCommand(cmdLine, cwdSentinel)

	// `-OutputFormat Text` prevents CLIXML error serialization — see
	// runEncodedPowerShellCommand for the full explanation.
	c := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-Command", probeCmd)
	hideWindow(c)
	// Headless hardening: authoritative non-interactive git/editor/credential env.
	hardenNonAgentCommand(c, cmdLine)
	if workDir != "" {
		c.Dir = workDir
	}

	// Use Start/Wait instead of CombinedOutput so the PID is registered
	// with the orphan scanner while the command runs.
	var combined bytes.Buffer
	c.Stdout = &combined
	c.Stderr = &combined
	if startErr := c.Start(); startErr != nil {
		return "", startErr
	}
	if c.Process != nil {
		globalProcessRegistry.Register(c.Process.Pid, "pubsub:fallback")
		defer globalProcessRegistry.Deregister(c.Process.Pid)
	}
	err := c.Wait()
	rawOut := combined.Bytes()
	out := string(rawOut)

	// Extract and strip the cwd probe from the output.
	if idx := strings.Index(out, cwdSentinel); idx != -1 {
		remainder := strings.TrimLeft(out[idx+len(cwdSentinel):], "\r\n")
		// remainder is: <newline><cwd path><newline>
		lines := strings.SplitN(remainder, "\n", 2)
		if len(lines) >= 1 {
			cwd := strings.TrimRight(lines[0], "\r\n ")
			if cwd != "" {
				setTrackedCwd(cwd)
			}
		}
		out = out[:idx]
	}

	return strings.TrimRight(out, "\r\n"), err
}

// resolveClaudePath finds the full path to claude.exe
// Claude Code CLI is typically installed in %APPDATA%\Claude\claude-code\<version>\claude.exe
func resolveClaudePath() string {
	// First check if claude is in PATH
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	if path, err := exec.LookPath("claude.exe"); err == nil {
		return path
	}

	// Check common Claude Code installation locations
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "claude"
	}

	// Claude Code CLI location: %APPDATA%\Claude\claude-code\<version>\claude.exe
	claudeCodeDir := filepath.Join(homeDir, "AppData", "Roaming", "Claude", "claude-code")
	if entries, err := os.ReadDir(claudeCodeDir); err == nil {
		// Find the latest version directory using semver comparison so that
		// v1.10.0 correctly ranks higher than v1.9.0 (lexicographic > fails here).
		var latestVersion string
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			// Normalise: semver.Compare requires a leading "v"
			candidate := name
			if !strings.HasPrefix(candidate, "v") {
				candidate = "v" + candidate
			}
			if latestVersion == "" {
				latestVersion = name
				continue
			}
			existing := latestVersion
			if !strings.HasPrefix(existing, "v") {
				existing = "v" + existing
			}
			if semver.Compare(candidate, existing) > 0 {
				latestVersion = name
			}
		}
		if latestVersion != "" {
			claudePath := filepath.Join(claudeCodeDir, latestVersion, "claude.exe")
			if _, err := os.Stat(claudePath); err == nil {
				fmt.Printf("%s[aiexpedite] Found Claude Code at: %s%s\n", colorCyan, claudePath, colorReset)
				return claudePath
			}
		}
	}

	// Fallback to just "claude" and hope it's in PATH
	return "claude"
}

// needsQuoting reports whether a command-line argument must be wrapped in
// double-quotes before being embedded in a PowerShell command string.
// Quotes are required when the arg contains spaces, literal double-quotes,
// or PowerShell metacharacters that would be misinterpreted without quoting.
func needsQuoting(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', '$', '&', '|', '<', '>', '(', ')', '{', '}', ';', ',', '@', '#':
			return true
		}
	}
	return false
}

/* --------------------------------------------------------------------------
   Path Safety Validation
   -------------------------------------------------------------------------- */

// isPathSafeUnder validates that path is within baseDir to prevent traversal.
// A separator is appended to the base before prefix-matching so that
// "/tmp2/file" is not incorrectly accepted when the base is "/tmp".
// Relative paths in path are resolved relative to baseDir.
func isPathSafeUnder(path, baseDir string) bool {
	// Resolve path relative to baseDir (handles both relative and absolute)
	var abs string
	if filepath.IsAbs(path) {
		abs = filepath.Clean(path)
	} else {
		abs = filepath.Clean(filepath.Join(baseDir, path))
	}
	base := filepath.Clean(baseDir) + string(filepath.Separator)
	absWithSep := abs + string(filepath.Separator)
	// Windows paths are case-insensitive - normalize to lowercase to prevent bypass
	if runtime.GOOS == "windows" {
		absWithSep = strings.ToLower(absWithSep)
		base = strings.ToLower(base)
	}
	return strings.HasPrefix(absWithSep, base)
}

/* --------------------------------------------------------------------------
   File Upload Helper Functions
   -------------------------------------------------------------------------- */

// mediaExtensions is the whitelist of file extensions eligible for upload.
// Kept narrow on purpose — we want screenshots, video captures, and test
// recordings, NOT test reports, source maps, or random JSON blobs that happen
// to land in the workdir while a command runs.
var mediaExtensions = map[string]struct{}{
	".png":  {},
	".jpg":  {},
	".jpeg": {},
	".gif":  {},
	".webp": {},
	".webm": {},
	".mp4":  {},
	".mov":  {},
}

// ignoredDirs are directory names pruned from the walk regardless of depth.
// The mtime filter would already exclude pre-existing files inside these
// trees, but pruning the walk saves 10-100x on repos with large dependency
// caches. Restricted to dependency/cache trees only — generic build-output
// directories (dist, build, out, target, bin, obj) are NOT pruned because
// test frameworks routinely write screenshots/videos under those paths and
// the upload feature is meant to discover them.
var ignoredDirs = map[string]struct{}{
	"node_modules":  {},
	".git":          {},
	".hg":           {},
	".svn":          {},
	".next":         {},
	".nuxt":         {},
	".turbo":        {},
	".cache":        {},
	".parcel-cache": {},
	".pytest_cache": {},
	".mypy_cache":   {},
	".ruff_cache":   {},
	".tox":          {},
	".venv":         {},
	"venv":          {},
	"__pycache__":   {},
	"vendor":        {},
}

// sensitiveBasenamePrefixes is a deny-list of basename prefixes that look
// like deliberately disguised credential files (e.g. "id_rsa.jpg",
// ".env.png", "credentials.mov"). Extension whitelisting alone doesn't
// catch a base64-encoded secret renamed to <something>.png; this is
// defense-in-depth against the user accidentally — or maliciously —
// dropping such a file into a workdir we're about to ship to GCS.
//
// Matched case-insensitively against the file basename (without
// directory) via simple HasPrefix comparison. Unicode-lookalike attacks
// (e.g. Cyrillic "е" in place of Latin "e") are NOT defended against —
// the primary control is the user not dropping credential files into
// their repo; this list catches the common-typo case.
var sensitiveBasenamePrefixes = []string{
	".env",
	"id_rsa",
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
	".npmrc",
	".pypirc",
	".netrc",
	".aws",
	".gcp",
	".kube",
	"credentials",
	"service-account",
	"service_account",
	"secret",
}

// mtimeSkew is the slack we subtract from the session start time to avoid
// dropping files written within a millisecond of session start due to clock
// rounding or filesystem timestamp resolution (HFS+ is 1s, FAT32 is 2s).
const mtimeSkew = 5 * time.Second

// maxUploadFiles caps total uploads per command/session.  Without a cap, a
// repo dropping hundreds of screenshots per run could exhaust memory and GCS
// quota. The mtime filter already excludes pre-existing files, so 50 is
// generous for a single test run.
const maxUploadFiles = 50

// errFileLimitReached is the sentinel returned from the WalkDir callback to
// abort the walk once we've collected maxUploadFiles. A typed sentinel lets
// the caller distinguish "we hit our cap" from "the filesystem returned a
// real error" without comparing length counts.
var errFileLimitReached = errors.New("file upload limit reached")

// detectOutputFilesSince finds media files newly written under workDir during
// the current command/session. Files are kept if:
//  1. Extension is in mediaExtensions (PNG, JPG, GIF, WEBP, WEBM, MP4, MOV)
//  2. Basename does not match any sensitiveBasenamePrefixes pattern
//  3. ModTime >= sessionStart - mtimeSkew
//  4. Path stays within workDir (no symlink escape)
//  5. Not inside an ignored dependency/cache directory (node_modules, .git, etc.)
//
// This replaces an older heuristic that scanned a hardcoded list of
// framework-specific directories (test-results/, test-results-ui/). The
// mtime-based approach catches screenshots wherever a framework or custom
// config writes them — cypress/screenshots/, wdio-output/, artifacts/,
// .maestro/output/, or anywhere else the user has configured — without
// requiring the Go binary to know about every framework.
//
// sessionStart should be the time the command (or interactive session)
// started. Pass time.Time{} (zero) to disable the mtime filter and accept
// any matching media file under workDir — only used by tests; production
// callers always pass a real start time.
func detectOutputFilesSince(workDir string, sessionStart time.Time) []string {
	files := []string{}

	if workDir == "" {
		return files
	}

	absBase, err := filepath.Abs(workDir)
	if err != nil {
		fmt.Printf("[file-upload] Cannot resolve workDir %q: %v\n", workDir, err)
		return files
	}

	// Resolve symlinks before walking. filepath.WalkDir does NOT descend into
	// symlinked directories, so if workDir itself (or any ancestor in the
	// path) is a symlink — common when the user's repo lives under a
	// symlinked path like /var → /private/var on macOS, or when CI mounts a
	// workspace via symlink — the walk would return nothing. EvalSymlinks
	// resolves the entire chain to a canonical real path. If resolution
	// fails (e.g., the workdir was deleted between command dispatch and
	// session end), fall through to the original absBase so the walk's own
	// error handling reports the not-exist condition.
	if resolved, resolveErr := filepath.EvalSymlinks(absBase); resolveErr == nil {
		absBase = resolved
	} else if !errors.Is(resolveErr, fs.ErrNotExist) {
		fmt.Printf("[file-upload] Cannot resolve symlinks for %q: %v\n", absBase, resolveErr)
	}

	cutoff := time.Time{}
	if !sessionStart.IsZero() {
		cutoff = sessionStart.Add(-mtimeSkew)
	}

	walkErr := filepath.WalkDir(absBase, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission and not-exist errors on individual subtrees (or on
			// the root itself, when the workdir was removed between command
			// dispatch and session end) should not abort the walk or spam
			// stderr — they're expected operational states.
			if !errors.Is(err, fs.ErrPermission) && !errors.Is(err, fs.ErrNotExist) {
				fmt.Printf("[file-upload] Walk error at %s: %v\n", path, err)
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			// Defense in depth: refuse any "directory" entry that is not a
			// pure directory. This catches symlinked dirs on POSIX, NTFS
			// junctions (created by `mklink /J`) and mount points on
			// Windows — both of which have ModeIrregular set on Go 1.20+
			// even though IsDir() returns true — plus anything else exotic
			// the OS surfaces (Windows reparse-point tags we don't know
			// about). filepath.WalkDir doesn't follow symlinks, but it DOES
			// descend into junctions, which is the gap we're closing here.
			if d.Type()&^os.ModeDir != 0 {
				return fs.SkipDir
			}

			// Prune ignored directories. Match by base name only — a project
			// named "vendor" at the repo root is rare enough that the false
			// positive is acceptable, and the user can rename if needed.
			if path != absBase {
				if _, skip := ignoredDirs[d.Name()]; skip {
					return fs.SkipDir
				}
				// Hidden directories are pruned too, EXCEPT the .maestro
				// convention used by Maestro for mobile UI testing output.
				name := d.Name()
				if strings.HasPrefix(name, ".") && name != ".maestro" {
					return fs.SkipDir
				}
			}
			return nil
		}

		if len(files) >= maxUploadFiles {
			return errFileLimitReached
		}

		// Only upload regular files. d.Type() returns just the type bits
		// (ModeDir, ModeSymlink, ModeNamedPipe, ModeSocket, ModeDevice,
		// ModeCharDevice, ModeIrregular) — a value of zero means a regular
		// file. This blocks symlinks, pipes, sockets, devices, and the
		// ModeIrregular tag Windows uses for reparse points other than
		// symlinks (e.g. NTFS junctions).
		if d.Type() != 0 {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := mediaExtensions[ext]; !ok {
			return nil
		}

		// Sensitive-name guard: even though the extension is on the media
		// allowlist, refuse to upload basenames that look like disguised
		// credential / secret files. Catches the case where someone (or
		// some misconfigured tool) drops `.env.png` or `id_rsa.jpg` into
		// the workdir.
		baseLower := strings.ToLower(filepath.Base(path))
		for _, prefix := range sensitiveBasenamePrefixes {
			if strings.HasPrefix(baseLower, prefix) {
				LogSecurityEvent(SecEvtPathTraversal, "blocked sensitive-looking filename from upload",
					"path", path, "site", "detectOutputFilesSince.sensitiveBasename")
				return nil
			}
		}

		// Defense in depth: the walk shouldn't leave absBase since we
		// rejected symlinks, but verify before uploading.
		if !isPathSafeUnder(path, absBase) {
			LogSecurityEvent(SecEvtPathTraversal, "blocked file outside base during walk",
				"path", path, "base_dir", absBase, "site", "detectOutputFilesSince")
			return nil
		}

		if !cutoff.IsZero() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			if info.ModTime().Before(cutoff) {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, errFileLimitReached) {
		// Only log genuine walk errors, not our own sentinel stop error.
		fmt.Printf("[file-upload] Walk error in %s: %v\n", absBase, walkErr)
	}

	return files
}

// extractWorkspaceID extracts workspaceID from command message
// Returns empty string if not provided - caller must handle this case
func extractWorkspaceID(cmd commandMsg) string {
	return cmd.WorkspaceID
}

/* --------------------------------------------------------------------------
   Global Session Manager
   -------------------------------------------------------------------------- */

// globalSessionManager is the package-level SessionManager instance,
// initialized in StartAgent (agent.go).
var globalSessionManager *SessionManager

// globalCodexAppServerManager is the package-level CodexAppServerManager
// instance, initialized in StartAgent (agent.go). Sessions launched here
// drive Codex via its JSON-RPC stdio app-server protocol (separate code path
// from the existing `codex exec` CLI integration in session.go).
var globalCodexAppServerManager *CodexAppServerManager

// globalGrokACPManager is the package-level GrokACPManager instance,
// initialized in StartAgent (agent.go). Sessions launched here drive xAI
// Grok Build CLI via its ACP (Agent Client Protocol) JSON-RPC stdio
// interface (`grok agent stdio`), preferring the user's local `grok login`
// cached token over an API-key fallback so usage ties to the terminal
// computer user's Grok / X account.
var globalGrokACPManager *GrokACPManager

// globalClaudeNativeManager is the package-level ClaudeNativeManager instance,
// initialized in StartAgent (agent.go). Sessions launched here drive Claude
// Code in structured `--output-format stream-json` mode and forward its frames
// verbatim as `claude_native_*` chunks (separate code path from the generic
// display-text `stream` integration in session.go).
var globalClaudeNativeManager *ClaudeNativeManager

// globalAntigravityNativeManager is the package-level AntigravityNativeManager
// instance, initialized in StartAgent (agent.go). Sessions use one-shot
// `agy --print` with exact `--conversation <id>` resume and publish
// antigravity_native_* chunks for the frontend native chat path.
var globalAntigravityNativeManager *AntigravityNativeManager

/* --------------------------------------------------------------------------
   handleSessionCommand — routes session_* commands to the SessionManager
   -------------------------------------------------------------------------- */

// shouldGateExecuteCommand reports whether the inbound `execute` command
// must be routed through the allow-list / approval-dialog flow. Returns
// false (skip gating) when the operator disabled allow-list enforcement,
// flipped the AllowAllCommands tray override, the allow-list is not yet
// initialised, OR the command already matches a permitted pattern.
// Centralising the boolean keeps handleMessage readable and gives the
// AllowAllCommands bypass a single unit-testable seam.
func shouldGateExecuteCommand(cfg *Config, al *AllowList, cmd string, args []string) bool {
	// AllowAllCommands is read via the synchronised accessor — the
	// tray-menu goroutine in main.go writes the toggle while Receive
	// callbacks land here on Pub/Sub goroutines, so a direct field
	// access would be a data race (go test -race).
	if !cfg.EnableAllowList || cfg.IsAllowAllCommands() || al == nil {
		return false
	}
	return !al.IsAllowed(cmd, args)
}

// commandApprovalDialogFn is an indirection over ShowCommandApprovalDialog so
// the approval-dialog branches (which otherwise shell out to native OS UI and
// can't run headless) are stubbable in tests. Production keeps the real dialog.
var commandApprovalDialogFn = ShowCommandApprovalDialog

// requiresNativeApprovalForStep forces the on-device native approval dialog for
// a high-risk Environment Setup step regardless of the allow-list or the
// AllowAllCommands override. Destructive steps (disk cleanup, permanent
// deletion, broad package updates) AND external/credential writes (CLI-agent /
// gh / git sign-in and config) require BOTH the chat-side scoped confirmation
// (enforced server-side by terminal-service's approval gate) AND this native
// approval, per the feature's safety model. Mirrors requiresNativeApproval() in
// shared-constants. The riskLevel is HMAC-signed (see signaturePayload) so it
// cannot be stripped to skip the gate.
func requiresNativeApprovalForStep(cmd commandMsg) bool {
	return cmd.RiskLevel == "destructive" || cmd.RiskLevel == "external_write"
}

// applyTimeoutPolicy resolves a native-approval dialog result under the
// operator's allow-on-timeout setting. A timeout (or explicit deny) is upgraded
// to a one-time approval ONLY when allow-on-timeout is configured AND the step
// is not a destructive Environment Setup step. Destructive cleanup/delete steps
// must never auto-approve on an unattended dialog, so for them a deny stays a
// deny regardless of the allow-on-timeout convenience — matching the feature's
// safety model (destructive requires an explicit local confirmation).
func applyTimeoutPolicy(result ApprovalResult, cfg *Config, cmd commandMsg) ApprovalResult {
	if result == ApprovalDeny &&
		cfg.ApprovalTimeoutAction == "allow" &&
		!requiresNativeApprovalForStep(cmd) {
		return ApprovalOnce
	}
	return result
}

// gateSessionEntryCommand applies the allowlist + user-approval dialog flow
// to the long-running entry-point commands (session_start and
// codex_appserver_start). For all other interactive commands it is a no-op
// pass-through — those target an already-allowed session and don't need to
// re-prompt.
//
// Returns true if processing should continue (handleSessionCommand should
// dispatch), false if a rejection has already been published and the inbound
// pub/sub message has been acked. Centralising this here removes the
// 40-line duplicate block that used to live inline for each entry kind.
func gateSessionEntryCommand(ctx context.Context, topic *pubsub.Publisher, m *pubsub.Message, cmd commandMsg, cfg *Config) bool {
	// A high-risk Environment Setup step (signed destructive/external_write —
	// e.g. an interactive CLI-agent / gh / git sign-in launched as
	// session_start) ALWAYS shows the native approval dialog, exactly like the
	// execute path's requiresNativeApprovalForStep gate. It therefore bypasses
	// the AllowAllCommands / disabled-allowlist / already-allowlisted
	// short-circuits below, so an interactive auth/setup flow can't skip local
	// approval. The riskLevel is HMAC-signed so it can't be stripped.
	forceNative := requiresNativeApprovalForStep(cmd)

	// AllowAllCommands short-circuits before any allow-list / dialog work
	// so session entry points behave the same as the execute path when
	// the operator has flipped the tray bypass. Read via the synchronised
	// accessor — see shouldGateExecuteCommand for the rationale.
	if !forceNative {
		if cfg.IsAllowAllCommands() {
			return true
		}
		if !cfg.EnableAllowList || defaultAllowList == nil {
			return true
		}
	}

	var allowCommand string
	var allowArgs []string
	// dialogArgs is what we show the user in the approval dialog. It
	// usually equals allowArgs, but for grok_acp_start with API-key
	// fallback enabled we pass the redacted form so an `--api-key xai-…`
	// value the orchestrator passed through doesn't leak into the
	// dialog text (the platform dialogs concatenate argv for display).
	var dialogArgs []string
	var denyOutput string
	// persistOnAlways controls whether an "Always" click persists an allow-list
	// pattern for this entry. Defaults to true; the grok_acp_start path forces
	// it false (see that case) so "Always" degrades to a one-time approval
	// there — persisting `grok *` would reopen the raw-execute bypass the grok
	// short-circuit exists to prevent.
	persistOnAlways := true

	switch cmd.Type {
	case "session_start":
		allowCommand = cmd.Command
		allowArgs = cmd.Args
		dialogArgs = allowArgs
		denyOutput = "Command denied by user: not in allow list"
	case "codex_appserver_start":
		// Gate against the synthesised `codex app-server …` argv the manager
		// will actually exec so the default `codex *` allowlist entry covers
		// app-server access without operators having to maintain a parallel
		// allowlist for the new entry kind.
		allowCommand = "codex"
		allowArgs = buildCodexAppServerArgs(cmd.Args)
		dialogArgs = allowArgs
		denyOutput = "codex app-server denied by user: not in allow list"
	case "claude_native_start":
		// Gate against the synthesised `claude …stream-json…` argv the manager
		// will actually exec so the default `claude *` allowlist entry covers
		// native-chat access without a parallel allowlist for the new entry
		// kind. buildClaudeInteractiveArgs also strips -p/--print, matching the
		// launch shape (its extracted prompt is delivered on stdin, not argv).
		allowCommand = "claude"
		allowArgs, _ = buildClaudeInteractiveArgs(cmd.Args)
		dialogArgs = allowArgs
		denyOutput = "claude native session denied by user: not in allow list"
	case "antigravity_native_start":
		// Gate against `agy` so the default `agy *` / `antigravity *` allowlist
		// entry covers native-chat access. The actual per-turn argv is built
		// later in Send; Start only registers the logical session.
		allowCommand = "agy"
		allowArgs = []string{"--print", "<prompt>"}
		dialogArgs = allowArgs
		denyOutput = "antigravity native session denied by user: not in allow list"
	case "grok_acp_start":
		// Unlike codex_appserver_start, grok_acp_start is NOT gated through
		// the shared execute allowlist or approval dialog WHEN signing is
		// enforced. Two reasons:
		//
		//   1. The default allowlist intentionally omits bare `grok` so a
		//      signed raw `execute` of `grok …` cannot bypass the ACP
		//      manager's EnableGrokAPIKeyFallback / EnableGrokAlwaysApprove
		//      / env-sanitisation gates. Adding a `grok agent stdio *`
		//      entry to satisfy grok_acp_start would re-open exactly that
		//      bypass for any execute carrying those same args.
		//
		//   2. ACP approvals are routed inside the manager + orchestrator
		//      (permission/request → orchestrator → per-workspace prompt),
		//      so the session-entry dialog is redundant. The manager still
		//      enforces argv/env sanitisation regardless of the gate.
		//
		// On "Always" the persisted pattern would also be `grok *` — i.e.
		// it would permanently neuter (1). Short-circuiting here avoids
		// both the global match and the Always-persist path.
		//
		// BUT when signature verification is disabled (cfg.CommandSecret
		// empty — the pre/unregistered mode the upstream check at line ~940
		// permits), nothing has authenticated the inbound command, so a
		// stray pubsub message could otherwise launch a Grok session
		// without ANY local gate. Fall back to the same allowlist +
		// approval-dialog path session_start uses in that mode so the
		// human operator still gets a prompt before spawn. The synthesised
		// `grok …` argv mirrors what the manager will actually exec,
		// keeping the dialog's display text and the persisted Always
		// pattern truthful.
		//
		// forceNative overrides this short-circuit: a signed high-risk step
		// (external_write sign-in/config) must still get the native dialog even
		// in signed mode — the manager's argv/env sanitisation still applies.
		if cfg.CommandSecret != "" && !forceNative {
			return true
		}
		allowCommand = "grok"
		allowArgs = buildGrokACPArgs(cmd.Args, cfg.EnableGrokAlwaysApprove)
		dialogArgs = redactGrokACPArgsForLog(allowArgs)
		denyOutput = "grok ACP session denied by user: not in allow list"
		// NEVER persist an allow-list entry for a Grok ACP session. Both paths
		// that reach here (unsigned mode, or a signed forced-native high-risk
		// step) set allowCommand="grok", so an "Always" click would persist
		// `grok *` (GeneratePatternFromCommand) — permanently allow-listing a
		// bare `grok` and reopening the raw `execute grok …` bypass this
		// short-circuit exists to close (a later raw execute would skip the ACP
		// manager's API-key/env-sanitisation gates). Treat "Always" as one-time.
		persistOnAlways = false
	default:
		// Mid-session interactive commands don't re-prompt — unless the step
		// itself is signed high-risk, in which case fall through to the native
		// dialog using the raw command/args.
		if !forceNative {
			return true
		}
		allowCommand = cmd.Command
		allowArgs = cmd.Args
		dialogArgs = allowArgs
		denyOutput = "Command denied by user: high-risk step requires approval"
	}

	// A non-high-risk step that's already allowlisted needs no dialog; a
	// high-risk step is never allowed to skip it (defaultAllowList may be nil
	// when the allowlist is disabled — guarded by !forceNative above).
	if !forceNative && defaultAllowList.IsAllowed(allowCommand, allowArgs) {
		return true
	}

	timeoutSec := cfg.ApprovalTimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	result := commandApprovalDialogFn(allowCommand, dialogArgs, timeoutSec)
	// Deny/timeout honours the allow-on-timeout convenience ONLY for
	// non-high-risk steps — destructive/external_write never auto-approve on an
	// unattended dialog (see applyTimeoutPolicy).
	result = applyTimeoutPolicy(result, cfg, cmd)
	switch result {
	case ApprovalDeny:
		fmt.Printf("%s[aiexpedite] %s denied by user%s\n", colorYellow, cmd.Type, colorReset)
		res := makeRejectionResult(
			cmd,
			cfg.AgentID,
			"denied",
			"ALLOWLIST_DENIED",
			denyOutput,
		)
		if err := publishMsg(ctx, topic, res); err != nil {
			m.Nack()
		} else {
			m.Ack()
		}
		return false
	case ApprovalAlways:
		// defaultAllowList can be nil on the forceNative path when the
		// allowlist is disabled — skip persistence rather than panic. The
		// grok_acp_start path also opts out (persistOnAlways=false) so "Always"
		// never persists `grok *` and reopens the raw-execute bypass.
		if persistOnAlways && defaultAllowList != nil {
			pattern := GeneratePatternFromCommand(allowCommand, allowArgs)
			if err := defaultAllowList.AddPattern(pattern); err != nil {
				fmt.Printf("%s[aiexpedite] Failed to add pattern to allow list: %v%s\n", colorYellow, err, colorReset)
			}
		}
	}
	return true
}

// newSessionPublishFn returns a PublishFunc that marshals each resultMsg and
// publishes it via the supplied Pub/Sub topic. logPrefix is used in error
// logs so the source ([session] vs. [codex-appserver]) stays distinguishable.
//
// Each publish uses a fresh context.Background() (not the sub.Receive
// callback ctx) because the callback ctx is cancelled once the inbound
// message is acked — which happens immediately after session_start returns,
// while stream / session_ended frames are published for minutes afterward.
// A cancelled context would silently drop every subsequent message.
func newSessionPublishFn(topic *pubsub.Publisher, logPrefix string) PublishFunc {
	return func(res resultMsg) {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Printf("%s%s Failed to marshal result: %v%s\n", colorRed, logPrefix, err, colorReset)
			return
		}
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, pubErr := topic.Publish(pubCtx, &pubsub.Message{Data: data}).Get(pubCtx)
		pubCancel()
		if pubErr != nil {
			fmt.Printf("%s%s Failed to publish result: %v%s\n", colorRed, logPrefix, pubErr, colorReset)
		}
	}
}

// handleSessionCommand handles interactive session commands (session_*,
// codex_appserver_*, grok_acp_*). It publishes results back via the
// provided Pub/Sub topic. cfg is threaded through so per-family handlers
// can apply config-driven policy (e.g. grok's API-key gate, workspace
// containment root) without reaching for a package-level config getter.
func handleSessionCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) {
	if isCodexAppServerCommand(cmd.Type) {
		handleCodexAppServerCommand(ctx, topic, cmd)
		return
	}
	if isClaudeNativeCommand(cmd.Type) {
		handleClaudeNativeCommand(ctx, topic, cmd)
		return
	}
	if isAntigravityNativeCommand(cmd.Type) {
		handleAntigravityNativeCommand(ctx, topic, cmd)
		return
	}
	if isGrokACPCommand(cmd.Type) {
		handleGrokACPCommand(ctx, topic, cmd, cfg)
		return
	}

	if globalSessionManager == nil {
		publishSessionError(ctx, topic, cmd, "session manager not initialized")
		return
	}

	publishFn := newSessionPublishFn(topic, "[session]")

	switch cmd.Type {
	case "session_start":
		if cmd.SessionID == "" {
			publishSessionError(ctx, topic, cmd, "sessionID is required for session_start")
			return
		}

		fmt.Printf("%s[session] Starting session %s for command: %s%s\n",
			colorCyan, cmd.SessionID, cmd.Command, colorReset)

		err := globalSessionManager.StartSession(
			cmd.SessionID,
			cmd.Command,
			cmd.Args,
			cmd.Cwd,
			cmd.WorkspaceID,
			cmd.UID,
			cmd.TimeoutMs,
			cmd.Tty,
			publishFn,
		)
		if err != nil {
			publishSessionError(ctx, topic, cmd, fmt.Sprintf("failed to start session: %v", err))
			return
		}

		// Publish session_started acknowledgment
		publishFn(resultMsg{
			ID:          cmd.ID,
			WorkspaceID: cmd.WorkspaceID,
			UID:         cmd.UID,
			Output:      "Session started",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "session_started",
			SessionID:   cmd.SessionID,
		})

	case "session_input":
		if cmd.SessionID == "" {
			publishSessionError(ctx, topic, cmd, "sessionID is required for session_input")
			return
		}

		fmt.Printf("%s[session] Sending input to session %s%s\n",
			colorBlue, cmd.SessionID, colorReset)

		if err := globalSessionManager.SendInput(cmd.SessionID, cmd.Input); err != nil {
			publishSessionError(ctx, topic, cmd, fmt.Sprintf("failed to send input: %v", err))
			return
		}

	case "session_signal":
		if cmd.SessionID == "" {
			publishSessionError(ctx, topic, cmd, "sessionID is required for session_signal")
			return
		}

		fmt.Printf("%s[session] Sending signal '%s' to session %s%s\n",
			colorYellow, cmd.Signal, cmd.SessionID, colorReset)

		if err := globalSessionManager.SignalSession(cmd.SessionID, cmd.Signal); err != nil {
			publishSessionError(ctx, topic, cmd, fmt.Sprintf("failed to send signal: %v", err))
			return
		}

	case "session_end":
		if cmd.SessionID == "" {
			publishSessionError(ctx, topic, cmd, "sessionID is required for session_end")
			return
		}

		fmt.Printf("%s[session] Ending session %s%s\n",
			colorYellow, cmd.SessionID, colorReset)

		if err := globalSessionManager.EndSession(cmd.SessionID); err != nil {
			publishSessionError(ctx, topic, cmd, fmt.Sprintf("failed to end session: %v", err))
			return
		}

	default:
		publishSessionError(ctx, topic, cmd, fmt.Sprintf("unknown session command type: %s", cmd.Type))
	}
}

// publishSessionError publishes an error result for a session command.
func publishSessionError(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, errMsg string) {
	fmt.Printf("%s[session] Error: %s%s\n", colorRed, errMsg, colorReset)

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		Output:      errMsg,
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "session_error",
		SessionID:   cmd.SessionID,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[session] Failed to publish error: %v%s\n", colorRed, err, colorReset)
	}
}

/* --------------------------------------------------------------------------
   Codex app-server (JSON-RPC over stdio) command routing
   -------------------------------------------------------------------------- */

// isCodexAppServerCommand returns true if cmdType is one of the Codex IDE
// app-server command kinds. Centralised so the dispatch in handleSessionCommand
// and any future tray/UI consumers stay in sync.
func isCodexAppServerCommand(cmdType string) bool {
	switch cmdType {
	case "codex_appserver_start", "codex_appserver_send", "codex_appserver_end":
		return true
	}
	return false
}

// handleCodexAppServerCommand dispatches codex_appserver_* commands to the
// CodexAppServerManager. Mirrors handleSessionCommand's shape — a single
// publishFn is shared with the manager so it can stream JSON-RPC frames
// (responses, notifications, server-initiated approval requests) back via
// Pub/Sub for the duration of the session.
func handleCodexAppServerCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg) {
	if globalCodexAppServerManager == nil {
		publishCodexAppServerError(ctx, topic, cmd, "codex app-server manager not initialized")
		return
	}

	publishFn := newSessionPublishFn(topic, "[codex-appserver]")

	switch cmd.Type {
	case "codex_appserver_start":
		if cmd.SessionID == "" {
			publishCodexAppServerError(ctx, topic, cmd, "sessionID is required for codex_appserver_start")
			return
		}

		fmt.Printf("%s[codex-appserver] Starting session %s (workspace=%s)%s\n",
			colorCyan, cmd.SessionID, cmd.WorkspaceID, colorReset)

		err := globalCodexAppServerManager.Start(
			cmd.SessionID,
			cmd.Cwd,
			cmd.Args,
			cmd.WorkspaceID,
			cmd.UID,
			publishFn,
		)
		if err != nil {
			publishCodexAppServerError(ctx, topic, cmd, fmt.Sprintf("failed to start codex app-server: %v", err))
			return
		}

		// Mirror session_started: a synchronous ack so the orchestrator can
		// proceed to send the JSON-RPC `initialize` request as soon as the
		// pipe is up. The first codex_appserver_message will follow once
		// codex emits its initialize response on stdout.
		publishFn(resultMsg{
			ID:          cmd.ID,
			WorkspaceID: cmd.WorkspaceID,
			UID:         cmd.UID,
			Output:      "Codex app-server started",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "codex_appserver_started",
			SessionID:   cmd.SessionID,
		})

	case "codex_appserver_send":
		if cmd.SessionID == "" {
			publishCodexAppServerError(ctx, topic, cmd, "sessionID is required for codex_appserver_send")
			return
		}
		if cmd.Input == "" {
			publishCodexAppServerError(ctx, topic, cmd, "input (JSON-RPC frame) is required for codex_appserver_send")
			return
		}

		if err := globalCodexAppServerManager.Send(cmd.SessionID, cmd.Input); err != nil {
			publishCodexAppServerError(ctx, topic, cmd, fmt.Sprintf("failed to send to codex app-server: %v", err))
			return
		}

	case "codex_appserver_end":
		if cmd.SessionID == "" {
			publishCodexAppServerError(ctx, topic, cmd, "sessionID is required for codex_appserver_end")
			return
		}

		fmt.Printf("%s[codex-appserver] Ending session %s%s\n",
			colorYellow, cmd.SessionID, colorReset)

		if err := globalCodexAppServerManager.End(cmd.SessionID); err != nil {
			publishCodexAppServerError(ctx, topic, cmd, fmt.Sprintf("failed to end codex app-server session: %v", err))
			return
		}

	default:
		publishCodexAppServerError(ctx, topic, cmd, fmt.Sprintf("unknown codex app-server command type: %s", cmd.Type))
	}
}

// publishCodexAppServerError surfaces a synchronous failure (bad request,
// manager not ready, send failure) back to the orchestrator as a
// `codex_appserver_error` frame so it can fail the in-flight call without
// waiting for a timeout.
func publishCodexAppServerError(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, errMsg string) {
	fmt.Printf("%s[codex-appserver] Error: %s%s\n", colorRed, errMsg, colorReset)

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		Output:      errMsg,
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "codex_appserver_error",
		SessionID:   cmd.SessionID,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[codex-appserver] Failed to publish error: %v%s\n", colorRed, err, colorReset)
	}
}

/* --------------------------------------------------------------------------
   Claude native (stream-json over stdio) command routing
   -------------------------------------------------------------------------- */

// isClaudeNativeCommand returns true if cmdType is one of the Claude native
// command kinds. Same shape as isCodexAppServerCommand / isGrokACPCommand.
func isClaudeNativeCommand(cmdType string) bool {
	switch cmdType {
	case "claude_native_start", "claude_native_send", "claude_native_end":
		return true
	}
	return false
}

// handleClaudeNativeCommand dispatches claude_native_* commands to the
// ClaudeNativeManager. Unlike codex/grok (which forward opaque JSON-RPC frames
// the orchestrator constructs), Claude turns are plain user text: `start`
// carries the optional initial prompt in cmd.Input, `send` carries the next
// user turn's text, and the manager wraps both in the NDJSON user envelope.
func handleClaudeNativeCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg) {
	if globalClaudeNativeManager == nil {
		publishClaudeNativeError(ctx, topic, cmd, "claude native manager not initialized")
		return
	}

	publishFn := newSessionPublishFn(topic, "[claude-native]")

	switch cmd.Type {
	case "claude_native_start":
		if cmd.SessionID == "" {
			publishClaudeNativeError(ctx, topic, cmd, "sessionID is required for claude_native_start")
			return
		}

		fmt.Printf("%s[claude-native] Starting session %s (workspace=%s)%s\n",
			colorCyan, cmd.SessionID, cmd.WorkspaceID, colorReset)

		// Fire the started ack from inside Start (after readers are wired but
		// before the initial prompt is written) so the ack is guaranteed to
		// precede any claude_native_message frames — consumers that initialize
		// state on the started frame rely on that ordering.
		onStarted := func() {
			publishFn(resultMsg{
				ID:          cmd.ID,
				WorkspaceID: cmd.WorkspaceID,
				UID:         cmd.UID,
				Output:      "Claude native started",
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "claude_native_started",
				SessionID:   cmd.SessionID,
			})
		}

		err := globalClaudeNativeManager.Start(
			cmd.SessionID,
			cmd.Cwd,
			cmd.Args,
			cmd.Input, // optional initial user prompt (plain text)
			cmd.WorkspaceID,
			cmd.UID,
			publishFn,
			onStarted,
		)
		if err != nil {
			publishClaudeNativeError(ctx, topic, cmd, fmt.Sprintf("failed to start claude native: %v", err))
			return
		}

	case "claude_native_send":
		if cmd.SessionID == "" {
			publishClaudeNativeError(ctx, topic, cmd, "sessionID is required for claude_native_send")
			return
		}
		if cmd.Input == "" {
			publishClaudeNativeError(ctx, topic, cmd, "input (user turn text) is required for claude_native_send")
			return
		}

		if err := globalClaudeNativeManager.Send(cmd.SessionID, cmd.Input); err != nil {
			publishClaudeNativeError(ctx, topic, cmd, fmt.Sprintf("failed to send to claude native: %v", err))
			return
		}

	case "claude_native_end":
		if cmd.SessionID == "" {
			publishClaudeNativeError(ctx, topic, cmd, "sessionID is required for claude_native_end")
			return
		}

		fmt.Printf("%s[claude-native] Ending session %s%s\n",
			colorYellow, cmd.SessionID, colorReset)

		if err := globalClaudeNativeManager.End(cmd.SessionID); err != nil {
			publishClaudeNativeError(ctx, topic, cmd, fmt.Sprintf("failed to end claude native session: %v", err))
			return
		}

	default:
		publishClaudeNativeError(ctx, topic, cmd, fmt.Sprintf("unknown claude native command type: %s", cmd.Type))
	}
}

// publishClaudeNativeError surfaces a synchronous failure back to the
// orchestrator as a `claude_native_error` frame.
func publishClaudeNativeError(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, errMsg string) {
	fmt.Printf("%s[claude-native] Error: %s%s\n", colorRed, errMsg, colorReset)

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		Output:      errMsg,
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "claude_native_error",
		SessionID:   cmd.SessionID,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[claude-native] Failed to publish error: %v%s\n", colorRed, err, colorReset)
	}
}

/* --------------------------------------------------------------------------
   Antigravity native (one-shot --print + --conversation resume) routing
   -------------------------------------------------------------------------- */

// handleAntigravityNativeCommand dispatches antigravity_native_* commands to
// the AntigravityNativeManager. Start registers a logical session (no process);
// Send runs one-shot agy --print with exact --conversation resume; End cancels
// any in-flight turn and drops the logical session.
func handleAntigravityNativeCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg) {
	if globalAntigravityNativeManager == nil {
		publishAntigravityNativeError(ctx, topic, cmd, "antigravity native manager not initialized")
		return
	}

	publishFn := newSessionPublishFn(topic, "[antigravity-native]")

	switch cmd.Type {
	case "antigravity_native_start":
		if cmd.SessionID == "" {
			publishAntigravityNativeError(ctx, topic, cmd, "sessionID is required for antigravity_native_start")
			return
		}

		fmt.Printf("%s[antigravity-native] Starting session %s (workspace=%s)%s\n",
			colorCyan, cmd.SessionID, cmd.WorkspaceID, colorReset)

		onStarted := func() {
			publishFn(resultMsg{
				ID:          cmd.ID,
				WorkspaceID: cmd.WorkspaceID,
				UID:         cmd.UID,
				Output:      "Antigravity native started",
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "antigravity_native_started",
				SessionID:   cmd.SessionID,
			})
		}

		err := globalAntigravityNativeManager.Start(
			cmd.SessionID,
			cmd.Cwd,
			cmd.WorkspaceID,
			cmd.UID,
			onStarted,
		)
		if err != nil {
			publishAntigravityNativeError(ctx, topic, cmd, fmt.Sprintf("failed to start antigravity native: %v", err))
			return
		}

	case "antigravity_native_send":
		if cmd.SessionID == "" {
			publishAntigravityNativeError(ctx, topic, cmd, "sessionID is required for antigravity_native_send")
			return
		}
		if cmd.Input == "" {
			publishAntigravityNativeError(ctx, topic, cmd, "input (user turn text) is required for antigravity_native_send")
			return
		}

		// Send is synchronous for the duration of the one-shot process so
		// Pub/Sub ack semantics match "turn accepted" after completion frames
		// are published. Run in-process; turn timeouts are enforced inside Send.
		timeout := time.Duration(0)
		if cmd.TimeoutMs > 0 {
			timeout = time.Duration(cmd.TimeoutMs) * time.Millisecond
		}
		if err := globalAntigravityNativeManager.Send(cmd.SessionID, cmd.Input, publishFn, timeout); err != nil {
			// Send publishes antigravity_native_error for ordinary turn failures
			// (timeout, empty response, oversize). Publish here only when Send
			// could not (missing/ended session) so the UI never stays stuck
			// running without a terminal frame. "session ended during turn" is
			// covered by antigravity_native_ended from End — do not double-fire.
			msg := err.Error()
			if strings.Contains(msg, "not found") ||
				(strings.Contains(msg, "has ended") && !strings.Contains(msg, "during turn")) {
				publishAntigravityNativeError(ctx, topic, cmd, fmt.Sprintf("failed to send to antigravity native: %v", err))
			}
			return
		}

	case "antigravity_native_end":
		if cmd.SessionID == "" {
			publishAntigravityNativeError(ctx, topic, cmd, "sessionID is required for antigravity_native_end")
			return
		}

		fmt.Printf("%s[antigravity-native] Ending session %s%s\n",
			colorYellow, cmd.SessionID, colorReset)

		if err := globalAntigravityNativeManager.End(cmd.SessionID); err != nil {
			// Still publish ended so the cloud can release reservations even if
			// the local session was already gone (idempotent teardown).
			publishFn(resultMsg{
				ID:          cmd.ID,
				WorkspaceID: cmd.WorkspaceID,
				UID:         cmd.UID,
				Output:      fmt.Sprintf("end: %v", err),
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
				Type:        "antigravity_native_ended",
				SessionID:   cmd.SessionID,
				ExitCode:    0,
			})
			return
		}
		publishFn(resultMsg{
			ID:          cmd.ID,
			WorkspaceID: cmd.WorkspaceID,
			UID:         cmd.UID,
			Output:      "Antigravity native ended",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "antigravity_native_ended",
			SessionID:   cmd.SessionID,
			ExitCode:    0,
		})

	default:
		publishAntigravityNativeError(ctx, topic, cmd, fmt.Sprintf("unknown antigravity native command type: %s", cmd.Type))
	}
}

// publishAntigravityNativeError surfaces a synchronous failure back to the
// orchestrator as an `antigravity_native_error` frame.
func publishAntigravityNativeError(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, errMsg string) {
	fmt.Printf("%s[antigravity-native] Error: %s%s\n", colorRed, errMsg, colorReset)

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		Output:      redactAntigravitySecrets(errMsg),
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "antigravity_native_error",
		SessionID:   cmd.SessionID,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[antigravity-native] Failed to publish error: %v%s\n", colorRed, err, colorReset)
	}
}

/* --------------------------------------------------------------------------
   Grok ACP (JSON-RPC over stdio) command routing
   -------------------------------------------------------------------------- */

// isGrokACPCommand returns true if cmdType is one of the Grok ACP command
// kinds. Same shape as isCodexAppServerCommand; both families share the
// JSON-RPC stdio dispatcher pattern.
func isGrokACPCommand(cmdType string) bool {
	switch cmdType {
	case "grok_acp_start", "grok_acp_send", "grok_acp_end":
		return true
	}
	return false
}

// handleGrokACPCommand dispatches grok_acp_* commands to the GrokACPManager.
// Mirrors handleCodexAppServerCommand's shape — a single publishFn is shared
// with the manager so it can stream JSON-RPC frames (responses, session/update
// notifications, server-initiated approval requests) back via Pub/Sub for the
// duration of the session. cfg drives per-session policy: API-key fallback
// gate (Config.EnableGrokAPIKeyFallback), always-approve gate
// (Config.EnableGrokAlwaysApprove), and workspace-root containment
// (Config.WorkingDirectory).
func handleGrokACPCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, cfg *Config) {
	if globalGrokACPManager == nil {
		publishGrokACPError(ctx, topic, cmd, "grok acp manager not initialized")
		return
	}

	publishFn := newSessionPublishFn(topic, "[grok-acp]")

	switch cmd.Type {
	case "grok_acp_start":
		if cmd.SessionID == "" {
			publishGrokACPError(ctx, topic, cmd, "sessionID is required for grok_acp_start")
			return
		}

		fmt.Printf("%s[grok-acp] Starting session %s (workspace=%s)%s\n",
			colorCyan, cmd.SessionID, cmd.WorkspaceID, colorReset)

		opts := GrokStartOptions{
			TimeoutMs:           cmd.TimeoutMs,
			AllowAPIKeyFallback: cfg != nil && cfg.EnableGrokAPIKeyFallback,
			AllowAlwaysApprove:  cfg != nil && cfg.EnableGrokAlwaysApprove,
		}
		if cfg != nil {
			opts.WorkspaceRoot = cfg.WorkingDirectory
		}

		err := globalGrokACPManager.Start(
			cmd.SessionID,
			cmd.Cwd,
			cmd.Args,
			cmd.WorkspaceID,
			cmd.UID,
			opts,
			publishFn,
		)
		if err != nil {
			publishGrokACPError(ctx, topic, cmd, fmt.Sprintf("failed to start grok acp: %v", err))
			return
		}

		// Synchronous ack so the orchestrator can proceed to send the ACP
		// `initialize` request as soon as the pipe is up. The first
		// grok_acp_message will follow once grok emits its initialize
		// response on stdout.
		publishFn(resultMsg{
			ID:          cmd.ID,
			WorkspaceID: cmd.WorkspaceID,
			UID:         cmd.UID,
			Output:      "Grok ACP started",
			Status:      "success",
			Ts:          time.Now().UnixMilli(),
			Version:     Version,
			Type:        "grok_acp_started",
			SessionID:   cmd.SessionID,
		})

		// Arm the first-frame watchdog AFTER the ack publish. publishFn
		// (newSessionPublishFn) can block for up to 30s when Pub/Sub is slow;
		// arming the watchdog from Start would include that publish latency
		// in the 45s budget and risk killing a healthy grok that is just
		// waiting on the orchestrator's `initialize` frame.
		globalGrokACPManager.ArmFirstFrameWatchdog(cmd.SessionID, publishFn)

	case "grok_acp_send":
		if cmd.SessionID == "" {
			publishGrokACPError(ctx, topic, cmd, "sessionID is required for grok_acp_send")
			return
		}
		if cmd.Input == "" {
			publishGrokACPError(ctx, topic, cmd, "input (JSON-RPC frame) is required for grok_acp_send")
			return
		}

		if err := globalGrokACPManager.Send(cmd.SessionID, cmd.Input); err != nil {
			publishGrokACPError(ctx, topic, cmd, fmt.Sprintf("failed to send to grok acp: %v", err))
			return
		}

	case "grok_acp_end":
		if cmd.SessionID == "" {
			publishGrokACPError(ctx, topic, cmd, "sessionID is required for grok_acp_end")
			return
		}

		fmt.Printf("%s[grok-acp] Ending session %s%s\n",
			colorYellow, cmd.SessionID, colorReset)

		if err := globalGrokACPManager.End(cmd.SessionID); err != nil {
			publishGrokACPError(ctx, topic, cmd, fmt.Sprintf("failed to end grok acp session: %v", err))
			return
		}

	default:
		publishGrokACPError(ctx, topic, cmd, fmt.Sprintf("unknown grok acp command type: %s", cmd.Type))
	}
}

// publishGrokACPError surfaces a synchronous failure (bad request, manager
// not ready, send failure) back to the orchestrator as a `grok_acp_error`
// frame so it can fail the in-flight call without waiting for a timeout.
func publishGrokACPError(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg, errMsg string) {
	fmt.Printf("%s[grok-acp] Error: %s%s\n", colorRed, errMsg, colorReset)

	res := resultMsg{
		ID:          cmd.ID,
		WorkspaceID: cmd.WorkspaceID,
		UID:         cmd.UID,
		Output:      errMsg,
		Status:      "error",
		Ts:          time.Now().UnixMilli(),
		Version:     Version,
		Type:        "grok_acp_error",
		SessionID:   cmd.SessionID,
	}
	if err := publishMsg(ctx, topic, res); err != nil {
		fmt.Printf("%s[grok-acp] Failed to publish error: %v%s\n", colorRed, err, colorReset)
	}
}
