//go:build darwin || linux

package main

import (
	"errors"
	"strings"
	"testing"
)

// A scripted `secret-tool`: lookup, clear and search answers by subcommand.
type scriptedSecretTool struct {
	lookupOut, searchOut              string
	lookupCode, clearCode, searchCode int
	searched                          bool
}

func (s *scriptedSecretTool) run(_ string, name string, args ...string) (string, int, error) {
	if name != "secret-tool" || len(args) == 0 {
		return "", -1, errors.New("unexpected command")
	}
	switch args[0] {
	case "lookup":
		return s.lookupOut, s.lookupCode, nil
	case "clear":
		return "", s.clearCode, nil
	case "search":
		s.searched = true
		return s.searchOut, s.searchCode, nil
	}
	return "", -1, errors.New("unexpected subcommand")
}

// Only an item that is really absent reads as errWhatsAppKeyNotFound: a
// failed, locked or timed-out Secret Service must never look like a deleted
// key, or the outbox would discard its queued rows.
func TestReadSecretToolKeyTellsMissingFromUnreadable(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	cases := []struct {
		name       string
		tool       scriptedSecretTool
		want       []byte
		notFound   bool
		wantSearch bool
	}{
		{name: "found", tool: scriptedSecretTool{lookupOut: encodeWhatsAppKeystoreSecret(key)}, want: key},
		{name: "missing: lookup and search both silent", tool: scriptedSecretTool{lookupCode: 1}, notFound: true, wantSearch: true},
		{name: "locked item: lookup silent, search lists it", tool: scriptedSecretTool{lookupCode: 1, searchOut: "[/org/freedesktop/secrets/collection/login/7]\nlabel = AI Expedite WhatsApp outbox key"}, wantSearch: true},
		{name: "service failure: search fails", tool: scriptedSecretTool{lookupCode: 1, searchCode: 1}, wantSearch: true},
		{name: "killed by the timeout", tool: scriptedSecretTool{lookupCode: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := c.tool
			got, err := readSecretToolKey(tool.run, "lnk_1")
			if tool.searched != c.wantSearch {
				t.Fatalf("searched = %v, want %v", tool.searched, c.wantSearch)
			}
			switch {
			case c.want != nil:
				if err != nil || string(got) != string(c.want) {
					t.Fatalf("got %q, %v", got, err)
				}
			case c.notFound:
				if !errors.Is(err, errWhatsAppKeyNotFound) {
					t.Fatalf("a missing item must be errWhatsAppKeyNotFound, got %v", err)
				}
			default:
				if err == nil || errors.Is(err, errWhatsAppKeyNotFound) {
					t.Fatalf("an unreadable item must be an error other than not-found, got %v", err)
				}
				if strings.Contains(err.Error(), encodeWhatsAppKeystoreSecret(key)) {
					t.Fatal("the error carries key material")
				}
			}
		})
	}
}

// `secret-tool clear` exits 1 when nothing matched as well as when the
// service failed. Only a key that is really absent may read as deleted
// (errWhatsAppKeyNotFound), or every idempotent cleanup — a replayed unlink,
// a retried pending wipe — would fail forever and never ack.
func TestClearSecretToolKeyIsIdempotent(t *testing.T) {
	locked := "[/org/freedesktop/secrets/collection/login/7]\nlabel = AI Expedite WhatsApp outbox key"
	cases := []struct {
		name       string
		tool       scriptedSecretTool
		ok         bool // deleted now
		absent     bool // already absent: errWhatsAppKeyNotFound
		wantSearch bool
	}{
		{name: "deleted", tool: scriptedSecretTool{}, ok: true},
		{name: "already absent", tool: scriptedSecretTool{clearCode: 1}, absent: true, wantSearch: true},
		{name: "locked item still there", tool: scriptedSecretTool{clearCode: 1, searchOut: locked}, wantSearch: true},
		{name: "service failure", tool: scriptedSecretTool{clearCode: 1, searchCode: 1}, wantSearch: true},
		{name: "killed by the timeout", tool: scriptedSecretTool{clearCode: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := c.tool
			err := clearSecretToolKey(tool.run, "lnk_1")
			if tool.searched != c.wantSearch {
				t.Fatalf("searched = %v, want %v", tool.searched, c.wantSearch)
			}
			switch {
			case c.ok:
				if err != nil {
					t.Fatalf("a successful clear returned %v", err)
				}
			case c.absent:
				if !errors.Is(err, errWhatsAppKeyNotFound) {
					t.Fatalf("an absent key must be errWhatsAppKeyNotFound, got %v", err)
				}
			default:
				if err == nil || errors.Is(err, errWhatsAppKeyNotFound) {
					t.Fatalf("a failed clear must be an error other than not-found, got %v", err)
				}
			}
		})
	}
}
