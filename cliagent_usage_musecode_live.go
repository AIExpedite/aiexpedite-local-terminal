// cliagent_usage_musecode_live.go — reads Muse Code's subscription quota when
// the user clicks Refresh on the CLI Agents card, and keeps its model list.
//
// Why this exists:
//
//	Muse Code learns its quota only from a model response: Meta streams a
//	`response.subscription_usage` frame beside the output, and the host keeps
//	the latest one in memory (MSP `usage/read`, `usage/changed`). Nothing is
//	written to disk and `muse exec --json` — what the chat manager drives —
//	never emits it, so there is no rollout log to backfill from as Codex has.
//	The only way to a current figure is to ask a host that has just talked to
//	Meta.
//
//	So a Refresh click does what the Antigravity probe does: start a private
//	`muse serve` (no session log, no writes, no shell, empty temp dir), send
//	one trivial turn at the cheapest effort its model accepts, and kill the host the moment the
//	reading lands (`usage/changed`, or a `usage/read` poll — see
//	museCodeLiveProbeConverse for why both), normally within seconds. The reading is
//	normalized and cached per account in musecode_usage_live.json, which the
//	parser turns into the card's 5-hour and weekly rows.
//
// Cost: that turn is a real request against the user's Muse quota (Muse
// sends its system prompt with it). It is spent only on a click, never by a
// periodic or tab-open refresh, and at most once per
// museCodeLiveProbeMinInterval per account: a click inside that window keeps
// the cached reading.
//
// The same host answers `model/list` first (a query, no model call), which
// refreshes the on-disk model list the gather reads; the gather's own model
// probe spawns `muse serve` only when that list is missing or stale.
//
// Every write that commits a reading to the cache — this probe's, or a run's
// allowance record (cliagent_usage_musecode_run.go) — advances the cache's
// capture generation on this process's epoch and hints terminal-service
// (cliagent_usage_propagate.go), so the reading reaches the card when it lands
// rather than at the next wake. The hint carries no figures; the refresh it
// asks for is a plain gather, which never spends a Muse turn.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	museCodeLiveProbeTimeout = 20 * time.Second
	// museCodeLiveProbeMinInterval: a click this soon after the last reading
	// for the same account keeps it instead of spending another turn.
	museCodeLiveProbeMinInterval = 5 * time.Minute
	// museCodeLiveProbePrompt is sent only because the host reports usage
	// with a model response; the host is killed as soon as the reading lands.
	museCodeLiveProbePrompt = "Reply with a single period."

	museCodeUsageLiveSchemaVersion = 2
	// museCodeUsageLiveMaxAccounts bounds the per-account readings kept; the
	// oldest observation is dropped first.
	museCodeUsageLiveMaxAccounts = 16
	// museCodeUnknownAccountKey scopes a reading when nothing on disk tells
	// accounts apart (a keychain login): the device's one Muse account.
	// Deliberate limit: distinguishing two keychain logins would mean reading
	// the OS keychain secret, which this agent never does. After a keychain
	// account switch the previous reading can show (labelled with its
	// observedAt) until the next Refresh, at most five minutes later.
	museCodeUnknownAccountKey = "device"
)

// museCodeUsageLiveCache is one account's persisted, normalized reading.
// Percentages are verbatim from Meta (integers, may exceed 100); times are
// epoch ms.
type museCodeUsageLiveCache struct {
	AccountKey   string `json:"accountKey"`
	ObservedAtMs int64  `json:"observedAtMs"`
	Window       struct {
		UsedPercent        int   `json:"usedPercent"`
		WindowDurationMins int   `json:"windowDurationMins"`
		ResetsAtMs         int64 `json:"resetsAtMs"`
	} `json:"window"`
	Weekly struct {
		UsedPercent int   `json:"usedPercent"`
		ResetsAtMs  int64 `json:"resetsAtMs"`
	} `json:"weekly"`
}

