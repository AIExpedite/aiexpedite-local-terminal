// File: environment.go
// Environment configuration - values set via ldflags at build time
package main

// Environment variables - overridden via ldflags during build:
//
//	go build -ldflags="-X main.EnvName=dev -X 'main.EnvDisplayName=AI Expedite Terminal (Dev)' ..."
var (
	// EnvName is the environment identifier (dev, stg, beta, prod)
	EnvName = "prod"

	// EnvDisplayName is shown in the system tray and installer
	EnvDisplayName = "AI Expedite Terminal"

	// EnvAPIEndpoint is the API base URL for device registration
	EnvAPIEndpoint = "https://api.aiexpedite.com/terminal"

	// EnvConfigSuffix is appended to config directory and registry keys
	// Empty for prod, "-Dev" for dev, "-Stg" for stg, "-Beta" for beta
	EnvConfigSuffix = ""
)
