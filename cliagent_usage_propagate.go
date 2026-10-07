// cliagent_usage_propagate.go — tells terminal-service that a fresh usage
// reading is waiting on this device.
//
// Why this exists:
//
//	The device publishes usage only in answer to `__cli_usage_refresh__`, and
//	terminal-service wakes those refreshes on session start, command dispatch,
//	a Refresh click and a few other backend events. A `codex` or `opencode` the
//	user runs in their own shell, the agent-initiated `__cli_smoke__`, and the
//	post-run debt ladder (which lands its reading well after the session-end
//	wake) produce a reading the backend never asks for — the card keeps a stale
//	observation, or none, until the six-hourly machine-info gather.
//
// When a provider's capture state commits an ADVANCED observation — the Codex
// cache (mergeCodexRateLimitCacheObserved) or the OpenCode usage ledger
// (updateOpenCodeUsageLedger) — the propagator sends one signed hint,
// `POST /device/:agentId/cli-usage/observed`, naming the committed capture
// generation. terminal-service answers it with one ordinary refresh — at most
// one per agent per 4 minutes, none when it already applied that generation,
// and without moving an Idle device to Active. The hint carries no metric
// values: numbers travel only inside the signed refresh receipt.
//
// Device-side bounds, PER PROVIDER (cliUsageGenerationSources):
//   - a 15 s trailing debounce collapses bursts;
//   - each provider keeps its OWN pending slot, so an OpenCode observation can
//     never evict a Codex one that has not been sent yet;
//   - after a hint is sent the observation stays pending for exactly ONE
//     follow-up at the next spacing boundary (the backend makes it a no-op once
//     the generation is applied), then it is dropped — at most two hints per
//     observation;
//   - nothing is sent while offline, draining, shutting down, unregistered or
//     before THAT provider's generation epoch is on disk; the observation is
//     kept and re-checked at the next boundary.
//
// The 5-minute spacing is GLOBAL: at most one hint leaves the device in that
// window, and when two providers are due the earlier one goes first.
//
// Pending state is in memory only. A reading captured just before an update
// handoff is recovered by the next process: once a provider's epoch rotation
// commits, a capture state that still renders a numeric metric — or holds a
// committed clear — is scheduled as a pending observation through the same
// gates.
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
	"sync"
	"time"
)

