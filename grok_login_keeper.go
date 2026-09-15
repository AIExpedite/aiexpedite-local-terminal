// grok_login_keeper.go — keeps the real Grok login alive on a computer whose
// Grok runs all happen on COPIES of it.
//
// Grok Build's login (`~/.grok/auth.json`) is a 6-hour access token plus a
// refresh token that ROTATES on every use: the refresh answers with a new pair,
// and the old refresh token stays redeemable only for a short grace window
// (measured 2026-09-15: accepted 44 s after rotation, rejected 2.5 min after).
// A home whose refresh is rejected is signed out — the CLI deletes its
// auth.json — and the user runs `grok login` again.
//
// Every Grok run this agent starts (an ACP session, a model discovery, a
// maintenance smoke) runs on an isolated temp copy of that file which is
// deleted when the run ends (grok_isolated_home.go). Without this file, the
// first copy to reach the 6-hour mark refreshed, took the only live token with
// it when it was removed, and left the real home holding a refresh token xAI
// revoked two minutes later. The next thing to touch the real home — the
// user's own `grok`, or a new copy taken after the 6-hour mark — was signed
// out. Every computer running the agent needed `grok login` every 6 hours.
//
// The rule that fixes it: THE NEWEST CREDENTIAL OF THE ACCOUNT WINS EVERYWHERE.
//
//   - The agent renews the real home itself, well ahead of expiry
//     (grokLoginKeepAhead), by running `grok models` against it with
//     GROK_AUTH_EARLY_INVALIDATION_SECS raised so the CLI refreshes now rather
//     than 5 minutes before the end. A copy therefore never reaches its own
//     refresh point while the keeper runs.
//   - After every renewal, and on every tick, the newest same-account
//     credential across the real home and every live copy is written to all
//     the others: a copy that did refresh on its own (a 401 mid-session, the
//     keeper down) is written BACK to the real home within one tick — inside
//     the grace window — and a renewed real home is fanned OUT to the copies.
//     Grok hot-reloads auth.json, so a running session picks the new file up.
//   - Credentials are ordered by the `create_time` the CLI stamps on every
//     refresh (falling back to `expires_at`), and only same-account files are
//     ever mixed: a copy of an account the user has since logged out of is
//     left alone.
//
// The keeper also sweeps stale `grok-acp-home-*` directories out of the temp
// dir: 178 of them had accumulated on one computer since July, each once
// holding a copy of the login.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// grokLoginKeepAhead is how far before the access token's expiry the real
	// home is renewed. Far more than the CLI's own 5-minute early refresh, so
	// no copy ever refreshes on its own while the keeper is running.
	grokLoginKeepAhead = 30 * time.Minute
	// grokLoginKeeperTick bounds how long a copy's self-refresh can go
	// unnoticed. It must stay well inside the ~2-minute grace window in which
	// the real home's superseded refresh token is still redeemable.
	grokLoginKeeperTick = 20 * time.Second
	// grokLoginRenewRetry spaces renewal attempts that changed nothing (CLI
	// missing, xAI unreachable) so a broken renewal is not spawned every tick.
	grokLoginRenewRetry = 5 * time.Minute
	// grokStaleIsolatedHomeAge is how old an unregistered grok-acp-home-*
	// directory must be before the sweep removes it. Long enough that a home
	// belonging to a session of a previous agent process that is still
	// running (an update restart) is never pulled out from under it.
	grokStaleIsolatedHomeAge   = 24 * time.Hour
	grokStaleIsolatedHomeSweep = 24 * time.Hour
	// grokIsolatedHomePrefix is the temp-dir prefix every isolated home uses.
	grokIsolatedHomePrefix = "grok-acp-home-"
	// grokAuthEarlyInvalidationEnv is the CLI's own knob: seconds before
	// `expires_at` at which it treats the access token as expired and
	// refreshes on the next run.
	grokAuthEarlyInvalidationEnv = "GROK_AUTH_EARLY_INVALIDATION_SECS"
)

// grokCredentialStamp is what the keeper compares: WHEN a credential was
// minted and for WHOM. It never holds the token.
type grokCredentialStamp struct {
	Home        string
	Fingerprint string
	// MintedAt orders credentials of one account: the CLI's `create_time`,
	// else `expires_at` (the same instant shifted by the token lifetime).
	MintedAt  time.Time
	ExpiresAt time.Time
	// Refreshable: the selected entry carries a refresh token, so renewing
	// it is Grok's job and not a `grok login`.
	Refreshable bool
}

