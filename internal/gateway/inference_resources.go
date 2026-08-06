package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/akz142857/Heimdall/internal/auth"
	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/akz142857/Heimdall/internal/redaction"
)

func (s *Service) inferenceResourcesTarget(ctx context.Context, key, model string, operation provider.Operation) (auth.AuthResult, provider.Target, string, error) {
	principal, targets, err := s.resolveRequest(ctx, key, model, operation, "requested operation is unsupported")
	if err != nil {
		return auth.AuthResult{}, provider.Target{}, "", err
	}
	if len(targets) != 1 {
		return auth.AuthResult{}, provider.Target{}, "", gatewayError("ambiguous_resource_route", "Phase 2 operations require exactly one eligible deployment", http.StatusConflict, nil)
	}
	return principal, targets[0], "", nil
}

func (s *Service) accountedInferenceResources(ctx context.Context, principal auth.AuthResult, model string, target provider.Target, inputUnits int64, requestID *string, invoke func() error) error {
	if inputUnits < 1 {
		inputUnits = 1
	}
	run, err := s.beginRequestRun(ctx, principal, model, []provider.Target{target}, inputUnits, inputUnits, 0)
	if err != nil {
		return err
	}
	defer run.close()
	if requestID != nil {
		*requestID = run.requestID
	}
	attempt, err := s.startAttempt(ctx, run, target, inputUnits, 0, 0, 0, 1)
	if err != nil {
		return s.exhaustedAttemptsError(err)
	}
	invokeErr := invoke()
	providerErr := invokeErr
	policyRejected := errors.Is(invokeErr, redaction.ErrPolicyRejected)
	if policyRejected {
		providerErr = nil
	}
	settlement := budget.Settlement{ProviderInputTokens: inputUnits, TokenEstimated: true, CostEstimated: true}
	setSettlementCost(&settlement, attempt.pricingTarget, attempt.accounting.ReservationMicrosUSD)
	if err := attempt.finish(providerErr, settlement); err != nil {
		return err
	}
	outcome := "success"
	if policyRejected {
		outcome = "policy_rejected"
	} else if providerErr != nil {
		outcome = "provider_error"
	}
	if err := s.finalizeRequest(run.requestLease, outcome); err != nil {
		return gatewayError("accounting_unavailable", "request accounting could not be finalized", 503, err)
	}
	if policyRejected {
		return gatewayError("sensitive_data_detected", "response contains material rejected by policy", 502, invokeErr)
	}
	if providerErr != nil {
		return mapProviderError(providerErr)
	}
	return nil
}