// Vars so tests can pin them small.
var (
	cliUsageHintDebounce        = 15 * time.Second
	cliUsageHintSpacing         = 5 * time.Minute
	cliUsageRotationFirstRetry  = 15 * time.Second
	cliUsageRotationRetryPeriod = 60 * time.Second
	cliUsageHintHTTPTimeout     = 2 * time.Second
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

// cliUsageGenerationSource is one provider that reports a capture generation.
// Adding a provider here is all it takes for its readings to reach the backend
// through the same bounded schedule; nothing else in this file names a provider.
type cliUsageGenerationSource struct {
	Provider string
	// Rotate moves the provider's persisted capture state onto this process's
	// epoch. `refused` reports a write to retry.
	Rotate func(now time.Time) (rotated, refused bool)
	// Recovery is the generation of a state already committed but never hinted
	// (a shutdown inside the debounce), or nil.
	Recovery func() *cliUsageGeneration
}

// cliUsageGenerationSources is the table. Codex's behaviour and its signed
// message are unchanged by OpenCode joining it.
// Populated in init(): the entries reference functions that transitively
// reference this table (a rotation completes through
// noteCLIUsageGenerationRotated), which Go rejects as an initialization cycle
// when spelled as a literal.
var cliUsageGenerationSources []cliUsageGenerationSource

func init() {
	cliUsageGenerationSources = []cliUsageGenerationSource{
		{Provider: codexUsageProvider, Rotate: codexRotateGenerationEpoch, Recovery: cliUsageRecoveryGeneration},
		{Provider: openCodeUsageProvider, Rotate: openCodeRotateGenerationEpoch, Recovery: openCodeUsageRecoveryGeneration},
	}
}

func cliUsageGenerationSourceFor(provider string) *cliUsageGenerationSource {
	for i := range cliUsageGenerationSources {
		if cliUsageGenerationSources[i].Provider == provider {
			return &cliUsageGenerationSources[i]
		}
	}
	return nil
}

// cliUsagePendingHint is one provider's pending observation and its two-hint
// budget.
type cliUsagePendingHint struct {
	generation cliUsageGeneration
	// followUp: the first hint went out; the next send is its one follow-up.
	followUp bool
	noteAt   time.Time
}

// cliUsageProviderHintState is everything the schedule keeps per provider.
type cliUsageProviderHintState struct {
	pending *cliUsagePendingHint
	// retryAt holds a blocked send back to the next boundary, so a gate
	// (offline, unregistered) cannot spin the timer.
	retryAt time.Time

	rotated         bool
	rotationRetryAt time.Time
	recoveryDue     bool
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

	providers map[string]*cliUsageProviderHintState
	// lastSentAt is GLOBAL: the 5-minute spacing bounds hints from the device,
	// not from one provider.
	lastSentAt time.Time

	// rotating tracks the startup rotation goroutines so a test reset can wait
	// for them instead of letting one write into the next test's state.
	rotating sync.WaitGroup
}

var cliUsagePropagator = &cliUsagePropagatorState{}

// providerLocked returns (creating if needed) one provider's slot. Only
// providers in cliUsageGenerationSources get one, so an unknown provider can
// never grow the map.
func (p *cliUsagePropagatorState) providerLocked(provider string) *cliUsageProviderHintState {
	if cliUsageGenerationSourceFor(provider) == nil {
		return nil
	}
	if p.providers == nil {
		p.providers = map[string]*cliUsageProviderHintState{}
	}
	state := p.providers[provider]
	if state == nil {
		state = &cliUsageProviderHintState{}
		p.providers[provider] = state
	}
	return state
}

// startCLIUsagePropagator is called from StartAgent. It rotates every provider's
// capture state onto this process's epoch (retrying a refused write on the
// timer) and, once each commits, schedules that provider's startup recovery
// observation. Observations noted before it runs were recorded and are sent
// through the normal schedule.
func startCLIUsagePropagator(cfg *Config) {
	p := cliUsagePropagator
	p.mu.Lock()
	if p.cfg != nil || p.stopped {
		p.mu.Unlock()
		return
	}
	p.cfg = cfg
	p.ctx, p.cancel = context.WithCancel(context.Background())
	for _, source := range cliUsageGenerationSources {
		if state := p.providerLocked(source.Provider); state != nil && state.rotated {
			// A capture committed during boot already rotated this epoch.
			state.recoveryDue = true
		}
	}
	p.rearmLocked(time.Now())
	p.rotating.Add(len(cliUsageGenerationSources))
	p.mu.Unlock()
	for _, source := range cliUsageGenerationSources {
		source := source
		go func() {
			defer p.rotating.Done()
			p.rotate(source, cliUsageRotationFirstRetry)
		}()
	}
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
	p.timerGen++
	if p.cancel != nil {
		p.cancel()
	}
	for _, state := range p.providers {
		if state.pending != nil {
			logCLIUsageHint("dropped")
			state.pending = nil
		}
	}
}

// noteCLIUsageObservationAdvanced records a committed, advanced observation for
// one provider. Before the propagator starts it only records it, so a frame
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
	state := p.providerLocked(provider)
	if state == nil {
		return
	}
	if state.pending != nil && state.pending.generation.Epoch == generation.Epoch &&
		state.pending.generation.Counter >= generation.Counter {
		return // not newer than what is already pending
	}
	now := time.Now()
	idle := state.pending == nil || state.pending.followUp
	// retryAt is deliberately KEPT: it is the boundary a closed gate (offline,
	// draining, unregistered) booked, and clearing it would let a device that
	// keeps committing observations re-check that gate every debounce instead
	// of once per spacing window.
	state.pending = &cliUsagePendingHint{generation: generation, noteAt: now}
	if p.cfg != nil {
		if due, ok := p.nextDueLocked(state, now); ok && idle && due.Sub(now) > cliUsageHintDebounce {
			// Throttled by the global spacing: kept, carrying the newest
			// generation, never dropped.
			logCLIUsageHint("deferred")
		}
		p.rearmLocked(now)
	}
}

// noteCLIUsageGenerationRotated is called (once per provider per process) by the
// capture write that first commits this process's epoch. It may run inside that
// write's transaction, so it only schedules: the recovery check reads the state
// from the timer.
func noteCLIUsageGenerationRotated(provider string) {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	state := p.providerLocked(provider)
	if state == nil || state.rotated {
		return
	}
	state.rotated = true
	state.rotationRetryAt = time.Time{}
	if p.cfg != nil {
		state.recoveryDue = true
		p.rearmLocked(time.Now())
	}
}

// rotate attempts one provider's epoch rotation; a refused write books a retry
// after `retry`, then every cliUsageRotationRetryPeriod for the life of the
// process.
func (p *cliUsagePropagatorState) rotate(source cliUsageGenerationSource, retry time.Duration) {
	_, refused := source.Rotate(time.Now())
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.providerLocked(source.Provider)
	if p.stopped || state == nil || state.rotated || !refused {
		return
	}
	logCLIUsageHint("rotation_retry")
	now := time.Now()
	state.rotationRetryAt = now.Add(retry)
	p.rearmLocked(now)
}

// nextDueLocked is when one provider's pending observation may be sent: the
// trailing debounce, the GLOBAL spacing since the last hint, and any boundary a
// blocked send booked. ok=false when nothing is pending.
func (p *cliUsagePropagatorState) nextDueLocked(state *cliUsageProviderHintState, now time.Time) (time.Time, bool) {
	if state.pending == nil {
		return time.Time{}, false
	}
	due := state.pending.noteAt.Add(cliUsageHintDebounce)
	if state.pending.followUp {
		due = now
	}
	if !p.lastSentAt.IsZero() {
		if spaced := p.lastSentAt.Add(cliUsageHintSpacing); spaced.After(due) {
			due = spaced
		}
	}
	if state.retryAt.After(due) {
		due = state.retryAt
	}
	return due, true
}

