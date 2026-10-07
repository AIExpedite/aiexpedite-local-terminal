// cliagent_usage_propagate.go — tells terminal-service that a fresh usage
// reading is waiting on this device.
//
// Why this exists:
//
//	The device publishes usage only in answer to `__cli_usage_refresh__`, and
//	terminal-service wakes those refreshes on session start, command dispatch,
//	a Refresh click and a few other backend events. A `codex` or `claude` the
//	user runs in their own shell, the agent-initiated `__cli_smoke__`, an
//	interactive Claude status-line render and the post-run debt ladders (which
//	land their reading well after the session-end wake) produce a reading the
//	backend never asks for — the card keeps a stale observation, or none, until
//	the six-hourly machine-info gather.
//
// When a provider's usage cache commits a new capture generation — Codex on an
// ADVANCED observation for the active account (mergeCodexRateLimitCacheObserved),
// Claude Code on a numeric row winning its newer-wins merge
// (mergeClaudeRateLimitCacheLocked) — the propagator sends one signed hint,
// `POST /device/:agentId/cli-usage/observed`, naming the provider and the
// committed generation. terminal-service answers it with one ordinary refresh —
// at most one per agent per 4 minutes, none when it already applied that
// provider's generation, and without moving an Idle device to Active. The hint
// carries no metric values: numbers travel only inside the signed refresh
// receipt.
//
// Device-side bounds:
//   - one pending slot PER PROVIDER, carrying that provider's newest
//     generation; a newer generation resets only its own provider's debounce
//     and two-hint budget;
//   - a 15 s trailing debounce collapses bursts;
//   - hints are at least 5 minutes apart for the whole device; at each spacing
//     boundary the provider whose hint fell due first goes (provider id breaks
//     a tie), so continuous activity on one provider cannot starve another;
//   - after a hint is sent the generation stays pending for exactly ONE
//     follow-up at a later spacing boundary (the backend makes it a no-op once
//     the generation is applied), then it is dropped — at most two hints per
//     generation;
//   - a generation a signed refresh receipt already carried is not hinted at
//     once: a successful publish leaves only ONE confirmation, due no sooner
//     than the spacing after the publish (beyond the backend's 4-minute
//     cooldown); a failed signing or publish releases it into the normal
//     initial-plus-follow-up lifecycle;
//   - nothing is sent while offline, draining, shutting down, unregistered, or
//     (Codex only) before this process's Codex generation epoch is on disk; the
//     generation is kept and re-checked at the next boundary.
//
// Discovering commits made elsewhere. The Claude status-line hook commits from
// short-lived processes of its own, so the resident agent learns of those
// commits by reading the cache: every Claude usage read outside a signed refresh
// notes a generation newer than the last one this process saw, and a fallback
// timer reads the small cache at most once a minute — only while Claude is
// detected, the cache exists, and the agent is online and not draining,
// shutting down or stopped.
//
// Pending state is in memory only. A reading captured just before an update
// handoff is recovered by the next process: once its Codex epoch rotation
// commits (codexRotateGenerationEpoch), a Codex cache that still renders a
// numeric metric — or holds a committed clear — is scheduled through the same
// gates; Claude's generation is persisted with its cache, so startup re-reads it
// once (versioning a legacy numeric snapshot first) and schedules it the same
// way. The backend skips a generation it already applied.
//
// Logs are fixed labels only (`[cli-usage] usage hint: <label>`) — never a
// path, account, fingerprint, id or response body.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Vars so tests can pin them small.
var (
	cliUsageHintDebounce          = 15 * time.Second
	cliUsageHintSpacing           = 5 * time.Minute
	cliUsageRotationFirstRetry    = 15 * time.Second
	cliUsageRotationRetryPeriod   = 60 * time.Second
	cliUsageHintHTTPTimeout       = 2 * time.Second
	cliUsageClaudeFallbackPeriod  = 60 * time.Second
	cliUsageClaudeFallbackEnabled = true
)

// cliUsageObservedHint is the signed body of POST /device/:agentId/cli-usage/observed.
// Every field except the signature is covered by it.
type cliUsageObservedHint struct {
	Timestamp       int64  `json:"timestamp"`
	Signature       string `json:"signature"`
	Provider        string `json:"provider"`
	GenerationEpoch int64  `json:"generationEpoch"`
	Generation      int64  `json:"generation"`
}

