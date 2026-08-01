package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/akz142857/Heimdall/internal/provider"
)

const maxPhase2ResponseBytes = 32 << 20

func (a *Adapter) Moderate(ctx context.Context, call provider.ModerationCall) (provider.ModerationResult, error) {
	if a.azure || a.providerType != "openai" {
		return provider.ModerationResult{}, phase2Unsupported("moderations")
	}
	if strings.TrimSpace(call.ProviderModel) == "" || len(call.Input) == 0 || !json.Valid(call.Input) {
		return provider.ModerationResult{}, phase2BadRequest("model and valid input are required")
	}
	payload := struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}{call.ProviderModel, call.Input}
	var response struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Results json.RawMessage `json:"results"`
	}
	requestID, _, err := a.phase2JSON(ctx, http.MethodPost, "moderations", call.RequestID, payload, &response)
	if err != nil {
		return provider.ModerationResult{}, err
	}
	if response.ID == "" || response.Model == "" || len(response.Results) == 0 || !json.Valid(response.Results) {
		return provider.ModerationResult{}, phase2Malformed("moderation response is invalid")
	}
	return provider.ModerationResult{ID: response.ID, Model: response.Model, Results: response.Results, ProviderRequestID: requestID}, nil
}

func (a *Adapter) GenerateImage(ctx context.Context, call provider.ImageCall) (provider.ImageResult, error) {
	if a.azure || a.providerType != "openai" {
		return provider.ImageResult{}, phase2Unsupported("images")
	}
	if strings.TrimSpace(call.Prompt) == "" || len(call.Prompt) > 32000 || call.Count < 1 || call.Count > 10 {
		return provider.ImageResult{}, phase2BadRequest("image prompt and n between 1 and 10 are required")
	}
	payload := struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Quality        string `json:"quality,omitempty"`
		Size           string `json:"size,omitempty"`
		ResponseFormat string `json:"response_format,omitempty"`
		Style          string `json:"style,omitempty"`
	}{Model: call.ProviderModel, Prompt: call.Prompt, N: call.Count, Quality: call.Quality, Size: call.Size, ResponseFormat: call.ResponseFormat, Style: call.Style}
	var response struct {
		Created int64                `json:"created"`
		Data    []provider.ImageData `json:"data"`
	}
	requestID, _, err := a.phase2JSON(ctx, http.MethodPost, "images/generations", call.RequestID, payload, &response)
	if err != nil {
		return provider.ImageResult{}, err
	}
	if response.Created <= 0 || len(response.Data) == 0 {
		return provider.ImageResult{}, phase2Malformed("image response is invalid")
	}
	for _, item := range response.Data {
		if (item.URL == "") == (item.Base64JSON == "") {
			return provider.ImageResult{}, phase2Malformed("image response must contain exactly one payload")
		}
	}
	return provider.ImageResult{Created: response.Created, Data: response.Data, ProviderRequestID: requestID}, nil
}

