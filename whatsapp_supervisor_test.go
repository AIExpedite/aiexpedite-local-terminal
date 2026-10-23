package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
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
func (c *fakeWAClient) IsConnected() bool            { c.mu.Lock(); defer c.mu.Unlock(); return c.connected }
func (c *fakeWAClient) HasSession() bool             { c.mu.Lock(); defer c.mu.Unlock(); return c.session }
func (c *fakeWAClient) OwnJID() types.JID            { return waMe }
func (c *fakeWAClient) PNForLID(types.JID) types.JID { return types.EmptyJID }
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
	onBound func()
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
	if f.onBound != nil {
		f.onBound()
	}
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

// A duplicate client on the same session is not a logout: the session and
// the outbox key stay, and this client stops instead of fighting it.
func TestWhatsAppSupervisorStreamReplacedKeepsTheSession(t *testing.T) {
	h, c := pairedHarness(t)
	connects, _ := c.count()
	c.emit(&events.StreamReplaced{})
	waitFor(t, "stopped", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrConnectFailing })) == 1
	})
	time.Sleep(50 * time.Millisecond)
	r := h.sup.runner("lnk_1")
	if r == nil || r.terminalError != waErrConnectFailing {
		t.Fatal("a replaced stream must keep the link and not reconnect")
	}
	if lastState(h.transport) == waStateAuthExpired {
		t.Fatal("a replaced stream is not an expired session")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session directory removed: %v", err)
	}
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatalf("outbox key removed: %v", err)
	}
	if after, logouts := c.count(); after != connects || logouts != 0 {
		t.Fatalf("connects %d→%d logouts %d after StreamReplaced", connects, after, logouts)
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

// A wipe queued before a re-link (fenced generation, failed pairing) runs
// either fully before the link start or fully after it — never through it.
// In the "after" case the queued wipe must spare the new generation's
// session entirely.
func TestWhatsAppSupervisorStaleAsyncWipeSparesTheRelink(t *testing.T) {
	h, _ := pairedHarness(t) // generation 2
	old := h.sup.runner("lnk_1")
	if old == nil {
		t.Fatal("no runner to wipe")
	}
	// The queued wipe serializes behind the lifecycle lock: while the test
	// holds it, the wipe must not touch the runner.
	h.sup.lifecycleMu.Lock()
	h.sup.discardAsync(old, true) // the generation-2 wipe, queued
	deadline := time.Now().Add(500 * time.Millisecond)
	for h.sup.runner("lnk_1") == old && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.sup.runner("lnk_1") != old {
		h.sup.lifecycleMu.Unlock()
		t.Fatal("an async wipe ran through the held lifecycle lock")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); err != nil {
		h.sup.lifecycleMu.Unlock()
		t.Fatal("an async wipe deleted the session while blocked on the lifecycle lock")
	}
	h.sup.lifecycleMu.Unlock()
	// Re-link: whatever order the queued wipe and the link start settle in,
	// the new generation's session survives.
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 3, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	next := h.sup.runner("lnk_1")
	if next == nil || next == old {
		t.Fatal("the re-link left no new runner")
	}
	waitFor(t, "stale wipe done", func() bool {
		h.sup.mu.Lock()
		defer h.sup.mu.Unlock()
		return h.sup.discardsInFlight == 0
	})
	if h.sup.runner("lnk_1") != next {
		t.Fatal("a stale queued wipe removed the relinked runner")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a stale queued wipe deleted the relinked session directory")
	}
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatal("a stale queued wipe deleted the relinked outbox key")
	}
}

// failingSince may predate a terminal error (a transient failure, then an
// outdated client or a restricted account). The connect-failing timer must
// not overwrite the terminal code with WHATSAPP_CONNECT_FAILING: recovery
// needs an upgrade or account action, not a retry.
func TestWhatsAppSupervisorTerminalErrorSurvivesConnectFailingTimer(t *testing.T) {
	h, c := pairedHarness(t)
	c.emit(&events.Disconnected{})
	time.Sleep(50 * time.Millisecond) // let noteFailing record failingSince
	c.emit(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	waitFor(t, "restricted", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrAccountRestricted })) == 1
	})
	r := h.sup.runner("lnk_1")
	if r == nil {
		t.Fatal("a restricted link must stay for a clean disconnect")
	}
	// The terminal path disconnects asynchronously: only once the client is
	// down does the timer see the link as not connected.
	waitFor(t, "client down", func() bool { return !c.IsConnected() })
	r.checkTimers(time.Now().Add(whatsAppConnectFailingAfter + time.Minute))
	time.Sleep(100 * time.Millisecond)
	if got := h.transport.find(func(p map[string]any) bool { return p["errorCode"] == waErrConnectFailing }); len(got) != 0 {
		t.Fatalf("a terminal code was overwritten: %v", got)
	}
	if r.terminalError != waErrAccountRestricted {
		t.Fatalf("terminalError = %q", r.terminalError)
	}
}

