// Tests for makeRejectionResult — the helper that builds the resultMsg
// echoed back when a command is refused (allowlist deny, stale, rate-limit,
// unauthorized).
//
// Regression context: until v0.9.3, the rejection paths in pubsub.go built
// resultMsg literals that omitted Command/Args entirely and used
// getTrackedCwd() (which is empty for never-started sessions). Every record
// in workspace/{id}/rejected_terminal_commands ended up as
//
//	{ command: null, args: [], cwd: null, rejectionReason: "ALLOWLIST_DENIED" }
//
// — diagnostically useless: 108 prod records, all unactionable.
//
// These tests pin the new contract:
//   - Command and Args are populated from cmd, passed through
//     redactSensitiveData/redactArgs so secrets don't leak.
//   - Cwd reflects the requested cwd from the inbound command, not
//     getTrackedCwd().
//   - Session metadata (Type/SessionID) is set when the command was a
//     session_*-routed call, omitted otherwise.
package main

import (
	"strings"
	"testing"
)

func TestMakeRejectionResult_PopulatesCommandAndArgs(t *testing.T) {
	cmd := commandMsg{
		ID:          "cmd-1",
		Command:     "git",
		Args:        []string{"status"},
		Cwd:         "/Users/daniel/aiexpedite/ai-service",
		WorkspaceID: "ws-1",
		UID:         "user-1",
		Ts:          1700000000000,
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Command != "git" {
		t.Errorf("Command = %q, want %q", res.Command, "git")
	}
	if len(res.Args) != 1 || res.Args[0] != "status" {
		t.Errorf("Args = %v, want [status]", res.Args)
	}
	if res.Cwd != cmd.Cwd {
		t.Errorf("Cwd = %q, want %q (the requested cwd, not the tracked cwd)", res.Cwd, cmd.Cwd)
	}
	if res.Status != "denied" || res.RejectionReason != "ALLOWLIST_DENIED" {
		t.Errorf("status/reason mismatch: %q / %q", res.Status, res.RejectionReason)
	}
	if res.Output != "Denied" {
		t.Errorf("Output = %q, want %q", res.Output, "Denied")
	}
	if res.AgentID != "agent-1" {
		t.Errorf("AgentID = %q, want %q", res.AgentID, "agent-1")
	}
	if res.WorkspaceID != "ws-1" || res.UID != "user-1" || res.ID != "cmd-1" {
		t.Errorf("identity fields mismatch: ws=%q uid=%q id=%q", res.WorkspaceID, res.UID, res.ID)
	}
	if res.Ts == 0 {
		t.Error("Ts should be set to current millis, got 0")
	}
	if res.Version == "" {
		t.Error("Version should be set to the build Version constant")
	}
	// Non-session commands do not carry session metadata.
	if res.Type != "" || res.SessionID != "" {
		t.Errorf("non-session rejection should not set Type/SessionID, got %q / %q", res.Type, res.SessionID)
	}
}

func TestMakeRejectionResult_RedactsSecretsInArgs(t *testing.T) {
	// LLM-generated commands occasionally embed bearer tokens or
	// credentials in args. These must not end up in the
	// rejected_terminal_commands collection.
	cmd := commandMsg{
		ID:      "cmd-2",
		Command: "curl",
		Args: []string{
			"-H",
			"Authorization: Bearer sk-live-fakekey-1234567890abcdef",
			"https://api.example.com",
		},
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	joined := strings.Join(res.Args, " ")
	if strings.Contains(joined, "sk-live-fakekey-1234567890abcdef") {
		t.Errorf("redaction failed — token leaked into Args: %q", joined)
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker in Args, got %q", joined)
	}
}

func TestMakeRejectionResult_RedactsSecretsInCommand(t *testing.T) {
	// Highly unusual but possible: secret embedded directly in the command
	// string (e.g. when the LLM joins everything into one token). Uses
	// the GitHub PAT pattern from initSensitivePatterns — `gh[pousr]_`
	// followed by ≥36 alphanumeric characters.
	const fakeToken = "ghp_AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555"
	cmd := commandMsg{
		ID:      "cmd-3",
		Command: "git config remote.origin.url https://x:" + fakeToken + "@github.com/foo/bar.git",
		Args:    nil,
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if strings.Contains(res.Command, fakeToken) {
		t.Errorf("redaction failed — GitHub token leaked into Command: %q", res.Command)
	}
}

func TestMakeRejectionResult_SessionRoutedSetsTypeAndSessionID(t *testing.T) {
	// session_start denials need Type+SessionID so the backend can
	// correlate the rejection with the terminalSession document the
	// frontend is listening to.
	cmd := commandMsg{
		ID:        "cmd-4",
		Command:   "claude",
		Args:      []string{"implement the login page"},
		Type:      "session_start",
		SessionID: "sess-abc",
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Type != "session_error" {
		t.Errorf("Type = %q, want %q", res.Type, "session_error")
	}
	if res.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-abc")
	}
}

func TestMakeRejectionResult_CodexAppServerStartUsesCodexErrorType(t *testing.T) {
	// codex_appserver_start rejections must come back labeled
	// codex_appserver_error so the orchestrator's IDE protocol handler can
	// route them — labelling them session_error (the catch-all used until
	// this fix) forced the orchestrator to special-case SessionID prefixes
	// to distinguish a session_start denial from a codex_appserver_start
	// denial.
	cmd := commandMsg{
		ID:        "cmd-codex-1",
		Command:   "codex",
		Args:      []string{"-c", `model="gpt-5.4"`},
		Type:      "codex_appserver_start",
		SessionID: "sess-codex-abc",
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Type != "codex_appserver_error" {
		t.Errorf("Type = %q, want %q", res.Type, "codex_appserver_error")
	}
	if res.SessionID != "sess-codex-abc" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-codex-abc")
	}
}

func TestMakeRejectionResult_CodexAppServerSendUsesCodexErrorType(t *testing.T) {
	// Stale/rate-limited/signature-failed codex_appserver_send rejections
	// route through the same makeRejectionResult path. They must also come
	// back labeled codex_appserver_error so the orchestrator's IDE handler
	// fails the in-flight JSON-RPC call instead of treating it as a generic
	// session error.
	cmd := commandMsg{
		ID:        "cmd-codex-2",
		Command:   "codex",
		Type:      "codex_appserver_send",
		SessionID: "sess-codex-xyz",
		Input:     `{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	}
	res := makeRejectionResult(cmd, "agent-1", "rate_limited", "RATE_LIMITED", "Rate limit exceeded")

	if res.Type != "codex_appserver_error" {
		t.Errorf("Type = %q, want %q", res.Type, "codex_appserver_error")
	}
}

func TestMakeRejectionResult_GrokACPStartUsesGrokErrorType(t *testing.T) {
	// grok_acp_start rejections must come back labeled grok_acp_error so the
	// orchestrator's ACP protocol handler can route them on Type alone,
	// independent of the codex IDE handler.
	cmd := commandMsg{
		ID:        "cmd-grok-1",
		Command:   "grok",
		Args:      []string{"--model", "grok-2-fast"},
		Type:      "grok_acp_start",
		SessionID: "sess-grok-abc",
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Type != "grok_acp_error" {
		t.Errorf("Type = %q, want %q", res.Type, "grok_acp_error")
	}
	if res.SessionID != "sess-grok-abc" {
		t.Errorf("SessionID = %q, want %q", res.SessionID, "sess-grok-abc")
	}
}

func TestMakeRejectionResult_GrokACPSendUsesGrokErrorType(t *testing.T) {
	// Stale/rate-limited grok_acp_send rejections route through the same path
	// and must come back labeled grok_acp_error so the orchestrator fails the
	// in-flight ACP call rather than treating it as a generic session error.
	cmd := commandMsg{
		ID:        "cmd-grok-2",
		Command:   "grok",
		Type:      "grok_acp_send",
		SessionID: "sess-grok-xyz",
		Input:     `{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
	}
	res := makeRejectionResult(cmd, "agent-1", "rate_limited", "RATE_LIMITED", "Rate limit exceeded")

	if res.Type != "grok_acp_error" {
		t.Errorf("Type = %q, want %q", res.Type, "grok_acp_error")
	}
}

func TestMakeRejectionResult_NoSessionMetadataForExecuteWithoutSessionID(t *testing.T) {
	// Type set but SessionID missing — historically a defensive case in
	// the inline literals. The helper requires BOTH to be present, since
	// a SessionID-less session_error is meaningless to the backend.
	cmd := commandMsg{
		ID:        "cmd-5",
		Command:   "ls",
		Type:      "execute",
		SessionID: "",
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Type != "" {
		t.Errorf("Type = %q, want empty (execute commands without SessionID)", res.Type)
	}
	if res.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", res.SessionID)
	}
}

func TestMakeRejectionResult_EmptyCmdCwdProducesEmptyResultCwd(t *testing.T) {
	// If the inbound command didn't request a cwd, the rejection record
	// should faithfully reflect that — don't fall back to a tracked cwd
	// from some other session.
	cmd := commandMsg{
		ID:      "cmd-6",
		Command: "git",
		Args:    []string{"status"},
		// Cwd intentionally empty
	}
	res := makeRejectionResult(cmd, "agent-1", "denied", "ALLOWLIST_DENIED", "Denied")

	if res.Cwd != "" {
		t.Errorf("Cwd = %q, want empty (cmd.Cwd was empty)", res.Cwd)
	}
}
