# CLI Agent Integration — Claude Code

This document records the canonical contract for how `aiexpedite-local-terminal`
launches Claude Code as a child process, and why the launch shape matters
for billing. **Maintainers: read the "Do not reintroduce `claude -p`" section
before changing anything here.**

## TL;DR

- Claude Code is launched as an **interactive `stream-json`** subprocess.
- The prompt is delivered as an NDJSON `{type:"user", …}` envelope on stdin;
  it never appears on argv.
- Any user-supplied `-p` / `--print` (and the equals-form variants `-p=…` /
  `--print=…`) is **stripped** in `buildClaudeInteractiveArgs`
  ([session.go](session.go) — `buildClaudeInteractiveArgs`).
- The child process environment is filtered by `sanitizeClaudeChildEnv`
  ([session.go](session.go)) so spawned Claude Code falls back to the stored
  `/login` subscription credential.
- The integration is **intentionally not based on `claude -p` or the Claude
  Agent SDK**.

## Launch model

`SessionManager.StartSession` in [session.go](session.go) is the single
entry point. For Claude it:

1. Resolves `claude` (or the platform shim) via `resolveExecutable`.
2. Builds argv via `buildClaudeInteractiveArgs`, which always sets:
   ```
   --output-format stream-json
   --input-format  stream-json
   --verbose
   --include-partial-messages
   --dangerously-skip-permissions
   ```
   and strips any user-supplied `-p` / `--print` (incl. equals-form).