// rearmLocked arms the ONE timer for the earliest thing any provider is waiting
// on — a due rotation retry, a startup recovery check, or a pending hint.
// Replacing a per-provider arm with this is what keeps two providers from
// overwriting each other's schedule.
func (p *cliUsagePropagatorState) rearmLocked(now time.Time) {
	if p.stopped {
		return
	}
	next := time.Time{}
	consider := func(at time.Time) {
		if at.IsZero() {
			return
		}
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, state := range p.providers {
		if state.recoveryDue {
			consider(now)
		}
		if !state.rotated {
			consider(state.rotationRetryAt)
		}
		if due, ok := p.nextDueLocked(state, now); ok {
			consider(due)
		}
	}
	if next.IsZero() {
		return
	}
	d := next.Sub(now)
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

// fire is the timer callback: due rotation retries, the startup recovery checks,
// then the single most overdue pending hint.
func (p *cliUsagePropagatorState) fire(gen uint64) {
	p.mu.Lock()
	if p.stopped || gen != p.timerGen {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	var retryRotations, recoveries []cliUsageGenerationSource
	for _, source := range cliUsageGenerationSources {
		state := p.providerLocked(source.Provider)
		if state == nil {
			continue
		}
		if !state.rotated && !state.rotationRetryAt.IsZero() && !now.Before(state.rotationRetryAt) {
			state.rotationRetryAt = time.Time{}
			retryRotations = append(retryRotations, source)
		}
		if state.recoveryDue {
			state.recoveryDue = false
			recoveries = append(recoveries, source)
		}
	}
	p.mu.Unlock()

	for _, source := range retryRotations {
		p.rotate(source, cliUsageRotationRetryPeriod)
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if stopped {
			return
		}
	}
	for _, source := range recoveries {
		if source.Recovery == nil {
			continue
		}
		if g := source.Recovery(); g != nil {
			noteCLIUsageObservationAdvanced(source.Provider, *g)
		}
	}
	p.sendDue()
}

// cliUsageRecoveryGeneration is the rotated generation of a Codex cache whose
// active account still renders at least one numeric metric, or whose
// contributors a committed clear retired (the out-of-quota shape, a rescope), or
// nil. A clear the previous process committed but never hinted (shutdown inside
// the debounce) would otherwise leave the backend publishing the old numbers.
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

// sendDue sends the single most overdue pending hint when every gate is open —
// one hint per global spacing window — and re-arms the timer for the rest.
func (p *cliUsagePropagatorState) sendDue() {
	now := time.Now()
	p.mu.Lock()
	if p.stopped || p.cfg == nil {
		p.mu.Unlock()
		return
	}
	provider, chosen := "", (*cliUsageProviderHintState)(nil)
	chosenDue := time.Time{}
	for _, source := range cliUsageGenerationSources {
		state := p.providers[source.Provider]
		if state == nil {
			continue
		}
		due, ok := p.nextDueLocked(state, now)
		if !ok || due.After(now) {
			continue
		}
		if chosen == nil || due.Before(chosenDue) {
			provider, chosen, chosenDue = source.Provider, state, due
		}
	}
	if chosen == nil {
		p.rearmLocked(now)
		p.mu.Unlock()
		return
	}
	if blocked := cliUsageHintBlocked(chosen.rotated); blocked != "" {
		logCLIUsageHint(blocked)
		chosen.retryAt = now.Add(cliUsageHintSpacing)
		p.rearmLocked(now)
		p.mu.Unlock()
		return
	}
	cfg, generation, followUp, ctx := p.cfg, chosen.pending.generation, chosen.pending.followUp, p.ctx
	p.mu.Unlock()

	// Read outside mu: configPersistenceMu is held across config file writes.
	agentID, secret := cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		logCLIUsageHint("skipped_unregistered")
		p.mu.Lock()
		if state := p.providers[provider]; state != nil {
			state.retryAt = time.Now().Add(cliUsageHintSpacing)
		}
		p.rearmLocked(time.Now())
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
	sentAt := time.Now()
	p.lastSentAt = sentAt
	state := p.providers[provider]
	if p.stopped || state == nil || state.pending == nil ||
		state.pending.generation != generation || state.pending.followUp != followUp {
		// A newer observation arrived during the send: it keeps its own
		// two-hint budget and waits for the spacing.
		p.rearmLocked(sentAt)
		return
	}
	if followUp {
		state.pending = nil
	} else {
		// Sent or failed, the observation keeps exactly one follow-up.
		state.pending.followUp = true
	}
	p.rearmLocked(sentAt)
}

// cliUsageHintBlocked names the gate holding a hint back, or "".
func cliUsageHintBlocked(rotated bool) string {
	switch {
	case IsShutdownInProgress(), isDraining():
		return "deferred"
	case IsOffline():
		return "skipped_offline"
	case !rotated:
		return "awaiting_rotation"
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
	if p.cancel != nil {
		p.cancel()
	}
	p.timerGen++
	p.cfg, p.ctx, p.cancel, p.stopped, p.timer = nil, nil, nil, false, nil
	p.providers = nil
	p.lastSentAt = time.Time{}
}
