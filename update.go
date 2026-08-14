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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// semverRegex validates a version string is well-formed semver with a leading
// "v" (e.g. v1.2.3). Pre-release / build metadata (v1.2.3-rc1+meta) is
// accepted. Used to reject release-body spoofing and stale config values.
var semverRegex = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// isValidSemver reports whether s looks like a semver tag this app can compare.
func isValidSemver(s string) bool {
	return semverRegex.MatchString(s)
}

// assetSuffixForGOOS returns the exact filename suffix the release publishes
// for the current platform. Using a strict suffix match (rather than a loose
// substring match on just "darwin-arm64") prevents accidentally selecting a
// non-binary asset like a checksum file that happens to contain the platform
// token in its name.
func assetSuffixForGOOS() string {
	switch runtime.GOOS {
	case "windows":
		return fmt.Sprintf("-%s-%s.exe", runtime.GOOS, runtime.GOARCH)
	case "darwin":
		return fmt.Sprintf("-%s-%s.dmg", runtime.GOOS, runtime.GOARCH)
	case "linux":
		return fmt.Sprintf("-%s-%s.AppImage", runtime.GOOS, runtime.GOARCH)
	default:
		// Fall back to a loose suffix for any future platform so the picker
		// still has something to match; strict extensions can be added later.
		return fmt.Sprintf("-%s-%s", runtime.GOOS, runtime.GOARCH)
	}
}

// updateHTTPClient is used for quick GitHub API metadata calls (60s timeout).
var updateHTTPClient = &http.Client{Timeout: 60 * time.Second}

// updateDownloadClient is used for binary asset downloads, which may be large
// and require more time. 10-minute timeout prevents indefinite hangs.
var updateDownloadClient = &http.Client{Timeout: 10 * time.Minute}

// openManualUpdateURL is replaceable in tests; production uses the platform's
// default browser launcher.
var openManualUpdateURL = openBrowser

const githubRepo = "AIExpedite/aiexpedite-local-terminal"

// ReleaseChannel determines which GitHub release to check for updates.
// Set via ldflags: -X main.ReleaseChannel=dev
// Values: "prod" (default), "dev", "stg", "beta"
var ReleaseChannel string = "prod"

// UpdateInfo contains information about an available update
type UpdateInfo struct {
	Available      bool
	LatestVersion  string
	CurrentVersion string
	AssetURL       string
	AssetName      string
}