3. Sanitises the child env via `sanitizeClaudeChildEnv` (see "Environment
   policy" below) and logs the stripped variable names.
4. Spawns the process with stdin/stdout/stderr pipes.
5. Writes the prompt as a single NDJSON line
   (`{"type":"user","message":{"role":"user","content":"…"},…}`) on stdin.
6. Keeps stdin **open** so the orchestrator can send follow-up turns via
   `SendInput`. `shouldCloseStdinAfterStart` in [session.go](session.go)
   returns `false` for `claude` regardless of whether the initial prompt was
   non-empty.

Stdin closes only when Claude emits a terminal `{"type":"result"}` event
(detected in `detectResultEvent` / `detectCLITerminalEvent`).

## Why not `claude -p` / Agent SDK

`claude -p` (print mode) and the Claude Agent SDK both put Claude in a
one-shot mode that exits after the first response. That breaks the
kickoff → `SendInput` handoff this driver depends on for multi-turn
sessions.

Starting **2026-06-15**, the choice also becomes a billing concern:

- On Pro / Max / Team subscriptions, **`claude -p` and Agent SDK usage will
  draw from a separate Agent SDK credit pool**, not the user's normal
  subscription allowance.
- Plain interactive Claude Code (the launch shape this driver uses)
  continues to draw from the normal subscription allowance.

Reintroducing `claude -p` here would silently move every session this
driver runs onto the Agent SDK credit pool — a real money leak with no UI
signal. The print-flag strip and the supporting tests in
[session_cli_test.go](session_cli_test.go) exist to make this regression
impossible to ship.

**Do not reintroduce `claude -p` for Claude Code agent sessions.** If you
need a one-shot Claude call, use a non-session execute path that builds its
own argv outside `buildClaudeInteractiveArgs`.

## Environment policy

`sanitizeClaudeChildEnv` ([session.go](session.go)) splits the strip set
into two named lists:

### `claudeAlwaysStripped` (every spawned session)

| Prefix         | Why                                                       |
| -------------- | --------------------------------------------------------- |
| `CLAUDECODE=`  | Tells nested claude it is already inside a Claude session |
| `CLAUDE_`      | IDE-context vars (`CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_AGENT_SDK_VERSION`, …) and `CLAUDE_CODE_OAUTH_TOKEN` (see note) |

**`CLAUDE_CODE_OAUTH_TOKEN` is intentionally swept by the `CLAUDE_` prefix.**
The integration relies on the user's interactive `/login` credentials
stored in `~/.claude/.credentials.json`; there is no current code path that
needs a headless OAuth token injected via env. If a future maintainer
wants subscription-safe headless token support, add an explicit whitelist
in `sanitizeClaudeChildEnv` rather than discovering the strip by accident.

### `claudeBillingStripped` (only when `isClaudeCommand(command)` is true)

| Prefix                  | Why                                                                                              |
| ----------------------- | ------------------------------------------------------------------------------------------------ |
| `ANTHROPIC_API_KEY=`    | Anthropic SDK precedence puts this ahead of the stored `/login` token — silent API-wallet billing |
| `ANTHROPIC_AUTH_TOKEN=` | Same precedence rule — silent API-wallet billing                                                  |

**Policy: force subscription billing.** There is no opt-in API-key escape
hatch from inside the driver. A developer who keeps `ANTHROPIC_API_KEY`
in their shell for unrelated SDK work would otherwise silently bill their
company API wallet for every interactive session this driver launches,
with no visible signal in the UI. If a user genuinely wants API-key
billing for a one-off, they can run `claude` directly outside the driver.

Stripped variable names are logged per session as a yellow
`[session] Stripped env vars from session <id>: …` line so the policy is
auditable, not hidden. Non-claude commands (codex, gemini, arbitrary
shells) are unaffected by the billing strip and keep their existing auth.

### Pinned entrypoint (claude only)

After the strip, `prepareClaudeChildEnv` ([session.go](session.go)) sets
`CLAUDE_CODE_ENTRYPOINT=cli` on the child env for claude commands. The
`CLAUDE_` sweep above first removes any **inherited** entrypoint (which would
be `claude-vscode` / `sdk-ts` if this agent was itself launched from a host
IDE or the Agent SDK); we then pin the honest `cli` value so the spawned
session self-identifies as the interactive CLI session it actually is.

This is load-bearing for the **2026-06-15** split: Anthropic classifies the
separate Agent SDK credit pool in part by entrypoint, so pinning `cli` makes
the favourable *interactive* classification deterministic instead of relying
on claude's default-when-unset. It is the truthful tag for this launch shape
— it is **not** spoofing; we never set `claude-vscode` or `sdk-ts`. The pin is
claude-specific: codex / gemini / shells never receive it.

## Enforcement points

- [`session.go` — `buildClaudeInteractiveArgs`](session.go) — strips
  `-p` / `--print` / `-p=` / `--print=` and forces the `stream-json`
  flag set.
- [`session.go` — `sanitizeClaudeChildEnv`](session.go) — strips the env
  prefixes above.
- [`session.go` — `prepareClaudeChildEnv`](session.go) — wraps the sanitiser
  and pins `CLAUDE_CODE_ENTRYPOINT=cli` for claude commands.
- [`session.go` — `shouldCloseStdinAfterStart`](session.go) — returns
  `false` for `claude` so multi-turn `SendInput` works.
- [`session_cli_test.go`](session_cli_test.go) — pins the print-flag strip
  behaviour (incl. equals-form variants).
- [`session_env_test.go`](session_env_test.go) — pins the env strip
  behaviour (billing vars, always-vars, non-claude carve-out, log payload).

---

# CLI Agent Integration — xAI Grok Build CLI (ACP)

xAI Grok Build CLI is integrated via its **ACP (Agent Client Protocol)
JSON-RPC** interface (`grok agent stdio`), not via the interactive TUI. The
implementation mirrors the Codex app-server manager — same JSON-RPC stdio
contract, same fail-fast policy, same lifecycle — but with auth posture and
naming specific to Grok.

## TL;DR

- Manager: [`GrokACPManager`](grok_acp.go) — spawns `grok agent stdio` and
  forwards every JSONL frame back to the orchestrator as `grok_acp_message`
  (stdout) / `grok_acp_stderr` (stderr) / `grok_acp_error` (protocol
  violation) / `grok_acp_ended` (process exit).
- Pub/Sub command types: `grok_acp_start`, `grok_acp_send`, `grok_acp_end`.
- **Auth preference**: orchestrator selects `cached_token` from the
  `initialize` response's `authMethods` so usage ties to the terminal
  computer user's local `grok login`. `xai.api_key` / `XAI_API_KEY` is an
  opt-in fallback only.
- Approval posture: **conservative by default**. The driver does NOT auto-
  enable always-approve / autonomous tool execution; the orchestrator must
  explicitly request it per workspace.

## How to install / login

```bash
# Install Grok Build CLI (xAI) — official installer
# Drops the `grok` binary at ~/.grok/bin/grok and prints the PATH export
# you'll need so the local terminal's `exec.LookPath("grok")` finds it.
curl -fsSL https://x.ai/cli/install.sh | bash

# Alternative (kept for environments without curl access):
#   npm install -g @xai/grok

# Subscription-bound auth — preferred by AI Expedite
grok login

# Optional API-key fallback (only if no cached_token is available)
export XAI_API_KEY="xai-..."
```

> The official `install.sh` drops the binary under `~/.grok/bin`. macOS
> GUI/launchd-spawned agents inherit a sparse PATH that often excludes that
> dir, so both `gatherCLIAgents` and `GrokACPManager.Start` automatically
> fall back to `$GROK_BIN_DIR` (the override `install.sh` itself reads) or
> `$HOME/.grok/bin` when `exec.LookPath("grok")` misses — no shell rc edit
> required for detection or session startup.

The agent detects `grok` via `gatherCLIAgents` in [systemInfo.go](systemInfo.go)
and reports installed-status + version on the auth/token uplink. The CLI
Agents tab reads from `cliAgents[]` (see
[cliagent_usage_grok.go](cliagent_usage_grok.go)); when the user hasn't run
`grok login`, the entry still appears with empty account + dashed capacity
gauges so the user sees they need to authenticate.

## Why ACP, not TUI scraping

`grok` (no subcommand) launches an interactive TUI built around terminal
escape sequences. Scraping that output is fragile — Grok ships TUI redesigns
frequently and there are no stable parse anchors. ACP is the supported
machine-to-machine entry point and offers:

- Stable JSON-RPC 2.0 framing (newline-delimited stdio).
- First-class `authenticate` method that surfaces `cached_token` so we don't
  need to grovel through dotfiles ourselves.
- Streaming `session/update` notifications for assistant deltas, tool calls,
  and approval requests.
- Clean cancellation via `session/cancel` + stdin-close.

`buildGrokACPArgs` in [grok_acp.go](grok_acp.go) strips any caller-supplied
`agent` / `stdio` / `chat` / `tui` / `run` tokens that would re-enter the TUI
path, so orchestrator typos can't accidentally fall back to TUI scraping. It
also always injects `--no-auto-update` (and strips any caller-supplied
`--auto-update`) — the xAI headless/scripting docs recommend this for
automated ACP children so a background update worker can't race the
JSON-RPC handshake and pollute stdout with non-protocol bytes (which would
surface as `grok_acp_error` and fail the in-flight `initialize` call).

## Environment policy

[`sanitizeGrokACPEnv`](grok_acp.go) strips the same nested-IDE markers as
the Codex sanitiser (`CLAUDECODE=`, `CLAUDE_*`, `CODEX_IDE_*`) so downstream
tooling doesn't think it's running embedded inside another IDE. **`XAI_API_KEY`
is stripped by default** — the user must explicitly enable API-key fallback
for this agent via `enable_grok_api_key_fallback` in
[`config.go`](config.go) before the env var (and any caller-supplied
`--api-key*` / `--auth*` args) flow through to the child. Without that
opt-in:

- A developer with `export XAI_API_KEY=...` in their shell rc can NOT
  accidentally bill their xAI API wallet for Grok sessions launched by this
  agent.
- A misbehaving orchestrator can NOT override the cached-token preference
  by passing `--api-key`/`--auth` via `cmd.Args` — both
  [`buildGrokACPArgs`](grok_acp.go) and [`sanitizeGrokACPEnv`](grok_acp.go)
  enforce the gate.

`GROK_*` config dir vars (notably `GROK_HOME`) are always preserved so the
local `grok login` cached token remains discoverable.

## Workspace containment

[`GrokACPManager.Start`](grok_acp.go) accepts a `GrokStartOptions.WorkspaceRoot`
the dispatcher sources from `Config.WorkingDirectory`. When non-empty,
Start resolves symlinks on BOTH the requested cwd and the root, then
rejects any cwd that escapes the root — defends against signed
`grok_acp_start` payloads targeting arbitrary directories the OS user
happens to read/write, including symlink-escape paths under a workspace
that point to a sibling.

## Per-session timeout

`cmd.TimeoutMs` from the inbound `grok_acp_start` is threaded into
`GrokStartOptions.TimeoutMs`. When non-zero, Start arms a `time.AfterFunc`
that — on fire — publishes a typed `grok_acp_error` (`"timed out after Xms"`)
and kills the child; [`waitForExit`](grok_acp.go) then publishes the
terminal `grok_acp_ended`. Requested values above `grokACPMaxLifetime`
(6 h, same as the stale GC) are clamped so a misbehaving orchestrator
can't request a longer session than our GC tolerates.

## Lifecycle

`Start` spawns the child, registers it with the global process registry,
launches stdout + stderr reader goroutines, and synchronously acks
`grok_acp_started`. `End` closes stdin (ACP's documented graceful-exit
path), then escalates to SIGINT → SIGKILL on the 5s timeout cascade.
`CleanupStale` ends sessions older than 6 h so an orchestrator crash that
drops `grok_acp_end` can't leak grok children indefinitely.

Frames are **never silently dropped**: oversize frames, escape-amplified
envelopes, stalled publish queues, or scanner errors all surface a fatal
`grok_acp_error` and force-kill the child, then publish the terminal
`grok_acp_ended`. The orchestrator's JSON-RPC state machine relies on
seeing every frame in `Seq` order; a silent drop would deadlock.

## Enforcement points

- [`grok_acp.go` — `buildGrokACPArgs`](grok_acp.go) — forces the `agent
  stdio` entry-point argv, strips TUI / chat / run tokens, and (when
  `allowAPIKey=false`) strips caller-supplied `--api-key*` / `--auth*`
  args.
- [`grok_acp.go` — `sanitizeGrokACPEnv`](grok_acp.go) — strips nested-IDE
  env markers; strips `XAI_API_KEY` unless `Config.EnableGrokAPIKeyFallback`
  is set; preserves `GROK_*` so the local cached-token path stays
  discoverable.
- [`grok_acp.go` — `pathInsideRoot`](grok_acp.go) — workspace containment
  helper (symlink-resolved, `filepath.Rel`-based, no `HasPrefix` shortcut).
- [`grok_acp.go` — `Start` deadline timer](grok_acp.go) — per-session
  TimeoutMs handling, clamped at `grokACPMaxLifetime`.
- [`pubsub.go` — `isGrokACPCommand` / `handleGrokACPCommand`](pubsub.go) —
  dispatches `grok_acp_*` commands; sources `GrokStartOptions` from
  `Config` + `cmd.TimeoutMs`; allowlist-gates `grok_acp_start` against the
  synthesised `grok agent stdio …` argv.
- [`grok_acp_test.go`](grok_acp_test.go) — pins the argv builder, env
  sanitizer, Send validation, full ACP handshake (initialize →
  authenticate → session/new → session/prompt → session/update →
  session/cancel → end), bad-frame surfacing, and missing-binary error.
- [`cliagent_usage_grok.go`](cliagent_usage_grok.go) +
  [`cliagent_usage_test.go`](cliagent_usage_test.go) — pins the
  cached-token discovery (auth.json + cached_token.json layouts,
  `$GROK_HOME` override) and the missing-login baseline entry.
