//go:build codexlive

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Certification run for live follow-ups with a real Codex CLI (L1/L2 spec
// §5.1). Codex is added to liveFollowUpCertified only after this passes on the
// codex version the device ships against, and that version becomes the
// broker row's minCliVersion. Run by hand on a computer with a signed-in
// `codex`, never against the owner's running agent or its sessions:
//
//	CODEX_LIVE_CERT=1 go test -tags codexlive -run TestCodexLiveFollowUpCertification -v -timeout 15m .
//
// CODEX_LIVE_CERT_APPROVAL=1 adds step 8 (a steer beside an open approval).
// It is never part of `go test ./...`.
//
// The run starts its OWN `codex app-server` through the device's manager, in
// a fresh empty folder, on an EPHEMERAL thread (no rollout is written), with
// notify, the MCP servers and the side-effecting features turned off. Never
// leave node_repl enabled: it would connect to the desktop app's pipe. It
// drives the real CLI through the device's own handler (handleSessionCommand)
// with commands signed the way terminal-service signs them.

var codexCertOverrides = []string{
	"-c", "notify=[]",
	"-c", "mcp_servers.node_repl.enabled=false",
	"-c", "mcp_servers.openaiDeveloperDocs.enabled=false",
	"--disable", "apps", "--disable", "browser_use", "--disable", "browser_use_external",
	"--disable", "in_app_browser", "--disable", "computer_use", "--disable", "plugins",
	"--disable", "hooks", "--disable", "goals", "--disable", "plugin_discovery_prefetch",
	"--disable", "remote_plugin", "--disable", "memories",
}

const (
	codexCertCountPrompt = "Count slowly from 1 to 30, one number per line. Do not use any tools."
	codexCertSteerText   = "When you reach 30, also write the word BANANA on its own line."
)

func TestCodexLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("CODEX_LIVE_CERT") != "1" {
		t.Skip("set CODEX_LIVE_CERT=1 to run the live certification")
	}
	executable := resolveExecutable("codex")
	if _, err := exec.LookPath(executable); err != nil {
		t.Skip("no codex CLI on PATH")
	}
	version := semverRe.FindString(cachedProbeVersion(executable))
	if version == "" {
		t.Fatalf("cannot read the codex version from %s", executable)
	}
	t.Logf("certifying codex %s (%s)", version, executable)
	// The test seam: certify the installed version for this run only.
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID: liveFollowUpCertified[claudeCodeCliAgentID],
		codexCliAgentID:      version,
	})
	ledger, ledgerPath, acks := installLiveCertLedger(t)

	const sid = "cert-codex-1"
	m := NewCodexAppServerManager(nil)
	prevManager := globalCodexAppServerManager
	globalCodexAppServerManager = m
	t.Cleanup(func() { globalCodexAppServerManager = prevManager })

	client := &liveCertClient{t: t, send: func(frame string) error { return m.Send(sid, frame) }}
	// approvalPolicy never asks for nothing; anything that still arrives is
	// declined, except during step 8, which holds it open on purpose.
	var holdServerRequests atomic.Bool
	client.onFrame = func(f jsonRPCFrame, _ string) {
		if !f.isRequest() || holdServerRequests.Load() {
			return
		}
		reply := `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"error":{"code":-32601,"message":"certification does not handle ` + f.Method + `"}}`
		if strings.Contains(f.Method, "pproval") {
			reply = `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"decision":"decline"}}`
		}
		go func() { _ = client.send(reply) }()
	}
	cwd := t.TempDir()
	if err := m.Start(sid, cwd, codexCertOverrides, "ws-cert", "uid-cert", client.publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(sid) })
	session := m.Get(sid)
	if session.liveFollowUps == nil {
		t.Fatalf("a session launched from codex %s has no live follow-up tracker (launchedVersion %q)", version, session.launchedVersion)
	}
	deliver := func(cmd commandMsg) resultMsg {
		t.Helper()
		seen := len(acks.snapshot())
		if !handleSessionCommand(context.Background(), nil, cmd, &Config{AgentID: "agent-cert"}) {
			t.Fatal("the follow-up was handed back")
		}
		return lastAck(t, acks, seen)
	}
	want := func(step string, ack resultMsg, outcome, reason, processedIn string) {
		t.Helper()
		if ack.FollowUpOutcome != outcome || ack.FollowUpReason != reason || ack.FollowUpProcessedIn != processedIn {
			t.Fatalf("%s: ack {%s %s %s}, want {%s %s %s}", step, ack.FollowUpOutcome, ack.FollowUpReason, ack.FollowUpProcessedIn, outcome, reason, processedIn)
		}
	}
	userMessagesWithClientID := func(clientID string) int {
		return client.count(0, func(f jsonRPCFrame, line string) bool {
			return f.Method == "item/completed" && containsAll(line, `"type":"userMessage"`, `"clientId":"`+clientID+`"`)
		})
	}

	// 1. initialize, an ephemeral read-only thread, the counting turn.
	client.request(`1`, "initialize", map[string]any{"clientInfo": map[string]any{"name": "aix-live-follow-up-cert", "title": "live follow-up certification", "version": Version}}, 60*time.Second)
	client.notify("initialized")
	r := client.request(`2`, "thread/start", map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": true}, 60*time.Second)
	var started struct {
		Thread struct {
			ID        string `json:"id"`
			Ephemeral bool   `json:"ephemeral"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(r.Result, &started); err != nil || started.Thread.ID == "" || !started.Thread.Ephemeral {
		t.Fatalf("thread/start did not give an ephemeral thread: %s", r.Result)
	}
	threadID := started.Thread.ID
	turnID := codexCertStartTurn(t, client, `3`, threadID, codexCertCountPrompt)

	// 2. On the first agent message delta, a relay_follow_up built as
	// terminal-service builds it.
	client.waitFor("the first agentMessage delta", 0, 120*time.Second, func(f jsonRPCFrame, _ string) bool { return f.Method == "item/agentMessage/delta" })
	cmd := liveCertCommand(t, ledger, sid, "cert-1", codexCertSteerText, threadID, turnID)
	// 3. written/current, durable across a restart of the ledger.
	want("live attempt", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	ledger = reopenFollowUpLedger(t, ledger, ledgerPath)
	globalRelayFollowUpLedger = ledger
	if row := ledger.Query("cert-1", ledger.LedgerID(), time.Now().UnixMilli()); row.State != followUpRowWritten {
		t.Fatalf("the written attempt is not durable: %+v", row)
	}
	// 4. The same command again is answered from the ledger.
	want("redelivery", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	// 6. (while the turn still runs) a stale expectedTurnId.
	stale := liveCertCommand(t, ledger, sid, "cert-2", "Also write the word STALE.", threadID, "turn-does-not-exist")
	want("stale turn", deliver(stale), followUpOutcomeNotWritten, followUpReasonTurnMismatch, "")

	// 5. The same turn completes, with the marker, and the steer reached
	// Codex exactly once.
	done := client.waitFor("turn/completed", 0, 5*time.Minute, func(f jsonRPCFrame, _ string) bool {
		return f.Method == codexMethodTurnCompleted && paramString(f, "turn", "id") == turnID
	})
	if n := client.count(0, isResponseTo(`"relay-followup-cert-1"`)); n != 1 {
		t.Fatalf("Codex answered %d turn/steer requests for one attempt", n)
	}
	if n := userMessagesWithClientID("fu-cert-1"); n != 1 {
		t.Fatalf("%d userMessage items carry the follow-up id (want 1: handled once)", n)
	}
	if n := userMessagesWithClientID("fu-cert-2"); n != 0 {
		t.Fatalf("the stale-turn attempt was applied")
	}
	if !strings.Contains(codexCertFinalAgentMessage(t, client.snapshot()[done]), "BANANA") {
		t.Fatalf("the turn's final agent message does not carry the steered marker: %s", client.snapshot()[done])
	}

	// 7. After turn/completed: no active turn, nothing applied.
	late := liveCertCommand(t, ledger, sid, "cert-3", "Also write the word LATE.", threadID, turnID)
	want("after the turn", deliver(late), followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	time.Sleep(2 * time.Second)
	if n := userMessagesWithClientID("fu-cert-3"); n != 0 {
		t.Fatalf("a steer after the turn was applied")
	}

	// 8. Optional: beside an open approval the device writes nothing (C13).
	if os.Getenv("CODEX_LIVE_CERT_APPROVAL") == "1" {
		holdServerRequests.Store(true)
		r := client.request(`4`, "thread/start", map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "untrusted", "ephemeral": true}, 60*time.Second)
		if err := json.Unmarshal(r.Result, &started); err != nil || started.Thread.ID == "" {
			t.Fatalf("thread/start (untrusted): %s", r.Result)
		}
		mark := len(client.snapshot())
		approvalTurn := codexCertStartTurn(t, client, `5`, started.Thread.ID, "Run the shell command `echo certification` and tell me what it printed.")
		i := client.waitFor("an approval request", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
			return f.isRequest() && strings.Contains(f.Method, "pproval")
		})
		req, _ := parseJSONRPCFrame(client.snapshot()[i])
		if !session.liveFollowUps.approvalPending() {
			t.Fatal("the device does not see the open approval")
		}
		beside := liveCertCommand(t, ledger, sid, "cert-4", "Also write the word BESIDE.", started.Thread.ID, approvalTurn)
		want("beside an approval", deliver(beside), followUpOutcomeNotWritten, followUpReasonApprovalPending, "")
		if n := client.count(0, isResponseTo(`"relay-followup-cert-4"`)); n != 0 {
			t.Fatal("a steer was written beside an open approval")
		}
		holdServerRequests.Store(false)
		if err := client.send(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"decision":"decline"}}`); err != nil {
			t.Fatal(err)
		}
		client.waitFor("the approval turn to complete", mark, 5*time.Minute, func(f jsonRPCFrame, _ string) bool {
			return f.Method == codexMethodTurnCompleted && paramString(f, "turn", "id") == approvalTurn
		})
	}
	t.Logf("codex %s: handled once, in the running turn, never beside an open approval — read the transcript, then certify %s", version, version)
}

func codexCertStartTurn(t *testing.T, client *liveCertClient, id, threadID, text string) string {
	t.Helper()
	r := client.request(id, "turn/start", map[string]any{"threadId": threadID, "input": []map[string]any{{"type": "text", "text": text}}}, 60*time.Second)
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(r.Result, &turn); err != nil || turn.Turn.ID == "" {
		t.Fatalf("turn/start: %s", r.Result)
	}
	return turn.Turn.ID
}

// codexCertFinalAgentMessage is the last agentMessage a turn/completed frame
// reports (C11: only the final one is in its items).
func codexCertFinalAgentMessage(t *testing.T, line string) string {
	t.Helper()
	var frame struct {
		Params struct {
			Turn struct {
				Items []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"items"`
			} `json:"turn"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, item := range frame.Params.Turn.Items {
		if item.Type == "agentMessage" {
			text = item.Text
		}
	}
	return text
}