// readGrokCredentialStamp reads the stamp of the credential Grok's resolver
// would present from a home. ok is false when the home holds no usable
// credential.
func readGrokCredentialStamp(home string) (grokCredentialStamp, bool) {
	raw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return grokCredentialStamp{}, false
	}
	var scoped map[string]struct {
		Key          string `json:"key"`
		Token        string `json:"token"`
		AccessToken  string `json:"access_token"`
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		CreateTime   string `json:"create_time"`
		ExpiresAt    string `json:"expires_at"`
	}
	if json.Unmarshal(raw, &scoped) != nil || len(scoped) == 0 {
		return grokCredentialStamp{}, false
	}
	keys := make([]string, 0, len(scoped))
	for key := range scoped {
		keys = append(keys, key)
	}
	for _, key := range grokScopeKeysByPrecedence(keys) {
		entry := scoped[key]
		if grokScopedCredential(entry.Key, entry.Token, entry.AccessToken, entry.IDToken) == "" {
			continue
		}
		stamp := grokCredentialStamp{
			Home:        home,
			Fingerprint: grokAccountFingerprintFor(home),
			Refreshable: entry.RefreshToken != "",
		}
		if t, err := time.Parse(time.RFC3339Nano, entry.ExpiresAt); err == nil {
			stamp.ExpiresAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, entry.CreateTime); err == nil {
			stamp.MintedAt = t
		} else {
			stamp.MintedAt = stamp.ExpiresAt
		}
		if stamp.Fingerprint == "" || stamp.MintedAt.IsZero() {
			return grokCredentialStamp{}, false
		}
		return stamp, true
	}
	return grokCredentialStamp{}, false
}

