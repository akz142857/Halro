package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/akz142857/Heimdall/internal/gateway"
	"github.com/akz142857/Heimdall/internal/openaiapi"
	"github.com/akz142857/Heimdall/internal/provider"
	"github.com/go-chi/chi/v5"
)

type Phase2Service interface {
	Moderations(context.Context, string, openaiapi.ModerationRequest) (openaiapi.ModerationResponse, error)
	Images(context.Context, string, openaiapi.ImageGenerationRequest) (openaiapi.ImageGenerationResponse, error)
	Speech(context.Context, string, openaiapi.SpeechRequest) (provider.SpeechResult, error)
	Transcription(context.Context, string, string, provider.TranscriptionCall) (provider.TranscriptionResult, error)
	Rerank(context.Context, string, openaiapi.RerankRequest) (provider.RerankResult, error)
	StartAsyncInvoke(context.Context, string, string, openaiapi.AsyncInvokeRequest) (provider.AsyncInvokeObject, error)
	GetAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error)
	CancelAsyncInvoke(context.Context, string, string) (provider.AsyncInvokeObject, error)
	CreateFile(context.Context, string, string, string, provider.FileCreateCall) (provider.FileObject, error)
	GetFile(context.Context, string, string) (provider.FileObject, error)
	DownloadFile(context.Context, string, string) (provider.FileContent, error)
	DeleteFile(context.Context, string, string) (provider.FileDeleteResult, error)
	CreateBatch(context.Context, string, string, provider.BatchCreateCall) (provider.BatchObject, error)
	GetBatch(context.Context, string, string) (provider.BatchObject, error)
	CancelBatch(context.Context, string, string) (provider.BatchObject, error)
}

