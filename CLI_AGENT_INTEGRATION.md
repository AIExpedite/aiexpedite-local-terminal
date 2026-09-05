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
2. Builds argv via `buildClaudeInteractiveArgs` ([claude_argv.go](claude_argv.go)), which always sets:
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
[claude_argv_test.go](claude_argv_test.go) exist to make this regression
impossible to ship.

**Do not reintroduce `claude -p` for Claude Code agent sessions.** If you
need a one-shot Claude call, use `buildClaudeNonInteractivePrintArgs` — the
sanctioned, tested shape described in the next section — never this builder.

## The sanctioned non-interactive shape (CLI-maintenance smoke)

The paragraph above says "use a non-session execute path that builds its own
argv" — that path now **exists, is tested, and is the only sanctioned one**:
`buildClaudeNonInteractivePrintArgs` in [claude_argv.go](claude_argv.go),
driven by `runClaudeCodeSmoke` in
[cliagent_smoke_claudecode.go](cliagent_smoke_claudecode.go).

Why it had to exist: while `buildClaudeInteractiveArgs` was the only Claude
argv builder, a print-mode probe pushed through it **lost its `--print`**,
inherited `--input-format stream-json`, and then had stdin closed with no
NDJSON envelope written. Claude 2.1.x rejects that combination before any
assistant turn —

```
Error: --input-format=stream-json requires output-format=stream-json.
```

— exits 1, and no marker can ever come back. That is the `errorCategory:
protocol` failure the CLI-maintenance smoke reported on **both** sides of an
upgrade.

**`-p` / `--print` is stripped ONLY on the session path.** The rule is about
sessions and billing, not about the flag itself: an interactive multi-turn
session must never be one-shot, and (see above) `claude -p` bills against the
Agent SDK credit pool. A single, bounded probe that spends one turn to prove
the binary still works is a deliberate, accounted-for exception — not a
loophole to route agent work through.

Resolved 2.1.x flag set (verified against **2.1.247**):

```
--print
--output-format json          # ONE terminal {"type":"result",…} envelope
--tools ""                    # claude's documented "disable all tools"
--max-turns 1
--strict-mcp-config           # preferred shape only …
--mcp-config {"mcpServers":{}}   # … so user MCP config cannot inject a server
```

- `--input-format` is left at its default (`text`) — pairing `stream-json`
  input with anything but `stream-json` output is the framing violation above.
- `--verbose` / `--include-partial-messages` apply only to `stream-json`
  output and are omitted.
- `--dangerously-skip-permissions` is **not** passed: it is refused outright in
  some sandbox/root contexts (a protocol-shaped exit 1 of its own) and is
  meaningless when no tool can run.
- The prompt (and the `AIEXPEDITE_CLI_SMOKE_OK_<8 hex>` marker nonce it
  carries) goes on **stdin as plain text**, closed immediately — never on argv,
  which would put it in a process listing and under the Windows
  `CreateProcess` ceiling.
- Two shapes are tried in order and the winner is cached per
  `(binary path, mtime, size)` — the same key `cachedProbeVersion` uses. The
  second rung is attempted **only** when the CLI positively rejected a flag the
  fallback drops (`unknown option '--strict-mcp-config'`), because option
  parsing precedes inference and is therefore the one failure a retry cannot
  double-charge for. Any other failure — malformed output, an undocumented
  error envelope, a timeout — stops after the first attempt, since a turn may
  already have been spent. The retryable-flag list is derived from the shapes
  themselves (`claudeRetryableArgvFlags`), never hand-listed.
- The per-attempt deadline is read from the **attempt's own context**, not the
  parent's. `exec.CommandContext` kills the child and then reports an
  `*exec.ExitError` ("signal: killed"), NOT an error wrapping
  `context.DeadlineExceeded`, so judging by the parent context read a timeout
  as `protocol` and sent the ladder off to burn a second full timeout.

Exit classification, mapped onto the closed `errorCategory` enum in
[cli_usage_refresh_receipt.go](cli_usage_refresh_receipt.go):

| Observation | `errorCategory` | `diagnostic` |
| --- | --- | --- |
| Binary absent, `--version` non-zero | `provider_unavailable` | `binary_missing` |
| `auth status --json` reports logged out | `not_authenticated` | `not_logged_in` |
| Result envelope carries an auth error | `not_authenticated` | `auth_error` |
| Provider refused the turn (API error status, overloaded, usage limit) | `provider_unavailable` | `provider_error` |
| The attempt's deadline killed the child | `provider_timeout` | `timeout` |
| CLI rejected one of our optional flags | `protocol` | `flag_rejected` |
| CLI rejected the input/output framing | `protocol` | `framing_rejected` |
| Exit non-zero with no `result` envelope, or unparseable JSON | `protocol` | `no_envelope` |
| Valid `result` envelope whose text is not exactly the marker | `parse_failed` | `marker_mismatch` |

