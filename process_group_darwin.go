//go:build darwin

// File: process_group_darwin.go
// -----------------------------------------------------------------------------
// macOS: the members of a process group (sysctl kern.proc.pgrp), and a
// process's environment (sysctl kern.procargs2). Used only to prove a leftover
// group is a session's own before it is ended (endLeftoverProcessTree).
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// processGroupMembers lists the processes whose process group is pgid, each
// marked exited when it is a zombie: such a member has no environment to read
// and can neither run nor be signalled.
func processGroupMembers(pgid int) ([]groupMember, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	out := make([]groupMember, 0, len(procs))
	for _, kp := range procs {
		if kp.Proc.P_pid > 0 {
			out = append(out, groupMember{
				PID:    int(kp.Proc.P_pid),
				Exited: kp.Proc.P_stat == darwinZombieState,
			})
		}
	}
	return out, nil
}

// processEnviron returns a process's environment from kern.procargs2:
// argc (int32), the exec path, NUL padding, argc arguments, then the
// environment strings, each NUL-terminated, ended by an empty string.
func processEnviron(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2Env(raw)
}

func parseProcArgs2Env(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, errors.New("short procargs2")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	// The exec path, then NUL padding.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, errors.New("malformed procargs2")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	// argc arguments.
	for n := 0; n < argc; n++ {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil, errors.New("malformed procargs2 argv")
		}
		rest = rest[j+1:]
	}
	var env []string
	for len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j <= 0 {
			break
		}
		env = append(env, string(rest[:j]))
		rest = rest[j+1:]
	}
	return env, nil
}
