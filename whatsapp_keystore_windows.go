//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// Windows Credential Manager: a generic credential "aiexpedite-whatsapp:<linkId>"
// holding the raw 32-byte outbox key, persisted for the local machine only
// (CRED_PERSIST_LOCAL_MACHINE — never roamed with the profile). Read, write
// and delete through advapi32 directly, as antigravity_keyring_windows.go
// reads; no child process, no window.

var (
	procCredWriteW  = advapi32ForKeyring.NewProc("CredWriteW")
	procCredDeleteW = advapi32ForKeyring.NewProc("CredDeleteW")
)

const (
	credPersistLocalMachine = 2
	errorNotFound           = syscall.Errno(1168) // ERROR_NOT_FOUND
)

type platformWhatsAppKeystore struct{}

func whatsAppCredentialTarget(account string) (*uint16, error) {
	return syscall.UTF16PtrFromString(whatsAppKeystoreService + ":" + account)
}

// Available: Credential Manager is part of every supported Windows.
func (platformWhatsAppKeystore) Available() bool { return true }

func (platformWhatsAppKeystore) Put(account string, secret []byte) error {
	target, err := whatsAppCredentialTarget(account)
	if err != nil {
		return err
	}
	user, err := syscall.UTF16PtrFromString(account)
	if err != nil {
		return err
	}
	if len(secret) == 0 {
		return errors.New("empty secret")
	}
	cred := winCredential{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(secret)),
		CredentialBlob:     &secret[0],
		Persist:            credPersistLocalMachine,
		UserName:           user,
	}
	r, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return callErr
	}
	return nil
}

func (platformWhatsAppKeystore) Get(account string) ([]byte, error) {
	target, err := whatsAppCredentialTarget(account)
	if err != nil {
		return nil, err
	}
	var cred *winCredential
	r, _, callErr := procCredReadW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r == 0 || cred == nil {
		if errors.Is(callErr, errorNotFound) {
			return nil, errWhatsAppKeyNotFound
		}
		return nil, callErr
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return nil, errWhatsAppKeyNotFound
	}
	out := make([]byte, cred.CredentialBlobSize)
	copy(out, unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	return out, nil
}

func (platformWhatsAppKeystore) Delete(account string) error {
	target, err := whatsAppCredentialTarget(account)
	if err != nil {
		return err
	}
	r, _, callErr := procCredDeleteW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(callErr, errorNotFound) {
			return errWhatsAppKeyNotFound
		}
		return callErr
	}
	return nil
}
