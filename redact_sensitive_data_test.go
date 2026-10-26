// File: redact_sensitive_data_test.go
// Unit tests for redactSensitiveData's URL-credential handling. Native stderr
// (git's `fatal: ...`) now reaches the published output, and it can echo a
// remote URL that carries credentials.
package main

import "testing"

func TestRedactSensitiveData_URLCredentials(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"git https remote with token",
			"fatal: unable to access 'https://user:ghs_abc@github.com/o/r.git/': The requested URL returned error: 403",
			"fatal: unable to access 'https://[REDACTED]@github.com/o/r.git/': The requested URL returned error: 403",
		},
		{"database scheme still covered", "postgres://u:p@h/db", "postgres://[REDACTED]@h/db"},
		{"mixed-case scheme", "MongoDB+SRV://admin:hunter2@cluster/x", "MongoDB+SRV://[REDACTED]@cluster/x"},
		{
			"URL without credentials unchanged",
			"Cloning into 'x'... https://github.com/AIExpedite/release-service.git",
			"Cloning into 'x'... https://github.com/AIExpedite/release-service.git",
		},
		{
			"two URLs on separate lines",
			"from https://a:s1@one.example/r\nnext line keeps: text\nto ssh://b:s2@two.example/r",
			"from https://[REDACTED]@one.example/r\nnext line keeps: text\nto ssh://[REDACTED]@two.example/r",
		},
		{"windows path unchanged", `C:\aiexpedite\release-service`, `C:\aiexpedite\release-service`},
		{
			"git destination error unchanged",
			"fatal: destination path 'x' already exists and is not an empty directory.",
			"fatal: destination path 'x' already exists and is not an empty directory.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactSensitiveData(c.in); got != c.want {
				t.Errorf("redactSensitiveData(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