// getWithRetry performs an HTTP GET with bounded retries for transient
// failures. Retries on: network errors, HTTP 5xx, HTTP 429 (respecting the
// Retry-After header up to a cap). Does NOT retry on: HTTP 4xx (other than
// 429), 2xx, 3xx — those are terminal states the caller should inspect.
//
// Bounded to 3 attempts with exponential backoff (1s, 2s, 4s) plus any
// server-directed Retry-After wait, capped so a single check never takes
// much longer than the caller's HTTP client timeout.
func getWithRetry(client *http.Client, url string) (*http.Response, error) {
	const maxAttempts = 3
	const retryAfterCap = 30 * time.Second

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff before each retry; floor of 1s, doubling.
			backoff := time.Duration(1<<attempt-1) * time.Second
			time.Sleep(backoff)
		}

		resp, err := client.Get(url) //nolint:noctx,gosec // URL is a constant repo endpoint
		if err != nil {
			lastErr = err
			continue
		}

		// Terminal success/redirect/4xx (except 429): return as-is.
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Retryable: drain the body so the connection can be reused, close,
		// and honor Retry-After on 429.
		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					wait := time.Duration(secs) * time.Second
					if wait > retryAfterCap {
						wait = retryAfterCap
					}
					time.Sleep(wait)
				}
			}
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// checkForNewVersion checks GitHub for a newer version without downloading.
// Returns UpdateInfo with Available=true if an update exists.
func checkForNewVersion() (*UpdateInfo, error) {
	info := &UpdateInfo{
		CurrentVersion: Version,
		Available:      false,
	}

	// Choose API endpoint based on release channel
	var url string
	if ReleaseChannel == "dev" || ReleaseChannel == "stg" || ReleaseChannel == "beta" {
		// Non-prod channels check their specific release tag
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/latest-%s", githubRepo, ReleaseChannel)
	} else {
		// Prod checks the latest release
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	}

	resp, err := getWithRetry(updateHTTPClient, url)
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
		Body    string `json:"body"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	// Limit response body to 1 MB to guard against unexpectedly large or
	// malformed responses from the GitHub API endpoint.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}

	cur := "v" + strings.TrimPrefix(Version, "v")
	latest := rel.TagName

	// Extract version from release body since tag_name may not be semver
	// Non-prod format: "**Version:** vX.Y.Z"
	// Prod format: "**Current version: vX.Y.Z**"
	if ReleaseChannel == "dev" || ReleaseChannel == "stg" || ReleaseChannel == "beta" {
		if idx := strings.Index(rel.Body, "**Version:** "); idx != -1 {
			start := idx + len("**Version:** ")
			end := strings.Index(rel.Body[start:], "\n")
			if end == -1 {
				end = len(rel.Body) - start
			}
			latest = strings.TrimSpace(rel.Body[start : start+end])
		}
	} else {
		// Prod: extract from "**Current version: vX.Y.Z**"
		if idx := strings.Index(rel.Body, "**Current version: "); idx != -1 {
			start := idx + len("**Current version: ")
			end := strings.Index(rel.Body[start:], "**")
			if end == -1 {
				end = strings.Index(rel.Body[start:], "\n")
			}
			if end == -1 {
				end = len(rel.Body) - start
			}
			latest = strings.TrimSpace(rel.Body[start : start+end])
		}
	}

	// Reject versions that do not parse as semver. A malformed or spoofed
	// release body could otherwise put an arbitrary string into `latest`
	// (e.g. "v999.0.0-ATTACK") and bypass the semver comparison below.
	if !isValidSemver(latest) {
		fmt.Printf("[update] Ignoring release: latest=%q is not valid semver\n", latest)
		return info, nil
	}
	if !isValidSemver(cur) {
		// Defensive: current Version should always parse, but if the build
		// was tagged weirdly we'd rather skip than compare garbage.
		fmt.Printf("[update] Skipping check: current version %q is not valid semver\n", cur)
		return info, nil
	}

	if semver.Compare(latest, cur) <= 0 {
		// No update needed
		return info, nil
	}

	// Pick the asset whose filename exactly ends with the platform-specific
	// suffix (e.g. "-darwin-arm64.dmg"). Using HasSuffix rather than Contains
	// prevents matching side-car files like SHA256SUMS or an unrelated asset
	// that happens to include the platform token in its name.
	wantSuffix := strings.ToLower(assetSuffixForGOOS())
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if strings.HasSuffix(name, wantSuffix) {
			info.Available = true
			info.LatestVersion = latest
			info.AssetURL = a.URL
			info.AssetName = a.Name
			break
		}
	}

	return info, nil
}

// downloadAndVerifyUpdate downloads the release asset for info, enforces the
// size floor/ceiling, computes its SHA-256, and verifies SLSA build provenance.
// It returns the path to the VERIFIED temp artifact — a raw executable on
// Windows/Linux, a .dmg on macOS — which the caller owns and must either apply
// (applyVerifiedUpdate) or delete.
//
// This is the "fetch and verify" half of the update, factored out of the old
// downloadAndApplyUpdate so the automatic path (download → drain → install) and
// the manual path (download → install now) share ONE integrity check rather
// than growing a second copy. It performs no restart and mutates no global
// state, so it is safe to call, verify, and discard.
func downloadAndVerifyUpdate(info *UpdateInfo) (string, error) {
	if info == nil || !info.Available || info.AssetURL == "" {
		return "", errors.New("no update available to download")
	}

	fmt.Printf("→ Downloading %s...\n", info.AssetName)

	// Temp artifact extension must match the asset kind so the OS treats it
	// correctly: an executable on Windows, a mountable image on macOS.
	pattern := "agent_update_*"
	switch runtime.GOOS {
	case "windows":
		pattern = "agent_update_*.exe"
	case "darwin":
		pattern = "agent_update_*.dmg"
	}
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	// Clean up the temp file on any error path; on success the caller owns it.
	success := false
	defer func() {
		tmp.Close()
		if !success {
			os.Remove(tmp.Name())
		}
	}()

	resp, err := updateDownloadClient.Get(info.AssetURL) //nolint:gosec // URL comes from authenticated GitHub API response
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	// Cap download at 200 MB to prevent disk exhaustion from an unexpectedly
	// large or malicious response body. Release artifacts are typically < 50 MB;
	// 200 MB leaves plenty of headroom while still bounding disk usage.
	const maxBinarySize = 200 << 20 // 200 MB
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBinarySize)); err != nil {
		return "", err
	}

	// Verify the download produced a valid artifact (not an empty/truncated response)
	stat, err := tmp.Stat()
	if err != nil {
		return "", fmt.Errorf("cannot stat downloaded file: %w", err)
	}
	const minBinarySize = 1 << 20 // 1 MB — release artifacts are typically 10-50 MB
	if stat.Size() < minBinarySize {
		return "", fmt.Errorf("downloaded file too small (%d bytes), likely corrupted", stat.Size())
	}

	digestHex := hex.EncodeToString(h.Sum(nil))
	fmt.Printf("→ Downloaded %d MB (SHA-256: %s)\n", stat.Size()>>20, digestHex)

	// Verify SLSA build provenance attestation BEFORE applying the update.
	// Without this, the SHA-256 we just computed proves nothing — it only
	// tells us "we got back what was at the URL", not "what was at the URL
	// was actually built by our CI". See attestation.go for the threat model.
	//
	// errAttestationDisabled means the user explicitly opted out via env var
	// (firewall escape hatch); proceed but with a clear log line. Any other
	// error means we couldn't establish provenance — refuse to apply.
	fmt.Println("→ Verifying build provenance...")
	if err := verifyBuildProvenance(tmp.Name(), digestHex); err != nil {
		if !errors.Is(err, errAttestationDisabled) {
			return "", fmt.Errorf("attestation verification failed, refusing to apply update: %w", err)
		}
	}

	success = true // caller owns the verified temp artifact now
	return tmp.Name(), nil
}

// downloadAndApplyUpdate downloads + verifies the update and applies it right
// away (the manual "Update Now" / "Install Update" path — the user asked for it
// explicitly, so it does NOT drain first). The automatic path calls
// downloadAndVerifyUpdate and applyVerifiedUpdate separately with a drain in
// between.
func downloadAndApplyUpdate(info *UpdateInfo) error {
	// Non-capable macOS channels publish signed but intentionally unnotarized
	// DMGs. Their check-and-offer UI must not feed those artifacts into the
	// silent bundle-replacement Gatekeeper path; the user's click opens the
	// release asset in the browser for the ordinary manual installation flow.
	if !silentUpdateCapable() {
		if info == nil || info.AssetURL == "" {
			return errors.New("no update release available to open")
		}
		return openManualUpdateURL(info.AssetURL)
	}
	path, err := downloadAndVerifyUpdate(info)
	if err != nil {
		return err
	}
	fmt.Println("→ Restarting with new version...")
	return applyManualVerifiedUpdate(path, info, applyVerifiedUpdate)
}

// applyManualVerifiedUpdate transfers a verified artifact to the platform
// installer. A failed apply leaves the current version running, so ownership
// remains here and the artifact must be removed immediately rather than
// waiting for the next process start's temp cleanup.
func applyManualVerifiedUpdate(path string, info *UpdateInfo, apply func(string, *UpdateInfo) error) error {
	if err := apply(path, info); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// installUpdatable reports whether this installation can update itself without
// a prompt, and if not, a one-line human reason for the tray "update blocked"
// status item. It is a cheap, side-effect-free probe run before any download:
//   - Windows: blocked while still running from the machine-wide (Program Files)
//     location — the per-user relocation must complete first.
//   - macOS: blocked when running from a mounted DMG, ~/Downloads, or another
//     unsupported/read-only location (see darwinLocationSupported).
//   - Any platform: blocked when the install directory is not writable.
//
// When it cannot determine the answer it fails OPEN (updatable) rather than
// blocking a healthy install.
func installUpdatable() (bool, string) {
	exe, err := runningUpdateTarget()
	if err != nil {
		return true, ""
	}

	switch runtime.GOOS {
	case "windows":
		if isMachineWideWindowsInstall(exe) {
			return false, "Automatic update unavailable — run the installer once to move AI Expedite to a per-user location"
		}
	case "darwin":
		if ok, reason := darwinLocationSupported(exe); !ok {
			return false, reason
		}
		// ~/Applications and /Applications (pre-relocation) are supported.
		return true, ""
	}

	// Generic writability probe of the install directory (covers Linux and a
	// relocated Windows install).
	if !dirWritable(filepath.Dir(exe)) {
		return false, "Automatic update unavailable — this installation is read-only; move the app to a writable location"
	}
	return true, ""
}

// runningUpdateTarget returns the installed artifact that an update must
// replace. AppImage launches the embedded binary from a read-only mount, while
// APPIMAGE points at the writable outer artifact the user actually started.
// Raw Linux binaries and all other platforms use os.Executable directly.
func runningUpdateTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	target := effectiveUpdateTarget(exe, runtime.GOOS, os.Getenv("APPIMAGE"))
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return target, nil
}

func effectiveUpdateTarget(exe, goos, appImage string) string {
	appImage = strings.TrimSpace(appImage)
	if goos == "linux" && appImage != "" && filepath.IsAbs(appImage) {
		return filepath.Clean(appImage)
	}
	return exe
}

// darwinBundlePath maps the running Mach-O executable path
// (<App>.app/Contents/MacOS/<binary>) back to the enclosing <App>.app bundle
// directory. Returns "" if exe is not inside a .app bundle. Pure string logic
// so it is testable and available on every platform (the location probe below
// runs from installUpdatable, which is compiled everywhere).
func darwinBundlePath(exe string) string {
	exe = filepath.ToSlash(exe)
	marker := ".app/Contents/MacOS/"
	idx := strings.Index(exe, marker)
	if idx == -1 {
		return ""
	}
	return exe[:idx+len(".app")]
}

// darwinLocationSupported reports whether a macOS install can self-update from
// its current location, with a human reason when it cannot. Supported: the
// user-writable ~/Applications (the new default) and /Applications before
// relocation. Unsupported: a mounted DMG, ~/Downloads, or any other read-only
// / ad-hoc path — those run normally but never self-update and say so.
func darwinLocationSupported(exe string) (bool, string) {
	exe = filepath.ToSlash(exe)
	bundle := darwinBundlePath(exe)
	if bundle == "" {
		// Not a bundled app (e.g. a raw `go run` build); fall back to a plain
		// writability check on the directory holding the executable.
		if !dirWritable(filepath.Dir(exe)) {
			return false, "Automatic update unavailable — this location is read-only; move AI Expedite to ~/Applications"
		}
		return true, ""
	}
	lower := strings.ToLower(bundle)
	home, _ := os.UserHomeDir()

	if strings.HasPrefix(lower, "/volumes/") {
		return false, "Automatic update unavailable — running from a mounted disk image; move AI Expedite to ~/Applications"
	}
	if home != "" && strings.HasPrefix(bundle, filepath.ToSlash(filepath.Join(home, "Downloads"))+"/") {
		return false, "Automatic update unavailable — running from Downloads; move AI Expedite to ~/Applications"
	}

	userApps := ""
	if home != "" {
		userApps = filepath.ToSlash(filepath.Join(home, "Applications"))
	}
	if (userApps != "" && strings.HasPrefix(bundle, userApps)) ||
		strings.HasPrefix(bundle, "/Applications") {
		return true, ""
	}

	// Any other location must at least be writable to update in place.
	if !dirWritable(filepath.Dir(bundle)) {
		return false, "Automatic update unavailable — this location is read-only; move AI Expedite to ~/Applications"
	}
	return true, ""
}

// isMachineWideWindowsInstall reports whether exe sits under a machine-wide
// Program Files location that requires elevation to write. Used to gate the
// automatic path until the per-user relocation has happened.
func isMachineWideWindowsInstall(exe string) bool {
	lower := strings.ToLower(filepath.Clean(exe))
	return strings.Contains(lower, `\program files\`) ||
		strings.Contains(lower, `\program files (x86)\`)
}

// dirWritable reports whether dir accepts a new file without elevation. It
// writes and removes a tiny probe file rather than inspecting permission bits,
// which are unreliable across platforms (ACLs, read-only mounts, MDM).
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	f, err := os.CreateTemp(dir, ".aixupd_probe_*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
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
