//go:build windows

// File: relay_cwd_fence_windows_test.go
// The device fence on Windows, for the reported case (a C:\AIExpedite grant,
// a session in C:\AIExpedite\frontend): the fence and the working directory may
// differ in case and still be one folder, and a JUNCTION inside the granted
// folder that leads out of it is refused. Go's EvalSymlinks has not followed
// junctions since 1.23, so fenceRealPath reads them itself.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeJunction creates a directory junction, or skips where none can be made.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create a junction on this host: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
}

func TestRefuseCwdOutsideFence_WindowsCaseIsOneFolder(t *testing.T) {
	base := resolvedTempDir(t)
	fence := filepath.Join(base, "AIExpedite")
	frontend := filepath.Join(fence, "frontend")
	if err := os.MkdirAll(frontend, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The grant's spelling and the session's differ only in case.
	if res := fenceRefusal(t, fence, strings.ToLower(frontend)); res != nil {
		t.Fatalf(`C:\AIExpedite must admit c:\aiexpedite\frontend: %+v`, *res)
	}
	if res := fenceRefusal(t, strings.ToUpper(fence), frontend); res != nil {
		t.Fatalf("an upper-case fence must admit its own sub-folder: %+v", *res)
	}
	// A similarly named sibling is still outside.
	sibling := filepath.Join(base, "AIExpedite2")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if res := fenceRefusal(t, fence, sibling); res == nil {
		t.Fatalf(`C:\AIExpedite must not admit C:\AIExpedite2`)
	}
}

func TestRefuseCwdOutsideFence_WindowsJunctionEscape(t *testing.T) {
	base := resolvedTempDir(t)
	fence := filepath.Join(base, "AIExpedite")
	frontend := filepath.Join(fence, "frontend")
	other := filepath.Join(base, "other")
	for _, d := range []string{frontend, filepath.Join(other, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	escape := filepath.Join(fence, "link")
	makeJunction(t, escape, other)
	stay := filepath.Join(fence, "stay")
	makeJunction(t, stay, frontend)

	for name, cwd := range map[string]string{
		"the junction itself":         escape,
		"a folder under the junction": filepath.Join(escape, "sub"),
	} {
		res := fenceRefusal(t, fence, cwd)
		if res == nil {
			t.Errorf("%s (%s) leads out of the granted folder and must be refused", name, cwd)
			continue
		}
		if res.Status != "denied" || res.RejectionReason != cwdOutsideGrantFolderRejectionCode {
			t.Errorf("%s: unexpected refusal %+v", name, *res)
		}
	}
	if res := fenceRefusal(t, fence, stay); res != nil {
		t.Errorf("a junction that stays inside the granted folder must be admitted: %+v", *res)
	}
	// A fence that is itself a junction resolves to its real folder.
	fenceJunction := filepath.Join(base, "fence-junction")
	makeJunction(t, fenceJunction, fence)
	if res := fenceRefusal(t, fenceJunction, frontend); res != nil {
		t.Errorf("a fence spelled through a junction must admit its own sub-folder: %+v", *res)
	}
	if res := fenceRefusal(t, fenceJunction, other); res == nil {
		t.Errorf("a fence spelled through a junction must still refuse a folder outside it")
	}
}
