package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type notifyingListener struct {
	net.Listener
	once      sync.Once
	accepting chan struct{}
}

func (listener *notifyingListener) Accept() (net.Conn, error) {
	listener.once.Do(func() { close(listener.accepting) })
	return listener.Listener.Accept()
}

func TestBrowserIdentityHandlerSignsOriginBoundProof(t *testing.T) {
	cfg := &Config{AgentID: "agent-123", CommandSecret: "secret-value"}
	request := httptest.NewRequest(http.MethodGet, "/v1/browser-identity?challenge=0123456789abcdef", nil)
	request.Header.Set("Origin", "https://aiexpedite.com")
	response := httptest.NewRecorder()

	browserIdentityHandler(cfg, "prod").ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var proof browserIdentityProof
	if err := json.Unmarshal(response.Body.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	if proof.AgentID != cfg.AgentID || proof.Challenge != "0123456789abcdef" || proof.Origin != "https://aiexpedite.com" || proof.Environment != "prod" {
		t.Fatalf("unexpected proof: %#v", proof)
	}
	message := "agent-123:" + strconv.FormatInt(proof.Timestamp, 10) + ":0123456789abcdef:https://aiexpedite.com:prod"
	mac := hmac.New(sha256.New, []byte(cfg.CommandSecret))
	_, _ = mac.Write([]byte(message))
	if proof.Signature != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("signature does not cover canonical proof")
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Access-Control-Allow-Origin") != "https://aiexpedite.com" {
		t.Fatalf("missing security headers: %#v", response.Header())
	}
	if body := response.Body.String(); containsAny(body, cfg.CommandSecret, "user_id", "deviceName") {
		t.Fatalf("response leaked private config data: %s", body)
	}
}

func TestBrowserIdentityOriginsAndUnregisteredResponse(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		origin      string
		wantStatus  int
	}{
		{"prod frontend", "prod", "https://aiexpedite.com", http.StatusConflict},
		{"prod rejects local", "prod", "http://localhost:3000", http.StatusForbidden},
		{"staging frontend", "stg", "https://stg.aiexpedite.com", http.StatusConflict},
		{"beta frontend", "beta", "https://beta.aiexpedite.com", http.StatusConflict},
		{"dev frontend", "dev", "https://dev.aiexpedite.com", http.StatusConflict},
		{"dev localhost", "dev", "http://localhost:3000", http.StatusConflict},
		{"dev loopback", "dev", "http://127.0.0.1:3000", http.StatusConflict},
		{"null rejected", "dev", "null", http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/browser-identity?challenge=0123456789abcdef", nil)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			browserIdentityHandler(&Config{}, test.environment).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantStatus == http.StatusConflict && response.Body.String() != "{\"status\":\"unregistered\"}\n" {
				t.Fatalf("unexpected unregistered response: %q", response.Body.String())
			}
		})
	}
}

func TestBrowserIdentityPreflightAndValidation(t *testing.T) {
	handler := browserIdentityHandler(&Config{}, "dev")
	preflight := httptest.NewRequest(http.MethodOptions, "/v1/browser-identity", nil)
	preflight.Header.Set("Origin", "http://localhost:3000")
	preflight.Header.Set("Access-Control-Request-Private-Network", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("unexpected preflight: %d %#v", response.Code, response.Header())
	}

	badChallenge := httptest.NewRequest(http.MethodGet, "/v1/browser-identity?challenge=short", nil)
	badChallenge.Header.Set("Origin", "http://localhost:3000")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, badChallenge)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("short challenge status = %d", response.Code)
	}
}

func TestBrowserIdentityServerConfiguration(t *testing.T) {
	wantPorts := map[string]int{"prod": 7682, "dev": 7683, "stg": 7684, "beta": 7685}
	for environment, port := range wantPorts {
		if got, ok := browserIdentityPort(environment); !ok || got != port {
			t.Fatalf("port for %s = %d, %v", environment, got, ok)
		}
		server, err := browserIdentityServer(&Config{}, environment)
		if err != nil {
			t.Fatal(err)
		}
		if server.ReadHeaderTimeout != 2*time.Second || server.ReadTimeout != 3*time.Second || server.WriteTimeout != 3*time.Second || server.IdleTimeout != 30*time.Second || server.MaxHeaderBytes != identityMaxHeaderBytes {
			t.Fatalf("unexpected timeouts: %#v", server)
		}
	}
	if _, err := browserIdentityServer(&Config{LocalTtydPort: 7683}, "dev"); err == nil {
		t.Fatal("expected ttyd/identity port collision to be rejected")
	}
	if resolvedTtydPort(0) != 7681 {
		t.Fatal("zero ttyd port must normalize to 7681")
	}
}

func TestBrowserIdentityHandlerSnapshotsConcurrentRegistration(t *testing.T) {
	cfg := &Config{AgentID: "agent-a", CommandSecret: "secret-a"}
	pairs := []struct{ agentID, secret string }{{"agent-a", "secret-a"}, {"agent-b", "secret-b"}}
	problems := make(chan error, 200)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(index int) {
			defer wg.Done()
			pair := pairs[index%len(pairs)]
			cfg.WithPersistenceLock(func() {
				cfg.AgentID = pair.agentID
				cfg.CommandSecret = pair.secret
			})
		}(i)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodGet, "/v1/browser-identity?challenge=0123456789abcdef", nil)
			request.Header.Set("Origin", "https://aiexpedite.com")
			response := httptest.NewRecorder()
			browserIdentityHandler(cfg, "prod").ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				problems <- fmt.Errorf("status = %d", response.Code)
				return
			}
			var proof browserIdentityProof
			if err := json.Unmarshal(response.Body.Bytes(), &proof); err != nil {
				problems <- err
				return
			}
			secret := map[string]string{"agent-a": "secret-a", "agent-b": "secret-b"}[proof.AgentID]
			message := fmt.Sprintf("%s:%d:%s:%s:%s", proof.AgentID, proof.Timestamp, proof.Challenge, proof.Origin, proof.Environment)
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write([]byte(message))
			if proof.Signature != hex.EncodeToString(mac.Sum(nil)) {
				problems <- fmt.Errorf("proof combined credentials from different registration snapshots")
			}
		}()
	}
	wg.Wait()
	close(problems)
	for err := range problems {
		t.Fatal(err)
	}
}

func TestShutdownBrowserIdentityServerIsIdempotent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	readyListener := &notifyingListener{Listener: listener, accepting: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	})}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(readyListener)
		close(serveDone)
	}()
	<-readyListener.accepting

	browserIdentityState.Lock()
	browserIdentityState.server = server
	browserIdentityState.Unlock()
	shutdownBrowserIdentityServer()
	shutdownBrowserIdentityServer()
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("identity server did not stop")
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
