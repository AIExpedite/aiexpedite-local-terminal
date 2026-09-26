//go:build !windows

package main

// platformPersistedPathSource: macOS and Linux have no persisted PATH store an
// installer writes to; refreshCommandPath probes well-known directories instead.
func platformPersistedPathSource() persistedPathSource {
	return nil
}
