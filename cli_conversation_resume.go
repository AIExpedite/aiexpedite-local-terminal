// File: cli_conversation_resume.go
// -----------------------------------------------------------------------------
// Exact conversation resume for the ONE-SHOT CLIs on the generic session path
// (Antigravity `agy`, OpenCode).
//
// On session_start these CLIs run one process per turn: the prompt goes in,
// stdin closes, the process exits and the session ends. A follow-up therefore
// needs a NEW process that reattaches to the SAME CLI conversation. This file is
// the device's two halves of that:
//
//	CAPTURE  the CLI reports its own conversation id on its JSON stream (agy's
//	         `conversation_id`, on the `init` event and every event after it;
//	         OpenCode's top-level `sessionID`). readOutputStream records the first
//	         one and publishes it on the next stream frame and on session_ended.
//	         extractDisplayText drops those events from the visible output, so
//	         without this the id never left the machine — which is why the cloud
//	         could only continue these CLIs by replaying text into a brand-new
//	         conversation.
//
//	SEED     a session_start carrying the SIGNED `conversationId` field
//	         (signaturePayload.ConversationID — signed for every command type) is
//	         a resume: the CLI's own flag is inserted into the shaped argv
//	         (`--conversation <id>` / `--session <id>`).
//
// Session control stays SERVER-OWNED. A caller-typed `--session` / `--continue`
// is still stripped by buildOpenCodeInteractiveArgs and `--continue` is still
// refused for agy; only the signed wire field reaches the flag, and the cloud
// resolves it from a prior session it has proven is the same CLI, owner, device
// and directory (terminal-service cliConversationResume.util.js).
//
// `--continue` is never used: it resumes "the most recent conversation" for the
// directory, which is another role's conversation the moment two roles share a
// checkout.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// cliConversationIDPattern mirrors terminal-service's CLI_CONVERSATION_ID_RE.
// agy mints UUIDs and OpenCode `ses_<base62>`; this is deliberately an allowlist
// of harmless characters rather than either exact shape, because the value
// becomes a process ARGUMENT — the one thing it must never do is start with `-`
// or carry whitespace or quotes.
var cliConversationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)

func isValidCliConversationID(id string) bool {
	return cliConversationIDPattern.MatchString(id)
}

// extractCliConversationID returns the CLI's own conversation id from one stdout
// line of a one-shot CLI, or "" when the line carries none (non-JSON output, a
// CLI that is not one-shot, an id that is not a safe argument).
func extractCliConversationID(command, line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return ""
	}
	var id string
	switch {
	case isAntigravityCommand(command):
		// `conversation_id` sits at the top level of `init` and inside the
		// payload object of every later event (`step_update`, `result`).
		var ev struct {
			ConversationID string `json:"conversation_id"`
			StepUpdate     struct {
				ConversationID string `json:"conversation_id"`
			} `json:"step_update"`
			Result struct {
				ConversationID string `json:"conversation_id"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
			return ""
		}
		id = ev.ConversationID
		if id == "" {
			id = ev.StepUpdate.ConversationID
		}
		if id == "" {
			id = ev.Result.ConversationID
		}
	case isOpenCodeCommand(command):
		// parseOpenCodeEventLine falls back through several id spellings, the last
		// of which (`info.id`) can be a MESSAGE id on some event types. The native
		// manager tolerates that because it re-validates against the session
		// store; here the first id wins for the whole session, so only OpenCode's
		// session-id shape (`ses_…`) is accepted.
		if _, sessionID, ok := parseOpenCodeEventLine(trimmed); ok && strings.HasPrefix(sessionID, "ses") {
			id = sessionID
		}
	default:
		return ""
	}
	if !isValidCliConversationID(id) {
		return ""
	}
	return id
}

// noteCliConversationID records the first conversation id this session's CLI
// reports. First-wins: a resumed agy run reports the id it was seeded with on
// every event, and a second, different id inside one process would be a
// sub-conversation the follow-up must not be pointed at.
func (s *CLISession) noteCliConversationID(line string) {
	id := extractCliConversationID(s.Command, line)
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.cliConversationID == "" {
		s.cliConversationID = id
	}
	s.mu.Unlock()
}

// takeUnpublishedCliConversationID returns the captured id exactly ONCE, for the
// next stream frame. The results path reads the session document when a frame
// carries an id, so stamping every frame would turn a per-session read into a
// per-chunk one; session_ended repeats it (capturedCliConversationID) so a lost
// frame still leaves the session resumable.
func (s *CLISession) takeUnpublishedCliConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliConversationID == "" || s.cliConversationPublished {
		return ""
	}
	s.cliConversationPublished = true
	return s.cliConversationID
}

func (s *CLISession) capturedCliConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cliConversationID
}

// applyCliResumeSeed inserts the CLI's exact-resume flag into an already-shaped
// one-shot argv. FAILS CLOSED: the cloud sends ONLY the follow-up text on a
// resume, so a start that cannot honour the seed must be refused rather than run
// that text in an empty conversation.
//
// callerArgs are the unshaped request args — needed because the shaped argv of a
// diagnostic invocation is the caller's argv verbatim.
func applyCliResumeSeed(command string, callerArgs, cliArgs []string, conversationID string, tty bool) ([]string, error) {
	if !isValidCliConversationID(conversationID) {
		return nil, fmt.Errorf("conversation resume refused: the conversation id is not a valid CLI argument")
	}
	if tty {
		// The PTY path re-shapes agy into its legacy argv form after this point.
		return nil, fmt.Errorf("conversation resume is not supported for tty sessions")
	}

	var flag string
	var insertAt int
	switch {
	case isAntigravityCommand(command):
		if !shouldUseAntigravityManagedStream(callerArgs) {
			return nil, fmt.Errorf("conversation resume refused: not a managed agy turn")
		}
		// buildAntigravityStreamingArgs leads with the permission flag and the two
		// format pairs; the seed goes straight after them, ahead of every caller
		// flag and the trailing tokens.
		flag, insertAt = "--conversation", 5
	case isOpenCodeCommand(command):
		if isOpenCodeDiagnosticInvocation(callerArgs) {
			return nil, fmt.Errorf("conversation resume refused: not an opencode run")
		}
		// buildOpenCodeInteractiveArgs leads with `run --format json`.
		flag, insertAt = "--session", 3
	default:
		return nil, fmt.Errorf("conversation resume is only supported for one-shot CLIs (agy, opencode); got %q", commandBaseName(command))
	}
	if len(cliArgs) < insertAt {
		return nil, fmt.Errorf("conversation resume refused: unexpected %s argument shape", commandBaseName(command))
	}
	// A caller-typed resume flag that survived shaping (agy passes
	// `--conversation` through) would now name a SECOND conversation.
	for _, a := range cliArgs {
		name, _, _ := strings.Cut(a, "=")
		if name == flag {
			return nil, fmt.Errorf("conversation resume refused: %s is already present in the arguments", flag)
		}
	}

	seeded := make([]string, 0, len(cliArgs)+2)
	seeded = append(seeded, cliArgs[:insertAt]...)
	seeded = append(seeded, flag, conversationID)
	seeded = append(seeded, cliArgs[insertAt:]...)
	return seeded, nil
}
