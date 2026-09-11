// cliagent_usage_live_probe.go — asks every installed coding CLI's provider for
// its CURRENT usage when the user clicks Refresh on the CLI Agents card.
//
// Why this exists:
//
//	Every provider except Claude only reveals usage as a side effect of real
//	work: Codex streams rate limits while an app-server session is open,
//	Antigravity's quota lives in a language server that dies with each `agy`
//	run, and Grok logs its credit pool only from the interactive TUI. The card
//	therefore showed whatever the last run happened to leave behind — days old,
//	for a quota window that had long since reset.
//
//	A click now asks each provider directly, then runs the normal gather, so the
//	signed receipt carries readings taken moments earlier:
//
//	  - Claude Code: the OAuth usage request already runs forced on every
//	    refresh (WithClaudeUsageForceProbe) — nothing extra here.
//	  - Codex: a short-lived `codex app-server` answers `account/rateLimits/read`
//	    from OpenAI's backend. No turn is spent. The response is handed to the
//	    same capture function that ingests live session frames.
//	  - Grok: GET of the credits endpoint the TUI uses
//	    (cliagent_usage_grok_live.go). No turn is spent.
//	  - Antigravity: `agy -p` is started in an empty temp directory only to bring
//	    up its language server, whose quota RPC forces a fresh fetch; the process
//	    is killed as soon as the reading lands, normally before any model output.
//	    The server is identified by the PID `agy` logs, so a run the user already
//	    has open is never read or touched.
//
//	Model discovery is warmed in the same window, with the time `agy models`
//	actually needs, so the bounded gather that follows answers from the cache.
//
// Boundaries: only the signed `live-probe` refresh (sent for a real click) runs
// this; the periodic and tab-open refreshes never do. Probes run in parallel
// under one budget; a concurrent click shares the running probe and a click
// within the cooldown reuses its result. Nothing a CLI prints is kept.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// cliUsageLiveProbeArg is the signed first arg terminal-service adds to a
	// __cli_usage_refresh__ that the user asked for by clicking Refresh.
	cliUsageLiveProbeArg = "live-probe"
	// cliUsageLiveProbeBudget bounds the whole probe phase; the gather that
	// follows keeps its own 10s deadline. terminal-service waits long enough for
	// both (CLI_USAGE_LIVE_PROBE_INFLIGHT_TIMEOUT_MS).
	cliUsageLiveProbeBudget = 30 * time.Second
	// cliUsageLiveProbeCooldown: a second click this soon after a completed
	// probe reuses its readings instead of asking every provider again.
	cliUsageLiveProbeCooldown = 15 * time.Second

	codexLiveProbeTimeout       = 15 * time.Second
	antigravityLiveProbeTimeout = 20 * time.Second
	antigravityLiveProbePoll    = 250 * time.Millisecond
	// antigravityLiveProbePrompt is sent only because `agy` needs a prompt to
	// start; the process is normally killed before a turn completes.
	antigravityLiveProbePrompt = "Reply with a single period."

	cliUsageLiveProbeMaxLine = 4 << 20
)

// Live-probe outcome codes — a closed set, logged on the device only.
const (
	liveProbeOutcomeOK          = "ok"
	liveProbeOutcomeTimeout     = "timeout"
	liveProbeOutcomeSpawnFailed = "spawn_failed"
	liveProbeOutcomeRPCError    = "rpc_error"
	liveProbeOutcomeNoReading   = "no_reading"
	liveProbeOutcomeNotSigned   = "not_attributable"
	liveProbeOutcomeCooldown    = "cooldown"
)

// cliUsageRefreshWantsLiveProbe reports whether a refresh carries the signed
// live-probe flag. Args are inside the command HMAC, so the flag cannot be
// added to a periodic refresh in transit.
func cliUsageRefreshWantsLiveProbe(cmd commandMsg) bool {
	return len(cmd.Args) > 0 && cmd.Args[0] == cliUsageLiveProbeArg
}

// Seams so tests drive the orchestration without spawning real CLIs.
var (
	probeCodexRateLimitsLiveFn   = probeCodexRateLimitsLive
	probeAntigravityQuotaLiveFn  = probeAntigravityQuotaLive
	probeGrokBillingLiveFn       = probeGrokBillingLive
	warmCLIAgentModelDiscoveryFn = warmCLIAgentModelDiscovery
	liveProbeDetectedAgents      = gatherCLIAgents
)

