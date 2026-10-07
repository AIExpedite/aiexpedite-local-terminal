// cliagent_usage_propagate.go — tells terminal-service that a fresh usage
// reading is waiting on this device.
//
// Why this exists:
//
//	The device publishes usage only in answer to `__cli_usage_refresh__`, and
//	terminal-service wakes those refreshes on session start, command dispatch,
//	a Refresh click and a few other backend events. A CLI the user runs in their
//	own shell, the agent-initiated `__cli_smoke__`, and the post-run debt ladder
//	(which lands its reading well after the session-end wake) produce a reading
//	the backend never asks for — the card keeps a stale observation, or none,
//	until the six-hourly machine-info gather.
//
// Provider-neutral. Each provider with a capture generation (Codex, Claude Code)
// notes a committed, ADVANCED observation of its active account here
// (noteCLIUsageObservationAdvanced), and the propagator sends one signed hint,
// `POST /device/:agentId/cli-usage/observed`, naming the provider and its
// committed capture generation. terminal-service answers it with one ordinary
// refresh — at most one per agent per 4 minutes across providers (one refresh
// collects every provider), none when it already applied that provider's
// generation, and without moving an Idle device to Active. The hint carries no
// metric values: numbers travel only inside the signed refresh receipt.
//
// Device-side bounds:
//   - a 15 s trailing debounce collapses bursts;
//   - hints are at least 5 minutes apart DEVICE-WIDE; one throttled by that
//     spacing is deferred (one pending per provider, carrying the newest
//     generation), never dropped; when several providers are due at one
//     boundary the oldest unsent observation goes first;
//   - after a hint is delivered or fails, the observation stays pending for
//     exactly ONE follow-up at the next spacing boundary, then it is dropped — at
//     most two delivered-or-failed hints per observation;
//   - the 202 body's `reason` sorts a refusal: `already_applied` and the
//     rejections a retry cannot change drop the observation at once; a
//     retryable refusal (a cooldown, a refresh in flight, an old server's
//     reason-less false, an unknown code) keeps it pending WITHOUT spending its
//     follow-up, at most cliUsageHintMaxRetryable times;
//   - nothing is sent while offline, draining, shutting down, unregistered or
//     before the provider's generation epoch is on disk; the observation is
//     kept and re-checked at the next boundary.
//
// Pending state is in memory only. A reading captured just before an update
// handoff is recovered by the next process: once a provider's epoch rotation
// commits (cliUsageGenerationSources), a cache that still renders a numeric
// metric — or holds a committed clear — is scheduled as a pending observation
// through the same gates.
//
// The propagator also owns the usage WATCHER, a 60 s timer of its own for the
// life of the process (the hint timer is one-shot and idles once nothing is
// pending). Each tick stamps a Claude reading written outside this process (the
// status-line hook) and, every 5 minutes, looks for Claude runs the agent did
// not start — see claudeUsageWatchTick.
//
// Logs are fixed labels only (`[cli-usage] usage hint: <label> provider=<id>`)
// — never a path, account, fingerprint, id, server reason or response body.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Vars so tests can pin them small.
var (
	cliUsageHintDebounce        = 15 * time.Second
	cliUsageHintSpacing         = 5 * time.Minute
	cliUsageRotationFirstRetry  = 15 * time.Second
	cliUsageRotationRetryPeriod = 60 * time.Second
	cliUsageHintHTTPTimeout     = 2 * time.Second
	cliUsageWatchPeriod         = 60 * time.Second
)

// cliUsageHintMaxRetryable caps the retryable refusals one observation may
// collect, whichever code caused them. An old server answers `already_applied`
// with a reason-less false, and an unknown future code is retryable too; the
// cap keeps either from being retried for the life of the process.
const cliUsageHintMaxRetryable = 3

// cliUsageHintResponseMaxBytes bounds how much of the 202 body is decoded.
const cliUsageHintResponseMaxBytes = 1024

