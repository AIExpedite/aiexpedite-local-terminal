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
		Args:            redactArgs(cmd.Args),
		RejectionReason: reason,
	}
	// Session-routed commands need Type/SessionID set so the backend can
	// correlate the rejection with the session document.
	if cmd.Type != "" && cmd.SessionID != "" {
		res.Type = "session_error"
		res.SessionID = cmd.SessionID
	}
	return res
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
// Field order must match: id, command, args, ts, type, sessionID, input, signal
type signaturePayload struct {
	ID        string   `json:"id"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Ts        int64    `json:"ts"`
	Type      string   `json:"type"`
	SessionID string   `json:"sessionID"`
	Input     string   `json:"input"`
	Signal    string   `json:"signal"`
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
		ID:        cmd.ID,
		Command:   cmd.Command,
		Args:      args,
		Ts:        cmd.Ts,
		Type:      cmd.Type,
		SessionID: cmd.SessionID,
		Input:     cmd.Input,
		Signal:    cmd.Signal,
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

	// Session fields (for interactive CLI agent sessions)
	Type      string `json:"type,omitempty"`      // "execute"|"session_start"|"session_input"|"session_signal"|"session_end"
	SessionID string `json:"sessionID,omitempty"` // Unique session identifier
	Input     string `json:"input,omitempty"`     // stdin text for session_input
	Signal    string `json:"signal,omitempty"`    // "interrupt"|"kill" for session_signal
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
	Type       string `json:"type,omitempty"`       // "result"|"stream"|"prompt"|"session_ended"
	SessionID  string `json:"sessionID,omitempty"`  // Session identifier
	ExitCode   int    `json:"exitCode,omitempty"`   // Process exit code (for session_ended)
	PromptText string `json:"promptText,omitempty"` // The question/approval text from CLI
	PromptType string `json:"promptType,omitempty"` // "permission"|"question"|"unknown"
	Seq        int    `json:"seq,omitempty"`        // Ordering sequence number for streaming
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

		// Reject oversized messages before parsing as a defense against memory
		// exhaustion from malformed or unexpectedly large Pub/Sub messages.
		//
		// Sized to accommodate large positional args — specifically the
		// kickoff-brief pattern in ai-service's TerminalWithFeatureDetailsTool,
		// which passes a verbatim feature spec (tens to hundreds of KB) as
		// args[0] so claude can be seeded with the signed-off doc without
		// AI paraphrasing. Briefs over ~78 KB were previously silently dropped
		// here and stranded the session at status=starting forever.
		//
		// 1 MB matches the next natural ceiling — Firestore's per-document cap,
		// which terminal-service hits when it writes args onto the
		// terminalSession doc. If something ever exceeds 1 MB the Firestore
		// write will throw a clear error instead of the silent pubsub drop.
		// Pub/Sub itself allows up to 10 MB, so this is well within transport
		// limits.
		const maxMessageSize = 1024 * 1024 // 1 MB
		if len(m.Data) > maxMessageSize {
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
			// bounded. If the JSON is malformed OR there's no sessionID
			// to attribute the failure to, fall through to the bare ack
			// (we have nothing to fail).
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err == nil && cmd.SessionID != "" {
				publishSessionError(ctx, topic, cmd,
					fmt.Sprintf("Command rejected: payload size %d bytes exceeds %d byte limit",
						len(m.Data), maxMessageSize))
			}

			m.Ack() // Ack so it isn't redelivered forever
			return
		}

		// Parse command silently (verbose logging removed in v0.4.12)
		var cmd commandMsg
		if err := json.Unmarshal(m.Data, &cmd); err != nil {
			fmt.Printf("%s[aiexpedite] Bad command payload: %v%s\n", colorRed, err, colorReset)
			m.Nack()
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
		// stale queued commands when terminal reconnects after being offline
		if isCommandStale(cmd.Ts) {
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
		if !checkRateLimit(cmd.UID, cmd.AgentID, cfg) {
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

		// ─── Interactive Session Routing ─────────────────────────────────
		// Route session_* commands to the SessionManager instead of shell execution
		if cmd.Type != "" && cmd.Type != "execute" {
			// Apply allowlist to session_start (same rules as execute commands)
			if cmd.Type == "session_start" && cfg.EnableAllowList && defaultAllowList != nil && !defaultAllowList.IsAllowed(cmd.Command, cmd.Args) {
				timeoutSec := cfg.ApprovalTimeoutSec
				if timeoutSec <= 0 {
					timeoutSec = 60
				}
				result := ShowCommandApprovalDialog(cmd.Command, cmd.Args, timeoutSec)
				if result == ApprovalDeny && cfg.ApprovalTimeoutAction == "allow" {
					result = ApprovalOnce
				}
				switch result {
				case ApprovalDeny:
					fmt.Printf("%s[aiexpedite] Session command denied by user%s\n", colorYellow, colorReset)
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
					pattern := GeneratePatternFromCommand(cmd.Command, cmd.Args)
					if err := defaultAllowList.AddPattern(pattern); err != nil {
						fmt.Printf("%s[aiexpedite] Failed to add pattern to allow list: %v%s\n", colorYellow, err, colorReset)
					}
				}
			}
			handleSessionCommand(ctx, topic, cmd)
			m.Ack()
			return
		}
		// ─────────────────────────────────────────────────────────────────

		// ─── Command Allow List Validation ───────────────────────────────
		if cfg.EnableAllowList && defaultAllowList != nil && !defaultAllowList.IsAllowed(cmd.Command, cmd.Args) {
			// Command not in allow list - show approval dialog

			// Get timeout settings from config
			timeoutSec := cfg.ApprovalTimeoutSec
			if timeoutSec <= 0 {
				timeoutSec = 60
			}

			// Show approval dialog
			result := ShowCommandApprovalDialog(cmd.Command, cmd.Args, timeoutSec)

			// Handle timeout based on config
			if result == ApprovalDeny && cfg.ApprovalTimeoutAction == "allow" {
				result = ApprovalOnce
			}

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
				pattern := GeneratePatternFromCommand(cmd.Command, cmd.Args)
				if err := defaultAllowList.AddPattern(pattern); err != nil {
					fmt.Printf("%s[aiexpedite] Failed to add pattern to allow list: %v%s\n", colorYellow, err, colorReset)
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

		// Execute command (silently - no internal logs)
		out, execErr := runLocalCommand(cfg, cmd.Command, cmd.Args, cmd.Cwd, cmd.TimeoutMs)

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
func runLocalCommand(cfg *Config, cmd string, args []string, cwd string, timeoutMs int64) (string, error) {
	// Default timeout: 120 seconds (matches server-side default)
	if timeoutMs <= 0 {
		timeoutMs = 120000
	}
	// Cap timeout at 4 hours regardless of what the server sends.
	// An unbounded caller-supplied timeout would hold a Pub/Sub receive goroutine
	// indefinitely, silently exhausting MaxOutstandingMessages slots and
	// preventing any new commands from being processed.
	// 4 hours accommodates long-running operations such as codex agent runs.
	const maxTimeoutMs = 4 * 60 * 60 * 1000 // 4 hours
	if timeoutMs > maxTimeoutMs {
		timeoutMs = maxTimeoutMs
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	// Set working directory with tracked cwd support:
	// 1. If server sent an explicit cwd that differs from the config default, use it (user changed settings)
	// 2. If server sent the same default cwd as config, prefer tracked cwd (user may have cd'd)
	// 3. If no cwd at all, use tracked cwd or config default
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

	// CLI coding agents (claude, codex, gemini) use interactive/streaming output
	// that doesn't work with persistent PowerShell stdin pipes.
	// Always spawn a new powershell.exe process for these commands.
	cmdLower := strings.ToLower(cmd)
	isCLIAgent := cmdLower == "claude" || cmdLower == "codex" || cmdLower == "gemini"
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
	probeCmd := cmdLine + "\nWrite-Host '" + cwdSentinel + "'\n(Get-Location).Path"

	// `-OutputFormat Text` prevents CLIXML error serialization — see
	// runEncodedPowerShellCommand for the full explanation.
	c := exec.CommandContext(ctx, psExe, "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-Command", probeCmd)
	hideWindow(c)
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

/* --------------------------------------------------------------------------
   handleSessionCommand — routes session_* commands to the SessionManager
   -------------------------------------------------------------------------- */

// handleSessionCommand handles interactive session commands (session_start,
// session_input, session_signal, session_end). It publishes results back
// via the provided Pub/Sub topic.
func handleSessionCommand(ctx context.Context, topic *pubsub.Publisher, cmd commandMsg) {
	if globalSessionManager == nil {
		publishSessionError(ctx, topic, cmd, "session manager not initialized")
		return
	}

	// Create a publish function that sends results via Pub/Sub.
	// Use context.Background() rather than the sub.Receive callback ctx: the
	// Pub/Sub library cancels that ctx once the message is acked, which happens
	// immediately after session_start returns — but stream/prompt/session_ended
	// messages are published for minutes afterward.  A cancelled context would
	// silently drop every subsequent message.
	publishFn := func(res resultMsg) {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Printf("%s[session] Failed to marshal result: %v%s\n", colorRed, err, colorReset)
			return
		}
		pubCtx, pubCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, pubErr := topic.Publish(pubCtx, &pubsub.Message{Data: data}).Get(pubCtx)
		pubCancel()
		if pubErr != nil {
			fmt.Printf("%s[session] Failed to publish result: %v%s\n", colorRed, pubErr, colorReset)
		}
	}

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
