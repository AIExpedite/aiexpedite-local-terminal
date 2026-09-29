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

// readMuseCodeUsageLiveFileLocked reads the cache; a missing, unreadable or
// older-schema file reads as empty. Caller holds museCodeUsageLiveCacheMu.
func readMuseCodeUsageLiveFileLocked() museCodeUsageLiveFile {
	var file museCodeUsageLiveFile
	if !readBoundedJSONFile(museCodeUsageLiveCachePath(), &file) || file.SchemaVersion != museCodeUsageLiveSchemaVersion {
		return museCodeUsageLiveFile{SchemaVersion: museCodeUsageLiveSchemaVersion}
	}
	return file
}

// saveMuseCodeUsageLive replaces that account's reading and keeps the others.
func saveMuseCodeUsageLive(entry museCodeUsageLiveCache) bool {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	path := museCodeUsageLiveCachePath()
	if path == "" || entry.AccountKey == "" {
		return false
	}
	file := readMuseCodeUsageLiveFileLocked()
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
	return writeMuseCodeJSONAtomic(path, file)
}

// loadMuseCodeUsageLive returns the cached reading for that account, if any.
func loadMuseCodeUsageLive(accountKey string) (museCodeUsageLiveCache, bool) {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	for _, r := range readMuseCodeUsageLiveFileLocked().Readings {
		if r.AccountKey == accountKey && r.ObservedAtMs > 0 {
			return r, true
		}
	}
	return museCodeUsageLiveCache{}, false
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

	// The model list rides the same host: a query, no model call. It also
	// names the efforts the session's model accepts, so the turn never asks
	// for one the model would reject.
	discovery, listed := museCodeListModels(ctx, client)
	if listed {
		saveMuseCodeModelsCache(agent.Version, accountKey, discovery, now())
	}
	home, _ := os.UserHomeDir()
	effort := museCodeProbeEffort(discovery, listed, readMuseCodeDefaultModel(home, os.Getenv))

	return museCodeLiveProbeConverse(ctx, client, readings, museCodeProbeTurn{
		AccountKey: accountKey,
		Effort:     effort,
		// Checked immediately before the reading is written: a login switch
		// while the turn ran makes the reading unattributable, and it must
		// never be cached (or start the cooldown) under the old account.
		SameAccount: func() bool { return currentMuseCodeAccountFingerprint() == fingerprint },
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
	_ = writeMuseCodeJSONAtomic(museCodeModelsCachePath(), museCodeModelsCacheFile{
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
			saveMuseCodeModelsCache(detected.Version, accountKey, discovery, now)
			return discovery, true
		}
	}
	return saved, haveSaved
}