The last two rows are the important distinction: a chatty model ("Sure! …") is
a **healthy** CLI answering unexpectedly (`parse_failed`), while a broken
invocation contract is ours to fix (`protocol`). Collapsing them would have
hidden this very regression.

Cost and privacy posture:

- One real inference turn per smoke, against the user's own subscription
  window. `--version` and `auth status --json` pre-checks short-circuit
  **before** a turn is spent; the ladder retries only on a pre-inference flag
  rejection; and a 15-minute per-CLI cooldown replays the last verdict —
  invalidated by a binary change, so the post-upgrade smoke is never answered
  from the pre-upgrade cache.
- The published `__cli_smoke_result__` carries
  `{cliId, version, status, errorCategory, markerMatched, durationMs,
  argvShapeId, diagnostic}` and nothing else. **No text the CLI authored is
  published — or retained anywhere.** `diagnostic` is a value from a closed set
  we define (`cliSmokeDiagnostic*`); the child's stdout and stderr are read only
  to choose between those constants and are then discarded.
- **The device's own log is not an exception.** Two earlier revisions got this
  wrong in the same way: the first published a 400-byte redacted stderr tail,
  the second merely moved that tail into the agent log. Neither is sound. The
  agent log rotates to disk and can be uploaded on request, and a denylist
  redactor only removes the secret shapes it anticipates — it does not catch
  `{"password":"…"}`, a raw settings fragment, a private path with no
  credential marker, or an unfamiliar key format. "Local" is not a safety
  property; not retaining the bytes is. The failure log line is rendered by
  `claudeSmokeFailureLogLine`, which takes the stderr **length** rather than the
  bytes — a function that cannot receive vendor text cannot leak it however a
  future caller wires it up — and emits only `shape=`, `category=`,
  `diagnostic=` and `stderrBytes=`.
- The marker, the prompt, the resolved argv, `~/.claude.json`, `settings.json`
  and any credential material are never published or logged at any severity.

## Usage capture across a CLI upgrade

A Claude upgrade rewrites `~/.claude/settings.json` and can drop our
`statusLine` command with it. That is handled independently of the smoke:
`ensureClaudeStatusLineHookIfStale` ([statusline_install.go](statusline_install.go))
re-verifies the hook at every run start (throttled, opt-out aware), and the
bounded utilization probe ([cliagent_usage_claudecode_probe.go](cliagent_usage_claudecode_probe.go))
refreshes a reading that has aged past the staleness TTL. The smoke only
contributes `claudeBinaryStamp` (path, mtime, size, version), which keys its own
cooldown so a verdict cached before an upgrade is never served for the
post-upgrade binary.

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

- [`claude_argv.go` — `buildClaudeInteractiveArgs`](claude_argv.go) — strips
  `-p` / `--print` / `-p=` / `--print=` and forces the `stream-json`
  flag set. (Moved out of `session.go`, unchanged, so the two Claude argv
  shapes share one print-flag classifier and cannot drift.)
- [`claude_argv.go` — `buildClaudeNonInteractivePrintArgs`](claude_argv.go) —
  the ONLY sanctioned one-shot shape: keeps `--print`, never requests
  `stream-json` framing, runs with no tools.
- [`session.go` — `sanitizeClaudeChildEnv`](session.go) — strips the env
  prefixes above.
- [`session.go` — `prepareClaudeChildEnv`](session.go) — wraps the sanitiser
  and pins `CLAUDE_CODE_ENTRYPOINT=cli` for claude commands.
- [`session.go` — `shouldCloseStdinAfterStart`](session.go) — returns
  `false` for `claude` so multi-turn `SendInput` works. The print path
  deliberately does not route through it.
- [`claude_argv_test.go`](claude_argv_test.go) — pins BOTH shapes: the
  print-flag strip (incl. equals-form variants) on the session path, and
  `--print` presence / no-stream-json / no-tools on the probe path.
- [`cliagent_smoke_claudecode_test.go`](cliagent_smoke_claudecode_test.go) —
  pins the exit classification, the pre-checks that spend no quota, the
  one-turn retry bound, the cooldown, and the fact that neither the published
  result nor the device log carries any text the CLI authored.
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

