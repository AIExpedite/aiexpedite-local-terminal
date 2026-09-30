// cliagent_smoke_opencode_update_survival_test.go — the maintenance harness's
// before / after pair, executed against the compiled stub.
//
// The harness smokes OpenCode once before and once after an upgrade. Two things
// have to hold for that pair to mean anything:
//
//   - the PRE-update smoke must not itself update the CLI. OpenCode updates in
//     the background during a run unless OPENCODE_DISABLE_AUTOUPDATE is set, so
//     every maintenance child runs with that pin (openCodeMaintenanceEnvPins);
//   - the POST-update smoke must test the new install, not replay the old
//     verdict. On Windows `npm install -g` leaves `opencode.cmd` byte-identical
//     and rewrites only the package behind it, so the cooldown stamp carries the
//     shim's content identity (openCodeBinaryIdentity).
//
// On Windows the stub is launched through a `.cmd` shim exactly as npm installs
// it; elsewhere it is the native binary.
package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openCodeUpdateInstall is one temporary OpenCode install: the stub binary
// inside an npm-style package, its version file (the package manifest an
// upgrade rewrites), and the path the smoke launches.
type openCodeUpdateInstall struct {
	exe         string
	versionFile string
	launchPath  string
}

func newOpenCodeUpdateInstall(t *testing.T, version string) openCodeUpdateInstall {
	t.Helper()
	stub := buildOpenCodeStub(t)
	root := t.TempDir()
	pkg := filepath.Join(root, "node_modules", "opencode-ai")
	exe := filepath.Join(pkg, "bin", "opencode")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	data, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	writeIdentityFile(t, exe, "")
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	install := openCodeUpdateInstall{
		exe:         exe,
		versionFile: filepath.Join(pkg, "package.json"),
		launchPath:  exe,
	}
	writeIdentityFile(t, install.versionFile, version)
	if runtime.GOOS == "windows" {
		install.launchPath = filepath.Join(root, "opencode.cmd")
		writeIdentityFile(t, install.launchPath, "@echo off\r\n\""+exe+"\" %*\r\n")
	}
	return install
}

// snapshot captures everything an update could touch.
func (i openCodeUpdateInstall) snapshot(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range []string{i.exe, i.versionFile, i.launchPath} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("install file %s missing: %v", filepath.Base(p), err)
		}
		out[p] = data
	}
	if _, err := os.Stat(i.exe + ".old"); err == nil {
		out["aside"] = []byte("present")
	}
	return out
}

func sameSnapshot(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// harnessUpgrade is the harness-managed update: the binary is replaced and the
// manifest rewritten to the new version at the same size and mtime, while the
// shim (on Windows) is left untouched — npm's own behaviour.
func (i openCodeUpdateInstall) harnessUpgrade(t *testing.T, next string) {
	t.Helper()
	data, err := os.ReadFile(i.exe)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(i.exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(i.exe, i.exe+".v1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	// A native binary IS the launch path, so its replacement must be visible to
	// the stat key the way a real upgrade's is.
	later := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(i.exe, later, later); err != nil {
		t.Fatal(err)
	}
	rewriteKeepingStat(t, i.versionFile, next)
}

func TestOpenCodeSmoke_PreUpdateSmokeDoesNotUpdateAndPostUpdateSmokeTestsTheNewInstall(t *testing.T) {
	openCodeSmokeEnv(t)
	const v1, v2, selfUpdated = "opencode 0.9.1", "opencode 0.9.2", "opencode 0.9.9"

	// Control: the stub really does mutate its own install when nothing pins it
	// off — otherwise the pinned run below would prove nothing.
	control := newOpenCodeUpdateInstall(t, v1)
	before := control.snapshot(t)
	cmd := exec.Command(control.exe, "run", "--format", "json")
	cmd.Env = append(os.Environ(),
		"OPENCODE_STUB_SELF_UPDATE="+control.versionFile,
		"OPENCODE_STUB_SELF_UPDATE_TO="+selfUpdated)
	cmd.Env = withoutEnvVar(cmd.Env, "OPENCODE_DISABLE_AUTOUPDATE")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("control run failed: %v\n%s", err, out)
	}
	if sameSnapshot(before, control.snapshot(t)) {
		t.Fatal("control: the unpinned stub left its install untouched, so the knob is broken")
	}

	// The install under test.
	install := newOpenCodeUpdateInstall(t, v1)
	stubOpenCodeSmokePath(t, install.launchPath)
	runLog := filepath.Join(t.TempDir(), "runs.log")
	envLog := filepath.Join(t.TempDir(), "env.log")
	t.Setenv("OPENCODE_STUB_VERSION_FILE", install.versionFile)
	t.Setenv("OPENCODE_STUB_SELF_UPDATE", install.versionFile)
	t.Setenv("OPENCODE_STUB_SELF_UPDATE_TO", selfUpdated)
	t.Setenv("OPENCODE_STUB_ECHO_STDIN", "1")
	t.Setenv("OPENCODE_STUB_RUN_LOG", runLog)
	t.Setenv("OPENCODE_STUB_ENV_LOG", envLog)

	pristine := install.snapshot(t)
	pre, replayed := runCLISmoke(context.Background(), "opencode")
	if replayed || pre.Status != cliSmokeStatusSuccess || !pre.MarkerMatched || pre.Version != v1 {
		t.Fatalf("pre-update smoke = %+v replayed=%t", pre, replayed)
	}
	if !sameSnapshot(pristine, install.snapshot(t)) {
		t.Fatal("the pre-update smoke updated the CLI it was measuring")
	}
	if logged, _ := os.ReadFile(envLog); !strings.Contains(string(logged), "autoupdate=true title=true") {
		t.Fatalf("the maintenance pins did not reach the child: %q", logged)
	}

	install.harnessUpgrade(t, v2)
	runsBefore := openCodeStubRunCount(t, runLog)

	post, replayed := runCLISmoke(context.Background(), "opencode")
	if replayed {
		t.Fatal("the post-update smoke replayed the pre-update verdict")
	}
	if post.Status != cliSmokeStatusSuccess || !post.MarkerMatched || post.Version != v2 {
		t.Fatalf("post-update smoke = %+v, want success reporting %q", post, v2)
	}
	if got := openCodeStubRunCount(t, runLog) - runsBefore; got != 1 {
		t.Fatalf("the post-update smoke spawned %d runs, want exactly 1", got)
	}

	// The resolved shape is filed under the NEW install's key and no other.
	current, ok := cliSmokeShapeKeyWithIdentity(install.launchPath, openCodeBinaryIdentity)
	if !ok {
		t.Fatal("launch path is not stattable")
	}
	cliSmokeShapeMu.Lock()
	defer cliSmokeShapeMu.Unlock()
	for key, shape := range cliSmokeShapeCache {
		if key.Path != install.launchPath {
			continue
		}
		if key != current || shape != openCodeRunShapeIDPlain {
			t.Errorf("shape %q filed under a stale key %+v (current %+v)", shape, key, current)
		}
	}
	if _, filed := cliSmokeShapeCache[current]; !filed {
		t.Error("the post-update shape was not filed under the new install's key")
	}
}

func openCodeStubRunCount(t *testing.T, runLog string) int {
	t.Helper()
	data, err := os.ReadFile(runLog)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

// withoutEnvVar drops every entry for key (case-insensitively on Windows).
func withoutEnvVar(env []string, key string) []string {
	out := env[:0:0]
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key)) {
			continue
		}
		out = append(out, e)
	}
	return out
}
