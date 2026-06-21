// File: stream_parser.go
// -----------------------------------------------------------------------------
// Parses structured JSON streaming output from CLI agents (Claude, Codex,
// Gemini) and extracts human-readable display text. This allows the frontend
// to show clean text instead of raw JSON events.
//
// Each CLI uses a different JSON streaming format:
//   - Claude: --output-format stream-json     (Anthropic API streaming events)
//   - Codex:  --json                          (JSONL events)
//   - Gemini: -o stream-json                  (NDJSON events)
//   - Grok:   --output-format streaming-json  (NDJSON: text / thought / end frames)
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

	// Route by the same basename normalization the session router and
	// detectCLITerminalEvent use (commandBaseName in session.go) so an
	// explicit path like `/home/user/.grok/bin/grok` or `C:\tools\grok.exe`
	// — which buildInteractiveCLIArgs already shapes as the streaming-json
	// `-p` headless turn — picks the matching parser here too. Without this
	// normalisation a path-launched session would fall through to the
	// default case and publish raw NDJSON frames as chat text.
	base := commandBaseName(command)
	switch {
	case strings.HasPrefix(base, "claude"):
		return extractClaudeDisplayText(raw)
	case strings.HasPrefix(base, "codex"):
		return extractCodexDisplayText(raw)
	case strings.HasPrefix(base, "gemini"):
		return extractGeminiDisplayText(raw)
	case strings.HasPrefix(base, "grok"):
		return extractGrokDisplayText(raw)
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
	case "assistant", "message":
		// Skip the wholesale assistant-turn recap. Claude is always launched
		// with `--include-partial-messages` (see buildClaudeInteractiveArgs in
		// session.go), which causes claude to emit BOTH:
		//   1. Streaming `stream_event` envelopes with content_block_delta
		//      (text_delta + content_block_start tool_use) — incremental.
		//   2. A final flat `{type:"assistant", message:{content:[...]}}`
		//      event (and/or `{type:"message", role:"assistant"}`) — the
		//      full assembled turn as a recap.
		// We capture (1) via extractClaudeInnerEvent → those produce the
		// visible streaming chunks. Capturing (2) too would emit every word
		// twice — once from the deltas, once from the recap — which is what
		// produced the visible "Octopuses have three hearts… Octopuses have
		// three hearts…" doubling in chat cards. Skip the recap; the deltas
		// are sufficient.
		//
		// If we ever stop passing `--include-partial-messages`, we'd lose
		// streaming text and would need to fall back to extracting from this
		// branch. For now `--include-partial-messages` is always set, so the
		// recap is pure duplication.
		return ""

	case "tool_use":
		name, _ := raw["name"].(string)
		if name != "" {
			return fmt.Sprintf("\n[Using tool: %s]\n", name)
		}
		return ""

	case "tool_result", "result", "init", "system", "user", "rate_limit_event":
		return "" // skip metadata
	}

	return ""
}

// extractContentFromMessage extracts display text from a Claude assistant message
// envelope: {"type":"assistant","message":{"content":[{"type":"text","text":"..."}]}}
func extractContentFromMessage(msg map[string]interface{}) string {
	content, ok := msg["content"].([]interface{})
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range content {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			text, _ := block["text"].(string)
			if text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			name, _ := block["name"].(string)
			if name != "" {
				parts = append(parts, fmt.Sprintf("\n[Using tool: %s]\n", name))
			}
		case "thinking":
			// Skip thinking blocks in display output
		}
	}
	return strings.Join(parts, "")
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
   Grok parser
   -------------------------------------------------------------------------- */

// extractGrokDisplayText extracts display text from a Grok streaming-json
// event. buildGrokInteractiveArgs forces `--output-format streaming-json`, so
// without this branch the parser would fall through to `default` and the user
// would see raw `{"type":"text",...}` / `thought` / `end` frames in chat
// instead of the assistant's words.
func extractGrokDisplayText(raw map[string]interface{}) string {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "text":
		text, _ := raw["text"].(string)
		return text

	case "tool_use":
		name, _ := raw["name"].(string)
		if name == "" {
			name, _ = raw["tool_name"].(string)
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

	case "thought", "end", "tool_result", "result", "init", "start":
		return "" // thinking + lifecycle / metadata frames, skip
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
