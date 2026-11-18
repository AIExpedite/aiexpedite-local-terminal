//go:build !windows

package main

import "path/filepath"

// realDirPath is path with every symlink resolved (filepath.EvalSymlinks), so
// dirWithin walks a path with no link left in it. Windows has its own, which
// resolves junctions too.
func realDirPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
