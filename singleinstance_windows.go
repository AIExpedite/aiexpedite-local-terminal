//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// agentInstanceLockPath is shared by the legacy and relocated copies for this
// Windows account and release channel because both use the same roaming config
// directory.
func agentInstanceLockPath() string {
	return filepath.Join(GetConfigDir(), "agent.instance.lock")
}

// tryAcquireAgentInstanceLock opens and non-blockingly locks path. The caller
// owns the returned file only when acquired is true.
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

// acquireAgentInstance prevents two normal agent processes for the same
// Windows account/channel from consuming the same device subscription. Lock
// setup fails open so an incidental filesystem problem does not make the agent
// unusable; actual lock contention fails closed.
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

// relocatedAgentIsRunning checks the same lock that the per-user copy holds.
// A stale Program Files launcher can therefore exit without starting another
// copy. If no process owns the lock, the caller may start the destination; the
// destination's startup lock still serializes simultaneous stale launchers.
func relocatedAgentIsRunning() bool {
	file, acquired, err := tryAcquireAgentInstanceLock(agentInstanceLockPath())
	if err != nil || !acquired {
		return err == nil && !acquired
	}
	_ = unlockFile(file)
	_ = file.Close()
	return false
}