// museCodeUsageLiveFile keeps one reading PER ACCOUNT, so switching accounts
// never erases another account's reading or its minimum probe interval.
type museCodeUsageLiveFile struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Readings      []museCodeUsageLiveCache `json:"readings"`
	// Generation is the capture generation (cliUsageGeneration) of the newest
	// committed write: every write that commits a reading advances it on this
	// process's epoch, and the parser publishes it with the signed-in
	// account's reading. One generation for the file, not one per account: a
	// reading is only ever written for the account signed in at that moment,
	// so an advance always describes what that account's card shows.
	// Additive, so the schema stays 2 and an older build ignores it.
	Generation *cliUsageGeneration `json:"generation,omitempty"`
}

// readingFor returns accountKey's reading, if the file holds one.
func (f museCodeUsageLiveFile) readingFor(accountKey string) (museCodeUsageLiveCache, bool) {
	for _, r := range f.Readings {
		if r.AccountKey == accountKey && r.ObservedAtMs > 0 {
			return r, true
		}
	}
	return museCodeUsageLiveCache{}, false
}

var (
	museCodeUsageLiveCacheMu sync.Mutex
	// museCodeGenerationRotated reports a committed cache write carrying this
	// process's epoch. Until then the file may hold a generation an earlier
	// process published, so the parser omits UsageGeneration and the
	// propagator holds Muse hints (providerBlockedLocked).
	museCodeGenerationRotated atomic.Bool
	// museCodeLastGeneration is the newest generation this process committed
	// (guarded by museCodeUsageLiveCacheMu), the floor the next write counts up
	// from. The file alone is not enough: a missing, unreadable or replaced
	// file reads as empty (readMuseCodeUsageLiveFileLocked), and a counter
	// restarted from it would reissue a generation terminal-service already
	// applied, whose hint it then skips as already applied.
	museCodeLastGeneration cliUsageGeneration
)

// museCodeUsageLiveCachePath: AIEXPEDITE_MUSECODE_USAGE_LIVE_CACHE overrides
// it (tests isolate from the real machine cache).
func museCodeUsageLiveCachePath() string {
	if p := os.Getenv("AIEXPEDITE_MUSECODE_USAGE_LIVE_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "musecode_usage_live.json")
}

// museCodeUsageAccountKey maps an account fingerprint onto the cache key.
func museCodeUsageAccountKey(fingerprint string) string {
	if fingerprint == "" {
		return museCodeUnknownAccountKey
	}
	return fingerprint
}

// parseMuseCodeSubscriptionUsage validates an MSP SubscriptionUsage payload
// (`usage/read`'s `usage` member, or `usage/changed` params).
func parseMuseCodeSubscriptionUsage(raw json.RawMessage) (museCodeUsageLiveCache, bool) {
	var out museCodeUsageLiveCache
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return museCodeUsageLiveCache{}, false
	}
	if out.ObservedAtMs <= 0 || out.Window.ResetsAtMs <= 0 || out.Weekly.ResetsAtMs <= 0 ||
		out.Window.WindowDurationMins <= 0 || out.Window.UsedPercent < 0 || out.Weekly.UsedPercent < 0 {
		return museCodeUsageLiveCache{}, false
	}
	return out, true
}

// readMuseCodeUsageLiveFileLocked reads the cache; a missing, unreadable or
// older-schema file reads as empty. Caller holds museCodeUsageLiveCacheMu.
func readMuseCodeUsageLiveFileLocked() museCodeUsageLiveFile {
	var file museCodeUsageLiveFile
	if !readBoundedJSONFile(museCodeUsageLiveCachePath(), &file) || file.SchemaVersion != museCodeUsageLiveSchemaVersion {
		return museCodeUsageLiveFile{SchemaVersion: museCodeUsageLiveSchemaVersion}
	}
	return file
}

