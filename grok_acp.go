// File: grok_acp.go
// -----------------------------------------------------------------------------
// GrokACPManager — long-lived `grok agent stdio` sessions used by AI Expedite
// to drive xAI Grok via its ACP (Agent Client Protocol) JSON-RPC 2.0 interface
// over the child process' stdio (newline-delimited JSON).
//
// The transport, framing, fail-fast policy, lifecycle and cleanup story live
// in the shared ACP core (acp_core.go) — this file contributes only the
// Grok-specific configuration: the `grok_acp_*` result-type names, the argv
// builder/sanitiser, the env policy, and the isolated-GROK_HOME spawn setup.
//
// Auth posture is enforced by the orchestrator, not here: per the feature
// brief, the orchestrator's `authenticate` flow MUST prefer Grok's
// `cached_token` (the user's local `grok login`) so usage ties to the
// terminal computer user's account/subscription. An `xai.api_key` /
// `XAI_API_KEY` fallback is opt-in only. This file is responsible for
// preserving the local Grok auth state (XAI_API_KEY and the `GROK_*` config
// dir vars survive the env sanitiser) and stripping vars that would confuse
// Grok inside a nested agent (CLAUDECODE / CLAUDE_ / CODEX_IDE_*).
// -----------------------------------------------------------------------------

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// grokACPSpec parameterises the shared ACP core for the Grok family. The
// stall hint mirrors the historical watchdog wording: a silent `grok agent
// stdio` is almost always blocked on an interactive re-authentication it
// cannot present over headless stdio.
var grokACPSpec = acpSpec{
	family:      "grok_acp",
	logTag:      "grok-acp",
	noun:        "grok acp",
	agentName:   "grok",
	transport:   "grok agent stdio",
	startHint:   "is `grok` on PATH or in ~/.grok/bin? run `grok login` to authenticate",
	stallHint:   "it is most likely not signed in on this computer (its saved grok login/token expired; run `grok` in a terminal on the terminal computer to sign in again) or wedged at startup.",
	messageType: "grok_acp_message",
	stderrType:  "grok_acp_stderr",
	errorType:   "grok_acp_error",
	endedType:   "grok_acp_ended",
	// Grok usage-limit telemetry: the ACP transport is the primary path for
	// normal Grok sessions (`grok_acp_start`), and xAI surfaces the
	// `usage_limit_reached` / `credit_limit_*` / `allow_access:false`
	// signals as `session/update` notifications on that stdout stream. The
	// raw `session_start` path in session.go already calls
	// captureGrokUsageLimitLine; without this hook, the CLI Agents card
	// stays Unknown for the primary Grok flow.
	captureLine: captureGrokUsageLimitLine,
}

// GrokACPSession is the Grok-family view of a shared ACP session. Alias
// rather than a wrapper: the session carries no Grok-specific state (the
// isolated GROK_HOME rides in the generic CleanupDir field).
type GrokACPSession = acpSession

// GrokStartOptions bundles the per-session policy knobs the dispatcher reads
// from Config + commandMsg before spawning a Grok ACP child. Bundled rather
// than threading 4+ positional args so future policy additions (e.g. tool
// auto-approval mode) don't churn every call site.
type GrokStartOptions struct {
	// TimeoutMs is the backend-requested per-session deadline. 0 means
	// "no deadline" and the session lives until the 6h stale GC, End(),
	// orchestrator-driven cancellation, or the child's natural exit. Values
	// above acpMaxLifetime are clamped to acpMaxLifetime — a
	// runaway-orchestrator can't request a longer session than our GC
	// would tolerate anyway.
	TimeoutMs int64

	// AllowAPIKeyFallback, when false, strips XAI_API_KEY from the child
	// env and any `--api-key*` / `--auth*` extra args. Default false enforces
	// the feature-brief invariant that API-key auth is OPT-IN only —
	// otherwise a user with `export XAI_API_KEY=...` in their shell rc would
	// silently bill their xAI API wallet for every Grok session this
	// integration launches. Sourced from Config.EnableGrokAPIKeyFallback.
	AllowAPIKeyFallback bool

	// AllowAlwaysApprove, when false, strips `--always-approve` /
	// `--auto-approve` (and equivalent `-c approval.mode=always|auto` /
	// `-c tools.always_approve=true` / `-c tools.auto_approve=true` config
	// overrides) from the spawn argv. Default false enforces the feature
	// brief's conservative approval posture — autonomous tool execution
	// must be an explicit per-workspace opt-in, not something a signed
	// `grok_acp_start` can flip via extra args. Sourced from
	// Config.EnableGrokAlwaysApprove.
	AllowAlwaysApprove bool

	// WorkspaceRoot, when non-empty, is treated as a containment root: the
	// requested cwd must resolve (after EvalSymlinks) to a path strictly
	// inside this root. When empty, no containment check runs — but Start
	// still requires cwd to be absolute and exist. Sourced from
	// Config.WorkingDirectory at the dispatcher.
	WorkspaceRoot string
}

/* --------------------------------------------------------------------------
   GrokACPManager — Grok configuration over the shared ACP core
   -------------------------------------------------------------------------- */

// GrokACPManager owns the active `grok agent stdio` processes. One manager
// handles many concurrent sessions, mirroring SessionManager's shape. All
// generic lifecycle methods (Send/End/Get/ActiveCount/CleanupStale/
// ShutdownAll/ArmFirstFrameWatchdog) are promoted from the embedded core.
type GrokACPManager struct {
	acpManager
}

