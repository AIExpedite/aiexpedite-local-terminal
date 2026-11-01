package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Re-link carry-over (whatsapp_supervisor.go, "dormant"): the outbox, the
// identity table and the key belong to the link, not to a generation.

// generationCloud answers batches like whatsapp-service: 409 below the
// current generation, 503 while unhealthy, otherwise an ack.
type generationCloud struct {
	mu      sync.Mutex
	current int64
	healthy bool
}

func (g *generationCloud) set(current int64, healthy bool) {
	g.mu.Lock()
	g.current, g.healthy = current, healthy
	g.mu.Unlock()
}

func (g *generationCloud) install(h *supervisorHarness) {
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if int64(p["generation"].(float64)) < g.current {
			return whatsAppSyncResult{Status: 409, Body: whatsAppSyncResponse{Code: waErrLinkFenced}}
		}
		if !g.healthy {
			return whatsAppSyncResult{Status: 503}
		}
		to := int64(p["seqTo"].(float64))
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
	}
	h.transport.mu.Unlock()
}

// uploadedUnder returns the deviceSeq a message was posted with under a
// generation, or 0.
func uploadedUnder(h *supervisorHarness, generation float64, waMessageID string) float64 {
	for _, p := range h.transport.find(func(p map[string]any) bool {
		return p["kind"] == waSyncKindBatch && p["generation"].(float64) == generation
	}) {
		for _, rec := range p["records"].([]any) {
			if m := rec.(map[string]any); m["waMessageId"] == waMessageID {
				return m["deviceSeq"].(float64)
			}
		}
	}
	return 0
}

func queueOne(t *testing.T, h *supervisorHarness, c *fakeWAClient, id string) {
	t.Helper()
	c.emit(liveMsg(waPeer, waPeer, false, id, &waE2E.Message{Conversation: proto.String("queued")}))
	waitFor(t, "queued", func() bool {
		r := h.sup.runner("lnk_1")
		if r == nil {
			return false
		}
		depth, _ := r.outboxDepth()
		return depth > 0
	})
}

func dormantRunner(h *supervisorHarness) bool {
	r := h.sup.runner("lnk_1")
	return r != nil && r.isDormant()
}

// The must-fix gap: "Link again" while an expired session's queue had not
// drained. The rows are carried over and upload under the new generation,
// with the same key.
func TestWhatsAppSupervisorRelinkCarriesTheUndrainedQueueOver(t *testing.T) {
	h, c := pairedHarness(t)
	prevFlush := whatsAppAuthFlushTimeout
	whatsAppAuthFlushTimeout = 50 * time.Millisecond
	defer func() { whatsAppAuthFlushTimeout = prevFlush }()
	cloud := &generationCloud{current: 2}
	cloud.install(h)
	queueOne(t, h, c, "Q1")
	keyBefore, _ := h.ks.Get("lnk_1")
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "authExpired", func() bool { return lastState(h.transport) == waStateAuthExpired })
	time.Sleep(150 * time.Millisecond) // the bounded drain fails against the outage

	cloud.set(3, true) // "Link again": generation 3, and the cloud is back
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 3, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "the carried row under generation 3", func() bool { return uploadedUnder(h, 3, "Q1") > 0 })
	keyAfter, err := h.ks.Get("lnk_1")
	if err != nil || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("the re-link replaced the outbox key, so the carried rows could not have opened")
	}
	if r := h.sup.runner("lnk_1"); r == nil || r.isDormant() || r.generationNow() != 3 {
		t.Fatal("the re-link is not the link's runner")
	}
}