## Model and effort discovery (Ship B5)

Every provider snapshot in `cliAgents[]` also carries what the CLI itself says
about its models — `modelDetails[{id, label, efforts, defaultEffort, noEffort}]`
and `modelsExhaustive` — probed once per binary and cached for 30 minutes
([cliagent_models.go](cliagent_models.go)): Codex from `~/.codex/models_cache.json`
plus the top-level `model` in `config.toml` (a cache written by a different
Codex build, or one missing the configured model, is reported non-exhaustive —
two Codex clients share that file); Antigravity from `agy models`, folding the
effort-suffixed slugs (`gemini-3.8-flash-high`) into one family with a scale
and marking unsuffixed slugs `noEffort` (`agy` refuses `--effort` for them);
Grok from `grok models` (default marked, any levels named on the line); Claude
Code as its alias set with the scale from `claude --help`, non-exhaustive;
OpenCode re-shaped from its readiness probe. A user-initiated usage refresh
resets the cache. The signed refresh receipt canonicalises both fields
(`testdata/cli_usage_refresh_receipt_vectors.json`, vector 4, mirrored in
terminal-service), so **terminal-service must deploy first**.
`AIX_B5_HARNESS_OUT=<file> go test -run TestB5ModelDiscoveryHarness -v` runs the
real probes on this machine and writes the snapshot.

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

## Approval policy

Per-tool permission prompts are **on by default**. xAI documents
`--always-approve` as the flag that skips them, and the design doc treats
`--auto-approve` as an equivalent name. Both flags — and the equivalent
config knobs `approval.mode=always|auto` and
`tools.always_approve=true` / `tools.auto_approve=true` — are stripped from
`cmd.Args` unless the workspace has explicitly opted into autonomous
execution via `enable_grok_always_approve` in [`config.go`](config.go).
Without that opt-in, a signed `grok_acp_start` cannot flip Grok onto
autonomous tool execution even if the orchestrator tries to forward the
flag (directly or through `-c|--config`). When approval needs to happen,
the orchestrator should route a JSON-RPC `permission/request` back through
the existing per-workspace approval gate and answer with
`permission/response`; the desktop never auto-allows.

## Workspace containment

Containment is **session-rooted**: [`GrokACPManager.Start`](grok_acp.go)
resolves symlinks on the requested cwd and stores the resolved path as the
session's own workspace root. Later `session/new` / `session/load` frames —
whose `params.cwd` originates in the orchestrator's LLM tool loop and is
model-suppliable — must resolve inside that root (`validateGrokACPSendCwd`),
so a session cannot be re-pointed outside the directory the start named,
including via symlink-escape paths under the workspace.

Start itself accepts any absolute, existing directory. It deliberately does
NOT compare the cwd against `Config.WorkingDirectory`: the server derives the
start cwd from its repo mappings (terminal-service `contributedPaths.util.js`),
and the device home is not a superset of those — the earlier device-home jail
refused directories the server itself had chosen (every repo checked out
outside the device home), while defending against nothing the same signed
command channel didn't already allow via claude/codex/exec. The antigravity
manager applies the same session-rooted model, re-resolving the cwd each turn
against the root captured at start (TOCTOU symlink-swap protection).

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
  args; (when `allowAlwaysApprove=false`) strips caller-supplied
  `--always-approve` / `--auto-approve` and the equivalent
  `-c approval.mode=always|auto` / `-c tools.always_approve=true` /
  `-c tools.auto_approve=true` config overrides.
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

# CLI Agent Integration — Google Antigravity (`agy`) native chat

Antigravity native chat is documented in full in
[`docs/antigravity-native-chat.md`](docs/antigravity-native-chat.md).

Summary:

- **Process model:** one-shot `agy` stream-json process per native turn; prompt
  is sent as an NDJSON user event on stdin and stdin is then closed.
- **Resume:** exact `--conversation <uuid>` only — never `--continue`.
- **Native ID capture:** after first turn, from
  `~/.gemini/antigravity-cli/cache/last_conversations.json` (cwd key) with a
  conversations-dir snapshot fallback.
- **Manager:** [`AntigravityNativeManager`](antigravity_native.go).
- **Pub/Sub:** `antigravity_native_{start,send,end}` →
  `antigravity_native_{started,message,stderr,error,ended}`.
