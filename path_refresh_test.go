package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type fakePathSource struct{ machine, user string }

func (f fakePathSource) MachinePath() string { return f.machine }
func (f fakePathSource) UserPath() string    { return f.user }

func TestMergePathEntries_FirstWinsDedupe(t *testing.T) {
	got := mergePathEntries(true,
		[]string{`C:\Tools`, ``, `C:\Windows\System32`},
		[]string{`c:\tools\`, `C:\Program Files\nodejs\`, ` C:\WINDOWS\system32 `},
		[]string{`C:\Users\me\AppData\Roaming\npm`, `C:\Program Files\nodejs`},
	)
	want := []string{`C:\Tools`, `C:\Windows\System32`, `C:\Program Files\nodejs\`, `C:\Users\me\AppData\Roaming\npm`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	// Case-sensitive (POSIX): /usr/Bin and /usr/bin are different directories.
	got = mergePathEntries(false, []string{"/usr/bin", "/usr/Bin", "/usr/bin/"})
	want = []string{"/usr/bin", "/usr/Bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("case-sensitive: got %q want %q", got, want)
	}

	// A bare root survives the trailing-separator normalization.
	if got := mergePathEntries(false, []string{"/", "/"}); !reflect.DeepEqual(got, []string{"/"}) {
		t.Fatalf("root: got %q", got)
	}
}

func TestWindowsRefreshedPath(t *testing.T) {
	tests := []struct {
		name    string
		current string
		src     persistedPathSource
		want    string
	}{
		{
			name:    "own entries first, then machine, then user",
			current: `C:\agent;C:\Windows\system32`,
			src: fakePathSource{
				machine: `C:\Windows\System32;C:\Program Files\nodejs\;C:\Program Files\Git\cmd`,
				user:    `C:\Users\me\AppData\Roaming\npm;C:\Users\me\AppData\Local\Microsoft\WinGet\Links`,
			},
			want: `C:\agent;C:\Windows\system32;C:\Program Files\nodejs\;C:\Program Files\Git\cmd;C:\Users\me\AppData\Roaming\npm;C:\Users\me\AppData\Local\Microsoft\WinGet\Links`,
		},
		{
			name:    "an entry already on the agent's PATH keeps its position",
			current: `C:\Program Files\nodejs;C:\agent`,
			src:     fakePathSource{machine: `C:\agent;C:\PROGRAM FILES\NODEJS\`},
			want:    `C:\Program Files\nodejs;C:\agent`,
		},
		{
			name:    "no source leaves the list deduped",
			current: `C:\a;;C:\A\;C:\b`,
			src:     nil,
			want:    `C:\a;C:\b`,
		},
		{
			name:    "empty registry values change nothing",
			current: `C:\a`,
			src:     fakePathSource{},
			want:    `C:\a`,
		},
		{
			name:    "empty agent PATH takes the registry's",
			current: ``,
			src:     fakePathSource{machine: `C:\m`, user: `C:\u`},
			want:    `C:\m;C:\u`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowsRefreshedPath(tt.current, tt.src); got != tt.want {
				t.Fatalf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestUnixRefreshedPath(t *testing.T) {
	exists := map[string]bool{
		"/opt/homebrew/bin":          true,
		"/usr/local/bin":             true,
		"/home/me/.local/bin":        false,
		"/home/me/.npm-global/bin":   true,
		"/already/on/path/elsewhere": true,
	}
	dirExists := func(p string) bool { return exists[p] }
	candidates := []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/me/.local/bin", "/home/me/.npm-global/bin"}

	got := unixRefreshedPath("/usr/bin:/bin:/usr/local/bin", candidates, dirExists)
	want := "/opt/homebrew/bin:/home/me/.npm-global/bin:/usr/bin:/bin:/usr/local/bin"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// Idempotent: a second refresh changes nothing.
	if again := unixRefreshedPath(got, candidates, dirExists); again != got {
		t.Fatalf("second refresh moved PATH: %q -> %q", got, again)
	}
	// Nothing exists: PATH unchanged.
	if got := unixRefreshedPath("/usr/bin:/bin", candidates, func(string) bool { return false }); got != "/usr/bin:/bin" {
		t.Fatalf("got %q", got)
	}
}

func TestUnixPathCandidates(t *testing.T) {
	got := unixPathCandidates("darwin", "/home/me", "/home/me/.npm-global")
	want := []string{"/opt/homebrew/bin", "/usr/local/bin",
		filepath.Join("/home/me", ".local", "bin"), filepath.Join("/home/me/.npm-global", "bin")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("darwin: got %q want %q", got, want)
	}
	// Linux adds Homebrew-on-Linux's system and per-user prefixes.
	got = unixPathCandidates("linux", "/home/me", "")
	want = []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin",
		filepath.Join("/home/me", ".linuxbrew", "bin"), filepath.Join("/home/me", ".local", "bin")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("linux: got %q want %q", got, want)
	}
	if got := unixPathCandidates("linux", "", ""); !reflect.DeepEqual(got, []string{"/opt/homebrew/bin", "/usr/local/bin", "/home/linuxbrew/.linuxbrew/bin"}) {
		t.Fatalf("linux no home: got %q", got)
	}
	if got := unixPathCandidates("darwin", "", ""); !reflect.DeepEqual(got, []string{"/opt/homebrew/bin", "/usr/local/bin"}) {
		t.Fatalf("no home: got %q", got)
	}
}

func TestNpmGlobalPrefix(t *testing.T) {
	home := filepath.FromSlash("/home/me")
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	npmrc := func(content string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if p != filepath.Join(home, ".npmrc") {
				return nil, errors.New("unexpected path " + p)
			}
			if content == "" {
				return nil, os.ErrNotExist
			}
			return []byte(content), nil
		}
	}
	tests := []struct {
		name string
		env  map[string]string
		rc   string
		want string
	}{
		{"env wins", map[string]string{"NPM_CONFIG_PREFIX": "/opt/npm"}, "prefix=/ignored", "/opt/npm"},
		{"lower-case env", map[string]string{"npm_config_prefix": "~/.npm-global"}, "", filepath.Join(home, ".npm-global")},
		{"npmrc prefix", nil, "registry=https://r\n# prefix=/commented\nprefix = ${HOME}/.npm-packages\n", filepath.Join(home, ".npm-packages")},
		{"npmrc quoted", nil, `prefix="/srv/npm"`, "/srv/npm"},
		{"no npmrc", nil, "", ""},
		{"npmrc without prefix", nil, "registry=https://r\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := npmGlobalPrefix(env(tt.env), home, npmrc(tt.rc)); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestPersistentPSPathAssignment(t *testing.T) {
	got := persistentPSPathAssignment("C:\\a;C:\\O'Brien\u2019s\\bin;C:\\$env:x\r\n")
	want := "$env:Path = 'C:\\a;C:\\O''Brien\u2019\u2019s\\bin;C:\\$env:x'\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("assignment must be one line: %q", got)
	}
}

// refreshCommandPath applies the merge to this process's PATH, and a second
// call is a no-op.
func TestRefreshCommandPath_AppliesAndIsStable(t *testing.T) {
	sep := string(os.PathListSeparator)
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		prev := newPersistedPathSource
		newPersistedPathSource = func() persistedPathSource {
			return fakePathSource{machine: `C:\Windows\System32`, user: dir}
		}
		t.Cleanup(func() { newPersistedPathSource = prev })
		t.Setenv("PATH", `C:\Windows\System32`)
		refreshCommandPath()
		if got := os.Getenv("PATH"); got != `C:\Windows\System32`+sep+dir {
			t.Fatalf("PATH after refresh = %q", got)
		}
	} else {
		// A HOME whose ~/.local/bin exists is prepended.
		local := filepath.Join(dir, ".local", "bin")
		if err := os.MkdirAll(local, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", dir)
		t.Setenv("NPM_CONFIG_PREFIX", "")
		t.Setenv("npm_config_prefix", "")
		t.Setenv("PATH", "/usr/bin")
		refreshCommandPath()
		got := os.Getenv("PATH")
		if !strings.Contains(got, local+sep) || !strings.HasSuffix(got, sep+"/usr/bin") {
			t.Fatalf("PATH after refresh = %q", got)
		}
	}
	before := os.Getenv("PATH")
	refreshCommandPath()
	if after := os.Getenv("PATH"); after != before {
		t.Fatalf("second refresh changed PATH: %q -> %q", before, after)
	}
}
