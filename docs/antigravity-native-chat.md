# Antigravity native chat

## Capability research (agy 1.1.1 / 1.1.2)

| Capability | Result |
|---|---|
| Non-interactive prompt | `--print` / `-p` takes the **prompt as flag value** (not positional) |
| Exact resume | `--conversation <uuid>` resumes that conversation |
| Ambiguous resume | `--continue` resumes most-recent — **never used** by AIExpedite |
| Pre-chosen IDs | Caller-supplied conversation IDs are ignored; agy mints UUIDs |
| Native ID capture | After first turn: `~/.gemini/antigravity-cli/cache/last_conversations.json[cwd]` (verify `.db` exists); fallback: new `conversations/*.db` |
| Streaming | Complete-only stdout (no reliable incremental stream) |
| Local storage | Per-conversation SQLite under `~/.gemini/antigravity-cli/conversations/` |
| Prompt size | Fail closed above 24KB (under CreateProcess ~32KB argv budget) |
| Replay trigger | Only recognized missing/stale conversation text — not generic non-zero exits |

Minimum supported version for native chat: **1.1.1**.

## Protocol

1. **Start** — register logical session on the device (no process). Publish `antigravity_native_started`.
2. **Send** — spawn `agy --dangerously-skip-permissions --print <prompt> [--conversation <id>]`, wait for exit, publish `antigravity_native_message` (complete response). Capture native ID after first turn.
3. **End** — kill in-flight process if any; publish `antigravity_native_ended`.

Between turns there is **no resident process**.

## Resume vs replay

- Prefer exact `--conversation <nativeId>` for follow-ups.
- If resume fails (missing conversation / recognized error), run **one** bounded transcript replay turn (new conversation), replace native ID, emit `[[antigravity_replay_recovery]]` marker for UI notice + telemetry.
- Never use `--continue`.

## Permissions threat model (`--dangerously-skip-permissions`)

Remote AIExpedite users cannot answer local interactive permission prompts. Without auto-approve the one-shot process hangs.

Safeguards for the native-chat path:

1. Per-device **Enable Antigravity chat sessions** opt-in (`chatCliAgents.antigravity.enabled`)
2. Workspace membership + device ownership checks (terminal-service)
3. Parent orchestration / terminal access approval layer
4. Working-directory scope on Start
5. Process registry + orphan cleanup limited to AIExpedite-owned PIDs
6. Telemetry redaction (no prompts, tokens, OAuth URLs)
7. Unrelated provider credentials stripped from child env

Legacy one-shot `buildAntigravityInteractiveArgs` (session_start / PTY execute)
uses the **same** `--dangerously-skip-permissions --print <prompt>` contract —
not a trailing positional. An older order (`--print` then the permission flag)
made agy 1.1.x treat `--dangerously-skip-permissions` as the prompt (tertiary
Review kickoff symptom).

## Recovery semantics

| Event | Behavior |
|---|---|
| Frontend refresh | Restore logical session from durable `terminalSession` + chat history; native ID lives on the device manager until End/expiry |
| Backend restart | Session docs in Firestore remain; device manager loses in-memory native IDs — next Send uses replay if ID missing |
| Workstation reconnect | Re-validate eligibility; same device only (no auto-redirect) |
| CLI process exit between turns | Normal |
| Stale session (6h) | Manager reaps; chat history preserved; next message needs a new Antigravity conversation |

## Troubleshooting

| Symptom | Action |
|---|---|
| No selector entry | Enable chat sessions in Terminal Settings; ensure device online + agy ≥ 1.1.1 |
| Auth required | Authenticate `agy` locally on the workstation (never paste tokens into chat) |
| Resume failed notice | Prior context may be truncated; continue or start a new chat |
| Stuck running | Stop/cancel; timeout kills the process and clears running state |

## Telemetry

Structured fields: provider=`antigravity`, device id, logical session id, turn latency, exit/error category, resume vs replay, cancel. Prompt/response content and secrets are redacted.