// saveMuseCodeUsageLive replaces that account's reading, keeps the others, and
// hints terminal-service about the generation the write committed.
func saveMuseCodeUsageLive(entry museCodeUsageLiveCache) bool {
	generation, committed := commitMuseCodeUsageLive(entry)
	noteMuseCodeUsageCommitted(generation, committed)
	return committed
}

// commitMuseCodeUsageLive is saveMuseCodeUsageLive's cache transaction.
func commitMuseCodeUsageLive(entry museCodeUsageLiveCache) (cliUsageGeneration, bool) {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	path := museCodeUsageLiveCachePath()
	if path == "" || entry.AccountKey == "" {
		return cliUsageGeneration{}, false
	}
	return writeMuseCodeUsageLiveLocked(readMuseCodeUsageLiveFileLocked(), entry)
}

// noteMuseCodeUsageCommitted hints terminal-service about a committed
// generation. Callers run it after the cache transaction returned, outside
// museCodeUsageLiveCacheMu: the propagator never holds its own lock across a
// cache transaction, and this keeps the order one-way (saveGrokBillingLive
// does the same).
func noteMuseCodeUsageCommitted(generation cliUsageGeneration, committed bool) {
	if committed {
		noteCLIUsageObservationAdvanced(museCodeUsageProvider, generation)
	}
}

// writeMuseCodeUsageLiveLocked writes `file` with `entry` replacing its
// account's reading, newest first, capped, under the next generation. Caller
// holds museCodeUsageLiveCacheMu.
func writeMuseCodeUsageLiveLocked(file museCodeUsageLiveFile, entry museCodeUsageLiveCache) (cliUsageGeneration, bool) {
	kept := []museCodeUsageLiveCache{entry}
	for _, r := range file.Readings {
		if r.AccountKey != entry.AccountKey && r.AccountKey != "" {
			kept = append(kept, r)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].ObservedAtMs > kept[j].ObservedAtMs })
	if len(kept) > museCodeUsageLiveMaxAccounts {
		kept = kept[:museCodeUsageLiveMaxAccounts]
	}
	file.Readings = kept
	return commitMuseCodeUsageFileLocked(file)
}

// commitMuseCodeUsageFileLocked writes file under the next generation
// (museCodeNextGenerationLocked). Only a committed write moves the generation,
// this process's floor (museCodeLastGeneration) and the rotation flag, so a
// write that failed can never be hinted or published. Caller holds
// museCodeUsageLiveCacheMu.
func commitMuseCodeUsageFileLocked(file museCodeUsageLiveFile) (cliUsageGeneration, bool) {
	next := museCodeNextGenerationLocked(file)
	file.Generation = &next
	if !writeJSONFileAtomic(museCodeUsageLiveCachePath(), file) {
		return cliUsageGeneration{}, false
	}
	museCodeLastGeneration = next
	museCodeGenerationRotated.Store(true)
	return next, true
}

// museCodeNextGenerationLocked is the generation the next committed write
// carries: this process's epoch, one past the higher of the file's counter
// (only when the file is on this epoch — counters are never compared across
// epochs) and the last one this process committed. Caller holds
// museCodeUsageLiveCacheMu.
func museCodeNextGenerationLocked(file museCodeUsageLiveFile) cliUsageGeneration {
	epoch := codexProcessGenerationEpoch.Load()
	var counter int64
	if file.Generation != nil && file.Generation.Epoch == epoch {
		counter = file.Generation.Counter
	}
	if museCodeLastGeneration.Epoch == epoch && museCodeLastGeneration.Counter > counter {
		counter = museCodeLastGeneration.Counter
	}
	return cliUsageGeneration{Epoch: epoch, Counter: counter + 1}
}

// museCodeUsageLiveView is one read of the cache for an account.
type museCodeUsageLiveView struct {
	reading museCodeUsageLiveCache
	// generation is the file's committed generation (nil when none).
	generation *cliUsageGeneration
}

