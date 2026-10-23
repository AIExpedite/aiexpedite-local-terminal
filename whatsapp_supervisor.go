// whatsapp_supervisor.go — one whatsmeow client per WhatsApp link, and the
// link's lifecycle on this computer.
//
// A link is (linkId, generation). The cloud bumps the generation on every
// pairing, re-link and disconnect; every report this agent sends carries the
// generation it holds, and a 409 WHATSAPP_LINK_FENCED answer means the cloud
// moved on: the session is logged out and wiped, never retried. That is what
// keeps an old agent process from resurrecting a disconnected link.
//
// State mapping (local/documents/features/whatsapp/WHATSAPP_INTEGRATION.md "Link states"):
//
//	link start accepted             → pairing
//	PairSuccess                     → connecting
//	Connected                       → syncingHistory (history pending) / connected
//	initial history done / 10 min   → connected
//	LoggedOut / StreamReplaced      → authExpired  (session wiped, no retry)
//	TemporaryBan / banned logout    → error WHATSAPP_ACCOUNT_RESTRICTED (no retry)
//	ClientOutdated / 405            → error WHATSAPP_CLIENT_OUTDATED (no retry)
//	30 min failing to connect       → error WHATSAPP_CONNECT_FAILING (keeps retrying)
//
// Restart: links.json is loaded at boot, so a restart resumes every link
// without a cloud command; GET /device/:agentId/whatsapp/links then reconciles
// (wipes a link the cloud no longer binds to this computer, finishes an unlink
// that was queued while the agent was down).
//
// Disconnect from cloud: while the user has the agent disconnected from the
// cloud (cfg.OfflineMode, the tray toggle) the supervisor is paused — no
// client runs and nothing is posted; links.json keeps every link so a
// reconnect resumes them.
//
// Drain and shutdown: a WhatsApp link registers NO drain work source — it
// must never hold up an agent update. Shutdown disconnects the clients,
// flushes what the deadline allows and leaves the session and the unflushed
// outbox on disk for the next start.
package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types/events"
)

// Persisted states and codes — pinned against shared-constants by
// whatsapp_contract_test.go.
const (
	waStatePairingRequired = "pairingRequired"
	waStatePairing         = "pairing"
	waStateConnecting      = "connecting"
	waStateSyncingHistory  = "syncingHistory"
	waStateConnected       = "connected"
	waStateAuthExpired     = "authExpired"
	waStateDisconnecting   = "disconnecting"
	waStateError           = "error"

	waErrAccountRestricted        = "WHATSAPP_ACCOUNT_RESTRICTED"
	waErrConnectFailing           = "WHATSAPP_CONNECT_FAILING"
	waErrClientOutdated           = "WHATSAPP_CLIENT_OUTDATED"
	waErrSecureStorageUnavailable = "WHATSAPP_SECURE_STORAGE_UNAVAILABLE"
	waErrLinkFenced               = "WHATSAPP_LINK_FENCED"

	waSyncKindState     = "state"
	waSyncKindHeartbeat = "heartbeat"
	waSyncKindPairing   = "pairing"
	waSyncKindBatch     = "batch"
	waSyncKindAck       = "ack"
	waAckUnlink         = "unlink"

	waPairQR        = "qr"
	waPairPhoneCode = "phoneCode"
)

// Timings. Vars so tests run them fast.
var (
	whatsAppHeartbeatInterval   = 60 * time.Second
	whatsAppHistoryQuiet        = 10 * time.Minute
	whatsAppConnectFailingAfter = 30 * time.Minute
	whatsAppConnectBackoffMin   = 1 * time.Second
	whatsAppConnectBackoffMax   = 5 * time.Minute
	whatsAppUploadBackoffMin    = 2 * time.Second
	whatsAppUploadBackoffMax    = 5 * time.Minute
	// A safety net only: every append wakes the uploader directly.
	whatsAppUploadIdlePoll    = 30 * time.Second
	whatsAppAuthPause         = 60 * time.Second
	whatsAppPairingPayloadTTL = 2 * time.Minute
	whatsAppLogoutTimeout     = 15 * time.Second
	whatsAppControlAttempts   = 6
	whatsAppAckAttempts       = 12
	whatsAppReconcileRetry    = 5 * time.Minute
	whatsAppUpdateDrainFlush  = 10 * time.Second
	// One auth-expiry drain round: the retry loop below re-arms it until
	// the outbox is empty.
	whatsAppAuthFlushTimeout = 2 * time.Minute
)

// backoffWithJitter doubles from min to max and adds up to 50% jitter.
func backoffWithJitter(attempt int, minD, maxD time.Duration) time.Duration {
	d := minD
	for i := 0; i < attempt && d < maxD; i++ {
		d *= 2
	}
	if d > maxD {
		d = maxD
	}
	return d + time.Duration(rand.Int64N(int64(d)/2+1))
}

/* --------------------------------------------------------------------------
   Supervisor
   -------------------------------------------------------------------------- */

type whatsAppSupervisor struct {
	mu        sync.Mutex
	transport whatsAppTransport
	ctx       context.Context
	cancel    context.CancelFunc
	links     map[string]*whatsAppLinkRunner
	started   bool
	stopped   bool
	// lifecycleMu serializes the link lifecycle: link start, cancel, unlink,
	// reconcile, wipe and cloud pause/resume. Each is a check followed by a
	// wipe/open and a map change; two interleaved could both see no runner,
	// pair two clients on the same stores and leave one untracked. Commands
	// arrive on concurrent Pub/Sub callbacks, so this lock is what keeps
	// "one link per computer" true. Wipes queued on other goroutines
	// (fence, pairing failure, authExpired) take it too: an asynchronous
	// wipe must never interleave with a re-link for the same link.
	// Never taken under mu.
	lifecycleMu sync.Mutex
	// paused: the user disconnected this agent from the cloud. pausedLinks
	// are the links held at the pause; persist keeps them in links.json.
	paused      bool
	pausedLinks []whatsAppLocalLink
	// pendingWipes are links whose deregistration cleanup failed; persist
	// keeps them in links.json (PendingWipe) so the next start retries.
	pendingWipes []whatsAppLocalLink
	// discardsInFlight counts wipes in flight (under mu), so shutdown never
	// exits halfway through deleting a session (key gone, directory still
	// there). A counter, not a WaitGroup: a fenced flush can start a discard
	// while shutdown is already waiting, and WaitGroup forbids Add then.
	discardsInFlight int
}

var globalWhatsAppSupervisor = &whatsAppSupervisor{}

// startWhatsAppSupervisor loads every local link and reconciles with the
// cloud. Safe to call once; later calls are no-ops. An agent that boots
// disconnected from the cloud starts paused: nothing runs until the user
// reconnects (followWhatsAppCloudConnection).
func startWhatsAppSupervisor(cfg *Config) {
	globalWhatsAppSupervisor.startWith(newHTTPWhatsAppTransport(cfg), IsOffline())
}

func (s *whatsAppSupervisor) start(transport whatsAppTransport) {
	s.startWith(transport, false)
}

func (s *whatsAppSupervisor) startWith(transport whatsAppTransport, paused bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.transport = transport
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if s.links == nil {
		s.links = map[string]*whatsAppLinkRunner{}
	}
	s.mu.Unlock()

	local, err := loadWhatsAppLocalLinks()
	if err != nil {
		logWhatsApp("-", "links.json unreadable: "+err.Error())
	}
	local, hadPending := s.retryPendingWipes(local)
	if paused {
		s.mu.Lock()
		s.paused, s.pausedLinks = true, local
		s.mu.Unlock()
		if hadPending {
			s.persist()
		}
		logWhatsApp("-", "disconnected from cloud; links paused")
		return
	}
	for _, l := range local {
		s.resume(l)
	}
	if hadPending {
		s.persist()
	}
	go s.reconcileLoop()
}

