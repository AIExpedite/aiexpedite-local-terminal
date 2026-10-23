// whatsapp_keystore.go — the per-link outbox key in the OS credential store.
//
// Each WhatsApp link's outbox (whatsapp_outbox.go) is encrypted under a random
// 32-byte key that never touches the disk: it lives in Windows Credential
// Manager, the macOS Keychain or the Linux Secret Service, under service
// `aiexpedite-whatsapp` and account `<linkId>`. A computer whose credential
// store is unusable (Linux without a Secret Service provider) is refused at
// link time with WHATSAPP_SECURE_STORAGE_UNAVAILABLE — this agent never falls
// back to a plaintext key.
//
// The first keystore WRITE path in the agent; the platform files are
// whatsapp_keystore_{windows,darwin,linux,other}.go.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
)

const whatsAppKeystoreService = "aiexpedite-whatsapp"

var errWhatsAppKeyNotFound = errors.New("whatsapp outbox key not found")

// whatsAppKeystore is the credential-store seam. The platform implementation
// is platformWhatsAppKeystore; tests install a memory store.
type whatsAppKeystore interface {
	Available() bool
	Put(account string, secret []byte) error
	// Get returns errWhatsAppKeyNotFound when the account has no entry.
	Get(account string) ([]byte, error)
	Delete(account string) error
}

var (
	whatsAppKeystoreMu      sync.Mutex
	activeWhatsAppKeystore  whatsAppKeystore = platformWhatsAppKeystore{}
	whatsAppKeystoreProbed  bool
	whatsAppKeystoreUsable  bool
)

func currentWhatsAppKeystore() whatsAppKeystore {
	whatsAppKeystoreMu.Lock()
	defer whatsAppKeystoreMu.Unlock()
	return activeWhatsAppKeystore
}

// setWhatsAppKeystoreForTest swaps the store and returns a restore func.
func setWhatsAppKeystoreForTest(ks whatsAppKeystore) func() {
	whatsAppKeystoreMu.Lock()
	prev, prevProbed, prevUsable := activeWhatsAppKeystore, whatsAppKeystoreProbed, whatsAppKeystoreUsable
	activeWhatsAppKeystore, whatsAppKeystoreProbed = ks, false
	whatsAppKeystoreMu.Unlock()
	return func() {
		whatsAppKeystoreMu.Lock()
		activeWhatsAppKeystore, whatsAppKeystoreProbed, whatsAppKeystoreUsable = prev, prevProbed, prevUsable
		whatsAppKeystoreMu.Unlock()
	}
}

// whatsAppSecureStorageAvailable is the capability reported to the cloud
// (capabilities.whatsapp.secureStorage). Probed once per process: the answer
// only changes when the person installs a keyring, and the next agent start
// picks that up.
func whatsAppSecureStorageAvailable() bool {
	whatsAppKeystoreMu.Lock()
	if whatsAppKeystoreProbed {
		usable := whatsAppKeystoreUsable
		whatsAppKeystoreMu.Unlock()
		return usable
	}
	ks := activeWhatsAppKeystore
	whatsAppKeystoreMu.Unlock()
	usable := ks.Available()
	whatsAppKeystoreMu.Lock()
	whatsAppKeystoreProbed, whatsAppKeystoreUsable = true, usable
	whatsAppKeystoreMu.Unlock()
	return usable
}

// loadOrCreateWhatsAppOutboxKey returns the link's outbox key, creating it on
// first use. create=false (an existing outbox) never mints a new key: a key
// that vanished means the rows are unreadable, which the caller handles by
// discarding them rather than silently re-keying.
func loadOrCreateWhatsAppOutboxKey(linkID string, create bool) ([]byte, error) {
	ks := currentWhatsAppKeystore()
	key, err := ks.Get(linkID)
	if err == nil && len(key) == 32 {
		return key, nil
	}
	if err != nil && !errors.Is(err, errWhatsAppKeyNotFound) {
		return nil, err
	}
	if !create {
		return nil, errWhatsAppKeyNotFound
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := ks.Put(linkID, key); err != nil {
		return nil, err
	}
	return key, nil
}

func deleteWhatsAppOutboxKey(linkID string) error {
	err := currentWhatsAppKeystore().Delete(linkID)
	if errors.Is(err, errWhatsAppKeyNotFound) {
		return nil
	}
	return err
}

// The CLI-backed stores (macOS `security`, Linux `secret-tool`) carry the key
// as base64 text; Credential Manager stores the raw bytes.
func encodeWhatsAppKeystoreSecret(secret []byte) string {
	return base64.StdEncoding.EncodeToString(secret)
}

func decodeWhatsAppKeystoreSecret(text string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(text)
}

// memoryWhatsAppKeystore is the test store.
type memoryWhatsAppKeystore struct {
	mu          sync.Mutex
	entries     map[string][]byte
	unavailable bool
}

func newMemoryWhatsAppKeystore() *memoryWhatsAppKeystore {
	return &memoryWhatsAppKeystore{entries: map[string][]byte{}}
}

func (m *memoryWhatsAppKeystore) Available() bool { return !m.unavailable }

func (m *memoryWhatsAppKeystore) Put(account string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return errors.New("keystore unavailable")
	}
	m.entries[account] = append([]byte(nil), secret...)
	return nil
}

func (m *memoryWhatsAppKeystore) Get(account string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[account]
	if !ok {
		return nil, errWhatsAppKeyNotFound
	}
	return append([]byte(nil), v...), nil
}

func (m *memoryWhatsAppKeystore) Delete(account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, account)
	return nil
}
