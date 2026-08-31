package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Copy the auth file under the first name that exists. Best-effort: a
	// missing/unreadable source is tolerated (grok surfaces the auth error
	// through the normal ACP flow).
	if srcBase != "" {
		for _, name := range []string{"auth.json", "cached_token.json"} {
			src := filepath.Join(srcBase, name)
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				continue
			}
			if werr := os.WriteFile(filepath.Join(dir, name), data, 0o600); werr != nil {
				cleanupErr := removeIsolatedGrokHome(dir)
				return "", errors.Join(fmt.Errorf("copy grok auth file %s: %w", name, werr), cleanupErr)
			}
		}
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

// removeIsolatedGrokHome unlinks the persistent sessions store before
// recursively removing the ephemeral home. If unlinking cannot be verified,
// it leaves the home and sessions entry in place and removes only siblings;
// leaking a small temp directory is safer than risking persistent transcripts.
func removeIsolatedGrokHome(home string) error {
	return removeIsolatedGrokHomeWithUnlink(home, unlinkGrokDirectory)
}

func removeIsolatedGrokHomeWithUnlink(home string, unlink func(string) error) error {
	if home == "" {
		return nil
	}

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
	if err := removeIsolatedGrokHome(home); err != nil {
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
		if entry.Name() == grokSessionsDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, entry.Name())); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove isolated grok home entry %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(cleanupErrs...)
}
