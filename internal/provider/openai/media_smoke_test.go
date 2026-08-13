package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/provider"
)

// TestRealMediaResourcesSmoke contacts a real OpenAI account and costs money. It
// covers `openai.media-resources.v1`, the profile that carries moderations,
// images, speech, transcription, files and batches — six operations with no
// real-account evidence at all until this existed. The chat smoke beside it
// proves a different profile and says nothing about these.
//
// It is separate from TestRealProviderSmoke rather than folded into it because
// the two have different costs and different side effects. Chat and embeddings
// are cheap and leave nothing behind; an image generation is priced like a
// generation, and files and batches create objects on the operator's account.
// Folding them together would make the cheap evidence unobtainable without
// paying for the expensive kind.
//
//	HALRO_REAL_PROVIDER_SMOKE=1
//	HALRO_SMOKE_PROFILE=openai_media
//	HALRO_SMOKE_BASE_URL=https://api.openai.com
//	HALRO_SMOKE_API_KEY=<OpenAI API key>
//
// Every operation is opted into by naming its model, so an operator can buy
// exactly the evidence they want:
//
//	HALRO_SMOKE_MODERATION_MODEL=omni-moderation-latest
//	HALRO_SMOKE_SPEECH_MODEL=gpt-4o-mini-tts
//	HALRO_SMOKE_TRANSCRIPTION_MODEL=gpt-4o-mini-transcribe
//	HALRO_SMOKE_IMAGE_MODEL=gpt-image-1            (the expensive one)
//	HALRO_SMOKE_RESOURCE_LIFECYCLE=1               (files and batches)
//
// Structure is asserted, never content: no transcript, no image, no moderation
// verdict is printed. The runner captures this process's output into an
// evidence file, and none of those belong in one.
func TestRealMediaResourcesSmoke(t *testing.T) {
	if os.Getenv("HALRO_REAL_PROVIDER_SMOKE") != "1" || os.Getenv("HALRO_SMOKE_PROFILE") != "openai_media" {
		t.Skip("set HALRO_REAL_PROVIDER_SMOKE=1 and HALRO_SMOKE_PROFILE=openai_media")
	}
	endpoint, err := url.Parse(os.Getenv("HALRO_SMOKE_BASE_URL"))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		t.Fatal("HALRO_SMOKE_BASE_URL must be an absolute HTTPS URL")
	}
	apiKey := os.Getenv("HALRO_SMOKE_API_KEY")
	if apiKey == "" {
		t.Fatal("HALRO_SMOKE_API_KEY is required")
	}
	adapter, err := NewWithOptions(Options{
		Endpoint: endpoint, APIKey: []byte(apiKey), Client: &http.Client{Timeout: 120 * time.Second},
		ProviderType: "openai",
		Capabilities: provider.Capabilities{
			Moderations: true, Images: true, Transcriptions: true, Speech: true, Files: true, Batches: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Speech runs before transcription so the audio it produces is what gets
	// transcribed. A fixture committed to the repository would be a recording
	// nobody can regenerate, and it would drift from whatever the transcription
	// models actually accept.
	var spokenAudio provider.SpeechResult
	if model := os.Getenv("HALRO_SMOKE_MODERATION_MODEL"); model != "" {
		t.Run("moderations", func(t *testing.T) { smokeModerations(ctx, t, adapter, model) })
	}
	if model := os.Getenv("HALRO_SMOKE_SPEECH_MODEL"); model != "" {
		t.Run("speech", func(t *testing.T) { spokenAudio = smokeSpeech(ctx, t, adapter, model) })
	}
	if model := os.Getenv("HALRO_SMOKE_TRANSCRIPTION_MODEL"); model != "" {
		t.Run("transcriptions", func(t *testing.T) { smokeTranscription(ctx, t, adapter, model, spokenAudio) })
	}
	if model := os.Getenv("HALRO_SMOKE_IMAGE_MODEL"); model != "" {
		t.Run("images", func(t *testing.T) { smokeImages(ctx, t, adapter, model) })
	}
	if os.Getenv("HALRO_SMOKE_RESOURCE_LIFECYCLE") == "1" {
		t.Run("files and batches", func(t *testing.T) { smokeResourceLifecycle(ctx, t, adapter) })
	}
}

func smokeModerations(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	result, err := adapter.Moderate(ctx, provider.ModerationCall{
		RequestID: "media-smoke-moderation", ProviderModel: model,
		Input: json.RawMessage(`"a harmless sentence about gardening"`),
	})
	if err != nil {
		t.Fatalf("moderation failed: %s", smokeErrorClass(err))
	}
	if result.ID == "" || result.Model == "" {
		t.Fatalf("moderation returned an incomplete envelope: id=%q model=%q", result.ID, result.Model)
	}
	// The verdict itself is not asserted and not printed. What matters here is
	// that the envelope decodes into the shape the gateway re-renders.
	var verdicts []map[string]any
	if err := json.Unmarshal(result.Results, &verdicts); err != nil || len(verdicts) == 0 {
		t.Fatalf("moderation results did not decode into a list: %v", err)
	}
}

func smokeSpeech(ctx context.Context, t *testing.T, adapter *Adapter, model string) provider.SpeechResult {
	t.Helper()
	result, err := adapter.Synthesize(ctx, provider.SpeechCall{
		RequestID: "media-smoke-speech", ProviderModel: model,
		Voice: "alloy", Input: "Halro smoke test.", ResponseFormat: "mp3",
	})
	if err != nil {
		t.Fatalf("speech failed: %s", smokeErrorClass(err))
	}
	if len(result.Data) == 0 {
		t.Fatal("speech returned no audio")
	}
	if !strings.HasPrefix(result.ContentType, "audio/") {
		t.Fatalf("speech returned content type %q, which is not audio", result.ContentType)
	}
	return result
}

// smokeTranscription feeds back what speech produced. Without a speech run it
// has nothing to send, and it says so rather than inventing silence: an empty
// upload would exercise the error path, not the operation.
func smokeTranscription(ctx context.Context, t *testing.T, adapter *Adapter, model string, spoken provider.SpeechResult) {
	t.Helper()
	if len(spoken.Data) == 0 {
		t.Skip("transcription needs audio; set HALRO_SMOKE_SPEECH_MODEL so speech can produce it")
	}
	result, err := adapter.Transcribe(ctx, provider.TranscriptionCall{
		RequestID: "media-smoke-transcription", ProviderModel: model,
		Filename: "smoke.mp3", ContentType: spoken.ContentType, Data: spoken.Data,
		ResponseFormat: "json",
	})
	if err != nil {
		t.Fatalf("transcription failed: %s", smokeErrorClass(err))
	}
	if len(result.Data) == 0 {
		t.Fatal("transcription returned no body")
	}
	var transcript struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.Data, &transcript); err != nil {
		t.Fatalf("transcription body did not decode as JSON: %v", err)
	}
	// The transcript text is not asserted or printed — it is model output. That
	// a non-empty one came back is the whole claim.
	if strings.TrimSpace(transcript.Text) == "" {
		t.Fatal("transcription returned an empty transcript")
	}
}

func smokeImages(ctx context.Context, t *testing.T, adapter *Adapter, model string) {
	t.Helper()
	result, err := adapter.GenerateImage(ctx, provider.ImageCall{
		RequestID: "media-smoke-image", ProviderModel: model,
		Prompt: "a plain grey square", Count: 1, Size: "1024x1024",
	})
	if err != nil {
		t.Fatalf("image generation failed: %s", smokeErrorClass(err))
	}
	if len(result.Data) == 0 {
		t.Fatal("image generation returned no data")
	}
	if result.Data[0].URL == "" && result.Data[0].Base64JSON == "" {
		t.Fatal("image generation returned neither a URL nor inline data")
	}
}

// smokeResourceLifecycle is the only part of this file that leaves anything
// behind. It uploads a file, reads it back, drives a batch through create and
// cancel, and deletes the file at the end. The batch record itself survives in
// a terminal state — OpenAI has no delete for batches — which is exactly why
// this is behind its own switch and why automatic capability detection excludes
// persistent primitives entirely.
func smokeResourceLifecycle(ctx context.Context, t *testing.T, adapter *Adapter) {
	t.Helper()
	line := `{"custom_id":"smoke-1","method":"POST","url":"/v1/chat/completions",` +
		`"body":{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Reply with OK."}],"max_completion_tokens":8}}`
	created, err := adapter.CreateFile(ctx, provider.FileCreateCall{
		RequestID: "media-smoke-file", Filename: "halro-smoke.jsonl",
		ContentType: "application/jsonl", Purpose: "batch", Data: []byte(line + "\n"),
	})
	if err != nil {
		t.Fatalf("file create failed: %s", smokeErrorClass(err))
	}
	if created.ID == "" {
		t.Fatal("file create returned no identifier")
	}
	// Deleted even if a later step fails, so a failed run does not leave the
	// operator's account holding the upload.
	defer func() {
		deleted, err := adapter.DeleteFile(context.WithoutCancel(ctx), "media-smoke-file-delete", created.ID)
		if err != nil {
			t.Errorf("file delete failed, the upload is still on the account: %s", smokeErrorClass(err))
			return
		}
		if !deleted.Deleted {
			t.Error("file delete reported the file was not deleted")
		}
	}()

	fetched, err := adapter.GetFile(ctx, "media-smoke-file-get", created.ID)
	if err != nil {
		t.Fatalf("file get failed: %s", smokeErrorClass(err))
	}
	if fetched.ID != created.ID || fetched.Bytes == 0 {
		t.Fatalf("file get returned a different or empty object: %#v", fetched)
	}
	content, err := adapter.DownloadFile(ctx, "media-smoke-file-content", created.ID)
	if err != nil {
		t.Fatalf("file download failed: %s", smokeErrorClass(err))
	}
	if len(content.Data) == 0 {
		t.Fatal("file download returned no bytes")
	}

	batch, err := adapter.CreateBatch(ctx, provider.BatchCreateCall{
		RequestID: "media-smoke-batch", InputFileID: created.ID,
		Endpoint: "/v1/chat/completions", CompletionWindow: "24h",
	})
	if err != nil {
		t.Fatalf("batch create failed: %s", smokeErrorClass(err))
	}
	if batch.ID == "" || batch.Status == "" {
		t.Fatalf("batch create returned an incomplete object: %#v", batch)
	}
	if got, err := adapter.GetBatch(ctx, "media-smoke-batch-get", batch.ID); err != nil {
		t.Fatalf("batch get failed: %s", smokeErrorClass(err))
	} else if got.ID != batch.ID {
		t.Fatalf("batch get returned a different batch: %q", got.ID)
	}
	// Cancelled immediately: this smoke buys evidence that the lifecycle works,
	// not a completed batch. A batch left to run would bill every request in the
	// input file and hold a slot for up to the completion window.
	cancelled, err := adapter.CancelBatch(ctx, "media-smoke-batch-cancel", batch.ID)
	if err != nil {
		t.Fatalf("batch cancel failed, the batch may still run: %s", smokeErrorClass(err))
	}
	switch cancelled.Status {
	case "cancelling", "cancelled", "failed", "completed", "expired":
	default:
		t.Fatalf("batch cancel left status %q, which is not a terminal or cancelling state", cancelled.Status)
	}
}
