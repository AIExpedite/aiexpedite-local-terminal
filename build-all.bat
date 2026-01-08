@echo off
setlocal enabledelayedexpansion

REM ============================================================================
REM AI Expedite Terminal - Multi-Environment Build Script
REM Builds Windows executable and installer with auto version increment
REM
REM Usage: build-all.bat [environment]
REM   environment: dev, stg, beta, prod (default: prod)
REM
REM NOTE: Linux and macOS builds require native compilation (CGO/systray)
REM       Use GitHub Actions workflow for cross-platform releases
REM ============================================================================

echo.
echo ============================================================================
echo    AI Expedite Terminal - Multi-Environment Build Script
echo ============================================================================
echo.

REM ----------------------------------------------------------------------------
REM Parse environment argument (default: prod)
REM ----------------------------------------------------------------------------
set "ENV=%~1"
if "%ENV%"=="" set "ENV=prod"

REM Validate environment
if /i not "%ENV%"=="dev" if /i not "%ENV%"=="stg" if /i not "%ENV%"=="beta" if /i not "%ENV%"=="prod" (
    echo ERROR: Invalid environment "%ENV%"
    echo        Valid options: dev, stg, beta, prod
    exit /b 1
)

REM ----------------------------------------------------------------------------
REM Set environment-specific variables
REM ----------------------------------------------------------------------------
if /i "%ENV%"=="dev" (
    set "DISPLAY_NAME=AI Expedite Terminal (Dev)"
    set "API_ENDPOINT=https://api.dev.aiexpedite.com/terminal"
    set "CONFIG_SUFFIX=-Dev"
    set "INSTALLER_ISS=installer-dev.iss"
    set "EXE_NAME=aiexpedite-terminal-dev.exe"
    set "INSTALLER_PREFIX=AIExpediteTerminal-Dev-Setup"
)
if /i "%ENV%"=="stg" (
    set "DISPLAY_NAME=AI Expedite Terminal (Stg)"
    set "API_ENDPOINT=https://api.stg.aiexpedite.com/terminal"
    set "CONFIG_SUFFIX=-Stg"
    set "INSTALLER_ISS=installer-stg.iss"
    set "EXE_NAME=aiexpedite-terminal-stg.exe"
    set "INSTALLER_PREFIX=AIExpediteTerminal-Stg-Setup"
)
if /i "%ENV%"=="beta" (
    set "DISPLAY_NAME=AI Expedite Terminal (Beta)"
    set "API_ENDPOINT=https://api.beta.aiexpedite.com/terminal"
    set "CONFIG_SUFFIX=-Beta"
    set "INSTALLER_ISS=installer-beta.iss"
    set "EXE_NAME=aiexpedite-terminal-beta.exe"
    set "INSTALLER_PREFIX=AIExpediteTerminal-Beta-Setup"
)
if /i "%ENV%"=="prod" (
    set "DISPLAY_NAME=AI Expedite Terminal"
    set "API_ENDPOINT=https://api.aiexpedite.com/terminal"
    set "CONFIG_SUFFIX="
    set "INSTALLER_ISS=installer-prod.iss"
    set "EXE_NAME=aiexpedite-terminal.exe"
    set "INSTALLER_PREFIX=AIExpediteTerminal-Setup"
)

echo Building for environment: %ENV%
echo   Display Name:   %DISPLAY_NAME%
echo   API Endpoint:   %API_ENDPOINT%
echo   Config Suffix:  %CONFIG_SUFFIX%
echo   Executable:     %EXE_NAME%
echo   Installer:      %INSTALLER_ISS%
echo.

REM ----------------------------------------------------------------------------
REM Step 1: Extract current version from agent.go
REM ----------------------------------------------------------------------------
echo [1/7] Reading current version from agent.go...

REM Extract version using findstr and string manipulation
for /f "tokens=*" %%a in ('findstr /C:"const version = " agent.go') do (
    set "VERSION_LINE=%%a"
)

REM Parse out the version number (e.g., from 'const version = "v0.2.0"')
REM Use PowerShell to extract just the version digits
for /f "delims=" %%v in ('powershell -NoProfile -Command "$line = '%VERSION_LINE%'; if ($line -match 'v(\d+\.\d+\.\d+)') { $matches[1] }"') do (
    set "CURRENT_VERSION=%%v"
)

