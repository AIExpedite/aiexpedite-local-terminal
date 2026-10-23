package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWhatsAppLinkIDsAreSafePathSegments(t *testing.T) {
	withWhatsAppTestEnv(t)
	for _, bad := range []string{"", "..", "../x", `a\b`, "a/b", "C:x", "a.b", strings.Repeat("a", 65)} {
		if _, err := whatsAppLinkDir(bad); err == nil {
			t.Errorf("accepted link id %q", bad)
		}
	}
	if _, err := whatsAppLinkDir("lnk_AZ-09"); err != nil {
		t.Fatal(err)
	}
}

func TestWhatsAppLocalLinksRoundTrip(t *testing.T) {
	withWhatsAppTestEnv(t)
	if links, err := loadWhatsAppLocalLinks(); err != nil || links != nil {
		t.Fatalf("empty: %v %v", links, err)
	}
	want := []whatsAppLocalLink{{LinkID: "lnk_1", WorkspaceID: "ws", Generation: 3, State: waStateConnected, HistoryComplete: true}}
	if err := saveWhatsAppLocalLinks(want); err != nil {
		t.Fatal(err)
	}
	got, err := loadWhatsAppLocalLinks()
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(whatsAppRootDir(), whatsAppLinksFileName))
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "key") {
		t.Fatal("links.json carries secret material")
	}
}

// plantWhatsAppDirLink plants a directory link: a junction on Windows (no privilege
// needed), a symlink elsewhere.
func plantWhatsAppDirLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("mklink /J unavailable: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

func TestWhatsAppWipeNeverFollowsALink(t *testing.T) {
	withWhatsAppTestEnv(t)
	dir, err := ensureWhatsAppLinkDir("lnk_wipe")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantWhatsAppDirLink(t, filepath.Join(dir, "planted"), outside)
	if err := os.WriteFile(filepath.Join(dir, whatsAppSessionDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wipeWhatsAppLinkDir("lnk_wipe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("link directory survived the wipe")
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "keep" {
		t.Fatal("the wipe followed the planted link")
	}
	// Absent is success.
	if err := wipeWhatsAppLinkDir("lnk_wipe"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureWhatsAppLinkDirRefusesAPlantedLink(t *testing.T) {
	_, root := withWhatsAppTestEnv(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plantWhatsAppDirLink(t, filepath.Join(root, "lnk_planted"), t.TempDir())
	if _, err := ensureWhatsAppLinkDir("lnk_planted"); err == nil {
		t.Fatal("a link where the session directory should be was accepted")
	}
}

func TestWhatsAppLinkDirIsOwnerOnly(t *testing.T) {
	withWhatsAppTestEnv(t)
	dir, err := ensureWhatsAppLinkDir("lnk_acl")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("icacls", dir).CombinedOutput()
		if err != nil {
			t.Skipf("icacls unavailable: %v", err)
		}
		lower := strings.ToLower(string(out))
		for _, who := range []string{"everyone", `builtin\users`, "authenticated users", `builtin\administrators`} {
			if strings.Contains(lower, who) {
				t.Fatalf("%s can access the session directory:\n%s", who, out)
			}
		}
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}