// NewGrokACPManager creates a fresh manager.
func NewGrokACPManager() *GrokACPManager {
	return &GrokACPManager{acpManager: newACPManager(grokACPSpec)}
}

// Start launches `grok agent stdio` in cwd. extraArgs are passed through
// after the built-in transport argv so the orchestrator can supply
// Grok-specific config knobs (e.g. `--model grok-2-fast`) without us
// special-casing every Grok flag. opts carries the per-session policy knobs
// (timeout, API-key gating, workspace containment) the dispatcher sourced
// from Config + commandMsg. The shared core owns cwd validation, containment
// and process lifecycle; the prepare callback below owns everything
// Grok-specific about the spawn.
func (m *GrokACPManager) Start(id, cwd string, extraArgs []string, workspaceID, uid string, opts GrokStartOptions, publishFn PublishFunc) error {
	return m.start(id, cwd, opts.WorkspaceRoot, opts.TimeoutMs, workspaceID, uid, publishFn, func() (acpSpawnPlan, error) {
		executable := resolveExecutable("grok")
		// PATH lookup miss is the common failure mode for macOS GUI/launchd
		// agents — Grok's installer drops the binary in ~/.grok/bin and only
		// touches shell rc, which the agent process never sources. Fall back to
		// the installer's default location before failing so a logged-in user
		// doesn't have to manually re-export PATH.
		if executable == "grok" {
			if p := resolveGrokInstallerBinary(); p != "" {
				executable = p
			}
		}
		// System-level requirements.toml (`/etc/grok/requirements.toml`) is NOT
		// redirected by GROK_HOME — that's the whole point of a system layer. The
		// per-session GROK_HOME isolation below neutralises the user-level layer
		// by omission, but a managed host that pins API-key auth or an always-
		// approve policy in the system file would still bypass the workspace's
		// opt-in gates. Fail closed before we spawn rather than silently launching
		// with the unsafe pinned posture. Opt out by setting both
		// EnableGrokAPIKeyFallback and EnableGrokAlwaysApprove to acknowledge the
		// pinned posture (or remove the system requirements file).
		if err := detectPinnedSystemGrokRequirements(opts.AllowAPIKeyFallback, opts.AllowAlwaysApprove); err != nil {
			return acpSpawnPlan{}, err
		}

		args := buildGrokACPArgs(extraArgs, opts.AllowAlwaysApprove)
		// args is `{"agent", "--model", <model>, ...}` by buildGrokACPArgs's
		// validated contract (see grokACPDefaultModel block); pull args[2] so
		// setupIsolatedGrokHome carries over the matching per-model api_key when
		// the user keeps it in the `[model.<resolvedModel>]` form.
		resolvedModel := grokACPDefaultModel
		if len(args) >= 3 && args[1] == "--model" {
			resolvedModel = args[2]
		}

		// Isolated GROK_HOME (replaces the old `--config <key>=` security
		// neutralizers, which are GONE as of grok 0.2.59 — `grok agent` rejects
		// `--config` / `--permission-mode` / `--no-auto-update` with "unexpected
		// argument", so the entire persisted-config-clear-via-argv approach is
		// dead). Instead we point the child at a per-session temp dir that
		// contains ONLY a copy of the real `grok login` auth file plus a minimal
		// clean config.toml. By NOT copying the user's real config.toml /
		// requirements.toml we neutralise every persisted-config vector by
		// omission: no `api_key` billing override, no auto-approve / permission
		// bypass, no pinned requirements layer. The cached-token handshake still
		// works because the auth file is the one piece we deliberately copy in.
		// Fail closed if isolation can't be established: with `--config` gone, the
		// argv has no neutralizers, so launching with the inherited (potentially
		// unsafe) GROK_HOME would silently bypass the workspace's opt-in gates.
		isolatedHome, err := setupIsolatedGrokHome(opts.AllowAPIKeyFallback, resolvedModel)
		if err != nil {
			return acpSpawnPlan{}, fmt.Errorf("grok ACP isolation setup failed; refusing to spawn with inherited GROK_HOME: %w", err)
		}

		env := sanitizeGrokACPEnv(os.Environ(), opts.AllowAPIKeyFallback)
		env = setEnvVar(env, "GROK_HOME", isolatedHome)

		return acpSpawnPlan{
			executable: executable,
			args:       args,
			logArgs:    redactGrokACPArgsForLog(args),
			env:        env,
			cleanupDir: isolatedHome,
		}, nil
	})
}

/* --------------------------------------------------------------------------
   argv + env builders
   -------------------------------------------------------------------------- */

// grokACPDefaultModel is the model the ACP child runs under unless the caller
// supplies its own `--model <x>` via extraArgs. Validated live against grok
// 0.2.59's ACP handshake (initialize → authenticate{cached_token} →
// session/new → session/prompt → end_turn).
const grokACPDefaultModel = "grok-build"

