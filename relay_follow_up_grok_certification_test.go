//go:build groklive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Certification run for live follow-ups with a real Grok Build CLI (L1/L2
// spec §5.2). Grok is added to liveFollowUpCertified only after this passes on
// the grok version the device ships against, and that version becomes the
// broker row's minCliVersion. Run by hand on a computer with a signed-in
// `grok`, never against the owner's running agent or its sessions:
//
//	GROK_LIVE_CERT=1 go test -tags groklive -run TestGrokLiveFollowUpCertification -v -timeout 30m .
//
// GROK_LIVE_CERT_STRANDED_N sets how many stranded-window rounds step 6 runs
// (default 10). It is never part of `go test ./...`.
//
// The run starts its OWN `grok agent --model grok-build stdio` through the
// device's manager, in a fresh empty folder. GROK_HOME points at a temporary
// copy of the login only, so the device's per-session isolation reads from
// and writes back to that copy: the owner's own Grok home (its sessions, its
// billing log) is never written. Every permission request is answered
// reject_once.

const (
	grokCertCountPrompt = "Count slowly from 1 to 30, one number per line. Do not use any tools."
	grokCertSteerText   = "When you reach 30, also write the word BANANA on its own line."
	grokCertShortPrompt = "Count from 1 to 5, one number per line. Do not use any tools."
)

