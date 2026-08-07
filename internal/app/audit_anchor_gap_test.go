package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
)

// TestVerifyAuditAnchorsReportsGapsAndRewinds covers the difference between
// "every anchor I was shown agrees" and "the record is complete". Judging only
// the lines present let whoever could edit the witness file delete the ones
// that disagreed and collect a clean report; the file is plain JSON lines with
// no integrity of its own, so that edit costs nothing.
//
// A gap is not proof of tampering — the emitter keeps a bounded ring and a
// witness that was offline long enough will legitimately miss anchors — but
// the operator has to be told which of the two situations they are looking at,
// and only the report can say.
func TestVerifyAuditAnchorsReportsGapsAndRewinds(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	anchor := func(sequence, records uint64) boltstore.AuditAnchor {
		return boltstore.AuditAnchor{
			Sequence: sequence, Records: records,
			LastHash: summary.LastHash, InstanceID: "ins_test",
		}
	}

	// Sequence 2 is absent: the shape left behind by deleting the one anchor
	// that would have disagreed.
	verdicts, err := VerifyAuditAnchors(context.Background(), cfg,
		[]boltstore.AuditAnchor{anchor(1, summary.Records), anchor(3, summary.Records)})
	if err != nil {
		t.Fatal(err)
	}
	if !hasVerdict(verdicts, AnchorVerdictMissing) {
		t.Fatalf("a skipped anchor sequence must be reported: %#v", verdicts)
	}

	// A sequence that goes backwards cannot come from one emitter counting up.
	verdicts, err = VerifyAuditAnchors(context.Background(), cfg,
		[]boltstore.AuditAnchor{anchor(5, summary.Records), anchor(2, summary.Records)})
	if err != nil {
		t.Fatal(err)
	}
	if !hasVerdict(verdicts, AnchorVerdictMisordered) {
		t.Fatalf("a rewound anchor sequence must be reported: %#v", verdicts)
	}
}

// TestVerifyAuditAnchorsReportsUnwitnessedTail pins the other half. Anchors
// that all agree still say nothing about records appended after the newest one
// — and that tail is exactly what a truncation would take.
func TestVerifyAuditAnchorsReportsUnwitnessedTail(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := VerifyAudit(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Records < 2 {
		t.Fatalf("expected more than one audit record, got %d", summary.Records)
	}

	// An anchor that only covers the first record: everything after it is
	// unwitnessed, and saying "agree" alone would imply otherwise.
	stale := boltstore.AuditAnchor{
		Sequence: 1, Records: 1, LastHash: summary.LastHash, InstanceID: "ins_test",
	}
	verdicts, err := VerifyAuditAnchors(context.Background(), cfg, []boltstore.AuditAnchor{stale})
	if err != nil {
		t.Fatal(err)
	}
	if !hasVerdict(verdicts, AnchorVerdictUnwitnessed) {
		t.Fatalf("records past the newest anchor must be reported: %#v", verdicts)
	}
}

func hasVerdict(verdicts []AnchorVerdict, outcome string) bool {
	for _, verdict := range verdicts {
		if verdict.Outcome == outcome {
			return true
		}
	}
	return false
}
