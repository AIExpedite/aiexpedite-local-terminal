// whatsapp_sync_client.go — the agent's two HTTP calls to terminal-service's
// WhatsApp front door.
//
//	POST /device/:agentId/whatsapp/sync   state, heartbeat, pairing, batch, ack
//	GET  /device/:agentId/whatsapp/links  the binding this computer should hold
//
// Both carry X-Agent-Timestamp and X-Agent-Signature =
// hex HMAC-SHA256(command_secret, agentId + ":" + timestamp + ":" + sha256hex(rawBody))
// (the GET signs an empty body). Unlike the /online-style agentId:timestamp
// scheme, this binds the BODY, so a captured request cannot be replayed with
// different records inside the timestamp window. terminal-service's
// buildWhatsAppSyncSignedMessage mirrors buildWhatsAppSyncSignedMessage here
// byte for byte; both assert testdata/whatsapp_sync_signature_vectors.json.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	whatsAppSyncHTTPTimeout   = 30 * time.Second
	whatsAppSyncHeaderTS      = "X-Agent-Timestamp"
	whatsAppSyncHeaderSig     = "X-Agent-Signature"
	whatsAppSyncResponseLimit = 64 * 1024
)

// buildWhatsAppSyncSignedMessage is the canonical the signature covers.
func buildWhatsAppSyncSignedMessage(agentID string, timestamp int64, rawBody []byte) string {
	sum := sha256.Sum256(rawBody)
	return agentID + ":" + strconv.FormatInt(timestamp, 10) + ":" + hex.EncodeToString(sum[:])
}

// whatsAppSyncResponse is terminal-service's answer. Tolerant reader: every
// field optional.
type whatsAppSyncResponse struct {
	OK       bool   `json:"ok"`
	AckedSeq *int64 `json:"ackedSeq,omitempty"`
	Code     string `json:"code,omitempty"`
	Ignored  bool   `json:"ignored,omitempty"`
}

// whatsAppSyncResult is one attempt's outcome.
type whatsAppSyncResult struct {
	Status     int // 0 = transport error
	Body       whatsAppSyncResponse
	RetryAfter time.Duration
}

// whatsAppBoundLink is one entry of GET /device/:agentId/whatsapp/links.
type whatsAppBoundLink struct {
	LinkID      string `json:"linkId"`
	WorkspaceID string `json:"workspaceID"`
	Generation  int64  `json:"generation"`
	State       string `json:"state"`
}

// whatsAppTransport is the network seam the supervisor uses; tests replace it.
type whatsAppTransport interface {
	Post(ctx context.Context, payload any) whatsAppSyncResult
	BoundLinks(ctx context.Context) ([]whatsAppBoundLink, int, error)
}

// httpWhatsAppTransport signs with the live agent credentials on every call,
// so a rotated secret is picked up without a restart.
type httpWhatsAppTransport struct {
	cfg    *Config
	client *http.Client
}

func newHTTPWhatsAppTransport(cfg *Config) *httpWhatsAppTransport {
	return &httpWhatsAppTransport{cfg: cfg, client: &http.Client{Timeout: whatsAppSyncHTTPTimeout}}
}

func (t *httpWhatsAppTransport) signedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	agentID, secret := t.cfg.usageHintCredentials()
	baseURL := getRegistrationURL()
	if agentID == "" || secret == "" || baseURL == "" {
		return nil, fmt.Errorf("unregistered")
	}
	url := fmt.Sprintf("%s/device/%s/whatsapp/%s", baseURL, agentID, path)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	ts := time.Now().UnixMilli()
	req.Header.Set(whatsAppSyncHeaderTS, strconv.FormatInt(ts, 10))
	req.Header.Set(whatsAppSyncHeaderSig, generateHMAC(buildWhatsAppSyncSignedMessage(agentID, ts, body), secret))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (t *httpWhatsAppTransport) Post(ctx context.Context, payload any) whatsAppSyncResult {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return whatsAppSyncResult{}
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	req, err := t.signedRequest(ctx, http.MethodPost, "sync", body)
	if err != nil {
		return whatsAppSyncResult{}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return whatsAppSyncResult{}
	}
	defer resp.Body.Close()
	res := whatsAppSyncResult{Status: resp.StatusCode, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	_ = json.NewDecoder(io.LimitReader(resp.Body, whatsAppSyncResponseLimit)).Decode(&res.Body)
	return res
}

func (t *httpWhatsAppTransport) BoundLinks(ctx context.Context) ([]whatsAppBoundLink, int, error) {
	req, err := t.signedRequest(ctx, http.MethodGet, "links", []byte{})
	if err != nil {
		return nil, 0, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("links: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Links []whatsAppBoundLink `json:"links"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, whatsAppSyncResponseLimit)).Decode(&body); err != nil {
		return nil, resp.StatusCode, err
	}
	return body.Links, resp.StatusCode, nil
}

// parseRetryAfter reads delta-seconds (the only form terminal-service sends).
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	if n > 3600 {
		n = 3600
	}
	return time.Duration(n) * time.Second
}
