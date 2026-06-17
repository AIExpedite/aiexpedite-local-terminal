// File: allowlist.go
// Command allow list validation for security
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// AllowList manages the list of allowed command patterns
type AllowList struct {
	patterns   []string
	compiled   []*regexp.Regexp
	mu         sync.RWMutex
	configPath string
}

var defaultAllowList *AllowList

// InitAllowList loads patterns from file or creates default
func InitAllowList() (*AllowList, error) {
	al := &AllowList{
		configPath: filepath.Join(GetConfigDir(), "allowed-commands.txt"),
	}

	if err := al.Load(); err != nil {
		if os.IsNotExist(err) {
			// Create default file
			if err := al.CreateDefault(); err != nil {
				return nil, err
			}
			if err := al.Load(); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	defaultAllowList = al
	return al, nil
}

// Load reads patterns from the config file
func (al *AllowList) Load() error {
	al.mu.Lock()
	defer al.mu.Unlock()

	file, err := os.Open(al.configPath)
	if err != nil {
		return err
	}
	defer file.Close()

	al.patterns = nil
	al.compiled = nil

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		al.patterns = append(al.patterns, line)
		al.compiled = append(al.compiled, patternToRegex(line))
	}

	return scanner.Err()
}

// IsAllowed checks if a command matches any pattern.
//
// Newlines inside individual args (e.g. a multi-line prompt passed to
// `claude`/`codex`/`gemini`) are collapsed to spaces before matching. The
// allowlist's job is to decide whether the *command pattern* is permitted —
// the newline-injection defense lives downstream in pubsub.go's shell-quoting
// layer, which rejects newlines for shell-bound invocations. Direct-exec
// paths (`exec.Command(cmd, args...)`) pass args as distinct argv slots, so
// a newline inside an arg can't break out into a second shell command.
//
// Without this normalization, any agent prompt containing `\n` (almost all
// of them) fails `^claude [^\n]*$` and gets routed to the approval dialog.
func (al *AllowList) IsAllowed(cmd string, args []string) bool {
	al.mu.RLock()
	defer al.mu.RUnlock()

	// Build full command string
	fullCmd := cmd
	if len(args) > 0 {
		fullCmd = cmd + " " + strings.Join(args, " ")
	}
	fullCmd = newlineToSpace.Replace(fullCmd)

	for _, re := range al.compiled {
		if re.MatchString(fullCmd) {
			return true
		}
	}
	return false
}

var newlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")

// AddPattern adds a new pattern and persists to file
func (al *AllowList) AddPattern(pattern string) error {
	al.mu.Lock()
	defer al.mu.Unlock()

	// Check if pattern already exists
	for _, existing := range al.patterns {
		if existing == pattern {
			return nil // Already exists, no need to add
		}
	}

	// Add to memory
	al.patterns = append(al.patterns, pattern)
	al.compiled = append(al.compiled, patternToRegex(pattern))

	// Append to file
	f, err := os.OpenFile(al.configPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString("\n# Added by user approval\n" + pattern + "\n")
	return err
}

// GetConfigPath returns the path to the allow list file
func (al *AllowList) GetConfigPath() string {
	return al.configPath
}

// patternToRegex converts glob-like pattern to regex
// "git *" becomes "^git [^\n]*$"
// "npm run *" becomes "^npm run [^\n]*$"
//
// Wildcard explicitly excludes newlines: a remote message could in principle
// contain `\n` in a command string (JSON parsers accept `\n` escapes), and
// `[\s\S]*` would let that through, allowing an attacker to chain "git status\n; rm -rf ~"
// against a `git *` allowlist entry. The shell-quoting layer in pubsub.go
// also rejects newlines, so this is defense-in-depth — but we want both
// independent layers to be correct.
func patternToRegex(pattern string) *regexp.Regexp {
	// Escape regex special chars except *
	escaped := regexp.QuoteMeta(pattern)
	// Convert * to [^\n]* — match anything EXCEPT newlines.
	escaped = strings.ReplaceAll(escaped, `\*`, `[^\n]*`)
	// Anchor the pattern
	escaped = "^" + escaped + "$"

	re, err := regexp.Compile("(?i)" + escaped) // Case insensitive
	if err != nil {
		// Fallback 1: literal exact match — regexp.QuoteMeta output is always valid
		if literal, err2 := regexp.Compile("(?i)^" + regexp.QuoteMeta(pattern) + "$"); err2 == nil {
			return literal
		}
		// Fallback 2: never-match regex — safer than a panic that disables the whole allow list
		return regexp.MustCompile(`\A\Z`) // matches only the empty string at start/end (never matches a command)
	}
	return re
}

// GeneratePatternFromCommand creates a pattern from a command
// Uses base command wildcard approach (most permissive):
// "rm -rf node_modules" -> "rm *"
// "git status" -> "git *"
// "npm run test" -> "npm *"
func GeneratePatternFromCommand(cmd string, args []string) string {
	// Base command + wildcard (user preference: most permissive)
	return cmd + " *"
}

// CreateDefault writes the default allow list file
func (al *AllowList) CreateDefault() error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(al.configPath), 0755); err != nil {
		return err
	}

	fmt.Printf("[allowlist] Creating default allow list at: %s\n", al.configPath)
	return os.WriteFile(al.configPath, []byte(defaultAllowListContent), 0600)
}

