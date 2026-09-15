package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// setupIsolatedGrokHome creates a per-session temp dir to use as the child's
// GROK_HOME and seeds it with the minimum surfaces the CLI needs:
//
//   - a copy of the real `grok login` auth file, so cached-token auth keeps
//     working without us inheriting anything else from the user's real
//     ~/.grok (api_key, auto-approve, pinned requirements.toml, …)
//   - a minimal clean config.toml (`[cli]\ninstaller = "internal"\nauto_update = false\n`)
//     — `auto_update = false` suppresses the headless updater check, which can
//     otherwise race `grok agent stdio` and emit non-JSON stdout that readStream
//     would treat as a fatal `grok_acp_error`
//   - a directory link for conversations so transcripts survive the ephemeral
//     home, plus a private billing log seeded with the copied account identity;
//     waitForExit later persists only its normalized allowlisted snapshot
//
// This replaces the dead `--config <key>=` neutralizer machinery: grok 0.2.59
// rejects `--config` outright, so we can no longer clear persisted config via
// argv. Pointing GROK_HOME at an isolated dir that simply OMITS the dangerous
// persisted files neutralises every persisted-config vector by construction.
//
// Source auth file: `$GROK_HOME/auth.json` when GROK_HOME is set, else
// `~/.grok/auth.json`; `cached_token.json` is tried as a fallback name. A
// missing auth file is NOT fatal — we proceed with just the clean config.toml
// and let grok surface any auth error through the normal ACP handshake.
//
// allowAPIKeyFallback opts in to preserving the user's persistent
// `api_key = "..."` entry from the source `config.toml` into the isolated
// config. Without this, users who opted into API-key fallback but keep their
// key in `~/.grok/config.toml` (xAI's documented persistent form) and do NOT
// export XAI_API_KEY would silently lose API-key auth in the isolated session.
// Both the root `[model] api_key` form AND the documented per-model
// `[model.<runtimeModel>] api_key` form are carried over (the per-model match
// for the resolved runtime model wins when both exist — mirroring grok's own
// precedence in the un-isolated config). All other persisted config
// (approval/permission knobs, other model.* fields, other tables) stays
// excluded by design.
//
// Returns the temp dir path. The caller (Start) owns its lifecycle and removes
// it through removeIsolatedGrokHome after the child exits (waitForExit) or on
// any pre-spawn failure.
func setupIsolatedGrokHome(allowAPIKeyFallback bool, runtimeModel string) (string, error) {
	return setupIsolatedGrokHomeFrom(allowAPIKeyFallback, runtimeModel, grokPersistentHome())
}

func setupIsolatedGrokHomeFrom(allowAPIKeyFallback bool, runtimeModel, srcBase string) (string, error) {
	return setupIsolatedGrokHomeWithSessionStore(allowAPIKeyFallback, runtimeModel, srcBase, true)
}

// setupIsolatedGrokSmokeHomeFrom creates the same auth-only, MCP-disabled home
// as the ACP path without linking the user's persistent conversation store.
// A one-shot maintenance smoke never resumes a conversation, so exposing that
// store would add filesystem surface without serving the smoke contract.
func setupIsolatedGrokSmokeHomeFrom(srcBase string) (string, error) {
	return setupIsolatedGrokHomeWithSessionStore(false, "", srcBase, false)
}