// Disconnecting while a reconcile holds the lifecycle lock must still stop
// uploads at once: the tray already reports Disconnected, so no batch,
// heartbeat or state report may leave afterwards. The eager pause marks the
// supervisor paused without the lock; the full stop follows once the lock
// is free.
func TestWhatsAppSupervisorEagerPauseStopsPostsDuringReconcile(t *testing.T) {
	h, c := pairedHarness(t)
	waitFor(t, "drained", func() bool {
		if r := h.sup.runner("lnk_1"); r != nil {
			return r.outbox.Depth() == 0
		}
		return false
	})
	time.Sleep(100 * time.Millisecond)
	before := len(h.transport.find(func(map[string]any) bool { return true }))

	// A reconcile in flight holds the lifecycle lock across its HTTP fetch.
	h.sup.lifecycleMu.Lock()
	h.sup.requestCloudPause()
	if !h.sup.isPaused() {
		h.sup.lifecycleMu.Unlock()
		t.Fatal("requestCloudPause must mark the supervisor paused without the lifecycle lock")
	}
	c.emit(liveMsg(waPeer, waPeer, false, "E1", &waE2E.Message{Conversation: proto.String("held")}))
	// The full stop cannot run yet; uploads must still stay silent.
	time.Sleep(200 * time.Millisecond)
	if after := len(h.transport.find(func(map[string]any) bool { return true })); after != before {
		h.sup.lifecycleMu.Unlock()
		t.Fatalf("%d posts left the computer after Disconnect while a reconcile held the lock", after-before)
	}
	if r := h.sup.runner("lnk_1"); r == nil || r.outbox.Depth() == 0 {
		h.sup.lifecycleMu.Unlock()
		t.Fatal("the message must stay queued until the full stop or reconnect")
	}
	h.sup.lifecycleMu.Unlock()

	// The queued full transition still completes the stop (the eager flag must
	// not trip the already-paused early return).
	h.sup.setCloudConnected(false)
	if h.sup.runner("lnk_1") != nil {
		t.Fatal("the full stop left a runner behind")
	}
}

// The reconcile fetch is network I/O: it must not hold the lifecycle lock,
// or a Disconnect waits behind it (up to the HTTP timeout) while uploads
// keep leaving.
func TestWhatsAppSupervisorReconcileFetchesWithoutTheLifecycleLock(t *testing.T) {
	h := newSupervisorHarness(t)
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStateConnected}}
	// Start without the background reconcile loop, so the reconcile below is
	// the only one: a second, concurrent reconcile takes the lock for its own
	// snapshot or mutation and would fail the probe spuriously.
	h.sup.startWith(h.transport, true)
	h.sup.mu.Lock()
	h.sup.paused = false
	h.sup.mu.Unlock()
	freeCh := make(chan bool, 4)
	h.transport.mu.Lock()
	h.transport.onBound = func() {
		// Report whether the lifecycle lock is free without disturbing it:
		// a successful TryLock is released at once.
		free := h.sup.lifecycleMu.TryLock()
		if free {
			h.sup.lifecycleMu.Unlock()
		}
		select {
		case freeCh <- free:
		default:
		}
	}
	h.transport.mu.Unlock()
	done := make(chan bool, 1)
	go func() { h.sup.reconcileOnce(); done <- true }()
	select {
	case got := <-freeCh:
		if !got {
			t.Fatal("reconcileOnce held the lifecycle lock across its BoundLinks fetch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile did not fetch")
	}
	<-done
}

// newRefusalHarness is a runner with an open outbox whose batch posts are
// answered by respond; every accepted record id is collected.
func newRefusalHarness(t *testing.T, linkID string, respond func(ids []string, p map[string]any) whatsAppSyncResult) (*whatsAppLinkRunner, *whatsAppOutbox, func() []string) {
	t.Helper()
	_, _ = withWhatsAppTestEnv(t)
	sup := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	sup.ctx, sup.cancel = context.WithCancel(context.Background())
	t.Cleanup(sup.cancel)
	var mu sync.Mutex
	var accepted []string
	tr := &fakeWATransport{}
	tr.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] != waSyncKindBatch {
			return whatsAppSyncResult{Status: 200}
		}
		var ids []string
		for _, rec := range p["records"].([]any) {
			ids = append(ids, rec.(map[string]any)["waMessageId"].(string))
		}
		res := respond(ids, p)
		if res.Status/100 == 2 {
			mu.Lock()
			accepted = append(accepted, ids...)
			mu.Unlock()
		}
		return res
	}
	sup.transport = tr
	r := sup.newRunner(linkID, "ws", 1)
	ob, _ := openTestOutbox(t, linkID)
	t.Cleanup(func() { ob.Close() })
	r.outbox = ob
	return r, ob, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), accepted...)
	}
}

func ackedThrough(p map[string]any) whatsAppSyncResult {
	to := int64(p["seqTo"].(float64))
	return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
}

