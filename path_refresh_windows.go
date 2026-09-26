//go:build windows

package main

import "golang.org/x/sys/windows/registry"

// registryPathSource reads the persisted machine and user PATH from the
// registry, %VAR%-expanded (readPersistedPath, install_runner_windows.go).
type registryPathSource struct{}

func (registryPathSource) MachinePath() string {
	return readPersistedPath(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Environment`)
}

func (registryPathSource) UserPath() string {
	return readPersistedPath(registry.CURRENT_USER, `Environment`)
}

func platformPersistedPathSource() persistedPathSource {
	return registryPathSource{}
}
