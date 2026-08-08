package gatewayapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/provider"
)

type inferenceResourcesFakeService struct {
	fakeService
	route string
	file  provider.FileCreateCall
}

func (s *inferenceResourcesFakeService) Moderations(_ context.Context, _ string, r openaiapi.ModerationRequest) (openaiapi.ModerationResponse, error) {
	return openaiapi.ModerationResponse{ID: "modr_1", Model: r.Model, Results: json.RawMessage(`[{"flagged":false}]`)}, nil
}
func (s *inferenceResourcesFakeService) Images(context.Context, string, openaiapi.ImageGenerationRequest) (openaiapi.ImageGenerationResponse, error) {
	return openaiapi.ImageGenerationResponse{}, nil
}
func (s *inferenceResourcesFakeService) Speech(context.Context, string, openaiapi.SpeechRequest) (provider.SpeechResult, error) {
	return provider.SpeechResult{ContentType: "audio/mpeg", Data: []byte("audio")}, nil
}
func (s *inferenceResourcesFakeService) Transcription(context.Context, string, string, provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	return provider.TranscriptionResult{ContentType: "application/json", Data: []byte(`{"text":"ok"}`)}, nil
}
func (s *inferenceResourcesFakeService) Rerank(context.Context, string, openaiapi.RerankRequest) (provider.RerankResult, error) {
	return provider.RerankResult{Results: []provider.RerankItem{{Index: 1, RelevanceScore: 0.75}}, ProviderRequestID: "provider-secret"}, nil
}
func (s *inferenceResourcesFakeService) StartAsyncInvoke(context.Context, string, string, openaiapi.AsyncInvokeRequest) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{InvocationARN: "async_1", Status: "InProgress", S3OutputURI: "s3://bucket/output", ProviderRequestID: "provider-secret", SubmittedAt: time.Date(2026, 8, 2, 1, 2, 3, 0, time.FixedZone("offset", 3600))}, nil
}
func (s *inferenceResourcesFakeService) GetAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{}, nil
}
func (s *inferenceResourcesFakeService) CancelAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{}, nil
}
func (s *inferenceResourcesFakeService) CreateFile(_ context.Context, _ string, route, _ string, call provider.FileCreateCall) (provider.FileObject, error) {
	s.route = route
	s.file = call
	return provider.FileObject{ID: "file_1", Object: "file", Bytes: int64(len(call.Data)), Filename: call.Filename, Purpose: call.Purpose}, nil
}
func (s *inferenceResourcesFakeService) GetFile(context.Context, string, string) (provider.FileObject, error) {
	return provider.FileObject{}, nil
}
func (s *inferenceResourcesFakeService) DownloadFile(context.Context, string, string) (provider.FileContent, error) {
	return provider.FileContent{}, nil
}
func (s *inferenceResourcesFakeService) DeleteFile(context.Context, string, string) (provider.FileDeleteResult, error) {
	return provider.FileDeleteResult{}, nil
}
func (s *inferenceResourcesFakeService) CreateBatch(context.Context, string, string, provider.BatchCreateCall) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (s *inferenceResourcesFakeService) GetBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (s *inferenceResourcesFakeService) CancelBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}

func TestModerationsNorthboundRejectsUnknownFields(t *testing.T) {
	handler, _ := New(&inferenceResourcesFakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"hi","unknown":true}`))
	request.Header.Set("Authorization", "Bearer gw")
	response := httptest.NewRecorder()
	handler.Moderations(response, request)
	if response.Code != 400 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestModerationsNorthboundUsesDefaultModel(t *testing.T) {
	handler, _ := New(&inferenceResourcesFakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"hi"}`))
	request.Header.Set("Authorization", "Bearer gw")
	response := httptest.NewRecorder()
	handler.Moderations(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "omni-moderation-latest") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestFileUploadRequiresExplicitRouteAndPreservesBytes(t *testing.T) {
	service := &inferenceResourcesFakeService{}
	handler, _ := New(service, 1<<20)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "input.jsonl")
	_, _ = part.Write([]byte("payload"))
	_ = writer.WriteField("purpose", "batch")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/files", &body)
	request.Header.Set("Authorization", "Bearer gw")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Halro-Route", "batch-model")
	request.Header.Set("Idempotency-Key", "upload-1")
	response := httptest.NewRecorder()
	handler.CreateFile(response, request)
	if response.Code != 200 || service.route != "batch-model" || string(service.file.Data) != "payload" {
		t.Fatalf("status=%d route=%q file=%#v body=%s", response.Code, service.route, service.file, response.Body.String())
	}
}

func TestRerankResponseUsesDeclaredWireShape(t *testing.T) {
	handler, _ := New(&inferenceResourcesFakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"rerank","query":"q","documents":["a","b"],"top_n":1}`))
	request.Header.Set("Authorization", "Bearer gw")
	response := httptest.NewRecorder()
	handler.Rerank(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["results"]; !ok || len(body) != 1 {
		t.Fatalf("unexpected rerank wire shape: %s", response.Body.String())
	}
}

func TestAsyncResponseUsesDeclaredWireShapeAndHidesProviderRequestID(t *testing.T) {
	handler, _ := New(&inferenceResourcesFakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/async/invocations", strings.NewReader(`{"model":"video","prompt":"scene","s3_output_uri":"s3://bucket/output","duration_seconds":6}`))
	request.Header.Set("Authorization", "Bearer gw")
	request.Header.Set("Idempotency-Key", "async-1")
	response := httptest.NewRecorder()
	handler.StartAsyncInvoke(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"invocation_arn", "status", "s3_output_uri", "submitted_at"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("missing %s: %s", field, response.Body.String())
		}
	}
	for _, forbidden := range []string{"ProviderRequestID", "provider_request_id", "InvocationARN", "SubmittedAt", "last_modified_at"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("unexpected %s: %s", forbidden, response.Body.String())
		}
	}
}