// A 400 says some record in the batch is invalid, not which one. The uploader
// must narrow the batch down until one record alone is refused and drop only
// that record: dropping the batch, or its head, loses valid messages the
// cloud never ingested.
func TestWhatsAppSupervisorInvalidBatchDropsOnlyTheRefusedRecord(t *testing.T) {
	r, ob, accepted := newRefusalHarness(t, "lnk_400", func(ids []string, p map[string]any) whatsAppSyncResult {
		for _, id := range ids {
			if id == "BAD" {
				return whatsAppSyncResult{Status: 400, Body: whatsAppSyncResponse{Code: "INVALID"}}
			}
		}
		return ackedThrough(p)
	})
	var records []whatsAppRecord
	var valid []string
	for i := 1; i <= 10; i++ {
		id := "B" + strconv.Itoa(i)
		if i == 7 {
			id = "BAD"
		} else {
			valid = append(valid, id)
		}
		records = append(records, textRecord("chat@s.whatsapp.net", id, "x"))
	}
	if err := ob.Append(records, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; ob.Depth() > 0; i++ {
		if i == 60 {
			t.Fatalf("the refused batch was never narrowed down: depth %d", ob.Depth())
		}
		r.uploadOnce(context.Background())
	}
	got := map[string]bool{}
	for _, id := range accepted() {
		got[id] = true
	}
	if got["BAD"] {
		t.Fatal("the invalid record was reported accepted")
	}
	for _, id := range valid {
		if !got[id] {
			t.Fatalf("valid record %s was dropped with the invalid one (accepted %v)", id, accepted())
		}
	}
	r.mu.Lock()
	limit := r.narrowLimit
	r.mu.Unlock()
	if limit != 0 {
		t.Fatalf("full batches did not resume after the invalid record was dropped (limit %d)", limit)
	}
}

// A refusal that does not reproduce drops nothing: the narrowed batches all
// go through and full batches resume.
func TestWhatsAppSupervisorTransientBatchRefusalDropsNothing(t *testing.T) {
	calls := 0
	r, ob, accepted := newRefusalHarness(t, "lnk_400t", func(ids []string, p map[string]any) whatsAppSyncResult {
		calls++
		if calls == 1 {
			return whatsAppSyncResult{Status: 400, Body: whatsAppSyncResponse{Code: "INVALID"}}
		}
		return ackedThrough(p)
	})
	if err := ob.Append([]whatsAppRecord{
		textRecord("chat@s.whatsapp.net", "B1", "one"),
		textRecord("chat@s.whatsapp.net", "B2", "two"),
		textRecord("chat@s.whatsapp.net", "B3", "three"),
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, outcome := r.uploadOnce(context.Background()); outcome != whatsAppUploadRetry {
		t.Fatalf("a refused batch must be retried, got outcome %d", outcome)
	}
	if d := ob.Depth(); d != 3 {
		t.Fatalf("a refusal of a multi-record batch dropped records: depth %d", d)
	}
	for i := 0; ob.Depth() > 0; i++ {
		if i == 10 {
			t.Fatalf("the retried records never went through: depth %d", ob.Depth())
		}
		r.uploadOnce(context.Background())
	}
	if got := accepted(); len(got) != 3 {
		t.Fatalf("every record must be delivered after a refusal that does not reproduce, got %v", got)
	}
	r.mu.Lock()
	limit := r.narrowLimit
	r.mu.Unlock()
	if limit != 0 {
		t.Fatalf("full batches did not resume (limit %d)", limit)
	}
}

// A link start that completes while BoundLinks is in flight is absent from
// the response through no fault of its own: the stale response must not
// wipe it. The reconcile reports failure so the loop retries against a
// fresh response.
func TestWhatsAppSupervisorReconcileSkipsLinksCreatedDuringFetch(t *testing.T) {
	_, _ = withWhatsAppTestEnv(t)
	sup := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	sup.ctx, sup.cancel = context.WithCancel(context.Background())
	defer sup.cancel()
	tr := &fakeWATransport{}
	tr.bound = []whatsAppBoundLink{{LinkID: "lnk_old", WorkspaceID: "ws", Generation: 1, State: waStateConnected}}
	sup.links["lnk_old"] = sup.newRunner("lnk_old", "ws", 1)
	tr.onBound = func() {
		// A link start completing while the fetch is in flight.
		fresh := sup.newRunner("lnk_new", "ws", 1)
		sup.mu.Lock()
		sup.links["lnk_new"] = fresh
		sup.mu.Unlock()
	}
	sup.transport = tr
	if sup.reconcileOnce() {
		t.Fatal("a reconcile raced by a link start must retry, not succeed")
	}
	if sup.runner("lnk_new") == nil {
		t.Fatal("the stale response wiped a link paired during the fetch")
	}
	if sup.runner("lnk_old") == nil {
		t.Fatal("a bound link must survive reconcile")
	}
}

// An auth expiry during an upload outage must not wipe captured messages:
// the outbox survives the bounded flush and drains once the cloud is back,
// and only then is the session wiped.
func TestWhatsAppSupervisorAuthExpiredFlushTimeoutRetainsOutbox(t *testing.T) {
	h, c := pairedHarness(t)
	prevFlush := whatsAppAuthFlushTimeout
	whatsAppAuthFlushTimeout = 50 * time.Millisecond
	defer func() { whatsAppAuthFlushTimeout = prevFlush }()
	var mu sync.Mutex
	healthy := false
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
		to := int64(p["seqTo"].(float64))
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "A1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "queued", func() bool {
		if r := h.sup.runner("lnk_1"); r != nil {
			depth, _ := r.outboxDepth()
			return depth > 0
		}
		return false
	})
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	// The bounded flush cannot drain against 503s: the link and its key
	// must still be there afterwards.
	time.Sleep(400 * time.Millisecond)
	if r := h.sup.runner("lnk_1"); r == nil {
		t.Fatal("auth expiry wiped a link whose outbox never drained")
	}
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatal("auth expiry deleted the outbox key with rows still queued")
	}
	mu.Lock()
	healthy = true
	mu.Unlock()
	waitFor(t, "drained wipe", func() bool { return h.sup.runner("lnk_1") == nil })
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the drained session was not wiped")
	}
}

