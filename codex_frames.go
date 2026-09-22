// codex_frames.go — the ONE reader of "what did Codex say".
//
// Codex has emitted its assistant text in several dialects across releases,
// and a post-update build can switch between them without warning:
//
//   - Pre-update, bare JSONL (`codex exec --json`):
//     {"type":"item.completed","item":{"type":"agent_message","text":"…"}}
//   - Nested: the event under `msg` / `payload` / `params.msg`, e.g.
//     {"msg":{"type":"agent_message","message":"…"}}
//   - JSON-RPC: the event name on `method` — `codex/event/agent_message`,
//     `item/completed` with an agentMessage item, `item/agentMessage/*`.
//   - Delta-only: `agent_message_delta` / `item/agentMessage/delta` chunks
//     with no terminal complete item.
//
// The terminal session path (extractCodexDisplayText + readOutputStream) and
// the maintenance smoke probe (cliagent_smoke_codex.go) both read through
// here, so the two paths cannot drift into separate dialect tables. The event
// NAME vocabulary is the freshness classifier's (codexBareEventName /
// codexNormalizeCompletionName in cliagent_usage_codex_freshness.go); this
// file adds only which names carry assistant text and under which key.
//
// The text keys are `text` then `message` (and `delta` for a delta frame) at
// the location the event name was found. Only `item.text` is evidenced by a
// captured transcript in this repo; if a captured post-update transcript
// disagrees, this is the one place to change.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// codexAssistantFrameKind classifies one Codex frame for assistant text.
type codexAssistantFrameKind int

const (
	codexAssistantNone codexAssistantFrameKind = iota
	// A complete assistant message: replaces whatever came before it.
	codexAssistantComplete
	// An incremental chunk of the message still being written.
	codexAssistantDelta
)

// codexAssistantMessageFrame reports whether a parsed Codex frame carries
// assistant text, and that text. It must run BEFORE any top-level `type`
// switch: a JSON-RPC frame has no top-level `type` at all. A frame whose text
// is empty reports codexAssistantNone — an empty message says nothing and
// must not replace a real one.
func codexAssistantMessageFrame(raw map[string]interface{}) (codexAssistantFrameKind, string) {
	for _, node := range codexAssistantEventNodes(raw) {
		kind, text := codexAssistantNodeText(node)
		if kind != codexAssistantNone && text != "" {
			return kind, text
		}
	}
	return codexAssistantNone, ""
}

// codexAssistantLine is codexAssistantMessageFrame for a raw stdout line.
func codexAssistantLine(line string) (codexAssistantFrameKind, string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return codexAssistantNone, ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return codexAssistantNone, ""
	}
	return codexAssistantMessageFrame(raw)
}

// isCodexAssistantDelta reports whether line is an incremental assistant text
// frame. readOutputStream buffers these on the side (no separator between
// chunks) instead of batching them, the same discipline
// isAntigravityAgentResponseDelta gives Antigravity.
func isCodexAssistantDelta(line string) bool {
	kind, _ := codexAssistantLine(line)
	return kind == codexAssistantDelta
}

// codexFoldAssistantText folds a whole Codex stdout into the assistant's
// answer: the LAST complete message wins; deltas are concatenated with NO
// separator (a newline at a frame boundary is itself an exact-marker failure)
// and used only when no complete message arrived, so a stream that deltas and
// then repeats itself yields the text once. Non-JSON lines (a banner, an
// updater notice) and unparseable lines are skipped.
func codexFoldAssistantText(stdout []byte) string {
	var complete string
	var deltas strings.Builder
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		switch kind, text := codexAssistantLine(string(line)); kind {
		case codexAssistantComplete:
			complete = text
		case codexAssistantDelta:
			deltas.WriteString(text)
		}
	}
	if complete != "" {
		return complete
	}
	return deltas.String()
}

// codexEventNode is one place a frame may name an event: the name, and the
// object whose keys carry that event's payload.
type codexEventNode struct {
	name string
	body map[string]interface{}
}

// codexAssistantEventNodes lists the (event name, payload) pairs of a frame in
// the envelope shapes codexFrameEventNames reads: the JSON-RPC method (payload
// in `params`, or `params.msg` for the legacy `codex/event/*` notifications),
// a bare `type`, and one nested under `msg` / `payload` / `params`.
func codexAssistantEventNodes(raw map[string]interface{}) []codexEventNode {
	var nodes []codexEventNode
	add := func(name interface{}, body map[string]interface{}) {
		if n, _ := name.(string); n != "" && body != nil {
			nodes = append(nodes, codexEventNode{name: n, body: body})
		}
	}
	params, _ := raw["params"].(map[string]interface{})
	add(raw["method"], params)
	if params != nil {
		msg, _ := params["msg"].(map[string]interface{})
		add(raw["method"], msg)
	}
	add(raw["type"], raw)
	for _, key := range []string{"msg", "payload", "params"} {
		nested, ok := raw[key].(map[string]interface{})
		if !ok {
			continue
		}
		add(nested["type"], nested)
		if msg, ok := nested["msg"].(map[string]interface{}); ok {
			add(msg["type"], msg)
		}
	}
	return nodes
}

// codexAssistantNodeText classifies one node by its event name and reads the
// text at that node.
func codexAssistantNodeText(node codexEventNode) (codexAssistantFrameKind, string) {
	name := strings.TrimPrefix(node.name, "codex/event/")
	switch codexBareEventName(name) {
	case "agent_message":
		return codexAssistantComplete, codexFirstString(node.body, "text", "message")
	case "agent_message_delta":
		return codexAssistantDelta, codexFirstString(node.body, "delta", "text", "message")
	}
	switch codexNormalizeCompletionName(name) {
	case "item.completed":
		// Only an agent-message ITEM is assistant text; command_execution and
		// file_change items are rendered by extractCodexItemCompleted.
		item, _ := node.body["item"].(map[string]interface{})
		if itemType, _ := item["type"].(string); itemType == "agent_message" || itemType == "agentMessage" {
			return codexAssistantComplete, codexFirstString(item, "text", "message")
		}
	case "agentMessage.completed":
		return codexAssistantComplete, codexFirstString(node.body, "text", "message")
	case "agentMessage.delta":
		return codexAssistantDelta, codexFirstString(node.body, "delta", "text", "message")
	}
	return codexAssistantNone, ""
}

// codexFirstString returns the first non-empty string value among keys.
func codexFirstString(body map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if s, _ := body[key].(string); s != "" {
			return s
		}
	}
	return ""
}
