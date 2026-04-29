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

// ─── attachCmdContext (used by both rejection and failure paths) ────────────

func TestAttachCmdContext_PopulatesEmptyFields(t *testing.T) {
	cmd := commandMsg{
		Command: "claude",
		Args:    []string{"implement the login page"},
		Cwd:     "/Users/daniel/aiexpedite/ai-service",
	}
	res := resultMsg{} // nothing populated

	attachCmdContext(&res, cmd)

	if res.Command != "claude" {
		t.Errorf("Command = %q, want %q", res.Command, "claude")
	}
	if len(res.Args) != 1 || res.Args[0] != "implement the login page" {
		t.Errorf("Args = %v, want [implement the login page]", res.Args)
	}
	if res.Cwd != "/Users/daniel/aiexpedite/ai-service" {
		t.Errorf("Cwd = %q, want the requested cwd", res.Cwd)
	}
}

func TestAttachCmdContext_DoesNotClobberPrePopulatedFields(t *testing.T) {
	// Authoritative field already set on the result (e.g. session_ended
	// passes the running session's Cwd, not the requested one) — must
	// not be overwritten.
	cmd := commandMsg{
		Command: "claude",
		Args:    []string{"alpha"},
		Cwd:     "/requested/path",
	}
	res := resultMsg{
		Command: "preset-cmd",
		Args:    []string{"preset-arg"},
		Cwd:     "/already/set",
	}

	attachCmdContext(&res, cmd)

	if res.Command != "preset-cmd" {
		t.Errorf("Command was clobbered: %q", res.Command)
	}
	if len(res.Args) != 1 || res.Args[0] != "preset-arg" {
		t.Errorf("Args were clobbered: %v", res.Args)
	}
	if res.Cwd != "/already/set" {
		t.Errorf("Cwd was clobbered: %q", res.Cwd)
	}
}

func TestAttachCmdContext_RedactsSecretsBeforeAttaching(t *testing.T) {
	// Secrets in args must be scrubbed BEFORE the message hits Pub/Sub
	// — same redaction guarantee makeRejectionResult provides for the
	// rejection path, now extended to the failure path.
	cmd := commandMsg{
		Command: "curl",
		Args: []string{
			"-H",
			"Authorization: Bearer sk-live-fakekey-1234567890abcdef",
		},
	}
	res := resultMsg{}

	attachCmdContext(&res, cmd)

	joined := strings.Join(res.Args, " ")
	if strings.Contains(joined, "sk-live-fakekey-1234567890abcdef") {
		t.Errorf("redaction failed — token leaked into Args: %q", joined)
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker, got %q", joined)
	}
}

func TestAttachCmdContext_HandlesEmptyCmdGracefully(t *testing.T) {
	// e.g. cmd was malformed at the boundary — attachCmdContext should
	// not panic or invent values.
	cmd := commandMsg{}
	res := resultMsg{}

	attachCmdContext(&res, cmd)

	if res.Command != "" || len(res.Args) != 0 || res.Cwd != "" {
		t.Errorf("attached non-empty values from empty cmd: %+v", res)
	}
}

func TestAttachCmdContext_LeavesArgsAloneWhenCmdHasNone(t *testing.T) {
	// Don't overwrite an existing non-empty Args slice with a nil/empty
	// one from cmd. Symmetric with the pre-populated test above.
	cmd := commandMsg{
		Command: "ls",
		// Args nil
	}
	res := resultMsg{
		Args: []string{"-la"}, // pretend an upstream caller already set this
	}

	attachCmdContext(&res, cmd)

	if len(res.Args) != 1 || res.Args[0] != "-la" {
		t.Errorf("Args clobbered when cmd had none: %v", res.Args)
	}
}
