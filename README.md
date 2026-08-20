# AI Expedite Terminal

AI Expedite Terminal is the small companion app that lets AI Expedite do work on
**your** computer — running your build, your tests, your git commands, and your
installed coding CLIs — instead of on a rented machine in the cloud.

It lives in your system tray (Windows) or menu bar (macOS). Once you pair it
with your AI Expedite account, tasks you start in the AI Expedite web app can be
carried out here, on the machine where your code, your tools and your logins
already are.

**You stay in control.** The app only accepts work from your own account, only
runs commands that match an allow list you can see and edit, and asks you before
running anything outside it.

---

## Contents

- [How it works](#how-it-works)
- [Installing](#installing)
- [Connecting the app to your account](#connecting-the-app-to-your-account)
- [The tray menu, item by item](#the-tray-menu-item-by-item)
- [The allow list](#the-allow-list)
- [Security](#security)
- [Files the app creates](#files-the-app-creates)
- [Updates](#updates)
- [Troubleshooting](#troubleshooting)
- [Uninstalling](#uninstalling)
- [Support](#support)

---

## How it works

1. You start a task in the AI Expedite web app and choose one of your registered
   machines.
2. That machine's copy of AI Expedite Terminal receives the task, checks it, and
   runs it in a real terminal session on your computer.
3. Output streams back to the web app as it happens, so you can watch the run,
   answer questions, and stop it at any time.
4. When a task produces files that help the AI understand the result —
   screenshots, test videos, test reports — those files are sent along with the
   output so they can be shown to you and reviewed by the AI.

Everything runs under your own user account, in your own working directories,
with your own tool installations and logins. If you have Claude Code, Codex,
Grok Build or Antigravity installed and signed in, AI Expedite can drive them
here using the credentials and plan you already pay for.

The app makes **outbound** connections only. It does not open a port for other
people to connect to, and nothing on your network can reach it from outside.

---

## Installing

### Windows

1. Download the AI Expedite Terminal installer from your AI Expedite account.
2. Run it and follow the wizard. You can choose whether the app starts
   automatically when you log in (recommended — otherwise tasks can't reach the
   machine until you start it manually).
3. The app installs for your user account only and does not require
   administrator rights.

The Windows app and installer are signed with an Extended Validation code
signing certificate, so Windows shows AI Expedite as the verified publisher
rather than an "unknown publisher" warning.

### macOS

1. Download the AI Expedite Terminal disk image (`.dmg`) for your Mac — Apple
   Silicon or Intel — from your AI Expedite account.
2. Open it and drag AI Expedite Terminal to your Applications folder.
3. Launch it from Applications. The app appears in the menu bar.

The macOS app is signed with an Apple Developer ID certificate and notarized by
Apple, so it opens without Gatekeeper warnings.

### After installing

The app runs quietly in the background. Look for the AI Expedite icon in your
system tray (Windows, bottom-right, possibly behind the "^" overflow arrow) or
menu bar (macOS, top-right).

---

## Connecting the app to your account

The first time it starts, the app walks you through pairing:

1. It shows a short **pairing code**.
2. Your browser opens to AI Expedite.
3. Sign in and enter the code to authorize this device.

Once you approve it, the machine appears in AI Expedite as one of your devices
and can start receiving work. The pairing code expires after a few minutes; if
it does, use **Register Device** from the tray menu to start again.

You can revoke a device at any time from the AI Expedite web app. The app on
that machine immediately stops receiving work and returns to the unregistered
state.

---

## The tray menu, item by item

Right-click (Windows) or click (macOS) the AI Expedite icon to open the menu.
Hovering over the icon shows the current status: connected, disconnected, or
waiting to be registered.

### Show Console

Opens a window showing what the app is doing right now — the tasks it receives,
the commands it runs, and any errors. It's read-only diagnostic output; nothing
you type in it is executed. Turn it on when you want to see what's happening or
when support asks for details, and turn it off when you're done. (Windows only.)

The same output is also written to a log file on disk — see
[Files the app creates](#files-the-app-creates).

### Register Device

Pairs this machine with your AI Expedite account, using the pairing flow
described above.

The item shows a checkmark and is greyed out once the machine is registered —
that's the normal, healthy state, and it means there's nothing to do here.
Clicking it then just shows you which account and device this machine is
registered as. If the device is removed from your account on the website, the
item becomes clickable again so you can re-pair.

### Disconnect from cloud

Temporarily stops this machine from receiving work, without quitting the app.
The device shows as offline in AI Expedite and no new tasks are sent to it.

Use this when you want the machine to yourself for a while — during a
presentation, on a metered connection, or while you're debugging something and
don't want anything else touching the working directory.

Click it again to reconnect. The setting survives a restart, so if you leave it
disconnected it stays disconnected until you turn it back on.

### Edit Allow List

Opens the folder containing `allowed-commands.txt`, the file that decides which
commands can run without asking you first. See
[The allow list](#the-allow-list) below.

### Reset Allow List

Replaces your `allowed-commands.txt` with the shipped defaults. You'll be asked
to confirm first, because any patterns you added yourself are discarded. Useful
if you've edited the file into a state you're not sure about, or after an
update adds support for new tools.

### Allow All Commands

Turns the safety gate **off**: every command sent to this machine runs
immediately, with no allow-list check and no approval prompt.

This is deliberately hard to enable by accident — it asks you to confirm, and
the change is recorded in the security log. Leave it off unless you have a
specific reason and fully trust the machine. Turning it back off takes effect
immediately.

### Automatically update

When checked (the default), the app keeps itself up to date in the background.
It checks shortly after launch and periodically after that, and installs new
versions on its own.

Updates never interrupt work: if a task is running when an update is ready, the
app waits for it to finish. While it's waiting, the menu shows
**"Updating after current work"** as a status line.

On the rare installation that can't replace itself — for example, an app copied
somewhere read-only — the menu shows **"Automatic update unavailable"** with
the reason, and you can update by downloading the latest version manually.

Uncheck the item if you'd rather decide when to update.

### Check for Updates

Checks immediately, whatever the automatic setting is. If a new version exists
you can install it now, be reminded later, or skip that version. If you choose
"later", an **Install Update (version)** item appears in the menu so you can
install it whenever it suits you.

If you're already current, it tells you so.

### Version

Shows the version you're running. It's greyed out because it's information, not
a button — quote it when contacting support.

### Quit

Shuts the app down completely. The device goes offline in AI Expedite and stops
receiving work until you start the app again. Any commands the app started are
cleaned up as it exits, so nothing is left running behind your back.

---

## The allow list

Every command that arrives from the cloud is checked against a plain-text file
on your machine before anything runs:

- **Windows:** `%APPDATA%\AIExpedite\allowed-commands.txt`
- **macOS:** `~/Library/Application Support/AIExpedite/allowed-commands.txt`

Use **Edit Allow List** in the tray menu to open that folder, then edit the file
in any text editor.

### What's in it

The file ships with sensible defaults for normal development work — `git`, `gh`,
`node`/`npm`/`yarn`/`pnpm`, Python and its tooling, build tools like `make`,
`cargo` and `go`, Docker and `kubectl`, cloud CLIs, test runners, linters, and
common shell utilities. It's grouped into commented sections so it's easy to
read.

### The format

- One pattern per line.
- Lines starting with `#` are comments and are ignored.
- `*` matches anything on the rest of the line.
- Matching is case-insensitive and must cover the **whole** command, so `git *`
  allows `git status` but not `sudo git status`.

```text
# Allow every git command
git
git *

# Allow only two npm scripts
npm run build
npm run test

# Allow one specific tool
terraform plan *
```

To **remove** something, delete its line (or comment it out with `#`). To
**add** something, put it on its own line. Changes take effect when the app next
loads the file, so quit and restart the app — or use the approval prompt below,
which applies immediately.

### What happens when a command isn't on the list

The app doesn't just fail — it asks you, with a dialog showing the exact command
and where it would run:

- **Yes** — run it this once.
- **No** — always allow this command from now on. The pattern is appended to
  `allowed-commands.txt` (marked `# Added by user approval`) and takes effect
  right away.
- **Cancel** — deny it. The task is told the command was refused.

If you don't answer within 60 seconds the dialog closes and the command is
**denied**. Denials are recorded in the security log.

Some commands always prompt even if a pattern would allow them — installs and
other destructive steps are marked as risky by AI Expedite before they're sent,
and that marking can't be stripped in transit, so you get the final say.

---

## Security

The app is built on the assumption that the network is hostile and that you
should be able to verify what it does.

**Only your account can send work here.** Pairing gives this machine its own
credentials. Every incoming task carries a signature computed with a secret that
only this device and AI Expedite know, and anything that doesn't verify is
dropped before it's even read as a command. A device removed from your account
stops working immediately.

**Nothing accepts inbound connections.** The app dials out to AI Expedite; there
is no port for anyone else to reach, and no listening service exposed to your
network.

**Commands are gated before they run.** The allow list is checked on your
machine, by the app, using a file you own and can read. Anything unmatched
requires your explicit approval, and the timeout answer is "deny".

**Overrides are loud, not silent.** Turning on "Allow All Commands" requires a
confirmation dialog and is written to the security log, along with when normal
protection was restored.

**There's an audit trail.** Denied commands, approval decisions, allow-list
overrides and update verification results are all appended to `security.log` as
structured JSON, so you can search it later.

**Updates are verified before they're installed.** Every release is signed —
EV code signing on Windows, Developer ID plus Apple notarization on macOS — and
the app additionally verifies the build provenance of the downloaded binary
before replacing itself. A binary that fails verification is refused, and the
refusal is logged.

**Your credentials stay yours.** The app never asks for, stores, or transmits
your logins for GitHub, your cloud providers, or your coding CLIs. It runs those
tools as you, using the sessions already on your machine. Its own config and log
files are written with owner-only permissions.

**File access stays where it belongs.** Attempts to read or write outside the
directory a task is scoped to are blocked and logged.

**It doesn't leave things running.** Processes started for a task are tracked and
cleaned up when the task ends or the app quits.

---

## Files the app creates

Everything lives in one folder:

- **Windows:** `%APPDATA%\AIExpedite\`
- **macOS:** `~/Library/Application Support/AIExpedite/`

| File | What it is |
| --- | --- |
| `config.json` | Your settings and this device's credentials. Don't share it. |
| `allowed-commands.txt` | The allow list described above. |
| `logs/agent.log` | What the app has been doing. Rotated automatically and capped in size. |
| `security.log` | Security events: denials, approvals, overrides, update verification. |

Deleting this folder resets the app to a clean, unregistered state.

---

## Updates

New versions are installed automatically by default, and only when the machine
is idle — a run in progress is always allowed to finish first. See
[Automatically update](#automatically-update) and
[Check for Updates](#check-for-updates) for the controls.

The app can update itself in place, so there's normally nothing for you to
download and no reinstall to run.

---

## Troubleshooting

**The device shows as offline in AI Expedite.**
Check that the app is running (tray icon present) and that **Disconnect from
cloud** is unchecked. If the icon is missing, start AI Expedite Terminal from
the Start menu or Applications folder.

**The device never appears in AI Expedite.**
It probably isn't paired yet. Open the tray menu and choose **Register Device**.
If the item is greyed out and checked, the machine *is* paired — check you're
looking at the right account in the web app.

**A task says a command was denied.**
The command didn't match the allow list and either you declined the prompt or it
timed out. Add a pattern for it via **Edit Allow List**, or re-run and choose
"No" (always allow) at the prompt.

**A tool isn't found when a task runs it.**
The app uses your normal environment, so the tool needs to be installed and on
your `PATH` for your user account. Install it, then restart AI Expedite Terminal
so it picks up the updated environment.

**A coding CLI asks to sign in.**
Sign in to that CLI yourself in a normal terminal (for example `claude`,
`codex`, or `grok`, following that tool's own instructions). AI Expedite uses
the session you create — it never handles those credentials itself.

**Something looks wrong and you want detail.**
Turn on **Show Console** and reproduce the problem, or send support the contents
of `logs/agent.log`.

**The app didn't start after a reboot.**
On Windows, check **Settings → Apps → Startup** and make sure AI Expedite is
enabled. On macOS, check **System Settings → General → Login Items**.

---

## Uninstalling

**Windows:** Settings → Apps → AI Expedite → Uninstall (or Control Panel →
Programs and Features).

**macOS:** Quit the app from the menu bar, then drag AI Expedite Terminal from
Applications to the Trash.

To remove local data as well, delete the folder listed in
[Files the app creates](#files-the-app-creates). It's also worth removing the
device from your AI Expedite account in the web app so it no longer appears in
your device list.

---

## Support

- Turn on **Show Console** to see live diagnostics.
- Note the version shown in the tray menu.
- Contact AI Expedite support with that information and the relevant portion of
  `logs/agent.log`.

---

## For developers

Build, packaging, signing and protocol documentation lives alongside the source:
`INSTALLER-README.md`, `WINDOWS_CODE_SIGNING.md`, `MACOS_CODE_SIGNING.md` and
`CLI_AGENT_INTEGRATION.md`.

## License

Copyright (c) AI Expedite. All rights reserved. Source-available; see `LICENSE`.
