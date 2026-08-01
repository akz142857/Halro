package openaiapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type ModerationRequest struct {
	Input json.RawMessage `json:"input"`
	Model string          `json:"model,omitempty"`
}
type ModerationResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Results json.RawMessage `json:"results"`
}
type ImageGenerationRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	N              int    `json:"n,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Size           string `json:"size,omitempty"`
	Style          string `json:"style,omitempty"`
}
type ImageGenerationResponse struct {
	Created int64       `json:"created"`
	Data    []ImageData `json:"data"`
}
type ImageData struct {
	URL           string `json:"url,omitempty"`
	Base64JSON    string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}
type SpeechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format,omitempty"`
	Speed          *float64 `json:"speed,omitempty"`
}
type RerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}
type AsyncInvokeRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	S3OutputURI     string `json:"s3_output_uri"`
	DurationSeconds int    `json:"duration_seconds"`
	Dimension       string `json:"dimension,omitempty"`
	FPS             string `json:"fps,omitempty"`
	Seed            *int64 `json:"seed,omitempty"`
}
type BatchCreateRequest struct {
	InputFileID      string            `json:"input_file_id"`
	Endpoint         string            `json:"endpoint"`
	CompletionWindow string            `json:"completion_window"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

func DecodeModerationRequest(decoder *json.Decoder) (ModerationRequest, error) {
	var value ModerationRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if len(value.Input) == 0 || !json.Valid(value.Input) || bytes.Equal(bytes.TrimSpace(value.Input), []byte("null")) {
		return value, errors.New("input is required")
	}
	if value.Model == "" {
		value.Model = "omni-moderation-latest"
	}
	return value, nil
}
func DecodeImageGenerationRequest(decoder *json.Decoder) (ImageGenerationRequest, error) {
	var value ImageGenerationRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if strings.TrimSpace(value.Prompt) == "" {
		return value, errors.New("prompt is required")
	}
	if value.N == 0 {
		value.N = 1
	}
	if value.N < 1 || value.N > 10 {
		return value, errors.New("n must be between 1 and 10")
	}
	return value, nil
}
func DecodeSpeechRequest(decoder *json.Decoder) (SpeechRequest, error) {
	var value SpeechRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if value.Model == "" || strings.TrimSpace(value.Input) == "" || value.Voice == "" {
		return value, errors.New("model, input, and voice are required")
	}
	if value.Speed != nil && (*value.Speed < 0.25 || *value.Speed > 4) {
		return value, errors.New("speed must be between 0.25 and 4")
	}
	return value, nil
}
func DecodeRerankRequest(decoder *json.Decoder) (RerankRequest, error) {
	var value RerankRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if value.Model == "" || strings.TrimSpace(value.Query) == "" || len(value.Documents) == 0 {
		return value, errors.New("model, query, and documents are required")
	}
	if value.TopN == 0 {
		value.TopN = len(value.Documents)
	}
	if value.TopN < 1 || value.TopN > len(value.Documents) {
		return value, errors.New("top_n is invalid")
	}
	return value, nil
}
func DecodeAsyncInvokeRequest(decoder *json.Decoder) (AsyncInvokeRequest, error) {
	var value AsyncInvokeRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if value.Model == "" || strings.TrimSpace(value.Prompt) == "" || value.S3OutputURI == "" {
		return value, errors.New("model, prompt, and s3_output_uri are required")
	}
	return value, nil
}
func DecodeBatchCreateRequest(decoder *json.Decoder) (BatchCreateRequest, error) {
	var value BatchCreateRequest
	if err := decodeStrict(decoder, &value); err != nil {
		return value, err
	}
	if value.InputFileID == "" || value.Endpoint == "" || value.CompletionWindow == "" {
		return value, errors.New("input_file_id, endpoint, and completion_window are required")
	}
	if len(value.Metadata) > 16 {
		return value, errors.New("metadata exceeds limit")
	}
	for key, item := range value.Metadata {
		if key == "" || len(key) > 64 || len(item) > 512 {
			return value, errors.New("metadata is invalid")
		}
	}
	return value, nil
}
func decodeStrict(decoder *json.Decoder, target any) error {
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