// retryPendingWipes finishes deregistration cleanups an earlier run could
// not complete. It returns the links still to resume; a pending-wipe link
// never is (its registration is gone). Cleanups that fail again stay in
// s.pendingWipes for the next start.
func (s *whatsAppSupervisor) retryPendingWipes(local []whatsAppLocalLink) (live []whatsAppLocalLink, hadPending bool) {
	var pending []whatsAppLocalLink
	for _, l := range local {
		if !l.PendingWipe {
			live = append(live, l)
			continue
		}
		hadPending = true
		if err := deleteWhatsAppLinkSecrets(l.LinkID); err != nil {
			logWhatsApp(l.LinkID, "deregistration cleanup failed again: "+err.Error())
			pending = append(pending, l)
		}
	}
	s.mu.Lock()
	s.pendingWipes = pending
	s.mu.Unlock()
	return live, hadPending
}

// followWhatsAppCloudConnection pauses or resumes the supervisor to match the
// tray's "Disconnect from cloud" state. It reads IsOffline() under the
// lifecycle lock, so toggles racing each other still settle on the last one.
func followWhatsAppCloudConnection() {
	s := globalWhatsAppSupervisor
	if IsOffline() {
		// The tray already reports Disconnected: stop new uploads,
		// heartbeats and state reports at once, even if a reconcile holds
		// the lifecycle lock across its HTTP fetch. The full stop below
		// follows once the lock is free.
		s.requestCloudPause()
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.setCloudConnectedLocked(!IsOffline())
}

// requestCloudPause marks the supervisor paused without the lifecycle lock,
// so posts stop even while a reconcile holds it across its fetch. It moves
// no runners: setCloudConnectedLocked completes the stop once the lock is
// free. Safe to call from the tray loop: it never blocks on the network.
func (s *whatsAppSupervisor) requestCloudPause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopped || s.paused {
		return
	}
	s.paused = true
}

func (s *whatsAppSupervisor) setCloudConnected(connected bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.setCloudConnectedLocked(connected)
}

// setCloudConnectedLocked: disconnected stops every client and its loops (no
// logout, no wipe, no flush — nothing may leave the computer); connected
// resumes the paused links and reconciles. Caller holds lifecycleMu.
func (s *whatsAppSupervisor) setCloudConnectedLocked(connected bool) {
	s.mu.Lock()
	// An eagerly paused disconnect (requestCloudPause ran while a reconcile
	// held this lock) still owes the full stop below: its runners are held,
	// so paused alone must not trip the early return.
	if !s.started || s.stopped || (s.paused == !connected && (connected || len(s.links) == 0)) {
		s.mu.Unlock()
		return
	}
	if !connected {
		s.paused = true
		runners := make([]*whatsAppLinkRunner, 0, len(s.links))
		for _, r := range s.links {
			runners = append(runners, r)
		}
		s.links = map[string]*whatsAppLinkRunner{}
		s.mu.Unlock()
		held := make([]whatsAppLocalLink, 0, len(runners))
		for _, r := range runners {
			held = append(held, r.localLink())
		}
		s.mu.Lock()
		s.pausedLinks = held
		s.mu.Unlock()
		for _, r := range runners {
			r.stop(false)
			_ = r.closeStores()
		}
		logWhatsApp("-", "disconnected from cloud; links paused")
		return
	}
	s.paused = false
	local := append([]whatsAppLocalLink(nil), s.pausedLinks...)
	s.mu.Unlock()
	logWhatsApp("-", "reconnected to cloud; resuming links")
	for _, l := range local {
		// An eager pause may still hold this link's runner (its full stop
		// is queued behind a reconcile): never pair a second client over it.
		if s.runner(l.LinkID) != nil {
			continue
		}
		s.resume(l)
	}
	s.mu.Lock()
	s.pausedLinks = nil
	s.mu.Unlock()
	go s.reconcileLoop()
}

func (s *whatsAppSupervisor) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// stopping reports that ShutdownAll started: background retries must hand
// the runners over instead of racing it.
func (s *whatsAppSupervisor) stopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *whatsAppSupervisor) runner(linkID string) *whatsAppLinkRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links[linkID]
}

// persistMu makes "snapshot the map, write links.json" one step: two
// interleaved persists could otherwise land an older snapshot last and re-list
// a link a concurrent wipe just removed.
var whatsAppPersistMu sync.Mutex

func (s *whatsAppSupervisor) persist() {
	whatsAppPersistMu.Lock()
	defer whatsAppPersistMu.Unlock()
	s.mu.Lock()
	out := make([]whatsAppLocalLink, 0, len(s.links)+len(s.pausedLinks))
	for _, r := range s.links {
		out = append(out, r.localLink())
	}
	for _, l := range s.pausedLinks {
		if _, running := s.links[l.LinkID]; !running {
			out = append(out, l)
		}
	}
	out = append(out, s.pendingWipes...)
	s.mu.Unlock()
	if err := saveWhatsAppLocalLinks(out); err != nil {
		logWhatsApp("-", "links.json not saved: "+err.Error())
	}
}

// reconcileLoop asks the cloud which link this computer should hold and
// retries until it has an answer.
func (s *whatsAppSupervisor) reconcileLoop() {
	for {
		if s.isPaused() || s.reconcileOnce() {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(whatsAppReconcileRetry):
		}
	}
}

func (s *whatsAppSupervisor) reconcileOnce() bool {
	// The fetch is network I/O with a 30 s timeout: never hold the lifecycle
	// lock across it, or a Disconnect waits behind it while uploads keep
	// leaving. Only the mutation step below needs the lock.
	s.lifecycleMu.Lock()
	if s.isPaused() {
		s.lifecycleMu.Unlock()
		return true // the reconnect starts a fresh reconcile
	}
	// Snapshot which links (and generations) predate the fetch: a link
	// start that completes while BoundLinks is in flight is absent from
	// the response through no fault of its own, and the stale response
	// must not act on it.
	s.mu.Lock()
	ctx := s.ctx
	pre := make(map[string]int64, len(s.links))
	for id, r := range s.links {
		pre[id] = r.generationNow()
	}
	s.mu.Unlock()
	s.lifecycleMu.Unlock()
	bound, status, err := s.transport.BoundLinks(ctx)
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.isPaused() {
		return true // disconnected mid-fetch: skip mutations; the reconnect reconciles
	}
	if err != nil {
		if status == 401 || status == 403 || status == 404 {
			return true // unregistered / revoked: nothing to reconcile against
		}
		return false
	}
	byID := map[string]whatsAppBoundLink{}
	for _, b := range bound {
		byID[b.LinkID] = b
	}
	s.mu.Lock()
	held := make([]*whatsAppLinkRunner, 0, len(s.links))
	for _, r := range s.links {
		held = append(held, r)
	}
	s.mu.Unlock()
	staleResponse := false
	for _, r := range held {
		if gen, ok := pre[r.linkID]; !ok || gen != r.generationNow() {
			// Created or re-linked while the fetch was in flight: the
			// response predates it. Leave it alone; the retry below
			// reconciles it against a fresh response.
			r.log("reconcile response predates this link; will reconcile again")
			staleResponse = true
			delete(byID, r.linkID)
			continue
		}
		b, ok := byID[r.linkID]
		switch {
		case !ok:
			r.log("not bound to this computer any more; wiping")
			s.discard(r, true)
		case b.State == waStateDisconnecting:
			s.unlinkLocked(r.linkID, b.Generation)
		case b.Generation > r.generationNow():
			r.log("superseded by a newer generation; wiping")
			s.discard(r, true)
		}
		delete(byID, r.linkID)
	}
	if staleResponse {
		return false
	}
	// A disconnect queued for a link this agent no longer holds locally: the
	// local cleanup is already done, so confirm it — but only once the
	// cleanup actually succeeded, or the ack strands session material.
	for _, b := range byID {
		if b.State == waStateDisconnecting && validWhatsAppLinkID(b.LinkID) {
			if err := deleteWhatsAppLinkSecrets(b.LinkID); err != nil {
				logWhatsApp(b.LinkID, "reconcile cleanup failed; unlink replay will retry")
				continue
			}
			go s.sendAck(b.LinkID, b.Generation)
		}
	}
	return true
}

