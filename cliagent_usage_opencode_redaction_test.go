package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_redaction_test.go — what OpenCode usage capture
   writes, logs and publishes is counts, hashed step keys and the account
   fingerprint. A frame carrying a key, an account and prompt text leaves none
   of them anywhere; the session id lives only inside a debt in the 0600 ledger.
   ------------------------------------------------------------------------ */

const (
	openCodeRedactionAPIKey  = "sk-ant-api03-REDACTIONCANARY0000000000000000"
	openCodeRedactionSession = "ses_REDACTIONCANARYSESSION"
	openCodeRedactionAccount = "canary.person@example.com"
	openCodeRedactionPrompt  = "summarise the CANARY-PROMPT quarterly numbers"
)

// openCodeRedactionFrames are the stream of a turn whose frames carry every
// secret the capture must never pass on.
func openCodeRedactionFrames(now time.Time) []string {
	return []string{
		fmt.Sprintf(`{"type":"session.created","sessionID":%q,"account":%q,"apiKey":%q}`, openCodeRedactionSession, openCodeRedactionAccount, openCodeRedactionAPIKey),
		fmt.Sprintf(`{"type":"text","sessionID":%q,"part":{"type":"text","text":%q}}`, openCodeRedactionSession, openCodeRedactionPrompt),
		fmt.Sprintf(`{"type":"step_finish","sessionID":%q,"part":{"id":"prt_canary","sessionID":%q,"messageID":"msg_canary","type":"step-finish","reason":"stop","cost":0.25,"tokens":{"input":500,"output":20,"reasoning":0,"cache":{"read":0,"write":0}},"metadata":{"key":%q,"user":%q,"prompt":%q},"time":{"end":%d}}}`,
			openCodeRedactionSession, openCodeRedactionSession, openCodeRedactionAPIKey, openCodeRedactionAccount, openCodeRedactionPrompt, now.UnixMilli()),
	}
}

func assertNoOpenCodeCanary(t *testing.T, where, text string, allowSession bool) {
	t.Helper()
	for _, secret := range []string{openCodeRedactionAPIKey, openCodeRedactionAccount, openCodeRedactionPrompt, "CANARY-PROMPT", "REDACTIONCANARY0"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s carries %q:\n%s", where, secret, text)
		}
	}
	if !allowSession && strings.Contains(text, openCodeRedactionSession) {
		t.Fatalf("%s carries the session id:\n%s", where, text)
	}
}

func TestOpenCodeUsageRedaction_NothingButCountsLeavesTheCapture(t *testing.T) {
	sched := openCodeUsageFixture(t, 1201)
	stubOpenCodeExecutable(t)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	stubOpenCodeExport(t, func(context.Context, string) ([]byte, bool) {
		return []byte(fmt.Sprintf(`{"info":{"title":%q},"messages":[]}`, openCodeRedactionPrompt)), true
	})
	rec, cfg := propagatorFixture(t)
	now := time.Now()
	// The startup gather names the install's providers; runs key to them.
	openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, now)

	logs := captureStdout(t, func() {
		// A run that captured usage.
		run := armOpenCodeUsageRunForExecutable("opencode", "")
		for _, line := range openCodeRedactionFrames(now) {
			captureOpenCodeUsageLine(run, line)
		}
		openCodeUsageInFlight.Wait()
		settleOpenCodeUsageRun(run, "")

		// A run that owes an export (its debt holds the session id).
		owed := armOpenCodeUsageRunForExecutable("opencode", "")
		captureOpenCodeUsageLine(owed, openCodeRedactionFrames(now)[0])
		openCodeUsageInFlight.Wait()
		settleOpenCodeUsageRun(owed, "")
		sched.fireNext()

		startCLIUsagePropagator(cfg)
		waitHints(t, rec, 1, 0)
	})
	assertNoOpenCodeCanary(t, "the device log", logs, false)

	// The ledger: the session id appears ONLY inside a debt.
	raw, err := os.ReadFile(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNoOpenCodeCanary(t, "the ledger file", string(raw), true)
	ledger := loadOpenCodeUsageLedger()
	for _, d := range ledger.Debts {
		d.SessionID = ""
		b, _ := json.Marshal(d)
		assertNoOpenCodeCanary(t, "a debt outside its session id", string(b), false)
	}
	without, _ := json.Marshal(struct {
		Buckets   []openCodeUsageBucket
		SeenSteps []string
	}{ledger.Buckets, ledger.SeenSteps})
	assertNoOpenCodeCanary(t, "the buckets and step keys", string(without), false)
	if info, err := os.Stat(openCodeUsageCachePath()); err == nil && info.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Fatalf("ledger mode = %v, want owner-only", info.Mode().Perm())
	}

	// The published usage.
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, now)
	published, _ := json.Marshal(usage)
	assertNoOpenCodeCanary(t, "the published cliAgentUsage", string(published), false)
	if len(usage.Metrics) == 0 {
		t.Fatal("the capture published no numbers; the redaction proof would be vacuous")
	}

	// The signed hint.
	for _, h := range rec.all() {
		body, _ := json.Marshal(h.hint)
		assertNoOpenCodeCanary(t, "the usage hint", string(body)+h.url, false)
	}
}
