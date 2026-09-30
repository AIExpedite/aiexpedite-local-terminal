// opencode_binary_identity_test.go — the content identity of a Windows
// `opencode.cmd` npm shim changes when npm replaces the package behind it, even
// though the shim's own bytes, size and mtime do not.
//
// The hash (openCodeShimPackageIdentity) is platform-neutral and tested on every
// OS; the gate that applies it only to `.cmd` / `.bat` paths
// (openCodeBinaryIdentity) is pinned per platform.
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openCodeShimFixture lays out what `npm install -g opencode-ai` leaves in the
// global prefix: the shim, the opencode-ai manifest, and one platform package
// nested under it. Returns the shim path.
func openCodeShimFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	shim := filepath.Join(root, "opencode.cmd")
	writeIdentityFile(t, shim, "@ECHO off\r\nnode \"%~dp0\\node_modules\\opencode-ai\\bin\\opencode\" %*\r\n")
	writeIdentityFile(t, filepath.Join(root, "node_modules", "opencode-ai", "package.json"),
		`{"name":"opencode-ai","version":"0.9.1"}`)
	writeIdentityFile(t, filepath.Join(root, "node_modules", "opencode-ai", "node_modules",
		"opencode-windows-x64", "package.json"), `{"name":"opencode-windows-x64","version":"0.9.1"}`)
	return shim
}

func writeIdentityFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rewriteKeepingStat replaces a file's content with a same-length body and
// restores its mtime — the upgrade shape a stat-keyed cache cannot see.
func rewriteKeepingStat(t *testing.T, path, body string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != info.Size() {
		t.Fatalf("fixture bug: replacement is %d bytes, original %d", len(body), info.Size())
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeShimPackageIdentity_ChangesWhenOnlyThePackageManifestChanges(t *testing.T) {
	shim := openCodeShimFixture(t)
	manifest := filepath.Join(filepath.Dir(shim), "node_modules", "opencode-ai", "package.json")
	before := openCodeShimPackageIdentity(shim)
	shimInfo, _ := os.Stat(shim)

	rewriteKeepingStat(t, manifest, `{"name":"opencode-ai","version":"0.9.2"}`)

	after := openCodeShimPackageIdentity(shim)
	if before == after {
		t.Fatal("identity unchanged after the package manifest was replaced")
	}
	if now, _ := os.Stat(shim); now.Size() != shimInfo.Size() || !now.ModTime().Equal(shimInfo.ModTime()) {
		t.Fatal("fixture bug: the shim itself changed")
	}
	if again := openCodeShimPackageIdentity(shim); again != after {
		t.Fatal("identity is not deterministic")
	}
}

func TestOpenCodeShimPackageIdentity_APlatformPackageChangeAloneIsANewIdentity(t *testing.T) {
	shim := openCodeShimFixture(t)
	before := openCodeShimPackageIdentity(shim)
	platform := filepath.Join(filepath.Dir(shim), "node_modules", "opencode-ai", "node_modules",
		"opencode-windows-x64", "package.json")
	rewriteKeepingStat(t, platform, `{"name":"opencode-windows-x64","version":"0.9.2"}`)
	if openCodeShimPackageIdentity(shim) == before {
		t.Fatal("a nested platform package change did not change the identity")
	}

	// A platform package installed BESIDE opencode-ai (npm's flattened layout)
	// counts too.
	withSibling := openCodeShimPackageIdentity(shim)
	writeIdentityFile(t, filepath.Join(filepath.Dir(shim), "node_modules", "opencode-windows-arm64", "package.json"),
		`{"name":"opencode-windows-arm64"}`)
	if openCodeShimPackageIdentity(shim) == withSibling {
		t.Fatal("a sibling platform package did not change the identity")
	}
}

func TestOpenCodeShimPackageIdentity_AMissingPackageIsAStableToken(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "opencode.cmd")
	writeIdentityFile(t, shim, "@ECHO off\r\n")
	first := openCodeShimPackageIdentity(shim)
	if first == "" || first != openCodeShimPackageIdentity(shim) {
		t.Fatalf("missing package must hash to a stable non-empty token, got %q", first)
	}
	// Installing the package is a change.
	writeIdentityFile(t, filepath.Join(root, "node_modules", "opencode-ai", "package.json"), `{}`)
	if openCodeShimPackageIdentity(shim) == first {
		t.Fatal("installing the package did not change the identity")
	}
}

func TestOpenCodeShimIdentityFiles_AreBoundedInCountAndSize(t *testing.T) {
	shim := openCodeShimFixture(t)
	modules := filepath.Join(filepath.Dir(shim), "node_modules")
	for i := 0; i < 20; i++ {
		writeIdentityFile(t, filepath.Join(modules, "opencode-windows-extra-"+string(rune('a'+i)), "package.json"), `{}`)
	}
	files := openCodeShimIdentityFiles(shim)
	if len(files) != openCodeIdentityMaxFiles {
		t.Fatalf("identity reads %d files, want the cap %d", len(files), openCodeIdentityMaxFiles)
	}
	root := filepath.Dir(shim)
	for _, f := range files {
		if rel, err := filepath.Rel(root, f); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("identity reads %q outside the shim's tree", f)
		}
	}

	// A change past the per-file byte cap is outside what is hashed: the read
	// stops at openCodeIdentityMaxFileBytes.
	manifest := filepath.Join(modules, "opencode-ai", "package.json")
	head := strings.Repeat("x", openCodeIdentityMaxFileBytes)
	writeIdentityFile(t, manifest, head+"tail-one")
	capped := openCodeShimPackageIdentity(shim)
	writeIdentityFile(t, manifest, head+"tail-two")
	if openCodeShimPackageIdentity(shim) != capped {
		t.Fatal("bytes past the per-file cap were read")
	}
}