func TestGrokLiveFollowUpCertification(t *testing.T) {
	if os.Getenv("GROK_LIVE_CERT") != "1" {
		t.Skip("set GROK_LIVE_CERT=1 to run the live certification")
	}
	executable := resolveExecutable("grok")
	if _, err := exec.LookPath(executable); err != nil {
		t.Skip("no grok CLI on PATH or in its installer directory")
	}
	version := semverRe.FindString(cachedProbeVersion(executable))
	if version == "" {
		t.Fatalf("cannot read the grok version from %s", executable)
	}
	t.Logf("certifying grok %s (%s)", version, executable)
	// The test binary moves HOME into a sandbox at startup (test_environment_test.go),
	// so grokPersistentHome() would look for the login there. Read it from the
	// person's real Grok home instead: GROK_HOME when they set one, else the home
	// directory as it was before the sandbox. It is only read, never written.
	loginHome := os.Getenv("GROK_HOME")
	if loginHome == "" {
		loginHome = filepath.Join(realHomeAtStartup, ".grok")
	}
	auth, err := os.ReadFile(filepath.Join(loginHome, "auth.json"))
	if err != nil {
		t.Skipf("grok is not signed in here: %v", err)
	}
	certHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(certHome, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_HOME", certHome)
	// The test seam: certify the installed version for this run only.
	pinLiveFollowUpKinds(t, map[string]string{
		claudeCodeCliAgentID: liveFollowUpCertified[claudeCodeCliAgentID],
		grokCliAgentID:       version,
	})
	ledger, _, acks := installLiveCertLedger(t)

	const sid = "cert-grok-1"
	m := NewGrokACPManager(nil)
	prevManager := globalGrokACPManager
	globalGrokACPManager = m
	t.Cleanup(func() { globalGrokACPManager = prevManager })

	client := &liveCertClient{t: t, send: func(frame string) error { return m.Send(sid, frame) }}
	client.onFrame = func(f jsonRPCFrame, _ string) {
		if !f.isRequest() {
			return
		}
		reply := `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`
		if f.Method == acpMethodRequestPermission {
			reply = `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"outcome":{"outcome":"cancelled"}}}`
			var p struct {
				Options []struct {
					OptionID string `json:"optionId"`
					Kind     string `json:"kind"`
				} `json:"options"`
			}
			if json.Unmarshal(f.Params, &p) == nil {
				for _, o := range p.Options {
					if o.Kind == "reject_once" {
						reply = `{"jsonrpc":"2.0","id":` + string(f.ID) + `,"result":{"outcome":{"outcome":"selected","optionId":` + strconv.Quote(o.OptionID) + `}}}`
						break
					}
				}
			}
		}
		go func() { _ = client.send(reply) }()
	}
	cwd := t.TempDir()
	if err := m.Start(sid, cwd, nil, "ws-cert", "uid-cert", GrokStartOptions{}, client.publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(sid) })
	session := m.Get(sid)
	if session.liveFollowUps == nil {
		t.Fatalf("a session launched from grok %s has no live follow-up tracker (launchedVersion %q)", version, session.launchedVersion)
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
	broadcasts := func(attemptID string) int {
		return client.count(0, func(f jsonRPCFrame, _ string) bool {
			return f.Method == grokMethodInterjection && paramString(f, "interjectionId") == attemptID
		})
	}
	isChunk := func(f jsonRPCFrame) bool {
		return f.Method == "session/update" && paramString(f, "update", "sessionUpdate") == "agent_message_chunk"
	}
	isFallbackStart := func(text string) func(f jsonRPCFrame, _ string) bool {
		return func(f jsonRPCFrame, _ string) bool {
			return f.Method == grokMethodQueueChanged && strings.HasPrefix(paramString(f, "runningPromptId"), grokInterjectFallbackPromptPrefix) &&
				(text == "" || paramString(f, "runningText") == text)
		}
	}

	// 1. initialize, authenticate with the cached login, a session, a prompt.
	client.request(`1`, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{
		"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, 60*time.Second)
	client.request(`2`, "authenticate", map[string]any{"methodId": "cached_token"}, 60*time.Second)
	r := client.request(`3`, acpMethodSessionNew, map[string]any{"cwd": cwd, "mcpServers": []any{}}, 120*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(r.Result, &created); err != nil || created.SessionID == "" || session.liveFollowUps.acpSession() != created.SessionID {
		t.Fatalf("session/new: %s (tracked %q)", r.Result, session.liveFollowUps.acpSession())
	}
	acp := created.SessionID
	const promptID = `"relay-turn-cert-1"`
	client.sendRequest(promptID, acpMethodSessionPrompt, map[string]any{"sessionId": acp, "prompt": []map[string]any{{"type": "text", "text": grokCertCountPrompt}}})

	// 2. On the first agent message chunk: written/current, one interject on
	// stdin (one answer to it), one broadcast.
	client.waitFor("the first agent_message_chunk", 0, 120*time.Second, func(f jsonRPCFrame, _ string) bool { return isChunk(f) })
	cmd := liveCertCommand(t, ledger, sid, "cert-g1", grokCertSteerText, acp, "")
	want("live attempt", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	client.waitFor("the interjection broadcast", 0, 10*time.Second, func(f jsonRPCFrame, _ string) bool {
		return f.Method == grokMethodInterjection && paramString(f, "interjectionId") == "cert-g1"
	})
	// 3. Redelivered: answered from the ledger, nothing more reaches Grok.
	want("redelivery", deliver(cmd), followUpOutcomeWritten, "", followUpProcessedInCurrent)
	time.Sleep(time.Second)
	if a, b := client.count(0, isResponseTo(`"relay-followup-cert-g1"`)), broadcasts("cert-g1"); a != 1 || b != 1 {
		t.Fatalf("one attempt reached Grok %d times (%d broadcasts)", a, b)
	}

	// 4. The marker streams before the prompt's answer, which comes once.
	answer := client.waitFor("the prompt's answer", 0, 5*time.Minute, isResponseTo(promptID))
	marker := -1
	frames := client.snapshot()
	for i := 0; i < answer; i++ {
		if f, ok := parseJSONRPCFrame(frames[i]); ok && isChunk(f) && strings.Contains(frames[i], "BANANA") {
			marker = i
		}
	}
	if marker < 0 {
		t.Fatal("the interjected marker never streamed inside the prompt's turn")
	}
	time.Sleep(2 * time.Second)
	if n := client.count(0, isResponseTo(promptID)); n != 1 {
		t.Fatalf("the prompt's answer was published %d times", n)
	}

	// 5. Idle: refused noActiveTurn, nothing written, no fallback turn starts.
	mark := len(client.snapshot())
	idle := liveCertCommand(t, ledger, sid, "cert-g2", "Reply with just the word IDLE.", acp, "")
	want("idle session", deliver(idle), followUpOutcomeNotWritten, followUpReasonNoActiveTurn, "")
	time.Sleep(3 * time.Second)
	if i := client.find(mark, isFallbackStart("")); i >= 0 || broadcasts("cert-g2") != 0 {
		t.Fatal("an idle attempt reached Grok and started a fallback turn")
	}

	// 6. The stranded window, best effort: write an interjection straight to
	// stdin at offsets around the running prompt's last response_completed,
	// as the device would have admitted it just before the turn ended.
	rounds := 10
	if n, err := strconv.Atoi(os.Getenv("GROK_LIVE_CERT_STRANDED_N")); err == nil && n > 0 {
		rounds = n
	}
	offsets := []time.Duration{0, 5, 10, 20, 40, 80, 160, 320, 640, 1280}
	var drained, held, afterAnswer, skipped int
	strandedBefore := grokStrandedAfterAnswer.Load()
	for k := 0; k < rounds; k++ {
		offset := offsets[k%len(offsets)] * time.Millisecond
		mark := len(client.snapshot())
		pid := fmt.Sprintf(`"relay-turn-cert-s%d"`, k)
		client.sendRequest(pid, acpMethodSessionPrompt, map[string]any{"sessionId": acp, "prompt": []map[string]any{{"type": "text", "text": grokCertShortPrompt}}})
		client.waitFor("response_completed", mark, 3*time.Minute, func(f jsonRPCFrame, _ string) bool {
			return f.Method == grokMethodSessionNotification && paramString(f, "update", "sessionUpdate") == "response_completed"
		})
		time.Sleep(offset)
		attempt := fmt.Sprintf("cert-s%d", k)
		text := fmt.Sprintf("Also write the word STRAND%d on its own line.", k)
		if !recordCertInterjection(session.liveFollowUps, pid, attempt, text) {
			skipped++
			t.Logf("round %d (+%s): the answer was already read; the device would not have written", k, offset)
			client.waitFor("the round's answer", mark, 3*time.Minute, isResponseTo(pid))
			continue
		}
		frame, _ := grokInterjectFrame(`cert-strand-`+strconv.Itoa(k), acp, text, attempt)
		if err := client.send(frame); err != nil {
			t.Fatal(err)
		}
		answerAt := client.waitFor("the round's answer", mark, 5*time.Minute, isResponseTo(pid))
		// Give a fallback that would start after the answer time to show.
		time.Sleep(5 * time.Second)
		start := client.find(mark, isFallbackStart(text))
		switch {
		case start < 0:
			drained++
			t.Logf("round %d (+%s): drained inside its turn", k, offset)
			continue
		case start < answerAt:
			fallbackID := paramString(mustParseFrame(t, client.snapshot()[start]), "runningPromptId")
			done := client.waitFor("the fallback's turn_completed", start, 5*time.Minute, func(f jsonRPCFrame, _ string) bool {
				return f.Method == grokMethodSessionNotification && paramString(f, "update", "prompt_id") == fallbackID
			})
			if done < answerAt {
				held++
				t.Logf("round %d (+%s): stranded; the answer was held until its fallback completed", k, offset)
				continue
			}
		}
		afterAnswer++
		t.Logf("round %d (+%s): stranded; the fallback ran AFTER its turn's answer", k, offset)
		client.waitFor("the fallback to finish", start, 5*time.Minute, func(f jsonRPCFrame, _ string) bool {
			return f.Method == grokMethodSessionNotification && paramString(f, "update", "sessionUpdate") == grokSessionUpdateTurnCompleted &&
				strings.HasPrefix(paramString(f, "update", "prompt_id"), grokInterjectFallbackPromptPrefix)
		})
	}
	t.Logf("stranded window over %d rounds: drained %d, held %d, after the answer %d, skipped %d (grok_stranded_after_answer +%d)",
		rounds, drained, held, afterAnswer, skipped, grokStrandedAfterAnswer.Load()-strandedBefore)
	if afterAnswer > 0 {
		t.Errorf("a fallback ran after its turn's answer: implement the bounded wait of §2.8 before certifying")
	}
	t.Logf("grok %s: handled once, in the running turn, never beside an open approval — read the transcript, then certify %s", version, version)
}

func mustParseFrame(t *testing.T, line string) jsonRPCFrame {
	t.Helper()
	f, ok := parseJSONRPCFrame(line)
	if !ok {
		t.Fatalf("not a JSON-RPC frame: %s", line)
	}
	return f
}
