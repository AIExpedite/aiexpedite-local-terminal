package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCLIUsageRefreshWantsLiveProbe(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"live-probe"}, true},
		{[]string{"LIVE-PROBE"}, false},
		{[]string{"other", "live-probe"}, false},
	}
	for _, c := range cases {
		if got := cliUsageRefreshWantsLiveProbe(commandMsg{Command: "__cli_usage_refresh__", Args: c.args}); got != c.want {
			t.Errorf("args=%v: got %v, want %v", c.args, got, c.want)
		}
	}
}

// fakeCodexAppServer answers the probe's initialize and rate-limit read the way
// Codex 0.154 does — no `jsonrpc` member, an unsolicited notification in
// between — and returns what it read from the probe.
func fakeCodexAppServer(t *testing.T, readResponse string) (io.Writer, io.Reader, <-chan []string) {
	t.Helper()
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	methods := make(chan []string, 1)
	// io.Pipe has no buffer, unlike a real stdio pipe: the server's output goes
	// through its own writer so a probe blocked writing a request can never
	// deadlock against a server blocked writing a notification.
	out := make(chan string, 16)
	go func() {
		defer fromServerW.Close()
		for line := range out {
			if _, err := io.WriteString(fromServerW, line); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(out)
		var seen []string
		scanner := bufio.NewScanner(toServerR)
		for scanner.Scan() {
			var frame struct {
				ID     *int   `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &frame) != nil {
				continue
			}
			seen = append(seen, frame.Method)
			switch {
			case frame.Method == "initialize":
				out <- `{"id":1,"result":{"userAgent":"codex"}}` + "\n"
				out <- `{"method":"remoteControl/status/changed","params":{},"emittedAtMs":1}` + "\n"
			case frame.Method == "account/rateLimits/read":
				out <- readResponse + "\n"
				methods <- seen
				return
			}
		}
		methods <- seen
	}()
	return toServerW, fromServerR, methods
}

// The shape Codex returned on 2026-09-11 for a Pro account: the main pool
// meters weekly only, and a second, named pool meters both windows.
const codexReadWeeklyMainPlusSpark = `{"id":2,"result":{` +
	`"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":91,"windowDurationMins":10080,"resetsAt":%RESET_W%},"secondary":null,"planType":"pro"},` +
	`"rateLimitsByLimitId":{` +
	`"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":91,"windowDurationMins":10080,"resetsAt":%RESET_W%},"secondary":null},` +
	`"codex_bengalfox":{"limitId":"codex_bengalfox","limitName":"GPT-5.3-Codex-Spark","primary":{"usedPercent":4,"windowDurationMins":300,"resetsAt":%RESET_S%},"secondary":{"usedPercent":1,"windowDurationMins":10080,"resetsAt":%RESET_W%}}` +
	`}}}`

func codexReadFixture(now time.Time) string {
	s := codexReadWeeklyMainPlusSpark
	s = strings.ReplaceAll(s, "%RESET_W%", strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10))
	s = strings.ReplaceAll(s, "%RESET_S%", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10))
	return s
}

func isolateCodexCache(t *testing.T) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "codex_rate_limits.json")
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", cache)
	t.Setenv("CODEX_HOME", t.TempDir())
	return cache
}

func TestCodexLiveProbeConverse_CapturesPoolsAndDropsMissingWindow(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	// A stale weekly reading from a days-old run, as AIE2's cache held.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":47,"window_minutes":10080,"resets_in_seconds":259200}}}}`,
		now.Add(-24*time.Hour),
	)

	stdin, stdout, methods := fakeCodexAppServer(t, codexReadFixture(now))
	if got := codexLiveProbeConverse(stdin, stdout, currentCodexAccountFingerprint()); got != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	if seen := <-methods; len(seen) != 3 || seen[0] != "initialize" || seen[1] != "initialized" || seen[2] != "account/rateLimits/read" {
		t.Fatalf("probe sent %v, want initialize → initialized → account/rateLimits/read", seen)
	}

	snap, ok := loadCodexRateLimitSnapshot(cache)
	if !ok {
		t.Fatal("expected cache")
	}
	if snap.FullSnapshotAtMs == 0 {
		t.Error("a live read is a full snapshot and must be recorded as one")
	}
	if snap.LimitNames["codex_bengalfox"] != "GPT-5.3-Codex-Spark" {
		t.Errorf("limit names=%v, want the Spark pool named", snap.LimitNames)
	}

	metrics := codexMetricsFromCache(time.Now(), "")
	if len(metrics) != 3 {
		t.Fatalf("metrics=%+v, want main weekly + two Spark rows", metrics)
	}
	main := metrics[0]
	if main.Kind != limitKindWeekly || main.Label != "Weekly quota" || main.Unknown || main.Consumed == nil || *main.Consumed != 91 {
		t.Errorf("main row=%+v, want Weekly quota at 91%% (the stale 47%% is superseded)", main)
	}
	if main.Model != "" {
		t.Errorf("main row model=%q, want none", main.Model)
	}
	spark5h, sparkWeekly := metrics[1], metrics[2]
	if spark5h.Kind != limitKindSession || spark5h.Label != "GPT-5.3-Codex-Spark — 5-hour session window" ||
		spark5h.Consumed == nil || *spark5h.Consumed != 4 || spark5h.Model != "gpt-5.3-codex-spark" {
		t.Errorf("spark session row=%+v", spark5h)
	}
	if sparkWeekly.Kind != limitKindWeekly || sparkWeekly.Label != "GPT-5.3-Codex-Spark — Weekly quota" ||
		sparkWeekly.Consumed == nil || *sparkWeekly.Consumed != 1 || sparkWeekly.Model != "gpt-5.3-codex-spark" {
		t.Errorf("spark weekly row=%+v", sparkWeekly)
	}
	for _, m := range metrics {
		if m.Kind == limitKindSession && m.Model == "" {
			t.Errorf("the main pool has no 5-hour window, yet a main session row was emitted: %+v", m)
		}
	}
}

