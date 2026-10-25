package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		// The sent hint leaves an armed follow-up. Quiesce the propagator
		// inside the capture so no later timer callback logs through
		// os.Stdout while this helper is restoring it.
		stopCLIUsagePropagator()
		resetCLIUsagePropagator()
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

// The direct-run reader reads store records that carry prompt text, tool
// output, file paths and an auth.json-shaped token beside the counts. The ledger,
// the device log and the published usage carry none of them — and, with no debt
// owed, not even the raw session id.
func TestOpenCodeUsageRedaction_TheDirectReaderKeepsNothingButCounts(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 1202)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	const canaryPath = "/home/canary/CANARY-PROMPT-project"
	extra := fmt.Sprintf(`,"path":{"cwd":%q,"root":%q},"system":[%q],"auth":{"type":"api","key":%q},"account":%q`,
		canaryPath, canaryPath, openCodeRedactionPrompt, openCodeRedactionAPIKey, openCodeRedactionAccount)
	store.write(openCodeTestMessage{session: openCodeRedactionSession, id: "msg_canary", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second),
		steps: [][3]int64{{500, 20, 0}}, cost: 0.25, extra: extra})
	partDir := filepath.Join(store.root, "storage", "part", "msg_canary")
	store.writeFile(filepath.Join(partDir, "prt_text.json"), fmt.Sprintf(`{"id":"prt_text","type":"text","text":%q}`, openCodeRedactionPrompt), clock.ms(-time.Hour))
	store.writeFile(filepath.Join(partDir, "prt_tool.json"), fmt.Sprintf(`{"id":"prt_tool","type":"tool","state":{"input":{"filePath":%q},"output":%q}}`, canaryPath, openCodeRedactionAPIKey), clock.ms(-time.Hour))
	store.writeFile(filepath.Join(store.root, "auth.json"), fmt.Sprintf(`{"anthropic":{"type":"api","key":%q}}`, openCodeRedactionAPIKey), clock.ms(-time.Hour))

	logs := captureStdout(t, func() { scanOpenCodeDirect(t) })
	assertNoOpenCodeCanary(t, "the device log", logs, false)

	raw, err := os.ReadFile(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNoOpenCodeCanary(t, "the ledger file", string(raw), false)
	if strings.Contains(string(raw), canaryPath) || strings.Contains(logs, canaryPath) {
		t.Fatal("a store path leaked")
	}

	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, clock.now)
	published, _ := json.Marshal(usage)
	assertNoOpenCodeCanary(t, "the published cliAgentUsage", string(published), false)
	if strings.Contains(string(published), canaryPath) {
		t.Fatal("the published usage carries a store path")
	}
	if len(usage.Metrics) == 0 || openCodeMetricByLabel(t, usage.Metrics, "Tokens today").Consumed == nil {
		t.Fatal("the reader published no numbers; the redaction proof would be vacuous")
	}
}

// OpenCode 2 rows carry prompt text, tool output, paths and a token inside
// session_message.data. The adapter's read, the ledger, the device log and the
// published usage carry counts, hashed keys and fixed labels only — whether the
// row is read by a scan or pays an owed debt.
func TestOpenCodeUsageRedaction_V2RowsLeaveNothingButCounts(t *testing.T) {
	sched, store, clock, _ := openCodeV2Fixture(t, 1203)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	const canaryPath = "/home/canary/CANARY-PROMPT-project"
	secretRow := func(session, id string, createdMs, completedMs int64) {
		store.row(session, id, "assistant", createdMs, completedMs, fmt.Sprintf(
			`{"text":%q,"path":{"cwd":%q},"tool":{"input":{"filePath":%q},"output":%q},"auth":{"key":%q},"account":%q,"cost":0.25,"tokens":{"input":500,"output":20,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":%d,"completed":%d}}`,
			openCodeRedactionPrompt, canaryPath, canaryPath, openCodeRedactionAPIKey, openCodeRedactionAPIKey, openCodeRedactionAccount, createdMs, completedMs))
		store.row(session, id+"_shell", "shell", completedMs, completedMs, fmt.Sprintf(`{"command":"cat %s","output":%q}`, canaryPath, openCodeRedactionAPIKey))
		store.idle(session, id+"_idle", completedMs)
	}
	// A direct run, read by a scan.
	store.row(openCodeRedactionSession, "msg_user", "user", clock.ms(-time.Hour), clock.ms(-time.Hour), fmt.Sprintf(`{"text":%q}`, openCodeRedactionPrompt))
	secretRow(openCodeRedactionSession, "msg_canary", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second))

	read, err := readOpenCodeDirectSQLite(context.Background(), openCodeStorageDir(), 0, openCodeDirectTestLimits())
	if err != nil || len(read.Messages) != 1 {
		t.Fatalf("read = %+v, %v", read, err)
	}
	dumped, _ := json.Marshal(read)
	assertNoOpenCodeCanary(t, "the adapter's read", string(dumped), true)
	if strings.Contains(string(dumped), canaryPath) {
		t.Fatal("the adapter's read carries a store path")
	}

	// A managed run whose stream carried no usage, paid from the store.
	run := armOpenCodeUsageRun("opencode", "", "")
	captureOpenCodeUsageLine(run, `{"type":"session.created","sessionID":"ses_managed"}`)
	openCodeUsageInFlight.Wait()
	clock.now = clock.now.Add(time.Second)
	secretRow("ses_managed", "msg_managed", clock.ms(-500*time.Millisecond), clock.ms(0))
	settleOpenCodeUsageRun(run, "")

	logs := captureStdout(t, func() {
		scanOpenCodeDirect(t)
		for sched.fireNext() {
		}
	})
	assertNoOpenCodeCanary(t, "the device log", logs, false)
	if strings.Contains(logs, canaryPath) || strings.Contains(logs, "ses_managed") {
		t.Fatal("the device log carries a path or a session id")
	}
	raw, err := os.ReadFile(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNoOpenCodeCanary(t, "the ledger file", string(raw), false)
	if strings.Contains(string(raw), canaryPath) {
		t.Fatal("the ledger carries a store path")
	}
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, clock.now)
	published, _ := json.Marshal(usage)
	assertNoOpenCodeCanary(t, "the published cliAgentUsage", string(published), false)
	if tokens := openCodeMetricByLabel(t, usage.Metrics, "Tokens today"); tokens.Consumed == nil || *tokens.Consumed != 1040 {
		t.Fatalf("tokens row = %+v, want both rows' 520; the redaction proof would be vacuous", tokens)
	}
}