func (a *Adapter) Transcribe(ctx context.Context, call provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	if a.azure || a.providerType != "openai" {
		return provider.TranscriptionResult{}, phase2Unsupported("audio transcription")
	}
	if len(call.Data) == 0 || len(call.Data) > 25<<20 || strings.TrimSpace(call.Filename) == "" {
		return provider.TranscriptionResult{}, phase2BadRequest("audio file is required and must not exceed 25 MiB")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := make(map[string][]string)
	partHeader["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, call.Filename)}
	partHeader["Content-Type"] = []string{call.ContentType}
	part, err := writer.CreatePart(textprotoHeader(partHeader))
	if err != nil {
		return provider.TranscriptionResult{}, phase2BadRequest("create audio multipart")
	}
	if _, err = part.Write(call.Data); err != nil {
		return provider.TranscriptionResult{}, err
	}
	fields := map[string]string{"model": call.ProviderModel, "language": call.Language, "prompt": call.Prompt, "response_format": call.ResponseFormat}
	if call.Temperature != nil {
		fields["temperature"] = fmt.Sprint(*call.Temperature)
	}
	for key, value := range fields {
		if value != "" {
			_ = writer.WriteField(key, value)
		}
	}
	_ = writer.Close()
	return a.phase2BinaryRequest(ctx, "audio/transcriptions", call.RequestID, writer.FormDataContentType(), body.Bytes())
}

func (a *Adapter) Synthesize(ctx context.Context, call provider.SpeechCall) (provider.SpeechResult, error) {
	if a.azure || a.providerType != "openai" {
		return provider.SpeechResult{}, phase2Unsupported("audio speech")
	}
	if strings.TrimSpace(call.Input) == "" || len(call.Input) > 4096 || strings.TrimSpace(call.Voice) == "" {
		return provider.SpeechResult{}, phase2BadRequest("speech input and voice are required")
	}
	payload := struct {
		Model          string   `json:"model"`
		Input          string   `json:"input"`
		Voice          string   `json:"voice"`
		ResponseFormat string   `json:"response_format,omitempty"`
		Speed          *float64 `json:"speed,omitempty"`
	}{call.ProviderModel, call.Input, call.Voice, call.ResponseFormat, call.Speed}
	encoded, _ := json.Marshal(payload)
	result, err := a.phase2BinaryRequest(ctx, "audio/speech", call.RequestID, "application/json", encoded)
	if err != nil {
		return provider.SpeechResult{}, err
	}
	return provider.SpeechResult{ContentType: result.ContentType, Data: result.Data, ProviderRequestID: result.ProviderRequestID}, nil
}

func (a *Adapter) CreateFile(ctx context.Context, call provider.FileCreateCall) (provider.FileObject, error) {
	if a.azure || a.providerType != "openai" {
		return provider.FileObject{}, phase2Unsupported("files")
	}
	if len(call.Data) == 0 || len(call.Data) > 512<<20 || strings.TrimSpace(call.Filename) == "" || strings.TrimSpace(call.Purpose) == "" {
		return provider.FileObject{}, phase2BadRequest("file, filename, and purpose are required")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, call.Filename))
	partHeader.Set("Content-Type", call.ContentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return provider.FileObject{}, phase2BadRequest("create file multipart")
	}
	if _, err := part.Write(call.Data); err != nil {
		return provider.FileObject{}, err
	}
	if err := writer.WriteField("purpose", call.Purpose); err != nil {
		return provider.FileObject{}, err
	}
	if err := writer.Close(); err != nil {
		return provider.FileObject{}, err
	}
	result, err := a.phase2Do(ctx, http.MethodPost, "files", call.RequestID, writer.FormDataContentType(), body.Bytes())
	if err != nil {
		return provider.FileObject{}, err
	}
	return decodeFileObject(result)
}
func (a *Adapter) GetFile(ctx context.Context, requestID, id string) (provider.FileObject, error) {
	if !validResourceID(id, "file-") {
		return provider.FileObject{}, phase2BadRequest("invalid file id")
	}
	result, err := a.phase2Do(ctx, http.MethodGet, "files/"+id, requestID, "", nil)
	if err != nil {
		return provider.FileObject{}, err
	}
	return decodeFileObject(result)
}
func (a *Adapter) DownloadFile(ctx context.Context, requestID, id string) (provider.FileContent, error) {
	if !validResourceID(id, "file-") {
		return provider.FileContent{}, phase2BadRequest("invalid file id")
	}
	result, err := a.phase2Do(ctx, http.MethodGet, "files/"+id+"/content", requestID, "", nil)
	if err != nil {
		return provider.FileContent{}, err
	}
	return provider.FileContent{ContentType: result.ContentType, Data: result.Data, ProviderRequestID: result.ProviderRequestID}, nil
}
func (a *Adapter) DeleteFile(ctx context.Context, requestID, id string) (provider.FileDeleteResult, error) {
	if !validResourceID(id, "file-") {
		return provider.FileDeleteResult{}, phase2BadRequest("invalid file id")
	}
	result, err := a.phase2Do(ctx, http.MethodDelete, "files/"+id, requestID, "", nil)
	if err != nil {
		return provider.FileDeleteResult{}, err
	}
	var output provider.FileDeleteResult
	if err := json.Unmarshal(result.Data, &output); err != nil || output.ID != id || !output.Deleted {
		return provider.FileDeleteResult{}, phase2Malformed("file deletion response is invalid")
	}
	output.ProviderRequestID = result.ProviderRequestID
	return output, nil
}

func (a *Adapter) CreateBatch(ctx context.Context, call provider.BatchCreateCall) (provider.BatchObject, error) {
	if !validResourceID(call.InputFileID, "file-") || call.Endpoint != "/v1/chat/completions" && call.Endpoint != "/v1/responses" && call.Endpoint != "/v1/embeddings" || call.CompletionWindow != "24h" {
		return provider.BatchObject{}, phase2BadRequest("batch input file, endpoint, and 24h completion window are required")
	}
	payload := struct {
		InputFileID      string            `json:"input_file_id"`
		Endpoint         string            `json:"endpoint"`
		CompletionWindow string            `json:"completion_window"`
		Metadata         map[string]string `json:"metadata,omitempty"`
	}{call.InputFileID, call.Endpoint, call.CompletionWindow, call.Metadata}
	var output provider.BatchObject
	requestID, _, err := a.phase2JSON(ctx, http.MethodPost, "batches", call.RequestID, payload, &output)
	if err != nil {
		return output, err
	}
	if !validResourceID(output.ID, "batch_") || output.InputFileID != call.InputFileID {
		return provider.BatchObject{}, phase2Malformed("batch response is invalid")
	}
	_ = requestID
	return output, nil
}
func (a *Adapter) GetBatch(ctx context.Context, requestID, id string) (provider.BatchObject, error) {
	return a.batchAction(ctx, http.MethodGet, requestID, id, "")
}
func (a *Adapter) CancelBatch(ctx context.Context, requestID, id string) (provider.BatchObject, error) {
	return a.batchAction(ctx, http.MethodPost, requestID, id, "/cancel")
}
func (a *Adapter) batchAction(ctx context.Context, method, requestID, id, suffix string) (provider.BatchObject, error) {
	if !validResourceID(id, "batch_") {
		return provider.BatchObject{}, phase2BadRequest("invalid batch id")
	}
	result, err := a.phase2Do(ctx, method, "batches/"+id+suffix, requestID, "application/json", nil)
	if err != nil {
		return provider.BatchObject{}, err
	}
	var output provider.BatchObject
	if err := json.Unmarshal(result.Data, &output); err != nil || output.ID != id || output.Status == "" {
		return provider.BatchObject{}, phase2Malformed("batch response is invalid")
	}
	return output, nil
}
func decodeFileObject(result phase2HTTPResult) (provider.FileObject, error) {
	var output provider.FileObject
	if err := json.Unmarshal(result.Data, &output); err != nil || !validResourceID(output.ID, "file-") || output.Filename == "" || output.Purpose == "" || output.Bytes < 0 {
		return provider.FileObject{}, phase2Malformed("file response is invalid")
	}
	return output, nil
}
func validResourceID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) > 160 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func (a *Adapter) phase2JSON(ctx context.Context, method, operation, requestID string, input, output any) (string, string, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return "", "", phase2BadRequest("encode request")
	}
	result, err := a.phase2Do(ctx, method, operation, requestID, "application/json", body)
	if err != nil {
		return "", "", err
	}
	if err := json.Unmarshal(result.Data, output); err != nil {
		return "", "", phase2Malformed("provider returned malformed JSON")
	}
	return result.ProviderRequestID, result.ContentType, nil
}