// buildGrokACPArgs constructs argv for `grok agent stdio`.
//
// VALIDATED CONTRACT (grok 0.2.59): the only supported shape is
//
//	grok agent --model <model> [--always-approve] stdio
//
// Two hard constraints discovered live against `grok agent --help`:
//
//  1. `grok agent` accepts ONLY a fixed flag set (--reauth, -m/--model,
//     --reasoning-effort, --always-approve, --agent-profile, --leader/
//     --no-leader, --grok-ws-*, --cli-chat-proxy-base-url,
//     --xai-api-base-url, --debug/--debug-file, --leader-socket). It does
//     NOT accept `--config`, `--permission-mode`, or `--no-auto-update` —
//     each is rejected with "unexpected argument". The entire `--config`-
//     based security-neutralizer approach the previous implementation used
//     is therefore dead; persisted-config vectors are now neutralised by the
//     isolated GROK_HOME set up in Start (see setupIsolatedGrokHome).
//  2. Flags MUST come BEFORE the `stdio` subcommand — `stdio` itself takes no
//     options, so anything after it is mis-parsed.
//
// Model selection: the default is grok-build. A caller-supplied `--model <x>`
// (or `--model=<x>`) in extraArgs REPLACES the default. Any other extraArgs
// that aren't valid `grok agent` flags — especially the now-rejected
// `--config*` / `--permission-mode*` / `--no-auto-update`, plus `--api-key*`
// and the POSIX `--` delimiter — are stripped by sanitizeGrokACPExtraArgs so
// a signed grok_acp_start can't smuggle an incompatible flag onto the argv.
//
// `--always-approve` is appended (between `--model <x>` and `stdio`) ONLY when
// allowAlwaysApprove is true. Default false keeps autonomous tool execution an
// explicit per-workspace opt-in (Config.EnableGrokAlwaysApprove) rather than
// something a signed grok_acp_start can flip via extra args.
//
// `--api-key{,-env}` / `--auth{,-method}` are NOT in `grok agent`'s accepted
// flag set (constraint #1 above) — passing them makes the child exit with
// "unexpected argument" instead of starting the JSON-RPC handshake. So they
// are stripped unconditionally by sanitizeGrokACPExtraArgs regardless of
// Config.EnableGrokAPIKeyFallback. The opt-in fallback flows through the
// supported channels instead: XAI_API_KEY env (preserved by sanitizeGrokACPEnv
// when AllowAPIKeyFallback=true) and the persisted `[model] api_key` line that
// setupIsolatedGrokHome copies into the isolated config.toml on the same gate.
func buildGrokACPArgs(extraArgs []string, allowAlwaysApprove bool) []string {
	model, sanitized := sanitizeGrokACPExtraArgs(extraArgs, grokACPDefaultModel, allowAlwaysApprove)

	args := []string{"agent", "--model", model}
	if allowAlwaysApprove {
		args = append(args, "--always-approve")
	}
	// Any remaining sanitized extras are valid `grok agent` flags the
	// orchestrator chose to pass through; they must precede the `stdio`
	// subcommand (constraint #2 above).
	args = append(args, sanitized...)
	args = append(args, "stdio")
	return args
}

// setupIsolatedGrokHome creates a per-session temp dir to use as the child's
// GROK_HOME and seeds it with exactly two things:
//
//   - a copy of the real `grok login` auth file, so cached-token auth keeps
//     working without us inheriting anything else from the user's real
//     ~/.grok (api_key, auto-approve, pinned requirements.toml, …)
//   - a minimal clean config.toml (`[cli]\ninstaller = "internal"\nauto_update = false\n`)
//     — `auto_update = false` suppresses the headless updater check, which can
//     otherwise race `grok agent stdio` and emit non-JSON stdout that readStream
//     would treat as a fatal `grok_acp_error`
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
// it after the child exits (waitForExit) or on any pre-spawn failure.
func setupIsolatedGrokHome(allowAPIKeyFallback bool, runtimeModel string) (string, error) {
	dir, err := os.MkdirTemp("", "grok-acp-home-")
	if err != nil {
		return "", fmt.Errorf("create isolated grok home: %w", err)
	}

	// Real ~/.grok base: prefer an inherited GROK_HOME (so a user who
	// relocated their grok dir is still honoured), else the OS home's .grok.
	srcBase := os.Getenv("GROK_HOME")
	if srcBase == "" {
		if home, herr := os.UserHomeDir(); herr == nil {
			srcBase = filepath.Join(home, ".grok")
		}
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
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("copy grok auth file %s: %w", name, werr)
			}
		}
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
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write isolated config.toml: %w", werr)
	}

	return dir, nil
}

