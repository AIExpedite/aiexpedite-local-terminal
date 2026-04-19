// Tests for the registration URL override validation. M3 in the security
// review: TERMINAL_SERVICE_URL with `http://` to a remote host would send
// device-init credentials in cleartext, so the env override is restricted
// to https:// (anywhere) or http:// to loopback only.
package main

import "testing"

func TestIsAllowedRegistrationURL(t *testing.T) {
	cases := []struct {
		url   string
		allow bool
		why   string
	}{
		// Production-style: HTTPS anywhere is fine.
		{"https://api.aiexpedite.com/terminal", true, "https to prod"},
		{"https://api.dev.aiexpedite.com/terminal", true, "https to dev"},
		{"https://example.com", true, "https to anywhere"},
		{"https://localhost:8080", true, "https loopback"},

		// Local dev: HTTP loopback variants — explicitly allowed.
		{"http://localhost:3000", true, "http loopback with port"},
		{"http://localhost/", true, "http loopback no port"},
		{"http://localhost", true, "http loopback bare"},
		{"http://127.0.0.1:8080", true, "http v4 loopback"},
		{"http://127.0.0.1", true, "http v4 loopback bare"},
		{"http://[::1]:8080", true, "http v6 loopback"},
		{"http://[::1]", true, "http v6 loopback bare"},

		// SECURITY: things that must be REJECTED — these are the attack
		// surface this fix is closing.
		{"http://api.aiexpedite.com", false, "http to remote host"},
		{"http://evil.com", false, "http to attacker host"},
		{"http://localhost.evil.com", false, "subdomain trickery"},
		{"http://127.0.0.1.evil.com", false, "ipv4-prefix trickery"},
		{"file:///etc/passwd", false, "file: scheme"},
		{"javascript:alert(1)", false, "javascript: scheme"},
		{"ftp://example.com", false, "ftp"},
		{"", false, "empty string"},
		{"localhost", false, "no scheme"},
		{"//localhost", false, "scheme-relative"},
		{" https://example.com", false, "leading whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			got := isAllowedRegistrationURL(tc.url)
			if got != tc.allow {
				t.Errorf("isAllowedRegistrationURL(%q) = %v, want %v (%s)",
					tc.url, got, tc.allow, tc.why)
			}
		})
	}
}

func TestGetRegistrationURL_OverrideAccepted(t *testing.T) {
	t.Setenv("TERMINAL_SERVICE_URL", "https://override.example.com/terminal")
	if got := getRegistrationURL(); got != "https://override.example.com/terminal" {
		t.Errorf("getRegistrationURL() with valid override = %q, want override URL", got)
	}
}

func TestGetRegistrationURL_BadOverrideFallsBack(t *testing.T) {
	// Bad override → fall back to EnvAPIEndpoint, NOT silently use the bad URL.
	t.Setenv("TERMINAL_SERVICE_URL", "http://evil.example.com")
	got := getRegistrationURL()
	if got != EnvAPIEndpoint {
		t.Errorf("getRegistrationURL() with rejected override = %q, want EnvAPIEndpoint=%q",
			got, EnvAPIEndpoint)
	}
}

func TestGetRegistrationURL_UnsetUsesBuildTimeEndpoint(t *testing.T) {
	t.Setenv("TERMINAL_SERVICE_URL", "")
	if got := getRegistrationURL(); got != EnvAPIEndpoint {
		t.Errorf("getRegistrationURL() with unset env = %q, want EnvAPIEndpoint=%q",
			got, EnvAPIEndpoint)
	}
}