func TestCodexMetrics_PoolRowsDisappearWhenPoolIsGone(t *testing.T) {
	isolateCodexCache(t)
	now := time.Now()
	stdin, stdout, _ := fakeCodexAppServer(t, codexReadFixture(now))
	if got := codexLiveProbeConverse(stdin, stdout, currentCodexAccountFingerprint()); got != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q", got)
	}
	// A later read in which the account no longer has the Spark pool, and the
	// main pool meters both windows again.
	captureCodexRateLimitLine(`{"jsonrpc":"2.0","id":2,"result":{"rateLimitsByLimitId":{"codex":{"limitName":null,`+
		`"primary":{"usedPercent":12,"windowDurationMins":300,"resetsInSeconds":3600},`+
		`"secondary":{"usedPercent":50,"windowDurationMins":10080,"resetsInSeconds":86400}}}}}`, now.Add(time.Minute))

	metrics := codexMetricsFromCache(now.Add(time.Minute), "")
	if len(metrics) != 2 {
		t.Fatalf("metrics=%+v, want the main pair only", metrics)
	}
	if metrics[0].Kind != limitKindSession || metrics[1].Kind != limitKindWeekly {
		t.Errorf("rows=%+v, want session then weekly", metrics)
	}
	for _, m := range metrics {
		if m.Model != "" {
			t.Errorf("a retired pool still labels a row: %+v", m)
		}
	}
}

func TestCodexLimitNames_SparseUpdateKeepsPoolName(t *testing.T) {
	cache := isolateCodexCache(t)
	now := time.Now()
	stdin, stdout, _ := fakeCodexAppServer(t, codexReadFixture(now))
	if got := codexLiveProbeConverse(stdin, stdout, currentCodexAccountFingerprint()); got != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q", got)
	}
	// Sparse updates restate the Spark pool's numbers: one omits limitName, the
	// other sends it null. Neither is authoritative about the name.
	for _, name := range []string{``, `"limitName":null,`} {
		captureCodexRateLimitLine(`{"method":"account/rateLimits/updated","params":{"rateLimitsByLimitId":{`+
			`"codex_bengalfox":{"limitId":"codex_bengalfox",`+name+
			`"primary":{"usedPercent":6,"windowDurationMins":300,"resetsInSeconds":3600}}}}}`, now.Add(time.Minute))
		snap, ok := loadCodexRateLimitSnapshot(cache)
		if !ok {
			t.Fatal("expected cache")
		}
		if snap.LimitNames["codex_bengalfox"] != "GPT-5.3-Codex-Spark" {
			t.Errorf("after sparse update %q limit names=%v, want the Spark pool still named", name, snap.LimitNames)
		}
	}
}

func TestCodexMergeLimitNames_OnlyAuthoritativeNullClears(t *testing.T) {
	contributors := map[string]map[string]codexRateLimitBucket{"primary": {"pool": {}}}
	cached := map[string]string{"pool": "Spark"}
	if got := codexMergeLimitNames(cached, map[string]string{"pool": ""}, false, contributors); got["pool"] != "Spark" {
		t.Errorf("sparse null cleared the name: %v", got)
	}
	if got := codexMergeLimitNames(cached, map[string]string{"pool": ""}, true, contributors); got["pool"] != "" {
		t.Errorf("authoritative null kept the name: %v", got)
	}
}

// TestExtractCodexLimitNames_OnlyNullOrEmptyClears: a limitName of a type this
// build does not understand (a forward-compatible schema change) is skipped,
// never recorded as the empty name codexMergeLimitNames treats as a clear.
func TestExtractCodexLimitNames_OnlyNullOrEmptyClears(t *testing.T) {
	var raw map[string]interface{}
	body := `{"method":"account/rateLimits/read","result":{"rateLimitsByLimitId":{` +
		`"named":{"limitName":" Spark "},` +
		`"nulled":{"limitName":null},` +
		`"emptied":{"limitName":""},` +
		`"object":{"limitName":{"display":"Spark"}},` +
		`"number":{"limitName":7},` +
		`"absent":{}}}}`
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	names := extractCodexLimitNames(raw)
	want := map[string]string{"named": "Spark", "nulled": "", "emptied": ""}
	if len(names) != len(want) {
		t.Fatalf("names=%v, want exactly %v", names, want)
	}
	for id, name := range want {
		if got, ok := names[id]; !ok || got != name {
			t.Errorf("names[%q]=%q (present=%v), want %q", id, got, ok, name)
		}
	}
}

