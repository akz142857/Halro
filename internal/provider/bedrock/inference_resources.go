package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func (a *Adapter) GenerateBedrockImage(ctx context.Context, call provider.ImageCall) (provider.ImageResult, error) {
	if a.profileID != domain.ProfileBedrockInvokeTitanImageV2 {
		return provider.ImageResult{}, badRequest("profile does not declare image generation", nil)
	}
	if err := ValidateProfileModel(a.profileID, call.ProviderModel); err != nil {
		return provider.ImageResult{}, badRequest(err.Error(), nil)
	}
	if strings.TrimSpace(call.Prompt) == "" || len(call.Prompt) > 512 || call.Count < 1 || call.Count > 5 {
		return provider.ImageResult{}, badRequest("Titan image prompt must be 1-512 bytes and n must be 1-5", nil)
	}
	width, height, ok := titanSize(call.Size)
	if !ok {
		return provider.ImageResult{}, badRequest("Titan image size is unsupported", nil)
	}
	quality := call.Quality
	if quality == "" {
		quality = "standard"
	}
	if quality != "standard" && quality != "premium" {
		return provider.ImageResult{}, badRequest("Titan image quality is unsupported", nil)
	}
	request := struct {
		TaskType          string `json:"taskType"`
		TextToImageParams struct {
			Text string `json:"text"`
		} `json:"textToImageParams"`
		ImageGenerationConfig struct {
			Quality        string  `json:"quality"`
			NumberOfImages int     `json:"numberOfImages"`
			Height         int     `json:"height"`
			Width          int     `json:"width"`
			CFGScale       float64 `json:"cfgScale"`
		} `json:"imageGenerationConfig"`
	}{TaskType: "TEXT_IMAGE"}
	request.TextToImageParams.Text = call.Prompt
	request.ImageGenerationConfig.Quality = quality
	request.ImageGenerationConfig.NumberOfImages = call.Count
	request.ImageGenerationConfig.Height = height
	request.ImageGenerationConfig.Width = width
	request.ImageGenerationConfig.CFGScale = 8
	var response struct {
		Images []string `json:"images"`
		Error  string   `json:"error"`
	}
	providerRequestID, err := a.postJSON(ctx, call.ProviderModel, "invoke", call.RequestID, request, &response)
	if err != nil {
		return provider.ImageResult{}, err
	}
	if response.Error != "" || len(response.Images) != call.Count {
		return provider.ImageResult{}, malformed("Titan image response is invalid", nil)
	}
	result := provider.ImageResult{Created: time.Now().Unix(), ProviderRequestID: providerRequestID}
	for _, encoded := range response.Images {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) == 0 || len(decoded) > 16<<20 {
			return provider.ImageResult{}, malformed("Titan image payload is invalid", err)
		}
		result.Data = append(result.Data, provider.ImageData{Base64JSON: encoded})
	}
	return result, nil
}

func (a *Adapter) Rerank(ctx context.Context, call provider.RerankCall) (provider.RerankResult, error) {
	if a.profileID != domain.ProfileBedrockAgentRerankCohere35 {
		return provider.RerankResult{}, badRequest("profile does not declare rerank", nil)
	}
	if call.ProviderModel != "cohere.rerank-v3-5:0" || strings.TrimSpace(call.Query) == "" || len(call.Query) > 10000 || len(call.Documents) < 1 || len(call.Documents) > 1000 || call.TopN < 1 || call.TopN > len(call.Documents) {
		return provider.RerankResult{}, badRequest("Cohere Rerank 3.5 request is outside the declared contract", nil)
	}
	region := agentRuntimeRegion(a.endpoint.Hostname())
	if region == "" {
		return provider.RerankResult{}, badRequest("invalid Bedrock Agent Runtime endpoint", nil)
	}
	type source struct {
		Type   string `json:"type"`
		Inline struct {
			Type string `json:"type"`
			Text struct {
				Text string `json:"text"`
			} `json:"textDocument"`
		} `json:"inlineDocumentSource"`
	}
	sources := make([]source, len(call.Documents))
	for index, document := range call.Documents {
		if strings.TrimSpace(document) == "" || len(document) > 50000 {
			return provider.RerankResult{}, badRequest("rerank documents must be non-empty and bounded", nil)
		}
		sources[index].Type = "INLINE"
		sources[index].Inline.Type = "TEXT"
		sources[index].Inline.Text.Text = document
	}
	request := map[string]any{"queries": []any{map[string]any{"type": "TEXT", "textQuery": map[string]string{"text": call.Query}}}, "sources": sources, "rerankingConfiguration": map[string]any{"type": "BEDROCK_RERANKING_MODEL", "bedrockRerankingConfiguration": map[string]any{"modelConfiguration": map[string]string{"modelArn": "arn:aws:bedrock:" + region + "::foundation-model/" + call.ProviderModel}, "numberOfResults": call.TopN}}}
	// The upstream shape is decoded on its own terms. It used to decode straight
	// into provider.RerankItem, which is the northbound wire shape and spells the
	// score relevance_score; AWS spells it relevanceScore, and the underscore is
	// enough that Go's case-insensitive fallback does not match it either. Every
	// score decoded as zero, the range check let zero through, and the fixture
	// below carried the real AWS spelling while the assertions only ever looked at
	// the index.
	var response struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevanceScore"`
		} `json:"results"`
	}
	requestID, err := a.runtimeJSON(ctx, http.MethodPost, "/rerank", call.RequestID, request, &response)
	if err != nil {
		return provider.RerankResult{}, err
	}
	if len(response.Results) != call.TopN {
		return provider.RerankResult{}, malformed("rerank response count is invalid", nil)
	}
	seen := map[int]bool{}
	results := make([]provider.RerankItem, 0, len(response.Results))
	for _, item := range response.Results {
		if item.Index < 0 || item.Index >= len(call.Documents) || item.RelevanceScore < 0 || item.RelevanceScore > 1 || seen[item.Index] {
			return provider.RerankResult{}, malformed("rerank response item is invalid", nil)
		}
		seen[item.Index] = true
		results = append(results, provider.RerankItem{Index: item.Index, RelevanceScore: item.RelevanceScore})
	}
	return provider.RerankResult{Results: results, ProviderRequestID: requestID}, nil
}

