//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os/exec"
)

// Linux Secret Service through `secret-tool` (libsecret). `store` reads the
// secret from stdin. A computer without a Secret Service provider (no
// secret-tool, no session bus, no unlocked keyring) is NOT available, and
// linking is refused with WHATSAPP_SECURE_STORAGE_UNAVAILABLE — never a
// plaintext fallback.

type platformWhatsAppKeystore struct{}

// Available proves a full round trip, because `secret-tool` on PATH says
// nothing about whether a provider answers on the session bus.
func (ks platformWhatsAppKeystore) Available() bool {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return false
	}
	probe := []byte("probe")
	if err := ks.Put(whatsAppKeystoreProbeAccount, probe); err != nil {
		return false
	}
	defer ks.Delete(whatsAppKeystoreProbeAccount)
	got, err := ks.Get(whatsAppKeystoreProbeAccount)
	return err == nil && bytes.Equal(got, probe)
}

func (platformWhatsAppKeystore) Put(account string, secret []byte) error {
	_, code, err := runWhatsAppKeystoreCLI(encodeWhatsAppKeystoreSecret(secret), "secret-tool", "store",
		"--label=AI Expedite WhatsApp outbox key", "service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("secret service write failed (exit %d)", code)
	}
	return nil
}

// Get reports errWhatsAppKeyNotFound only for an item that is really absent
// (readSecretToolKey): a failed or locked Secret Service is an error, never a
// deleted key.
func (platformWhatsAppKeystore) Get(account string) ([]byte, error) {
	return readSecretToolKey(runWhatsAppKeystoreCLI, account)
}

// Delete reports an absent key as errWhatsAppKeyNotFound (clearSecretToolKey),
// so deleteWhatsAppOutboxKey stays idempotent: `secret-tool clear` alone
// exits 1 for nothing-to-clear and for a failed service alike.
func (platformWhatsAppKeystore) Delete(account string) error {
	return clearSecretToolKey(runWhatsAppKeystoreCLI, account)
}
