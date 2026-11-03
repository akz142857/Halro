package anthropicapi

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Keep the old default for existing integrations. SDKs released with the new
// contract send the current date explicitly; their requests retain that date.
const CurrentVersion = "2026-10-21"

func IsSupportedVersion(version string) bool {
	return version == SupportedVersion || version == CurrentVersion
}

// ValidateVersionPayload applies the new version's removals before admission.
// Raw presence matters: even null sampling members are no longer accepted.
func ValidateVersionPayload(version string, payload json.RawMessage) error {
	if !IsSupportedVersion(version) {
		return errors.New("unsupported anthropic-version")
	}
	if version != CurrentVersion {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		return err
	}
	for _, field := range []string{"temperature", "top_p", "top_k"} {
		if _, exists := body[field]; exists {
			return fmt.Errorf("%s is not supported by anthropic-version %s", field, version)
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		return err
	}
	for _, message := range messages {
		if message.Role == "user" {
			if err := validateUserCitations(message.Content); err != nil {
				return err
			}
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var kind string
			_ = json.Unmarshal(block["type"], &kind)
			if message.Role == "assistant" && kind == "document" {
				return errors.New("document blocks must be in user messages in anthropic-version " + CurrentVersion)
			}
		}
	}
	return nil
}

func validateUserCitations(content json.RawMessage) error {
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil // String content has no citation members.
	}
	for _, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		if kind == "text" && len(block["citations"]) > 0 {
			var citations []json.RawMessage
			if err := json.Unmarshal(block["citations"], &citations); err != nil {
				return err
			}
			if len(citations) > 0 {
				return errors.New("user text citations are not supported by anthropic-version " + CurrentVersion)
			}
		}
		if kind == "search_result" || kind == "tool_result" {
			if err := validateUserCitations(block["content"]); err != nil {
				return err
			}
		}
	}
	return nil
}
