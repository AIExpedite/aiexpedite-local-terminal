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
// claiming the quota is unknowable.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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

	antigravityQuotaRPC  = "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
	antigravityStatusRPC = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
)

// antigravityHTTPPortRe matches the port line the language server logs at
// startup. Only the plain-HTTP listener is matched: the sibling HTTPS/gRPC port
// serves the same RPCs behind a self-signed certificate, and trusting an
// unverifiable certificate to reach a service already reachable in the clear on
// loopback would be a downgrade, not an upgrade.
// The trailing \b is load-bearing: without it "for HTTPS (gRPC)" also matches
// (HTTP is a prefix of HTTPS) and the TLS port gets probed in the clear.
var antigravityHTTPPortRe = regexp.MustCompile(`listening on random port at (\d{2,5}) for HTTP\b`)

// antigravityQuotaBucket is one row of the provider's quota panel.
type antigravityQuotaBucket struct {
	BucketID          string  `json:"bucketId"`
	Group             string  `json:"group"`
	DisplayName       string  `json:"displayName"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remainingFraction"`
	ResetTime         string  `json:"resetTime"`
}

// antigravityQuotaSnapshot is what we cache between `agy` runs.
type antigravityQuotaSnapshot struct {
	ObservedAt         string                   `json:"observedAt"`
	AccountFingerprint string                   `json:"accountFingerprint"`
	Account            string                   `json:"account"`
	Plan               string                   `json:"plan"`
	Buckets            []antigravityQuotaBucket `json:"buckets"`
}

// antigravityQuotaCachePath is the cache location inside the agent's data dir.
// AIEXPEDITE_AGY_QUOTA_CACHE overrides it (tests isolate from the real machine
// cache; ops can relocate it if the data dir is read-only).
func antigravityQuotaCachePath() string {
	if p := os.Getenv("AIEXPEDITE_AGY_QUOTA_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "antigravity_quota.json")
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
		body, err := os.ReadFile(file.path)
		if err != nil {
			continue
		}
		matches := antigravityHTTPPortRe.FindAllSubmatch(body, -1)
		// Later lines are later restarts within the same run — prefer them.
		for i := len(matches) - 1; i >= 0; i-- {
			port := 0
			if _, err := fmt.Sscanf(string(matches[i][1]), "%d", &port); err != nil {
				continue
			}
			if port <= 0 || port > 65535 || seen[port] {
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
		var quota struct {
			Response struct {
				Groups []struct {
					DisplayName string `json:"displayName"`
					Buckets     []struct {
						BucketID          string  `json:"bucketId"`
						DisplayName       string  `json:"displayName"`
						Window            string  `json:"window"`
						RemainingFraction float64 `json:"remainingFraction"`
						ResetTime         string  `json:"resetTime"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"response"`
		}
		if !antigravityPostJSON(ctx, client, port, antigravityQuotaRPC, &quota) {
			continue
		}
		snap := antigravityQuotaSnapshot{ObservedAt: now.UTC().Format(time.RFC3339)}
		for _, group := range quota.Response.Groups {
			for _, bucket := range group.Buckets {
				snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{
					BucketID:          bucket.BucketID,
					Group:             group.DisplayName,
					DisplayName:       bucket.DisplayName,
					Window:            bucket.Window,
					RemainingFraction: bucket.RemainingFraction,
					ResetTime:         bucket.ResetTime,
				})
			}
		}
		if len(snap.Buckets) == 0 {
			continue
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
	return antigravityQuotaSnapshot{}, false
}

// loadAntigravityQuotaSnapshot reads the cached snapshot, scoped to the current
// account exactly as the Claude/Codex caches are: a snapshot captured under a
// different signed-in account must not be attributed to this one.
func loadAntigravityQuotaSnapshot(currentFingerprint string) (antigravityQuotaSnapshot, bool) {
	var snap antigravityQuotaSnapshot
	if !readJSONFile(antigravityQuotaCachePath(), &snap) {
		return antigravityQuotaSnapshot{}, false
	}
	if snap.AccountFingerprint != currentFingerprint || len(snap.Buckets) == 0 {
		return antigravityQuotaSnapshot{}, false
	}
	return snap, true
}

// saveAntigravityQuotaSnapshot persists a fresh reading. Write-then-rename so a
// concurrent gather never observes a half-written file. Best-effort: a read-only
// data dir costs us the between-runs display, not the live one.
func saveAntigravityQuotaSnapshot(snap antigravityQuotaSnapshot) {
	path := antigravityQuotaCachePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
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
	metrics := make([]cliAgentUsageMetric, 0, len(rows))
	for _, r := range rows {
		metrics = append(metrics, r.metric)
	}
	return metrics
}