func TestCodexMetrics_NoFullSnapshotKeepsPlaceholders(t *testing.T) {
	isolateCodexCache(t)
	now := time.Now()
	// Only sparse evidence: nothing has shown which windows the account has.
	captureCodexRateLimitLine(
		`{"method":"token_count","params":{"rate_limits":{"secondary":{"used_percent":40,"window_minutes":10080,"resets_in_seconds":604800}}}}`,
		now,
	)
	metrics := codexMetricsFromCache(now, "")
	if len(metrics) != 2 || !metrics[0].Unknown || metrics[1].Unknown {
		t.Fatalf("metrics=%+v, want an Unknown session placeholder and a known weekly", metrics)
	}
}

func TestCodexLiveProbeConverse_RPCErrorLeavesCacheAlone(t *testing.T) {
	cache := isolateCodexCache(t)
	stdin, stdout, _ := fakeCodexAppServer(t, `{"id":2,"error":{"code":-32000,"message":"failed to fetch codex rate limits"}}`)
	if got := codexLiveProbeConverse(stdin, stdout, currentCodexAccountFingerprint()); got != liveProbeOutcomeRPCError {
		t.Fatalf("outcome=%q, want rpc_error", got)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("a failed read must not write the cache (stat err=%v)", err)
	}
}

/* ---------------------------------- Grok ---------------------------------- */

func writeGrokAuth(t *testing.T, home, key, email string, expires time.Time) {
	t.Helper()
	auth := map[string]any{
		grokExactOIDCScope: map[string]any{
			"key":           key,
			"email":         email,
			"user_id":       "user-" + email,
			"refresh_token": "opaque-refresh",
			"expires_at":    expires.UTC().Format(time.RFC3339Nano),
		},
	}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func isolateGrok(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("GROK_HOME", home)
	t.Setenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE", filepath.Join(t.TempDir(), "grok_billing_live.json"))
	origRenew := runGrokLoginRenewal
	origURL := grokBillingLiveURL
	t.Cleanup(func() {
		runGrokLoginRenewal = origRenew
		grokBillingLiveURL = origURL
	})
	runGrokLoginRenewal = func(context.Context, string, string) { t.Error("renewal must not run for a valid token") }
	return home
}

const grokBillingFixture = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-07T22:28:32.746607+00:00","end":"%END%"},` +
	`"creditUsagePercent":9,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"productUsage":[{"product":"GrokBuild","usagePercent":9}],"isUnifiedBillingUser":true}}`

func grokBillingServer(t *testing.T, handler func(auth string) (int, string)) *int32 {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		status, body := handler(r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	grokBillingLiveURL = srv.URL + "/v1/billing?format=credits"
	return &calls
}

func grokFixtureBody(end time.Time) string {
	return strings.ReplaceAll(grokBillingFixture, "%END%", end.UTC().Format(time.RFC3339Nano))
}

func TestProbeGrokBillingLive_CachesAndParserPrefersIt(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "dan@example.com", now.Add(5*time.Hour))
	grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer access-A" {
			return http.StatusUnauthorized, `{}`
		}
		return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour))
	})

	if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(home), detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	if len(usage.Metrics) == 0 || usage.Metrics[0].Unknown || usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 9 {
		t.Fatalf("metrics=%+v, want Weekly credits at 9%% from the live reading", usage.Metrics)
	}
	if usage.Metrics[0].Label != "Weekly credits" || usage.Metrics[0].ResetAt == "" {
		t.Errorf("row=%+v, want labelled weekly credits with a reset", usage.Metrics[0])
	}
}

func TestProbeGrokBillingLive_ExpiredTokenLetsGrokRenewThenRetries(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "stale", "dan@example.com", now.Add(-time.Minute))
	renewals := 0
	runGrokLoginRenewal = func(_ context.Context, _ string, base string) {
		renewals++
		writeGrokAuth(t, base, "fresh", "dan@example.com", now.Add(6*time.Hour))
	}
	grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer fresh" {
			return http.StatusUnauthorized, `{}`
		}
		return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour))
	})
	if got := probeGrokBillingLive(context.Background(), "grok", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	if renewals != 1 {
		t.Errorf("renewals=%d, want exactly one", renewals)
	}
}

func TestProbeGrokBillingLive_UnauthorizedRenewsOnceThenGivesUp(t *testing.T) {
	home := isolateGrok(t)
	writeGrokAuth(t, home, "revoked", "dan@example.com", time.Now().Add(time.Hour))
	renewals := 0
	runGrokLoginRenewal = func(context.Context, string, string) { renewals++ }
	calls := grokBillingServer(t, func(string) (int, string) { return http.StatusUnauthorized, `{}` })

	if got := probeGrokBillingLive(context.Background(), "grok", time.Now); got != grokLiveOutcomeUnauthorized {
		t.Fatalf("outcome=%q, want unauthorized", got)
	}
	if renewals != 1 || atomic.LoadInt32(calls) != 2 {
		t.Errorf("renewals=%d requests=%d, want one renewal and one retry", renewals, atomic.LoadInt32(calls))
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE")); !os.IsNotExist(err) {
		t.Errorf("nothing may be cached after a refusal (stat err=%v)", err)
	}
}