if "%CURRENT_VERSION%"=="" (
    echo ERROR: Could not read version from agent.go
    echo        VERSION_LINE was: %VERSION_LINE%
    exit /b 1
)

echo       Current version: %CURRENT_VERSION%

REM ----------------------------------------------------------------------------
REM Step 2: Auto-increment patch version
REM ----------------------------------------------------------------------------
echo [2/7] Incrementing patch version...

for /f "tokens=1,2,3 delims=." %%a in ("%CURRENT_VERSION%") do (
    set "MAJOR=%%a"
    set "MINOR=%%b"
    set "PATCH=%%c"
)

set /a "NEW_PATCH=%PATCH%+1"
set "NEW_VERSION=%MAJOR%.%MINOR%.%NEW_PATCH%"

echo       New version: %NEW_VERSION%

REM ----------------------------------------------------------------------------
REM Step 3: Update version in all files
REM ----------------------------------------------------------------------------
echo [3/7] Updating version in source files...

REM Update agent.go (preserve original encoding - no BOM)
powershell -NoProfile -Command "$f='agent.go'; $c=[IO.File]::ReadAllText($f); $c=$c -replace 'const version = \"v[0-9]+\.[0-9]+\.[0-9]+\"', 'const version = \"v%NEW_VERSION%\"'; [IO.File]::WriteAllText($f,$c)"
if errorlevel 1 (
    echo ERROR: Failed to update agent.go
    exit /b 1
)
echo       - agent.go updated

REM Update all installer .iss files (MyAppVersion and MyAppVersionString)
for %%f in (installer-dev.iss installer-stg.iss installer-beta.iss installer-prod.iss) do (
    if exist "%%f" (
        powershell -NoProfile -Command "$f='%%f'; $c=[IO.File]::ReadAllText($f); $c=$c -replace '#define MyAppVersion \"[0-9]+\.[0-9]+\.[0-9]+\"', '#define MyAppVersion \"%NEW_VERSION%\"'; $c=$c -replace '#define MyAppVersionString \"[0-9]+\.[0-9]+\.[0-9]+-?[a-z]*\"', '#define MyAppVersionString \"%NEW_VERSION%\"'; [IO.File]::WriteAllText($f,$c)"
        echo       - %%f updated
    )
)

REM Update winres/winres.json (multiple version fields)
powershell -NoProfile -Command "$f='winres/winres.json'; $c=[IO.File]::ReadAllText($f); $c=$c -replace '\"version\": \"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\"', '\"version\": \"%NEW_VERSION%.0\"'; $c=$c -replace '\"file_version\": \"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\"', '\"file_version\": \"%NEW_VERSION%.0\"'; $c=$c -replace '\"product_version\": \"[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\"', '\"product_version\": \"%NEW_VERSION%.0\"'; $c=$c -replace '\"FileVersion\": \"[0-9]+\.[0-9]+\.[0-9]+\"', '\"FileVersion\": \"%NEW_VERSION%\"'; $c=$c -replace '\"ProductVersion\": \"[0-9]+\.[0-9]+\.[0-9]+\"', '\"ProductVersion\": \"%NEW_VERSION%\"'; [IO.File]::WriteAllText($f,$c)"
if errorlevel 1 (
    echo ERROR: Failed to update winres/winres.json
    exit /b 1
)
echo       - winres/winres.json updated

REM ----------------------------------------------------------------------------
REM Step 4: Generate Windows resources
REM ----------------------------------------------------------------------------
echo [4/7] Generating Windows resources...

go-winres make
if errorlevel 1 (
    echo ERROR: Failed to generate Windows resources
    echo Make sure go-winres is installed: go install github.com/tc-hib/go-winres@latest
    exit /b 1
)
echo       - Windows resources generated (.syso files)

REM ----------------------------------------------------------------------------
REM Step 5: Create output directory
REM ----------------------------------------------------------------------------
echo [5/7] Creating output directory...

set "OUTPUT_DIR=releases\v%NEW_VERSION%"
if not exist "%OUTPUT_DIR%" mkdir "%OUTPUT_DIR%"
echo       - Output directory: %OUTPUT_DIR%

REM ----------------------------------------------------------------------------
REM Step 6: Build Windows executable with environment-specific ldflags
REM ----------------------------------------------------------------------------
echo [6/7] Building Windows executable for %ENV%...

set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64

