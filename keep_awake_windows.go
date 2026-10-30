//go:build windows
// +build windows

// File: keep_awake_windows.go
// -----------------------------------------------------------------------------
// Windows keep-awake (keep_awake.go): a power request of type
// PowerRequestSystemRequired (idle sleep only; the display may still turn
// off, and a sleep the person asks for still happens).
//
// Chosen over SetThreadExecutionState, which is PER OS THREAD: goroutines move
// between threads, so the state would follow whichever thread the call ran on.
// A power request is a handle; it is cleared and closed on release, and the OS
// drops it with the process if the agent dies.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procPowerCreateRequest = windows.NewLazySystemDLL("kernel32.dll").NewProc("PowerCreateRequest")
	procPowerSetRequest    = windows.NewLazySystemDLL("kernel32.dll").NewProc("PowerSetRequest")
	procPowerClearRequest  = windows.NewLazySystemDLL("kernel32.dll").NewProc("PowerClearRequest")
)

const (
	powerRequestContextVersion      = 0   // POWER_REQUEST_CONTEXT_VERSION
	powerRequestContextSimpleString = 0x1 // POWER_REQUEST_CONTEXT_SIMPLE_STRING
	powerRequestSystemRequired      = 1   // POWER_REQUEST_TYPE PowerRequestSystemRequired
)

// reasonContext is REASON_CONTEXT with the simple-string arm of its union,
// padded to the union's full size (the detailed arm: HMODULE, two ULONGs and
// a pointer).
type reasonContext struct {
	Version            uint32
	Flags              uint32
	SimpleReasonString *uint16
	_                  [2 * unsafe.Sizeof(uintptr(0))]byte
}

// A power request never ends on its own, so onLost is never called.
func acquirePlatformKeepAwake(func(error)) (func(), error) {
	for _, proc := range []*windows.LazyProc{procPowerCreateRequest, procPowerSetRequest, procPowerClearRequest} {
		if err := proc.Find(); err != nil {
			return nil, err
		}
	}
	reason, err := windows.UTF16PtrFromString(keepAwakeReason)
	if err != nil {
		return nil, err
	}
	ctx := reasonContext{
		Version:            powerRequestContextVersion,
		Flags:              powerRequestContextSimpleString,
		SimpleReasonString: reason,
	}
	r, _, callErr := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	handle := windows.Handle(r)
	if handle == windows.InvalidHandle || handle == 0 {
		return nil, fmt.Errorf("PowerCreateRequest: %w", callErr)
	}
	if ok, _, callErr := procPowerSetRequest.Call(uintptr(handle), powerRequestSystemRequired); ok == 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("PowerSetRequest: %w", callErr)
	}
	return func() {
		_, _, _ = procPowerClearRequest.Call(uintptr(handle), powerRequestSystemRequired)
		_ = windows.CloseHandle(handle)
	}, nil
}
