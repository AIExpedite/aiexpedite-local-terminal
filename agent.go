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
	"runtime"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/gorilla/websocket"
)

const version     = "v0.1.0"
const githubRepo  = "AIExpedite/aiexpedite-local-terminal" // repo used for auto‑update

var (
	ttydCmd       *exec.Cmd          // ttyd process (killed on exit)
	shutdownChan  = make(chan struct{})
	updatePending bool
	updatePath    string
)

// StartAgent launches tmux (optional), ttyd and the relay loop.
func StartAgent(cfg *Config) {
	useTmux := true
	if err := ensureTmux(); err != nil {
		if runtime.GOOS == "windows" {
			useTmux = false // Windows fallback: single shell
			fmt.Println("Warning:", err, "- running without tmux.")
		} else {
			fmt.Println("Fatal:", err)
			return
		}
	}

	if err := ensureTtyd(); err != nil {
		fmt.Println("Fatal:", err)
		return
	}

	if useTmux {
		if err := startTmuxSession(); err != nil {
			fmt.Println("Fatal:", err)
			return
		}
	}

	// build ttyd command
	shellCmd := []string{}
	if useTmux {
		shellCmd = []string{"tmux", "attach", "-t", tmuxSessionName}
	} else {
		sh := os.Getenv("SHELL")
		if sh == "" {
			if runtime.GOOS == "windows" {
				sh = "powershell"
			} else {
				sh = "/bin/bash"
			}
		}
		shellCmd = []string{sh}
	}

	port := cfg.LocalTtydPort
	if port == 0 {
		port = 7681
	}
	args := append([]string{"-p", fmt.Sprintf("%d", port), "-i", "127.0.0.1"}, shellCmd...)
	ttydCmd = exec.Command("ttyd", args...)
	if err := ttydCmd.Start(); err != nil {
		fmt.Println("Fatal: cannot start ttyd –", err)
		return
	}
	fmt.Println("ttyd listening on 127.0.0.1:", port)

	// start relay loop
	go relayConnectionLoop(cfg)

	// background update check
	if cfg.AutoUpdate {
		go func() {
			time.Sleep(5 * time.Second)
			if err := checkForUpdate(); err != nil {
				fmt.Println("Update check error:", err)
			}
		}()
	}
}

// ───────────────────────────────── Relay ────────────────────────────────────

func relayConnectionLoop(cfg *Config) {
	for {
		select {
		case <-shutdownChan:
			return
		default:
		}

		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			fmt.Println("TLS config error:", err)
			time.Sleep(10 * time.Second)
			continue
		}

		dialer := websocket.DefaultDialer
		dialer.TLSClientConfig = tlsCfg
		header := http.Header{}
		header.Set("Authorization", "Bearer "+cfg.JWT)

		ws, _, err := dialer.Dial(cfg.RelayURL, header)
		if err != nil {
			fmt.Println("Relay connection failed:", err)
			time.Sleep(5 * time.Second)
			continue
		}
		fmt.Println("Connected to relay")

		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				fmt.Println("Relay closed:", err)
				break
			}
			fmt.Println("Relay message:", string(msg))
		}
		ws.Close()
		time.Sleep(5 * time.Second)
	}
}

// ───────────────────────────── TLS / mTLS helper ────────────────────────────

func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CAFile != "" {
		ca, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("invalid CA")
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.CertFile != "" && cfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

// ───────────────────────────── Auto‑update logic ────────────────────────────

func checkForUpdate() error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	newVer := strings.TrimPrefix(rel.TagName, "v")
	curVer := strings.TrimPrefix(version, "v")
	if !isNewerVersion(curVer, newVer) {
		fmt.Println("No update available; current", version)
		return nil
	}

	var assetURL, assetName string
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		switch runtime.GOOS {
		case "windows":
			if strings.HasSuffix(name, ".exe") {
				assetURL, assetName = a.URL, a.Name
			}
		case "darwin":
			if strings.Contains(name, "darwin") || strings.HasSuffix(name, ".pkg") {
				assetURL, assetName = a.URL, a.Name
			}
		default: // linux
			if strings.Contains(name, "linux") {
				assetURL, assetName = a.URL, a.Name
			}
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no asset for %s", runtime.GOOS)
	}

	fmt.Println("Downloading", assetName)
	tmp, err := os.CreateTemp("", "agent_update_*")
	if err != nil {
		return err
	}
	defer tmp.Close()

	r2, err := http.Get(assetURL)
	if err != nil {
		return err
	}
	defer r2.Body.Close()
	if _, err := io.Copy(tmp, r2.Body); err != nil {
		return err
	}

	// (checksum verification omitted for brevity)

	updatePath = tmp.Name()
	updatePending = true
	systray.Quit()
	return nil
}

func isNewerVersion(cur, new string) bool {
	cc := strings.Split(cur, ".")
	nc := strings.Split(new, ".")
	for i := 0; i < len(cc) && i < len(nc); i++ {
		var ci, ni int
		fmt.Sscanf(cc[i], "%d", &ci)
		fmt.Sscanf(nc[i], "%d", &ni)
		if ni > ci {
			return true
		} else if ni < ci {
			return false
		}
	}
	return len(nc) > len(cc)
}
