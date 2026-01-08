; AI Expedite Terminal (Beta) - Inno Setup Installer Script
; This script creates a Windows installer for AI Expedite Terminal (Beta environment)

#define MyAppName "AI Expedite Terminal (Beta)"
#define MyAppVersion "0.2.5"
#define MyAppVersionString "0.2.5"
#define MyAppPublisher "AI Expedite"
#define MyAppURL "https://beta.aiexpedite.com"
#define MyAppExeName "aiexpedite-terminal-beta.exe"
#define MyAppDescription "AI Expedite Terminal for remote terminal access (Beta environment)"
#define MyEnvSuffix "-Beta"

[Setup]
; NOTE: The value of AppId uniquely identifies this application.
; Do not use the same AppId value in installers for other applications.
; Each environment has a unique AppId so they can be installed side-by-side.
AppId={{B8E4A9C1-7D3F-4E2B-9A1C-8F6E5D4CBETA}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}
AppUpdatesURL={#MyAppURL}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
AllowNoIcons=yes
LicenseFile=
InfoBeforeFile=
InfoAfterFile=
OutputDir=installer-output
OutputBaseFilename=AIExpediteTerminal-Beta-Setup-{#MyAppVersionString}
SetupIconFile=assets\aiexpedite-icon.ico
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
PrivilegesRequired=admin
ArchitecturesAllowed=x64
ArchitecturesInstallIn64BitMode=x64
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
VersionInfoVersion={#MyAppVersion}
VersionInfoCompany={#MyAppPublisher}
VersionInfoDescription={#MyAppDescription}
VersionInfoProductName={#MyAppName}
VersionInfoProductVersion={#MyAppVersion}

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked
Name: "quicklaunchicon"; Description: "{cm:CreateQuickLaunchIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked; OnlyBelowVersion: 6.1; Check: not IsAdminInstallMode
Name: "startup"; Description: "Run {#MyAppName} at Windows startup"; GroupDescription: "Startup Options:"

[Files]
Source: "{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion
Source: "assets\aiexpedite-logo.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-256.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-128.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-64.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-48.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-32.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\aiexpedite-logo-16.png"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "README.md"; DestDir: "{app}"; Flags: ignoreversion isreadme

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{group}\{cm:UninstallProgram,{#MyAppName}}"; Filename: "{uninstallexe}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon
Name: "{userappdata}\Microsoft\Internet Explorer\Quick Launch\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: quicklaunchicon

[Registry]
; Run at startup (optional - only if user selects the task)
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "AIExpedite{#MyEnvSuffix}"; ValueData: """{app}\{#MyAppExeName}"""; Flags: uninsdeletevalue; Tasks: startup

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
; Stop the application before uninstalling
Filename: "taskkill"; Parameters: "/F /IM {#MyAppExeName}"; Flags: runhidden; RunOnceId: "StopApp"

[Code]
// Check if application is running before installation
function InitializeSetup(): Boolean;
var
  ResultCode: Integer;
begin
  // Try to gracefully stop the application if it's running
  Exec('taskkill', '/IM {#MyAppExeName} /F', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Result := True;
end;

// Check if application is running before uninstall
function InitializeUninstall(): Boolean;
var
  ResultCode: Integer;
begin
  // Try to gracefully stop the application if it's running
  Exec('taskkill', '/IM {#MyAppExeName} /F', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Result := True;
end;
