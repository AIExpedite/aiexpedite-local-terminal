// whatsapp_client.go — the whatsmeow adapter behind the whatsAppClient seam.
//
// The supervisor talks to whatsAppClient only, so its state machine is tested
// against a fake that emits real whatsmeow event structs
// (whatsapp_supervisor_test.go). This file is the one place that constructs a
// whatsmeow.Client: the session store is whatsmeow's own sqlstore on the
// agent's cgo-free modernc.org/sqlite driver (registered as "sqlite"; the
// dialect whatsmeow expects is "sqlite3"), in the owner-only link directory.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// whatsAppClient is what the supervisor needs from a WhatsApp client.
type whatsAppClient interface {
	AddEventHandler(func(any))
	Connect() error
	Disconnect()
	Logout(ctx context.Context) error
	IsConnected() bool
	// HasSession reports a paired device identity in the store.
	HasSession() bool
	OwnJID() types.JID
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	PairPhone(ctx context.Context, phone string) (string, error)
	ParseWebMessage(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)
	// Close releases the session database. The client must be disconnected.
	Close() error
}

// newWhatsAppClient opens (creating) dir/session.db. A var so tests install a
// fake.
var newWhatsAppClient = openWhatsmeowClient

var whatsAppDevicePropsOnce sync.Once

// configureWhatsAppDeviceProps names the companion "AI Expedite" in the phone's
// Linked devices list, so the person can recognise — and remove — it.
func configureWhatsAppDeviceProps() {
	whatsAppDevicePropsOnce.Do(func() {
		store.SetOSInfo("AI Expedite", [3]uint32{1, 0, 0})
	})
}

type whatsmeowClient struct {
	cli *whatsmeow.Client
	db  *sql.DB
}

func openWhatsmeowClient(dir string) (whatsAppClient, error) {
	configureWhatsAppDeviceProps()
	path := filepath.Join(dir, whatsAppSessionDBName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	log := newWhatsAppLogger("store")
	container := sqlstore.NewWithDB(db, "sqlite3", log)
	ctx := context.Background()
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp session store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp session store: %w", err)
	}
	cli := whatsmeow.NewClient(device, newWhatsAppLogger("client"))
	cli.EnableAutoReconnect = true
	return &whatsmeowClient{cli: cli, db: db}, nil
}

func (c *whatsmeowClient) AddEventHandler(h func(any)) {
	c.cli.AddEventHandler(func(evt any) { h(evt) })
}
func (c *whatsmeowClient) Connect() error { return c.cli.Connect() }
func (c *whatsmeowClient) Disconnect()    { c.cli.Disconnect() }
func (c *whatsmeowClient) Logout(ctx context.Context) error {
	return c.cli.Logout(ctx)
}
func (c *whatsmeowClient) IsConnected() bool { return c.cli.IsConnected() }
func (c *whatsmeowClient) HasSession() bool  { return c.cli.Store != nil && c.cli.Store.ID != nil }
func (c *whatsmeowClient) OwnJID() types.JID {
	if c.cli.Store == nil || c.cli.Store.ID == nil {
		return types.EmptyJID
	}
	return c.cli.Store.ID.ToNonAD()
}
func (c *whatsmeowClient) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return c.cli.GetQRChannel(ctx)
}

// PairPhone asks WhatsApp for an 8-character code the person types on the
// phone. The display name must be "Browser (OS)".
func (c *whatsmeowClient) PairPhone(ctx context.Context, phone string) (string, error) {
	return c.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome ("+whatsAppOSDisplayName()+")")
}
func (c *whatsmeowClient) ParseWebMessage(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	return c.cli.ParseWebMessage(chat, msg)
}
func (c *whatsmeowClient) Close() error { return c.db.Close() }

func whatsAppOSDisplayName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "Mac OS"
	default:
		return "Linux"
	}
}

// whatsAppClientVersion is the whatsmeow module version compiled in
// (diagnostics.clientVersion), so a protocol break is attributable.
func whatsAppClientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "go.mau.fi/whatsmeow" {
				return dep.Version
			}
		}
	}
	return "unknown"
}

/* --------------------------------------------------------------------------
   Logging — linkId and event kinds only
   -------------------------------------------------------------------------- */

// whatsAppJIDPattern masks WhatsApp addresses (phone numbers, LIDs, groups)
// in any whatsmeow line that quotes one.
var whatsAppJIDPattern = regexp.MustCompile(`\+?[0-9]{5,}(?:[.:][0-9]+)*@(?:s\.whatsapp\.net|lid|g\.us|c\.us|broadcast|newsletter)`)

// whatsAppPairCodePattern is the 8-character phone pairing code ("ABCD-EFGH").
var whatsAppPairCodePattern = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)

// redactWhatsAppLog is redactAgentSecrets (which masks QR refs) plus pair codes
// and WhatsApp addresses.
func redactWhatsAppLog(s string) string {
	out := redactAgentSecrets(s)
	out = whatsAppPairCodePattern.ReplaceAllString(out, "[pair-code]")
	return whatsAppJIDPattern.ReplaceAllString(out, "[jid]")
}

// whatsAppLogSink receives every WhatsApp log line after redaction. A var so
// tests capture the lines without swapping the process-wide os.Stdout.
var whatsAppLogSink = func(line string) { fmt.Println(line) }

func logWhatsApp(linkID, event string) {
	whatsAppLogSink(redactWhatsAppLog(fmt.Sprintf("%s[whatsapp] link=%s %s%s", colorCyan, linkID, event, colorReset)))
}

// whatsAppLogger wraps whatsmeow's logger at INFO-and-above (debug lines carry
// protocol frames) and redacts every line.
type whatsAppLogger struct{ module string }

func newWhatsAppLogger(module string) waLog.Logger { return whatsAppLogger{module: module} }

func (l whatsAppLogger) emit(level, msg string, args ...any) {
	whatsAppLogSink(redactWhatsAppLog(fmt.Sprintf("[whatsapp/%s] %s %s", l.module, level, fmt.Sprintf(msg, args...))))
}
func (l whatsAppLogger) Warnf(msg string, args ...any)  { l.emit("WARN", msg, args...) }
func (l whatsAppLogger) Errorf(msg string, args ...any) { l.emit("ERROR", msg, args...) }
func (l whatsAppLogger) Infof(msg string, args ...any)  { l.emit("INFO", msg, args...) }
func (l whatsAppLogger) Debugf(string, ...any)          {}
func (l whatsAppLogger) Sub(module string) waLog.Logger {
	return whatsAppLogger{module: l.module + "/" + module}
}