// resume restarts a link from links.json.
func (s *whatsAppSupervisor) resume(l whatsAppLocalLink) {
	r := s.newRunner(l.LinkID, l.WorkspaceID, l.Generation)
	r.state = l.State
	r.historyComplete = l.HistoryComplete
	if err := r.open(false); err != nil {
		r.log("resume failed: " + err.Error())
		s.discard(r, false)
		return
	}
	if !r.client.HasSession() {
		// No paired identity: either the agent stopped mid-pairing (the QR
		// died with it) or the store was lost. Neither can continue here.
		next := waStateAuthExpired
		if l.State == waStatePairing || l.State == waStatePairingRequired {
			next = waStatePairingRequired
		}
		if depth, _ := r.outboxDepth(); next == waStateAuthExpired && depth > 0 {
			// An auth-expiry drain a pause or shutdown cut short: the
			// captured rows still upload before the wipe.
			s.mu.Lock()
			s.links[r.linkID] = r
			s.mu.Unlock()
			r.authExpired()
			return
		}
		r.setState(next, "")
		r.reportState(nil)
		s.discard(r, false)
		return
	}
	s.mu.Lock()
	s.links[r.linkID] = r
	s.mu.Unlock()
	r.startLoops()
	if r.terminalError == "" {
		go r.connectLoop()
	}
}

func (s *whatsAppSupervisor) newRunner(linkID, workspaceID string, generation int64) *whatsAppLinkRunner {
	ctx, cancel := context.WithCancel(s.ctx)
	return &whatsAppLinkRunner{
		sup: s, linkID: linkID, workspaceID: workspaceID, generation: generation,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), seenChats: map[string]bool{},
		state: waStatePairing,
	}
}

// discard stops a runner and deletes everything it had on this computer.
// logout=true first removes the companion from the phone's Linked devices.
// The caller holds lifecycleMu. It reports whether the local session
// material (outbox key, session directory) is actually gone: callers that
// confirm to the cloud must withhold their ack until it is.
func (s *whatsAppSupervisor) discard(r *whatsAppLinkRunner, logout bool) error {
	s.trackDiscard(1)
	defer s.trackDiscard(-1)
	return s.discardNow(r, logout)
}

// discardAsync is discard on its own goroutine. The in-flight count is taken
// BEFORE the goroutine starts, so a shutdown that is already waiting cannot
// miss it. The goroutine takes lifecycleMu: a wipe queued here (fence,
// pairing failure) must not run while a link start pairs the next
// generation, and discardNow's ownership check then spares it.
func (s *whatsAppSupervisor) discardAsync(r *whatsAppLinkRunner, logout bool) {
	s.trackDiscard(1)
	go func() {
		defer s.trackDiscard(-1)
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		s.discardNow(r, logout)
	}()
}

func (s *whatsAppSupervisor) trackDiscard(delta int) {
	s.mu.Lock()
	s.discardsInFlight += delta
	s.mu.Unlock()
}

// discardNow is the destructive half of discard. The caller holds
// lifecycleMu, so a wipe queued before a re-link runs either fully before
// the link start or fully after it — never through it. In the "after" case
// s.links already points at the new generation's runner, and the key, the
// directory and the map entry belong to it: deleting them would wipe the
// just-paired session, so they are left alone.
func (s *whatsAppSupervisor) discardNow(r *whatsAppLinkRunner, logout bool) error {
	r.stop(logout)
	_ = r.closeStores()
	s.mu.Lock()
	if current := s.links[r.linkID]; current != nil && current != r {
		s.mu.Unlock()
		r.log("wipe skipped: the link was relinked while this wipe was queued")
		return nil
	}
	s.mu.Unlock()
	keyErr := deleteWhatsAppOutboxKey(r.linkID)
	if keyErr != nil {
		r.log("outbox key not deleted: " + keyErr.Error())
	}
	dirErr := wipeWhatsAppLinkDir(r.linkID)
	if dirErr != nil {
		r.log("session directory not deleted: " + dirErr.Error())
	}
	err := errors.Join(keyErr, dirErr)
	s.mu.Lock()
	if s.links[r.linkID] == r {
		delete(s.links, r.linkID)
	}
	kept := s.pausedLinks[:0]
	for _, l := range s.pausedLinks {
		if l.LinkID != r.linkID {
			kept = append(kept, l)
		}
	}
	s.pausedLinks = kept
	if err != nil {
		// The runner is gone, so links.json keeps the link marked
		// PendingWipe and the next start retries the cleanup: an
		// asynchronous wipe (fence, pairing failure) has no caller that
		// would retry it.
		s.forgetPendingWipeLocked(r.linkID)
		l := r.localLink()
		l.PendingWipe = true
		s.pendingWipes = append(s.pendingWipes, l)
	}
	s.mu.Unlock()
	s.persist()
	return err
}

// forgetPendingWipeLocked drops a link's pending-wipe entry. The caller
// holds mu.
func (s *whatsAppSupervisor) forgetPendingWipeLocked(linkID string) {
	kept := s.pendingWipes[:0]
	for _, l := range s.pendingWipes {
		if l.LinkID != linkID {
			kept = append(kept, l)
		}
	}
	s.pendingWipes = kept
}

// dropPendingWipe forgets a pending wipe a replayed cleanup just finished and
// reports whether there was one.
func (s *whatsAppSupervisor) dropPendingWipe(linkID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.pendingWipes)
	s.forgetPendingWipeLocked(linkID)
	return len(s.pendingWipes) != before
}

// deleteWhatsAppLinkSecrets removes a link's local session material without
// a runner: the outbox key and the session directory. Absent material is
// success; anything else fails so the caller can withhold its ack until a
// replay retries the cleanup.
func deleteWhatsAppLinkSecrets(linkID string) error {
	return errors.Join(deleteWhatsAppOutboxKey(linkID), wipeWhatsAppLinkDir(linkID))
}

/* --------------------------------------------------------------------------
   Commands (whatsapp_commands.go parses and calls these)
   -------------------------------------------------------------------------- */

type whatsAppLinkStartParams struct {
	LinkID      string `json:"linkId"`
	Generation  int64  `json:"generation"`
	WorkspaceID string `json:"workspaceID"`
	Method      string `json:"method"`
	BrowserPub  string `json:"browserPub"`
	PhoneNumber string `json:"phoneNumber,omitempty"`
}