// buildCLIUsageObservedSignedMessage mirrors terminal-service's
// buildCliUsageObservedSignedMessage byte for byte:
// `${agentId}:${timestamp}:${provider}:${generationEpoch}:${generation}`.
func buildCLIUsageObservedSignedMessage(agentID string, timestamp int64, provider string, generation cliUsageGeneration) string {
	return fmt.Sprintf("%s:%d:%s:%d:%d", agentID, timestamp, provider, generation.Epoch, generation.Counter)
}

// sendCLIUsageObservedHint makes ONE attempt and reports the HTTP status (0 on
// a transport error). A var so tests never reach the network.
var sendCLIUsageObservedHint = func(ctx context.Context, url string, hint cliUsageObservedHint) int {
	body, err := json.Marshal(hint)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, cliUsageHintHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: cliUsageHintHTTPTimeout}).Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// cliUsagePendingHint is one provider's pending generation.
type cliUsagePendingHint struct {
	generation cliUsageGeneration
	// notedAt starts a new generation's trailing debounce.
	notedAt time.Time
	// followUp: the next send is this generation's last one — the follow-up
	// after its first hint, or the confirmation of a published receipt — due
	// at notBefore (the first hint's send instant, or the spacing after the
	// publish) rather than after the debounce.
	followUp  bool
	notBefore time.Time
}

// due is when h may go, before the device-wide spacing.
func (h *cliUsagePendingHint) due() time.Time {
	if h.followUp {
		return h.notBefore
	}
	return h.notedAt.Add(cliUsageHintDebounce)
}

// cliUsagePropagatorState is guarded by mu. The propagator never holds mu while
// doing I/O or a cache transaction: noteCLIUsageGenerationRotated is called from
// INSIDE a cache transaction, so the reverse order would deadlock.
type cliUsagePropagatorState struct {
	mu      sync.Mutex
	cfg     *Config
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	timer   *time.Timer
	// timerGen invalidates a callback whose timer was replaced after it fired.
	timerGen uint64

	pending map[string]*cliUsagePendingHint
	// seen is the newest generation per provider this process has noted,
	// reserved for a receipt or recovered — what a cache read must beat to
	// count as a discovery.
	seen       map[string]cliUsageGeneration
	lastSentAt time.Time

	rotated         bool
	rotationRetryAt time.Time
	recoveryDue     bool
	// claudeRecoveryDue: startup has not yet re-read the Claude cache.
	claudeRecoveryDue bool
	fallbackTimer     *time.Timer
	// fallbackGen invalidates a fallback callback whose timer was replaced.
	fallbackGen uint64
	// rotating tracks the startup rotation goroutine so a test reset can wait
	// for it instead of letting it write into the next test's cache.
	rotating sync.WaitGroup
}

var cliUsagePropagator = &cliUsagePropagatorState{}

// startCLIUsagePropagator is called from StartAgent. It rotates the Codex cache
// onto this process's epoch (retrying a refused write on the timer) and, once
// that commits, schedules the Codex startup recovery observation; it re-reads
// the Claude cache once and starts the Claude fallback timer. Observations
// noted before it runs were recorded and are sent through the normal schedule.
func startCLIUsagePropagator(cfg *Config) {
	p := cliUsagePropagator
	p.mu.Lock()
	if p.cfg != nil || p.stopped {
		p.mu.Unlock()
		return
	}
	p.cfg = cfg
	p.ctx, p.cancel = context.WithCancel(context.Background())
	if p.rotated {
		// A capture committed during boot already rotated the epoch.
		p.recoveryDue = true
	}
	p.claudeRecoveryDue = true
	p.armLocked(0)
	p.armFallbackLocked()
	p.rotating.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.rotating.Done()
		p.rotate(cliUsageRotationFirstRetry)
	}()
}

// stopCLIUsagePropagator runs from gracefulShutdown. No hint is sent after it.
func stopCLIUsagePropagator() {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.fallbackTimer != nil {
		p.fallbackTimer.Stop()
	}
	p.timerGen++
	p.fallbackGen++
	if p.cancel != nil {
		p.cancel()
	}
	if len(p.pending) > 0 {
		logCLIUsageHint("dropped")
		p.pending = nil
	}
}

