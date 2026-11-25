package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/akz142857/Halro/internal/anthropicapi"
)

func usageEvent(kind, usage string) anthropicapi.RawStreamEvent {
	if kind == "message_start" {
		return anthropicapi.RawStreamEvent{Type: kind, Data: json.RawMessage(`{"message":{"usage":` + usage + `}}`)}
	}
	return anthropicapi.RawStreamEvent{Type: kind, Data: json.RawMessage(`{"usage":` + usage + `}`)}
}

// What a stream's usage record ends up saying, across the two spellings of the
// thinking span that this adapter now serves.
//
// The span is a display split of output tokens and is not priced, so nothing
// here moves money. What it moves is whether an operator can see what a
// reasoning model spent its budget on, which is how the Kimi output-bound
// finding was made in the first place.
func TestStreamUsageKeepsTheThinkingSpanWhicheverSpellingCarriesIt(t *testing.T) {
	for _, test := range []struct {
		name   string
		events []anthropicapi.RawStreamEvent
		want   int64
	}{
		{
			name: "the flat member, restated by the delta",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1}`),
				usageEvent("message_delta", `{"output_tokens":40,"thinking_tokens":30}`),
			},
			want: 30,
		},
		{
			// Kimi's Anthropic face. The flat member is absent and the count sits
			// under output_tokens_details, which a decoder reading only the flat
			// one reports as zero on every Kimi stream.
			name: "the nested member, which is the one Kimi sends",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1}`),
				usageEvent("message_delta", `{"output_tokens":40,"output_tokens_details":{"thinking_tokens":25}}`),
			},
			want: 25,
		},
		{
			// The defect this test was written for. message_start copies the whole
			// usage struct, so a span reported there and omitted from the delta was
			// overwritten with zero by the lines meant to carry it — the cache
			// tiers beside them had been guarded for exactly this and these had
			// not.
			name: "reported at the start and omitted from the delta",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1,"output_tokens_details":{"thinking_tokens":42}}`),
				usageEvent("message_delta", `{"output_tokens":40}`),
			},
			want: 42,
		},
		{
			name: "the same, in the flat spelling",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1,"thinking_tokens":42}`),
				usageEvent("message_delta", `{"output_tokens":40}`),
			},
			want: 42,
		},
		{
			// A delta that does restate the span wins, including when it changes
			// spelling: both members move together, so the record can never mix a
			// flat count from one event with a nested one from another and report a
			// total no upstream sent.
			name: "restated by the delta in the other spelling",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1,"thinking_tokens":42}`),
				usageEvent("message_delta", `{"output_tokens":40,"output_tokens_details":{"thinking_tokens":55}}`),
			},
			want: 55,
		},
		{
			name: "no span anywhere",
			events: []anthropicapi.RawStreamEvent{
				usageEvent("message_start", `{"input_tokens":10,"output_tokens":1}`),
				usageEvent("message_delta", `{"output_tokens":40}`),
			},
			want: 0,
		},
	} {
		var usage *anthropicapi.Usage
		for _, event := range test.events {
			usage = updateUsage(usage, event)
		}
		if got := usage.ReasoningTokens(); got != test.want {
			t.Errorf("%s: reasoning tokens %d, want %d", test.name, got, test.want)
		}
		// The guard must not cost the counts beside it. Output tokens are the
		// delta's own running total when that counter is present.
		if usage.OutputTokens != 40 {
			t.Errorf("%s: output tokens %d, want the delta's 40", test.name, usage.OutputTokens)
		}
		if usage.InputTokens != 10 {
			t.Errorf("%s: input tokens %d, want the start's 10", test.name, usage.InputTokens)
		}
	}
}

func TestStreamUsageDoesNotInventOrEraseTokenCounts(t *testing.T) {
	if usage := updateUsage(nil, usageEvent("message_start", `null`)); usage != nil {
		t.Fatalf("absent usage became %+v", usage)
	}
	if usage := updateUsage(nil, anthropicapi.RawStreamEvent{Type: "error"}); usage != nil {
		t.Fatalf("error frame invented usage %+v", usage)
	}
	if usage := updateUsage(nil, usageEvent("message_delta", `{}`)); usage != nil {
		t.Fatalf("empty delta invented usage %+v", usage)
	}
	usage := updateUsage(nil, usageEvent("message_start", `{"input_tokens":10,"output_tokens":1}`))
	if !usage.OutputIncomplete {
		t.Fatal("initial output count was marked final")
	}
	usage = updateUsage(usage, usageEvent("message_delta", `{"output_tokens":40}`))
	usage = updateUsage(usage, usageEvent("message_delta", `{"cache_creation_input_tokens":2}`))
	if usage.InputTokens != 10 || usage.OutputTokens != 40 || usage.CacheCreationInputTokens != 2 || usage.OutputIncomplete {
		t.Fatalf("partial delta corrupted counters: %+v", usage)
	}
}

// The cache tiers were already guarded, with a stated reason: losing either one
// here silently under-prices the whole stream. Pinned alongside so the two
// guards are read as one rule rather than as a habit.
func TestStreamUsageKeepsTheCacheTiersAcrossADeltaThatOmitsThem(t *testing.T) {
	var usage *anthropicapi.Usage
	usage = updateUsage(usage, usageEvent("message_start",
		`{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":88,"cache_creation_input_tokens":12}`))
	usage = updateUsage(usage, usageEvent("message_delta", `{"output_tokens":40}`))
	if usage.CacheReadInputTokens != 88 || usage.CacheCreationInputTokens != 12 {
		t.Fatalf("a delta that says nothing about the cache dropped it: read=%d write=%d",
			usage.CacheReadInputTokens, usage.CacheCreationInputTokens)
	}
}

