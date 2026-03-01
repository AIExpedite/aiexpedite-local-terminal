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
	// Unique marker to separate cwd from command output
	psCwdMarker = "<<<AIEXPEDITE_CWD_7f3d2a1b>>>"
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
	isPwsh   bool   // true if using pwsh.exe (PowerShell 7+, supports && natively)
	lastCwd  string // last known working directory of this PS process
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

// NewPersistentPowerShell creates a new persistent PowerShell process.
// Prefers pwsh.exe (PowerShell 7+) which supports && natively, falling back to powershell.exe.
func NewPersistentPowerShell() (*PersistentPowerShell, error) {
	psExe := "powershell.exe"
	if _, err := exec.LookPath("pwsh.exe"); err == nil {
		psExe = "pwsh.exe"
		fmt.Println("[powershell] Using PowerShell 7+ (pwsh.exe)")
	}

	cmd := exec.Command(psExe,
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
		isPwsh:   psExe == "pwsh.exe",
	}

	// Verify process is responsive with a health check
	if err := ps.healthCheck(); err != nil {
		ps.closeInternal()
		return nil, fmt.Errorf("PowerShell health check failed: %w", err)
	}

	fmt.Println("[powershell] Persistent PowerShell process started")
	return ps, nil
}

// Execute runs a command and returns the output.
// Tracks the working directory across calls — only issues Set-Location when
// the requested cwd differs from where the process already is.
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

	// Only Set-Location when the requested cwd is new and different from where we already are
	if cwd != "" && !strings.EqualFold(cwd, ps.lastCwd) {
		escapedCwd := strings.ReplaceAll(cwd, "'", "''")
		fullCmd.WriteString(fmt.Sprintf("Set-Location -LiteralPath '%s'; ", escapedCwd))
	}

	// Wrap user command in a script block so && / || operators inside the command
	// don't consume the delimiter. Without this, PowerShell's operator precedence
	// causes `cmd1 && cmd2; Write-Host DELIMITER` to skip the delimiter when cmd1 fails.
	fullCmd.WriteString(fmt.Sprintf("& { %s }", command))

	// Use newlines (not ;) to separate delimiter lines — newlines are unconditional
	// statement separators that cannot be captured by && or || operators.
	fullCmd.WriteString(fmt.Sprintf("\nWrite-Host ''\nWrite-Host '%s'\n(Get-Location).Path | Write-Host\nWrite-Host '%s'",
		psCwdMarker, psDelimiter))

	// Send command to PowerShell
	_, err := fmt.Fprintln(ps.stdin, fullCmd.String())
	if err != nil {
		ps.healthy = false
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	// Read output until delimiter with timeout, stripping cwd marker lines
	type execResult struct {
		output string
		newCwd string
	}
	resultChan := make(chan execResult, 1)
	errChan := make(chan error, 1)

	go func() {
		var output strings.Builder
		var newCwd string
		cwdMarkerSeen := false

		for {
			line, err := ps.stdout.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}

			line = strings.TrimRight(line, "\r\n")

			if line == psDelimiter {
				resultChan <- execResult{output: output.String(), newCwd: newCwd}
				return
			}

			// Detect and capture cwd marker sequence
			if line == psCwdMarker {
				cwdMarkerSeen = true
				continue
			}
			if cwdMarkerSeen {
				newCwd = line
				cwdMarkerSeen = false
				continue
			}

			if output.Len() > 0 {
				output.WriteString("\r\n")
			}
			output.WriteString(line)
		}
	}()

	// Wait with timeout
	select {
	case result := <-resultChan:
		if result.newCwd != "" {
			ps.lastCwd = result.newCwd
		}
		return result.output, nil
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

// IsPersistentPSPwsh returns true if the global persistent PowerShell uses pwsh.exe (PS 7+).
// Used to decide whether bash-style commands (&&, ||) can be routed through PS instead of cmd.exe.
func IsPersistentPSPwsh() bool {
	globalPSLock.Lock()
	defer globalPSLock.Unlock()
	return globalPS != nil && globalPS.healthy && globalPS.isPwsh
}

// GetTrackedCwd returns the last known working directory from the global persistent PowerShell.
func GetTrackedCwd() string {
	globalPSLock.Lock()
	defer globalPSLock.Unlock()
	if globalPS != nil && globalPS.healthy {
		return globalPS.lastCwd
	}
	return ""
}
