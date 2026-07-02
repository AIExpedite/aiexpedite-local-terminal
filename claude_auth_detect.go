package main

// claude_auth_detect.go — detect a not-signed-in / not-authorized Claude Code
// from its stream-json stdout so the session can fail fast with an actionable
// message instead of hanging until the 6h stale GC and completing empty.
//
// Why this is needed: for claude specifically the driver strips every
// env-based credential (ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN /
// CLAUDE_CODE_OAUTH_TOKEN — see sanitizeClaudeChildEnv) to force subscription
// `/login` billing. So an expired or absent `~/.claude/.credentials.json`
// login means the spawned claude launches, emits its `system/init`, and then
// cannot complete a turn. On the stream-json output that surfaces as one of:
//
//   - a `system` / `api_retry` event whose `error` category is an auth class:
//       {"type":"system","subtype":"api_retry","error":"authentication_failed"}
//   - a terminal `result` with is_error carrying the canonical login message:
//       {"type":"result","is_error":true,"result":"Invalid API key · Please run /login"}
//
// The stream parser otherwise discards `system`/`result` events as metadata
// (see extractClaudeFlatEvent), so without this the failure reached the
// orchestrator as empty output with no reason. This is the Claude analogue of
// grok's "not signed in" fail-fast, but proactive: claude tells us the reason.

import (
	"encoding/json"
	"strings"
)

// claudeAuthFailure describes an authentication/login failure Claude Code
// reported on its stream-json stdout.
type claudeAuthFailure struct {
	// Category is the machine-readable reason: the api_retry `error` value
	// ("authentication_failed" / "oauth_org_not_allowed") or "invalid_api_key"
	// synthesised from a terminal result. Surfaced in the fail-fast message.
	Category string
}

// claudeAuthErrorCategories are the `api_retry` error categories that mean
// "cannot authenticate", as opposed to transient classes (rate_limit,
// overloaded, server_error) that a retry can clear and that are handled
// elsewhere. `billing_error` is intentionally excluded — a subscription/credit
// problem is a distinct condition (surfaced by the rate-limit path), not a
// login failure the user fixes with `/login`.
var claudeAuthErrorCategories = map[string]bool{
	"authentication_failed": true,
	"oauth_org_not_allowed": true,
}

// claudeAuthResultMarkers are lowercased substrings that identify an auth
// failure inside a terminal `result` event's text. Kept deliberately narrow so
// a non-auth error result (e.g. a tool failure) never trips the fail-fast and
// kills a session that was otherwise working.
var claudeAuthResultMarkers = []string{
	"invalid api key",
	"please run /login",
	"please run `/login`",
	"not logged in",
	"not authenticated",
	"oauth_org_not_allowed",
	"authentication_error",
}

// detectClaudeAuthFailure inspects one Claude Code stream-json line and returns
// a non-nil descriptor when the line reports an authentication/login failure.
// Best-effort: returns nil on any non-JSON or unrelated line, so it is safe to
// call on every line of the hot read loop.
func detectClaudeAuthFailure(line string) *claudeAuthFailure {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return nil
	}

	switch eventType, _ := event["type"].(string); eventType {
	case "system":
		// {"type":"system","subtype":"api_retry","error":"authentication_failed",...}
		if subtype, _ := event["subtype"].(string); subtype == "api_retry" {
			if category, _ := event["error"].(string); claudeAuthErrorCategories[category] {
				return &claudeAuthFailure{Category: category}
			}
		}
	case "result":
		// {"type":"result","is_error":true,"result":"Invalid API key · Please run /login"}
		if isErr, _ := event["is_error"].(bool); isErr {
			if text := claudeResultErrorText(event); text != "" {
				lower := strings.ToLower(text)
				for _, marker := range claudeAuthResultMarkers {
					if strings.Contains(lower, marker) {
						return &claudeAuthFailure{Category: "invalid_api_key"}
					}
				}
			}
		}
	}
	return nil
}

// claudeResultErrorText pulls the human-readable error text out of a terminal
// `result` event, tolerating the field names Claude Code has used for it across
// versions (`result`, `error`, `message`).
func claudeResultErrorText(event map[string]interface{}) string {
	for _, key := range []string{"result", "error", "message"} {
		if v, ok := event[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