// ResetAllowList deletes the existing allow list and recreates it with defaults
func ResetAllowList() error {
	if defaultAllowList == nil {
		return fmt.Errorf("allow list not initialized")
	}

	// Delete existing file
	if err := os.Remove(defaultAllowList.configPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove old allow list: %w", err)
	}

	// Create new default file
	if err := defaultAllowList.CreateDefault(); err != nil {
		return fmt.Errorf("failed to create default allow list: %w", err)
	}

	// Reload patterns
	if err := defaultAllowList.Load(); err != nil {
		return fmt.Errorf("failed to reload allow list: %w", err)
	}

	fmt.Println("[allowlist] Allow list has been reset to defaults")
	return nil
}

const defaultAllowListContent = `# ============================================
# AI Expedite Local Terminal - Allowed Commands
# ============================================
# Patterns support wildcards: * matches anything
# Lines starting with # are comments
# One pattern per line
# ============================================

# --- File System (Read-only / Safe) ---
ls
ls *
dir
dir *
pwd
cd *
cat *
head *
tail *
less *
more *
find *
tree *
wc *
file *
stat *
du *
df *

# --- Text Processing ---
grep *
awk *
sed *
sort *
uniq *
cut *
tr *
diff *
echo *
printf *

# --- Git (All operations) ---
git
git *

# --- GitHub CLI ---
gh
gh *

# --- Node.js / JavaScript ---
node
node *
npm
npm *
npx *
yarn
yarn *
pnpm
pnpm *
bun
bun *
deno
deno *
nvm *

# --- Python ---
python
python *
python3
python3 *
pip *
pip3 *
pipenv *
poetry *
conda *
pytest *
mypy *
black *
ruff *
uv *

# --- Build Tools ---
make
make *
cmake *
gradle *
mvn *
cargo
cargo *
go
go *
rustc *
gcc *
g++ *
clang *
java
java *
javac *
dotnet
dotnet *
ruby
ruby *

# --- Package Managers ---
apt list *
apt show *
brew *
choco *
winget *
scoop *

# --- Docker ---
docker
docker *
docker ps
docker ps *
docker images
docker images *
docker logs *
docker inspect *
docker stats
docker stats *
docker top *
docker exec *
docker run *
docker build *
docker info
docker version
docker compose *
docker-compose *

# --- Kubernetes ---
kubectl
kubectl version
kubectl get *
kubectl describe *
kubectl logs *
kubectl exec *
kubectl port-forward *
kubectl apply *
kubectl delete *
helm *

# --- Cloud CLIs ---
gcloud *
aws *
az *
firebase *
vercel *
netlify *
fly *
railway *
terraform
terraform *
ansible *

# --- Testing Frameworks ---
jest *
vitest *
playwright *
cypress *
mocha *
pytest *
cargo test *
go test *

# --- Linters & Formatters ---
eslint *
prettier *
tsc *
biome *
rubocop *
golint *
gofmt *

# --- Network Tools ---
curl *
wget *
ping *
nslookup *
dig *
host *
traceroute *
netstat
netstat *
ss *
ifconfig
ifconfig *
ip *

# --- Process Management ---
ps
ps *
top
htop
which *
whereis *
type *
command *

# --- Editors ---
code *
vim *
nvim *
nano *
emacs *

# --- Internal Commands ---
__ping__

# --- Misc Dev Tools ---
jq *
yq *
xargs *
env
printenv *
whoami
hostname
date
cal
uptime
clear
cls
history
alias *
touch *
mkdir *
cp *
mv *
rm *
chmod *
chown *

# --- Windows Specific ---
powershell *
powershell.exe *
pwsh *
pwsh.exe *
cmd *
cmd.exe *
type *
copy *
xcopy *
robocopy *
move *
rmdir *
del *
ren *
attrib *
icacls *
tasklist *
taskkill *
systeminfo
ver
set
where *

# --- PowerShell Cmdlets (for Terminal Discovery Agent) ---
Get-ChildItem *
Get-Command *
Get-CimInstance *
ForEach-Object *
Write-Output *
Select-Object *
Format-List *
Format-Table *
@(*
$*

# --- CLI Coding Agents ---
claude
claude *
codex
codex *
gemini
gemini *
agy
agy *
# No "grok" / "grok *" / "grok agent stdio *" entries: the ACP manager owns
# the only legitimate Grok entry point. gateSessionEntryCommand short-circuits
# grok_acp_start so the synthesised "grok agent stdio ..." argv does not need
# a default allowlist match, and a raw execute of "grok ..." (bare or
# "grok agent stdio ...") stays gated by the dialog so it cannot bypass the
# manager's EnableGrokAPIKeyFallback / EnableGrokAlwaysApprove / env
# sanitisation.

# --- Remote/SSH ---
ssh *
scp *
rsync *
sftp *

# --- Database CLIs ---
psql *
mysql *
mongosh *
redis-cli *
sqlite3 *

# --- Compression ---
tar *
gzip *
gunzip *
zip *
unzip *
7z *

# --- Version Managers ---
nvm *
rbenv *
pyenv *
asdf *
volta *
fnm *
`