// linkStart begins pairing a (new or re-linked) generation. Any older
// session of the same link is logged out and wiped first: a re-link always
// pairs fresh. Serialized with every other lifecycle step (lifecycleMu).
func (s *whatsAppSupervisor) linkStart(p whatsAppLinkStartParams) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.isPaused() {
		logWhatsApp(p.LinkID, "link start ignored: disconnected from cloud")
		return
	}
	if existing := s.runner(p.LinkID); existing != nil {
		if existing.generationNow() >= p.Generation {
			existing.log("duplicate link start ignored")
			return
		}
		s.discard(existing, true)
	}
	// Another link held by this computer is superseded too: the cloud allows
	// one active link per computer, so a start for a different link means the
	// old one is over.
	s.mu.Lock()
	var others []*whatsAppLinkRunner
	for id, r := range s.links {
		if id != p.LinkID {
			others = append(others, r)
		}
	}
	s.mu.Unlock()
	for _, r := range others {
		s.discard(r, true)
	}

	r := s.newRunner(p.LinkID, p.WorkspaceID, p.Generation)
	if !whatsAppSecureStorageAvailable() {
		r.log("refused: no OS credential store")
		r.setState(waStateError, waErrSecureStorageUnavailable)
		r.reportState(nil)
		return
	}
	_ = wipeWhatsAppLinkDir(p.LinkID)
	_ = deleteWhatsAppOutboxKey(p.LinkID)
	// A pending wipe of this id would delete the session paired below.
	s.mu.Lock()
	s.forgetPendingWipeLocked(p.LinkID)
	s.mu.Unlock()
	if err := r.open(true); err != nil {
		r.log("pairing could not start: " + err.Error())
		r.setState(waStatePairingRequired, "")
		r.reportState(nil)
		s.discard(r, false)
		return
	}
	s.mu.Lock()
	s.links[r.linkID] = r
	s.mu.Unlock()
	r.setState(waStatePairing, "")
	s.persist()
	r.reportState(nil)
	r.startLoops()
	go r.pair(p.Method, p.BrowserPub, p.PhoneNumber)
}

// linkCancel abandons a pairing in progress.
func (s *whatsAppSupervisor) linkCancel(linkID string, generation int64) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	r := s.runner(linkID)
	if r == nil || r.generationNow() > generation {
		return
	}
	if st := r.currentState(); st != waStatePairing {
		return
	}
	r.log("pairing cancelled")
	s.discard(r, false)
}

// unlink logs the companion out on WhatsApp's side, wipes everything local
// and confirms with an ack carrying the disconnect's generation. Idempotent:
// a redelivered unlink for a link already gone just acks again. Fenced: an
// unlink older than the generation this computer holds (delayed or
// redelivered after a re-link) is ignored — it must never wipe the newer
// session.
func (s *whatsAppSupervisor) unlink(linkID string, generation int64) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.isPaused() {
		// Not acked: the cloud keeps the link disconnecting, and the
		// reconcile after a reconnect replays it.
		logWhatsApp(linkID, "unlink deferred: disconnected from cloud")
		return
	}
	s.unlinkLocked(linkID, generation)
}

func (s *whatsAppSupervisor) unlinkLocked(linkID string, generation int64) {
	if r := s.runner(linkID); r != nil {
		if r.generationNow() > generation {
			r.log("stale unlink ignored")
			return
		}
		r.log("unlinking")
		if err := s.discard(r, true); err != nil {
			// Not acked: the cloud keeps the link disconnecting, so the
			// unlink is replayed (command redelivery, reconcile) and the
			// cleanup retried. Acking now would finish the disconnect
			// while session material still sits on this computer.
			r.log("unlink not confirmed: local cleanup failed, will retry on replay")
			return
		}
	} else if err := deleteWhatsAppLinkSecrets(linkID); err != nil {
		logWhatsApp(linkID, "unlink not confirmed: local cleanup failed, will retry on replay")
		return
	} else if s.dropPendingWipe(linkID) {
		s.persist()
	}
	go s.sendAck(linkID, generation)
}

func (s *whatsAppSupervisor) sendAck(linkID string, generation int64) {
	payload := map[string]any{"v": 1, "linkId": linkID, "generation": generation, "kind": waSyncKindAck, "command": waAckUnlink}
	for attempt := 0; attempt < whatsAppAckAttempts; attempt++ {
		if s.isPaused() {
			return // the reconcile after a reconnect confirms it
		}
		res := s.transport.Post(s.ctx, payload)
		if res.Status/100 == 2 {
			logWhatsApp(linkID, "unlink confirmed")
			return
		}
		if res.Status == 400 || res.Status == 401 || res.Status == 403 || res.Status == 409 {
			logWhatsApp(linkID, "unlink ack refused: "+res.Body.Code)
			return
		}
		wait := backoffWithJitter(attempt, whatsAppUploadBackoffMin, whatsAppUploadBackoffMax)
		if res.RetryAfter > wait {
			wait = res.RetryAfter
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// resyncState re-reports state and a heartbeat now.
func (s *whatsAppSupervisor) resyncState(linkID string, generation int64) {
	r := s.runner(linkID)
	if r == nil || r.generationNow() != generation {
		return
	}
	r.reportState(nil)
	r.heartbeat()
}

// ShutdownAll disconnects every client and flushes the outboxes within ctx's
// deadline (capped at whatsAppUpdateDrainFlush). Files stay on disk.
func (s *whatsAppSupervisor) ShutdownAll(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	runners := make([]*whatsAppLinkRunner, 0, len(s.links))
	for _, r := range s.links {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	defer s.waitDiscards(ctx)
	if len(runners) == 0 {
		if s.cancel != nil {
			s.cancel()
		}
		return
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), whatsAppUpdateDrainFlush)
	defer cancel()
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok {
			var c2 context.CancelFunc
			flushCtx, c2 = context.WithDeadline(flushCtx, dl)
			defer c2()
		}
	}
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func(r *whatsAppLinkRunner) {
			defer wg.Done()
			// Stop the upload loop first, so the flush below is the only
			// uploader (two would send the same range twice).
			r.cancel()
			r.waitLoops()
			r.stopClientOnly()
			r.flush(flushCtx)
		}(r)
	}
	wg.Wait()
	if s.cancel != nil {
		s.cancel()
	}
	for _, r := range runners {
		_ = r.closeStores()
	}
	s.persist()
}

// waitDiscards waits for wipes already in flight, at most 5 s and never past
// the caller's own deadline (Windows gives the whole shutdown 2–4 s).
func (s *whatsAppSupervisor) waitDiscards(ctx context.Context) {
	deadline := time.Now().Add(5 * time.Second)
	if ctx != nil {
		if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
			deadline = dl
		}
	}
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := s.discardsInFlight
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WipeAll logs out and deletes every local link: the computer was
// deregistered, so nothing it held may outlive the registration. A link whose
// cleanup fails stays in links.json marked PendingWipe, so the next start
// retries it instead of orphaning its session material.
func (s *whatsAppSupervisor) WipeAll() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// Read before the discards below rewrite links.json: it also lists the
	// paused links, earlier pending wipes and links that failed to load.
	onDisk, err := loadWhatsAppLocalLinks()
	manifestLost := false
	if err != nil {
		// An unreadable links.json must not be replaced by a list that
		// forgets its links: recover their ids from the session
		// directories, and if even that fails leave the file as it is.
		logWhatsApp("-", "links.json unreadable during deregistration: "+err.Error())
		if onDisk, err = listWhatsAppLinkDirs(); err != nil {
			logWhatsApp("-", "session directories unreadable: "+err.Error())
			manifestLost = true
		}
	}
	s.mu.Lock()
	others := append(onDisk, s.pausedLinks...)
	s.pausedLinks, s.pendingWipes = nil, nil
	runners := make([]*whatsAppLinkRunner, 0, len(s.links))
	for _, r := range s.links {
		runners = append(runners, r)
	}
	s.mu.Unlock()
	var pending []whatsAppLocalLink
	done := map[string]bool{}
	for _, r := range runners {
		l := r.localLink()
		done[l.LinkID] = true
		if err := s.discard(r, true); err != nil {
			l.PendingWipe = true
			pending = append(pending, l)
		}
	}
	for _, l := range others {
		if done[l.LinkID] {
			continue
		}
		done[l.LinkID] = true
		if err := deleteWhatsAppLinkSecrets(l.LinkID); err != nil {
			l.PendingWipe = true
			pending = append(pending, l)
		}
	}
	for _, l := range pending {
		logWhatsApp(l.LinkID, "deregistration cleanup failed; retried on the next start")
	}
	s.mu.Lock()
	s.pendingWipes = pending
	s.mu.Unlock()
	if manifestLost {
		return
	}
	s.persist()
}

