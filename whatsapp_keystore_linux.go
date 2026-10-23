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

const whatsAppKeystoreProbeAccount = "availability-probe"

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

func (platformWhatsAppKeystore) Get(account string) ([]byte, error) {
	out, code, err := runWhatsAppKeystoreCLI("", "secret-tool", "lookup",
		"service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return nil, err
	}
	// secret-tool exits 1 with no output for a missing item.
	if out == "" {
		return nil, errWhatsAppKeyNotFound
	}
	if code != 0 {
		return nil, fmt.Errorf("secret service read failed (exit %d)", code)
	}
	return decodeWhatsAppKeystoreSecret(out)
}

func (platformWhatsAppKeystore) Delete(account string) error {
	_, code, err := runWhatsAppKeystoreCLI("", "secret-tool", "clear",
		"service", whatsAppKeystoreService, "account", account)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("secret service delete failed (exit %d)", code)
	}
	return nil
}
