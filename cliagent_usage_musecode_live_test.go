package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMuseServe is a pipe-backed MSP host. It answers the calls the client
// makes and records them; usage is the `usage/read` payload and changed the
// `usage/changed` params sent right after `turn/start` (nil = none).
type fakeMuseServe struct {
	mu      sync.Mutex
	methods []string
	usage   json.RawMessage
	changed json.RawMessage
	models  json.RawMessage
	rpcErr  map[string]bool
	// afterTurn is what usage/read answers once a turn has started — the
	// Windows route, where the notification is held until the next request.
	afterTurn json.RawMessage
	turned    bool
}

func (f *fakeMuseServe) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// start wires a client to the fake and completes the handshake, exactly as
// startMuseCodeMSP does for a real host.
func (f *fakeMuseServe) start(ctx context.Context, _ string, onNotify func(string, json.RawMessage)) (*museCodeMSPClient, error) {
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	go f.serve(serverIn, serverOut)
	c := newMuseCodeMSPClient(clientOut, clientIn, onNotify)
	if err := c.handshake(ctx); err != nil {
		_ = clientOut.Close()
		return nil, err
	}
	return c, nil
}

func (f *fakeMuseServe) serve(in io.Reader, out io.WriteCloser) {
	defer out.Close()
	write := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = out.Write(append(b, '\n'))
	}
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, req.Method)
		fail := f.rpcErr[req.Method]
		turned := f.turned
		if req.Method == "turn/start" {
			f.turned = true
		}
		f.mu.Unlock()
		if req.ID == nil {
			continue
		}
		if fail {
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32603, "message": "boom"}})
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "muse", "version": "1.4.0"}}
		case "usage/read":
			if f.usage != nil {
				result = map[string]any{"usage": f.usage}
			} else if turned && f.afterTurn != nil {
				result = map[string]any{"usage": f.afterTurn}
			}
		case "model/list":
			if f.models != nil {
				result = f.models
			}
		case "session/start":
			result = map[string]any{"session": map[string]any{"sessionId": "s-1"}, "viewCursor": "c"}
		case "turn/start":
			// A real host answers every turn/start with a command ack and a
			// session event before usage arrives; send noise first.
			write(map[string]any{"jsonrpc": "2.0", "method": "turn/started", "params": map[string]any{"turnId": "t"}})
			write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{"status": "accepted"}})
			if f.changed != nil {
				write(map[string]any{"jsonrpc": "2.0", "method": "usage/changed", "params": f.changed})
			}
			continue
		}
		write(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

// museUsageJSON is an MSP SubscriptionUsage payload shaped like a real one
// (captured from Muse Code 1.4.0 on 2026-09-29); weekly is over quota.
func museUsageJSON(window, weekly, observed time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"window":{"usedPercent":12,"windowDurationMins":300,"resetsAtMs":%d},"weekly":{"usedPercent":140,"resetsAtMs":%d},"tier":"27681631238169137","observedAtMs":%d}`,
		window.UnixMilli(), weekly.UnixMilli(), observed.UnixMilli()))
}

// isolateMuseCode points every Muse file this package reads or writes at temp
// paths and restores the protocol seam afterwards.
func isolateMuseCode(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("META_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("MUSE_AUTH_PATH", "")
	t.Setenv("AIEXPEDITE_MUSECODE_USAGE_LIVE_CACHE", filepath.Join(t.TempDir(), "musecode_usage_live.json"))
	t.Setenv("AIEXPEDITE_MUSECODE_MODELS_CACHE", filepath.Join(t.TempDir(), "musecode_models.json"))
	orig := startMuseCodeMSPFn
	t.Cleanup(func() { startMuseCodeMSPFn = orig })
	return home
}

func writeMuseConfig(t *testing.T, home, name, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "muse")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const museAuthFixture = `{"schema_version":1,"providers":{"meta":{"access_token":"SECRET-TOKEN","api_key":"SECRET-KEY","user_email":"Dev@Example.com","user_full_name":"Dev User","mechanism":"oauth"}}}`

/* --------------------------------- Parser --------------------------------- */

func TestMuseCodeParser_IdentityModelAndUnknownQuota(t *testing.T) {
	home := isolateMuseCode(t)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	writeMuseConfig(t, home, "settings.json", `{"schema_version":1,"provider":"meta","model":"muse-spark-1.3","reasoning_effort":"high"}`)
	now := time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC)

	usage, ok := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Version: "1.4.0-R4302.1", Path: "/x/muse"}, now)
	if !ok || usage == nil {
		t.Fatal("parser must always emit an entry")
	}
	if usage.Account != "Dev@Example.com" || usage.AccountFingerprint != museCodeAccountFingerprint("dev@example.com") {
		t.Fatalf("account=%q fp=%q", usage.Account, usage.AccountFingerprint)
	}
	if usage.Model != "muse-spark-1.3" {
		t.Fatalf("model=%q", usage.Model)
	}
	if usage.AuthState != museCodeAuthReady {
		t.Fatalf("authState=%q", usage.AuthState)
	}
	b, _ := json.Marshal(usage)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("a credential reached the snapshot: %s", b)
	}
	if len(usage.Metrics) != 2 || !usage.Metrics[0].Unknown || !usage.Metrics[1].Unknown ||
		usage.Metrics[0].Kind != limitKindSession || usage.Metrics[1].Kind != limitKindWeekly {
		t.Fatalf("without a reading both windows must read Unknown, got %#v", usage.Metrics)
	}
}

func TestMuseCodeParser_MuseAuthPathOverride(t *testing.T) {
	home := isolateMuseCode(t)
	path := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(path, []byte(museAuthFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSE_AUTH_PATH", path)
	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if usage.AuthState != museCodeAuthReady || usage.Account != "Dev@Example.com" {
		t.Fatalf("MUSE_AUTH_PATH ignored: %#v", usage)
	}
}

func TestMuseCodeParser_MetricsFromLiveReading(t *testing.T) {
	home := isolateMuseCode(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC)
	reading, ok := parseMuseCodeSubscriptionUsage(museUsageJSON(now.Add(2*time.Hour), now.Add(5*24*time.Hour), now.Add(-time.Minute)))
	if !ok {
		t.Fatal("fixture must parse")
	}
	reading.AccountKey = museCodeAccountFingerprint("dev@example.com")
	if !saveMuseCodeUsageLive(reading) {
		t.Fatal("save failed")
	}

	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if len(usage.Metrics) != 2 {
		t.Fatalf("metrics=%#v", usage.Metrics)
	}
	session, weekly := usage.Metrics[0], usage.Metrics[1]
	if session.Unknown || *session.Consumed != 12 || *session.Remaining != 88 || session.Label != "5-hour session window" {
		t.Fatalf("session=%#v", session)
	}
	if session.ResetAt != now.Add(2*time.Hour).UTC().Format(time.RFC3339) || session.ObservedAt == "" {
		t.Fatalf("session times=%q/%q", session.ResetAt, session.ObservedAt)
	}
	if weekly.Unknown || *weekly.Consumed != 100 || *weekly.Remaining != 0 {
		t.Fatalf("an over-quota 140%% must clamp to fully used, got %#v", weekly)
	}
	if usage.DataSource != "muse usage/read" {
		t.Fatalf("dataSource=%q", usage.DataSource)
	}

	// Once the 5-hour window has reset, its old figure describes a window
	// that no longer exists.
	later, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now.Add(3*time.Hour))
	if !later.Metrics[0].Unknown || later.Metrics[0].ObservedAt == "" || later.Metrics[1].Unknown {
		t.Fatalf("after the window reset: %#v", later.Metrics)
	}
}

func TestMuseCodeParser_ReadingOfAnotherAccountIsIgnored(t *testing.T) {
	home := isolateMuseCode(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	reading, _ := parseMuseCodeSubscriptionUsage(museUsageJSON(now.Add(time.Hour), now.Add(48*time.Hour), now))
	reading.AccountKey = museCodeAccountFingerprint("someone-else@example.com")
	saveMuseCodeUsageLive(reading)

	usage, _ := museCodeUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, now)
	if !usage.Metrics[0].Unknown || !usage.Metrics[1].Unknown {
		t.Fatalf("another account's reading was shown: %#v", usage.Metrics)
	}
}

func TestParseMuseCodeSubscriptionUsage_RejectsIncompletePayloads(t *testing.T) {
	for _, raw := range []string{``, `{}`, `null`, `{"window":{"usedPercent":1,"windowDurationMins":300,"resetsAtMs":1},"weekly":{"usedPercent":1,"resetsAtMs":1}}`,
		`{"window":{"usedPercent":-1,"windowDurationMins":300,"resetsAtMs":1},"weekly":{"usedPercent":1,"resetsAtMs":1},"observedAtMs":1}`} {
		if _, ok := parseMuseCodeSubscriptionUsage(json.RawMessage(raw)); ok {
			t.Errorf("accepted %q", raw)
		}
	}
}

/* --------------------------------- Probe ---------------------------------- */

func TestProbeMuseCodeUsageLive_TurnThenFirstUsageChanged(t *testing.T) {
	home := isolateMuseCode(t)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	now := time.Now()
	fake := &fakeMuseServe{
		changed: museUsageJSON(now.Add(time.Hour), now.Add(72*time.Hour), now),
		models:  json.RawMessage(`{"providerId":"meta","profileId":"tbh","source":"providerCatalog","models":[{"modelId":"muse-spark-1.3","displayLabel":"muse-spark-1.3","isDefault":false,"variants":["minimal","low","high","max"]},{"modelId":"muse-spark-1.3-contributor","displayLabel":"muse-spark-1.3-contributor","isDefault":true,"variants":["low","high"]}]}`),
	}
	startMuseCodeMSPFn = fake.start

	outcome := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Version: "1.4.0", Path: "/x/muse"}, time.Now)
	if outcome != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q, calls=%v", outcome, fake.seen())
	}
	want := []string{"initialize", "initialized", "model/list", "usage/read", "session/start", "turn/start"}
	if got := fake.seen(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v, want %v", got, want)
	}
	cached, ok := loadMuseCodeUsageLive(museCodeAccountFingerprint("dev@example.com"))
	if !ok || cached.Window.UsedPercent != 12 || cached.Weekly.UsedPercent != 140 {
		t.Fatalf("cached=%#v ok=%v", cached, ok)
	}
	models, fresh, ok := loadMuseCodeModelsCache("1.4.0", time.Now())
	if !ok || !fresh || len(models.Models) != 2 || models.DefaultModel != "muse-spark-1.3-contributor" || !models.Exhaustive {
		t.Fatalf("models=%#v fresh=%v ok=%v", models, fresh, ok)
	}

	// A second click inside the minimum interval spends no turn.
	startMuseCodeMSPFn = func(context.Context, string, func(string, json.RawMessage)) (*museCodeMSPClient, error) {
		t.Fatal("a click inside the minimum interval must not start muse serve")
		return nil, nil
	}
	if again := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); again != liveProbeOutcomeCooldown {
		t.Fatalf("second click=%q", again)
	}
}

func TestProbeMuseCodeUsageLive_ObservedUsageNeedsNoTurn(t *testing.T) {
	isolateMuseCode(t)
	now := time.Now()
	fake := &fakeMuseServe{usage: museUsageJSON(now.Add(time.Hour), now.Add(72*time.Hour), now)}
	startMuseCodeMSPFn = fake.start
	if outcome := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); outcome != liveProbeOutcomeOK {
		t.Fatalf("outcome=%q", outcome)
	}
	for _, method := range fake.seen() {
		if method == "turn/start" || method == "session/start" {
			t.Fatalf("a host that already observed usage must not be sent a turn: %v", fake.seen())
		}
	}
	// No identity on disk: the reading is scoped to this device's one account.
	if _, ok := loadMuseCodeUsageLive(museCodeUnknownAccountKey); !ok {
		t.Fatal("reading was not cached under the device key")
	}
}

func TestProbeMuseCodeUsageLive_PollsWhenTheNotificationIsHeld(t *testing.T) {
	isolateMuseCode(t)
	orig := museCodeUsagePollInterval
	museCodeUsagePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { museCodeUsagePollInterval = orig })
	now := time.Now()
	fake := &fakeMuseServe{afterTurn: museUsageJSON(now.Add(time.Hour), now.Add(72*time.Hour), now)}
	startMuseCodeMSPFn = fake.start
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got := probeMuseCodeUsageLive(ctx, detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); got != liveProbeOutcomeOK {
		t.Fatalf("got %q, calls=%v", got, fake.seen())
	}
	reads := 0
	for _, m := range fake.seen() {
		if m == "usage/read" {
			reads++
		}
	}
	if reads < 2 {
		t.Fatalf("the reading must come from a poll after the turn (usage/read x%d)", reads)
	}
}

func TestProbeMuseCodeUsageLive_FailureOutcomes(t *testing.T) {
	t.Run("spawn failure", func(t *testing.T) {
		isolateMuseCode(t)
		startMuseCodeMSPFn = func(context.Context, string, func(string, json.RawMessage)) (*museCodeMSPClient, error) {
			return nil, errors.New("no binary")
		}
		if got := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); got != liveProbeOutcomeSpawnFailed {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("turn rejected", func(t *testing.T) {
		isolateMuseCode(t)
		fake := &fakeMuseServe{rpcErr: map[string]bool{"turn/start": true}}
		startMuseCodeMSPFn = fake.start
		if got := probeMuseCodeUsageLive(context.Background(), detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); got != liveProbeOutcomeRPCError {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("no usage ever arrives", func(t *testing.T) {
		isolateMuseCode(t)
		fake := &fakeMuseServe{}
		startMuseCodeMSPFn = fake.start
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if got := probeMuseCodeUsageLive(ctx, detectedCLIAgent{Detected: true, Path: "/x/muse"}, time.Now); got != liveProbeOutcomeTimeout {
			t.Fatalf("got %q", got)
		}
		if _, ok := loadMuseCodeUsageLive(museCodeUnknownAccountKey); ok {
			t.Fatal("nothing observed, yet a reading was cached")
		}
	})
}

func TestRunCLIUsageLiveProbes_IncludesMuseCode(t *testing.T) {
	stubLiveProbes(t)
	orig := probeMuseCodeUsageLiveFn
	t.Cleanup(func() { probeMuseCodeUsageLiveFn = orig })
	detect := liveProbeDetectedAgents
	liveProbeDetectedAgents = func() map[string]detectedCLIAgent {
		m := detect()
		m["museCode"] = detectedCLIAgent{Detected: true, Path: "muse"}
		return m
	}
	var warmedAfter bool
	var probed bool
	probeMuseCodeUsageLiveFn = func(context.Context, detectedCLIAgent, func() time.Time) string {
		probed = true
		return liveProbeOutcomeOK
	}
	warmCLIAgentModelDiscoveryFn = func(_ context.Context, id string, _ detectedCLIAgent, _ string) {
		if id == "museCode" {
			warmedAfter = probed
		}
	}
	outcomes := runCLIUsageLiveProbes(context.Background())
	if outcomes["museCode"] != liveProbeOutcomeOK {
		t.Fatalf("outcomes=%v", outcomes)
	}
	if !warmedAfter {
		t.Fatal("Muse's model list must be warmed after its probe, which refreshes it")
	}
}

/* --------------------------------- Models --------------------------------- */

func TestDiscoverMuseCodeModels_CacheThenHostThenStaleFallback(t *testing.T) {
	isolateMuseCode(t)
	calls := 0
	fake := &fakeMuseServe{models: json.RawMessage(`{"providerId":"meta","profileId":"tbh","source":"providerCatalog","models":[{"modelId":"muse-spark-1.3","displayLabel":"muse-spark-1.3","isDefault":true,"variants":["minimal","xhigh","bogus"]}]}`)}
	startMuseCodeMSPFn = func(ctx context.Context, l string, n func(string, json.RawMessage)) (*museCodeMSPClient, error) {
		calls++
		return fake.start(ctx, l, n)
	}
	agent := detectedCLIAgent{Detected: true, Version: "1.4.0", Path: "/x/muse"}

	got, ok := discoverCLIAgentModels(context.Background(), "musecode", agent, "")
	if !ok || calls != 1 || len(got.Models) != 1 || strings.Join(got.Models[0].Efforts, ",") != "minimal,xhigh" || got.DefaultModel != "muse-spark-1.3" {
		t.Fatalf("got=%#v ok=%v calls=%d", got, ok, calls)
	}
	// Fresh on disk: no second host.
	if _, ok := discoverMuseCodeModels(context.Background(), agent); !ok || calls != 1 {
		t.Fatalf("a fresh saved list must not start muse serve (calls=%d)", calls)
	}
	// A different installed version invalidates the saved list; a host that
	// cannot start then reports nothing rather than another build's list.
	startMuseCodeMSPFn = func(context.Context, string, func(string, json.RawMessage)) (*museCodeMSPClient, error) {
		return nil, errors.New("down")
	}
	if _, ok := discoverMuseCodeModels(context.Background(), detectedCLIAgent{Detected: true, Version: "1.5.0"}); ok {
		t.Fatal("another version's list was reused")
	}
}

func TestParseMuseCodeModelList_NonProviderCatalogIsAFloor(t *testing.T) {
	got, ok := parseMuseCodeModelList(json.RawMessage(`{"source":"bundledCatalog","models":[{"modelId":"a","displayLabel":"A"},{"modelId":"a"}]}`))
	if !ok || got.Exhaustive || len(got.Models) != 1 || got.Models[0].Label != "A" {
		t.Fatalf("got=%#v ok=%v", got, ok)
	}
	if _, ok := parseMuseCodeModelList(json.RawMessage(`{"source":"providerCatalog","models":[]}`)); ok {
		t.Fatal("an empty catalog is inconclusive")
	}
}

/* ---------------------------------- Misc ---------------------------------- */

func TestMuseCodeServeBinary_PrefersTheRealBuildBesideTheLauncher(t *testing.T) {
	dir := t.TempDir()
	launcher := filepath.Join(dir, "muse.cmd")
	_ = os.WriteFile(launcher, []byte("@echo off"), 0o755)
	if got := museCodeServeBinary(launcher); got != launcher {
		t.Fatalf("no version file: got %q", got)
	}
	_ = os.WriteFile(filepath.Join(dir, ".muse-version"), []byte("1.4.0-R4302.1\n"), 0o644)
	if got := museCodeServeBinary(launcher); got != launcher {
		t.Fatalf("version file but no build: got %q", got)
	}
	name := "muse-bin-1.4.0-R4302.1"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	_ = os.WriteFile(filepath.Join(dir, name), []byte("bin"), 0o755)
	if got := museCodeServeBinary(launcher); got != filepath.Join(dir, name) {
		t.Fatalf("got %q", got)
	}
}

func TestNewMuseCodeCommandID_IsUUIDv7(t *testing.T) {
	id := newMuseCodeCommandID()
	if len(id) != 36 || id[14] != '7' || !strings.ContainsAny(string(id[19]), "89ab") {
		t.Fatalf("not a v7 UUID: %q", id)
	}
	if a, b := newMuseCodeCommandID(), newMuseCodeCommandID(); a == b {
		t.Fatal("ids repeat")
	}
}