// whatsAppCapability is what systemInfo.go reports.
type whatsAppCapability struct {
	Supported     bool `json:"supported"`
	SecureStorage bool `json:"secureStorage"`
}

func currentWhatsAppCapability() *whatsAppCapability {
	return &whatsAppCapability{Supported: true, SecureStorage: whatsAppSecureStorageAvailable()}
}

/* --------------------------------------------------------------------------
   Runner — one link
   -------------------------------------------------------------------------- */

type whatsAppLinkRunner struct {
	sup         *whatsAppSupervisor
	linkID      string
	workspaceID string

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}

	mu              sync.Mutex
	generation      int64
	state           string
	errorCode       string
	terminalError   string // a no-retry error: the client is not reconnected
	historyComplete bool
	accountPhone    string
	accountName     string
	client          whatsAppClient
	outbox          *whatsAppOutbox
	pairSeq         int64
	connectedAt     time.Time
	lastHistoryAt   time.Time
	reconnectedAt   time.Time
	failingSince    time.Time
	reconnects      []time.Time
	lastEventAt     time.Time
	lastErrorAt     time.Time
	historySynced   int64
	liveSynced      int64
	lastAckedSeq    int64
	uploadPausedTil time.Time
	seenChats       map[string]bool
	stopped         bool
	loopsStarted    bool
	// loops tracks the upload and heartbeat goroutines, so stop() can wait
	// for them before the stores close: on Windows an outbox file still open
	// in a query cannot be deleted.
	loops sync.WaitGroup
}

func (r *whatsAppLinkRunner) log(event string) { logWhatsApp(r.linkID, event) }

func (r *whatsAppLinkRunner) generationNow() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

func (r *whatsAppLinkRunner) currentState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *whatsAppLinkRunner) localLink() whatsAppLocalLink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return whatsAppLocalLink{LinkID: r.linkID, WorkspaceID: r.workspaceID, Generation: r.generation, State: r.state, HistoryComplete: r.historyComplete}
}

func (r *whatsAppLinkRunner) setState(state, code string) {
	r.mu.Lock()
	r.state, r.errorCode = state, code
	if code != "" {
		r.lastErrorAt = time.Now()
	}
	r.mu.Unlock()
}

// open prepares the directory, the outbox and the client.
func (r *whatsAppLinkRunner) open(fresh bool) error {
	dir, err := ensureWhatsAppLinkDir(r.linkID)
	if err != nil {
		return err
	}
	ob, discarded, err := openWhatsAppOutbox(dir, r.linkID)
	if err != nil {
		return err
	}
	if discarded {
		r.log("outbox key missing; unreadable rows discarded")
	}
	client, err := newWhatsAppClient(dir)
	if err != nil {
		ob.Close()
		return err
	}
	if fresh && client.HasSession() {
		// A wiped directory cannot hold a session; refuse to reuse one.
		client.Close()
		ob.Close()
		return errors.New("stale session found in a fresh link directory")
	}
	client.AddEventHandler(r.handleEvent)
	r.mu.Lock()
	r.client, r.outbox = client, ob
	r.mu.Unlock()
	return nil
}

func (r *whatsAppLinkRunner) startLoops() {
	r.mu.Lock()
	if r.loopsStarted {
		r.mu.Unlock()
		return
	}
	r.loopsStarted = true
	r.loops.Add(2)
	r.mu.Unlock()
	go func() { defer r.loops.Done(); r.uploadLoop() }()
	go func() { defer r.loops.Done(); r.heartbeatLoop() }()
}

// waitLoops waits (bounded) for the upload and heartbeat goroutines to exit
// after the runner's context was cancelled.
func (r *whatsAppLinkRunner) waitLoops() {
	done := make(chan struct{})
	go func() { r.loops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		r.log("loops did not stop in time")
	}
}

// stop ends the runner's loops and its client.
func (r *whatsAppLinkRunner) stop(logout bool) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.stopped = true
	client := r.client
	r.mu.Unlock()
	r.cancel()
	r.waitLoops()
	if client == nil {
		return
	}
	if logout && client.HasSession() {
		ctx, cancel := context.WithTimeout(context.Background(), whatsAppLogoutTimeout)
		if !client.IsConnected() {
			_ = client.Connect()
		}
		if err := client.Logout(ctx); err != nil {
			r.log("logout not confirmed by WhatsApp; remove 'AI Expedite' from Linked devices on the phone")
		}
		cancel()
	}
	client.Disconnect()
}

func (r *whatsAppLinkRunner) stopClientOnly() {
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client != nil {
		client.Disconnect()
	}
}

func (r *whatsAppLinkRunner) closeStores() error {
	r.mu.Lock()
	client, ob := r.client, r.outbox
	r.client, r.outbox = nil, nil
	r.mu.Unlock()
	var errs []error
	if ob != nil {
		errs = append(errs, ob.Close())
	}
	if client != nil {
		errs = append(errs, client.Close())
	}
	return errors.Join(errs...)
}

// fence handles 409 WHATSAPP_LINK_FENCED: the cloud no longer accepts this
// generation. Terminal — logged out, wiped, never retried.
func (r *whatsAppLinkRunner) fence() {
	r.log("fenced by the cloud; wiping this session")
	r.sup.discardAsync(r, true)
}

/* -------------------------------- pairing -------------------------------- */

func (r *whatsAppLinkRunner) pair(method, browserPub, phone string) {
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return
	}
	qr, err := client.GetQRChannel(r.ctx)
	if err != nil {
		r.log("pairing could not start: " + err.Error())
		r.pairingFailed()
		return
	}
	if err := client.Connect(); err != nil {
		r.log("pairing connect failed: " + err.Error())
		r.pairingFailed()
		return
	}
	phoneRequested := false
	for {
		select {
		case <-r.ctx.Done():
			return
		case item, ok := <-qr:
			if !ok {
				return
			}
			switch {
			case item.Event == whatsmeow.QRChannelEventCode:
				if method == waPairPhoneCode {
					if phoneRequested {
						continue // the phone code stays valid; QR refreshes are not shown
					}
					phoneRequested = true
					code, err := client.PairPhone(r.ctx, phone)
					phone = ""
					if err != nil {
						r.log("phone-code pairing refused by WhatsApp")
						r.pairingFailed()
						return
					}
					r.postPairing(browserPub, waPairPhoneCode, code, whatsAppPairingPayloadTTL)
					continue
				}
				r.postPairing(browserPub, waPairQR, item.Code, item.Timeout)
			case item == whatsmeow.QRChannelSuccess:
				return // PairSuccess / Connected events carry the state
			default:
				r.log("pairing ended: " + item.Event)
				r.pairingFailed()
				return
			}
		}
	}
}

