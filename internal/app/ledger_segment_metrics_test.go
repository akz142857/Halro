package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
)

func TestLedgerSegmentMetricsMatchCommittedDirectoryAfterCompression(t *testing.T) {
	r, _ := openSeededRuntime(t)
	check := func() {
		t.Helper()
		var want ledger.SegmentSummary
		err := r.ledger.WalkSegments(context.Background(), func(segment ledger.Segment) error {
			want.Generations++
			want.PlainBytes += segment.Length
			want.StoredBytes += segment.StoredLength
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		metrics := renderMetricsForTest(t, r)
		for _, series := range []string{
			fmt.Sprintf("halro_ledger_sealed_generations %d\n", want.Generations),
			fmt.Sprintf("halro_ledger_sealed_bytes %d\n", want.PlainBytes),
			fmt.Sprintf("halro_ledger_sealed_stored_bytes %d\n", want.StoredBytes),
		} {
			if !strings.Contains(metrics, series) {
				t.Fatalf("missing metric %q", series)
			}
		}
	}
	check()
	rolled, err := r.ledger.Roll()
	if err != nil || !rolled.Rolled {
		t.Fatalf("roll=%+v err=%v", rolled, err)
	}
	check()
	if _, err := r.ledger.Compact(rolled.Sealed.Generation); err != nil {
		t.Fatal(err)
	}
	check()
}
