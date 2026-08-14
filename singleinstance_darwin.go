//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func agentInstanceLockPath() string {
	return filepath.Join(GetConfigDir(), "agent.instance.lock")
}

func tryAcquireAgentInstanceLock(path string) (file *os.File, acquired bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	acquired, err = tryLockFileExclusive(file)
	if err != nil || !acquired {
		_ = file.Close()
		return nil, acquired, err
	}
	return file, true, nil
}

// acquireAgentInstance ensures the forced first-relocation launch cannot leave
// two macOS agents consuming the same per-account subscription. The legacy
// process exits before acquiring this lock; the relocated process acquires it
// before creating a tray or starting Pub/Sub.
func acquireAgentInstance() (release func(), acquired bool) {
	file, acquired, err := tryAcquireAgentInstanceLock(agentInstanceLockPath())
	if err != nil {
		fmt.Printf("[startup] Could not create agent instance lock (continuing): %v\n", err)
		return func() {}, true
	}
	if !acquired {
		return func() {}, false
	}
	return func() {
		_ = unlockFile(file)
		_ = file.Close()
	}, true
}
