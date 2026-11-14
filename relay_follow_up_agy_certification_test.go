//go:build agylive

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Certification run for Antigravity on its ACP driver (L3/L4 spec §6.4) with
// a real `agy acp` (1.5.4 or newer). Antigravity is added to
// liveFollowUpCertified, and its shared-constants row is certified, only after
// this passes on the version the device ships against. Run by hand on a
// computer with a signed-in agy, never against the owner's running agent or
// its sessions:
//
//	AGY_LIVE_CERT=1 go test -tags agylive -run TestAntigravityLiveFollowUpCertification -v -timeout 60m .
//
// It runs in a fresh folder outside every agy trustedWorkspaces entry; set
// AGY_LIVE_CERT_TRUSTED_DIR to an EMPTY folder inside one to repeat step 1
// there. AGY_LIVE_CERT_CONVERSATION names a conversation the one-shot
// `--conversation` created, for step 4.
//
// The launch is the device's own (`agy acp`, a fresh per-launch
// ANTIGRAVITY_SERVER_CSRF_TOKEN). The listeners' token check is NOT tested
// here — AI Expedite never connects to them: re-run the port check of
// aixwt/rs/_notes/design/AGY_ACP_PORT_PROBE.md on every agy version this is
// certified against (local/documents/security/ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md).

func TestAntigravityLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("AGY_LIVE_CERT") != "1" {
		t.Skip("set AGY_LIVE_CERT=1 to run the live certification")
	}
	b := readACPDriverBinary(antigravityACPProfile)
	if !acpDriverEligible(antigravityACPProfile, b) {
		t.Skipf("no agy %s or newer installed here (%+v)", antigravityACPMinVersion, b)
	}
	t.Logf("certifying agy %s (%s)", b.version, b.path)
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID:  liveFollowUpCertified[claudeCodeCliAgentID],
		antigravityCliAgentID: b.version,
	})
	ledger, _, acks := installLiveCertLedger(t)
	m := newACPManager(nil, antigravityACPProfile)
	prev := globalAntigravityACPManager
	globalAntigravityACPManager = m
	t.Cleanup(func() { globalAntigravityACPManager = prev })

	folders := []string{t.TempDir()}
	if trusted := os.Getenv("AGY_LIVE_CERT_TRUSTED_DIR"); trusted != "" {
		folders = append(folders, trusted)
	}
	for n, cwd := range folders {
		sid := "cert-agy-" + string(rune('a'+n))
		client := &liveCertClient{t: t, send: func(frame string) error { return m.Send(sid, frame) }}
		client.onFrame = func(f jsonRPCFrame, _ string) {
			if f.isRequest() && f.Method == acpMethodRequestPermission {
				go func() { _ = client.send(certPermissionAnswer(f, "reject_once")) }()
			}
		}
		if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", ACPStartOptions{}, client.publish); err != nil {
			t.Fatalf("start in %s: %v", cwd, err)
		}
		session := m.Get(sid)
		client.request(`"relay-acp-init"`, acpMethodInitialize, map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, 60*time.Second)
		r := client.request(`"relay-acp-new"`, acpMethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": []any{}}, 120*time.Second)
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(r.Result, &created); err != nil || created.SessionID == "" || session.liveFollowUps.acpSession() != created.SessionID {
			t.Fatalf("session/new: %s", r.Result)
		}
		acp := created.SessionID
		prompt := func(id, text string) {
			client.sendRequest(id, acpMethodSessionPrompt, map[string]any{"sessionId": acp, "prompt": []map[string]any{{"type": "text", "text": text}}})
		}
		isPermission := func(f jsonRPCFrame, _ string) bool { return f.isRequest() && f.Method == acpMethodRequestPermission }

		// 1. Approvals in default mode: a file write and a non-trivial command
		// each ask. If they do not, Antigravity's approval model stays `none`.
		for _, step := range []struct{ name, text string }{
			{"write", "Create a file named cert.txt containing the word CERT."},
			{"command", "Run the shell command `git --version` and tell me its output."},
		} {
			mark := len(client.snapshot())
			pid := `"relay-turn-cert-` + step.name + `"`
			prompt(pid, step.text)
			end := client.waitFor(step.name+" answer", mark, 5*time.Minute, isResponseTo(pid))
			if at := client.find(mark, isPermission); at < 0 || at > end {
				t.Errorf("%s (%s): agy did not ask before it — its approval model stays none", step.name, cwd)
			}
		}
		if _, err := os.Stat(filepath.Join(cwd, "cert.txt")); err == nil {
			t.Errorf("a denied write landed in %s", cwd)
		}
		if n > 0 {
			_ = m.End(sid)
			continue
		}

		deliver := func(cmd commandMsg) resultMsg {
			t.Helper()
			seen := len(acks.snapshot())
			if !handleSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-cert"}) {
				t.Fatal("the follow-up was handed back")
			}
			return lastAck(t, acks, seen)
		}
		// 2. A mid-turn second prompt takes the turn over: the earlier prompt
		// answers with no usage, annotated, and the final answer has usage.
		mark := len(client.snapshot())
		const p2 = `"relay-turn-cert-2"`
		prompt(p2, "Count slowly from 1 to 30, one number per line. Do not use any tools.")
		client.waitFor("a chunk", mark, 2*time.Minute, func(f jsonRPCFrame, line string) bool {
			return f.Method == "session/update" && strings.Contains(line, "agent_message_chunk")
		})
		ack := deliver(liveCertCommand(t, ledger, sid, "cert-a2", "Also end your reply with the word STEERED.", acp, ""))
		if ack.FollowUpOutcome != followUpOutcomeWritten || ack.FollowUpProcessedIn != followUpProcessedInCurrent {
			t.Fatalf("handover attempt: %+v", ack)
		}
		early := client.waitFor("the handed-over answer", mark, time.Minute, isResponseTo(p2))
		if acpHandoverAnswer(client.snapshot()[early]) != true {
			t.Errorf("the earlier prompt's answer carried usage: %s", client.snapshot()[early])
		}
		final := client.waitFor("the follow-up's answer", mark, 5*time.Minute, isResponseTo(`"relay-turn-fu_cert-a2"`))
		if !strings.Contains(client.snapshot()[final], `"usage"`) {
			t.Errorf("the final answer carried no usage: %s", client.snapshot()[final])
		}

		// 3. A prompt right after the turn ended runs as the next turn.
		mark = len(client.snapshot())
		const p3 = `"relay-turn-cert-3"`
		prompt(p3, "Reply with just the word ONE.")
		client.waitFor("the short turn's answer", mark, 2*time.Minute, isResponseTo(p3))
		ack = deliver(liveCertCommand(t, ledger, sid, "cert-a3", "Reply with just TWO.", acp, ""))
		if ack.FollowUpOutcome != followUpOutcomeNotWritten || ack.FollowUpReason != followUpReasonNoActiveTurn {
			t.Fatalf("an attempt after the turn ended: %+v", ack)
		}

		// 4. Resume of a conversation the one-shot driver created.
		if conv := os.Getenv("AGY_LIVE_CERT_CONVERSATION"); conv != "" {
			r := client.request(`"relay-acp-resume"`, acpMethodSessionResume, map[string]any{"sessionId": conv, "cwd": cwd, "mcpServers": []any{}}, 2*time.Minute)
			t.Logf("resume of %s: %s", conv, r.Result)
		}
		// 5. The missing-session error reads as gone.
		mark = len(client.snapshot())
		client.sendRequest(`"relay-acp-load"`, acpMethodSessionLoad, map[string]any{"sessionId": "00000000-0000-0000-0000-000000000000", "cwd": cwd, "mcpServers": []any{}})
		at := client.waitFor("the missing session's answer", mark, time.Minute, isResponseTo(`"relay-acp-load"`))
		t.Logf("missing-session answer (must match \\bsession not found\\b): %s", client.snapshot()[at])
		_ = m.End(sid)
	}
	t.Logf("agy %s: read the transcript, re-run the port check, then certify %s", b.version, b.version)
}