// loadMuseCodeUsageLive returns the cached reading for that account, if any.
func loadMuseCodeUsageLive(accountKey string) (museCodeUsageLiveCache, bool) {
	view, ok := loadMuseCodeUsageLiveView(accountKey)
	return view.reading, ok
}

// loadMuseCodeUsageLiveView is loadMuseCodeUsageLive with the file's
// generation, read under the same lock as the reading it versions.
func loadMuseCodeUsageLiveView(accountKey string) (museCodeUsageLiveView, bool) {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	file := readMuseCodeUsageLiveFileLocked()
	reading, ok := file.readingFor(accountKey)
	if !ok {
		return museCodeUsageLiveView{}, false
	}
	view := museCodeUsageLiveView{reading: reading}
	if file.Generation != nil {
		g := *file.Generation
		view.generation = &g
	}
	return view, true
}

// recoverMuseCodeUsageGeneration is Muse Code's startup recovery
// (startCLIUsagePropagator). A reading committed just before an update
// hand-off — whose hint died with the previous process inside its debounce —
// is moved onto this process's epoch in one write and hinted once. Only the
// signed-in account's reading, while it still renders a number, is
// recovered; the backend skips a generation it already applied. Startup asks
// once: a write that does not commit leaves the reading to the next refresh,
// as before Muse Code sent hints.
func recoverMuseCodeUsageGeneration() {
	if g := museCodeRecoveryGeneration(time.Now()); g != nil {
		noteCLIUsageObservationAdvanced(museCodeUsageProvider, *g)
	}
}

// museCodeRecoveryGeneration rotates the cache onto this process's epoch when
// it holds a reading for the account signed in now that renders a number at
// now, and returns the generation the cache carries afterwards — nil when
// there is nothing to recover, the write did not commit, or this process
// already committed (and hinted) the file's generation itself.
func museCodeRecoveryGeneration(now time.Time) *cliUsageGeneration {
	accountKey := museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	if museCodeUsageLiveCachePath() == "" {
		return nil
	}
	file := readMuseCodeUsageLiveFileLocked()
	reading, ok := file.readingFor(accountKey)
	if !ok || !museCodeUsageMetricsNumeric(museCodeUsageMetrics(reading, true, now)) {
		return nil
	}
	if file.Generation != nil && file.Generation.Epoch == codexProcessGenerationEpoch.Load() {
		return nil
	}
	g, committed := commitMuseCodeUsageFileLocked(file)
	if !committed {
		logMuseCodeUsage("recovery_write_failed")
		return nil
	}
	return &g
}

// museCodeUsageMetricsNumeric reports whether any row renders a number.
func museCodeUsageMetricsNumeric(metrics []cliAgentUsageMetric) bool {
	for _, m := range metrics {
		if !m.Unknown && m.Consumed != nil {
			return true
		}
	}
	return false
}

// logMuseCodeUsage prints a fixed label — never a path, account or figure.
func logMuseCodeUsage(label string) {
	fmt.Printf("%s[cli-usage] musecode usage: %s%s\n", colorYellow, label, colorReset)
}

// resetMuseCodeUsageGeneration forgets this process's generation state, as a
// new process starts with none (tests, through resetCLIUsagePropagator).
func resetMuseCodeUsageGeneration() {
	museCodeUsageLiveCacheMu.Lock()
	museCodeLastGeneration = cliUsageGeneration{}
	museCodeUsageLiveCacheMu.Unlock()
	museCodeGenerationRotated.Store(false)
}

