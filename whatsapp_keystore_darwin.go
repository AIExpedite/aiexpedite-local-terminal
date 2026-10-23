//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os/exec"
)

// macOS Keychain through `security`, like the existing keychain probes. The add
// goes through `security -i`, which reads its command line from stdin, so the
// key never appears in argv.

type platformWhatsAppKeystore struct{}

const securityItemNotFound = 44 // errSecItemNotFound as `security` exits

// Available proves a full round trip, because `security` on PATH says nothing
// about whether the Keychain accepts a write (a locked or noninteractive
// session refuses it).
func (ks platformWhatsAppKeystore) Available() bool {
	if _, err := exec.LookPath("security"); err != nil {
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
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		whatsAppKeystoreService, account, encodeWhatsAppKeystoreSecret(secret))
	_, code, err := runWhatsAppKeystoreCLI(line, "security", "-i")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("keychain write failed (exit %d)", code)
	}
	return nil
}

func (platformWhatsAppKeystore) Get(account string) ([]byte, error) {
	out, code, err := runWhatsAppKeystoreCLI("", "security", "find-generic-password",
		"-s", whatsAppKeystoreService, "-a", account, "-w")
	if err != nil {
		return nil, err
	}
	if code == securityItemNotFound || (code == 0 && out == "") {
		return nil, errWhatsAppKeyNotFound
	}
	if code != 0 {
		return nil, fmt.Errorf("keychain read failed (exit %d)", code)
	}
	return decodeWhatsAppKeystoreSecret(out)
}

func (platformWhatsAppKeystore) Delete(account string) error {
	_, code, err := runWhatsAppKeystoreCLI("", "security", "delete-generic-password",
		"-s", whatsAppKeystoreService, "-a", account)
	if err != nil {
		return err
	}
	if code == securityItemNotFound {
		return errWhatsAppKeyNotFound
	}
	if code != 0 {
		return fmt.Errorf("keychain delete failed (exit %d)", code)
	}
	return nil
}
