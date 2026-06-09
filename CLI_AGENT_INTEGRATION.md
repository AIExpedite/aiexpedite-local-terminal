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