// probeMuseCodeUsageLive runs one bounded quota read for the signed-in Muse
// account (see the file comment). Outcomes are the shared liveProbeOutcome*
// codes.
func probeMuseCodeUsageLive(parent context.Context, agent detectedCLIAgent, now func() time.Time) string {
	launcher := strings.TrimSpace(agent.Path)
	if launcher == "" {
		launcher = resolveMuseCodeExecutable()
	}
	fingerprint := currentMuseCodeAccountFingerprint()
	accountKey := museCodeUsageAccountKey(fingerprint)
	// The quota cooldown does not apply while Muse Code is marked signed out:
	// Refresh is how a keychain re-login proves itself, and a refused probe
	// fails before any model call, so it spends nothing (musecode_auth_signal.go).
	if last, ok := loadMuseCodeUsageLive(accountKey); ok && !museCodeSignedOutMarked() {
		if since := now().Sub(time.UnixMilli(last.ObservedAtMs)); since >= 0 && since < museCodeLiveProbeMinInterval {
			return liveProbeOutcomeCooldown
		}
	}

	ctx, cancel := context.WithTimeout(parent, museCodeLiveProbeTimeout)
	defer cancel()

	readings := make(chan json.RawMessage, 1)
	onNotify := func(method string, params json.RawMessage) {
		if method != "usage/changed" {
			return
		}
		select {
		case readings <- params:
		default:
		}
	}
	client, err := startMuseCodeMSPFn(ctx, launcher, onNotify)
	if err != nil {
		if ctx.Err() != nil {
			return liveProbeOutcomeTimeout
		}
		return liveProbeOutcomeSpawnFailed
	}
	defer client.Close()

	// The model list rides the same host: a query, no model call. It also
	// names the efforts the session's model accepts, so the turn never asks
	// for one the model would reject.
	discovery, listed := museCodeListModels(ctx, client)
	if listed {
		if museCodeAccountUnchanged(accountKey) {
			saveMuseCodeModelsCache(agent.Version, accountKey, discovery, now())
		}
	}
	home, _ := os.UserHomeDir()
	effort := museCodeProbeEffort(discovery, listed, readMuseCodeDefaultModel(home, os.Getenv))

	return museCodeLiveProbeConverse(ctx, client, readings, museCodeProbeTurn{
		AccountKey: accountKey,
		Effort:     effort,
		// Checked immediately before the reading is written: a login switch
		// while the turn ran makes the reading unattributable, and it must
		// never be cached (or start the cooldown) under the old account.
		SameAccount: func() bool { return museCodeAccountUnchanged(accountKey) },
	})
}

// museCodeProbeTurn is what the converse step needs beyond the host.
type museCodeProbeTurn struct {
	AccountKey string
	// Effort is the reasoning effort for the probe turn; "" omits the field
	// and lets the host use its default.
	Effort string
	// SameAccount reports whether the account the probe started under is
	// still signed in. nil means "not checked".
	SameAccount func() bool
}

// museCodeProbeEffort picks the cheapest effort the probe turn's model
// accepts. The session runs settings.json's model when the catalog lists it,
// else the catalog default; with neither known, the cheapest effort EVERY
// listed model accepts. "" (omit the field) when nothing is known — an effort
// the model does not advertise can make the host reject the turn.
func museCodeProbeEffort(discovery cliAgentModelDiscovery, listed bool, settingsModel string) string {
	if !listed || len(discovery.Models) == 0 {
		return ""
	}
	find := func(id string) *cliAgentModelDetail {
		for i := range discovery.Models {
			if id != "" && discovery.Models[i].ID == id {
				return &discovery.Models[i]
			}
		}
		return nil
	}
	model := find(settingsModel)
	if model == nil {
		model = find(discovery.DefaultModel)
	}
	if model != nil {
		if len(model.Efforts) == 0 {
			return ""
		}
		return model.Efforts[0] // ordered low → high (appendEffort)
	}
	var common []string
	for i, m := range discovery.Models {
		if i == 0 {
			common = append(common, m.Efforts...)
			continue
		}
		kept := common[:0]
		for _, e := range common {
			if containsString(m.Efforts, e) {
				kept = append(kept, e)
			}
		}
		common = kept
	}
	if len(common) == 0 {
		return ""
	}
	return common[0]
}