func TestOpenCodeBinaryIdentity_ANativeBinaryKeepsTheStatKey(t *testing.T) {
	native := filepath.Join(t.TempDir(), "opencode")
	writeIdentityFile(t, native, "binary")
	if got := openCodeBinaryIdentity(native); got != "" {
		t.Fatalf("a native binary must add no content identity, got %q", got)
	}
	if got := openCodeBinaryIdentity(""); got != "" {
		t.Fatalf("an empty path must add no identity, got %q", got)
	}
	shim := openCodeShimFixture(t)
	got := openCodeBinaryIdentity(shim)
	if runtime.GOOS == "windows" && got == "" {
		t.Fatal("a Windows .cmd shim must carry a content identity")
	}
	if runtime.GOOS != "windows" && got != "" {
		t.Fatalf("no path is a shim off Windows, got %q", got)
	}
}

// The `--version` cache keys on the shim's (path, mtime, size) alone; a package
// replaced under an unchanged shim must drop the cached reading so the next
// probe reports the new version.
func TestOpenCodeProbeVersion_DropsACachedReadingWhenTheShimPackageChanges(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only a Windows .cmd path is a shim")
	}
	openCodeSmokeEnv(t)
	shim := openCodeShimFixture(t)
	seedProbeVersion(t, shim, "0.9.1")
	if got := openCodeProbeVersion(shim); got != "0.9.1" {
		t.Fatalf("seeded reading not served: %q", got)
	}

	manifest := filepath.Join(filepath.Dir(shim), "node_modules", "opencode-ai", "package.json")
	rewriteKeepingStat(t, manifest, `{"name":"opencode-ai","version":"0.9.2"}`)
	// The shim's stat key is unchanged, so only the identity check can drop it.
	openCodeForgetVersionOnIdentityChange(shim)
	versionProbeMu.Lock()
	_, stillCached := versionProbeCache[versionProbeKeyFor(t, shim)]
	versionProbeMu.Unlock()
	if stillCached {
		t.Fatal("the pre-upgrade version reading survived a package replacement")
	}
}

func versionProbeKeyFor(t *testing.T, path string) versionProbeKey {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return versionProbeKey{Path: path, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
}

// Only a regular file is opened: a manifest path that is a directory (or, via an
// npm link, a FIFO or device) hashes as the missing token rather than being read.
func TestOpenCodeShimPackageIdentity_ANonRegularManifestIsTheMissingToken(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "opencode.cmd")
	writeIdentityFile(t, shim, "@ECHO off\r\n")
	absent := openCodeShimPackageIdentity(shim)
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "opencode-ai", "package.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := openCodeShimPackageIdentity(shim); got != absent {
		t.Fatal("a directory named package.json must hash exactly like a missing manifest")
	}
}

// An unrelated `opencode-*` package in npm's global node_modules (a plugin) is
// neither hashed nor able to crowd the real platform package out of the cap.
func TestOpenCodeShimIdentityFiles_IgnoreUnrelatedOpenCodePackages(t *testing.T) {
	shim := openCodeShimFixture(t)
	before := openCodeShimPackageIdentity(shim)
	modules := filepath.Join(filepath.Dir(shim), "node_modules")
	for i := 0; i < 20; i++ {
		writeIdentityFile(t, filepath.Join(modules, "opencode-plugin-"+string(rune('a'+i)), "package.json"), `{}`)
	}
	if openCodeShimPackageIdentity(shim) != before {
		t.Fatal("unrelated opencode-* packages changed the identity")
	}
	for _, f := range openCodeShimIdentityFiles(shim) {
		if strings.Contains(f, "opencode-plugin-") {
			t.Fatalf("identity reads an unrelated package: %s", f)
		}
	}
}
