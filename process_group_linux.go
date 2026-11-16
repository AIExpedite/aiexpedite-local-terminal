//go:build linux

// File: process_group_linux.go
// -----------------------------------------------------------------------------
// Linux: the members of a process group, and a process's environment, read
// from /proc. Used only to prove a leftover group is a session's own before it
// is ended (endLeftoverProcessTree).
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
)

// processGroupMembers lists the PIDs whose process group is pgid.
func processGroupMembers(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited meanwhile
		}
		if g, ok := pgrpFromStat(raw); ok && g == pgid {
			out = append(out, pid)
		}
	}
	return out, nil
}

// pgrpFromStat reads field 5 (pgrp) of /proc/<pid>/stat.
func pgrpFromStat(raw []byte) (int, bool) {
	s := string(raw)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(s[end+1:])
	// fields[0] is field 3 (state), [1] ppid, [2] pgrp.
	if len(fields) < 3 {
		return 0, false
	}
	g, err := strconv.Atoi(fields[2])
	return g, err == nil
}

// processEnviron returns a process's environment.
func processEnviron(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("empty environment")
	}
	var out []string
	for _, kv := range bytes.Split(raw, []byte{0}) {
		if len(kv) > 0 {
			out = append(out, string(kv))
		}
	}
	return out, nil
}
