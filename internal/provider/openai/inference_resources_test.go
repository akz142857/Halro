package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
)

func inferenceResourcesTestAdapter(t *testing.T, transport roundTripFunc) *Adapter {
	t.Helper()
	endpoint, _ := url.Parse("https://api.openai.com")
	adapter, err := New(endpoint, []byte("secret"), &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestModerationInferenceResourcesUsesTypedWireContract(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.openai.com/v1/moderations" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"omni-moderation-latest"` || string(body["input"]) != `"hello"` {
			t.Fatalf("body=%s", body)
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		header.Set("x-request-id", "up_1")
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(`{"id":"modr_1","model":"omni-moderation-latest","results":[{"flagged":false}]}`)), Request: request}, nil
	})
	result, err := adapter.Moderate(context.Background(), provider.ModerationCall{RequestID: "req_1", ProviderModel: "omni-moderation-latest", Input: json.RawMessage(`"hello"`)})
	if err != nil || result.ID != "modr_1" || result.ProviderRequestID != "up_1" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestImageInferenceResourcesRejectsAmbiguousPayload(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"url":"https://x","b64_json":"also"}]}`)), Request: request}, nil
	})
	_, err := adapter.GenerateImage(context.Background(), provider.ImageCall{ProviderModel: "gpt-image-2.5-flare", Prompt: "test", Count: 1})
	var providerErr *provider.Error
	if err == nil || !errors.As(err, &providerErr) || providerErr.Class != provider.ErrorMalformed {
		t.Fatalf("err=%v", err)
	}
}

func TestSpeechInferenceResourcesReturnsBoundedBinary(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/audio/speech" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("request=%#v", request)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio")), Request: request}, nil
	})
	result, err := adapter.Synthesize(context.Background(), provider.SpeechCall{ProviderModel: "gpt-4o-mini-tts", Input: "hello", Voice: "alloy"})
	if err != nil || result.ContentType != "audio/mpeg" || string(result.Data) != "audio" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestFileLifecycleUsesEscapedFixedResourcePaths(t *testing.T) {
	calls := 0
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/v1/files/file-abc" || request.Method != http.MethodGet {
			t.Fatalf("request=%s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"file-abc","object":"file","bytes":2,"created_at":1,"filename":"in.jsonl","purpose":"batch"}`)), Request: request}, nil
	})
	file, err := adapter.GetFile(context.Background(), "req", "file-abc")
	if err != nil || file.Filename != "in.jsonl" || calls != 1 {
		t.Fatalf("file=%#v calls=%d err=%v", file, calls, err)
	}
	if _, err := adapter.GetFile(context.Background(), "req", "file-abc/content"); err == nil || calls != 1 {
		t.Fatalf("unsafe id reached transport: err=%v calls=%d", err, calls)
	}
}

func TestBatchCreationIsLimitedToDeclaredEndpoints(t *testing.T) {
	calls := 0
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("should not be called")
	})
	_, err := adapter.CreateBatch(context.Background(), provider.BatchCreateCall{InputFileID: "file-abc", Endpoint: "/v1/fine_tuning/jobs", CompletionWindow: "24h"})
	if err == nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestCurrentGPTImageParametersReachTheWire(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["output_format"]) != `"webp"` || string(body["background"]) != `"transparent"` || string(body["output_compression"]) != "0" || body["response_format"] != nil || body["style"] != nil {
			t.Fatalf("incorrect image dialect: %v", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"data":[{"b64_json":"image"}]}`))}, nil
	})
	compression := 0
	_, err := adapter.GenerateImage(context.Background(), provider.ImageCall{ProviderModel: "gpt-image-2.5-flare", Prompt: "test", Count: 1, OutputFormat: "webp", Background: "transparent", OutputCompression: &compression})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTranscribeLanguageAndKeywordHintsUseCurrentMultipartFields(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(request *http.Request) (*http.Response, error) {
		reader, err := request.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string][]string{}
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(part)
			fields[part.FormName()] = append(fields[part.FormName()], string(body))
		}
		if len(fields["languages[]"]) != 1 || fields["languages[]"][0] != "en" || len(fields["keywords[]"]) != 2 || fields["language"] != nil {
			t.Fatalf("incorrect transcription hints: %v", fields)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"text":"ok","languages":[{"code":"en"}]}`))}, nil
	})
	_, err := adapter.Transcribe(context.Background(), provider.TranscriptionCall{ProviderModel: "gpt-transcribe", Filename: "audio.wav", ContentType: "audio/wav", Data: []byte("audio"), Language: "en", Keywords: []string{"OpenAI", "Halro"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRetiredImagesAndUnsupportedMediaControlsAreRefusedBeforeIO(t *testing.T) {
	adapter := inferenceResourcesTestAdapter(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid media request reached upstream")
		return nil, nil
	})
	for _, call := range []provider.ImageCall{{ProviderModel: "gpt-image-1", Prompt: "x", Count: 1}, {ProviderModel: "dall-e-3", Prompt: "x", Count: 1}, {ProviderModel: "gpt-image-2.5-flare", Prompt: "x", Count: 1, ResponseFormat: "url"}} {
		if _, err := adapter.GenerateImage(context.Background(), call); err == nil {
			t.Fatal("unsupported image request accepted")
		}
	}
	if _, err := adapter.Transcribe(context.Background(), provider.TranscriptionCall{ProviderModel: "gpt-transcribe", Filename: "x.wav", Data: []byte("audio"), Keywords: []string{"invalid\nkeyword"}}); err == nil {
		t.Fatal("invalid keyword accepted")
	}
}
