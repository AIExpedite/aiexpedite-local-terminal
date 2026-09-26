package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
	shaC = "3333333333333333333333333333333333333333"
	shaD = "4444444444444444444444444444444444444444"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeRepo lays out a minimal .git directory.
func makeRepo(t *testing.T, dir, config, head string, refs map[string]string, packed string) {
	t.Helper()
	g := filepath.Join(dir, ".git")
	writeFile(t, filepath.Join(g, "config"), config)
	writeFile(t, filepath.Join(g, "HEAD"), head+"\n")
	for ref, sha := range refs {
		writeFile(t, filepath.Join(g, filepath.FromSlash(ref)), sha+"\n")
	}
	if packed != "" {
		writeFile(t, filepath.Join(g, "packed-refs"), packed)
	}
}

func TestFindRepositoryCheckouts(t *testing.T) {
	clone := t.TempDir()
	home := t.TempDir()

	// Loose ref, two remotes, a token in the https URL.
	makeRepo(t, filepath.Join(clone, "frontend"), `[core]
	bare = false
[remote "origin"]
	url = https://x-access-token:ghs_SECRETSECRET@github.com/AIExpedite/frontend.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[remote "upstream"]
	url = git@github.com:upstream/frontend.git
	url = https://second.example/ignored.git
[branch "dev"]
	remote = origin
`, "ref: refs/heads/dev", map[string]string{"refs/heads/dev": shaA}, "")

	// Packed ref only.
	makeRepo(t, filepath.Join(clone, "terminal-service"), `[remote "origin"]
	url = https://github.com/AIExpedite/terminal-service ; trailing comment
`, "ref: refs/heads/main", nil, "# pack-refs with: peeled fully-peeled sorted\n"+shaB+" refs/heads/main\n^"+shaC+"\n")

	// Detached HEAD, no remotes.
	makeRepo(t, filepath.Join(clone, "detached"), "[core]\n", shaC, nil, "")

	// Unborn branch: HEAD names a branch with no commits.
	makeRepo(t, filepath.Join(clone, "fresh"), "[core]\n", "ref: refs/heads/main", nil, "")

	// A linked worktree: .git is a file; HEAD lives in the worktree git dir,
	// config and refs in the common dir.
	mainRepo := filepath.Join(home, "local")
	makeRepo(t, mainRepo, `[remote "origin"]
	url = https://github.com/AIExpedite/local.git
`, "ref: refs/heads/main", map[string]string{"refs/heads/main": shaA, "refs/heads/docs/x": shaD}, "")
	wtGit := filepath.Join(mainRepo, ".git", "worktrees", "wt")
	writeFile(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/docs/x\n")
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	wt := filepath.Join(clone, "local-wt")
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")

	// Not checkouts / too deep for depth 1.
	if err := os.MkdirAll(filepath.Join(clone, "plain-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeRepo(t, filepath.Join(clone, "group", "nested"), `[remote "origin"]
	url = https://github.com/o/nested.git
`, "ref: refs/heads/main", map[string]string{"refs/heads/main": shaB}, "")
	writeFile(t, filepath.Join(clone, "a-file.txt"), "x")

	res, err := findRepositoryCheckouts(context.Background(), envFindReposRequest{
		Roots: []string{clone, home, clone, "relative/ignored", filepath.Join(home, "does-not-exist")},
		Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Fatal("unexpected truncation")
	}
	got := map[string]envCheckout{}
	for _, c := range res.Checkouts {
		got[filepath.Base(c.Path)] = c
	}
	wantNames := []string{"detached", "fresh", "frontend", "local", "local-wt", "terminal-service"}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	if len(names) != len(wantNames) {
		t.Fatalf("checkouts %v, want %v", names, wantNames)
	}
	for _, n := range wantNames {
		if _, ok := got[n]; !ok {
			t.Fatalf("missing checkout %s in %v", n, names)
		}
	}

	fe := got["frontend"]
	if fe.Path != filepath.Join(clone, "frontend") || fe.HeadSha != shaA || fe.Branch != "dev" {
		t.Fatalf("frontend = %+v", fe)
	}
	wantRemotes := map[string]string{
		"origin":   "https://github.com/AIExpedite/frontend.git",
		"upstream": "git@github.com:upstream/frontend.git",
	}
	if !reflect.DeepEqual(fe.Remotes, wantRemotes) {
		t.Fatalf("frontend remotes = %v, want %v", fe.Remotes, wantRemotes)
	}
	if ts := got["terminal-service"]; ts.HeadSha != shaB || ts.Branch != "main" ||
		ts.Remotes["origin"] != "https://github.com/AIExpedite/terminal-service" {
		t.Fatalf("terminal-service = %+v", ts)
	}
	if d := got["detached"]; d.HeadSha != shaC || d.Branch != "" || len(d.Remotes) != 0 || d.Remotes == nil {
		t.Fatalf("detached = %+v (remotes must be {} not null)", d)
	}
	if f := got["fresh"]; f.HeadSha != "" || f.Branch != "main" {
		t.Fatalf("fresh = %+v", f)
	}
	if w := got["local-wt"]; w.HeadSha != shaD || w.Branch != "docs/x" || w.Remotes["origin"] != "https://github.com/AIExpedite/local.git" {
		t.Fatalf("worktree = %+v", w)
	}

	// The JSON result never carries the token.
	out, err := encodeRedactedJSON(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "\n") {
		t.Fatalf("encoded result leaked a credential or spans lines: %s", out)
	}

	// Depth 2 reaches group/nested; a checkout is never descended into.
	res, err = findRepositoryCheckouts(context.Background(), envFindReposRequest{Roots: []string{clone}, Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range res.Checkouts {
		if c.Path == filepath.Join(clone, "group", "nested") {
			found = true
		}
	}
	if !found {
		t.Fatalf("depth 2 missed group/nested: %+v", res.Checkouts)
	}
}

func TestFindRepositoryCheckouts_CapAndInputs(t *testing.T) {
	if _, err := findRepositoryCheckouts(context.Background(), envFindReposRequest{}); err == nil {
		t.Fatal("expected an error without roots")
	}
	root := t.TempDir()
	for i := 0; i < maxFindReposCheckouts+5; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("r%04d", i), ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	res, err := findRepositoryCheckouts(context.Background(), envFindReposRequest{Roots: []string{root}, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Checkouts) != maxFindReposCheckouts || !res.Truncated {
		t.Fatalf("got %d checkouts truncated=%v, want %d and truncated", len(res.Checkouts), res.Truncated, maxFindReposCheckouts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := findRepositoryCheckouts(ctx, envFindReposRequest{Roots: []string{root}}); err == nil {
		t.Fatal("expected a cancelled context to stop the scan")
	}
}

func TestParseGitRemotesAndSanitize(t *testing.T) {
	got := parseGitRemotes([]byte(`# comment
[remote "with \"quote"]
	url = "https://user:pw@example.com/a.git"
[Remote "Case"]
	URL = ssh://git:pw@example.com/b.git
[remote "nourl"]
	fetch = x
[remote "empty"]
`))
	want := map[string]string{
		`with "quote`: "https://example.com/a.git",
		"Case":        "ssh://git@example.com/b.git",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSanitizeRemoteURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// http(s): userinfo dropped entirely.
		{"https://ghp_abc@github.com/o/r.git", "https://github.com/o/r.git"},
		{"http://u:p@h.example/x?token=keep-me", "http://h.example/x?token=keep-me"},
		{"https://x-access-token:ghs_SECRET@github.com/o/r", "https://github.com/o/r"},
		// Malformed userinfo that url.Parse rejects: still stripped.
		{"https://user:secret%oops@example.com/repo.git", "https://example.com/repo.git"},
		{"https://user:%zz%@example.com", "https://example.com"},
		// Several `@` in the userinfo: everything up to the last one in the authority goes.
		{"https://user:p@ss@w@rd@example.com/r.git", "https://example.com/r.git"},
		{"https://tok@en@example.com:8443/r.git?x=1#f", "https://example.com:8443/r.git?x=1#f"},
		{"git+https://u:p@example.com/r.git", "git+https://example.com/r.git"},
		// A password containing `/` can't be delimited: redacted.
		{"https://user:pa/ss@example.com/r.git", redactedRemote},
		// Nothing but userinfo: redacted.
		{"https://user:pw@", redactedRemote},
		// No credentials: unchanged.
		{"https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"https://example.com:8443/group/r.git", "https://example.com:8443/group/r.git"},
		// ssh:// keeps a password-free username.
		{"ssh://git@github.com/o/r.git", "ssh://git@github.com/o/r.git"},
		{"ssh://git:pw@example.com:2222/r.git", "ssh://git@example.com:2222/r.git"},
		{"ssh://git:p%w@d@example.com/r.git", "ssh://git@example.com/r.git"},
		{"ssh://:pw@example.com/r.git", "ssh://example.com/r.git"},
		// scp-like forms.
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"github.com:o/r.git", "github.com:o/r.git"},
		{"user:pass@host.example:o/r.git", "user@host.example:o/r.git"},
		{"user:p@ss@host.example:o/r.git", "user@host.example:o/r.git"},
		{"user:pass@host.example:a/@b.git", "user@host.example:a/@b.git"},
		{"user:pass@nohostpath", redactedRemote},
		// Local paths pass through, `@` and all.
		{"/srv/git/r.git", "/srv/git/r.git"},
		{`C:\Users\a@b\repo`, `C:\Users\a@b\repo`},
		{"./r@x", "./r@x"},
		{"../a:b@c", "../a:b@c"},
	}
	secrets := []string{"secret", "pass", "SECRET", "ghp_abc", "p@ss", ":pw", "%zz%", "tok@en"}
	for _, c := range cases {
		got := sanitizeRemoteURL(c.in)
		if got != c.want {
			t.Errorf("sanitizeRemoteURL(%q) = %q, want %q", c.in, got, c.want)
		}
		if got == c.in {
			continue
		}
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("sanitizeRemoteURL(%q) = %q still carries %q", c.in, got, s)
			}
		}
	}
	// Through the config parser too, a malformed credential never survives.
	got := parseGitRemotes([]byte("[remote \"origin\"]\n\turl = https://user:secret%oops@example.com/repo.git\n"))
	if got["origin"] != "https://example.com/repo.git" {
		t.Fatalf("parsed %v", got)
	}
}
