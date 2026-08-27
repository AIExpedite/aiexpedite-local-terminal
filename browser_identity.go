package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultTtydPort         = 7681
	identityChallengeMinLen = 16
	identityChallengeMaxLen = 128
	identityMaxHeaderBytes  = 8 << 10
)

var identityPorts = map[string]int{
	"prod": 7682,
	"dev":  7683,
	"stg":  7684,
	"beta": 7685,
}

var identityOrigins = map[string]map[string]struct{}{
	"prod": {"https://aiexpedite.com": {}},
	"stg":  {"https://stg.aiexpedite.com": {}},
	"beta": {"https://beta.aiexpedite.com": {}},
	"dev": {
		"https://dev.aiexpedite.com": {},
		"http://localhost:3000":      {},
		"http://127.0.0.1:3000":      {},
	},
}

type browserIdentityProof struct {
	AgentID     string `json:"agentId"`
	Timestamp   int64  `json:"timestamp"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	Environment string `json:"environment"`
	Signature   string `json:"signature"`
}

type browserIdentityError struct {
	Status string `json:"status"`
}

var browserIdentityState struct {
	sync.Mutex
	server *http.Server
}

func browserIdentityPort(environment string) (int, bool) {
	port, ok := identityPorts[environment]
	return port, ok
}

func resolvedTtydPort(configured int) int {
	if configured == 0 {
		return defaultTtydPort
	}
	return configured
}

func browserIdentityServer(cfg *Config, environment string) (*http.Server, error) {
	port, ok := browserIdentityPort(environment)
	if !ok {
		return nil, fmt.Errorf("unsupported release environment %q", environment)
	}
	if cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	if resolvedTtydPort(cfg.LocalTtydPort) == port {
		return nil, fmt.Errorf("identity port %d conflicts with the configured ttyd port", port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/browser-identity", browserIdentityHandler(cfg, environment))
	return &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    identityMaxHeaderBytes,
	}, nil
}

func startBrowserIdentityServer(cfg *Config) {
	server, err := browserIdentityServer(cfg, EnvName)
	if err != nil {
		fmt.Printf("[browser-identity] Identity detection unavailable: %v\n", err)
		return
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		fmt.Printf("[browser-identity] Could not listen on %s: %v; terminal startup will continue\n", server.Addr, err)
		return
	}

	browserIdentityState.Lock()
	browserIdentityState.server = server
	browserIdentityState.Unlock()
	go func() {
		fmt.Printf("[browser-identity] Listening on http://%s\n", server.Addr)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("[browser-identity] Listener stopped unexpectedly: %v\n", err)
		}
	}()
}

func shutdownBrowserIdentityServer() {
	browserIdentityState.Lock()
	server := browserIdentityState.server
	browserIdentityState.server = nil
	browserIdentityState.Unlock()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("[browser-identity] Shutdown warning: %v\n", err)
	}
}

func browserIdentityHandler(cfg *Config, environment string) http.HandlerFunc {
	allowedOrigins := identityOrigins[environment]
	return func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Vary", "Origin")
		origin := request.Header.Get("Origin")
		if _, allowed := allowedOrigins[origin]; !allowed {
			http.Error(response, "origin not allowed", http.StatusForbidden)
			return
		}
		response.Header().Set("Access-Control-Allow-Origin", origin)

		if request.Method == http.MethodOptions {
			response.Header().Set("Access-Control-Allow-Methods", http.MethodGet)
			if strings.EqualFold(request.Header.Get("Access-Control-Request-Private-Network"), "true") {
				response.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			response.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		challenge := request.URL.Query().Get("challenge")
		if len(challenge) < identityChallengeMinLen || len(challenge) > identityChallengeMaxLen {
			http.Error(response, "invalid challenge", http.StatusBadRequest)
			return
		}

		var agentID, secret string
		cfg.WithPersistenceLock(func() {
			agentID = cfg.AgentID
			secret = cfg.CommandSecret
		})
		response.Header().Set("Content-Type", "application/json")
		if agentID == "" || secret == "" {
			response.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(response).Encode(browserIdentityError{Status: "unregistered"})
			return
		}

		timestamp := time.Now().UnixMilli()
		message := fmt.Sprintf("%s:%d:%s:%s:%s", agentID, timestamp, challenge, origin, environment)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(message))
		proof := browserIdentityProof{
			AgentID:     agentID,
			Timestamp:   timestamp,
			Challenge:   challenge,
			Origin:      origin,
			Environment: environment,
			Signature:   hex.EncodeToString(mac.Sum(nil)),
		}
		_ = json.NewEncoder(response).Encode(proof)
	}
}
