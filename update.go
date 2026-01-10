// File: update.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// UpdateInfo contains information about an available update
type UpdateInfo struct {
	Available      bool
	LatestVersion  string
	CurrentVersion string
	AssetURL       string
	AssetName      string
}

// checkForNewVersion checks GitHub for a newer version without downloading.
// Returns UpdateInfo with Available=true if an update exists.
func checkForNewVersion() (*UpdateInfo, error) {
	info := &UpdateInfo{
		CurrentVersion: Version,
		Available:      false,
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No releases found
		return info, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}

	cur := "v" + strings.TrimPrefix(Version, "v")
	latest := rel.TagName
	if semver.Compare(latest, cur) <= 0 {
		// No update needed
		return info, nil
	}

	// Pick the asset matching GOOS / GOARCH
	targetSuffix := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, targetSuffix) {
			info.Available = true
			info.LatestVersion = latest
			info.AssetURL = a.URL
			info.AssetName = a.Name
			break
		}
	}

	return info, nil
}

// downloadAndApplyUpdate downloads the update binary and triggers a restart.
// Sets updatePath and updatePending globals, then calls systray.Quit().
func downloadAndApplyUpdate(info *UpdateInfo) error {
	if !info.Available || info.AssetURL == "" {
		return errors.New("no update available to download")
	}

	fmt.Printf("→ Downloading %s...\n", info.AssetName)

	tmp, err := os.CreateTemp("", "agent_update_*")
	if err != nil {
		return err
	}
	defer tmp.Close()

	resp, err := http.Get(info.AssetURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return err
	}

	_ = os.Chmod(tmp.Name(), 0o755) // make executable
	fmt.Printf("→ Downloaded (SHA-256: %s)\n", hex.EncodeToString(h.Sum(nil)))

	// Set global state for restart
	updatePath = tmp.Name()
	updatePending = true

	fmt.Println("→ Restarting with new version...")
	systray.Quit() // graceful restart
	return nil
}

// checkForUpdate is the legacy function for manual "Check for Updates" menu option.
// It checks for updates and immediately downloads if available.
func checkForUpdate() error {
	info, err := checkForNewVersion()
	if err != nil {
		return err
	}

	if !info.Available {
		fmt.Println("No update available; current", Version)
		return nil
	}

	return downloadAndApplyUpdate(info)
}