// readGrokPersistedAPIKey returns the raw TOML value (quoted or literal, as
// found) of an `api_key` line from the given source `config.toml`, plus the
// section header it was found under (`"model"` or `"model.<runtimeModel>"`).
// Returns ("", "") when the file is missing/unreadable or no matching key is
// present. Returning the raw value preserves whichever quoting style the user
// wrote (`"xai-..."`, `'xai-...'`, or basic strings) without re-quoting
// heuristics that could corrupt embedded characters.
//
// Both the root `[model] api_key` form AND the documented per-model
// `[model.<name>] api_key` form (xAI Enterprise "API key example") are
// honoured, because users who explicitly opted into EnableGrokAPIKeyFallback
// shouldn't silently lose the fallback just because their persistent key lives
// in the per-model section that matches the model the agent runs under
// (default `grok-build`). Per-model match for `runtimeModel` takes precedence
// over the root `[model]` default — same precedence grok itself applies when
// it loads the un-isolated config — and the returned section header is mirrored
// into the isolated config so the carryover behaves identically.
//
// Tracks the active section using the same line-oriented sweep as
// detectPinnedSystemGrokRequirementsFile, with inline `#` strip and the same
// array-of-tables guard.
func readGrokPersistedAPIKey(path, runtimeModel string) (string, string) {
	if path == "" {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	const maxBytes = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(f, maxBytes))
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	var currentSection string
	var rootSection, rootValue string
	var perModelSection, perModelValue string
	perModelMatch := ""
	if runtimeModel != "" {
		perModelMatch = "model." + strings.ToLower(runtimeModel) + ".api_key"
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = grokTOMLStripInlineComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			currentSection = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		bareKey := strings.ToLower(strings.TrimSpace(line[:eq]))
		key := bareKey
		if currentSection != "" && !strings.Contains(bareKey, ".") {
			key = currentSection + "." + bareKey
		}
		if key == "model.api_key" && rootValue == "" {
			rootSection = "model"
			rootValue = strings.TrimSpace(line[eq+1:])
			continue
		}
		if perModelMatch != "" && key == perModelMatch && perModelValue == "" {
			perModelSection = "model." + strings.ToLower(runtimeModel)
			perModelValue = strings.TrimSpace(line[eq+1:])
		}
	}
	if perModelValue != "" {
		return perModelSection, perModelValue
	}
	return rootSection, rootValue
}

// grokSystemRequirementsPath is the documented system-level pinned-config
// layer (https://docs.x.ai/build/enterprise#configuration). Unlike user-level
// `~/.grok/requirements.toml`, it is NOT redirected by GROK_HOME — that's the
// point of a system file — so the per-session isolation in setupIsolatedGrokHome
// cannot neutralise pins set here. Operators relocate by overriding the var in
// tests; production reads it as-is.
var grokSystemRequirementsPath = "/etc/grok/requirements.toml"

// grokSystemManagedConfigPath is the second system-level layer xAI's
// enterprise loader reads. Unlike `~/.grok/managed_config.toml` (which IS
// redirected by GROK_HOME and therefore neutralised by setupIsolatedGrokHome),
// the system path is fixed and survives the isolation, so an operator pinning
// `model.api_key = "..."` or `permission_rules = ["Bash(*)"]` here would
// silently bypass `EnableGrokAPIKeyFallback` / `EnableGrokAlwaysApprove`.
// Scanned with the same line-oriented TOML logic as the requirements layer.
var grokSystemManagedConfigPath = "/etc/grok/managed_config.toml"

// claudeManagedSettingsPathsFn enumerates the Claude Code `managed-settings.json`
// locations xAI's Grok enterprise loader is documented to import
// `permissions.allow` rules from. These imports run BEFORE the per-tool prompt
// and are not redirected by GROK_HOME, so a non-empty allow list pinned here
// would route around `EnableGrokAlwaysApprove`. Per-OS system paths only —
// user-scope `~/.claude/settings.json` is intentionally NOT scanned because
// Grok's enterprise import is documented as the MDM-managed layer; treating
// ad-hoc user settings as pinned would over-fail-closed on the common
// single-user dev box. Held as a var so tests can inject paths.
var claudeManagedSettingsPathsFn = claudeManagedSettingsPaths

// detectPinnedSystemGrokRequirements refuses to start a session when a
// system-level Grok config layer pins API-key auth or a permissive approval
// policy AND the workspace has not opted into the matching gate. Both gates
// open ⇒ caller has acknowledged the pinned posture, so we let it through.
//
// Two TOML layers are scanned (`/etc/grok/requirements.toml` and
// `/etc/grok/managed_config.toml`) plus Claude Code's
// `managed-settings.json` system locations — none of these are redirected by
// GROK_HOME, so the per-session isolation in setupIsolatedGrokHome cannot
// neutralise pins set here. The TOML scan is intentionally minimal — a line-
// level keyword sweep, not a TOML parser — because the only goal here is to
// catch the dangerous markers the argv-strip surface in
// sanitizeGrokACPExtraArgs / sanitizeGrokACPEnv already neutralises at the
// per-process layer. Missing/unreadable files ⇒ skipped (best-effort by
// design; matches setupIsolatedGrokHome's tolerance for missing inputs).
func detectPinnedSystemGrokRequirements(allowAPIKey, allowAlwaysApprove bool) error {
	if allowAPIKey && allowAlwaysApprove {
		return nil
	}
	for _, p := range []string{grokSystemRequirementsPath, grokSystemManagedConfigPath} {
		if err := detectPinnedSystemGrokRequirementsFile(p, allowAPIKey, allowAlwaysApprove); err != nil {
			return err
		}
	}
	if !allowAlwaysApprove {
		// xAI's Grok enterprise loader documents importing Claude Code's
		// `managed-settings.json` and evaluating its `permissions.allow`
		// rules BEFORE the per-tool prompt
		// (https://docs.x.ai/build/enterprise#permissions). Those rules are
		// not under GROK_HOME, so the isolation cannot neutralise them; fail
		// closed when an MDM policy has set one and the workspace has not
		// opted into EnableGrokAlwaysApprove.
		for _, p := range claudeManagedSettingsPathsFn() {
			if hit, ok := detectClaudeManagedSettingsAllowRule(p); ok {
				return fmt.Errorf("grok imports Claude Code's managed-settings.json permission rules and %s contains a `permissions.allow` entry; the per-session isolated GROK_HOME cannot override an imported Claude allow rule — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the imported allow rule", hit)
			}
		}
	}
	return nil
}