func (h *Handler) phase2JSON(writer http.ResponseWriter, request *http.Request, decode func(*json.Decoder) (any, error), invoke func(context.Context, string, any) (any, error)) {
	writer.Header().Set("Cache-Control", "no-store")
	request, ok := h.withSourceIP(writer, request)
	if !ok {
		return
	}
	key, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="heimdall"`)
		writeError(writer, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	if h.phase2 == nil {
		writeError(writer, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxRequestBytes)
	decoded, err := decode(json.NewDecoder(request.Body))
	if err != nil {
		writeError(writer, 400, "invalid_request_error", "invalid request body", nil)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.routeTimeout)
	defer cancel()
	response, err := invoke(ctx, key, decoded)
	if err != nil {
		h.phase2Error(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}
func (h *Handler) phase2Error(writer http.ResponseWriter, err error) {
	var gatewayError *gateway.Error
	if errors.As(err, &gatewayError) {
		setRetryAfter(writer, gatewayError.RetryAfter)
		writeError(writer, gatewayError.HTTPStatus, gatewayError.Code, gatewayError.Message, nil)
		return
	}
	writeError(writer, 500, "internal_error", "internal server error", nil)
}
func (h *Handler) Moderations(w http.ResponseWriter, r *http.Request) {
	h.phase2JSON(w, r, func(d *json.Decoder) (any, error) { return openaiapi.DecodeModerationRequest(d) }, func(ctx context.Context, key string, v any) (any, error) {
		return h.phase2.Moderations(ctx, key, v.(openaiapi.ModerationRequest))
	})
}
func (h *Handler) Images(w http.ResponseWriter, r *http.Request) {
	h.phase2JSON(w, r, func(d *json.Decoder) (any, error) { return openaiapi.DecodeImageGenerationRequest(d) }, func(ctx context.Context, key string, v any) (any, error) {
		return h.phase2.Images(ctx, key, v.(openaiapi.ImageGenerationRequest))
	})
}
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	h.phase2JSON(w, r, func(d *json.Decoder) (any, error) { return openaiapi.DecodeRerankRequest(d) }, func(ctx context.Context, key string, v any) (any, error) {
		return h.phase2.Rerank(ctx, key, v.(openaiapi.RerankRequest))
	})
}
func (h *Handler) StartAsyncInvoke(w http.ResponseWriter, r *http.Request) {
	h.phase2JSON(w, r, func(d *json.Decoder) (any, error) { return openaiapi.DecodeAsyncInvokeRequest(d) }, func(ctx context.Context, key string, v any) (any, error) {
		return h.phase2.StartAsyncInvoke(ctx, key, strings.TrimSpace(r.Header.Get("Idempotency-Key")), v.(openaiapi.AsyncInvokeRequest))
	})
}
func (h *Handler) GetAsyncInvoke(w http.ResponseWriter, r *http.Request) { h.asyncAction(w, r, false) }
func (h *Handler) CancelAsyncInvoke(w http.ResponseWriter, r *http.Request) {
	h.asyncAction(w, r, true)
}
func (h *Handler) asyncAction(w http.ResponseWriter, r *http.Request, cancelAsync bool) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	var result provider.AsyncInvokeObject
	var err error
	if cancelAsync {
		result, err = h.phase2.CancelAsyncInvoke(ctx, key, chi.URLParam(r, "asyncID"))
	} else {
		result, err = h.phase2.GetAsyncInvoke(ctx, key, chi.URLParam(r, "asyncID"))
	}
	if err != nil {
		h.phase2Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func (h *Handler) Speech(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	decoded, err := openaiapi.DecodeSpeechRequest(json.NewDecoder(r.Body))
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid request body", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	result, err := h.phase2.Speech(ctx, key, decoded)
	if err != nil {
		h.phase2Error(w, err)
		return
	}
	contentType := result.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(200)
	_, _ = w.Write(result.Data)
}
func (h *Handler) Transcriptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(w, 400, "invalid_request_error", "multipart/form-data is required", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "invalid_request_error", "invalid multipart body", nil)
		return
	}
	var call provider.TranscriptionCall
	model := ""
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, 400, "invalid_request_error", "invalid multipart body", nil)
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, h.maxRequestBytes+1))
		if err != nil || int64(len(data)) > h.maxRequestBytes {
			writeError(w, 400, "request_too_large", "multipart field exceeds limit", nil)
			return
		}
		name := part.FormName()
		switch name {
		case "file":
			if len(call.Data) != 0 || part.FileName() == "" {
				writeError(w, 400, "invalid_request_error", "exactly one file is required", nil)
				return
			}
			call.Data = data
			call.Filename = part.FileName()
			call.ContentType = part.Header.Get("Content-Type")
		case "model":
			model = string(data)
		case "language":
			call.Language = string(data)
		case "prompt":
			call.Prompt = string(data)
		case "response_format":
			call.ResponseFormat = string(data)
		case "temperature":
			value, e := strconv.ParseFloat(string(data), 64)
			if e != nil || value < 0 || value > 1 {
				writeError(w, 400, "invalid_request_error", "temperature is invalid", nil)
				return
			}
			call.Temperature = &value
		default:
			writeError(w, 400, "invalid_request_error", "unknown multipart field", nil)
			return
		}
	}
	if strings.TrimSpace(model) == "" || len(call.Data) == 0 {
		writeError(w, 400, "invalid_request_error", "model and file are required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	result, err := h.phase2.Transcription(ctx, key, model, call)
	if err != nil {
		h.phase2Error(w, err)
		return
	}
	w.Header().Set("Content-Type", result.ContentType)
	_, _ = w.Write(result.Data)
}

func (h *Handler) CreateFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	route := strings.TrimSpace(r.Header.Get("Heimdall-Route"))
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "invalid_request_error", "multipart/form-data is required", nil)
		return
	}
	var call provider.FileCreateCall
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, 400, "invalid_request_error", "invalid multipart body", nil)
			return
		}
		data, err := io.ReadAll(io.LimitReader(part, h.maxRequestBytes+1))
		if err != nil || int64(len(data)) > h.maxRequestBytes {
			writeError(w, 400, "request_too_large", "multipart field exceeds limit", nil)
			return
		}
		switch part.FormName() {
		case "file":
			if len(call.Data) != 0 || part.FileName() == "" {
				writeError(w, 400, "invalid_request_error", "exactly one file is required", nil)
				return
			}
			call.Data = data
			call.Filename = part.FileName()
			call.ContentType = part.Header.Get("Content-Type")
		case "purpose":
			call.Purpose = string(data)
		default:
			writeError(w, 400, "invalid_request_error", "unknown multipart field", nil)
			return
		}
	}
	if len(call.Data) == 0 || call.Purpose == "" {
		writeError(w, 400, "invalid_request_error", "file and purpose are required", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	result, err := h.phase2.CreateFile(ctx, key, route, strings.TrimSpace(r.Header.Get("Idempotency-Key")), call)
	if err != nil {
		h.phase2Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
func (h *Handler) GetFile(w http.ResponseWriter, r *http.Request)      { h.fileAction(w, r, "get") }
func (h *Handler) DownloadFile(w http.ResponseWriter, r *http.Request) { h.fileAction(w, r, "content") }
func (h *Handler) DeleteFile(w http.ResponseWriter, r *http.Request)   { h.fileAction(w, r, "delete") }
func (h *Handler) fileAction(w http.ResponseWriter, r *http.Request, action string) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	resourceID := chi.URLParam(r, "fileID")
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	switch action {
	case "get":
		result, err := h.phase2.GetFile(ctx, key, resourceID)
		if err != nil {
			h.phase2Error(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	case "content":
		result, err := h.phase2.DownloadFile(ctx, key, resourceID)
		if err != nil {
			h.phase2Error(w, err)
			return
		}
		w.Header().Set("Content-Type", result.ContentType)
		_, _ = w.Write(result.Data)
	case "delete":
		result, err := h.phase2.DeleteFile(ctx, key, resourceID)
		if err != nil {
			h.phase2Error(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
func (h *Handler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	h.phase2JSON(w, r, func(d *json.Decoder) (any, error) { return openaiapi.DecodeBatchCreateRequest(d) }, func(ctx context.Context, key string, v any) (any, error) {
		request := v.(openaiapi.BatchCreateRequest)
		return h.phase2.CreateBatch(ctx, key, strings.TrimSpace(r.Header.Get("Idempotency-Key")), provider.BatchCreateCall{InputFileID: request.InputFileID, Endpoint: request.Endpoint, CompletionWindow: request.CompletionWindow, Metadata: request.Metadata})
	})
}
func (h *Handler) GetBatch(w http.ResponseWriter, r *http.Request)    { h.batchAction(w, r, false) }
func (h *Handler) CancelBatch(w http.ResponseWriter, r *http.Request) { h.batchAction(w, r, true) }
func (h *Handler) batchAction(w http.ResponseWriter, r *http.Request, cancelBatch bool) {
	w.Header().Set("Cache-Control", "no-store")
	if h.phase2 == nil {
		writeError(w, 501, "unsupported_feature", "Phase 2 API is unavailable", nil)
		return
	}
	r, ok := h.withSourceIP(w, r)
	if !ok {
		return
	}
	key, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, 401, "invalid_api_key", "missing or invalid bearer token", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.routeTimeout)
	defer cancel()
	var result provider.BatchObject
	var err error
	if cancelBatch {
		result, err = h.phase2.CancelBatch(ctx, key, chi.URLParam(r, "batchID"))
	} else {
		result, err = h.phase2.GetBatch(ctx, key, chi.URLParam(r, "batchID"))
	}
	if err != nil {
		h.phase2Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
