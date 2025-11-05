# AI Expedite Terminal

A professional Windows system tray application that provides remote terminal access via ttyd and Google Cloud Pub/Sub. Features automatic installation, a modern installer, and comprehensive debugging tools.

## Features

- **🎨 Custom Branding**: Professional AI Expedite logo with transparent background
- **📦 Windows Installer**: Professional Inno Setup installer that registers in Add/Remove Programs
- **🔄 Auto-start on Login**: Automatically starts when you log in to Windows (configurable during installation)
- **🖥️ Console Toggle**: Show/hide console window for debugging and monitoring via system tray menu
- **🔧 Automatic ttyd Installation**: Automatically installs `ttyd` via winget or scoop if not found
- **🌐 Web-Based Terminal**: Provides browser-based terminal access via ttyd on localhost:7681
- **☁️ Pub/Sub Integration**: Receives and executes terminal commands via Google Cloud Pub/Sub (optional)
- **🔐 System Tray Integration**: Runs silently in the background with easy access from system tray

## Installation

### For End Users

1. Download `AIExpediteTerminal-Setup-0.2.0.exe` from the releases
2. Run the installer
3. Choose your installation options:
   - Installation directory (default: `C:\Program Files\AI Expedite Terminal`)
   - Create desktop shortcut (optional)
   - Run at Windows startup (checked by default)
4. Click Install and follow the wizard

The application will:
- Install to Program Files
- Create Start Menu shortcuts
- Register in Windows Add/Remove Programs
- Automatically install ttyd if not present
- Start the terminal server on port 7681

### Uninstallation

Uninstall via:
- **Windows Settings** → Apps → AI Expedite Terminal
- **Control Panel** → Programs and Features
- **Start Menu** → AI Expedite Terminal → Uninstall

## Usage

### System Tray Menu

Right-click the AI Expedite logo in the system tray for options:

