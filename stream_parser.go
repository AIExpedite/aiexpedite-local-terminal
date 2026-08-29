// File: stream_parser.go
// -----------------------------------------------------------------------------
// Parses structured JSON streaming output from CLI agents
// and extracts human-readable display text. This allows the frontend
// to show clean text instead of raw JSON events.
//
// Each CLI uses a different JSON streaming format:
//   - Claude: --output-format stream-json     (Anthropic API streaming events)
//   - Codex:  --json                          (JSONL events)
//   - Grok:   --output-format streaming-json  (NDJSON: text / thought / end frames)
//   - Antigravity: --output-format stream-json (NDJSON: step_update / result)
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// isClaudeStructuredStreamLine reports whether a line came from Claude's
// --output-format stream-json output (a JSON object with a `type` field) as
// opposed to a passthrough plain-text/stderr line. The claude no-output
// watchdog uses this to distinguish real assistant content from a stalled
// login banner: extractDisplayText echoes non-JSON and malformed-JSON lines
// verbatim, so a not-signed-in claude that prints a plain banner and hangs
// would otherwise disarm the fail-fast on passthrough text.
func isClaudeStructuredStreamLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	_, ok := raw["type"].(string)
	return ok
}

// isClaudeRateLimitEventLine reports whether a line is a Claude
// `rate_limit_event` structured stream frame — allowed heartbeat or
// hard rejection. Used by the no-output watchdog: extractDisplayText
// treats rate_limit_event as metadata, but the frame itself proves
// Claude's stream reached us (Claude is signed in and emitting per-window
// telemetry), so a slow first turn — say, a large repo scan — that only
// produces rate_limit_event heartbeats within the 120s budget must NOT
// be killed with a misleading /login error. captureClaudeRateLimitLine
// only surfaces rejections, so it can't be used to detect the allowed-
// heartbeat case on its own.
func isClaudeRateLimitEventLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	if !strings.Contains(trimmed, "rate_limit_event") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	t, _ := raw["type"].(string)
	return t == "rate_limit_event"
}

// isClaudeTerminalResultLine reports whether a line is Claude's terminal
// `result` frame — the one it emits when a turn is complete, carrying the final
// text and the run's summary. Used to fire the bounded utilization probe at the
// exact moment a run finished, which is when the account's real percentages have
// just moved and the card most needs a fresh numeric reading.
func isClaudeTerminalResultLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	// Cheap prefilter before the decode: this runs on every stdout line of every
	// Claude session.
	if !strings.Contains(trimmed, `"result"`) {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	t, _ := raw["type"].(string)
	return t == "result"
}

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
	case strings.HasPrefix(base, "grok"):
		return extractGrokDisplayText(raw, line)
	case isAntigravityCommand(command):
		return extractAntigravityDisplayText(raw)
	default:
		return line // passthrough unknown CLI agents
	}
}

// isAntigravityAgentResponseDelta reports whether line is an incremental
// assistant text frame. readOutputStream uses this to concatenate adjacent
// deltas without inserting the newline used between ordinary output lines.
func isAntigravityAgentResponseDelta(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return false
	}
	if eventType, _ := raw["event"].(string); eventType != "step_update" {
		return false
	}
	update, _ := raw["step_update"].(map[string]interface{})
	if update == nil {
		return false
	}
	stepType, _ := update["step_type"].(string)
	return stepType == "agent_response"
}