var (
	cliUsageLiveProbeGroup    singleflight.Group
	cliUsageLiveProbeMu       sync.Mutex
	cliUsageLiveProbeLastDone time.Time
	cliUsageLiveProbeLast     map[string]string
)

// runCLIUsageLiveProbes runs every applicable probe in parallel and returns a
// provider → outcome map. A caller that arrives while a probe is running shares
// it; one that arrives within the cooldown of a finished probe gets its result.
func runCLIUsageLiveProbes(ctx context.Context) map[string]string {
	v, _, _ := cliUsageLiveProbeGroup.Do("live", func() (any, error) {
		cliUsageLiveProbeMu.Lock()
		if !cliUsageLiveProbeLastDone.IsZero() && time.Since(cliUsageLiveProbeLastDone) < cliUsageLiveProbeCooldown {
			replay := map[string]string{}
			for provider := range cliUsageLiveProbeLast {
				replay[provider] = liveProbeOutcomeCooldown
			}
			cliUsageLiveProbeMu.Unlock()
			return replay, nil
		}
		cliUsageLiveProbeMu.Unlock()

		outcomes := runCLIUsageLiveProbesOnce(ctx)

		cliUsageLiveProbeMu.Lock()
		cliUsageLiveProbeLastDone = time.Now()
		cliUsageLiveProbeLast = outcomes
		cliUsageLiveProbeMu.Unlock()
		return outcomes, nil
	})
	outcomes, _ := v.(map[string]string)
	return outcomes
}

func runCLIUsageLiveProbesOnce(parent context.Context) map[string]string {
	ctx, cancel := context.WithTimeout(parent, cliUsageLiveProbeBudget)
	defer cancel()

	detected := liveProbeDetectedAgents()
	home, _ := os.UserHomeDir()

	var mu sync.Mutex
	outcomes := map[string]string{}
	var wg sync.WaitGroup
	run := func(provider string, probe func() string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome := safeLiveProbe(probe)
			mu.Lock()
			outcomes[provider] = outcome
			mu.Unlock()
		}()
	}

	if agent, ok := detected["codex"]; ok && agent.Detected {
		run("codex", func() string { return probeCodexRateLimitsLiveFn(ctx, agent.Path) })
	}
	if agent, ok := detected["grok"]; ok && agent.Detected {
		run("grok", func() string { return probeGrokBillingLiveFn(ctx, agent.Path, time.Now) })
	}
	// Model lists are part of the same click: the bounded gather gives a list
	// probe two seconds, which `agy models` (a network fetch) never finishes in.
	resetCLIAgentModelProbeCache()
	if agent, ok := detected["antigravity"]; ok && agent.Detected {
		// `agy` names its log after the SECOND it started (cli-YYYYMMDD_HHMMSS.log),
		// so a quota probe and `agy models` launched together share one log file
		// and the PID line the probe matches on is overwritten. The list waits
		// for the probe.
		run("antigravity", func() string {
			outcome := probeAntigravityQuotaLiveFn(ctx, agent.Path, home)
			warmCLIAgentModelDiscoveryFn(ctx, "antigravity", agent, home)
			return outcome
		})
	}
	for id, agent := range detected {
		if !agent.Detected || id == "antigravity" {
			continue
		}
		id, agent := id, agent
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = safeLiveProbe(func() string {
				warmCLIAgentModelDiscoveryFn(ctx, id, agent, home)
				return liveProbeOutcomeOK
			})
		}()
	}
	wg.Wait()

	providers := make([]string, 0, len(outcomes))
	for provider := range outcomes {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	parts := make([]string, 0, len(providers))
	for _, provider := range providers {
		parts = append(parts, provider+"="+outcomes[provider])
	}
	fmt.Printf("%s[cli-usage] live probe: %s%s\n", colorCyan, strings.Join(parts, " "), colorReset)
	return outcomes
}

// safeLiveProbe keeps a panicking probe from taking the refresh down with it:
// the refresh still gathers and publishes whatever the caches hold.
func safeLiveProbe(probe func() string) (outcome string) {
	defer func() {
		if r := recover(); r != nil {
			outcome = "panic"
		}
	}()
	return probe()
}