// detectPinnedSystemGrokRequirementsFile is the per-path scanner that backs
// detectPinnedSystemGrokRequirements. Split out so the system layers can be
// iterated cleanly and so tests can target a single path. Missing/unreadable/
// empty path ⇒ nil (best-effort).
func detectPinnedSystemGrokRequirementsFile(path string, allowAPIKey, allowAlwaysApprove bool) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	const maxBytes = 1 << 20
	scanner := bufio.NewScanner(io.LimitReader(f, maxBytes))
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	// Track the active TOML section so a `[permission]` header followed by a
	// bare `rules = ["Bash(*)"]` line is classified as `permission.rules` —
	// the documented section-form of the allow-list pin. Without this the
	// switch below would see the unqualified key `rules` and skip the line,
	// letting a system-layer allow rule bypass the gate. Array-of-tables
	// (`[[name]]`) is intentionally ignored: the keys we care about are all
	// scalar tables, and treating `[[arr]]` as a section would mis-prefix
	// unrelated keys inside it.
	var currentSection string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Quote-aware inline-`#` strip so `always_approve = true # managed`
		// reduces to `true` while a `pattern = "Bash(#magic)"` literal stays
		// intact — a naive strings.IndexByte('#') would corrupt the latter and
		// silently let a pinned allow rule with a `#` in its pattern route
		// past the gate.
		line = grokTOMLStripInlineComment(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			currentSection = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		lower := strings.ToLower(line)
		eq := strings.IndexByte(lower, '=')
		if eq <= 0 {
			continue
		}
		bareKey := strings.TrimSpace(lower[:eq])
		key := bareKey
		if currentSection != "" && bareKey != "" && !strings.Contains(bareKey, ".") {
			key = currentSection + "." + bareKey
		}
		// Synthesise a section-qualified `key = ...` line so the keyword
		// scanners below see `permission.rules` instead of the unqualified
		// `rules` when the file uses section-form.
		qualifiedLower := key + lower[eq:]

		if !allowAPIKey && lineMentionsGrokAuthPin(qualifiedLower) {
			return fmt.Errorf("grok requirements pin API-key auth in %s; refusing to spawn — set Config.EnableGrokAPIKeyFallback=true to opt in, or remove the pinned credential", path)
		}
		if allowAlwaysApprove {
			continue
		}
		if lineMentionsGrokApprovalPin(qualifiedLower) {
			return fmt.Errorf("grok requirements pin a permissive approval policy in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned policy", path)
		}
		// `permission_rules` / `permission.rules` and the `policy.allow` /
		// `permissions.allow` / `tools.allow` cousins are documented xAI
		// allow-list keys that the boolean/mode-style scan above does NOT
		// catch. They have to be handled here because operators on managed
		// hosts commonly pin a `permission_rules = ["Bash(*)"]` or
		// `permission_rules = [{action = "allow", ...}]` allow rule in the
		// system layer — and that layer is NOT redirected by GROK_HOME, so
		// the isolation in setupIsolatedGrokHome cannot neutralise it.
		// Multi-line array form needs continuation accumulation before
		// classification or a `[\n {action = "allow", ...}\n]` would be read
		// as the first-line value `[` and miss the allow entry entirely.
		rawVal := strings.TrimSpace(line[eq+1:])
		switch key {
		case "permission_rules", "permission.rules":
			if grokTOMLBracketDepth(rawVal) > 0 {
				rawVal = accumulateGrokTOMLArrayContinuation(scanner, rawVal)
			}
			if grokPermissionRulesValueHasAllowAction(rawVal) {
				return fmt.Errorf("grok requirements pin a permissive permission_rules allow entry in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned rule", path)
			}
		case "policy.allow", "permissions.allow", "tools.allow":
			cleaned := strings.TrimSpace(strings.Trim(rawVal, `"'`))
			if cleaned != "" && cleaned != "[]" && cleaned != "[ ]" {
				return fmt.Errorf("grok requirements pin a permissive %s entry in %s; refusing to spawn — set Config.EnableGrokAlwaysApprove=true to opt in, or remove the pinned rule", key, path)
			}
		}
	}
	return nil
}

// claudeManagedSettingsPaths returns the OS-specific Claude Code
// `managed-settings.json` locations. See claudeManagedSettingsPathsFn for the
// rationale on which paths are scanned and which are deliberately omitted.
func claudeManagedSettingsPaths() []string {
	paths := make([]string, 0, 2)
	switch runtime.GOOS {
	case "darwin":
		paths = append(paths, "/Library/Application Support/ClaudeCode/managed-settings.json")
	case "windows":
		programData := os.Getenv("ProgramData")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		paths = append(paths, filepath.Join(programData, "ClaudeCode", "managed-settings.json"))
	default:
		paths = append(paths, "/etc/claude-code/managed-settings.json")
	}
	return paths
}

// detectClaudeManagedSettingsAllowRule reports whether the Claude
// `managed-settings.json` at `path` contains a non-empty `permissions.allow`
// array. Returns the path on hit so the error message can point the operator
// at the exact file. Missing/unreadable/malformed/empty files yield false —
// best-effort by design, matching detectPinnedSystemGrokRequirementsFile's
// tolerance for missing inputs.
func detectClaudeManagedSettingsAllowRule(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return "", false
	}
	var parsed struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", false
	}
	for _, rule := range parsed.Permissions.Allow {
		if strings.TrimSpace(rule) != "" {
			return path, true
		}
	}
	return "", false
}

