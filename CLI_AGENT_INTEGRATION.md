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
[cliagent_smoke_claudecode.go](cliagent_smoke_claudecode.go). The
provider-agnostic half of the probe — the closed `cliSmokeDiagnostic*` set,
the metric-only `cliSmokeResult`, the cooldown, the singleflight collapse and
the per-binary resolved-shape cache — lives in
[cliagent_smoke.go](cliagent_smoke.go) and is shared with the Grok smoke
(see the Grok section below). `runCLISmoke` routes a `cliId` through the
`cliSmokeProviders` table (`claudeCode`, `grok`); anything else is
`provider_unavailable` / `unknown_cli` without spawning.

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
contributes `cliSmokeBinaryStamp` (path, mtime, size, version), which keys its
own cooldown so a verdict cached before an upgrade is never served for the
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
Grok from `grok models` (order and default) enriched from `~/.grok/models_cache.json`,
which listing refreshes when signed in and which carries the per-model
`reasoning_efforts` menu the TUI's `/model` picker shows; Claude
Code as its alias set with the scale from `claude --help`, non-exhaustive;
OpenCode re-shaped from its readiness probe, non-exhaustive. A user-initiated usage refresh
resets the cache. The signed refresh receipt canonicalises both fields
(`testdata/cli_usage_refresh_receipt_vectors.json`, vector 4, mirrored in
terminal-service), so **terminal-service must deploy first**.

**Quota pool per model (Ship B6, v1.0.21).** A detail row also carries `pool`
when the device can SAY which quota pool the model spends: a model the
provider meters under its own window — the usage snapshot has a metric naming
that model (Claude Code's weekly Fable window, a Codex per-model pool such as
`GPT-5.3-Codex-Spark`) — reports the window's key, the model id lower-cased,
which is exactly how shared-constants keys a nested sub-limit
(`nestedPoolNameFor`). The annotation is re-derived on every gather from THAT
gather's snapshot (`annotateModelPoolsFromUsage`), so the 30-minute probe cache
holds the raw discovery only. Every other model is reported WITHOUT a pool and
the backend falls through to the catalog rule (`cliAgents/<id>.modelPools`):
Antigravity in particular stays there — `agy models` prints no group, and the
quota RPC meters groups ("Gemini Models", "Claude and GPT models") without
saying which model belongs to which, so the device does not guess a mapping
that would outrank the authored rule. Bounded to 64 bytes
(`cliUsageMaxModelPoolLength` = shared `MODEL_POOL_NAME_MAX`); an over-long
name is dropped rather than truncated, since a name truncated on one side only
matches no verdict and routes the model fail-open. Vector 5 of the shared
receipt file pins the `pool` byte layout; terminal-service's receipt allowlist
must carry `pool` before a device running this build reports it (a receipt
field the verifier does not know rejects the whole refresh).
`AIX_B5_HARNESS_OUT=<file> go test -run TestB5ModelDiscoveryHarness -v` runs the
real probes on this machine and writes the snapshot.

Three rules keep a probe from doing damage on the way:

- **A probe's deadline is derived, never fresh.** `runCLIAgentModelProbe` takes
  the caller's gather context and layers its own 20s cap on top, so
  `GatherCLIAgentUsageOnly` — which runs every provider serially under one 10s
  deadline — can never be held past that by a slow `agy models`. An
  inconclusive probe whose context expired is NOT cached, or one refresh that
  ran out of time would hide the agent's models for the whole 30-minute TTL.
  Under a bounded gather the probes are rationed by ONE
  `cliAgentDiscoveryBudget` shared across every provider (4s of time actually
  spent per gather, at most 2s per probe), and a probe never takes what the
  providers still to be polled need — 3s (one utilization probe) for each
  later parser that performs bounded I/O (Claude — one window per serial child
  it declares — Codex, OpenCode and Antigravity, whose loopback quota request
  also derives from the gather; only Grok reads purely local state), summed by
  `cliAgentUsageGatherReservesAfter`
  over the ordered run, so the first provider on a fully loaded box gets a
  short probe and the last gets the full cap. A probe the gather cannot afford is skipped
  and the cache answers; a cold cache reports nothing and the next gather asks.
  Discovery as a whole is therefore bounded under the refresh deadline. The
  same rule reaches OpenCode's readiness probes: `openCodeUsageParser`
  implements `ParseContext`, so `opencode models` and `opencode auth list`
  derive their 3s caps from the gather context (the earlier deadline wins) and
  an answer the deadline cut is returned uncached with the forced re-probe
  intact, instead of pinning "unknown" to the card for the readiness TTL. The
  optional `auth list` gets only what the gather can spare beyond the same 3s
  reserve (`optionalOpenCodeProbeContext`) and is skipped otherwise — the
  provider names then derive from the listed model ids — so a stall there can
  never have the conclusive `models` answer discarded as canceled. Claude's
  `claude auth status --json` (`claudeAuthStatusProbe`) takes the gather
  context the same way, so both of Claude's probes end with the gather.
