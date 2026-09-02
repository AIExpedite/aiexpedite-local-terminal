// cliagent_usage_antigravity_quota.go — reads Antigravity's quota from the
// language server the CLI runs on this machine.
//
// `agy` has no quota file and no non-interactive quota command: `/usage` renders
// the panel from a language server that every CLI run starts in-process on a
// random loopback port. That server answers the same RPC over plain HTTP:
//
//	POST http://127.0.0.1:<port>/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary
//	{} → {"response":{"groups":[{"displayName":"Gemini Models","buckets":[
//	      {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.96,
//	       "resetTime":"2026-08-12T04:37:41Z"}, …]}]}}
//
// The port is not guessable, but the CLI logs it:
//
//	server.go:584] Language server listening on random port at 54452 for HTTP
//
// so discovery is a log read, not a port scan. Everything stays on loopback and
// no credential of ours is involved — the server answers for whichever account
// is signed into `agy` on this computer.
//
// The server only exists while `agy` is running, so a successful read is cached.
// Between runs the card shows the last observation with its true age rather than
// claiming the quota is unknowable. Because a gather almost never coincides with
// a live run, the freshness of that cache comes from the run-scoped poller in
// cliagent_usage_antigravity_capture.go, which reads this server WHILE the agent
// has `agy` up.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Loopback RPCs are answered by an in-process server; a slow reply means the
	// CLI is busy, not that the network is congested. Keep the whole probe well
	// inside the 10s gather budget shared with every other provider.
	antigravityQuotaTimeout = 2 * time.Second
	// Bound the decode: the real payload is ~1.5 KB. This guards against reading
	// an unbounded body from a process that is not the language server at all
	// (the port is recycled by the OS once `agy` exits).
	antigravityQuotaMaxBody = 256 * 1024
	// How many recent CLI logs to mine for a port. Each `agy` run writes its own
	// file; only a running one answers, so a handful of candidates covers the
	// case where the newest run has already exited.
	antigravityQuotaMaxLogs  = 4
	antigravityQuotaMaxPorts = 8
	// Per-side cap when scanning a log for port lines. A long-running `agy`
	// session writes an unbounded log, and slurping four of those would cost
	// real memory and could blow the 10s gather budget shared with every other
	// provider. Both ends are read because the ports are logged at STARTUP —
	// near the head — while an in-run restart appends a newer pair at the tail.
	antigravityLogScanBytes = 128 * 1024

	antigravityQuotaRPC  = "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
	antigravityStatusRPC = "/exa.language_server_pb.LanguageServerService/GetUserStatus"

	// antigravityQuotaSchemaVersion stamps every snapshot this build writes.
	// Readers deliberately do NOT reject an unknown value: after a rollback the
	// older agent must still be able to replay a cache a newer one wrote, and
	// every field it does not understand is simply absent from its struct. The
	// version is diagnostic metadata, not a gate.
	antigravityQuotaSchemaVersion = 1
	// Per-string byte caps applied before the cache is written. The top-level
	// cap matches canonicalCLIUsageMetric's 256-byte bound so a value that
	// survives the cache also survives the signed refresh. Bucket fields are
	// held tighter because antigravityQuotaMetrics composes them into a metric
	// Label ("<group> — <window>") that is itself bounded at 256.
	antigravityQuotaMaxFieldBytes       = 256
	antigravityQuotaMaxBucketFieldBytes = 96
)

// antigravityHTTPPortPatterns matches the port lines the language server logs at
// startup, in the order they are tried. Only plain-HTTP listeners are matched:
// the sibling HTTPS/gRPC port serves the same RPCs behind a self-signed
// certificate, and trusting an unverifiable certificate to reach a service
// already reachable in the clear on loopback would be a downgrade, not an
// upgrade. Every pattern must therefore name HTTP explicitly, and the trailing
// \b is load-bearing: without it "for HTTPS (gRPC)" also matches (HTTP is a
// prefix of HTTPS) and the TLS port gets probed in the clear.
//
// The second spelling exists for post-update resilience: an `agy` self-update
// that drops "random" from the line keeps capture working. A build that renames
// the line entirely degrades to today's behavior (no port, stale reading) rather
// than falling back to the TLS port.
var antigravityHTTPPortPatterns = []*regexp.Regexp{
	regexp.MustCompile(`listening on random port at (\d{2,5}) for HTTP\b`),
	regexp.MustCompile(`listening on port (\d{2,5}) for HTTP\b`),
}