// When the credential-store deletion fails, the unlink must not be acked:
// the cloud keeps the link disconnecting and replays the unlink, which
// confirms once the cleanup actually succeeds.
func TestWhatsAppSupervisorUnlinkWithholdsAckWhenCleanupFails(t *testing.T) {
	h, c := pairedHarness(t)
	h.ks.unavailable = true
	h.sup.unlink("lnk_1", 3)
	if acks := h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck }); len(acks) != 0 {
		t.Fatalf("an unlink with failed cleanup was acked: %v", acks)
	}
	if _, logouts := c.count(); logouts != 1 {
		t.Fatal("the companion must still be logged out before the retry")
	}
	h.ks.unavailable = false
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatal("the failed cleanup must leave the outbox key for the replay")
	}
	h.sup.unlink("lnk_1", 3) // replayed by the cloud
	waitFor(t, "replayed ack", func() bool {
		return len(h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindAck })) == 1
	})
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the replayed unlink must delete the outbox key")
	}
}

// A history chunk bigger than the absolute caps must be split, not retried
// whole forever: with the uploader draining, every piece is admittable and
// every record is delivered exactly once.
func TestWhatsAppSupervisorOversizedHistoryChunkSplits(t *testing.T) {
	_, _ = withWhatsAppTestEnv(t)
	prevMax := whatsAppOutboxMaxRecords
	whatsAppOutboxMaxRecords = 6
	defer func() { whatsAppOutboxMaxRecords = prevMax }()
	sup := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	sup.ctx, sup.cancel = context.WithCancel(context.Background())
	defer sup.cancel()
	tr := &fakeWATransport{}
	sup.transport = tr
	r := sup.newRunner("lnk_split", "ws", 1)
	dir, err := ensureWhatsAppLinkDir("lnk_split")
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := openWhatsAppOutbox(dir, "lnk_split")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	r.outbox = ob
	r.startLoops()
	var records []whatsAppRecord
	for i := 0; i < 10; i++ {
		records = append(records, textRecord("chat@s.whatsapp.net", "H"+strconv.Itoa(i), "msg"))
	}
	if got := r.appendHistoryChunk(ob, records); got != len(records) {
		t.Fatalf("admitted %d of %d oversized-chunk records", got, len(records))
	}
	// onHistorySync kicks the uploader after every chunk; the parked loop
	// does not poll fast enough to matter here.
	r.kickUploader()
	waitFor(t, "drained", func() bool { return ob.Depth() == 0 })
	seen := map[int64]bool{}
	for _, p := range tr.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }) {
		for seq := int64(p["seqFrom"].(float64)); seq <= int64(p["seqTo"].(float64)); seq++ {
			seen[seq] = true
		}
	}
	for seq := int64(1); seq <= int64(len(records)); seq++ {
		if !seen[seq] {
			t.Fatalf("record with deviceSeq %d was never uploaded", seq)
		}
	}
}