func (r *whatsAppLinkRunner) pairingFailed() {
	r.setState(waStatePairingRequired, "")
	r.reportState(nil)
	r.sup.discardAsync(r, false)
}

func (r *whatsAppLinkRunner) postPairing(browserPub, method, code string, ttl time.Duration) {
	if ttl <= 0 || ttl > whatsAppPairingPayloadTTL {
		ttl = whatsAppPairingPayloadTTL
	}
	expiresAt := time.Now().Add(ttl).UnixMilli()
	sealed, err := sealWhatsAppPairing(browserPub, whatsAppPairingPlaintext{Method: method, Code: code, ExpiresAt: expiresAt})
	if err != nil {
		r.log("pairing payload not sealed: " + err.Error())
		r.pairingFailed()
		return
	}
	r.mu.Lock()
	r.pairSeq++
	seq, gen := r.pairSeq, r.generation
	r.mu.Unlock()
	r.postControl(map[string]any{
		"v": 1, "linkId": r.linkID, "generation": gen, "kind": waSyncKindPairing,
		"seq": seq, "method": method, "ephemeralPub": sealed.EphemeralPub, "iv": sealed.IV,
		"ciphertext": sealed.Ciphertext, "expiresAt": expiresAt,
	})
	r.log("pairing payload sent")
}

/* -------------------------------- events --------------------------------- */

func (r *whatsAppLinkRunner) handleEvent(raw any) {
	r.mu.Lock()
	r.lastEventAt = time.Now()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return
	}
	switch evt := raw.(type) {
	case *events.PairSuccess:
		r.mu.Lock()
		r.accountPhone = evt.ID.User
		r.accountName = evt.BusinessName
		r.mu.Unlock()
		r.setState(waStateConnecting, "")
		r.sup.persist()
		r.reportState(nil)
		r.log("paired")
	case *events.Connected:
		r.mu.Lock()
		now := time.Now()
		if !r.connectedAt.IsZero() {
			r.reconnects = append(r.reconnects, now)
		}
		r.connectedAt, r.reconnectedAt, r.failingSince = now, now, time.Time{}
		next := waStateSyncingHistory
		if r.historyComplete {
			next = waStateConnected
		}
		r.mu.Unlock()
		r.setState(next, "")
		r.sup.persist()
		r.reportState(nil)
		r.log("connected")
	case *events.HistorySync:
		r.onHistorySync(evt.Data)
	case *events.Message:
		r.onLiveMessage(evt)
	case *events.GroupInfo:
		r.appendLive(normalizeWhatsAppGroupInfo(evt))
	case *events.LoggedOut:
		if evt.Reason == events.ConnectFailureUnknownLogout {
			r.terminal(waStateError, waErrAccountRestricted)
			return
		}
		r.authExpired()
	case *events.StreamReplaced:
		r.authExpired()
	case *events.TemporaryBan:
		r.terminal(waStateError, waErrAccountRestricted)
	case *events.ClientOutdated:
		r.terminal(waStateError, waErrClientOutdated)
	case *events.ConnectFailure:
		switch evt.Reason {
		case events.ConnectFailureLoggedOut, events.ConnectFailureMainDeviceGone:
			r.authExpired()
		case events.ConnectFailureTempBanned, events.ConnectFailureUnknownLogout:
			r.terminal(waStateError, waErrAccountRestricted)
		case events.ConnectFailureClientOutdated:
			r.terminal(waStateError, waErrClientOutdated)
		default:
			r.noteFailing()
		}
	case *events.Disconnected:
		r.noteFailing()
	}
}

func (r *whatsAppLinkRunner) noteFailing() {
	r.mu.Lock()
	if r.failingSince.IsZero() {
		r.failingSince = time.Now()
	}
	r.mu.Unlock()
}

// terminal records a no-retry error: the client is disconnected and not
// reconnected; the session stays so a person can still Disconnect cleanly.
func (r *whatsAppLinkRunner) terminal(state, code string) {
	r.mu.Lock()
	r.terminalError = code
	client := r.client
	r.mu.Unlock()
	r.setState(state, code)
	r.sup.persist()
	r.reportState(nil)
	r.log("stopped: " + code)
	if client != nil {
		go client.Disconnect()
	}
}

// authExpired: WhatsApp removed this companion. The session is useless; what
// is already in the outbox still uploads (same generation), then the link is
// wiped. Only "Link again" (a new generation) recovers.
func (r *whatsAppLinkRunner) authExpired() {
	r.mu.Lock()
	r.terminalError = waStateAuthExpired
	client := r.client
	r.mu.Unlock()
	r.setState(waStateAuthExpired, "")
	r.sup.persist()
	r.reportState(nil)
	r.log("authentication expired")
	// Counted as a wipe in flight from now: shutdown cancels the supervisor
	// context (ending the flush below) and then waits for the wipe.
	r.sup.trackDiscard(1)
	go func() {
		defer r.sup.trackDiscard(-1)
		// The flush is the only uploader from here on.
		r.cancel()
		r.waitLoops()
		if client != nil {
			client.Disconnect()
		}
		ctx, cancel := context.WithTimeout(r.sup.ctx, whatsAppAuthFlushTimeout)
		r.flush(ctx)
		cancel()
		// The session is dead but the outbox is device-authenticated, so
		// rows the bounded flush could not deliver stay queued: keep
		// draining instead of wiping captured messages with their key.
		// Only an empty outbox is wiped below. A closed one is not empty:
		// a cloud pause or shutdown closed the stores under this drain, the
		// rows are still on disk, and the next resume drains them.
		for attempt := 0; ; attempt++ {
			depth, open := r.outboxDepth()
			if !open {
				r.log("stores closed while draining; the outbox stays for the next start")
				return
			}
			if depth == 0 {
				break
			}
			if r.sup.ctx.Err() != nil || r.sup.stopping() {
				return // shutdown owns the runner now; files stay for next boot
			}
			if cur := r.sup.runner(r.linkID); cur != r {
				r.log("relinked while draining; the new generation owns cleanup")
				return
			}
			select {
			case <-r.sup.ctx.Done():
				return
			case <-time.After(backoffWithJitter(attempt, whatsAppUploadBackoffMin, whatsAppUploadBackoffMax)):
			}
			ctx, cancel := context.WithTimeout(r.sup.ctx, whatsAppAuthFlushTimeout)
			r.flush(ctx)
			cancel()
		}
		// Serialized like discardAsync: "Link again" may have paired the
		// next generation while the flush ran, and discardNow must spare it.
		r.sup.lifecycleMu.Lock()
		defer r.sup.lifecycleMu.Unlock()
		r.sup.discardNow(r, false)
	}()
}

// outboxDepth is the queued rows still needing delivery. open is false once
// the stores are closed: a closed outbox is not a drained one.
func (r *whatsAppLinkRunner) outboxDepth() (depth int64, open bool) {
	r.mu.Lock()
	ob := r.outbox
	r.mu.Unlock()
	if ob == nil {
		return 0, false
	}
	return ob.Depth(), true
}

// connectLoop connects a resumed link, backing off on failure. whatsmeow's own
// auto-reconnect takes over once connected.
func (r *whatsAppLinkRunner) connectLoop() {
	for attempt := 0; ; attempt++ {
		r.mu.Lock()
		client, terminal := r.client, r.terminalError
		r.mu.Unlock()
		if client == nil || terminal != "" || r.ctx.Err() != nil {
			return
		}
		if client.IsConnected() {
			return
		}
		r.setState(waStateConnecting, "")
		if err := client.Connect(); err == nil {
			return
		}
		r.noteFailing()
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(backoffWithJitter(attempt, whatsAppConnectBackoffMin, whatsAppConnectBackoffMax)):
		}
	}
}

