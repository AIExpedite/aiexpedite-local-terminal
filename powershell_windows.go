//go:build windows
// +build windows

// File: powershell_windows.go
// Persistent PowerShell process management for Windows.
// Eliminates 300-800ms PowerShell startup overhead per command by reusing a single process.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// Unique delimiter to mark end of command output
	psDelimiter = "<<<AIEXPEDITE_CMD_DONE_7f3d2a1b>>>"
	// Timeout for health check commands
	psHealthTimeout = 5 * time.Second
	// Maximum time for a single command execution
	psCommandTimeout = 60 * time.Minute
)

// PersistentPowerShell manages a long-running PowerShell process
type PersistentPowerShell struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   io.ReadCloser
	mutex    sync.Mutex
	healthy  bool
	lastUsed time.Time
}

var (
	globalPS     *PersistentPowerShell
	globalPSLock sync.Mutex
)

// GetPowerShell returns the global PowerShell instance, creating if needed
func GetPowerShell() (*PersistentPowerShell, error) {
	globalPSLock.Lock()
	defer globalPSLock.Unlock()

	if globalPS != nil && globalPS.IsHealthy() {
		return globalPS, nil
	}

	// Kill old instance if exists
	if globalPS != nil {
		globalPS.closeInternal()
		globalPS = nil
	}

	ps, err := NewPersistentPowerShell()
	if err != nil {
		return nil, err
	}
	globalPS = ps
	return ps, nil
}

// NewPersistentPowerShell creates a new persistent PowerShell process
func NewPersistentPowerShell() (*PersistentPowerShell, error) {
	cmd := exec.Command("powershell.exe",
		"-NoProfile",
		"-NoLogo",
		"-NonInteractive",
		"-Command", "-", // Read commands from stdin
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start PowerShell: %w", err)
	}

	ps := &PersistentPowerShell{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReader(stdout),
		stderr:   stderr,
		healthy:  true,
		lastUsed: time.Now(),
	}

	// Verify process is responsive with a health check
	if err := ps.healthCheck(); err != nil {
		ps.closeInternal()
		return nil, fmt.Errorf("PowerShell health check failed: %w", err)
	}

	fmt.Println("[powershell] Persistent PowerShell process started")
	return ps, nil
}

// Execute runs a command and returns the output
func (ps *PersistentPowerShell) Execute(ctx context.Context, command string, cwd string) (string, error) {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()

	if !ps.healthy {
		return "", fmt.Errorf("PowerShell process is not healthy")
	}

	ps.lastUsed = time.Now()

	// Build command with working directory and delimiter
	// We wrap everything to ensure delimiter is always printed, even on errors
	var fullCmd strings.Builder

	if cwd != "" {
		// Change to working directory first (escape single quotes)
		escapedCwd := strings.ReplaceAll(cwd, "'", "''")
		fullCmd.WriteString(fmt.Sprintf("Set-Location -LiteralPath '%s'; ", escapedCwd))
	}

	// Execute the command and capture any errors
	fullCmd.WriteString(command)

	// Always print delimiter at end
	fullCmd.WriteString(fmt.Sprintf("; Write-Host '%s'", psDelimiter))

	// Send command to PowerShell
	_, err := fmt.Fprintln(ps.stdin, fullCmd.String())
	if err != nil {
		ps.healthy = false
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	// Read output until delimiter with timeout
	outputChan := make(chan string, 1)
	errChan := make(chan error, 1)

	go func() {
		var output strings.Builder
		for {
			line, err := ps.stdout.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if line == psDelimiter {
				outputChan <- output.String()
				return
			}
			if output.Len() > 0 {
				output.WriteString("\r\n")
			}
			output.WriteString(line)
		}
	}()

	// Wait with timeout
	select {
	case output := <-outputChan:
		return output, nil
	case err := <-errChan:
		ps.healthy = false
		return "", fmt.Errorf("failed to read output: %w", err)
	case <-ctx.Done():
		ps.healthy = false
		return "", fmt.Errorf("command cancelled")
	case <-time.After(psCommandTimeout):
		ps.healthy = false
		return "", fmt.Errorf("command timed out after %v", psCommandTimeout)
	}
}

// healthCheck verifies the PowerShell process is responsive
func (ps *PersistentPowerShell) healthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), psHealthTimeout)
	defer cancel()

	output, err := ps.Execute(ctx, "Write-Host 'OK'", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "OK" {
		return fmt.Errorf("unexpected health check response: %q", output)
	}
	return nil
}

// IsHealthy returns whether the PowerShell process is healthy
// This method acquires the mutex to check process state
func (ps *PersistentPowerShell) IsHealthy() bool {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()

	if !ps.healthy {
		return false
	}

	// Check if process has exited
	if ps.cmd.ProcessState != nil {
		ps.healthy = false
		return false
	}

	return true
}

// closeInternal terminates the PowerShell process (must be called with mutex held or from global lock)
func (ps *PersistentPowerShell) closeInternal() {
	ps.healthy = false

	if ps.stdin != nil {
		ps.stdin.Close()
	}
	if ps.stderr != nil {
		ps.stderr.Close()
	}
	if ps.cmd != nil && ps.cmd.Process != nil {
		ps.cmd.Process.Kill()
		ps.cmd.Wait()
	}

	fmt.Println("[powershell] Persistent PowerShell process closed")
}

// Close terminates the PowerShell process
func (ps *PersistentPowerShell) Close() {
	ps.mutex.Lock()
	defer ps.mutex.Unlock()
	ps.closeInternal()
}

// RestartPowerShell forcefully restarts the PowerShell process
// Use this when the process becomes unresponsive
func RestartPowerShell() error {
	globalPSLock.Lock()
	defer globalPSLock.Unlock()

	if globalPS != nil {
		globalPS.Close()
		globalPS = nil
	}

	ps, err := NewPersistentPowerShell()
	if err != nil {
		return err
	}
	globalPS = ps
	fmt.Println("[powershell] PowerShell process restarted")
	return nil
}

// ShutdownPowerShell closes the global PowerShell process
// Call this on application exit
func ShutdownPowerShell() {
	globalPSLock.Lock()
	defer globalPSLock.Unlock()

	if globalPS != nil {
		globalPS.Close()
		globalPS = nil
	}
}
