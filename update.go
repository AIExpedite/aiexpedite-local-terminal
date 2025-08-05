// File: update.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/getlantern/systray"
)

const githubRepo = "AIExpedite/aiexpedite-local-terminal"

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
			if strings.Contains(name, "darwin") {
				assetURL, assetName = a.URL, a.Name
			}
		default: // linux
			if strings.Contains(name, "linux") {
				assetURL, assetName = a.URL, a.Name
			}
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no binary asset for %s", runtime.GOOS)
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
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if _, err := io.Copy(w, r2.Body); err != nil {
		return err
	}
	fmt.Printf("SHA‑256: %s\n", hex.EncodeToString(h.Sum(nil)))

	updatePath = tmp.Name()
	updatePending = true
	systray.Quit() // trigger graceful restart
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