func (r *whatsAppLinkRunner) normalizeContext() whatsAppNormalizeContext {
	r.mu.Lock()
	defer r.mu.Unlock()
	nc := whatsAppNormalizeContext{ReconnectedAt: r.reconnectedAt}
	if r.client != nil {
		nc.OwnJID = r.client.OwnJID()
	}
	return nc
}

// appendHistoryChunk stores records, waiting for the uploader when the
// outbox is at its caps. A chunk that alone exceeds the absolute caps is
// split: no drain could ever admit it whole, so retrying it unchanged would
// spin forever. Returns the admitted prefix length; a short return means the
// runner stopped (or one unsatisfiable record was skipped) and the caller
// must not count the rest as synced.
func (r *whatsAppLinkRunner) appendHistoryChunk(ob *whatsAppOutbox, records []whatsAppRecord) int {
	admitted := 0
	for admitted < len(records) {
		rest := records[admitted:]
		err := ob.Append(rest, true)
		if err == nil {
			return len(records)
		}
		if !errors.Is(err, errWhatsAppOutboxFull) {
			r.log("history chunk not stored: " + err.Error())
			return admitted
		}
		if len(rest) == 1 {
			// A single record the empty queue refuses cannot exist in
			// practice; wedging history on it would be worse than logging it.
			r.log("history record exceeds the outbox caps; skipped")
			return admitted + 1
		}
		if int64(len(rest)) > whatsAppOutboxMaxRecords || ob.Depth() == 0 {
			// Unsatisfiable whole: more records than the cap allows, or a
			// chunk the empty queue still refuses (its sealed bytes alone
			// pass the byte cap). Halving always terminates — a single
			// record fits an empty queue.
			half := len(rest) / 2
			if got := r.appendHistoryChunk(ob, rest[:half]); got < half {
				return admitted + got
			}
			admitted += half
			continue
		}
		// At the cap: wait for the uploader to make room, then retry the
		// same chunk — history consumption pauses, messages are not dropped.
		// Append refuses a chunk that would overshoot the caps, so the
		// queue never passes them however large one chunk is.
		r.kickUploader()
		select {
		case <-r.ctx.Done():
			return admitted
		case <-time.After(2 * time.Second):
		}
	}
	return admitted
}

// onHistorySync appends one chunk. When the outbox is at its cap this BLOCKS
// whatsmeow's event loop until the uploader makes room — pausing history
// consumption rather than dropping messages.
func (r *whatsAppLinkRunner) onHistorySync(data *waHistorySync.HistorySync) {
	if data == nil {
		return
	}
	switch data.GetSyncType() {
	case waHistorySync.HistorySync_INITIAL_BOOTSTRAP, waHistorySync.HistorySync_RECENT, waHistorySync.HistorySync_FULL:
	default:
		return // push names, status, non-blocking data: not messages
	}
	r.mu.Lock()
	client := r.client
	r.lastHistoryAt = time.Now()
	ob := r.outbox
	r.mu.Unlock()
	if client == nil || ob == nil {
		return
	}
	records, skipped := normalizeWhatsAppHistory(data, client.ParseWebMessage, r.normalizeContext())
	admitted := r.appendHistoryChunk(ob, records)
	whole := admitted == len(records)
	records = records[:admitted]
	r.mu.Lock()
	r.historySynced += int64(countWhatsAppMessages(records))
	r.mu.Unlock()
	if skipped > 0 {
		r.log("history chunk: some entries skipped")
	}
	r.kickUploader()
	if data.GetSyncType() != waHistorySync.HistorySync_FULL && data.GetProgress() >= 100 {
		if !whole {
			// A pause, shutdown or outbox failure cut the final chunk
			// short: its suffix was never queued, so the sync is not
			// complete and must not be persisted or reported as such.
			r.log("final history chunk only partly stored; history not marked complete")
			return
		}
		r.completeHistory()
	}
}

func (r *whatsAppLinkRunner) completeHistory() {
	r.mu.Lock()
	if r.historyComplete {
		r.mu.Unlock()
		return
	}
	r.historyComplete = true
	state := r.state
	r.mu.Unlock()
	if state == waStateSyncingHistory {
		r.setState(waStateConnected, "")
	}
	r.sup.persist()
	r.reportState(map[string]any{"historySyncComplete": true})
	r.log("history sync complete")
}

func (r *whatsAppLinkRunner) onLiveMessage(evt *events.Message) {
	records := normalizeWhatsAppMessage(evt, waSourceLive, r.normalizeContext())
	if len(records) == 0 {
		return
	}
	chat := records[0].ChatJid
	r.mu.Lock()
	first := !r.seenChats[chat]
	r.seenChats[chat] = true
	r.mu.Unlock()
	if first {
		name := ""
		if !evt.Info.IsGroup && !evt.Info.IsFromMe {
			name = evt.Info.PushName
		}
		records = append([]whatsAppRecord{whatsAppConversationRecord(evt.Info.MessageSource, name)}, records...)
	}
	r.appendLive(records)
}

func (r *whatsAppLinkRunner) appendLive(records []whatsAppRecord) {
	if len(records) == 0 {
		return
	}
	r.mu.Lock()
	ob := r.outbox
	r.mu.Unlock()
	if ob == nil {
		return
	}
	if err := ob.Append(records, false); err != nil {
		r.log("live message not stored: " + err.Error())
		return
	}
	r.mu.Lock()
	r.liveSynced += int64(countWhatsAppMessages(records))
	r.mu.Unlock()
	r.kickUploader()
}

func countWhatsAppMessages(records []whatsAppRecord) int {
	n := 0
	for _, rec := range records {
		if rec.Kind == waRecordMessage && rec.Op == waOpNew {
			n++
		}
	}
	return n
}

/* -------------------------------- uploads -------------------------------- */

func (r *whatsAppLinkRunner) kickUploader() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *whatsAppLinkRunner) uploadLoop() {
	attempt := 0
	for {
		wait := whatsAppUploadIdlePoll
		sent, outcome := r.uploadOnce(r.ctx)
		switch outcome {
		case whatsAppUploadRetry:
			wait = backoffWithJitter(attempt, whatsAppUploadBackoffMin, whatsAppUploadBackoffMax)
			if sent.RetryAfter > wait {
				wait = sent.RetryAfter
			}
			attempt++
		case whatsAppUploadSent:
			attempt = 0
			wait = 0
		case whatsAppUploadStop:
			return
		default:
			attempt = 0
		}
		if wait == 0 {
			if r.ctx.Err() != nil {
				return
			}
			continue
		}
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		case <-time.After(wait):
		}
	}
}

type whatsAppUploadOutcome int

const (
	whatsAppUploadIdle whatsAppUploadOutcome = iota
	whatsAppUploadSent
	whatsAppUploadRetry
	whatsAppUploadStop
)