// museCodeLiveProbeConverse drives usage/read → (session/start → turn/start →
// first usage/changed) over a started host. Split out so tests drive it with
// pipes instead of a real Muse Code.
func museCodeLiveProbeConverse(ctx context.Context, client *museCodeMSPClient, readings <-chan json.RawMessage, turn museCodeProbeTurn) string {
	capture := func(raw json.RawMessage) string {
		reading, ok := parseMuseCodeSubscriptionUsage(raw)
		if !ok {
			return liveProbeOutcomeNoReading
		}
		if turn.SameAccount != nil && !turn.SameAccount() {
			return liveProbeOutcomeAccountChanged
		}
		reading.AccountKey = turn.AccountKey
		if !saveMuseCodeUsageLive(reading) {
			return liveProbeOutcomeNoReading
		}
		// Meta answered a turn: signed in (a keychain login clears its
		// signed-out mark this way — musecode_auth_signal.go).
		recordMuseCodeSignedIn(time.Now())
		return liveProbeOutcomeOK
	}

	// A host that already talked to Meta answers without a turn. A fresh one
	// answers `{}` (MSP: absence, not an error).
	raw, err := client.Call(ctx, "usage/read", map[string]any{})
	if err != nil {
		return museCodeProbeErrorOutcome(ctx)
	}
	var read struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &read) == nil && len(read.Usage) > 0 && string(read.Usage) != "null" {
		return capture(read.Usage)
	}

	started, err := client.Call(ctx, "session/start", map[string]any{
		"commandId":     newMuseCodeCommandID(),
		"workspaceRoot": client.tmpDir,
	})
	if err != nil {
		return museCodeProbeErrorOutcome(ctx)
	}
	var session struct {
		Session struct {
			SessionID string `json:"sessionId"`
		} `json:"session"`
	}
	if json.Unmarshal(started, &session) != nil || session.Session.SessionID == "" {
		return liveProbeOutcomeRPCError
	}
	params := map[string]any{
		"commandId": newMuseCodeCommandID(),
		"sessionId": session.Session.SessionID,
		"input":     []map[string]any{{"type": "text", "text": museCodeLiveProbePrompt}},
	}
	if turn.Effort != "" {
		params["reasoningEffort"] = turn.Effort
	}
	if _, err := client.Call(ctx, "turn/start", params); err != nil {
		return museCodeProbeErrorOutcome(ctx)
	}

	// Wait for the reading on BOTH routes: the `usage/changed` notification,
	// and a `usage/read` poll. The poll is not redundant. On Windows, with
	// stdio on anonymous pipes (what os/exec creates), Muse Code 1.4.0 holds
	// its outbound notifications until the next inbound frame arrives: a
	// client that only waits never sees `usage/changed` (observed 2026-09-29;
	// a Node client on named pipes did). Each poll is both a free query and
	// the frame that releases anything queued.
	poll := time.NewTicker(museCodeUsagePollInterval)
	defer poll.Stop()
	for {
		select {
		case params := <-readings:
			return capture(params)
		case <-poll.C:
			raw, err := client.Call(ctx, "usage/read", map[string]any{})
			if err != nil {
				if ctx.Err() != nil {
					return liveProbeOutcomeTimeout
				}
				continue
			}
			read.Usage = nil
			if json.Unmarshal(raw, &read) == nil && len(read.Usage) > 0 && string(read.Usage) != "null" {
				return capture(read.Usage)
			}
		case <-client.Done():
			return liveProbeOutcomeNoReading
		case <-ctx.Done():
			return liveProbeOutcomeTimeout
		}
	}
}

// museCodeUsagePollInterval paces the `usage/read` polls after the turn
// starts (see museCodeLiveProbeConverse). A var so tests can shorten it.
var museCodeUsagePollInterval = time.Second

func museCodeProbeErrorOutcome(ctx context.Context) string {
	if ctx.Err() != nil {
		return liveProbeOutcomeTimeout
	}
	return liveProbeOutcomeRPCError
}

/* --------------------------------------------------------------------------
   Model list
   -------------------------------------------------------------------------- */

