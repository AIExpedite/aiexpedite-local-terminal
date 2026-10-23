package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

/* ------------------------------- fakes ---------------------------------- */

type fakeWAClient struct {
	mu        sync.Mutex
	handlers  []func(any)
	connected bool
	session   bool
	connects  int
	logouts   int
	closed    bool
	qr        chan whatsmeow.QRChannelItem
}

func (c *fakeWAClient) AddEventHandler(h func(any)) {
	c.mu.Lock()
	c.handlers = append(c.handlers, h)
	c.mu.Unlock()
}
func (c *fakeWAClient) emit(evt any) {
	c.mu.Lock()
	hs := append([]func(any){}, c.handlers...)
	c.mu.Unlock()
	for _, h := range hs {
		h(evt)
	}
}
func (c *fakeWAClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connects++
	c.connected = true
	return nil
}
func (c *fakeWAClient) Disconnect() {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
}
func (c *fakeWAClient) Logout(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logouts++
	c.session = false
	return nil
}
func (c *fakeWAClient) IsConnected() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.connected }
func (c *fakeWAClient) HasSession() bool  { c.mu.Lock(); defer c.mu.Unlock(); return c.session }
func (c *fakeWAClient) OwnJID() types.JID { return waMe }
func (c *fakeWAClient) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return c.qr, nil
}
func (c *fakeWAClient) PairPhone(context.Context, string) (string, error) { return "ABCD-EFGH", nil }
func (c *fakeWAClient) ParseWebMessage(chat types.JID, web *waWeb.WebMessageInfo) (*events.Message, error) {
	return liveMsg(chat, waPeer, false, web.GetKey().GetID(), web.GetMessage()), nil
}
func (c *fakeWAClient) Close() error { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *fakeWAClient) count() (connects, logouts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects, c.logouts
}

type fakeWATransport struct {
	mu      sync.Mutex
	posts   []map[string]any
	raw     []string
	respond func(payload map[string]any) whatsAppSyncResult
	bound   []whatsAppBoundLink
}

func (f *fakeWATransport) Post(_ context.Context, payload any) whatsAppSyncResult {
	b, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.mu.Lock()
	f.posts = append(f.posts, m)
	f.raw = append(f.raw, string(b))
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(m)
	}
	res := whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true}}
	if m["kind"] == waSyncKindBatch {
		to := int64(m["seqTo"].(float64))
		res.Body.AckedSeq = &to
	}
	return res
}

func (f *fakeWATransport) BoundLinks(context.Context) ([]whatsAppBoundLink, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bound, 200, nil
}

func (f *fakeWATransport) find(pred func(map[string]any) bool) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, p := range f.posts {
		if pred(p) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeWATransport) states() []string {
	var out []string
	for _, p := range f.find(func(p map[string]any) bool { return p["kind"] == waSyncKindState }) {
		out = append(out, p["state"].(string))
	}
	return out
}

func (f *fakeWATransport) allRaw() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.raw, "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func lastState(f *fakeWATransport) string {
	s := f.states()
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

// supervisorHarness wires a fresh supervisor to fakes in a temp root.
type supervisorHarness struct {
	sup       *whatsAppSupervisor
	transport *fakeWATransport
	ks        *memoryWhatsAppKeystore
	mu        sync.Mutex
	clients   []*fakeWAClient
	session   bool // HasSession of the next client created
}

func newSupervisorHarness(t *testing.T) *supervisorHarness {
	t.Helper()
	ks, _ := withWhatsAppTestEnv(t)
	h := &supervisorHarness{sup: &whatsAppSupervisor{}, transport: &fakeWATransport{}, ks: ks}
	prevFactory := newWhatsAppClient
	newWhatsAppClient = func(string) (whatsAppClient, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		c := &fakeWAClient{qr: make(chan whatsmeow.QRChannelItem, 4), session: h.session}
		h.clients = append(h.clients, c)
		return c, nil
	}
	prev := []time.Duration{whatsAppUploadBackoffMin, whatsAppUploadBackoffMax, whatsAppConnectBackoffMin, whatsAppUploadIdlePoll}
	whatsAppUploadBackoffMin, whatsAppUploadBackoffMax, whatsAppConnectBackoffMin, whatsAppUploadIdlePoll = time.Millisecond, 5*time.Millisecond, time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		h.sup.ShutdownAll(context.Background())
		newWhatsAppClient = prevFactory
		whatsAppUploadBackoffMin, whatsAppUploadBackoffMax, whatsAppConnectBackoffMin, whatsAppUploadIdlePoll = prev[0], prev[1], prev[2], prev[3]
	})
	return h
}

