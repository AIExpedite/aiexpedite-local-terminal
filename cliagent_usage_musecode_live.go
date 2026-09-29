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
//	one trivial turn at minimal effort, and kill the host the moment the
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
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	museCodeUsageLiveSchemaVersion = 1
	// museCodeUnknownAccountKey scopes a reading when no identity is on disk
	// (a keychain login): the device's one Muse account, whoever it is.
	museCodeUnknownAccountKey = "device"
)

// museCodeUsageLiveCache is the persisted, normalized reading. Percentages are
// verbatim from Meta (integers, may exceed 100); times are epoch ms.
type museCodeUsageLiveCache struct {
	SchemaVersion int    `json:"schemaVersion"`
	AccountKey    string `json:"accountKey"`
	ObservedAtMs  int64  `json:"observedAtMs"`
	Window        struct {
		UsedPercent        int   `json:"usedPercent"`
		WindowDurationMins int   `json:"windowDurationMins"`
		ResetsAtMs         int64 `json:"resetsAtMs"`
	} `json:"window"`
	Weekly struct {
		UsedPercent int   `json:"usedPercent"`
		ResetsAtMs  int64 `json:"resetsAtMs"`
	} `json:"weekly"`
}

var museCodeUsageLiveCacheMu sync.Mutex

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

func saveMuseCodeUsageLive(entry museCodeUsageLiveCache) bool {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	path := museCodeUsageLiveCachePath()
	if path == "" || entry.AccountKey == "" {
		return false
	}
	entry.SchemaVersion = museCodeUsageLiveSchemaVersion
	return writeMuseCodeJSONAtomic(path, entry)
}

// loadMuseCodeUsageLive returns the cached reading for that account, if any.
func loadMuseCodeUsageLive(accountKey string) (museCodeUsageLiveCache, bool) {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	var entry museCodeUsageLiveCache
	if !readBoundedJSONFile(museCodeUsageLiveCachePath(), &entry) {
		return museCodeUsageLiveCache{}, false
	}
	if entry.SchemaVersion != museCodeUsageLiveSchemaVersion || entry.AccountKey != accountKey || entry.ObservedAtMs <= 0 {
		return museCodeUsageLiveCache{}, false
	}
	return entry, true
}

func writeMuseCodeJSONAtomic(path string, value any) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
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
	if last, ok := loadMuseCodeUsageLive(accountKey); ok {
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

	// The model list rides the same host: a query, no model call.
	if discovery, ok := museCodeListModels(ctx, client); ok {
		saveMuseCodeModelsCache(agent.Version, discovery, now())
	}

	outcome := museCodeLiveProbeConverse(ctx, client, readings, accountKey)
	if outcome == liveProbeOutcomeOK && currentMuseCodeAccountFingerprint() != fingerprint {
		// Someone signed in as another account while the turn ran; the
		// reading cannot be attributed to the account signed in now.
		return liveProbeOutcomeAccountChanged
	}
	return outcome
}

// museCodeLiveProbeConverse drives usage/read → (session/start → turn/start →
// first usage/changed) over a started host. Split out so tests drive it with
// pipes instead of a real Muse Code.
func museCodeLiveProbeConverse(ctx context.Context, client *museCodeMSPClient, readings <-chan json.RawMessage, accountKey string) string {
	capture := func(raw json.RawMessage) string {
		reading, ok := parseMuseCodeSubscriptionUsage(raw)
		if !ok {
			return liveProbeOutcomeNoReading
		}
		reading.AccountKey = accountKey
		if !saveMuseCodeUsageLive(reading) {
			return liveProbeOutcomeNoReading
		}
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
	if _, err := client.Call(ctx, "turn/start", map[string]any{
		"commandId":       newMuseCodeCommandID(),
		"sessionId":       session.Session.SessionID,
		"input":           []map[string]any{{"type": "text", "text": museCodeLiveProbePrompt}},
		"reasoningEffort": "minimal",
	}); err != nil {
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
	Version      string                `json:"version"`
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

func saveMuseCodeModelsCache(version string, discovery cliAgentModelDiscovery, now time.Time) {
	if len(discovery.Models) == 0 {
		return
	}
	museCodeModelsCacheMu.Lock()
	defer museCodeModelsCacheMu.Unlock()
	_ = writeMuseCodeJSONAtomic(museCodeModelsCachePath(), museCodeModelsCacheFile{
		Version:      version,
		FetchedAtMs:  now.UnixMilli(),
		Exhaustive:   discovery.Exhaustive,
		DefaultModel: discovery.DefaultModel,
		Models:       boundedModelDetails(discovery.Models),
	})
}

// loadMuseCodeModelsCache returns the saved list for this installed version,
// and whether it is still fresh.
func loadMuseCodeModelsCache(version string, now time.Time) (discovery cliAgentModelDiscovery, fresh, ok bool) {
	museCodeModelsCacheMu.Lock()
	defer museCodeModelsCacheMu.Unlock()
	var file museCodeModelsCacheFile
	if !readBoundedJSONFile(museCodeModelsCachePath(), &file) || len(file.Models) == 0 || file.Version != version {
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
	saved, fresh, haveSaved := loadMuseCodeModelsCache(detected.Version, now)
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
			saveMuseCodeModelsCache(detected.Version, discovery, now)
			return discovery, true
		}
	}
	return saved, haveSaved
}