// museCodeModelsCacheMaxAge: the catalog changes with Meta's releases, not
// minutes; past this the gather asks the host again.
const museCodeModelsCacheMaxAge = 6 * time.Hour

type museCodeModelsCacheFile struct {
	Version string `json:"version"`
	// AccountKey: a provider catalog can differ by account/entitlement, so a
	// list is only reused for the account that fetched it.
	AccountKey   string                `json:"accountKey"`
	FetchedAtMs  int64                 `json:"fetchedAtMs"`
	Exhaustive   bool                  `json:"exhaustive"`
	DefaultModel string                `json:"defaultModel,omitempty"`
	Models       []cliAgentModelDetail `json:"models"`
}

var museCodeModelsCacheMu sync.Mutex

// museCodeModelsCachePath: AIEXPEDITE_MUSECODE_MODELS_CACHE overrides it.
func museCodeModelsCachePath() string {
	if p := os.Getenv("AIEXPEDITE_MUSECODE_MODELS_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "musecode_models.json")
}

func saveMuseCodeModelsCache(version, accountKey string, discovery cliAgentModelDiscovery, now time.Time) {
	if len(discovery.Models) == 0 {
		return
	}
	museCodeModelsCacheMu.Lock()
	defer museCodeModelsCacheMu.Unlock()
	_ = writeJSONFileAtomic(museCodeModelsCachePath(), museCodeModelsCacheFile{
		Version:      version,
		AccountKey:   accountKey,
		FetchedAtMs:  now.UnixMilli(),
		Exhaustive:   discovery.Exhaustive,
		DefaultModel: discovery.DefaultModel,
		Models:       boundedModelDetails(discovery.Models),
	})
}

// loadMuseCodeModelsCache returns the saved list for this installed version
// and account, and whether it is still fresh.
func loadMuseCodeModelsCache(version, accountKey string, now time.Time) (discovery cliAgentModelDiscovery, fresh, ok bool) {
	museCodeModelsCacheMu.Lock()
	defer museCodeModelsCacheMu.Unlock()
	var file museCodeModelsCacheFile
	if !readBoundedJSONFile(museCodeModelsCachePath(), &file) || len(file.Models) == 0 ||
		file.Version != version || file.AccountKey != accountKey {
		return cliAgentModelDiscovery{}, false, false
	}
	age := now.Sub(time.UnixMilli(file.FetchedAtMs))
	return cliAgentModelDiscovery{
		Models:       file.Models,
		Exhaustive:   file.Exhaustive,
		DefaultModel: file.DefaultModel,
	}, age >= 0 && age < museCodeModelsCacheMaxAge, true
}

// discoverMuseCodeModels answers from the saved list while it is fresh, asks
// a private `muse serve` otherwise, and falls back to the saved list (same
// installed version) when the host cannot answer inside the caller's budget.
func discoverMuseCodeModels(ctx context.Context, detected detectedCLIAgent) (cliAgentModelDiscovery, bool) {
	now := time.Now()
	accountKey := museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())
	saved, fresh, haveSaved := loadMuseCodeModelsCache(detected.Version, accountKey, now)
	if haveSaved && fresh {
		return saved, true
	}
	launcher := strings.TrimSpace(detected.Path)
	if launcher == "" {
		launcher = resolveMuseCodeExecutable()
	}
	if client, err := startMuseCodeMSPFn(ctx, launcher, nil); err == nil {
		discovery, ok := museCodeListModels(ctx, client)
		client.Close()
		if ok {
			// Only an answer for the account the host was started under, still
			// signed in, is kept or returned. After a switch the answer is
			// inconclusive: returning it would let the shared in-memory cache
			// file it under the key computed before the switch.
			if !museCodeAccountUnchanged(accountKey) {
				return cliAgentModelDiscovery{}, false
			}
			saveMuseCodeModelsCache(detected.Version, accountKey, discovery, now)
			return discovery, true
		}
	}
	return saved, haveSaved
}
