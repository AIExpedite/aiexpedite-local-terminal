// browser_result_client.go — the agent's two HTTP calls to terminal-service's
// browser front door.
//
//	POST /device/:agentId/browser/result                    JSON result of one command
//	POST /device/:agentId/browser/screenshot?commandId=<id>  the image, raw, BEFORE its result
//
// Both are body-signed exactly like the WhatsApp front door: headers
// X-Agent-Timestamp (ms) and X-Agent-Signature =
// hex HMAC-SHA256(command_secret, agentId + ":" + timestamp + ":" + sha256hex(rawBody)),
// the canonical buildDeviceBodySignedMessage builds (the vectors in
// testdata/whatsapp_sync_signature_vectors.json pin it in both repos).
//
// A 5xx or a network failure is retried with backoff for up to
// browserResultRetryWindow; a 4xx never is (409 means the command already
// finished, which is success for an idempotent post).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"time"
)

// browserResultBody is contract 2a.
type browserResultBody struct {
	V         int           `json:"v"`
	CommandID string        `json:"commandId"`
	OK        bool          `json:"ok"`
	Route     string        `json:"route,omitempty"`
	Result    any           `json:"result,omitempty"`
	Error     *browserError `json:"error,omitempty"`
	Ms        int64         `json:"ms"`
}

type browserPoster interface {
	PostResult(ctx context.Context, body browserResultBody) error
	PostScreenshot(ctx context.Context, commandID string, data []byte, contentType string) error
}

type httpBrowserPoster struct {
	creds   func() (agentID, secret string)
	baseURL func() string
	client  *http.Client
	// backoff between attempts (tests shorten it).
	backoff func(attempt int) time.Duration
}

func newHTTPBrowserPoster(cfg *Config) *httpBrowserPoster {
	return &httpBrowserPoster{
		creds:   cfg.usageHintCredentials,
		baseURL: getRegistrationURL,
		client:  &http.Client{Timeout: 30 * time.Second},
		backoff: func(attempt int) time.Duration {
			d := time.Second << attempt
			if d > 15*time.Second {
				d = 15 * time.Second
			}
			return d
		},
	}
}

var errBrowserUnregistered = errors.New("this computer is not registered")

// browserPostError is a final answer from terminal-service (a 4xx other than 409).
type browserPostError struct{ Status int }

func (e *browserPostError) Error() string {
	return fmt.Sprintf("terminal-service answered HTTP %d", e.Status)
}

// encodeBrowserJSON marshals without HTML escaping (the bytes signed are the bytes sent).
func encodeBrowserJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (h *httpBrowserPoster) PostResult(ctx context.Context, body browserResultBody) error {
	raw, err := encodeBrowserJSON(body)
	if err != nil {
		return err
	}
	return h.post(ctx, "result", nil, raw, "application/json")
}

func (h *httpBrowserPoster) PostScreenshot(ctx context.Context, commandID string, data []byte, contentType string) error {
	q := neturl.Values{"commandId": {commandID}}
	return h.post(ctx, "screenshot", q, data, contentType)
}

func (h *httpBrowserPoster) post(ctx context.Context, path string, q neturl.Values, body []byte, contentType string) error {
	deadline := time.Now().Add(browserResultRetryWindow)
	for attempt := 0; ; attempt++ {
		status, err := h.once(ctx, path, q, body, contentType)
		switch {
		case err == nil && (status/100 == 2 || status == http.StatusConflict):
			return nil
		case err == nil && status/100 == 4:
			return &browserPostError{Status: status}
		case errors.Is(err, errBrowserUnregistered):
			return err
		}
		wait := h.backoff(attempt)
		if time.Now().Add(wait).After(deadline) || ctx.Err() != nil {
			if err != nil {
				return err
			}
			return &browserPostError{Status: status}
		}
		sleepCtx(ctx, int(wait/time.Millisecond))
	}
}

func (h *httpBrowserPoster) once(ctx context.Context, path string, q neturl.Values, body []byte, contentType string) (int, error) {
	agentID, secret := h.creds()
	base := h.baseURL()
	if agentID == "" || secret == "" || base == "" {
		return 0, errBrowserUnregistered
	}
	u := fmt.Sprintf("%s/device/%s/browser/%s", base, neturl.PathEscape(agentID), path)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := time.Now().UnixMilli()
	req.Header.Set(whatsAppSyncHeaderTS, strconv.FormatInt(ts, 10))
	req.Header.Set(whatsAppSyncHeaderSig, generateHMAC(buildDeviceBodySignedMessage(agentID, ts, body), secret))
	req.Header.Set("Content-Type", contentType)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// fitBrowserResult keeps a result body under BROWSER_LIMITS.MAX_RESULT_BYTES.
// Only a read can grow that large: its page text is cut first, then its lists.
func fitBrowserResult(body browserResultBody) browserResultBody {
	limit := browserMaxResultBytes - 4096
	raw, err := encodeBrowserJSON(body)
	if err == nil && len(raw) <= limit {
		return body
	}
	if rr, ok := body.Result.(browserReadResult); ok {
		for i := 0; i < 12; i++ {
			switch {
			case len(rr.Text) > 2000:
				rr.Text = clipRunesJS(rr.Text, jsLength(rr.Text)/2)
				rr.TextTruncated = true
			case len(rr.Links) > 20:
				rr.Links = rr.Links[:len(rr.Links)/2]
			case len(rr.Buttons) > 20:
				rr.Buttons = rr.Buttons[:len(rr.Buttons)/2]
			case len(rr.Fields) > 20:
				rr.Fields = rr.Fields[:len(rr.Fields)/2]
			default:
				i = 12
			}
			body.Result = rr
			if raw, err := encodeBrowserJSON(body); err == nil && len(raw) <= limit {
				return body
			}
		}
	}
	return browserResultBody{V: body.V, CommandID: body.CommandID, OK: false, Route: body.Route, Ms: body.Ms,
		Error: browserErr(browserErrPageNotResponding, "the page's content is too large to send")}
}