// cliUsageProcessGenerationEpoch is this agent process's generation epoch, shared
// by every provider that reports one, so one restart rotates them all: drawn
// once at start, uniform in [1, 2^53-1] so it stays a JavaScript-safe integer on
// the wire and in Firestore. It is NOT derived from the clock, so two restarts
// in the same millisecond (or under a frozen test clock) still get distinct
// epochs; a collision is ~2^-53 per restart. The backend replaces an applied
// generation from a different epoch instead of comparing counters across
// epochs, so a restored, rolled-back or copied cache — or a restart — can never
// be suppressed by an old, higher applied counter.
//
// Drawn in init() of EVERY process, including `statusline-hook`, so holding one
// is no evidence of being the agent — see cliUsagePropagatorStarted.
var cliUsageProcessGenerationEpoch atomic.Int64

// cliUsageDrawGenerationEpoch is the draw; a var so a test can force a value or
// a collision.
var cliUsageDrawGenerationEpoch = func() int64 {
	const maxSafe = int64(1)<<53 - 1
	if n, err := rand.Int(rand.Reader, big.NewInt(maxSafe)); err == nil {
		return n.Int64() + 1
	}
	// crypto/rand does not fail on supported platforms; never publish epoch 0.
	return time.Now().UnixNano()&maxSafe | 1
}

func init() {
	cliUsageProcessGenerationEpoch.Store(cliUsageDrawGenerationEpoch())
	cliUsageGenerationSources = []cliUsageGenerationSource{
		{provider: codexUsageProvider, rotate: codexRotateGenerationEpoch, recoveryGeneration: codexRecoveryGeneration},
		{provider: claudeUsageProvider, rotate: claudeRotateGenerationEpoch, recoveryGeneration: claudeRecoveryGeneration},
	}
	cliUsageWatchTick = claudeUsageWatchTick
}

// cliUsageGenerationSource is one provider whose cache carries a capture
// generation: how to rotate that cache onto this process's epoch at start
// (rotated, or refused — a refused write is retried), and which generation to
// recover once it has rotated (nil when nothing published is on disk).
type cliUsageGenerationSource struct {
	provider           string
	rotate             func(now time.Time) (rotated, refused bool)
	recoveryGeneration func() *cliUsageGeneration
}

// cliUsageGenerationSources is the registry rotation and recovery iterate. A
// var so a test can substitute a source; filled in init() because its entries
// reach back into the propagator (an initialization cycle otherwise).
var cliUsageGenerationSources []cliUsageGenerationSource

// cliUsageWatchTick is the watcher's per-tick work; a var so a test can count
// ticks. mayRequest is false while offline or draining: local stamping still
// runs, anything that can lead to an endpoint request does not.
var cliUsageWatchTick func(now time.Time, mayRequest bool)

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

// cliUsageHintOutcome is one attempt's answer: the HTTP status (0 on a
// transport error) and, for a 2xx, the decoded `{ accepted, reason? }`. The
// reason is a closed server code used only to classify; it is never logged.
type cliUsageHintOutcome struct {
	status   int
	accepted bool
	reason   string
}

