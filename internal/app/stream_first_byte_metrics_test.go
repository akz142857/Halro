package app

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/gatewayapi"
	"github.com/akz142857/Halro/internal/usage"
)

// TestStreamFirstByteExposition pins the wire format.
//
// A classic histogram's buckets are cumulative and its `+Inf` bucket has to
// equal the count, or every percentile a dashboard computes from it is wrong in
// a way nothing reports. §5 is signed against this series, so the exposition is
// part of what is being signed.
func TestStreamFirstByteExposition(t *testing.T) {
	var buckets [len(usage.LatencyBucketsMillis)]uint64
	// One sample under the ladder's third bound, one under its fifth, and one
	// past the last — so the cumulative sum and the overflow both have to show.
	buckets[2], buckets[4] = 1, 2
	rendered := renderFirstByte(t, []gatewayapi.StreamFirstByteSample{{
		Operation: "chat_completions", Buckets: buckets, Overflow: 1, Count: 4, SumMillis: 900,
	}})

	var cumulative []uint64
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.HasPrefix(line, "halro_stream_first_byte_seconds_bucket{") {
			continue
		}
		if !strings.Contains(line, `operation="chat_completions"`) {
			t.Fatalf("a bucket line lost its operation label: %q", line)
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		cumulative = append(cumulative, value)
	}
	if len(cumulative) != len(usage.LatencyBucketsMillis)+1 {
		t.Fatalf("rendered %d bucket lines, want one per bound plus +Inf", len(cumulative))
	}
	for index := 1; index < len(cumulative); index++ {
		if cumulative[index] < cumulative[index-1] {
			t.Fatalf("bucket %d (%d) is below its predecessor (%d); the histogram is not cumulative",
				index, cumulative[index], cumulative[index-1])
		}
	}
	if final := cumulative[len(cumulative)-1]; final != 4 {
		t.Fatalf("+Inf is %d and the count is 4; the overflow sample was lost", final)
	}
	if !strings.Contains(rendered, `halro_stream_first_byte_seconds_count{operation="chat_completions"} 4`) {
		t.Fatalf("count line missing or wrong:\n%s", rendered)
	}
	// Seconds, with trailing zeros trimmed the way every other duration here
	// renders: 900ms is 0.9, not 0.900.
	if !strings.Contains(rendered, `halro_stream_first_byte_seconds_sum{operation="chat_completions"} 0.9`) {
		t.Fatalf("sum line missing or not in seconds:\n%s", rendered)
	}
}

// TestStreamFirstByteRendersOperationsInAStableOrder. Two scrapes of an
// unchanged process must produce identical text, or a diff of the endpoint is
// noise.
func TestStreamFirstByteRendersOperationsInAStableOrder(t *testing.T) {
	samples := []gatewayapi.StreamFirstByteSample{
		{Operation: "responses", Count: 1},
		{Operation: "chat_completions", Count: 1},
		{Operation: "messages_native", Count: 1},
		{Operation: "messages", Count: 1},
	}
	first := renderFirstByte(t, samples)
	second := renderFirstByte(t, samples)
	if first != second {
		t.Fatal("two renders of the same samples differ")
	}
	order := []string{"chat_completions", "messages", "messages_native", "responses"}
	position := -1
	for _, operation := range order {
		next := strings.Index(first, `operation="`+operation+`"`)
		if next < position {
			t.Fatalf("operations are not rendered in order: %s appears at %d", operation, next)
		}
		position = next
	}
}

// TestStreamFirstByteRendersNothingForAQuietInstance. An instance that has
// served no stream has no distribution, and inventing zero-valued series for
// four operations would claim four endpoints are in use.
func TestStreamFirstByteRendersNothingForAQuietInstance(t *testing.T) {
	rendered := renderFirstByte(t, nil)
	if strings.Contains(rendered, "_bucket{") {
		t.Fatalf("a quiet instance rendered buckets:\n%s", rendered)
	}
	if !strings.Contains(rendered, "# TYPE halro_stream_first_byte_seconds histogram") {
		t.Fatal("the series header should still be declared so a scrape knows the type")
	}
}

func renderFirstByte(t *testing.T, samples []gatewayapi.StreamFirstByteSample) string {
	t.Helper()
	var buffer bytes.Buffer
	output := bufio.NewWriter(&buffer)
	writeStreamFirstByte(output, samples)
	if err := output.Flush(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}
