// cliagent_usage_musecode.go — Meta Muse Code (`muse`) usage parser.
//
// An identity/readiness probe only: binary, version, and whether the CLI has a
// credential. Muse Code publishes no local usage or quota file we can read, so
// the snapshot carries NO metrics — the card shows "metrics unknown" rather
// than placeholder bars for a limit window nobody can observe. Emitting the
// entry at all is what lists the agent on the Computers tab.
//
// # WHY AUTH FAILS OPEN
//
// Muse Code authenticates by `muse login` (browser / device code, stored in
// its config dir) or META_API_KEY, and has no `muse account status` command.
// The on-disk credential layout is not documented, so only POSITIVE evidence
// is reported: an API key in the environment or a credential file in a known
// config location reads "ready"; anything else reads "unknown", which matches
// none of the frontend's Login-required branches. Reporting
// "unauthenticated" on a guessed path would paint a red chip on a working
// install the user cannot act on — the worst failure this parser can have.
//
// SECRETS: never reads a credential's contents — presence only.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	museCodeAuthReady   = "ready"
	museCodeAuthUnknown = "unknown"
)

type museCodeUsageParser struct{}

func (museCodeUsageParser) Provider() string { return "museCode" }

// museCodeCredentialFileNames are the candidate credential files inside the
// Muse Code config dir. Unverified names, deliberately a short list: a miss
// degrades to "unknown", never to a Login-required chip.
var museCodeCredentialFileNames = []string{"credentials.json", "auth.json", "credentials"}

func (p museCodeUsageParser) Parse(home string, detected detectedCLIAgent, now time.Time) (*cliAgentUsage, bool) {
	usage := &cliAgentUsage{
		CliAgentID:  "museCode",
		Provider:    p.Provider(),
		Name:        firstNonEmpty(detected.Name, "Muse Code"),
		Version:     detected.Version,
		Path:        detected.Path,
		DataSource:  "muse credentials",
		CollectedAt: now.UTC().Format(time.RFC3339),
		// Muse Code reports no login deadline of its own.
		LoginExpirationState: loginExpirationNotReported,
	}
	if museCodeHasCredential(home, os.Getenv) {
		usage.AuthState = museCodeAuthReady
		usage.Authenticated = authBoolPtr(true)
	} else {
		// Authenticated stays NIL: the frontend reads `false` as a hard
		// Login-required signal.
		usage.AuthState = museCodeAuthUnknown
	}
	return usage, true
}

// museCodeHasCredential reports positive evidence of a usable credential.
func museCodeHasCredential(home string, getenv func(string) string) bool {
	if strings.TrimSpace(getenv("META_API_KEY")) != "" {
		return true
	}
	for _, dir := range museCodeConfigDirs(home, getenv) {
		for _, name := range museCodeCredentialFileNames {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() && info.Size() > 0 {
				return true
			}
		}
	}
	return false
}

// museCodeConfigDirs returns candidate config dirs: $XDG_CONFIG_HOME/muse,
// then ~/.config/muse (Muse Code resolves user skills from the same roots).
func museCodeConfigDirs(home string, getenv func(string) string) []string {
	var dirs []string
	if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "muse"))
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".config", "muse"))
	}
	return dirs
}
