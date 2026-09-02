package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

const cliUsageReceiptDomain = "AIEXPEDITE_CLI_USAGE_REFRESH_RECEIPT\n"

const (
	cliUsageErrorProviderTimeout     = "provider_timeout"
	cliUsageErrorProviderUnavailable = "provider_unavailable"
	cliUsageErrorNotAuthenticated    = "not_authenticated"
	cliUsageErrorParseFailed         = "parse_failed"
	cliUsageErrorCollectionFailed    = "collection_failed"
	cliUsageErrorReceiptBounds       = "receipt_bounds"
	// cliUsageErrorProtocol marks a CLI that ran but violated its own invocation
	// contract — an unknown or rejected flag, or a framing mismatch such as Claude
	// 2.1.x's `--input-format=stream-json requires output-format=stream-json`.
	// Deliberately distinct from parse_failed: a protocol failure is OURS to fix in
	// the argv shape, while a parse failure means the CLI answered and its output
	// shape changed under us. Collapsing the two hid the smoke regression this
	// category exists to report.
	cliUsageErrorProtocol = "protocol"
	cliUsageErrorInternal = "internal_error"
)

type cliUsageReceiptBody struct {
	Version     int                         `json:"version"`
	RefreshID   string                      `json:"refreshId"`
	ChallengeTs int64                       `json:"challengeTs"`
	Success     bool                        `json:"success"`
	CliAgents   []canonicalCLIUsageProvider `json:"cliAgents"`
	Errors      []cliAgentUsageError        `json:"errors"`
}