- **`grok models` describes the same service, config and login as an ACP
  session.** `sanitizeGrokModelListEnv` starts from the maintenance-smoke
  sanitizer (every `GROK_*` stripped, telemetry / Rust noise dropped, the
  `grokNeutralisedIntegrationSwitches` pinned to 0) and restores an explicit
  allowlist — `grokModelListRoutingEnv`: `GROK_HOME` (the login and cache
  directory the merged cache is read from), `GROK_CONFIG_PATH`, the endpoint
  overrides (`GROK_API_BASE_URL`, `GROK_MODELS_BASE_URL`,
  `GROK_MODELS_LIST_URL`, `XAI_API_BASE_URL`) — plus `XAI_API_KEY` ONLY when
  `Config.EnableGrokAPIKeyFallback` is on, the same opt-in the ACP launch
  honours. Not the whole `GROK_*` family: `GROK_LOG_FILE` and
  `GROK_FUTURE_EXECUTION_OVERRIDE` change what a headless child DOES (a raw
  diagnostics sink outside the isolated home, a swapped execution path) and
  the smoke suite pins that no such sink is ever written. Listing with the
  routing vars stripped would ask the default service for a catalog the
  configured sessions never use, or list logged-out on a key-authenticated
  host, and publish that as exhaustive. And the list runs against an
  ISOLATED home built by `setupIsolatedGrokHomeFrom` exactly as a managed
  session's is — cached login copied in, the persisted `[model] api_key` only
  under the same opt-in — so a key the user never opted into cannot
  authenticate the list and publish another account's catalog; the cache the
  list just refreshed there is read first, the real home's cache is the
  fallback. If the isolated home cannot be built the list is NOT run against
  the real home — it fails closed like a managed session, and the cache alone
  answers. The isolated home is seeded with the ACP default runtime model
  (`grokACPDefaultModel`) so a key kept in the per-model
  `[model.<runtime>] api_key` form rides along under the opt-in as it does for
  a session. **Grok discovery is ALWAYS a floor** (`mergeGrokDiscovery` never
  raises `Exhaustive`): which account a session runs as is decided per
  session — the opt-in key, a per-model persisted key, an external
  `GROK_CONFIG_PATH` (possibly relative to the session's cwd), and the
  project's own `.grok/config.toml` found upward from that cwd — and a
  device-level probe has no session cwd, so it cannot prove it listed the
  catalog a given session will see; an exhaustive claim would let routing
  veto a model that session accepts. A backend catalog entry
  with its own id reaches discovery through its
  `capabilities.utilization.parserKey`, as the usage parser does; OpenCode's
  legacy `models` list drops an id past its own 2048-byte receipt bound (as
  the detail rows drop one past 256) and collapses repeated ids, which the
  receipt rejects as duplicate identities. **OpenCode discovery is ALWAYS a
  floor too**, for the same reason as Grok's: `opencode models` runs with the
  daemon's cwd, while a session runs in its repo, where a project-level
  `opencode.json` can add providers and models the device-level probe never
  lists.
- **Codex without a models cache still reports its configured model.** A fresh
  install or a cleared cache reports the top-level `model` from `config.toml`
  (basic `"…"` or literal `'…'` string) as a one-model, non-exhaustive floor,
  not nothing.
- **A Codex cache is ALWAYS a floor.** `models_cache.json` is server-fetched
  and only a `codex` run refreshes it, so the catalog can gain a model with no
  binary upgrade and a same-`client_version` cache would not know; Codex
  discovery is therefore never exhaustive (the writer-version comparison still
  drives Grok, whose list probe refreshes its cache live). And a model whose
  listed levels are all outside the shared union has an UNKNOWN scale —
  `noEffort` is reported only when Codex listed no levels at all, because
  `noEffort` tells the resolver to drop the flag.
- **Cache-only Grok discovery is a floor.** When the list command fails but the
  cache reads, `modelsExhaustive` is false regardless of the build-version
  match — Grok fetches this catalog from its backend, so it can gain a model
  with no binary upgrade and an exhaustive claim would let routing veto a model
  the CLI accepts.

Both `modelDetails` and the legacy `models` list are truncated to
`cliUsageMaxModelsPerProvider`; the receipt rejects a provider that exceeds it
on either field, so an over-cap vendor answer must never reach `canonicalProvider`
whole. Three more bounds follow from the same rule (Codex round 2 on #147):

- **A detail row the receipt would reject is dropped, not carried.** A model id
  over 256 bytes (the `modelDetails[].id` bound — the legacy `models` list
  allows 2048) or an empty id is filtered by `boundedModelDetails` before the
  cap, so a user-configured OpenCode provider id or a long vendor slug cannot
  turn the whole refresh into `usage result rejected`.
- **A capped or filtered list is not exhaustive — for any CLI.** OpenCode's
  readiness probe stops reading AT the cap, so a list of exactly that length
  may be truncated, and an id dropped from the details was still listed
  (OpenCode is never exhaustive anyway — see the floor rule above); for
  Codex / Antigravity / Grok the generic branch compares the bounded details
  against what the probe returned. In every such case `modelsExhaustive` is
  false, or routing would veto a model the CLI runs. The same bound guards
  `model`: a configured or reported default past 256 bytes is left unset
  rather than copied into a field `canonicalProvider` would reject.
- **A reset invalidates probes already in flight.** The cache carries a
  generation that every reset advances; a probe stores its answer only under
  the generation it started in. Otherwise the six-hour gather mid-probe when a
  user forces a refresh would repopulate the cache with its pre-reset list and
  the refresh would read that for the whole TTL.

## The no-tools maintenance smoke (`__cli_smoke__`, cliId `grok`)

Grok runs on the **same signed `__cli_smoke__` channel as Claude**:
`runGrokSmoke` in [cliagent_smoke_grok.go](cliagent_smoke_grok.go), argv from
[grok_argv.go](grok_argv.go), shared core in
[cliagent_smoke.go](cliagent_smoke.go). The harness dispatches
`__cli_smoke__` with `["grok"]` as the (HMAC-signed) first arg and receives one
correlated `__cli_smoke_result__`.

**Do not use `session_start` for a Grok `-p` / no-tools smoke.** The signed
`session_start` maintenance transport (`--tools "" --disable-web-search
--no-subagents --max-turns 1 --verbatim <marker prompt>`, promoted by
`sessionStartArgsForCommand`) is kept working for older publishers, but it is
a thin caller of the same builder: `StartSession` validates the wire request
with `validateGrokSmokeRequest`, stages the prompt in a file, and spawns the
**same canonical argv** below (`validateGrokSmokeShape` at both call sites).
It returns streamed frames, not a structured verdict. `grok agent stdio`
(the ACP path) refuses every root-only one-shot flag outright — silently
dropping `--tools ""` would turn a no-tools smoke into a fully tooled session —
and its error names `__cli_smoke__`.

Why this exists: the previous contract was a hand-frozen **12-token** child
argv re-asserted in two places, and it carried an **empty argv element**
(`--tools ""`). On Windows the `grok` on PATH is frequently an npm `.cmd` shim,
and a `cmd.exe` re-parse of the shim's `%*` can drop an empty operand, leaving
`--tools` to swallow `--disable-web-search` as its value. The child then exits
non-zero **during option parsing, before inference**, so no marker is ever
echoed and the pre- and post-update smokes fail identically — while `grok
models` (no empty operand, none of the contested flags) stays healthy, and the
post-update usage refresh replays the pre-update observation because the
smoke never fetched credits.

Canonical shape (rung 0, `streaming-notools-prompt-file`):

```
--output-format=streaming-json   # per-event NDJSON frames; `end` is the terminal envelope
--tools=                         # THE FIX: equals-form empty value, no separate empty argv element
--disable-web-search
--no-subagents
--max-turns=1
--prompt-file <0600 temp file>   # the marker prompt; never inline -p
```

- **No argv element is ever the empty string**, `--tools` is **always the
  equals form**, and the **marker nonce never appears in argv** — Grok's
  headless mode does not read a piped stdin, and `-p <nonce>` would sit in a
  process listing any local user can read, so the prompt is staged by
  `writeGrokPromptFile` (owner-only, under `~/.ai-expedite/grok-prompts/`) and
  removed exactly once after the child is reaped. All three are pinned by
  [grok_argv_test.go](grok_argv_test.go).
- `--no-auto-update` is **dropped from rung 0** (current builds reject it at
  the root command — exactly the pre-inference exit being reported — and the
  isolated home already pins `auto_update = false`) and `--verbatim` is gone
  (not a documented root flag; the prompt asks for a verbatim echo). Rung 1
  (`streaming-notools-prompt-file-legacy`) drops the two isolation switches
  and carries `--no-auto-update`, the flag set an older build documented.
- **Walk order follows the installed version** (`grokSmokeArgvShapesForVersion`):
  a build reporting a version below `grokSmokeHardenedFlagsMinVersion`
  (1.0.13, the first release documenting the isolation switches) tries the
  legacy rung first; everything else — including an unparseable version —
  keeps the canonical order. Every rung is always present, so a wrong guess
  costs one free pre-inference spawn, not the verdict. The legacy
  `session_start` smoke streams ONE child and cannot walk, so the
  version-ordered first entry **is** its resolution of a compatible rung: an
  older publisher's pre-update smoke against a pre-1.0.13 build is no longer
  rejected during option parsing before it can bless the upgrade.
- The ladder retries **only** on `flag_rejected` naming a flag another rung
  drops (`grokSmokeRetryableFlags`, derived from the shapes in both
  directions, never hand-listed): option parsing precedes inference, so a
  rejected rung costs no quota. A rejection of a flag every rung carries is
  reported as `flag_rejected` without a second child. The winning rung is
  cached per `(path, mtime, size)`; the session_start smoke takes its rung
  from the same cache.
- **Windows `.cmd` / `.bat` shim:** the smoke routes the launch through
  `cmd.exe` with an explicit command line (`grokSmokeShimCommand` →
  `configureGrokWindowsCommandLine`): `call "%AIEXPEDITE_GROK_SMOKE_SHIM%"
  <fixed flags> --prompt-file "%AIEXPEDITE_GROK_SMOKE_PROMPT_FILE%"`. Both
  paths ride in the environment, the script carries only the ladder's fixed
  flag tokens, and `HideWindow` survives. A native `grok.exe` is spawned
  directly. [cliagent_smoke_grok_windows_test.go](cliagent_smoke_grok_windows_test.go)
  (`//go:build windows`) round-trips every token through a real batch shim.

Pre-inference checks, all free (no turn spent, never pinned by the cooldown):
the binary must answer `--version` (`binary_missing`); the version must be
parseable and `detectGrokMaintenanceSmokeSystemConfig` must accept the system
config posture (`internal`, via the typed `grokSmokePreflightError` — the layer's
contents are never published); the auth-only, MCP-disabled isolated home from
`setupIsolatedGrokSmokeHomeFrom` must build (`internal`); and
`assessIsolatedGrokLaunch` must find a usable login (`not_logged_in`). The child
runs with `sanitizeGrokMaintenanceSmokeEnv` (every `GROK_*` / `OTEL_*` / Rust
diagnostic stripped, the integration switches pinned off), `GROK_HOME` /
`HOME` / `USERPROFILE` / `PWD` redirected into the isolated tree, and an empty
workspace as cwd.

Classification, onto the same closed sets as Claude:

| Observation | `errorCategory` | `diagnostic` |
| --- | --- | --- |
| Binary absent, `--version` non-zero | `provider_unavailable` | `binary_missing` |
| Unparseable version, refused system config, isolation setup failed | `internal_error` | `internal` |
| Isolated login unusable (`assessIsolatedGrokLaunch`) | `not_authenticated` | `not_logged_in` |
| `error` frame reporting auth | `not_authenticated` | `auth_error` |
| `error` frame reporting a limit / credit / outage | `provider_unavailable` | `provider_error` |
| The attempt's deadline killed the child | `provider_timeout` | `timeout` |
| Option-parse rejection of one of our flags | `protocol` | `flag_rejected` |
| Rejection of `--output-format` / `streaming-json` / `--prompt-file` | `protocol` | `framing_rejected` |
| Exit with no terminal `end` frame (non-zero, or clean with no frames) | `protocol` | `no_envelope` |
| `end` frame whose concatenated `text` deltas are not exactly the marker | `parse_failed` | `marker_mismatch` |

`text` frames are incremental deltas (`text`, or `data` from 1.0.13) and are
concatenated with **no separator** — the same rule `readOutputStream` applies
for the session smoke — because a separator at a frame boundary corrupts the
exact marker.

Cost and privacy posture match Claude's: one real inference turn per smoke
against the user's own Grok window, the 15-minute per-CLI cooldown keyed on
the binary stamp (`cliSmokeBinaryStamp`), `singleflight` collapse of concurrent
callers, at most one retry child per smoke and only on a pre-inference
rejection. Worst case per upgrade: 2 spent turns + 2 free rejected spawns. The
published result carries `{cliId, version, status, errorCategory,
markerMatched, durationMs, argvShapeId, diagnostic}` and nothing else; the
child's stdout/stderr, the prompt, the nonce, the resolved argv and the
isolated home's contents (`auth.json`, `config.toml`, the private log) are
never published or logged (`grokSmokeFailureLogLine` takes the stderr
**length**, not the bytes).

### Post-update freshness

"Usage freshens after upgrade" holds because three things hold together:

1. The cooldown is keyed on the binary stamp, so an upgrade **invalidates** the
   pre-update verdict and the post-update smoke actually executes.
2. The smoke child writes its `billing: fetched credits config` record into
   the isolated home; `runGrokSmoke` calls the existing
   `persistGrokManagedBillingSnapshot(isolatedHome, persistentHome,
   producerContested)` after the last child exits — whatever the verdict — and
   before the isolated home is removed. Same merge `waitForExit` performs for
   the session path; a contested producer still refuses the merge.
3. `grokNewestBillingObservation` ([cliagent_usage_grok.go](cliagent_usage_grok.go))
   resolves the card's observation as the **newest** of the persistent log tail
   (a direct run's own record, or a merged smoke / ACP record) and the live
   billing cache, so a direct (ACP) run, a terminal (session) run and a smoke
   all advance `latestObservedAt`. The account gate is upstream of that choice:
   a foreign record or a foreign live entry never ages the observation forward.

Pinned by [cliagent_smoke_grok_test.go](cliagent_smoke_grok_test.go)
(classification, budget, cooldown, redaction, prompt file),
[cliagent_usage_grok_freshness_test.go](cliagent_usage_grok_freshness_test.go)
(newest-source selection) and
[cliagent_usage_grok_post_update_freshness_test.go](cliagent_usage_grok_post_update_freshness_test.go)
(the upgrade end to end across the smoke and usage layers). The end-to-end
"real grok binary echoes the marker" case stays a manual pre-release check on
a Windows device.

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
- [`grok_acp.go` — `grokACPRootOnlyArg`](grok_acp.go) — rejects (never
  strips) every root-only one-shot flag (`--tools`, `--max-turns`,
  `--prompt-file`, `-p`, …) and points the caller at `__cli_smoke__`.
- [`grok_argv.go`](grok_argv.go) — `grokSmokeArgvShapes` /
  `buildGrokNoToolsSmokeArgs` / `validateGrokSmokeShape` /
  `validateGrokSmokeRequest` / `sanitizeGrokMaintenanceSmokeEnv`: the ONLY
  source of the maintenance-smoke argv, for both `__cli_smoke__` and the
  `session_start` transport.
- [`cliagent_smoke_grok.go` — `runGrokSmoke`](cliagent_smoke_grok.go) — the
  probe: preflight → isolated home → prompt file → ladder → classify → billing
  merge → cleanup; `grokSmokeShimCommand` for Windows `.cmd` shims.
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

## Grok login keeper (why `grok login` was needed every 6 hours)

Grok Build's login is a 6-hour access token plus a refresh token that
**rotates on every use**: a refresh answers with a new pair, and the old
refresh token stays redeemable only for a short grace window (measured on
2026-09-15 with `grok` 1.0.30: accepted 44 s after rotation, rejected 2.5 min
after). When a refresh is rejected the CLI signs that home out — it deletes
`auth.json` — and the user is back at `grok login`. The CLI itself refreshes
silently, inside `grok agent stdio` too, 5 min before expiry
(`GROK_AUTH_EARLY_INVALIDATION_SECS`), and hot-reloads `auth.json`.

Every Grok run this agent starts uses an isolated temp **copy** of the login
(`grok_isolated_home.go`) that is deleted when the run ends. The first copy to
reach the 6-hour mark refreshed, took the only live token with it, and left
the real home holding a refresh token xAI revoked two minutes later. The next
thing to touch the real home — the user's own `grok`, or a new copy taken
after the 6-hour mark — was signed out. Every computer running the agent
needed `grok login` every 6 hours, and the frontend had learnt to call a
Grok "refreshable" deadline a hard one.

`grok_login_keeper.go` applies one rule: **the newest credential of the
account wins everywhere.**

- Every 20 s the keeper reads the real home. Inside 30 min of expiry
  (`grokLoginKeepAhead`) it runs `grok models` against the real home with
  `GROK_AUTH_EARLY_INVALIDATION_SECS=1800`, so the CLI refreshes now. A copy
  therefore never reaches its own 5-minute refresh point while the keeper runs.
  A renewal that changes nothing (CLI missing, xAI unreachable) is retried
  every 5 min, not every tick.
- After every renewal — the keeper's or the live probe's — and on every tick,
  `reconcileGrokLogin` writes the newest same-account credential (ordered by
  the CLI's `create_time`) into the real home and every live copy, atomically
  (write beside, rename over). A copy that refreshed on its own is written
  BACK to the real home within one tick, inside the grace window; a renewed
  real home is fanned OUT to the copies, which hot-reload it. Copies of another
  account are never mixed in.
  Removing an isolated home reconciles first, so a short `grok models` run
  that refreshed hands its credential back before the file dies; and the tick
  reconciles BEFORE judging the real home, so a copy's newer credential is
  written back rather than the superseded real one being renewed. Renewal and
  reconciliation run under one login lock.
- `grokLoginGuard` still serializes renewals (two rotations minutes apart sign
  the loser out) and still keeps a copy from being taken mid-renewal, but a
  live copy no longer blocks a renewal: it receives the result instead.
- The keeper also sweeps `grok-acp-home-*` directories older than 24 h that no
  live process owns (178 had accumulated on one computer since July). Every
  agent process holds an exclusive lock on `aix-owner.lock` inside each home
  it created, released by the OS when it dies, so a second agent process on
  the same computer (the dev and the release channel) never sweeps a home
  whose session is still running, however old the directory looks. A
  candidate is registered as a copy before removal, so a newer credential an
  earlier process left in it is written back to the real home first; the
  removal goes through `removeIsolatedGrokHome` so a linked conversation
  store is unlinked, never deleted.
- Two agent processes can share one Grok home (the dev and the release
  channel on one computer). A renewal pass — write-back, CLI, fan-out — holds
  the cross-process `aix-renew.lock` beside the real home's `auth.json`, so
  their keepers never rotate one refresh token twice; the loser skips its
  tick (the probe reports `login_busy`). Before renewing, a keeper also
  looks at every `grok-acp-home-*` home in the temp dir it did not create —
  another process's live copies and the orphans of a process that died
  right after a child refreshed: a newer same-account credential one of them
  holds is written back to the real home instead of the superseded token
  being renewed. Those copies are read, never written. The keeper finds the
  CLI the way the launchers do (PATH, then the installer's `~/.grok/bin`).
- Every `auth.json` replacement stages its bytes in a uniquely named file
  beside the target (`os.CreateTemp`), never a fixed name: two agent
  processes reconciling one home must not rename each other's half-written
  bytes into place.

Frontend: `UNTRUSTED_AUTO_RENEWAL_PROVIDERS` (cliAgentAvailability.js) is empty
again from the same day; a genuine sign-out still reaches it as authState
`missing`, because the CLI deletes the file.

Never test rotation by reusing a real home's refresh token from a copy: the
real one is revoked two minutes later.

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

With the poller armed, that same replay carries the **in-run** `observedAt`: the
gather still finds no port, but the snapshot it loads was written while the run's
own server was alive. The gather adds no post-smoke probe of its own; the only
read that happens after the run releases is the capture's own 2s tail (below),
which is still an in-run read and can itself write a newer `observedAt` while the
server shuts down.

`ParseContext` replays that cache under three predicates, not one. Current
identity comes from `~/.gemini/antigravity-cli/settings.json`, or from legacy
`~/.agy/config.json` when the modern file is absent:

1. **Scoped load.** The cached `accountFingerprint` equals the fingerprint of the
   account that identity file names — replayed as stored.
2. **No identity on disk.** That is the usual case, since the account lives in
   the OS keyring, so the fingerprint is empty and `loadAntigravityQuotaSnapshot`
   refuses by construction. The cache is then replayed under the account that
   PRODUCED it (`loadAntigravityQuotaSnapshotByProducer`) and the card names that
   account. **No live probe is required for this.**
3. **A recent live probe outranks the file.** When the identity file names
   someone the cache does not match, a live probe from the last
   `antigravityLiveProducerTTL = 2m` (a Refresh click) whose own server named the
   cache's producer wins: the cache is replayed and the card takes the *probe's*
   account and plan, because that server's login is newer than the one the file
   still records. The probe is matched against the CACHE, never against
   `settings.json`.

A file-named account with no such probe and no matching cache leaves the reading
unreplayed.

[`cliagent_usage_antigravity_capture.go`](cliagent_usage_antigravity_capture.go)
closes that gap by reading the server **while it exists**:

- **Armed at every spawn.** The arm sites are
  [`runOneShot`](antigravity_native.go) (native chat, `"native turn"`),
  [`runPTYCommand`](pty_session_unix.go) (tty=true session + `execute`,
  `"PTY session"`), [`StartSession`](session.go) (pipe session path,
  `"pipe session"`, released in `waitForExit`),
  [`runLocalCommandUnix`](pubsub.go) (tty=false `execute` — the bare-exec route,
  which a DIRECT `agy …` takes on Windows too, `"local execute"`) and
  [`runLocalCommandWindows`](pubsub.go) (the WRAPPED Windows transport chain,
  `"windows execute"`). Native chat calls
  `startAntigravityQuotaCapture` directly — it already knows it is spawning
  `agy`; every other site goes through `armAntigravityCaptureForCommand`, which
  pairs `commandRunsAntigravity` with the arm in one place so a site cannot drift
  out of step with the classifier. Windows has no PTY path, so capture there
  comes from native chat, the pipe path and `execute`.
  `commandRunsAntigravity`
  ([`cliagent_usage_antigravity_command.go`](cliagent_usage_antigravity_command.go))
  sees through every wrapper transport terminal-service emits
  (`wrapperScriptPayload` in [`headless_env.go`](headless_env.go)): the POSIX
  shells' `-c` / `-lc`, `cmd /c` and `/k`, PowerShell's `-Command` / `-c`, and
  `powershell`/`pwsh` `-EncodedCommand <base64>` — the wrapper flag is matched at
  ANY argument index, so `-NoProfile` / `-NonInteractive` / `-OutputFormat Text`
  ahead of it change nothing. That includes the `scriptMode: "file"` launcher whose
  outer payload carries the real script as a nested base64 literal. The decoded
  script is a classification input and nothing else — it is never logged,
  persisted or returned, and the only value that leaves that file is a bool. A
  payload over `antigravityClassifyMaxPayloadBytes` (256 KB) classifies as *not*
  Antigravity rather than growing the decode budget, and that cap is applied to
  an `-EncodedCommand` argument BEFORE it is decoded
  (`encodedCommandFitsClassifyBudget`): the payload is base64 of UTF-16LE, so an
  argument whose base64 already exceeds the bound cannot decode under it, and
  refusing on the encoded length keeps an oversized argument from allocating the
  base64 buffer, the UTF-16 slice and the decoded string on the way to being
  rejected. The nested file-mode literal needs no separate check — it is only
  reached from a script that already passed the cap.
- **One shared, refcounted poller.** Concurrent `agy` runs join the same
  goroutine; it stops when the last one releases it.
- **Timing is everything.** The server dies *with* the child, so a post-exit
  probe cannot be relied on to find anything. The poller therefore probes
  **immediately at arm**, then ramps
  (`antigravityCaptureInitialPollInterval = 250ms` ×
  `antigravityCaptureInitialPolls = 12`, covering server bind time and short
  turns) down to `antigravityCapturePollInterval = 3s`. Arm timing is per path:
  native chat, PTY, the pipe session and the Unix `execute` arm **after a
  successful `Start`**, so the immediate probe cannot run before the process
  exists, and a failed spawn arms nothing at all.
  `runLocalCommandWindows` is the exception. It has no single post-`Start` hook —
  each transport in the chain (encoded-argument PowerShell, temp `.ps1`, a
  dedicated one-shot process, `runViaShell`, the persistent instance and its
  fallbacks) owns its own process — so it takes one `defer` arm at **function
  entry**, before any child exists. What that costs is not one wasted probe:
  PowerShell startup alone is 300–800ms, and until a port is memoized every tick
  is a full discovery attempt that walks both install trees. So the pre-server
  window is several log scans — the immediate probe plus a 250ms tick for as long
  as the ramp lasts (`antigravityCaptureInitialPolls = 12` discovery attempts,
  ≈3s), then one every 3s. Those attempts are bounded by the same
  `antigravityCaptureMaxAttempts = 200` / `antigravityCaptureMaxDuration = 15m`
  caps, after which the poller parks (or, with no port memoized, returns and
  waits for release). **An early arm is not a guarantee of capture:** a server
  that binds and exits entirely inside a gap between ticks is never sampled, and
  with `memoPort == 0` there is no tail to catch it either, so that smoke still
  finishes stale. What the entry-time arm does buy is that a sequential failover
  through the chain cannot double-arm.
  Arming is still gated on the classifier: a **classified** Windows execute arms
  once at entry and releases on return even when no transport ever starts a
  child; an **unclassified** one gets `armAntigravityCaptureForCommand`'s no-op
  release and arms nothing at all (`antigravityCaptureArms` stays 0 —
  `TestAntigravityFreshness_NonAgyWindowsExecuteArmsNothing`).
  Note the route split: a DIRECT `agy …` on Windows never reaches that defer.
  `runLocalCommand` sends it to `runLocalCommandUnix` on the `isAntigravityCommand`
  predicate (the CLI streams, and wrapping it in PowerShell breaks the stream), so
  it arms `"local execute"` after a successful `Start` like the Unix path.
  `runLocalCommandWindows` owns only the WRAPPED transports.
- **Bounded discovery, continuous live-port reads.**
  `antigravityCaptureMaxAttempts = 200` and
  `antigravityCaptureMaxDuration = 15m` bound attempts that may scan logs. Once
  either cap is reached, a memoized live port continues receiving cheap 3s
  loopback reads until the run ends; no further log scans occur. If no port was
  found, the poller parks until release.
- **A short tail after the last release.** A turn's quota is debited at the END
  of the turn, and the execute path releases the instant `Wait` returns while the
  server is still shutting down and still answering. So after the last armed run
  releases, the poller keeps reading for `antigravityCaptureTailGrace = 2s` —
  memoized port only, no discovery and no log scanning. A run that exits with no
  memoized port, or one stopped by the CSRF gate, skips the tail entirely. That
  tail is the last in-run read, not a second gather.
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
- **Test seam.** `AIEXPEDITE_AGY_CAPTURE_INTERVAL` shortens the tick and
  `AIEXPEDITE_AGY_CAPTURE_TAIL` the tail window (both mirror
  `AIEXPEDITE_AGY_QUOTA_CACHE`); a non-positive or unparseable value falls back
  to the shipped constant.

### Antigravity ≥ 1.2.2 refuses loopback quota reads (the CSRF gate)

Everything above assumes the language server answers a bare loopback POST. From
`agy` 1.2.2 (first seen 2026-09-11) it does not: a CSRF interceptor in front of
every RPC answers

```
401 {"code":"unauthenticated","message":"missing CSRF token"}
```

on the HTTP and the TLS port alike, before and after the server has
authenticated its account. The per-run `x-codeium-csrf-token` value is
generated by the CLI and handed only to its own tool subprocesses
(`ANTIGRAVITY_CSRF_TOKEN` + `ANTIGRAVITY_LS_ADDRESS`). Established on a 1.2.3
install on 2026-09-15, and worth not re-deriving: print mode (`agy -p`, the
only headless mode) loads `hooks.json` but fires no hook event
(SessionStart / PreInvocation / PostInvocation / Stop / PreToolUse); a
user-level `mcp_config.json` stdio server does start in print mode but
receives no `ANTIGRAVITY_*` variable; an `ANTIGRAVITY_CSRF_TOKEN` set on the
`agy` spawn is not adopted ("invalid CSRF token"); the value is composed per
child spawn, not kept in the process environment block; and no Origin /
Referer / Sec-Fetch variant is exempt. Workspace-level `.agents/hooks.json`
and `.mcp.json` never load from an untrusted directory.

The server was only ever a proxy for Google's Code Assist API, and the OAuth
token it uses is on the machine: `agy` keeps it in the OS keyring (Credential
Manager `gemini:antigravity` on Windows, Keychain service `gemini` / account
`antigravity` on macOS, secret-service on Linux) as JSON of the shape
`{"auth_method", "id_token", "token": {access_token, token_type, refresh_token,
expiry}}` — read off a real entry's key names on 2026-09-15; a bare OAuth2
token JSON is accepted too, and an entry in any other shape is logged by its
key names only. **On macOS the Keychain returns that JSON inside go-keyring's
base64 envelope** (`go-keyring-base64:<base64>` — the Go library `agy` stores
through encodes before `security add-generic-password`), which v1.0.24-26
logged as "not a JSON object" and reported `codeassist_no_login` on every
Refresh while Windows, whose Credential Manager blob is unencoded, worked;
v1.0.27 strips the prefix and decodes (`decodeAntigravityKeyringEnvelope`,
prefix-driven so the Windows / Linux paths are untouched). So a gated build is read the way the Grok and Claude
probes read theirs — the CLI's own stored credential against the vendor's
endpoint ([`cliagent_usage_antigravity_codeassist.go`](cliagent_usage_antigravity_codeassist.go),
keyring readers in `antigravity_keyring_*.go`):
`POST https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`
with `Authorization: Bearer` **and agy's own `User-Agent`**
(`antigravity/cli/<version> (aidev_client; os_type=…; arch=…; auth_method=consumer)`,
`antigravityCodeAssistUserAgent`): Google licenses the endpoint per client and
identifies the client by that header — the same token with Go's default
User-Agent is answered `403 PERMISSION_DENIED / SUBSCRIPTION_REQUIRED` ("You
do not have a valid license of this product"), which is what v1.0.24-25 logged
as `codeassist_unauthorized` on every Refresh (found and verified on AIE2,
2026-09-15; v1.0.26 carries the header). The account is named by the stored `id_token` else
Google's userinfo, persisted through the same allowlisted snapshot path under
that account's fingerprint. Only the Refresh click runs it, only once the
loopback route is known gated; the agent never renews the login (an expired
token gets the click's `agy models` warm-up first — `agy` refreshes its
keyring token on every run — then one retry; still expired is reported as
`codeassist_token_expired`). Endpoint overrides are loopback-only, redirects
are refused, no proxy is inherited, the refresh token is never decoded.
Outcomes are the `codeassist_*` codes in the device log; a non-2xx status is
logged by number so a moved request shape (400) is distinguishable from
Google being down (5xx).

What the agent does instead ([`cliagent_usage_antigravity_gate.go`](cliagent_usage_antigravity_gate.go)):

- `antigravityPostJSONOutcome` classifies the refusal (`antigravityRPCGated`)
  apart from every other failure; `fetchAntigravityQuotaOnPortOutcome` and
  `fetchAntigravityQuotaDetailed` carry it up.
- **The run-scoped poller stops at the first refusal** and parks until the run
  ends (no 15-minute log scan collecting 401s, no tail window).
- **The Refresh click stops at the first refusal**, kills the probe `agy`
  before its model turn, and records the build in the gate marker
  (`antigravity_quota_gate.json` in the agent's data dir;
  `AIEXPEDITE_AGY_QUOTA_GATE` relocates it). Later clicks on the same build
  spawn no `agy` at all (outcome `gated`). The marker is per build — an
  `agy` update is tried the first time it is seen — and expires after
  `antigravityQuotaGateRecheck` (24 h); a successful reading clears it.
- **The card says why, only while it has to.** The parser keeps the last
  snapshot with its original `observedAt`; while no reading newer than the
  gate marker exists it sets `notice` / `noticeSeverity: warning` naming the
  build and that reading's date, instead of striped bars with no explanation.
  A reading the Code Assist route took after the refusal outranks the marker
  (`antigravityGateOutranksReading`; the poller re-noting the same refusal
  keeps the marker's first `observedAt`). The "a run completed after the last
  observation" log line is suppressed while the build is gated — nothing could
  have captured it.

### Run-completion freshness: the debt a finished `agy` run owes

The gate above leaves a hole the poller cannot fill. On **every current build**
the in-run capture reads nothing, so a finished direct or terminal `agy` run
left `observedAt` exactly where it was — and the one route that still returns
numbers, the Code Assist read, was wired to the Refresh click only. A passing
CLI-maintenance smoke was therefore still followed by a days-old reading.
[`cliagent_usage_antigravity_freshness.go`](cliagent_usage_antigravity_freshness.go)
gives Antigravity the run-completion path Codex
([`cliagent_usage_codex_freshness.go`](cliagent_usage_codex_freshness.go)) and
Claude (`triggerClaudeUsageProbeAfterRun`) already have, at a fraction of the
size: no rollout scanning, no cursor, one outbound read.

- **Arm.** `startAntigravityQuotaCapture` calls `armAntigravityUsageRunFloor(now)`
  and keeps the returned floor in the `finish` closure. All five spawn sites
  inherit it, because they all reach that function — no new call sites. The
  floor is returned immediately and PERSISTED on a goroutine, like
  `armCodexUsageRunFloor`: this is the spawn path (the Windows chain arms at
  function entry), and arming must never block the run. A write that loses the
  race with its own settle costs nothing — the arm only raises the floor, and a
  floor left behind for a run that did get its reading is dropped by the
  payment's cached-reading check before any request is sent.
- **Settle.** That `finish` runs `antigravityUsageRunSettled(floor, captured,
  gated)` on its own goroutine, inside the existing `sync.Once`. It must **not**
  wait for the poller: the poller is refcounted and exits only when the LAST
  armed run releases, so settling there would park a finished run's refresh
  behind a long interactive session sharing it. The decision is a time
  comparison against that run's own floor, never a flag — the debt clears only
  when the cached reading (`cachedAntigravityObservedAt`) covers it, within
  `antigravityObservedAtGrace = 1s` for the one-second resolution of RFC3339.
  `captured` is a hint (`antigravityCaptureLastPersistedMs`) that makes the
  ungated case cheap and appears in the log line.
- **Pay.** One `probeAntigravityQuotaCodeAssist` per debt, then one retry after
  `antigravityRefreshAfterRunRetryDelay` (5 s) — `antigravityRefreshAfterRunMaxAttempts
  = 2`, each under `antigravityCodeAssistTimeout` (8 s), under a process-wide
  single flight, with one debt at a time (a later run's floor REPLACES the
  pending one rather than queueing). A new debt's first read is spaced by
  `antigravityRefreshMinInterval` (60 s); a retry within one debt is the same
  unpaid run and bypasses it, as does the startup adoption. A debt the interval
  blocks is KEPT, not dropped — there is deliberately no timer to come back for
  it, because the next run's settle (or the next agent start) pays it, and a
  background timer per debt is work the user never asked for. The Refresh click
  does not go through this worker, so a user-initiated refresh is never
  throttled by it. **Ceiling: one outbound call per minute, whatever the run
  volume** — 200 short runs in an hour still spend at most 60.
- **It never spawns `agy`.** The click may run the `agy models` warm-up to make
  the CLI refresh its own keyring token; doing that behind the user's back on
  run teardown is a different class of side effect. So `codeassist_token_expired`
  KEEPS the debt and its budget (the next real run refreshes the keyring for
  free) and only `codeassist_no_login` stops attempting. The build the request
  identifies itself as comes from `antigravityCodeAssistBuildVersion` — the same
  cached `--version` detection already ran, shared with the click.
- **Skipped entirely while offline** (`IsOffline`): an offline agent makes no
  outbound request, and the debt waits for the next run rather than retiring,
  because offline is temporary. An **uninstalled** `agy` retires it without an
  attempt — no retry and no notice for a provider the card no longer shows.
- **Clear.** `settleAntigravityRunFreshness` is called from inside
  `writeAntigravityQuotaSnapshotLocked`, so EVERY route that lands a reading is
  a settler: the in-run loopback, the Code Assist read, a Refresh click, a
  concurrent run's poller. The lock order is cache → freshness; nothing under
  the freshness lock may read the cache.
- **Survive.** The debt is a file (`antigravity_quota_freshness.json` in the
  agent's data dir; `AIEXPEDITE_AGY_FRESHNESS` relocates it), so `StartAgent`'s
  `payOwedAntigravityUsageRefresh` pays ONE bounded read for a run the previous
  process never settled (crash, restart, self-update) — placed after `isOffline`
  is published so the first attempt honours offline mode. A floor further than
  `antigravityRunFloorLocalSkew` (30 s) ahead of `now` is a clock rollback and is
  discarded rather than parked in the future; a debt older than
  `antigravityRefreshOwedMaxAge` (30 min) retires, so an unpayable one can never
  pin a worker or a warning forever.
- **Report.** `antigravityFreshnessNotice(lastObservedAt, now) (notice, pending)`
  is the single accessor — no caller reads the state file. `notice` is empty on
  a gated build (the gate banner above already names the build and the reading's
  age; two sources for one banner would drift) and until the bounded attempts
  are spent. `pending` doubles as the "is a debt owed" query, and suppresses
  `antigravityMissedRun` for the same run so the pair cannot double-report it.
- **Redaction.** The state file holds `schemaVersion`, four epoch-millisecond
  fields, an attempt count, the `gated` bool, the hashed `accountFingerprint`
  (written by the PAYMENT — at arm time no server has named an account; it is
  diagnostic only, since clearing is decided by time alone) and a closed-set
  `codeassist_*` outcome. Never a token, a keyring payload, `settings.json`
  contents, an account email, a command line, a prompt, a port or log text.
- **Not covered, deliberately:** `commandRunsAntigravity` still does not peel an
  interpreter nested inside a composite statement (`cd repo && powershell
  -EncodedCommand <b64>`), so such a run arms nothing. Widening the classifier
  now costs an outbound Google call per false positive, so it belongs in its own
  change with its own false-positive review.
- **Test seams.** `AIEXPEDITE_AGY_FRESHNESS` relocates the state file;
  `antigravityRefreshAfterRunRetryDelay` and `antigravityRefreshMinInterval` are
  vars so a test pins them small; `antigravityUsageRefreshWaitIdle` waits the
  single-flight worker out. The mock CLI mode `antigravity-quota-gated`
  (`session_integration_test.go`) is a child-owned language server that refuses
  every RPC — the shape every current build has.

# Live usage probe — the Refresh click on the CLI Agents card

Passive capture only ever reports what the last run left behind, so a CLI that
was not used for a day showed a day-old pool whose windows had since reset
(every row striped). A **click** on the card's Refresh button now asks each
provider for its current figures before the gather runs
([`cliagent_usage_live_probe.go`](cliagent_usage_live_probe.go)).

- **Only a click.** terminal-service adds the first arg `live-probe` to the
  `__cli_usage_refresh__` it sends for `source: manual_refresh`. Args are inside
  the command HMAC, so the flag cannot be added in transit. Tab-open wakes, the
  active-refresh loop and heartbeats never carry it, and an older agent ignores
  it. Automatic refreshes also stopped resetting the model-probe cache — that
  reset relaunched `agy models` every five minutes while the tab was open.
- **What each probe does** (all in parallel, one 30 s budget, then the normal
  10 s gather):
  - Claude Code — nothing extra; the gather's OAuth usage request is already
    forced on every refresh.
  - Codex — a private `codex app-server --listen stdio://` answers
    `account/rateLimits/read`. It fetches from
    `chatgpt.com/backend-api/wham/usage` (it fails when offline), so the reading
    is live, and no turn is spent. Codex omits the `jsonrpc` member, so the
    probe re-wraps its own response before `captureCodexRateLimitLine`.
  - Grok — GET `cli-chat-proxy.grok.com/v1/billing?format=credits` with the
    token Grok's resolver presents ([`cliagent_usage_grok_live.go`](cliagent_usage_grok_live.go)).
    Headless Grok (`grok -p`, ACP) never fetches credits, so no run could
    refresh this. The agent never renews the login itself; an expired token or
    a 401 runs `grok models` once against the real home so Grok renews, then the
    request is retried once.
  - Antigravity — `agy -p` in an empty temp dir, only to start its language
    server; the quota RPC forces a fresh fetch and the run is killed as soon as
    the reading is persisted (~1.5 s, normally before any model output). The
    server is found by the PID `agy` logs, so a run the user has open is never
    read. `agy` names its log by the SECOND it started, so `agy models` waits
    for the probe instead of sharing that log file. On a build that refuses
    the RPC (≥ 1.2.2, see the CSRF gate above) the probe stops at the first
    refusal and later clicks skip the spawn.
  - Grok's token is the freshest one for the account across the real home AND
    every live isolated copy (`grokFreshestPresentedToken`). A renewal takes
    its turn for at most `grokLoginRenewGap` (2 s) when the login keeper is
    mid-renewal — never the whole probe budget, which turned "the login is
    busy" into a transport error — reports `login_busy` when it could not,
    and fans the renewed file out to every live copy (see "Grok login keeper").
- **Model lists** are re-listed in the same window with the time the slowest
  list needs; the bounded gather's 2 s discovery slot never fits `agy models`.
- **Concurrency.** Clicks share one running probe (singleflight) and a click
  within 15 s of a finished probe reuses it. Probes are not CLI sessions: they
  take no reservation or session slot and never touch a running session.
- **No window, on either console host.** Every background child goes through
  `hideWindow`, which sets BOTH `HideWindow` (SW_HIDE, honoured by conhost)
  and `CREATE_NO_WINDOW`. On a Windows 11 machine whose default terminal
  application is Windows Terminal, a new console created by a windowless
  parent is handed to Windows Terminal, which ignores SW_HIDE and opens a
  visible tab titled with the child's path — `agy models` flashed one on every
  refresh (confirmed with a `SetWinEventHook` watcher, 2026-09-15).
  `CREATE_NO_WINDOW` creates the console without a window, so nothing is
  delegated. `TestNoHandRolledHideWindow` fails any file that sets
  `HideWindow` by hand instead of calling the helper.

## Codex rows follow what Codex reports

`account/rateLimits/read` is a full snapshot, so once one has been seen
(`fullSnapshotAtMs` in the cache) the main pool renders only the windows the
account has — a weekly-only plan shows one row, not a striped 5-hour
placeholder. An account reporting no window at all keeps both placeholders
(the spent-quota shape). Each named limit (`limitName`, e.g. `codex_bengalfox`
→ "GPT-5.3-Codex-Spark") renders as its own rows labelled with the pool name
and carrying the model id, instead of being folded into the main rows where
its 0 % session window stood in for a main pool that had none. Routing still
sees those rows in Codex's shared pool (no `metric.pool` on the wire yet —
the B6 device half), exactly as the folded rows were before.