// sendCLIUsageObservedHint makes ONE attempt. A var so tests never reach the
// network.
var sendCLIUsageObservedHint = func(ctx context.Context, url string, hint cliUsageObservedHint) cliUsageHintOutcome {
	body, err := json.Marshal(hint)
	if err != nil {
		return cliUsageHintOutcome{}
	}
	ctx, cancel := context.WithTimeout(ctx, cliUsageHintHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliUsageHintOutcome{}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: cliUsageHintHTTPTimeout}).Do(req)
	if err != nil {
		return cliUsageHintOutcome{}
	}
	defer resp.Body.Close()
	outcome := cliUsageHintOutcome{status: resp.StatusCode}
	if resp.StatusCode/100 == 2 {
		var decoded struct {
			Accepted bool   `json:"accepted"`
			Reason   string `json:"reason"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, cliUsageHintResponseMaxBytes))
		if json.Unmarshal(raw, &decoded) == nil {
			outcome.accepted, outcome.reason = decoded.Accepted, decoded.Reason
		}
	}
	return outcome
}

// cliUsageHintClass is how the propagator treats an outcome.
type cliUsageHintClass int

const (
	cliUsageHintDelivered cliUsageHintClass = iota
	cliUsageHintAlreadyApplied
	cliUsageHintRejected
	cliUsageHintRetryable
	cliUsageHintFailed
)

// cliUsageHintRejectedReasons are the server refusals a retry cannot change.
// Every other reason — and a reason-less false — is retryable, so a new server
// code fails safe as "try again, boundedly".
var cliUsageHintRejectedReasons = map[string]bool{
	"provider_not_reported":      true,
	"missing_identity":           true,
	"unknown_agent":              true,
	"invalid_input":              true,
	"owner_changed":              true,
	"contribution_revoked":       true,
	"contributor_left_workspace": true,
	"caller_left_workspace":      true,
}

func classifyCLIUsageHintOutcome(outcome cliUsageHintOutcome) cliUsageHintClass {
	switch {
	case outcome.status/100 != 2:
		return cliUsageHintFailed
	case outcome.accepted:
		return cliUsageHintDelivered
	case outcome.reason == "already_applied":
		return cliUsageHintAlreadyApplied
	case cliUsageHintRejectedReasons[outcome.reason]:
		return cliUsageHintRejected
	}
	return cliUsageHintRetryable
}

// cliUsagePendingObservation is one provider's unsent (or follow-up) hint.
type cliUsagePendingObservation struct {
	generation cliUsageGeneration
	notedAt    time.Time
	// seq orders observations by when they were noted; the clock cannot (two
	// notes can share an instant on a coarse clock).
	seq uint64
	// followUp: the observation's first hint was delivered or failed; the next
	// send is its one follow-up.
	followUp bool
	// retrying: a retryable refusal came back; the next send waits only for the
	// spacing, not for the debounce.
	retrying          bool
	retryableRefusals int
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

	// watchTimer is the watcher's own periodic timer (see the file header);
	// watchGen invalidates a tick whose timer was replaced or stopped.
	watchTimer *time.Timer
	watchGen   uint64

	pending    map[string]*cliUsagePendingObservation
	noteSeq    uint64
	lastSentAt time.Time

	// rotated / rotationRefused / recoveryDue are per generation source.
	rotated         map[string]bool
	rotationRefused map[string]bool
	recoveryDue     map[string]bool
	rotationRetryAt time.Time
	// rotating tracks the startup rotation goroutine so a test reset can wait
	// for it instead of letting it write into the next test's cache.
	rotating sync.WaitGroup
	// noting tracks the goroutines that note a stamped Claude generation after
	// a merge (claudeStampAfterMerge), for the same reason.
	noting sync.WaitGroup
}

var cliUsagePropagator = &cliUsagePropagatorState{}

// cliUsagePropagatorRunning is true between startCLIUsagePropagator and
// stopCLIUsagePropagator: the one signal that this process is the agent, which
// is the only process allowed to advance Claude's generation.
var cliUsagePropagatorRunning atomic.Bool

// cliUsagePropagatorStarted reports whether this process runs the propagator.
func cliUsagePropagatorStarted() bool { return cliUsagePropagatorRunning.Load() }

func (p *cliUsagePropagatorState) initMapsLocked() {
	if p.pending == nil {
		p.pending = map[string]*cliUsagePendingObservation{}
	}
	if p.rotated == nil {
		p.rotated = map[string]bool{}
	}
	if p.rotationRefused == nil {
		p.rotationRefused = map[string]bool{}
	}
	if p.recoveryDue == nil {
		p.recoveryDue = map[string]bool{}
	}
}

// startCLIUsagePropagator is called from StartAgent. It rotates every generation
// source onto this process's epoch (retrying a refused write on the timer) and,
// as each commits, schedules its startup recovery observation. Observations
// noted before it runs were recorded and are sent through the normal schedule.
func startCLIUsagePropagator(cfg *Config) {
	p := cliUsagePropagator
	p.mu.Lock()
	if p.cfg != nil || p.stopped {
		p.mu.Unlock()
		return
	}
	p.initMapsLocked()
	p.cfg = cfg
	p.ctx, p.cancel = context.WithCancel(context.Background())
	cliUsagePropagatorRunning.Store(true)
	armNow := false
	for provider := range p.rotated {
		// A capture committed during boot already rotated this source.
		p.recoveryDue[provider] = true
		armNow = true
	}
	if armNow {
		p.armLocked(0)
	} else if len(p.pending) > 0 {
		p.armLocked(p.nextSendDelayLocked())
	}
	p.armWatchLocked()
	p.rotating.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.rotating.Done()
		p.rotate(cliUsageRotationFirstRetry)
	}()
}

// stopCLIUsagePropagator runs from gracefulShutdown. No hint is sent, and no
// watcher tick runs, after it.
func stopCLIUsagePropagator() {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	cliUsagePropagatorRunning.Store(false)
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timerGen++
	if p.watchTimer != nil {
		p.watchTimer.Stop()
	}
	p.watchGen++
	if p.cancel != nil {
		p.cancel()
	}
	for provider := range p.pending {
		logCLIUsageHint("dropped", provider)
		delete(p.pending, provider)
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
	p.initMapsLocked()
	prev := p.pending[provider]
	if prev != nil && prev.generation.Epoch == generation.Epoch && prev.generation.Counter >= generation.Counter {
		return // not newer than what is already pending
	}
	idle := prev == nil || prev.followUp
	p.noteSeq++
	p.pending[provider] = &cliUsagePendingObservation{generation: generation, notedAt: time.Now(), seq: p.noteSeq}
	if p.cfg != nil {
		d := p.nextSendDelayLocked()
		if idle && d > cliUsageHintDebounce {
			// Throttled by the spacing: kept, carrying the newest generation.
			logCLIUsageHint("deferred", provider)
		}
		p.armLocked(d)
	}
}

// noteCLIUsageGenerationRotated is called (once per process and provider) by
// the cache write that first commits this process's epoch. It runs inside that
// transaction, so it only schedules: the recovery check reads the cache from
// the timer.
func noteCLIUsageGenerationRotated(provider string) {
	p := cliUsagePropagator
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initMapsLocked()
	if p.rotated[provider] || p.stopped {
		return
	}
	p.rotated[provider] = true
	delete(p.rotationRefused, provider)
	if len(p.rotationRefused) == 0 {
		p.rotationRetryAt = time.Time{}
	}
	if p.cfg != nil {
		p.recoveryDue[provider] = true
		p.armLocked(0)
	}
}

// rotate attempts the epoch rotation of every source not yet rotated; a refused
// write books a retry after `retry`, then every cliUsageRotationRetryPeriod for
// the life of the process.
func (p *cliUsagePropagatorState) rotate(retry time.Duration) {
	p.mu.Lock()
	p.initMapsLocked()
	todo := make([]cliUsageGenerationSource, 0, len(cliUsageGenerationSources))
	for _, source := range cliUsageGenerationSources {
		if !p.rotated[source.provider] {
			todo = append(todo, source)
		}
	}
	p.mu.Unlock()

	refused := map[string]bool{}
	for _, source := range todo {
		if _, r := source.rotate(time.Now()); r {
			refused[source.provider] = true
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	retrying := false
	for provider := range refused {
		if p.rotated[provider] {
			continue // an unrelated write completed it meanwhile
		}
		p.rotationRefused[provider] = true
		retrying = true
		logCLIUsageHint("rotation_retry", provider)
	}
	if !retrying {
		return
	}
	p.rotationRetryAt = time.Now().Add(retry)
	p.armLocked(retry)
}

// nextSendDelayLocked is how long the next pending observation must wait: its
// trailing debounce (not for a follow-up or a retry), and the device-wide
// spacing since the last hint. Zero when one is due now.
func (p *cliUsagePropagatorState) nextSendDelayLocked() time.Duration {
	now := time.Now()
	var due time.Time
	for _, obs := range p.pending {
		d := obs.dueAt(now)
		if due.IsZero() || d.Before(due) {
			due = d
		}
	}
	if due.IsZero() {
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

func (o *cliUsagePendingObservation) dueAt(now time.Time) time.Time {
	if o.followUp || o.retrying {
		return now
	}
	return o.notedAt.Add(cliUsageHintDebounce)
}

// pickDueLocked chooses which pending observation goes at this boundary: among
// those past their debounce whose source has rotated, an unsent one before a
// follow-up, and the oldest first. "" when none qualifies; blocked names a due
// observation still waiting for its rotation.
func (p *cliUsagePropagatorState) pickDueLocked(now time.Time) (provider string, awaitingRotation bool) {
	var best *cliUsagePendingObservation
	for name, obs := range p.pending {
		if obs.dueAt(now).After(now) {
			continue
		}
		if !p.rotated[name] {
			awaitingRotation = true
			continue
		}
		if best == nil ||
			(best.followUp && !obs.followUp) ||
			(best.followUp == obs.followUp && obs.seq < best.seq) {
			best, provider = obs, name
		}
	}
	return provider, awaitingRotation
}

// armLocked (re)arms the one hint timer to fire after d, replacing any earlier
// one. A pending rotation retry that is due sooner keeps its own instant.
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

// fire is the hint timer callback: a due rotation retry, the startup recovery
// checks, then the pending hint.
func (p *cliUsagePropagatorState) fire(gen uint64) {
	p.mu.Lock()
	if p.stopped || gen != p.timerGen {
		p.mu.Unlock()
		return
	}
	if !p.rotationRetryAt.IsZero() && !time.Now().Before(p.rotationRetryAt) {
		p.rotationRetryAt = time.Time{}
		p.mu.Unlock()
		p.rotate(cliUsageRotationRetryPeriod)
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
	}
	var recoverable []cliUsageGenerationSource
	for _, source := range cliUsageGenerationSources {
		if p.recoveryDue[source.provider] {
			delete(p.recoveryDue, source.provider)
			recoverable = append(recoverable, source)
		}
	}
	p.mu.Unlock()

	for _, source := range recoverable {
		if g := source.recoveryGeneration(); g != nil {
			noteCLIUsageObservationAdvanced(source.provider, *g)
		}
	}
	p.sendDue()
}

// sendDue sends the due pending hint when every gate is open; otherwise it
// re-arms the timer for the next boundary.
func (p *cliUsagePropagatorState) sendDue() {
	p.mu.Lock()
	if p.stopped || len(p.pending) == 0 || p.cfg == nil {
		p.mu.Unlock()
		return
	}
	if d := p.nextSendDelayLocked(); d > 0 {
		p.armLocked(d)
		p.mu.Unlock()
		return
	}
	provider, awaitingRotation := p.pickDueLocked(time.Now())
	if provider == "" {
		if awaitingRotation {
			logCLIUsageHint("awaiting_rotation", "")
		}
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	if blocked := cliUsageHintBlocked(); blocked != "" {
		logCLIUsageHint(blocked, provider)
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}
	obs := p.pending[provider]
	cfg, generation, followUp, ctx := p.cfg, obs.generation, obs.followUp, p.ctx
	p.mu.Unlock()

	// Read outside mu: configPersistenceMu is held across config file writes.
	agentID, secret := cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		logCLIUsageHint("skipped_unregistered", provider)
		p.mu.Lock()
		p.armLocked(cliUsageHintSpacing)
		p.mu.Unlock()
		return
	}

	timestamp := time.Now().UnixMilli()
	outcome := sendCLIUsageObservedHint(ctx, fmt.Sprintf("%s/device/%s/cli-usage/observed", baseURL, agentID), cliUsageObservedHint{
		Timestamp:       timestamp,
		Signature:       generateHMAC(buildCLIUsageObservedSignedMessage(agentID, timestamp, provider, generation), secret),
		Provider:        provider,
		GenerationEpoch: generation.Epoch,
		Generation:      generation.Counter,
	})
	class := classifyCLIUsageHintOutcome(outcome)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSentAt = time.Now()
	current := p.pending[provider]
	if p.stopped || current == nil || current.generation != generation {
		// A newer observation arrived during the send: it keeps its own budget
		// and waits for the spacing. Only the log line describes this send.
		logCLIUsageHint(cliUsageHintSentLabel(class, followUp, outcome.status), provider)
		if !p.stopped && len(p.pending) > 0 {
			p.armLocked(p.nextSendDelayLocked())
		}
		return
	}
	switch class {
	case cliUsageHintAlreadyApplied:
		logCLIUsageHint("dropped_applied", provider)
		delete(p.pending, provider)
	case cliUsageHintRejected:
		logCLIUsageHint("dropped_rejected", provider)
		delete(p.pending, provider)
	case cliUsageHintRetryable:
		current.retryableRefusals++
		if current.retryableRefusals >= cliUsageHintMaxRetryable {
			logCLIUsageHint("dropped_retryable", provider)
			delete(p.pending, provider)
			break
		}
		// The follow-up is not spent: the server published nothing.
		logCLIUsageHint("refused_retryable", provider)
		current.retrying = true
	default:
		logCLIUsageHint(cliUsageHintSentLabel(class, followUp, outcome.status), provider)
		if followUp {
			delete(p.pending, provider)
			break
		}
		// Delivered or failed, the observation keeps exactly one follow-up.
		current.followUp, current.retrying = true, false
	}
	if len(p.pending) > 0 {
		p.armLocked(p.nextSendDelayLocked())
	}
}

// cliUsageHintSentLabel is the fixed label for a delivered or failed send.
func cliUsageHintSentLabel(class cliUsageHintClass, followUp bool, status int) string {
	switch {
	case class == cliUsageHintFailed:
		return fmt.Sprintf("failed_%d", status)
	case class != cliUsageHintDelivered:
		return "refused"
	case followUp:
		return "followup_sent"
	}
	return "sent"
}

// cliUsageHintBlocked names the device gate holding a hint back, or "".
func cliUsageHintBlocked() string {
	switch {
	case IsShutdownInProgress(), isDraining():
		return "deferred"
	case IsOffline():
		return "skipped_offline"
	}
	return ""
}

// armWatchLocked (re)arms the watcher one period out.
func (p *cliUsagePropagatorState) armWatchLocked() {
	if p.stopped {
		return
	}
	if p.watchTimer != nil {
		p.watchTimer.Stop()
	}
	p.watchGen++
	gen := p.watchGen
	p.watchTimer = time.AfterFunc(cliUsageWatchPeriod, func() { p.watch(gen) })
}

// watch is the watcher tick. It reads its gates under mu and releases it before
// any cache work: the tick's stamp runs a cache transaction, and
// noteCLIUsageGenerationRotated takes mu from inside one, so holding mu here
// would invert the lock order.
func (p *cliUsagePropagatorState) watch(gen uint64) {
	p.mu.Lock()
	if p.stopped || gen != p.watchGen {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	if !IsShutdownInProgress() {
		func() {
			defer func() { _ = recover() }()
			cliUsageWatchTick(time.Now(), !isDraining() && !IsOffline())
		}()
	}

	p.mu.Lock()
	if gen == p.watchGen {
		p.armWatchLocked()
	}
	p.mu.Unlock()
}

func logCLIUsageHint(label, provider string) {
	if provider == "" {
		fmt.Printf("%s[cli-usage] usage hint: %s%s\n", colorCyan, label, colorReset)
		return
	}
	fmt.Printf("%s[cli-usage] usage hint: %s provider=%s%s\n", colorCyan, label, provider, colorReset)
}

// resetCLIUsagePropagator restores the zero propagator in place (tests): a
// timer callback already running sees the bumped generations and does nothing.
func resetCLIUsagePropagator() {
	p := cliUsagePropagator
	p.rotating.Wait()
	p.noting.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
	}
	if p.watchTimer != nil {
		p.watchTimer.Stop()
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.timerGen++
	p.watchGen++
	cliUsagePropagatorRunning.Store(false)
	p.cfg, p.ctx, p.cancel, p.stopped, p.timer, p.watchTimer = nil, nil, nil, false, nil, nil
	p.pending, p.lastSentAt = nil, time.Time{}
	p.rotated, p.rotationRefused, p.recoveryDue, p.rotationRetryAt = nil, nil, nil, time.Time{}
}