// grokTOMLStripInlineComment removes a trailing `# ...` comment from a TOML
// line, honoring `"..."` / `'...'` string contents so a `#` inside a quoted
// pattern (`pattern = "Bash(#magic)"`) is preserved. Without this the line-
// oriented requirements scanner would corrupt valid pinned values that
// embed `#` in a pattern literal — silently letting them route past the
// approval gate.
func grokTOMLStripInlineComment(line string) string {
	inDouble := false
	inSingle := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inDouble {
			if c == '\\' && i+1 < len(line) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '#':
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return line
}

// grokTOMLBracketDepth counts net `[` minus `]` characters outside TOML basic
// ("...") and literal ('...') strings, so a `pattern = "Bash[*]"` literal
// inside `permission_rules` doesn't unbalance the count.
func grokTOMLBracketDepth(s string) int {
	depth := 0
	inDouble := false
	inSingle := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		switch c {
		case '"':
			inDouble = true
		case '\'':
			inSingle = true
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

// accumulateGrokTOMLArrayContinuation reads continuation lines from scanner
// while the running bracket depth (starting at the depth of `initial`) is
// still positive, joining them into a single logical value. Used by
// detectPinnedSystemGrokRequirements so a `permission_rules = [\n {action =
// "allow", ...}\n]` hand-formatted across multiple lines is classified on
// the full array value rather than the first-line `[`. Bounded at 256
// continuation lines so a corrupted file with no closing `]` can't stall
// the launch.
func accumulateGrokTOMLArrayContinuation(scanner *bufio.Scanner, initial string) string {
	depth := grokTOMLBracketDepth(initial)
	if depth <= 0 {
		return initial
	}
	parts := []string{initial}
	const maxContinuationLines = 256
	for i := 0; i < maxContinuationLines && depth > 0 && scanner.Scan(); i++ {
		ln := strings.TrimSpace(grokTOMLStripInlineComment(scanner.Text()))
		if ln == "" {
			continue
		}
		parts = append(parts, ln)
		depth += grokTOMLBracketDepth(ln)
	}
	return strings.Join(parts, " ")
}

// lineMentionsGrokAuthPin reports whether a normalised TOML line names an
// API-key credential — `api_key`/`env_key` as a key on any `model.*` / `xai.*`
// scope, with a non-empty quoted or env-style value. Kept as a flat keyword
// match (rather than a full TOML parser) because the system requirements file
// is operator-controlled and the false-positive risk on a key named "api_key"
// in another section is acceptable: failing closed is the safe direction.
func lineMentionsGrokAuthPin(lower string) bool {
	if !strings.Contains(lower, "api_key") && !strings.Contains(lower, "env_key") {
		return false
	}
	eq := strings.IndexByte(lower, '=')
	if eq < 0 {
		return false
	}
	val := strings.TrimSpace(lower[eq+1:])
	// Empty value (`api_key = ""`) is a deliberate clear — that's what the
	// old per-process `--config api_key=` neutralizer emitted, and we should
	// not refuse on it.
	if val == "" || val == `""` || val == `''` {
		return false
	}
	return true
}

// lineMentionsGrokApprovalPin reports whether a normalised TOML line pins one
// of the approval bypasses (`always_approve = true`, `auto_approve = true`,
// `approval.mode = "always"|"auto"|"always-approve"|"auto-approve"`,
// `yolo = true`, `permission_mode` matching isGrokPermissionModeBypassValue —
// the full `bypass*` / `accept-edits` / `always*` / `auto*` set — or a
// non-empty allow-list such as `policy.allow = [...]`). Bypass-value gating
// is delegated to isGrokPermissionModeBypassValue (and mirrors the
// approval-mode value set isGrokApprovalConfigKV gates argv on) so a system-
// layer pin like `permission_mode = "acceptEdits"` or `approval.mode =
// "always-approve"` trips the requirements gate identically to the argv
// `--config permission_mode=…` / `--config approval.mode=…` surface — the
// two surfaces must stay in lockstep, otherwise a managed host can route
// past the per-tool prompt despite EnableGrokAlwaysApprove=false.
func lineMentionsGrokApprovalPin(lower string) bool {
	eq := strings.IndexByte(lower, '=')
	if eq < 0 {
		return false
	}
	key := strings.TrimSpace(lower[:eq])
	val := trimGrokTOMLStringQuotes(strings.TrimSpace(lower[eq+1:]))
	switch {
	case (strings.Contains(key, "always_approve") || strings.Contains(key, "auto_approve") || key == "yolo") && val == "true":
		return true
	case strings.HasSuffix(key, "approval.mode") || key == "approval_mode" || key == "approval" || key == "mode":
		// Mirror isGrokApprovalConfigKV's approval-mode bypass-value set
		// (`always|auto` plus the documented dashed long-forms). Without the
		// long-form variants a `/etc/grok/requirements.toml` pinning
		// `approval.mode = "always-approve"` would slip past the gate while
		// the same argv `--config approval.mode=always-approve` is stripped.
		return val == "always" || val == "auto" || val == "always-approve" || val == "auto-approve"
	case strings.Contains(key, "permission_mode") || strings.Contains(key, "permission-mode"):
		return isGrokPermissionModeBypassValue(val)
	case strings.HasSuffix(key, "policy.allow") ||
		strings.HasSuffix(key, ".allow") ||
		key == "allow" || key == "allow_rules" || key == "allowlist":
		// Any non-empty allow rule auto-approves matching tools, which is
		// the same bypass surface as `always_approve = true`. Empty list /
		// empty string ⇒ deliberate clear, treat as benign.
		// `permission_rules` / `permission.rules` are NOT classified here:
		// xAI documents `action = "deny"` rules as policy-tightening (deny
		// takes precedence), so a deny-only pin from an MDM policy must not
		// trip this broad refusal. The structured switch in
		// detectPinnedSystemGrokRequirements routes `permission_rules`
		// values through grokPermissionRulesValueHasAllowAction, which only
		// fires on actual allow entries.
		return val != "" && val != "[]"
	}
	return false
}

// sanitizeGrokACPExtraArgs filters caller-supplied extra args down to tokens
// that are safe and valid to splice onto a `grok agent … stdio` argv, and
// extracts a caller `--model <x>` selector.
//
// It returns (model, cleaned):
//   - model is the caller's `--model` / `--model=` value when present and
//     non-empty, else defaultModel. The `--model` flag+value are consumed
//     here (not re-emitted) because buildGrokACPArgs positions `--model`
//     itself.
//   - cleaned is the remaining extras with dangerous/incompatible tokens
//     dropped: the grok-0.2.59-rejected `--config*` / `--permission-mode*` /
//     `--no-auto-update` / `--auto-update`, the credential flags `--api-key*`
//     / `--auth*` (stripped UNCONDITIONALLY — `grok agent` does not accept
//     them and would reject the argv with "unexpected argument"; the API-key
//     fallback opt-in flows through XAI_API_KEY env and the persisted
//     `[model] api_key` config.toml line instead), the `--cwd*` containment
//     side-door, `--always-approve` / `--auto-approve` (owned by buildGrokACPArgs), the
//     duplicate entry tokens (`agent`/`stdio`/`chat`/`tui`/`run`), and the
//     POSIX `--` end-of-options delimiter. `--allow <pattern>` / `--allow=…`
//     are xAI's documented pre-prompt allow rules (matching tools auto-approve
//     BEFORE the per-tool prompt runs) — stripped when allowAlwaysApprove is
//     false, mirroring the raw `session_start` path's stripGrokAllowRulePairs
//     sweep so a signed grok_acp_start cannot route around the per-tool prompt
//     by handing `--allow Bash(*)` through extras. `--deny` is policy-tightening
//     and is preserved on both sides of the gate.
func sanitizeGrokACPExtraArgs(extraArgs []string, defaultModel string, allowAlwaysApprove bool) (string, []string) {
	model := defaultModel
	cleaned := make([]string, 0, len(extraArgs))
	skipNext := false
	for i := 0; i < len(extraArgs); i++ {
		a := extraArgs[i]
		if skipNext {
			skipNext = false
			continue
		}
		lower := strings.ToLower(a)

		// Caller model selector — consume and record; buildGrokACPArgs emits
		// the `--model` flag itself.
		if lower == "--model" || lower == "-m" {
			if i+1 < len(extraArgs) {
				if v := strings.TrimSpace(extraArgs[i+1]); v != "" {
					model = v
				}
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--model=") {
			if v := strings.TrimSpace(a[len("--model="):]); v != "" {
				model = v
			}
			continue
		}
		if strings.HasPrefix(lower, "-m=") {
			if v := strings.TrimSpace(a[len("-m="):]); v != "" {
				model = v
			}
			continue
		}

		// Duplicate entry / subcommand tokens that would re-enter the TUI
		// path or duplicate the argv we build.
		switch lower {
		case "agent", "stdio", "chat", "tui", "run":
			continue
		}

		// Flags grok 0.2.59's `grok agent` rejects outright ("unexpected
		// argument"). The previous `--config`-based neutralizers are dead;
		// these must never reach the argv. `--config` / `-c` and
		// `--permission-mode` historically took a separate value, so skip the
		// following token too when not in equals form.
		if lower == "--config" || lower == "-c" ||
			lower == "--permission-mode" || lower == "--permission_mode" {
			if !strings.Contains(a, "=") && i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--config=") || strings.HasPrefix(lower, "-c=") ||
			strings.HasPrefix(lower, "--permission-mode=") || strings.HasPrefix(lower, "--permission_mode=") {
			continue
		}
		if lower == "--no-auto-update" || lower == "--auto-update" {
			continue
		}

		// Credential side-doors: `grok agent` does not accept `--api-key{,-env}`
		// / `--auth{,-method}` (constraint #1 in buildGrokACPArgs) — passing
		// them makes the child exit with "unexpected argument" instead of
		// starting the JSON-RPC handshake. Strip unconditionally, even when
		// EnableGrokAPIKeyFallback is true: the opt-in fallback flows through
		// XAI_API_KEY env (sanitizeGrokACPEnv) and the persisted
		// `[model] api_key` config.toml line (setupIsolatedGrokHome), not argv.
		if isGrokAuthOverrideArg(lower) {
			if !strings.Contains(a, "=") && i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}

		// `--cwd` would override the proc.Dir start validated against the
		// workspace root — drop both forms.
		if lower == "--cwd" {
			if i+1 < len(extraArgs) {
				skipNext = true
			}
			continue
		}
		if strings.HasPrefix(lower, "--cwd=") {
			continue
		}

		// `--always-approve` is owned by buildGrokACPArgs (gated on the
		// per-workspace opt-in) — never let a caller inject it directly.
		// `--auto-approve` is the documented alias on some grok builds and
		// behaves identically as an approval bypass, so it has to be
		// stripped on the same gate — otherwise extras like
		// `["--auto-approve"]` would slip past the always-approve sanitiser
		// (or hard-fail startup on versions that reject the alias).
		if lower == "--always-approve" || strings.HasPrefix(lower, "--always-approve=") ||
			lower == "--auto-approve" || strings.HasPrefix(lower, "--auto-approve=") {
			continue
		}

		// `--allow <pattern>` / `--allow=<pattern>` is xAI's documented pre-
		// prompt allow rule — matching tool calls auto-approve before the
		// per-tool prompt runs, the same bypass surface as `--always-approve`.
		// The raw `session_start` path strips it on the same gate (via
		// stripGrokAllowRulePairs); mirror that here so a signed
		// grok_acp_start passing `--allow Bash(*)` through extras cannot
		// route around the per-tool prompt when EnableGrokAlwaysApprove is
		// false. `--deny` is policy-tightening (deny takes precedence in
		// xAI's docs) and is preserved on both sides of the gate.
		if !allowAlwaysApprove {
			if lower == "--allow" {
				if i+1 < len(extraArgs) {
					skipNext = true
				}
				continue
			}
			if strings.HasPrefix(lower, "--allow=") {
				continue
			}
		}

		// POSIX end-of-options delimiter: `stdio` is appended after these
		// extras, so a surviving `--` would demote it to an operand. Drop it.
		if a == "--" {
			continue
		}

		cleaned = append(cleaned, a)
	}
	return model, cleaned
}

// redactGrokACPArgsForLog masks credential-bearing values before the startup
// banner is printed. sanitizeGrokACPExtraArgs strips `--api-key{,-env}` /
// `--auth{,-method}` from the argv unconditionally (grok agent rejects them),
// so in normal flow there is nothing to mask here. Kept as defence-in-depth in
// case a future code path appends a credential-bearing token to args without
// routing through the sanitiser; equals-form values are masked inline and
// separate-value form masks the following token. Output is passed through
// redactArgs so other secret patterns (bearer tokens, AWS keys, etc.) the
// per-arg regex recognises in caller-supplied extra args are also caught.
func redactGrokACPArgsForLog(args []string) []string {
	out := make([]string, len(args))
	maskNext := false
	for i, a := range args {
		if maskNext {
			out[i] = "[REDACTED]"
			maskNext = false
			continue
		}
		lower := strings.ToLower(a)
		if isGrokAuthOverrideArg(lower) {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				out[i] = a[:eq+1] + "[REDACTED]"
			} else {
				out[i] = a
				maskNext = true
			}
			continue
		}
		out[i] = a
	}
	return redactArgs(out)
}

// isGrokAuthOverrideArg reports whether a caller-supplied arg would let
// the orchestrator point Grok at an API key (or non-cached-token auth
// method) and bypass the default subscription-bound flow. Each known flag
// is enumerated explicitly — a broader `--api-key*` prefix match would
// silently strip flags we don't know about (`--api-key-foo` etc.) and risk
// breaking legitimate non-auth args future Grok releases might ship.
// Match is case-insensitive; callers normalise via strings.ToLower first.
func isGrokAuthOverrideArg(lower string) bool {
	authFlags := []string{
		"--api-key",
		"--api-key-env",
		"--auth",
		"--auth-method",
	}
	for _, f := range authFlags {
		if lower == f || strings.HasPrefix(lower, f+"=") {
			return true
		}
	}
	return false
}

// sanitizeGrokACPEnv applies a strip list to the inherited environment
// before forwarding it to the Grok ACP child. Behaviour:
//
//   - GROK_* / GROK_HOME / PATH / HOME / locale / proxy etc. are forwarded
//     by omission — we never list them in the strip set so the child's
//     shell environment stays intact and `grok login`'s cached token under
//     $GROK_HOME / ~/.grok remains discoverable.
//   - XAI_API_KEY is stripped UNLESS allowAPIKey is true. This is the
//     finding-#3 defence: without a config-level opt-in
//     (Config.EnableGrokAPIKeyFallback), a user who has `export
//     XAI_API_KEY=...` in their shell would otherwise silently fall over
//     to API-key billing if cached-token auth ever fails, despite the
//     feature brief mandating API-key auth be opt-in only.
//   - CLAUDECODE / CLAUDE_* / CODEX_IDE_* are unconditionally stripped
//     because they would tell downstream tooling it is running embedded
//     inside another IDE / agent, which is not true here.
//
// We do NOT pin a `GROK_*` allowlist — a strip-only list keeps the child's
// shell environment intact without us having to enumerate every harmless
// variable Grok might care about.
func sanitizeGrokACPEnv(env []string, allowAPIKey bool) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		upper := strings.ToUpper(e)
		if strings.HasPrefix(upper, "CLAUDECODE=") ||
			strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "CODEX_IDE_") {
			continue
		}
		if !allowAPIKey && strings.HasPrefix(upper, "XAI_API_KEY=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}