// warmCLIAgentModelDiscovery fills the model-probe cache for one agent under
// the live-probe budget, so the gather that follows reads the cached list.
func warmCLIAgentModelDiscovery(ctx context.Context, agentID string, agent detectedCLIAgent, home string) {
	for _, entry := range activeCLIAgentCatalog() {
		if entry.ID == agentID {
			_, _ = cachedCLIAgentModelDiscovery(ctx, cliAgentCatalogParserKey(entry), agent, home, time.Now())
			return
		}
	}
}

/* --------------------------------------------------------------------------
   Codex
   -------------------------------------------------------------------------- */

// codexLiveProbeReadID is the JSON-RPC id of the one rate-limit read.
const codexLiveProbeReadID = 2

// probeCodexRateLimitsLive starts a private `codex app-server`, asks it for the
// account's rate limits and hands the answer to captureCodexRateLimitLine.
// The app-server fetches the figures from OpenAI (it fails when offline), so
// the reading is current, not a replay of local state.
func probeCodexRateLimitsLive(parent context.Context, codexPath string) string {
	if codexPath == "" {
		codexPath = resolveExecutable("codex")
	}
	ctx, cancel := context.WithTimeout(parent, codexLiveProbeTimeout)
	defer cancel()

	cmd := exec.Command(codexPath, buildCodexAppServerArgs(nil)...)
	cmd.Dir = os.TempDir()
	cmd.Env = sanitizeCodexAppServerEnv(os.Environ())
	cmd.Stderr = io.Discard
	hideWindow(cmd)
	// Unix: own process group, so the tree kill below reaches every child.
	detachControllingTTY(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return liveProbeOutcomeSpawnFailed
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return liveProbeOutcomeSpawnFailed
	}
	if err := cmd.Start(); err != nil {
		return liveProbeOutcomeSpawnFailed
	}
	// Whatever happens below, the whole tree goes: on Windows the npm shim is
	// cmd.exe → node → codex.exe.
	defer func() {
		_ = stdin.Close()
		killAntigravityProcessTree(cmd)
		waited := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
	}()

	result := make(chan string, 1)
	go func() { result <- codexLiveProbeConverse(stdin, stdout) }()
	select {
	case outcome := <-result:
		return outcome
	case <-ctx.Done():
		return liveProbeOutcomeTimeout
	}
}

// codexLiveProbeConverse runs the initialize → read exchange over the child's
// stdio. Split out so tests can drive it with pipes instead of a real Codex.
func codexLiveProbeConverse(stdin io.Writer, stdout io.Reader) string {
	send := func(frame map[string]any) bool {
		b, err := json.Marshal(frame)
		if err != nil {
			return false
		}
		_, err = stdin.Write(append(b, '\n'))
		return err == nil
	}
	if !send(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{"name": "aiexpedite-usage-probe", "version": Version}},
	}) {
		return liveProbeOutcomeSpawnFailed
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), cliUsageLiveProbeMaxLine)
	for scanner.Scan() {
		var frame struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.ID == nil {
			continue
		}
		switch *frame.ID {
		case 1:
			if len(frame.Error) > 0 {
				return liveProbeOutcomeRPCError
			}
			if !send(map[string]any{"method": "initialized"}) ||
				!send(map[string]any{"id": codexLiveProbeReadID, "method": "account/rateLimits/read"}) {
				return liveProbeOutcomeSpawnFailed
			}
		case codexLiveProbeReadID:
			if len(frame.Error) > 0 || len(frame.Result) == 0 {
				return liveProbeOutcomeRPCError
			}
			// Codex omits the `jsonrpc` member, which captureCodexRateLimitLine
			// requires before it trusts a `result` as a full snapshot. This frame
			// answers the request made just above, so it is re-wrapped as the
			// JSON-RPC response it is.
			envelope, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      codexLiveProbeReadID,
				"result":  frame.Result,
			})
			if err != nil {
				return liveProbeOutcomeRPCError
			}
			captureCodexRateLimitLine(string(envelope), time.Now())
			return liveProbeOutcomeOK
		}
	}
	return liveProbeOutcomeNoReading
}