// noteCLIUsageObservationAdvanced records a committed, advanced generation for
// provider. Before the propagator starts it only records it, so a frame
// captured during boot is not lost.
func noteCLIUsageObservationAdvanced(provider string, generation cliUsageGeneration) {
	if generation.Epoch <= 0 || generation.Counter <= 0 {
		return
	}
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.markSeenLocked(provider, generation)
	if h := p.pending[provider]; h != nil && h.generation.covers(generation) {
		return // not newer than what is already pending
	}
	if p.pending == nil {
		p.pending = map[string]*cliUsagePendingHint{}
	}
	p.pending[provider] = &cliUsagePendingHint{generation: generation, notedAt: time.Now()}
	if p.cfg != nil {
		if d := p.sendDelayLocked(p.pending[provider]); d > cliUsageHintDebounce {
			// Throttled by the spacing: kept, carrying the newest generation.
			logCLIUsageHint("deferred")
		}
		p.armNextLocked()
	}
}

// noteCLIUsageGenerationObserved records a generation read from (or committed
// to) a usage cache: a discovery only when it is newer than the last one this
// process saw for the provider, so repeated reads of one commit stay quiet.
func noteCLIUsageGenerationObserved(provider string, generation cliUsageGeneration) {
	p := cliUsagePropagator
	p.mu.Lock()
	seen, ok := p.seen[provider]
	p.mu.Unlock()
	if ok && seen.covers(generation) {
		return
	}
	noteCLIUsageObservationAdvanced(provider, generation)
}

func (p *cliUsagePropagatorState) markSeenLocked(provider string, generation cliUsageGeneration) {
	if seen, ok := p.seen[provider]; ok && seen.covers(generation) {
		return
	}
	if p.seen == nil {
		p.seen = map[string]cliUsageGeneration{}
	}
	p.seen[provider] = generation
}

// cliUsageReceiptReservation collects the generations a signed refresh
// discovered while gathering, so they travel in that receipt instead of
// queueing an immediate hint. settleCLIUsageReceipt resolves it once the
// receipt is published, or not.
type cliUsageReceiptReservation struct {
	mu          sync.Mutex
	generations map[string]cliUsageGeneration
}

type cliUsageReceiptReservationKey struct{}

// withCLIUsageReceiptReservation marks a usage gather as producing a signed
// refresh receipt.
func withCLIUsageReceiptReservation(ctx context.Context) (context.Context, *cliUsageReceiptReservation) {
	r := &cliUsageReceiptReservation{generations: map[string]cliUsageGeneration{}}
	return context.WithValue(ctx, cliUsageReceiptReservationKey{}, r), r
}

// observeCLIUsageGeneration is what a provider parser calls with the generation
// its published metrics were read from (nil when unversioned). Inside a signed
// refresh a NEW generation is reserved for that receipt; outside one it is
// noted as a discovery.
func observeCLIUsageGeneration(ctx context.Context, provider string, generation *cliUsageGeneration) {
	if generation == nil || generation.Epoch <= 0 || generation.Counter <= 0 {
		return
	}
	r, _ := ctx.Value(cliUsageReceiptReservationKey{}).(*cliUsageReceiptReservation)
	if r == nil {
		noteCLIUsageGenerationObserved(provider, *generation)
		return
	}
	p := cliUsagePropagator
	p.mu.Lock()
	seen, ok := p.seen[provider]
	newer := !ok || !seen.covers(*generation)
	if newer {
		p.markSeenLocked(provider, *generation)
	}
	p.mu.Unlock()
	if !newer {
		return // already noted (pending, or its hints went out): nothing to reserve
	}
	r.mu.Lock()
	r.generations[provider] = *generation
	r.mu.Unlock()
}

