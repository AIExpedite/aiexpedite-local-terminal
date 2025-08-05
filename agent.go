package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"runtime"
	"github.com/gorilla/websocket"
)

const version = "v0.1.0"
const githubRepo = "ExampleCorp/tray-agent" // GitHub repo for updates

var ttydCmd *exec.Cmd           // process for ttyd (to terminate on exit)
var shutdownChan = make(chan struct{})
var updatePending bool
var updatePath string

// StartAgent launches the necessary components (tmux session, ttyd server, relay connection).
func StartAgent(cfg *Config) {
	// Ensure dependencies
	useTmux := true
	if err := ensureTmux(); err != nil {
		if cfg != nil && runtime.GOOS == "windows" {
			// tmux missing on Windows; fall back to single shell mode:contentReference[oaicite:11]{index=11}
			useTmux = false
			fmt.Println("Warning:", err.Error(), "- will run without tmux.")
		} else {
			fmt.Println("Fatal:", err)
			return // cannot proceed without tmux on non-Windows
		}
	}
	if err := ensureTtyd(); err != nil {
		fmt.Println("Fatal:", err)
		return
	}
	// If using tmux, start or ensure the session exists
	if useTmux {
		if err := startTmuxSession(); err != nil {
			fmt.Println("Fatal: tmux session could not be started -", err)
			return
		}
	}
	// Determine command for ttyd to run
	var cmdArgs []string
	if useTmux {
		// Use tmux session
		cmdArgs = []string{"tmux", "attach", "-t", tmuxSessionName}
	} else {
		// Fallback: use a single shell session
		shell := os.Getenv("SHELL")
		if shell == "" {
			if runtime.GOOS == "windows" {
				shell = "powershell"
			} else {
				shell = "/bin/bash"
			}
		}
		cmdArgs = []string{shell}
	}
	// Launch ttyd (web terminal) on configured port
	port := cfg.LocalTtydPort
	if port == 0 {
		port = 7681
	}
	portStr := fmt.Sprintf("%d", port)
	args := []string{"-p", portStr, "-i", "127.0.0.1"}
	args = append(args, cmdArgs...)
	ttydCmd = exec.Command("ttyd", args...)
	if err := ttydCmd.Start(); err != nil {
		fmt.Println("Fatal: failed to start ttyd -", err)
		return
	}
	fmt.Println("ttyd running on port", port)
	// Connect to cloud relay (runs indefinitely in background)
	go relayConnectionLoop(cfg)
	// Optionally, check for updates at startup
	if cfg.AutoUpdate {
		go func() {
			// Delay a bit before checking for updates (to avoid blocking startup)
			time.Sleep(5 * time.Second)
			if err := checkForUpdate(); err != nil {
				fmt.Println("Update check error:", err)
			}
		}()
	}
}

// relayConnectionLoop maintains a persistent WebSocket connection to the relay server.
func relayConnectionLoop(cfg *Config) {
	for {
		select {
		case <-shutdownChan:
			return
		default:
		}
		// Prepare WebSocket dialer with TLS config (for WSS, possibly mTLS)
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			fmt.Println("Relay TLS config error:", err)
			// Wait and retry
			time.Sleep(10 * time.Second)
			continue
		}
		dialer := websocket.DefaultDialer
		dialer.TLSClientConfig = tlsCfg
		header := http.Header{}
		header.Set("Authorization", "Bearer "+cfg.JWT)
		wsConn, _, err := dialer.Dial(cfg.RelayURL, header)
		if err != nil {
			fmt.Println("Relay connection failed:", err)
			time.Sleep(5 * time.Second)
			continue
		}
		fmt.Println("Connected to relay server")
		// Read messages until error (or shutdown)
		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				fmt.Println("Relay connection closed:", err)
				break
			}
			// Handle incoming relay messages (this could be extended to handle control commands)
			text := string(message)
			fmt.Println("Relay message:", text)
			// (e.g., parse JSON commands to open new sessions, etc.)
		}
		wsConn.Close()
		// Attempt reconnection after a delay
		select {
		case <-shutdownChan:
			return
		case <-time.After(5 * time.Second):
			continue
		}
	}
}