func (h *supervisorHarness) client(i int) *fakeWAClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.clients) {
		return nil
	}
	return h.clients[i]
}

func newBrowserKey(t *testing.T) (*ecdh.PrivateKey, string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k, b64url.EncodeToString(k.PublicKey().Bytes())
}

// pairedHarness drives a QR pairing to `connected`.
func pairedHarness(t *testing.T) (*supervisorHarness, *fakeWAClient) {
	h := newSupervisorHarness(t)
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStateConnected}}
	h.sup.start(h.transport)
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 2, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "client", func() bool { return h.client(0) != nil })
	c := h.client(0)
	c.emit(&events.PairSuccess{ID: types.NewJID("15550000001", types.DefaultUserServer)})
	c.mu.Lock()
	c.session = true
	c.mu.Unlock()
	c.emit(&events.Connected{})
	c.emit(&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(), Progress: proto.Uint32(100)}})
	waitFor(t, "connected", func() bool { return lastState(h.transport) == waStateConnected })
	return h, c
}

/* -------------------------------- tests --------------------------------- */

func TestWhatsAppSupervisorQRPairingIsSealedForTheBrowser(t *testing.T) {
	h := newSupervisorHarness(t)
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStatePairing}}
	h.sup.start(h.transport)
	browser, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 2, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "client", func() bool { return h.client(0) != nil })
	const qr = "2@SECRETQRREF,abcdefgh,ijklmnop,qrstuvwx"
	h.client(0).qr <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: qr, Timeout: time.Minute}
	waitFor(t, "pairing payload", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindPairing })) == 1
	})
	p := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindPairing })[0]
	if p["generation"].(float64) != 2 || p["seq"].(float64) != 1 || p["method"] != waPairQR {
		t.Fatalf("pairing post = %v", p)
	}
	got, err := openWhatsAppPairing(browser, whatsAppPairingSealed{EphemeralPub: p["ephemeralPub"].(string), IV: p["iv"].(string), Ciphertext: p["ciphertext"].(string)})
	if err != nil || got.Code != qr {
		t.Fatalf("browser could not open the payload: %+v %v", got, err)
	}
	if strings.Contains(h.transport.allRaw(), "SECRETQRREF") {
		t.Fatal("the QR ref left the computer in plaintext")
	}
	if states := h.transport.states(); len(states) == 0 || states[0] != waStatePairing {
		t.Fatalf("states = %v", states)
	}
}

func TestWhatsAppSupervisorPhoneCodePairing(t *testing.T) {
	h := newSupervisorHarness(t)
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 1, State: waStatePairing}}
	h.sup.start(h.transport)
	browser, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 1, WorkspaceID: "ws", Method: waPairPhoneCode, BrowserPub: pub, PhoneNumber: "15551234567"})
	waitFor(t, "client", func() bool { return h.client(0) != nil })
	h.client(0).qr <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@x,y,z,w", Timeout: time.Minute}
	h.client(0).qr <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@x2,y,z,w", Timeout: time.Minute}
	waitFor(t, "pairing payload", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindPairing })) >= 1
	})
	time.Sleep(50 * time.Millisecond)
	posts := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindPairing })
	if len(posts) != 1 {
		t.Fatalf("a phone code is requested once, got %d payloads", len(posts))
	}
	got, err := openWhatsAppPairing(browser, whatsAppPairingSealed{EphemeralPub: posts[0]["ephemeralPub"].(string), IV: posts[0]["iv"].(string), Ciphertext: posts[0]["ciphertext"].(string)})
	if err != nil || got.Code != "ABCD-EFGH" || got.Method != waPairPhoneCode {
		t.Fatalf("payload = %+v %v", got, err)
	}
	raw := h.transport.allRaw()
	if strings.Contains(raw, "15551234567") || strings.Contains(raw, "ABCD-EFGH") {
		t.Fatal("phone number or pair code left the computer in plaintext")
	}
}

