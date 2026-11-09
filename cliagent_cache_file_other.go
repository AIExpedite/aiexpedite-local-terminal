//go:build !windows

package main

// cacheFileReplaceInProgress: a rename on POSIX never refuses a reader.
func cacheFileReplaceInProgress(error) bool { return false }