// settleCLIUsageReceipt resolves a signed refresh's reservation. published
// reports that the receipt reached Pub/Sub, and agents is what it carried.
//
// Published: each carried generation that was reserved, or that is the
// provider's pending generation (or newer), becomes ONE confirmation due no
// sooner than the spacing after the publish — the backend applies the receipt,
// and the confirmation is a no-op unless it rejected it. A provider whose
// pending generation is newer than the receipt's keeps its own lifecycle.
// Not published (signing or publish failure): every reservation is released
// into the normal initial-plus-follow-up lifecycle.
func settleCLIUsageReceipt(r *cliUsageReceiptReservation, published bool, agents []cliAgentUsage) {
	if r == nil {
		return
	}
	r.mu.Lock()
	reserved := r.generations
	r.generations = map[string]cliUsageGeneration{}
	r.mu.Unlock()

	if !published {
		for provider, generation := range reserved {
			releaseCLIUsageReservation(provider, generation)
		}
		if len(reserved) > 0 {
			logCLIUsageHint("receipt_released")
		}
		return
	}
	carried := map[string]cliUsageGeneration{}
	p := cliUsagePropagator
	p.mu.Lock()
	confirmed := false
	if !p.stopped {
		now := time.Now()
		for _, agent := range agents {
			if agent.UsageGeneration == nil {
				continue
			}
			provider, generation := agent.Provider, *agent.UsageGeneration
			carried[provider] = generation
			h := p.pending[provider]
			if h != nil && !generation.covers(h.generation) {
				continue // a newer generation is pending: it keeps its own lifecycle
			}
			if res, ok := reserved[provider]; h == nil && (!ok || !generation.covers(res)) {
				continue // nothing undelivered for this provider
			}
			if p.pending == nil {
				p.pending = map[string]*cliUsagePendingHint{}
			}
			p.pending[provider] = &cliUsagePendingHint{generation: generation, followUp: true, notBefore: now.Add(cliUsageHintSpacing)}
			confirmed = true
		}
		if confirmed && p.cfg != nil {
			p.armNextLocked()
		}
	}
	p.mu.Unlock()
	if confirmed {
		logCLIUsageHint("confirmation_pending")
	}
	// A reservation the receipt did not carry — its provider failed after
	// reading, or the receipt signed an older generation — was never delivered.
	for provider, generation := range reserved {
		if c, ok := carried[provider]; !ok || !c.covers(generation) {
			releaseCLIUsageReservation(provider, generation)
		}
	}
}

// releaseCLIUsageReservation hands an undelivered reservation back to the
// ordinary lifecycle — unless a generation noted for the provider after it was
// reserved is pending (a newer counter, or another epoch after an account
// switch). That one supersedes it and keeps its own debounce and budget;
// releasing over it would hint the stale generation in its place.
func releaseCLIUsageReservation(provider string, generation cliUsageGeneration) {
	p := cliUsagePropagator
	p.mu.Lock()
	h := p.pending[provider]
	superseded := h != nil && !generation.covers(h.generation)
	p.mu.Unlock()
	if superseded {
		return
	}
	noteCLIUsageObservationAdvanced(provider, generation)
}

// noteCLIUsageGenerationRotated is called (once per process) by the cache write
// that first commits this process's Codex epoch. It runs inside that
// transaction, so it only schedules: the recovery check reads the cache from
// the timer.
func noteCLIUsageGenerationRotated() {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rotated || p.stopped {
		return
	}
	p.rotated = true
	p.rotationRetryAt = time.Time{}
	if p.cfg != nil {
		p.recoveryDue = true
		p.armLocked(0)
	}
}

// rotate attempts the epoch rotation; a refused write books a retry after
// `retry`, then every cliUsageRotationRetryPeriod for the life of the process.
func (p *cliUsagePropagatorState) rotate(retry time.Duration) {
	_, refused := codexRotateGenerationEpoch(time.Now())
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.rotated || !refused {
		return
	}
	logCLIUsageHint("rotation_retry")
	p.rotationRetryAt = time.Now().Add(retry)
	p.armLocked(retry)
}

// sendDelayLocked is how long h must still wait: its own due instant, and the
// device-wide spacing since the last hint.
func (p *cliUsagePropagatorState) sendDelayLocked(h *cliUsagePendingHint) time.Duration {
	due := h.due()
	if !p.lastSentAt.IsZero() {
		if spaced := p.lastSentAt.Add(cliUsageHintSpacing); spaced.After(due) {
			due = spaced
		}
	}
	if d := time.Until(due); d > 0 {
		return d
	}
	return 0
}

// providerBlockedLocked: a Codex generation waits for this process's Codex
// epoch rotation (codexPublishableGeneration publishes nothing before it).
func (p *cliUsagePropagatorState) providerBlockedLocked(provider string) bool {
	return provider == codexUsageProvider && !p.rotated
}

// nextHintLocked picks the pending provider whose hint fell due first (provider
// id breaks a tie), skipping providers that cannot be sent yet. ok is false
// when nothing is eligible.
func (p *cliUsagePropagatorState) nextHintLocked() (provider string, h *cliUsagePendingHint, ok bool) {
	for candidate, pending := range p.pending {
		if p.providerBlockedLocked(candidate) {
			continue
		}
		if !ok || pending.due().Before(h.due()) || (pending.due().Equal(h.due()) && candidate < provider) {
			provider, h, ok = candidate, pending, true
		}
	}
	return provider, h, ok
}

