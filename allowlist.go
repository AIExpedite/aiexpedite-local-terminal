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

// IsAllowed checks if a command matches any pattern
func (al *AllowList) IsAllowed(cmd string, args []string) bool {
	al.mu.RLock()
	defer al.mu.RUnlock()

	// Build full command string
	fullCmd := cmd
	if len(args) > 0 {
		fullCmd = cmd + " " + strings.Join(args, " ")
	}

	for _, re := range al.compiled {
		if re.MatchString(fullCmd) {
			return true
		}
	}
	return false
}

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
// "git *" becomes "^git .*$"
// "npm run *" becomes "^npm run .*$"
func patternToRegex(pattern string) *regexp.Regexp {
	// Escape regex special chars except *
	escaped := regexp.QuoteMeta(pattern)
	// Convert * to .* (use [\s\S]* to match across newlines)
	escaped = strings.ReplaceAll(escaped, `\*`, `[\s\S]*`)
	// Anchor the pattern
	escaped = "^" + escaped + "$"

	re, err := regexp.Compile("(?i)" + escaped) // Case insensitive
	if err != nil {
		// Fallback to literal match if regex fails
		return regexp.MustCompile("(?i)^" + regexp.QuoteMeta(pattern) + "$")
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
pwsh *
cmd *
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

# --- CLI Coding Agents ---
claude
claude *
codex
codex *
gemini
gemini *

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
