package provider

import (
	"context"
	"encoding/json"
	"time"
)

type ModerationCall struct {
	RequestID, ProviderModel string
	Input                    json.RawMessage
}
type ModerationResult struct {
	ID, Model         string
	Results           json.RawMessage
	ProviderRequestID string
}
type ImageCall struct {
	RequestID, ProviderModel, Prompt, Quality, Size, ResponseFormat, Style string
	Count                                                                  int
}
type ImageData struct {
	URL           string `json:"url,omitempty"`
	Base64JSON    string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
type ImageResult struct {
	Created           int64
	Data              []ImageData
	ProviderRequestID string
}
type TranscriptionCall struct {
	RequestID, ProviderModel, Filename, ContentType, Language, Prompt, ResponseFormat string
	Temperature                                                                       *float64
	Data                                                                              []byte
}
type TranscriptionResult struct {
	ContentType       string
	Data              []byte
	ProviderRequestID string
}
type SpeechCall struct {
	RequestID, ProviderModel, Voice, Input, ResponseFormat string
	Speed                                                  *float64
}
type SpeechResult struct {
	ContentType       string
	Data              []byte
	ProviderRequestID string
}

type StatelessInferenceResourcesAdapter interface {
	Moderate(context.Context, ModerationCall) (ModerationResult, error)
	GenerateImage(context.Context, ImageCall) (ImageResult, error)
	Transcribe(context.Context, TranscriptionCall) (TranscriptionResult, error)
	Synthesize(context.Context, SpeechCall) (SpeechResult, error)
}

type FileCreateCall struct {
	RequestID, Filename, ContentType, Purpose string
	Data                                      []byte
}
type FileObject struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Bytes         int64  `json:"bytes"`
	CreatedAt     int64  `json:"created_at"`
	Filename      string `json:"filename"`
	Purpose       string `json:"purpose"`
	Status        string `json:"status,omitempty"`
	StatusDetails string `json:"status_details,omitempty"`
}
type FileContent struct {
	ContentType       string
	Data              []byte
	ProviderRequestID string
}
type FileDeleteResult struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Deleted           bool   `json:"deleted"`
	ProviderRequestID string `json:"-"`
}
type BatchCreateCall struct {
	RequestID, InputFileID, Endpoint, CompletionWindow string
	Metadata                                           map[string]string
	// InputRequests carries the batch's input lines when the upstream has no
	// copy of the file to refer to. An upstream whose batches are created from
	// an uploaded file gets InputFileID and ignores this; one whose batches
	// carry their requests inline has no file to name and needs the bytes.
	//
	// The gateway fills it exactly when the input file is local-only, which is
	// the same condition: if the upstream was never given the file, the requests
	// have to travel with the batch.
	InputRequests []byte
}
type BatchObject struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Endpoint         string            `json:"endpoint"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	Status           string            `json:"status"`
	OutputFileID     string            `json:"output_file_id,omitempty"`
	ErrorFileID      string            `json:"error_file_id,omitempty"`
	CreatedAt        int64             `json:"created_at"`
	ExpiresAt        int64             `json:"expires_at"`
	CompletedAt      int64             `json:"completed_at,omitempty"`
	FailedAt         int64             `json:"failed_at,omitempty"`
	CancellingAt     int64             `json:"cancelling_at,omitempty"`
	CancelledAt      int64             `json:"cancelled_at,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	RawErrors        json.RawMessage   `json:"errors,omitempty"`
}

type ResourceInferenceResourcesAdapter interface {
	CreateFile(context.Context, FileCreateCall) (FileObject, error)
	GetFile(context.Context, string, string) (FileObject, error)
	DownloadFile(context.Context, string, string) (FileContent, error)
	DeleteFile(context.Context, string, string) (FileDeleteResult, error)
	CreateBatch(context.Context, BatchCreateCall) (BatchObject, error)
	GetBatch(context.Context, string, string) (BatchObject, error)
	CancelBatch(context.Context, string, string) (BatchObject, error)
}

type RerankCall struct {
	RequestID, ProviderModel, Query string
	Documents                       []string
	TopN                            int
}
type RerankItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}
type RerankResult struct {
	Results           []RerankItem
	ProviderRequestID string
}
type AsyncInvokeCall struct {
	RequestID, ProviderModel, Prompt, S3OutputURI string
	DurationSeconds                               int
	Dimension, FPS                                string
	Seed                                          *int64
}
type AsyncInvokeObject struct {
	InvocationARN, Status, S3OutputURI, FailureMessage, ProviderRequestID string
	SubmittedAt, LastModifiedAt                                           time.Time
}
type BedrockInferenceResourcesAdapter interface {
	Rerank(context.Context, RerankCall) (RerankResult, error)
	StartAsyncInvoke(context.Context, AsyncInvokeCall) (AsyncInvokeObject, error)
	GetAsyncInvoke(context.Context, string, string) (AsyncInvokeObject, error)
	GenerateBedrockImage(context.Context, ImageCall) (ImageResult, error)
}