- **Open Terminal**: Opens the web-based terminal in your default browser (http://127.0.0.1:7681)
- **Check for Updates**: Manually check for application updates (when enabled)
- **☑ Show Console**: Toggle console window visibility for debugging and monitoring
- **Quit**: Exit the application

### Console Window

The "Show Console" feature allows you to view application logs in real-time:

```
Warning: tmux not found on Windows (requires WSL or MSYS2) - running without tmux.
Found ttyd at: C:\Users\...\AppData\Local\Microsoft\WinGet\Packages\...\ttyd.exe
ttyd listening on 127.0.0.1: 7681
[pubsub] disabled – project_id empty
```

This is useful for:
- Monitoring ttyd status
- Debugging connection issues
- Viewing startup messages
- Checking configuration problems

## Configuration

Configuration file location: `%APPDATA%\TrayAgent\config.json`

Default configuration:
```json
{
    "project_id": "",
    "commands_subscription": "terminal-commands-sub",
    "results_topic": "terminal-results",
    "local_ttyd_port": 7681,
    "auto_update": false
}
```

### Configuration Options

- **project_id**: Google Cloud Project ID for Pub/Sub integration (leave empty to disable)
- **commands_subscription**: Pub/Sub subscription name for receiving commands
- **results_topic**: Pub/Sub topic name for sending command results
- **local_ttyd_port**: Port for ttyd web server (default: 7681)
- **auto_update**: Enable automatic update checks (default: false)

### Enabling Auto-Update

To enable automatic updates:
1. Stop the application (right-click tray icon → Quit)
2. Edit `%APPDATA%\TrayAgent\config.json`
3. Set `"auto_update": true`
4. Restart the application

**Note**: Auto-update requires a GitHub release with the appropriate binary assets.

## Building from Source

### Prerequisites

- **Go 1.24+**
- **Windows 7 or later**
- **go-winres** (installed automatically by build.bat)

### Quick Build

Build the executable:

```batch
build.bat
```

This will:
1. Install go-winres if needed
2. Generate Windows resource files (.syso) with the AI Expedite icon
3. Build `aiexpedite-terminal.exe` with embedded icon and version information

Output: `aiexpedite-terminal.exe`

### Creating the Installer

#### Prerequisites

1. Install Inno Setup from: https://jrsoftware.org/isdl.php
2. During installation, select "Add to PATH"

#### Build Installer

```batch
build-installer.bat
```

Or manually:
```batch
"C:\Program Files (x86)\Inno Setup 6\ISCC.exe" installer.iss
```

Output: `installer-output\AIExpediteTerminal-Setup-0.2.0.exe`

### Icon and Branding

The application uses custom AI Expedite branding:

**System Tray Icon**: `assets/aiexpedite-icon.ico` (black logo, transparent background)
**Windows Icon**: `assets/aiexpedite-logo-*.png` (multiple resolutions: 16x16 to 256x256)

#### Customizing the Icon

1. Replace logo files in `assets/` directory
2. Run `npm run resize-icon` to create multiple sizes (requires Node.js and sharp)
3. Run `build.bat` to rebuild with new icon
4. Run `build-installer.bat` to create new installer

### Installer Configuration

Edit `installer.iss` to customize:
- Application name and version
- Publisher information
- Installation directory
- Startup options
- File associations

See `INSTALLER-README.md` for detailed documentation.

## Troubleshooting

### ttyd Not Found

If you see "Fatal: ttyd not found", install it manually:

**Option 1 - winget (recommended):**
```powershell
winget install tsl0922.ttyd
```

**Option 2 - scoop:**
```powershell
scoop install ttyd
```

After installation, restart the application.

### Terminal Not Loading

1. **Check if ttyd is running**: Right-click tray icon → Show Console
2. **Verify port 7681 is listening**: `netstat -ano | findstr :7681`
3. **Check for conflicts**: Another application might be using port 7681
4. **Change port**: Edit `%APPDATA%\TrayAgent\config.json` and set a different port

### tmux Warning on Windows

The warning "Warning: tmux not found on Windows - running without tmux" is **normal and can be ignored**. The application will use PowerShell directly on Windows. tmux is only used on Linux/macOS systems.

### Auto-start Not Working

If the application doesn't start automatically:

1. Check Windows Startup apps: **Settings → Apps → Startup**
2. Look for "AI Expedite Terminal" in the list
3. Ensure it's enabled
4. Alternatively, check registry: `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

### Console Window Appears at Startup

The application runs in the background without a console by default. If a console appears at startup, it will automatically hide after a few seconds. You can toggle it manually via the system tray menu.

### GitHub API 404 Error

This error appears if auto-update is enabled but no GitHub releases exist yet. To disable:

1. Stop the application (right-click tray icon → Quit)
2. Edit `%APPDATA%\TrayAgent\config.json`
3. Set `"auto_update": false`
4. Restart the application

Alternatively, the error is harmless and only appears once at startup.

### Unknown Publisher Warning

When running the installer, Windows may show an "Unknown Publisher" warning. This is normal for unsigned applications. To remove this warning:

1. Purchase a code signing certificate from a trusted Certificate Authority (Comodo, DigiCert, etc.)
2. Sign both the executable and installer with the certificate
3. For immediate trust, use an Extended Validation (EV) certificate

To bypass the warning:
1. Click "More info" on the SmartScreen warning
2. Click "Run anyway"

## Development

### Project Structure

```
aiexpedite-local-terminal/
├── main.go                 # Entry point, system tray setup
├── agent.go                # Background workers (ttyd, pub/sub, updates)
├── config.go               # Configuration management
├── ttyd.go                 # ttyd installation and management
├── tmux.go                 # tmux session management (Linux/macOS)
├── pubsub.go               # Google Cloud Pub/Sub integration
├── update.go               # Auto-update functionality
├── paths.go                # Path utilities
├── tray_windows.go         # Windows-specific tray functionality
├── tray_linux.go           # Linux-specific tray functionality
├── tray_darwin.go          # macOS-specific tray functionality
├── build.bat               # Build script
├── installer.iss           # Inno Setup installer script
├── build-installer.bat     # Installer build script
├── winres/
│   └── winres.json         # Windows resource configuration
├── assets/
│   ├── aiexpedite-icon.ico # System tray icon
│   └── aiexpedite-logo-*.png # Application icons (multiple sizes)
└── installer-output/
    └── AIExpediteTerminal-Setup-0.2.0.exe
```

### Adding Features

1. **Update the code** in relevant `.go` files
2. **Rebuild**: Run `build.bat`
3. **Test**: Run `aiexpedite-terminal.exe` manually with console visible
4. **Create installer**: Run `build-installer.bat`
5. **Test installer**: Run the installer and verify all features work

## Version History

### v0.2.0 (Current)
- ✨ Added custom AI Expedite branding with professional icon
- 📦 Created Windows installer with Inno Setup
- 🖥️ Added console toggle feature for debugging
- 🔧 Improved ttyd auto-installation
- 🔄 Auto-start configuration (checked by default in installer)
- 🐛 Disabled auto-update by default to prevent GitHub 404 errors
- 📝 Comprehensive documentation and troubleshooting

### v0.1.0
- Initial release with basic ttyd integration
- Pub/Sub support for remote command execution
- Basic system tray functionality

## Support

For issues, questions, or feature requests:
- Check the troubleshooting section above
- Use the "Show Console" feature to view diagnostic information
- Contact AI Expedite support

## License

Copyright (c) AI Expedite. All rights reserved.
