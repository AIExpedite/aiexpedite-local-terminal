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

// ExitCodeError represents a command that ran successfully in the shell but
// exited with a non-zero exit code. This is distinct from a process-level
// failure (pipe broken, PS crashed, timeout) so callers can avoid restarting
// PowerShell unnecessarily.
type ExitCodeError struct {
	Code   int
	Output string
}

func (e *ExitCodeError) Error() string {
	if e.Output != "" {
		return fmt.Sprintf("exit code %d\n%s", e.Code, e.Output)
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

const (
	// Unique delimiter to mark end of command output
	psDelimiter = "<<<AIEXPEDITE_CMD_DONE_7f3d2a1b>>>"
	// Unique marker to separate cwd from command output
	psCwdMarker = "<<<AIEXPEDITE_CWD_7f3d2a1b>>>"
	// Unique marker to separate exit code from command output
	psExitCodeMarker = "<<<AIEXPEDITE_EXIT_7f3d2a1b>>>"
	// Timeout for health check commands
	psHealthTimeout = 5 * time.Second
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
	// Capture $LASTEXITCODE immediately after the script block (before any other
	// statements that could reset it), then emit the protocol markers.
	fullCmd.WriteString(fmt.Sprintf(
		"\n$__aix_exit = $LASTEXITCODE\nWrite-Host ''\nWrite-Host '%s'\nWrite-Host $__aix_exit\n(Get-Location).Path | Write-Host\nWrite-Host '%s'",
		psExitCodeMarker, psDelimiter))

	// Send command to PowerShell
	_, err := fmt.Fprintln(ps.stdin, fullCmd.String())
	if err != nil {
		ps.healthy = false
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	// Read output until delimiter with timeout, capturing exit code and cwd markers
	type execResult struct {
		output   string
		newCwd   string
		exitCode int
	}
	resultChan := make(chan execResult, 1)
	errChan := make(chan error, 1)

	go func() {
		var output strings.Builder
		var newCwd string
		exitCode := 0
		exitCodeMarkerSeen := false
		exitCodeCaptured := false
		outputTruncated := false

		// Cap output at 10 MB.  Beyond this limit the result cannot be published
		// to Pub/Sub anyway (64 KB message cap), and an unbounded builder would
		// exhaust RAM on commands that produce very large output (e.g. `type
		// largefile.bin`).  We stop appending but continue draining stdout so the
		// protocol markers (exit code, cwd, delimiter) are still processed.
		const maxOutputBytes = 10 * 1024 * 1024 // 10 MB

		for {
			line, err := ps.stdout.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}

			line = strings.TrimRight(line, "\r\n")

			if line == psDelimiter {
				resultChan <- execResult{output: output.String(), newCwd: newCwd, exitCode: exitCode}
				return
			}

			// Detect exit code marker: next line is the exit code, line after is cwd
			if line == psExitCodeMarker {
				exitCodeMarkerSeen = true
				continue
			}
			if exitCodeMarkerSeen && !exitCodeCaptured {
				if n, err := fmt.Sscanf(line, "%d", &exitCode); err != nil || n != 1 {
					// Malformed exit code line — assume failure so errors aren't hidden
					exitCode = 1
				}
				exitCodeCaptured = true
				continue
			}
			// After exit code, next non-empty line is the cwd (captured until delimiter)
			if exitCodeCaptured && newCwd == "" && line != "" {
				newCwd = line
				continue
			}

			if !outputTruncated {
				if output.Len()+len(line)+2 > maxOutputBytes {
					output.WriteString("\r\n[output truncated: exceeded 10 MB limit]")
					outputTruncated = true
					// Keep draining stdout so markers/delimiter are still received
				} else {
					if output.Len() > 0 {
						output.WriteString("\r\n")
					}
					output.WriteString(line)
				}
			}
		}
	}()

	// Wait for result, pipe error, or context cancellation/timeout.
	// The caller is responsible for setting an appropriate deadline on ctx —
	// we do not impose a secondary timeout here that would override it.
	select {
	case result := <-resultChan:
		if result.newCwd != "" {
			ps.lastCwd = result.newCwd
		}
		if result.exitCode != 0 {
			return result.output, &ExitCodeError{Code: result.exitCode, Output: result.output}
		}
		return result.output, nil
	case err := <-errChan:
		ps.healthy = false
		return "", fmt.Errorf("failed to read output: %w", err)
	case <-ctx.Done():
		ps.healthy = false
		// Kill the process so the reader goroutine unblocks on pipe EOF
		if ps.cmd.Process != nil {
			ps.cmd.Process.Kill()
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("command timed out")
		}
		return "", fmt.Errorf("command cancelled")
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