func (s *Service) Moderations(ctx context.Context, key string, request openaiapi.ModerationRequest) (openaiapi.ModerationResponse, error) {
	principal, target, requestID, err := s.inferenceResourcesTarget(ctx, key, request.Model, provider.OperationModerations)
	if err != nil {
		return openaiapi.ModerationResponse{}, err
	}
	adapter, ok := target.Adapter.(provider.StatelessInferenceResourcesAdapter)
	if !ok {
		return openaiapi.ModerationResponse{}, gatewayError("unsupported_feature", "moderation adapter is unavailable", 400, nil)
	}
	request.Input, err = s.redactor.ProcessJSON(principal.Project.RedactionPolicyID, "inbound", request.Input)
	if err != nil {
		return openaiapi.ModerationResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	var result provider.ModerationResult
	err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(len(request.Input))/4+1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.Moderate(ctx, provider.ModerationCall{RequestID: requestID, ProviderModel: target.ProviderModel, Input: request.Input})
		if callErr == nil {
			result.Results, callErr = s.redactor.ProcessJSON(principal.Project.RedactionPolicyID, "outbound", result.Results)
		}
		return callErr
	})
	if err != nil {
		return openaiapi.ModerationResponse{}, err
	}
	return openaiapi.ModerationResponse{ID: result.ID, Model: request.Model, Results: result.Results}, nil
}
func (s *Service) Images(ctx context.Context, key string, request openaiapi.ImageGenerationRequest) (openaiapi.ImageGenerationResponse, error) {
	principal, target, requestID, err := s.inferenceResourcesTarget(ctx, key, request.Model, provider.OperationImages)
	if err != nil {
		return openaiapi.ImageGenerationResponse{}, err
	}
	var result provider.ImageResult
	request.Prompt, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", request.Prompt)
	if err != nil {
		return openaiapi.ImageGenerationResponse{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	if adapter, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter); ok && target.ProfileID != domain.ProfileOpenAIMediaResources {
		err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(len(request.Prompt))/4+1, &requestID, func() error {
			var callErr error
			result, callErr = adapter.GenerateBedrockImage(ctx, provider.ImageCall{RequestID: requestID, ProviderModel: target.ProviderModel, Prompt: request.Prompt, Count: request.N, Quality: request.Quality, Size: request.Size, ResponseFormat: request.ResponseFormat, Style: request.Style})
			if callErr == nil {
				callErr = s.redactImageResult(principal.Project.RedactionPolicyID, &result)
			}
			return callErr
		})
	} else if adapter, ok := target.Adapter.(provider.StatelessInferenceResourcesAdapter); ok {
		err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(len(request.Prompt))/4+1, &requestID, func() error {
			var callErr error
			result, callErr = adapter.GenerateImage(ctx, provider.ImageCall{RequestID: requestID, ProviderModel: target.ProviderModel, Prompt: request.Prompt, Count: request.N, Quality: request.Quality, Size: request.Size, ResponseFormat: request.ResponseFormat, Style: request.Style})
			if callErr == nil {
				callErr = s.redactImageResult(principal.Project.RedactionPolicyID, &result)
			}
			return callErr
		})
	} else {
		return openaiapi.ImageGenerationResponse{}, gatewayError("unsupported_feature", "image adapter is unavailable", 400, nil)
	}
	if err != nil {
		return openaiapi.ImageGenerationResponse{}, err
	}
	data := make([]openaiapi.ImageData, len(result.Data))
	for i, v := range result.Data {
		data[i] = openaiapi.ImageData{URL: v.URL, Base64JSON: v.Base64JSON, RevisedPrompt: v.RevisedPrompt}
	}
	return openaiapi.ImageGenerationResponse{Created: result.Created, Data: data}, nil
}
func (s *Service) Speech(ctx context.Context, key string, request openaiapi.SpeechRequest) (provider.SpeechResult, error) {
	principal, target, requestID, err := s.inferenceResourcesTarget(ctx, key, request.Model, provider.OperationSpeech)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	adapter, ok := target.Adapter.(provider.StatelessInferenceResourcesAdapter)
	if !ok {
		return provider.SpeechResult{}, gatewayError("unsupported_feature", "speech adapter is unavailable", 400, nil)
	}
	request.Input, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", request.Input)
	if err != nil {
		return provider.SpeechResult{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	var result provider.SpeechResult
	err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(len(request.Input))/4+1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.Synthesize(ctx, provider.SpeechCall{RequestID: requestID, ProviderModel: target.ProviderModel, Input: request.Input, Voice: request.Voice, ResponseFormat: request.ResponseFormat, Speed: request.Speed})
		return callErr
	})
	if err != nil {
		return provider.SpeechResult{}, err
	}
	return result, nil
}
func (s *Service) Transcription(ctx context.Context, key, model string, call provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	principal, target, requestID, err := s.inferenceResourcesTarget(ctx, key, model, provider.OperationTranscriptions)
	if err != nil {
		return provider.TranscriptionResult{}, err
	}
	adapter, ok := target.Adapter.(provider.StatelessInferenceResourcesAdapter)
	if !ok {
		return provider.TranscriptionResult{}, gatewayError("unsupported_feature", "transcription adapter is unavailable", 400, nil)
	}
	if err := s.contentScanner.ScanAudio(call.Filename, call.ContentType, call.Data); err != nil {
		return provider.TranscriptionResult{}, gatewayError("content_rejected", "audio was rejected by media policy", 400, err)
	}
	call.ProviderModel = target.ProviderModel
	call.Prompt, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", call.Prompt)
	if err != nil {
		return provider.TranscriptionResult{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	var result provider.TranscriptionResult
	err = s.accountedInferenceResources(ctx, principal, model, target, int64(len(call.Data))/4+1, &requestID, func() error {
		call.RequestID = requestID
		var callErr error
		result, callErr = adapter.Transcribe(ctx, call)
		if callErr == nil {
			callErr = s.redactTranscriptionResult(principal.Project.RedactionPolicyID, &result)
		}
		return callErr
	})
	if err != nil {
		return provider.TranscriptionResult{}, err
	}
	return result, nil
}

func (s *Service) redactImageResult(policyID string, result *provider.ImageResult) error {
	for index := range result.Data {
		processed, err := s.redactor.ProcessText(policyID, "outbound", result.Data[index].RevisedPrompt)
		if err != nil {
			return err
		}
		result.Data[index].RevisedPrompt = processed
	}
	return nil
}

func (s *Service) redactTranscriptionResult(policyID string, result *provider.TranscriptionResult) error {
	if strings.Contains(strings.ToLower(result.ContentType), "json") {
		processed, err := s.redactor.ProcessJSON(policyID, "outbound", result.Data)
		if err != nil {
			return err
		}
		result.Data = processed
		return nil
	}
	processed, err := s.redactor.ProcessText(policyID, "outbound", string(result.Data))
	if err != nil {
		return err
	}
	result.Data = []byte(processed)
	return nil
}
func (s *Service) Rerank(ctx context.Context, key string, request openaiapi.RerankRequest) (provider.RerankResult, error) {
	principal, target, requestID, err := s.inferenceResourcesTarget(ctx, key, request.Model, provider.OperationRerank)
	if err != nil {
		return provider.RerankResult{}, err
	}
	adapter, ok := target.Adapter.(provider.BedrockInferenceResourcesAdapter)
	if !ok {
		return provider.RerankResult{}, gatewayError("unsupported_feature", "rerank adapter is unavailable", 400, nil)
	}
	request.Query, err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", request.Query)
	if err != nil {
		return provider.RerankResult{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
	}
	for i := range request.Documents {
		request.Documents[i], err = s.redactor.ProcessText(principal.Project.RedactionPolicyID, "inbound", request.Documents[i])
		if err != nil {
			return provider.RerankResult{}, gatewayError("sensitive_data_detected", "request contains secret material", 400, err)
		}
	}
	units := len(request.Query)
	for _, doc := range request.Documents {
		units += len(doc)
	}
	var result provider.RerankResult
	err = s.accountedInferenceResources(ctx, principal, request.Model, target, int64(units)/4+1, &requestID, func() error {
		var callErr error
		result, callErr = adapter.Rerank(ctx, provider.RerankCall{RequestID: requestID, ProviderModel: target.ProviderModel, Query: request.Query, Documents: request.Documents, TopN: request.TopN})
		return callErr
	})
	if err != nil {
		return provider.RerankResult{}, err
	}
	return result, nil
}
