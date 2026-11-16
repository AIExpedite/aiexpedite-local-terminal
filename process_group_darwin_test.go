//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"reflect"
	"testing"
)

func TestParseProcArgs2Env(t *testing.T) {
	var raw []byte
	argc := make([]byte, 4)
	binary.LittleEndian.PutUint32(argc, 2)
	raw = append(raw, argc...)
	raw = append(raw, []byte("/usr/bin/x\x00\x00\x00\x00")...)
	raw = append(raw, []byte("x\x00--flag\x00")...)
	raw = append(raw, []byte("A=1\x00"+sessionOwnerEnvVar+"=b:s\x00\x00junk")...)
	env, err := parseProcArgs2Env(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", sessionOwnerEnvVar + "=b:s"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
}

// The real sysctl answers for this very process.
func TestProcessEnvironOfSelf(t *testing.T) {
	t.Setenv("AIX_PROCARGS_TEST", "1")
	env, err := processEnviron(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if len(env) == 0 {
		t.Fatalf("no environment read for this process")
	}
}
