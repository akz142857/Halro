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

	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
)

type phase2FakeService struct {
	fakeService
	route string
	file  provider.FileCreateCall
}

func (s *phase2FakeService) Moderations(_ context.Context, _ string, r openaiapi.ModerationRequest) (openaiapi.ModerationResponse, error) {
	return openaiapi.ModerationResponse{ID: "modr_1", Model: r.Model, Results: json.RawMessage(`[{"flagged":false}]`)}, nil
}
func (s *phase2FakeService) Images(context.Context, string, openaiapi.ImageGenerationRequest) (openaiapi.ImageGenerationResponse, error) {
	return openaiapi.ImageGenerationResponse{}, nil
}
func (s *phase2FakeService) Speech(context.Context, string, openaiapi.SpeechRequest) (provider.SpeechResult, error) {
	return provider.SpeechResult{ContentType: "audio/mpeg", Data: []byte("audio")}, nil
}
func (s *phase2FakeService) Transcription(context.Context, string, string, provider.TranscriptionCall) (provider.TranscriptionResult, error) {
	return provider.TranscriptionResult{ContentType: "application/json", Data: []byte(`{"text":"ok"}`)}, nil
}
func (s *phase2FakeService) Rerank(context.Context, string, openaiapi.RerankRequest) (provider.RerankResult, error) {
	return provider.RerankResult{}, nil
}
func (s *phase2FakeService) StartAsyncInvoke(context.Context, string, string, openaiapi.AsyncInvokeRequest) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{InvocationARN: "async_1", Status: "InProgress"}, nil
}
func (s *phase2FakeService) GetAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{}, nil
}
func (s *phase2FakeService) CancelAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error) {
	return provider.AsyncInvokeObject{}, nil
}
func (s *phase2FakeService) CreateFile(_ context.Context, _ string, route, _ string, call provider.FileCreateCall) (provider.FileObject, error) {
	s.route = route
	s.file = call
	return provider.FileObject{ID: "file_1", Object: "file", Bytes: int64(len(call.Data)), Filename: call.Filename, Purpose: call.Purpose}, nil
}
func (s *phase2FakeService) GetFile(context.Context, string, string) (provider.FileObject, error) {
	return provider.FileObject{}, nil
}
func (s *phase2FakeService) DownloadFile(context.Context, string, string) (provider.FileContent, error) {
	return provider.FileContent{}, nil
}
func (s *phase2FakeService) DeleteFile(context.Context, string, string) (provider.FileDeleteResult, error) {
	return provider.FileDeleteResult{}, nil
}
func (s *phase2FakeService) CreateBatch(context.Context, string, string, provider.BatchCreateCall) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (s *phase2FakeService) GetBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}
func (s *phase2FakeService) CancelBatch(context.Context, string, string) (provider.BatchObject, error) {
	return provider.BatchObject{}, nil
}

func TestModerationsNorthboundRejectsUnknownFields(t *testing.T) {
	handler, _ := New(&phase2FakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"hi","unknown":true}`))
	request.Header.Set("Authorization", "Bearer gw")
	response := httptest.NewRecorder()
	handler.Moderations(response, request)
	if response.Code != 400 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestModerationsNorthboundUsesDefaultModel(t *testing.T) {
	handler, _ := New(&phase2FakeService{}, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/moderations", strings.NewReader(`{"input":"hi"}`))
	request.Header.Set("Authorization", "Bearer gw")
	response := httptest.NewRecorder()
	handler.Moderations(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "omni-moderation-latest") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestFileUploadRequiresExplicitRouteAndPreservesBytes(t *testing.T) {
	service := &phase2FakeService{}
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
	request.Header.Set("Heimdall-Route", "batch-model")
	request.Header.Set("Idempotency-Key", "upload-1")
	response := httptest.NewRecorder()
	handler.CreateFile(response, request)
	if response.Code != 200 || service.route != "batch-model" || string(service.file.Data) != "payload" {
		t.Fatalf("status=%d route=%q file=%#v body=%s", response.Code, service.route, service.file, response.Body.String())
	}
}
