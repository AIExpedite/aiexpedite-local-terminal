package main

import (
	"strings"
	"testing"
)

// codexFramesTestMarker stands in for the probe's per-run nonce.
const codexFramesTestMarker = "AIEXPEDITE_CODEX_SMOKE_OK_0badc0de"

// The four dialects of the marker parity contract, plus the frames that
// surround assistant text in a real turn. Each row is one line; the reader must
// recover the marker from every assistant shape and nothing from the rest.
//
// The post-update rows follow the event-NAME vocabulary the freshness
// classifiers already accept (method / type / msg / payload / params) with the
// text at `text` or `message` — the keys codex_frames.go documents as the one
// place to change if a captured device transcript disagrees.
func TestCodexAssistantMessageFrame_ReadsEveryDialect(t *testing.T) {
	m := codexFramesTestMarker
	for _, tc := range []struct {
		name string
		line string
		kind codexAssistantFrameKind
		text string
	}{
		// Pre-update, bare JSONL — the only shape fixtured before this change.
		{"pre-update item.completed", `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"` + m + `"}}`, codexAssistantComplete, m},
		// Post-update, nested.
		{"msg nested message", `{"id":"0","msg":{"type":"agent_message","message":"` + m + `"}}`, codexAssistantComplete, m},
		{"msg nested text", `{"id":"0","msg":{"type":"agent_message","text":"` + m + `"}}`, codexAssistantComplete, m},
		{"payload nested", `{"type":"event_msg","payload":{"type":"agent_message","message":"` + m + `"}}`, codexAssistantComplete, m},
		{"params.msg nested", `{"params":{"msg":{"type":"agent_message","message":"` + m + `"}}}`, codexAssistantComplete, m},
		// Post-update, JSON-RPC.
		{"method codex/event/agent_message", `{"jsonrpc":"2.0","method":"codex/event/agent_message","params":{"id":"1","msg":{"type":"agent_message","message":"` + m + `"}}}`, codexAssistantComplete, m},
		{"method item/completed agentMessage", `{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"type":"agentMessage","id":"i","text":"` + m + `"}}}`, codexAssistantComplete, m},
		{"method item/agentMessage/completed", `{"method":"item/agentMessage/completed","params":{"text":"` + m + `"}}`, codexAssistantComplete, m},
		// Delta-only.
		{"legacy delta", `{"id":"0","msg":{"type":"agent_message_delta","delta":"AIEXPEDITE_"}}`, codexAssistantDelta, "AIEXPEDITE_"},
		{"bare delta", `{"type":"agent_message_delta","delta":"CODEX_"}`, codexAssistantDelta, "CODEX_"},
		{"method codex/event delta", `{"method":"codex/event/agent_message_delta","params":{"msg":{"type":"agent_message_delta","delta":"SMOKE"}}}`, codexAssistantDelta, "SMOKE"},
		{"method item/agentMessage/delta", `{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"itemId":"i","delta":"_OK_"}}`, codexAssistantDelta, "_OK_"},
		// Everything else in a turn carries no assistant text.
		{"reasoning item", `{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`, codexAssistantNone, ""},
		{"command_execution item", `{"type":"item.completed","item":{"type":"command_execution","command":"ls"}}`, codexAssistantNone, ""},
		{"agent_message item.started", `{"type":"item.started","item":{"type":"agent_message","text":"early"}}`, codexAssistantNone, ""},
		{"reasoning delta", `{"msg":{"type":"agent_reasoning_delta","delta":"hmm"}}`, codexAssistantNone, ""},
		{"rate limits", `{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":5}}}}`, codexAssistantNone, ""},
		{"turn.completed", `{"type":"turn.completed","usage":{"input_tokens":1}}`, codexAssistantNone, ""},
		{"empty agent_message", `{"type":"item.completed","item":{"type":"agent_message","text":""}}`, codexAssistantNone, ""},
		{"unparseable", `{"type":"item.completed","item":`, codexAssistantNone, ""},
		{"non-JSON banner", `Codex v9.9.9 — update available`, codexAssistantNone, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, text := codexAssistantLine(tc.line)
			if kind != tc.kind || text != tc.text {
				t.Fatalf("codexAssistantLine = (%d, %q), want (%d, %q)", kind, text, tc.kind, tc.text)
			}
			if got := isCodexAssistantDelta(tc.line); got != (tc.kind == codexAssistantDelta) {
				t.Fatalf("isCodexAssistantDelta = %v, want %v", got, tc.kind == codexAssistantDelta)
			}
		})
	}
}

func TestCodexFoldAssistantText(t *testing.T) {
	m := codexFramesTestMarker
	noise := []string{
		`Codex v9.9.9 banner`,
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{}}}`,
		`{not json`,
	}
	deltas := []string{
		`{"method":"item/agentMessage/delta","params":{"delta":"AIEXPEDITE_CODEX_"}}`,
		`{"method":"item/agentMessage/delta","params":{"delta":"SMOKE_OK_"}}`,
		`{"method":"item/agentMessage/delta","params":{"delta":"0badc0de"}}`,
	}
	complete := `{"method":"codex/event/agent_message","params":{"msg":{"type":"agent_message","message":"` + m + `"}}}`
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"pre-update complete", append(append([]string{}, noise...),
			`{"type":"item.completed","item":{"type":"agent_message","text":"`+m+`"}}`, `{"type":"turn.completed"}`), m},
		{"post-update complete", append(append([]string{}, noise...), complete), m},
		// No separator is inserted at a frame boundary.
		{"delta only", append(append([]string{}, noise...), deltas...), m},
		// Deltas then the complete message that repeats them: the marker ONCE.
		{"delta then complete", append(append(append([]string{}, deltas...), noise...), complete), m},
		// The last complete message wins.
		{"last complete wins", []string{
			`{"type":"item.completed","item":{"type":"agent_message","text":"Working on it"}}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":"` + m + `"}}`,
		}, m},
		{"no assistant text", noise, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := []byte(strings.Join(tc.lines, "\n") + "\n")
			if got := codexFoldAssistantText(stdout); got != tc.want {
				t.Fatalf("codexFoldAssistantText = %q, want %q", got, tc.want)
			}
		})
	}
}