// The same, when the expired session's drain is fenced before the link start
// reaches this computer: the fence keeps the rows (the cloud still binds the
// link here, under the newer generation) and the link start adopts them
// without renumbering.
func TestWhatsAppSupervisorFencedDrainHandsTheQueueToTheRelink(t *testing.T) {
	h, c := pairedHarness(t)
	cloud := &generationCloud{current: 2}
	cloud.install(h)
	queueOne(t, h, c, "Q2")
	r := h.sup.runner("lnk_1")
	r.mu.Lock()
	next := r.outbox.next
	r.mu.Unlock()
	h.transport.mu.Lock()
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 3, State: waStatePairing}}
	h.transport.mu.Unlock()
	cloud.set(3, true) // the person clicked "Link again"; the start is still on its way
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "fenced to dormant", func() bool { return dormantRunner(h) })

	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 3, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "the carried row under generation 3", func() bool { return uploadedUnder(h, 3, "Q2") > 0 })
	if got := int64(uploadedUnder(h, 3, "Q2")); got >= next {
		t.Fatalf("the carried row was renumbered (%d, next was %d)", got, next)
	}
}

// A link start carries the cloud's replay watermark, which a re-link never
// resets: the new generation's records must start above it, or the cloud
// acks them as replays without writing them.
func TestWhatsAppSupervisorLinkStartStartsAboveTheCloudWatermark(t *testing.T) {
	h := newSupervisorHarness(t)
	// Bound here, so the startup reconcile cannot wipe the link mid-test.
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 4, State: waStatePairing}}
	h.sup.start(h.transport)
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 4, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub, LastAckedSeq: 700})
	waitFor(t, "client", func() bool { return h.client(0) != nil })
	h.client(0).emit(liveMsg(waPeer, waPeer, false, "W1", &waE2E.Message{Conversation: proto.String("x")}))
	isBatch := func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }
	waitFor(t, "batch", func() bool { return len(h.transport.find(isBatch)) > 0 })
	if b := h.transport.find(isBatch)[0]; b["seqFrom"].(float64) != 701 {
		t.Fatalf("seqFrom = %v, want 701", b["seqFrom"])
	}
}

// Every person keeps the identity the cloud stores them under across a
// re-link, even when the new session knows a mapping the old one did not.
func TestWhatsAppSupervisorRelinkKeepsEveryPersonsIdentity(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(liveMsg(waPeerLID, waPeerLID, false, "I1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "uploaded", func() bool { return uploadedUnder(h, 2, "I1") > 0 })
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })

	h.mu.Lock()
	h.alternates = map[types.JID]types.JID{waPeer: waPeerLID, waPeerLID: waPeer}
	h.mu.Unlock()
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 3, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "client", func() bool { return h.client(1) != nil })
	h.client(1).emit(liveMsg(waPeer, waPeer, false, "I2", &waE2E.Message{Conversation: proto.String("y")}))
	waitFor(t, "uploaded", func() bool { return uploadedUnder(h, 3, "I2") > 0 })
	for _, p := range h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }) {
		for _, rec := range p["records"].([]any) {
			if m := rec.(map[string]any); m["chatJid"] != waPeerLID.String() {
				t.Fatalf("a record changed identity across the re-link: %v", m)
			}
		}
	}
}

// A dormant link survives a restart without a client, and an unlink still
// wipes it and confirms.
func TestWhatsAppSupervisorDormantLinkSurvivesRestartAndUnlinkWipesIt(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })
	h.sup.ShutdownAll(context.Background())
	h.mu.Lock()
	clients := len(h.clients)
	h.mu.Unlock()

	next := &whatsAppSupervisor{}
	next.start(h.transport)
	defer next.ShutdownAll(context.Background())
	if r := next.runner("lnk_1"); r == nil || !r.isDormant() {
		t.Fatal("the dormant link was not held after a restart")
	}
	h.mu.Lock()
	opened := len(h.clients) - clients
	h.mu.Unlock()
	if opened != 0 {
		t.Fatal("a dormant link opened a WhatsApp client")
	}

	next.unlink("lnk_1", 3)
	dir, _ := whatsAppLinkDir("lnk_1")
	waitFor(t, "wiped", func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) && next.runner("lnk_1") == nil })
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the outbox key survived the unlink")
	}
	waitFor(t, "ack", func() bool {
		return len(h.transport.find(func(p map[string]any) bool {
			return p["kind"] == waSyncKindAck && p["generation"].(float64) == 3
		})) == 1
	})
}