// antigravityQuotaBucket is one row of the provider's quota panel.
type antigravityQuotaBucket struct {
	BucketID          string  `json:"bucketId"`
	Group             string  `json:"group"`
	DisplayName       string  `json:"displayName"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
}

// antigravityQuotaSnapshot is what we cache between `agy` runs. The persisted
// shape is an allowlist, not a dump: observation time, the account it belongs
// to, the plan name, and the numeric buckets. Discovered ports, log excerpts,
// argv, prompts and settings.json contents are deliberately absent.
type antigravityQuotaSnapshot struct {
	// SchemaVersion is written but never enforced on read — see
	// antigravityQuotaSchemaVersion.
	SchemaVersion      int                      `json:"schemaVersion,omitempty"`
	ObservedAt         string                   `json:"observedAt"`
	AccountFingerprint string                   `json:"accountFingerprint"`
	Account            string                   `json:"account"`
	Plan               string                   `json:"plan"`
	Buckets            []antigravityQuotaBucket `json:"buckets"`
}

// antigravityQuotaCacheMu serializes cache writers within this process — see
// saveAntigravityQuotaSnapshot.
var antigravityQuotaCacheMu sync.Mutex

// antigravityQuotaCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_AGY_QUOTA_CACHE overrides it (tests isolate from the real machine
// cache; ops can relocate it if the data dir is read-only).
func antigravityQuotaCachePath() string {
	if p := os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "antigravity_quota.json")
}

// antigravityQuotaBases returns the install trees to probe for a live language
// server, most likely first. Quota discovery reads the CLI's logs, which live
// under whichever install is actually in use: probing only the config-bearing
// tree would miss a running server whose install has no config file yet, and
// probing only the modern tree would never find a legacy install's `~/.agy/log`.
//
// Single-sourced so the gather-time parser and the run-scoped capture poller can
// never diverge on which tree they read, and re-resolved on every call rather
// than memoized so an `agy` self-update that migrates ~/.agy →
// ~/.gemini/antigravity-cli mid-flight is picked up without restarting.
func antigravityQuotaBases(home string) []string {
	modern := expandHome(home, filepath.Join(".gemini", "antigravity-cli"))
	legacy := expandHome(home, ".agy")
	// The legacy tree leads only when it is the one holding config AND the
	// modern tree holds none — exactly the ordering antigravityUsageParser.Parse
	// derives from the same two files.
	var cfg antigravityConfig
	if !readJSONFile(expandHome(modern, "settings.json"), &cfg) &&
		readJSONFile(expandHome(legacy, "config.json"), &cfg) {
		return nonEmptyPaths(legacy, modern)
	}
	return nonEmptyPaths(modern, legacy)
}

// nonEmptyPaths drops the "" entries expandHome returns for an unresolvable
// home, so callers never probe a path rooted at the filesystem root.
func nonEmptyPaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// antigravityLogDir resolves the CLI's log directory under an already-resolved
// antigravity-cli base.
func antigravityLogDir(base string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(base, "log")
}

// discoverAntigravityHTTPPorts returns candidate loopback ports, newest run
// first. Ports are read from the CLI's own logs — the server picks a random one
// per run, so there is nothing to hard-code and nothing to scan for.
func discoverAntigravityHTTPPorts(base string) []int {
	dir := antigravityLogDir(base)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type logFile struct {
		path    string
		modTime time.Time
	}
	files := make([]logFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{path: filepath.Join(dir, entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) > antigravityQuotaMaxLogs {
		files = files[:antigravityQuotaMaxLogs]
	}

	ports := make([]int, 0, antigravityQuotaMaxPorts)
	seen := map[int]bool{}
	for _, file := range files {
		body, err := readBoundedHeadTail(file.path, antigravityLogScanBytes)
		if err != nil {
			continue
		}
		for _, port := range antigravityPortsInLog(body) {
			if seen[port] {
				continue
			}
			seen[port] = true
			ports = append(ports, port)
			if len(ports) >= antigravityQuotaMaxPorts {
				return ports
			}
		}
	}
	return ports
}

// antigravityPortsInLog extracts every plain-HTTP listener port from one log
// body, newest first. Matches from ALL tolerated spellings are ordered by their
// position in the file rather than by pattern, because a later line is a later
// restart within the same run and must be probed first regardless of which
// spelling that build happens to use.
func antigravityPortsInLog(body []byte) []int {
	type portMatch struct{ at, port int }
	found := make([]portMatch, 0, 4)
	for _, re := range antigravityHTTPPortPatterns {
		for _, loc := range re.FindAllSubmatchIndex(body, -1) {
			port := 0
			if _, err := fmt.Sscanf(string(body[loc[2]:loc[3]]), "%d", &port); err != nil {
				continue
			}
			if port <= 0 || port > 65535 {
				continue
			}
			found = append(found, portMatch{at: loc[0], port: port})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].at > found[j].at })
	ports := make([]int, 0, len(found))
	for _, m := range found {
		ports = append(ports, m.port)
	}
	return ports
}

// readBoundedHeadTail returns at most `limit` bytes from the start of a file and
// at most `limit` from its end, joined by a newline so no match can straddle the
// gap. A file within 2*limit is returned whole. The two halves are concatenated
// head-then-tail, which keeps the caller's "later match wins" scan correct.
func readBoundedHeadTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 2*limit {
		return io.ReadAll(io.LimitReader(f, 2*limit))
	}

	head := make([]byte, limit)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}
	if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
		return nil, err
	}
	tail := make([]byte, limit)
	if _, err := io.ReadFull(f, tail); err != nil {
		return nil, err
	}
	return append(append(head, '\n'), tail...), nil
}

// antigravityLoopbackClient is an HTTP client pinned to the loopback interface.
// The DialContext guard is what makes "127.0.0.1 only" a property of the
// transport rather than of every call site: a log line that somehow named a
// remote host could otherwise turn a local quota read into an outbound request.
func antigravityLoopbackClient() *http.Client {
	return &http.Client{
		Timeout: antigravityQuotaTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
					return nil, fmt.Errorf("antigravity quota: refusing non-loopback address %q", addr)
				}
				dialer := &net.Dialer{Timeout: antigravityQuotaTimeout}
				return dialer.DialContext(ctx, network, addr)
			},
		},
	}
}

// antigravityPostJSON performs one loopback RPC and decodes into out.
func antigravityPostJSON(ctx context.Context, client *http.Client, port int, path string, out any) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, antigravityQuotaMaxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, antigravityQuotaMaxBody))
	if err != nil {
		return false
	}
	return json.Unmarshal(body, out) == nil
}

// fetchAntigravityQuota queries the first candidate port that answers. Returns
// (snapshot, true) only when at least one usable bucket came back — a server
// that answers with an empty quota (signed out, or a plan with no metered pool)
// must not overwrite a good cached reading with nothing.
func fetchAntigravityQuota(ctx context.Context, base string, now time.Time) (antigravityQuotaSnapshot, bool) {
	ports := discoverAntigravityHTTPPorts(base)
	if len(ports) == 0 {
		return antigravityQuotaSnapshot{}, false
	}
	client := antigravityLoopbackClient()
	for _, port := range ports {
		if snap, ok := fetchAntigravityQuotaOnPort(ctx, client, port, now); ok {
			return snap, true
		}
	}
	return antigravityQuotaSnapshot{}, false
}

// fetchAntigravityQuotaOnPort runs the quota + identity RPC pair against ONE
// already-known loopback port. Split out from the discovery wrapper so the
// run-scoped capture poller can retry a memoized port without re-scanning the
// CLI's logs — log scanning, not the RPC, is what makes a probe expensive
// (antigravityQuotaMaxLogs files × 2×antigravityLogScanBytes per attempt).
func fetchAntigravityQuotaOnPort(ctx context.Context, client *http.Client, port int, now time.Time) (antigravityQuotaSnapshot, bool) {
	var quota struct {
		Response struct {
			Groups []struct {
				DisplayName string `json:"displayName"`
				Buckets     []struct {
					BucketID    string `json:"bucketId"`
					DisplayName string `json:"displayName"`
					Window      string `json:"window"`
					// Pointer so an absent or null fraction is distinguishable
					// from a real 0 — decoded into a plain float64 it would
					// silently become "100% consumed".
					RemainingFraction *float64 `json:"remainingFraction"`
					ResetTime         string   `json:"resetTime"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"response"`
	}
	if !antigravityPostJSON(ctx, client, port, antigravityQuotaRPC, &quota) {
		return antigravityQuotaSnapshot{}, false
	}
	snap := antigravityQuotaSnapshot{ObservedAt: now.UTC().Format(time.RFC3339)}
	plottable := 0
	for _, group := range quota.Response.Groups {
		for _, bucket := range group.Buckets {
			// A bucket with no usable fraction is not a reading. Keeping it
			// would both render as 100% consumed and let a malformed payload
			// count as an observation that replaces a good cached one.
			if bucket.RemainingFraction == nil ||
				math.IsNaN(*bucket.RemainingFraction) ||
				*bucket.RemainingFraction < 0 || *bucket.RemainingFraction > 1 {
				continue
			}
			snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{
				BucketID:          bucket.BucketID,
				Group:             group.DisplayName,
				DisplayName:       bucket.DisplayName,
				Window:            bucket.Window,
				RemainingFraction: *bucket.RemainingFraction,
				ResetTime:         bucket.ResetTime,
			})
			if _, _, ok := antigravityWindowKind(bucket.Window); ok {
				plottable++
			}
		}
	}
	// A response we cannot plot is not an observation. Counting raw buckets
	// here would let a schema addition — every window renamed, say — pass as
	// success and overwrite the last usable cached reading with rows that
	// all render Unknown.
	if plottable == 0 {
		return antigravityQuotaSnapshot{}, false
	}
	// Identity comes from the same server, on the same port, in the same
	// probe — so the quota and the account it belongs to can never be
	// stitched together from two different signed-in sessions.
	var status struct {
		UserStatus struct {
			Name       string `json:"name"`
			Email      string `json:"email"`
			PlanStatus struct {
				PlanInfo struct {
					PlanName string `json:"planName"`
				} `json:"planInfo"`
			} `json:"planStatus"`
		} `json:"userStatus"`
	}
	if antigravityPostJSON(ctx, client, port, antigravityStatusRPC, &status) {
		snap.Account = firstNonEmpty(status.UserStatus.Email, status.UserStatus.Name)
		snap.Plan = status.UserStatus.PlanStatus.PlanInfo.PlanName
	}
	return snap, true
}

// loadAntigravityQuotaSnapshot reads the cached snapshot, scoped to the current
// account exactly as the Claude/Codex caches are: a snapshot captured under a
// different signed-in account must not be attributed to this one.
func loadAntigravityQuotaSnapshot(currentFingerprint string) (antigravityQuotaSnapshot, bool) {
	// An unscoped snapshot matches every unidentified account, so replaying one
	// could attribute a previous account's quota to whoever is signed in now.
	// saveAntigravityQuotaSnapshot never writes one; refuse to read one too, so
	// a cache left by an older build cannot be replayed either.
	if currentFingerprint == "" {
		return antigravityQuotaSnapshot{}, false
	}
	snap, ok := readAntigravityQuotaCache()
	if !ok || snap.AccountFingerprint != currentFingerprint {
		return antigravityQuotaSnapshot{}, false
	}
	return snap, true
}

// loadAntigravityQuotaSnapshotByProducer returns the cached snapshot together
// with the identity that PRODUCED it, for the case where no current identity is
// known at all.
//
// settings.json usually carries no account, so scoping the replay by the current
// fingerprint alone would make the between-runs cache dead in the common case —
// it would only ever load for the unusual settings file that happens to
// duplicate the server identity. Replaying under the stored producer is honest:
// the card names the account the reading belongs to instead of assuming it is
// whoever is signed in now, and there is no current identity for it to conflict
// with. A settings file that DOES name a different account fails the scoped load
// above and never reaches here.
func loadAntigravityQuotaSnapshotByProducer() (antigravityQuotaSnapshot, bool) {
	snap, ok := readAntigravityQuotaCache()
	if !ok || snap.AccountFingerprint == "" {
		return antigravityQuotaSnapshot{}, false
	}
	return snap, true
}

func readAntigravityQuotaCache() (antigravityQuotaSnapshot, bool) {
	var snap antigravityQuotaSnapshot
	if !readJSONFile(antigravityQuotaCachePath(), &snap) {
		return antigravityQuotaSnapshot{}, false
	}
	if len(snap.Buckets) == 0 {
		return antigravityQuotaSnapshot{}, false
	}
	return snap, true
}

// saveAntigravityQuotaSnapshot persists a fresh reading unconditionally.
func saveAntigravityQuotaSnapshot(snap antigravityQuotaSnapshot) {
	// The periodic machine-info gather, a demand-driven refresh and the
	// run-scoped capture poller can all save here at once IN THE SAME PROCESS.
	antigravityQuotaCacheMu.Lock()
	defer antigravityQuotaCacheMu.Unlock()
	_ = writeAntigravityQuotaSnapshotLocked(snap)
}

// saveAntigravityQuotaSnapshotIfNewer persists a reading only when it is not
// older than what is already cached FOR THE SAME ACCOUNT. The run-scoped capture
// poller and a concurrent gather both write this file; without the guard a
// probe that started earlier but finished later would age the card backwards.
// A fingerprint change always writes: an account switch is not a stale reading,
// it is a different pool, and the newly signed-in account's real figure must
// replace the previous account's regardless of timestamps.
// Returns whether the cache now holds this reading.
func saveAntigravityQuotaSnapshotIfNewer(snap antigravityQuotaSnapshot) bool {
	antigravityQuotaCacheMu.Lock()
	defer antigravityQuotaCacheMu.Unlock()

	if existing, ok := readAntigravityQuotaCache(); ok &&
		existing.AccountFingerprint == snap.AccountFingerprint &&
		!antigravityQuotaObservedAfter(snap.ObservedAt, existing.ObservedAt) {
		return false
	}
	return writeAntigravityQuotaSnapshotLocked(snap)
}

// antigravityQuotaObservedAfter reports whether observation time a is strictly
// later than b. An unparseable a never wins (we cannot prove it is newer); an
// unparseable b always loses (any real timestamp beats a corrupt one).
func antigravityQuotaObservedAfter(a, b string) bool {
	at, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	bt, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return true
	}
	return at.After(bt)
}

// writeAntigravityQuotaSnapshotLocked sanitizes and atomically replaces the
// cache. Write-then-rename so a concurrent gather never observes a half-written
// file. Best-effort: a read-only data dir costs us the between-runs display, not
// the live one. Callers MUST hold antigravityQuotaCacheMu. Returns whether the
// cache file was actually replaced.
func writeAntigravityQuotaSnapshotLocked(snap antigravityQuotaSnapshot) bool {
	// Persist ONLY a reading whose account the server itself named. If
	// GetUserStatus failed, the fingerprint would fall back to settings.json —
	// usually empty — and an empty scope matches any later unidentified
	// account, which is exactly how one account's quota would end up displayed
	// under another. Losing the between-runs display is the safe failure.
	if snap.AccountFingerprint == "" {
		return false
	}
	snap = sanitizeAntigravityQuotaSnapshot(snap)
	// Sanitizing can empty a snapshot whose every window was unrecognized; such
	// a file is unreadable anyway (readAntigravityQuotaCache rejects it) and
	// writing it would only destroy a good previous reading.
	if len(snap.Buckets) == 0 {
		return false
	}
	path := antigravityQuotaCachePath()
	if path == "" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return false
	}
	// The nanosecond suffix keeps two writers (or a stale temp from a crashed
	// run) off the same intermediate file — a shared temp name would let one
	// writer truncate the bytes another is about to rename into place.
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// sanitizeAntigravityQuotaSnapshot reduces a reading to exactly what may be
// persisted: observation time, the account it belongs to, the plan name, and the
// plottable numeric buckets. Unrecognized windows are dropped (they can never
// render, and keeping them would let a renamed window bloat the cache), bucket
// count is capped at the signed-refresh metric cap, and every string is
// length-clamped so a hostile or malformed server payload cannot grow the file
// or overflow the receipt's per-field bounds.
func sanitizeAntigravityQuotaSnapshot(snap antigravityQuotaSnapshot) antigravityQuotaSnapshot {
	out := antigravityQuotaSnapshot{
		SchemaVersion:      antigravityQuotaSchemaVersion,
		ObservedAt:         clampAntigravityQuotaField(snap.ObservedAt, antigravityQuotaMaxFieldBytes),
		AccountFingerprint: clampAntigravityQuotaField(snap.AccountFingerprint, antigravityQuotaMaxFieldBytes),
		Account:            clampAntigravityQuotaField(snap.Account, antigravityQuotaMaxFieldBytes),
		Plan:               clampAntigravityQuotaField(snap.Plan, antigravityQuotaMaxFieldBytes),
	}
	for _, bucket := range snap.Buckets {
		if _, _, ok := antigravityWindowKind(bucket.Window); !ok {
			continue
		}
		out.Buckets = append(out.Buckets, antigravityQuotaBucket{
			BucketID:          clampAntigravityQuotaField(bucket.BucketID, antigravityQuotaMaxBucketFieldBytes),
			Group:             clampAntigravityQuotaField(bucket.Group, antigravityQuotaMaxBucketFieldBytes),
			DisplayName:       clampAntigravityQuotaField(bucket.DisplayName, antigravityQuotaMaxBucketFieldBytes),
			Window:            clampAntigravityQuotaField(bucket.Window, antigravityQuotaMaxBucketFieldBytes),
			RemainingFraction: bucket.RemainingFraction,
			ResetTime:         clampAntigravityQuotaField(bucket.ResetTime, antigravityQuotaMaxBucketFieldBytes),
		})
		if len(out.Buckets) >= cliUsageMaxMetricsPerProvider {
			break
		}
	}
	return out
}

// clampAntigravityQuotaField trims and bounds a persisted string to max BYTES
// (the unit the receipt's `bounded` check uses) without splitting a UTF-8 rune.
func clampAntigravityQuotaField(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// antigravityWindowKind maps the provider's window onto our limit kinds. An
// unrecognized window is skipped rather than guessed — the card derives a
// metric's window length from the kind, so a wrong mapping would keep a stale
// reading on screen past its real expiry.
func antigravityWindowKind(window string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h":
		return limitKindSession, "5-hour session window", true
	case "daily":
		return limitKindDaily, "Daily quota", true
	case "weekly":
		return limitKindWeekly, "Weekly quota", true
	case "monthly":
		return limitKindMonthly, "Monthly quota", true
	}
	return "", "", false
}

// antigravityGroupLabel shortens a group's display name for the row label
// ("Gemini Models" → "Gemini", "Claude and GPT models" → "Claude and GPT").
func antigravityGroupLabel(group string) string {
	trimmed := strings.TrimSpace(group)
	for _, suffix := range []string{" Models", " models", " Model", " model"} {
		if strings.HasSuffix(trimmed, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(trimmed, suffix))
		}
	}
	return trimmed
}

// antigravityQuotaMetrics renders the cached/fresh snapshot as capacity rows.
// Buckets are ordered session-window first within each group, matching how the
// Claude and Codex cards read.
func antigravityQuotaMetrics(snap antigravityQuotaSnapshot, now time.Time) []cliAgentUsageMetric {
	type row struct {
		metric cliAgentUsageMetric
		group  int
		window int
	}
	groupOrder := map[string]int{}
	rows := make([]row, 0, len(snap.Buckets))

	for _, bucket := range snap.Buckets {
		kind, windowLabel, ok := antigravityWindowKind(bucket.Window)
		if !ok {
			continue
		}
		groupIdx, seen := groupOrder[bucket.Group]
		if !seen {
			groupIdx = len(groupOrder)
			groupOrder[bucket.Group] = groupIdx
		}
		label := windowLabel
		if group := antigravityGroupLabel(bucket.Group); group != "" {
			label = fmt.Sprintf("%s — %s", group, windowLabel)
		}
		used := clampPercent((1 - bucket.RemainingFraction) * 100)
		metric := cliAgentUsageMetric{
			Kind:       kind,
			Label:      label,
			Unit:       "%",
			ObservedAt: snap.ObservedAt,
		}
		reset, resetErr := time.Parse(time.RFC3339, bucket.ResetTime)
		switch {
		case resetErr == nil && !now.Before(reset):
			// The window rolled over after the observation — the pool has
			// refilled by an unknown amount, so plot nothing.
			metric.Unknown = true
		default:
			metric.Total = floatPtr(100)
			metric.Consumed = floatPtr(used)
			metric.Remaining = floatPtr(100 - used)
			if resetErr == nil {
				metric.ResetAt = reset.UTC().Format(time.RFC3339)
			}
		}
		windowRank := 1
		if kind == limitKindSession {
			windowRank = 0
		}
		rows = append(rows, row{metric: metric, group: groupIdx, window: windowRank})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].group != rows[j].group {
			return rows[i].group < rows[j].group
		}
		return rows[i].window < rows[j].window
	})
	if len(rows) > cliUsageMaxMetricsPerProvider {
		rows = rows[:cliUsageMaxMetricsPerProvider]
	}
	metrics := make([]cliAgentUsageMetric, 0, len(rows))
	for _, r := range rows {
		metrics = append(metrics, r.metric)
	}
	return metrics
}
