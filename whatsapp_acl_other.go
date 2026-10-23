//go:build !windows

package main

import "os"

// restrictToOwner makes a WhatsApp directory readable by its owner only.
func restrictToOwner(path string) error {
	return os.Chmod(path, 0o700)
}