func TestWhatsAppSupervisorStateMappingAndUpload(t *testing.T) {
	h, c := pairedHarness(t)
	states := h.transport.states()
	want := []string{waStatePairing, waStateConnecting, waStateSyncingHistory, waStateConnected}
	for i, s := range want {
		if i >= len(states) || states[i] != s {
			t.Fatalf("states = %v, want prefix %v", states, want)
		}
	}
	// The account's own number is reported once paired (for display).
	if p := h.transport.find(func(p map[string]any) bool { return p["state"] == waStateConnecting }); p[0]["accountPhone"] != "15550000001" {
		t.Fatalf("accountPhone = %v", p[0]["accountPhone"])
	}

	c.emit(liveMsg(waPeer, waPeer, false, "LIVE1", &waE2E.Message{Conversation: proto.String("hello there")}))
	waitFor(t, "batch", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch })) == 1
	})
	b := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch })[0]
	recs := b["records"].([]any)
	if len(recs) != 2 || recs[0].(map[string]any)["kind"] != waRecordConversation || recs[1].(map[string]any)["text"] != "hello there" {
		t.Fatalf("records = %v", recs)
	}
	if b["seqFrom"].(float64) != 1 || b["seqTo"].(float64) != 2 || b["generation"].(float64) != 2 {
		t.Fatalf("batch envelope = %v", b)
	}
	waitFor(t, "outbox drained", func() bool { r := h.sup.runner("lnk_1"); return r != nil && r.outbox.Depth() == 0 })
}

func TestWhatsAppSupervisorDisconnectedDoesNotFlapState(t *testing.T) {
	h, c := pairedHarness(t)
	before := len(h.transport.states())
	c.emit(&events.Disconnected{})
	time.Sleep(100 * time.Millisecond)
	if after := len(h.transport.states()); after != before {
		t.Fatalf("a transient disconnect reported state: %v", h.transport.states()[before:])
	}
	// 30 minutes of failing is reported, and the agent keeps retrying.
	r := h.sup.runner("lnk_1")
	c.Disconnect()
	r.checkTimers(time.Now().Add(whatsAppConnectFailingAfter + time.Minute))
	waitFor(t, "connect failing", func() bool { return lastState(h.transport) == waStateError })
	p := h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrConnectFailing })
	if len(p) == 0 {
		t.Fatal("WHATSAPP_CONNECT_FAILING not reported")
	}
}

func TestWhatsAppSupervisorLoggedOutWipesAndNeverRetries(t *testing.T) {
	h, c := pairedHarness(t)
	connects, _ := c.count()
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	waitFor(t, "authExpired", func() bool { return lastState(h.transport) == waStateAuthExpired })
	waitFor(t, "wiped", func() bool { return h.sup.runner("lnk_1") == nil })
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session directory survived authExpired")
	}
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("outbox key survived authExpired")
	}
	time.Sleep(50 * time.Millisecond)
	if after, _ := c.count(); after != connects {
		t.Fatal("reconnected after LoggedOut")
	}
}

func TestWhatsAppSupervisorTemporaryBanIsTerminal(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	waitFor(t, "restricted", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrAccountRestricted })) == 1
	})
	if r := h.sup.runner("lnk_1"); r == nil || r.terminalError != waErrAccountRestricted {
		t.Fatal("restricted link must stay (for a clean disconnect) but never reconnect")
	}
}

func TestWhatsAppSupervisorFencedBatchWipesTheSession(t *testing.T) {
	h, c := pairedHarness(t)
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] == waSyncKindBatch {
			return whatsAppSyncResult{Status: 409, Body: whatsAppSyncResponse{Code: waErrLinkFenced}}
		}
		return whatsAppSyncResult{Status: 200}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "F1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "fenced", func() bool { return h.sup.runner("lnk_1") == nil })
	if _, logouts := c.count(); logouts != 1 {
		t.Fatal("a fenced session is logged out")
	}
	links, _ := loadWhatsAppLocalLinks()
	if len(links) != 0 {
		t.Fatalf("links.json still lists %v", links)
	}
}

