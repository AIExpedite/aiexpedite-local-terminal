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
)

const cliUsageReceiptDomain = "AIEXPEDITE_CLI_USAGE_REFRESH_RECEIPT\n"

type cliUsageReceiptBody struct {
	Version     int                  `json:"version"`
	RefreshID   string               `json:"refreshId"`
	ChallengeTs int64                `json:"challengeTs"`
	Success     bool                 `json:"success"`
	CliAgents   []cliAgentUsage      `json:"cliAgents"`
	Errors      []cliAgentUsageError `json:"errors"`
}

func canonicalCLIUsageRefreshReceipt(refreshID string, challengeTs int64, success bool, agents []cliAgentUsage, errs []cliAgentUsageError) ([]byte, []cliAgentUsage, []cliAgentUsageError, error) {
	if refreshID == "" || len(refreshID) > 256 || challengeTs <= 0 || len(agents) > 64 {
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
	for _, agent := range normalizedAgents {
		if len(agent.Metrics) > 32 || len(agent.Models) > 128 {
			return nil, nil, nil, errors.New("invalid receipt bounds")
		}
		for _, metric := range agent.Metrics {
			for _, n := range []*float64{metric.Total, metric.Remaining, metric.Consumed} {
				if n != nil && (math.IsInf(*n, 0) || math.IsNaN(*n)) {
					return nil, nil, nil, errors.New("non-finite metric")
				}
			}
		}
	}
	normalizedErrors := append([]cliAgentUsageError(nil), errs...)
	if normalizedAgents == nil {
		normalizedAgents = []cliAgentUsage{}
	}
	if normalizedErrors == nil {
		normalizedErrors = []cliAgentUsageError{}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(cliUsageReceiptBody{1, refreshID, challengeTs, success, normalizedAgents, normalizedErrors})
	body := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	if err != nil || len(body) > 256*1024 {
		return nil, nil, nil, errors.New("receipt too large")
	}
	return append([]byte(cliUsageReceiptDomain), body...), normalizedAgents, normalizedErrors, nil
}

func signCLIUsageRefreshReceipt(secret, refreshID string, challengeTs int64, success bool, agents []cliAgentUsage, errs []cliAgentUsageError) (string, []cliAgentUsage, []cliAgentUsageError, error) {
	canonical, normalizedAgents, normalizedErrors, err := canonicalCLIUsageRefreshReceipt(refreshID, challengeTs, success, agents, errs)
	if err != nil {
		return "", nil, nil, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil)), normalizedAgents, normalizedErrors, nil
}
