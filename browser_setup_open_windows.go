//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Chrome's installer registers App Paths\chrome.exe (per user or per machine);
// its default value is the full path of chrome.exe.
const chromeAppPathsKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe`

func init() {
	windowsChromeAppPaths = func() []string {
		var out []string
		for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
			k, err := registry.OpenKey(root, chromeAppPathsKey, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			if v, _, err := k.GetStringValue(""); err == nil && v != "" {
				out = append(out, strings.Trim(v, `"`))
			}
			k.Close()
		}
		return out
	}
}
