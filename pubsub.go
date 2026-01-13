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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/pubsub"
	"github.com/getlantern/systray"
	"golang.org/x/time/rate"
)

/* --------------------------------------------------------------------------
   Console Color Output
   -------------------------------------------------------------------------- */

// ANSI color codes for console output
const (
	colorReset   = "\033[0m"
	colorCyan    = "\033[36m"    // Received commands
	colorGreen   = "\033[32m"    // Success output
	colorYellow  = "\033[33m"    // Warnings
	colorRed     = "\033[31m"    // Errors
	colorMagenta = "\033[35m"    // System messages
	colorBlue    = "\033[34m"    // Info/metadata
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

/* --------------------------------------------------------------------------
   Per-UID Rate Limiting
   -------------------------------------------------------------------------- */

var (
	// Per-UID rate limiters: 10 commands/second with burst of 20
	uidRateLimiters = make(map[string]*rate.Limiter)
	uidRateMutex    sync.RWMutex
)

// getUIDRateLimiter returns or creates a rate limiter for the given UID
func getUIDRateLimiter(uid string, cfg *Config) *rate.Limiter {
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

	// Use config values with defaults
	rateLimit := cfg.RateLimitPerSecond
	if rateLimit <= 0 {
		rateLimit = 10
	}
	burst := cfg.RateLimitBurst
	if burst <= 0 {
		burst = 20
	}

	limiter = rate.NewLimiter(rate.Limit(rateLimit), burst)
	uidRateLimiters[uid] = limiter
	return limiter
}

// checkRateLimit checks if a command should be rate-limited
// Returns true if the command is allowed, false if rate-limited
func checkRateLimit(uid string, cfg *Config) bool {
	if uid == "" {
		return true // Allow commands without UID (backwards compatibility)
	}
	limiter := getUIDRateLimiter(uid, cfg)
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
	// Use configurable MaxOutstandingMessages for parallel processing
	sub.ReceiveSettings.MaxOutstandingMessages = cfg.MaxOutstandingMessages
	if sub.ReceiveSettings.MaxOutstandingMessages <= 0 {
		sub.ReceiveSettings.MaxOutstandingMessages = 5 // Default to 5 for better throughput
	}
	fmt.Printf("[pubsub] MaxOutstandingMessages set to %d\n", sub.ReceiveSettings.MaxOutstandingMessages)

	topic := client.Topic(cfg.ResultsTopic)

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

			// Parse command silently (verbose logging removed in v0.4.12)
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err != nil {
				fmt.Printf("%s[aiexpedite] Bad command payload: %v%s\n", colorRed, err, colorReset)
				m.Nack()
				return
			}

			// ─── Per-UID Rate Limiting ─────────────────────────────────────────
			if !checkRateLimit(cmd.UID, cfg) {
				fmt.Printf("%s[aiexpedite] Rate limit exceeded%s\n", colorYellow, colorReset)

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
					fmt.Printf("%s[aiexpedite] Command targeted at different agent%s\n", colorYellow, colorReset)
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
					fmt.Printf("%s[aiexpedite] Command missing signature%s\n", colorRed, colorReset)
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
					fmt.Printf("%s[aiexpedite] Invalid command signature%s\n", colorRed, colorReset)
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
				// Signature verified - proceed silently
			}
			// ─────────────────────────────────────────────────────────────────

			// ─── Special Ping Command Handler ────────────────────────────────
			// Responds immediately without shell execution for online status checks
			if cmd.Command == "__ping__" {
				// Ping is silent - no console output
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
					fmt.Printf("%s[aiexpedite] Ping publish error: %v%s\n", colorRed, err, colorReset)
				}
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
			fmt.Printf("%s> %s%s\n", colorGreen, redactSensitiveData(cmdDisplay), colorReset)

			// Execute command (silently - no internal logs)
			out, execErr := runLocalCommand(cfg, cmd.Command, cmd.Args, cmd.Cwd)

			// Show output or terminal error
			if execErr != nil {
				// Terminal error - shown in red without [aiexpedite] prefix
				fmt.Printf("%s%v%s\n", colorRed, execErr, colorReset)
				if out != "" {
					fmt.Println(out)
				}
			} else if out != "" {
				fmt.Println(out)
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

						// Get reusable GCS client (much faster than creating per command)
						storageClient, storageErr := GetStorageClient(ctx)
						if storageErr != nil {
							fmt.Printf("[file-upload] Failed to get storage client: %v\n", storageErr)
						} else {
							// Don't close - client is reused globally

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
			// Publish result silently - only log errors
			if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
				fmt.Printf("%s[aiexpedite] Publish error: %v%s\n", colorRed, err, colorReset)
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

// runEncodedPowerShellCommand executes a Base64-encoded PowerShell script.
// This is the most reliable way to execute PowerShell commands as it completely
// bypasses all shell escaping issues.
// It captures stdout and stderr separately and filters CLIXML progress messages
// to avoid false "exit status 1" errors when commands produce valid output.
func runEncodedPowerShellCommand(encodedScript string, workDir string) (string, error) {
	// Build PowerShell arguments
	psArgs := []string{
		"-NoProfile",
		"-NonInteractive",
		"-EncodedCommand",
		encodedScript,
	}

	c := exec.Command("powershell.exe", psArgs...)
	if workDir != "" {
		c.Dir = workDir
	}

	// Capture stdout and stderr separately to handle CLIXML filtering
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()

	stdoutStr := stdout.String()
	stderrStr := stderr.String()

	// Filter CLIXML from stderr (progress/verbose messages)
	filteredStderr := filterCLIXML(stderrStr)

	// Combine output (stdout first, then filtered stderr if any)
	output := stdoutStr
	if strings.TrimSpace(filteredStderr) != "" {
		output += "\n" + filteredStderr
	}

	// Determine if this is a real error
	var finalErr error
	if err != nil {
		hasOnlyCLIXML := stderrStr != "" && strings.TrimSpace(filteredStderr) == ""
		hasMeaningfulOutput := strings.TrimSpace(stdoutStr) != ""

		if hasOnlyCLIXML && hasMeaningfulOutput {
			// CLIXML on stderr but valid stdout - not a real error
			finalErr = nil
		} else if hasMeaningfulOutput {
			// Has output despite error - return output
			finalErr = nil
		} else {
			// Real error - no useful output
			finalErr = err
		}
	}

	return output, finalErr
}

/* runLocalCommand executes the command using persistent PowerShell for low latency. */
func runLocalCommand(cfg *Config, cmd string, args []string, cwd string) (string, error) {
	// Set working directory: priority is command cwd > config default > process cwd
	workDir := cwd
	if workDir == "" && cfg != nil {
		workDir = cfg.WorkingDirectory
	}

	// Check if this is an encoded PowerShell command (already Base64 encoded by terminal-service)
	isEncodedPowerShell := strings.ToLower(cmd) == "powershell" &&
		len(args) >= 2 &&
		strings.ToLower(args[0]) == "-encodedcommand"

	if isEncodedPowerShell {
		return runEncodedPowerShellCommand(args[1], workDir)
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
		return runEncodedPowerShellCommand(encoded, workDir)
	}

	// Original behavior for non-PowerShell commands
	// Construct the full command line
	cmdLine := cmd
	if len(args) > 0 {
		for _, arg := range args {
			if containsSpace(arg) {
				cmdLine += " \"" + arg + "\""
			} else {
				cmdLine += " " + arg
			}
		}
	}

	// Try persistent PowerShell first (much faster - avoids 300-800ms startup)
	ps, err := GetPowerShell()
	if err != nil {
		fmt.Printf("%s[aiexpedite] Persistent PowerShell unavailable, using fallback%s\n", colorYellow, colorReset)
		return runLocalCommandFallback(cmdLine, workDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	output, err := ps.Execute(ctx, cmdLine, workDir)
	if err != nil {
		// If persistent PS failed, try restarting it once
		if restartErr := RestartPowerShell(); restartErr != nil {
			fmt.Printf("%s[aiexpedite] PowerShell restart failed, using fallback%s\n", colorYellow, colorReset)
			return runLocalCommandFallback(cmdLine, workDir)
		}

		// Try again with fresh process
		ps, err = GetPowerShell()
		if err != nil {
			return runLocalCommandFallback(cmdLine, workDir)
		}

		output, err = ps.Execute(ctx, cmdLine, workDir)
		if err != nil {
			return runLocalCommandFallback(cmdLine, workDir)
		}
	}

	return output, nil
}

// runLocalCommandFallback uses traditional process spawning (slow but reliable)
func runLocalCommandFallback(cmdLine string, workDir string) (string, error) {
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmdLine)
	if workDir != "" {
		c.Dir = workDir
	}

	out, err := c.CombinedOutput()
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