// uploadOnce sends the oldest batch. 2xx acks; 400 drops only the batch's
// head record and retries the remainder (one bad record must not wedge the
// queue, and the rest of the batch may be valid); 401/403 pauses uploads;
// 409 fences; 429, 5xx and transport errors are retried with backoff.
func (r *whatsAppLinkRunner) uploadOnce(ctx context.Context) (whatsAppSyncResult, whatsAppUploadOutcome) {
	r.mu.Lock()
	ob, gen, paused := r.outbox, r.generation, time.Now().Before(r.uploadPausedTil)
	r.mu.Unlock()
	if ob == nil {
		return whatsAppSyncResult{}, whatsAppUploadStop
	}
	if paused || r.sup.isPaused() {
		return whatsAppSyncResult{}, whatsAppUploadIdle
	}
	batch, err := ob.Next()
	if err != nil {
		r.log("outbox read failed: " + err.Error())
		return whatsAppSyncResult{}, whatsAppUploadRetry
	}
	if batch == nil {
		return whatsAppSyncResult{}, whatsAppUploadIdle
	}
	res := r.sup.transport.Post(ctx, map[string]any{
		"v": 1, "linkId": r.linkID, "generation": gen, "kind": waSyncKindBatch,
		"seqFrom": batch.SeqFrom, "seqTo": batch.SeqTo, "records": batch.Records,
	})
	switch {
	case res.Status/100 == 2:
		acked := batch.SeqTo
		if res.Body.AckedSeq != nil {
			acked = min(*res.Body.AckedSeq, batch.SeqTo)
		}
		if err := ob.Ack(acked); err != nil {
			r.log("outbox ack failed: " + err.Error())
		}
		r.mu.Lock()
		r.lastAckedSeq = max(r.lastAckedSeq, acked)
		r.mu.Unlock()
		if acked < batch.SeqFrom {
			return res, whatsAppUploadRetry
		}
		return res, whatsAppUploadSent
	case res.Status == 400:
		// Only the head record is known bad: dropping the whole range
		// would silently lose up to 199 valid messages the cloud never
		// ingested. Drop one, retry the rest with backoff.
		r.log("batch refused as invalid (" + res.Body.Code + "); dropping head record")
		_ = ob.Ack(batch.SeqFrom)
		return res, whatsAppUploadRetry
	case res.Status == 401 || res.Status == 403:
		r.mu.Lock()
		r.uploadPausedTil = time.Now().Add(whatsAppAuthPause)
		r.mu.Unlock()
		return res, whatsAppUploadIdle
	case res.Status == 409:
		r.fence()
		return res, whatsAppUploadStop
	default:
		return res, whatsAppUploadRetry
	}
}

// flush uploads until the outbox is empty or ctx ends.
func (r *whatsAppLinkRunner) flush(ctx context.Context) {
	for ctx.Err() == nil {
		res, outcome := r.uploadOnce(ctx)
		switch outcome {
		case whatsAppUploadSent:
			continue
		case whatsAppUploadRetry:
			wait := max(res.RetryAfter, whatsAppUploadBackoffMin)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		default:
			return
		}
	}
}

/* ----------------------------- state reports ----------------------------- */

func (r *whatsAppLinkRunner) diagnostics() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	kept := r.reconnects[:0]
	for _, t := range r.reconnects {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.reconnects = kept
	d := map[string]any{
		"clientVersion":         whatsAppClientVersion(),
		"agentVersion":          Version,
		"reconnectCount24h":     len(kept),
		"historyMessagesSynced": r.historySynced,
		"liveMessagesSynced":    r.liveSynced,
		"lastAckedSeq":          r.lastAckedSeq,
	}
	if r.outbox != nil {
		d["outboxDepth"] = r.outbox.Depth()
	}
	if !r.lastEventAt.IsZero() {
		d["lastEventAt"] = r.lastEventAt.UnixMilli()
	}
	if r.errorCode != "" {
		d["lastErrorCode"] = r.errorCode
		d["lastErrorAt"] = r.lastErrorAt.UnixMilli()
	}
	return d
}

// reportState posts the current state (best effort, bounded retries).
func (r *whatsAppLinkRunner) reportState(extra map[string]any) {
	r.mu.Lock()
	payload := map[string]any{
		"v": 1, "linkId": r.linkID, "generation": r.generation, "kind": waSyncKindState, "state": r.state,
	}
	if r.errorCode != "" {
		payload["errorCode"] = r.errorCode
	}
	if r.accountPhone != "" {
		payload["accountPhone"] = r.accountPhone
	}
	if r.accountName != "" {
		payload["accountName"] = r.accountName
	}
	if r.historyComplete {
		payload["historySyncComplete"] = true
	}
	r.mu.Unlock()
	for k, v := range extra {
		payload[k] = v
	}
	payload["diagnostics"] = r.diagnostics()
	r.postControl(payload)
}

// postControl sends a non-batch report with a few retries. 409 fences.
func (r *whatsAppLinkRunner) postControl(payload map[string]any) {
	for attempt := 0; attempt < whatsAppControlAttempts; attempt++ {
		if r.sup.isPaused() {
			return
		}
		res := r.sup.transport.Post(r.sup.ctx, payload)
		switch {
		case res.Status/100 == 2:
			return
		case res.Status == 409:
			r.fence()
			return
		case res.Status == 400 || res.Status == 401 || res.Status == 403:
			r.log("report refused: " + res.Body.Code)
			return
		}
		wait := backoffWithJitter(attempt, whatsAppUploadBackoffMin, 30*time.Second)
		if res.RetryAfter > wait {
			wait = res.RetryAfter
		}
		select {
		case <-r.sup.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (r *whatsAppLinkRunner) heartbeat() {
	if r.sup.isPaused() {
		return
	}
	r.mu.Lock()
	gen := r.generation
	r.mu.Unlock()
	res := r.sup.transport.Post(r.ctx, map[string]any{
		"v": 1, "linkId": r.linkID, "generation": gen, "kind": waSyncKindHeartbeat, "diagnostics": r.diagnostics(),
	})
	switch {
	case res.Status == 409:
		r.fence()
	case res.Status/100 == 2:
		r.mu.Lock()
		r.uploadPausedTil = time.Time{} // credentials work again
		r.mu.Unlock()
	}
}

// heartbeatLoop sends the 60 s heartbeat and runs the two timers: history
// "quiet" completion and the 30-minute connect-failing report.
func (r *whatsAppLinkRunner) heartbeatLoop() {
	t := time.NewTicker(whatsAppHeartbeatInterval)
	defer t.Stop()
	r.heartbeat()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
		}
		r.heartbeat()
		r.checkTimers(time.Now())
	}
}

func (r *whatsAppLinkRunner) checkTimers(now time.Time) {
	r.mu.Lock()
	state, failingSince, code := r.state, r.failingSince, r.errorCode
	terminal := r.terminalError
	lastHistory := r.lastHistoryAt
	if lastHistory.IsZero() {
		lastHistory = r.connectedAt
	}
	connected := r.client != nil && r.client.IsConnected()
	r.mu.Unlock()
	if state == waStateSyncingHistory && !lastHistory.IsZero() && now.Sub(lastHistory) >= whatsAppHistoryQuiet {
		r.completeHistory()
	}
	if connected {
		r.mu.Lock()
		r.failingSince = time.Time{}
		r.mu.Unlock()
		if state == waStateError && code == waErrConnectFailing {
			next := waStateConnected
			if !r.localLink().HistoryComplete {
				next = waStateSyncingHistory
			}
			r.setState(next, "")
			r.reportState(nil)
		}
		return
	}
	// A terminal error (outdated client, restricted account) needs an
	// upgrade or account action, not a retry: failingSince may predate it
	// from an earlier transient failure, but the terminal code must stand.
	if terminal == "" && !failingSince.IsZero() && now.Sub(failingSince) >= whatsAppConnectFailingAfter && code != waErrConnectFailing {
		r.setState(waStateError, waErrConnectFailing)
		r.reportState(nil)
		r.log("still failing to connect; retrying")
	}
}