func TestWhatsAppSupervisorRetriesTransientUploadFailures(t *testing.T) {
	h, c := pairedHarness(t)
	var mu sync.Mutex
	fails := 2
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			if fails == 1 {
				return whatsAppSyncResult{Status: 503}
			}
			return whatsAppSyncResult{Status: 429, RetryAfter: time.Millisecond}
		}
		to := int64(p["seqTo"].(float64))
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "R1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "drained after retries", func() bool { r := h.sup.runner("lnk_1"); return r != nil && r.outbox.Depth() == 0 })
	batches := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch })
	if len(batches) != 3 {
		t.Fatalf("expected 2 retries + success, got %d", len(batches))
	}
	for _, b := range batches {
		if b["seqFrom"] != batches[0]["seqFrom"] {
			t.Fatal("a retry must resend the same range")
		}
	}
}

func TestWhatsAppSupervisorUnlinkLogsOutWipesAndAcks(t *testing.T) {
	h, c := pairedHarness(t)
	h.sup.unlink("lnk_1", 3)
	waitFor(t, "ack", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck })) == 1
	})
	ack := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck })[0]
	if ack["generation"].(float64) != 3 || ack["command"] != waAckUnlink {
		t.Fatalf("ack = %v", ack)
	}
	if _, logouts := c.count(); logouts != 1 {
		t.Fatal("unlink must log the companion out")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("session directory survived unlink")
	}
	// A redelivered unlink just acks again.
	h.sup.unlink("lnk_1", 3)
	waitFor(t, "second ack", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck })) == 2
	})
}

// An unlink older than the held generation (delayed or redelivered after a
// re-link) must not touch the newer session.
func TestWhatsAppSupervisorStaleUnlinkIsFenced(t *testing.T) {
	h, c := pairedHarness(t) // generation 2
	h.sup.unlink("lnk_1", 1)
	time.Sleep(50 * time.Millisecond)
	if h.sup.runner("lnk_1") == nil {
		t.Fatal("a stale unlink discarded the current runner")
	}
	if _, logouts := c.count(); logouts != 0 {
		t.Fatal("a stale unlink logged the current session out")
	}
	if acks := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck }); len(acks) != 0 {
		t.Fatalf("a stale unlink was acked: %v", acks)
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a stale unlink wiped the session directory")
	}
}

// Concurrent deliveries of the same link start pair exactly one client.
func TestWhatsAppSupervisorConcurrentLinkStartsAreSerialized(t *testing.T) {
	h := newSupervisorHarness(t)
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStatePairing}}
	h.sup.start(h.transport)
	_, pub := newBrowserKey(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 2, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
		}()
	}
	wg.Wait()
	if h.client(0) == nil || h.client(1) != nil {
		t.Fatal("concurrent link starts must open exactly one client")
	}
	if h.sup.runner("lnk_1") == nil {
		t.Fatal("the link start left no tracked runner")
	}
}

// Disconnect from cloud: nothing runs or posts; reconnect resumes the link.
func TestWhatsAppSupervisorPausesWhileDisconnectedFromCloud(t *testing.T) {
	h, c := pairedHarness(t)
	h.session = true
	h.sup.setCloudConnected(false)
	if c.IsConnected() {
		t.Fatal("the client stayed connected after Disconnect from cloud")
	}
	if h.sup.runner("lnk_1") != nil {
		t.Fatal("a runner survived the pause")
	}
	if l, err := loadWhatsAppLocalLinks(); err != nil || len(l) != 1 {
		t.Fatalf("links.json must keep the paused link: %v %v", l, err)
	}
	before := len(h.transport.find(func(map[string]any) bool { return true }))
	h.sup.unlink("lnk_1", 3) // deferred while paused
	time.Sleep(50 * time.Millisecond)
	if after := len(h.transport.find(func(map[string]any) bool { return true })); after != before {
		t.Fatalf("%d posts left the computer while disconnected from cloud", after-before)
	}
	if _, logouts := c.count(); logouts != 0 {
		t.Fatal("an unlink ran while disconnected from cloud")
	}

	h.sup.setCloudConnected(true)
	waitFor(t, "resumed client connects", func() bool {
		c2 := h.client(1)
		if c2 == nil {
			return false
		}
		n, _ := c2.count()
		return n == 1
	})
	if h.sup.runner("lnk_1") == nil {
		t.Fatal("reconnect did not resume the link")
	}
}