func (a *Adapter) phase2BinaryRequest(ctx context.Context, operation, requestID, contentType string, body []byte) (provider.TranscriptionResult, error) {
	result, err := a.phase2Do(ctx, http.MethodPost, operation, requestID, contentType, body)
	if err != nil {
		return provider.TranscriptionResult{}, err
	}
	return provider.TranscriptionResult{ContentType: result.ContentType, Data: result.Data, ProviderRequestID: result.ProviderRequestID}, nil
}

type phase2HTTPResult struct {
	ContentType, ProviderRequestID string
	Data                           []byte
}

func (a *Adapter) phase2Do(ctx context.Context, method, operation, requestID, contentType string, body []byte) (phase2HTTPResult, error) {
	endpoint := a.operationURL("", operation)
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return phase2HTTPResult{}, phase2BadRequest("create provider request")
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "*/*")
	if requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	if err := a.authorize(request); err != nil {
		return phase2HTTPResult{}, &provider.Error{Class: provider.ErrorAuthentication, Message: "authorize provider request", Cause: err}
	}
	response, err := a.client.Do(request)
	if err != nil {
		return phase2HTTPResult{}, &provider.Error{Class: provider.ErrorConnect, Retryable: true, Message: "provider request failed", Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return phase2HTTPResult{}, classifyHTTPError(response.StatusCode, limitedErrorMessage(response.Body))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxPhase2ResponseBytes+1))
	if err != nil {
		return phase2HTTPResult{}, &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: "read provider response", Cause: err}
	}
	if len(data) > maxPhase2ResponseBytes {
		return phase2HTTPResult{}, phase2Malformed("provider response exceeds limit")
	}
	return phase2HTTPResult{ContentType: response.Header.Get("Content-Type"), ProviderRequestID: response.Header.Get("x-request-id"), Data: data}, nil
}
func phase2BadRequest(message string) error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: message}
}
func phase2Unsupported(name string) error {
	return &provider.Error{Class: provider.ErrorBadRequest, Message: name + " is not declared by this provider profile"}
}
func phase2Malformed(message string) error {
	return &provider.Error{Class: provider.ErrorMalformed, Ambiguous: true, Message: message}
}
func textprotoHeader(value map[string][]string) textproto.MIMEHeader {
	return textproto.MIMEHeader(value)
}
