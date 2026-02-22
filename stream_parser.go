// File: stream_parser.go
// -----------------------------------------------------------------------------
// Parses structured JSON streaming output from CLI agents (Claude, Codex,
// Gemini) and extracts human-readable display text. This allows the frontend
// to show clean text instead of raw JSON events.
//
// Each CLI uses a different JSON streaming format:
//   - Claude: --output-format stream-json  (Anthropic API streaming events)
//   - Codex:  --json                       (JSONL events)
//   - Gemini: -o stream-json               (NDJSON events)
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// extractDisplayText parses a single stdout line from a CLI agent and returns
// the human-readable text to display. Returns empty string if the line should
// be skipped (internal events, metadata, etc.).
//
// For non-JSON lines (plain text, errors), returns the line as-is (passthrough).
func extractDisplayText(command, line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}

	// Quick check: must look like JSON to attempt parsing
	if !strings.HasPrefix(trimmed, "{") {
		return line // passthrough non-JSON lines (plain text, stderr, etc.)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return line // passthrough malformed JSON
	}

	cmdLower := strings.ToLower(command)
	switch {
	case cmdLower == "claude" || strings.HasPrefix(cmdLower, "claude"):
		return extractClaudeDisplayText(raw)
	case cmdLower == "codex" || strings.HasPrefix(cmdLower, "codex"):
		return extractCodexDisplayText(raw)
	case cmdLower == "gemini" || strings.HasPrefix(cmdLower, "gemini"):
		return extractGeminiDisplayText(raw)
	default:
		return line // passthrough unknown CLI agents
	}
}

/* --------------------------------------------------------------------------
   Claude Code parser
   -------------------------------------------------------------------------- */

// extractClaudeDisplayText extracts display text from a Claude stream-json event.
// Handles both the stream_event envelope format and flat Claude Code events.
func extractClaudeDisplayText(raw map[string]interface{}) string {
	eventType, _ := raw["type"].(string)

	// Handle stream_event envelope: unwrap inner event
	if eventType == "stream_event" {
		inner, ok := raw["event"].(map[string]interface{})
		if !ok {
			return ""
		}
		return extractClaudeInnerEvent(inner)
	}

	// Handle flat Claude Code events (message, tool_use, tool_result, result)
	return extractClaudeFlatEvent(raw, eventType)
}

// extractClaudeInnerEvent handles Anthropic API streaming events inside a
// stream_event envelope (content_block_delta, content_block_start, etc.).
func extractClaudeInnerEvent(event map[string]interface{}) string {
	eventType, _ := event["type"].(string)

	switch eventType {
	case "content_block_delta":
		delta, ok := event["delta"].(map[string]interface{})
		if !ok {
			return ""
		}
		deltaType, _ := delta["type"].(string)
		switch deltaType {
		case "text_delta":
			text, _ := delta["text"].(string)
			return text
		case "thinking_delta":
			thinking, _ := delta["thinking"].(string)
			return thinking
		default:
			// input_json_delta, signature_delta — skip
			return ""
		}

	case "content_block_start":
		block, ok := event["content_block"].(map[string]interface{})
		if !ok {
			return ""
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "tool_use":
			name, _ := block["name"].(string)
			if name != "" {
				return fmt.Sprintf("\n[Using tool: %s]\n", name)
			}
		case "thinking":
			return "\n--- Thinking ---\n"
		}
		return ""

	case "content_block_stop", "message_start", "message_delta",
		"message_stop", "ping":
		return "" // metadata events, skip
	}

	return ""
}

// extractClaudeFlatEvent handles Claude Code's native flat event format.
func extractClaudeFlatEvent(raw map[string]interface{}, eventType string) string {
	switch eventType {
	case "message":
		role, _ := raw["role"].(string)
		if role != "assistant" {
			return ""
		}
		return extractContentArray(raw)

	case "tool_use":
		name, _ := raw["name"].(string)
		if name != "" {
			return fmt.Sprintf("\n[Using tool: %s]\n", name)
		}
		return ""

	case "tool_result", "result", "init", "system":
		return "" // skip metadata
	}

	return ""
}

/* --------------------------------------------------------------------------
   Codex parser
   -------------------------------------------------------------------------- */

// extractCodexDisplayText extracts display text from a Codex JSONL event.
func extractCodexDisplayText(raw map[string]interface{}) string {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "item.completed":
		return extractCodexItemCompleted(raw)

	case "turn.failed":
		if errMsg, _ := raw["error"].(string); errMsg != "" {
			return fmt.Sprintf("\n[Error: %s]\n", errMsg)
		}
		return "\n[Turn failed]\n"

	case "error":
		if errMsg, _ := raw["message"].(string); errMsg != "" {
			return fmt.Sprintf("\n[Error: %s]\n", errMsg)
		}
		return ""

	case "thread.started", "turn.started", "turn.completed",
		"item.started", "thread.completed":
		return "" // lifecycle events, skip
	}

	return ""
}

// extractCodexItemCompleted extracts text from a Codex item.completed event.
func extractCodexItemCompleted(raw map[string]interface{}) string {
	item, ok := raw["item"].(map[string]interface{})
	if !ok {
		return ""
	}
	itemType, _ := item["type"].(string)

	switch itemType {
	case "agent_message":
		text, _ := item["text"].(string)
		return text
	case "command_execution":
		command, _ := item["command"].(string)
		if command != "" {
			return fmt.Sprintf("\n[Executing: %s]\n", truncateString(command, 120))
		}
	case "file_change":
		path, _ := item["path"].(string)
		action, _ := item["action"].(string)
		if path != "" {
			if action != "" {
				return fmt.Sprintf("\n[File %s: %s]\n", action, path)
			}
			return fmt.Sprintf("\n[File change: %s]\n", path)
		}
	}

	return ""
}

/* --------------------------------------------------------------------------
   Gemini CLI parser
   -------------------------------------------------------------------------- */

// extractGeminiDisplayText extracts display text from a Gemini stream-json event.
func extractGeminiDisplayText(raw map[string]interface{}) string {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "message":
		role, _ := raw["role"].(string)
		if role != "assistant" && role != "model" {
			return ""
		}
		// Try content array first, then direct string
		if text := extractContentArray(raw); text != "" {
			return text
		}
		if content, ok := raw["content"].(string); ok {
			return content
		}
		return ""

	case "tool_use":
		name, _ := raw["tool_name"].(string)
		if name == "" {
			name, _ = raw["name"].(string)
		}
		if name != "" {
			return fmt.Sprintf("\n[Using tool: %s]\n", name)
		}
		return ""

	case "error":
		if errMsg, _ := raw["message"].(string); errMsg != "" {
			return fmt.Sprintf("\n[Error: %s]\n", errMsg)
		}
		return ""

	case "tool_result", "result", "init":
		return "" // skip metadata
	}

	return ""
}

/* --------------------------------------------------------------------------
   Shared helpers
   -------------------------------------------------------------------------- */

// extractContentArray extracts text from a "content" array field, used by both
// Claude and Gemini flat events. The content array contains items like
// {"type": "text", "text": "..."}.
func extractContentArray(raw map[string]interface{}) string {
	content, ok := raw["content"].([]interface{})
	if !ok {
		return ""
	}

	var parts []string
	for _, item := range content {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		itemType, _ := itemMap["type"].(string)
		if itemType == "text" {
			text, _ := itemMap["text"].(string)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "")
}
