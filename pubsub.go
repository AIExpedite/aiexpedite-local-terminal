// File: pubsub.go
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/getlantern/systray"
	"golang.org/x/time/rate"
)

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

/* --------------------------------------------------------------------------
   Per-UID Rate Limiting
   -------------------------------------------------------------------------- */

var (
	// Per-UID rate limiters: 10 commands/second with burst of 20
	uidRateLimiters = make(map[string]*rate.Limiter)
	uidRateMutex    sync.RWMutex
)

// getUIDRateLimiter returns or creates a rate limiter for the given UID
func getUIDRateLimiter(uid string) *rate.Limiter {
	uidRateMutex.RLock()
	limiter, exists := uidRateLimiters[uid]
	uidRateMutex.RUnlock()

	if exists {
		return limiter
	}

	uidRateMutex.Lock()
	defer uidRateMutex.Unlock()

	// Double-check after acquiring write lock
	if limiter, exists = uidRateLimiters[uid]; exists {
		return limiter
	}

	// Create new limiter: 10 commands per second with burst of 20
	limiter = rate.NewLimiter(rate.Limit(10), 20)
	uidRateLimiters[uid] = limiter
	return limiter
}

// checkRateLimit checks if a command should be rate-limited
// Returns true if the command is allowed, false if rate-limited
func checkRateLimit(uid string) bool {
	if uid == "" {
		return true // Allow commands without UID (backwards compatibility)
	}
	limiter := getUIDRateLimiter(uid)
	return limiter.Allow()
}

/* --------------------------------------------------------------------------
   Command Signature Verification (HMAC-SHA256)
   -------------------------------------------------------------------------- */

// verifySignature verifies the HMAC-SHA256 signature of a command
// Returns true if signature is valid, false otherwise
func verifySignature(cmd commandMsg, secret string) bool {
	// Create canonical representation matching backend signCommand()
	signatureData := fmt.Sprintf(`{"id":"%s","command":"%s","args":%s,"ts":%d}`,
		cmd.ID,
		cmd.Command,
		argsToJSON(cmd.Args),
		cmd.Ts,
	)

	// Compute expected signature
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signatureData))
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
	Cwd         string   `json:"cwd,omitempty"`       // Working directory for command execution
	WorkspaceID string   `json:"workspaceID"`         // Workspace scope for file uploads
	UID         string   `json:"uid"`
	Ts          int64    `json:"ts"`
	AgentID     string   `json:"agentId,omitempty"`   // Target agent for signature verification
	Signature   string   `json:"signature,omitempty"` // HMAC-SHA256 signature of command
}

/* Outgoing result payload (matches backend publishResult struct) */
type resultMsg struct {
	ID           string        `json:"id"`
	WorkspaceID  string        `json:"workspaceID,omitempty"` // Workspace scope for audit trail
	UID          string        `json:"uid"`
	Output       string        `json:"output"`
	Status       string        `json:"status"` // "success" | "partial" | "error" | "denied" | "rate_limited" | "unauthorized"
	Ts           int64         `json:"ts"`
	Version      string        `json:"version,omitempty"`      // Terminal app version
	Files        []FileInfo    `json:"files,omitempty"`        // Uploaded file metadata
	UploadErrors []UploadError `json:"uploadErrors,omitempty"` // File upload failures
}

/* --------------------------------------------------------------------------
   StartPubSubLoop – reconnection wrapper with exponential backoff
   -------------------------------------------------------------------------- */
