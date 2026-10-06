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
// When a provider's capture commits an ADVANCED observation — the Codex cache
// for the active account (mergeCodexRateLimitCacheObserved), or OpenCode's
// usage ledger (cliagent_usage_opencode_capture.go) — the propagator sends one
// signed hint,
// `POST /device/:agentId/cli-usage/observed`, naming the committed capture
// generation. terminal-service answers it with one ordinary refresh — at most
// one per agent per 4 minutes, none when it already applied that generation,
// and without moving an Idle device to Active. The hint carries no metric
// values: numbers travel only inside the signed refresh receipt.
//
// The providers that send hints mirror terminal-service's
// CLI_USAGE_HINT_PROVIDERS (src/config/terminal.config.js) by hand — the device
// cannot import it: codexUsageProvider and openCodeUsageProvider. A hint from
// any other provider is accepted and ignored there.
//
// Device-side bounds:
//   - a 15 s trailing debounce collapses bursts;
//   - hints are at least 5 minutes apart, DEVICE-wide: every hint earns the
//     same all-provider refresh. One throttled by that spacing is deferred
//     (one pending per provider, carrying its newest generation), never
//     dropped; when several are due, the oldest observation goes first;
//   - after a hint is sent the observation stays pending for exactly ONE
//     follow-up at the next spacing boundary (the backend makes it a no-op once
//     the generation is applied), then it is dropped — at most two hints per
//     observation, per provider;
//   - nothing is sent while offline, draining, shutting down, unregistered or
//     before this process's generation epoch is on disk for that provider; the
//     observation is kept and re-checked at the next boundary.
//
// Pending state is in memory only. A reading captured just before an update
// handoff is recovered by the next process: once its epoch rotation commits
// (codexRotateGenerationEpoch), a cache that still renders a numeric Codex
// metric — or holds a committed clear (no contributors, a generation) — is
// scheduled as a pending observation through the same gates. An OpenCode
// ledger with numbers for today is rotated and scheduled the same way
// (openCodeRecoveryGeneration).
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

	// pending holds at most one observation per provider, so one provider's
	// observation never overwrites another's.
	pending    map[string]*pendingCLIUsageHint
	lastSentAt time.Time

	rotated         bool
	rotationRetryAt time.Time
	recoveryDue     bool
	// rotating tracks the startup rotation goroutine so a test reset can wait
	// for it instead of letting it write into the next test's cache.
	rotating sync.WaitGroup
}

