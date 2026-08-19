package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probeSourceGlobs are the files whose exec sites run as BACKGROUND probes:
// CLI-agent detection, usage/rate-limit collection, and machine-info gathering.
// Nothing here is user-initiated, so a console child that pops a window flashes
// it onto the desktop of a user who did not ask for anything — the agent itself
// runs windowless in the tray. Every one of these must call hideWindow.
var probeSourceGlobs = []string{
	"cliagent_usage*.go",
	"cliagent_ratelimit*.go",
	"systemInfo*.go",
}

// TestBackgroundProbesHideConsoleWindow fails when a probe spawns a child
// process without hiding its console window. Two probes (`claude auth status`
// and `codex login status`) shipped without it and flashed a console on every
// usage/rate-limit collection; this pins the whole family so the next probe
// added here can't reintroduce it.
func TestBackgroundProbesHideConsoleWindow(t *testing.T) {
	var files []string
	for _, glob := range probeSourceGlobs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %q: %v", glob, err)
		}
		for _, f := range matched {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		t.Fatal("no probe source files matched — globs are stale")
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			code := line
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			if !strings.Contains(code, "exec.Command(") && !strings.Contains(code, "exec.CommandContext(") {
				continue
			}
			// The hideWindow call has to land before the process starts, so it
			// sits within a few lines of construction in every existing site.
			window := lines[i:min(i+8, len(lines))]
			if strings.Contains(strings.Join(window, "\n"), "hideWindow(") {
				continue
			}
			t.Errorf("%s:%d spawns a child process without hideWindow() — "+
				"background probes must not flash a console window:\n\t%s",
				file, i+1, strings.TrimSpace(line))
		}
	}
}