// armNextLocked arms the timer for the next eligible hint, or for the next
// spacing boundary while every pending provider is blocked.
func (p *cliUsagePropagatorState) armNextLocked() {
	if len(p.pending) == 0 {
		return
	}
	if _, h, ok := p.nextHintLocked(); ok {
		p.armLocked(p.sendDelayLocked(h))
		return
	}
	p.armLocked(cliUsageHintSpacing)
}

// armLocked (re)arms the one timer to fire after d, replacing any earlier one.
// A pending rotation retry that is due sooner keeps its own instant.
func (p *cliUsagePropagatorState) armLocked(d time.Duration) {
	if p.stopped {
		return
	}
	if !p.rotationRetryAt.IsZero() {
		if r := time.Until(p.rotationRetryAt); r < d {
			d = r
		}
	}
	if d < 0 {
		d = 0
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timerGen++
	gen := p.timerGen
	p.timer = time.AfterFunc(d, func() { p.fire(gen) })
}

// fire is the timer callback: a due rotation retry, the startup recovery
// checks, then the next due hint.
func (p *cliUsagePropagatorState) fire(gen uint64) {
	p.mu.Lock()
	if p.stopped || gen != p.timerGen {
		p.mu.Unlock()
		return
	}
	if !p.rotated && !p.rotationRetryAt.IsZero() && !time.Now().Before(p.rotationRetryAt) {
		p.rotationRetryAt = time.Time{}
		p.mu.Unlock()
		p.rotate(cliUsageRotationRetryPeriod)
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
	}
	recovery, claudeRecovery := p.recoveryDue, p.claudeRecoveryDue
	p.recoveryDue, p.claudeRecoveryDue = false, false
	p.mu.Unlock()

	if recovery {
		if g := cliUsageRecoveryGeneration(); g != nil {
			noteCLIUsageObservationAdvanced(codexUsageProvider, *g)
		}
	}
	if claudeRecovery {
		if g := claudeUsageRecoveryGeneration(); g != nil {
			noteCLIUsageObservationAdvanced(claudeUsageProvider, *g)
		}
	}
	p.sendDue()
}

// cliUsageRecoveryGeneration is the rotated generation of a cache whose active
// account still renders at least one numeric Codex metric, or whose contributors
// a committed clear retired (the out-of-quota shape, a rescope), or nil. A clear
// the previous process committed but never hinted (shutdown inside the debounce)
// would otherwise leave the backend publishing the old numbers.
func cliUsageRecoveryGeneration() *cliUsageGeneration {
	view := codexCacheViewForAccount(currentCodexAccountFingerprint())
	generation := codexPublishableGeneration(view)
	if generation == nil {
		return nil
	}
	if len(view.contributors) == 0 {
		return generation
	}
	for _, m := range codexMetricsFromView(view, time.Now()) {
		if !m.Unknown && m.Consumed != nil {
			return generation
		}
	}
	return nil
}

// claudeUsageRecoveryGeneration is the committed generation of a Claude cache
// that holds a numeric reading, versioning a legacy (pre-generation) numeric
// snapshot first, or nil (no cache, no numeric reading, or a versioning write
// the locks refused — the next numeric commit versions it). A commit whose
// process exited inside the debounce — an update handoff, a status-line hook —
// is recovered this way; the backend skips one it already applied.
func claudeUsageRecoveryGeneration() *cliUsageGeneration {
	return claudeEnsureCacheGeneration(claudeRateLimitCachePath())
}

// claudeUsageFallbackDetected reports whether the last machine-info gather
// detected Claude Code; a var so tests can pin it.
var claudeUsageFallbackDetected = func() bool {
	info := GetMachineInfo()
	return info != nil && info.DetectedCliAgents[claudeUsageProvider].Detected
}

// armFallbackLocked (re)arms the Claude fallback read.
func (p *cliUsagePropagatorState) armFallbackLocked() {
	if p.stopped || !cliUsageClaudeFallbackEnabled {
		return
	}
	if p.fallbackTimer != nil {
		p.fallbackTimer.Stop()
	}
	p.fallbackGen++
	gen := p.fallbackGen
	p.fallbackTimer = time.AfterFunc(cliUsageClaudeFallbackPeriod, func() { p.fallback(gen) })
}

// fallback discovers a Claude generation another process committed (the
// status-line hook) when no ordinary read has: one small cache read, only
// while every gate is open.
func (p *cliUsagePropagatorState) fallback(gen uint64) {
	p.mu.Lock()
	if p.stopped || gen != p.fallbackGen {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	if g := claudeUsageFallbackGeneration(); g != nil {
		noteCLIUsageGenerationObserved(claudeUsageProvider, *g)
	}
	p.mu.Lock()
	if gen == p.fallbackGen {
		p.armFallbackLocked()
	}
	p.mu.Unlock()
}

// claudeUsageFallbackGeneration reads the Claude cache's committed generation,
// or nil without reading while offline, draining, shutting down, Claude is not
// detected, or no cache exists.
func claudeUsageFallbackGeneration() *cliUsageGeneration {
	if IsShutdownInProgress() || isDraining() || IsOffline() || !claudeUsageFallbackDetected() {
		return nil
	}
	path := claudeRateLimitCachePath()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	snap, ok := claudeUsageFallbackLoad(path)
	if !ok {
		return nil
	}
	return claudeSnapshotGeneration(snap)
}

// claudeUsageFallbackLoad is the fallback's one cache read; a var so tests can
// count reads.
var claudeUsageFallbackLoad = loadClaudeRateLimitSnapshot

// sendDue sends the next due hint when every gate is open; otherwise it
// re-arms the timer for the next boundary.
func (p *cliUsagePropagatorState) sendDue() {
	p.mu.Lock()
	if p.stopped || len(p.pending) == 0 || p.cfg == nil {
		p.mu.Unlock()
		return
	}
	provider, h, ok := p.nextHintLocked()
	if !ok {
		logCLIUsageHint("awaiting_rotation")
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	if d := p.sendDelayLocked(h); d > 0 {
		p.armLocked(d)
		p.mu.Unlock()
		return
	}
	if blocked := cliUsageHintBlocked(); blocked != "" {
		logCLIUsageHint(blocked)
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	cfg, generation, followUp, ctx := p.cfg, h.generation, h.followUp, p.ctx
	p.mu.Unlock()

	// Read outside mu: configPersistenceMu is held across config file writes.
	agentID, secret := cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		logCLIUsageHint("skipped_unregistered")
		p.mu.Lock()
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}

	timestamp := time.Now().UnixMilli()
	status := sendCLIUsageObservedHint(ctx, fmt.Sprintf("%s/device/%s/cli-usage/observed", baseURL, agentID), cliUsageObservedHint{
		Timestamp:       timestamp,
		Signature:       generateHMAC(buildCLIUsageObservedSignedMessage(agentID, timestamp, provider, generation), secret),
		Provider:        provider,
		GenerationEpoch: generation.Epoch,
		Generation:      generation.Counter,
	})
	switch {
	case status/100 == 2 && followUp:
		logCLIUsageHint("followup_sent")
	case status/100 == 2:
		logCLIUsageHint("sent")
	default:
		logCLIUsageHint(fmt.Sprintf("failed_%d", status))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.lastSentAt = now
	if p.stopped {
		return
	}
	// A newer generation (or a receipt confirmation) that replaced this slot
	// during the send keeps its own budget and waits for the spacing.
	if cur := p.pending[provider]; cur != nil && cur.generation == generation && cur.followUp == followUp {
		if followUp {
			delete(p.pending, provider)
		} else {
			// Sent or failed, the generation keeps exactly one follow-up.
			cur.followUp, cur.notBefore = true, now
		}
	}
	p.armNextLocked()
}

// cliUsageHintBlocked names the device-wide gate holding hints back, or "".
func cliUsageHintBlocked() string {
	switch {
	case IsShutdownInProgress(), isDraining():
		return "deferred"
	case IsOffline():
		return "skipped_offline"
	}
	return ""
}

func logCLIUsageHint(label string) {
	fmt.Printf("%s[cli-usage] usage hint: %s%s\n", colorCyan, label, colorReset)
}

// resetCLIUsagePropagator restores the zero propagator in place (tests): a
// timer callback already running sees the bumped timerGen and does nothing.
func resetCLIUsagePropagator() {
	p := cliUsagePropagator
	p.rotating.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.fallbackTimer != nil {
		p.fallbackTimer.Stop()
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.timerGen++
	p.fallbackGen++
	p.cfg, p.ctx, p.cancel, p.stopped, p.timer, p.fallbackTimer = nil, nil, nil, false, nil, nil
	p.pending, p.seen, p.lastSentAt = nil, nil, time.Time{}
	p.rotated, p.rotationRetryAt, p.recoveryDue, p.claudeRecoveryDue = false, time.Time{}, false, false
}
