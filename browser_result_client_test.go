package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The browser front door signs exactly as the WhatsApp front door does, so
// terminal-service's one generalised verifier checks both: the shared vectors
// (testdata/whatsapp_sync_signature_vectors.json, identical in both repos) pin
// the canonical.
func TestBrowserResultSignatureVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/whatsapp_sync_signature_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name, AgentID, Secret, Body, SignedMessage, Signature string
			Timestamp                                             int64
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil || len(file.Vectors) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, v := range file.Vectors {
		msg := buildDeviceBodySignedMessage(v.AgentID, v.Timestamp, []byte(v.Body))
		if msg != v.SignedMessage || generateHMAC(msg, v.Secret) != v.Signature {
			t.Errorf("%s: canonical or signature drifted", v.Name)
		}
	}
}

type recordedPost struct {
	path, query, contentType, ts, sig string
	body                              []byte
}

func newTestBrowserPoster(t *testing.T, handler func(n int, r recordedPost) int) (*httpBrowserPoster, func() []recordedPost) {
	t.Helper()
	var mu sync.Mutex
	var got []recordedPost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recordedPost{path: r.URL.Path, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"),
			ts: r.Header.Get("X-Agent-Timestamp"), sig: r.Header.Get("X-Agent-Signature"), body: b}
		mu.Lock()
		got = append(got, rec)
		n := len(got)
		mu.Unlock()
		w.WriteHeader(handler(n, rec))
	}))
	t.Cleanup(srv.Close)
	p := &httpBrowserPoster{
		creds:   func() (string, string) { return "agent-1", "secret" },
		baseURL: func() string { return srv.URL },
		client:  srv.Client(),
		backoff: func(int) time.Duration { return 5 * time.Millisecond },
	}
	return p, func() []recordedPost { mu.Lock(); defer mu.Unlock(); return append([]recordedPost(nil), got...) }
}

func verifyBrowserSignature(t *testing.T, r recordedPost) {
	t.Helper()
	ts, err := strconv.ParseInt(r.ts, 10, 64)
	if err != nil || time.Since(time.UnixMilli(ts)) > time.Minute {
		t.Fatalf("timestamp %q", r.ts)
	}
	sum := sha256.Sum256(r.body)
	want := generateHMAC("agent-1:"+r.ts+":"+hex.EncodeToString(sum[:]), "secret")
	if r.sig != want {
		t.Fatalf("signature %s, want %s", r.sig, want)
	}
}

func TestBrowserPosterSignsAndPostsResult(t *testing.T) {
	p, got := newTestBrowserPoster(t, func(int, recordedPost) int { return 200 })
	body := browserResultBody{V: 1, CommandID: "cmd-1", OK: true, Route: browserRouteDevTools,
		Result: map[string]any{"title": "<b>A & B</b>"}, Ms: 12}
	if err := p.PostResult(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	posts := got()
	if len(posts) != 1 || posts[0].path != "/device/agent-1/browser/result" || posts[0].contentType != "application/json" {
		t.Fatalf("%+v", posts)
	}
	verifyBrowserSignature(t, posts[0])
	if string(posts[0].body) != `{"v":1,"commandId":"cmd-1","ok":true,"route":"devtools","result":{"title":"<b>A & B</b>"},"ms":12}` {
		t.Fatalf("body %s", posts[0].body)
	}
	fail := browserResultBody{V: 1, CommandID: "cmd-2", OK: false, Error: browserErr(browserErrTabNotFound, "gone"), Ms: 3}
	if err := p.PostResult(context.Background(), fail); err != nil {
		t.Fatal(err)
	}
	if b := string(got()[1].body); b != `{"v":1,"commandId":"cmd-2","ok":false,"error":{"code":"BROWSER_TAB_NOT_FOUND","message":"gone"},"ms":3}` {
		t.Fatalf("error body %s", b)
	}
}

func TestBrowserPosterUploadsScreenshot(t *testing.T) {
	p, got := newTestBrowserPoster(t, func(int, recordedPost) int { return 200 })
	img := []byte{0xff, 0xd8, 0xff, 0x00, 0x01}
	if err := p.PostScreenshot(context.Background(), "cmd-9", img, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	r := got()[0]
	if r.path != "/device/agent-1/browser/screenshot" || r.query != "commandId=cmd-9" || r.contentType != "image/jpeg" || string(r.body) != string(img) {
		t.Fatalf("%+v", r)
	}
	verifyBrowserSignature(t, r)
}

func TestBrowserPosterRetries5xxNever4xx(t *testing.T) {
	p, got := newTestBrowserPoster(t, func(n int, _ recordedPost) int {
		if n < 3 {
			return 503
		}
		return 200
	})
	if err := p.PostResult(context.Background(), browserResultBody{V: 1, CommandID: "c"}); err != nil || len(got()) != 3 {
		t.Fatalf("5xx: %v after %d attempts", err, len(got()))
	}

	p, got = newTestBrowserPoster(t, func(int, recordedPost) int { return 401 })
	err := p.PostResult(context.Background(), browserResultBody{V: 1, CommandID: "c"})
	var pe *browserPostError
	if !errors.As(err, &pe) || pe.Status != 401 || len(got()) != 1 {
		t.Fatalf("4xx: %v after %d attempts", err, len(got()))
	}

	// 409: the command already finished; an idempotent post is done.
	p, got = newTestBrowserPoster(t, func(int, recordedPost) int { return 409 })
	if err := p.PostResult(context.Background(), browserResultBody{V: 1, CommandID: "c"}); err != nil || len(got()) != 1 {
		t.Fatalf("409: %v after %d attempts", err, len(got()))
	}

	// Unregistered: nothing is sent.
	p, got = newTestBrowserPoster(t, func(int, recordedPost) int { return 200 })
	p.creds = func() (string, string) { return "", "" }
	p.backoff = func(int) time.Duration { return browserResultRetryWindow }
	if err := p.PostResult(context.Background(), browserResultBody{V: 1, CommandID: "c"}); err == nil || len(got()) != 0 {
		t.Fatalf("unregistered: %v, %d posts", err, len(got()))
	}
}

func TestFitBrowserResult(t *testing.T) {
	big := browserReadResult{TabID: "d:A", Text: strings.Repeat("word ", 4000)}
	for i := 0; i < browserMaxReadItems; i++ {
		big.Links = append(big.Links, browserReadLink{Ref: "f1_a", Text: "x", Href: "https://example.com/" + strings.Repeat("p", 3000)})
	}
	body := fitBrowserResult(browserResultBody{V: 1, CommandID: "c", OK: true, Result: big})
	raw, _ := encodeBrowserJSON(body)
	if !body.OK || len(raw) > browserMaxResultBytes {
		t.Fatalf("ok=%v size=%d", body.OK, len(raw))
	}
	small := browserResultBody{V: 1, CommandID: "c", OK: true, Result: map[string]any{"a": 1}}
	if got := fitBrowserResult(small); got.Result == nil || !got.OK {
		t.Fatal("a small result must pass unchanged")
	}
	huge := browserResultBody{V: 1, CommandID: "c", OK: true, Result: map[string]any{"a": strings.Repeat("x", browserMaxResultBytes)}}
	if got := fitBrowserResult(huge); got.OK || got.Error == nil {
		t.Fatal("an oversized result that cannot shrink must become an error")
	}
}