REM Build ldflags with environment-specific values
set "LDFLAGS=-X main.EnvName=%ENV% -X \"main.EnvDisplayName=%DISPLAY_NAME%\" -X main.EnvAPIEndpoint=%API_ENDPOINT% -X main.EnvConfigSuffix=%CONFIG_SUFFIX%"

REM Build the main release executable
go build -ldflags "%LDFLAGS%" -o "%OUTPUT_DIR%\%EXE_NAME%" .
if errorlevel 1 (
    echo ERROR: Windows build failed
    exit /b 1
)
echo       - %EXE_NAME% built

REM Also build a copy for the installer (in current directory)
go build -ldflags "%LDFLAGS% -H=windowsgui" -o "%EXE_NAME%" .
if errorlevel 1 (
    echo WARNING: Installer executable build failed
)
echo       - %EXE_NAME% built (for installer, windowsgui mode)

REM Reset environment
set GOOS=
set GOARCH=
set CGO_ENABLED=

REM ----------------------------------------------------------------------------
REM Step 7: Build Windows installer
REM ----------------------------------------------------------------------------
echo [7/7] Building Windows installer...

REM Check for Inno Setup in PATH or standard locations
set "ISCC_PATH="
where iscc >nul 2>nul && set "ISCC_PATH=iscc"
if "%ISCC_PATH%"=="" if exist "%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe" set "ISCC_PATH=%ProgramFiles(x86)%\Inno Setup 6\ISCC.exe"
if "%ISCC_PATH%"=="" if exist "%ProgramFiles%\Inno Setup 6\ISCC.exe" set "ISCC_PATH=%ProgramFiles%\Inno Setup 6\ISCC.exe"

if "%ISCC_PATH%"=="" (
    echo WARNING: Inno Setup not found, skipping installer creation
    echo          Install from: https://jrsoftware.org/isdl.php
    goto :checksums
)

REM Update installer output directory to point to release folder
powershell -NoProfile -Command "$f='%INSTALLER_ISS%'; $c=[IO.File]::ReadAllText($f); $c=$c -replace 'OutputDir=installer-output', 'OutputDir=releases\\v%NEW_VERSION%'; [IO.File]::WriteAllText($f,$c)"

REM Run Inno Setup
"%ISCC_PATH%" %INSTALLER_ISS%
if errorlevel 1 (
    echo WARNING: Installer build failed
) else (
    echo       - %INSTALLER_PREFIX%-%NEW_VERSION%.exe created
)

REM Restore installer output directory
powershell -NoProfile -Command "$f='%INSTALLER_ISS%'; $c=[IO.File]::ReadAllText($f); $c=$c -replace 'OutputDir=releases\\\\v%NEW_VERSION%', 'OutputDir=installer-output'; [IO.File]::WriteAllText($f,$c)"

:checksums
REM ----------------------------------------------------------------------------
REM Generate checksums
REM ----------------------------------------------------------------------------
echo.
echo Generating checksums...

pushd "%OUTPUT_DIR%"
REM Use PowerShell for reliable checksum generation
powershell -NoProfile -Command "Get-ChildItem -Filter *.exe | ForEach-Object { $hash = (Get-FileHash $_.Name -Algorithm SHA256).Hash.ToLower(); \"$hash  $($_.Name)\" }" > SHA256SUMS
popd
echo       - SHA256SUMS generated

REM ----------------------------------------------------------------------------
REM Clean up temporary executable
REM ----------------------------------------------------------------------------
if exist "%EXE_NAME%" del "%EXE_NAME%"

REM ----------------------------------------------------------------------------
REM Done!
REM ----------------------------------------------------------------------------
echo.
echo ============================================================================
echo    BUILD COMPLETE - Version %NEW_VERSION% (%ENV%)
echo ============================================================================
echo.
echo Output files in: %OUTPUT_DIR%\
echo.
dir /b "%OUTPUT_DIR%"
echo.
echo NOTE: Linux and macOS builds require native compilation.
echo       Push the version changes and create a tag to trigger GitHub Actions:
echo.
echo         git add -A
echo         git commit -m "Release v%NEW_VERSION%"
echo         git tag v%NEW_VERSION%
echo         git push origin main --tags
echo.
echo       This will build all platforms via GitHub Actions.
echo.
echo To build all environments, run:
echo         build-all.bat dev
echo         build-all.bat stg
echo         build-all.bat beta
echo         build-all.bat prod
echo.

endlocal
