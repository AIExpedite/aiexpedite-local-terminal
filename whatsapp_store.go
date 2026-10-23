// whatsapp_store.go — where a WhatsApp link lives on this computer, and how it
// is removed.
//
//	<ConfigDir>/whatsapp/links.json           which links are loaded (no secrets)
//	<ConfigDir>/whatsapp/<linkId>/session.db  whatsmeow sqlstore (Signal keys)
//	<ConfigDir>/whatsapp/<linkId>/outbox.db   not-yet-uploaded records, AES-GCM
//
// Every link directory is created owner-only (0700; on Windows a protected
// DACL granting only the current user — whatsapp_acl_windows.go), so the
// session keys are protected by the OS account on the same footing as the
// agent's own command_secret. The outbox key lives in the OS credential store
// (whatsapp_keystore_*.go), never on disk.
//
// A link is removed with linkSafeRemoveAll, which never descends a symlink or
// a Windows junction: a planted junction inside a session directory must not
// turn "forget this WhatsApp link" into deleting whatever it points at.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	whatsAppDirName       = "whatsapp"
	whatsAppLinksFileName = "links.json"
	whatsAppSessionDBName = "session.db"
	whatsAppOutboxDBName  = "outbox.db"
)

// whatsAppLinkIDPattern bounds a cloud-issued link id before it becomes a
// path segment: no separators, no dots, no drive letters.
var whatsAppLinkIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validWhatsAppLinkID(linkID string) bool {
	return whatsAppLinkIDPattern.MatchString(linkID)
}

// whatsAppRootDirFn is replaced in tests.
var whatsAppRootDirFn = func() string {
	return filepath.Join(GetConfigDir(), whatsAppDirName)
}

func whatsAppRootDir() string { return whatsAppRootDirFn() }

func whatsAppLinkDir(linkID string) (string, error) {
	if !validWhatsAppLinkID(linkID) {
		return "", fmt.Errorf("invalid WhatsApp link id")
	}
	return filepath.Join(whatsAppRootDir(), linkID), nil
}

// ensureWhatsAppLinkDir creates the link's directory owner-only and refuses
// one that is a link (a symlink / junction planted where the directory
// should be).
func ensureWhatsAppLinkDir(linkID string) (string, error) {
	root := whatsAppRootDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := restrictToOwner(root); err != nil {
		return "", err
	}
	dir, err := whatsAppLinkDir(linkID)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err == nil {
		if isWorktreeLink(info, dir) || !info.IsDir() {
			return "", fmt.Errorf("WhatsApp link directory is not a plain directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if err := restrictToOwner(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// wipeWhatsAppLinkDir deletes a link's directory without following links. An
// absent directory is success.
func wipeWhatsAppLinkDir(linkID string) error {
	dir, err := whatsAppLinkDir(linkID)
	if err != nil {
		return err
	}
	// A file a just-closed SQLite handle released can stay locked for a
	// moment on Windows; retry briefly rather than leave session keys behind.
	for attempt := 0; ; attempt++ {
		_, err = linkSafeRemoveAll(dir)
		if err == nil || attempt == 4 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
	}
}

// listWhatsAppLinkDirs recovers the links held on this computer from their
// session directories, for when links.json cannot be read. Only plain
// directories named with a valid link id count; an absent root is no links.
func listWhatsAppLinkDirs() ([]whatsAppLocalLink, error) {
	entries, err := os.ReadDir(whatsAppRootDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []whatsAppLocalLink
	for _, e := range entries {
		if e.IsDir() && validWhatsAppLinkID(e.Name()) {
			out = append(out, whatsAppLocalLink{LinkID: e.Name()})
		}
	}
	return out, nil
}

/* --------------------------------------------------------------------------
   links.json — which links this agent holds
   -------------------------------------------------------------------------- */

// whatsAppLocalLink is one loaded link. No secrets: the session lives in
// session.db and the outbox key in the OS credential store.
type whatsAppLocalLink struct {
	LinkID      string `json:"linkId"`
	WorkspaceID string `json:"workspaceID"`
	Generation  int64  `json:"generation"`
	State       string `json:"state"`
	// HistoryComplete records that the link-time history sync finished, so a
	// restart reports `connected` instead of `syncingHistory` again.
	HistoryComplete bool `json:"historyComplete,omitempty"`
	// PendingWipe marks a link a deregistration could not fully delete (a
	// locked credential store, a held file). It is never resumed; the next
	// start retries the cleanup until it succeeds.
	PendingWipe bool `json:"pendingWipe,omitempty"`
}

var whatsAppLinksFileMu sync.Mutex

func loadWhatsAppLocalLinks() ([]whatsAppLocalLink, error) {
	whatsAppLinksFileMu.Lock()
	defer whatsAppLinksFileMu.Unlock()
	raw, err := os.ReadFile(filepath.Join(whatsAppRootDir(), whatsAppLinksFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Links []whatsAppLocalLink `json:"links"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	out := file.Links[:0]
	for _, l := range file.Links {
		if validWhatsAppLinkID(l.LinkID) {
			out = append(out, l)
		}
	}
	return out, nil
}

// saveWhatsAppLocalLinks replaces links.json atomically (temp file + rename).
func saveWhatsAppLocalLinks(links []whatsAppLocalLink) error {
	whatsAppLinksFileMu.Lock()
	defer whatsAppLinksFileMu.Unlock()
	root := whatsAppRootDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := restrictToOwner(root); err != nil {
		return err
	}
	if links == nil {
		links = []whatsAppLocalLink{}
	}
	body, err := json.MarshalIndent(struct {
		Links []whatsAppLocalLink `json:"links"`
	}{links}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, "links-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(root, whatsAppLinksFileName)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