func setupIsolatedGrokHomeWithSessionStore(allowAPIKeyFallback bool, runtimeModel, srcBase string, linkSessionStore bool) (string, error) {
	dir, err := os.MkdirTemp("", "grok-acp-home-")
	if err != nil {
		return "", fmt.Errorf("create isolated grok home: %w", err)
	}
	// Registration and the credential copy happen under the login lock, as
	// one step: a renewal (the keeper's or the live probe's) can then neither
	// rotate the login between the source read and the destination write,
	// nor reconcile past a home that is registered but not yet seeded — either
	// would launch this child on a credential the rotation just superseded.
	// Every removal path releases the registration.
	if err := seedIsolatedGrokLogin(dir, srcBase); err != nil {
		return "", err
	}
	if lerr := seedGrokManagedBillingIdentity(dir); lerr != nil {
		fmt.Printf("%s[grok-acp] managed billing identity not seeded (usage freshness may be unavailable): %v%s\n",
			colorYellow, lerr, colorReset)
	}

	// Minimal clean config.toml — deliberately carries no approval/permission
	// knobs, so none of the user's real persisted policy leaks into the
	// isolated session. When allowAPIKeyFallback is true and the source
	// `config.toml` contains either `[model] api_key = "..."` OR the
	// per-model `[model.<runtimeModel>] api_key = "..."` form, that single
	// line is carried over (under the same section header it came from) so
	// the opt-in fallback also works for users whose key lives in xAI's
	// documented persistent form (not just `XAI_API_KEY`).
	// `auto_update = false` matches xAI's documented headless/scripting guidance:
	// without it, an updater check can race `grok agent stdio` and dump non-JSON
	// stdout that readStream treats as a fatal `grok_acp_error`.
	//
	// `[compat.cursor] mcps = false` + `[compat.claude] mcps = false` suppress
	// grok's vendor-MCP scan of the HOST's `~/.cursor/mcp.json` and
	// `~/.claude.json` — those files live outside $GROK_HOME so the isolated
	// dir alone can't hide them, and a slow vendor MCP (e.g. a `visualization`
	// proxy) otherwise blocks `session/new` ~10s before the ACP turn times out.
	cfg := "[cli]\ninstaller = \"internal\"\nauto_update = false\n" +
		"\n[compat.cursor]\nmcps = false\n" +
		"\n[compat.claude]\nmcps = false\n"
	if allowAPIKeyFallback && srcBase != "" {
		section, apiKey := readGrokPersistedAPIKey(filepath.Join(srcBase, "config.toml"), runtimeModel)
		if apiKey != "" {
			cfg += "\n[" + section + "]\napi_key = " + apiKey + "\n"
		}
	}
	if werr := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); werr != nil {
		cleanupErr := removeIsolatedGrokHome(dir)
		return "", errors.Join(fmt.Errorf("write isolated config.toml: %w", werr), cleanupErr)
	}

	if linkSessionStore {
		// Point `sessions` at the persistent per-device conversation store. Grok
		// keys transcripts by GROK_HOME, so without this the store dies with the
		// temp dir and every cross-session `session/load` fails FS_NOT_FOUND (see
		// grok_session_store.go). Non-fatal by design: a session with an
		// ephemeral store still runs, it just cannot be reattached later.
		if lerr := linkGrokSessionStore(dir); lerr != nil {
			fmt.Printf("%s[grok-acp] conversation store not persisted (resume will cold-start): %v%s\n",
				colorYellow, lerr, colorReset)
		}
		pruneGrokSessionStoreOnce()
	}

	return dir, nil
}

// grokLoginSeedWait bounds how long creating an isolated home waits for the
// login lock. Longer than a whole CLI renewal, which is the longest anything
// holds it; a lock held past that is a stuck renewal, and the home is then
// not created rather than seeded beside it.
var grokLoginSeedWait = grokLoginRenewTimeout + 5*time.Second

// seedIsolatedGrokLogin registers dir as a copy of the login and copies the
// credential into it, under the login lock. A missing/unreadable source is
// tolerated (grok surfaces the auth error through the normal ACP flow); the
// dir is removed on a write failure or when the lock never came free.
func seedIsolatedGrokLogin(dir, srcBase string) error {
	ctx, cancel := context.WithTimeout(context.Background(), grokLoginSeedWait)
	defer cancel()
	release, ok := grokLogin.beginRenewal(ctx)
	if !ok {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("create isolated grok home: the login was busy renewing for %s", grokLoginSeedWait)
	}
	defer release()
	grokLogin.registerCopyHeld(dir)
	if srcBase == "" {
		return nil
	}
	for _, name := range []string{"auth.json", "cached_token.json"} {
		data, rerr := os.ReadFile(filepath.Join(srcBase, name))
		if rerr != nil {
			continue
		}
		if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil {
			grokLogin.releaseCopy(dir)
			return errors.Join(fmt.Errorf("copy grok auth file %s: %w", name, werr), os.RemoveAll(dir))
		}
	}
	return nil
}

// grokCopyCredentialPreserved reports whether deleting copyHome loses nothing:
// the copy holds no credential, its credential is another account's (the
// real home was re-logged-in underneath it; not ours to carry), the real
// home is signed out (a logout is not undone by a copy), or the real home
// already holds a credential at least as new as the copy's.
func grokCopyCredentialPreserved(base, copyHome string) bool {
	copyStamp, ok := readGrokCredentialStamp(copyHome)
	if !ok {
		return true
	}
	real, ok := readGrokCredentialStamp(base)
	if !ok || real.Fingerprint != copyStamp.Fingerprint {
		return true
	}
	return !real.MintedAt.Before(copyStamp.MintedAt)
}