/* --------------------------------------------------------------------------
   Antigravity
   -------------------------------------------------------------------------- */

// antigravityLanguageServerPIDPattern is the startup line every `agy` run logs.
// It names the PID of the process that owns the server — the `agy` process
// itself — which is what ties a log (and its port) to the run we started.
var antigravityLanguageServerPIDPattern = regexp.MustCompile(`Starting language server process with pid (\d+)`)

// antigravityHTTPPortForPID returns the plain-HTTP port logged by the run with
// the given PID, or 0 while that run has not logged it yet.
func antigravityHTTPPortForPID(base string, pid int) int {
	dir := antigravityLogDir(base)
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type logFile struct {
		path    string
		modTime time.Time
	}
	files := make([]logFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{path: filepath.Join(dir, entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) > antigravityQuotaMaxLogs {
		files = files[:antigravityQuotaMaxLogs]
	}
	want := strconv.Itoa(pid)
	for _, file := range files {
		body, err := readBoundedHeadTail(file.path, antigravityLogScanBytes)
		if err != nil {
			continue
		}
		match := antigravityLanguageServerPIDPattern.FindSubmatch(body)
		if match == nil || string(match[1]) != want {
			continue
		}
		if ports := antigravityPortsInLog(body); len(ports) > 0 {
			return ports[0]
		}
		return 0
	}
	return 0
}

// probeAntigravityQuotaLive starts `agy` only to bring up its language server,
// reads the quota the server fetches for its signed-in account, and kills the
// run as soon as the reading is persisted.
func probeAntigravityQuotaLive(parent context.Context, agyPath, home string) string {
	if agyPath == "" {
		agyPath = resolveExecutable("agy")
	}
	ctx, cancel := context.WithTimeout(parent, antigravityLiveProbeTimeout)
	defer cancel()

	workDir, err := os.MkdirTemp("", "aix-agy-usage-")
	if err != nil {
		return liveProbeOutcomeSpawnFailed
	}
	defer removeDirEventually(workDir)

	cmd := exec.Command(agyPath, "-p", antigravityLiveProbePrompt, "--print-timeout", "30s")
	cmd.Dir = workDir
	cmd.Env = sanitizeAntigravityEnv(os.Environ())
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	hideWindow(cmd)
	// Unix: own process group, so the tree kill below reaches every child.
	detachControllingTTY(cmd)
	if err := cmd.Start(); err != nil {
		return liveProbeOutcomeSpawnFailed
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	defer func() {
		killAntigravityProcessTree(cmd)
		// A descendant that escaped the tree kill can hold the output pipes
		// open; the probe does not wait on it past a short grace.
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}()

	pid := cmd.Process.Pid
	client := antigravityLoopbackClient()
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(antigravityLiveProbePoll)
	defer ticker.Stop()
	sawReading := false
	for {
		port := 0
		for _, base := range antigravityQuotaBases(home) {
			if port = antigravityHTTPPortForPID(base, pid); port != 0 {
				break
			}
		}
		if port != 0 {
			attemptCtx, attemptCancel := context.WithTimeout(ctx, antigravityQuotaTimeout)
			snap, ok := fetchAntigravityQuotaOnPort(attemptCtx, client, port, time.Now())
			attemptCancel()
			if ok {
				sawReading = true
				// Persisted only under the account the server itself named —
				// the same attribution rule the run-scoped poller follows.
				if persisted, _ := antigravityCapturePersist(snap); persisted {
					return liveProbeOutcomeOK
				}
			}
		}
		select {
		case <-ctx.Done():
			if sawReading {
				return liveProbeOutcomeNotSigned
			}
			return liveProbeOutcomeTimeout
		case <-exited:
			if sawReading {
				return liveProbeOutcomeNotSigned
			}
			return liveProbeOutcomeNoReading
		case <-ticker.C:
		}
	}
}

// removeDirEventually deletes a probe's temp directory. Windows keeps a killed
// process's working directory locked for a moment, so a failed delete is
// retried in the background instead of failing the probe.
func removeDirEventually(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	go func() {
		for i := 0; i < 20; i++ {
			time.Sleep(500 * time.Millisecond)
			if os.RemoveAll(dir) == nil {
				return
			}
		}
	}()
}