- **Minimum version:** agy ≥ 1.1.15 (stream-json stdin support).
- **Permissions:** `--dangerously-skip-permissions` on native-chat and all
  session paths (remote users cannot answer local prompts). Native chat and the
  normal pipe-based session path use stream-json stdin; only the Unix PTY
  compatibility path retains `--print <prompt>` argv delivery.

## Antigravity quota capture (why `agy` needs an active poller)

Claude and Codex leak their usage numbers passively — Claude prints a rate-limit
line on stdout that [`captureClaudeRateLimitLine`](cliagent_ratelimit.go) merges
into a fingerprint-scoped cache, Codex and Grok write files that outlive the run.
`agy` does neither. Its quota is served **only** by the language server each CLI
run starts on a random loopback port, and that server dies with the process.

The consequence: the `__cli_usage_refresh__` gather runs when nothing is
executing, so it never finds a port, replays the cached snapshot with its
*original* `observedAt`, and the CLI Agents card ages a day-old pool no matter
how many runs succeed. That is the `observed_stale` /
`latestObservedAt = 2026-08-28T05:13:22Z` symptom.

[`cliagent_usage_antigravity_capture.go`](cliagent_usage_antigravity_capture.go)
closes that gap by reading the server **while it exists**:

- **Armed at every spawn.** `startAntigravityQuotaCapture(label)` is called from
  [`runOneShot`](antigravity_native.go) (native chat),
  [`runPTYCommand`](pty_session_unix.go) (tty=true session + `execute`),
  [`StartSession`](session.go) (pipe session path, released in `waitForExit`) and
  [`runLocalCommand`](pubsub.go) (tty=false `execute` — direct spawn on Unix and
  a dedicated one-shot process on Windows). All but
  native chat gate on `commandRunsAntigravity`, which also sees through the
  `bash -c "agy …"` wrapper terminal-service ships. Windows has no PTY path, so
  capture there comes from native chat, the pipe path and `execute`.
  **Known gap:** a command that arrives already wrapped as
  `powershell -EncodedCommand <base64>` is opaque at this layer and is not
  classified; decoding caller-supplied base64 to sniff for a program name is a
  worse trade than losing freshness on that one route.
- **One shared, refcounted poller.** Concurrent `agy` runs join the same
  goroutine; it stops when the last one releases it.
- **Timing is everything.** The server dies *with* the child, so a post-exit
  probe cannot be relied on to find anything. The poller therefore probes
  **immediately at arm**, then ramps
  (`antigravityCaptureInitialPollInterval = 250ms` ×
  `antigravityCaptureInitialPolls = 12`, covering server bind time and short
  turns) down to `antigravityCapturePollInterval = 3s`. Capture is armed only
  after the child successfully starts, so the immediate probe cannot run before
  the process exists.
- **Bounded discovery, continuous live-port reads.**
  `antigravityCaptureMaxAttempts = 200` and
  `antigravityCaptureMaxDuration = 15m` bound attempts that may scan logs. Once
  either cap is reached, a memoized live port continues receiving cheap 3s
  loopback reads until the run ends; no further log scans occur. If no port was
  found, the poller parks until release.
- **Cheap.** The expensive part of a probe is log scanning (4 files ×
  2×128 KB), not the RPC, so the winning port is memoized for the life of the run
  and rediscovered only after an RPC failure. One HTTP client/transport is reused
  for the poller's lifetime and its idle connections are closed on shutdown.
  Install bases are re-resolved on each discovery attempt, so an `agy` self-update that migrates `~/.agy` →
  `~/.gemini/antigravity-cli` mid-flight is picked up without a restart.
- **Monotonic writes.** `saveAntigravityQuotaSnapshotIfNewer` never lets a
  slower in-flight probe age the card backwards for the same account; an account
  switch always wins regardless of timestamps.
- **Redaction allowlist.** Only `observedAt`, `accountFingerprint`, `account`,
  `plan` and the numeric buckets are persisted
  (`sanitizeAntigravityQuotaSnapshot`, which also drops unplottable windows and
  length-clamps every string). Discovered ports, log text, argv, prompts and
  `settings.json` contents are never written to the cache and never logged. A
  reading the server could not attribute (`GetUserStatus` failed) is retried on
  the next tick rather than cached under a settings-file account.
- **Test seam.** `AIEXPEDITE_AGY_CAPTURE_INTERVAL` shortens the tick (mirrors
  `AIEXPEDITE_AGY_QUOTA_CACHE`); a non-positive or unparseable value falls back
  to the shipped constant.