func isAntigravitySuccessResult(line string) bool {
	var raw struct {
		Event  string `json:"event"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &raw); err != nil {
		return false
	}
	return raw.Event == "result" && strings.EqualFold(raw.Result.Status, "SUCCESS")
}

// detectAntigravityErrorResult reports whether line is an Antigravity terminal
// result event with status: "ERROR", returning the error detail if present.
func detectAntigravityErrorResult(command, line string) (string, bool) {
	if !isAntigravityCommand(command) {
		return "", false
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return "", false
	}
	if eventType, _ := raw["event"].(string); eventType != "result" {
		return "", false
	}
	result, _ := raw["result"].(map[string]interface{})
	if result == nil {
		return "", false
	}
	status, _ := result["status"].(string)
	if !strings.EqualFold(status, "ERROR") {
		return "", false
	}
	errText, _ := result["error"].(string)
	if errText == "" {
		errText = "no error detail"
	}
	return errText, true
}

/* --------------------------------------------------------------------------
   Google Antigravity parser
   -------------------------------------------------------------------------- */

// extractAntigravityDisplayText extracts display text from an Antigravity stream-json event.
// Tool activity is surfaced as it executes during step_update events, and the authoritative
// full response is emitted on the terminal SUCCESS result event. Terminal errors are surfaced
// because they are the authoritative record of why the turn failed.
func extractAntigravityDisplayText(raw map[string]interface{}) string {
	eventType, _ := raw["event"].(string)
	switch eventType {
	case "step_update":
		update, _ := raw["step_update"].(map[string]interface{})
		if update == nil {
			return ""
		}
		stepType, _ := update["step_type"].(string)
		if stepType == "tool" {
			name, _ := update["tool_name"].(string)
			if name != "" {
				return fmt.Sprintf("\n[Using tool: %s]\n", name)
			}
		}
		return ""
	case "result":
		result, _ := raw["result"].(map[string]interface{})
		if result == nil {
			return ""
		}
		status, _ := result["status"].(string)
		if strings.EqualFold(status, "SUCCESS") {
			resp, _ := result["response"].(string)
			return resp
		}
		errText, _ := result["error"].(string)
		if errText == "" {
			errText = "no error detail"
		}
		return fmt.Sprintf("\n[Antigravity turn failed: %s]\n", errText)
	case "init":
		return ""
	}
	return ""
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

	case "result":
		// A SUCCESSFUL result is the turn recap — skip it, the deltas already
		// carried every word (same reasoning as the assistant branch above).
		//
		// An `is_error` result is different in kind: it is the ONLY record of
		// WHY the turn failed, and discarding it is what made the 2026-08-07
		// prod stall unreadable. Claude Code on a terminal computer with an
		// expired login completed turn after turn in ~1s, each one emitting an
		// is_error result this branch threw away. The device still published
		// the `turn_complete` prompt marker (session.go keys that off the
		// result event), so the session looked healthy while every chunk was
		// empty — the orchestrator saw "empty output", could not tell a broken
		// CLI from a quiet one, and backed off 54 times over 16 hours.
		//
		// Auth-class failures never reach here: detectClaudeAuthFailure runs
		// first in the read loop and `continue`s after publishing its own
		// actionable message. What surfaces here is every OTHER fatal turn
		// error — quota exhaustion, context overflow, a killed subprocess.
		if isErr, _ := raw["is_error"].(bool); isErr {
			if text := claudeResultErrorText(raw); text != "" {
				return fmt.Sprintf("\n[Claude turn failed: %s]\n", text)
			}
			// is_error with no text still beats silence: the orchestrator
			// learns the turn FAILED rather than inferring an empty answer.
			return "\n[Claude turn failed with no error detail]\n"
		}
		return ""

	case "tool_result", "init", "system", "user", "rate_limit_event":
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
   Grok parser
   -------------------------------------------------------------------------- */

// extractGrokDisplayText extracts display text from a Grok streaming-json
// event. buildGrokInteractiveArgs forces `--output-format streaming-json`, so
// without this branch the parser would fall through to `default` and the user
// would see raw `{"type":"text",...}` / `thought` / `end` frames in chat
// instead of the assistant's words.
//
// `line` is the raw JSON line and is used as a passthrough fallback when the
// frame has no `type` field at all — which is the shape of structured JSON
// output emitted by carved-out Grok subcommands (e.g. `grok sessions --json`)
// that bypass the managed `--output-format streaming-json -p` headless path.
// Every event in the streaming-json schema carries a `type` field, so
// "no type" is a reliable signal that this is subcommand output the user
// asked for rather than a streaming frame to filter.
func extractGrokDisplayText(raw map[string]interface{}, line string) string {
	if _, hasType := raw["type"]; !hasType {
		return line
	}
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "text":
		text, _ := raw["text"].(string)
		if text == "" {
			// Grok 1.0.13 renamed the streaming-json text payload from
			// `text` to `data`. Keep the legacy field first so older installed
			// versions remain compatible across a maintenance update.
			text, _ = raw["data"].(string)
		}
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

// extractContentArray extracts text from a "content" array field, used by
// Claude flat events. The content array contains items like
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