// A cloud pause that closes the stores under an auth-expiry drain must not
// read as "drained": the outbox and its key survive, and the resume after
// reconnect drains them before the wipe.
func TestWhatsAppSupervisorAuthExpiredDrainSurvivesCloudPause(t *testing.T) {
	h, c := pairedHarness(t)
	prevFlush := whatsAppAuthFlushTimeout
	whatsAppAuthFlushTimeout = 50 * time.Millisecond
	defer func() { whatsAppAuthFlushTimeout = prevFlush }()
	var mu sync.Mutex
	healthy := false
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
		to := int64(p["seqTo"].(float64))
		return whatsAppSyncResult{Status: 200, Body: whatsAppSyncResponse{OK: true, AckedSeq: &to}}
	}
	h.transport.mu.Unlock()
	c.emit(liveMsg(waPeer, waPeer, false, "A1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "queued", func() bool {
		if r := h.sup.runner("lnk_1"); r != nil {
			depth, _ := r.outboxDepth()
			return depth > 0
		}
		return false
	})
	c.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	time.Sleep(100 * time.Millisecond)
	h.sup.setCloudConnected(false)
	time.Sleep(200 * time.Millisecond)
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatal("a pause during the auth-expiry drain deleted the undrained outbox key")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a pause during the auth-expiry drain wiped the undrained outbox")
	}
	mu.Lock()
	healthy = true
	mu.Unlock()
	h.sup.setCloudConnected(true) // the next client has no session: still expired
	waitFor(t, "drained after resume", func() bool {
		_, err := h.ks.Get("lnk_1")
		return err != nil && h.sup.runner("lnk_1") == nil
	})
	var delivered bool
	for _, p := range h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }) {
		for _, rec := range p["records"].([]any) {
			if rec.(map[string]any)["waMessageId"] == "A1" {
				delivered = true
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !delivered {
		t.Fatal("the captured message was never uploaded")
	}
}

// A deregistration whose cleanup fails must leave a durable marker, so the
// next start retries the wipe instead of orphaning the session material.
func TestWhatsAppSupervisorWipeAllKeepsFailedCleanupForRetry(t *testing.T) {
	h, _ := pairedHarness(t)
	h.ks.unavailable = true
	h.sup.WipeAll()
	h.ks.unavailable = false
	local, err := loadWhatsAppLocalLinks()
	if err != nil || len(local) != 1 || local[0].LinkID != "lnk_1" || !local[0].PendingWipe {
		t.Fatalf("links.json must keep the failed wipe for a retry: %+v %v", local, err)
	}
	if _, err := h.ks.Get("lnk_1"); err != nil {
		t.Fatal("the outbox key should still be there after the failed cleanup")
	}

	next := &whatsAppSupervisor{}
	next.start(h.transport)
	defer next.ShutdownAll(context.Background())
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the next start did not retry the pending wipe")
	}
	dir, _ := whatsAppLinkDir("lnk_1")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the next start left the session directory")
	}
	if next.runner("lnk_1") != nil {
		t.Fatal("a pending-wipe link must never be resumed")
	}
	if local, _ := loadWhatsAppLocalLinks(); len(local) != 0 {
		t.Fatalf("links.json still lists a completed wipe: %+v", local)
	}
}

