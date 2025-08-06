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
	"golang.org/x/mod/semver"
)

const githubRepo = "AIExpedite/aiexpedite-local-terminal"

func checkForUpdate() error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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

	cur := "v" + strings.TrimPrefix(version, "v")
	new := rel.TagName
	if semver.Compare(new, cur) <= 0 {
		fmt.Println("No update available; current", version)
		return nil
	}

	// Choose the correct binary asset (GOOS & GOARCH)
	targetSuffix := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	var assetURL, assetName string
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, targetSuffix) {
			assetURL, assetName = a.URL, a.Name
			break
		}
	}
	if assetURL == "" {
		return fmt.Errorf("no binary asset for %s/%s", runtime.GOOS, runtime.GOARCH)
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
	if _, err := io.Copy(io.MultiWriter(tmp, h), r2.Body); err != nil {
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o755) // make executable
	fmt.Printf("SHA‑256: %s\n", hex.EncodeToString(h.Sum(nil)))

	updatePath = tmp.Name()
	updatePending = true
	systray.Quit() // trigger graceful restart
	return nil
}