// An agent that boots disconnected from cloud opens nothing until reconnect.
func TestWhatsAppSupervisorBootsPausedWhenOffline(t *testing.T) {
	h := newSupervisorHarness(t)
	if _, err := ensureWhatsAppLinkDir("lnk_boot"); err != nil {
		t.Fatal(err)
	}
	_ = saveWhatsAppLocalLinks([]whatsAppLocalLink{{LinkID: "lnk_boot", WorkspaceID: "ws", Generation: 5, State: waStateConnected, HistoryComplete: true}})
	h.session = true
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_boot", WorkspaceID: "ws", Generation: 5, State: waStateConnected}}
	h.sup.startWith(h.transport, true)
	time.Sleep(50 * time.Millisecond)
	if h.client(0) != nil || len(h.transport.find(func(map[string]any) bool { return true })) != 0 {
		t.Fatal("an offline boot resumed a link or posted")
	}
	h.sup.setCloudConnected(true)
	waitFor(t, "resumed", func() bool { return h.sup.runner("lnk_boot") != nil && h.client(0) != nil })
}

func TestWhatsAppSupervisorResumesFromLinksJSONAtBoot(t *testing.T) {
	h := newSupervisorHarness(t)
	if _, err := ensureWhatsAppLinkDir("lnk_boot"); err != nil {
		t.Fatal(err)
	}
	_ = saveWhatsAppLocalLinks([]whatsAppLocalLink{{LinkID: "lnk_boot", WorkspaceID: "ws", Generation: 5, State: waStateConnected, HistoryComplete: true}})
	h.session = true
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_boot", WorkspaceID: "ws", Generation: 5, State: waStateConnected}}
	h.sup.start(h.transport)
	waitFor(t, "connect", func() bool {
		c := h.client(0)
		n, _ := 0, 0
		if c != nil {
			n, _ = c.count()
		}
		return n == 1
	})
	h.client(0).emit(&events.Connected{})
	waitFor(t, "connected (history already done)", func() bool { return lastState(h.transport) == waStateConnected })
	if h.sup.runner("lnk_boot") == nil {
		t.Fatal("a bound link must survive reconcile")
	}
}

func TestWhatsAppSupervisorReconcileWipesAnUnboundLink(t *testing.T) {
	h := newSupervisorHarness(t)
	if _, err := ensureWhatsAppLinkDir("lnk_gone"); err != nil {
		t.Fatal(err)
	}
	_ = saveWhatsAppLocalLinks([]whatsAppLocalLink{{LinkID: "lnk_gone", WorkspaceID: "ws", Generation: 1, State: waStateConnected}})
	h.session = true
	h.sup.start(h.transport) // bound: none
	waitFor(t, "wiped", func() bool { return h.sup.runner("lnk_gone") == nil })
	dir, _ := whatsAppLinkDir("lnk_gone")
	waitFor(t, "dir removed", func() bool { _, err := os.Stat(dir); return os.IsNotExist(err) })
	waitFor(t, "links.json rewritten", func() bool { l, err := loadWhatsAppLocalLinks(); return err == nil && len(l) == 0 })
	time.Sleep(20 * time.Millisecond) // let the last persist's rename settle before TempDir cleanup
}