// A link start is acked only once it is durably accepted: by the time the
// handler returns the runner exists and links.json lists it.
func TestWhatsAppLinkStartCommandIsAcceptedBeforeReturning(t *testing.T) {
	h := newSupervisorHarness(t)
	h.sup.start(h.transport)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	defer func() { globalWhatsAppSupervisor = prev }()
	_, pub := newBrowserKey(t)
	arg, _ := json.Marshal(whatsAppLinkStartParams{LinkID: "lnk_ack", Generation: 1, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	handleWhatsAppCommand(commandMsg{Command: whatsAppLinkStartCommand, Args: []string{string(arg)}})
	if h.sup.runner("lnk_ack") == nil {
		t.Fatal("the link start was not accepted before the handler returned")
	}
	local, _ := loadWhatsAppLocalLinks()
	if len(local) != 1 || local[0].LinkID != "lnk_ack" {
		t.Fatalf("links.json does not list the accepted link: %+v", local)
	}
}

// A fenced session wiped on its own goroutine whose cleanup fails has no
// caller to retry it: links.json must keep it marked PendingWipe, and the
// next start finishes the wipe.
func TestWhatsAppSupervisorFailedAsyncWipeIsRetriedOnNextStart(t *testing.T) {
	h, c := pairedHarness(t)
	h.transport.mu.Lock()
	h.transport.respond = func(p map[string]any) whatsAppSyncResult {
		if p["kind"] == waSyncKindBatch {
			return whatsAppSyncResult{Status: 409, Body: whatsAppSyncResponse{Code: waErrLinkFenced}}
		}
		return whatsAppSyncResult{Status: 200}
	}
	h.transport.mu.Unlock()
	h.ks.unavailable = true
	c.emit(liveMsg(waPeer, waPeer, false, "F1", &waE2E.Message{Conversation: proto.String("x")}))
	waitFor(t, "fenced", func() bool { return h.sup.runner("lnk_1") == nil })
	h.sup.waitDiscards(context.Background())
	h.ks.unavailable = false
	local, err := loadWhatsAppLocalLinks()
	if err != nil || len(local) != 1 || local[0].LinkID != "lnk_1" || !local[0].PendingWipe {
		t.Fatalf("links.json must keep the failed wipe for a retry: %+v %v", local, err)
	}
	h.sup.ShutdownAll(context.Background())

	next := &whatsAppSupervisor{}
	next.start(h.transport)
	defer next.ShutdownAll(context.Background())
	if _, err := h.ks.Get("lnk_1"); err == nil {
		t.Fatal("the next start did not retry the failed wipe")
	}
	if next.runner("lnk_1") != nil {
		t.Fatal("a pending-wipe link must never be resumed")
	}
}

// A deregistration that cannot read links.json must not replace it with an
// empty list: the links are recovered from their session directories, and a
// cleanup that fails stays marked PendingWipe.
func TestWhatsAppSupervisorWipeAllRecoversLinksFromAnUnreadableManifest(t *testing.T) {
	h := newSupervisorHarness(t)
	if _, err := ensureWhatsAppLinkDir("lnk_orphan"); err != nil {
		t.Fatal(err)
	}
	if err := h.ks.Put("lnk_orphan", []byte("key")); err != nil {
		t.Fatal(err)
	}
	manifest := whatsAppRootDir() + string(os.PathSeparator) + whatsAppLinksFileName
	if err := os.WriteFile(manifest, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ks.unavailable = true
	h.sup.WipeAll()
	h.ks.unavailable = false
	local, err := loadWhatsAppLocalLinks()
	if err != nil || len(local) != 1 || local[0].LinkID != "lnk_orphan" || !local[0].PendingWipe {
		t.Fatalf("links.json lost the link it could not wipe: %+v %v", local, err)
	}
	h.sup.WipeAll()
	if _, err := h.ks.Get("lnk_orphan"); err == nil {
		t.Fatal("the retried deregistration left the outbox key")
	}
	if local, _ := loadWhatsAppLocalLinks(); len(local) != 0 {
		t.Fatalf("links.json still lists a completed wipe: %+v", local)
	}
}

// A final history chunk cut short (here: the runner stops while the outbox
// is at its cap) must not mark history complete: its suffix was never queued.
func TestWhatsAppSupervisorPartlyStoredFinalHistoryChunkIsNotComplete(t *testing.T) {
	_, _ = withWhatsAppTestEnv(t)
	prevMax := whatsAppOutboxMaxRecords
	whatsAppOutboxMaxRecords = 2
	defer func() { whatsAppOutboxMaxRecords = prevMax }()
	sup := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	sup.ctx, sup.cancel = context.WithCancel(context.Background())
	defer sup.cancel()
	sup.transport = &fakeWATransport{}
	r := sup.newRunner("lnk_hist", "ws", 1)
	dir, err := ensureWhatsAppLinkDir("lnk_hist")
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := openWhatsAppOutbox(dir, "lnk_hist")
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if err := ob.Append([]whatsAppRecord{textRecord("chat@s.whatsapp.net", "L1", "x"), textRecord("chat@s.whatsapp.net", "L2", "x")}, false); err != nil {
		t.Fatal(err)
	}
	r.outbox = ob
	r.client = &fakeWAClient{qr: make(chan whatsmeow.QRChannelItem)}
	r.state = waStateSyncingHistory
	web := &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(waPeer.String()), ID: proto.String("H1")}, MessageTimestamp: proto.Uint64(1_800_000_000), Message: &waE2E.Message{Conversation: proto.String("old")}}
	data := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(), Progress: proto.Uint32(100),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(waPeer.String()), Messages: []*waHistorySync.HistorySyncMsg{{Message: web}}}},
	}
	done := make(chan struct{})
	go func() { r.onHistorySync(data); close(done) }()
	time.Sleep(100 * time.Millisecond)
	r.cancel() // a pause or shutdown while the chunk waits for room
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the history append did not stop with the runner")
	}
	if r.localLink().HistoryComplete {
		t.Fatal("a partly stored final chunk marked history complete")
	}
	if got := sup.transport.(*fakeWATransport).find(func(p map[string]any) bool { return p["historySyncComplete"] == true }); len(got) != 0 {
		t.Fatalf("a partly stored final chunk reported completion: %v", got)
	}
}