func TestStreamUsageMergesCacheWriteLifetimeDetails(t *testing.T) {
	usage := updateUsage(nil, usageEvent("message_start",
		`{"input_tokens":10,"cache_creation_input_tokens":12,"cache_creation":{"ephemeral_1h_input_tokens":12}}`))
	usage = updateUsage(usage, usageEvent("message_delta", `{"output_tokens":2}`))
	if got := semanticUsage(*usage).CacheWriteLongInputTokens; got != 12 {
		t.Fatalf("omitted details lost the start's long write count: %d", got)
	}
	usage = updateUsage(usage, usageEvent("message_delta", `{"cache_creation_input_tokens":14,"output_tokens":3}`))
	if got := semanticUsage(*usage); got.CacheWriteInputTokens != 14 || got.CacheWriteLongInputTokens != 12 || got.CacheWriteTTLPair != "" {
		t.Fatalf("a higher cumulative total erased a known long span or fabricated a complete split: %+v", got)
	}
	usage = updateUsage(usage, usageEvent("message_delta",
		`{"cache_creation_input_tokens":16,"cache_creation":{"ephemeral_5m_input_tokens":12,"ephemeral_1h_input_tokens":4},"output_tokens":5}`))
	if got := portableUsage(*usage); got.CacheWritePromptTokens() != 16 || got.CacheWriteLongTokens != 4 {
		t.Fatalf("updated details were lost: %+v", got)
	}
	usage = updateUsage(usage, usageEvent("message_delta", `{"cache_creation_input_tokens":0,"output_tokens":8}`))
	if got := semanticUsage(*usage); got.CacheWriteInputTokens != 0 || got.CacheWriteLongInputTokens != 0 {
		t.Fatalf("explicit zero did not replace the start's details: %+v", got)
	}
}

func TestStreamUsageDistinguishesOmittedInputFromReportedZero(t *testing.T) {
	for _, start := range []string{`null`, `{}`, `{"cache_read_input_tokens":3}`} {
		usage := updateUsage(nil, usageEvent("message_start", start))
		usage = updateUsage(usage, usageEvent("message_delta", `{"output_tokens":7}`))
		if !usage.InputIncomplete || usage.OutputIncomplete || usage.OutputTokens != 7 {
			t.Fatalf("start %s lost input presence or reported output: %+v", start, usage)
		}
		usage = updateUsage(usage, usageEvent("message_delta", `{"input_tokens":0}`))
		if usage.InputIncomplete || usage.InputTokens != 0 || usage.OutputTokens != 7 {
			t.Fatalf("start %s did not preserve an explicit zero: %+v", start, usage)
		}
	}
	usage := updateUsage(nil, usageEvent("message_start", `{"input_tokens":0}`))
	if usage.InputIncomplete {
		t.Fatalf("explicit zero in start was treated as absent: %+v", usage)
	}
}