func TestWhatsAppSupervisorRefusesWithoutSecureStorage(t *testing.T) {
	h := newSupervisorHarness(t)
	h.ks.unavailable = true
	h.sup.start(h.transport)
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 1, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "refused", func() bool { return lastState(h.transport) == waStateError })
	if p := h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrSecureStorageUnavailable }); len(p) != 1 {
		t.Fatal("WHATSAPP_SECURE_STORAGE_UNAVAILABLE not reported")
	}
	if h.client(0) != nil {
		t.Fatal("a client was created without secure storage")
	}
}

func TestWhatsAppSupervisorDuplicateLinkStartIgnored(t *testing.T) {
	h, _ := pairedHarness(t)
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 2, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	time.Sleep(50 * time.Millisecond)
	if h.client(1) != nil {
		t.Fatal("a redelivered link start re-paired")
	}
}

// No message content, JID or pairing secret reaches the agent log.
func TestWhatsAppSupervisorLogsCarryNoContent(t *testing.T) {
	var mu sync.Mutex
	var captured strings.Builder
	prevSink := whatsAppLogSink
	whatsAppLogSink = func(line string) {
		mu.Lock()
		captured.WriteString(line)
		captured.WriteByte('\n')
		mu.Unlock()
	}
	t.Cleanup(func() { whatsAppLogSink = prevSink })

	h, c := pairedHarness(t)
	c.emit(liveMsg(waPeer, waPeer, false, "LOG1", &waE2E.Message{Conversation: proto.String("TOP SECRET BODY")}))
	waitFor(t, "uploaded", func() bool { r := h.sup.runner("lnk_1"); return r != nil && r.outbox.Depth() == 0 })
	c.emit(&events.LoggedOut{})
	waitFor(t, "wiped", func() bool { return h.sup.runner("lnk_1") == nil })

	mu.Lock()
	out := captured.String()
	mu.Unlock()
	for _, leak := range []string{"TOP SECRET BODY", waPeer.User, "15550000001"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "link=lnk_1") {
		t.Fatalf("expected link-scoped log lines, got:\n%s", out)
	}
}

// Shutdown stops the upload loop and then flushes what is pending, once.
func TestWhatsAppSupervisorShutdownFlushesThePendingOutbox(t *testing.T) {
	h, c := pairedHarness(t)
	var mu sync.Mutex
	healthy := false
	var acked int
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		mu.Lock()
		defer mu.Unlock()
		if !healthy {
			return whatsAppSyncResult{Status: 503}
		}
		acked++
		to := int64(p["seqTo"].(float64))
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "S1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "a refused upload", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch })) >= 1
	})
	r := h.sup.runner("lnk_1")
	if r.outbox.Depth() == 0 {
		t.Fatal("expected a pending outbox")
	}
	mu.Lock()
	healthy = true
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.sup.ShutdownAll(ctx)
	mu.Lock()
	defer mu.Unlock()
	if acked != 1 {
		t.Fatalf("the pending range must be flushed exactly once, got %d accepted uploads", acked)
	}
}

// A flush fenced (409) during shutdown starts a discard while shutdown is
// already waiting; shutdown must wait for it rather than race it.
func TestWhatsAppSupervisorShutdownWaitsForAFencedFlushWipe(t *testing.T) {
	h, c := pairedHarness(t)
	var mu sync.Mutex
	fenced := false
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		mu.Lock()
		defer mu.Unlock()
		if fenced {
			return whatsAppSyncResult{Status: 409, Body: whatsAppSyncResponse{Code: waErrLinkFenced}}
		}
		return whatsAppSyncResult{Status: 503}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "F2", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "a refused upload", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch })) >= 1
	})
	mu.Lock()
	fenced = true
	mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.sup.ShutdownAll(ctx)
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("shutdown returned before the fenced session was wiped")
	}
}

// A wipe that never finishes cannot hold shutdown past the caller's deadline.
func TestWhatsAppSupervisorWaitDiscardsHonoursTheDeadline(t *testing.T) {
	s := &whatsAppSupervisor{}
	s.trackDiscard(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.waitDiscards(ctx)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("waited %v past a 100ms deadline", took)
	}
	s.trackDiscard(-1)
	start = time.Now()
	s.waitDiscards(context.Background())
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("nothing in flight must not wait")
	}
}