// A single history record refused because the queue is at its cap must wait
// for room like any other chunk. Only a record the EMPTY queue refuses is
// skipped; skipping one at the cap would drop it, and in the final chunk
// would let history be marked complete without it.
func TestWhatsAppSupervisorSingleHistoryRecordAtTheCapWaitsForRoom(t *testing.T) {
	_, _ = withWhatsAppTestEnv(t)
	prevMax, prevPoll := whatsAppOutboxMaxRecords, whatsAppHistoryRoomPoll
	whatsAppOutboxMaxRecords, whatsAppHistoryRoomPoll = 2, 5*time.Millisecond
	defer func() { whatsAppOutboxMaxRecords, whatsAppHistoryRoomPoll = prevMax, prevPoll }()
	sup := &whatsAppSupervisor{links: map[string]*whatsAppLinkRunner{}}
	sup.ctx, sup.cancel = context.WithCancel(context.Background())
	defer sup.cancel()
	sup.transport = &fakeWATransport{}
	r := sup.newRunner("lnk_one", "ws", 1)
	ob, _ := openTestOutbox(t, "lnk_one")
	defer ob.Close()
	// Live records always land, so they can hold the queue at its cap.
	if err := ob.Append([]whatsAppRecord{textRecord("chat@s.whatsapp.net", "L1", "x"), textRecord("chat@s.whatsapp.net", "L2", "x")}, false); err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	go func() {
		got <- r.appendHistoryChunk(ob, []whatsAppRecord{textRecord("chat@s.whatsapp.net", "H1", "old")})
	}()
	select {
	case n := <-got:
		t.Fatalf("a single history record at the cap returned %d (depth %d) instead of waiting for room", n, ob.Depth())
	case <-time.After(100 * time.Millisecond):
	}
	if err := ob.Ack(2); err != nil { // the uploader makes room
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n != 1 {
			t.Fatalf("admitted %d of 1 record after room was made", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the history record was never admitted after room was made")
	}
	batch, err := ob.Next()
	if err != nil || batch == nil || len(batch.Records) != 1 || !strings.Contains(string(batch.Records[0]), `"H1"`) {
		t.Fatalf("the history record is not queued: %+v %v", batch, err)
	}
}

// A cloud pause moves the runners to pausedLinks in one step: a persist from
// a client event that races the pause must never write a links.json that
// lists the link neither as running nor as paused.
func TestWhatsAppSupervisorCloudPauseNeverPersistsAnEmptyManifest(t *testing.T) {
	h, _ := pairedHarness(t)
	r := h.sup.runner("lnk_1")
	if r == nil {
		t.Fatal("no runner")
	}
	// Hold the runner's lock so the pause stops where it snapshots the link,
	// then let a persist run against whatever the pause has exposed so far.
	r.mu.Lock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.sup.setCloudConnected(false) }()
	time.Sleep(50 * time.Millisecond)
	go func() { defer wg.Done(); h.sup.persist() }()
	time.Sleep(50 * time.Millisecond)
	r.mu.Unlock()
	wg.Wait()
	local, err := loadWhatsAppLocalLinks()
	if err != nil || len(local) != 1 || local[0].LinkID != "lnk_1" {
		t.Fatalf("a persist racing the pause forgot the paused link: %+v %v", local, err)
	}
}

// A link whose stores cannot be opened at boot (here: the credential store
// is locked) must not be wiped: it stays in links.json with its session,
// key and queued rows, and resumes once the store answers again.
func TestWhatsAppSupervisorResumeHoldsALinkWhoseStoresCannotOpen(t *testing.T) {
	h := newSupervisorHarness(t)
	prevMin, prevMax := whatsAppResumeRetryMin, whatsAppResumeRetryMax
	whatsAppResumeRetryMin, whatsAppResumeRetryMax = 5*time.Millisecond, 20*time.Millisecond
	defer func() { whatsAppResumeRetryMin, whatsAppResumeRetryMax = prevMin, prevMax }()
	ob, dir := openTestOutbox(t, "lnk_held")
	if err := ob.Append([]whatsAppRecord{textRecord(waPeer.String(), "Q1", "queued before the restart")}, false); err != nil {
		t.Fatal(err)
	}
	ob.Close()
	_ = saveWhatsAppLocalLinks([]whatsAppLocalLink{{LinkID: "lnk_held", WorkspaceID: "ws", Generation: 5, State: waStateConnected, HistoryComplete: true}})
	h.session = true
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_held", WorkspaceID: "ws", Generation: 5, State: waStateConnected}}
	h.ks.mu.Lock()
	h.ks.locked = true
	h.ks.mu.Unlock()
	h.sup.start(h.transport)
	time.Sleep(100 * time.Millisecond) // several retries fail meanwhile
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a store that could not open wiped the session directory")
	}
	h.ks.mu.Lock()
	_, kept := h.ks.entries["lnk_held"]
	h.ks.mu.Unlock()
	if !kept {
		t.Fatal("a store that could not open deleted the outbox key")
	}
	if local, err := loadWhatsAppLocalLinks(); err != nil || len(local) != 1 || local[0].LinkID != "lnk_held" {
		t.Fatalf("links.json must keep the held link: %+v %v", local, err)
	}
	if h.sup.runner("lnk_held") == nil {
		t.Fatal("the held link is not reachable by unlink / re-link / reconcile")
	}
	if posts := h.transport.find(func(map[string]any) bool { return true }); len(posts) != 0 {
		t.Fatalf("a held link posted: %v", posts)
	}

	h.ks.mu.Lock()
	h.ks.locked = false
	h.ks.mu.Unlock()
	waitFor(t, "resumed once the store answers", func() bool {
		c := h.client(0)
		if c == nil {
			return false
		}
		n, _ := c.count()
		return n == 1
	})
	waitFor(t, "the queued row uploaded", func() bool {
		for _, p := range h.transport.find(func(p map[string]any) bool { return p["kind"] == waSyncKindBatch }) {
			for _, rec := range p["records"].([]any) {
				if rec.(map[string]any)["waMessageId"] == "Q1" {
					return true
				}
			}
		}
		return false
	})
}