func StartPubSubLoop(cfg *Config) {
	fmt.Println("[pubsub] StartPubSubLoop called")
	fmt.Printf("[pubsub] Config: ProjectID=%s, Subscription=%s, Topic=%s\n", cfg.ProjectID, cfg.CommandsSubscription, cfg.ResultsTopic)

	if cfg.ProjectID == "" {
		fmt.Println("[pubsub] disabled – project_id empty")
		return
	}

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

		fmt.Printf("[pubsub] connection lost: %v\n", err)
		fmt.Printf("[pubsub] reconnecting in %v...\n", backoff)
		systray.SetTooltip(EnvDisplayName + " – Reconnecting...")

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

		// Exponential backoff (1s → 1.5s → 2.25s → ... → 5min max)
		backoff = time.Duration(float64(backoff) * 1.5)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

/* --------------------------------------------------------------------------
   runPubSubConnection – handles a single connection attempt
   Returns nil on clean shutdown, error on connection failure
   -------------------------------------------------------------------------- */
func runPubSubConnection(cfg *Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for shutdown or offline signal
	go func() {
		select {
		case <-shutdownChan:
			cancel()
		case offline := <-offlineChan:
			if offline {
				SetOffline(true)
				cancel()
			} else {
				// Re-queue the online signal for the main loop
				select {
				case offlineChan <- false:
				default:
				}
			}
		}
	}()

	fmt.Println("[pubsub] creating Pub/Sub client...")
	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return fmt.Errorf("client creation failed: %w", err)
	}
	defer client.Close()

	fmt.Println("[pubsub] client created successfully")

	sub := client.Subscription(cfg.CommandsSubscription)
	sub.ReceiveSettings.Synchronous = true
	sub.ReceiveSettings.MaxOutstandingMessages = 1

	topic := client.Topic(cfg.ResultsTopic)

	fmt.Printf("[pubsub] connected to subscription: %s\n", cfg.CommandsSubscription)
	systray.SetTooltip(EnvDisplayName + " – Connected")

	fmt.Printf("[pubsub] listening for commands on: %s\n", cfg.CommandsSubscription)
	err = sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
			// Panic recovery to prevent app crash on unhandled errors
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[pubsub] PANIC in message handler: %v\n", r)
					m.Nack() // Let Pub/Sub redeliver
				}
			}()

			// Redact sensitive data before logging received command
			fmt.Printf("[pubsub] received command: %s\n", redactSensitiveData(string(m.Data)))
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err != nil {
				fmt.Println("[pubsub] bad payload:", err)
				m.Nack()
				return
			}

			// ─── Per-UID Rate Limiting ─────────────────────────────────────────
			if !checkRateLimit(cmd.UID) {
				fmt.Printf("[security] Rate limit exceeded for UID: %s\n", cmd.UID)

				// Send rate_limited response back to user for immediate feedback
				res := resultMsg{
					ID:          cmd.ID,
					WorkspaceID: cmd.WorkspaceID,
					UID:         cmd.UID,
					Output:      "Command rate limit exceeded. Please wait before retrying.",
					Status:      "rate_limited",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
				}
				bytes, _ := json.Marshal(res)
				if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
					fmt.Println("[pubsub] publish error:", err)
				}
				m.Ack()
				return
			}
			// ─────────────────────────────────────────────────────────────────

			// ─── Command Signature Verification ──────────────────────────────
			// If agent has a secret configured, ALL commands MUST be signed (strict mode)
			if cfg.CommandSecret != "" {
				// Check if command is targeted at this agent
				if cmd.AgentID != "" && cmd.AgentID != cfg.AgentID {
					fmt.Printf("[security] Command targeted at different agent: %s (this agent: %s)\n", cmd.AgentID, cfg.AgentID)
					res := resultMsg{
						ID:          cmd.ID,
						WorkspaceID: cmd.WorkspaceID,
						UID:         cmd.UID,
						Output:      "Command rejected: targeted at different agent",
						Status:      "unauthorized",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
					}
					bytes, _ := json.Marshal(res)
					if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
						fmt.Println("[pubsub] publish error:", err)
					}
					m.Ack()
					return
				}

				// Verify signature (strict mode - no signature = reject)
				if cmd.Signature == "" {
					fmt.Printf("[security] Command missing signature (strict mode enabled)\n")
					res := resultMsg{
						ID:          cmd.ID,
						WorkspaceID: cmd.WorkspaceID,
						UID:         cmd.UID,
						Output:      "Command rejected: signature required but not provided",
						Status:      "unauthorized",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
					}
					bytes, _ := json.Marshal(res)
					if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
						fmt.Println("[pubsub] publish error:", err)
					}
					m.Ack()
					return
				}

				if !verifySignature(cmd, cfg.CommandSecret) {
					fmt.Printf("[security] Invalid command signature for ID: %s\n", cmd.ID)
					res := resultMsg{
						ID:          cmd.ID,
						WorkspaceID: cmd.WorkspaceID,
						UID:         cmd.UID,
						Output:      "Command rejected: invalid signature",
						Status:      "unauthorized",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
					}
					bytes, _ := json.Marshal(res)
					if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
						fmt.Println("[pubsub] publish error:", err)
					}
					m.Ack()
					return
				}
				fmt.Printf("[security] Command signature verified for ID: %s\n", cmd.ID)
			}
			// ─────────────────────────────────────────────────────────────────

			// ─── Special Ping Command Handler ────────────────────────────────
			// Responds immediately without shell execution for online status checks
			if cmd.Command == "__ping__" {
				fmt.Println("[pubsub] received ping command, responding immediately")
				res := resultMsg{
					ID:          cmd.ID,
					WorkspaceID: cmd.WorkspaceID,
					UID:         cmd.UID,
					Output:      "pong",
					Status:      "success",
					Ts:          time.Now().UnixMilli(),
					Version:     Version,
				}
				bytes, _ := json.Marshal(res)
				if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
					fmt.Println("[pubsub] ping publish error:", err)
				}
				m.Ack()
				return
			}
			// ─────────────────────────────────────────────────────────────────

			// ─── Command Allow List Validation ───────────────────────────────
			if cfg.EnableAllowList && defaultAllowList != nil && !defaultAllowList.IsAllowed(cmd.Command, cmd.Args) {
				// Redact args when logging security events
				fmt.Printf("[security] Command not in allow list: %s\n", redactCommandForLog(cmd.Command, cmd.Args))

				// Get timeout settings from config
				timeoutSec := cfg.ApprovalTimeoutSec
				if timeoutSec <= 0 {
					timeoutSec = 60
				}

				// Show approval dialog
				result := ShowCommandApprovalDialog(cmd.Command, cmd.Args, timeoutSec)

				// Handle timeout based on config
				if result == ApprovalDeny && cfg.ApprovalTimeoutAction == "allow" {
					fmt.Println("[security] Timeout - auto-allowing per config")
					result = ApprovalOnce
				}

				switch result {
				case ApprovalDeny:
					fmt.Println("[security] User denied command execution")
					if cfg.LogDeniedCommands {
						// Redact args in audit log
						fmt.Printf("[security-audit] DENIED: %s (user: %s)\n", redactCommandForLog(cmd.Command, cmd.Args), cmd.UID)
					}

					// Send denial result back to backend
					res := resultMsg{
						ID:          cmd.ID,
						WorkspaceID: cmd.WorkspaceID,
						UID:         cmd.UID,
						Output:      "Command denied by user: not in allow list",
						Status:      "denied",
						Ts:          time.Now().UnixMilli(),
						Version:     Version,
					}
					bytes, _ := json.Marshal(res)
					if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
						fmt.Println("[pubsub] publish error:", err)
					}
					m.Ack()
					return

				case ApprovalAlways:
					pattern := GeneratePatternFromCommand(cmd.Command, cmd.Args)
					if err := defaultAllowList.AddPattern(pattern); err != nil {
						fmt.Printf("[security] Failed to add pattern to allow list: %v\n", err)
					} else {
						fmt.Printf("[security] Added pattern to allow list: %s\n", pattern)
					}
					// Fall through to execute

				case ApprovalOnce:
					fmt.Println("[security] User approved command (once)")
					// Fall through to execute
				}
			}
			// ─────────────────────────────────────────────────────────────────

			// Redact command args before logging
			fmt.Printf("[pubsub] executing command: %s\n", redactCommandForLog(cmd.Command, cmd.Args))
			out, execErr := runLocalCommand(cfg, cmd.Command, cmd.Args, cmd.Cwd)
			// Redact output before logging (may contain secrets)
			fmt.Printf("[pubsub] command output (length=%d): %s\n", len(out), redactSensitiveData(out))
			if execErr != nil {
				fmt.Printf("[pubsub] command error: %v\n", execErr)
			}

			res := resultMsg{
				ID:          cmd.ID,
				WorkspaceID: cmd.WorkspaceID,
				UID:         cmd.UID,
				Output:      out,
				Status:      "success",
				Ts:          time.Now().UnixMilli(),
				Version:     Version,
			}
			if execErr != nil {
				res.Status = "error"
				res.Output = execErr.Error() + "\n" + out
			}

			// File upload integration
			if cfg.EnableFileUpload && res.Status != "error" {
				files := detectOutputFiles(cmd.Command, out)
				if len(files) > 0 {
					// Security: Block file upload if workspaceID is missing
					workspaceID := extractWorkspaceID(cmd)
					if workspaceID == "" {
						fmt.Println("[file-upload] BLOCKED - no workspaceID provided (security: refusing to upload to default bucket)")
					} else {
						fmt.Printf("[file-upload] Detected %d output files, uploading to GCS (workspace: %s)...\n", len(files), workspaceID)

						// Initialize GCS client
						storageClient, storageErr := InitStorageClient(ctx)
						if storageErr != nil {
							fmt.Printf("[file-upload] Failed to initialize storage client: %v\n", storageErr)
						} else {
							defer storageClient.Close()

							// Create logger
							logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

							// Upload files
							uploadResult := UploadFiles(
								ctx,
								storageClient,
								cfg.StorageBucket,
								files,
								workspaceID,
								cmd.ID,
								logger,
							)

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
						}
					}
				}
			}

			bytes, _ := json.Marshal(res)
			fmt.Printf("[pubsub] publishing result to topic: %s (payload size: %d bytes)\n", cfg.ResultsTopic, len(bytes))
			fmt.Printf("[pubsub] result preview - ID: %s, UID: %s, Status: %s, OutputLength: %d\n", res.ID, res.UID, res.Status, len(res.Output))
			// Redact result JSON before logging (contains output which may have secrets)
			if len(string(bytes)) > 500 {
				fmt.Printf("[pubsub] result JSON (truncated): %s...\n", redactSensitiveData(string(bytes)[:500]))
			} else {
				fmt.Printf("[pubsub] result JSON: %s\n", redactSensitiveData(string(bytes)))
			}
			if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
				fmt.Println("[pubsub] publish error:", err)
			} else {
				fmt.Println("[pubsub] result published successfully")
			}
			m.Ack()
		fmt.Println("[pubsub] message acknowledged")
	})

	// Return nil on clean shutdown, error otherwise
	if ctx.Err() != nil && !IsOffline() {
		return nil // Clean shutdown
	}
	return err
}

