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

/* -------------------------------------------------------------------------- */
/*  StartPubSubLoop – listens for commands and publishes results              */
/* -------------------------------------------------------------------------- */
func StartPubSubLoop(cfg *Config) {
	if cfg.ProjectID == "" {
		fmt.Println("[pubsub] disabled – project_id empty")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-shutdownChan; cancel() }() // graceful exit on tray quit

	client, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		fmt.Println("[pubsub] client error:", err)
		return
	}

	sub := client.Subscription(cfg.CommandsSubscription)
	sub.ReceiveSettings.Synchronous = true
	sub.ReceiveSettings.MaxOutstandingMessages = 1

	topic := client.Topic(cfg.ResultsTopic)

	go func() {
		err := sub.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
			var cmd commandMsg
			if err := json.Unmarshal(m.Data, &cmd); err != nil {
				fmt.Println("[pubsub] bad payload:", err)
				m.Nack()
				return
			}

			out, execErr := runLocalCommand(cmd.Command, cmd.Args)
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
			if _, err := topic.Publish(ctx, &pubsub.Message{Data: bytes}).Get(ctx); err != nil {
				fmt.Println("[pubsub] publish error:", err)
			}
			m.Ack()
		})
		if err != nil && ctx.Err() == nil { // ignore shutdown‑induced error
			fmt.Println("[pubsub] subscription ended:", err)
		}
	}()
}

/* runLocalCommand executes the command and returns combined stdout/stderr. */
func runLocalCommand(cmd string, args []string) (string, error) {
	c := exec.Command(cmd, args...)
	out, err := c.CombinedOutput()
	return string(out), err
}
