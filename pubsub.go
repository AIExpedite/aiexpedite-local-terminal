// File: pubsub.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
	ID     string `json:"id"`
	UID    string `json:"uid"`
	Output string `json:"output"`
	Status string `json:"status"` // "success" | "error"
	Ts     int64  `json:"ts"`
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