// A link start delivered while the supervisor is still paused (reconnect
// races the Pub/Sub loop) is not acked, so Pub/Sub redelivers it; the
// redelivery after the resume is accepted.
func TestWhatsAppLinkStartWhilePausedIsNotAcked(t *testing.T) {
	h := newSupervisorHarness(t)
	h.sup.startWith(h.transport, true)
	prev := globalWhatsAppSupervisor
	globalWhatsAppSupervisor = h.sup
	defer func() { globalWhatsAppSupervisor = prev }()
	_, pub := newBrowserKey(t)
	arg, _ := json.Marshal(whatsAppLinkStartParams{LinkID: "lnk_paused", Generation: 1, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	cmd := commandMsg{Command: whatsAppLinkStartCommand, Args: []string{string(arg)}}
	if handleWhatsAppCommand(cmd) {
		t.Fatal("a link start rejected while paused was acked")
	}
	if h.sup.runner("lnk_paused") != nil {
		t.Fatal("a paused supervisor started a runner")
	}
	h.sup.setCloudConnected(true)
	if !handleWhatsAppCommand(cmd) {
		t.Fatal("the redelivered link start was not acked after the resume")
	}
	if h.sup.runner("lnk_paused") == nil {
		t.Fatal("the redelivered link start was not accepted")
	}
}

// A malformed link start is final: acking it stops a redelivery loop.
func TestWhatsAppMalformedLinkStartIsAcked(t *testing.T) {
	if !handleWhatsAppCommand(commandMsg{Command: whatsAppLinkStartCommand, Args: []string{"not json"}}) {
		t.Fatal("a malformed link start must be acked")
	}
}

// slowProbeKeystore counts concurrent Available probes.
type slowProbeKeystore struct {
	*memoryWhatsAppKeystore
	mu           sync.Mutex
	active, peak int
	probes       int
}

func (k *slowProbeKeystore) Available() bool {
	k.mu.Lock()
	k.active++
	k.probes++
	if k.active > k.peak {
		k.peak = k.active
	}
	k.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	k.mu.Lock()
	k.active--
	k.mu.Unlock()
	return true
}

// Concurrent callers share one availability probe: every probe uses the same
// credential-store account, so overlapping probes could delete each other's
// entry and cache a false "unavailable".
func TestWhatsAppSecureStorageProbeIsSingleFlight(t *testing.T) {
	ks := &slowProbeKeystore{memoryWhatsAppKeystore: newMemoryWhatsAppKeystore()}
	defer setWhatsAppKeystoreForTest(ks)()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !whatsAppSecureStorageAvailable() {
				t.Error("probe reported unavailable")
			}
		}()
	}
	wg.Wait()
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.peak != 1 || ks.probes != 1 {
		t.Fatalf("probes ran concurrently or repeatedly: peak=%d probes=%d", ks.peak, ks.probes)
	}
}

// ctxBlockingTransport holds a history-complete state report until the
// context it was posted with ends, like a request to an unresponsive
// terminal-service. Everything else goes to the fake.
type ctxBlockingTransport struct {
	*fakeWATransport
	blocked chan struct{}
}

func (b *ctxBlockingTransport) Post(ctx context.Context, payload any) whatsAppSyncResult {
	if m, ok := payload.(map[string]any); ok && m["kind"] == waSyncKindState && m["historySyncComplete"] == true {
		select {
		case b.blocked <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return whatsAppSyncResult{}
	}
	return b.fakeWATransport.Post(ctx, payload)
}

// A state report in flight on the heartbeat loop (here: the quiet-history
// completion, hanging on an unresponsive server) must not hold shutdown past
// the caller's deadline: Windows gives the whole shutdown 2-4 s, and the
// teardown steps after WhatsApp still have to run.
func TestWhatsAppSupervisorShutdownHonoursTheCallerDeadline(t *testing.T) {
	h := newSupervisorHarness(t)
	prevHB, prevQuiet := whatsAppHeartbeatInterval, whatsAppHistoryQuiet
	whatsAppHeartbeatInterval, whatsAppHistoryQuiet = 10*time.Millisecond, time.Millisecond
	defer func() { whatsAppHeartbeatInterval, whatsAppHistoryQuiet = prevHB, prevQuiet }()
	tr := &ctxBlockingTransport{fakeWATransport: h.transport, blocked: make(chan struct{}, 1)}
	h.transport.bound = []whatsAppBoundLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 2, State: waStateConnected}}
	h.sup.start(tr)
	_, pub := newBrowserKey(t)
	h.sup.linkStart(whatsAppLinkStartParams{LinkID: "lnk_1", Generation: 2, WorkspaceID: "ws", Method: waPairQR, BrowserPub: pub})
	waitFor(t, "client", func() bool { return h.client(0) != nil })
	c := h.client(0)
	c.emit(&events.PairSuccess{ID: types.NewJID("15550000001", types.DefaultUserServer)})
	c.mu.Lock()
	c.session = true
	c.mu.Unlock()
	c.emit(&events.Connected{}) // syncingHistory; the heartbeat loop completes it after the quiet
	select {
	case <-tr.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the heartbeat loop never reported history completion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	h.sup.ShutdownAll(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %v with a 300ms deadline", took)
	}
	if local, err := loadWhatsAppLocalLinks(); err != nil || len(local) != 1 {
		t.Fatalf("shutdown must still record the link: %+v %v", local, err)
	}
}