func agentRuntimeRegion(host string) string {
	parts := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	if len(parts) < 4 || parts[0] != "bedrock-agent-runtime" {
		return ""
	}
	return parts[1]
}

func titanSize(value string) (int, int, bool) {
	switch value {
	case "", "1024x1024":
		return 1024, 1024, true
	case "512x512":
		return 512, 512, true
	case "768x768":
		return 768, 768, true
	case "1152x768":
		return 1152, 768, true
	case "768x1152":
		return 768, 1152, true
	default:
		return 0, 0, false
	}
}

func (a *Adapter) StartAsyncInvoke(ctx context.Context, call provider.AsyncInvokeCall) (provider.AsyncInvokeObject, error) {
	if a.profileID != domain.ProfileBedrockAsyncNovaReel {
		return provider.AsyncInvokeObject{}, badRequest("profile does not declare async invoke", nil)
	}
	if err := ValidateProfileModel(a.profileID, call.ProviderModel); err != nil {
		return provider.AsyncInvokeObject{}, badRequest(err.Error(), nil)
	}
	if strings.TrimSpace(call.Prompt) == "" || len(call.Prompt) > 512 || !validS3URI(call.S3OutputURI) {
		return provider.AsyncInvokeObject{}, badRequest("Nova Reel prompt and explicit s3 output URI are required", nil)
	}
	request := struct {
		ModelID          string `json:"modelId"`
		ModelInput       any    `json:"modelInput"`
		OutputDataConfig any    `json:"outputDataConfig"`
	}{ModelID: call.ProviderModel, ModelInput: map[string]any{"taskType": "TEXT_VIDEO", "textToVideoParams": map[string]any{"text": call.Prompt}, "videoGenerationConfig": map[string]any{"durationSeconds": call.DurationSeconds, "dimension": call.Dimension, "fps": call.FPS, "seed": call.Seed}}, OutputDataConfig: map[string]any{"s3OutputDataConfig": map[string]string{"s3Uri": call.S3OutputURI}}}
	var response struct {
		InvocationARN string `json:"invocationArn"`
	}
	requestID, err := a.runtimeJSON(ctx, http.MethodPost, "/async-invoke", call.RequestID, request, &response)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	if !strings.HasPrefix(response.InvocationARN, "arn:aws:bedrock:") {
		return provider.AsyncInvokeObject{}, malformed("Nova Reel invocation ARN is invalid", nil)
	}
	return provider.AsyncInvokeObject{InvocationARN: response.InvocationARN, Status: "InProgress", S3OutputURI: call.S3OutputURI, ProviderRequestID: requestID}, nil
}

func (a *Adapter) GetAsyncInvoke(ctx context.Context, requestID, invocationARN string) (provider.AsyncInvokeObject, error) {
	if a.profileID != domain.ProfileBedrockAsyncNovaReel || !strings.HasPrefix(invocationARN, "arn:aws:bedrock:") {
		return provider.AsyncInvokeObject{}, badRequest("invalid async invocation ARN", nil)
	}
	var response struct {
		InvocationARN    string    `json:"invocationArn"`
		Status           string    `json:"status"`
		FailureMessage   string    `json:"failureMessage"`
		SubmitTime       time.Time `json:"submitTime"`
		LastModifiedTime time.Time `json:"lastModifiedTime"`
		OutputDataConfig struct {
			S3 struct {
				URI string `json:"s3Uri"`
			} `json:"s3OutputDataConfig"`
		} `json:"outputDataConfig"`
	}
	upstreamID := url.PathEscape(invocationARN)
	providerID, err := a.runtimeJSON(ctx, http.MethodGet, "/async-invoke/"+upstreamID, requestID, nil, &response)
	if err != nil {
		return provider.AsyncInvokeObject{}, err
	}
	if response.InvocationARN != invocationARN || response.Status == "" {
		return provider.AsyncInvokeObject{}, malformed("async invocation response is invalid", nil)
	}
	return provider.AsyncInvokeObject{InvocationARN: response.InvocationARN, Status: response.Status, S3OutputURI: response.OutputDataConfig.S3.URI, FailureMessage: response.FailureMessage, ProviderRequestID: providerID, SubmittedAt: response.SubmitTime, LastModifiedAt: response.LastModifiedTime}, nil
}

func (a *Adapter) runtimeJSON(ctx context.Context, method, path, requestID string, input, output any) (string, error) {
	var encoded []byte
	var err error
	if input != nil {
		encoded, err = json.Marshal(input)
		if err != nil {
			return "", badRequest("encode Bedrock request", err)
		}
	}
	endpoint := *a.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return "", badRequest("create Bedrock request", err)
	}
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Request-ID", requestID)
	if err := a.authorizer.Authorize(request, encoded); err != nil {
		return "", badRequest("sign Bedrock request", err)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return "", transportError("Bedrock request failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", classifyHTTP(response)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(payload) > maxResponseBytes {
		return "", malformed("read Bedrock response", err)
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return "", malformed("decode Bedrock response", err)
	}
	return providerRequestID(response.Header), nil
}
func validS3URI(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "s3" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && strings.HasSuffix(parsed.Path, "/")
}
