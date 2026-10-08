package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_opencode_direct_windows_test.go — where the reader finds
   OpenCode's store on Windows, and that reading it never holds a handle that
   would block OpenCode's own rename of a record.
   ------------------------------------------------------------------------ */

func TestOpenCodeDirectWindows_FindsTheStoreUnderEveryOverride(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("OPENCODE_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	def := filepath.Join(profile, ".local", "share", "opencode")
	if err := os.MkdirAll(filepath.Join(def, "storage", "message"), 0o755); err != nil {
		t.Fatal(err)
	}
	if layout, root := openCodeStoreLayout(); layout != openCodeStoreLayoutJSON || root != def {
		t.Fatalf("default = %q %q, want json under %%USERPROFILE%%", layout, root)
	}

	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "opencode", "opencode.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if layout, root := openCodeStoreLayout(); layout != openCodeStoreLayoutSQLite || root != filepath.Join(xdg, "opencode") {
		t.Fatalf("xdg = %q %q", layout, root)
	}

	data := t.TempDir()
	t.Setenv("OPENCODE_DATA", data)
	if layout, root := openCodeStoreLayout(); layout != openCodeStoreLayoutUnknown || root != data {
		t.Fatalf("OPENCODE_DATA = %q %q, want its own (empty) store", layout, root)
	}
}

// Windows refuses a rename over a file another handle holds open (Go opens
// files without FILE_SHARE_DELETE). After a scan, OpenCode can rename over
// every record the reader touched.
func TestOpenCodeDirectWindows_ReadingNeverBlocksOpenCodesRename(t *testing.T) {
	_, store, clock := openCodeDirectFixture(t, 2201)
	store.write(openCodeTestMessage{session: "ses_a", id: "msg_a", createdMs: clock.ms(-time.Hour), completedMs: clock.ms(-time.Hour + time.Second), steps: [][3]int64{{3, 0, 0}, {4, 0, 0}}})
	scanOpenCodeDirect(t)
	if b := openCodeDirectBucket(t, "", clock.now); b.tokens() != 7 {
		t.Fatalf("bucket = %+v", b)
	}
	for _, path := range []string{
		filepath.Join(store.root, "storage", "message", "ses_a", "msg_a.json"),
		filepath.Join(store.root, "storage", "part", "msg_a", "prt_msg_a_0.json"),
	} {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatalf("OpenCode's rename over %s failed: %v", filepath.Base(path), err)
		}
	}
}