/* runLocalCommand executes the command and returns combined stdout/stderr. */
func runLocalCommand(cfg *Config, cmd string, args []string, cwd string) (string, error) {
	// On Windows, run commands through PowerShell to support built-ins like dir, ls, etc.
	// Construct the full command line
	cmdLine := cmd
	if len(args) > 0 {
		// Quote arguments that contain spaces
		for _, arg := range args {
			if containsSpace(arg) {
				cmdLine += " \"" + arg + "\""
			} else {
				cmdLine += " " + arg
			}
		}
	}

	// Execute via PowerShell
	fmt.Printf("[exec] Running PowerShell command: %s\n", cmdLine)
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmdLine)

	// Set working directory: priority is command cwd > config default > process cwd
	workDir := cwd
	if workDir == "" && cfg != nil {
		workDir = cfg.WorkingDirectory
	}
	if workDir != "" {
		c.Dir = workDir
		fmt.Printf("[exec] Working directory: %s\n", workDir)
	}

	out, err := c.CombinedOutput()
	if err != nil {
		fmt.Printf("[exec] PowerShell error: %v\n", err)
	}
	fmt.Printf("[exec] PowerShell output length: %d bytes\n", len(out))
	return string(out), err
}

func containsSpace(s string) bool {
	for _, r := range s {
		if r == ' ' {
			return true
		}
	}
	return false
}

