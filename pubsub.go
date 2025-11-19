// File: pubsub.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/pubsub"
)

/* Incoming command payload (matches backend publishCommand struct) */
type commandMsg struct {
	ID      string   `json:"id"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	UID     string   `json:"uid"`
	Ts      int64    `json:"ts"`
}

/* Outgoing result payload (matches backend publishResult struct) */
type resultMsg struct {
	ID           string        `json:"id"`
	UID          string        `json:"uid"`
	Output       string        `json:"output"`
	Status       string        `json:"status"` // "success" | "partial" | "error"
	Ts           int64         `json:"ts"`
	Files        []FileInfo    `json:"files,omitempty"`        // Uploaded file metadata
	UploadErrors []UploadError `json:"uploadErrors,omitempty"` // File upload failures
}

/* --------------------------------------------------------------------------
   StartPubSubLoop – listens for commands and publishes results
   -------------------------------------------------------------------------- */
func StartPubSubLoop(cfg *Config) {
	fmt.Println("[pubsub] StartPubSubLoop called")
	fmt.Printf("[pubsub] Config: ProjectID=%s, Subscription=%s, Topic=%s\n", cfg.ProjectID, cfg.CommandsSubscription, cfg.ResultsTopic)

	if cfg.ProjectID == "" {
		fmt.Println("[pubsub] disabled – project_id empty")
		return
	}

	fmt.Println("[pubsub] creating Pub/Sub client...")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-shutdownChan; cancel() }() // graceful exit on tray quit

	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		fmt.Println("[pubsub] client error:", err)
		return
	}
	fmt.Println("[pubsub] client created successfully")
	// Ensure underlying connections close when ctx is done
	go func() {
		<-ctx.Done()
		_ = client.Close()
	}()

	sub := client.Subscription(cfg.CommandsSubscription)
	sub.ReceiveSettings.Synchronous = true
	sub.ReceiveSettings.MaxOutstandingMessages = 1

	topic := client.Topic(cfg.ResultsTopic)

	fmt.Printf("[pubsub] connecting to subscription: %s\n", cfg.CommandsSubscription)

	go func() {
		fmt.Printf("[pubsub] listening for commands on: %s\n", cfg.CommandsSubscription)
		err := sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
			fmt.Printf("[pubsub] received command: %s\n", string(m.Data))
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err != nil {
				fmt.Println("[pubsub] bad payload:", err)
				m.Nack()
				return
			}

			fmt.Printf("[pubsub] executing command: %s %v\n", cmd.Command, cmd.Args)
			out, execErr := runLocalCommand(cmd.Command, cmd.Args)
			fmt.Printf("[pubsub] command output (length=%d): %s\n", len(out), out)
			if execErr != nil {
				fmt.Printf("[pubsub] command error: %v\n", execErr)
			}

			res := resultMsg{
				ID:     cmd.ID,
				UID:    cmd.UID,
				Output: out,
				Status: "success",
				Ts:     time.Now().UnixMilli(),
			}
			if execErr != nil {
				res.Status = "error"
				res.Output = execErr.Error() + "\n" + out
			}

			// File upload integration
			if cfg.EnableFileUpload && res.Status != "error" {
				files := detectOutputFiles(cmd.Command, out)
				if len(files) > 0 {
					fmt.Printf("[file-upload] Detected %d output files, uploading to GCS...\n", len(files))

					// Initialize GCS client
					storageClient, storageErr := InitStorageClient(ctx)
					if storageErr != nil {
						fmt.Printf("[file-upload] Failed to initialize storage client: %v\n", storageErr)
					} else {
						defer storageClient.Close()

						// Create logger
						logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

						// Upload files
						workspaceID := extractWorkspaceID(cmd)
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

			bytes, _ := json.Marshal(res)
			fmt.Printf("[pubsub] publishing result to topic: %s (payload size: %d bytes)\n", cfg.ResultsTopic, len(bytes))
			fmt.Printf("[pubsub] result preview - ID: %s, UID: %s, Status: %s, OutputLength: %d\n", res.ID, res.UID, res.Status, len(res.Output))
			// Only print first 500 chars to avoid buffer issues
			if len(string(bytes)) > 500 {
				fmt.Printf("[pubsub] result JSON (truncated): %s...\n", string(bytes)[:500])
			} else {
				fmt.Printf("[pubsub] result JSON: %s\n", string(bytes))
			}
			if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
				fmt.Println("[pubsub] publish error:", err)
			} else {
				fmt.Println("[pubsub] result published successfully")
			}
			m.Ack()
			fmt.Println("[pubsub] message acknowledged")
		})
		if err != nil && ctx.Err() == nil { // ignore shutdown‑induced error
			fmt.Println("[pubsub] subscription ended:", err)
		}
	}()
}

/* runLocalCommand executes the command and returns combined stdout/stderr. */
func runLocalCommand(cmd string, args []string) (string, error) {
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
	c := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmdLine)
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
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
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

// extractWorkspaceID extracts workspaceID from command context
// TODO: Pass workspaceID in command metadata in future
func extractWorkspaceID(cmd commandMsg) string {
	// Placeholder: return default workspace
	// In future, parse from command args or metadata
	return "default-workspace"
}