// pendingCLIUsageHint is one provider's newest unsent (or once-sent)
// observation.
type pendingCLIUsageHint struct {
	generation cliUsageGeneration
	// followUp: the first hint went out; the next send is its one follow-up.
	followUp bool
	notedAt  time.Time
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
	} else if len(p.pending) > 0 {
		p.armLocked(p.nextSendDelayLocked())
	}
	p.rotating.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.rotating.Done()
		p.rotate(cliUsageRotationFirstRetry)
		// OpenCode's ledger rotates on its own: a device that never ran Codex
		// has no Codex rotation to wait on.
		if g := openCodeRecoveryGeneration(time.Now()); g != nil {
			noteCLIUsageObservationAdvanced(openCodeUsageProvider, *g)
		}
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
	if len(p.pending) > 0 {
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
	prior := p.pending[provider]
	if prior != nil && prior.generation.Epoch == generation.Epoch && prior.generation.Counter >= generation.Counter {
		return // not newer than what is already pending
	}
	idle := prior == nil || prior.followUp
	if p.pending == nil {
		p.pending = map[string]*pendingCLIUsageHint{}
	}
	entry := &pendingCLIUsageHint{generation: generation, notedAt: time.Now()}
	p.pending[provider] = entry
	if p.cfg != nil {
		d := p.nextSendDelayLocked()
		if idle && p.entrySendDelayLocked(entry) > cliUsageHintDebounce {
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

// nextSendDelayLocked is how long until the first pending observation is due.
func (p *cliUsagePropagatorState) nextSendDelayLocked() time.Duration {
	_, d := p.nextPendingLocked()
	return d
}

// nextPendingLocked picks the provider to send next: the shortest wait, and
// among those due together the oldest observation. "" when nothing is pending.
func (p *cliUsagePropagatorState) nextPendingLocked() (string, time.Duration) {
	provider, best := "", time.Duration(0)
	for name, entry := range p.pending {
		d := p.entrySendDelayLocked(entry)
		if provider == "" || d < best || (d == best && entry.notedAt.Before(p.pending[provider].notedAt)) {
			provider, best = name, d
		}
	}
	return provider, best
}

// entrySendDelayLocked is how long one observation must wait: its trailing
// debounce, and the device-wide spacing since the last hint.
func (p *cliUsagePropagatorState) entrySendDelayLocked(entry *pendingCLIUsageHint) time.Duration {
	now := time.Now()
	due := entry.notedAt.Add(cliUsageHintDebounce)
	if entry.followUp {
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

// sendDue sends the next due hint when every gate is open; otherwise it
// re-arms the timer for the next boundary.
func (p *cliUsagePropagatorState) sendDue() {
	p.mu.Lock()
	if p.stopped || len(p.pending) == 0 || p.cfg == nil {
		p.mu.Unlock()
		return
	}
	provider, d := p.nextPendingLocked()
	if d > 0 {
		p.armLocked(d)
		p.mu.Unlock()
		return
	}
	blocked := cliUsageHintBlocked(p.rotatedForLocked(provider))
	if blocked == "awaiting_rotation" {
		// The rotation gate is per provider: one still waiting on its epoch must
		// not hold back another's due hint.
		if other := p.nextDueRotatedLocked(); other != "" {
			provider, blocked = other, ""
		}
	}
	if blocked != "" {
		logCLIUsageHint(blocked)
		retry := cliUsageHintSpacing
		if blocked == "awaiting_rotation" {
			// …nor delay it to the next boundary: wake when it falls due.
			for name, entry := range p.pending {
				if d := p.entrySendDelayLocked(entry); p.rotatedForLocked(name) && d < retry {
					retry = d
				}
			}
		}
		p.armLocked(retry)
		p.mu.Unlock()
		return
	}
	entry := p.pending[provider]
	cfg, generation, followUp, ctx := p.cfg, entry.generation, entry.followUp, p.ctx
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
	p.lastSentAt = time.Now()
	if p.stopped {
		return
	}
	// A newer observation of this provider that arrived during the send keeps
	// its own two-hint budget and waits for the spacing.
	if current := p.pending[provider]; current != nil && current.generation == generation {
		if followUp {
			delete(p.pending, provider)
		} else {
			// Sent or failed, the observation keeps exactly one follow-up.
			current.followUp = true
		}
	}
	if len(p.pending) > 0 {
		p.armLocked(p.nextSendDelayLocked())
	}
}

// nextDueRotatedLocked is the oldest due observation whose provider's epoch is
// on disk, or "".
func (p *cliUsagePropagatorState) nextDueRotatedLocked() string {
	provider := ""
	for name, entry := range p.pending {
		if p.entrySendDelayLocked(entry) > 0 || !p.rotatedForLocked(name) {
			continue
		}
		if provider == "" || entry.notedAt.Before(p.pending[provider].notedAt) {
			provider = name
		}
	}
	return provider
}

// rotatedForLocked reports whether provider's capture generation is on this
// process's epoch: Codex's cache rotation, or a committed OpenCode ledger write.
func (p *cliUsagePropagatorState) rotatedForLocked(provider string) bool {
	if provider == openCodeUsageProvider {
		return openCodeGenerationRotated.Load()
	}
	return p.rotated
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
	p.pending, p.lastSentAt = nil, time.Time{}
	p.rotated, p.rotationRetryAt, p.recoveryDue = false, time.Time{}, false
}