// An OpenCode 1.19 store keeps credentials (account, control_account,
// credential, session_share) beside the session title and directory and the
// message / part data the V1 family is read from. A scan, an owed debt's
// payment and a Refresh leave counts, hashed keys and fixed labels only; a
// session id appears only inside a debt, as everywhere else.
func TestOpenCodeUsageRedaction_A119StoreLeavesNothingButCounts(t *testing.T) {
	sched, store, clock, _ := openCode119Fixture(t, 1204)
	stubOpenCodeReadiness(t, "anthropic/claude-sonnet-4-5\n", true)
	const canaryPath = "/home/canary/CANARY-PROMPT-project"
	const canaryToken = "oc_at_REDACTIONCANARY0access"
	store.exec(`INSERT INTO account VALUES ('acc_1', ?, 'https://opencode.example', ?, ?, 0, 1, 1)`, openCodeRedactionAccount, canaryToken, openCodeRedactionAPIKey)
	store.exec(`INSERT INTO control_account VALUES (?, 'https://opencode.example', ?, ?, 0, 1, 1, 1)`, openCodeRedactionAccount, canaryToken, openCodeRedactionAPIKey)
	store.exec(`INSERT INTO credential (id, label, value, time_created, time_updated) VALUES ('cred_1', 'anthropic', ?, 1, 1)`, openCodeRedactionAPIKey)
	store.exec(`INSERT INTO session_share VALUES (?, 'shr_1', ?, 'https://opncd.example/s', 1, 1)`, openCodeRedactionSession, canaryToken)
	secretTurn := func(session, id string, createdMs, completedMs int64) {
		store.v1Assistant(session, id, createdMs, completedMs, [][3]int64{{500, 20, 0}}, 0.25)
		store.exec(`UPDATE session SET title = ?, directory = ? WHERE id = ?`, openCodeRedactionPrompt, canaryPath, session)
		store.exec(`UPDATE message SET data = json_set(data, '$.path.cwd', ?, '$.system', ?, '$.account', ?) WHERE id = ?`, canaryPath, openCodeRedactionPrompt, openCodeRedactionAccount, id)
		store.exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`, id+"_tool", id, session, createdMs, completedMs,
			fmt.Sprintf(`{"type":"tool","state":{"input":{"filePath":%q},"output":%q}}`, canaryPath, openCodeRedactionAPIKey))
	}
	// A direct run, read by a scan.
	secretTurn(openCodeRedactionSession, "msg_canary", clock.ms(-time.Hour), clock.ms(-time.Hour+time.Second))
	// A managed run whose stream carried no usage: one debt paid by the ladder,
	// one by a Refresh.
	owe := func(session, id string) {
		run := armOpenCodeUsageRun("opencode", "", "")
		captureOpenCodeUsageLine(run, fmt.Sprintf(`{"type":"session.created","sessionID":%q}`, session))
		openCodeUsageInFlight.Wait()
		clock.now = clock.now.Add(time.Second)
		secretTurn(session, id, clock.ms(-500*time.Millisecond), clock.ms(0))
		settleOpenCodeUsageRun(run, "")
	}

	logs := captureStdout(t, func() {
		scanOpenCodeDirect(t)
		owe("ses_managed", "msg_managed")
		for sched.fireNext() {
		}
		owe("ses_clicked", "msg_clicked")
		openCodeDirectScanOnRefresh(context.Background())
	})
	assertNoOpenCodeCanary(t, "the device log", logs, false)
	for _, leak := range []string{canaryPath, canaryToken, "ses_managed", "ses_clicked"} {
		if strings.Contains(logs, leak) {
			t.Fatalf("the device log carries %q", leak)
		}
	}
	raw, err := os.ReadFile(openCodeUsageCachePath())
	if err != nil {
		t.Fatal(err)
	}
	assertNoOpenCodeCanary(t, "the ledger file", string(raw), false)
	if strings.Contains(string(raw), canaryPath) || strings.Contains(string(raw), canaryToken) {
		t.Fatal("the ledger carries a store path or token")
	}
	usage, _ := openCodeUsageParser{}.ParseContext(context.Background(), "", detectedCLIAgent{Path: "opencode"}, clock.now)
	published, _ := json.Marshal(usage)
	assertNoOpenCodeCanary(t, "the published cliAgentUsage", string(published), false)
	if strings.Contains(string(published), canaryPath) || strings.Contains(string(published), canaryToken) {
		t.Fatal("the published usage carries a store path or token")
	}
	if tokens := openCodeMetricByLabel(t, usage.Metrics, "Tokens today"); tokens.Consumed == nil || *tokens.Consumed != 1560 {
		t.Fatalf("tokens row = %+v, want three turns' 520; the redaction proof would be vacuous", tokens)
	}
}