func TestGrokPresentedToken_MatchesTheUsableTokenResolver(t *testing.T) {
	cases := []struct {
		name, file, body, want string
	}{
		{"id_token-only scope", "auth.json",
			`{"` + grokExactOIDCScope + `":{"id_token":"id-only","expires_at":"2099-01-01T00:00:00Z"}}`, "id-only"},
		{"access token before id_token", "auth.json",
			`{"` + grokExactOIDCScope + `":{"access_token":"access","id_token":"id"}}`, "access"},
		{"token before access_token, as grokAuthExpiry reads it", "auth.json",
			`{"` + grokExactOIDCScope + `":{"access_token":"access","token":"tok"}}`, "tok"},
		{"key before every other credential", "auth.json",
			`{"` + grokExactOIDCScope + `":{"key":"k","token":"tok","access_token":"access"}}`, "k"},
		{"legacy cached_token.json", "cached_token.json",
			`{"cached_token":{"access_token":"legacy-access","id_token":"legacy-id"}}`, "legacy-access"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			if err := os.WriteFile(filepath.Join(base, tc.file), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if !grokHasUsableToken(base) {
				t.Fatal("fixture must be a login the card counts as usable")
			}
			if got, _, _ := grokPresentedToken(base); got != tc.want {
				t.Errorf("token=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadGrokBillingLiveSnapshot_OtherAccountNeverReplayed(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "a@example.com", now.Add(5*time.Hour))
	grokBillingServer(t, func(string) (int, string) { return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour)) })
	if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q", got)
	}
	// The user signs into a different account; A's pool must not show under B.
	writeGrokAuth(t, home, "access-B", "b@example.com", now.Add(5*time.Hour))
	if _, ok := loadGrokBillingLiveSnapshot(grokAccountFingerprintFor(home)); ok {
		t.Fatal("a live reading from another account was replayed")
	}
}

/* ------------------------------- Antigravity ------------------------------ */

func TestAntigravityHTTPPortForPID_OnlyTheRunWeStarted(t *testing.T) {
	base := t.TempDir()
	logDir := filepath.Join(base, "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, pid, port int, mod time.Time) {
		body := "I0911 server.go:1544] Starting language server process with pid " + strconv.Itoa(pid) + "\n" +
			"I0911 server.go:612] Language server listening on random port at " + strconv.Itoa(port+1) + " for HTTPS (gRPC)\n" +
			"I0911 server.go:620] Language server listening on random port at " + strconv.Itoa(port) + " for HTTP\n"
		path := filepath.Join(logDir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(path, mod, mod)
	}
	now := time.Now()
	write("cli-ours.log", 1111, 50000, now.Add(-time.Second))
	// A run the user started more recently must never be read.
	write("cli-users-session.log", 2222, 60000, now)

	if got := antigravityHTTPPortForPID(base, 1111); got != 50000 {
		t.Errorf("port=%d, want 50000 from the run with our pid", got)
	}
	if got := antigravityHTTPPortForPID(base, 3333); got != 0 {
		t.Errorf("port=%d, want 0 for a pid that has not logged yet", got)
	}
}

func TestAntigravityHTTPPortForPID_SharedLogReadsOnlyOurBlock(t *testing.T) {
	base := t.TempDir()
	logDir := filepath.Join(base, "log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two runs started in the same second share one log; the other run's block
	// comes first and our block has not logged its port yet.
	body := "I0911 server.go:1544] Starting language server process with pid 2222\n" +
		"I0911 server.go:620] Language server listening on random port at 60000 for HTTP\n" +
		"I0911 server.go:1544] Starting language server process with pid 1111\n"
	path := filepath.Join(logDir, "cli-shared.log")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := antigravityHTTPPortForPID(base, 1111); got != 0 {
		t.Fatalf("port=%d, want 0 until our own block logs a port", got)
	}
	body += "I0911 server.go:620] Language server listening on random port at 50000 for HTTP\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := antigravityHTTPPortForPID(base, 1111); got != 50000 {
		t.Errorf("port=%d, want 50000 from our block", got)
	}
	if got := antigravityHTTPPortForPID(base, 2222); got != 60000 {
		t.Errorf("port=%d, want 60000 for the other run's block", got)
	}
}

/* ------------------------------ Orchestration ----------------------------- */

func stubLiveProbes(t *testing.T) *int32 {
	t.Helper()
	var calls int32
	origCodex, origAgy, origGrok, origWarm, origDetect := probeCodexRateLimitsLiveFn, probeAntigravityQuotaLiveFn, probeGrokBillingLiveFn, warmCLIAgentModelDiscoveryFn, liveProbeDetectedAgents
	t.Cleanup(func() {
		probeCodexRateLimitsLiveFn, probeAntigravityQuotaLiveFn, probeGrokBillingLiveFn = origCodex, origAgy, origGrok
		warmCLIAgentModelDiscoveryFn, liveProbeDetectedAgents = origWarm, origDetect
		cliUsageLiveProbeMu.Lock()
		cliUsageLiveProbeLastDone, cliUsageLiveProbeLast = time.Time{}, nil
		cliUsageLiveProbeMu.Unlock()
	})
	cliUsageLiveProbeMu.Lock()
	cliUsageLiveProbeLastDone, cliUsageLiveProbeLast = time.Time{}, nil
	cliUsageLiveProbeMu.Unlock()
	liveProbeDetectedAgents = func() map[string]detectedCLIAgent {
		return map[string]detectedCLIAgent{
			"codex":       {Detected: true, Path: "codex"},
			"antigravity": {Detected: true, Path: "agy"},
			"grok":        {Detected: true, Path: "grok"},
			"claudeCode":  {Detected: true, Path: "claude"},
		}
	}
	slow := func() { atomic.AddInt32(&calls, 1); time.Sleep(50 * time.Millisecond) }
	probeCodexRateLimitsLiveFn = func(context.Context, string) string { slow(); return liveProbeOutcomeOK }
	probeAntigravityQuotaLiveFn = func(context.Context, string, string) string { slow(); return liveProbeOutcomeTimeout }
	probeGrokBillingLiveFn = func(context.Context, string, func() time.Time) string { slow(); panic("boom") }
	warmCLIAgentModelDiscoveryFn = func(context.Context, string, detectedCLIAgent, string) {}
	return &calls
}

func TestRunCLIUsageLiveProbes_ParallelOutcomesAndCooldown(t *testing.T) {
	calls := stubLiveProbes(t)

	outcomes := runCLIUsageLiveProbes(context.Background())
	want := map[string]string{"codex": "ok", "antigravity": "timeout", "grok": "panic"}
	for provider, outcome := range want {
		if outcomes[provider] != outcome {
			t.Errorf("%s=%q, want %q (all=%v)", provider, outcomes[provider], outcome, outcomes)
		}
	}
	if _, ok := outcomes["claudeCode"]; ok {
		t.Error("Claude is refreshed by the gather's forced probe, not here")
	}
	if atomic.LoadInt32(calls) != 3 {
		t.Fatalf("probe calls=%d, want 3", atomic.LoadInt32(calls))
	}

	// A second click inside the cooldown asks nobody again.
	again := runCLIUsageLiveProbes(context.Background())
	if atomic.LoadInt32(calls) != 3 {
		t.Errorf("a click within the cooldown re-probed (calls=%d)", atomic.LoadInt32(calls))
	}
	if again["codex"] != liveProbeOutcomeCooldown {
		t.Errorf("cooldown outcome=%v", again)
	}
}

func TestRunCLIUsageLiveProbes_ConcurrentClicksShareOneRun(t *testing.T) {
	calls := stubLiveProbes(t)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runCLIUsageLiveProbes(context.Background())
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("probe calls=%d across five simultaneous clicks, want one run (3 probes)", got)
	}
}

func TestRunCLIUsageLiveProbes_AntigravityModelListWaitsForQuotaProbe(t *testing.T) {
	stubLiveProbes(t)
	var probeDone atomic.Bool
	var listedBeforeProbe atomic.Bool
	probeAntigravityQuotaLiveFn = func(context.Context, string, string) string {
		time.Sleep(30 * time.Millisecond)
		probeDone.Store(true)
		return liveProbeOutcomeOK
	}
	warmCLIAgentModelDiscoveryFn = func(_ context.Context, id string, _ detectedCLIAgent, _ string) {
		if id == "antigravity" && !probeDone.Load() {
			listedBeforeProbe.Store(true)
		}
	}
	runCLIUsageLiveProbes(context.Background())
	if listedBeforeProbe.Load() {
		t.Fatal("`agy models` ran alongside the quota probe; two agy runs started in the same second share one log file")
	}
}

// TestRunCLIUsageLiveProbes_GrokModelListWaitsForBillingProbe: the billing probe
// renews the cached login by running `grok models` against the REAL home, while
// model discovery runs `grok models` against an isolated COPY of it. Run at the
// same time, the isolated child can rotate the login last and take the renewed
// credential with the temporary home it is deleted with, leaving the real home
// holding an invalidated token.
func TestRunCLIUsageLiveProbes_GrokModelListWaitsForBillingProbe(t *testing.T) {
	stubLiveProbes(t)
	var probeDone atomic.Bool
	var listedBeforeProbe atomic.Bool
	probeGrokBillingLiveFn = func(context.Context, string, func() time.Time) string {
		time.Sleep(30 * time.Millisecond)
		probeDone.Store(true)
		return liveProbeOutcomeOK
	}
	warmCLIAgentModelDiscoveryFn = func(_ context.Context, id string, _ detectedCLIAgent, _ string) {
		if id == "grok" && !probeDone.Load() {
			listedBeforeProbe.Store(true)
		}
	}
	outcomes := runCLIUsageLiveProbes(context.Background())
	if listedBeforeProbe.Load() {
		t.Fatal("`grok models` ran alongside the billing probe; both children can renew the same rotating login")
	}
	if outcomes["grok"] != liveProbeOutcomeOK {
		t.Errorf("grok=%q, want the billing probe's own outcome, not the list's", outcomes["grok"])
	}
}

// TestGrokAccountFingerprint_AccessAndIDTokenScopes: the identity preflight must
// recognize the same credential fields grokPresentedToken accepts, or a login
// whose only credential is an access/id token is refused as no_account before
// the token is ever used.
func TestGrokAccountFingerprint_AccessAndIDTokenScopes(t *testing.T) {
	claims := unsignedJWT(t, map[string]any{"email": "scoped@example.com"})
	cases := []struct{ name, body string }{
		{"id_token-only scope", `{"` + grokExactOIDCScope + `":{"id_token":"` + claims + `"}}`},
		{"access_token-only scope", `{"` + grokExactOIDCScope + `":{"access_token":"` + claims + `"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := grokPresentedToken(base); got == "" {
				t.Fatal("fixture must be a token-bearing login")
			}
			account, _ := readGrokAccountAndPlan(base)
			if account != "scoped@example.com" {
				t.Errorf("account=%q, want scoped@example.com", account)
			}
			if grokAccountFingerprintFor(base) == "" {
				t.Error("fingerprint is empty, so the live probe would report no_account")
			}
		})
	}
}

// TestGrokScopedAuthClaims_SiblingFieldsWithoutAJWT: a non-JWT access token must
// not stop the plain sibling fields from naming the account.
func TestGrokScopedAuthClaims_SiblingFieldsWithoutAJWT(t *testing.T) {
	base := t.TempDir()
	body := `{"` + grokExactOIDCScope + `":{"access_token":"opaque","email":"sibling@example.com"}}`
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if account, _ := readGrokAccountAndPlan(base); account != "sibling@example.com" {
		t.Errorf("account=%q, want sibling@example.com", account)
	}
}

// TestGrokScopedAuthClaims_IdentityFromThePresentedCredentialOnly: an opaque
// `key` is what Grok presents, so a JWT in the fallback `access_token` (stale,
// or another login's) must not name the account the billing reading is cached
// under. The entry's explicit fields do; with none, there is no identity.
func TestGrokScopedAuthClaims_IdentityFromThePresentedCredentialOnly(t *testing.T) {
	other := unsignedJWT(t, map[string]any{"email": "other-login@example.com"})
	cases := []struct {
		name, body, want string
	}{
		{
			"opaque key with sibling email",
			`{"` + grokExactOIDCScope + `":{"key":"opaque-key","access_token":"` + other + `","email":"presented@example.com"}}`,
			"presented@example.com",
		},
		{
			"opaque key without identity fields",
			`{"` + grokExactOIDCScope + `":{"key":"opaque-key","id_token":"` + other + `"}}`,
			"",
		},
		{
			// The OIDC scope is what Grok presents; the legacy scope's login is
			// never used, so it must not name the account either.
			"opaque OIDC scope never falls through to the legacy scope",
			`{"` + grokExactOIDCScope + `":{"token":"opaque-oidc"},"` + grokExactLegacyScope + `":{"key":"` + other + `"}}`,
			"",
		},
		{
			"JWT key wins over a sibling JWT",
			`{"` + grokExactOIDCScope + `":{"key":"` + unsignedJWT(t, map[string]any{"email": "key@example.com"}) + `","access_token":"` + other + `"}}`,
			"key@example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if account, _ := readGrokAccountAndPlan(base); account != tc.want {
				t.Errorf("account=%q, want %q", account, tc.want)
			}
		})
	}
}

// TestAbsoluteCodexHomeEnv: the app-server child runs from the system temp dir,
// so a relative CODEX_HOME must be resolved against the daemon's cwd first.
func TestAbsoluteCodexHomeEnv(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(wd, "relative-codex-home")
	cases := []struct{ name, in, want string }{
		{"relative is resolved", "CODEX_HOME=relative-codex-home", "CODEX_HOME=" + abs},
		{"absolute is untouched", "CODEX_HOME=" + abs, "CODEX_HOME=" + abs},
		{"empty is untouched", "CODEX_HOME=", "CODEX_HOME="},
		{"other vars are untouched", "CODEX_HOME_OTHER=rel", "CODEX_HOME_OTHER=rel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := absoluteCodexHomeEnv([]string{"PATH=/usr/bin", tc.in})
			if len(out) != 2 || out[0] != "PATH=/usr/bin" {
				t.Fatalf("env=%v, other entries must survive unchanged", out)
			}
			if out[1] != tc.want {
				t.Errorf("env=%q, want %q", out[1], tc.want)
			}
		})
	}
}

// TestProbeGrokBillingLive_KeepsTheTierTheLogStated: the billing response states
// the credit pool, never the plan name — only the TUI's log line carries the
// tier. A live refresh that wins on recency must not blank the displayed plan.
func TestProbeGrokBillingLive_KeepsTheTierTheLogStated(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "access-A", "dan@example.com", now.Add(5*time.Hour))
	// An older log line — the only statement of the subscription tier. The
	// session line attributes it to the account that is signed in.
	helperWriteGrokLog(t, home,
		`{"ts":"`+now.Add(-72*time.Hour).UTC().Format(time.RFC3339)+`","msg":"session start","ctx":{"user_id":"user-dan@example.com"}}`,
		grokBillingLine(
			now.Add(-48*time.Hour).UTC().Format(time.RFC3339), 33, "USAGE_PERIOD_TYPE_WEEKLY",
			now.Add(-96*time.Hour).UTC().Format(time.RFC3339), now.Add(24*time.Hour).UTC().Format(time.RFC3339)))
	if snap, ok := readGrokBillingSnapshot(home, grokIdentityCandidates(home)); !ok || snap.SubscriptionTier != "SuperGrok" {
		t.Fatalf("fixture must give the log a tier (ok=%v tier=%q)", ok, snap.SubscriptionTier)
	}
	grokBillingServer(t, func(string) (int, string) {
		return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour))
	})

	if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(home), detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatal("parse failed")
	}
	if usage.Plan != "SuperGrok" {
		t.Errorf("plan=%q, want the tier the log stated to survive the live refresh", usage.Plan)
	}
	if len(usage.Metrics) == 0 || usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 9 {
		t.Errorf("metrics=%+v, want the live 9%% reading to still win", usage.Metrics)
	}
}

// TestProbeGrokBillingLive_SubsecondNewerThanASameSecondLogRecord: Grok's log
// stamps carry milliseconds, so a live reading taken later in the same second
// must still win newest-first — its fraction may not be truncated away.
func TestProbeGrokBillingLive_SubsecondNewerThanASameSecondLogRecord(t *testing.T) {
	home := isolateGrok(t)
	second := time.Now().UTC().Truncate(time.Second)
	writeGrokAuth(t, home, "access-A", "dan@example.com", second.Add(5*time.Hour))
	helperWriteGrokLog(t, home,
		`{"ts":"`+second.Add(-time.Hour).Format(time.RFC3339)+`","msg":"session start","ctx":{"user_id":"user-dan@example.com"}}`,
		grokBillingLine(
			second.Add(511*time.Millisecond).Format(time.RFC3339Nano), 33, "USAGE_PERIOD_TYPE_WEEKLY",
			second.Add(-96*time.Hour).Format(time.RFC3339), second.Add(24*time.Hour).Format(time.RFC3339)))
	grokBillingServer(t, func(string) (int, string) {
		return http.StatusOK, grokFixtureBody(second.Add(72 * time.Hour))
	})
	liveAt := second.Add(900 * time.Millisecond)
	if got := probeGrokBillingLive(context.Background(), "", func() time.Time { return liveAt }); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok", got)
	}
	usage, ok := grokUsageParser{}.Parse(filepath.Dir(home), detectedCLIAgent{Detected: true}, liveAt)
	if !ok {
		t.Fatal("parse failed")
	}
	if len(usage.Metrics) == 0 || usage.Metrics[0].Consumed == nil || *usage.Metrics[0].Consumed != 9 {
		t.Errorf("metrics=%+v, want the live 9%% reading, not the same-second 33%% log record", usage.Metrics)
	}
}

// TestCodexLiveProbeConverse_AccountSwitchMidProbeIsDropped: the reading
// belongs to the credential the child was spawned with. When auth.json names a
// different account by the time it arrives, nothing may be cached — otherwise
// account A's limits would be published under account B.
func TestCodexLiveProbeConverse_AccountSwitchMidProbeIsDropped(t *testing.T) {
	cache := isolateCodexCache(t)
	stdin, stdout, _ := fakeCodexAppServer(t, codexReadFixture(time.Now()))
	spawnedUnder := fingerprintAccount("codex", "account-A")
	if spawnedUnder == currentCodexAccountFingerprint() {
		t.Fatal("fixture must sign a different account in than the one spawned")
	}
	if got := codexLiveProbeConverse(stdin, stdout, spawnedUnder); got != liveProbeOutcomeAccountChanged {
		t.Fatalf("outcome=%q, want %q", got, liveProbeOutcomeAccountChanged)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("a reading from another account must not be cached (stat err=%v)", err)
	}
}

func resetAntigravityLiveProducer(t *testing.T) {
	t.Helper()
	clear := func() { noteAntigravityLiveProducerForTest("", time.Time{}) }
	clear()
	t.Cleanup(clear)
}

func noteAntigravityLiveProducerForTest(fingerprint string, at time.Time) {
	antigravityLiveProducer.mu.Lock()
	defer antigravityLiveProducer.mu.Unlock()
	antigravityLiveProducer.fingerprint = fingerprint
	antigravityLiveProducer.at = at
}

// TestAntigravityUsageParser_LiveProbeProducerOutranksStaleSettings: the account
// lives in the OS keyring, so settings.json can name a previous login (A) while
// `agy` is signed into B. The gather a Refresh click runs next must show the
// reading the click's probe just took from B — and only for that gather: once
// the probe's word is stale, settings.json stands again.
func TestAntigravityUsageParser_LiveProbeProducerOutranksStaleSettings(t *testing.T) {
	resetAntigravityLiveProducer(t)
	home := t.TempDir()
	t.Setenv("AIEXPEDITE_AGY_QUOTA_CACHE", filepath.Join(t.TempDir(), "agyq.json"))
	helperWriteJSON(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"),
		map[string]any{"email": "a@example.com"})
	now := time.Now().UTC()
	fresh := antigravityQuotaSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		Account:    "b@example.com",
		Plan:       "Pro",
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "weekly", RemainingFraction: 0.4, ResetTime: now.Add(72 * time.Hour).Format(time.RFC3339)},
		},
	}
	if persisted, _ := antigravityCapturePersist(fresh); !persisted {
		t.Fatal("fixture must persist B's reading")
	}
	bFingerprint := fingerprintAccount("antigravity", "b@example.com")

	parse := func() *cliAgentUsage {
		usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
		if !ok {
			t.Fatal("parse failed")
		}
		return usage
	}

	// Without a probe, settings.json's account stands and B's reading is not
	// plotted under it (the existing conflict rule).
	if usage := parse(); usage.Account != "a@example.com" || !usage.Metrics[0].Unknown {
		t.Fatalf("no probe: account=%q metrics=%+v, want A with placeholders", usage.Account, usage.Metrics)
	}

	noteAntigravityLiveProducer(bFingerprint, time.Now())
	usage := parse()
	if usage.Account != "b@example.com" || usage.AccountFingerprint != bFingerprint {
		t.Errorf("after probe: account/fingerprint=%q/%q, want the account the probe's server named",
			usage.Account, usage.AccountFingerprint)
	}
	if len(usage.Metrics) != 1 || usage.Metrics[0].Unknown {
		t.Errorf("after probe: metrics=%+v, want B's fresh reading", usage.Metrics)
	}

	// The probe attests B, but the cache holds another account's reading: it is
	// never replayed on the probe's word.
	noteAntigravityLiveProducer(fingerprintAccount("antigravity", "c@example.com"), time.Now())
	if usage := parse(); usage.Account != "a@example.com" || !usage.Metrics[0].Unknown {
		t.Errorf("other producer: account=%q metrics=%+v, want A with placeholders", usage.Account, usage.Metrics)
	}

	noteAntigravityLiveProducer(bFingerprint, time.Now().Add(-antigravityLiveProducerTTL-time.Second))
	if usage := parse(); usage.Account != "a@example.com" || !usage.Metrics[0].Unknown {
		t.Errorf("stale probe: account=%q metrics=%+v, want settings.json to stand again", usage.Account, usage.Metrics)
	}
}

// TestProbeGrokBillingLive_FlatCredentialNamesItsAccount: in the flat layout the
// only statement of identity can be the claims of the credential Grok presents.
// The probe must fingerprint from that same credential instead of giving up
// with no_account before asking for the reading.
func TestProbeGrokBillingLive_FlatCredentialNamesItsAccount(t *testing.T) {
	for _, field := range []string{"access_token", "token", "key", "id_token"} {
		t.Run(field, func(t *testing.T) {
			home := isolateGrok(t)
			now := time.Now()
			jwt := unsignedJWT(t, map[string]any{"email": "flat@example.com", "exp": now.Add(5 * time.Hour).Unix()})
			helperWriteJSON(t, filepath.Join(home, "auth.json"), map[string]any{field: jwt})
			if got := grokAccountFingerprintFor(home); got != fingerprintAccount("grok", "flat@example.com") {
				t.Fatalf("fingerprint=%q, want the presented credential's account", got)
			}
			var sent string
			grokBillingServer(t, func(auth string) (int, string) {
				sent = auth
				return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour))
			})
			if got := probeGrokBillingLive(context.Background(), "", time.Now); got != grokLiveOutcomeOK {
				t.Fatalf("outcome=%q, want ok", got)
			}
			if sent != "Bearer "+jwt {
				t.Errorf("Authorization=%q, want the credential the fingerprint came from", sent)
			}
		})
	}
}

// TestGrokIdentityCandidates_PresentedClaimsOnlyWhenTheyNameTheAccount: stale
// identity fields for A beside a credential that claims B keep A as the account,
// so B must not become an accepted identity for matching billing records.
func TestGrokIdentityCandidates_PresentedClaimsOnlyWhenTheyNameTheAccount(t *testing.T) {
	jwtB := unsignedJWT(t, map[string]any{"email": "b@example.com"})
	cases := []struct {
		name string
		auth map[string]any
		want []string
		deny string
	}{
		{"identity fields win, B excluded", map[string]any{"email": "a@example.com", "access_token": jwtB}, []string{"a@example.com"}, "b@example.com"},
		{"only the credential names the account", map[string]any{"access_token": jwtB}, []string{"b@example.com"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			helperWriteJSON(t, filepath.Join(base, "auth.json"), tc.auth)
			got := grokIdentityCandidates(base)
			for _, w := range tc.want {
				if !containsString(got, w) {
					t.Errorf("candidates=%v, want %q", got, w)
				}
			}
			if tc.deny != "" && containsString(got, tc.deny) {
				t.Errorf("candidates=%v, must not accept %q", got, tc.deny)
			}
		})
	}
}
