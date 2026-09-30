// cliagent_usage_propagate.go — tells terminal-service that a fresh usage
// reading is waiting on this device.
//
// Why this exists:
//
//	The device publishes usage only in answer to `__cli_usage_refresh__`, and
//	terminal-service wakes those refreshes on session start, command dispatch,
//	a Refresh click and a few other backend events. A `codex` the user runs in
//	their own shell, the agent-initiated `__cli_smoke__`, and the post-run debt
//	ladder (which lands its reading well after the session-end wake) produce a
//	reading the backend never asks for — the card keeps a stale observation, or
//	none, until the six-hourly machine-info gather.
//
// When the Codex cache commits an ADVANCED observation for the active account
// (mergeCodexRateLimitCacheObserved), the propagator sends one signed hint,
// `POST /device/:agentId/cli-usage/observed`, naming the committed capture
// generation. terminal-service answers it with one ordinary refresh — at most
// one per agent per 4 minutes, none when it already applied that generation,
// and without moving an Idle device to Active. The hint carries no metric
// values: numbers travel only inside the signed refresh receipt.
//
// Device-side bounds:
//   - a 15 s trailing debounce collapses bursts;
//   - hints are at least 5 minutes apart; one throttled by that spacing is
//     deferred (one pending at most, carrying the newest generation), never
//     dropped;
//   - after a hint is sent the observation stays pending for exactly ONE
//     follow-up at the next spacing boundary (the backend makes it a no-op once
//     the generation is applied), then it is dropped — at most two hints per
//     observation;
//   - nothing is sent while offline, draining, shutting down, unregistered or
//     before this process's generation epoch is on disk; the observation is
//     kept and re-checked at the next boundary.
//
// Pending state is in memory only. A reading captured just before an update
// handoff is recovered by the next process: once its epoch rotation commits
// (codexRotateGenerationEpoch), a cache that still renders a numeric Codex
// metric is scheduled as a pending observation through the same gates.
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

	pendingProvider string
	pending         *cliUsageGeneration
	// followUp: the pending observation's first hint went out; the next send is
	// its one follow-up.
	followUp   bool
	lastNoteAt time.Time
	lastSentAt time.Time

	rotated         bool
	rotationRetryAt time.Time
	recoveryDue     bool
	// rotating tracks the startup rotation goroutine so a test reset can wait
	// for it instead of letting it write into the next test's cache.
	rotating sync.WaitGroup
}

var cliUsagePropagator = &cliUsagePropagatorState{}

// startCLIUsagePropagator is called from StartAgent. It rotates the Codex cache
// onto this process's epoch (retrying a refused write on the timer) and, once
// that commits, schedules the startup recovery observation. Observations noted
// before it runs were recorded and are sent through the normal schedule.
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
		p.armLocked(0)
	} else if p.pending != nil {
		p.armLocked(p.nextSendDelayLocked())
	}
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
	p.timerGen++
	if p.cancel != nil {
		p.cancel()
	}
	if p.pending != nil {
		logCLIUsageHint("dropped")
		p.pending = nil
	}
}

// noteCLIUsageObservationAdvanced records a committed, advanced observation.
// Before the propagator starts it only records it, so a frame captured during
// boot is not lost.
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
	if p.pending != nil && p.pendingProvider == provider && p.pending.Epoch == generation.Epoch &&
		p.pending.Counter >= generation.Counter {
		return // not newer than what is already pending
	}
	idle := p.pending == nil || p.followUp
	g := generation
	p.pending, p.pendingProvider, p.followUp = &g, provider, false
	p.lastNoteAt = time.Now()
	if p.cfg != nil {
		d := p.nextSendDelayLocked()
		if idle && d > cliUsageHintDebounce {
			// Throttled by the spacing: kept, carrying the newest generation.
			logCLIUsageHint("deferred")
		}
		p.armLocked(d)
	}
}

// noteCLIUsageGenerationRotated is called (once per process) by the cache write
// that first commits this process's epoch. It runs inside that transaction, so
// it only schedules: the recovery check reads the cache from the timer.
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

// nextSendDelayLocked is how long the pending observation must wait: the
// trailing debounce, and the spacing since the last hint.
func (p *cliUsagePropagatorState) nextSendDelayLocked() time.Duration {
	now := time.Now()
	due := p.lastNoteAt.Add(cliUsageHintDebounce)
	if p.followUp {
		due = now
	}
	if !p.lastSentAt.IsZero() {
		if spaced := p.lastSentAt.Add(cliUsageHintSpacing); spaced.After(due) {
			due = spaced
		}
	}
	if d := due.Sub(now); d > 0 {
		return d
	}
	return 0
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

// fire is the timer callback: a due rotation retry, the startup recovery check,
// then the pending hint.
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
	recovery := p.recoveryDue
	p.recoveryDue = false
	p.mu.Unlock()

	if recovery {
		if g := cliUsageRecoveryGeneration(); g != nil {
			noteCLIUsageObservationAdvanced(codexUsageProvider, *g)
		}
	}
	p.sendDue()
}

// cliUsageRecoveryGeneration is the rotated generation of a cache whose active
// account still renders at least one numeric Codex metric, or nil.
func cliUsageRecoveryGeneration() *cliUsageGeneration {
	metrics, generation := codexMetricsAndGenerationFromCache(time.Now(), currentCodexAccountFingerprint())
	if generation == nil {
		return nil
	}
	for _, m := range metrics {
		if !m.Unknown && m.Consumed != nil {
			return generation
		}
	}
	return nil
}

// sendDue sends the pending hint when it is due and every gate is open;
// otherwise it re-arms the timer for the next boundary.
func (p *cliUsagePropagatorState) sendDue() {
	p.mu.Lock()
	if p.stopped || p.pending == nil || p.cfg == nil {
		p.mu.Unlock()
		return
	}
	if d := p.nextSendDelayLocked(); d > 0 {
		p.armLocked(d)
		p.mu.Unlock()
		return
	}
	if blocked := cliUsageHintBlocked(p.rotated); blocked != "" {
		logCLIUsageHint(blocked)
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	agentID, secret := p.cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		logCLIUsageHint("skipped_unregistered")
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	provider, generation, followUp, ctx := p.pendingProvider, *p.pending, p.followUp, p.ctx
	p.mu.Unlock()

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
	p.lastSentAt = time.Now()
	if p.stopped || p.pending == nil || *p.pending != generation || p.pendingProvider != provider {
		// A newer observation arrived during the send: it keeps its own
		// two-hint budget and waits for the spacing.
		if !p.stopped && p.pending != nil {
			p.armLocked(p.nextSendDelayLocked())
		}
		return
	}
	if followUp {
		p.pending, p.followUp = nil, false
		return
	}
	// Sent or failed, the observation keeps exactly one follow-up.
	p.followUp = true
	p.armLocked(p.nextSendDelayLocked())
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
	p.pendingProvider, p.pending, p.followUp = "", nil, false
	p.lastNoteAt, p.lastSentAt = time.Time{}, time.Time{}
	p.rotated, p.rotationRetryAt, p.recoveryDue = false, time.Time{}, false
}