// removeIsolatedGrokHome unlinks the persistent sessions store before
// recursively removing the ephemeral home. If unlinking cannot be verified,
// it leaves the home and sessions entry in place and removes only siblings;
// leaking a small temp directory is safer than risking persistent transcripts.
func removeIsolatedGrokHome(home string) error {
	return removeIsolatedGrokHomeWithUnlink(home, unlinkGrokDirectory)
}

func removeIsolatedGrokHomeWithUnlink(home string, unlink func(string) error) error {
	return removeIsolatedGrokHomeAttempt(home, grokPersistentHome(), unlink, 0)
}

// errGrokLoginBusy: the home was NOT removed this time because its login copy
// could not be reconciled while a renewal held the login; a retry is
// scheduled and the copy stays registered until then.
var errGrokLoginBusy = errors.New("grok login copy not reconciled yet: renewal in flight")

// grokLoginRemovalRetries bounds how many times a removal retries after a
// renewal in flight; past that the copy is still kept (files and registration)
// rather than deleted with a credential nothing else holds.
const grokLoginRemovalRetries = 3

// removeIsolatedGrokHomeAttempt is one removal attempt. base is the real home
// the copy belongs to, resolved once on the first attempt so a deferred retry
// never reconciles against whatever GROK_HOME points at when it fires. A
// retry for a home that has since been released (removed by another path)
// is a no-op.
func removeIsolatedGrokHomeAttempt(home, base string, unlink func(string) error, attempt int) error {
	if home == "" {
		return nil
	}
	if attempt > 0 && !grokLogin.holds(home) {
		return nil
	}
	// The copy may hold the account's newest credential — a `grok models`
	// discovery that refreshed and is being removed seconds later never lives
	// to see a keeper tick. Reconcile while it is still registered, so its
	// renewal reaches the real home before the file is deleted. The wait
	// outlasts a whole CLI renewal; when even that is not enough, the copy is
	// kept — files and registration — and the removal retries later, rather
	// than deleting a credential nothing else holds.
	// Getting the lock is not the same as getting the credential across: the
	// write into the real home can still fail (the CLI holding auth.json.lock
	// past our wait, a sharing violation on Windows). So the test is on the
	// outcome — the real home must hold a credential at least as new as this
	// copy's — and not on whether the pass ran.
	_, ok := reconcileGrokLoginWithin(base, grokLoginRemovalReconcileWait)
	if ok && !grokCopyCredentialPreserved(base, home) {
		ok = false
	}
	if !ok {
		fmt.Printf("%s[grok-acp] isolated home kept for now (attempt %d): its login copy could not be handed to the real home yet%s\n",
			colorYellow, attempt+1, colorReset)
		if attempt < grokLoginRemovalRetries {
			time.AfterFunc(grokLoginRemovalReconcileWait, func() {
				_ = removeIsolatedGrokHomeAttempt(home, base, unlink, attempt+1)
			})
		}
		return errGrokLoginBusy
	}
	defer grokLogin.releaseCopy(home)

	link := filepath.Join(home, grokSessionsDirName)
	if err := unlink(link); err != nil {
		if errors.Is(err, errGrokStoreNotLinked) {
			fmt.Printf("%s[grok-acp] conversation store was never linked; resume will cold-start%s\n",
				colorYellow, colorReset)
			return os.RemoveAll(home)
		}

		cleanupErr := removeIsolatedGrokHomeSiblings(home)
		return errors.Join(fmt.Errorf("unlink isolated grok session store: %w", err), cleanupErr)
	}

	return os.RemoveAll(home)
}

// cleanupIsolatedGrokHome is the non-fatal lifecycle wrapper used once a
// caller already has a primary result to return or publish. It keeps cleanup
// failures observable without changing session/auth error semantics.
func cleanupIsolatedGrokHome(home, sessionID string) {
	if err := removeIsolatedGrokHome(home); err != nil && !errors.Is(err, errGrokLoginBusy) {
		fmt.Printf("%s[grok-acp] isolated home cleanup failed for %s: %v%s\n",
			colorYellow, sessionID, err, colorReset)
	}
}

func removeIsolatedGrokHomeSiblings(home string) error {
	entries, err := os.ReadDir(home)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list isolated grok home after unlink failure: %w", err)
	}

	var cleanupErrs []error
	for _, entry := range entries {
		isSessionsEntry := entry.Name() == grokSessionsDirName
		if runtime.GOOS == "windows" {
			isSessionsEntry = strings.EqualFold(entry.Name(), grokSessionsDirName)
		}
		if isSessionsEntry {
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, entry.Name())); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove isolated grok home entry %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(cleanupErrs...)
}
