//go:build windows

package main

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
)

func TestConfigureCommandCancellationPreservesCommandContextDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd.exe", "/c", "exit 0")
	before := reflect.ValueOf(cmd.Cancel).Pointer()
	configureCommandCancellation(cmd)
	after := reflect.ValueOf(cmd.Cancel).Pointer()

	if after != before {
		t.Fatal("Windows cancellation callback was replaced; timed-out processes must retain CommandContext's default Process.Kill")
	}
}
