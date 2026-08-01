package anthropicapi

import "testing"

func TestStreamValidatorAcceptsThinkingSignatureLifecycle(t *testing.T) {
	validator := NewStreamValidator()
	events := []RawStreamEvent{
		{"message_start", []byte(`{"type":"message_start","message":{"id":"msg_1"}}`)},
		{"content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)},
		{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"x"}}`)},
		{"content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)},
		{"content_block_stop", []byte(`{"type":"content_block_stop","index":0}`)},
		{"message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)},
		{"message_stop", []byte(`{"type":"message_stop"}`)},
	}
	for _, event := range events {
		if err := validator.Accept(event); err != nil {
			t.Fatalf("%s: %v", event.Type, err)
		}
	}
	if err := validator.Finalize(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamValidatorRejectsMismatchedEventName(t *testing.T) {
	validator := NewStreamValidator()
	if err := validator.Accept(RawStreamEvent{"message_start", []byte(`{"type":"ping"}`)}); err == nil {
		t.Fatal("expected mismatch rejection")
	}
}
