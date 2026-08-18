//go:build !windows

package main

import "os"

func atomicReplaceConfigFile(source, destination string) error {
	return os.Rename(source, destination)
}