type canonicalCLIUsageMetric struct {
	Kind       string  `json:"kind"`
	Label      string  `json:"label,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Total      *string `json:"total,omitempty"`
	Remaining  *string `json:"remaining,omitempty"`
	Consumed   *string `json:"consumed,omitempty"`
	ResetAt    string  `json:"resetAt,omitempty"`
	ObservedAt string  `json:"observedAt,omitempty"`
	Model      string  `json:"model,omitempty"`
	Unknown    bool    `json:"unknown,omitempty"`
}

type canonicalCLIUsageProvider struct {
	CliAgentID           string                    `json:"cliAgentId,omitempty"`
	Provider             string                    `json:"provider"`
	Name                 string                    `json:"name,omitempty"`
	Version              string                    `json:"version,omitempty"`
	Path                 string                    `json:"path,omitempty"`
	Account              string                    `json:"account,omitempty"`
	Plan                 string                    `json:"plan,omitempty"`
	Model                string                    `json:"model,omitempty"`
	Models               []string                  `json:"models,omitempty"`
	AccountFingerprint   string                    `json:"accountFingerprint,omitempty"`
	Metrics              []canonicalCLIUsageMetric `json:"metrics,omitempty"`
	CollectedAt          string                    `json:"collectedAt"`
	DataSource           string                    `json:"dataSource,omitempty"`
	Authenticated        *bool                     `json:"authenticated,omitempty"`
	AuthState            string                    `json:"authState,omitempty"`
	LoginExpiresAt       string                    `json:"loginExpiresAt,omitempty"`
	LoginExpirationState string                    `json:"loginExpirationState,omitempty"`
	Notice               string                    `json:"notice,omitempty"`
	NoticeSeverity       string                    `json:"noticeSeverity,omitempty"`
	NoticeURL            string                    `json:"noticeUrl,omitempty"`
}

func canonicalFloat(value *float64) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if math.IsInf(*value, 0) || math.IsNaN(*value) {
		return nil, errors.New("non-finite metric")
	}
	formatted := "0"
	if *value != 0 {
		formatted = strconv.FormatFloat(*value, 'f', -1, 64)
	}
	return &formatted, nil
}

func bounded(value string, max int) bool { return len([]byte(value)) <= max }

func safeCLIUsageErrorCategory(item cliAgentUsageError) string {
	if item.ErrorCategory != "" {
		switch item.ErrorCategory {
		case cliUsageErrorProviderTimeout, cliUsageErrorProviderUnavailable,
			cliUsageErrorNotAuthenticated, cliUsageErrorParseFailed,
			cliUsageErrorCollectionFailed, cliUsageErrorReceiptBounds,
			cliUsageErrorProtocol, cliUsageErrorInternal:
			return item.ErrorCategory
		}
	}
	message := strings.ToLower(item.Message)
	switch {
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return cliUsageErrorProviderTimeout
	case strings.Contains(message, "not authenticated"), strings.Contains(message, "not signed in"), strings.Contains(message, "login"):
		return cliUsageErrorNotAuthenticated
	case strings.Contains(message, "parse"), strings.Contains(message, "decode"), strings.Contains(message, "unmarshal"):
		return cliUsageErrorParseFailed
	case strings.Contains(message, "unavailable"), strings.Contains(message, "not found"):
		return cliUsageErrorProviderUnavailable
	case strings.Contains(message, "collect"), strings.Contains(message, "gather"):
		return cliUsageErrorCollectionFailed
	case strings.Contains(message, "protocol"), strings.Contains(message, "stream-json"),
		strings.Contains(message, "input-format"):
		// Legacy free-text callers (and any producer predating the closed enum)
		// describe the Claude framing failure in these words; without this arm they
		// collapse to internal_error and the maintenance flow loses the one signal
		// that says "the invocation shape is wrong".
		return cliUsageErrorProtocol
	default:
		return cliUsageErrorInternal
	}
}

func canonicalProvider(agent cliAgentUsage) (canonicalCLIUsageProvider, error) {
	short := []string{agent.CliAgentID, agent.Provider, agent.Name, agent.Version, agent.Plan, agent.Model, agent.CollectedAt, agent.DataSource, agent.AuthState, agent.LoginExpiresAt, agent.LoginExpirationState, agent.NoticeSeverity}
	for _, value := range short {
		if !bounded(value, 256) {
			return canonicalCLIUsageProvider{}, errors.New("invalid receipt bounds")
		}
	}
	for _, value := range []string{agent.Path, agent.Account, agent.AccountFingerprint, agent.Notice, agent.NoticeURL} {
		if !bounded(value, 2048) {
			return canonicalCLIUsageProvider{}, errors.New("invalid receipt bounds")
		}
	}
	if len(agent.Metrics) > cliUsageMaxMetricsPerProvider || len(agent.Models) > cliUsageMaxModelsPerProvider {
		return canonicalCLIUsageProvider{}, errors.New("invalid receipt bounds")
	}
	for _, model := range agent.Models {
		if !bounded(model, 2048) {
			return canonicalCLIUsageProvider{}, errors.New("invalid receipt bounds")
		}
	}
	metrics := make([]canonicalCLIUsageMetric, 0, len(agent.Metrics))
	metricIDs := map[string]struct{}{}
	for _, metric := range agent.Metrics {
		for _, value := range []string{metric.Kind, metric.Label, metric.Unit, metric.ResetAt, metric.ObservedAt, metric.Model} {
			if !bounded(value, 256) {
				return canonicalCLIUsageProvider{}, errors.New("invalid receipt bounds")
			}
		}
		identity := metric.Kind + "\x00" + metric.Label + "\x00" + metric.Model
		if _, duplicate := metricIDs[identity]; duplicate {
			return canonicalCLIUsageProvider{}, errors.New("duplicate metric identity")
		}
		metricIDs[identity] = struct{}{}
		total, err := canonicalFloat(metric.Total)
		if err != nil {
			return canonicalCLIUsageProvider{}, err
		}
		remaining, err := canonicalFloat(metric.Remaining)
		if err != nil {
			return canonicalCLIUsageProvider{}, err
		}
		consumed, err := canonicalFloat(metric.Consumed)
		if err != nil {
			return canonicalCLIUsageProvider{}, err
		}
		metrics = append(metrics, canonicalCLIUsageMetric{metric.Kind, metric.Label, metric.Unit, total, remaining, consumed, metric.ResetAt, metric.ObservedAt, metric.Model, metric.Unknown})
	}
	return canonicalCLIUsageProvider{
		CliAgentID: agent.CliAgentID, Provider: agent.Provider, Name: agent.Name,
		Version: agent.Version, Path: agent.Path, Account: agent.Account, Plan: agent.Plan,
		Model: agent.Model, Models: agent.Models, AccountFingerprint: agent.AccountFingerprint,
		Metrics: metrics, CollectedAt: agent.CollectedAt, DataSource: agent.DataSource,
		Authenticated: agent.Authenticated, AuthState: agent.AuthState,
		LoginExpiresAt: agent.LoginExpiresAt, LoginExpirationState: agent.LoginExpirationState,
		Notice: agent.Notice, NoticeSeverity: agent.NoticeSeverity, NoticeURL: agent.NoticeURL,
	}, nil
}

func canonicalCLIUsageRefreshReceipt(refreshID string, challengeTs int64, success bool, agents []cliAgentUsage, errs []cliAgentUsageError) ([]byte, []cliAgentUsage, []cliAgentUsageError, error) {
	if refreshID == "" || len(refreshID) > 256 || challengeTs <= 0 || len(agents) > cliUsageMaxProviders {
		return nil, nil, nil, errors.New("invalid receipt bounds")
	}
	normalizedAgents := append([]cliAgentUsage(nil), agents...)
	sort.SliceStable(normalizedAgents, func(i, j int) bool {
		a, b := normalizedAgents[i], normalizedAgents[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.CliAgentID != b.CliAgentID {
			return a.CliAgentID < b.CliAgentID
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	canonicalAgents := make([]canonicalCLIUsageProvider, 0, len(normalizedAgents))
	providerIDs := map[string]struct{}{}
	for _, agent := range normalizedAgents {
		identity := agent.Provider + "\x00" + agent.CliAgentID
		if _, duplicate := providerIDs[identity]; duplicate {
			return nil, nil, nil, errors.New("duplicate provider identity")
		}
		providerIDs[identity] = struct{}{}
		provider, err := canonicalProvider(agent)
		if err != nil {
			return nil, nil, nil, err
		}
		canonicalAgents = append(canonicalAgents, provider)
	}
	normalizedErrors := append([]cliAgentUsageError(nil), errs...)
	if normalizedAgents == nil {
		normalizedAgents = []cliAgentUsage{}
	}
	if normalizedErrors == nil {
		normalizedErrors = []cliAgentUsageError{}
	}
	for index, item := range normalizedErrors {
		if !bounded(item.Provider, 256) {
			return nil, nil, nil, errors.New("invalid receipt bounds")
		}
		normalizedErrors[index] = cliAgentUsageError{
			Provider: item.Provider, ErrorCategory: safeCLIUsageErrorCategory(item),
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(cliUsageReceiptBody{
		Version: 1, RefreshID: refreshID, ChallengeTs: challengeTs, Success: success,
		CliAgents: canonicalAgents, Errors: normalizedErrors,
	})
	body := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	if err != nil || len(body) > 256*1024 {
		return nil, nil, nil, errors.New("receipt too large")
	}
	return append([]byte(cliUsageReceiptDomain), body...), normalizedAgents, normalizedErrors, nil
}

func signCLIUsageRefreshReceipt(secret, refreshID string, challengeTs int64, success bool, agents []cliAgentUsage, errs []cliAgentUsageError) (string, []cliAgentUsage, []cliAgentUsageError, error) {
	if secret == "" {
		return "", nil, nil, errors.New("missing receipt secret")
	}
	canonical, normalizedAgents, normalizedErrors, err := canonicalCLIUsageRefreshReceipt(refreshID, challengeTs, success, agents, errs)
	if err != nil {
		return "", nil, nil, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil)), normalizedAgents, normalizedErrors, nil
}