// One link per computer: a start for another link ends a dormant one for good.
func TestWhatsAppSupervisorLinkStartForAnotherLinkWipesADormantOne(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_2", Generation: 1, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the superseded dormant link's files survived")
	}
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the superseded dormant link's key survived")
	}
	if h.sup.runner("lnk_1") != nil || h.sup.runner("lnk_2") == nil {
		t.Fatal("wrong links held")
	}
}

// fastDormantRecheck shortens the dormant re-check for one test. The
// supervisor reads it once at start, on the test goroutine, so the restore
// never races the loop.
func fastDormantRecheck(t *testing.T) {
	prev := whatsAppDormantRecheck
	whatsAppDormantRecheck = 20 * time.Millisecond
	t.Cleanup(func() { whatsAppDormantRecheck = prev })
}

// A dormant link sends nothing, so the cloud ending it (an erase, the access
// sweep for a removed member) reaches this computer through the periodic
// re-check, not only at the next agent start.
func TestWhatsAppSupervisorDormantRecheckWipesALinkTheCloudEnded(t *testing.T) {
	fastDormantRecheck(t)
	h, c := pairedHarness(t)
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })
	h.transport.mu.Lock()
	h.transport.bound = nil // erased: the binding is gone
	h.transport.mu.Unlock()
	dir, _ := whatsAppLinkDir("lnk_1")
	waitFor(t, "wiped by the re-check", func() bool {
		_, err := os.Stat(dir)
		return os.IsNotExist(err) && h.sup.runner("lnk_1") == nil
	})
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the outbox key outlived the link")
	}
}

// Still bound here (a re-link not yet started, an expired link nobody acted
// on): the re-check keeps it, and asks again later.
func TestWhatsAppSupervisorDormantRecheckKeepsALinkStillBoundHere(t *testing.T) {
	fastDormantRecheck(t)
	h, c := pairedHarness(t)
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "dormant", func() bool { return dormantRunner(h) })
	h.transport.mu.Lock()
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStateAuthExpired}}
	before := h.transport.boundCalls
	h.transport.mu.Unlock()
	waitFor(t, "two more re-checks", func() bool {
		h.transport.mu.Lock()
		defer h.transport.mu.Unlock()
		return h.transport.boundCalls >= before+2
	})
	if !dormantRunner(h) {
		t.Fatal("a link still bound here was not kept")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(filepath.Join(dir, whatsAppOutboxDBName)); err != nil {
		t.Fatalf("the kept link lost its outbox: %v", err)
	}
}

// 3. A reconcile that finds a LIVE runner superseded by a newer generation of
// the same link (a re-pair the link start has not reached yet) ends the
// session and keeps the outbox for that link start.
func TestWhatsAppSupervisorReconcileRetiresALiveSupersededRunner(t *testing.T) {
	h, c := pairedHarness(t)
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] == waSyncKindBatch {
			return whatsAppSyncResult{Status: 503} // keep the row queued
		}
		return whatsAppSyncResult{Status: 200}
	}
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 3, State: waStatePairing}}
	h.transport.mu.Unlock()
	queueOne(t, h, c, "S1")
	if !h.sup.reconcileOnce() {
		t.Fatal("reconcile did not settle")
	}
	if !dormantRunner(h) {
		t.Fatal("a superseded live runner was not retired")
	}
	if _, logouts := c.count(); logouts != 1 {
		t.Fatal("the superseded session was not logged out")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	ob, _, err := openWhatsAppOutbox(dir, "lnk_1")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if ob.Depth() == 0 {
		t.Fatal("the superseded generation's queued row was dropped")
	}
}