// buildTLSConfig creates a TLS configuration for the WebSocket connection, including mTLS if configured.
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	// Load CA for server certificate verification if provided
	if cfg.CAFile != "" {
		caData, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read CA file: %w", err)
		}
		caPool := x509.NewCertPool()
		if ok := caPool.AppendCertsFromPEM(caData); !ok {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsCfg.RootCAs = caPool
	}
	// Load client certificate for mTLS if provided
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("cannot load client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// checkForUpdate checks GitHub Releases for a newer version and performs an update if available.
func checkForUpdate() error {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	resp, err := http.Get(apiURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return err
	}
	newVer := release.TagName
	if newVer == "" {
		return fmt.Errorf("no release tag found")
	}
	// Normalize version strings (remove leading 'v')
	curVer := strings.TrimPrefix(version, "v")
	newVerClean := strings.TrimPrefix(newVer, "v")
	if newVerClean == curVer || !isNewerVersion(curVer, newVerClean) {
		fmt.Println("No update available (current version:", version, ")")
		return nil
	}
	fmt.Println("New version available:", newVer)
	// Find appropriate asset for this OS
	var assetURL, assetName string
	for _, asset := range release.Assets {
		name := strings.ToLower(asset.Name)
		if runtime.GOOS == "windows" && strings.HasSuffix(name, ".exe") {
			assetURL = asset.BrowserDownloadURL
			assetName = asset.Name
			break
		}
		if runtime.GOOS == "darwin" && (strings.Contains(name, "darwin") || strings.HasSuffix(name, ".pkg")) {
			assetURL = asset.BrowserDownloadURL
			assetName = asset.Name
			break
		}
		if runtime.GOOS == "linux" && (strings.Contains(name, "linux") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".deb")) {
			assetURL = asset.BrowserDownloadURL
			assetName = asset.Name
			break
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no downloadable asset found for %s", runtime.GOOS)
	}
	// Also look for checksum file in assets
	var checksumURL string
	for _, asset := range release.Assets {
		if strings.HasPrefix(strings.ToUpper(asset.Name), "SHA256SUM") {
			checksumURL = asset.BrowserDownloadURL
			break
		}
	}
	// Download the asset
	fmt.Println("Downloading update:", assetURL)
	tmpFile, err := os.CreateTemp("", "agent_update_*")
	if err != nil {
		return err
	}
	defer tmpFile.Close()
	resp2, err := http.Get(assetURL)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if _, err := io.Copy(tmpFile, resp2.Body); err != nil {
		return err
	}
	downloadPath := tmpFile.Name()
	// Verify checksum if available
	if checksumURL != "" {
		resp3, err := http.Get(checksumURL)
		if err == nil {
			defer resp3.Body.Close()
			sumsData, _ := io.ReadAll(resp3.Body)
			// Compute SHA-256 of the downloaded file and compare with published checksum:contentReference[oaicite:12]{index=12}
			_, _ = tmpFile.Seek(0, io.SeekStart)
			hash := sha256.New()
			if _, err := io.Copy(hash, tmpFile); err != nil {
				return fmt.Errorf("failed to compute checksum: %w", err)
			}
			calculated := fmt.Sprintf("%x", hash.Sum(nil))
			for _, line := range strings.Split(string(sumsData), "\n") {
				if strings.Contains(line, assetName) {
					fields := strings.Fields(line)
					if len(fields) > 0 && strings.EqualFold(fields[0], calculated) {
						fmt.Println("Downloaded update verified (SHA-256 matched).")
					} else {
						return fmt.Errorf("checksum mismatch, update file may be corrupted")
					}
					break
				}
			}
		}
	}
	// Prepare to apply update
	updatePath = downloadPath
	updatePending = true
	fmt.Println("Update downloaded to", downloadPath)
	// Trigger application of update (will happen in onExit)
	systray.Quit()
	return nil
}

// isNewerVersion compares two semantic version strings (no 'v' prefix) and returns true if newVer > curVer.
func isNewerVersion(curVer, newVer string) bool {
	curParts := strings.Split(curVer, ".")
	newParts := strings.Split(newVer, ".")
	for i := 0; i < len(curParts) && i < len(newParts); i++ {
		// convert to int if possible
		var curNum, newNum int
		fmt.Sscanf(curParts[i], "%d", &curNum)
		fmt.Sscanf(newParts[i], "%d", &newNum)
		if newNum > curNum {
			return true
		} else if newNum < curNum {
			return false
		}
	}
	return len(newParts) > len(curParts) // e.g., "1.2" vs "1.2.1"
}
