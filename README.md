# aiexpedite-local-terminal

`aiexpedite-local-terminal` is a lightweight helper tool that bridges AI Expedite’s cloud services with your local machine’s terminal. With a simple install, you can securely connect your local shell to AI Expedite for real‑time command execution, logging, and workflow automation.

---

## Key Features

-  **Secure WebSocket Bridge**: Connects your local terminal over a private WebSocket, authenticated via JWT.
-  **Cross‑Platform Binaries**: Precompiled helper executables for macOS, Windows, and Linux.
-  **Zero Configuration**: No complex setup—download the binary, run it, and you’re connected.
-  **Policy‑Driven Access**: Integrates with Open Policy Agent (OPA) for fine‑grained access control.
-  **Persistent Sessions**: Automatic reconnects and session persistence ensure uninterrupted workflows.

---

## How It Works

1. **Launch the Helper**: Run the `ai-expedite-terminal-service` binary on your local machine. It starts:
   -  A WebSocket server on port `3080` for terminal I/O.
   -  An HTTP endpoint on port `3090` for JWT-based reauthentication.
2. **Authenticate**: The helper reads your AI Expedite JWT and verifies permissions via an embedded OPA policy.
3. **Open WebSocket**: The React UI in AI Expedite’s web app (via `TerminalComponent`) opens a WebSocket to `ws://localhost:3080/api/terminal`, sending user keystrokes and rendering remote output in your browser.
4. **PTY Proxy**: The helper spawns a local PTY (bash or PowerShell) and proxies data back and forth, letting you run shell commands as if you were logged into a remote container.

---

## Prerequisites

-  Docker (if you prefer containerized installation)
-  `/usr/local/bin` (or equivalent) in your `PATH`
-  A valid AI Expedite JWT (the helper will prompt you to POST it to `/token`)

---

## Installation

### macOS / Linux (Standalone)

1. Download the latest binary for your platform:
   ```bash
   curl -L \
     https://github.com/AIExpedite/aiexpedite-local-terminal/releases/latest/download/ai-expedite-terminal-service-macos \
     -o ai-expedite-terminal-service && chmod +x ai-expedite-terminal-service
   ```
2. Move it into your `PATH`:
   ```bash
   mv ai-expedite-terminal-service /usr/local/bin/
   ```

### Windows (PowerShell)

```powershell
Invoke-WebRequest -Uri \
  https://github.com/AIExpedite/aiexpedite-local-terminal/releases/latest/download/ai-expedite-terminal-service-win.exe \
  -OutFile ai-expedite-terminal-service.exe
```

Optionally add to your PATH by moving the EXE to a folder in your `%PATH%`.

---

## Usage

1. **Start the helper**:

   ```bash
   ai-expedite-terminal-service
   ```

   It will block until you POST your JWT to the HTTP endpoint.

2. **Authenticate** (in a separate shell):

   ```bash
   curl -X POST --data-binary @~/.aiexpedite/jwt.txt http://localhost:3090/token
   ```

3. **Access in Browser**: In AI Expedite’s web UI, navigate to the Test Step. The embedded terminal will auto‐connect and display your local shell.

---

## Configuration

All paths and ports are configurable via environment variables:

-  `PORT_TERMINAL` (default: 3080)
-  `PORT_TOKEN` (default: 3090)
-  `HELPER_VERBOSE` (set to `1` to enable debug logging)

Example:

```bash
export PORT_TERMINAL=4000 PORT_TOKEN=4001 HELPER_VERBOSE=1
ai-expedite-terminal-service
```

---

## Troubleshooting

-  **404 on Download**: Ensure you’ve published a GitHub release with the matching platform asset name.
-  **WebSocket Error**: Check that the helper is running and listening on the expected port.
-  **Permission Denied**: Confirm your JWT has a valid `workspaceID` and `userID` and that the embedded OPA policy allows your session.

---

## License

This project is released under the [MIT License](LICENSE).