/* --------------------------------------------------------------------------
   Path Safety Validation
   -------------------------------------------------------------------------- */

// isPathSafe validates that a path is within the current working directory
// to prevent path traversal attacks
func isPathSafe(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	// Ensure the path is within current working directory
	return strings.HasPrefix(abs, cwd)
}

/* --------------------------------------------------------------------------
   File Upload Helper Functions
   -------------------------------------------------------------------------- */

// detectOutputFiles finds files to upload based on command and output
func detectOutputFiles(command string, output string) []string {
	files := []string{}

	// Pattern 1: Playwright test artifacts
	if containsSubstring(command, "playwright") || containsSubstring(command, "pwtest") {
		fmt.Println("[file-upload] Detected Playwright command, scanning for test artifacts...")
		// Look for test-results directory
		files = appendFilesFromDir(files, "test-results", []string{".png", ".webm", ".mp4", ".json", ".html"})
		fmt.Printf("[file-upload] Found %d Playwright artifacts\n", len(files))
	}

	// Pattern 2: Generic screenshots in current directory
	if containsSubstring(command, "screenshot") || containsSubstring(output, "screenshot") {
		files = appendFilesFromDir(files, ".", []string{".png", ".jpg", ".jpeg"})
	}

	// Pattern 3: Video recordings
	if containsSubstring(command, "record") || containsSubstring(output, "recording") {
		files = appendFilesFromDir(files, ".", []string{".webm", ".mp4", ".mov"})
	}

	return files
}

// containsSubstring checks if haystack contains needle (case-insensitive)
func containsSubstring(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		(haystack == needle || strings.Contains(strings.ToLower(haystack), strings.ToLower(needle)))
}

// appendFilesFromDir recursively finds files with given extensions in directory
func appendFilesFromDir(files []string, dir string, extensions []string) []string {
	// Security: Validate directory is within safe boundaries
	if !isPathSafe(dir) {
		fmt.Printf("[security] Blocked path traversal attempt: %s\n", dir)
		return files
	}

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			// Security: Validate each file path as well
			if !isPathSafe(path) {
				fmt.Printf("[security] Blocked file path traversal: %s\n", path)
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			for _, targetExt := range extensions {
				if ext == targetExt {
					files = append(files, path)
					break
				}
			}
		}
		return nil
	})
	return files
}

// extractWorkspaceID extracts workspaceID from command message
// Returns empty string if not provided - caller must handle this case
func extractWorkspaceID(cmd commandMsg) string {
	return cmd.WorkspaceID
}
