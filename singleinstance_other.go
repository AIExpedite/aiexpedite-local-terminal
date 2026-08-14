//go:build !windows

package main

// Other platforms rely on their native application launcher behavior during
// relocation. Windows needs an explicit per-account guard because launching an
// executable directly always creates a new process.
func acquireAgentInstance() (release func(), acquired bool) {
	return func() {}, true
}
