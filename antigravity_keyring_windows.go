//go:build windows

package main

import (
	"context"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows keyring read for the Antigravity login: the generic credential
// `agy` writes to Credential Manager as "<service>:<user>". Read with
// CredReadW directly — no new dependency, no child process, no window.
//
// The blob is returned as written. `agy` stores UTF-8 JSON; a UTF-16 blob
// (the Credential Manager convention some writers follow) is transcoded so
// the caller always sees the JSON.

var (
	advapi32ForKeyring = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW      = advapi32ForKeyring.NewProc("CredReadW")
	procCredFree       = advapi32ForKeyring.NewProc("CredFree")
)

const credTypeGeneric = 1

// winCredential mirrors CREDENTIALW; only CredentialBlob/Size are read.
type winCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func readAntigravityKeyringCredential(_ context.Context) ([]byte, bool) {
	target, err := syscall.UTF16PtrFromString(antigravityKeyringService + ":" + antigravityKeyringUser)
	if err != nil {
		return nil, false
	}
	var cred *winCredential
	r, _, _ := procCredReadW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r == 0 || cred == nil {
		return nil, false
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return nil, false
	}
	blob := make([]byte, cred.CredentialBlobSize)
	copy(blob, unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	return normalizeCredentialBlob(blob), true
}

// normalizeCredentialBlob transcodes a UTF-16LE blob to UTF-8. A JSON blob
// starts with '{' either way; in UTF-16 that is 0x7B 0x00.
func normalizeCredentialBlob(blob []byte) []byte {
	if len(blob) >= 2 && len(blob)%2 == 0 && blob[1] == 0 && blob[0] != 0 {
		u := make([]uint16, len(blob)/2)
		for i := range u {
			u[i] = uint16(blob[2*i]) | uint16(blob[2*i+1])<<8
		}
		return []byte(string(utf16.Decode(u)))
	}
	return blob
}