// replaceGrokAuthFile installs src's auth.json into dstHome atomically (write
// beside, rename over), so Grok's hot reload never sees a half-written file.
func replaceGrokAuthFile(dstHome, srcHome string) error {
	data, err := os.ReadFile(filepath.Join(srcHome, "auth.json"))
	if err != nil {
		return err
	}
	dst := filepath.Join(dstHome, "auth.json")
	tmp := dst + ".aix-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	// atomicReplaceConfigFile is MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH) on
	// Windows and rename(2) elsewhere: the replacement is one step, over an
	// existing file, on every platform the agent runs on.
	if err := atomicReplaceConfigFile(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// reconcileGrokLogin applies the newest-wins rule across the real home and
// every live copy of the real home's account. Returns how many homes were
// rewritten. Safe to call at any time: reading rotates nothing, and a rewrite
// only ever replaces an OLDER credential of the SAME account with a newer one.
//
// A real home with no credential is never recreated from a copy. That absence
// is a decision or a fact — the user ran `grok logout`, or the CLI signed the
// home out — and copies of a login that no longer exists are the CLI's to
// fail, not the keeper's to resurrect; the copies are also not consulted for
// what the account is, so two of them holding different accounts can never
// sign the real home into one of them.
func reconcileGrokLogin(base string) int {
	if base == "" {
		return 0
	}
	real, realOK := readGrokCredentialStamp(base)
	if !realOK {
		return 0
	}
	account := real.Fingerprint
	stamps := []grokCredentialStamp{real}
	for _, home := range grokLogin.liveCopies() {
		if s, ok := readGrokCredentialStamp(home); ok {
			stamps = append(stamps, s)
		}
	}
	if len(stamps) < 2 {
		return 0
	}
	newest := real
	for _, s := range stamps {
		if s.Fingerprint == account && s.MintedAt.After(newest.MintedAt) {
			newest = s
		}
	}
	rewritten := 0
	if newest.Home != base {
		if err := replaceGrokAuthFile(base, newest.Home); err != nil {
			fmt.Printf("%s[grok-login] could not write the renewed login back to the real home: %v%s\n",
				colorYellow, err, colorReset)
		} else {
			rewritten++
			fmt.Printf("%s[grok-login] wrote a session's renewed login back to the real home%s\n",
				colorGreen, colorReset)
		}
	}
	for _, s := range stamps {
		if s.Home == base || s.Home == newest.Home || s.Fingerprint != account || !s.MintedAt.Before(newest.MintedAt) {
			continue
		}
		if err := replaceGrokAuthFile(s.Home, newest.Home); err != nil {
			fmt.Printf("%s[grok-login] could not fan the renewed login out to a session copy: %v%s\n",
				colorYellow, err, colorReset)
			continue
		}
		rewritten++
	}
	return rewritten
}

// grokLoginKeeperState is the keeper's memory between ticks.
type grokLoginKeeperState struct {
	mu            sync.Mutex
	lastRenewTry  time.Time
	lastRenewFrom time.Time
	lastSweep     time.Time
}

var grokKeeper = &grokLoginKeeperState{}

// grokLoginKeeperOnce is one tick: renew the real home when its access token
// is inside grokLoginKeepAhead, then reconcile. grokPath may be "" (the CLI is
// not installed or not on PATH): reconciliation still runs, renewal is skipped.
// Returns whether a renewal was attempted.
func grokLoginKeeperOnce(ctx context.Context, grokPath string, now time.Time) bool {
	base := grokPersistentHome()
	if base == "" {
		return false
	}
	renewed := false
	if stamp, ok := readGrokCredentialStamp(base); ok && stamp.Refreshable && !stamp.ExpiresAt.IsZero() &&
		grokPath != "" && !now.Add(grokLoginKeepAhead).Before(stamp.ExpiresAt) {
		grokKeeper.mu.Lock()
		// One attempt per credential per retry window: a renewal that left
		// the same token in place is not retried every tick.
		due := !stamp.MintedAt.Equal(grokKeeper.lastRenewFrom) || now.Sub(grokKeeper.lastRenewTry) >= grokLoginRenewRetry
		if due {
			grokKeeper.lastRenewTry = now
			grokKeeper.lastRenewFrom = stamp.MintedAt
		}
		grokKeeper.mu.Unlock()
		if due {
			renewCtx, cancel := context.WithTimeout(ctx, grokLoginRenewTimeout)
			if release, ok := grokLogin.beginRenewal(renewCtx); ok {
				runGrokLoginRenewal(ctx, grokPath, base)
				release()
				renewed = true
			}
			cancel()
			if after, ok := readGrokCredentialStamp(base); ok && after.MintedAt.After(stamp.MintedAt) {
				fmt.Printf("%s[grok-login] renewed the Grok login ahead of expiry (next expiry %s)%s\n",
					colorGreen, after.ExpiresAt.UTC().Format(time.RFC3339), colorReset)
			} else if renewed {
				fmt.Printf("%s[grok-login] renewal ran but the login did not change; retrying in %s%s\n",
					colorYellow, grokLoginRenewRetry, colorReset)
			}
		}
	}
	reconcileGrokLogin(base)
	return renewed
}

// grokLoginKeeperBinary resolves the CLI the keeper renews with. "" when it
// is not installed; the keeper then only reconciles.
func grokLoginKeeperBinary() string {
	if path, err := exec.LookPath("grok"); err == nil {
		return path
	}
	return ""
}

// runGrokLoginKeeper is the background loop, started once at agent startup.
func runGrokLoginKeeper(ctx context.Context) {
	sweepStaleIsolatedGrokHomes(time.Now())
	ticker := time.NewTicker(grokLoginKeeperTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			grokLoginKeeperOnce(ctx, grokLoginKeeperBinary(), now)
			grokKeeper.mu.Lock()
			sweepDue := now.Sub(grokKeeper.lastSweep) >= grokStaleIsolatedHomeSweep
			grokKeeper.mu.Unlock()
			if sweepDue {
				sweepStaleIsolatedGrokHomes(now)
			}
		}
	}
}

// grokLoginRenewEnv is the environment a renewal child runs with: the
// sanitized maintenance environment (every GROK_* sink and XAI_API_KEY
// stripped, so the cached login is the only credential it can use), the home
// to renew, and the early-invalidation knob raised to the keeper's horizon so
// the CLI refreshes NOW rather than five minutes before the end.
func grokLoginRenewEnv(base string) []string {
	env := setEnvVar(sanitizeGrokMaintenanceSmokeEnv(os.Environ()), "GROK_HOME", base)
	return setEnvVar(env, grokAuthEarlyInvalidationEnv, strconv.Itoa(int(grokLoginKeepAhead/time.Second)))
}

// sweepStaleIsolatedGrokHomes removes grok-acp-home-* directories that no
// live copy owns and that have not been touched for grokStaleIsolatedHomeAge.
// Each is removed through removeIsolatedGrokHome, so a linked conversation
// store is unlinked first and never deleted. Returns how many were removed.
func sweepStaleIsolatedGrokHomes(now time.Time) int {
	grokKeeper.mu.Lock()
	grokKeeper.lastSweep = now
	grokKeeper.mu.Unlock()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0
	}
	live := map[string]struct{}{}
	for _, home := range grokLogin.liveCopies() {
		live[filepath.Clean(home)] = struct{}{}
	}
	removed, failed := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), grokIsolatedHomePrefix) {
			continue
		}
		dir := filepath.Join(os.TempDir(), entry.Name())
		if _, ok := live[filepath.Clean(dir)]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < grokStaleIsolatedHomeAge {
			continue
		}
		if err := removeIsolatedGrokHome(dir); err != nil {
			failed++
			continue
		}
		removed++
	}
	if removed > 0 || failed > 0 {
		fmt.Printf("%s[grok-login] swept %d stale isolated Grok home(s) from the temp dir (%d could not be removed)%s\n",
			colorCyan, removed, failed, colorReset)
	}
	return removed
}
