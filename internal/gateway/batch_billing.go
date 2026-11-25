package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akz142857/Halro/internal/budget"
	openaiwire "github.com/akz142857/Halro/internal/compatibility/openai"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/semantic"
)

type batchItemBound struct {
	Input  int64
	Output int64
}

// Keep bounds and hashed correlation IDs after submission. The source file
// can be deleted independently while the provider still holds the work.
type batchBillingPlan map[string]batchItemBound

func batchItemKey(id string) string {
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:])
}

func scanBatchLines(data []byte, read func([]byte) error) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), len(data)+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) != 0 {
			if err := read(line); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func planBatchBilling(data []byte, endpoint string, target provider.Target, project domain.Project, local bool) (batchBillingPlan, batchItemBound, error) {
	plan := make(batchBillingPlan)
	var total batchItemBound
	err := scanBatchLines(data, func(raw []byte) error {
		var line struct {
			ID     string          `json:"custom_id"`
			Method string          `json:"method"`
			URL    string          `json:"url"`
			Body   json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(raw, &line); err != nil || strings.TrimSpace(line.ID) == "" || len(line.Body) == 0 ||
			(line.Method != "POST" && !(local && line.Method == "")) ||
			(line.URL != endpoint && !(local && line.URL == "")) {
			return errors.New("batch lines require a unique ID, a POST body and the batch endpoint")
		}
		key := batchItemKey(line.ID)
		if _, exists := plan[key]; exists {
			return errors.New("batch lines must have unique IDs")
		}
		var body struct {
			Model               string `json:"model"`
			Stream              bool   `json:"stream"`
			MaxTokens           int64  `json:"max_tokens"`
			MaxCompletionTokens int64  `json:"max_completion_tokens"`
			MaxOutputTokens     int64  `json:"max_output_tokens"`
			Choices             int64  `json:"n"`
			BestOf              int64  `json:"best_of"`
		}
		if err := json.Unmarshal(line.Body, &body); err != nil || body.Stream ||
			(body.Model != target.ProviderModel && !(local && body.Model == target.PublicModel)) {
			return errors.New("batch line must address the priced deployment and use a non-streaming body")
		}
		input := estimateInputTokens(int64(len(line.Body)))
		var output int64
		switch endpoint {
		case "/v1/chat/completions", "/v1/responses", "/v1/completions":
			output = max(body.MaxTokens, body.MaxCompletionTokens, body.MaxOutputTokens)
			if output == 0 {
				// The provider may choose a default larger than the project
				// allows because a batch line bypasses synchronous admission.
				return errors.New("batch generation lines require an explicit output token limit")
			}
			choices := max(1, body.Choices, body.BestOf)
			if output <= 0 || output > project.MaxOutputTokens || choices > maxAccountableTokens/output {
				return errors.New("batch generation output exceeds the priced bounds")
			}
			output *= choices
			if endpoint == "/v1/chat/completions" {
				var chat openaiapi.ChatCompletionRequest
				if err := json.Unmarshal(line.Body, &chat); err != nil {
					return errors.New("batch chat payload is invalid")
				}
				canonical, err := openaiwire.DecodeGenerate(chat)
				if err != nil {
					return errors.New("batch chat payload is invalid")
				}
				input = estimateGenerateInputTokens(int64(len(line.Body)), canonical)
			}
		case "/v1/embeddings", "/v1/moderations":
		default:
			return errors.New("batch endpoint has no supported billing unit")
		}
		if input > project.MaxInputTokens || input > maxAccountableTokens-total.Input || output > maxAccountableTokens-total.Output {
			return errors.New("batch token bound exceeds the supported range")
		}
		plan[key] = batchItemBound{Input: input, Output: output}
		total.Input += input
		total.Output += output
		return nil
	})
	if err == nil && len(plan) == 0 {
		err = errors.New("batch input is empty")
	}
	return plan, total, err
}

func terminalBatchStatus(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (s *Service) settleBatchStatus(ctx context.Context, resource domain.ProviderResource, target provider.Target, batch provider.BatchObject, collected *batchBillingResults) error {
	if !terminalBatchStatus(batch.Status) {
		return nil
	}
	attempt, pending := s.accounting.PendingResourceAttempt(resource)
	if !pending {
		return nil
	}
	data, err := s.readResourceObject(resource)
	if err != nil {
		return err
	}
	var remaining batchBillingPlan
	if err := json.Unmarshal(data, &remaining); err != nil {
		return errors.New("batch billing bounds could not be decoded")
	}

	if collected == nil {
		files, err := s.readBatchBillingResults(ctx, resource, target, batch)
		if err != nil {
			return err
		}
		collected = &files
	}
	tier := attempt.PriceSnapshot.Tier()
	tier.CacheWriteRateEvidence = attempt.CacheWriteRateEvidence
	var tokens domain.BilledTokens
	var billed bool
	var estimatedInput, estimatedOutput int64
	costEstimated := tier.Provenance != nil && tier.Provenance.Source == domain.PriceTierZoneUnavailable
	record := func(item domain.BilledTokens) error {
		if item.Input > maxAccountableTokens-tokens.Input || item.Output > maxAccountableTokens-tokens.Output {
			return errors.New("batch usage exceeds the supported range")
		}
		tokens.Input += item.Input
		tokens.CachedInput += item.CachedInput
		tokens.CacheWrite += item.CacheWrite
		tokens.CacheWriteLong += item.CacheWriteLong
		tokens.Output += item.Output
		billed = true
		return nil
	}
	useEstimate := func(bound batchItemBound) error {
		if err := record(attempt.PriceSnapshot.ConservativeTokens(bound.Input, bound.Output)); err != nil {
			return err
		}
		costEstimated = true
		estimatedInput += bound.Input
		estimatedOutput += bound.Output
		return nil
	}
	addReported := func(usage *semantic.Usage) error {
		item := domain.BilledTokens{Input: usage.InputTokens, CachedInput: usage.CachedInputTokens,
			CacheWrite: usage.CacheWriteInputTokens, CacheWriteLong: usage.CacheWriteLongInputTokens, Output: usage.OutputTokens}
		costEstimated = costEstimated || tier.CacheWriteCostEstimated(item, usage.CacheWriteTTLPair != "")
		return record(item)
	}
	read := func(raw []byte) error {
		var line struct {
			ID       string `json:"custom_id"`
			Response *struct {
				StatusCode int `json:"status_code"`
				Body       struct {
					Usage json.RawMessage `json:"usage"`
				} `json:"body"`
			} `json:"response"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &line); err != nil {
			return errors.New("batch result line is not readable")
		}
		key := batchItemKey(line.ID)
		bound, exists := remaining[key]
		if !exists {
			return nil // Repeated result lines must not add a second charge.
		}
		delete(remaining, key)
		if usage, ok := collected.reported[line.ID]; ok && validSemanticUsage(&usage) {
			return addReported(&usage)
		}
		if line.Response != nil && len(line.Response.Body.Usage) > 0 {
			if usage := decodeBatchUsage(line.Response.Body.Usage); validSemanticUsage(usage) {
				return addReported(usage)
			}
		}
		if line.Error != nil {
			switch line.Error.Code {
			case "batch_expired", "batch_cancelled", "batch_canceled", "expired", "cancelled", "invalid_request_error", "invalid_request":
				return nil
			}
		}
		if line.Response != nil && line.Response.StatusCode >= 400 && line.Response.StatusCode < 500 {
			return nil
		}
		return useEstimate(bound)
	}
	if err := scanBatchLines(collected.output, read); err != nil {
		return err
	}
	if err := scanBatchLines(collected.errors, read); err != nil {
		return err
	}
	// OpenAI's failed status is batch validation failure. For a completed,
	// expired or cancelled job, missing item evidence can include work done.
	preExecutionFailure := batch.Status == "failed" && batch.FailedAt != 0 &&
		len(collected.output) == 0 && len(collected.errors) == 0 && batch.ResultsURL == "" && batch.OutputFileID == "" && batch.ErrorFileID == ""
	if !preExecutionFailure {
		for _, bound := range remaining {
			if err := useEstimate(bound); err != nil {
				return err
			}
		}
	}
	outcome := "provider_error"
	if batch.Status == "completed" {
		outcome = "success"
	}
	if !billed {
		return s.accounting.SettleResource(ctx, resource, budget.Settlement{Outcome: "provider_error"})
	}
	settlement := budget.Settlement{CostEstimated: costEstimated, Outcome: outcome}
	if estimatedInput != 0 || estimatedOutput != 0 {
		if estimatedInput == tokens.Input && estimatedOutput == tokens.Output {
			settlement.TokenEstimated = true
		} else {
			settlement.EstimatedInputTokens, settlement.EstimatedOutputTokens = estimatedInput, estimatedOutput
		}
	}
	return s.settleResourceUsage(ctx, resource, attempt, tokens, settlement)
}

func decodeBatchUsage(data []byte) *semantic.Usage {
	hasCounter := func(value json.RawMessage) bool {
		return value != nil && !bytes.Equal(value, []byte("null"))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil
	}
	if hasCounter(fields["input_tokens"]) || hasCounter(fields["output_tokens"]) {
		var usage openaiapi.ResponseUsage
		if hasCounter(fields["input_tokens"]) && hasCounter(fields["output_tokens"]) &&
			json.Unmarshal(data, &usage) == nil {
			return openaiwire.DecodeResponseUsage(&usage)
		}
		return nil
	}
	var usage openaiapi.Usage
	if hasCounter(fields["prompt_tokens"]) && (hasCounter(fields["completion_tokens"]) || hasCounter(fields["total_tokens"])) &&
		json.Unmarshal(data, &usage) == nil {
		return openaiwire.DecodeUsage(&usage)
	}
	return nil
}

type batchBillingResults struct {
	output, errors []byte
	reported       map[string]semantic.Usage
}

func fetchProviderBatchResults(ctx context.Context, target provider.Target, requestID, batchID, location string) (batchBillingResults, error) {
	if fetcher, ok := target.Adapter.(provider.BatchResultsUsageAdapter); ok {
		data, usages, err := fetcher.FetchBatchResultsWithUsage(ctx, requestID, batchID, location)
		return batchBillingResults{output: data, reported: usages}, err
	}
	fetcher, ok := target.Adapter.(provider.BatchResultsAdapter)
	if !ok {
		return batchBillingResults{}, errors.New("batch result adapter is unavailable")
	}
	data, err := fetcher.FetchBatchResults(ctx, requestID, batchID, location)
	return batchBillingResults{output: data}, err
}

func (s *Service) readBatchBillingResults(ctx context.Context, resource domain.ProviderResource, target provider.Target, batch provider.BatchObject) (batchBillingResults, error) {
	if batch.ResultsURL != "" {
		if resource.OutputFileID != "" {
			file, err := s.resources.ProviderResource(ctx, resource.ProjectID, resource.OutputFileID)
			if err != nil {
				return batchBillingResults{}, err
			}
			content, err := s.readResourceObject(file)
			return batchBillingResults{output: content}, err
		}
		return fetchProviderBatchResults(ctx, target, "", resource.UpstreamID, batch.ResultsURL)
	}
	if batch.OutputFileID == "" && batch.ErrorFileID == "" {
		return batchBillingResults{}, nil
	}
	adapter, ok := target.Adapter.(provider.ResourceFilesAdapter)
	if !ok {
		return batchBillingResults{}, errors.New("batch result files adapter is unavailable")
	}
	read := func(id string) ([]byte, error) {
		if id == "" {
			return nil, nil
		}
		file, err := adapter.DownloadFile(ctx, "", id)
		if err != nil {
			return nil, fmt.Errorf("read batch result file: %w", err)
		}
		return file.Data, nil
	}
	output, err := read(batch.OutputFileID)
	if err != nil {
		return batchBillingResults{}, err
	}
	errorsFile, err := read(batch.ErrorFileID)
	return batchBillingResults{output: output, errors: errorsFile}, err
}
